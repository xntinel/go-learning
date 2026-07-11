# sync.Cond: Condition Variables for Backend Coordination — Concepts

`sync.Cond` is the primitive you reach for when goroutines must block on an
arbitrary predicate over shared mutable state that a channel cannot cleanly
express: "wait until this specific key is loaded", "wait until the in-flight
count hits zero", "wait until the pool has a free slot", "wait until the config
version advances past the one I last saw". A channel is a great hand-off and a
great fan-out of *values*; a `Cond` is a great fan-out of a *state change* to
many waiters who each re-evaluate a predicate over data they all share under one
lock. The senior skill has two halves. First, knowing exactly when `Cond` beats
channels (multi-waiter broadcast on shared mutable state, N-waiter fan-out,
predicates over an aggregate like a count) and when it loses (single hand-off,
cancellation, timeout, `select` composition — use channels). Second, knowing how
to bolt on the two things `Cond` fundamentally lacks: cancellation/timeout (there
is no `context` on `Wait`) and deadlock safety. This file is the conceptual
foundation for nine independent production artifacts — a bounded buffer, a
readiness latch, a connection pool, a single-flight cache, a drain barrier, a
pausable worker pool, a cancellable wait, a weighted semaphore, and a config
fan-out — each tested under `-race` and made deterministic with Go 1.25
`testing/synctest` so blocking and wakeup are asserted without a single sleep.

## The model: a predicate, its mutex, and a waiting list

A `sync.Cond` bundles three things: a `sync.Locker` you supply, an internal list
of parked goroutines, and the `Signal`/`Broadcast`/`Wait` operations over that
list. The constructor takes the lock:

```go
mu := new(sync.Mutex)
cond := sync.NewCond(mu)
```

The lock is *passed in*, not created inside, for one reason that governs
everything else: the SAME mutex must protect both the boolean condition and the
data that condition reads. If `notEmpty` means "`len(buf) > 0`", then the same
mutex that guards `buf` must be the one `cond.L` refers to. The field is
`cond.L` (the embedded `Locker`), and it must be held whenever you observe the
condition, mutate the condition, or call `Wait`. A `Cond` whose predicate reads
state guarded by a *different* lock is a data race waiting to happen — the whole
design rests on one lock covering condition and data together.

### Wait is an atomic unlock-suspend-relock

`cond.Wait()` does three things as one indivisible step: it releases `cond.L`,
suspends the calling goroutine on the waiting list, and re-acquires `cond.L`
before it returns. That atomicity is the entire point of the primitive. Consider
the naive alternative — check the predicate, then go to sleep on a channel:
between your check and your sleep, another goroutine could change the state and
fire the wakeup, and you would sleep through it forever (the "lost wakeup"). By
folding "release the lock" and "join the waiting list" into one atomic action
that no other goroutine can interleave with, `Wait` guarantees that any
`Signal`/`Broadcast` issued *after* you decided to wait will reach you. Per the
Go memory model, a call to `Cond.Broadcast` or `Cond.Signal` synchronizes-before
any `Wait` it unblocks: state written under the lock before the signal is visible
to the woken waiter after `Wait` returns, as long as both touch it under
`cond.L`.

### The predicate MUST be a for loop, never an if

This is the single most important rule and the most common bug:

```go
cond.L.Lock()
for !ready {          // for, not if
	cond.Wait()
}
// ready is now guaranteed true, under the lock
cond.L.Unlock()
```

`Wait` returning does NOT mean your predicate holds. It only means someone called
`Signal`/`Broadcast`. By the time your goroutine re-acquires the lock and runs,
the state may be false again: another waiter was scheduled first and consumed the
one unit of freed state, or the state was mutated back, or (on some runtimes) a
spurious wakeup occurred. The `for` loop re-checks the predicate on every wakeup
and re-parks if it is still false. An `if` proceeds on a stale, possibly-false
condition and corrupts state. Verified against the package documentation: "Wait
cannot return unless awoken by Broadcast or Signal ... Because c.L is not locked
while Wait is waiting, the caller typically cannot assume that the condition is
true when Wait returns. Instead, the caller should Wait in a loop." The loop is
not defensive style; it is required for correctness.

## Signal versus Broadcast: the decision that defines the design

`Signal` wakes at most one waiter; `Broadcast` wakes all of them. Choosing wrong
is a silent deadlock or a CPU-burning livelock, so treat it as a design decision,
not a detail.

Use `Signal` when exactly one waiter can make progress on one unit of freed
state, and every waiter is interchangeable: one slot opened in a bounded buffer,
one connection returned to a pool. Waking more than one would just make the
extras re-check, find nothing, and re-sleep — wasteful but not wrong.

Use `Broadcast` when any of these holds:

- The state change may satisfy MANY waiters at once (a config version advanced;
  everyone waiting for a newer version can proceed).
- Waiters have HETEROGENEOUS predicates (a weighted semaphore where one waiter
  needs 2 permits and another needs 8). Here `Signal` is a genuine bug: it may
  wake the 8-permit waiter when only 3 permits freed — it re-sleeps — while the
  2-permit waiter that could proceed is never woken and starves. That is a
  lost-wakeup deadlock produced entirely by choosing `Signal`.
- A TERMINAL state must release everyone: `Close`, `Drain` reaching zero,
  `Resume` from a paused state. Every blocked waiter must wake to observe the new
  terminal condition (and typically return an error or exit).

The rule of thumb: `Signal` only when one freed unit helps exactly one
indistinguishable waiter; `Broadcast` whenever the number of satisfiable waiters
is more than one, unknown, or heterogeneous. When in doubt, `Broadcast` is the
safe default — it costs some extra re-checks but never loses a wakeup.

### Two predicates want two Conds over one mutex

A bounded buffer has two distinct predicates: producers wait for "not full",
consumers wait for "not empty". The correct structure is TWO `Cond` instances
sharing the ONE mutex that guards the buffer — `notFull` and `notEmpty`, both
built with `sync.NewCond(&b.mu)`. A `Put` that frees the "not empty" condition
signals `notEmpty`; a `Get` that frees the "not full" condition signals
`notFull`. Using a single `Cond` for both predicates works only by luck: a
`Signal` meant to wake a producer may instead wake a consumer, who re-checks the
wrong predicate, finds it false, and re-sleeps. It limps along on `Broadcast`
and polling, burns CPU, and is fragile. One `Cond` per predicate over the shared
mutex is the clean design.

## What Cond does not have (and how senior code compensates)

### No context, no timeout

`Wait` takes no `context.Context` and cannot be deadlined or cancelled. Wrapping
`Wait` in a `select` does nothing — you cannot `select` over a `Cond`. Passing a
`ctx` to a function that calls `Wait` does nothing on its own. To make a wait
cancellable you must run a watcher goroutine that `Broadcast`s the `Cond` when
`ctx.Done()` fires, and re-check `ctx.Err()` inside the predicate loop:

```go
// caller holds cond.L
done := make(chan struct{})
defer close(done)
go func() {
	select {
	case <-ctx.Done():
		cond.L.Lock()
		cond.Broadcast()  // kick the waiter so it re-checks ctx.Err()
		cond.L.Unlock()
	case <-done:          // waiter returned first; watcher exits cleanly
	}
}()
for !pred() {
	if err := ctx.Err(); err != nil {
		return err
	}
	cond.Wait()
}
return nil
```

Two things make this correct. The watcher takes `cond.L` before broadcasting, so
it cannot fire in the atomic gap between the waiter's predicate check and its
`Wait` (the waiter holds the lock across both, and `Wait` releases atomically) —
no lost wakeup. And the `done` channel guarantees the watcher ALWAYS exits: if
the predicate becomes true first, the waiter returns, `defer close(done)` fires,
and the watcher's `select` takes the `done` branch and returns. A cancellation
patch that leaks its watcher goroutine on the happy path is a slow resource leak;
the `done`/`defer` pairing is non-negotiable.

### No select composition, no fairness

You cannot compose a `Cond` into a `select` alongside a channel receive or a
timer. If your coordination needs `select`, a timeout, or a single hand-off, a
channel is the better primitive — the package docs themselves say most simple
cases should use channels. And `Cond` guarantees NO fairness: there is no FIFO
ordering of waiters, and the runtime may wake them in any order. Never encode
ordering in "who I think wakes first"; encode all ordering in the predicate and
the shared state. If order matters, put a sequence number or a queue in the state
and check it in the predicate.

### Must not be copied

A `Cond` holds a waiting list and a `noCopy` sentinel that `go vet`'s `copylocks`
checker flags. Copying a struct that embeds a `sync.Cond` or `sync.Mutex` by
value — returning it, ranging over a slice of them — gives the copy a detached
waiting list, so signals reach the wrong instance. Always store and pass
`*sync.Cond`, construct it once in the constructor against the owning struct's
own mutex address (`sync.NewCond(&s.mu)`), and never copy the struct after first
use.

## Deterministic testing with Go 1.25 testing/synctest

Concurrent blocking code is the worst thing to test the naive way: you start a
goroutine, `time.Sleep(20 * time.Millisecond)` and *hope* it reached `Wait`, then
assert. That is slow and flaky. Go 1.25's `testing/synctest` fixes this. Inside a
`synctest.Test(t, func(t *testing.T){ ... })` bubble, `Cond.Wait` is a *durably
blocked* state, and `synctest.Wait()` returns only once every OTHER goroutine in
the bubble is durably blocked. So the pattern becomes: start the goroutines, call
`synctest.Wait()` to deterministically confirm they are all parked on `Cond.Wait`,
assert nothing has progressed, perform the state change (`Put`, `Release`,
`Publish`, `Resume`), call `synctest.Wait()` again, and assert the wakeup — with
zero real sleeps and zero flakiness. `synctest.Test` also fails the test if any
goroutine leaks or the bubble deadlocks, which is exactly the failure mode a
mis-chosen `Signal` or a leaked watcher produces. One caveat to keep honest:
acquiring a `sync.Mutex` is NOT a durably-blocked state (a goroutine spinning to
lock a mutex held outside the bubble would otherwise look falsely blocked), so
structure assertions around goroutines parked in `Cond.Wait` — which have already
released the lock — not around lock contention.

## Common Mistakes

### Using if instead of for around Wait

Wrong: `if !ready { cond.Wait() }`. The predicate can be false again on wakeup —
another waiter consumed the freed state, or it was mutated back — so the
goroutine proceeds on a false condition and corrupts state.

Fix: always loop, `for !ready { cond.Wait() }`. Re-check the predicate on every
wakeup and re-park if it still does not hold.

### Calling Wait or signalling without holding the lock

Wrong: `cond.Wait()` with the lock not held panics with `sync: unlocked
cond.Wait`. Mutating the predicate without the lock races with waiters in the gap
between their check and their park, losing wakeups.

Fix: hold `cond.L` across the whole check-mutate-signal sequence. `Wait` requires
the lock on entry; the state change that satisfies a predicate must happen under
the same lock.

### Forgetting to signal after a state change

Wrong: `Put` appends an item but never signals `notEmpty`. A consumer parked in
`Get` sleeps forever even though data is present.

Fix: every mutation that can satisfy some waiter's predicate must `Signal` or
`Broadcast` the corresponding condition.

### Using Signal where Broadcast is required

Wrong: `Signal` with heterogeneous waiter predicates (a weighted semaphore) or a
terminal state (`Close`/`Resume`/`Drain`). `Signal` can wake one waiter that
still cannot proceed while a satisfiable waiter starves — a lost-wakeup deadlock.

Fix: `Broadcast` when many waiters may now proceed, when predicates differ, or
when a terminal state must release everyone. Reserve `Signal` for one freed unit
that helps exactly one interchangeable waiter.

### One Cond for two distinct predicates

Wrong: a single `Cond` for both "not full" and "not empty". A `Signal` meant for
a producer wakes a consumer who re-checks the wrong predicate and re-sleeps; it
limps along by luck and burns CPU.

Fix: one `Cond` per predicate over the shared mutex.

### Signalling before mutating the state, or outside the lock

Wrong: signal, then update the state; or signal without the lock. The woken
goroutine may re-acquire the lock and re-check before the state is updated (or
observe a stale value) and silently re-sleep.

Fix: mutate the state under the lock, then signal. Signalling while still holding
the lock is safe and is the simplest correct ordering.

### Expecting Wait to honor a context or time out

Wrong: wrapping `Wait` in a `select` or passing a `ctx` and expecting
cancellation. `Wait` cannot be cancelled natively.

Fix: `Broadcast` from a `ctx`-watcher goroutine and re-check `ctx.Err()` in the
loop — and guarantee that watcher always exits (via a `done` channel closed with
`defer`) so it never leaks.

### Copying a struct that embeds a Cond or Mutex

Wrong: returning a struct with an embedded `sync.Cond`/`sync.Mutex` by value, or
ranging over a slice of them. `go vet` copylocks flags it; the copy has a
detached waiting list, so signals reach the wrong instance.

Fix: construct once and pass by pointer (`*sync.Cond`, `*T`).

### Relying on wakeup order or how many Broadcast releases

Wrong: assuming FIFO wakeup, or that `Broadcast` releases waiters in a particular
order or rate. There is no fairness guarantee.

Fix: put all ordering in the predicate and shared state; correctness must not
depend on which goroutine the runtime wakes first.

### Testing blocking with time.Sleep

Wrong: `time.Sleep(20 * time.Millisecond)` and hoping the goroutine reached
`Wait` before you assert. Slow and flaky.

Fix: under Go 1.25 use `testing/synctest`: `synctest.Wait()` deterministically
confirms every other goroutine is durably blocked on `Cond.Wait` before you
assert, with no real sleep.

Next: [01-bounded-buffer.md](01-bounded-buffer.md)
