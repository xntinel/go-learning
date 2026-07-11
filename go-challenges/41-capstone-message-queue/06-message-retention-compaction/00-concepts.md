# 6. Message Retention and Log Compaction — Concepts

Log compaction is the feature that turns a message queue from a temporary buffer into a durable, queryable state store. Two mechanisms govern a message's lifecycle: *retention* throws away data that is too old or too voluminous to keep, and *compaction* keeps only the latest value per key so the log converges to the current state of the world. The hard part is not the idea; it is making both correct under concurrent producers, efficient at segment granularity, and safe for consumers that have not yet caught up. This file is the conceptual foundation. Read it once and you will have everything you need to work through the exercises, which build the log piece by piece as independent, self-contained Go modules.

## Concepts

### The segment-based log model

Messages are not stored as individual rows in a table. They are appended to an *active segment* — a buffer with a fixed size ceiling. When the active segment fills, it is sealed and a new one opens. Sealed segments are immutable: readers never contend with the writer, and retention or compaction can process a sealed segment without locking out producers.

This design has one decisive consequence: **retention and compaction operate at segment granularity, never at message granularity.** Deleting one message from the middle of a segment would require rewriting the whole segment; deleting an entire segment is a single unlink. Every policy in this lesson is therefore phrased as a question about a segment as a whole, never about a single message.

A sealed `Segment` captures exactly the metadata the policies need:

- `SizeBytes()` is the sum of key and value byte lengths across all of its messages.
- `FirstTimestamp()` and `LastTimestamp()` bound the segment in time.
- `BaseOffset()` is the log offset of its first message; it gives the total order across segments.

A `Log` is then just an ordered list of sealed segments guarded by a lock. Producers `Append`; a retention or compaction pass builds a new list off to the side and installs it atomically with one write-locked swap, so no reader ever observes a half-applied result.

### Time-based and size-based retention

Time retention deletes any segment whose *newest* message is older than a configured `MaxAge`. Because the segment is immutable and ordered, `LastTimestamp()` alone decides eligibility: if even the youngest message in the segment is past the window, every message in it is, and the whole segment can go. This is O(S) in the number of segments, independent of the message count.

Size retention caps total volume. It deletes the oldest segments — lowest `BaseOffset()` first — until the total is back within `MaxBytes`. The one invariant that is easy to miss: at least one segment must always survive. A consumer that has read nothing yet needs *some* segment to start from; an empty log is a log with no entry point. So even a single segment larger than `MaxBytes` is preserved rather than deleted.

The two policies are independent and compose. A topic can carry a 7-day age limit and a 100 GB size cap simultaneously; whichever trigger fires first wins. A background reaper that runs both on a timer is the natural home for the combination, and it is where the concurrency story gets interesting: the reaper mutates the segment list while producers are still appending.

### Log compaction: the two-pass algorithm

For topics where each message is the current state of an entity — a user record, a product price, an account balance — a consumer only needs the *latest* value per key. Compaction rewrites the log to hold exactly one message per distinct key: the one with the highest offset.

The algorithm is two passes over the messages.

**Pass 1 builds the keep set.** Scan every message and, for each key, record the highest offset seen. The result is a `map[string]int64` from key to the offset of its surviving message. This map is O(K) in the number of *distinct* keys, not O(N) in the message count: a million messages across ten thousand keys needs ten thousand map entries, no matter how many versions each key accumulated.

**Pass 2 writes the compacted segment.** Re-read the messages in offset order and emit a message only when its offset equals the latest offset recorded for its key in pass 1. Everything else is a superseded version and is dropped.

Two passes are required because the keep/drop decision for any message depends on whether a *later* message with the same key exists, which a single forward pass cannot know without buffering every value. A single backward pass could know it, but it would emit messages in descending offset order and need a second allocation to restore ascending order — the same cost, with a misleading shape. The forward two-pass design reads each segment exactly twice in sequential order, which is what storage hardware is fast at, and holds only the offset map between the passes.

Keyless messages (empty key) are never compacted: with no key there is no "latest value per key" relation to apply, so they pass through unchanged. Sort and compare offsets as integers, never as their string forms — lexicographically `"9" > "10"`, which would silently designate an older version as the survivor.

### Tombstones and the delete-retention window

A tombstone is a message with a non-empty key and a nil value. It records that a key was deleted. Compaction cannot simply drop a deleted key's messages and move on, because a consumer that has not yet read the segment needs to *observe* the deletion to evict its own cached value. If the tombstone vanished the instant compaction first saw it, a slow consumer would never learn the key was deleted and would serve a stale value forever.

So a tombstone has a two-stage lifecycle keyed off a `DeleteRetention` duration. On the first compaction after the deletion, the compactor records the tombstone's creation time and *keeps* it in the compacted segment. On a later compaction, once more than `DeleteRetention` has elapsed since that recorded time, the tombstone is finally removed from both the segment and the per-key metadata. The window is the grace period during which every consumer is assumed to have had a chance to observe the delete.

### The dirty-ratio trigger

Compaction is not free, so it should not run on every append. "Dirty" bytes are bytes that have not yet been compacted — those in segments whose `BaseOffset()` lies above the last compacted offset. The dirty ratio is `dirty bytes / total bytes`. When it crosses a threshold (Kafka's default is 0.5), a compaction cycle is triggered.

The ratio guards against two opposite failures. Compacting too eagerly turns every write into a full rescan and wastes I/O. Compacting too rarely lets redundant versions pile up, inflating both storage and the time a fresh consumer needs to catch up to the current state. A self-compacting log measures the ratio after each append, runs a cycle only when it crosses the threshold, and reports the bytes reclaimed so the amortized cost is visible.

## Common Mistakes

### Deleting every segment under size pressure

Wrong: looping over segments oldest-first and deleting each one whose removal is needed to get under `MaxBytes`, with no floor.

What happens: when the whole log is one oversized segment, the loop deletes it and leaves an empty log. A consumer that calls `Segments()` gets nothing and has no offset to start from. The size policy must stop while at least one segment remains, even when that segment alone exceeds `MaxBytes`. The guard is a single `i < len(sorted)-1` bound on the deletion loop.

### One-pass compaction newest-to-oldest

Wrong: scanning messages from newest to oldest, keeping the first occurrence of each key, and appending results into a slice.

What happens: it is functionally correct for an in-memory slice but it accumulates messages in *descending* offset order and needs a reversing pass before the compacted segment can be written, an extra allocation and an extra O(K) pass. Worse, it implies compaction can be done in one backward sweep over storage, which is false: disks are fast forward and slow backward. Build the offset map in a forward pass, then emit in a second forward pass.

### Removing a tombstone the first time compaction sees it

Wrong: treating a tombstone like any superseded version and dropping it during the compaction that first encounters it.

What happens: a consumer that reads the log after that compaction never sees the delete and caches the deleted key's old value indefinitely. The tombstone must survive at least `DeleteRetention` so every consumer has a window to observe the deletion before the record is reclaimed.

### Comparing offsets as strings

Wrong: storing offsets as string map keys and choosing the "latest" by lexicographic comparison.

What happens: `"9"` sorts after `"10"`, so the algorithm designates offset 9 as the survivor when offset 10 is the true latest, keeping a stale value and discarding the current one. Always carry the offset as `int64` and compare numerically.

### Mutating the segment list in place while readers hold it

Wrong: a retention pass that sorts or truncates the live `[]*Segment` the log exposes.

What happens: a concurrent reader iterating the slice sees a half-mutated list, and the race detector flags the unsynchronized write. Build the new segment list off to the side without holding the lock, then install it with one write-locked `ReplaceSegments`. The swap is the only point under the write lock, and it is atomic with respect to every reader.

---

Next: [01-segmented-log.md](01-segmented-log.md)
