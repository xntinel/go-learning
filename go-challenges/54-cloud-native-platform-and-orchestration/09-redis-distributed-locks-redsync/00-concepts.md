# Distributed Locks with Redis and redsync — Concepts

A distributed lock is what you reach for when a critical section must be
serialized across replicas: a singleton cron that must run on exactly one pod, a
cache rebuild that must not stampede, a per-tenant reconciliation that must not
double-charge. `go-redsync/redsync` implements the Redlock algorithm over one or
more independent Redis masters, so this topic is really about the gap between "I
hold the lock" and "it is safe to act on that belief". Internalize one sentence
and the rest follows: a Redis lock is a LEASE with a TTL, not a mutex. It can be
lost silently to a garbage-collection pause, an OS scheduler preemption, a
network partition, or clock drift, while your goroutine keeps running inside what
it thinks is a protected region. This file is the conceptual foundation for the
three independent exercises that follow.

## Concepts

### A Redis lock is a lease, not a mutex

An in-process `sync.Mutex` is held from `Lock` to `Unlock` with no third outcome.
A Redis lock is a key with a TTL. Holding it means "no one else holds it right
now, on a quorum of nodes" — it does not mean "I will keep holding it until I
release it". The TTL can expire while your process is still inside the critical
section, and Redis will happily hand the lock to someone else the instant it
does. Every correctness question about distributed locking reduces to this: the
lock is a time-boxed lease, and the clock does not stop for your goroutine.

### The Redlock algorithm, and what single-node means

Redlock, as redsync implements it, is: `SET key <unique-value> NX PX <ttl>`
against N independent Redis masters. The lock is considered held only if it was
acquired on a majority (N/2+1) of the nodes within a time budget, after
subtracting the elapsed acquisition time and a drift factor from the validity
window. Unlock and Extend are Lua scripts that first check that the stored value
matches the value this client holds, then `DEL` or `PEXPIRE` — so a client can
never release or renew a lock that a different owner now holds.

A crucial caveat for real deployments: single-node redsync is just N=1. It is a
check-and-set `SETNX` with a check-and-delete unlock, and it has no quorum
safety. If that one Redis fails over to a replica that had not yet received the
`SET`, the lock is silently lost and two holders can coexist. Believing that a
single Redis gives you Redlock's majority guarantee is a common and dangerous
mistake; the quorum only exists when you point redsync at several genuinely
independent masters.

### Why the lock value must be unique per acquisition

Unlock and Extend compare the value stored in Redis to the value this `Mutex`
object holds. If two clients used the same value, a client whose lease had
already expired could delete or renew the lock that a new owner just acquired —
exactly the corruption the check-and-set is meant to prevent. redsync generates a
cryptographically random value per acquisition by default; `WithGenValueFunc` and
`WithValue` let you control it (the fencing exercise uses `WithGenValueFunc` to
make the value a monotonic token). The random value guarantees *ownership checks*;
it is not, and cannot be, an ordering token — see fencing below.

### TTL sizing is the core trade-off

Pick the TTL too short and normal work, or a single stop-the-world GC pause,
loses the lease mid-flight, producing a split brain where two holders run at
once. Pick it too long and a crashed holder blocks everyone until the lease
expires, which is an availability hit. The rule of thumb is a TTL comfortably
above the worst-case latency of the protected section — or, when the work can run
arbitrarily long, a short TTL plus a renewal watchdog that extends the lease
while the work proceeds.

### Renewal: holding a lease longer than its TTL

To hold a lease longer than its TTL you renew it. `ExtendContext` resets the
expiry; you call it from a background goroutine on a ticker at roughly TTL/2, so
a single missed renewal still leaves slack before expiry. The renewal must run on
a *separate* goroutine from the work: if you renew on the same goroutine that is
executing a long synchronous call, the renewal never fires and the lease silently
expires underneath you. And the decisive rule: if `Extend` reports failure you
must assume the lease is lost and abort the work — cancel a context so the
critical section stops — rather than pressing on unprotected. `Extend` returning
`ok == false` is a normal, expected outcome, not merely an error to log. (Note
the exact contract: with a single node, a lost lease makes `ExtendContext` return
`(false, err)` — the boolean is the signal to trust; do not wait for a specific
sentinel.)

### The fundamental Redlock limitation

This is the Kleppmann-versus-antirez debate, and a senior should know both
sides. Redlock guarantees mutual exclusion only under bounded clock drift,
bounded process pauses, and bounded network delay. A long STW GC pause, an OS
scheduler preemption, or a network partition can make holder A believe it still
holds the lock after its lease has expired and holder B has legitimately acquired
it. Redlock cannot then prevent A from acting on its stale belief. No amount of
tuning closes this gap; it is inherent to a lease-based lock over an
unsynchronized network. Redlock gives you at-most-N-holders under happy
conditions, not guaranteed mutual exclusion under adversarial timing.

### Fencing tokens close the gap — and redsync does not provide them

The fix is a fencing token: a strictly monotonically increasing number, minted at
acquire time (with Redis `INCR`, or a real consensus store) and passed to the
guarded resource. The resource records the highest token it has ever served and
rejects any operation carrying a token less than or equal to it. Now a stale
holder's late write is a no-op regardless of what that holder believes about the
lock, because a newer holder already advanced the token. redsync does not do this
for you; `Mutex.Value()` is unique but *not* monotonic, so it cannot order two
holders. You mint the token yourself (the fencing exercise wires `INCR` into
`WithGenValueFunc`) and the guarded resource enforces the ordering.

### Efficiency versus correctness — the decision that drives everything

Use Redlock when the lock is an *optimization*: avoid duplicate work, reduce
contention, keep two pods from rebuilding the same cache. There an occasional
double-execution is merely wasteful, and Redlock's happy-path guarantee is
enough. Do NOT rely on Redlock alone when the guarded action must never happen
twice — money movement, non-idempotent external calls. There you need fencing
tokens and/or a linearizable store (etcd, ZooKeeper, Postgres advisory locks).
This is the on-call sentence to remember: Redlock for efficiency, fencing tokens
or consensus for correctness.

### Idempotency is the pragmatic partner of locking

Because no distributed lock is perfectly safe, correctness-critical writes should
also be idempotent: dedupe keys, conditional updates, upserts keyed on a request
id. Then a rare mutual-exclusion violation degrades to a harmless retry rather
than corruption. Locking reduces the probability of a double effect; idempotency
bounds its blast radius. Senior systems use both.

### Contention and failure are different, and code must distinguish them

When acquisition fails, redsync tells you *why* through the error. A peer holding
the lock surfaces as `redsync.ErrFailed` or a `*redsync.ErrTaken` (which carries
the indices of the contended nodes): that is normal — back off, retry later, or
skip this tick. A connection refusal, timeout, or Redis error means the
infrastructure is degraded: alert, and fail closed or open per policy. Collapsing
the two into one "lock failed" branch masks outages and turns a Redis incident
into silent, unexplained work-skipping.

### WithSetNXOnExtend trades a guarantee for availability

`WithSetNXOnExtend` lets `Extend` re-create a key that vanished (for example a
Redis restart with no persistence) using `SET NX`, improving availability. But it
weakens the guarantee that the lease was *continuously* held — during the gap
another holder could have slipped in. That is acceptable for an efficiency lock
and dangerous for a correctness lock. Reach for it deliberately, not by default.

## Common Mistakes

### Treating the lock as a real mutex

Wrong: assuming the lock is held for the entire critical section, ignoring that
the TTL can expire mid-flight during a GC pause or partition.

Fix: size the TTL against worst-case latency, renew a lease you must hold longer
than its expiry, and for correctness add fencing tokens. Treat "I hold it" as a
belief with an expiry, not a fact.

### Discarding the (bool, error) result of Unlock/Extend

Wrong: `m.UnlockContext(ctx)` or `m.ExtendContext(ctx)` called for its side
effect, ignoring the returned boolean. A `false` with a `nil` error is a
legitimate "you no longer hold it", not noise.

Fix: inspect the boolean. On `Extend`, a `false` means the lease is lost and the
work must abort; on `Unlock`, a `false` means the lease had already expired or was
taken over — which is information, not a bug to swallow.

### Continuing after a failed renewal

Wrong: logging an `Extend` failure and letting the protected work run on. That is
exactly how you get two active holders.

Fix: cancel the derived context the moment renewal fails so the critical section
stops at the same instant the lease is no longer provably held.

### Using Mutex.Value() as a fencing token

Wrong: passing the Redlock random value to the guarded resource as if it ordered
holders. It is unique but not monotonic; it cannot say which of two holders is
newer.

Fix: mint a monotonic token with `INCR` (or a consensus store) via
`WithGenValueFunc`, and have the resource reject tokens that do not strictly
increase.

### Trusting single-node redsync for quorum safety

Wrong: pointing redsync at one Redis and assuming Redlock's majority guarantee. A
single failover to a stale replica loses the lock.

Fix: for quorum safety use several independent masters; otherwise be explicit that
this is a best-effort efficiency lock, not a safety guarantee.

### Renewing on the work goroutine, or leaking the watchdog

Wrong: calling `Extend` inline in the work loop (so a long call starves renewal),
or starting a renewal ticker with no `ticker.Stop()` and no `select` on
`ctx.Done()` (so it leaks when the parent is cancelled).

Fix: renew on a dedicated goroutine driven by a ticker; `defer ticker.Stop()` and
select on `ctx.Done()` so it exits cleanly when the parent context ends.

### Blocking with Lock() on a hot path

Wrong: calling the retrying `LockContext` (up to `WithTries`) where you only
wanted "am I the leader this tick?", causing latency spikes under contention.

Fix: use `TryLockContext`, which attempts once and returns immediately, for
leader-election-per-tick decisions.

### Sharing one *Mutex across goroutines

Wrong: reusing a single `*redsync.Mutex` from concurrent goroutines. A `Mutex`
holds single-owner state (its value, its until) and is not itself
concurrency-safe.

Fix: create one `Mutex` per acquisition.

### Relying on the lock for correctness it cannot provide

Wrong: guarding exactly-once money movement with Redlock alone.

Fix: layer fencing tokens and idempotency on top, or use a linearizable store for
the correctness-critical decision. The lock is an optimization; the token and the
idempotent write are the safety.

Next: [01-redlock-mutex-lifecycle.md](01-redlock-mutex-lifecycle.md)
