# 1. Pipeline Pattern — Concepts

A pipeline is a series of stages connected by channels, where each stage is a group of goroutines running the same function: it reads values from one or more inbound channels, runs a transformation, and sends results on an outbound channel. The first stage is a source, the last is a sink, and everything in between is a transform. This is the structure Sameer Ajmani documented in "Go Concurrency Patterns: Pipelines and cancellation" on the Go blog, and it is the backbone of almost every streaming data service written in Go: ETL jobs, log processors, request fan-out, and batch workers all reduce to "stages connected by channels." This file is the conceptual foundation. Read it once and you will have everything you need to reason through each exercise, which build the pattern up from the canonical close-and-cancel contract to two production-grade services as independent, self-contained Go modules.

## Concepts

### A Stage Is A Function Returning A Channel

The unit of a pipeline is a stage, and the idiomatic shape of a stage is `func stage(in <-chan T) <-chan U`. The function makes its outbound channel, launches a goroutine that loops over `in` and sends transformed values to `out`, and returns `out` immediately while the goroutine runs in the background. The caller never sees the goroutine; it sees only a receive-only channel it can read or hand to the next stage. Composition is therefore just nesting: `Format(Square(Generate(...)))` wires three goroutines together and returns the final channel, and the data flows the moment something starts draining the end.

Two properties make this shape compose cleanly. The channel is returned as `<-chan U` (receive-only) so a downstream stage cannot accidentally send into or close a channel it does not own. And the transformation is a plain function of the inbound value, so a stage has no dependency on what produced its input or what consumes its output: any stage that emits `<-chan int` fits in front of any stage that consumes `<-chan int`.

### The Close Contract: Only The Sender Closes, Exactly Once

The signal that a stage is finished is the close of its outbound channel. A `for v := range in` loop exits precisely when `in` is closed and drained, so closing cascades termination down the chain: the source closes its channel when it runs out of values, the next stage's range loop ends and it closes its channel, and so on to the sink. The discipline that makes this safe is ownership: the goroutine that sends on a channel is the only one allowed to close it, and it closes exactly once. The Go blog states the rule bluntly — "Sends on a closed channel panic, so it's important to ensure all sends are done before calling close." For a single-sender stage, `defer close(out)` at the top of the goroutine is the whole implementation of the contract: it runs on every return path, including an early return, and it runs exactly once.

A receiver must never close a channel to tell the sender to stop. That inverts ownership and the next send panics with "send on closed channel." The correct way for a receiver to signal "stop" is a separate cancellation channel, covered below.

### Fan-In Needs A WaitGroup To Delay The Close

When a single outbound channel has multiple senders — the fan-in or `Merge` stage, where N inbound channels are multiplexed onto one `out` — `defer close(out)` no longer works, because there is no single goroutine that knows when all the others are done. Closing after the first sender finishes would panic the others mid-send. The textbook recipe is a `sync.WaitGroup`: one goroutine per inbound channel calls `wg.Done()` when its channel drains, and a separate closer goroutine calls `wg.Wait()` and then `close(out)`. The critical ordering detail is that `wg.Add(len(cs))` must run before the sender goroutines are launched and before the closer's `wg.Wait()` can observe a zero count; doing the `Add` up front, on the calling goroutine, removes the race. With this recipe a `Merge` of zero channels closes `out` immediately, which is the correct degenerate behavior.

### Cancellation: The `done` Channel And `context.Context`

A consumer that stops reading early — it found what it wanted, or an error aborted the request — leaves every upstream goroutine blocked forever on `out <- v`, because nothing will ever receive that value again. That is a goroutine leak: the goroutines are unreachable, never scheduled, and never garbage-collected. The fix is an explicit cancellation signal threaded through every stage. The classic form is a shared `done <-chan struct{}`: the consumer does `defer close(done)`, and every send in every stage becomes a select:

```go
select {
case out <- v:
case <-done:
	return
}
```

Closing `done` broadcasts to every stage at once, because a receive on a closed channel always proceeds immediately, and the empty struct carries no data — the close event is the entire message. The deferred `close(out)` still runs on the cancellation return path, so the channels still close cleanly.

`context.Context` is the same idea wearing the standard-library uniform, and it is what production code uses. `ctx.Done()` is exactly the `done` channel; `<-ctx.Done()` replaces `<-done`; and `ctx.Err()` tells you afterward whether the cancellation was a deadline (`context.DeadlineExceeded`) or an explicit cancel (`context.Canceled`). Context also composes: `context.WithCancel`, `context.WithTimeout`, and `context.WithDeadline` derive child contexts so a service can impose its own deadline on top of the caller's, and cancelling the parent cancels every child. A pipeline that takes a `context.Context` as its first parameter and selects on `ctx.Done()` at every send is cancellable, deadline-aware, and leak-free by construction.

### Bounded Stages And Backpressure

An unbuffered channel forces a rendezvous: a send blocks until a receive is ready, so a fast producer is paced exactly by the slowest consumer. That pacing is backpressure, and it is the property that keeps a pipeline's memory bounded. The moment you reach for buffering, you are trading memory for decoupling: `make(chan T, N)` lets a producer run up to N values ahead of its consumer before it blocks, which smooths over bursts but caps the in-flight work at N per stage. A bounded buffer (a small, fixed N) is the right tool for absorbing jitter while keeping a hard ceiling on memory; an unbounded buffer is not a tool at all, it is a memory leak waiting for a slow consumer.

Buffering is emphatically not a substitute for cancellation. Sizing a channel `make(chan int, count)` to "hold all the values" appears to fix the early-exit leak, but the Go blog flags this as bad code: it is fragile (the leak returns the instant the count is wrong or a stage is added) and it defeats backpressure (the producer races to fill the buffer regardless of consumer speed). Bound a stage to absorb bursts; cancel a stage to stop it. The two concerns are orthogonal.

### Graceful Draining Versus Hard Cancellation On Shutdown

Shutdown has two distinct meanings and a service must choose deliberately. Hard cancellation says "stop now, drop whatever is in flight": cancel the context, every stage returns at its next select, and in-progress values are discarded. Graceful draining says "stop accepting new work, but finish everything already accepted, then exit." A log or event processor almost always wants draining — losing the last second of buffered events on a deploy is a real outage — while a request-scoped pipeline whose client has already disconnected wants hard cancellation, because finishing work nobody will read is wasted effort.

The mechanics of draining are precise. To drain, you stop the source (reject new submissions and stop reading the input), then close the input channel, and let the worker goroutines run their `for e := range in` loops to completion: ranging over a closed channel delivers every buffered element before the loop ends. A `sync.WaitGroup` tracks the workers, and `Shutdown` blocks on `wg.Wait()` until the last worker returns, at which point you know every accepted event was processed and every goroutine has exited. Bounding the wait with a context lets a stuck handler turn into a timeout rather than a hang. The subtlety that bites people is the race between a `Submit` still sending and a `Shutdown` closing the same channel: guard the closed-flag and the close with a mutex (a `sync.RWMutex` where `Submit` takes the read lock and `Shutdown` the write lock) so a send and the close are mutually exclusive, and a late `Submit` returns an error instead of panicking on a closed channel.

### Instrumentation Without Races

A production stage is observed: counts of items processed, dropped, and failed; a gauge of in-flight work; sometimes latency. The discipline is that metrics touched from multiple goroutines must be synchronized. The cheap, lock-free tool is the `sync/atomic` typed counter (`atomic.Int64`): `Add(1)` on each processed item, `Load()` to snapshot, with no mutex on the hot path. When each goroutine owns a private counter and the totals are merged only after `wg.Wait()`, no synchronization is needed at all, because the `Wait` establishes a happens-before edge between the goroutines' last writes and the merging read. The wrong design — several goroutines incrementing one plain `int` field — is a data race the detector will flag and an undercount in production. Reach for an atomic on a shared counter, or for per-goroutine counters merged after the barrier.

### Detecting Goroutine Leaks

A pipeline bug rarely panics; it leaks. A stage that forgets to select on cancellation, or a `Merge` that closes early, leaves goroutines parked on a channel operation that will never complete. The two tools that catch this are the race detector and a goroutine count. `go test -race` instruments every memory access and channel operation and reports the close-before-send and the unsynchronized-counter bugs directly; it is mandatory for any concurrent code and every exercise here runs under it. Leak detection is coarser: `runtime.NumGoroutine()` before and after a unit of work, with the expectation that the number returns to its baseline once the work and its cancellation have completed. Because goroutine teardown is asynchronous, a robust leak check polls the count for a short grace period rather than asserting it on the first read. The structural guarantee is stronger than the count, though: if `Shutdown` blocks on a `WaitGroup` that every worker decrements, then `Shutdown` returning is itself proof that no worker leaked.

## Common Mistakes

### Closing The Outbound Channel Twice

Wrong: `defer close(out)` plus a second explicit `close(out)` after the loop.

What happens: the deferred close runs after the explicit one and panics with "close of closed channel," crashing the program.

Fix: rely on `defer close(out)` alone. It runs once on every return path, including the cancellation branch, which is exactly the close-exactly-once contract.

### Closing A Channel You Do Not Own

Wrong: a downstream consumer calls `close(out)` on a producer's channel to make the producer stop.

What happens: the producer's next send panics with "send on closed channel," because sends on a closed channel always panic.

Fix: keep close ownership with the sender. To signal "stop" from the consumer side, close a separate `done <-chan struct{}` or cancel a `context.Context`; the producer selects on it and returns, and the producer still owns and closes its own channel.

### Adding Buffers To Hide A Leak

Wrong: `make(chan int, 10000)` instead of threading cancellation, to keep upstream goroutines from blocking when a consumer stops early.

What happens: the leak is merely masked until the buffer fills or the producer count changes, and the producer no longer paces to the consumer, so backpressure is gone. The Go blog explicitly calls this fragile.

Fix: pass `done` or `ctx` to every stage and select on it around every send. Use a small bounded buffer only to absorb bursts, never to size away a leak.

### Forgetting `wg.Add` Before Launching The Senders In A Fan-In

Wrong: starting the closer goroutine, or the per-channel sender goroutines, before `wg.Add(len(cs))`.

What happens: the closer's `wg.Wait()` can observe a zero count before any `Add` runs, close `out` while senders are still sending, and panic on the next send under `-race`.

Fix: call `wg.Add(len(cs))` on the calling goroutine, before the `go` statements, and start the closer after the senders. Then `Wait` cannot return early.

### Incrementing A Shared Counter Without Synchronization

Wrong: several worker goroutines doing `stats.Processed++` on one shared struct field.

What happens: a data race — lost increments and an undercount in production, flagged immediately by `go test -race`.

Fix: use an `atomic.Int64` and `Add(1)`/`Load()`, or give each goroutine a private counter and sum them only after `wg.Wait()`, which provides the happens-before edge that makes the read safe.

### Asserting An Exact Item Count After Cancellation

Wrong: a test that cancels mid-stream and asserts exactly N values were received before the stop.

What happens: the select between `out <- v` and `<-done` is resolved randomly when both are ready, so one extra value may slip through after cancellation. The test is flaky.

Fix: cancellation guarantees prompt termination and no leak, not an exact stop count. Assert that the pipeline drains and terminates (the receive loop ends without hanging), and bound the count with an inequality rather than pinning it to an exact value.

---

Next: [01-pipeline-core.md](01-pipeline-core.md)
