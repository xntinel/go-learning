# Building a Thread-Safe Cache — Concepts

Every backend service eventually grows an in-process cache: in front of a
database, an auth-token endpoint, a pricing service, a downstream API. The naive
version — one `map` behind one `sync.Mutex` — works in the demo and melts in
production: every operation serializes through a single lock, a hot-key miss
stampedes the database with five hundred identical queries, entries never
expire (or all expire in the same second), and memory grows until the OOM
killer arrives. This lesson builds the cache the way it is built in production
services and libraries like groupcache and ristretto: lock-striped shards to
keep p99 latency flat under contention, TTL semantics checked lazily on read
and swept in the background, singleflight coalescing to kill stampedes, LRU
bounds so memory cannot grow without limit, TTL jitter and negative caching so
load spikes do not synchronize, stale-while-revalidate to trade freshness for
tail latency, atomic metrics wired to `expvar`, an HTTP response-cache
middleware as the consumer-facing artifact, and benchmarks that quantify why 16
shards beat one mutex. Read this file once; each of the ten exercises that
follow is an independent module that assumes this foundation.

## Concepts

### Lock striping distributes contention

A single mutex over the whole cache turns every operation into a queue: under
load, goroutines convoy behind the lock and p99 latency tracks the length of
that queue, not the cost of a map lookup. Lock striping splits the keyspace
into N shards, each with its own `sync.RWMutex`; operations on different shards
never touch the same lock and proceed in parallel. Shard selection is a cheap
non-cryptographic hash of the key modulo N — FNV-1a via `hash/fnv` is the
conventional choice: fast, decent distribution, no allocation beyond the hasher.

Striping has limits worth internalizing. It helps *contention*, not hot spots:
if 90% of traffic reads one key, all of it lands on one shard and striping buys
nothing (that problem is solved by the read path being an `RLock`, and beyond
that by per-CPU or copy-on-write designs). And returns diminish once the shard
count greatly exceeds the number of goroutines that can actually run — past
roughly 4x the core count, more shards mostly just cost memory. Exercise 10
measures exactly where the curve flattens.

### RWMutex economics

`sync.RWMutex` admits unlimited concurrent readers under `RLock` and gives
`Lock` exclusive access, so a 99%-read cache scales with striping plus read
locks. Two caveats make this less free than it looks. First, `RWMutex` is more
expensive than `Mutex` in the uncontended case and has writer-preference
handoff behavior: once a writer is waiting, new readers block behind it, so a
steady trickle of writes can stall the read path in bursts. Second — and this
is the deep one — any bookkeeping on the read path silently converts reads into
writes. Move an element to the front of an LRU list on `Get` and the read path
now mutates shared state, which means `Lock`, not `RLock`; increment a hit
counter under the shard mutex and you have widened the critical section for
bookkeeping. This is precisely why production caches keep metrics in
`sync/atomic` counters (exercise 8) and why high-performance caches abandon
strict LRU for sampled or frequency-based approximations (exercise 5 discusses
the trade-off; ristretto's TinyLFU is the canonical example).

### TTL: lazy expiry plus a background sweep — both, not either

Storing an `expiresAt` with each entry makes `Get` self-checking: if
`time.Now()` is past the deadline, treat the entry as missing and return the
zero value. That is *lazy expiry*, and it is the correctness half — no caller
ever observes a stale value. But lazy-only leaks: an entry that expires and is
never read again occupies memory forever. The background sweep is the memory
half: a goroutine on a `time.Ticker` periodically walks the shards and deletes
expired entries. Sweep-only is wrong too — between sweeps, `Get` would serve
dead values. You need both.

The sweep has one iron rule: lock shards *one at a time*. Acquire shard 0's
lock, sweep it, release, move to shard 1. Holding all shard locks turns cleanup
into a stop-the-world pause where every foreground operation on every shard
blocks; nesting shard locks additionally creates lock-ordering deadlock risk
against any other code path that touches two shards. The acquire-release loop
means the sweeper only ever contends with traffic on the one shard it is
currently visiting.

### Goroutine lifecycle is part of the API contract

`StartCleanup` spawns a goroutine, and that makes lifecycle part of the type's
public contract. The caller needs two things: a way to *stop* the goroutine (a
stop channel, or better a `context.Context`), and a way to *observe that it
stopped* (a done channel the goroutine closes on exit). Without the second,
shutdown code can only sleep and hope — and "sleep and hope" is how leaked
tickers survive into tests (tripping goroutine-leak detectors) and into
production (a ticker per cache instance firing for the process lifetime). Go
1.24's `t.Context()` makes the context-scoped variant natural to test: cancel,
then block on the done channel with a timeout, proving the goroutine actually
exited. Exercise 2 builds both variants.

### Cache stampede and singleflight

The classic incident: a hot key expires, and in the next ten milliseconds five
hundred concurrent requests all miss, and all five hundred run the same
database query. The database, sized for the cached steady state, falls over;
its latency spike causes more timeouts and retries; the retry storm finishes
the job. `golang.org/x/sync/singleflight` is the standard fix:
`Group.Do(key, fn)` collapses concurrent calls with the same key into a single
execution of `fn` whose result every waiter shares (the third return value,
`shared`, reports when that happened). Two rules keep it safe. Never cache the
loader's error as if it were a value — return it to all waiters and store
nothing. And call `Group.Forget(key)` after a failure so the next request
starts a fresh load instead of the group pinning a poisoned in-flight call.
Note the scope: singleflight coalesces within one process. Fifty instances
each still send one query — for fleet-scale protection you layer on TTL
jitter, stale-while-revalidate, or a distributed lock.

### Synchronized expiry, TTL jitter, and negative caching

A deploy warms 100k keys with `ttl = 5m`. Five minutes later all 100k expire
in the same second and the database absorbs a synchronized load spike — then
the refill re-synchronizes them and the spike repeats every five minutes. The
fix is to decorrelate expiry by adding random jitter to each TTL:
`ttl + rand.N(maxJitter)` using `math/rand/v2`, whose generic `rand.N` works
directly over `time.Duration` (never resurrect the deprecated global-seed
patterns from `math/rand` v1). A 10-20% jitter fraction spreads the expiry of
a warmed cohort across a wide window at negligible freshness cost.

Negative caching solves the inverse leak: a lookup for a *nonexistent* key is
never cached (there is no value to store), so a client polling a missing user
id hammers the database on every request. Cache the miss itself — a typed
"not found" envelope distinct from any real value — with a deliberately short
TTL. Short, because a negative entry delays visibility of creation: a user
registered one second after a negative entry was cached stays invisible until
that entry expires. Positive TTLs are a staleness trade-off; negative TTLs are
an *existence* trade-off, and existence changes matter more.

### Freshness versus latency: stale-while-revalidate

For read-heavy endpoints — feature flags, pricing tables, public profiles —
tail latency matters more than perfect freshness, and freshness is a spectrum,
not a boolean. RFC 5861 names the pattern for HTTP; the in-process version
gives each entry two horizons: `freshUntil` and `staleUntil`. Inside the fresh
window, serve immediately. Between the horizons, serve the stale value
*immediately* and trigger exactly one background refresh (guarded by an
`atomic.Bool` compare-and-swap or singleflight, so twenty concurrent stale
readers do not launch twenty refreshes). Past `staleUntil`, block on the loader
like a cold miss. The companion semantics, stale-if-error, fall out naturally:
a failed background refresh keeps serving the stale value until the hard
deadline instead of surfacing the failure to users.

One trap is severe enough to call out here: the background refresh must not
inherit the request's cancellable context. If it does, the first client to
disconnect cancels the refresh for everyone. `context.WithoutCancel(ctx)`
(Go 1.21+) keeps the request's values (trace ids, auth) while detaching its
cancellation.

### Bounded memory: LRU and its costs

An unbounded cache is a memory leak with a nicer name — an OOM with extra
steps. The textbook bound is LRU: each shard holds
`map[string]*list.Element` plus a `container/list.List`; `Get` moves the
element to the front, `Set` pushes new entries to the front and evicts from
`Back()` when the shard exceeds its capacity. Two consequences follow. Strict
LRU destroys the read-lock optimization — `MoveToFront` mutates the list, so
`Get` needs the exclusive `Lock`, and a read-locked `Get` that splices the
list is a data race the detector will catch. And per-shard capacities make the
global bound approximate: keys hash unevenly, so a "10,000 entry" cache with
16 shards of 625 may evict from a full shard while others sit half-empty.
Production caches accept both costs or sidestep them with sampled eviction
(Redis picks a few random candidates and evicts the oldest) or admission
policies (ristretto's TinyLFU), trading exactness of the recency order for
read-path speed.

### Observability: atomic counters and expvar

You cannot size or debug a cache you cannot see. Hits, misses, sets, deletes,
expirations, and evictions belong in `sync/atomic` counters incremented on the
relevant paths — never counters guarded by the shard mutex, because widening a
lock's critical section for bookkeeping taxes every operation to serve a
monitoring read that happens once per scrape. A `Stats()` method snapshots the
counters and computes the hit ratio (guard the zero-traffic division). The
counters are read at slightly different instants, so a snapshot can be
momentarily torn — hits observed, the matching miss not yet — and for
monitoring that is fine; do not add locking to fix a non-problem. Publish the
snapshot on the standard `/debug/vars` endpoint with
`expvar.Publish(name, expvar.Func(...))` and every scraper that speaks JSON
can read it. Resist per-key metrics: keys are unbounded user input, and
unbounded label cardinality is how monitoring systems die.

### Zero values are ambiguous

`Get` returning `(zero, false)` is the miss contract, but generics make one
edge sharp: store a nil pointer in a `Cache[*User]` and `Get` returns
`(nil, true)` — a hit that looks exactly like a miss. Callers either treat it
as missing (breaking the cache's purpose) or dereference it (panicking).
Choose explicitly: document that nil values are the caller's problem, reject
nil at `Set`, or wrap values in a result envelope with its own "present" flag —
which is the same envelope negative caching needs anyway.

### Measure, don't guess

Every design claim above is measurable, and exercise 10 measures them.
`testing.B.RunParallel` distributes `b.N` iterations across `GOMAXPROCS`
goroutines that each pull work via `(*testing.PB).Next`;
`b.SetParallelism(p)` multiplies the goroutine count to push contention past
the core count; `b.ReportAllocs` surfaces allocation costs. Benchmarking a
single mutex against 1, 4, 16, and 64 shards against `sync.Map` across read
ratios shows each design's regime: the writer convoy on the single mutex, the
striping curve flattening past ~4x cores, and `sync.Map` winning
append-mostly disjoint-key workloads (its documented target) while losing
mixed read-write ones to a plain sharded map. Two disciplines keep benchmarks
honest: run a conformance test over every implementation first so you are
measuring equivalent semantics, and never read timings taken under `-race` as
performance numbers — the race detector is a correctness gate that slows
memory access by an order of magnitude. The next lesson turns these benchmark
suspicions into evidence with the mutex and block profilers.

## Common Mistakes

### Slow work while holding a shard lock

Wrong: performing a network call, running the loader, or JSON-encoding a value
between `Lock` and `Unlock`. Every key that hashes to that shard convoys
behind one straggler, and one slow database query becomes a latency spike for
an unrelated fraction of the keyspace.

Fix: copy what you need under the lock, release, do the slow work, then store
the result under a fresh `Lock`. The lock protects the map, not the workflow.

### Sweeping with all shard locks held

Wrong: acquiring every shard's lock up front and then iterating, or nesting
one shard's lock inside another's. Cleanup becomes a stop-the-world pause, and
nested acquisition creates lock-ordering deadlock risk.

Fix: acquire shard i, sweep it, release, move to i+1. The sweeper contends
with at most one shard's traffic at a time.

### Leaking the sweeper

Wrong: calling `StartCleanup` and never closing the stop channel or cancelling
the context. The goroutine and its ticker live for the process lifetime; in
tests, leak detectors trip and timers keep firing into later tests.

Fix: pair every start with a deferred stop, and wait on the returned done
channel so shutdown is observed, not hoped for.

### Storing nil pointers in Cache[*T]

Wrong: `Set("k", nil, ttl)` on a `Cache[*Foo]`. `Get` returns `(nil, true)`
and the caller either misreads it as a miss or dereferences it.

Fix: define the contract explicitly — forbid nil at `Set`, or use a result
envelope with an explicit presence flag (which negative caching requires
anyway).

### Caching loader errors, or forgetting Forget

Wrong: storing the error result of a singleflight load as if it were a value,
or leaving a failed call in the group. One transient database error gets
pinned and served to every caller until TTL — or every caller joins a
poisoned flight.

Fix: return errors to all waiters, cache nothing, and call `Group.Forget(key)`
after a failure so the next caller retries. If you *want* to cache failures,
that is negative caching: deliberate, typed, and with its own short TTL.

### LRU bookkeeping under RLock

Wrong: calling `MoveToFront` in a `Get` that holds only `RLock`. The list
splice is a write; concurrent read-locked Gets corrupt the list, and
`go test -race` flags it.

Fix: LRU forces `Lock` on the read path. That is the documented cost of strict
LRU versus a plain TTL cache — accept it or use sampled eviction.

### Identical TTLs on bulk-warmed keys

Wrong: warming a cohort of keys with the same TTL. They expire in the same
instant and the origin takes a synchronized, periodic load spike.

Fix: `SetJittered` — add `rand.N(maxJitter)` from `math/rand/v2` to each TTL
so expirations decorrelate.

### Caching the wrong HTTP responses

Wrong: a response-cache middleware that stores `Set-Cookie` headers (leaking
one user's session to another), caches 500s (pinning an outage), caches
per-user content keyed only by URL, or buffers unbounded bodies; also the
recorder bug where `WriteHeader` is honored after `Write` already defaulted
the status.

Fix: bypass requests with `Authorization`, store only 200 GETs without
`Set-Cookie`, cap the buffered body size, and make the recorder latch the
status on first `Write`.

### Exact-count assertions in timing-dependent tests

Wrong: asserting an exact hit count when TTL expiry races a background
sweeper. The test flakes in CI under load, gets retried until green, and stops
meaning anything.

Fix: pin hard invariants — no panic, race detector silent, some hits, size
bounds — and use injected clocks (or `testing/synctest`) where you need
deterministic freshness windows.

### Benchmarking the wrong thing

Wrong: comparing implementations that do not have equivalent semantics, or
quoting numbers from a `-race` run.

Fix: run a shared conformance test over every benchmarked implementation
first, and benchmark without the race detector; `-race` is a correctness gate,
not a performance environment.

Next: [01-sharded-ttl-cache-core.md](01-sharded-ttl-cache-core.md)
