# Idempotent Consumers and the Inbox Pattern — Concepts

Every real broker delivers *at least once*. Kafka redelivers a partition after a
consumer rebalance or an ack that arrives past the session timeout; SQS makes a
message visible again when its visibility timeout expires before the delete call
lands; NATS JetStream and Redis Streams both replay un-acked messages from their
pending lists. Even the transactional outbox relay from lesson 06 can double
publish: it commits the domain row and the outbox row together, but if the relay
crashes between publishing to the broker and marking the outbox row sent, the next
relay run republishes the same event. The broker cannot fix this — de-duplicating
a side effect it never sees (an email, a charge, a `balance += amount`) is not its
job. Correctness is won on the consumer side, and this lesson is about the
mechanism that wins it: the inbox.

## Concepts

### Delivery semantics, stated precisely

A transport offers one of two useful contracts. *At-most-once* means a message is
delivered zero or one times: the broker never redelivers, so a lost ack loses the
message. *At-least-once* means it is delivered one or more times: nothing is lost,
but duplicates happen. "Exactly-once delivery" does not exist at the transport
layer for a consumer that has an external side effect — it is a marketing phrase
for a bundle of features (idempotent producers, transactional offsets) that give
exactly-once *within a stream*, not across an email gateway or a payment API. The
honest engineering position is: pick at-least-once (you cannot afford to lose
events), then *manufacture* effectively-once processing with an idempotent
consumer. Exactly-once is at-least-once delivery plus a consumer that collapses
duplicates.

### The core invariant: mark and apply commit together

"Process a message" decomposes into two writes: *mark* this message id as consumed
(the inbox row) and *apply* its domain side effect (the balance update, the row
insert). The whole pattern rests on one invariant: those two writes commit or roll
back **in a single transaction**. Split them and you get one of two corruptions.
If you apply the side effect and then, in a separate step, mark the id — and crash
in between — the redelivery finds no inbox row, applies again, and you have a
double-apply (a customer charged twice). If you mark the id first and then apply —
and crash in between — the redelivery finds the inbox row, skips, and the side
effect is lost forever: a lost update marked as done. Only a shared transaction
closes both gaps: either both the inbox row and the domain mutation are durable, or
neither is, and the broker's redelivery gets a clean retry.

### Natural versus imposed idempotency

Not every operation needs an inbox. Some are *naturally idempotent*: `SET x = v`,
add-to-set, `PUT /users/42` with a client-chosen id, "mark order shipped". Applying
them twice lands in the same state as applying them once, so a duplicate is
harmless and no inbox is required. Others are *not* idempotent: `balance +=
amount`, append-to-log, "send the welcome email", "increment a counter". For these
you must *impose* idempotency, either with a natural business unique key (an order
number the second insert violates) or with an inbox table that records the message
id. A senior engineer picks the cheapest mechanism per operation and does not pay
for an inbox where an upsert already gives idempotency for free.

### Dedup scope is per consumer group, not global

The inbox key is a composite: `(subscriber_id, message_id)` — the consumer group
plus the message id, not the message id alone. A single topic usually feeds several
independent consumers: the billing service and the analytics service both read
`OrderPlaced`, and each must process it exactly once *from its own point of view*.
If the inbox keyed on `message_id` globally, the first consumer to record the id
would hide the message from the second consumer forever. Scoping the key to the
consumer group gives each subscriber its own dedup namespace. In a database this is
a two-column primary key or unique index; in memory it is a struct key of two
strings.

### Concurrency and the TOCTOU trap

Two workers can hold the *same* redelivered message at the same time. A Kafka
rebalance can hand a partition to a new owner while the old owner is still
processing; an SQS visibility timeout can expire and re-serve a message mid-flight.
So the dedup decision is a concurrent one, and the naive shape is a race:

```go
if !store.Has(id) { // both workers see false
    store.Put(id)   // both write
    handle(msg)     // both apply — double-apply
}
```

Between the `Has` check and the `Put` there is a time-of-check-to-time-of-use gap
that two goroutines slip through together. The fix is not a bigger read; it is to
make the claim *atomic* — a single operation that either wins or loses. In SQL that
is `INSERT ... ON CONFLICT DO NOTHING` (or a unique-constraint violation you catch)
where the database serializes the writers and exactly one insert succeeds. In this
lesson's in-memory model it is a check-and-set performed entirely under one write
lock, so the branch is on the *result* of the atomic claim (`firstSeen`), never on
a separate prior read. The guarantee lives in the atomic operation, not in an
if-statement.

### Result caching for request/reply

A duplicate should not merely be *skipped* — for a request/reply or a stateful ack
it should return the **same response** the first processing produced. Re-running
the handler "because it is idempotent anyway" can still observe changed state and
compute a different answer, or re-fire an external call. Storing the outcome keyed
by id turns the consumer into a true idempotency layer, exactly like Stripe's
`Idempotency-Key`: the second request with the same key gets the cached result of
the first. This gives three distinguishable states for an id: *never seen* (run the
handler), *seen and done* (return the cached result), and *seen and in-flight*
(another worker holds it — wait or let the unique constraint reject you).

### Error and retry semantics

The inbox row must be committed **only when the handler succeeds**. If the handler
fails and you record the id anyway, you have permanently swallowed a message that
should have been retried — the broker redelivers, the inbox says "done", and the
side effect never happens. So on error the transaction rolls back: no inbox row, so
the next redelivery re-invokes the handler and can succeed. The flip side is the
poison message: an event that fails every time will be redelivered forever and can
wedge a partition. That needs a retry ceiling and a dead-letter destination
(lesson 10), which is the escape hatch the inbox's retry loop needs so it does not
spin unbounded.

### Deriving a stable idempotency key

The dedup key has to be *stable across redeliveries of the same event*. The best
key is a producer-assigned identifier carried in a header — a UUIDv7 or a business
key — because it survives transport reassignment: the same logical event keeps its
id even if the broker gives it a new offset or message number. When no such id
exists, the fallback is a content hash: SHA-256 over a *canonical* serialization of
the payload, hex-encoded to a string. Content hashing has two sharp edges. First,
it wrongly de-duplicates two events that are legitimately identical (two separate
`$5 top-up` requests hash the same), so an explicit id must always win when
present. Second, it demands a canonical byte form: `fmt.Sprintf` of a struct or a
map iterated in random order produces different bytes for the same event and breaks
dedup silently. `encoding/json` sorts map keys deterministically, which gives a
stable canonical form for free — that guarantee is the whole reason the hash works.

### Retention: the inbox grows forever

An inbox table that is never pruned grows without bound and eventually dominates
write latency, because every claim probes an ever-larger index. So a retention job
deletes rows older than some horizon. The horizon is not arbitrary: it must exceed
the broker's *maximum redelivery window*. Prune a row while a very late redelivery
of that same message could still arrive, and you have re-opened the duplicate gap —
the id is gone, the message looks new, the side effect fires twice. Size retention
to the broker's redelivery guarantees (SQS's message retention, Kafka's offset
reset horizon), not to whatever keeps the table small. An alternative to a separate
inbox table is embedding the last-processed id *in the business row* itself, which
prunes automatically as the row is updated.

### Inbox versus outbox versus broker-native dedup

Three mechanisms are easy to confuse. The *outbox* (lesson 06) is producer-side: it
makes publishing atomic with the domain write so an event is never lost. The
*inbox* is consumer-side: it makes processing atomic with the domain write so an
event is never applied twice. Broker-native exactly-once (Kafka's idempotent
producer and transactional offsets, lesson 02) covers state that lives *inside the
stream* — read-process-write where the output is another Kafka topic. None of these
removes the need for a consumer inbox when the side effect is *external*: an email,
a charge, a call to a third-party API. Kafka can make your offset commit and your
output-topic write atomic, but it cannot un-send an email. For any externally
visible, non-idempotent effect, the inbox stays.

## Common Mistakes

### Check-then-insert instead of an atomic claim

Wrong: `if !store.Has(id) { store.Put(id); handle() }`. Two concurrent
redeliveries both pass `Has` and both call `handle` — a double-apply through the
TOCTOU gap. Fix: make the claim atomic (a unique constraint, `INSERT ... ON
CONFLICT DO NOTHING`, or a single locked check-and-set) and branch on its
`firstSeen` result, never on a separate prior read.

### Recording the id in a different transaction from the side effect

Wrong: mark the id in one commit and apply the change in another. A crash between
them gives a double-apply or a lost update. Fix: one transaction (or one atomic
`RecordAndApply` closure) covers both the inbox row and the domain mutation.

### Recording the id even when the handler failed

Wrong: commit the inbox row before knowing the handler succeeded. A transient
failure now permanently drops the message — the redelivery sees "done" and skips.
Fix: commit the inbox row only on success; on error, roll back so the broker
redelivers and retries.

### Global dedup instead of per consumer group

Wrong: key the inbox on `message_id` alone. The first consumer to record an id
hides the message from every other independent consumer of the topic. Fix: key on
`(consumer_group, message_id)` so each subscriber has its own dedup namespace.

### Not returning the cached result on a redelivery

Wrong: skip a duplicate but recompute or re-fire the response. A re-run can observe
changed state and return a different answer, or trigger an external call twice. Fix:
store the first outcome and return it verbatim for request/reply flows.

### Content-hashing an unstable serialization

Wrong: hash `fmt.Sprintf("%v", msg)` or a map iterated in random order — identical
events produce different keys and dedup silently fails. Fix: hash a canonical form.
`encoding/json` sorts map keys, giving a deterministic byte sequence; or hash
explicit, sorted fields.

### Never pruning the inbox

Wrong: let the inbox table grow forever; write latency degrades as the index
bloats. Fix: run a retention job sized to the broker's maximum redelivery window,
and never prune inside that window or you re-open the duplicate gap.

### Assuming a broker's exactly-once removes the need for an inbox

Wrong: "Kafka EOS is on, so we do not need consumer dedup." Broker exactly-once
covers in-stream state, not external effects. Fix: keep the inbox for any
externally visible, non-idempotent side effect (emails, payments, third-party
APIs).

Next: [01-inbox-store.md](01-inbox-store.md)
