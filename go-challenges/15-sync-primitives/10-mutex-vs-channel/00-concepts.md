# Mutex vs Channel: Choosing the Right Synchronization Tool — Concepts

Go's concurrency proverb says "Do not communicate by sharing memory; share
memory by communicating." Taken as dogma, it produces terrible code: caches
funneled through manager goroutines, work queues built on mutex-guarded
slices, and hand-rolled semaphores where a library already exists. Taken as
engineering guidance, it is one of the sharpest design heuristics in the
language. This lesson builds the same production artifact — a token-bucket
rate limiter guarding an upstream API — twice, once with `sync.Mutex` and
once with a buffered channel, pins both to an identical race-checked
behavioral contract, and then extends into the three shapes rate limiting
actually takes in production: per-client HTTP middleware with eviction,
context-aware blocking `Wait`, and bounded fan-out with a weighted
semaphore. It ends the honest way: with benchmarks and a migration to
`golang.org/x/time/rate`.

## The proverb decoded: ownership, not prohibition

"Share memory by communicating" is a statement about *ownership transfer*,
not a ban on mutexes. When a value's ownership moves between goroutines — a
unit of work handed to a worker, a result handed back, an error propagated —
a channel makes the handoff explicit and race-free by construction: after
the send, the sender must not touch the value; after the receive, the
receiver owns it exclusively. No lock is needed because at any instant
exactly one goroutine considers the value its own.

When state *stays put* and multiple goroutines mutate it — a counter, a
cache, a token count, a configuration map — nothing is being handed off.
Forcing a channel into that shape means the state is still shared; you have
merely serialized every access through a goroutine and a channel hop. A
mutex is simpler, faster, and clearer for guarded state. The standard
library agrees with this reading: `net/http.Server`, `database/sql.DB`, and
`golang.org/x/time/rate.Limiter` are all built on mutexes, and the `sync`
package documentation's advice that "higher-level synchronization is better
done via channels" is guidance about program *architecture*, not a claim
that `Mutex` is a code smell.

The Go wiki's MutexOrChannel page compresses the decision into a table worth
memorizing:

- Channels: passing ownership of data, distributing units of work,
  communicating asynchronous results, coordinating goroutine lifecycle
  (done/stop signals).
- Mutexes: guarding a struct's internal state, counters and flags, caches
  and lookup tables, multi-field updates that must change atomically
  together.

The last item is underrated. A mutex trivially protects an invariant
spanning several fields ("`tokens` and `lastRefill` are always consistent
with each other") because the critical section covers both writes. There is
no natural channel encoding of a multi-field invariant that is not just a
manager goroutine in disguise.

## Serialize vs admit-N: the semantic difference

`Mutex.Lock` creates a critical section exactly one goroutine wide. A
buffered channel of capacity N is a *counting semaphore*: up to N holders
proceed concurrently. These are different primitives, not two syntaxes for
the same thing, and real designs hinge on the difference. A connection pool
or a fan-out bound wants N-concurrent semantics — eight requests in flight
at once, the ninth waits. An invariant-preserving state transition wants
one-at-a-time semantics — nobody may observe `tokens` mid-update.

The channel-as-semaphore idiom is three lines:

```
sem := make(chan struct{}, 8)
sem <- struct{}{}   // acquire (blocks when 8 are held)
<-sem               // release
```

or the inverse — pre-fill the channel with N tokens and *receive* to
acquire, which is the shape this lesson's `ChannelLimiter` uses because it
makes "no tokens left" a failed non-blocking receive. `select` with a
`default` case turns either direction into a non-blocking `TryAcquire`;
`select` on `ctx.Done()` turns it into a cancellable acquire. What the idiom
cannot express is *weighted* acquisition — a heavy request that should
consume three slots. That is precisely the gap `golang.org/x/sync/semaphore`
fills with `Acquire(ctx, n)`.

## Two refill strategies, two architectures

Both limiter implementations in this lesson enforce the same policy — burst
of N, sustained rate of R per second — but they refill differently, and the
difference is architectural, not cosmetic.

The mutex version refills *continuously*: on every `Allow` call it computes
`elapsed = now - lastRefill` and credits `elapsed * refillRate` fractional
tokens, capped at the maximum. This is precise (a call 3.7ms after the last
one credits exactly 0.0037·R tokens), needs no background goroutine, and
keeps the whole limiter a passive value with no lifecycle. The cost is that
rate arithmetic runs inside the critical section, and blocking acquisition
("wait until a token exists") has to be built by hand — compute the delay
under the lock, sleep outside it.

The channel version refills *discretely*: a goroutine wakes on a
`time.Ticker` and sends one token per tick, dropping the token via
`select`/`default` when the bucket is already full. Granularity is coarser —
one token per 100ms tick caps the sustained rate at 10/s regardless of
demand pattern, and ticks offered to a full bucket are lost rather than
banked. But blocking acquisition falls out naturally (`<-tokens` just
blocks), and cancellable blocking acquisition is a two-case `select`.

The deep consequence of the discrete design: the type now *owns a
goroutine*, and goroutine lifecycle becomes part of its public API contract.
Any constructor that starts a goroutine must ship a `Close` (or `Stop`) that
actually terminates it, that `Close` must be idempotent — `sync.Once` around
`close(stop)` is the standard mechanism, because closing a closed channel
panics — and a test must prove the goroutine really exits. Skip any of the
three and every constructor call in a long-lived server leaks one goroutine
plus one ticker, forever. The mutex limiter has no such obligations; that
asymmetry alone often decides the choice.

## Never block while holding a mutex

The one iron law when extending the mutex limiter with `Wait(ctx)`: compute
the sleep duration *under* the lock, then release the lock *before*
sleeping. Sleeping — or doing I/O, or a channel operation — while holding
the mutex collapses all concurrency: every other caller, including plain
non-blocking `Allow` calls, queues behind your nap. In the worst case it
deadlocks: the condition you are sleeping for may only become true through
another goroutine that needs the lock you are holding. After waking,
re-acquire and re-check, because the world may have changed while you slept.
The channel limiter never faces this problem — `select { case <-tokens: case
<-ctx.Done(): }` holds nothing while blocked — which is the clearest
expression of "blocking acquisition is natural for channels, awkward for
mutexes."

## Failure modes differ by tool, so tests must too

Mutex bugs are torn reads and writes (`tokens += elapsed*rate` from two
goroutines at once), check-then-act gaps (read the token count under one
lock acquisition, spend under another), and forgotten `Unlock`s. The race
detector catches the first class ruthlessly, and `defer mu.Unlock()`
discipline eliminates the third; the second is caught only by a test that
hammers the API from many goroutines and asserts an exact invariant.

Channel bugs are different animals: deadlocks (everyone blocked on a
receive nobody will satisfy), goroutine leaks (the refill goroutine that
never learned to stop), and sends on closed channels (a `Close` racing a
refill tick). The countermeasures are timeouts in tests, goroutine-exit
assertions, and lifecycle tests — call `Close` twice, prove no panic; prove
the background goroutine terminates.

This is why the lesson's contract suite is a module of its own. The exact-N
trick is its centerpiece: with refill disabled (rate 0 for the mutex
version, an interval of `time.Hour` for the channel version), both limiters
must allow *exactly* `burst` operations across any number of goroutines —
not "roughly burst". An approximate assertion ("about 500 allowed") hides
off-by-N races and is simultaneously flaky and weak; an exact assertion plus
a silent race detector together constitute a genuine correctness proof for
concurrent admission logic. Whenever a test wants exact counts, it must
first make the system deterministic by turning refill off; whenever it wants
to observe refill, it either injects a clock or polls with a deadline
instead of assuming scheduler timing.

## The production default: import, don't hand-roll

The hand-rolled limiters in this lesson exist to make the mutex-vs-channel
trade-off visible and testable. In production code you should reach for
`golang.org/x/time/rate` first: `rate.NewLimiter(rate.Limit(r), b)` gives
you `Allow`, a context-aware `Wait`, `Reserve` for pre-booking capacity with
a known delay, `SetLimit`/`SetBurst` for tuning a live limiter, and years of
edge-case fixes in the time arithmetic (monotonic clock handling, burst
overflow, cancellation refunds). Internally, `rate.Limiter` is a mutex
around a float64 token count with continuous refill — structurally the same
design as this lesson's `MutexLimiter`. The standard tool chose the mutex.
That resolves the proverb with evidence rather than dogma: for guarded
shared state, even the library that *could* have been built on channels was
not.

Contention, finally, is empirical. A mutex-guarded counter beats a
channel-serialized one by roughly an order of magnitude because a channel
operation is itself a lock acquisition plus a queue manipulation plus a
scheduler interaction. But the gap between limiter implementations depends
on parallelism, critical-section length, and cache behavior — which is why
the last module measures with `b.RunParallel` across `-cpu` settings instead
of asserting folklore. A senior engineer's decision procedure is: shape the
design by ownership (wiki table), then confirm the hot path with a
benchmark.

## Common Mistakes

Serializing shared-state mutations through a channel-fed manager goroutine.
The pattern `ops := make(chan func())` with a single goroutine applying
each closure to a cache looks proverb-compliant, but the state is still
shared, every access now costs a channel hop and a goroutine wakeup,
throughput collapses under load, and returning errors or values requires a
reply channel per operation. A plain `sync.Mutex` (or `RWMutex` for
read-heavy maps) on the struct is simpler and faster.

Using a mutex-guarded slice as a work queue. Lock, pop the head, unlock —
this reinvents `chan Work` badly: there is no blocking receive so consumers
busy-poll when the queue is empty, there is no close-based completion
signal, and slice re-slicing under a lock is easy to get wrong. Work
distribution is exactly the ownership-transfer shape channels are for.

Leaking the refill or janitor goroutine. A limiter constructor that spawns
a ticker goroutine without exposing `Close` — or whose `Close` panics on
the second call because `close(stop)` is not guarded by `sync.Once` — turns
every construction in a long-lived server into a permanent goroutine plus
ticker leak. Lifecycle is part of the API; test the double-`Close` and test
the goroutine's actual exit.

Shipping concurrent admission code verified only by single-goroutine tests.
Races in `tokens += elapsed * rate` and check-then-act gaps between reading
and spending tokens appear only under contention. The minimum bar is
`go test -race` plus a multi-goroutine storm asserting an exact allowed
count with refill disabled.

Per-client limiter maps without eviction. `map[apiKey]*Limiter` under a
mutex grows monotonically under key churn — or under an attacker cycling
API keys — which is an unbounded memory leak. Every per-client registry
needs `lastSeen` tracking and a TTL janitor that deletes idle entries.

Sleeping or doing I/O while holding the mutex in `Wait(ctx)`. Computing the
refill delay and then sleeping inside the critical section blocks every
other caller for the entire wait. Release the lock before blocking,
`select` on the timer versus `ctx.Done()`, and re-check state after waking.

Forgetting `ctx.Done()` in blocking channel acquires. A bare `<-sem` in a
request path ignores cancellation: canceled requests keep occupying
semaphore slots, and goroutines pile up behind a slow downstream. Always
`select` on `ctx.Done()` alongside the acquire and return `ctx.Err()`.

Asserting approximate counts in limiter tests. "Allowed should be about
500" hides off-by-N races and still flakes. With refill disabled the
correct assertion is *exactly* the burst size.

Assuming the ticker-refill limiter has the same rate semantics as the
continuous one. One token per tick means a 100ms interval caps sustained
throughput at 10/s no matter how demand arrives, and tokens offered while
the bucket is full are dropped, not banked. Burst recovery and sustained
rate genuinely differ from the fractional-refill design; pick deliberately.

Hand-rolling in production what `golang.org/x/time/rate` provides. Custom
limiters lack `Reserve` semantics, `WaitN`, `SetLimit` hot-tuning, and the
library's battle-tested time math. Hand-roll to learn; import to ship.

Next: [01-mutex-token-bucket.md](01-mutex-token-bucket.md)
