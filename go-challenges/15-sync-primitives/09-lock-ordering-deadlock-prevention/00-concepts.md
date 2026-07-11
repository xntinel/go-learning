# Lock Ordering and Deadlock Prevention — Concepts

A deadlock is the production incident class that leaves no crash, no error log,
and no stack trace in your alerting: two goroutines each hold a lock the other
needs, both block forever, and the process keeps serving everything that does
not touch those locks — until connection pools drain, worker queues fill, and
p99 latency climbs while every health check still returns 200. This lesson is
about owning that failure mode end to end: preventing it by design (consistent
lock ordering, hierarchies that span every blocking resource, redesigns that
stop needing two locks at all), enforcing the design in CI (rank-asserting
debug mutexes, watchdog harnesses that fail with goroutine dumps instead of
hanging the pipeline), and recognizing it in production dumps. The exercises
build the real artifacts this requires: a transfer engine, a sharded session
store with cross-shard moves, a context-aware lock so HTTP handlers degrade to
503 instead of hanging, a lockdep-lite rank checker, and an escrow redesign
that survives when the resources move to different services.

## Concepts

### Deadlock is a cycle in the wait-for graph

Model every blocked goroutine as an edge: goroutine G holds resource X and
waits for resource Y, so draw an edge from X to Y. A deadlock exists exactly
when that graph contains a cycle. The classic two-account case is the smallest
cycle:

```
Goroutine 1: holds A.mu, waits for B.mu     edge A -> B
Goroutine 2: holds B.mu, waits for A.mu     edge B -> A
```

The classical Coffman conditions name the four ingredients every deadlock
needs: mutual exclusion (the resource cannot be shared), hold-and-wait (a
goroutine keeps what it has while waiting for more), no preemption (nothing
takes a lock away from its holder), and circular wait (the cycle itself). The
conditions are not trivia — they are a map of the fixes, because every
prevention technique in this lesson breaks exactly one of them. Consistent
ordering breaks circular wait. `TryLock` with release-and-retry breaks
hold-and-wait. A context-aware lock adds a form of preemption (the waiter gives
up on deadline). The escrow redesign removes multi-hold entirely, so
hold-and-wait never applies. When you evaluate a design, ask which condition it
breaks; if the answer is "none", the design is deadlock-prone no matter how
carefully it is written.

### Why consistent ordering works: the DAG argument

Assign every lock a position in a total order — an account ID, a shard index, a
documented rank — and require that any goroutine holding lock X may only
acquire locks that come after X in that order. Now every edge in the wait-for
graph points "upward" in the order, so the graph is a directed acyclic graph,
and a DAG cannot contain a cycle. That one-sentence proof is the entire
foundation of the technique, and it dictates two non-negotiable properties of
the order itself.

First, the order must be a stable property of the *resource*, never of the
caller. Ordering by argument position (`from` before `to`) is the classic bug:
two goroutines transferring in opposite directions between the same accounts
acquire in opposite orders, and the cycle is back. Order by account ID, by key,
by shard index — something both callers compute identically.

Second, the order must be honored by *every* code path that takes more than
one of those locks. A `Transfer` that orders perfectly is worthless if the
monthly reconciliation job, the admin endpoint, or the metrics snapshot walks
the same accounts locking in slice order. One unordered multi-lock path
anywhere reintroduces the cycle, which is why the discipline has to be written
down, and why the rank-checking mutex in exercise 5 exists: documentation that
panics is the only documentation that stays true.

### The runtime detector will not save you

Go's runtime prints `fatal error: all goroutines are asleep - deadlock!` only
when literally every goroutine in the process is blocked. That fires reliably
in a toy `main` with two goroutines. In any real server it never fires: the
HTTP listener is accepting, tickers are ticking, background sweepers are
sweeping — so a wedged pair of request goroutines just sits there forever.
The observable symptoms are indirect: goroutine count climbs monotonically
(every new request that touches the wedged locks parks behind them), the
affected endpoints time out, and memory grows with the parked stacks.

Detection is therefore your job, in two places. In production, the goroutine
dump: `runtime.Stack(buf, true)` or the pprof endpoint
`/debug/pprof/goroutine?debug=2` shows every goroutine's state and stack, and a
deadlocked pair is unmistakable — two goroutines both `sync.Mutex.Lock` state,
each inside a function holding the lock the other wants. In CI, a watchdog
harness (exercise 9): run the suspect function with a timeout, and on expiry
fail the test *with the full dump attached*, because a test job that just hangs
until the ten-minute kill tells you nothing about which locks were involved.

### Self-deadlock: the same-resource case

`sync.Mutex` is not reentrant. If `from == to`, the ordered-acquisition code
locks one mutex, then tries to lock the same mutex again, and blocks forever on
itself — a one-goroutine, one-lock "cycle" that no ordering can fix. Every
multi-resource operation needs an identity guard before the ordered path: check
`from == to` in a transfer, check `shardIndex(old) == shardIndex(new)` in a
cross-shard move and take the single-lock fast path. The sharded-store exercise
shows how easy this is to miss: two *different* keys are not two different
shards, so guarding on key equality is not enough — you must guard on the
identity of the *lock*, not the identity of the argument.

### RWMutex has no upgrade path

A recurring self-deadlock shape in cache code: take `RLock` to check for a
value, miss, and try to take `Lock` to fill — while still holding the read
lock. `sync.RWMutex` has no upgrade operation; the write lock waits for all
readers including you, forever. The documented rule is stricter than most
people expect: the docs also prohibit *recursive read locking*, because a
blocked writer stalls all later `RLock` calls (to prevent writer starvation),
so goroutine G's second `RLock` can wait on a writer that waits on G's first —
a cycle through the fairness mechanism itself. The correct fill pattern is
release-then-recheck (double-checked locking): `RLock`, check, `RUnlock`,
`Lock`, *check again* because another goroutine may have won the race, fill,
`Unlock`. Exercise 7 builds it into a config cache and discusses when
`sync.Once`-per-key or singleflight is the better tool.

### TryLock breaks hold-and-wait — at the price of livelock

When no total order over the resources exists — two stock locations owned by
different teams, identified by opaque UUIDs neither team will renumber —
`sync.Mutex.TryLock` (Go 1.18+) offers the other classical fix: acquire the
first lock, *try* the second, and on failure release everything, back off, and
retry. No goroutine ever holds one lock while blocking on another, so
hold-and-wait is broken and deadlock is impossible. What you buy instead is
livelock risk: two contenders that retry in lockstep can fail against each
other forever, which is why the backoff must be jittered (randomized) and the
retries bounded with a typed error the caller can act on. The `TryLock` docs
themselves warn that correct uses "are rare" and that reaching for it is often
a sign of a deeper design problem — treat it as the tool of last resort for
resources you genuinely cannot order, not as a convenience.

### Cycles span resource types, not just mutexes

The wait-for graph does not care what kind of resource an edge goes through. A
bounded connection pool (a buffered-channel semaphore), a worker queue, a
`sync.WaitGroup`, an unbuffered channel send — all of them are "hold and wait"
edges. The nastiest production deadlocks mix types: path A takes a pool slot
then a record lock; path B takes the record lock then waits for a pool slot;
with all K slots held by A-style workers that are blocked on records B-style
workers hold, the system wedges with no two mutexes ever in conflict. The
acquisition hierarchy must therefore be documented across *every* blocking
resource — "pool slot before any record lock" — and code that cannot comply
must be restructured to release before it blocks. Exercise 8 builds exactly
this failure and its fix.

### Enforcement: make the ordering panic, not hang

A lock-ordering discipline that lives in a comment decays. The Linux kernel's
lockdep and CockroachDB's mutex wrappers solve this by attaching a *rank* to
every lock and asserting, at acquisition time, that the goroutine holds no
lock of equal or higher rank. A violation becomes an immediate panic with both
lock names in the message — caught by the first CI run whose code path takes
the locks in the wrong order, even if the timing never actually deadlocks. Go
has no goroutine-local storage to track held locks implicitly, so the idiomatic
port passes an explicit trace through the call chain, which has a side benefit:
the lock-passing parameter documents which functions acquire locks at all.
Exercise 5 builds this wrapper with a registry-before-tenant-before-session
hierarchy.

### Preemption for request paths: context-aware locks

`sync.Mutex.Lock` cannot be interrupted: no deadline, no cancellation. If an
HTTP handler blocks on a mutex that a wedged goroutine holds, the handler hangs
past its request deadline, the client retries, and the wedge accumulates
goroutines. A lock built on a 1-buffered channel (`chan struct{}` with capacity
1, send-to-acquire, receive-to-release) can be acquired inside a `select` with
`ctx.Done()`, converting an indefinite wait into a bounded one that returns
`ctx.Err()`. The handler degrades to a 503 with Retry-After — an observable,
alertable error instead of a silent hang. The trade-offs are real: a channel
lock is slower than `sync.Mutex`, has no fairness guarantees, and no vet/race
integration, so use it where cancellability matters (request paths crossing
long-held locks), not as a general replacement.

### The senior fix: stop needing two locks

Ordering, ranks, and TryLock all manage the danger of holding two locks. The
strongest move is to eliminate it. A two-phase escrow transfer debits the
source under only its own lock, records the in-flight amount in an escrow
ledger, then credits the destination under only its own lock, with an
idempotent compensation crediting the escrow back on phase-two failure. No
goroutine ever holds two account locks, so the wait-for graph over accounts has
no multi-hold edges at all. The price is a bounded invariant window: during a
transfer, the sum of account balances alone is short by the in-flight amount —
only accounts *plus escrow* is conserved. That is exactly the shape of the saga
pattern in distributed systems, and it is the only shape that survives when the
two accounts stop sharing a process: you cannot mutex-order two rows in two
different services' databases. Exercise 10 builds it in-process, where its
invariants can be tested exactly.

### Lock scope discipline

Independent of ordering: never hold a lock across I/O, a channel operation, a
pool acquire, or a callback into code you do not control. Any of those can
block on a resource whose holder wants your lock, extending the wait-for graph
in ways no mutex-only analysis will find — and even when it does not deadlock,
it multiplies contention and tail latency. The pattern is always the same: copy
what you need under the lock, release, then do the slow work. When the slow
work must be exclusive per key (a cache fill), that is a signal to reach for
singleflight or a per-key once, not to stretch the critical section.

## Common Mistakes

### Locking in caller-argument order

Wrong: `from.mu.Lock(); to.mu.Lock()`. Two goroutines transferring in opposite
directions between the same accounts acquire in opposite orders and form the
cycle. Fix: order by a stable property of the resource — ID, key, shard index —
that both callers compute identically.

### Forgetting the same-resource guard

Wrong: entering the ordered path when `from == to` (or when two distinct keys
hash to the same shard). The code locks one mutex twice; `sync.Mutex` is not
reentrant, so it blocks on itself forever. Fix: guard on the identity of the
lock about to be taken — `from == to`, `shardIndex(a) == shardIndex(b)` — and
take a single-lock fast path or return a typed error.

### Ordering Transfer but not the auxiliary paths

Wrong: the transfer engine orders by ID, but the totals endpoint, the
reconciliation job, or an admin handler locks accounts in slice order. One
unordered multi-lock path anywhere reintroduces the cycle. Fix: the ordering is
a property of the *resource set*, not of one function; every multi-lock path
sorts first, and a rank-asserting mutex turns violations into panics.

### Attempting an RWMutex upgrade

Wrong: `RLock`, see a miss, call `Lock` while still holding the read lock —
self-deadlock, because the writer waits for all readers including you. Also
wrong: recursive `RLock` on one goroutine, which the docs prohibit because a
blocked writer stalls the second `RLock`. Fix: release-then-recheck
(double-checked locking), accepting that another goroutine may fill first.

### "Fixing" ordering with one global mutex

Wrong: wrapping every transfer in a single global lock. It is correct — and it
collapses throughput to single-threaded, defeating the reason per-resource
locks exist. Fix: keep per-resource locks and order them; reserve a global lock
for genuinely global state.

### TryLock retry loops without jitter or a budget

Wrong: `for !mu.TryLock() {}` or fixed-interval retries. Contenders fail in
lockstep (livelock) or spin-burn a core. Fix: randomized backoff
(`rand/v2.Int64N`) and a bounded retry budget that returns a typed error the
caller can convert into a 503 or a queue requeue.

### Holding a mutex while blocking on a channel or pool

Wrong: acquiring a semaphore slot, sending on a channel, or making a network
call while holding a mutex. The cycle now runs through a non-mutex resource and
no mutex-only reasoning will find it. Fix: document the acquisition hierarchy
across all blocking resources, and release the lock before any operation that
can block on one.

### Trusting the runtime deadlock panic in tests

Wrong: assuming a deadlocked test will crash with `all goroutines are asleep`.
It will not — the test runner's own goroutines are alive, so the job hangs
until the external timeout kills it with no stack pointing at the locks. Fix: a
watchdog harness that runs the function with a deadline and fails with a full
`runtime.Stack(buf, true)` dump on expiry.

### Reasoning about release order

Wrong: carefully unlocking in reverse acquisition order "to avoid deadlock".
Release order is irrelevant to deadlock — only acquisition order creates
wait-for edges. (Release order can matter for other invariants, such as
briefly-inconsistent intermediate states becoming visible, but not for
cycles.) Fix: spend the care on acquisition order and on keeping critical
sections small.

Next: [01-ordered-transfer-bank.md](01-ordered-transfer-bank.md)
