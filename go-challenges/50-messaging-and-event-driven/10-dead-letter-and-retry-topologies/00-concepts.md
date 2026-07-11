# Dead-Letter Queues and Retry Topologies — Concepts

A senior backend engineer almost never gets to "just retry". Retry is not a
single decision; it is a *topology*: which failures are transient and worth
another attempt, which are poison and will never succeed, how long to wait
between attempts so a blip does not become a stampede, how much total effort to
spend before giving up, and — critically — where a message goes when it has
exhausted every attempt. Get any of these wrong and the failure modes are
severe: a poison message loops forever and starves a partition, an
un-jittered retry storm turns a five-second dependency blip into a ten-minute
outage, or a message silently vanishes because the "dead-letter queue" was a
black hole nobody watched. This file is the model. The same invariants recur
across Kafka, RabbitMQ, SQS, NATS JetStream, Redis Streams, and Postgres job
queues; only the API surface changes. Read it once and the three exercises
that follow — a broker-agnostic retry engine, a JetStream DLQ relay, and a
Redis Streams poison reclaimer — are variations on one theme.

## Concepts

### At-least-once is the premise, so idempotency is the precondition

Every DLQ/retry design starts from one broker guarantee: at-least-once
delivery. A broker redelivers a message whenever it does not see an
acknowledgement in time (a crash, a slow handler, a dropped ack). That is what
makes retries possible at all — but it also means a handler can run more than
once for the same message. Retries are only *safe* because processing is
idempotent; retries plus a non-idempotent handler multiply side effects into
double charges and duplicate emails. This is why the inbox/dedupe pattern
(lesson 07) is a precondition for aggressive retries, not an optional extra.
The order is: make the handler idempotent first, then turn up the retries.

### Transient versus poison is the central classification

The single most important decision in the whole topology is sorting a failure
into one of two buckets. A *transient* (retryable) failure — a timeout, a 503,
a database deadlock, a connection reset — may well succeed on the next attempt
because the cause is temporary. A *poison* (terminal) failure — a
schema-invalid payload, a business-rule rejection, a 4xx client error — will
*never* succeed no matter how many times you retry, because nothing about
retrying changes the input. Retrying a poison message is the classic,
catastrophic failure mode: an infinite redelivery loop that head-of-line-blocks
the partition, burns the retry budget, and never reaches a DLQ. The correct
action for poison is to terminate or park it *immediately* — in JetStream terms
`Term`, not `Nak`; in a job queue, mark it discarded, not retryable. A useful
third bucket sits between them: *rate-limited*, where the dependency explicitly
asks you to wait (an HTTP 429 with `Retry-After`); it is retryable, but not
before the stated delay.

### Backoff schedule design: exponential, capped, jittered

Once you have decided to retry, the question is *when*. Fixed-delay retries are
the worst option: every failed client wakes on the same cadence and hits the
recovering dependency in synchronized waves. Exponential backoff — `base *
2^attempt` — spreads attempts out over time, which is much better, but it does
*not* solve synchronization: a thousand clients that all failed at the same
instant compute the same exponential delay and still fire together. The fix is
*jitter*, randomness added on top of the interval. Two standard forms:
*full jitter* picks a uniform random delay in `[0, interval]`, and *equal
jitter* uses `interval/2 + random[0, interval/2]`, which keeps a floor so a
retry is never near-immediate. Two caps are mandatory. A per-attempt cap
(`MaxInterval`) stops `2^n` from growing into multi-hour delays or overflowing
the duration type. A total cap (`MaxRetries` and/or `MaxElapsedTime`) stops a
message from retrying forever. A schedule with no total cap is just a slower
infinite loop.

### Retries amplify load, so bound them with a budget

Retries have a dark side that only shows up under stress. During an outage,
`N` retries turn one failed request into `N+1` attempts against a dependency
that is *already* struggling. Scale that across every client and the retry
traffic alone can keep the dependency down long after the original trigger
cleared — a *retry storm*, the engine of a *metastable failure* that outlives
its cause. Two mechanisms bound the amplification. A *retry budget* caps
retries as a small fraction of total traffic (a token-bucket that refills
slowly, so when everything is failing the budget empties and most requests get
one attempt, not five). A *circuit breaker* stops calling a dependency
entirely once it looks dead, shedding load so it can recover. Without one of
these, aggressive retries prolong exactly the outages they were meant to
survive.

### Redelivery counters: how you know when to give up

To stop retrying you need to count attempts, and every broker exposes that
count differently — this is a frequent source of off-by-one bugs. JetStream
gives you `Msg.Metadata().NumDelivered` and a `ConsumerConfig.MaxDeliver`
ceiling. Redis Streams exposes a per-entry `RetryCount` through `XPENDING`. SQS
has `ApproximateReceiveCount` compared against a redrive policy's
`maxReceiveCount`. Kafka has *no* native counter at all; you track attempts in
message headers or build a chain of retry topics. The trap is knowing whether
the counter is pre- or post-increment for your broker: comparing it with the
wrong operator DLQs a message one attempt early, or grants it one extra
delivery past the limit. Whenever you wire a counter, write the test that pins
the exact boundary.

### A DLQ is durable parking, not a sink

A dead-letter queue exists so that a message which exhausted its retries lands
somewhere *durable and inspectable* instead of being dropped or looping. The
word "queue" is misleading: the point is not throughput, it is that a human or
an automation can later inspect the message, fix the root cause, and *redrive*
(replay) it back into normal processing. A DLQ with no alerting and no redrive
path is worse than useless — it is a slower, quieter data-loss mechanism that
hides the loss behind a growing number nobody reads. Two things make a DLQ
real. First, *enrichment*: park the message with a failure envelope — the
reason, the attempt count, first-seen/last-seen timestamps, the last error, and
the original subject/topic — so triage does not require guesswork. Second, a
*redrive path*: tooling that moves entries from the DLQ back to the live stream
after a fix. The last exercise builds exactly this, because it is the on-call
tooling teams wish they had at 3am.

### The invariants are identical; only the primitives change

The reason this lesson is broker-agnostic is that the mechanisms differ but the
invariants do not. RabbitMQ builds a delay ladder from dead-letter exchanges
plus per-queue TTL "retry queues". SQS attaches a redrive policy that points at
a DLQ after `maxReceiveCount`. JetStream combines `MaxDeliver` + `BackOff` with
a `MAX_DELIVERIES` advisory you relay into a DLQ stream. Redis Streams gives you
no DLQ at all — you build the reclaimer (`XAUTOCLAIM`) and the DLQ stream
yourself. Watermill wraps the whole thing in `Retry` + `PoisonQueue`
middleware. Underneath, every one of them is: at-least-once delivery, a
redelivery counter, a backoff ladder, terminal parking, and a redrive. Learn
the invariants and each broker is a mapping exercise.

### Ordering versus retry is a real tension

On a partitioned or strictly-ordered stream — a Kafka partition, a JetStream
ordered consumer, a single Redis Streams consumer — blocking-retry of one
message stalls the head of the line for everything behind it. You cannot both
preserve strict order and retry-in-place without risking a stall. There are two
honest choices, and you must make one consciously. *Side-line* the poison
message to a retry or DLQ topic and advance the stream: this preserves
throughput but breaks strict ordering for the side-lined message. Or *accept
head-of-line blocking*: preserve order at the cost of a possible stall while one
message retries. There is no third option that gives you both; pretending
otherwise is how ordered pipelines mysteriously stop.

### In-broker delay beats in-process sleep

A subtle but important implementation choice: where does the backoff wait
happen? The tempting, wrong answer is to `time.Sleep` inside the handler. That
holds the delivery lease (the `AckWait`/visibility window) while the worker
sleeps, so two things go wrong at once — the worker slot is wasted doing
nothing, and if the sleep outlasts the lease the broker concludes the message
was lost and redelivers it *to another consumer*, producing duplicate
processing. The right answer pushes the wait back to the broker:
`NakWithDelay`, a delay queue, SQS visibility-timeout extension, River's
`snooze`. For anything beyond sub-second backoff, always prefer broker-side
delay so the worker is freed and the lease is not held.

### Observability is part of the topology

Metrics are not an add-on here; they are how you know the topology is working.
Three signals matter: DLQ *depth* (how many parked messages), *redelivery
rate* (how often messages are being retried), and *time-to-DLQ* (how long a
message struggles before it is parked). A rising DLQ depth or a redelivery
spike is the earliest, clearest sign of a poison deploy or a failing
dependency — earlier and clearer than scanning error logs. Wire alerts on these
gauges. A DLQ you do not alert on is invisible, and an invisible DLQ is the
data-loss mechanism described above.

## Common Mistakes

### Retrying poison messages forever

Wrong: treating every error the same and `Nak`-ing (or leaving unacked)
whatever fails, including a message with a malformed payload. The broker
redelivers it endlessly; on an ordered stream it head-of-line-blocks everything
behind it and never reaches a DLQ. Fix: classify errors, and `Term`/park
terminal failures immediately instead of retrying them.

### Exponential backoff with no jitter

Wrong: `delay = base * 2^attempt` with no randomness. After a dependency blip
that failed a thousand clients at once, all thousand compute the identical delay
and retry in a synchronized wave — a thundering herd that re-knocks the
dependency over. Fix: apply full or equal jitter over the computed interval so
the retries spread out in time.

### No interval cap and no total cap

Wrong: an uncapped `base * 2^attempt` that grows into multi-hour delays (or
overflows the duration type), with no `MaxRetries`/`MaxElapsedTime` so a
message retries forever. Fix: cap each attempt with `MaxInterval` and cap the
total effort with `MaxRetries` and/or `MaxElapsedTime`.

### Sleeping in the handler for the backoff

Wrong: `time.Sleep(delay)` inside the handler. It holds the ack lease past
`AckWait`/visibility, so the broker redelivers the message to another consumer
while the first is still sleeping — duplicate processing — and wastes a worker
slot the whole time. Fix: use broker-side delayed redelivery (`NakWithDelay`,
delay queues, `snooze`) so the wait is off the worker.

### Off-by-one on the delivery counter

Wrong: comparing `NumDelivered`/`RetryCount` against the limit with the wrong
operator, so the message either DLQs one attempt early or gets one extra
delivery past `MaxDeliver`. Fix: know whether the counter is pre- or
post-increment for your broker and pin the exact boundary with a test.

### Treating the DLQ as a black hole

Wrong: routing exhausted messages to a DLQ with no alert on its depth and no
redrive tooling. Parked messages are effectively lost; the growing depth is a
number nobody watches. Fix: alert on DLQ depth and time-to-DLQ, and ship a
redrive path so parked messages can be replayed after a fix.

### Retrying without idempotency

Wrong: enabling aggressive retries over an at-least-once broker while the
handler has observable side effects. Every redelivery repeats the side effect —
double charges, duplicate notifications. Fix: make the handler idempotent (or
dedupe via an inbox) before turning retries up.

### No retry budget or circuit breaker

Wrong: unbounded retries with no budget. During an outage the retry traffic
amplifies the load `N`-fold and keeps the dependency down — a metastable
failure that outlives its trigger. Fix: bound retries as a fraction of traffic
with a token-bucket budget, and shed load with a circuit breaker when the
dependency is down.

### Losing context on the way to the DLQ

Wrong: parking only the raw payload, with no reason, attempt count, or original
topic. Triage becomes archaeology. Fix: wrap the message in a failure envelope
carrying the reason, attempts, timestamps, last error, and original
subject/topic.

### Non-deterministic jitter in tests

Wrong: using the real global `rand` and real time in the retry code, so the
ladder is different on every run and the tests cannot assert exact delays. Fix:
inject a seeded source (`math/rand/v2` `rand.NewPCG`) and a clock function, so
the schedule is reproducible and the boundaries are testable.

Next: [01-retry-policy-engine.md](01-retry-policy-engine.md)
