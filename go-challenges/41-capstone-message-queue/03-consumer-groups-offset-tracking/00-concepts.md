# 3. Consumer Groups and Offset Tracking - Concepts

Consumer groups are where a message queue stops being a log and starts being a distributed system. A group of consumers must divide a topic's partitions among themselves, each must record exactly which messages it has processed so a restart resumes in the right place, and the group must reorganize itself the moment any member crashes or a new one joins. Three problems compound: partition assignment (who reads what), durable offset tracking (how far each partition got), and failure detection (who is still alive). This file is the conceptual foundation. Read it once and you have what you need to reason through each exercise, which builds one independent, self-contained Go module at a time: the assignment strategies, the offset store and lag math, the full coordinator with heartbeats, the per-member rebalance delta, and the delivery-semantics consequence of when you commit.

## The Offset Is the Source of Truth

An offset is a monotonically increasing integer assigned to each message within a single partition. Partitions are independent: partition 0 has its own offset sequence starting at 0, and so does partition 1. A consumer group's committed offset for a partition is its high-water mark - every message at or below that offset is considered processed by the group.

Consumer lag for a partition is `latest - committed`, where `latest` is the highest offset written and `committed` is the last offset the group recorded as processed. The committed offset uses a sentinel of -1 to mean "nothing committed". This is deliberate: 0 is a valid offset (the first message), so a separate sentinel is the only way to distinguish "processed message 0" from "processed nothing". With `latest = 99` and `committed = -1`, lag is `99 - (-1) = 100`, the exact count of unprocessed messages at offsets 0 through 99. With `committed = 99`, lag is 0. A partition with nothing written reports lag 0.

The commit must be durable, separately from the data. Production systems store committed offsets in a dedicated place: Kafka uses an internal compacted topic named `__consumer_offsets`, keyed by `(group, topic, partition)`. The store in these exercises is an in-memory map with the same shape - `group -> partition -> offset` - and the same contract: a commit overwrites the previous high-water mark, and a fetch on an unknown key returns the -1 sentinel.

## Consumer Groups Divide Work Through Partition Assignment

A topic has N partitions. A consumer group has M consumers. Each partition belongs to exactly one consumer in the group at a time. The partition is the unit of parallelism: you cannot split one partition across two consumers in the same group, so a group with more consumers than partitions leaves the excess consumers idle. This is why partition count is a capacity-planning decision - it sets the ceiling on a group's parallelism for the life of the topic.

The coordinator holds a `generation` counter. Every rebalance increments it. A generation is the group's logical clock: a commit or an assignment is only meaningful relative to the generation that produced it. A consumer that fetched its assignment at generation 3 but tries to commit at generation 5 is committing against a superseded view of the world, and the coordinator can reject that commit because the generation does not match.

## Partition Assignment Strategies: Range, RoundRobin, Sticky

Range divides sorted partitions into contiguous blocks, one per sorted consumer. With 10 partitions and 3 consumers the first consumer receives 4 (the remainder) and the others receive 3 each. Range is simple and keeps consecutive partition numbers together, but the first consumer always bears any extra load, and across a topic with many consumer groups the first consumer of every group is consistently the busiest.

RoundRobin interleaves partitions across consumers in rotation: partition 0 to the first consumer, partition 1 to the second, wrapping with `partition mod M`. Distribution is maximally even, never off by more than one partition, but consecutive partitions land on different consumers, which costs locality on workloads where related keys cluster in a narrow partition range.

Sticky starts from the previous assignment, keeps every partition still owned by a surviving consumer, and redistributes only the partitions orphaned by departed consumers. When one of three consumers leaves, only that consumer's partitions move; the other two keep everything they had. This matters enormously for consumers that hold per-partition state - an in-memory aggregation, an open file handle, a warmed cache - because every partition that moves forces the new owner to rebuild that state from the committed offset forward. The right way to measure an assignor is not just "is it balanced" but "how many already-owned partitions changed hands", and on that metric sticky is dramatically better than recomputing from scratch (an "eager" rebalance), which can shuffle nearly every partition even when only one consumer left.

## The Rebalance Protocol: Membership Changes Pause Delivery

When a consumer joins or leaves, the coordinator transitions through a small state machine: STABLE (normal operation) -> PREPARING (membership changed) -> COMPLETING (new assignment computed and pushed) -> STABLE. In an in-process coordinator this transition is synchronous - the coordinator holds its lock while it computes the assignment and invokes listener callbacks. In a real distributed system, such as Kafka's group coordinator, these states span network round-trips and a configurable rebalance timeout. The synchronous model is correct for an in-process coordinator and teaches the state machine without the networking.

A rebalance is best understood as a per-consumer delta, not a global recomputation. Each surviving consumer needs to know exactly two things: which partitions it must stop owning (revoked) and which it must begin owning (assigned). The revoked set must be acted on first - the consumer commits its final offsets for those partitions before giving them up - because the partition's new owner will resume from the committed offset. If the old owner has not committed its last processed offset by the time the new owner starts, those messages are redelivered. This is the `RebalanceListener` contract: `OnPartitionsRevoked` is the consumer's last chance to commit, and `OnPartitionsAssigned` is where it rebuilds state for its new partitions.

The two designs for handling in-flight work during a rebalance mirror the two assignor philosophies. An eager rebalance has every consumer revoke all its partitions, then receive a freshly computed assignment - simple, but it stops the whole group and moves almost everything. A cooperative (incremental) rebalance computes the minimal delta and only revokes the partitions that actually change owner, so consumers that keep their partitions never stop processing them. Kafka moved to cooperative rebalancing for exactly this reason.

## Heartbeat-Based Failure Detection

Each consumer sends periodic heartbeats. If no heartbeat arrives within the session timeout, the coordinator declares the consumer dead, revokes its partitions, removes it from the group, and triggers a rebalance. The session timeout is a trade-off: shorter means faster failure detection but more false positives from a GC pause or a network hiccup, which trigger needless rebalances; longer means slower recovery from a real crash. Production defaults are a session timeout of 30-45 seconds against a heartbeat interval of about 3 seconds, so roughly ten heartbeats can be missed before a consumer is evicted.

Testing time-based logic without real sleeps requires abstracting the clock. A `Clock` interface with a single `Now()` method, backed by a fake whose time only advances when the test calls `Advance`, makes expiry deterministic: the test advances past the timeout, calls the expiry sweep, and asserts exactly the silent member was removed - no `time.Sleep`, no flakiness, instant runs even under the race detector.

## When You Commit Decides Your Delivery Guarantee

Commit timing is the single knob that selects a delivery guarantee, and it is the most consequential decision in this whole topic. Processing a message and committing its offset are two separate, non-atomic steps, and a crash can land in the window between them. The order of the two steps decides what that crash costs.

Commit before processing gives at-most-once delivery. If the consumer commits offset 5 and then crashes before processing message 5, the restart resumes at offset 6 and message 5 is silently lost. No duplicates, but no guarantee every message is handled.

Commit after processing gives at-least-once delivery. If the consumer processes message 5 and then crashes before committing, the restart resumes at offset 5 (the last committed offset plus one) and reprocesses message 5. Every message is handled at least once, but a crash in the window causes a duplicate.

Exactly-once is not a third commit ordering; it requires the offset commit and the processing side effect to land in one atomic transaction (Kafka's transactional producer with `read-process-write`, or an idempotent consumer that deduplicates by offset). That is out of scope here, but the architecture must not preclude it: keep the committed offset and the processed result in places that could be written together. The practical default for almost every system is at-least-once plus idempotent processing, because losing data (at-most-once) is usually worse than handling a duplicate.

## Common Mistakes

### Off-by-One in the Lag Formula

Wrong: `lag = latest - committed - 1`. This under-counts by one whenever something is committed and breaks badly at the `committed = -1` sentinel, reporting `latest` instead of `latest + 1`.

Fix: `lag = latest - committed`, clamped at 0. With `latest = 99` and `committed = -1`, lag is 100, the exact count of offsets 0..99. With `committed = 99`, lag is 0. Clamp negatives to 0 so a committed offset that briefly runs ahead of the recorded latest never reports a nonsensical negative lag.

### Committing the Offset Before Processing Is Durable

Wrong: calling commit immediately on fetch, before the processing result is stored. A crash between the commit and the processing silently loses the message - this is at-most-once delivery, usually by accident rather than by design.

Fix: commit only after the processing result is durable (written to a database, acknowledged by a downstream service, flushed to disk). That is at-least-once: on restart the consumer reprocesses from the last committed offset and only the messages after it. Choose the ordering deliberately, per the delivery guarantee you actually want.

### Not Committing in OnPartitionsRevoked

Wrong: treating a rebalance as transparent and letting `OnPartitionsRevoked` return without committing the partitions about to be taken away. The new owner resumes from the last committed offset, so every message the old owner processed but did not commit is redelivered.

Fix: `OnPartitionsRevoked` is the consumer's last chance to commit for the partitions it is losing. Commit there, synchronously, before the callback returns and the partition changes hands.

### Using a Stale Generation After a Rebalance

Wrong: caching `gen := coordinator.Generation()` once and never rechecking it, then committing or processing against assignments from a superseded generation. A consumer can overwrite the progress of whichever consumer now owns the partition.

Fix: re-read the generation after any join, leave, or expiry, and compare it against the cached value. A consumer must stop processing and committing a partition the instant it is revoked, and must treat any work tagged with an older generation as void.

### Recomputing the Whole Assignment on Every Rebalance (Eager When You Could Be Sticky)

Wrong: running RoundRobin or Range from scratch on every membership change. When one consumer of twelve leaves, an eager recompute can move nearly every partition, forcing every consumer to rebuild per-partition state even though only one consumer actually departed.

Fix: use a sticky/cooperative assignor that preserves surviving consumers' partitions and moves only the orphans. Measure success by the count of already-owned partitions that changed hands, not just by balance.

---

Next: [01-partition-assignment.md](01-partition-assignment.md)
