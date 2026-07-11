# Distributed Rate Limiting with Redis — Concepts

A rate limiter is easy to write and hard to get right. The toy version —
`golang.org/x/time/rate.NewLimiter` in a package variable — is correct for a
single process and silently wrong the moment you run more than one replica. This
lesson is about the decision a senior engineer actually owns: which algorithm
protects a shared, horizontally-scaled service, how to make the check atomic on a
central store, and how the limiter behaves when that store is slow or gone. Get
those three answers right and you have a limiter your on-call team can defend at
3 a.m.; get them wrong and the limiter becomes the outage it was meant to
prevent.

## Why the state cannot live in the process

A limiter answers one question: has `key` used more than its quota in the current
window? To answer it, the limiter must count. If the count lives in process
memory, then in a fleet of N replicas behind a load balancer each replica keeps
its own count and grants the full quota. A limit of "100 requests per minute per
user" becomes "100 x N per minute per user" — the effective global limit scales
with your deployment, which is the opposite of a limit. In-memory limiters
(`x/time/rate`) are the right tool only when the thing you are protecting is
also per-process (a local CPU-bound section, a single connection pool). Anything
shared — a downstream API, a database, a paid third-party — needs a shared
counter.

Redis is the usual home for that counter: fast, atomic primitives, TTLs for free,
and every replica already talks to it. The cost is explicit and must be
acknowledged: you have added a network round trip to the hot path of every
request. That round trip is why the failure-mode discussion later is not
optional.

## The atomicity requirement: why Lua, not MULTI/EXEC

Rate limiting is a read-decide-write: read the current count, decide whether this
request fits, and write the incremented count (or append the timestamp). If those
three steps are separate Redis commands, two concurrent requests can both read
the same stale count, both decide they fit, and both write — and the limit is
breached under exactly the load it exists to control. `GET` then `INCR` in Go is
this bug.

The instinct to reach for a Redis transaction (`MULTI`/`EXEC`) does not fix it.
`MULTI` queues commands and runs them together, but it cannot branch on a value
read *inside* the transaction: you cannot say "read the count, and only if it is
below the limit, increment it" because the queued `INCR` has no access to the
result of the queued `GET`. Transactions give you all-or-nothing, not
read-decide-write.

The tool that does is a Lua script run with `EVAL`. Redis executes a script
atomically start to finish — no other command interleaves — and the script can
read a value, branch on it in Lua, and conditionally write, all in one
indivisible server-side step. That is why every serious Redis limiter is a Lua
script: the prune, the count, and the add happen together or not at all, and two
concurrent callers cannot both pass a stale check. In go-redis, `redis.NewScript`
plus `(*Script).Run` sends `EVALSHA` (the cached script hash) and transparently
falls back to `EVAL` with the full body the first time, so you pay to transmit
the script once, not per request.

## The four algorithms and their trade-offs

**Fixed-window counter.** One `INCR` on a key named for the current window
(`limit:user42:minute-871234`), with `PEXPIRE` set to the window length. O(1)
memory, one or two commands, trivially correct within a window. Its flaw is the
boundary burst: a client can send the full quota in the last instant of one
window and the full quota again in the first instant of the next, so up to 2x the
limit passes in a span shorter than a single window. For many APIs that is
acceptable; for a strict downstream budget it is not.

**Sliding-window log.** Store every request as a member in a sorted set scored by
its timestamp. On each request, `ZREMRANGEBYSCORE` prunes entries older than the
window, `ZCARD` counts what remains, and if there is room `ZADD` appends the new
one. Exact — no boundary burst — but memory is O(requests in window) per key,
which for a hot key is a lot of small ZSET members. Use it when precision matters
and per-key volume is bounded.

**Sliding-window counter.** A cheap approximation of the log: keep a counter for
the current and previous fixed windows and blend them by how far into the current
window you are (`prev * (1 - elapsed_fraction) + curr`). O(1) memory, no boundary
burst as bad as fixed-window, but only approximate near the seam. A good default
when you want smoothness without the log's memory.

**Token bucket / GCRA.** Model a bucket that refills at a steady rate up to a
maximum (the burst). Each request takes a token; if none remain it is denied.
This gives a smooth long-run rate while permitting a controlled burst — the
behavior most APIs actually want. GCRA (the Generic Cell Rate Algorithm) is the
elegant O(1) implementation: instead of storing a token count it stores a single
"theoretical arrival time" per key and compares it to now. `redis_rate` (the
go-redis rate limiter) is GCRA under the hood; `Limit{Rate, Burst, Period}` says
"Rate requests per Period, tolerating a Burst above the steady rate."

## GCRA as virtual time

GCRA is worth understanding because it looks like magic: constant state, smooth
rate, explicit burst. The trick is that it does not count tokens. It stores one
timestamp, the theoretical arrival time (TAT) — the earliest moment at which the
bucket would be empty if requests kept arriving at exactly the allowed rate. Each
request computes how far the TAT is ahead of now: if it is within the burst
tolerance, the request is allowed and the TAT is pushed forward by one interval;
if it is further ahead than the tolerance, the request is too early and is
denied, with the gap telling you the Retry-After. One number, O(1), and Burst is
literally how far ahead of real time a caller may run.

## Clocks, time source, and TTL hygiene

A sliding-window log is only as consistent as the clock that scores its entries.
If each client stamps entries with its own wall clock, a client whose clock runs
fast reserves future slots and one whose clock lags double-counts — clock skew
corrupts the window. The fix is to read the clock from one authority: call
`redis.call('TIME')` *inside* the Lua script and score entries by the Redis
server clock, so every replica agrees regardless of its own drift. Mind the
units: `TIME` returns seconds and microseconds; ZSET scores are floats and
`PEXPIRE` takes milliseconds, so convert deliberately and pick one resolution.

Every rate-limit key must expire. A limiter creates one key per distinct client,
and clients come and go; without `EXPIRE`/`PEXPIRE` those keys accumulate forever
and leak Redis memory. Set the TTL to at least one window and refresh it on every
write *inside the script*, so a key that keeps seeing traffic keeps its TTL and a
key that goes idle is reclaimed. A classic bug is setting the TTL only on the
first `INCR` and losing it on a reset path.

## Failure modes: the limiter must not become the outage

The round trip to Redis is on the hot path of every request. If Redis gets slow,
every request gets slow; if Redis is unreachable and the call has no timeout,
every request hangs. A rate limiter with no timeout is a latency amplifier that
converts a Redis blip into a site-wide stall. So wrap every limiter call in a
short `context.WithTimeout` — a few milliseconds — so a struggling Redis costs you
that budget and no more.

When the call errors or times out you must have already decided, per endpoint,
what to do. **Fail-open** admits the request: it protects availability at the risk
of overloading the thing you were limiting. **Fail-closed** rejects the request:
it protects the downstream at the risk of a self-inflicted outage, because a Redis
blip now takes down your whole API. Neither is universally right. A public,
best-effort endpoint usually fails open; a limiter guarding an expensive or
fragile downstream usually fails closed. The non-negotiable part is that you
decide deliberately and emit a metric (allowed / denied / degraded) so on-call
can see the moment the limiter starts degrading — a limiter silently failing open
during an incident is the worst of both worlds.

## The response contract

A denied request returns `429 Too Many Requests` with a `Retry-After` header
telling the client when to come back. Alongside it, emit the IETF RateLimit
headers — `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset` — which
supersede the older ad-hoc `X-RateLimit-*` names and let well-behaved clients
self-throttle before they are rejected. One subtlety: if every rejected client is
told the same `Retry-After`, they all retry at the same instant and produce a
synchronized thundering herd. Add a small random jitter to `Retry-After` so the
retries spread out.

## Hot keys, cluster slots, and cost

Per-user keys distribute load across Redis naturally; a single global key (one
counter for the whole service) concentrates every request on one shard and
becomes a hot spot. In Redis Cluster this matters more: all keys touched by one
Lua script must hash to the same slot, so a script that needs several keys must
use hash tags (`{user42}:a`, `{user42}:b`) to co-locate them. Cost-wise, each
check is one round trip plus a Lua execution; script caching via EVALSHA avoids
resending the body, but you cannot pipeline the check away, because the decision
must gate the request synchronously — the answer has to come back before you
handle or reject the call.

## Common Mistakes

### Read, compare in Go, then write as separate commands

Doing `GET`, comparing the count in Go, then `INCR`/`SET` is a race: two
concurrent requests read the same count, both decide they fit, and both write, so
the limit is exceeded under load. The decision and the mutation must be one atomic
step. Move the whole read-decide-write into a single Lua `EVAL`.

### Using MULTI/EXEC to fix the race

`MULTI`/`EXEC` runs queued commands together but cannot branch on a value read
mid-transaction, so it cannot express "increment only if under the limit." It
solves a different problem. Lua is the tool that reads, branches, and conditionally
writes atomically.

### Forgetting to expire the key (or losing the TTL on reset)

A limiter mints one key per client; without `PEXPIRE` those keys live forever and
leak memory. Setting the TTL only on the first `INCR` and dropping it when the
window resets has the same effect. Set and refresh the TTL on every write inside
the script, to at least one window.

### Scoring the sliding-window log with the client's clock

Stamping ZSET entries with each caller's wall clock lets a skewed client claim too
much or too little quota. Read the clock once, from the Redis server, with
`redis.call('TIME')` inside the script, so all replicas agree.

### Treating fixed-window as if it were smooth

Fixed-window is cheap but permits nearly 2x the limit across the window boundary.
If your downstream budget is strict, that burst is a real breach; choose a sliding
window or token bucket instead of assuming fixed-window is "close enough."

### No timeout on the Redis call

A limiter call with no `context.WithTimeout` turns a slow or unreachable Redis into
latency on every request. Bound each call to a few milliseconds so the limiter can
never add more than that budget to a request.

### Blindly failing open or closed

Picking a degradation policy by reflex is the mistake. Fail-closed on a Redis blip
can take the whole API down; fail-open silently disables protection during the
exact incident it exists for. Decide per endpoint, and emit a degraded metric so
the choice is observable.

### Reading redis.Nil as an error

The first request for a key finds nothing; go-redis signals that with `redis.Nil`,
which is the empty-key case, not a failure. Treating it as an error produces
spurious rejections or 500s. Handle `redis.Nil` as "no prior state" — though a
well-written Lua script avoids the issue by handling the missing key itself.

### Retry-After with no jitter

Returning the same `Retry-After` to every rejected client makes them all retry at
the same instant, producing a synchronized burst. Add a small random jitter so
the retries spread over an interval.

### Assuming an in-memory limiter is enough for many replicas

`golang.org/x/time/rate` is a per-process token bucket. In a multi-replica
deployment each process grants the full quota, so the effective global limit is N
times what you intended. Centralize the state in Redis for anything shared.

Next: [01-sliding-window-atomic-lua.md](01-sliding-window-atomic-lua.md)
