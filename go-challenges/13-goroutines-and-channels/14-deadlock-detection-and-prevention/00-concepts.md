# Deadlock Detection and Prevention in Concurrent Backends — Concepts

A deadlock is the failure mode that turns a healthy service into a silent hang.
There is no crash, no stack trace in the log, no error returned to a caller —
just requests piling up on wedged goroutines until the load balancer's timeout
fires and someone gets paged for "high latency" with nothing in the logs to
explain it. Unlike a panic, a deadlock is invisible from the outside until it has
already exhausted a resource. This lesson treats deadlock as an operability
problem, not a syntax curiosity: every module is a production-shaped artifact — a
money transfer, a read-through cache, a bounded worker pool during shutdown, a
request/response RPC over channels, a lease manager acquiring N locks — where one
mistaken lock order, channel handshake, or shutdown sequence can wedge the whole
thing. The goal is to prove freedom from deadlock by design, and to test for it so
a hang shows up in CI as a goroutine dump instead of in production as a page.

## The four Coffman conditions

A deadlock requires four conditions to hold simultaneously, first stated by
Coffman, Elphick, and Shoshani in 1971:

1. **Mutual exclusion** — a resource (a mutex, a channel slot) is held by at most
   one goroutine at a time.
2. **Hold-and-wait** — a goroutine holds one resource while blocking to acquire
   another.
3. **No preemption** — a resource cannot be forcibly taken from the goroutine
   holding it; it is only released voluntarily.
4. **Circular wait** — there is a cycle of goroutines, each waiting for a resource
   the next one in the cycle holds.

The practical consequence is the whole discipline of this lesson: breaking *any
one* of the four prevents deadlock. You rarely get to remove mutual exclusion (the
lock exists for a reason) or no preemption (Go mutexes are not preemptible). So the
two conditions you actually attack are hold-and-wait and circular wait. Every
prevention technique here maps to breaking a specific condition: a global lock
order removes circular wait; `TryLock` with backoff removes hold-and-wait; a
cancellable `select` removes the "no way out" that turns a blocked goroutine into a
permanent one.

## The runtime detector only sees TOTAL deadlock

Go's runtime has a deadlock detector, and it is worth understanding exactly what it
does and does not catch, because misunderstanding it is the single most dangerous
assumption a backend engineer can make here. When the scheduler finds that *every*
goroutine in the program is blocked with no possibility of progress, it aborts with
`fatal error: all goroutines are asleep - deadlock!`. That is a total deadlock: the
whole process is frozen and the runtime can prove it.

Real services almost never hit total deadlock. A service has an HTTP server
goroutine parked in `accept`, background tickers, a metrics exporter — there is
always *some* runnable goroutine, so the "all asleep" condition is never true. What
actually happens in production is a **partial deadlock**: the two goroutines
handling a money transfer wedge in a circular wait, or a producer blocks forever on
a channel nobody reads, while the rest of the process keeps running. The runtime
detector never fires. The request goroutines are stuck; the health check still
returns 200; latency climbs; connections leak. This is invisible to the runtime and
it is the realistic failure. The lesson's through-line: you cannot rely on the
runtime to tell you about the deadlocks that matter — you must design them out and
test for them explicitly, with a watchdog that dumps goroutine stacks.

## Circular wait: a global lock order

If two goroutines must each hold two locks, and one acquires them in the order
(A, B) while the other acquires (B, A), a `A->B` transfer and a `B->A` transfer can
interleave so each holds the lock the other needs. That is textbook circular wait.

The fix is a **total order** over lockable resources. Pick a stable key — an account
ID, a pointer address, a monotonically assigned sequence number — and always acquire
locks in ascending key order, everywhere, no exceptions. With a total order a cycle
is impossible: a cycle would require some goroutine holding a higher-keyed lock while
waiting on a lower-keyed one, which the discipline forbids. Unlock in reverse order
via stacked `defer`s so release mirrors acquisition. This generalizes cleanly from
two locks to N: sort the resources by key, acquire in order, and no batch — of any
size, over any overlapping subset — can deadlock against another.

The key must be stable and total. Sorting by a mutable field, or by a key that can
collide (two distinct resources comparing equal), reintroduces the cycle. When two
resources share a key you need a tie-breaker that is itself total, such as the
pointer address via `uintptr`.

## Hold-and-wait: TryLock and bounded backoff

`sync.Mutex.TryLock` (Go 1.18+) attempts to acquire the lock and returns `false`
immediately if it is held, rather than blocking. This lets you break hold-and-wait:
acquire the first lock, then *try* the second; if the try fails, release the first
lock and start over. Because you never hold one lock while blocking on another, the
hold-and-wait condition never holds.

There is one absolute rule about `TryLock`: a `false` return means "the lock is
contended, back off" — it never means "proceed without the lock." Treating `false`
as permission to continue is a data-corruption bug far worse than the deadlock you
were avoiding. The correct response to `false` is to release what you hold and retry
with **bounded backoff**: sleep a short, jittered interval and try again, under a
context deadline so the retry loop cannot spin forever. Jitter (a random component
via `math/rand/v2`) prevents two goroutines from retrying in lockstep and colliding
repeatedly — a livelock, which is deadlock's busy-waiting cousin. When the deadline
expires, return `context.DeadlineExceeded` rather than looping again: a bounded wait
that gives up is preferable to an unbounded one that hangs.

## Non-reentrant mutexes and the locked/unlocked method pair

`sync.Mutex` is **not reentrant**: a goroutine that already holds the mutex will
deadlock if it tries to lock it again, directly or transitively. There is no owner
tracking and no recursion count — the second `Lock` simply blocks forever waiting for
a release that can only come from the goroutine that is now stuck trying to lock. This
bites when an exported method locks the mutex and then calls another exported method
that also locks it.

The idiomatic fix is the **locked/unlocked method pair**: the exported method
acquires the lock and calls only private helpers that assume the lock is already
held (and never lock it themselves). Name or document the private helpers so it is
clear they must be called with the lock held (`func (l *Ledger) balance() int64`
assumes locked; `func (l *Ledger) Balance() int64` locks and calls `balance()`).
Never call an exported locking method from inside a locked section. Go's `sync`
package deliberately offers no reentrant mutex, because reentrancy hides exactly the
lock-scope confusion this convention makes explicit.

## RWMutex has no lock upgrade

`sync.RWMutex` allows many concurrent readers or one writer. It does **not** support
lock upgrade: a goroutine holding `RLock` cannot call `Lock` on the same mutex —
that self-deadlocks, because `Lock` waits for all readers (including the caller) to
release, and the caller is waiting on itself. This is the classic read-through cache
bug: `RLock`, miss, then `Lock` to fill, without dropping the read lock first.

The fix is the **double-checked (check-lock-recheck) pattern**: take `RLock`, read;
on a miss, *release* the read lock, take the write `Lock`, then re-check whether
another goroutine filled the entry while you were between locks, and only compute if
it is still missing. The re-check matters because the gap between dropping `RLock`
and taking `Lock` is a window where another writer can win. Optionally add
single-flight via `TryLock` so a thundering herd of readers does not all recompute:
only the goroutine that wins the write lock does the expensive load.

## Channels: every blocking op needs an exit

A send or receive on a channel blocks until the other side is ready. If the other
side never becomes ready — a slow consumer, a caller that timed out, a producer that
already exited — the blocked goroutine is wedged forever. That is a partial deadlock
the runtime cannot see. The discipline: every blocking channel operation in a service
should be a `select` that also watches `ctx.Done()`, so a cancellation or timeout
unblocks the goroutine and it returns cleanly instead of leaking:

```go
select {
case ch <- job:
	return nil
case <-ctx.Done():
	return context.Cause(ctx)
}
```

`context.Cause` (Go 1.20+) returns the specific error the context was cancelled with,
which is more informative than `ctx.Err()` when a `context.WithTimeoutCause` or a
manual `cancel(err)` supplied one.

A **buffered reply channel of capacity 1** solves a specific and common wedge: an
actor-style server goroutine receives requests carrying a reply channel and sends the
response back. If the reply channel is unbuffered and the caller has already timed out
and walked away, the server blocks forever on the send — one slow caller wedges the
whole server. Buffer the reply channel with capacity 1 and the server can always
deposit its response and move on, whether or not the caller is still listening.

## Shutdown ordering: close, drain, wait

Graceful shutdown is where channel and `WaitGroup` mistakes concentrate. The rules:

- **One closer.** A channel is closed by its sole producer (or via `sync.Once`), exactly
  once. Closing from a consumer, or from multiple producers, panics
  (`send on closed channel` or `close of closed channel`). Forgetting to close it means
  a `range` over it never terminates and the ranging goroutine leaks.
- **Results need somewhere to go.** If workers write results into a channel nobody is
  reading during shutdown, they block on the send and `WaitGroup.Wait` never returns —
  a partial deadlock. Buffer the results channel, or run a drain goroutine that keeps
  reading until the workers finish.
- **Order matters.** The correct sequence is: stop accepting new work, close the jobs
  channel, let workers drain remaining jobs and exit, then `Wait`. Waiting before the
  results have a consumer, or closing before the producer is done, both wedge.

## Bounded concurrency without slot leaks

Limiting how many operations run at once — `errgroup.Group.SetLimit(K)` or a
`golang.org/x/sync/semaphore.Weighted` — prevents resource-exhaustion deadlocks
(too many goroutines each holding a connection, waiting for one that will never free
up). But bounded concurrency introduces its own deadlock risk: if a slot (a
semaphore permit) is acquired and then leaked on an error or panic path, the pool
slowly starves until no goroutine can acquire and the whole batch wedges. The
discipline is to `defer` the release immediately after a successful acquire, so no
code path — success, error, or panic — can leave a permit un-returned.

## Detection in practice

Three tools, layered:

- `go test -race` catches data races, which are frequently the root cause behind lock
  misuse; run concurrency tests with `-race -count=1` (or higher `-count`) to shake
  out ordering-dependent bugs that pass once and fail the tenth time.
- A **per-test watchdog** runs the test body under a deadline and, on timeout, calls
  `runtime.Stack(buf, true)` to dump every goroutine's stack and fails the test with
  that dump. This turns an invisible partial deadlock in CI into an actionable stack
  trace pointing at the exact blocked line — the single most valuable tool in this
  lesson.
- In a live service, `GODEBUG` and the `net/http/pprof` goroutine profile
  (`/debug/pprof/goroutine?debug=2`) show where goroutines are parked, so you can see
  a growing pile stuck on the same channel send or lock acquisition.

## Common Mistakes

### Inconsistent lock order across call sites

Wrong: one path locks `from` then `to`, another locks `to` then `from`. The two form
a circular wait under concurrency — the textbook deadlock.

Fix: sort by a stable, total key and acquire in that global order at every call site.
Unlock in reverse via stacked defers.

### Treating a false TryLock as permission to proceed

Wrong: `if !mu.TryLock() { /* continue anyway */ }`, mutating shared state without the
lock and corrupting it.

Fix: `false` means contended. Release what you hold, back off with jitter, and retry
under a deadline, or return an error. Never continue unguarded.

### Holding a mutex across a blocking or external call

Wrong: locking, then calling a callback, doing network I/O, taking another lock, or
sending on a channel while holding the lock. This invites deadlock and adds the full
latency of the slow call to every contending goroutine.

Fix: snapshot the data you need under the lock, release it, then do the slow work.

### Calling an exported locking method from inside the lock

Wrong: `Withdraw` locks the mutex and calls `Balance`, which locks the same
non-reentrant mutex — a self-deadlock.

Fix: split public locked methods from private lock-free helpers. The locked path calls
only helpers that assume the lock is held.

### RLock-then-Lock expecting a lock upgrade

Wrong: hold `RLock`, then call `Lock` on the same `RWMutex` to fill a cache miss.
`RWMutex` has no upgrade and this deadlocks.

Fix: drop the `RLock`, take `Lock`, re-check whether the entry now exists (double-check),
then compute only if still missing.

### Blocking forever with no cancellation branch

Wrong: an unbuffered channel send/receive, or `WaitGroup.Wait`, with no `ctx.Done()`
path. A stuck consumer leaks the producer goroutine — a partial deadlock the runtime
never reports.

Fix: `select` over the operation and `ctx.Done()`; return `context.Cause(ctx)` on
cancellation.

### Closing a channel from the wrong place

Wrong: closing from a consumer or from several producers (panic), or never closing it
so a `range` never ends (leak).

Fix: exactly one closer — the sole producer — closing exactly once, via clear ownership
or `sync.Once`.

### Writing results nobody reads during shutdown

Wrong: workers send results into an unbuffered channel after the reader has stopped, so
they block and `Wait` hangs.

Fix: buffer the results channel or run a drain goroutine, and order close/drain/Wait
correctly.

### Leaking a semaphore or errgroup slot on the error path

Wrong: acquiring a permit, then returning early on an error without releasing it. The
pool starves into a deadlock over time.

Fix: `defer sem.Release(1)` immediately after a successful acquire, so every path returns
the permit.

### Assuming green tests mean deadlock-free

Wrong: shipping because the tests passed once and a hang "usually" does not happen.
Ordering-dependent deadlocks hide until production load.

Fix: run `-race` with high `-count`, and guard hang-prone tests with a watchdog that
dumps goroutine stacks so a hang fails the test with a diagnosis instead of timing out
silently.

Next: [01-ordered-lock-transfer.md](01-ordered-lock-transfer.md)
