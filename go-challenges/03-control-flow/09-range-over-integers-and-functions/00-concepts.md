# Range Over Integers and Function Iterators — Concepts

Range-over-func is not a syntax curiosity. It is the mechanism modern Go backends
use to expose lazy, composable, memory-bounded streams without allocating
intermediate slices or spinning up channels and goroutines. A senior engineer
meets `iter.Seq` at the boundaries of real systems: a repository layer that
streams a million rows without materializing them, a cloud SDK paginator that
fetches the next page only when the current one is consumed, a log decoder that
must propagate a parse error mid-stream, a bulk writer that batches a stream into
DB-sized chunks, a retry loop expressed as a sequence of attempts, an idempotency
guard over an event stream. One invariant is load-bearing across all of them:
cooperative early termination. When the consumer stops — context cancelled, first
error seen, first match found, `Take(k)` satisfied — the producer must stop too
and run its cleanup (close the cursor, cancel the HTTP request, release the
goroutine) instead of running to completion. This file is the conceptual
foundation; read it once and each of the ten independent exercises that follow
becomes a variation on the same contract.

## Concepts

### `for i := range n` is the counted loop

`for i := range 5 { ... }` iterates `i` over `0,1,2,3,4`. The compiler emits the
same code as `for i := 0; i < 5; i++`, and the intent is clearer when the bound
is a plain count. This form landed in Go 1.22. It is unrelated to iterators
except that it reads the same way, and it is the natural loop to drive a producer.

### `iter.Seq` is a named function type, not a new capability

The `iter` package defines two aliases:

```go
type Seq[V any] func(yield func(V) bool)
type Seq2[K, V any] func(yield func(K, V) bool)
```

Any function of that exact shape is rangeable — the aliases exist for
documentation and for interoperation with `slices` and `maps` helpers, not to
grant new behavior. A plain `func(yield func(int) bool)` and an `iter.Seq[int]`
are assignable to each other because their underlying types are identical. Typing
your combinators as `iter.Seq[T]` is what lets them flow into `slices.Collect`,
`slices.Values`, `maps.All`, and the rest of the standard iterator surface.

Range-over-func was a `GOEXPERIMENT` in Go 1.22 and was stabilized — default on —
in Go 1.23. The string and slice/map iterator helpers (`strings.Lines`,
`strings.SplitSeq`, `strings.FieldsSeq`, `slices.Sorted`, `maps.Insert`, and the
rest) arrived in Go 1.24. Any lesson still framing this as "1.22 only" is stale;
set the `go` directive to at least 1.24 for the stdlib iterator helpers.

### The push model: the iterator drives

In a push iterator the producer is in control. It calls `yield(v)` once per value.
The runtime rewrites the consumer's `for v := range seq { ... }` loop body into
the `yield` closure, so calling `yield(v)` runs one iteration of the consumer's
loop. `yield` returns a `bool`: `true` means keep going, `false` means the
consumer is done — it hit a `break`, a `return`, a `panic`, or an outer loop's
`continue`/`goto` that leaves the range. When `yield` returns `false` the iterator
MUST stop immediately and unwind. Ignoring that return value is the single most
common correctness bug with iterators, and it silently leaks resources.

### Cooperative termination is a contract, not an optimization

Because the consumer can stop the producer at any yield, any resource the producer
holds — an open cursor, an in-flight HTTP body, a spawned goroutine — must be
released on every exit path, including the early-break path and the panic path.
The idiom is a `defer` at the top of the iterator body:

```go
func ScanUsers(rs Rows) iter.Seq[User] {
	return func(yield func(User) bool) {
		defer rs.Close() // runs on exhaustion, on early break, on panic
		for rs.Next() {
			if !yield(user(rs)) {
				return
			}
		}
	}
}
```

Putting the `Close()` after the loop instead of in a `defer` means an early break
or a panic skips it, and the DB connection leaks. This is not a style preference;
it is the whole point of the pattern.

### Combinators compose by wrapping `yield`

`Map`, `Filter`, `Take` are all iterators that wrap another iterator. `Map(f,
src)` returns an `iter.Seq[U]` whose body runs `src` with a yield that applies
`f`. `Filter(pred, src)` forwards a value only when `pred` holds, and crucially
returns `true` (keep asking upstream) when it drops a value. `Take(n, src)` counts
and returns `false` from its wrapper to stop `src` after `n` values. The output
type of one stage is the input type of the next, so the type-parameter chain must
be consistent end to end: a `Map` that produces `string` cannot feed a `Filter`
over `int`, and the compiler rejects it.

### Lazy streaming is memory-bounded and short-circuiting

`Take(1, Filter(pred, src))` walks `src` only until the first match, then stops.
`Collect(Filter(pred, src))[0]` walks all of `src`, allocates the whole slice, and
then throws away everything but the first element. The iterator form turns an
`O(n)` materialization into `O(1)` look-ahead whenever the consumer stops early.
That is why the repository, pagination, and log exercises all express their work
as `iter.Seq` rather than returning a `[]T`: the caller decides how much to pull.

### `iter.Pull` for when the push model is the wrong shape

Some algorithms cannot be written as a single push loop because they need to look
at more than one stream at once — a merge-join of two sorted inputs must peek the
front of both sides and advance only the smaller. `iter.Pull(seq)` converts a
push iterator into a pull-based pair `next, stop := iter.Pull(seq)`: `next()`
returns the next `(value, true)` or `(zero, false)` at exhaustion, and `stop()`
tears the sequence down. Pull runs `seq` on a goroutine, so the rule is absolute:
`defer stop()` immediately after the `iter.Pull` call. Forgetting it leaks the
goroutine. Calling `next()` after `stop()` returns `(zero, false)`, and you must
never call `next()` concurrently from two goroutines.

### Map iteration order is unspecified and randomized

`maps.Keys`, `maps.Values`, and `maps.All` yield in an arbitrary, deliberately
randomized order. Anything you serialize, log, diff against a golden file, or
compare must impose an order first: `slices.Sorted(maps.Keys(m))` or
`slices.SortedFunc(...)`. A config dump that ranges a map directly is
non-deterministic and will flake in tests and confuse diffs in production.

### Error propagation has two idioms

Iterators surface errors two ways. The first is `iter.Seq2[T, error]`: yield the
error alongside each value and have the consumer check `err` on every step,
stopping on the first non-nil. This is right when a per-item failure should halt
iteration — a streaming decoder that hits a malformed line. The second is a
captured `*error` (or an `Err()` method) that the consumer reads after the loop —
this mirrors `database/sql`'s `rows.Err()` and fits when the error is a
terminal condition of the whole scan rather than a per-item event. Both appear in
this lesson; pick by whether the failure is per-item (Seq2) or terminal (captured
error).

### Closure state must be reset and bounded

Batching carries a buffer across yields; dedup carries a seen-set; windowing
carries a ring. That state lives in the iterator's closure and is created fresh
each time the `iter.Seq` is invoked — which means a sequence is re-runnable, but a
single run must reset its own buffer after each flush. The dangerous case is an
unbounded seen-map in a long-lived stream: it grows without limit and is a memory
leak. Production dedup is windowed or TTL-backed for exactly this reason.

### `strings.Lines` versus `bufio.Scanner`

Both stream lines. `strings.Lines(s)` yields each line of an in-memory string
*including* its terminating newline, and yields a final unterminated line as-is;
it never copies the whole result into a `[]string`. `bufio.Scanner` streams lines
from an `io.Reader` with a configurable buffer cap and strips the newline. Choose
`Scanner` for reader sources with a size bound, `Lines`/`SplitSeq`/`FieldsSeq` for
in-memory strings you want to tokenize lazily.

## Common Mistakes

### Ignoring yield's return value

Wrong: `for i := range n { yield(i) }`. The consumer's `break` is silently
dropped, the producer runs to completion, and any cursor, HTTP body, or goroutine
it holds leaks. Fix: `if !yield(v) { return }` on every yield.

### Cleanup after the loop instead of in a defer

Wrong: closing the cursor on the line after the `for` loop. An early break or a
panic skips it. Fix: `defer rs.Close()` at the top of the iterator body so it runs
on every exit path.

### Calling iter.Pull without deferring stop

Wrong: `next, stop := iter.Pull(seq)` and then returning early on error without
ever calling `stop`. The pulled sequence's goroutine never terminates. Fix:
`defer stop()` on the line right after the `iter.Pull`.

### Yielding after stop, or continuing after yield returned false

Wrong: calling `yield` again after it already returned `false`, or spawning a
goroutine inside the iterator that keeps calling `yield`. Both trigger the runtime
panic "range function continued iteration after loop body returned false" or a
data race. Fix: `return` the instant `yield` reports `false`.

### Serializing or diffing map output without sorting

Wrong: ranging `maps.All(cfg)` straight into a log line or golden file. The order
is randomized, so the output is non-deterministic and flaky. Fix:
`slices.Sorted(maps.Keys(cfg))` before emitting.

### Aliasing a reused batch buffer

Wrong: a batching iterator that yields the same backing slice every flush. After
the next batch mutates the buffer, the consumer's retained slice shows corrupted
data. Fix: copy into a fresh slice per batch, or document that the batch is valid
only until the next iteration.

### Materializing when you need the first N

Wrong: `Collect(Filter(pred, src))[0]` to get one match. It walks and allocates
everything. Fix: `Take(1, Filter(pred, src))` or a `First` combinator so early
termination actually short-circuits.

### Assuming range-over-func exists in Go 1.22 by default

It was `GOEXPERIMENT=rangefunc` in 1.22 and became default in 1.23; the
`strings`/`slices`/`maps` iterator helpers arrived in 1.24. Set the `go` directive
to at least 1.24 or the stdlib helpers will not resolve.

### Swallowing decode errors mid-stream

Wrong: a streaming decoder that skips a malformed line and keeps going, silently
corrupting the result. Fix: `iter.Seq2[T, error]`, yield the error, and stop on
the first one — or accumulate and report explicitly.

### Unbounded dedup or seen-maps in a long-running consumer

Wrong: a `map[K]struct{}` that only grows in an infinite event stream. Memory
climbs without bound. Fix: a windowed or TTL-bounded dedup.

### Not checking ctx.Err() inside a long-running iterator

Wrong: a retry or streaming iterator that keeps producing after its request was
cancelled. Fix: check `ctx.Err()` before each yield and stop.

### Type-parameter mismatch when composing

Wrong: chaining a `Map` that outputs `string` into a stage that consumes `int`.
Fix: keep the `T`/`U` chain consistent — every stage's output type is the next
stage's input type.

Next: [01-streaming-pipeline-combinators.md](01-streaming-pipeline-combinators.md)
