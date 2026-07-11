# Defer, Stacking, and Deterministic Resource Cleanup — Concepts

`defer` is the mechanism Go gives you for one job: guaranteeing that something
runs on the way out of a function, no matter which way out that is. Unlock a
mutex, close a file, return a connection to a pool, roll back a transaction,
turn a panic into a 500 — all of these are "on the way out" work, and all of
them must happen on the normal return, the error return, and the panic. That is
the whole reason `defer` exists, and it is why a senior Go codebase is full of
it. The mechanics are small; the ways to get them subtly wrong are not. This
file is the conceptual foundation for the nine independent exercises that
follow. Read it once and you have the model you need to reason through every one
of them.

## The LIFO stack

Deferred calls run in last-in-first-out order when the surrounding function
returns. Three defers run third, second, first. That order is not an accident of
implementation; it is exactly the order you want for nested resource ownership.
If you acquire a lock, then begin a transaction, then open a file, the correct
teardown is the mirror image: close the file, roll back or commit the
transaction, release the lock. Registering `defer` in acquisition order gives you
release in reverse order for free. The first resource acquired is the last one
released, which is the only ordering that keeps every resource valid for as long
as anything nested inside it still needs it.

This LIFO symmetry is the same principle behind graceful shutdown (stop
accepting new requests, then close the database, then flush telemetry — reverse
of startup) and behind saga-style rollback (undo the steps you completed, newest
first). When the set of things to tear down is known statically, a stack of
`defer` statements expresses it. When it is dynamic — decided at runtime — you
build the LIFO stack yourself out of closures, which is `defer` semantics you
control explicitly.

## Argument evaluation happens at the `defer` statement, not at the call

This is the single most common `defer` bug, and it costs people hours. A
deferred call's function value, receiver, and arguments are all evaluated the
moment the `defer` statement executes — not when the deferred call actually runs
at return time. So

```go
start := time.Now()
defer record(time.Since(start))
```

freezes `time.Since(start)` immediately, when it is essentially zero, and records
that zero at return. The duration you wanted — the time the function actually
took — is never measured. The fix is to defer a closure instead:

```go
start := time.Now()
defer func() { record(time.Since(start)) }()
```

The closure body runs at return time and reads `time.Now()` live, so it sees the
real elapsed time. The same rule is what lets a deferred closure read a named
return value that the function sets later, or read a status code assigned near
the end of the handler: the closure captures variables by reference and observes
their final values, whereas an argument captures a snapshot at the defer point.

## `defer` runs on every exit — return, error, and panic

A deferred call fires on a normal `return`, on an early error `return`, and while
a panic unwinds the stack. That is precisely what makes it the right tool for
cleanup and for recover boundaries: there is no exit path that skips it. The two
exceptions are worth memorizing because they are absolute. `os.Exit` terminates
the process immediately and runs no deferred functions. A fatal crash of the
process (a `log.Fatal`, which calls `os.Exit`, or a signal that kills the
process) does the same. Deferred cleanup is a within-process, within-goroutine
guarantee, not a "the program is ending" guarantee.

## Named returns are how a defer changes what the caller sees

Without a named return value, a deferred closure can observe the world but cannot
alter the value the function hands back. With one, it can. This is the mechanism
behind three production-critical patterns:

- Capturing a `Close`/`Flush`/`Commit` error into the returned error. A write can
  succeed while the final flush fails (disk full, a short write to a network
  sink); `defer f.Close()` discards that error silently. The correct form is
  `defer func() { err = errors.Join(err, f.Close()) }()` over a named `err`.
- Wrapping the error with context on the way out (`fmt.Errorf("...: %w", err)`),
  so the caller gets a `%w`-wrapped chain it can match with `errors.Is`.
- Converting a recovered panic into a returned error, so a panic at a boundary
  becomes an ordinary error the caller can handle.

If the function signature is `func ... (err error)` and the deferred closure
assigns to `err`, the caller sees the assignment. If it is `func ... error` with
an anonymous return, the closure cannot influence the result.

## recover only works from a directly-deferred function

`recover()` stops a panic and returns its value only when it is called *directly*
inside a function that was deferred in the panicking goroutine. Call it anywhere
else — at the top level, or inside a helper that the deferred function calls —
and it returns `nil` while the panic keeps unwinding. So a recover boundary is
always shaped like this:

```go
defer func() {
	if r := recover(); r != nil {
		// clean up, translate, or re-panic
	}
}()
```

recover is also goroutine-local. A `defer`/`recover` in one goroutine cannot
catch a panic in another; an unrecovered panic in any goroutine crashes the whole
process. If you start a goroutine that might panic, it needs its own recover
boundary.

The discipline that separates a robust boundary from a dangerous one is
selectivity. A recover-all middleware that swallows everything hides real bugs
and, worse, breaks protocols that use panic as a control signal. In an HTTP
server, `http.ErrAbortHandler` is a sentinel the server itself panics with to
abort a connection; a recovery middleware must re-panic on it rather than turn it
into a 500. The rule is: recover, log the value together with `debug.Stack()` so
the failure is never silent, translate what you can legitimately handle, and
re-panic on the values you must not swallow.

## The cost of defer, and the one place it actually bites

Historically each `defer` allocated a small record on the heap, which is why
old advice fretted about `defer` in hot loops. Since Go 1.14 the compiler
"open-codes" defers — for the common case of at most eight defers that are not
inside a loop, the deferred call is compiled almost as if you had written it
inline at each return, and the cost is negligible. So do not contort readable
code to avoid a defer in a normal function.

The real footgun is `defer` inside a loop. A deferred call is scoped to the
enclosing *function*, not the enclosing block or loop iteration. So

```go
for _, path := range paths {
	f, _ := os.Open(path)
	defer f.Close()
}
```

does not close each file at the end of its iteration; it stacks up one deferred
`Close` per iteration and runs them all only when the whole function returns. Feed
it ten thousand paths and you hold ten thousand file descriptors open at once —
a descriptor leak that looks fine in a small test and exhausts the process in
production. The fix is to extract a per-iteration helper: move the open/use/close
into its own function so the `defer` fires at the end of each call, keeping at
most one resource live.

## The exception-safe unlock, and when the defer is wrong

`mu.Lock(); defer mu.Unlock()` is the canonical safe pattern: whatever happens in
the critical section — early return, error, panic — the lock is released. Use it
by default. But `defer` also *widens* the critical section to the entire rest of
the function. If slow work follows the part that actually needs the lock — an
I/O write, a blocking channel send, an RPC — the deferred unlock holds the lock
across all of it, turning a mutex into a serialization bottleneck and a source of
latency. The senior move is the copy-under-lock pattern: take the lock, copy the
shared state you need (for a map, `maps.Clone`), release the lock explicitly, and
*then* do the slow work on your private copy. The defer is right for the common
short critical section and wrong the moment the function keeps doing slow work
after the state is read.

## Idempotent close and the exactly-once / exactly-one-of invariants

A `Close` that might be called more than once — by a `defer` and an explicit call,
or by two goroutines racing — must be safe the second time. "Safe" means a no-op,
not a double free, not a second return of the same connection to a pool (which
over-fills it and hands the same connection to two callers). Guard it with an
`atomic.Bool` and `CompareAndSwap`, so exactly one caller wins the transition
from open to closed and the rest return immediately, or with `sync.OnceFunc`.
This is the exactly-once invariant.

Its cousin is the exactly-one-of invariant, which governs transactions: on any
given path you must call *either* `Commit` *or* `Rollback`, never both and never
neither. The idiomatic implementation keys the choice on a named return error
inside a single deferred closure — roll back if the function is returning an
error or panicking, commit otherwise, and surface the commit error if the commit
itself fails.

## Bounding every wait with a context

Deferred cleanup guarantees a resource is released *eventually*, but "eventually"
is not good enough when a dependency hangs. A pool that hands out connections
does not police its borrowers; the borrower's `defer conn.Close()` is the
contract, and a slow or forgotten borrower can starve everyone else. Bound the
borrow with a context deadline so `Acquire` gives up instead of blocking forever.
The same applies at process scope: `Server.Shutdown(context.Background())` lets a
single stuck in-flight request block the process from ever exiting; always pass a
`context.WithTimeout` and proceed to close the remaining resources when it
expires. Deterministic cleanup means bounded cleanup.

## Common Mistakes

### defer inside a range loop over many items

`for _, p := range paths { f, _ := os.Open(p); defer f.Close() }` holds every
file handle until the enclosing function returns, because a defer is
function-scoped, not iteration-scoped. Extract a helper that opens, uses, and
defers `Close` within one call, so each handle is released at the end of its own
iteration and at most one is live.

### Dropping the Close/Flush error

`defer f.Close()` on a writable file or a buffered writer silently discards the
error from the final flush. A write that succeeded in memory but failed to reach
the disk or the network is reported as success. Capture it with a named return:
`defer func() { err = errors.Join(err, f.Close()) }()`.

### Passing a value to defer and expecting it read later

`defer record(time.Since(start))` evaluates the duration at the `defer` statement
(near zero), not at return. Use a closure — `defer func() { record(time.Since(start)) }()`
— to read live values, including named returns set later in the function.

### Calling recover outside a directly-deferred function

recover in a helper that the deferred function calls, or at the top level of the
function, returns `nil` and the panic keeps unwinding. It must be called directly
inside the deferred function, in the same goroutine that panicked.

### A recover-all middleware that swallows everything

Turning every recovered value into a generic 500 hides real bugs and breaks
protocols. Re-panic on `http.ErrAbortHandler` and on runtime errors you cannot
legitimately handle, and always log the recovered value with `debug.Stack()` so
the failure is not silent.

### Deferring Unlock and then doing slow I/O

`mu.Lock(); defer mu.Unlock()` followed by a network write holds the lock across
the write, serializing every other goroutine behind slow I/O. Copy the guarded
state under the lock, unlock explicitly, then do the slow work on the copy.

### Rolling back partial setup with defers that also fire on success

A fixed sequence of `defer cleanup()` statements meant to undo partial work also
tears down the resources on the success path. Use an armable cleanup stack you
disarm on success, or key the rollback on a named return error so it runs only
when the function is actually failing.

### Non-idempotent Close

A `defer Close` plus an explicit `Close`, or two goroutines both closing,
double-frees or returns the same connection to a pool twice. Guard the transition
with `atomic.Bool.CompareAndSwap` or `sync.OnceFunc` so the second call is a
no-op.

### Violating the exactly-one-of transaction invariant

A helper that commits but ignores the commit error, or that rolls back after a
successful commit, breaks the "exactly one of {commit, rollback}" rule. Drive the
choice from a single deferred closure keyed on the named return error.

### Unbounded graceful shutdown

`Server.Shutdown(context.Background())` lets one hung request block the process
from exiting forever. Pass a `context.WithTimeout` and move on to close the
remaining resources when the deadline expires.

Next: [01-connection-pool-defer-return.md](01-connection-pool-defer-return.md)
