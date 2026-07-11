# 1. Source Connectors — Concepts

A stream processing engine ingests data from many origins — log files, TCP sockets, HTTP endpoints, in-memory generators, replayable commit logs — but the rest of the pipeline must never know which one it is talking to. The hard problem is designing a `Source` interface clean enough to hide every per-origin concern (polling loops, per-connection goroutines, HTTP handlers, backpressure, offset tracking) behind a single lifecycle contract: open, emit records, shut down gracefully when the context is cancelled. This file is the conceptual foundation. Read it once and you will have everything you need to reason through each exercise, which builds the ingest layer piece by piece as independent, self-contained Go modules: a file tailer, a TCP listener, an HTTP receiver, a fan-in multiplexer, an in-memory generator, a replayable at-least-once source with offset commit, and a rate-limited token-bucket source.

## Concepts

### The Source Interface And Its Lifecycle Contract

Every source implements the same three methods. `Open(ctx)` starts the source's internal goroutines and returns two channels: a record channel that delivers `Record` values, and an error channel that delivers non-fatal errors the pipeline can log or meter without stopping. Both channels are closed exactly once, after all internal goroutines finish, so a consumer can `range` the record channel to drain it cleanly and know the source is done when the range loop exits. `Close()` signals intent to stop and blocks until the source has fully shut down. `Metrics()` returns a lock-free snapshot of the source's counters.

The canonical `Close` pattern cancels an internal context — created in `Open` and kept separate from the caller's context so the two compose independently — then waits for the source's `sync.WaitGroup` to reach zero before returning. The two-context design matters because the caller's context may be cancelled independently of `Close`: a downstream deadline expiring and an explicit `Close()` must both converge on the same shutdown logic without a deadlock and without closing a channel twice.

### Channel Backpressure: Block, Drop, Or Reject

A buffered channel between a source and the downstream operators is the simplest backpressure mechanism in Go. When the buffer is full there are exactly three things a producer can do, and the right choice is a property of the data, not of the code:

- Block (`ch <- r`): stall the producer until the consumer catches up. Correct for a durable event log where no record may be lost. The block must be guarded by the context (`select { case ch <- r: case <-ctx.Done(): return }`) so shutdown is never wedged behind a full buffer.
- Drop (`select { case ch <- r: default: }`): discard the record when the buffer is full. Correct for a metrics source where only the latest sample matters.
- Reject (return 429): push the backpressure back to the caller so it can retry. Correct for an HTTP source whose client speaks a protocol with an explicit "slow down" signal.

The file and TCP sources in this lesson use guarded blocking sends; the HTTP source uses a non-blocking send and returns 429 when full; the rate-limited source shapes the producer rather than the buffer. Mixing these up — a non-blocking drop on a durable log, or an unguarded block that ignores cancellation — is the single most common source bug.

### Goroutine Lifecycle And The WaitGroup Rule

Each source tracks every goroutine it spawns with a `sync.WaitGroup`, and the rule is mechanical: `wg.Add(1)` before `go func()`, `defer wg.Done()` as the first statement inside the goroutine. A separate goroutine performs `wg.Wait()` and only then closes the channels. This ordering is the whole game: closing a channel that another goroutine is still writing to is an unrecoverable runtime panic, so the channel may be closed only after every writer has provably returned. The `defer` guarantees `wg.Done` runs even if the goroutine panics or returns early, which is why `Add` goes outside the goroutine and `Done` is deferred inside.

A subtle re-open hazard appears in sources that can be opened, closed, and opened again (the replayable and rate-limited sources). A `WaitGroup` may be reused only after every prior `Wait` has returned, so those sources gate `Close` on a per-open `done` channel that the closer goroutine signals after it has finished, rather than letting `Close` and the closer race on the same `WaitGroup`.

### File Tailing And Position Tracking

A POSIX read past the end of a file returns zero bytes, not an error, which is exactly why `tail -f` works. The tail loop opens the file, seeks to the end with `io.SeekEnd` so it emits only content written after `Open` (not the file's existing history), then drives a `bufio.Scanner`. When `Scan` returns false with no error it has hit EOF; the loop sleeps for a poll interval and resets the scanner to pick up bytes appended in the meantime. Treating that EOF as fatal would turn an idle file into a dead source.

Position tracking is the first taste of the offset idea that the replayable source develops fully. The file source counts bytes read into an `atomic.Int64` so `Metrics` can report throughput without taking a lock; a production tailer persists a byte offset per file so that after a restart it resumes from where it left off rather than re-reading or skipping. The general principle is that a source's position is a small piece of durable state that, combined with the origin, fully determines what to deliver next.

### Fan-In With MultiSource

`MultiSource` starts each child source and merges their record channels into one. The pattern is the fan-in from the Go blog on pipelines, applied to a dynamic set of children: one forwarding goroutine per child reads that child's channel and re-sends each record onto the merged channel, guarded by the context; a single closer goroutine waits for all forwarders via the `WaitGroup` and then closes the merged channel exactly once. Because each child obeys the same lifecycle contract, the multiplexer does not care whether a child is a file, a socket, or a generator — it sees only `<-chan Record`. When every child drains and closes its channel, every forwarder returns, the `WaitGroup` hits zero, and the merged channel closes on its own, so a consumer ranging over the merged channel observes a clean end of stream.

### Delivery Semantics: At-Least-Once, Offsets, And Commit

A source's delivery guarantee is the contract for what happens across a failure. At-most-once delivers each record zero or one times: simple, but loses data on a crash. Exactly-once is the ideal and the hardest to build, requiring coordination between the source's position and the downstream sink. At-least-once is the pragmatic middle ground that most real pipelines target: every record is delivered one or more times, so the only failure mode is a duplicate, which a downstream idempotent operation can absorb.

At-least-once falls out of one rule: never advance the committed position until the consumer has acknowledged the record. The source delivers records tagged with a monotonic offset; the consumer processes a record and then calls `Commit(offset)`; on restart the source replays from the first uncommitted offset. If the consumer crashes after delivery but before its commit, those records are redelivered — the duplicate that "at-least-once" names. The commit point must move only forward; a stale or out-of-order acknowledgement must never rewind it, or already-committed records would be replayed forever. This is the model Kafka consumer offsets implement, and the replayable source in this lesson builds a minimal version of it.

### Generators, Bounded vs Unbounded, And Rate Limiting

Not every source reads I/O. A generator source produces records from a function — synthetic load for a benchmark, a fixed test fixture, a counter — and it crystallizes the difference between a bounded and an unbounded source. A bounded source has a natural end: when the generator signals exhaustion it returns, the channel closes, and a `range` over it terminates. An unbounded source never ends on its own; it runs until the context is cancelled, and the only way a consumer stops it is `Close`. The lifecycle contract is identical for both — the difference is purely whether the producing goroutine ever returns voluntarily.

Rate limiting shapes a source's output to a maximum sustained rate, smoothing a bursty origin into a steady stream the downstream can absorb. The token-bucket algorithm is the standard tool: a bucket holds up to `burst` tokens, refills one token every `1/rate` seconds, and a record may be emitted only when a token is available. A short burst up to the bucket capacity passes through immediately; sustained output is capped at the refill rate. In Go a token bucket is naturally a buffered channel of tokens fed by a `time.Ticker`, and the production-grade version of exactly this design lives in `golang.org/x/time/rate`.

## Common Mistakes

### Closing A Channel While A Goroutine Is Still Writing To It

Wrong: calling `close(records)` inside `Close()` directly while producer goroutines are still running. This panics with `send on closed channel` the instant a producer's next send lands. Fix: close the channel only from the single goroutine that has just returned from `wg.Wait()`, so every writer has provably finished. Every writer calls `wg.Done()` via `defer` and stops writing before `wg.Wait()` returns.

### Forgetting `defer wg.Done()` Or Calling `wg.Add` Inside The Goroutine

Wrong: putting `wg.Add(1)` inside the goroutine, or calling `wg.Done()` at the end of the function body instead of deferring it. The `Add`-inside form races with `wg.Wait()` and can let the closer run before the goroutine is counted; the non-deferred `Done` is skipped when the goroutine returns early or panics, leaking the count and hanging `Close` forever. Fix: `wg.Add(1)` before `go`, then `defer wg.Done()` as the first statement inside.

### Not Draining The Error Channel

Wrong: ignoring the `<-chan error` entirely. With an unbuffered error channel and no reader, the first error send blocks the producing goroutine forever and leaks it. Fix: always consume the error channel, even if only to log, or use a buffered error channel with non-blocking sends (the approach in this lesson) so excess errors are dropped rather than blocking a producer.

### Using A Non-Blocking Send For Records That Must Not Be Dropped

Wrong: `select { case ch <- r: default: }` for a durable log source where every record matters. Silent drops are invisible data loss that no test catches unless it asserts an exact count. Fix: use a context-guarded blocking send for durable sources; reserve the non-blocking drop for sources where the latest value supersedes older ones, and the explicit 429 reject for protocols whose clients can retry.

### Advancing The Commit Point Before The Consumer Acknowledges

Wrong: marking a record committed as soon as it is delivered, or letting a stale acknowledgement move the commit point backward. The first turns at-least-once into at-most-once — a crash mid-processing loses the in-flight record. The second replays already-committed records forever. Fix: commit only on explicit acknowledgement from the consumer, and make the commit point strictly monotonic so a duplicate or reordered ack is ignored.

### Reusing A WaitGroup Across Re-Opens Without A Barrier

Wrong: in a source that supports `Open` after `Close`, having both `Close` and the channel-closer goroutine call `Wait` on the same `WaitGroup`, then re-`Add` on the next `Open`. The re-`Add` can race the previous closer's `Wait`. Fix: gate `Close` on a per-open `done` channel the closer signals last, so the previous cycle is provably finished before the next `Open` touches the `WaitGroup`.

---

Next: [01-file-source.md](01-file-source.md)
