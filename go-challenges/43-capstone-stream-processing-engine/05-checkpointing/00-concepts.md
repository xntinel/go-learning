# 5. Checkpointing — Concepts

Checkpointing is what turns a stream processing engine from a fragile, stateless pipe into a fault-tolerant system that survives a crash without losing or duplicating records. The hard part is capturing a consistent snapshot of all operator state across a concurrent pipeline without pausing the stream. This file is the conceptual foundation for the whole lesson: the Chandy-Lamport barrier algorithm, barrier alignment for fan-in operators, atomic and durable state persistence, the coordinator's checkpoint lifecycle, recovery, and the two storage refinements a production engine eventually needs — a pluggable state-backend abstraction and incremental (delta) snapshots. Read it once and every exercise becomes a matter of turning one of these ideas into code.

## Consistent Snapshots Without Stopping

A naive approach to fault tolerance takes a full stop: pause all operators, flush all buffers, write state to disk, then resume. This is a global barrier and it defeats the purpose of streaming, because the latency spike while everything is frozen is unacceptable in production.

The Chandy-Lamport algorithm avoids stopping by using marker messages called barriers. The coordinator injects a barrier into each source operator; the barrier travels through the pipeline alongside normal records. When an operator receives a barrier, it takes a snapshot of its local state (window contents, accumulator values, watermark positions) and forwards the barrier to its output channels. The snapshot is taken at the exact point where the barrier was encountered, which corresponds to a consistent global cut: every record that arrived before the barrier has been processed, and no record that arrived after it has yet been counted.

The key invariant is that messages within a single channel are ordered, so a barrier on channel C cleanly separates "records processed before this checkpoint" from "records not yet processed". All channels must deliver their barriers before the snapshot is declared complete.

## Barrier Alignment for Fan-In Operators

A simple map or filter operator has one input channel; the barrier passes straight through. An operator with multiple input channels — a join, or an aggregation over several partitions — must align barriers before it snapshots.

When lane 0 delivers a barrier but lane 1 has not, the operator cannot snapshot yet: records from lane 1 that arrive before its barrier belong to the current checkpoint and must be processed first. Records from lane 0 that arrive after its barrier belong to the next checkpoint and must be held back.

The alignment policy follows from that: once a lane delivers its barrier, buffer subsequent records from that lane in memory; keep processing records from lanes that have not yet delivered their barrier. When every lane has delivered the same barrier, flush the buffers in lane order, emit the barrier downstream, and then snapshot. Only after the snapshot is the barrier forwarded.

This buffering is bounded by the volume of records that arrive on fast lanes while slow lanes catch up. In practice checkpoint intervals are seconds to minutes; a production engine imposes a buffer-size limit and aborts a checkpoint if it is exceeded, because an unbounded aligner is a memory leak waiting for a slow lane.

## Atomic and Durable State Persistence

Operator state is serialized to bytes (JSON or gob) and written to a state backend. Two distinct properties matter, and they are easy to conflate.

Atomicity means a partial write must never appear as a valid snapshot. A process crash mid-write must leave either the old file or the new file, never a half-written one. The POSIX rename trick gives this on a local filesystem: write the full serialized state to a temporary file in the same directory as the destination, then call `os.Rename(tmp, dst)`. On POSIX filesystems rename is atomic — the destination ends up pointing at the old inode or the new one, never partial bytes. Placing the temp file in the same directory matters: a rename across filesystem boundaries (different mount points) is not atomic and fails with `EXDEV`. `os.CreateTemp(dir, pattern)` in the target directory guarantees same-filesystem placement.

Durability is the stronger property: the write must survive a power loss, not just a clean process exit. Rename is atomic with respect to concurrent readers even without fsync, but after a power cut the kernel may still hold the rename — and the file's data — only in its page cache. A crash-safe write therefore needs two `fsync` calls: one on the data file (so its bytes reach stable storage before the rename publishes it) and one on the parent directory (so the new directory entry created by the rename is itself durable). Skip the file fsync and a file can exist by name yet contain zeros; skip the directory fsync and the file can vanish entirely after a crash even though the rename "succeeded".

## The Checkpoint Lifecycle

The coordinator owns the full lifecycle:

1. Trigger: assign a monotonically increasing `CheckpointID`, record which operators must acknowledge, and inject a `Barrier` into every source.
2. Propagate: the barrier flows through the pipeline alongside records. Each operator snapshots its state on receipt, saves it, and calls `Acknowledge(id, operatorID)` on the coordinator.
3. Finalize: when all operators have acknowledged, the coordinator calls `MarkComplete`. The checkpoint is now durable and recoverable.
4. Prune: keep only the most recent N completed checkpoints and delete older ones to reclaim disk space.

The coordinator must tolerate slow operators: the barrier for checkpoint N+1 may be injected before checkpoint N completes. It therefore tracks several in-flight checkpoints concurrently in a map keyed by `CheckpointID`, each with its own per-operator acknowledgement set.

## Recovery and Exactly-Once Semantics

After a failure and restart, the engine finds the highest completed checkpoint with `LatestCheckpoint`, loads each operator's saved state with `LoadState`, and calls `operator.Restore(bytes)`. It then tells each source to seek to the offset recorded in the checkpoint (a source operator includes its read offset in its own snapshot) and resumes.

Records between the checkpoint position and the crash point are replayed from the source. The reason recovery does not double-count them is that `Restore` *sets* the operator's state to the snapshot value rather than replaying the pre-checkpoint records: a counter restored at 7 and then fed 4 replayed records ends at 11, not 18. If the sink is idempotent (it writes with a unique output key) or the engine deduplicates by output sequence number, replay produces no duplicate output and the result is exactly-once. Without idempotent sinks the guarantee is at-least-once: no records are lost, but some may be reprocessed.

## Pluggable State Backends

The operators should not know whether their state lives in memory, on a local disk, or in object storage. Hiding the store behind a `StateBackend` interface — `SaveState`, `LoadState`, `MarkComplete`, `LatestCheckpoint`, `Close` — lets a pipeline run against a fast in-memory backend in unit tests and a durable file (or remote) backend in production without touching operator code. The discipline that keeps the two implementations honest is a single conformance test suite run against every backend through the interface, so the in-memory and file backends are proven to behave identically.

One subtlety the in-memory backend must respect is byte isolation: `SaveState` must store a private copy of the state slice. If it keeps the caller's slice, a later mutation of that slice silently corrupts the stored snapshot — a bug the file backend cannot have, because it serializes through the filesystem.

## Incremental Snapshots

Operator state can grow to gigabytes. Writing all of it on every checkpoint is wasteful when only a few keys changed since the previous one. An incremental checkpoint persists only the keys that changed since the last checkpoint (a delta) and writes a complete snapshot only periodically. Recovery rebuilds state by loading the most recent full snapshot at or before the target checkpoint and then replaying the deltas forward in order, where a later delta's value for a key overrides an earlier one.

The periodic full snapshot is not optional: without it, recovery would have to replay every delta since the dawn of the job, and a corrupt or missing early delta would make recovery impossible. The full-snapshot interval is the knob that trades write cost (full snapshots are large) against recovery cost (more deltas to replay). This is exactly how production engines such as Flink's RocksDB state backend implement incremental checkpoints.

## Common Mistakes

Creating the temp file outside the destination directory. `os.CreateTemp("", ...)` puts the temp file in the system temp dir, typically a different filesystem from the state directory, so the subsequent `os.Rename` fails with `EXDEV: invalid cross-device link`. Pass the target directory as the first argument so the temp file and destination share a filesystem and the rename is atomic.

Confusing atomic with durable. Temp-then-rename is atomic but not durable: after a power loss the data or the rename may still be only in the page cache. A write that must survive a crash needs an fsync on the file before the rename and an fsync on the parent directory after it. Omitting either one is a latent data-loss bug that no test on a cleanly-exiting process will ever reveal.

Sending on the output channel while holding the alignment lock. If the aligner sends to its output channel while holding its mutex, and the consumer of that channel calls `Push` before draining, the send blocks with the lock held while `Push` blocks waiting for the lock — a deadlock. Collect the events to emit under the lock, release the lock, then send.

Forgetting to reset alignment state after a checkpoint completes. After every lane delivers its barrier and the buffers are flushed, the per-lane `delivered` flags, the arrived count, and the current checkpoint ID must be reset. If they are not, the next checkpoint's barriers are rejected as duplicates or mismatches and alignment is permanently stuck.

Marking a checkpoint complete before all operators have acknowledged. `Acknowledge` must iterate every operator ID and return early while any one is still outstanding. Calling `MarkComplete` on the first acknowledgement produces a checkpoint that recovery will read with some operators snapshotted and others not — an inconsistent global state that is worse than having no checkpoint at all.

Using `os.IsNotExist` instead of `errors.Is(err, os.ErrNotExist)`. `os.IsNotExist` does not unwrap error chains; `errors.Is` does. Since Go 1.13 the `os` and `fmt` wrapping conventions mean `errors.Is(err, os.ErrNotExist)` is the correct, chain-aware test, and it keeps working when the error is wrapped with `%w`.

Re-counting replayed records on recovery. If `Restore` replays the pre-checkpoint records instead of setting the state to the snapshot value, a counter at 7 that recovers and then sees 4 replayed records ends at 18 rather than 11. Restore must load the snapshot value directly; the whole point of the checkpoint is to avoid reprocessing what it already captured.

Next: [01-atomic-state-backend.md](01-atomic-state-backend.md)
