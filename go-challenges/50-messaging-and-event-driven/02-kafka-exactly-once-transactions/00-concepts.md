# Kafka Transactions and Exactly-Once Semantics — Concepts

"Exactly-once" is the phrase that ends more interviews than it starts, because
most engineers repeat it without knowing where the guarantee begins and, more
importantly, where it ends. Kafka does not give you exactly-once *delivery* over
the network. It gives you exactly-once *processing within a Kafka-to-Kafka
stream*: the records a stage produces and the consumer offsets it commits are
written as one atomic transaction, so a consume-transform-produce loop either
advances completely or not at all. The instant your sink stops being Kafka — a
row in Postgres, a charge on a card, an email — that guarantee evaporates and you
are back to at-least-once, which you must make safe with idempotency (the outbox
and inbox patterns in lessons 6 and 7). This file gives you the whole model so
the three exercises that follow are about wiring, not about discovering what the
words mean.

## Concepts

### What exactly-once actually means in Kafka

The guarantee is narrow and precise: for a pipeline that reads from a Kafka
topic, transforms records, and writes derived records back to Kafka, each input
record affects the output *exactly once*, even across producer restarts and
consumer-group rebalances. It works because the produced records and the
committed input offsets live in a single transaction. Commit the offsets and you
have durably recorded "these inputs are done" at the same instant you made their
outputs visible; abort and neither happened. There is no window where output
exists but the offset is unrecorded (which would reprocess and duplicate) or the
offset is recorded but the output is missing (which would drop data).

This is *processing* semantics, not *delivery* semantics. A record can still be
transmitted more than once on the wire; the broker deduplicates it. And the
guarantee holds only while both sides are Kafka. Write to an external system
inside your handler and that write is not part of the transaction — it will not
roll back when the transaction aborts, and it will happen again when the input is
reprocessed. State this boundary out loud in any design review.

### The two primitives underneath

Exactly-once is built from two independent mechanisms. The first is the
**idempotent producer**: the broker assigns each producer a producer id and
tracks a monotonic sequence number per partition, so a record the producer
retries after a network hiccup is recognized as a duplicate and dropped. This
alone removes duplicates caused by producer retries, and it is cheap enough to be
on by default in modern clients.

The second is **transactions** layered on top. You give the producer a stable
`TransactionalID`. On startup the broker associates that id with a producer epoch;
producing, and committing consumer offsets, can then be grouped into a
transaction that commits or aborts atomically across multiple partitions. The
`TransactionalID` also provides identity across restarts, which is what makes
fencing (below) possible. Idempotence kills retry duplicates on a single
partition; transactions add all-or-nothing atomicity across many partitions plus
the offset commit.

### The read-process-write loop and why offsets commit inside the transaction

The canonical shape is: poll a batch from the input topic, transform each record,
produce the derived records, and commit the consumed offsets. The subtle,
load-bearing rule is that the offset commit must happen *inside the same
transaction* as the produces. If you commit offsets separately — a plain
`CommitOffsets`, or worse an auto-commit ticker — you reopen the classic gap: the
produce succeeds, the process dies before the offset commit, and on restart the
same inputs are reprocessed and their outputs duplicated. Putting the produce and
the offset commit in one transaction closes that gap by construction. franz-go's
`GroupTransactSession` does exactly this: its `End` commits the group's offsets as
part of committing the transaction.

### read_committed and the Last Stable Offset

A transaction is only useful to downstream consumers if they can avoid reading
records that later abort. That is the job of the isolation level. A
**read_committed** consumer returns records only up to the **Last Stable Offset**
(LSO) — the offset before the earliest still-open transaction — and it filters out
records belonging to aborted transactions using an abort index the broker
maintains. A **read_uncommitted** consumer (the default) returns everything,
including records that will be rolled back.

There are two consequences a senior engineer must internalize. First, on the
consuming side of any exactly-once pipeline you *must* set read_committed
explicitly; the default will happily surface records that never really happened.
Second, read_committed consumer lag is bounded by open transactions: while a
transaction is open, the LSO cannot advance past it, so a stalled producer holds
back every downstream read_committed reader. That is why you keep
`TransactionTimeout` short — seconds, not the 60-second default — so a wedged
producer is fenced and its transaction aborted quickly instead of stalling the
whole downstream.

### Producer fencing and zombies

Fencing is the mechanism that makes exactly-once correct across restarts and
rebalances. When a new producer initializes with the same `TransactionalID` as an
old one, the broker bumps the producer epoch and *fences* the old instance: any
further produce or commit from the zombie fails with `PRODUCER_FENCED` or
`INVALID_PRODUCER_EPOCH`. This is why a crashed-and-restarted worker cannot have
its in-flight transaction silently committed by the corpse of the previous
process, and why two instances sharing a `TransactionalID` will fence each other
into uselessness.

Two rules follow. The `TransactionalID` must be **stable per logical producer
instance** — stable across restarts (so recovery and fencing work) and unique
across concurrently running instances (so they do not fence each other).
Randomizing it per restart breaks recovery; sharing it across live instances
breaks liveness. And fencing errors are **fatal**: retrying a fenced producer can
never succeed, so the correct response is to tear the client down and recreate it,
not to loop.

### GroupTransactSession's safety rail

A rebalance can revoke one of your input partitions mid-transaction, which means
another group member may now own that input and will reprocess it. If your zombie
went ahead and committed, that input would be processed twice.
`GroupTransactSession` hooks partition revocation and loss and forces a pending
`End` to *abort* when a rebalance happened during the transaction. It trades the
occasional redone batch for the guarantee that the same input is never committed
twice. The visible consequence is that `End` can return `committed == false` with
a *nil* error — the transaction was safely aborted because of a rebalance, not
because anything failed. Treat that as an expected outcome (retry the batch), not
as a bug.

### The error taxonomy and recovery protocol

Transactional errors fall into a few classes, and conflating them is the most
common operational bug:

- `OPERATION_NOT_ATTEMPTED`: in a batched end-transaction RPC the broker skipped
  the `EndTxn` for this producer. It was *not* attempted, so the safe move is to
  call `EndTransaction` again with `TryAbort`. It is not success and it is not a
  generic retry.
- `TRANSACTION_ABORTABLE` and `UNKNOWN_PRODUCER_ID`: recoverable. Abort the
  current transaction and continue with a fresh one.
- `CONCURRENT_TRANSACTIONS`: transient; retry the operation after a short
  backoff.
- `PRODUCER_FENCED`, `INVALID_PRODUCER_EPOCH`, `INVALID_TXN_STATE`: fatal for
  this producer. A newer instance fenced you, or the producer's transactional
  state machine is broken. Recreate the client.

franz-go's `EndTransaction`/`End` retry internally on transient conditions, so by
the time an error surfaces to you it is meaningful: you classify it and act,
rather than wrapping it in yet another naive retry loop.

### franz-go API precision that trips people up

`TransactionEndTry` is a named **bool**, not an int enum: `TryCommit` is `true`,
`TryAbort` is `false`. The idiom is therefore to derive the argument straight from
the produce result:

```
committed, err := sess.End(ctx, kgo.TransactionEndTry(firstErr == nil))
```

`FetchIsolationLevel` defaults to `ReadUncommitted`, so read_committed is opt-in
via `kgo.FetchIsolationLevel(kgo.ReadCommitted())`. `EndTransaction` does *not*
flush: on the raw client you call `Flush` (and, for offsets,
`CommitOffsetsForTransaction`) before `EndTransaction`; `GroupTransactSession.End`
does the flushing for you. Finally, `RequireStableFetchOffsets` is now a
deprecated no-op — KIP-447 stable offsets are always on — so ignore older advice
that treats it as a required option.

### The cost and the core tuning tension

Transactions are not free. Each one writes transaction markers to the log and
costs an extra `AddPartitionsToTxn` plus `EndTxn` round trip, and read_committed
adds LSO-bounded latency downstream. You amortize the per-transaction overhead by
batching many records into one transaction. But a larger transaction has a larger
abort blast radius (more work redone when it aborts) and holds the LSO longer
(more downstream latency). Batch size versus latency-and-abort-cost is the central
tuning decision; there is no universally right number, only a trade-off you make
deliberately per pipeline.

### Where exactly-once is the wrong tool

Reach for something else whenever the sink is not Kafka: cross-system atomicity
(Kafka plus a relational database), long-running side effects, or calls to
external non-idempotent APIs. Kafka transactions cannot enroll a Postgres write or
an HTTP call. For those, the correct patterns are the transactional **outbox**
(write the domain row and an outbox row in one database transaction, then relay to
Kafka) and the idempotent **inbox** (dedupe at-least-once delivery by message id).
Those lessons exist precisely because exactly-once does not extend past Kafka.

## Common Mistakes

### Believing exactly-once covers your database or an HTTP call

Wrong: assuming "exactly-once" means no duplicates anywhere, including a database
write or a downstream API call inside your handler. Fix: scope it to
Kafka-to-Kafka with offsets committed inside the transaction. For any other sink,
fall back to at-least-once plus idempotency (outbox/inbox).

### Committing consumer offsets outside the transaction

Wrong: producing inside a transaction but committing offsets with a separate
`CommitOffsets` call or an auto-commit loop. Fix: commit offsets *inside* the
transaction (with `GroupTransactSession`, `End` does this); committing them
separately reopens the reprocessing gap and defeats the whole design.

### Forgetting read_committed on the consumer

Wrong: leaving the consumer at its default `ReadUncommitted`, so the pipeline
reads records that later abort. Fix: set
`kgo.FetchIsolationLevel(kgo.ReadCommitted())` on every consumer downstream of a
transactional producer.

### Treating TransactionEndTry as an int enum or inverting it

Wrong: guessing that `TryCommit` is `0` or `1`, or passing the wrong constant.
Fix: it is a named bool — `TryCommit` is `true`, `TryAbort` is `false` — so write
`End(ctx, produceErr == nil)`.

### Ignoring the OPERATION_NOT_ATTEMPTED protocol

Wrong: treating `OPERATION_NOT_ATTEMPTED` as a success, or as a generic
retryable error. Fix: it means the broker did not attempt the end, so call
`EndTransaction` again with `TryAbort`.

### Retrying on fatal producer errors

Wrong: looping on `PRODUCER_FENCED`, `INVALID_PRODUCER_EPOCH`, or
`INVALID_TXN_STATE`. Fix: these are fatal — a newer instance fenced you or the
state machine is broken. Tear down and recreate the client; a retry never
succeeds.

### Choosing the TransactionalID wrong

Wrong: reusing one `TransactionalID` across concurrently running instances (they
fence each other) or randomizing it per restart (recovery and fencing break).
Fix: make it stable per logical producer instance — stable across restarts,
unique across live instances.

### Leaving TransactionTimeout at the default

Wrong: keeping the 60-second default, so a stalled producer holds the LSO and
stalls every downstream read_committed consumer for a minute. Fix: set a short
timeout (seconds) so a wedged producer is fenced and its transaction aborted
quickly.

### Doing non-idempotent side effects inside the transaction

Wrong: writing to a database, sending an email, or charging a card inside the
transaction and assuming it rolls back on abort. Fix: only Kafka produces and
offset commits are transactional; move external effects behind the outbox/inbox.

### Assuming a rebalance mid-transaction is harmless

Wrong: treating `End` returning `committed == false` as a failure to fix, or not
guarding rebalances at all (a zombie then commits input another member
reprocesses). Fix: use `GroupTransactSession`, which aborts on rebalance, and
treat `committed == false` with a nil error as an expected retry, not a bug.

### Using RequireStableFetchOffsets as if it were required

Wrong: copying old code that passes `RequireStableFetchOffsets` as a mandatory
option. Fix: it is now a deprecated no-op; stable offsets are always on. Drop it.

Next: [01-transactional-producer.md](01-transactional-producer.md)
