# Multi-Stage Pipeline Cancellation — Concepts

Streaming pipelines are how backends process unbounded work: ingest -> validate
-> enrich -> persist/publish, each stage a goroutine linked to the next by a
channel. Wiring the channels is the easy part and not what separates a senior
engineer. The hard part — the part that decides whether the service survives
contact with production — is guaranteeing **bounded resources and deterministic
teardown under partial failure**. A pipeline that works on the happy path but
leaks a goroutine every time a request context is cancelled will, under load,
accumulate blocked goroutines until the process runs out of memory or schedulable
threads and falls over. A pipeline with no backpressure lets a fast producer
outrun a slow consumer and grows an unbounded in-flight buffer until it OOMs. A
pipeline that fans out one goroutine per item straight into a database exhausts
the connection pool and takes the database down with it.

So the real specification for a production pipeline is five properties, and every
one of them is a cancellation-and-resource concern, not a data-flow concern:

1. It never leaks a goroutine when the caller gives up — a request context is
   cancelled, a deadline fires, or an upstream stage errors.
2. It applies backpressure, so a fast producer cannot outrun a slow consumer and
   blow up memory.
3. It bounds concurrency, so fan-out cannot exhaust a connection pool or a
   downstream rate quota.
4. It drains in-flight work on graceful shutdown instead of dropping it.
5. It reports *why* it stopped — user cancel versus deadline versus stage error —
   as a first-class signal for logs, metrics, and alerting.

This file is the model behind all ten independent exercises that follow: the
three canonical stage shapes, a fan-out worker pool, a goroutine-leak harness, a
fan-in merge, bounded fan-out with `errgroup`, a batching stage, a rate-limited
egress stage, a cancellable retry stage, a graceful drain, and a cause-based
observability wrapper. Read it once and you have the reasoning for every module.

## Channel close ownership

The single most important ownership rule in Go concurrency: the goroutine that
*writes* a channel is the *only* one that may close it. Closing a channel from any
other goroutine is a data race on the channel's state and, if the writer is still
sending, panics with "send on closed channel"; a second close of an
already-closed channel panics unconditionally. There is no safe way for a
consumer to close a producer's output "to make it stop" — the consumer cannot
know the producer is done.

The stage pattern encodes this: create `out`, launch one goroutine whose *first*
line is `defer close(out)`, and return `out`. Close is now coupled to the
writer's exit, whatever causes that exit — the input range ending, an error
`return`, or a `return` on `ctx.Done()`. The downstream stage ranges over `out`
and its range ends exactly when the writer closes. To make a producer stop, you
never close its channel from outside; you cancel its context and let it close its
own output on the way out.

```go
func stage(ctx context.Context, in <-chan int) <-chan string {
	out := make(chan string)
	go func() {
		defer close(out) // the writer, and only the writer, closes out
		for v := range in {
			select {
			case out <- transform(v):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
```

## Preemptive cancellation requires select on every send

Cancellation is preemptive only where you make it preemptive. A stage that checks
`ctx.Done()` on the *receive* side but does a bare `out <- v` on the send side
still leaks: when the downstream stage exits (because it saw `ctx.Done()` first),
nothing reads `out`, and the bare send blocks forever. The goroutine is now stuck
on a send that will never complete, holding whatever it captured, invisible to
`range` but very visible to `runtime.NumGoroutine`.

The non-negotiable send shape is:

```go
select {
case out <- v:
case <-ctx.Done():
	return
}
```

Every blocking send in a stage must be wrapped this way. Omitting it is the single
most common pipeline goroutine leak, and it is exactly what the leak harness in
Exercise 3 is built to catch.

## Cancellation is cooperative, not preemptive at the runtime level

Closing `ctx.Done()` does not stop a goroutine. It only unblocks goroutines that
are *actively selecting on* `ctx.Done()`. A goroutine in a tight CPU loop, blocked
in a raw channel operation with no `ctx.Done()` case, or parked in a blocking
syscall ignores cancellation completely — the runtime will not interrupt it. This
is why "cancellation" in Go is a cooperative protocol: the context signals intent,
and every blocking point in a stage must have a `ctx.Done()` escape for that
intent to take effect. A stage is only as cancellable as its least-cancellable
blocking operation.

## Cause-carrying cancellation makes the reason first-class

`ctx.Err()` is lossy. Whether a context was cancelled by a user hanging up, by an
upstream stage rejecting a value, or by a shutdown signal, `ctx.Err()` collapses
them all to the same `context.Canceled`. In production that distinction is exactly
what you need: an SRE wants to alert on *stage-error* terminations and ignore
routine *user cancels*, and a retry policy must retry a transient failure but not
a user cancel.

`context.WithCancelCause` returns a `context.CancelCauseFunc` that takes an error;
`context.Cause(ctx)` returns that specific error. When a stage detects a permanent
failure, it calls `cancel(fmt.Errorf("%w: ...", ErrTransform, ...))` and returns;
every other stage sees `ctx.Done()` and exits, and the caller reads the typed
reason with `context.Cause`. This is why cause-carrying cancellation is the
production default over plain `WithCancel`: the reason is data, not a log string.

## Deadlines vs cancels: WithTimeoutCause

A deadline is just a cancellation on a timer. `ctx.Err()` on an expired deadline
returns the generic `context.DeadlineExceeded`. `context.WithTimeoutCause(parent,
d, cause)` lets the deadline carry a *typed* sentinel, so `context.Cause`
distinguishes "this specific deadline fired" from an unrelated stage error even
though both look like a cancel to `ctx.Err()`. Prefer `WithTimeoutCause` whenever
the reason for a timeout matters downstream — for logging which budget was blown,
or for a metric label.

## Backpressure is a feature, not a limitation

An unbuffered (or small-buffered) channel is a synchronization point: the producer
blocks on send until the consumer is ready to receive. That coupling *is*
backpressure, and it is what bounds in-flight memory — a producer physically
cannot get more than a channel-buffer's worth ahead of its consumer. Engineers
new to channels often "fix" a slow pipeline by adding a large buffer or, worse,
dropping the select-send and letting the producer race ahead. That does not make
the consumer faster; it just moves the queue from the channel into unbounded
memory and converts a merely-slow pipeline into an OOM. Choose buffer sizes to
smooth bursts, not to decouple producer from consumer.

## Bounded fan-out is a resource-safety requirement

Fan-out — N workers pulling from one channel — is how you parallelize a slow
per-item operation. Unbounded fan-out — one fresh goroutine per item, each opening
a DB connection or calling a downstream API — is how you exhaust a connection pool
and trip a rate quota. The bound is not an optimization; it is a correctness
constraint set by the downstream's real capacity. Cap in-flight work with a fixed
worker count, a semaphore channel, or `errgroup.SetLimit(n)`, and size the limit
to what the downstream can actually absorb.

## errgroup gives fail-fast coordination for free

`errgroup.WithContext(ctx)` returns a group and a derived context that is
cancelled the instant any `Go`-launched function returns a non-nil error;
`group.Wait()` returns that first error. That is fail-fast fan-out with sibling
cancellation, hand-rolled `WaitGroup` + `CancelCause` collapsed into a few lines.
`SetLimit(n)` bounds concurrency; `TryGo` starts a function only if the group is
below its limit, returning `false` otherwise. The one caveat: `Wait` returns only
*one* untyped error — the first. When callers need a typed reason, or need to know
*which* stage failed, pair errgroup with `context.Cause` (feed the same error into
a `CancelCauseFunc`) or record per-stage errors explicitly.

## Fan-in mirrors fan-out

Merging M upstream channels into one downstream channel is fan-out inverted, and
it has the same close-ownership problem: many forwarders write the merged output,
but exactly one goroutine may close it, and only after every forwarder has exited.
The shape is a `sync.WaitGroup` counting the M forwarders and one closer that
`Wait`s then `close`s. Each forwarder must select-send + `ctx.Done()` so a cancel
drains all M inputs and the merged output ends cleanly, with no hung forwarder and
no double close.

## Timer hygiene inside stage loops

`time.After(d)` allocates a fresh `*time.Timer` every call and never stops it
early; used inside a per-element loop it produces one allocation per item — at
high throughput, garbage by the million, each timer living until it fires. The fix
is one `time.NewTimer` created *outside* the loop, reused each cycle with
`Stop`-then-drain-then-`Reset`. And for any wait — a backoff sleep, a flush
interval — select over `timer.C` *and* `ctx.Done()` so a cancel preempts the wait
instead of blocking for its full duration. A `time.Sleep` in a stage is a
cancellation black hole: the goroutine sleeps straight through a shutdown.

Reusing a timer correctly has a sharp edge: after `Stop()` returns `false` (the
timer already fired), its channel may hold a stale value you must drain before
`Reset`, or the next cycle wakes immediately on the old fire. Guard the drain with
a non-blocking `select { case <-t.C: default: }`.

## Graceful drain separates "stop accepting" from "stop working"

On `SIGTERM`, dropping in-flight jobs is often unacceptable (a half-written
record, a half-published batch). Graceful drain splits the shutdown into two
distinct actions: *stop accepting* new work (close the intake so no new items
enter) and *let in-flight work finish* under a bounded budget. The trap is
cancelling the very context the stages run on — that kills in-flight items
mid-write. Instead, derive a drain context with `context.WithoutCancel(parent)`,
which produces a context that does *not* inherit the parent's cancellation, so the
drain survives the shutdown that triggered it. Bound it with
`context.WithTimeout` (or `WithTimeoutCause`) for the drain budget, and use
`context.AfterFunc` to hard-cancel stragglers that overrun. `Cause` then tells you
whether the drain finished cleanly or was cut off by its deadline.

## Rate limiting inside a stage must be cancellation-safe

To keep a fan-out under a downstream API's requests-per-second, gate emission
through a `golang.org/x/time/rate.Limiter`. The cancellation-safe primitive is
`limiter.Wait(ctx)`: it blocks until a token is available *or* `ctx` is cancelled,
returning promptly in the latter case. `Allow()` is non-blocking and drops when no
token is available; `Reserve()` returns a reservation with a delay you must honor
yourself. A naive `time.Sleep` between emits both ignores cancellation and does
not compose with a burst allowance. Inside a cancellable stage, `Wait(ctx)` is the
right tool; `Allow`/`Reserve` are for load-shedding and custom scheduling.

## Goroutine leaks are the primary failure mode — and they are testable

Every property above ultimately shows up as a goroutine that either exits or does
not. That makes leaks the primary failure mode *and* the primary thing to test.
The harness: snapshot `runtime.NumGoroutine()` before, run and tear down the
pipeline (cancel, then drain the output so blocked sends unblock), then poll until
the count settles back to baseline within a small threshold — a poll-until-settle
loop is far less flaky than a fixed `time.Sleep`, which is a bet on scheduler
timing that loses on a loaded CI box. Always run the pipeline tests under `-race`;
a stage that closes from the wrong goroutine or shares state without
synchronization shows up there even when the leak test is green.

## Common Mistakes

### Closing a channel from the wrong goroutine

Wrong: the consumer closes the producer's output when it decides to stop reading.
If the producer is still sending, this panics with "send on closed channel"; a
second close panics unconditionally. Fix: only the writer closes. Signal the
writer to stop via `ctx` and let it close its own output on exit.

### No select on the send path

Wrong: `for v := range in { out <- f(v) }`. When the downstream stage exits on
`ctx.Done()`, this send blocks forever and the goroutine leaks. Fix: wrap every
send in `select { case out <- f(v): case <-ctx.Done(): return }`.

### Using time.After inside a per-element stage loop

Wrong: `select { case <-time.After(d): ...; }` inside a loop over every element —
one timer allocated per item, never stopped early. Fix: one `time.NewTimer`
outside the loop with `Stop`+drain+`Reset`, or `context.WithTimeout` for an
overall deadline.

### Ignoring context.Cause and checking only ctx.Err()

Wrong: after the pipeline stops, reading only `ctx.Err()` — user-cancel and
stage-error both surface as `context.Canceled`, so logs and metrics cannot tell a
routine shutdown from a real failure. Fix: use `context.Cause` (with
`WithCancelCause` / `WithTimeoutCause`) to retrieve the specific reason.

### Unbounded fan-out

Wrong: one goroutine per item straight into a DB or downstream API — exhausts the
connection pool and trips rate limits. Fix: bound concurrency with
`errgroup.SetLimit(n)` or a semaphore sized to real downstream capacity.

### time.Sleep for backoff or rate limiting

Wrong: `time.Sleep` between emits or between retry attempts — the goroutine sleeps
through a cancellation and delays shutdown. Fix: select over a timer channel and
`ctx.Done()`, or use `rate.Limiter.Wait(ctx)`.

### Draining a reused timer wrong

Wrong: calling `Reset` on a timer whose channel still holds a stale fire — the
next cycle wakes immediately. Fix: after `Stop()` returns `false`, drain with a
non-blocking `select { case <-t.C: default: }` before `Reset`.

### Treating errgroup's Wait error as the whole story

Wrong: assuming `Wait`'s error is the complete failure picture — it is only the
first error, so a second, more important failure or the identity of the failing
stage is lost. Fix: when that matters, record per-stage errors or carry a typed
reason via `context.Cause`.

### Dropping in-flight work by cancelling the stages' own context on shutdown

Wrong: on `SIGTERM`, cancelling the same context the stages use — kills in-flight
items mid-write. Fix: separate intake-close from work-cancel; derive the drain
context with `context.WithoutCancel` plus a drain deadline so accepted work
finishes.

### Relying on a fixed time.Sleep in leak tests

Wrong: sleeping a fixed duration before reading `NumGoroutine` — flaky on loaded
CI. Fix: poll until the count settles (a bounded retry loop), and always run the
pipeline tests under `-race`.

Next: [01-stage-primitives-generate-transform-collect.md](01-stage-primitives-generate-transform-collect.md)
