# 8. Full Stream Engine — Concepts

The seven preceding lessons each built one layer in isolation — source connectors, operators, windowing, watermarks, checkpointing, parallel execution, sink connectors. This capstone assembles them into one engine. The hard part is no longer any single algorithm; it is the wiring, because a stream engine coordinates five independent contracts at once and a mistake in any one of them corrupts results silently rather than crashing. This file is the conceptual foundation for both exercises, which build the assembled engine as two self-contained Go modules: a job-graph engine (a declarative builder, a graph compiler that inserts shuffles automatically, a lifecycle-managing job manager, and a backpressure monitor) and a runnable windowed engine (a real, goroutine-driven pipeline that executes an end-to-end, watermark-fired, parallel keyed aggregation and collects the results).

## Concepts

### The integration problem: five coordination contracts

A production stream engine (Apache Flink, Kafka Streams) coordinates five concerns simultaneously, and each one rides through the same network of channels:

- **Checkpoint barriers** are special markers injected by the coordinator that flow downstream alongside records to trigger a consistent distributed snapshot (the Chandy-Lamport algorithm from lesson 05). An operator with multiple inputs must *align* barriers: it blocks each input that has delivered its barrier until every input has, so the snapshot reflects a single logical cut.
- **Watermarks** are timestamps asserting that no record older than `T` will arrive. An operator with several upstream partitions emits the *minimum* watermark across all of them, never the maximum — only the slowest input determines how far event time has truly advanced.
- **Backpressure** propagates by blocking on full buffered channels. A slow sink stalls its upstream operator, which stalls the source, which throttles ingestion. No explicit signal is sent; the bounded channel *is* the signal.
- **Lifecycle cancellation** propagates through `context.Context`. Cancelling the root context unblocks every goroutine guarded by `select { case <-ch: case <-ctx.Done(): }`, so the whole topology tears down without leaking goroutines.
- **Key-based partitioning** requires that all records sharing a key reach the same parallel instance of a keyed operator. This forces a shuffle — a network redistribution — between a non-keyed stage and the keyed stage that follows.

The central design decision is to carry every message kind — records, barriers, watermarks — in a single tagged-union `Message` type through every inter-task channel. One channel type per upstream link lets barrier-alignment and watermark-tracking code manage a single channel per input instead of three synchronized ones; a `Type` discriminator selects the payload, and operators that do not care about barriers or watermarks simply forward them unchanged.

### The declarative API as a compiler boundary

The first module separates *declaring* a pipeline from *executing* one. A `JobBuilder` records the user's pipeline as a slice of stage values; it starts no goroutines and allocates no channels. `Build()` validates the declaration and returns an immutable `*JobDecl`. A separate `Compile()` walks the stages and materializes a `*TaskGraph` — a DAG of `*TaskNode` values connected by upstream pointers. This boundary means the user cannot confuse declaration order with execution order: the graph is the single authoritative representation of data flow, and it is the thing you inspect, schedule, and instrument.

The builder uses the *accumulator-error* pattern. Every method first checks `b.err != nil` and returns the builder unchanged if an error is already latched; `Build()` reports that first error. This lets a caller chain `Source(...).Map(...).KeyBy(...)` fluently without an `if err != nil` after each call. The subtlety is that only the *first* error is kept, so `NewJobBuilder("").Source(nil).Build()` returns the empty-name error, not the nil-source error.

### Automatic shuffle insertion before keyed operators

A keyed operator — a windowed aggregation or a keyed join — needs every record for a given key to land on the same parallel instance, or each instance computes against a partial view of the key's state and the aggregate is wrong. The compiler enforces this structurally: when it reaches a `KeyBy` stage whose upstream is not already partitioned by key, it inserts a `Shuffle` node before it. The shuffle marks where data must cross partition boundaries; the partitioning function (`hash(key) % N`) runs inside the shuffle's goroutines at execution time. A boolean `keyed` flag tracks whether the stream is currently partitioned; it flips on after a `KeyBy` and resets on a re-key, so a second `KeyBy` downstream gets its own shuffle.

### The job lifecycle state machine

A submitted job moves through `Created → Running → (Checkpointing | Failing) → (Finished | Cancelled)`. The `JobManager` is the only writer of the status field, always under its mutex, while reads go through `Status(id)` and transitions through `Cancel(id)`. Each job holds a derived `context.CancelFunc`; `Cancel` fires it, which unblocks every goroutine waiting on `ctx.Done()`, and then records the terminal status. `Cancel` on an already-terminal job returns `ErrJobNotRunning` rather than silently succeeding, so a double-cancel bug is observable instead of hidden. ID generation uses a lock-free `atomic.Uint64`, so submitting jobs concurrently never contends on the status mutex just to mint an identifier.

### Backpressure monitoring via channel fill level

The cheapest per-link backpressure signal is a buffered channel's fill ratio, `len(ch) / cap(ch)`. A channel pinned at 100% means its consumer cannot keep up — that link is the bottleneck; a channel at 0% means the producer is starving it. The monitor polls registered channels on demand with `Sample()` rather than instrumenting every send. Polling is correct because a channel that is *persistently* full is a bottleneck whether you observe it once a second or on every record, and per-record observation would itself become a bottleneck on the hot path. Operationally you call `Sample()` on a ticker and expose the snapshot as a metric or a status endpoint.

### Executing a topology: the runnable engine

A compiled graph is inert. The second module is where data actually moves: each operator becomes a goroutine, and adjacent operators are joined by a bounded channel that carries the same record/watermark message union. The pipeline is `source → map → filter → shuffle(by key) → windowed reduce (parallel) → sink`. The source emits records in event-time order and interleaves watermarks; map and filter transform and drop records and forward watermarks untouched; the shuffle routes each record to one of `N` window partitions by `hash(key) % N` and *broadcasts* every watermark to all `N` partitions; each window partition keeps a per-window, per-key accumulator and fires a window only when a watermark proves event time has passed the window's end; the sink fans in the fired results.

Three properties make this correct and testable. First, **watermark-driven firing**: a tumbling window `[start, start+size)` emits its aggregate the moment a watermark `≥ start+size` arrives, and a final watermark of `+∞` flushes every still-open window at end of stream — so results never depend on wall-clock timing and the test output is deterministic. Second, **partition independence**: because the shuffle sends a given key to exactly one partition, every record for that key accumulates in the same place, so the aggregate is identical at any parallelism — a property you can assert directly by running the same input at parallelism 1, 2, 4 and comparing. Third, **clean fan-in shutdown**: the result channel is closed by exactly one goroutine that first `wg.Wait()`s for all window operators, which is the only safe way to close a channel that several goroutines write to.

The reduce step is just a `func(a, b int64) int64`: pass `a + b` for counts or sums, or `max(a, b)` for a high-water aggregate. The engine combines the first value into the accumulator and folds each subsequent value with the reduce function, so the same machinery computes word counts, running sums, or per-window maxima without any change to the execution code.

## Common Mistakes

### Forgetting the shuffle between KeyBy and its upstream

Wrong: routing records directly from a non-keyed map into a keyed operator and assuming same-key records reach the same instance. With `N` parallel instances, round-robin or broadcast routing scatters one key across several instances; each sees a partial view and the aggregate is silently wrong. Fix: insert the shuffle (the compiler does this automatically before any `KeyBy` whose upstream is not already keyed) so the partitioning function sends each key to exactly one instance. The runnable engine encodes the same rule as `hash(key) % N`.

### Emitting the maximum watermark across upstream partitions instead of the minimum

Wrong: forwarding the largest watermark seen across all inputs. The downstream window then fires early because it believes time has advanced past the boundary, while a slower partition still holds earlier records — which are then dropped as late. Fix: track the latest watermark per upstream partition and emit `min(watermarks)`; only when *every* partition has passed the boundary can the window safely close.

### Closing the result channel before all writers are done

Wrong: letting one fan-in goroutine `close(results)` while another is still writing to it. The runtime panics with `send on closed channel`. Fix: close the merged channel from a single goroutine that first `wg.Wait()`s for every writer. This is the canonical fan-in pattern from the Go pipelines blog post, and the runnable engine uses exactly it.

### Misusing the accumulator-error pattern

Wrong: checking `NewJobBuilder("").Build()` for `ErrNoSource`. The builder latches `ErrEmptyJobName` first and turns subsequent method calls into no-ops, so `Build()` reports the *first* error, not the last. Fix: treat `setErr` as a one-write latch — validate the name before chaining, and match on `ErrEmptyJobName` with `errors.Is` when the name might be empty.

### Reading window results in nondeterministic order

Wrong: asserting on the order results arrive at the sink. With parallel window partitions the fan-in interleaves results by goroutine scheduling, so the order varies run to run. Fix: sort the collected results by `(windowStart, key)` before comparing — the engine does this so its output and tests are reproducible, and so the demo prints the same block on every run.

---

Next: [01-job-graph-engine.md](01-job-graph-engine.md)
