# Event-Driven Pipelines with Watermill — Concepts

Every team that runs consumers eventually hand-rolls the same scaffolding around
each one: pull a message, attach a correlation id, recover from panics so one bad
payload does not kill the goroutine, retry transient failures with backoff, give
up after N attempts and shunt the message somewhere it can be inspected, and shut
down without dropping in-flight work. Watermill is the library that turns that
scaffolding into a small, ordered, reusable middleware stack sitting in front of a
business handler you write once. The senior skill this lesson teaches is not
"another Kafka client" — it is composing those cross-cutting concerns correctly,
reasoning precisely about middleware *order* and at-least-once redelivery, and
writing handlers that depend only on interfaces so the same code runs against an
in-memory transport in tests and a real broker in production.

The exercises deliberately use the in-memory GoChannel transport so they are
hermetic and deterministic: no broker, no network, nothing to flake in CI. The
final exercise shows the identical router wired to Redis Streams behind a build
tag — the day-job task of "make our event consumer infra-portable and testable."

## Watermill is a router, not a broker

Watermill does not store messages, does not give you exactly-once, and does not
provide its own delivery guarantees. It is a *router plus middleware framework*.
The unit of orchestration is `message.Router`, which wires one or more handlers,
each of the shape:

```
Subscriber -> [middleware chain] -> HandlerFunc -> Publisher
```

The transport — GoChannel, Kafka, NATS JetStream, Redis Streams — is pluggable
behind two interfaces, `message.Publisher` and `message.Subscriber`. Whatever
durability, ordering, and redelivery you get comes from the transport, not from
Watermill. Internalize this boundary: GoChannel is an in-process fan-out for
tests and in-process eventing; it is not a durable broker, and Watermill's own
retry is in-memory. Durable retry and exactly-once-in-effect come from the
transport's redelivery combined with idempotent handlers, which is exactly why
the outbox and inbox patterns in the next two lessons exist.

## The message.Router pipeline

A handler is registered with `AddHandler(name, subscribeTopic, subscriber,
publishTopic, publisher, handlerFunc)`. The router subscribes to
`subscribeTopic`, runs each incoming message through the middleware chain and
then the `HandlerFunc`, and publishes whatever messages the handler returns to
`publishTopic`. A `HandlerFunc` has the signature
`func(msg *message.Message) ([]*message.Message, error)`: return the produced
messages and `nil` to acknowledge the input, or return an error to negatively
acknowledge it. A consumer that produces nothing uses
`AddNoPublisherHandler(name, subscribeTopic, subscriber, handlerFunc)` whose
handler is `func(msg *message.Message) error` — there is no publish topic and no
return slice, so trying to "return messages to publish" from it is a category
error.

`Router.Run(ctx)` blocks: it starts every handler, closes the channel returned by
`Router.Running()` once they are all up, and keeps running until `ctx` is
cancelled. That `Running()` channel is the synchronization point tests use to
know the router is ready before they publish.

## Middleware order is semantics, not style

`Router.AddMiddleware(m...)` (router-wide) and `Handler.AddMiddleware(m...)`
(one handler only) both take an ordered list. The chain wraps the handler
*outermost-first*: a list `[A, B, C]` produces `A(B(C(handler)))`. `A` runs first
on the way in and last on the way out; `C` is closest to the handler. Getting the
order wrong does not produce a compile error — it silently changes behavior. Two
orderings matter enough to memorize:

- **Retry must wrap Recoverer** (add `Retry` before `Recoverer`). `Recoverer`
  catches a panic and returns it as an ordinary error. For `Retry` to *see* that
  error and retry, `Recoverer` has to be *inside* the retry loop, i.e. added
  after `Retry`. If you add `Recoverer` first (outermost), a panic unwinds
  straight past the retry loop and is caught once but never retried. The official
  Watermill example orders them `CorrelationID, Retry, Recoverer` for exactly
  this reason.

- **PoisonQueue must wrap Retry** (add `PoisonQueue` before `Retry`). The poison
  middleware, when its wrapped handler returns an error, publishes the message to
  the dead-letter topic and then returns `nil` — it *swallows* the error so the
  message is acked and not redelivered forever. Therefore `Retry` must sit
  *inside* `PoisonQueue`: the retry loop runs to exhaustion first, and only the
  final error reaches `PoisonQueue`. Invert them and `PoisonQueue`, being inside
  the loop, swallows the very first failure and returns `nil`, so `Retry` sees
  success and never retries — your DLQ fills up on the first attempt.

`CorrelationID` belongs near the outside so the id is attached before inner layers
log or produce, and `Timeout` typically goes innermost so each individual attempt
gets its own deadline rather than the whole retry sequence sharing one.

## Ack/Nack and at-least-once

A handler returning `nil` acks the message; returning an error nacks it. A nack,
on a real transport, means the message reappears later (redelivery). The
unavoidable consequence is that **handlers must be idempotent**: the same message
may be delivered more than once, on redelivery after a nack or after a consumer
crash between processing and ack. At-least-once is the default reality of message
systems; exactly-once "delivery" does not exist end to end, only
exactly-once *effect*, which you build with idempotent handlers plus dedup
(the inbox pattern). GoChannel, being in-process, is a fan-out to all current
subscribers and is not a durability substitute.

## Retry is in-memory and per-process

`middleware.Retry` blocks the handler's goroutine and sleeps between attempts with
exponential backoff (`InitialInterval` scaled by `Multiplier`, capped at
`MaxInterval`), nacking only after `MaxRetries` is exhausted. This retry state
lives entirely in memory: if the process dies mid-backoff, the pending retries are
gone. In-memory retry is a convenience for smoothing over brief blips, not a
substitute for the broker's own redelivery. Durable retry means an unacked message
reappears from the transport and a handler that can safely reprocess it.

## Poison queue as failure isolation

Without a dead-letter path, a permanently-failing "poison" message is redelivered
forever, and on an ordered partition it blocks every message behind it — one bad
payload starves a whole consumer. `middleware.PoisonQueue(pub, topic)` routes a
message whose handler has exhausted its retries to a dedicated topic, tagging it
with diagnostic metadata — `reason_poisoned` (the error string), `topic_poisoned`,
`handler_poisoned`, `subscriber_poisoned` — so the main flow keeps moving and the
failure is observable and replayable. A second consumer on the poison topic can
alert, log, or feed a replay tool.

## Transient versus permanent failures

Retrying a validation error (a malformed payload will never become valid) wastes
time and delays dead-lettering; *not* retrying a network blip drops recoverable
work. The two need different handling. `PoisonQueueWithFilter(pub, topic,
shouldGoToPoisonQueue)` takes a predicate over the error: when it returns true the
message is dead-lettered, when false the error is propagated so the surrounding
`Retry` (or the transport) can retry it. Placed *inside* `Retry`, a filter that
returns true for permanent errors sends them straight to the DLQ on the first
attempt while transient errors fall through and are retried. `Retry.ShouldRetry`
is the complementary hook that classifies at the retry layer itself.

## Graceful shutdown

`Router.Run(ctx)` returns when `ctx` is cancelled: it stops accepting new
messages, drains in-flight handlers up to `RouterConfig.CloseTimeout`, then
returns `nil`. Wire it to `signal.NotifyContext(ctx, os.Interrupt,
syscall.SIGTERM)` so a `SIGTERM` from an orchestrator triggers a clean drain.
Skipping this — calling `os.Exit`, or dropping the process without cancelling and
honoring `CloseTimeout` — nacks in-flight messages, which on an at-least-once
transport means duplicate reprocessing on the next start.

## Correlation and observability

`middleware.CorrelationID` reads the correlation id from an incoming message's
metadata and copies it onto every message the handler produces, so a single id
flows across a multi-hop pipeline. `middleware.MessageCorrelationID(msg)` reads
it back and `middleware.SetCorrelationID(id, msg)` sets it on a message you are
about to publish. Combined with structured metadata, this is how you trace an
event chain that fans out across topics.

## Throttle and Timeout as backpressure and safety

`middleware.NewThrottle(count, duration)` caps how many messages a handler
processes per interval, protecting a fragile downstream from a burst.
`middleware.Timeout(d)` cancels the message's context after `d` so a stuck handler
cannot hold a processing slot forever — provided the handler actually watches
`ctx.Done()`. Both are cross-cutting concerns that belong in the middleware stack,
not copy-pasted into every handler body.

## Transport portability is the payoff

Because a handler and its wiring depend only on `message.Publisher` and
`message.Subscriber`, the same router runs against GoChannel in unit tests — fast,
hermetic, deterministic — and against Kafka, Redis Streams, or NATS in production
by swapping only the constructor that builds the pub/sub. Handlers and middleware
do not change. This is what makes event-driven code testable without standing up a
broker, and it is the concrete deliverable of the third exercise.

## Common Mistakes

### Adding middleware in the wrong order

Wrong: `AddMiddleware(PoisonQueue, ...)` after `Retry`, so the poison middleware
is inside the retry loop and dead-letters on the first failure; or `Recoverer`
before `Retry`, so panics are caught but never retried.

Fix: order is `CorrelationID, PoisonQueue, Retry, Recoverer, Timeout` from
outermost to innermost. `PoisonQueue` outside `Retry` (retries exhaust first);
`Recoverer` inside `Retry` (recovered panics become retryable errors).

### Treating in-memory Retry as durable

Wrong: relying on `middleware.Retry` to survive a restart. Its backoff state is in
memory; process death discards it.

Fix: durability comes from the transport's redelivery plus an idempotent handler.
Use in-memory retry only to smooth over sub-second blips.

### Non-idempotent handlers on at-least-once transports

Wrong: incrementing a balance or inserting a row unconditionally, assuming each
message arrives once. A nack or crash triggers redelivery and the effect is
applied twice.

Fix: make the handler idempotent — dedupe on a message id (the inbox pattern) or
use an upsert keyed by a natural id.

### Using GoChannel as if it were a broker

Wrong: expecting `gochannel.GoChannel` to persist messages, replay for late
subscribers by default, or provide consumer-group offsets.

Fix: it is in-process and non-persistent (`Persistent: false` by default) with no
offset semantics; use it for tests and in-process eventing, and a real broker for
production.

### Forgetting graceful shutdown

Wrong: killing the process without cancelling `Run`'s context, so in-flight
messages are nacked and reprocessed after restart.

Fix: derive the context from `signal.NotifyContext` and set a `CloseTimeout` long
enough to drain in-flight handlers.

### Dropping constructor errors

Wrong: ignoring the second return value of `NewRouter`, `PoisonQueue`, or
`PoisonQueueWithFilter`. An empty poison topic returns
`middleware.ErrInvalidPoisonQueueTopic`, and silently discarding it hides the
misconfiguration until messages vanish.

Fix: check and propagate every constructor error.

### Retrying permanent errors

Wrong: sending validation failures through the same unconditional `Retry`, burning
attempts on payloads that will never succeed and delaying the DLQ.

Fix: classify with `PoisonQueueWithFilter` or `Retry.ShouldRetry` so only
transient errors are retried and permanent ones are dead-lettered immediately.

### Coupling handlers to a concrete transport

Wrong: taking a `*gochannel.GoChannel` or a `*kafka.Publisher` as a handler
dependency, which pins the code to one transport and destroys testability.

Fix: depend on `message.Publisher` and `message.Subscriber`; choose the concrete
type in one wiring function.

Next: [01-router-middleware-pipeline.md](01-router-middleware-pipeline.md)
