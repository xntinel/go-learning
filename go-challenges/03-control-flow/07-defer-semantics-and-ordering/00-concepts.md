# Defer Semantics and Ordering: Cleanup, LIFO, and Named-Return Error Handling — Concepts

`defer` is the mechanism that makes Go cleanup correct across every exit path a
function has: a normal `return`, an error return, or a panic unwinding through
the stack. Senior backend work lives on those exit paths. Closing DB rows,
unlocking a mutex, rolling back a transaction, draining and closing an HTTP
response body so the keep-alive connection is reused, flushing a buffered writer,
capturing a shutdown error — each is a cleanup that must run no matter how the
function leaves. `defer` is what guarantees it runs, and the subtle failures are
all senior-level: an ignored deferred `Close()` error that silently drops
buffered data, a `defer` inside a loop that leaks file descriptors until the
process hits its `ulimit`, a deferred metric that snapshots `time.Now()` at the
wrong instant, a deferred rollback that must observe the function's final error.
This file is the conceptual foundation for the toolkit of production cleanup
patterns in the exercises that follow. Read it once and every module makes sense.

## Concepts

### Execution timing: at function return, not at end of block

A deferred call runs when the *surrounding function* returns — not at the end of
the enclosing block, not at the end of a loop iteration, not when the variable
goes out of scope. "Returns" means all three of the ways a function can leave: a
`return` statement, an unrecovered panic unwinding through the frame, or
`runtime.Goexit`. This is the property that makes `defer` a reliable cleanup
contract: whatever path the function takes out, the deferred call fires exactly
once on the way out. It is also the property that surprises people who expect
block scoping. A `defer` written inside an `if` or a `for` still fires at the end
of the whole function, having merely been *registered* when control reached it.

### LIFO ordering: last registered runs first

When a function registers several deferred calls, they run in last-in-first-out
order at return. Register A, then B, then C; on return C runs, then B, then A.
This is not an accident of implementation — it is exactly what correct teardown
of a layered resource stack needs. You acquire a pooled connection, then begin a
transaction on it, then take an advisory lock inside the transaction. The correct
release order is the reverse: drop the lock, roll back or commit the transaction,
return the connection. Stacking one `defer` per acquisition, in acquisition
order, produces reverse-order release for free, with no manual bookkeeping. If
you instead hand-order the cleanup and release the connection before the lock,
you have a bug; LIFO defers make that class of bug impossible to write by
accident.

### Argument evaluation timing: now, not at return

This is the single most misunderstood rule. When control reaches a `defer`
statement, Go evaluates *the function value, the receiver, and every argument*
immediately, then postpones only the *call*. `defer f(x)` snapshots the current
value of `x` at the `defer` statement; if `x` changes before the function
returns, the deferred call still uses the snapshotted value. `defer
metrics.Observe(time.Since(start))` computes `time.Since(start)` right now —
which, right after `start := time.Now()`, is approximately zero — and defers a
call with that frozen zero duration. The fix is to wrap the work in a closure:
`defer func() { metrics.Observe(time.Since(start)) }()`. The closure value is
evaluated at the `defer` statement, but its *body* — including the
`time.Since(start)` call — runs at return, reading `start` and the clock then.
The rule generalizes: put anything that must be evaluated at return time inside a
deferred closure body, not in the deferred call's argument list.

### Named return values: a deferred closure can read and modify them

A function with named results — `func Save(ctx context.Context) (err error)` —
gives its deferred closures access to those result variables *after* the `return`
statement has assigned them and *before* the caller sees them. This is the idiom
behind two of the most important patterns in backend Go. First, deferred
rollback-or-commit: the closure reads the named `err`; if it is non-nil (or a
panic is in flight) it rolls the transaction back, otherwise it commits, and it
writes any commit/rollback failure back into `err` so the caller observes it.
Second, capturing a cleanup error: a deferred closure that flushes and closes a
writer can assign the first non-nil error into the named `err`, turning a full
disk on `Flush` into a real returned error instead of silent data loss. The
mechanism only works with the function's *named* return variable. A deferred
closure that reads a shadowed local `err` — a fresh `err` declared with `:=`
inside a block — never sees the outcome the caller will see, and decides commit
versus rollback on stale information.

### Defer runs on the panic path too

Deferred calls run while a panic unwinds the stack, which is what lets cleanup
survive a panic: a mutex still unlocks, a connection still closes, a transaction
still rolls back even when the goroutine is crashing. Combined with `recover`
(the next lesson's subject), a deferred closure can also convert a panic into a
returned error. But note the split: the *cleanup* value of `defer` does not
require `recover` at all. You defer `mu.Unlock()` purely so the lock is released
on the panic path; you are not trying to swallow the panic, only to not leave a
lock held while the stack unwinds. Cleanup and recovery are separate uses of the
same mechanism.

### Cost model: cheap, but not free in hot loops

Each `defer` registers a record describing the deferred call. Since Go 1.14,
"open-coded" defers make the common case — a small, statically known number of
defers in a function — nearly as cheap as a direct call; the compiler inlines the
deferred call at each return site. That optimization does not apply when the
number of defers is not statically bounded, which is exactly the defer-in-a-loop
case: those fall back to heap-allocated defer records. So the guidance is
nuanced. Use `defer` freely for the one-or-few cleanups a normal function has;
it is idiomatic and near-free. But in a genuinely hot path that releases a pooled
object every iteration, the per-iteration defer record can show up in a profile,
and an explicit `Release()` call is the right choice there.

### Scope of deferred cleanup is the whole function

`defer mu.Unlock()` is correct-by-default: it guarantees the lock is released on
every exit path. But its *scope* is the entire remaining function body, so the
lock is held until the function returns — including across any slow I/O that
happens after the `defer`. On a hot read-through cache, holding the mutex across
a slow backend load serializes every unrelated key lookup behind it. The fix is
not to abandon `defer mu.Unlock()` — it is to shrink the function whose boundary
bounds the critical section. Extract a small helper that locks, touches the map,
and returns; its `defer mu.Unlock()` now scopes the lock to just the map access,
and the slow load runs in the caller, outside any lock.

### Defer-in-loop accumulates

Because deferred calls fire at *function* return, N iterations of a loop that each
`defer f.Close()` register N deferred calls that all fire together when the
enclosing function finally returns. Every file handle, every connection, stays
open until then. At small N nobody notices; at scale the process runs out of file
descriptors or exhausts a connection pool. The fix is structural: extract a
per-item helper function that opens, works, and defers `Close` *in its own scope*,
so each descriptor closes at the end of its iteration when the helper returns.
The loop calls the helper once per item; the helper's function boundary is the
per-iteration cleanup boundary.

### Deferred cleanup errors must not be discarded

`Close()`, `Flush()`, `Rollback()`, and `resp.Body.Close()` can all fail, and on
write paths the failure is not cosmetic. A buffered writer whose deferred
`Close()` runs but whose `Flush()` error is ignored has silently dropped every
byte still in the buffer. A transaction whose `Rollback()` fails after the primary
operation already failed has left a broken transaction that an operator needs to
see. The `defer f.Close()` shorthand is fine for read paths where a close error
is genuinely uninteresting, but on any write path the deferred cleanup must
capture its error — into a named return via a closure, or joined with
`errors.Join` when there is already a primary error to preserve. Swallowing a
rollback error because "the operation already failed anyway" hides exactly the
signal that says the datastore is now in an uncertain state.

### Multiple defers versus one closure

Several independent `defer` statements that unwind LIFO are the most readable
choice when the cleanups are independent — each acquisition pairs with its own
release and you want reverse order automatically. A *single* deferred closure is
the right tool when the cleanup must branch on the final `err` (commit versus
rollback), coordinate an explicit ordering, or capture and combine multiple
cleanup errors into one returned value. Reach for the closure form deliberately,
when its extra power is needed; otherwise prefer the plain stacked defers.

### Modern Go: the loop-variable capture dance is gone

Before Go 1.22, a loop variable was shared across iterations, so a `defer` or a
goroutine closure that captured it saw the final value; the workaround was to
shadow it with `x := x` at the top of the loop body. Go 1.22 changed loop
semantics so each iteration has its own copy of the loop variable. Capturing the
loop variable in a deferred closure now does the obvious thing, and the `x := x`
dance is unnecessary and misleading — leaving it in signals code written for an
older toolchain or by someone unsure of the version. The surrounding code in
these exercises also uses the other modern idioms where they fit: `for i := range
n` for counted loops and `t.Context()` in tests.

## Common Mistakes

### Deferring resource release inside a for loop

Wrong: `for _, p := range paths { f, _ := os.Open(p); defer f.Close(); ... }`.
Every handle stays open until the enclosing function returns, so a large
directory exhausts the process's file descriptors. Fix: extract a helper that
opens, works, and `defer`s `Close` in its own scope, and call it once per item;
each descriptor now closes at the end of its iteration.

### Ignoring a deferred Close or Flush error on a write path

Wrong: `defer f.Close()` on a buffered writer while never checking `Flush`.
Buffered bytes are dropped and the error vanishes. Fix: use a named-return
deferred closure that flushes, then closes, and assigns the first non-nil error
into the returned `err`, so a full disk surfaces as a real error.

### Using the argument form when you need the value at return time

Wrong: `defer metrics.Observe(time.Since(start))`. `time.Since(start)` is
evaluated at the `defer` statement — approximately zero right after `start` is
set — and the frozen zero is what gets recorded. Fix: the closure form,
`defer func() { metrics.Observe(time.Since(start)) }()`, which reads the clock at
return.

### Forgetting that defer holds the lock for the whole function

Wrong: `defer mu.Unlock()` at the top of a method that then does a slow backend
load; the lock is held across the I/O and serializes unrelated work. Fix: extract
a tiny critical-section helper whose function boundary scopes the lock to just
the map access, and do the slow load outside it.

### Closing a response body without draining it (or not at all)

Wrong: `defer resp.Body.Close()` without first draining with
`io.Copy(io.Discard, resp.Body)`, so the keep-alive connection is not reused; or
forgetting to close on the non-2xx branch entirely. Fix: a deferred closure that
drains then closes, placed before any status check so it runs on every path.

### Reading a shadowed local err in a deferred rollback closure

Wrong: the deferred closure inspects an `err` that was re-declared with `:=`
inside a block, so it never sees the function's real outcome and commits or rolls
back on stale information. Fix: give the function a *named* return `err` and have
the closure read and write that.

### Assuming defers run at end of block or loop iteration

Wrong: expecting a `defer` inside a `for` or `if` to fire when that block ends.
It fires at *function* return. Fix: if you need per-iteration cleanup, move the
work into a helper whose return is the cleanup point.

### Keeping the pre-1.22 loop-variable capture workaround

Wrong: writing `s := s` before capturing a loop variable in a deferred closure on
a Go 1.22+ toolchain — unnecessary and misleading. Fix: capture the loop variable
directly; each iteration already has its own copy. (Conversely, do not *assume*
per-iteration capture without knowing the toolchain is 1.22+.)

### Swallowing a rollback error after the primary operation failed

Wrong: on the error path, calling `tx.Rollback()` and discarding its error
because the operation already failed. A failed rollback leaves a broken
transaction operators must know about. Fix: join the rollback error with the
primary error via `errors.Join` so both surface.

### Using defer for hot-path per-iteration pooled-object release

Wrong: `defer pool.Put(obj)` inside a tight loop where the defer record shows up
in profiles. Fix: call `pool.Put(obj)` explicitly at the end of the iteration;
reserve `defer` for the common one-or-few-cleanups case.

Next: [01-txstore-deferred-rollback.md](01-txstore-deferred-rollback.md)
