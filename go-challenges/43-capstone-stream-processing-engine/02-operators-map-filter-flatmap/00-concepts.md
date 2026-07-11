# 2. Stream Operators — Concepts

A stream operator is a small machine that reads records from one channel, does one transformation, and writes the result to another. Map, filter, and flat-map are the three stateless primitives every stream engine is built from, and they look trivial in isolation. The difficulty is everything around the transform: a uniform interface so any operator composes with any other, backpressure so a slow consumer slows the producer instead of dropping data, graceful shutdown when a context is cancelled, error routing that does not crash the pipeline, and an optimization pass that fuses adjacent operators to delete redundant goroutines and channels. This file is the conceptual foundation for the whole chapter section. Read it once and you have everything needed to reason through each exercise, which builds the operators one at a time as independent, self-contained Go modules: the operator interface plus Map, then Filter and FlatMap, then the fusing pipeline builder, and finally three operators a production engine cannot do without — a stateful scan/reduce, a key-partitioning fan-out, and a lazy pull-based operator set built on Go 1.23 range-over-func iterators.

## Concepts

### The Operator Interface

Every operator satisfies the same one-method interface:

```go
type Operator interface {
	Process(ctx context.Context, in <-chan Record) (<-chan Record, <-chan error)
}
```

`Process` spawns a goroutine, owns its output channel, and closes it when the input is exhausted or the context is cancelled. The caller receives two channels: the record stream and a single-slot error channel. Keeping error delivery separate from the record stream lets callers decide whether to drain both concurrently or sequentially without blocking the pipeline. Embedding errors inside the record type would contaminate the domain type and force every downstream operator to unwrap before transforming; separating the two channels is the approach `errgroup`, `io.Pipe`, and Go's `net` package all take. Because the signature is identical for Map, Filter, and FlatMap, the pipeline builder can wire any operator's output into any other operator's input without knowing the concrete type.

### Backpressure Through Blocking Sends

Every operator performs a blocking send on its output channel, wrapped in a `select` on `ctx.Done()`:

```go
select {
case out <- res:
	m.metrics.Out.Add(1)
case <-ctx.Done():
	return
}
```

A buffered output channel (default depth 16) absorbs micro-bursts. When the buffer fills, the send blocks; because the operator is blocked it stops reading from `in`; the upstream operator then blocks on its own send. The back-pressure propagates all the way to the source without any record being dropped and without any busy-wait spin loop. The alternative — a non-blocking send with a `default` branch that drops the record — prevents deadlocks but silently loses data, which is almost always wrong in a data-processing pipeline.

### Error Routing

Transform functions return `(Result, error)`. What happens to a failing record is decided by an `ErrorHandler` that returns one of three actions:

```go
const (
	Skip  ErrorAction = iota // discard the record, continue
	Retry                     // re-apply the transform once
	Abort                     // send the error downstream, stop the operator
)
```

The default is `SkipOnError`. The caller can supply `AbortOnError` or a custom handler via `WithErrorHandler`. The Abort path wraps the original error with `%w` (`fmt.Errorf("map operator: %w", err)`) so callers can match it with `errors.Is`, exactly as `database/sql` and `net/http` do. The error is sent with a non-blocking send to a capacity-1 channel, so an operator never deadlocks trying to report a failure no one is reading.

### Operator Fusion

A pipeline with three `Map` steps creates three goroutines and three intermediate channels. When two adjacent `MapOperator` values are detected during `Build`, the pipeline collapses them into one operator whose function is the composition `outer(inner(r))`:

```go
fused := NewMap(func(r Record) (Record, error) {
	r2, err := inner(r)
	if err != nil {
		return Record{}, err
	}
	return outer(r2)
})
```

The fusion pass makes a single left-to-right scan, so three adjacent maps become one fused plus one remaining, and a `Filter` between two maps breaks adjacency so neither map is fused. Apache Flink calls this "chaining" and applies it automatically for operators that share the same parallelism with no shuffle between them. Fusion is only safe when the operators are adjacent and stateless: composing two functions that each close over mutable state would interleave their state changes in an order the author never intended, so the fusion pass is deliberately restricted to the pure `*MapOperator` type.

### Metrics

Each operator holds a `*Metrics` value with four `atomic.Int64` counters: `In`, `Out`, `Dropped`, and `Errors`. Using `sync/atomic` avoids a mutex on the hot path and lets any goroutine read the counters concurrently without locking. The `-race` flag is mandatory when testing operators precisely because these counters are shared between the operator goroutine and the test goroutine that reads them; a test that passes without `-race` but fails with it has a real data race that `atomic.Int64` is there to prevent.

### Stateful Operators: Scan and Reduce

Map, filter, and flat-map are stateless: each output depends only on the current input. A stateful operator carries an accumulator across records. Scan emits the running aggregate after every element (a running sum emits 1, 3, 6, 10 for inputs 1, 2, 3, 4); Reduce folds the whole stream and emits a single final value when the input closes. The accumulator lives inside the operator's single goroutine, which is what makes it safe without a lock — only that goroutine reads or writes it. Keyed state (`ScanByKey`) keeps an independent accumulator per key in a plain map, again lock-free because one goroutine owns the map, and preserves per-key ordering because records for a given key are folded in arrival order. This single-owner-goroutine pattern is the foundation of how a real engine partitions state, and it is why stateful operators must never be fused: fusion would share that state across operators with no ordering guarantee.

### Key Partitioning (keyBy)

To process a stream in parallel while keeping per-key ordering, you partition it: route every record to one of N sub-streams chosen by a hash of its key, so all records with the same key always land in the same partition. Each partition is then an independent ordered substream that a downstream worker can consume concurrently. This is Flink's `keyBy`, Kafka's partitioner, and a MapReduce shuffle — the same idea each time. The crucial property is determinism: `hash(key) % N` must send a key to the same partition every time, or per-key ordering and keyed state both break. The trade-off to understand is head-of-line blocking: a single router goroutine feeding N channels stalls all partitions if one partition's consumer is slow, so partition channels are buffered to decouple the router from a temporarily slow worker.

### Lazy Pull-Based Operators (range-over-func)

The channel operators above are push-based and concurrent: a goroutine pushes records downstream. Go 1.23 added range-over-func iterators (`iter.Seq[V]`), which give a pull-based, single-goroutine alternative. An `iter.Seq[V]` is just a function `func(yield func(V) bool)`; a `for v := range seq` loop calls it, and each `yield(v)` hands one value to the loop body. A map operator over `iter.Seq` is a few lines with no channels and no goroutines:

```go
func MapSeq[In, Out any](seq iter.Seq[In], fn func(In) Out) iter.Seq[Out] {
	return func(yield func(Out) bool) {
		for v := range seq {
			if !yield(fn(v)) {
				return
			}
		}
	}
}
```

The `if !yield(...) { return }` is load-bearing: `yield` returns false when the consumer stops early (a `break` in the range loop), and propagating that false upstream is how early termination short-circuits a chain of lazy operators and stops an infinite generator. These operators are lazy — nothing runs until the final range loop pulls — and allocation-free of channels, which makes them ideal for in-memory transformation chains where the concurrency of channel operators would be pure overhead.

## Common Mistakes

### Closing the Output Channel Before the Read Loop Exits

Wrong: calling `close(out)` at the top of the goroutine before the `for r := range in` loop. Sends on `out` then panic with "send on closed channel". Fix: use `defer close(out)` so the close runs only after the read loop returns, which is after the input is exhausted or the context is cancelled.

### Non-Blocking Sends That Silently Drop Records

Wrong: a `select` with `case out <- res:` and a bare `default:` that discards the record when the buffer is full. Under load the pipeline silently loses data with no error and no deadlock — the worst kind of bug. Fix: block on the send and also select on `ctx.Done()`, so a slow consumer applies backpressure and only a cancelled context aborts the send.

### Not Draining the Error Channel

Wrong: `out, _ := op.Process(ctx, in)` and ranging only over `out`. Because the Abort path uses a non-blocking send to a capacity-1 channel there is no deadlock, but the error is silently lost. Fix: always drain both channels — typically a background goroutine ranging over `errc` while the main goroutine ranges over `out`.

### Fusing Stateful Operators

Wrong: composing two operators that each close over mutable state. The fused function interleaves their state mutations in an order neither author intended. Fix: restrict fusion to pure, stateless operators; in these exercises fusion is limited to `*MapOperator`, and stateful Scan/Reduce operators are never fused.

### Forgetting Context Cancellation Between FlatMap Sends

Wrong: a plain `for _, r := range results { out <- r }` inside flat-map. A burst of ten thousand output records from one input cannot be interrupted mid-flight, so the goroutine leaks until something drains the channel. Fix: wrap every inner send in a `select` that also listens on `ctx.Done()`.

### Dropping the yield Result in a range-over-func Operator

Wrong: writing `for v := range seq { yield(fn(v)) }` and ignoring `yield`'s boolean result. When the consumer breaks early, the operator keeps pulling from an upstream generator that may be infinite, hanging the program. Fix: `if !yield(fn(v)) { return }` so early termination propagates up the whole chain.

---

Next: [01-operator-core-and-map.md](01-operator-core-and-map.md)
