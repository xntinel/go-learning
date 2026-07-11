# Graceful Shutdown with Context — Concepts

Graceful shutdown is where context, select, signals, and lifecycle management
converge into the single most operationally load-bearing path a backend service
owns: the code that runs between "the orchestrator sent SIGTERM" and "the process
exits." It is invisible in the happy path and catastrophic when wrong. Get it
right and a rolling deploy is a non-event; get it wrong and every deploy drops
in-flight requests as 502s, leaves transactions half-committed, redelivers
un-acked messages so idempotency breaks, and — worst of all — hangs past the
orchestrator's grace period and gets SIGKILL'd mid-drain, which defeats the
entire point of shutting down gracefully. A senior engineer owns this path
because nobody else can: it cuts across the HTTP server, the worker pool, the
database and broker connections, the readiness probe, and `main()` itself. This
file is the conceptual spine; read it once and each of the ten independent
exercises that follow is a real slice of a production service's lifecycle.

## SIGTERM is a request, SIGKILL is not

An orchestrator stops a process in two acts. First it sends SIGTERM — a polite,
catchable request to wind down. Then, after a grace period, it sends SIGKILL,
which is uncatchable and immediate. On Kubernetes that grace period is
`terminationGracePeriodSeconds`, default 30 seconds. Everything your shutdown
does — flip readiness, wait for endpoint propagation, drain HTTP, drain workers,
close the database pool — must fit inside that window. If the drain overruns, the
kubelet SIGKILLs the container and whatever was still draining is lost. So the
budget is not advisory: it is a hard wall, and your job is to do the most
important cleanup first and bound every phase so no single stuck component eats
the whole window.

## signal.NotifyContext turns a signal into a cancellation

`signal.NotifyContext(parent, sig...)` returns `(ctx, stop)`. The first matching
OS signal cancels `ctx`. That is the elegant part: every component you already
wrote to select on `ctx.Done()` — every worker, every context-aware DB call,
every request handler deriving from the server context — participates in shutdown
with zero extra wiring. The signal becomes an ordinary cancellation that
propagates through the tree you already built.

```go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer stop()
<-ctx.Done()
stop() // restore default disposition; a SECOND signal is now fatal
```

`stop()` unregisters the handler and restores the signal's default disposition.
This is the double-Ctrl+C escape hatch: after the first SIGINT begins a drain, an
impatient operator hitting Ctrl+C again should be able to force-kill a wedged
process. If you never call `stop()`, the library keeps suppressing the default
handler and the second signal does nothing. The idiom is `defer stop()` for the
unconditional case plus an explicit `stop()` at the very start of the drain so
defaults are restored the instant shutdown begins.

## http.Server.Shutdown drains, but does not cancel

`server.Shutdown(ctx)` stops accepting new connections and waits for in-flight
requests to return. If `ctx` expires first, it force-closes the lingering
connections and returns `context.DeadlineExceeded`. Two subtleties trip up most
engineers.

First, the context you pass to `Shutdown` must come from
`context.WithTimeout(context.Background(), ...)`, not the already-cancelled root
context. By the time shutdown runs, the root is `Done()` — deriving the shutdown
context from it gives the drain zero milliseconds and force-closes everything
immediately. The drain budget must be an independent timer rooted at
`Background()`.

Second, `Shutdown` does not cancel the contexts of in-flight requests. It waits
for handlers to return on their own; it never signals `r.Context().Done()`. A
long-poll or streaming handler that only watches its request context will run
until it finishes naturally or the shutdown timeout force-closes its connection.
To actively cut such handlers you must propagate a separate cancellation into
`r.Context()`, typically by wiring `BaseContext` to a context you control and
cancelling it. And `Shutdown` returning `nil` does not by itself prove every
handler finished — hijacked connections, streaming responses, and background
goroutines a handler spawned can outlive it. A real service verifies drain
completeness with an in-flight gauge, not just `Shutdown`'s return value.

## Reverse dependency order is the invariant

Stop components in the reverse of the order they started, which is the reverse of
the dependency graph. Stop ingress first: the HTTP listener, so no new work
enters. Then drain the workers, which may still be calling shared services. Then,
last, close the shared resources everything depends on — the database pool, the
message-broker connection. Reverse this and you create the classic corruption
bug: close the pool first and an in-flight handler or worker writes to a closed
resource mid-operation, panicking or emitting a 500 at the worst possible moment.
The mental model is a stack: last dependency acquired is the last one released.

## Every phase needs its own bounded budget

One total grace budget is not enough; each phase needs its own slice of it. If
the HTTP drain and the worker drain share one deadline, a stuck handler consumes
the whole budget and the worker drain and pool close never run before SIGKILL.
Carve the total into per-phase `context.WithTimeout` deadlines. A phase that
blows its slice must give up — log the residual, record that it was force-closed
— and yield to the next phase. The process still exits, but the fact that a phase
was truncated must be recorded, because that is what the exit code reports.

## The exit code is the only drain-quality signal

The orchestrator and your SLO dashboards learn whether the drain was clean from
exactly one thing: the process exit status. Zero means every phase drained within
its budget. Non-zero means at least one phase was force-closed and in-flight work
may have been dropped. Always exiting 0 regardless of outcome is a silent lie: it
hides dropped requests and corrupt shutdowns from every dashboard that would
otherwise alert on them. Map "any phase forced" to a non-zero code and surface
`context.Cause` in the log so on-call sees which deadline fired, not a bare
"context deadline exceeded."

## The Kubernetes readiness-drain race

On Kubernetes, SIGTERM delivery and endpoint deregistration happen concurrently,
not in sequence. When a pod is deleted, the kubelet sends SIGTERM at the same
time the endpoints controller begins removing the pod from Service endpoints —
and that removal must propagate through the API server, the endpoints controller,
and every node's kube-proxy before traffic stops arriving. If you drain the
instant SIGTERM lands, requests already routed to the pod (and new ones still
being routed during propagation) hit a server that has stopped accepting, and
clients see connection-refused or 502 on every rolling deploy.

The fix is a deliberate sequence: on SIGTERM, first flip the readiness probe to
fail (an atomic flag that `/readyz` reads and returns 503), then sleep a
propagation delay so the endpoints controller and kube-proxy remove the pod from
rotation, and only then begin the graceful HTTP and worker drain. Throughout,
liveness (`/livez`) must stay healthy — if liveness fails during drain the
kubelet kills the pod early and truncates the drain. Readiness fails to stop new
traffic; liveness stays green to keep the drain alive.

## errgroup ties component lifetime together

`errgroup.WithContext(parent)` returns `(g, gctx)` sharing a cancellable context.
Each long-lived component runs under `g.Go`; the first one to return a non-nil
error cancels `gctx`, which propagates the shutdown signal to every sibling, and
`g.Wait()` returns that first error. This is how a production service treats a
fatal subsystem failure — a listener that cannot bind, a worker whose downstream
died — as a coordinated teardown trigger, not just external signals. One
normalization is mandatory: a component wrapping `http.Server.ListenAndServe`
must translate `http.ErrServerClosed` to `nil`, because that error is the normal
signal that `Shutdown` was called, not a failure. Treat it as fatal and every
clean stop cancels the group and looks like a crash.

## Draining workers must be raced against a timer

`sync.WaitGroup` is the natural tool to wait for background workers to finish, but
`wg.Wait()` blocks unconditionally. A single wedged worker then hangs the process
past the grace period and gets SIGKILL'd — the opposite of graceful. The idiom is
to close a channel after `wg.Wait()` in a goroutine and `select` on that channel
against `time.After(budget)`, so the drain either completes cleanly or gives up
and reports the residual.

```go
done := make(chan struct{})
go func() { wg.Wait(); close(done) }()
select {
case <-done: // clean
case <-time.After(timeout): // wedged worker; give up, report
}
```

## Shutdown must be idempotent

A double signal is normal operational reality: the operator hits Ctrl+C twice, or
a signal races with a fatal-error-triggered teardown. Shutdown must survive that
without panicking. `context.CancelFunc` is already safe to call twice — a second
`cancel()` is a no-op. But `close(chan)` is not: closing an already-closed channel
panics with "close of closed channel," exactly when the operator is trying to
force a stuck process to stop. Guard the teardown with `sync.Once` so a repeated
invocation is a no-op that returns the memoized result, never a double-close. The
restored default signal handler still force-kills on the truly-second OS signal;
`sync.Once` only guards the *program's* drain logic against re-entry.

## Aggregating and surfacing failure

`errors.Join` collects per-phase failures into one error while preserving each
cause, so `errors.Is`/`errors.As` still find the individual sentinels. Pair it
with `context.Cause`, which surfaces the specific cancellation or deadline that
fired, so an operator reading the shutdown log sees "worker drain: deadline
exceeded after 5s" instead of a bare, unactionable "context deadline exceeded."
A good shutdown emits a structured, honest account of what drained and what did
not, and the exit code that matches it.

## Common Mistakes

### Passing context.Background() with no timeout to Server.Shutdown

Wrong: `server.Shutdown(context.Background())`. A single misbehaving long-poll
client then blocks shutdown forever, and the process hangs until SIGKILL. Fix:
always `context.WithTimeout(context.Background(), budget)` so the drain is
bounded.

### Deriving the shutdown context from the cancelled root

Wrong: `ctx` from the root context (already `Done()` when shutdown begins) is
passed to `Shutdown`. The drain gets zero milliseconds and force-closes every
connection immediately. Fix: root the shutdown timeout at
`context.Background()`, independent of the cancelled root.

### Never calling stop() after the first signal

Wrong: catch SIGTERM with `signal.NotifyContext` and never call `stop()`. The
default handler stays suppressed, so a second Ctrl+C does nothing and the operator
cannot force-kill a wedged drain. Fix: `defer stop()` plus an explicit `stop()` at
drain start.

### Closing the pool before stopping ingress

Wrong: cancel workers or close the DB pool before `server.Shutdown`. In-flight
handlers then touch a nil or closed dependency mid-request and panic or 500. Fix:
reverse dependency order — HTTP first, workers second, shared resources last.

### Trusting Shutdown's return as proof of a clean drain

Wrong: assume `Shutdown` returning `nil` means every handler finished. Hijacked,
streaming, or background-goroutine work can outlive the handler. Fix: verify with
an in-flight gauge that reaches zero.

### Assuming Shutdown cancels request contexts

Wrong: expect long handlers to stop when `Shutdown` runs. It waits; it does not
cancel `r.Context()`. Fix: wire a separate cancellation (via `BaseContext`) into
request contexts if you need to actively cut them.

### Skipping the readiness flip and propagation delay

Wrong: drain the instant SIGTERM arrives. Requests already routed from the Service
land on a server that stopped accepting, surfacing as connection-refused/502 on
every rolling deploy. Fix: flip readiness to fail, sleep the propagation delay,
then drain — keeping liveness healthy throughout.

### Always exiting 0

Wrong: `os.Exit(0)` regardless of drain outcome. The orchestrator and dashboards
can no longer tell a clean shutdown from one that dropped work. Fix: non-zero exit
when any phase was force-closed.

### Blocking on wg.Wait() with no timer

Wrong: `wg.Wait()` alone. One wedged worker hangs the process past the grace
period into SIGKILL. Fix: race `wg.Wait()` against `time.After(budget)`.

### Non-idempotent teardown that double-closes

Wrong: teardown that closes a channel or re-runs on a second signal, panicking with
"close of closed channel." Fix: guard with `sync.Once`; remember `cancel()` is safe
twice but `close(chan)` is not.

### One stuck phase consuming the whole budget

Wrong: share one deadline across all phases, so a stuck HTTP drain starves the
worker drain and pool close. Fix: give each phase its own bounded slice of the
total budget.

### Treating http.ErrServerClosed as fatal

Wrong: let `ListenAndServe` returning `http.ErrServerClosed` propagate as an error
from a supervised component. Every clean stop then looks like a crash and, under
errgroup, cancels the group. Fix: normalize `http.ErrServerClosed` to `nil`.

Next: [01-worker-lifecycle-cancellation.md](01-worker-lifecycle-cancellation.md)
