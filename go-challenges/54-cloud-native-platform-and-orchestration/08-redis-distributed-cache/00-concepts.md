# Distributed Caching with Redis — Concepts

Redis in front of a database is not a magic speedup box. The moment you put it
there you have created a second, weakly-consistent copy of authoritative data,
and every hard question that follows is about that copy: how stale is it allowed
to be, how do you invalidate it, and what happens to the origin when the cache
misses, changes shape, or disappears entirely. A senior engineer reasons about a
cache the way they reason about a replica — explicitly, in terms of staleness
windows and failure behavior under load — not as a black box that "makes things
fast." This file is the conceptual ground for the three exercises that follow:
cache-aside reads with jittered TTL, write-path invalidation and the dual-write
race, and stampede protection with `singleflight` plus a `SetNX` lock.

## Concepts

### The cache is a second, weakly-consistent copy

There is no invalidation scheme that makes the cache and the origin
transactionally consistent without giving up the performance you cached for. A
write updates two independent systems that share no transaction; between those
two operations, or after a crash, the two copies disagree. So every design choice
here reduces to one question: how much staleness can this data tolerate, and how
do you bound it? You do not eliminate inconsistency; you choose its size and its
duration and you make the failure modes boring.

### Read-path patterns: cache-aside, read-through, write-through, write-behind

Four patterns show up, and they differ in who owns the loading logic and when the
cache is written.

- Cache-aside (lazy loading): the application checks the cache, and on a miss it
  loads from the origin and writes the value back itself. The cache is a dumb
  key/value store; the app owns the logic. This is the default for a reason: it
  only ever caches keys that were actually requested, and it is naturally
  resilient to a cache outage because a miss and an error both just fall through
  to the origin.
- Read-through: the cache library itself loads from the origin on a miss (the app
  only ever talks to the cache). Cleaner call sites, but the cache layer now owns
  a dependency on your database and a cache outage is harder to fail open around.
- Write-through: on a write, update the origin and the cache together so the cache
  stays warm. Keeps hot keys populated; costs a cache write on every origin write,
  and can cache values no reader ever asks for.
- Write-behind (write-back): buffer writes in the cache and flush to the origin
  asynchronously. Highest write throughput, but the cache is now authoritative
  for un-flushed data and a crash loses it. Only worth it with durable buffering
  or change-data-capture.

Cache-aside is what the exercises build first because it is the resilient default;
write-through and delete-on-write are the write-path counterparts.

### redis.Nil is a value, not a failure

`go-redis` returns the sentinel error `redis.Nil` from `Get` when the key does not
exist. That is a cache miss — a normal, expected outcome — and it must be
distinguished from a real error (connection refused, timeout, OOM) with
`errors.Is(err, redis.Nil)`. Conflate the two and you get one of two bugs: either
every cache miss becomes a 500, or, worse, you treat a Redis outage as "key not
found" and serve wrong or empty data to users. The read path has exactly three
branches — hit (`err == nil`), miss (`errors.Is(err, redis.Nil)`), and failure
(any other error) — and each needs its own behavior.

### TTL is the staleness bound and the safety valve

A TTL is the primary way you bound staleness. It is also the primary safety valve:
it caps the blast radius of a bad cached value and of the dual-write race below,
because no matter what goes wrong, the entry self-corrects within the TTL.
Choosing a TTL is a trade between hit rate (longer is better) and freshness
(shorter is better); there is no universally right value, only a value chosen with
the data's tolerance for staleness in mind.

### Correlated mass-expiry (the expiry avalanche)

If many keys are written together — a cold start, a cache warm, a deploy that
flushed the cache — and they all get the same TTL, they all expire in the same
instant. At that instant every one of them misses at once and the origin is hit by
a synchronized wave. The fix is jitter: instead of a fixed TTL, use `base +
random(0, spread)` so the expiry times are decorrelated and the load is smeared
across a window instead of spiking. This is a one-line change (`base +
rand.N(jitter)`) that prevents a whole class of incident.

### Cache stampede, dogpile, thundering herd

A related but distinct failure: a single hot key expires, and every concurrent
request for it misses simultaneously and recomputes it at the same time. Instead
of the cache removing load from the origin, the expiry moment amplifies exactly
the load the cache existed to absorb — one expired key can take down a database.
There are two layers to the fix, because there are two scopes of concurrency:

- Within one process, request coalescing collapses the duplicate concurrent loads
  into a single execution. `golang.org/x/sync/singleflight` does this: many
  goroutines calling `Do(key, fn)` for the same key run `fn` once and all receive
  its result; the returned `shared` bool tells you the result was fanned out.
- Across processes, `singleflight` does nothing — N pods each collapse internally
  to one call, so the origin still sees N calls. To coalesce cluster-wide you need
  a distributed lock (only the lock winner recomputes) or probabilistic early
  recomputation. The exercise layers a `SetNX` lock on top of `singleflight` for
  exactly this reason.

### Write-path invalidation: delete-on-write vs update-on-write

When the origin changes, the cache must be made coherent. Two strategies:

- Delete-on-write: delete the key and let the next read re-populate it from the
  origin. Simple, self-healing, and it never caches a value nobody asked for. Its
  cost is a guaranteed miss (and origin load) on the next read.
- Update-on-write: overwrite the cached value with the new one. Keeps the key hot,
  but under concurrency two writers can interleave and leave a stale value cached
  (writer A reads, writer B reads, B writes cache, A writes cache — A's older
  value wins).

Delete-on-write plus a modest TTL is the common senior default: the delete bounds
the immediate staleness and the TTL bounds anything the delete missed.

### The dual-write inconsistency

Writing the origin and then the cache (or the reverse) is two operations across
two systems with no shared transaction. A crash or a reorder between them leaves
the cache disagreeing with the origin: origin updated, cache still holding the old
value (or a delete that never happened). You cannot eliminate this cheaply — only
bound it with a short TTL, detect it, or move to change-data-capture / write-behind
where a log is the source of truth. The honest engineering move is to name the
window out loud and size it, not to pretend an ordering makes it atomic.

### Negative caching and cache penetration

Cache penetration is traffic for keys that do not exist: every request misses the
cache (nothing to hit) and hits the origin, which is exactly the attack pattern of
key-enumeration abuse. The defense is negative caching: cache the fact that a key
is not found, under a short TTL, so repeated lookups for a missing key are
absorbed by the cache instead of hammering the origin. The short TTL keeps a key
that later gets created from staying invisible for long.

### Fail-open vs fail-closed

A cache is a performance dependency, not an availability dependency — unless you
make it one. If your read path returns an error to the caller when Redis is
unreachable, a cache outage becomes an application outage: you have converted a
latency optimization into a hard dependency. Cache-aside should fail open — a
Redis error falls through to the origin (with a timeout, and ideally a circuit
breaker in front) so an outage is a latency event, not a 500 storm. Fail-closed is
occasionally correct (when serving stale/absent data is worse than serving an
error), but it must be a deliberate choice, not an accident of error handling.

### Serialization is part of the cache contract

Whatever codec crosses the wire — `encoding/json` here, or a faster binary codec
in production — is part of the contract between the writer and every future
reader. Change the shape of the cached struct and the old bytes still sitting in
Redis will fail to decode, or worse, silently mis-decode into a plausible-looking
wrong value. The fix is to version the key namespace: prefix keys with a schema
version (`app:v2:user:...`), so a format change becomes a clean, total miss that
re-populates from the origin instead of a decode error or a corruption. Bumping
the namespace is the safe way to "flush" after a shape change.

### Distributed lock correctness for stampede control

The `SetNX` lock that coordinates stampede control across replicas has two
non-negotiable correctness properties. First, it must carry a TTL, so that a
holder that crashes cannot deadlock the key forever — `SET key token EX ttl NX` in
one command. Second, it must be released with a compare-and-delete keyed on a
unique owner token, not a plain `DEL`: if your load outran the lock TTL, the lock
may already belong to another owner, and a blind `DEL` would delete their lock.
The compare-and-delete has to be atomic, which means a Lua script via `EVAL`:
`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del",
KEYS[1]) else return 0 end`. This is a lightweight lock for coalescing recomputes,
not a general-purpose mutex; the redsync/Redlock lesson that follows raises the
correctness bar for locks you actually depend on for mutual exclusion.

### Connection pooling, timeouts, pipelining, transactions

Two operational details decide how the cache behaves under load. First, timeouts:
`redis.Options` fields `PoolSize`, `ReadTimeout`, `WriteTimeout`, and
`ContextTimeoutEnabled` govern behavior when Redis is slow. Without timeouts, a
slow Redis stalls goroutines, they hold pool connections, the pool exhausts, and
the incident propagates upstream into your service — the opposite of failing open.
Second, batching: `Pipeline` sends multiple commands in one round trip for
throughput; `TxPipeline` wraps them in `MULTI/EXEC` so the Redis-side commands
apply atomically. Neither makes the origin-plus-cache pair atomic — they only make
the cache-side mutations efficient (pipeline) or atomic among themselves
(transaction).

## Common Mistakes

### Treating redis.Nil as an error

Wrong: returning or logging an error on every `Get` that comes back `redis.Nil`,
turning normal misses into 500s — or ignoring the distinction entirely and
treating a real outage as "not found," serving empty data.

Fix: branch on exactly three cases — `err == nil` (hit), `errors.Is(err,
redis.Nil)` (miss, load from origin), any other error (failure, fail open).

### One TTL for every key

Wrong: `Set(ctx, key, v, 30*time.Minute)` for everything, so a batch written
together expires together and stampedes the origin in one instant.

Fix: add jitter — `base + rand.N(spread)` with `math/rand/v2` — so expiry times
are decorrelated.

### Assuming singleflight solved the stampede

Wrong: adding `singleflight` and declaring the thundering herd fixed. It only
coalesces within one process; a ten-pod deployment still hits the origin ten times
when the hot key expires.

Fix: layer a `SetNX` lock (with a TTL and a token) so cluster-wide only one loader
recomputes, and the others read the value the winner wrote.

### A lock with no TTL, or released with a plain DEL

Wrong: `SetNX` without an expiration (a crashed holder deadlocks the key forever),
or releasing with `DEL lockkey` (which can delete a lock a different owner acquired
after yours expired).

Fix: `SET key token EX ttl NX`, and release with an `EVAL` compare-and-delete that
only deletes when the stored value still equals your token.

### Caching a failed load

Wrong: storing the zero value or an error marker when the origin load fails, so
every subsequent reader gets the poisoned entry until it expires.

Fix: never cache on origin error; return the error (fail open) and let the next
request retry against the origin.

### Fail-closed caching

Wrong: returning an error to the caller when Redis is down, converting a
performance dependency into an availability dependency.

Fix: on any non-`redis.Nil` error, fall through to the origin so the outage is a
latency event, not an outage.

### Update-on-write racing to a stale value

Wrong: two concurrent writers both overwrite the cached value; the one that reads
first but writes last leaves stale data cached.

Fix: prefer delete-on-write plus a bounded TTL; the next read re-populates from
the origin.

### Pretending the dual write is atomic

Wrong: writing origin then cache and assuming they can never disagree, so no TTL
and no acknowledgment of the window.

Fix: name the window, bound it with a short TTL, and accept that a crash between
the two writes leaves the cache stale until expiry (or move to CDC).

### No client timeouts

Wrong: a default client with no `ReadTimeout`/`WriteTimeout` and no context
deadline; a slow Redis stalls goroutines that hold pool connections until the pool
is exhausted.

Fix: set timeouts on `redis.Options` and pass a context with a deadline into every
call.

### Changing the struct shape without versioning the namespace

Wrong: adding or renaming a field on a cached struct while old bytes are still in
Redis, so `json.Unmarshal` fails or silently mis-decodes.

Fix: bump the key namespace (`v1` to `v2`) on any serialized-shape change so old
entries become clean misses.

### Global rand.Seed for jitter or tokens

Wrong: `rand.Seed(time.Now().UnixNano())` and the deprecated global `math/rand`
for jitter, or `math/rand` for a lock token.

Fix: `math/rand/v2` (no seeding needed) for jitter, `crypto/rand` for an
unguessable lock owner token.

### Leaking or per-request clients

Wrong: creating a new `redis.Client` per request (no pooling) or never calling
`Close`, leaking connections.

Fix: build one pooled client at startup, reuse it everywhere, and `defer
rdb.Close()` at shutdown.

Next: [01-cache-aside-with-ttl.md](01-cache-aside-with-ttl.md)
