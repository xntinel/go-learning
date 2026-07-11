# Context WithCancel: Cancellation Trees, Causes, and Detached Work — Concepts

`context.WithCancel` is the backbone of every request-scoped lifecycle in a Go
backend. It is how an aborted HTTP request tears down the database query, the
outbound RPC, and the goroutines it spawned, all at once, without leaking any of
them. Treat cancellation as a production concern, not a language feature: the
question is never "does this compile" but "when the caller walks away, does every
piece of in-flight work stop, and can I tell from the logs why". This file is the
conceptual foundation. Read it once and you have everything you need to reason
through the nine independent exercises that follow — the canonical select-on-Done
worker loop, the cancellable generator that unwedges its producer on the send case,
the idempotent leak-proof cancel, the hedged fan-out that cancels its losers with a
diagnosable cause, the context-respecting retry, the worker pool, the `AfterFunc`
cleanup hook, the detached audit write, and a mini `errgroup` built from primitives.

## Concepts

### A context is a node in a tree of Done channels

`context.Background()` is the root of the tree; it is never cancelled and its
`Done()` returns a nil channel that never closes. `context.WithCancel(parent)`
returns a *child* context plus a `cancel` function. The child's `Done()` channel
closes when either you call `cancel` or the parent's `Done()` closes — whichever
happens first. That is the entire mechanism, and it is what makes cancellation
*transitive*: cancel a node and the close propagates down to every descendant at
once. It is also *one-way and terminal*: a cancelled context never becomes active
again, and its `Done()` channel, once closed, stays closed. There is no "un-cancel".

Because the tree is the unit of teardown, the shape of a well-behaved backend is:
one context per inbound request, derived children for each fan-out or background
step, and a single `cancel` at the top that collapses the whole subtree when the
request ends.

### Cancellation is a signal, not a stop

Closing `Done()` does exactly one thing: it unblocks the goroutines that are
`select`-ing on it. It does not preempt anything. A tight CPU loop that never
checks `ctx.Done()`, a blocking syscall issued without a deadline, a `time.Sleep`
with no escape hatch — all of them keep running after cancel, oblivious. This is
why cancellation is called *cooperative*: every long-running operation must
either poll `ctx.Err()` at a checkpoint or block on `ctx.Done()` in a `select`.
Code that does neither cannot be cancelled, and no amount of calling `cancel`
from the outside will change that. The canonical consumer loop is therefore a
`select` with a `case <-ctx.Done()` arm alongside the real work.

### ctx.Err() is the state discriminator

`ctx.Err()` reports what state the context is in: `nil` while active,
`context.Canceled` after `cancel` was called, `context.DeadlineExceeded` after a
deadline or timeout fired. Those are the two terminal error values, and control
flow keys off them. Always compare with `errors.Is`, never `==`: a value that
reaches you may have been wrapped with `%w` on the way up, and `errors.Is` sees
through the wrapping while `==` does not. `errors.Is(err, context.Canceled)` is
correct; `err == context.Canceled` is a latent bug that fires the day someone
adds a wrap.

### context.Cause carries the diagnosable reason

`ctx.Err()` is deliberately coarse: after any cancel it is just
`context.Canceled`, which makes for useless logs ("context canceled" tells you
nothing about *why*). `context.WithCancelCause(parent)` (Go 1.20) returns a
`CancelCauseFunc` — a `func(cause error)` — and `context.Cause(ctx)` returns the
error you passed when you cancelled. The division of labor is precise:
`ctx.Err()` stays `context.Canceled`/`context.DeadlineExceeded` for control flow,
while `context.Cause(ctx)` carries the specific reason — which replica won the
race, which sibling task failed, that the server is shutting down. This is what
turns an opaque "context canceled" line into an actionable one. If you never set
a cause, `context.Cause(ctx)` falls back to returning `ctx.Err()`, so it is
always safe to call.

### Always defer cancel()

The `CancelFunc` (and `CancelCauseFunc`) does real cleanup: it releases the
child's `Done` channel and any associated timer, and detaches the child from the
parent's list of children so the parent can be garbage-collected without it.
Skipping the call leaks that state until the parent itself is cancelled — in a
long-lived server with a `Background` root, "until the parent is cancelled" means
*forever*. `go vet`'s `lostcancel` analyzer flags a `cancel` that is not called
on every path; treat that finding as a build failure, not a style nit. The
idiom is `ctx, cancel := context.WithCancel(parent); defer cancel()`. Calling
`cancel` more than once is explicitly safe — the second and later calls are
no-ops — which is exactly what makes idempotent-cancel patterns and "cancel on
every path plus a defensive defer" legal.

### AfterFunc hangs cleanup off cancellation without a babysitter

Before Go 1.21 the way to run cleanup when a context was cancelled was to
hand-roll `go func() { <-ctx.Done(); cleanup() }()` — a whole goroutine that
exists only to wait. `context.AfterFunc(ctx, f)` replaces it: it schedules `f` to
run on its own goroutine when `ctx` is cancelled, and returns a `stop func() bool`.
Calling `stop()` returns `true` if it prevented `f` from running (the context had
not been cancelled yet) and `false` if `f` had already been started or stopped.
This composes cleanly with normal completion: register the cleanup with
`AfterFunc`, and on the success path call `stop()` so the cleanup does not also
fire. It is the right tool for releasing a lease, decrementing a gauge, or
emitting a "request aborted" metric exactly when — and only when — a cancel
actually happens. `f` runs in its own goroutine, so whatever it touches must be
safe for concurrent use, and using a `sync.Once` around the release keeps
"cancel fired and success path both ran" from releasing twice.

### WithoutCancel detaches best-effort work that must outlive the request

Some side effects must complete even though the request that triggered them is
already gone: writing an audit record, flushing a metric, warming a cache. If you
hand such work the request's context, cancelling the request kills the write too
— you lose the audit of the very request you most wanted to record.
`context.WithoutCancel(parent)` (Go 1.21) returns a context that keeps the
parent's *values* (trace id, request id, auth principal) but is immune to its
cancellation and deadline. That is the correct detach: values preserved, lifetime
severed. The one discipline that goes with it: a detached context has no deadline,
so it can run unbounded — always wrap it in its own `context.WithTimeout` so the
background write cannot hang forever.

### Contexts are safe for concurrent use

The same `ctx` can be handed to any number of goroutines; they all observe the
same cancellation at the same instant with no extra locking. That guarantee is
what makes fan-out, worker pools, and first-error coordinators possible with a
single shared child context: you derive one child, pass it to N workers, and one
`cancel` reaches all of them simultaneously. You never need a mutex to protect a
context.

### The first-error-cancels-all pattern is errgroup, demystified

The coordination at the heart of `golang.org/x/sync/errgroup` is just three
primitives: `context.WithCancelCause` + a `sync.WaitGroup` + a `sync.Once`.
Launch each task on one shared child context; the instant a task returns a
non-nil error, record it under the `Once` and call `cancel(err)` so the error
becomes the cancellation cause every sibling can read; `Wait` for all tasks to
finish; return the first recorded error. The `Once` guarantees exactly one
"first" error wins even when several fail at once, and the shared child means one
failure tears down the rest. Build it from primitives once and `errgroup` stops
being magic.

## Common Mistakes

### Forgetting to call cancel

Wrong: `ctx, _ := context.WithCancel(parent)`. The discarded `cancel` leaks the
child context — and any `AfterFunc` or timer state hung off it — until the parent
is cancelled, which in a server rooted at `context.Background()` is never. Fix:
`ctx, cancel := context.WithCancel(parent); defer cancel()`. `go vet` reports
this as `lostcancel`; treat it as a failed build.

### Comparing ctx.Err() with ==

Wrong: `if ctx.Err() == context.Canceled`. It works only because nothing has
wrapped the value yet; it breaks silently the day an error travels up through a
`%w` wrap or a custom cause. Fix:
`errors.Is(ctx.Err(), context.Canceled)` and
`errors.Is(err, context.DeadlineExceeded)`.

### Confusing ctx.Err() with context.Cause(ctx)

Wrong: after a `WithCancelCause` cancel, logging `ctx.Err()` — which is still the
coarse `context.Canceled` — and thinking you recorded the reason. The specific
"why" lives only in `context.Cause(ctx)`. Fix: use `ctx.Err()` for control flow
(is it canceled or deadline?) and `context.Cause(ctx)` for the diagnostic you
deliberately attached. Logging `Err()` instead of `Cause()` throws away the
reason.

### Storing a context in a struct field

Wrong: `type Service struct { ctx context.Context }`. That freezes one context
for the object's whole lifetime and hides cancellation from the individual calls
that should carry their own. Fix: pass `ctx` as the first parameter of every
method that needs it. The Go blog's "Contexts and structs" is the canonical
statement of this rule.

### Passing context.Background() in the middle of a call chain

Wrong: a repository method that accepts `ctx` but issues the SQL query with
`context.Background()`. The request's deadline and cancellation never reach the
database, so an aborted request keeps hammering it. Fix: thread the received
`ctx` into every downstream call — the DB driver, the HTTP client, the child
goroutine.

### Treating cancellation as a hard stop

Wrong: assuming `cancel()` kills goroutines. It only closes `Done()`. A worker
that never `select`s on `ctx.Done()`, or a syscall issued with no deadline, runs
to completion regardless. Fix: make every long operation block on or poll
`ctx.Done()`; cancellation is a signal the code must choose to honor.

### Sleeping between retries with time.Sleep

Wrong: `time.Sleep(backoff)` between attempts — a bare sleep ignores cancellation,
so a cancelled request still waits out the full backoff before noticing. Fix:
sleep via a `select` on a `*time.Timer`'s channel versus `ctx.Done()`, and
`Stop()` the timer so you do not leak it.

### Detaching background work with the wrong tool

Wrong: firing a background audit write with a bare `context.Background()` (which
loses the request's trace id and other values), or with `context.WithoutCancel`
but no timeout (which lets the detached work run unbounded). Fix: use
`context.WithoutCancel(parent)` to keep the values, then wrap it in
`context.WithTimeout` to bound it.

### Leaking a producer goroutine on the send case

Wrong: a generator that does `out <- v` without also selecting on `ctx.Done()`.
The moment the consumer stops reading after a cancel, that send blocks forever and
the producer goroutine leaks. Fix: every channel send inside a cancellable
goroutine must be a `select` against `ctx.Done()`, so an unread send unwinds the
goroutine instead of wedging it.

Next: [01-worker-cancel-loop.md](01-worker-cancel-loop.md)
