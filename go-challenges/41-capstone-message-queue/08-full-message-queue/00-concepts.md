# 8. Full Message Queue System — Concepts

Integrating every subsystem built in the earlier lessons into a single production-style broker is the hardest step in this capstone: independent state machines — partition logs, consumer groups, long-polling fetchers, durable offsets, retention — must share one lifecycle without data races, deadlocks, or resource leaks. This file is the conceptual foundation. Read it once and you will have everything you need to reason through each exercise, which build the broker piece by piece as independent, self-contained Go modules: a partition log, a consumer-group coordinator, the orchestrating broker, an end-to-end produce/consume/commit/replay flow with durable offsets, and a deterministic load generator.

## Concepts

### Composition over Inheritance: the Broker as an Orchestrator

The broker does not subclass anything. It holds concrete pointers to each subsystem and sequences their lifecycles. Construction initializes them in dependency order (partition logs and the offset store before the coordinator, all of them before any network listener). `Start` makes external state reachable. `Shutdown` closes them in reverse: stop accepting work, wake and drain in-flight requests, then close logs. Violating that order is the single most common integration bug: closing a partition log while a handler goroutine is still reading from it produces an `os.ErrClosed` panic path or silent data loss.

### The Lock Hierarchy: One Order, Always

The central concurrency invariant is a fixed lock-acquisition order. The broker's `mu` (an `RWMutex`) guards the `topics` map. Each `PartitionLog` carries its own `mu` plus a `sync.Cond`. The two locks are always taken in the same order — `Broker.mu` first, then `PartitionLog.mu` — which is what makes the system deadlock-free. A goroutine that needs to produce a message read-locks `Broker.mu` briefly to look up the partition, releases it, then locks `PartitionLog.mu` for the write. No method ever calls back into `Broker.mu` while holding a `PartitionLog.mu`. The consumer-group coordinator has its own independent mutex hierarchy and is never called while a partition lock is held.

### The Produce Path: an Append-Only Binary Log

Each partition is an append-only file with an in-memory index mapping a logical offset to a byte position in the file. A record is a fixed 24-byte header followed by the key and value bytes:

```text
Offset  Size  Field
     0     8  offset    (int64, big-endian)
     8     8  timestamp (int64 nanoseconds, big-endian)
    16     4  keyLen    (uint32, big-endian)
    20     4  valueLen  (uint32, big-endian)
    24  keyLen   key bytes
     ?  valueLen value bytes
```

The design decision that matters here is that *both* length fields live in the fixed header, before either variable-length field. A tempting but broken alternative writes `keyLen`, then the key, then `valueLen`, then the value — interleaving a length field after a variable field. A decoder that reads a fixed 24-byte header then expects `valueLen` at byte 20 disagrees with that encoder the moment a key is non-empty, and the bug hides until the first record with a real key. Keeping both lengths in the header means the decoder reads exactly 24 bytes, learns both sizes, and then reads two variable slices with no further parsing.

Appending locks `PartitionLog.mu`, records the current file size as the index entry's byte position, writes the encoded record, increments `nextOff`, and broadcasts on the condition variable to wake any long-polling fetcher. Reads use `os.File.ReadAt`, not `Seek`+`Read`: `ReadAt` does not move the shared file cursor, so reads and writes interleave safely with no coordination beyond holding the lock long enough to consult the index.

### The Consume Path: Long-Polling with a Condition Variable

A fetch that finds no new messages at the requested offset does not return immediately; it waits. The pattern uses `sync.Cond` tied to `PartitionLog.mu`:

1. Acquire `PartitionLog.mu`.
2. Check for messages at the offset. If present, return them.
3. Call `cond.Wait()`, which atomically releases the mutex and suspends the goroutine.
4. On wake — from a producer's `Broadcast`, from the broker stopping, or from context cancellation — re-check.

Context cancellation is wired in by a watcher goroutine that broadcasts the condition when `ctx.Done()` closes. The watcher must acquire `PartitionLog.mu` before it broadcasts. That detail closes a missed-wakeup race: if the watcher could broadcast in the window after the fetcher checked `ctx.Err()` and before it called `Wait()`, the broadcast would be lost and the fetcher would block forever. Holding the mutex forces the broadcast to land either before the fetcher's check or after it is genuinely parked inside `Wait()`. With `defer cancel()` on the derived context, the watcher goroutine exits promptly when the fetch returns for any reason, so there is no goroutine leak.

`Broadcast` rather than `Signal` is mandatory when more than one fetcher may wait on the same partition: `Signal` wakes exactly one goroutine, leaving the rest blocked indefinitely.

### Consumer Group Coordination and Rebalancing

The coordinator maintains per-group membership and a per-group map of committed offsets keyed by topic-partition. It is intentionally decoupled from the partition logs: a consumer commits the offset of the last record it processed, the coordinator stores it, and on restart a consumer fetches the committed offset and resumes from there. This two-phase loop — fetch offset, consume, commit offset — is exactly how Kafka achieves at-least-once delivery. Exactly-once requires the consumer to make processing idempotent or to commit inside the same transaction as its side effects.

Partition assignment uses a deterministic range strategy. Sort the member IDs, then divide the partitions as evenly as possible: with `n` partitions and `m` members, member `i` (in sorted order) receives `n/m` partitions, and the first `n%m` members each receive one extra. Sorting the IDs is what makes the assignment stable and reproducible across machines. Every join or leave triggers a full recomputation — a rebalance — so that after membership settles, the union of all members' assignments covers every partition exactly once. The full incremental rebalance protocol used by real brokers (generation IDs, revocation rounds, sticky assignment) is deferred; the primitive here is the range computation plus a clean rebalance-on-change rule.

### Durable Offsets: Replay Across a Restart

Recovering the message log is only half of durability. If committed offsets live only in memory, a broker restart resets every group to the beginning and the next consumer reprocesses the entire log. A production broker persists committed offsets too — Kafka stores them in an internal `__consumer_offsets` topic. The end-to-end exercise builds the same idea in miniature: an append-only offset log where each commit appends one length-framed record `(group, topic, partition, offset)` and `Sync`s it. On startup the broker replays that file, last-write-wins per key, rebuilding the committed-offset map. A commit interrupted by a crash leaves a partial trailing record, which replay detects by a short read and ignores — the same tail-truncation discipline the message log uses. With durable offsets, the produce → persist → consume → commit → restart → replay cycle is correct: messages survive in the log, the committed offset survives in the offset store, and the replacement consumer resumes exactly where the previous one left off.

### Crash Recovery: Segment Replay and Tail Truncation

On open, a `PartitionLog` reads its segment from the beginning, decoding one record at a time, rebuilding the in-memory index and resetting `nextOff` to one past the highest offset seen. A truncated final record — a partial write that the kernel buffered but never fully flushed before the crash — is detected by a short `ReadAt` returning `io.EOF` (or `io.ErrUnexpectedEOF`) partway through a record, at which point the file is truncated back to the last complete record. This is the minimal write-ahead-log recovery pattern: the log itself is the source of truth.

One subtlety governs `nextOff`. It must be set to `max(offset)+1` over the recovered records, not to `len(index)`. If records were ever removed from the front of a segment (compaction), the count and the highest offset diverge, and seeding `nextOff` from the count would reassign offsets that already exist. Tracking the maximum offset keeps assignment monotonic.

### Observability: Metrics as a First-Class, Lock-Free Concern

Operational counters — messages produced, messages consumed, bytes in, bytes out, active connections — use `sync/atomic` exclusively: no mutex, no allocation on the hot path. An HTTP admin endpoint renders them in the Prometheus text exposition format directly from Go fields, without importing a client library. For production the client library is preferable, but generating the text by hand shows exactly what a scraper reads: a `# HELP` line, a `# TYPE` line, and a `name value` sample per metric.

### Throughput and Latency: What Is Actually Being Measured

It is tempting to advertise a single headline number such as "500,000 messages per second." That number is meaningless without the hardware, the value size, the partition count, the fsync policy, and whether the race detector is on, and quoting it as a guarantee is dishonest. What *is* stable and worth measuring is the *count* of work the system performs under a fixed load: a load generator that produces a known number of records across a known number of partitions and then consumes them must report exactly those counts back, deterministically, on any machine. Counts are reproducible; wall-clock timings are not. The benchmark exercise therefore drives a deterministic workload and asserts on counts (produced equals consumed equals the planned total), while a standard Go `Benchmark` function is provided separately for anyone who wants to measure real timings on their own hardware. The qualitative truth to internalize: produce throughput is bounded by how often you fsync (batching amortizes it), fetch throughput is bounded by how fast you can copy from the OS page cache, and long-poll latency is bounded by how quickly a producer's `Broadcast` wakes a parked fetcher.

## Common Mistakes

### Interleaving a Length Field After a Variable Field

Wrong: encoding `keyLen`, then the key bytes, then `valueLen`, then the value, while the decoder reads a fixed 24-byte header expecting both lengths up front. The two agree only when the key is empty, so every test that uses a `nil` key passes and the bug ships. Fix: put `keyLen` and `valueLen` both inside the fixed header, before either variable-length field, so the decoder reads 24 bytes and then two sized slices.

### Closing a Subsystem While a Handler Still Uses It

Wrong: calling `PartitionLog.Close()` during `Shutdown` while an in-flight fetch goroutine holds `pl.mu` and is parked in `cond.Wait()`. Fix: `Shutdown` closes the `stopped` channel, broadcasts every partition's condition so parked fetchers wake and return, drains the wait group, and only then closes the logs. Because closing a log also acquires `pl.mu`, it naturally blocks until the last fetcher releases the lock.

### Acquiring Broker.mu While Holding PartitionLog.mu

Wrong: a method that holds `pl.mu` then calls a helper that takes `b.mu`. If another goroutine holds `b.mu` and is waiting for `pl.mu`, both deadlock. Fix: always take `b.mu` first to look up the partition, release it, then take `pl.mu`. Never re-enter `b.mu` while holding a partition lock.

### Missed Wakeup in the Long-Poll Watcher

Wrong: a context-watcher goroutine that calls `cond.Broadcast()` without first acquiring `pl.mu`. The broadcast can land in the gap between the fetcher's `ctx.Err()` check and its `cond.Wait()`, and is then lost, leaving the fetcher blocked until a later producer happens to wake it. Fix: the watcher locks `pl.mu`, broadcasts, unlocks. Use `Broadcast`, never `Signal`, when multiple fetchers may wait.

### Seeding nextOff from the Index Length

Wrong: after recovery, setting `nextOff = len(index)`. If any front records were ever removed, the count is smaller than the highest offset plus one, and the broker reassigns existing offsets. Fix: track `nextOff = max(offset)+1` across recovered records.

### Quoting a Fabricated Throughput Number

Wrong: claiming a fixed messages-per-second figure as if it were a guarantee. Fix: assert on deterministic counts (produced equals consumed equals planned), and leave real timing to a `Benchmark` the reader runs on their own hardware.

---

Next: [01-partition-log.md](01-partition-log.md)
