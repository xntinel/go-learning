# sync.RWMutex: Read/Write Locking for Read-Heavy Server State — Concepts

A `sync.Mutex` serializes every access, including the concurrent reads that never
mutate anything. For the shape that dominates backend services — read on every
request, write on a rare reload — that serialization is pure, wasted contention:
readers block one another for no reason. `sync.RWMutex` exists for exactly this
shape. It has two acquisition modes: `RLock`, a shared read lock any number of
goroutines may hold at once, and `Lock`, an exclusive write lock that excludes
every other reader and writer. Config and feature-flag registries, routing and
backend-pool tables, metric registries, per-key limiter maps, read-through caches
— all are "read constantly, write occasionally" state, and all are the RWMutex's
home ground.

The senior skill is not "how to call `RLock`". It is knowing *when* an RWMutex
actually beats a plain `Mutex`, when it quietly loses, and when to abandon locking
on the read path entirely for a copy-on-write `atomic.Pointer` snapshot. It is
also knowing the handful of failure modes that turn this innocuous type into a
production deadlock or a data race: the self-deadlock from an `RLock`→`Lock`
upgrade, writing under a read lock, forgetting the double-check on a cache miss,
holding the write lock across a slow network load, and copying a struct that
embeds the mutex. This file is the model; the exercises drill each failure mode
into a production-shaped artifact with a `-race` gated test.

## The two modes and what they cost

`RLock` is shared: two goroutines that only ever call `RLock`/`RUnlock` never
block each other. `Lock` is exclusive: while one goroutine holds the write lock,
every other goroutine calling `Lock` *or* `RLock` blocks until `Unlock`. That
exclusion is the whole point — it is what makes a write safe while readers are
active — but it is also the cost model. The runtime tracks a live reader count,
and every `RLock`/`RUnlock` does atomic work on that counter. For a critical
section that is a single map lookup, that per-call atomic overhead can dominate,
and a plain `Mutex` is faster. RWMutex is not a free upgrade; it is a trade that
pays off only under the right ratio.

Choosing it is an empirical decision, not a reflex. The rule of thumb is: reach
for `RWMutex` only when reads dominate writes by roughly 10:1 or more *and* the
read-side critical section is non-trivial (more than a single field read). On
short sections or write-heavy loads, benchmark first — the plain `Mutex` usually
wins. Exercise 9 builds the benchmark harness that produces the evidence a senior
uses to justify the choice, instead of guessing.

## The no-upgrade, no-recursion rule (load-bearing)

The documentation is explicit and this is where real deadlocks come from: an
`RLock` cannot be upgraded to a `Lock`, a `Lock` cannot be downgraded to an
`RLock`, and recursive read-locking is not guaranteed safe. The upgrade case is
the deadly one. If a goroutine holds an `RLock` and then calls `Lock`, it
deadlocks against itself: a blocked `Lock` request blocks *new* readers (to keep
writers from starving), and the caller is one of those new readers waiting on a
lock it will never get because it is itself still holding the read lock. The fix
is always the same shape: `RUnlock` first, then `Lock`, then re-read the protected
state under the write lock — because between releasing the read lock and taking
the write lock, another goroutine may have changed that state, so you must look
again.

## Writer non-starvation is a fairness guarantee, not a speed promise

Once a writer is blocked in `Lock`, newly arriving `RLock` calls queue behind it
rather than jumping ahead. This is what stops an endless stream of overlapping
readers from starving a waiting writer forever: the writer is guaranteed to
eventually acquire the lock. It is a fairness guarantee only. It does not make the
writer *fast* — a long tail of readers that already hold the lock still delays the
writer until they drain; it merely cannot block it indefinitely. Do not read
"non-starvation" as "writes are cheap".

## What the memory model actually promises

The reason a value written under `Lock` is safely visible to a later `RLock`
holder — with no extra atomics, no channel, no ceremony — is a documented
happens-before edge, not luck. The `sync` documentation states it precisely: for
any n and m with n < m, the n'th call to `Unlock` is synchronized before the m'th
call to `Lock`, exactly as with a plain `Mutex`; and additionally, for any call
to `RLock` there exists an n such that the n'th `Unlock` is synchronized before
the return of that `RLock`, and the matching `RUnlock` is synchronized before the
n+1'th call to `Lock`. Unpacked: every write completed before a writer's
`Unlock` is visible to every reader whose `RLock` returns after it, and every
read completed before `RUnlock` is ordered before the next writer's critical
section. This is why the pattern "mutate the map under `Lock`, read it under
`RLock`" needs nothing else — the lock pair *is* the memory barrier. It is also
why skipping the lock "just for a quick read" is not a minor sin: an unlocked
read has no happens-before edge to any write, so the race detector flags it and
the hardware is free to serve a stale or torn value.

## Double-check on a cache miss

A read-through cache reads under `RLock`, and on a miss must take `Lock` to load
and store. The trap: if several goroutines all miss the same hot key at once, each
takes the write lock in turn and each calls the (expensive) loader — a thundering
herd of redundant loads. The double-check pattern collapses that: check under
`RLock`; on a miss, release the read lock, take `Lock`, and *re-check the map
before loading*, because a goroutine that got the write lock first may have
already stored the value. Without the re-check the pattern is just a slow serial
load-storm. Exercise 2 pins this contract with a concurrent-single-load test.

## Separate the structure lock from the value

For registries — a metrics counter table, a per-key limiter map — the sharpest
pattern is to let the RWMutex protect only the *map structure* and an `atomic`
(or a per-entry mutex) protect each *value*. `Inc(name)` takes `RLock`, finds the
existing `*atomic.Int64`, releases the read lock, and does `counter.Add(1)`
lock-free. Only a first-seen name falls through to `Lock` + double-check to
register a new counter. Steady state — every increment of an already-registered
metric — never touches the write lock and never blocks another increment.
Exercises 4 and 7 build this map-lock-plus-value-atomic combination for metrics
and rate limiters.

## Never hold the write lock across slow work

The most damaging real bug: taking `Lock` and then doing a network round-trip, a
disk read, or acquiring a lock on another subsystem while holding it. Every reader
now blocks behind one slow write, and read throughput collapses to serial. The
discipline is: compute or load *outside* the lock, then take `Lock` only to
publish the finished result. In a cache, run the loader unlocked and store under a
brief `Lock`. In Exercise 8, a non-blocking refresh uses `TryLock` for
single-flight and does the slow load without holding the value lock at all, so
request goroutines never queue behind an in-flight refresh.

## Lazy expiry under RLock: report the miss, never delete

TTL caches expose a subtle collision between the read path and the no-upgrade
rule. `Get` checks the entry's deadline under `RLock`, and when the entry has
expired the tempting move is to delete it right there — after all, you are
already holding a lock and looking at the dead entry. Both available moves are
wrong. Deleting under `RLock` is a map mutation another concurrent `RLock`
holder can observe: a data race the `-race` detector will flag. Upgrading to
`Lock` while still holding `RLock` is the self-deadlock from the no-upgrade
rule. The correct shape is *lazy expiry*: the read path only ever answers the
question — an expired entry reports a miss exactly as if it were absent — and
leaves the entry physically in the map. Physical eviction belongs to a separate
sweeper that periodically takes one `Lock` and batch-deletes everything past its
deadline, amortizing the write-lock cost over many evictions instead of paying
it inside request-path reads. The consequence to internalize: between expiry and
the next sweep, `Get` says miss while an internal length count still includes
the corpse — that divergence is correct behavior, not a bug. Exercise 6 builds
this cache, including the graceful shutdown of the sweeper goroutine, which is
its own classic leak.

## Copy-on-write with atomic.Pointer: graduating off the read lock

For read-mostly *immutable* snapshots there is a level above RWMutex:
copy-on-write with `atomic.Pointer[T]`. Readers do a lock-free `Load` of the
current snapshot pointer and never touch a lock, so they can never block behind a
writer at all. A writer builds a brand-new immutable value and publishes it with
`Store` (or a `CompareAndSwap` loop when the new value derives from the old). The
cost is an allocation per write and the discipline of never mutating a published
snapshot. When the state is read-mostly and can be treated as an immutable value
you swap wholesale, COW makes the read path zero-lock. Exercise 5 builds the same
config both ways and benchmarks the crossover; Go 1.19's typed atomics
(`atomic.Int64`, `atomic.Pointer[T]`) are the ergonomic, misuse-resistant API for
this — use the methods, keep the atomic addressable, never copy it.

## TryLock and TryRLock: narrow, legitimate, easily abused

Since Go 1.18, `TryLock`/`TryRLock` return a `bool` and never block. Their honest
uses are narrow: skip-if-busy semantics (a scheduled refresh that returns the
current value rather than queueing when a refresh is already running), a
deadlock-avoidance probe, a liveness check. They are a code smell when used as a
general acquisition mechanism, and a spin loop of `TryLock` is worse than plain
blocking — it burns CPU and fights the scheduler. Use them for "if I cannot get it
immediately, do something else", never for "acquire a lock I actually need".

## The copy hazard and go vet

A `sync.RWMutex` must never be copied after first use — a copy carries a stale
snapshot of the internal reader count and state, so the copy and the original no
longer coordinate. Embed the mutex by value in a struct and always pass that
struct by pointer; returning it by value, ranging over a slice of values, or
storing it in a map by value all silently copy the lock. `go vet`'s `copylocks`
pass catches this, which is one more reason `go vet` is part of the gate.

## RLocker for Locker-shaped APIs

`(*RWMutex).RLocker()` returns a `sync.Locker` whose `Lock`/`Unlock` are actually
`RLock`/`RUnlock`. It exists to hand the read side of an RWMutex to an API that
wants a plain `Locker` — most notably `sync.Cond`, which is constructed with a
`Locker`. It is a rarely-needed adapter, but knowing it exists explains how a
read lock can participate where only a `Locker` is accepted.

## Common Mistakes

### Calling Lock while holding RLock (attempted upgrade)

Wrong: inside an `RLock` section you decide the state needs updating and call
`Lock` without releasing the read lock. Self-deadlock: the pending `Lock` blocks
new readers including the calling goroutine, which is still holding `RLock`. Fix:
`RUnlock` first, take `Lock`, then re-read the state under the write lock — it may
have changed in the gap.

### Writing to protected state under RLock

Wrong: mutating a map or field while holding only `RLock`. Another goroutine can
hold `RLock` and read the same field concurrently — a data race. The `-race`
detector catches it; without it you get torn or stale reads. Any mutation needs
the exclusive `Lock`.

### Omitting the double-check after acquiring Lock on a miss

Wrong: on a cache miss, take `Lock` and immediately call the loader without
re-checking the map. Every goroutine that missed the same key loads it
redundantly — a thundering herd. Fix: re-read the map under `Lock`; if another
goroutine already stored the value, return it instead of loading again.

### Holding Lock across a slow load

Wrong: `Lock`, then a DB query or HTTP call, then store, then `Unlock`. All
readers block behind one network round-trip and throughput goes serial. Fix: load
outside the lock, take `Lock` only to publish the result.

### Reaching for RWMutex by reflex on short sections

Wrong: swapping every `Mutex` for an `RWMutex` because it "sounds faster for
reads". The reader-count atomics make it slower than a plain `Mutex` when the
section is a single lookup. Fix: benchmark; RWMutex wins only at high read ratios
with non-trivial read sections.

### Copying a struct that embeds a RWMutex

Wrong: returning the guarded struct by value, ranging over a `[]Struct` of
values, or storing it in a map by value. The copy shares no locking state with the
original. Fix: always use pointers to the guarded struct; let `go vet` copylocks
catch the accident.

### Forgetting to release on an early return or panic

Wrong: an early `return` or a panic between `Lock` and `Unlock` leaves the lock
held forever, and every subsequent reader and writer hangs. Fix: `defer RUnlock`
/ `defer Unlock` immediately after acquiring, unless a fast-path return is
deliberately kept lock-free.

### Mixing the value atomic with the map lock incorrectly

Wrong: reading a counter pointer under `RLock` is fine, but then deleting or
inserting a map entry without upgrading to `Lock`, or storing a plain value where
a lock-free reader expects an atomic. The map's structure needs the write lock;
only the per-entry atomic is safe to touch outside it.

### Assuming writer non-starvation means writers are fast

Wrong: treating the fairness guarantee as a latency guarantee. It only promises
the writer eventually acquires the lock; a long tail of overlapping readers still
delays it. Design the write path to be brief regardless.

### Using TryLock in a spin loop

Wrong: `for !mu.TryLock() { }` as a general acquire. Busy-waiting burns a core and
defeats the scheduler. `TryLock` is for skip-if-busy semantics, not for a lock you
actually need — for that, just block on `Lock`.

### Leaking the sweeper goroutine

Wrong: starting a `time.Ticker` eviction loop in a constructor with no `Stop`
method and no done channel. Every cache instance leaks a goroutine and a ticker
forever — tests accumulate them, and graceful shutdown never completes. Fix:
give the loop a `select` on a done channel, close it from an idempotent `Stop`
(a `sync.Once` around the `close`), and `defer ticker.Stop()` inside the loop.

### Recursive read-locking across call boundaries

Wrong: a helper that takes `RLock` while its caller already holds `RLock`. The
docs explicitly prohibit recursive read-locking: if a writer's `Lock` queues
between the two acquisitions, the inner `RLock` blocks behind the writer, the
writer blocks behind the outer `RLock`, and the goroutine deadlocks against
itself. Fix: establish a locking discipline — exported methods lock, unexported
helpers assume the lock is held and never lock themselves.

Next: [01-config-store.md](01-config-store.md)
