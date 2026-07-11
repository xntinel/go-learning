# Durable Messaging with NATS JetStream — Concepts

Core NATS is fire-and-forget: a publish that no subscriber is listening for is
gone, and there is no log to replay. JetStream layers an append-only, replayable,
acknowledged message store on top of that same single server binary. For a senior
backend engineer the interesting question is not "how do I call Publish" but
"where does JetStream sit between fire-and-forget pub/sub and a full log like
Kafka, and how do its knobs map onto correctness guarantees I already reason
about?" This file answers that. Read it once and every design decision in the
three exercises that follow — building a stream config from a policy, deciding
ack/nak/term, folding a replay into a projection — becomes a small, testable
choice rather than a mystery behind the client library.

## Concepts

### Where JetStream sits

Core NATS gives you subject-based publish/subscribe with at-most-once delivery
and zero persistence: if nobody is subscribed when you publish, the message is
dropped, and there is nothing to replay later. JetStream is a persistence layer
that runs inside the same `nats-server` process (enabled with `-js` or a config
block). It captures messages published to configured subjects into an
append-only **stream**, tracks per-consumer delivery position and acknowledgement
state, and supports redelivery and replay.

The right mental frame is "durable messaging with far less operational surface
than Kafka", not "a Kafka replacement". A single lightweight binary gives you
replayable at-least-once streams with no ZooKeeper, no broker fleet, no partition
rebalancing. The trade-off is real: JetStream has no true partitions (ordering is
per-stream, not per-partition-key), and its throughput and retention ceilings are
lower than a tuned Kafka cluster. Reach for it when you want durability and
replay without the cluster; reach for Kafka when you need very high throughput,
partitioned parallelism, or the broader ecosystem.

### The stream/consumer split is the central model

This is the one idea that everything else hangs off. Two objects, two
responsibilities:

- The **stream** owns the durable messages and the retention rules. It is the
  log. It decides what is stored and for how long.
- The **consumer** owns the delivery cursor, the acknowledgement state, and the
  redelivery policy. It is a view over the stream: "I am at sequence 4210, I have
  these three messages un-acked, redeliver them after 30s".

Because the cursor lives on the consumer, not the stream, multiple independent
consumers can read the same stream at their own positions and their own pace. One
consumer can be tailing the live edge while another replays from the beginning to
rebuild a read model. Adding a second consumer is therefore a *config operation*
on the stream, not a repartitioning of the data. Contrast that with Kafka, where
parallelism is bounded by partition count and adding consumers past that number
does nothing. In JetStream, scaling consumers is a consumer-configuration
problem.

### Retention policy is a correctness decision, not a tuning knob

A stream's `Retention` field decides *who owns deletion*, and picking the wrong
one silently destroys data:

- **LimitsPolicy** (the default): messages are kept until a limit is hit —
  `MaxMsgs`, `MaxBytes`, or `MaxAge`. Acknowledgement does not delete anything.
  This is the event-log shape: many independent consumers, full replay.
- **InterestPolicy**: a message is deleted once *every* consumer bound to the
  stream at publish time has acknowledged it. Storage tracks interest; there is
  no long-term history for late-arriving consumers.
- **WorkQueuePolicy**: a message is deleted on the *first* acknowledgement. The
  stream is a queue with exactly one logical consumer per subject. A second
  consumer on the same subject, or a later replay, finds nothing.

Choosing WorkQueue for a stream you intended to replay or fan out is a classic
silent-data-loss bug: the first ack removes the message before anyone else sees
it. If you want replay or fan-out, use LimitsPolicy (or InterestPolicy). Use
WorkQueue only for a genuine single-owner job queue.

### Storage and MaxAge bound your durability envelope

`Storage` is `FileStorage` (survives a server restart, on disk) or
`MemoryStorage` (fast, gone on restart). Durable does not mean infinite:
`MaxAge`, `MaxMsgs`, and `MaxBytes` bound growth, and messages age out when a
limit is reached. A stream with a one-hour `MaxAge` is durable across a restart
but will not have yesterday's events to replay. Choosing storage and limits is
choosing your cost and durability envelope explicitly.

### Delivery is at-least-once; exactly-once is approximated at the edges

By default JetStream delivers at least once. A message can be delivered more than
once (a redelivery after a missed ack, a producer retry after a lost publish
ack). Exactly-once is approximated at two edges and the broker guarantees neither
for free:

- **Producer side**: set a `Nats-Msg-Id` header (via `WithMsgID`) on publish.
  Within the stream's `Duplicates` window, a second publish carrying the same id
  is collapsed to the already-stored message and the returned `PubAck` has
  `Duplicate = true`. This is what makes an at-least-once producer retry safe:
  the retry reuses the same id, so it does not create a second stored copy. The
  id must be derived *deterministically from the event content or key*; a UUID
  or `time.Now()` generated per attempt defeats the whole mechanism.
- **Consumer side**: you must make your handler idempotent, or use `DoubleAck`
  so a lost ack cannot cause a redelivery-driven duplicate side effect. The
  broker does not deduplicate your side effects.

### Acknowledgement semantics are the whole game

With `AckExplicitPolicy` every delivered message needs an explicit terminal
signal, and which one you choose is your retry topology:

- **Ack**: processed successfully; the consumer advances past it.
- **Nak / NakWithDelay(d)**: negative acknowledgement; redeliver now, or after
  delay `d`. Use for a *retryable* failure (a transient downstream error).
- **Term / TermWithReason(reason)**: permanently stop redelivering this message,
  *regardless of MaxDeliver*. This is the poison-message / dead-letter signal.
  Use for a *permanent* failure (a message that will never succeed no matter how
  many times you retry, e.g. an un-decodable payload).
- **InProgress**: "still working" — resets the `AckWait` timer so a long-running
  handler is not treated as failed and redelivered mid-flight. It is a heartbeat,
  not a terminal action.
- **DoubleAck(ctx)**: like Ack, but waits for the server to confirm the ack was
  recorded. A plain Ack can be lost in flight; if it is, the message is
  redelivered and your side effect runs twice. `DoubleAck` closes that window at
  the cost of a round trip; use it in a read-process-write flow where a duplicate
  is harmful and the handler is not otherwise idempotent.

The cardinal rule: with explicit acks, *every* code path must reach exactly one
terminal action. A handler that returns without acking will hit `AckWait` and be
redelivered forever.

### Redelivery flow control

Four consumer knobs shape redelivery:

- **AckWait**: how long the server waits for an ack before redelivering. Server
  default is 30s.
- **MaxDeliver**: cap on delivery attempts. Server default is -1 (unlimited),
  which means a poison message with no `Term` loops forever.
- **BackOff**: a slice of per-attempt delays. When set, it *overrides* AckWait
  for redelivery timing of un-acked messages; the last interval repeats for any
  attempts beyond the slice length. Setting both AckWait and BackOff and
  expecting AckWait to govern timing is a common confusion — pick one model.
- **MaxAckPending**: the maximum number of outstanding un-acked messages. This is
  your backpressure lever. Once it is hit, the consumer stops receiving new
  messages until it acks some, which under load looks exactly like a stall. Size
  it to your handler concurrency and ack promptly.

`Metadata().NumDelivered` on a received message tells the handler which attempt
this is — it is how a consumer knows it is on a retry and can decide to give up.

### Pull consumers are the modern default

The `jetstream` package (`github.com/nats-io/nats.go/jetstream`) supersedes the
legacy `nats.JetStreamContext` API returned by `nc.JetStream()`. In the new
package almost every call takes a `context.Context`, and consumption is
pull-based: `Consume` (callback), `Messages` (iterator), or `Fetch` (batch). Pull
gives the client explicit control over batch size and flow, which makes
horizontal scaling and backpressure explicit rather than implicit. New code
should use the `jetstream` subpackage and pull consumers; push consumers and the
legacy API are legacy.

### Replay is a first-class capability

Rebuilding a read model from history is not a hack in JetStream; it is what a
consumer's `DeliverPolicy` is for. The start point can be `DeliverAllPolicy`
(from the beginning), `DeliverNewPolicy` (only new), `DeliverLastPerSubjectPolicy`
(snapshot of the last per subject), `DeliverByStartSequencePolicy` with
`OptStartSeq`, or `DeliverByStartTimePolicy` with `OptStartTime`. An
**ordered consumer** (`OrderedConsumer`) is an ephemeral consumer that guarantees
strictly ordered, gap-free delivery and transparently recreates itself on error —
exactly what you want for rebuilding a projection. During a replay,
`Metadata().NumPending` counts how many matching messages are still undelivered,
so `NumPending == 0` is the signal that the replay has caught up to the live tail
and the rebuild is complete.

### Keep decisions testable, keep I/O at the edges

The design lesson that runs through all three exercises: the JetStream-specific
glue (create a stream, bind a consumer, loop over messages) is thin and
mechanical. The parts that carry real production bugs are pure logic — deriving a
stable message id, deciding ack vs nak vs term from an error and a delivery
count, folding an event into a projection. Structure a broker integration so that
logic is ordinary deterministic Go you unit-test offline, and the network calls
are a thin adapter at the edge. In these exercises that means the pure functions
have plain offline tests, and the actual `nats.go` calls live behind a
`//go:build online` tag, run against a real server in a networked test.

## Common Mistakes

### Confusing core NATS with JetStream

Calling `nc.Publish` (core NATS) and expecting the message to be persisted.
Only `js.Publish`/`js.PublishMsg` to a subject that a stream captures is durable,
and the subject must match one of the stream's `Subjects`. A publish to a subject
no stream listens on is silently a core-NATS fire-and-forget.

### Using the legacy JetStream API in new code

Wrong: `js, _ := nc.JetStream(); js.AddStream(...)`. That is the legacy
`nats.JetStreamContext`. Fix: `js, _ := jetstream.New(nc); js.CreateOrUpdateStream(ctx, ...)`.
The `jetstream` subpackage is the current API and takes a `context.Context` on
every call.

### Assuming Ack is implicit

With `AckExplicitPolicy` a handler that returns without calling `msg.Ack()` will
hit `AckWait` and be redelivered — forever if `MaxDeliver` is unlimited. Fix:
make every path terminal — `Ack` on success, `Term` on poison, `Nak` on a
retryable error.

### Using Nak for a permanently bad message

`Nak` redelivers up to `MaxDeliver`, so a message that can never succeed
poison-loops until the ceiling. Fix: classify the error and `Term` (or
`TermWithReason`) non-retryable failures so the message is dropped immediately
instead of retried pointlessly.

### Non-deterministic message ids

Deriving the `Nats-Msg-Id` from `time.Now()` or a fresh UUID per attempt means a
producer retry carries a *different* id, so the dedup window never matches and the
retry creates a duplicate. Fix: derive the id deterministically from event
content or a business key, so a retry reuses the same id inside the `Duplicates`
window.

### WorkQueue retention for a stream you want to replay

The first ack deletes the message; a second consumer or a later replay finds
nothing. Fix: LimitsPolicy (or InterestPolicy) for replayable or fan-out streams;
WorkQueue only for single-owner queues.

### Leaking the consume loop

Ignoring the `ConsumeContext` returned by `Consume` (or the `MessagesContext`
from `Messages`) and never calling `Stop()` leaks goroutines and subscriptions.
Fix: `defer cc.Stop()`.

### Treating DoubleAck as needless overhead

In a read-process-write flow, a plain `Ack` can be lost, causing redelivery and a
duplicate side effect. Fix: use `DoubleAck(ctx)` where a duplicate would be
harmful and the handler is not otherwise idempotent.

### Expecting AckWait to govern timing when BackOff is set

When `BackOff` is set it drives per-attempt redelivery delays and `AckWait` is
effectively superseded. Fix: choose one model deliberately rather than setting
both and reasoning about the wrong one.

### Ignoring MaxAckPending under load

Once outstanding un-acked messages reach `MaxAckPending`, the consumer stops
receiving new messages and it looks like a stall. Fix: size `MaxAckPending` to
your handler concurrency and ack promptly.

Next: [01-durable-stream-publisher.md](01-durable-stream-publisher.md)
