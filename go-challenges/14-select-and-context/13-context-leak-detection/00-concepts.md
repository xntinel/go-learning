# Context Leak Detection — Concepts

Context leaks are the quiet killers of long-lived Go services. Every
`context.WithCancel`, `context.WithTimeout`, or `context.WithDeadline` allocates
a node and hangs it off its parent; the node is removed only when its cancel
function runs, the parent is cancelled, or (for the timer variants) the deadline
fires. In a service rooted at `context.Background()`, a cancel function that is
never called means "removed never" — the node is pinned for the life of the
process. One such node is invisible. Millions, accumulated over a day of steady
traffic, are an out-of-memory crash with a stack trace that points nowhere useful.

A senior engineer does not fight this with willpower ("remember to `defer
cancel`"). They build the guardrails that make the leak impossible to ship: an
instrumented detector that names the leaking call site, a `goleak` gate in CI
that fails the build on any surviving goroutine, cause-tagged cancellation so an
operator can tell a client-disconnect from a load-shed from a deadline, a
`pprof`-based triage path for a process that is already melting in production, a
zero-overhead production shim behind a build tag so the instrumentation never
taxes the hot path, and correct handling of detached work (`WithoutCancel`) that
must still be independently bounded. This file is the model behind the ten
exercises that follow; read it once and each module becomes a small, focused
build.

## Concepts

### The context tree, and what actually removes a node

`context.WithCancel(parent)` returns a child context and a `CancelFunc`.
Internally the child registers itself with the nearest cancellable ancestor by
appending to that ancestor's `children` set (the runtime calls this
`propagateCancel`; the set lives on `cancelCtx.children`). The parent now holds a
reference to the child, the child holds its grandchildren, and so on. There are
exactly three events that remove a child from its parent's set: the child's own
`cancel()` runs, the parent itself becomes `Done`, or — for `WithTimeout` and
`WithDeadline` — the internal timer fires. If none of those happen, the node
stays. Rooted at `context.Background()` (which is never cancelled and has no
timer), an uncalled `cancel` pins the node for the process lifetime. This is a
genuine memory leak, not merely untidy code: the retained graph can only grow.

### One defect, three symptoms

The pinned node is the memory symptom. But the same missing `cancel` usually
drags two more leaks behind it. For `WithTimeout`/`WithDeadline` there is a live
runtime timer kept armed until the deadline (or forever, if the deadline is far
away and cancel is never called). And any goroutine parked in a
`select { case <-ctx.Done(): ... }` waiting on that context stays blocked — a
leaked goroutine. Different tools see different symptoms of this one defect:
`go vet`'s `lostcancel` analyzer catches the *syntactic* case (a cancel assigned
and never used in the same function); `goleak` catches the parked *goroutine*;
the runtime detector in this lesson catches the pinned *context node*. They are
complementary, not redundant — none of them alone catches every shape of the bug.

### `context.AfterFunc` is the deregistration primitive

Go 1.21 added `context.AfterFunc(ctx, f)`: it arranges for `f` to run in its own
goroutine once `ctx` is `Done`, and returns a `stop func() bool` that cancels the
arrangement. If `ctx` is already `Done` when you call it, `f` is scheduled
immediately. This is exactly the hook a detector needs: register a context when
it is created, and `AfterFunc(ctx, deregister)` guarantees the node is removed
from the registry the moment the context becomes `Done` — whether that came from
the child's own cancel, from the parent being cancelled, or from a timer firing.
The detector never has to poll. Because `delete` on a missing map key is a no-op,
the callback is idempotent: if the code path *also* deregisters explicitly on
`cancel()`, the second delete is harmless. That idempotency is why the detector
does not need to hold onto the `stop` function `AfterFunc` returns.

### `runtime.Caller` turns a leak into an address

A count of leaked contexts is useless for triage; a *file:line* is actionable.
`runtime.Caller(skip)` returns the program counter, file, and line of the frame
`skip` levels above the call, and `runtime.FuncForPC(pc).Name()` turns the PC
into a function name. The `skip` accounting is load-bearing and easy to get wrong
by one. Measured from inside the helper that calls `runtime.Caller`, the frames
are: 0 = the helper itself, 1 = the detector's `register`, 2 = the `WithCancel`
wrapper, 3 = the user's handler. A detector that captures `skip=2` reports its
own `leakdetect.go` on every leak and is worthless for triage; `skip=3` names the
handler. The only way to be sure is a test that asserts the report contains the
caller's file, because inlining can shift the count — the first exercise builds
exactly that test.

### Grace period: cancel is often slightly late

Cancel is frequently deferred, or called from a goroutine that the scheduler has
not run yet. A leak check that fires the instant a context is created would flag
a context that is about to be cancelled a microsecond later. So "leaked" is
defined against a short grace window: a context counts as leaked only if it is
still uncancelled *past* the grace period. This is why the test helper
`AssertNoLeaks` sleeps before it calls `Check` — the sleep lets any pending
`AfterFunc` callbacks drain and lets deferred cancels run, so a correctly-written
handler is never falsely accused.

### `context.Cause` is the diagnosability upgrade over `ctx.Err()`

`ctx.Err()` is deliberately coarse: it only ever returns `context.Canceled` or
`context.DeadlineExceeded`. That tells you *that* a context ended, never *why*.
Go 1.20's `context.WithCancelCause` returns a `CancelCauseFunc` that takes an
`error`, and `context.Cause(ctx)` returns exactly that error. Go 1.21 extended
this to timers with `WithTimeoutCause` and `WithDeadlineCause`, whose supplied
cause is what `Cause` returns *on expiry*. In a request pipeline this is the
difference between a leak report that says "cancelled" and one that says
"cancelled: client disconnected" versus "cancelled: load shed" versus "deadline
exceeded". One subtlety to internalize: with `WithTimeoutCause`, the returned
plain `CancelFunc` does *not* set the cause — only the timer expiry uses the
supplied cause; an explicit cancel still yields `context.Canceled`.

### `context.WithoutCancel` detaches, and thereby unbounds

Go 1.21's `context.WithoutCancel(parent)` returns a child that copies the
parent's values but is *not* cancelled when the parent is: its `Done()` is `nil`,
its `Err()` and `Cause()` are `nil`, and it has no deadline. This is the correct
tool for fire-and-forget work that must outlive a request — an audit-log write, a
metrics flush — because the request's cancellation must not abort it. But it is a
trap: by detaching from the parent you have also removed the parent's deadline,
so a `WithoutCancel` context on its own is unbounded. A goroutine that blocks on
work derived from it can hang forever. Detached work must be *re-bounded* with a
fresh `WithTimeout`, or it becomes exactly the leak you were trying to avoid.

### Zero-overhead instrumentation with build tags

Instrumentation that allocates a record, takes a mutex, and touches a map on
every context creation is fine in CI and unacceptable on a hot request path. The
standard resolution is two files exposing an *identical* exported API behind
opposite build constraints: `//go:build leakdetect` compiles the real tracking
`Detector`, `//go:build !leakdetect` compiles a pass-through shim whose methods
delegate straight to `context.With*` and allocate nothing. Callers write one code
path. CI builds with `-tags leakdetect` and gets full tracking; the production
binary omits the tag and pays zero overhead — no map, no lock, no record. This is
the same pattern the standard library and large services use to keep diagnostics
out of the fast path.

### `goleak` verifies at the goroutine layer

Uber's `go.uber.org/goleak` is complementary to the context detector: it snapshots
the set of running goroutines and fails if any unexpected ones survive.
`goleak.VerifyTestMain(m)` gates a whole package after all its tests finish;
`goleak.VerifyNone(t)` gates a single test but is incompatible with
`t.Parallel()` because it cannot attribute goroutines to one parallel test;
`goleak.Find()` returns an error describing surviving goroutines without failing
anything, which is useful for asserting *inside* a test that a leak currently
exists. `IgnoreTopFunction` and `IgnoreCurrent` suppress known-benign background
goroutines (a connection pool's reaper, a metrics flusher). Wired into CI,
`VerifyTestMain` fails the build the moment a test leaks a goroutine — the
strongest guardrail of the set, because it needs no manual assertion.

### Live-process triage with `runtime/pprof`

When a process is already climbing toward OOM, you cannot re-run it under a test.
`runtime.NumGoroutine()` gives the current count; `runtime/pprof.Lookup("goroutine")`
returns the goroutine `Profile`, whose `Count()` is that same number and whose
`WriteTo(w, 1)` dumps every goroutine's stack. Counting how many stacks contain a
target frame — say, blocked on `ctx.Done` inside one package — localizes the leak
to a call site in a running process. Cross-checking `NumGoroutine` growth against
the `Detector`'s `ActiveContexts` tells you whether a goroutine climb is
context-driven or something else. A `/debug/leaks` endpoint that reports both
numbers side by side is a cheap, high-value observability hook.

### The detector must itself be race-clean

The detector is exercised concurrently from many request goroutines, so its own
state must be correct under `-race`. `register`/`deregister` mutate a shared map
under a `sync.Mutex`; the running total uses `atomic.Int64` so `TotalCreated`
never needs the lock; and `delete`-on-missing-key makes double-deregistration
(an explicit `cancel` racing its own `AfterFunc`) safe. A leak detector that
itself has a data race is worse than none, so `go test -race` is the floor, not a
nicety.

## Common Mistakes

### Dropping the CancelFunc on the floor

Wrong: `ctx, cancel := context.WithCancel(parent)` then using `ctx` and never
calling `cancel` — storing it in a struct, returning it and ignoring it, or
simply forgetting. Each request down that path adds one permanent node to the
context tree; over a day of traffic that is millions of nodes and a steadily
rising heap. Fix: `defer cancel()` on the very next line after the assignment,
always, even when a later error return seems to make it moot — the deferred call
still runs.

### Checking for leaks before AfterFunc has drained

Wrong: call `cancel()` and immediately call `Check()`. The `AfterFunc`
deregistration runs in its own goroutine that the scheduler has not necessarily
run yet, so a correctly-cancelled context is still in the registry and is falsely
reported. Fix: cancel, then wait briefly (this is what `AssertNoLeaks` does) so
the callback drains before you read the registry.

### Scoping a context to a long-lived goroutine

Wrong: create a context at the top of a worker goroutine that runs for the life
of the server, with `defer cancel()` as the only cancellation point. That cancel
fires when the goroutine exits — which may be never — while every timeout or
child derived inside the loop accumulates. Fix: scope the context to the *task*
(per-request, per-job, per-message), not to the worker; create and cancel it
inside the loop body.

### Reading ctx.Err() when you wanted the cause

Wrong: after a `WithCancelCause` cancellation, read `ctx.Err()` expecting the
sentinel you passed. `Err()` always returns `context.Canceled` (or
`DeadlineExceeded`). Fix: use `context.Cause(ctx)`. And remember that
`WithTimeoutCause`/`WithDeadlineCause` apply the supplied cause only on *expiry* —
the plain `CancelFunc` they return still yields `context.Canceled`.

### Detaching with WithoutCancel and forgetting the deadline

Wrong: `ctx := context.WithoutCancel(r.Context())` for a background audit write,
then handing `ctx` to code that blocks. Nothing will ever cancel it; the
goroutine can block forever. Fix: wrap the detached context in a fresh
`context.WithTimeout` so the detached work is independently bounded, and cancel
that timeout when the work finishes.

### Trusting go vet to catch every leak

Wrong: assuming `go vet` / `lostcancel` flags all missing cancels. It only flags
the syntactic case where a `CancelFunc` is assigned and never used within the same
function; it says nothing about a cancel that is stored in a struct, passed to
another function, or skipped on one branch of an `if`. Fix: layer `goleak` and the
runtime detector on top of vet — they catch the cases static analysis cannot see.

### Calling goleak.VerifyNone inside a parallel test

Wrong: `func TestX(t *testing.T) { t.Parallel(); defer goleak.VerifyNone(t) }`.
While that test runs, other parallel tests have their own goroutines running, and
`VerifyNone` cannot tell them apart from a leak, so it flags unrelated goroutines.
Fix: use `goleak.VerifyTestMain(m)` for a package with parallel tests, and reserve
`VerifyNone` for serial tests.

### Getting the runtime.Caller skip depth wrong

Wrong: capturing `runtime.Caller(2)` from inside the detector so every leak report
points at `leakdetect.go` instead of the handler that leaked. An off-by-one in the
skip count makes the whole tool useless for triage, and nothing about it fails to
compile — it silently misreports. Fix: count the frames precisely (helper +
`register` + wrapper = 3 to reach user code) and add a test that asserts the report
names user code, so inlining changes are caught.

Next: [01-detector-core.md](01-detector-core.md)
