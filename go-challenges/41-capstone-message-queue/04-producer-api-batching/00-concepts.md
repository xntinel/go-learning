# 4. Producer API with Batching — Concepts

A producer that sends one message per network call pays a full round-trip penalty for every record. At 100 microseconds per round-trip that caps throughput at 10,000 messages per second regardless of how much the broker can absorb. The fix is batching: accumulate records destined for the same topic-partition, then send one network request for the whole group. This chapter builds the moving parts of a real producer as independent, self-contained Go modules: an asynchronous accumulator with two flush triggers and futures, a retry path with exponential backoff and jitter, batch-level compression, and an idempotent producer whose broker deduplicates retries so that a lost acknowledgment never turns into a duplicated record. Read this file once and you will have the reasoning needed for every exercise.

## Concepts

### Why Batch at All: Amortizing the Round-Trip

A network round-trip has a fixed cost paid once per request no matter how many bytes ride along. If each `Send` blocks on its own request, the producer's ceiling is `1 / round_trip` messages per second, and a faster broker cannot help because the producer is the bottleneck. Batching changes the unit of work from one record to one request carrying many records. With a batch of `B` records the per-record overhead falls to `round_trip / B`, and throughput rises toward the point where the link bandwidth or the broker, not the latency, is the limit. The cost is latency: a record that arrives first now waits for the batch to fill or for a timer to fire before it ships. The whole design is a controlled trade of a small, bounded latency increase for a large throughput gain, and the two flush triggers below are the knobs that bound that latency.

### The Accumulator-Sender Split

The producer has two concurrent roles that must never block each other. The accumulator is the `Send` call path: it holds a short mutex, appends a record to the current in-progress batch for that topic-partition, and, when the batch becomes full, hands the finished batch off. The sender is a background goroutine that reads finished batches and dispatches them to the broker, possibly several at once up to a concurrency bound.

The only shared channel between the two roles is a buffered channel of ready batches (`readyCh`). The mutex protects the accumulator's map of in-progress batches; the sender never touches that map, so the two paths rarely contend. The single most important discipline here is that the mutex is never held while sending on `readyCh`. If a goroutine held the lock and the channel were full, no other `Send` could make progress and the sender could not drain, producing a deadlock. The correct shape is: under the lock, decide which batch (if any) is now ready and store it in a local variable; release the lock; then send that batch on the channel.

### Two Flush Triggers

A batch becomes ready in exactly two ways, and a healthy producer arms both.

The size trigger fires inside `Send`: each append adds `len(value)` to the batch's running byte count, and when that count reaches `MaxBatchBytes` the batch is removed from the map and pushed to the channel immediately. Size flushing is what keeps a high-throughput stream from buffering unbounded memory.

The time trigger fires from a linger timer. When the first record of a new batch is added, the producer arms a `time.AfterFunc(linger, ...)`. If the batch never fills, the timer's callback removes it from the map and pushes it, bounding the latency of a slow stream to at most `LingerMs`. The two triggers race: a timer may fire at the same instant a size flush removes the same batch. The resolution is that both paths take the mutex and both check whether the batch is still in the map; whoever deletes it first wins, and the loser becomes a harmless no-op. Linger here is the same idea as Kafka's `linger.ms` and Nagle's algorithm in TCP: wait a little to coalesce, but never wait longer than a fixed bound.

### Clean Shutdown: t.Stop and the Timer WaitGroup

Shutting down without losing a record or panicking on a closed channel is the subtlest part of the whole design, and it turns entirely on the contract of `time.Timer.Stop`. `Stop` returns `true` if it stopped the timer before the callback could run (the callback will never run), and `false` if the timer had already fired (the callback is running or queued and will run). A second fact matters just as much: a callback that has already begun cannot be stopped, so `Stop` returning `false` means a goroutine is, right now, about to send on `readyCh`.

To shut down safely the producer tracks in-flight callbacks with a `sync.WaitGroup`. It calls `Add(1)` before arming each timer, and the callback calls `Done` from a `defer`. Every place that calls `Stop` inspects the return value: if `Stop` returned `true`, the code calls `Done` itself, because the callback that would have called it will never run. `Close` then runs in a fixed order: mark closed so new sends are rejected, flush all pending batches, `Wait` on the timer WaitGroup so no callback can still be queued, only then `close(readyCh)`, and finally `Wait` on the sender WaitGroup. Closing the channel before the timer WaitGroup drains is the classic bug: an already-fired callback wakes up, sends on the now-closed channel, and the program panics.

### Future: Exactly-Once Resolution

An asynchronous `Send` returns a `Future` instead of a result. A future wraps a buffered channel of capacity one. `resolve` performs a non-blocking send (`select { case ch <- r: default: }`), so the first result lands in the buffer and any later resolve is silently dropped; the producer therefore never blocks while resolving and never panics on a double resolve. `Get` selects over the result channel and a `time.After` timeout. `OnComplete` spawns one goroutine that reads the single result and invokes a callback. The contract is one future per `Send`, appended to exactly one batch, and `resolveAll` walks that batch's futures once, giving record `i` the offset `baseOffset + i`. Because the channel is buffered to one, the resolving goroutine and a waiting `Get` can never deadlock against each other.

### Retry with Exponential Backoff and Jitter

A transient broker error (a closed connection, a timeout, a not-leader response) should be retried; a permanent one (message too large, unknown topic, authentication failure) should not. The retry path retries up to `Retries` times and, before attempt `n` counted from one, sleeps for an exponentially growing backoff with random jitter:

```
base   = RetryBackoffMs * 2^(n-1)
sleep  = base * (0.75 + rand*0.5)      // base +-25%
```

The exponential growth keeps a struggling broker from being hammered; the jitter is the part people skip and regret. Without it, every producer that hit the same outage retries at exactly the same instants, so the recovery moment sees a synchronized spike, the thundering herd, that knocks the broker back down. Spreading each retry across a +-25% window decorrelates the herd. Permanent errors are detected with `errors.Is` against the package's sentinel set and break out of the loop immediately rather than burning the full retry budget on an error that can never succeed.

### Idempotent Delivery: Deduplicating Retries

Retries create a correctness problem that batching makes worse. Suppose the producer sends a batch, the broker appends it durably, and then the acknowledgment is lost on the way back. The producer, seeing no ack, retries the same batch, and a naive broker appends it a second time. The stream now contains the records twice. This is the difference between at-least-once and exactly-once delivery, and it is solved with a sequence number rather than by trying to make the network reliable.

Each producer instance is assigned a Producer ID. For every topic-partition it keeps a monotonically increasing sequence counter, and a batch captures the counter value at creation time as its `BaseSeq`. The crucial discipline is that the producer increments the counter when a record is enqueued, not when it is acknowledged, so a retried batch carries the exact same `(ProducerID, BaseSeq)` it carried the first time. The broker keeps, per partition, the highest `BaseSeq` it has already committed for each Producer ID. When a batch arrives whose `BaseSeq` is less than or equal to what it has already seen, the broker recognizes a duplicate, skips the append, and returns the offset it assigned originally. The lost ack is now harmless: the retry is acknowledged from the dedup table and the record exists exactly once. This is precisely Kafka's idempotent producer (`enable.idempotence=true`), and the sequence must be stable across retries for it to work, which is why `BaseSeq` is frozen at batch creation.

### Compression

Compression is applied per batch, not per record, which is the second reason to batch: a batch of similar JSON or log lines shares dictionary entries, so the compression ratio on the batch is far better than compressing each tiny record alone. `Payload` serializes the records into one buffer and optionally runs it through a codec; the batch carries a codec tag so the broker knows how to decompress. The standard library's `compress/gzip` is the codec used here because it ships with Go and needs no dependency. The one rule that bites newcomers is that a `gzip.Writer` must be `Close`d, not merely flushed, before the bytes are valid: `Close` writes the gzip trailer (the CRC and length), and a reader rejects a stream without it. Production systems usually prefer Snappy or LZ4 for their far lower CPU cost at a slightly worse ratio, but those require third-party packages and the mechanics of choosing and tagging a codec are identical.

## Common Mistakes

### Holding the Mutex While Sending to the Channel

Wrong: calling `readyCh <- batch` while still inside the critical section guarded by `defer mu.Unlock()`. If the channel is full, the send blocks with the lock held; no other goroutine can `Send`, the sender cannot drain because draining a finished batch does not require the accumulator lock but the producers feeding new work are all stuck, and under sustained load the system wedges. Fix: under the lock, record the ready batch in a local; unlock; then send. Every module here follows that shape.

### Ignoring the Return Value of t.Stop

Wrong: calling `t.Stop()` and discarding the result, then expecting a timer WaitGroup to drain. When `Stop` returns `true` the callback will never run, so its `defer wg.Done()` will never execute, and `wg.Wait()` in `Close` blocks forever. Fix: wherever you stop a tracked timer, write `if t.Stop() { wg.Done() }`. When `Stop` returns `false`, do nothing, because the already-running callback will call `Done` itself.

### Closing the Ready Channel Before Timer Callbacks Drain

Wrong: `close(readyCh)` in `Close` without first waiting on the timer WaitGroup. A timer that fired microseconds earlier has a callback queued to send on the channel; closing it first makes that send panic. Fix: `Flush`, then `timerWg.Wait()`, then `close(readyCh)`, then `senderWg.Wait()`. Order is the whole correctness argument.

### Incrementing the Sequence Number on Acknowledgment Instead of on Enqueue

Wrong: bumping the per-partition sequence only after the broker acknowledges. A retry of an unacknowledged batch would then be assigned a fresh, higher `BaseSeq`, the broker would not recognize it as a duplicate, and the record would be committed twice, defeating the entire idempotence scheme. Fix: assign and freeze `BaseSeq` when the record is enqueued, so every retry of a batch carries the identical key.

### Forgetting to Close the gzip Writer

Wrong: calling `w.Flush()` (or nothing) and reading `buf.Bytes()` as the payload. The gzip trailer is written by `Close`, so a flushed-but-unclosed stream is truncated and every reader rejects it. Fix: `Close` the writer and check its error before using the buffer; treat `Close` as part of producing the bytes, not as cleanup.

---

Next: [01-batching-and-futures.md](01-batching-and-futures.md)
