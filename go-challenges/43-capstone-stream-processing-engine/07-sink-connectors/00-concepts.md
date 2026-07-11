# 7. Sink Connectors — Concepts

Writing results out of a stream pipeline is harder than it looks. The bytes reach their destination — a file, a TCP socket, an HTTP endpoint, a key-value store — but the delivery guarantee depends entirely on what happens when something fails mid-write. A sink that loses an acknowledged record is a data-loss bug; a sink that re-emits a record on retry is a double-counting bug. This file is the conceptual foundation for the chapter: read it once and you will have everything you need to reason through the exercises, which build sink connectors one independent, self-contained Go module at a time — a file sink with two-phase commit, a reconnecting TCP sink, a batching HTTP sink with idempotency keys, an idempotent keyed-upsert sink, a dead-letter sink with poison-pill isolation, and a transactional coordinator that wires two-phase commit to checkpoint barriers.

## Concepts

### The Sink Lifecycle and Ownership

Every sink shares the same four-method lifecycle: `Open` then `Write` (zero or more times, possibly interleaved with `Flush`) then `Close`. `Open` allocates resources — file handles, TCP connections, background goroutines — and is where a sink also performs crash recovery, such as cleaning up stale temp files from a previous run. `Write` either buffers records or sends them immediately. `Flush` forces buffered data out. `Close` flushes whatever remains, releases every resource, and must be safe to call exactly once even if `Open` never succeeded. Two contracts make a sink composable: `Close` is an implicit flush (a record written but never explicitly flushed must still reach the destination on `Close`), and every long-running operation respects a `context.Context` so the pipeline can shut down promptly instead of blocking on a dead network path.

The ownership rule that trips people up is the background goroutine. A sink that flushes partial batches on a timer starts a goroutine in `Open`; `Close` must cancel that goroutine's context and then *wait for it to exit* before returning. A `Close` that cancels but does not join leaves a goroutine running that can flush stale data, race a freshly constructed sink, or prevent the process from exiting cleanly.

### Delivery Guarantees: At-Least-Once, At-Most-Once, Exactly-Once

There are three delivery guarantees, and they are defined by what a retry does.

At-most-once never retries: a record is sent once, and if the send fails the record is dropped. It is the cheapest and the only one that can lose data. Almost nobody wants it on purpose.

At-least-once retries until it succeeds: a record reaches the destination one or more times. A timeout during a write triggers a retry, and if the original write actually landed, the destination now holds a duplicate. This is the default for most sinks because it is simple and never loses data, and it is *correct* as long as the downstream effect is idempotent.

Exactly-once means each record's effect lands exactly once despite retries and crashes. It is never free. There are two fundamentally different ways to get it, and choosing between them is the central design decision of this chapter:

1. Make the write idempotent so that at-least-once delivery has an exactly-once *effect*. A keyed upsert (`PUT key = value`) is naturally idempotent: applying it twice leaves the same state. This needs no coordination with the engine — it is the cheapest path to exactly-once and the first one to reach for when the destination supports keyed writes.
2. Make the write transactional and coordinate the commit with the engine's checkpoints via two-phase commit. This works for destinations that cannot dedupe — an append-only file, an analytics table — and is the path that file and database sinks take.

### Two-Phase Commit and the Checkpoint Barrier

Two-phase commit (2PC) splits a write into a durable-but-invisible phase and an atomic make-visible phase, and ties the second phase to the engine's checkpoint protocol:

1. `PrepareCommit(checkpointID)` (Flink calls this `snapshotState`): flush the pending records to a staging file — `{path}.ckpt-{id}.tmp` — and fsync it. The data is now durable on disk but not yet at its final, visible location.
2. `Commit(checkpointID)` (Flink calls this `notifyCheckpointComplete`): `os.Rename` the staging file to its final committed name. On POSIX a rename within one filesystem is atomic, so the records appear all-at-once or not at all.

The protocol's correctness rests on the order of events around a checkpoint barrier. The engine injects a barrier into the stream; when the barrier reaches the sink, the sink pre-commits (phase one) and reports success to the checkpoint coordinator. Only once *every* operator has acknowledged the checkpoint does the coordinator broadcast "checkpoint N complete", which triggers each sink's `Commit` (phase two). The two phases bracket the moment the checkpoint becomes durable: anything pre-committed is recoverable, anything committed is visible, and the window between them is the only place a crash can leave work to finish.

The recovery rule closes the loop. On `Open` after a crash, a sink that finds a staging `.tmp` file knows phase one completed but phase two may not have. If the engine's checkpoint for that staging file *did* complete (it is recorded in the engine's own checkpoint state), the sink re-runs `Commit` — and because the commit is an idempotent rename, re-committing an already-committed transaction is a no-op. If the checkpoint did *not* complete, the staging file is aborted (deleted) and the engine replays those records from the last good checkpoint. Nothing half-written is ever committed; nothing committed is ever committed twice. This is exactly the protocol Apache Flink's `TwoPhaseCommitSinkFunction` implements, and the same one its file sink uses in streaming mode.

### Idempotent Writes: Keyed Upserts and Version Guards

When the destination is a key-value store, exactly-once collapses into a much simpler property. A keyed upsert — write `value` at `key`, overwriting whatever was there — is idempotent: delivering the same record twice produces the same row. At-least-once delivery plus idempotent upserts equals exactly-once *state*, with no two-phase commit and no checkpoint coordination at all.

There is one subtlety that separates a toy from a correct implementation: reordering. At-least-once delivery can re-deliver an *old* version of a key after a newer one has already landed (a retry of an early message arrives late). A blind upsert would let the stale value clobber the fresh one. The fix is a version guard: every record carries a monotonically increasing per-key version, and the sink installs an incoming write only if its version is greater than or equal to the stored version. A strictly higher version wins; an equal version is a harmless exact duplicate; a lower version is stale and rejected. This is last-writer-wins-by-version, and it makes the sink correct not just under duplication but under reordering — the two failure modes at-least-once delivery actually produces.

### Batching and the Dual-Trigger Flush

Writing one record per I/O call is slow: each call pays the full syscall or network round-trip. A batching layer accumulates records and flushes when *either* of two conditions is met first:

- Batch size: the accumulator reaches `N` records. This trigger fires synchronously inside `Write`, so it needs no extra goroutine.
- Flush interval: a background ticker fires every `D`, draining whatever has accumulated. This handles low-volume streams where the batch would otherwise never fill.

Both triggers together bound the two costs that matter. The size trigger bounds memory (the batch can never exceed `N`); the interval trigger bounds latency (a record waits at most `D` before it ships). Size-only batching has unbounded latency at low volume — a single record can sit forever waiting for the batch to fill. Interval-only batching has unbounded memory at high volume — a burst can grow the batch without limit between ticks. Production sinks always run both.

### Idempotent HTTP Writes

An HTTP `POST` that times out may already have been processed by the server; the response was simply lost. Retrying re-sends the batch, and without a defense the server double-processes it. The standard defense is the `Idempotency-Key` request header: the sender assigns a unique, stable key per logical batch, and the receiver records which keys it has already applied and ignores repeats. The critical detail is that the key must be stable across *all retries of the same batch*. Derive it from a monotonically increasing batch sequence number, never from random data — a fresh UUID per attempt defeats the entire mechanism, because the server sees each retry as a brand-new batch.

### Reconnection and Cancellable Exponential Backoff

A TCP connection can break at any time. The correct reconnection pattern is: detect the write error, close the broken connection, wait a backoff duration, then redial; on success resume, on failure double the backoff (capped at a maximum) and retry. The backoff must double — a fixed retry interval hammers a struggling server and can prevent it from recovering — and it must be capped so it does not grow without bound.

The wait must be *cancellable*. Writing `time.Sleep(backoff)` is a bug: when the pipeline shuts down and cancels the context, a sleeping goroutine sleeps through the cancellation and the whole pipeline hangs until the sleep expires. The correct form selects on a timer and the context together, so cancellation wins immediately:

```go
select {
case <-time.After(backoff):
case <-ctx.Done():
	return ctx.Err()
}
```

### Dead-Letter Queues and Poison-Pill Isolation

Retrying forever assumes failures are transient. Some are not: a malformed record that the destination rejects every single time is a poison pill, and a sink that retries it forever blocks the entire pipeline behind one bad record. The dead-letter queue (DLQ) breaks the deadlock. After a bounded number of retries, the sink routes the failing record to a separate dead-letter destination and moves on, so a single poison record costs one quarantined record instead of total pipeline stall.

The refinement that distinguishes a senior implementation is poison isolation. When a *batch* fails — and many destinations reject a whole batch if any single record in it is invalid — naively dead-lettering the entire batch quarantines every healthy record alongside the one poison record. Binary-split isolation avoids that: a failing batch is bisected and each half retried independently; the healthy half succeeds, the poisoned half is bisected again, and the recursion terminates at size-one batches that are dead-lettered individually. Every healthy record is still delivered; only the genuine poison records are quarantined. Because the DLQ path and the retry path can both produce duplicates, a DLQ sink pairs naturally with an idempotent downstream — the two patterns compose.

### Holding a Lock Across I/O

The performance trap that shows up in every sink is holding a mutex across a blocking I/O call. If the same lock that serializes `Write` is held while the HTTP client does its round-trip, every concurrent `Write` blocks for the full network latency, and the sink's throughput collapses to one request at a time. The fix is to copy the batch out of the shared state under the lock, release the lock, and only then perform the I/O. The drain-under-lock-send-without-lock pattern is the standard shape for a concurrent batching sink.

## Common Mistakes

### Holding a Mutex During Blocking I/O

Calling the HTTP client or a buffered writer's `Flush` while still holding the `sync.Mutex` that serializes `Write` serializes the whole sink behind the network: every `Write` waits out a full round-trip. Copy the batch out under the lock, unlock, then do the I/O. The `drainBatch` pattern in the HTTP sink does exactly this.

### Using time.Sleep in a Retry Loop

`time.Sleep(backoff)` is not cancellable. When the context is cancelled during shutdown, the goroutine sleeps through it and the pipeline hangs until the sleep expires. Use `select { case <-time.After(backoff): case <-ctx.Done(): return ctx.Err() }` so cancellation wins immediately.

### Using a Random Idempotency-Key per Attempt

Generating a fresh UUID per `send` call gives every retry a different key, so the receiver cannot recognize the retry as a duplicate and processes the same batch multiple times. Derive the key from a batch sequence counter that advances once per *logical batch*, not once per network attempt, so every retry of one batch carries the same key.

### Not Cleaning Stale Temp Files on Open

Skipping the scan for `.ckpt-*.tmp` files in `Open` leaves a staging file behind after a crash during `PrepareCommit`. On the next run the file is neither committed nor deleted, and the sink's state diverges from the engine's checkpoint. `Open` must glob `{path}.ckpt-*.tmp` and remove the matches (or, for a transactional coordinator, reload them as recoverable pending transactions) so recovery is well-defined.

### Not Waiting for the Background Goroutine to Exit

A `Close` that calls `cancel()` and returns without `<-done` lets the background flush goroutine keep running. It may flush stale data, race a new sink instance, or hold the process open. `Close` must cancel the context and then block on the done channel the goroutine closes on exit.

### Blind Upserts Without a Version Guard

Treating every delivery as the newest value lets a late re-delivery of an old version overwrite a newer stored value. Carry a per-key version and install a write only when its version is greater than or equal to the stored one, so reordering — not just duplication — is handled.

### Dead-Lettering a Whole Batch for One Poison Record

When a destination rejects a batch because one record is invalid, quarantining the entire batch loses every healthy record with it. Bisect the failing batch and retry the halves so only the genuinely poisoned records reach the dead-letter destination.

---

Next: [01-file-sink-two-phase-commit.md](01-file-sink-two-phase-commit.md)
