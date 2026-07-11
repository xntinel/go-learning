# Building a Context-Aware Service Framework — Concepts

Every Go backend service, once it grows past a single `main()` that dials a
database and calls `http.ListenAndServe`, quietly re-implements the same object:
a lifecycle manager. It registers components, starts them in an order that
respects their dependencies, blocks until a signal arrives, and then tears them
down in the reverse order so nothing closes a resource another component still
holds. This chapter's earlier lessons taught the primitives in isolation —
`select`, `ctx.Done()`, `WithCancel`, `WithTimeout`, value propagation, graceful
shutdown. This capstone assembles them into the manager itself, and the whole
lesson is about *operability*, not syntax: the difference between a framework
that survives a hung dependency, a flaky upstream, and a rolling deploy, and one
that leaves a port bound and drops live requests.

Read this file once. The ten exercises that follow are each an independent,
self-contained slice of a real production `main()` / `internal/app` package, and
every idea they rely on is explained here first.

## The Service contract is the single seam

The whole framework hangs off one interface:

```go
type Service interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}
```

`Name` is for logs and health output. `Start` is called once at boot; its
contract is the part engineers get wrong. `Start` must *not block
indefinitely*: it launches whatever goroutines the component needs (an HTTP
`Serve` loop, a ticker worker, a consumer) and returns as soon as the component
is running. If `Start` runs the work loop itself, `Start` never returns, and no
later service ever starts. `Stop` is the mirror: it must complete within a
bounded budget, because it runs in a sequence and a `Stop` that blocks forever
starves every teardown step queued behind it — the connection pool never closes,
the transaction never commits, the listener never releases its port.

## Registration order is dependency order

The cheapest correct dependency model is a list. Services start in the order
they are registered and stop in the exact reverse order. There is no topological
sort and no dependency graph: the operator encodes the dependency edges by
choosing the registration sequence. Register the database before the connection
pool, the pool before the HTTP server, the HTTP server before the background
workers. Teardown then unwinds `workers → http → pool → db`, so by the time the
pool closes, the HTTP server has already stopped accepting requests and no
handler can touch a closed connection. Reverse-order teardown is not a nicety;
it is what makes "close the pool" safe. A test must pin this invariant, because
it is the single most common thing a refactor silently breaks.

## Stop must get a fresh context, never the root

By the time teardown runs, the root context is already cancelled — that
cancellation is *why* teardown is running. If you hand that cancelled context to
`Stop`, `Stop` gets zero budget: `server.Shutdown(rootCtx)` returns
`context.Canceled` immediately without draining a single connection. The
framework must give each `Stop` a *fresh* budget:
`context.WithTimeout(context.Background(), cfg.StopTimeout)`. `context.Background()`
is the root here precisely because it is never cancelled; the timeout is the only
bound. Reusing the request/root context for cleanup is the classic
"graceful shutdown that drains nothing" bug.

## Failure severity is first-class configuration

Not every dependency is load-bearing. An HTTP server is critical: without it the
process serves no clients, so a failed `Start` must abort boot and unwind the
services already started. A metrics exporter is not: if its `Start` fails, you
log it and keep serving traffic, because dropping observability is better than
dropping availability. Collapsing the two is a bug in either direction —
treating everything critical means a dead metrics sidecar takes down traffic;
treating everything non-critical means a missing database is silently ignored and
every request 500s. `Config{Critical bool}` makes the choice explicit per
service, and the framework branches on it during startup.

## Bounded startup: a hung dependency must not wedge boot

`Start` calls the outside world — a DB dial, a migration, a service-discovery
lookup — and the outside world hangs. Without a per-service `StartTimeout`, a
stuck dial blocks boot forever with no signal: the process is neither up nor
crashed, so the orchestrator's liveness probe eventually kills it with no useful
diagnostic. `context.WithTimeoutCause(ctx, budget, cause)` bounds the `Start`
call and, when it fires, `context.Cause(ctx)` returns *your* cause value — a
sentence naming exactly which dependency stalled and why. That turns "boot hung"
into "boot aborted: dependency \"orders-db\" did not become ready within 5s",
which is the difference between a five-minute and a five-hour incident. The
framework must also distinguish the two failure modes — "`Start` returned an
error" versus "`Start` exceeded its deadline" — and report them differently,
because they point at different root causes.

## Supervised restart needs capped backoff with jitter

Some dependencies are flaky by nature: a message broker that restarts, an
upstream that sheds load. A supervisor restarts a component whose run loop exits
with a non-nil error, but a naive `for { run() }` retry loop is a self-inflicted
outage. You need three things. First, exponential backoff so a persistently-down
dependency is not hammered thousands of times a second. Second, a *cap* on that
backoff so the delay does not grow to hours. Third — and this is the one people
omit — *jitter*: without it, every replica that lost the same dependency retries
in lockstep, producing a synchronized reconnect storm ("thundering herd") that
knocks the dependency back down the instant it recovers. Full jitter (sleep a
random duration in `(0, cap]`) decorrelates the fleet. A stable-uptime reset
window keeps a component that ran fine for an hour from inheriting the backoff of
an unrelated failure a day ago. And every wait must be a `select` on both the
backoff timer *and* `ctx.Done()`, so a shutdown that arrives mid-backoff exits
instantly instead of blocking until the timer fires.

## Readiness is not liveness

Two probes answer two different questions. *Liveness* asks "is the process
alive?" — if not, restart it. *Readiness* asks "should this instance receive
traffic right now?" — if not, the load balancer holds requests but does not kill
the pod. Conflating them is a classic outage: return `200` from readiness before
the database pool is warm and the load balancer routes traffic straight into
`500`s. A readiness gate aggregates per-dependency health checks, each bounded by
its own timeout via `select { case <-ctx.Done(): ...; case res := <-ch: ... }`,
and reports `503 Service Unavailable` until every *critical* dependency reports
healthy, `200` afterwards. The per-check timeout matters: one hung dependency
must not make the whole `/readyz` request hang, so each check is bounded
independently and a slow check counts as unhealthy without stalling the others.
`errors.Join` collects every failing dependency's name so the probe output tells
you which one is down.

## errgroup gives structured supervision

Hand-rolled goroutine supervision — a `sync.WaitGroup`, a shared error protected
by a mutex, a `done` channel, a "first error wins" flag — is easy to get subtly
wrong. `golang.org/x/sync/errgroup` is the structured version:
`g, ctx := errgroup.WithContext(root)` derives a context that is cancelled the
first time any `g.Go` function returns a non-nil error (or when `Wait` returns).
Every long-running worker runs in `g.Go` against that derived context; the first
fatal error cancels it, all peers observe `ctx.Done()` and exit, and `g.Wait()`
returns that first error for the process exit code. `g.SetLimit(n)` bounds how
many `g.Go` functions run concurrently, which throttles startup for components
that are expensive to initialize. It replaces the error-prone bookkeeping with a
single object whose semantics are documented and tested.

## Context values carry request-scoped metadata, keyed by an unexported type

`context.WithValue` is for request-scoped metadata that crosses API boundaries —
a request ID, a trace ID, an authenticated principal, a deadline — not for
smuggling mandatory function arguments (those belong in the signature, where the
compiler can check them). The key must be an *unexported named type*, never a
bare `string` or `int`. Two packages that both key on `"request_id"` (a plain
string) silently collide and overwrite each other; two packages that each define
their own unexported `type ctxKey int` cannot collide, because the key's
*identity* includes its type, and one package's `ctxKey` is a different type from
another's even though both have underlying type `int`. The accessor returns
`(value, ok)` so a missing value is a clean miss, not a panic on a bad type
assertion.

## Detached background work survives shutdown

Cancellation propagates down a context tree, which is usually what you want — but
not always. A final metrics flush, an audit-log write, or a last checkpoint must
complete *even though* the request or root context that triggered it is being
cancelled. `context.WithoutCancel(parent)` returns a child that copies the
parent's values but is severed from its cancellation: the parent can be cancelled
and the child's `Err()` stays `nil`, so the must-finish work runs to completion.
`context.AfterFunc(ctx, f)` registers `f` to run in its own goroutine once `ctx`
is done, and returns a `stop` function that deregisters it (returning `true` if it
stopped `f` before it ran). That is the clean way to hook cancellation for a
final flush without spinning up and managing your own `<-ctx.Done()` goroutine.

## Zero-dropped-request drain

Graceful HTTP shutdown (`server.Shutdown`) stops accepting new connections and
waits for in-flight requests on connections it knows about, but a correct
zero-drop rolling deploy usually needs explicit in-flight accounting so the
component itself knows when the last handler finished. The pattern: an atomic
`draining` flag that flips new requests to `503` the instant shutdown starts (so
the load balancer stops sending work), a `sync.WaitGroup` that counts in-flight
requests, and a `Stop` that waits on the group bounded by `StopTimeout` —
returning promptly once the group drains, or returning a deadline error and
force-closing if a stuck handler outlives the budget. That last branch is a
deliberate trade: a single wedged request must not block the entire deploy
forever, so past the budget you force-close and let the orchestrator move on.

## A silent stuck Stop is an operational landmine

The tempting shortcut is to treat teardown as "best effort" and discard `Stop`
errors. Do not. A `Stop` that times out silently can leave a port bound, a file
open, or a transaction uncommitted — and the symptom surfaces one deploy later as
`bind: address already in use` when the next process cannot claim the port. Log
every `Stop` error, and for a critical resource, exit the process non-zero so the
orchestrator knows cleanup was incomplete rather than assuming a clean handoff.
The `address already in use` on the next boot is the smell of a swallowed
teardown error.

## Common Mistakes

### Running the work loop directly in Start

Wrong: `func (s *server) Start(ctx context.Context) error { for { work() } }`.
`Start` never returns, so `Run` never reaches the next service and the process
comes up half-started with no error. Fix: launch the loop in a goroutine and
return `nil` once it is running; keep the goroutine watching `ctx.Done()` so it
exits on shutdown.

### Passing the already-cancelled root context to Stop

Wrong: calling `Stop` with the root context, which is cancelled by the time
teardown runs, so `Shutdown` gets zero budget and drains nothing. Fix: derive a
fresh budget from `context.WithTimeout(context.Background(), cfg.StopTimeout)`
for each `Stop`.

### Expecting services to stop in registration order

Wrong: registering `db`, `cache`, `api` and assuming `db` stops first. The
framework stops in *reverse*: `api`, `cache`, `db`. A test must pin the exact
reverse sequence, because it is the invariant that keeps teardown from closing a
resource a later component still holds.

### Using a bare string or int as a context value key

Wrong: `context.WithValue(ctx, "request_id", id)`. Another package keying on the
same string silently collides. Fix: define an unexported `type ctxKey int` (or
struct) and key on a constant of that type; its type is part of its identity, so
cross-package collision is impossible.

### Putting required parameters in context values

Wrong: passing a mandatory `*sql.DB` or a required user ID through
`context.WithValue` to avoid threading it through signatures. Context values are
untyped at the call site and invisible to the compiler, so a missing one is a
runtime `nil`, not a build error. Fix: context carries request-scoped *metadata*;
mandatory inputs are function arguments.

### Retrying without a backoff cap, without jitter, or ignoring ctx

Wrong: a `for { if err := dial(); err == nil { break }; time.Sleep(d) }` retry
with no cap (delay grows unbounded or stays at a tight interval hammering the
dependency), no jitter (every replica retries in lockstep, a reconnect storm), or
a bare `time.Sleep` that ignores `ctx.Done()` so shutdown hangs until the timer
fires. Fix: cap the exponential backoff, add full jitter, and wait with a
`select` on both the timer and `ctx.Done()`.

### Treating readiness and liveness as one probe

Wrong: returning `200` from readiness before the dependencies are actually up.
The load balancer routes traffic into a not-yet-ready instance and users get
`500`s. Fix: readiness aggregates per-dependency health and returns `503` until
every critical dependency passes; liveness only reports process aliveness.

### Discarding the Stop error as "best effort"

Wrong: `_ = svc.Stop(ctx)`. A stuck teardown that leaves the port bound is now
invisible until the next deploy fails with `address already in use`. Fix: log
every `Stop` error; for critical resources, exit non-zero.

### Letting shutdown abort a final flush

Wrong: running a final metrics/audit flush on the request or root context, which
is cancelled during shutdown, so the flush is aborted mid-write. Fix: run it on
`context.WithoutCancel(ctx)` so cancellation of the parent does not reach it.

### Leaking the supervisor goroutine on shutdown

Wrong: a supervisor whose backoff wait selects only on the retry timer. A
shutdown mid-backoff cannot interrupt it, so the goroutine lives until the timer
fires — a leak and a slow shutdown. Fix: `select` on both the timer and
`ctx.Done()` and return promptly on cancellation.

### Assuming Shutdown alone proves requests drained

Wrong: calling `server.Shutdown` and assuming zero requests were dropped without
tracking in-flight work. `Shutdown` stops accepting new connections, but to know
the last handler actually finished you need `WaitGroup` accounting plus a
`draining` flag that sheds new work with `503`. Fix: count in-flight requests and
wait on the group, bounded by the stop budget.

Next: [01-service-lifecycle-core.md](01-service-lifecycle-core.md)
