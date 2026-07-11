# Kafka Clients with franz-go — Concepts

Kafka is a distributed append-only log, and a Go service talks to it through a
client library. For a senior backend engineer three libraries are on the table:
Sarama (the original, callback-heavy, split into separate producer/consumer
types), confluent-kafka-go (a cgo wrapper over librdkafka, so your build now
depends on a C toolchain and a shared library), and franz-go (pure Go, no cgo,
a single unified client, complete protocol coverage including the idempotent
producer, exactly-once transactions, cooperative rebalancing, and the KIP-848
next-generation consumer group protocol). This chapter uses franz-go because it
is the one that stays out of your way: one `*kgo.Client` both produces and
consumes, and a companion `kadm` package covers topic and offset administration
so you do not reach for a second library to inspect a group.

The whole chapter turns on one sentence you should be able to recite for any
consumer you own: *Kafka gives you at-least-once delivery by default, and every
duplicate or lost message is a consequence of where you put your commit relative
to your side effect.* Everything below is a corollary of that sentence.

## Delivery semantics are the entire game

There are three delivery contracts, and you choose which one you have by where
the offset commit sits relative to the work the message triggers.

- **At-least-once** (the default, and what you almost always want): process the
  message first, then commit its offset. If the process crashes between the two,
  the offset was never advanced, so on restart the message is redelivered and
  reprocessed. The cost is duplicates; the guarantee is no loss.
- **At-most-once**: commit the offset first, then process. If you crash between
  the two, the offset already moved, so the message is never redelivered — it is
  lost. Almost nobody wants this on purpose; it is the accidental result of
  autocommit racing slow processing.
- **Exactly-once-in-stream**: use Kafka transactions to make the consume, the
  process-and-produce, and the offset commit one atomic unit. That is the next
  lesson; it is the only honest way to get "exactly once", and only within a
  Kafka-to-Kafka read-process-write.

The practical consequence: at-least-once forces your downstream side effects to
be idempotent (which is a good property anyway), and the size of your duplicate
window is exactly the batch you have processed-but-not-yet-committed.

## The idempotent producer

On the produce side, "at-least-once" shows up as duplicate-on-retry: the client
sends a batch, the network drops the acknowledgement, the client retries, and
the broker appends the batch twice. The idempotent producer eliminates that
*within a producer session*. The broker assigns the client a producer id and the
client stamps each record batch with a monotonic sequence number per partition;
the broker rejects a batch whose sequence it has already seen, so a retried batch
is deduplicated rather than appended twice.

franz-go enables idempotent production **by default**. Idempotence requires
`acks=all` (the broker must wait for all in-sync replicas), so the matching
choice is `kgo.RequiredAcks(kgo.AllISRAcks())`. If you instead ask for
`kgo.LeaderAck()` (acks=1) or `kgo.NoAck()` (acks=0) while idempotence is on,
`kgo.NewClient` returns an error — you would have to explicitly
`kgo.DisableIdempotentWrite()` to weaken the contract. That refusal is a feature:
it stops you from silently pairing a durability setting with a producer mode that
cannot honor it. The `acks` trade-off itself is the classic one: `AllISRAcks` is
durable but pays the latency of the slowest in-sync replica; `LeaderAck` returns
as soon as the leader has the write and can lose it if the leader fails over
before replication; `NoAck` is fire-and-forget and can lose data freely.

## Partitioning fixes both ordering and load

A topic is split into partitions, and *ordering is only guaranteed within a
single partition*. There is no global order across a topic. The partition a
record lands on is chosen from its key: same key hashes to the same partition, so
records that share a key are ordered relative to each other. This is the lever
you use to get ordering where it matters — key every event for one aggregate
(one customer, one order, one account) by that aggregate's id, and all of its
events stay in order on one partition. Records with a nil key are spread across
partitions with no ordering guarantee.

The same mechanism is a load hazard. If your key has low cardinality or is
skewed — one tenant that dwarfs the rest, a null-ish default key — one partition
becomes hot. A hot partition caps throughput at what a single partition and a
single consumer can handle, no matter how many partitions the topic has. Choosing
a key is choosing a trade-off between ordering scope and even load; there is no
key that gives you both global ordering and perfect balance.

franz-go's default partitioner is `StickyKeyPartitioner`, which is compatible
with Kafka's own default (murmur2 hashing of the key). Compatibility matters when
more than one client writes the same topic: if two producers hash keys
differently, the same key can land on different partitions depending on who
produced it, breaking the per-key ordering you thought you had. Match the
partitioner across every writer of a topic.

## Cooperative-sticky rebalancing

A consumer group divides a topic's partitions among its members. When membership
changes — a consumer joins, leaves, or dies — the group rebalances. The old
"eager" balancers (range, roundrobin, sticky) do this stop-the-world: every
member revokes *all* of its partitions, the group reassigns everything, and
processing pauses across the whole group during the churn. The cooperative-sticky
balancer (franz-go's default) instead revokes only the partitions that actually
move; members keep the partitions they retain and processing continues on them.

The catch is that cooperative-sticky is *incompatible* with the eager balancers.
A group must agree on its balancer. If you drop a franz-go client (cooperative by
default) into a group whose other members run eager range/roundrobin, the group
cannot form until you align them — configure the balancer explicitly with
`kgo.Balancers(...)` so a mixed fleet negotiates a common one during migration.

## Manual commit and the rebalance hazard

Autocommit periodically commits the latest polled offsets on a timer (every five
seconds by default) and on group leave. It is convenient and *wrong for slow
work*: it can commit an offset for a record your handler has not finished, so a
crash loses that record. The safe at-least-once pattern is
`kgo.DisableAutoCommit()` plus `CommitRecords(...)` *after* processing.

A second, subtler pattern decouples "I finished this record" from the network
commit: `kgo.AutoCommitMarks()` turns autocommit into "commit only marked
records", and you call `MarkCommitRecords(...)` after processing each record. The
background committer then flushes the marks on its interval. You keep the safety
(nothing commits until you mark it done) but amortize the commit round-trips.

The reason commit placement is delicate is that *offset management runs
independently of consumption, so a rebalance can happen between any two polls*. If
a partition is revoked from you and you later commit an offset for it, you either
commit into a partition you no longer own or you lose the progress you made on it.
The mechanism to prevent this is `kgo.OnPartitionsRevoked` — a callback that runs
before you lose the partitions, where you flush uncommitted offsets with
`CommitUncommittedOffsets`. An alternative is `kgo.BlockRebalanceOnPoll`, which
holds off rebalances until you finish the current batch and re-poll; it demands a
`RebalanceTimeout` long enough to cover your processing and an explicit
`CommitUncommittedOffsets` before you allow the rebalance. Either way, the
invariant is: commit-before-losing-a-partition.

## The poll model and back-pressure

`PollFetches` returns a batch while the client fetches the next batch in the
background (double-buffering), so throughput does not stall waiting on the
network. But your processing time *between polls* is bounded by the session and
rebalance timeouts: if you take too long, the broker assumes you are dead and
rebalances your partitions to someone else mid-batch. `PollRecords(ctx, n)` caps
the batch to `n` records, which bounds memory and gives you predictable commit
granularity. If you must do slow work per batch without triggering a rebalance,
that is exactly when `BlockRebalanceOnPoll` plus a generous `RebalanceTimeout`
earns its place.

## Errors do not arrive where you expect

Two error paths trip people up. First, fetch errors: `Fetches.Errors()` (or
`EachError`) returns non-retriable fetch errors — retriable ones are retried
internally by the client. The franz-go README quickstart *panics* on these to
keep the example short; that is wrong for production. Classify them and continue
the loop, because a transient error on one partition should not kill the worker.
Second, produce errors: the asynchronous `Produce` call does **not** return the
produce error. It hands the record to the client and returns; the error is
delivered later to the promise callback. Treating the lack of a return value as
success is silent data loss. Use the promise, or `ProduceSync` whose
`ProduceResults.FirstErr()` surfaces the first failure, or a `FirstErrPromise` to
aggregate one error over an asynchronous batch.

## Consumer lag is the primary health signal

The one number an SRE watches for a consumer group is lag: for each partition,
`lag = log-end-offset - committed-offset`. Rising lag means consumers are falling
behind producers. Flat, high lag on a group with no active member means a stuck
or perpetually-rebalancing consumer, not a throughput problem. Lag arithmetic has
edge cases that are easy to get wrong by hand — a partition with a committed
offset but no live member (idle or mid-rebalance), and a partition the group has
never consumed (no commit at all, so the "lag" is the whole log). The `kadm`
package computes all of this for you via `Client.Lag`, which internally uses
`CalculateGroupLag` over described-group offsets and listed end offsets. Reach for
it rather than re-deriving the offset math.

## Client lifecycle

One `*kgo.Client` is long-lived, goroutine-safe, and can both produce and
consume; create it once per process and share it. Do not create a client per
request or per message — that churns metadata fetches and TCP connections for no
benefit. And always `Close()` it on shutdown: `Close` flushes buffered records
and leaves the group cleanly, which triggers a final offset commit and lets the
group rebalance immediately. Dropping a client without `Close` skips that final
commit and forces the rest of the group to wait out the session timeout before
they can take over your partitions.

## Testing against a broker without breaking CI

Everything above is network code: it needs a running Kafka. `go test ./...` on a
laptop or in CI without a broker must still pass. The convention this lesson
follows — and what real teams do — is to keep the client-touching code behind a
`//go:build kafka` build tag, and to extract the pure logic (which record key an
entity maps to, how to classify a fetch error, how to compute a lag report) into
untagged code that has no Kafka dependency at all. The pure logic is unit-tested
offline and deterministically; the integration tests run only when you build with
`-tags kafka` against a broker whose address comes from an environment variable.
That is how you get fast, always-green unit tests and honest integration coverage
from the same module.

## Common Mistakes

### Committing before the side effect completes

Wrong: rely on autocommit (or call `CommitRecords` up front) while the handler
does slow work. A crash commits offsets for records that were never fully
processed, silently dropping them — at-least-once quietly becomes at-most-once.

Fix: `DisableAutoCommit` and `CommitRecords` only after every record in the batch
is processed (or `AutoCommitMarks` + `MarkCommitRecords` after each record).

### Panicking on fetch errors

Wrong: copy the README quickstart and `panic` on `Fetches.Errors()`. One
transient partition error takes down the whole worker.

Fix: iterate `EachError`, classify retriable versus fatal, log and continue on
transient errors, and stop only on a genuinely fatal one.

### Treating async Produce as fire-and-forget-success

Wrong: call `Produce` and move on because it returned no error. The produce error
is delivered to the promise, not returned; ignoring the promise loses data.

Fix: check the promise, or use `ProduceSync(...).FirstErr()`, or a
`FirstErrPromise` to aggregate the first error over an asynchronous batch.

### Assuming global ordering

Wrong: expect events to be consumed in produce order across a topic, or key
related events differently and still assume they stay ordered.

Fix: ordering holds only within a partition. Key every event that must stay
ordered by the same aggregate id so it lands on one partition.

### Mismatched balancers in a mixed fleet

Wrong: add a default franz-go client (cooperative-sticky) to a group whose other
members use eager range or roundrobin. The group cannot form.

Fix: set `kgo.Balancers(...)` explicitly so every member agrees on one balancer
during a migration.

### Ignoring the rebalance window

Wrong: process a batch with no `OnPartitionsRevoked` and no `BlockRebalanceOnPoll`.
A rebalance mid-batch leaves uncommitted work that another member reprocesses or
that is lost.

Fix: commit-before-losing-a-partition — flush with `CommitUncommittedOffsets` in
`OnPartitionsRevoked`, or block rebalances during a poll with a long enough
`RebalanceTimeout`.

### Dropping the client instead of closing it

Wrong: let the process exit without `Close`. The final commit is skipped and the
group stalls until the session timeout expires before it can rebalance.

Fix: `defer cl.Close()` so shutdown flushes, commits, and leaves the group.

### A client per message

Wrong: construct a `*kgo.Client` for each request or message. Every one re-fetches
metadata and opens connections.

Fix: create one long-lived, goroutine-safe client per process and share it.

### Re-implementing lag arithmetic

Wrong: subtract committed from end offsets by hand and forget the nil-commit and
idle-member edge cases.

Fix: use `kadm.Client.Lag` / `CalculateGroupLag`, which handle those cases.

Next: [01-resilient-producer-and-partitioning.md](01-resilient-producer-and-partitioning.md)
