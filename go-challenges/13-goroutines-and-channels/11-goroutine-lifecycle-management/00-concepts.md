# Goroutine Lifecycle Management — Concepts

Every long-lived backend process is a graph of goroutines: an HTTP server, a
queue consumer, a token refresher, a metrics flusher, a supervisor watching a
worker. Each of those goroutines must start deterministically, run under
cancellation, and stop without leaking. The discipline that makes this tractable
is the same one you already apply to file handles, database connections, and
sockets: treat a goroutine as an *owned resource* with an explicit lifecycle
contract. Whoever starts it owns it; the owner provides a way to stop it, and the
owner waits for it to finish. This file is the conceptual foundation for the nine
independent exercises that follow — read it once and you have the model you need
to reason through each one.

## Concepts

### A goroutine is an owned resource, not fire-and-forget

`go f()` is one keyword and no return value, which makes it dangerously easy to
spawn a goroutine and forget it. But a goroutine holds a stack (initially small,
but it can grow), it may hold references that the garbage collector cannot
reclaim, and if it is blocked it will sit in the scheduler forever. A goroutine
with no owner and no way to stop is the root cause of a *goroutine leak*: the
process's `runtime.NumGoroutine()` climbs, memory grows slowly, and eventually
the service is OOM-killed in production with no obvious culprit in the code that
"looks" correct.

The fix is a rule, applied everywhere: the function that starts a goroutine is
responsible for its whole lifecycle. It exposes a `Stop` (or `Close`, or
`Shutdown`) that signals the goroutine to leave, waits for it to actually leave,
and reports the outcome. A goroutine you cannot point at an owner is a bug, not a
style preference.

### The lifecycle has three phases with distinct responsibilities

Start, Run, Stop are not just a mnemonic; each phase owns different work.

- **Start** allocates: it creates the channels the goroutine will select on, any
  timers or tickers, and the goroutine itself. Start must be safe against misuse
  — calling it twice without a stop in between would orphan the first goroutine,
  so Start guards that (a state field, or `sync.Once`) and rejects a double start.
- **Run** is the work loop. It processes work while watching a cancellation
  signal. Every blocking operation in the loop lives inside a `select` that also
  watches the cancel case, so the loop can always leave.
- **Stop** signals, drains, and releases. It tells the run loop to leave (close a
  stop channel, cancel a context), *waits* for it to finish (a done channel or a
  `WaitGroup`), and cleans up. Stop must also be safe against misuse: stopping a
  worker that never started, or stopping twice, should return a clear error
  rather than panic or block forever.

### Signal and wait are two separate steps

This is the single most common lifecycle bug. Closing a stop channel or
cancelling a context only *asks* the goroutine to leave; it does not make it gone.
The goroutine may be mid-iteration, mid-cleanup, or not yet scheduled. If `Stop`
returns the instant it sends the signal, the goroutine is very likely still
running when the caller believes shutdown is complete. That is why shutdowns
"hang" (nobody waited, so the process exits mid-write) or "leak" (nobody waited,
so the goroutine outlives the thing that owned it).

The owner must block on a *done* signal after sending the *stop* signal:

```go
close(w.stop) // signal: please leave
<-w.done      // wait: confirm you left
```

A `sync.WaitGroup` serves the same purpose for a set of goroutines: `wg.Wait()`
returns only once every worker has called `Done`.

### Cancellation propagates via context; Go cannot kill a goroutine

There is no `goroutine.Kill`. A goroutine stops only when *its own code* observes
a cancellation and returns. Cancelling a `context.Context` closes `ctx.Done()`;
it does not preempt the goroutine or unwind its stack. Code that never checks
`ctx.Done()` (or a stop channel) keeps running after cancellation, forever. This
is why every blocking operation in a run loop must sit in a `select` with the
cancel case, and why blocking I/O must use context-aware APIs (a database call
that takes a `ctx`, an HTTP request built with `http.NewRequestWithContext`).
"Assume cancellation stops the goroutine" is a leak waiting to happen.

### `close(ch)` is a one-shot broadcast — and a foot-gun

Closing a channel is the idiomatic fan-out cancel: every receiver blocked on
`<-ch` wakes at once, and every future receive returns the zero value
immediately. That makes `chan struct{}` closed by the owner the natural "everyone
stop now" signal. But close has two panics attached: closing an already-closed
channel panics, and sending on a closed channel panics. So a channel must have a
single owner that closes it exactly once, guarded by `sync.Once` or by a
nil-under-mutex pattern, and senders must never race the close — they select on a
done signal instead of relying on the channel staying open.

### Graceful versus abrupt teardown is a real trade-off

Two teardown strategies, opposite costs:

- **Graceful drain** finishes in-flight and queued work before stopping:
  `http.Server.Shutdown` stops accepting new connections and waits for active
  requests to complete; a worker pool closes its input and processes what is
  already queued. The cost is an unbounded wait — a slow client or a stuck job
  can make graceful shutdown hang.
- **Abrupt cancel** bounds latency by dropping in-flight work: cancel the
  context, everyone returns now. The cost is lost work.

Production teardown almost always wants both, sequenced by a deadline: ask for a
graceful drain, but if it does not complete within N seconds, upgrade to abrupt.
That is exactly what a `context.WithTimeout` passed to `Shutdown` expresses.

### A deadline-bounded Stop must not block forever

If `Stop` blocks on `<-done` unconditionally, one stuck goroutine hangs the whole
shutdown. The robust pattern races the wait against a deadline:

```go
select {
case <-done:
	return nil
case <-ctx.Done():
	return ErrStopTimeout
}
```

If the goroutine drains in time, return success. If the deadline hits, return a
timeout error and stop *waiting* — the caller unblocks and shutdown proceeds. If
you detach the still-running goroutine, ensure it will still terminate cleanly
(it eventually observes its stop signal and returns without a double-close), so
"detached" means "no longer waited on," not "leaked."

### Shutdown should be observable

When a service stops, operators need to know *why*: was it an operator-requested
stop, an upstream dependency failure, or a deadline? `context.WithCancelCause`
lets the code that cancels attach a *cause* error, and `context.Cause(ctx)`
retrieves it. `ctx.Err()` still reports the coarse reason (`context.Canceled` or
`context.DeadlineExceeded`), but `context.Cause` reports the specific,
machine-readable reason you attached. A component that records `context.Cause` in
its final status turns an opaque shutdown into a diagnosable one.

### A panic in a goroutine crashes the whole process

A panic unwinds only the panicking goroutine's stack; but if it reaches the top
of that stack unrecovered, the runtime crashes the *entire process*. There is no
per-goroutine isolation. A long-lived background worker that processes untrusted
or complex input therefore needs a `recover` boundary at the goroutine's top, or
one bad message takes down every other connection the process was serving. A
*supervisor* turns that recovered panic into a bounded, backed-off restart rather
than a crash — and the backoff matters: restarting instantly in a tight loop
(a worker that panics on startup) pins a CPU and floods the logs, so restarts use
capped exponential backoff and a restart budget after which the supervisor gives
up and reports the failure upward.

### Modern lifecycle helpers reduce boilerplate and bugs

The standard library and `x/sync` have absorbed several of these patterns:

- `sync.WaitGroup.Go` (Go 1.25) fuses `Add(1)` / `go` / `defer Done()` into one
  call, eliminating the classic "Add inside the goroutine" race.
- `golang.org/x/sync/errgroup` gives bounded fan-out: `errgroup.WithContext`
  derives a context that is cancelled when the first task errors, `SetLimit`
  caps concurrency, and `Wait` returns the first error. `TryGo` starts a task
  only if a slot is free.
- `context.AfterFunc` (Go 1.21) registers a function to run in its own goroutine
  when a context is cancelled — a tidy way to wire cleanup to a lifecycle without
  a bespoke watcher goroutine.

Prefer these over hand-rolled equivalents; they encode the correct ordering.

### Leaks are observable and testable

Lifecycle correctness is not a matter of faith. `runtime.NumGoroutine()` reports
the live goroutine count, so a test can assert it returns to baseline after a
component is stopped. `go.uber.org/goleak` does this rigorously: registered from
`TestMain` via `goleak.VerifyTestMain(m)`, it fails the test run if any goroutine
is still parked after the tests finish (with a retry window to avoid flaking on
goroutines mid-exit). Note that `goleak.VerifyNone(t)` cannot be combined with
`t.Parallel()` — it cannot attribute goroutines to a specific parallel test — so
parallel suites use `VerifyTestMain`. And `go test -race` catches the concurrent
state bugs that lifecycle code is prone to: a `result` counter or a `running`
flag touched by both the goroutine and its owner without synchronization passes
casual testing and fails `-race`.

## Common Mistakes

### Signalling stop but never waiting

Wrong: `Stop` closes the stop channel (or cancels the context) and returns
immediately. The goroutine is still running, or half torn down, after `Stop`
returns — shutdown races the goroutine and either hangs or leaks.

Fix: block on a done channel or `WaitGroup` in `Stop`, after signalling. Signal
and wait are two steps.

### Allowing a second Start without a Stop

Wrong: `Start` launches a goroutine unconditionally. Calling it twice orphans the
first goroutine — a guaranteed leak.

Fix: guard with a state field or `sync.Once` and return `ErrAlreadyStarted` on the
second call.

### Double-closing a channel, or closing a channel a sender still writes to

Wrong: two code paths both `close(ch)`, or a goroutine sends on a channel the
owner has closed. Both panic at runtime.

Fix: one owner closes exactly once (`sync.Once`, or nil-out under a mutex);
senders `select` on a done signal rather than assuming the channel stays open.

### A blocking op with no `select` on the cancel case

Wrong: a run loop with a bare `<-ch` receive, a bare `ch <- v` send, or a network
call that ignores `ctx`. Cancellation is invisible to it, so the goroutine never
exits.

Fix: every blocking operation sits in a `select` with the cancel case, and I/O
uses context-aware APIs.

### Assuming context cancellation force-stops a goroutine

Wrong: cancelling `ctx` and assuming the goroutine is now gone. Cancellation only
closes `ctx.Done()`; code that never checks it keeps running.

Fix: the goroutine must observe `ctx.Done()` and return, and the owner must still
wait for it.

### `Shutdown` with no timeout, or treating `ErrServerClosed` as failure

Wrong: `srv.Shutdown(context.Background())` blocks until every connection drains
— a slow client hangs shutdown forever. Or: logging the `http.ErrServerClosed`
that `ListenAndServe` returns on a clean shutdown as an error.

Fix: pass a `context.WithTimeout` to `Shutdown` so graceful upgrades to abrupt,
and special-case `errors.Is(err, http.ErrServerClosed)` as the normal path.

### A goroutine sending on a channel whose reader went away

Wrong: spawning a goroutine that does `out <- v` on an unbuffered channel, then
having the caller stop reading `out`. The goroutine parks on the send forever.

Fix: make the send `select` on `ctx.Done()` / a done channel, or size and close
the channel so the send cannot block indefinitely.

### Not recovering panics in a background worker (or restarting with no backoff)

Wrong: a long-lived consumer with no `recover`, so one bad message crashes the
process. Or: recovering and restarting instantly, producing a CPU-pinning hot
loop.

Fix: `recover` at the goroutine boundary, and restart with capped exponential
backoff and a restart budget.

### Using `goleak.VerifyNone` with `t.Parallel`

Wrong: `t.Parallel()` plus `defer goleak.VerifyNone(t)` — goleak cannot tell
which parallel test owns which goroutine and reports false leaks.

Fix: use `goleak.VerifyTestMain(m)` for parallel suites; reserve `VerifyNone` for
serial tests.

### Sharing worker state without synchronization

Wrong: reading and writing a result counter, a running flag, or a last-error
field from both the goroutine and the owner with no mutex or atomic. It passes
casual testing and fails `-race`, and corrupts under load.

Fix: guard the shared state with a mutex, an atomic, or hand it across a channel.

Next: [01-start-stop-worker.md](01-start-stop-worker.md)
