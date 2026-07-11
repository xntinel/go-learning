# sync.Mutex: Mutual Exclusion for Shared Backend State — Concepts

A senior backend engineer does not meet `sync.Mutex` as a syntax feature. You
meet it as the default tool for guarding in-process shared state that outlives a
single request: the in-flight request gauge every service exports, a metrics
registry, a TTL cache in front of a database, a per-tenant rate-limiter bucket,
an idempotency store behind an at-least-once consumer, an admission gate, a
rolling latency aggregate. All of these are one small struct plus a mutex, and
all of them are wrong in the same handful of ways. This file is the model and
the trade-offs; the exercises that follow each build one of those artifacts as a
standalone, race-gated module.

The payload here is not "call Lock, then Unlock". It is operational: how long a
critical section may be held before it becomes a tail-latency amplifier, why
copying a mutex silently corrupts synchronization and why `go vet` is the only
thing that catches it before production, when `TryLock` is a legitimate
backpressure tool versus a smell, how two mutexes create lock-ordering
deadlocks, and when the right answer is not a mutex at all.

## Concepts

### A data race, defined precisely, and the only proof you have

The Go memory model defines a data race on a memory location as two operations —
one a read-like op, one a write-like op (or two writes) — on the same location,
unordered by *happens-before*, where at least one is a non-synchronizing
(ordinary, non-atomic) access. That is not a heuristic; it is the definition. A
program with a data race has undefined behavior for the racing accesses even if
it appears to run correctly today.

The only mechanism in the toolchain that proves anything about races is
`go test -race`, which builds with ThreadSanitizer and reports a race with
`WARNING: DATA RACE` and a non-zero exit. Two things about it are easy to get
wrong. First, it detects, it does not verify: a clean `-race` run only means no
race was observed on the interleavings your test actually exercised. It proves
*presence*, never *absence*. Thin coverage or a lucky scheduler can hide a real
race. Second, "it looked fine in staging" is not evidence of anything. Race
freedom is a property you engineer with synchronization and then probe with
`-race`; it is never something you eyeball.

### A mutex is a happens-before edge, not just a gate

The reason a mutex fixes a race is subtle and worth stating exactly. Per the Go
memory model, for a single `sync.Mutex`, the n'th call to `Unlock`
synchronizes-before the m'th call to `Lock` for all `n < m`. That edge is what
makes writes performed inside one critical section visible to a goroutine that
enters a *later* critical section. Plain, non-atomic shared writes create no
such edge and are visible to another goroutine only by luck. So the mutex is not
merely serializing access in wall-clock time; it is publishing memory. This is
why "I only read the field, I did not write it" is still a race if some other
goroutine writes it without the same lock: the read has no happens-before edge to
the write.

The `sync` primitives (`Mutex`, `RWMutex`, `Cond`, `Once`, `WaitGroup`), the
`sync/atomic` operations, and channel operations all establish happens-before
edges. Ordinary assignments do not.

### The zero value is ready; embed it, share it by pointer

`sync.Mutex` needs no constructor. Its zero value is an unlocked, usable mutex.
The idiom is to embed it as the first field of the struct whose fields it
protects, which documents the association and keeps the guarded fields directly
below it:

```
type Gauge struct {
	mu sync.Mutex
	n  int64
}
```

Then use pointer receivers on every method, so the one mutex is shared across
all calls and never copied. A value receiver would hand each call its own copy
of the struct — including its own copy of the mutex — which is the next hazard.

### Never copy a mutex after first use

A `Mutex` contains internal state, and its copy is a distinct, freshly-unlocked
mutex that shares no memory with the original. Operations on a copy synchronize
with nothing. The compiler stays completely silent about this; the runtime does
not complain either. The single pre-runtime guard is the `go vet` `copylocks`
analyzer, which statically flags any code that copies a value containing a lock.
Treat a `copylocks` finding as a hard failure, not a suggestion.

The copy vectors are easy to introduce by accident: a value receiver on a
method of a lock-containing struct; passing such a struct to a function by
value; returning it by value (a `Snapshot() Config` that returns the struct
rather than `*Config`); appending such a struct to a slice; ranging over a slice
of them by value. The defense is uniform: hold and pass these types by pointer,
and when you must return a snapshot, return a copy of the plain fields (or a
separate value type that contains no lock), never the lock itself.

### The critical-section discipline: the tail-latency rule

A mutex serializes every waiter, so every nanosecond spent holding the lock is
added to the latency of every goroutine queued behind it. Under load this turns
a lock into a tail-latency amplifier. The rule that keeps a mutex cheap: do the
minimum under the lock — read or copy the raw fields — release it, then do the
slow work (I/O, JSON encoding, division, formatting, a channel send) outside.
`Snapshot`-style methods embody this: lock, copy `count`/`sum`/`max` into
locals, unlock, and only then compute the mean. The alternative — dividing and
formatting while holding the lock — widens the critical section for no reason
and can even deadlock if the slow work re-enters the same lock.

### Pair Lock with Unlock, and know the failure shapes

Place `defer mu.Unlock()` immediately after `mu.Lock()` when the critical
section spans any branching or can panic; the defer then fires on every return
path and on a panic. For a tiny, straight-line, hot-path section you may unlock
explicitly to avoid the defer's small cost, but the default is defer. Two
failure shapes matter: `Unlock` on a mutex you do not hold is a runtime panic,
and a missing `Unlock` after `Lock` blocks every future `Lock` forever — if
nothing else is runnable the runtime prints
`fatal error: all goroutines are asleep - deadlock!`, otherwise the service just
hangs. A `Mutex` also has no concept of an owner: unlocking from a different
goroutine than the one that locked is not caught as long as the mutex is locked,
but it is a design bug.

### TryLock exists, and the stdlib warns you about it

`Mutex.TryLock() bool` acquires the lock if it is free and returns immediately
with `false` if it is not, never blocking. It is genuinely useful for a narrow
set of patterns: non-blocking admission control, backpressure, and
single-flight-style "a refresh is already running, serve stale instead of
piling on" guards. But the standard library's own documentation notes that use
of `TryLock` is rare and often a sign of a deeper problem in a particular use of
mutexes — frequently the real answer is a queue, a channel, or redesigned
ownership. Reach for it deliberately, and remember it obeys the same Unlock
contract: after a `TryLock` that returns `true`, you still must `Unlock`, or you
deadlock exactly as with `Lock`.

### Check-then-act must be one critical section

The most common correctness bug with a guarded map is splitting a decision from
the mutation across two lock/unlock pairs. A "does this key exist? no — insert
it" written as one `Lock/Unlock` for the check and another for the insert is not
atomic: two goroutines can both observe the key absent and both insert. That
window breaks idempotency guards (a request runs twice) and doubles cache fills.
The decision and the mutation that depends on it must happen under a single,
continuous hold of the lock — or you must explicitly accept and handle the race
window.

### Two mutexes mean lock ordering

The moment code holds two mutexes at once, ordering matters. If one path locks
`from` then `to` while another concurrently locks `to` then `from`, each can
grab its first lock and block forever on the second: the classic ABBA deadlock.
The fix is a consistent global acquisition order — sort the locks by a stable
key (an account ID, a shard index) and always take them in that order. Crucially,
`-race` does *not* catch deadlocks; a deadlock is not a data race. You surface it
with a bounded `go test -timeout`, which turns a hang into a failure.

### Choosing the right tool, and granularity

A mutex is the default for read/write shared mutable state. It is not always the
right default:

- `sync/atomic` for a single counter or flag — one word, no critical section.
- `sync.RWMutex` only when reads vastly dominate writes and the critical section
  is non-trivial; for a tiny section the read/write bookkeeping can cost more
  than a plain `Mutex`.
- `sync.Map` for read-mostly, write-once, or disjoint-key workloads (a stable
  key set hammered with reads).
- Channels when you can transfer *ownership* of the data instead of sharing it —
  "share memory by communicating".

Granularity is the other axis. One coarse mutex over the whole structure is the
simplest and, at low contention, often the fastest choice. Under real
contention, sharding — an array of mutex-guarded buckets keyed by a hash of the
key — cuts contention roughly by the shard count, at the cost of more complexity
and the loss of any operation that must be atomic across shards. Start coarse;
shard only when a contention profile says to.

## Common Mistakes

### Forgetting to Unlock

Wrong: `mu.Lock()` with no matching `Unlock` on some return path. Every later
`Lock` blocks forever; with nothing else runnable the runtime prints
`fatal error: all goroutines are asleep - deadlock!`, otherwise the service
hangs silently. Fix: `defer mu.Unlock()` immediately after `Lock` so it survives
every branch and panic.

### Value receiver on a lock-containing struct

Wrong: `func (c Counter) Inc()` when `Counter` embeds a `sync.Mutex`. Each call
copies the struct and its mutex, so the critical section synchronizes nothing.
`-race` catches it at runtime and `go vet` `copylocks` flags the copy
statically. Fix: pointer receivers everywhere, so the one mutex is shared.

### Returning or passing a lock by value

Wrong: `func (c *Config) Snapshot() Config` returns the struct — including its
mutex — by value; the same happens passing such a struct as a function argument
or appending it to a slice. Fix: return `*Config`, or return a plain value type
(or a copied map) that contains no lock.

### Holding the lock across slow work

Wrong: locking, then doing network/disk I/O, JSON encoding, or a channel send
before unlocking. The mutex becomes a tail-latency amplifier and can deadlock if
the slow work re-enters the lock. Fix: copy the data under the lock, unlock,
then do the slow work.

### Non-atomic check-then-act

Wrong: one `Lock/Unlock` to check whether a key exists and a second to insert
it. Two goroutines both see it absent and both insert, breaking idempotency and
doubling cache fills. Fix: decide and mutate under a single continuous hold.

### Re-locking a non-reentrant mutex

Wrong: a method that holds the lock calls another exported method that also
locks the same mutex. `sync.Mutex` is not reentrant, so this self-deadlocks.
Fix: split into a public method that locks and an unexported helper that assumes
the lock is already held.

### Inconsistent lock ordering across two mutexes

Wrong: `from.Lock(); to.Lock()` in one path and the reverse in another produces
an ABBA deadlock under concurrent opposite transfers. Fix: acquire both locks in
a consistent global order by a stable key.

### Unlocking a mutex you do not hold

Wrong: calling `Unlock` when the mutex is not locked panics at runtime; a
`Mutex` also has no owner, so unlocking from a different goroutine than locked is
a design bug even when it does not panic. Fix: keep each Lock/Unlock pair inside
one function scope, ideally with `defer`.

### Reaching for TryLock to paper over contention

Wrong: sprinkling `TryLock` to avoid blocking usually hides a design flaw that
wants a queue, a channel, or reworked ownership. Fix: use `TryLock` only for
genuine non-blocking admission or backpressure, and always `Unlock` on the
success path.

### Trusting a clean -race run as a proof of correctness

Wrong: concluding "no races" because `go test -race` passed once. It only proves
no race was observed on the interleavings exercised; insufficient coverage or a
lucky schedule can hide a real one. Fix: exercise the concurrent paths hard
(many goroutines, many iterations) and keep `-race` in CI, understanding it
bounds risk rather than eliminating it.

Next: [01-request-inflight-gauge.md](01-request-inflight-gauge.md)
