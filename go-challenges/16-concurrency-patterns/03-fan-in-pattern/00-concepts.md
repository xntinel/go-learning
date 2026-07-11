# Fan-In Pattern — Concepts

Fan-in is the dual of fan-out: where fan-out spreads one stream across many workers, fan-in multiplexes many input channels back onto a single output channel. It is the join step of almost every concurrent pipeline — the point where the results of N parallel producers become one stream a single consumer can range over. This file is the conceptual foundation. Read it once and you will have everything you need to reason through the three exercises, which build from the canonical merge to two senior, real-world fan-ins: an ordered k-way merge across database shards with per-source error propagation, and a scatter-gather search aggregator bounded by a deadline. Each exercise is its own self-contained Go module.

## Concepts

### Fan-In Is Multiplexing N Channels Into One

A fan-in function takes a slice (or variadic) of `<-chan T` and returns a single `<-chan T`. Internally it starts one goroutine per input channel; each goroutine copies values from its input to the shared output and exits when its input closes. The function returns the output channel immediately, before any value has been forwarded, so the caller can start ranging over it while the producers are still running. The goroutines outlive the call and keep forwarding until every input is drained.

This shape is worth internalizing because it is the same regardless of what the channels carry. The canonical version carries `int`; the shard merge carries records; the scatter-gather version carries search outcomes. What changes between them is the aggregation policy — arrival order versus key order, all-or-nothing versus best-effort-by-deadline — not the underlying "one goroutine per source, one shared sink" skeleton.

### The Close Must Wait For Every Writer

The single hardest invariant in fan-in is closing the output exactly once, and only after the last sender is done. A send on a closed channel panics, and closing a channel a second time also panics. If any input goroutine could close the output, the first one to finish would close it and every other goroutine would panic on its next send. So no input goroutine ever closes the output.

The standard-library answer, the one the Go blog's pipeline article uses, is a `sync.WaitGroup`. Each input goroutine calls `wg.Done` when its input drains. A separate, dedicated closer goroutine calls `wg.Wait` and then `close(out)`. That closer is the only code path that closes the output, so the close happens once, after every writer has stopped.

### `wg.Add` Must Run Before The Goroutines Start, Never Inside Them

A subtle ordering bug sinks naive implementations. If `wg.Add(1)` is called inside each input goroutine rather than `wg.Add(len(cs))` synchronously before the loop, the closer goroutine can run `wg.Wait` before any `Add` has executed, observe a zero counter, and close the output immediately — while the senders are still about to start. Every subsequent send then panics.

The mandatory order is: `wg.Add(len(cs))` runs synchronously on the calling goroutine, then the `go output(c)` calls spawn the senders, then `go func(){ wg.Wait(); close(out) }()` spawns the closer. Because the `Add` completes before the calling goroutine even reaches the `go` statements, the counter is correct before any goroutine that could call `Wait` exists. The race detector is what catches the broken ordering: without `-race` a missing-before-spawn `Add` is a flaky timing bug that passes most runs.

### Fan-In Loses Cross-Source Order (Unless You Restore It)

Basic fan-in preserves order *within* each source but not *across* sources. Two inputs emitting `1, 2` and `10, 20` produce the four values in whatever interleaving the scheduler picks, which differs run to run. Tests of a basic merge must therefore sort or set-compare, never assert a fixed sequence.

When you genuinely need a globally ordered output — for example merging already-sorted streams from database shards — you cannot get it by reading "whatever arrives next," because the value sitting in a fast source's channel may have a larger key than one still in transit from a slow source. You need a k-way merge: read one element ahead from every source, keep the candidate heads in a min-heap keyed by the ordering field, and always emit the global minimum. That turns N sorted streams into one sorted stream, and it is the heart of the second exercise.

### Per-Source Error Propagation

A toy merge carries only values, so a source has exactly two states: more data, or closed. A real shard or replica has a third: it can fail mid-stream — a connection drops, a query errors, a disk read fails. The merge must surface that failure rather than silently treating the source as merely finished, otherwise a partial result masquerades as a complete one.

The clean way to add an error channel is to make each source emit a tagged item that is *either* a value *or* a terminal error, then have the merge expose two outputs: the value channel and a separate error channel. The consumer ranges the value channel to completion, then reads the error channel once to learn whether the stream ended cleanly or was cut short. Make the error channel buffered with capacity one so the merge goroutine can deposit the error and exit without waiting for a reader, and so closing it after a clean run yields a nil error to the consumer. The policy choice — fail fast on the first error, or gather every source's error — is yours; the second exercise fails fast, which is the right default when a missing shard makes the whole result untrustworthy.

### Deadlines, Partial Results, and Why the Sink Must Be Buffered

The third real-world wrinkle is the deadline. When you scatter a query to many backends and gather the answers, you rarely wait for the slowest one — you take whatever arrived before a deadline and move on. A `context.Context` with a timeout expresses exactly this: the gather loop selects between receiving the next result and `ctx.Done()`, and on `ctx.Done()` it returns the partial set it has, counting the rest as timed out.

This introduces a correctness trap that the race detector and a leak test expose. When gather returns early, the slow backends are still running. If their result channel were unbuffered, every abandoned goroutine would block forever on its send — no one is receiving anymore — and leak. The fix is to buffer the result channel to the number of sources, so every producer, fast or slow, can always complete its send and exit even after gather has stopped listening. Buffering the sink to the producer count is the standard way to make a best-effort fan-in leak-free: it decouples "the consumer stopped caring" from "the producer can finish."

### Nil Channels Block Forever — They Do Not "Finish"

A myth worth killing explicitly: a `nil` channel does not behave like a closed one. Receiving from a closed channel returns immediately with the zero value and `ok == false`, which is why `for v := range closedCh` terminates. Receiving from a `nil` channel blocks forever — the Go spec is unambiguous — so `for v := range nilCh` never returns. Passing a `nil` channel into a fan-in is therefore not a harmless no-op: the input goroutine assigned to it blocks on the range forever, never calls `wg.Done`, the `WaitGroup` never reaches zero, and the output is never closed. The whole merge hangs and the goroutine leaks. Fan-in code that may receive untrusted inputs must reject or filter `nil` channels; it must never assume they will "just finish." (The blocking-forever property is occasionally useful on purpose: disabling a `select` case by setting its channel to `nil` is a real idiom — but that is the opposite of "terminates immediately.")

## Common Mistakes

### Closing The Output Channel From Inside A Worker

Wrong: each input goroutine ends with `defer close(out)`. What happens: the first goroutine to finish closes `out`, and every other goroutine panics on its next `out <- n`, or panics double-closing. Fix: input goroutines only call `wg.Done`; a single dedicated closer goroutine performs the one `close(out)` after `wg.Wait`.

### Calling `wg.Wait` Before `wg.Add`

Wrong: spawning the closer goroutine, or calling `wg.Add(1)` inside each worker, such that `Wait` can observe a zero counter before the workers are counted. What happens: `Wait` returns immediately, the output closes, and the still-starting senders panic. Fix: `wg.Add(len(cs))` runs synchronously before any goroutine spawns; only then spawn the workers, then the closer.

### Assuming Cross-Source Order Is Preserved

Wrong: asserting `Merge(Generate(1,2), Generate(3,4))` emits `1,2,3,4` in that exact order. What happens: scheduling decides the interleaving; the assertion is flaky. Fix: for a basic merge, sort or set-compare in the test. For a genuinely ordered result, do a heap-based k-way merge instead of plain fan-in.

### Treating A Failed Source As A Finished One

Wrong: a source that errors mid-stream just closes its channel, indistinguishable from a clean end. What happens: the consumer believes it received the complete result when it actually got a truncated one. Fix: carry a terminal error item (or a dedicated error channel) so the consumer can tell a clean end from a cut-short one.

### An Unbuffered Sink In A Best-Effort Gather

Wrong: scatter-gather with a deadline writes results to an unbuffered channel and returns early on `ctx.Done()`. What happens: the abandoned slow producers block forever on their sends and leak. Fix: buffer the result channel to the number of producers so every send can complete regardless of whether the consumer is still receiving.

### Passing A Nil Channel Into Fan-In

Wrong: assuming `Merge(nil, realCh)` ignores the `nil` input. What happens: the goroutine ranging the `nil` channel blocks forever, `wg.Done` is never called, and the output never closes — the merge deadlocks and leaks. Fix: never pass `nil` channels into a merge; filter them out or reject them at the boundary.

Next: [01-merging-channels.md](01-merging-channels.md)
