# Context Propagation — Concepts

A `context.Context` is worthless unless it *reaches* the code that does the work.
Creating a 5-second deadline in an HTTP handler means nothing if the deadline
never travels down to the database driver, the outbound HTTP client, or the
fan-out workers that actually block on I/O. Propagation is the invariant that
carries three things — cancellation, deadlines, and request-scoped metadata —
from the edge of the system to its leaves. In production this single invariant is
where most latency-budget overruns and goroutine leaks are born: one layer that
quietly swaps the caller's `ctx` for `context.Background()` detaches the entire
downstream subtree from the request's lifetime, and from that moment a client
hang-up or an SLO timeout can no longer stop anything below it. This file is the
conceptual spine of the lesson; read it once and each of the nine independent
exercises that follow will make sense on its own.

Treat propagation not as a style preference but as a load-bearing architectural
contract — one a senior engineer designs deliberately and then *enforces*, by
tooling rather than by hoping every future pull request remembers the rule.

## Concepts

### Propagation is a first-parameter rule

Every function that performs I/O or may run for a non-trivial time takes
`context.Context` as its **first** parameter, conventionally named `ctx`, and
forwards that same `ctx` to every downstream call it makes. This is not folklore;
the `context` package documentation states it directly: "Do not store Contexts
inside a struct type; instead, pass a Context explicitly to each function that
needs it ... The Context should be the first parameter, typically named ctx." The
rule has a positive form — thread your `ctx` through everything — and a negative
form that is the more important one to internalize: never manufacture a fresh
root context in the middle of a call chain.

The reason the parameter position is fixed by convention is that it makes the
contract *mechanically checkable*. If `ctx` is always `In(1)` of every exported
method, a reflection-based fitness test can walk a registry of handlers and fail
CI the moment someone adds a method that breaks the shape. A convention you can
only enforce by code review is a convention that eventually rots; the final
exercise turns this one into a test.

### `context.Background()` mid-chain is the canonical propagation bug

The single most common context defect is a function that accepts `ctx` and then
ignores it, calling a downstream dependency with `context.Background()` (or
`context.TODO()`) instead:

```go
func (b *BrokenService) GetUser(ctx context.Context, id string) (string, error) {
	// BUG: the caller's deadline and cancellation are thrown away here.
	return b.repo.Get(context.Background(), id)
}
```

`context.Background()` is never cancelled and has no deadline, so everything below
this line is now detached from the request. When the client disconnects or the
5-second SLO deadline fires, the cancellation signal propagates down the tree
until it hits this node and stops. The database query runs to completion against
a caller that is already gone; the goroutine that issued it outlives the request;
under load these accumulate into a goroutine leak and a connection-pool
exhaustion that looks, from the outside, like a mysterious latency cliff. The
fix is trivial to state and easy to forget: forward the `ctx` you were given.

A structural variant of the same bug is storing the context in a struct field and
reading it back later. `type Service struct { ctx context.Context }` captures a
single snapshot at construction time; a method that reads `s.ctx` is reading a
context that has nothing to do with the *current* request. Pass `ctx` per call
instead. The Go blog post "Contexts and structs" is the canonical treatment.

### Wrap the cause with `%w`, not `%v`

When a downstream call returns a cancellation error, the caller should wrap it
with `fmt.Errorf("layer: %w", err)`. The `%w` verb preserves the wrapped error in
the chain so that `errors.Is(err, context.DeadlineExceeded)` still returns true at
the top of a stack that is three or five layers deep. The `%v` verb flattens the
error into a plain string; the classification information is destroyed, and a
top-level handler that wants to emit the right metric label or status code can no
longer tell "we ran out of time" from "the user cancelled" from "the key was not
found". Every layer that adds context to an error and still wants that error to be
*classifiable* must use `%w`. This is why a handler can log a richly-prefixed
message like `handler: service: repo: context deadline exceeded` and *also*
branch correctly on `errors.Is(err, context.DeadlineExceeded)` — the string is
for humans, the wrapped chain is for machines, and `%w` gives you both.

### `ctx.Err()` versus `context.Cause(ctx)`

`ctx.Err()` is deliberately coarse: it returns only `context.Canceled` or
`context.DeadlineExceeded`, and nothing else. That is the right vocabulary for
"should I stop?" but the wrong vocabulary for "*why* did we stop?", which is what
metrics and error taxonomies need. Since Go 1.20, `context.Cause(ctx)` returns the
domain-specific error that was attached when the context was cancelled — the
value passed to a `CancelCauseFunc`, or the cause registered with
`WithTimeoutCause`/`WithDeadlineCause`. This lets a request coordinator answer a
much richer question than `Err()` can: was this a genuine deadline, a client that
hung up, or an operator-triggered load-shed?

The API shape matters and traps people:

- `context.WithCancelCause(parent)` returns a `CancelCauseFunc` — a
  `func(cause error)`. Calling `cancel(ErrClientDisconnected)` sets the cause;
  `ctx.Err()` is still `context.Canceled` but `context.Cause(ctx)` returns
  `ErrClientDisconnected`.
- `context.WithTimeoutCause(parent, d, cause)` (Go 1.21) attaches a cause to the
  *timeout* path. When the timer fires, `ctx.Err()` is `context.DeadlineExceeded`
  and `context.Cause(ctx)` returns your custom `cause`. Crucially the `CancelFunc`
  it returns does **not** set the cause — only the timeout firing does. If you
  cancel it manually before the timer, the cause is `context.Canceled`.
- For a plain `WithTimeout` (no cause) or `WithCancel`, `context.Cause(ctx)` falls
  back to `ctx.Err()`, so you get `DeadlineExceeded`/`Canceled` — never nil after
  cancellation. Before any cancellation, `context.Cause(ctx)` returns nil.

The classification module builds a coordinator around exactly these distinctions.

### Fan-out shares one cancellation signal

When you launch N concurrent calls, hand every goroutine the *same* `ctx`. One
close of `ctx.Done()` then wakes all of them at once — a timeout or a first
failure cancels the whole fan-out simultaneously rather than each call timing out
serially. The hand-rolled shape is a buffered result channel, a `sync.WaitGroup`,
and a `close(out)` after `wg.Wait()` so the receiver's `range` terminates cleanly.

`golang.org/x/sync/errgroup` formalizes this pattern and is what you reach for in
real code. `errgroup.WithContext(ctx)` returns a group and a derived context; the
first goroutine to return a non-nil error cancels that derived context, so the
remaining goroutines observe `ctx.Done()` and abandon their work. `Group.SetLimit(n)`
bounds the number of in-flight goroutines, which is essential when the fan-out
would otherwise open a thousand simultaneous connections to a downstream that can
handle ten. Two data-race traps ride along with fan-out: write results into a
preallocated slice indexed by position (or serialize through a channel) rather
than into a shared map, and — on toolchains before Go 1.22 — copy the loop
variable; on Go 1.22+ each iteration has its own variable, and `for i := range n`
with index-based writes sidesteps the issue entirely.

### Deadline budgeting: derive down, never widen

An end-to-end SLO is a budget that shrinks as it is spent. If a request arrives
with a 200ms deadline and the auth hop takes 40ms, the profile hop should see
roughly 160ms remaining, and the billing hop after it even less. You implement
this by reading `ctx.Deadline()`, computing `time.Until(deadline)`, and deriving a
per-hop child timeout that can only *reduce* the inherited deadline. The
anti-pattern is a downstream hop that calls `context.WithTimeout(ctx, 5*time.Second)`
unconditionally: if the inherited deadline is tighter, the child correctly keeps
the earlier one (a context's effective deadline is the minimum of its own and its
parent's), but if the inherited deadline is *looser* or absent, this hop has
silently granted itself a fresh, generous budget that overrides the SLO. Read the
inherited deadline first; only shorten. And fail fast: if the remaining budget is
below a floor, there is no point starting a call that cannot possibly finish in
time — return an "budget exhausted" error immediately instead of doing doomed
work and timing out anyway.

### `context.WithValue` is for request-scoped metadata only

`context.WithValue` carries request-scoped, immutable metadata — a request ID, a
tenant identifier, an authenticated subject — across API boundaries without
threading extra parameters through every signature. It is emphatically *not* a
general-purpose parameter-passing or dependency-injection mechanism; optional
function arguments and mutable state do not belong in a context. Two rules keep it
safe. First, key the value with an **unexported package-local type**, never a bare
string or an exported type, so that two packages cannot collide on the same key.
Second, expose typed accessors (`RequestID(ctx) (string, bool)`) that do a
comma-ok type assertion and return a safe zero value when the key is absent,
rather than letting callers reach into `ctx.Value` and panic on a type mismatch.
Values survive context derivation: a child from `WithTimeout`/`WithCancel` still
sees every value its ancestors carried.

### Detached cleanup outlives cancellation

Cancellation is contagious downward and that is usually what you want — but some
work must run *because* the request was cancelled, and therefore must not itself
be cancelled by that same signal. Recording that a request was aborted, releasing
a distributed lock, flushing a buffered audit write: if you run these on the
already-cancelled request context, they are cancelled before they can complete.
Go 1.21 added `context.WithoutCancel(parent)`, which derives a child that keeps
the parent's *values* (so the correlation ID survives) but drops its cancellation
and deadline — its `Done()` channel is nil and its `Err()` stays nil after the
parent is cancelled. Pair it with `context.AfterFunc(parent, f)`, which runs `f`
in its own goroutine the instant `parent` is cancelled and returns a
`stop func() bool`: `stop()` returns true if it de-registered `f` before it ran,
false if `f` had already been scheduled. The detached-cleanup module wires these
together so a lease is released and an audit row is written even after the caller
hangs up.

### Cancellation is one-directional

Values flow down and so does cancellation, but never up. Cancelling a parent
cancels every descendant; cancelling a child never touches the parent or its
siblings. This is what makes per-request child contexts safe: a handler can
`WithTimeout` off the server's base context, and cancelling that child when the
request ends cannot disturb any other in-flight request. It is also why
`WithoutCancel` is the *only* way to escape a parent's cancellation — there is no
"re-parent to something longer-lived" short of explicitly detaching.

### The contract can be enforced mechanically

Because "ctx is the first parameter" is a structural property, you can check it
with reflection instead of trusting review. Given a registry of service and
handler values, a fitness test walks every exported method, and for each method
that takes parameters asserts that `method.Type.In(1)` implements
`context.Context` (obtained as `reflect.TypeOf((*context.Context)(nil)).Elem()`),
with a documented allowlist for zero-argument accessors. Wired into CI, this turns
a review-time convention into a gate that catches a propagation regression before
it ships — the same philosophy as an architecture-fitness test in a large
codebase.

## Common Mistakes

### Creating `context.Background()` in the middle of a call chain

Wrong: a function receives `ctx` but calls a dependency with
`context.Background()` or `context.TODO()`. The deadline and cancellation never
reach downstream, the goroutine outlives the request, and the leak is invisible
until the pool exhausts. This is the single most common context bug.

Fix: forward the `ctx` you were handed to every downstream call.

### Storing context in a struct field

Wrong: `type Service struct { ctx context.Context }`, with methods reading
`s.ctx`. The struct holds a stale snapshot from construction time; cancelling the
current request never reaches work that reads the field later.

Fix: pass `ctx` as the first parameter of each method. See "Contexts and structs".

### Wrapping a cancellation error with `%v`

Wrong: `fmt.Errorf("service: %v", err)` flattens the cause into a string, so
`errors.Is(err, context.DeadlineExceeded)` silently returns false at the top of
the stack and the handler mislabels the outcome.

Fix: wrap with `%w` at every layer that adds context to a cancellation error.

### Dropping the CancelFunc

Wrong: `ctx, _ := context.WithCancel(parent)` discards the cancel function; the
derived context's resources are held until the parent cancels, and `go vet`'s
`lostcancel` analyzer flags it.

Fix: `ctx, cancel := context.WithCancel(parent); defer cancel()` — always.

### Confusing `context.Cause` with `ctx.Err`

Wrong: reading `context.Cause(ctx)` when you meant `ctx.Err()` (or the reverse).
Before cancellation `Cause` returns nil; the `CancelFunc` returned by
`WithTimeoutCause` does not set the cause — only the timeout firing does; and for
a plain `WithTimeout`, `Cause` falls back to `DeadlineExceeded`.

Fix: use `ctx.Err()` for the coarse "canceled vs deadline" branch and
`context.Cause(ctx)` for the domain-specific reason feeding metrics.

### Widening an inherited deadline

Wrong: a downstream hop calls `context.WithTimeout(ctx, generousTimeout)` without
inspecting the inherited deadline, silently granting itself a fresh budget that
overrides the end-to-end SLO when the parent's deadline is looser or absent.

Fix: read `ctx.Deadline()`/`time.Until(deadline)` and derive a per-hop timeout
that can only shorten; fail fast below a floor.

### Using a string or exported type as a context key

Wrong: `ctx.Value("request_id")` with a bare string key (or an exported type)
invites collisions across packages that happen to pick the same key.

Fix: define an unexported package-local key type and expose typed accessors.

### Running cleanup on the cancelled request context

Wrong: flushing the audit buffer or releasing the lease on the already-cancelled
`ctx`, so the cleanup is itself cancelled and never completes.

Fix: derive `context.WithoutCancel(ctx)` for the cleanup so it survives the
request's cancellation, and trigger it with `context.AfterFunc`.

### Racing on fan-out results

Wrong: N goroutines writing into one shared `map` — a data race that `-race`
reports.

Fix: write into a preallocated slice indexed by position, or serialize results
through a channel; on pre-1.22 toolchains also copy the loop variable.

Next: [01-layered-propagation-stack.md](01-layered-propagation-stack.md)
