# Testing Time-Dependent Code: Injected Clocks and testing/synctest — Concepts

Time is the single most common source of flaky backend tests. Retries, rate
limiters, caches, circuit breakers, heartbeats, session validation, request
timeouts — every one of these is *defined* by elapsed time, and a test that
literally waits for that time to pass is slow, non-deterministic, and usually
wrong under CI load. A `time.Sleep(2 * time.Second)` in a test does not prove a
TTL expires after two seconds; it proves your machine was not so overloaded that
the goroutine woke late, and it costs two seconds every run. Senior engineers
remove wall-clock time from the system under test entirely, using one of two
production techniques this lesson teaches side by side. Read this file once and
you have the model for every exercise that follows; each exercise is an
independent artifact a backend engineer owns in production.

## Concepts

### Two techniques, one goal: make elapsed time an explicit, controllable input

Both techniques below exist to turn "how much time has passed" from an
uncontrollable ambient fact into an input a test can set precisely.

Technique A — **dependency injection of a narrow time abstraction.** The code
takes a small `Clock` interface (`Now() time.Time`, maybe `After(d)
<-chan time.Time`), or even a single `now func() time.Time`, and calls *that*
instead of `time.Now()`. Production wires in a real clock; the test wires in a
fake it advances by hand. This works on every Go version, keeps time an explicit
and mockable input, and — crucially — is the right design when *callers*
legitimately need to control time (a scheduler, a token validator whose "now" is
a security-relevant input).

Technique B — **`testing/synctest`.** Stabilized in Go 1.25 (experimental in
1.24), `synctest.Test(t, func(t *testing.T){ ... })` runs your function inside a
*bubble* whose `time` package is virtualized. The code under test calls the real
`time.Sleep`, `time.After`, `time.NewTicker`, and `context.WithTimeout` exactly
as it does in production — no interface, no injection — and the bubble's fake
clock advances underneath it. A 30-second timeout fires in microseconds, in
correct causal order. This is the right tool when the code legitimately relies on
stdlib timers and goroutines and you want to test it *as written* rather than
abstract the timers away behind a seam.

The recurring senior judgment is choosing between them. Inject a `Clock` when
time is a domain input callers control (schedulers, JWT `now`, business
deadlines). Reach for `synctest` when the code correctly uses stdlib
timers/goroutines and you would otherwise be adding an abstraction that exists
only to satisfy the test.

### Keep the injected interface minimal (interface segregation)

A `Clock` with twenty methods forces a fake with twenty methods, and every test
that constructs the fake pays for all of them. Depend on the smallest surface the
unit actually calls. Most code needs only `Now()`; a scheduler that waits needs
`Now()` plus `After()`; a token-bucket limiter that computes elapsed time needs
`Now()` alone. A single `now func() time.Time` field is often enough and is the
lightest fake of all. The fat clock is a real cost, not a hypothetical one — it
shows up as noise in every test file.

### FakeClock mechanics

A fake clock is a struct holding a settable current instant. `Now()` returns it,
`Advance(d)` moves it forward, and (if the interface needs it) `After(d)` returns
a pre-loaded one-buffered channel carrying `now.Add(d)`. That is the whole
implementation. It is deterministic and O(1) with zero real waiting: the test
says "pretend three hours passed" and three hours pass, instantly and exactly. A
small subtlety worth building in from the start: if a test advances the clock
from one goroutine while the system under test reads it from another, guard the
stored instant with a mutex so the race detector stays quiet.

### The synctest bubble and its fake clock, precisely

Inside a bubble the clock starts at `2000-01-01 00:00:00 UTC` and only advances
when **every** goroutine in the bubble is *durably blocked*. So a
`time.Sleep(2 * time.Second)` parks the goroutine; once the whole bubble is
blocked, the clock jumps forward to the next scheduled timer and wakes it. This
makes `time.Sleep`, `time.After`, `time.NewTicker`, and the timers behind
`context.WithTimeout`/`WithDeadline` all fire instantly yet in the same causal
order they would at real speed. `synctest.Test` waits for every goroutine in the
bubble to exit before returning, so a goroutine you start must be stoppable — a
leak becomes a reported deadlock, not a silent leak.

### "Durably blocked" is the whole contract

A goroutine is durably blocked when it can *only* be unblocked by another
goroutine in the same bubble: a send/receive or `select` on a bubble channel,
`sync.Cond.Wait`, `sync.WaitGroup.Wait` (with the `Add` done inside the bubble),
and `time.Sleep`. Several waits are deliberately **excluded** and do not count:
contending on a `sync.Mutex`/`RWMutex`, real network or file I/O, and syscalls.
The reason mutex acquisition is excluded is subtle: a goroutine waiting on a lock
held by code *outside* the bubble would otherwise look durably blocked, so
synctest never treats lock acquisition as durable. The practical rule: no real
I/O and no cross-bubble blocking inside a bubble, or the clock stalls and the
test deadlocks. Acquiring an *uncontended* mutex briefly is fine — it does not
block — so ordinary mutex-guarded state works; it is *waiting* on a contended or
external lock that breaks the model.

### synctest.Wait is your race-free barrier

`synctest.Wait()` blocks the calling goroutine until every *other* goroutine in
the bubble is durably blocked. This removes the race between "I advanced the
clock" and "the background goroutine reacted." After a `time.Sleep` fires a
worker's ticker, calling `synctest.Wait()` guarantees the worker finished its
flush and parked again before your assertion reads the result. Only one goroutine
may call `Wait` at a time, and it must be called from inside the bubble.

### Go 1.23+ timer semantics that matter for tests

Since Go 1.23, `Timer.C` and `Ticker.C` are effectively unbuffered, and an
unreferenced, unstopped timer or ticker is now GC-reclaimable — so `time.After`
in a `select` no longer leaks the underlying timer, and you can use it in a loop
without the old cleanup ritual. You still call `Ticker.Stop()` to end a loop and
`Timer.Stop()`/`Timer.Reset()` for debouncing. A timer created with
`time.AfterFunc` has no channel, so `Reset` on it needs no drain — that is
exactly what makes `AfterFunc` the clean primitive for a debouncer.

### Injecting now for validation logic

Stateless validators — JWT `exp`/`nbf`, session TTL, lease deadlines — should
take `now` as a parameter (a `time.Time` or a `Clock`) rather than calling
`time.Now()` internally. That lets a test exercise the not-yet-valid, expired,
and within-skew-leeway boundaries at exact instants instead of racing the wall
clock. Clock-skew leeway matters in real distributed systems: two services'
clocks drift by seconds, so a token that is technically one second past `exp` on
the validating node may be perfectly valid on the issuing node. Accepting within
a small leeway of both edges is the standard tolerance, and it is only testable
precisely when `now` is an input.

### Context deadlines are time-dependent code too

`context.WithTimeout`/`WithDeadline` schedule against the runtime clock, which
`synctest` virtualizes. A 30-second request timeout can therefore be tested in
microseconds under a bubble, both the timely-success path and the
`context.DeadlineExceeded` path. Under injection you instead pass a
clock-derived deadline. Either way, a request-timeout test should never actually
wait 30 seconds.

### Edge-boundary discipline

Expiry, refill, and deadline logic hinges on `<` versus `<=`. Controlled time
lets you assert the *exact* tick where an entry expires or a token is granted.
Test at `t = TTL-1ns` (must still be live), `t = TTL` (the boundary — this is
where an off-by-one hides), and `t = TTL+1ns` (definitely gone). "Wait until
sometime later and check it is gone" lets a `<` vs `<=` bug survive forever;
pinning the boundary instant kills it.

### Determinism and parallelism

FakeClock-based tests are `t.Parallel`-safe and fully reproducible — each has its
own clock. `synctest` bubbles are different: the `*testing.T` handed to the
bubble function must **not** call `t.Parallel`, `t.Run`, or `t.Deadline` (the
*outer* test may still call `t.Parallel` before entering the bubble). Each bubble
is its own isolated clock, so structure bubble tests flat and self-contained.

## Common Mistakes

### Sleeping to "wait for" a timer or goroutine

Wrong: `time.Sleep(100 * time.Millisecond); checkState()` to give a background
timer time to fire. Slow, flaky under CI load, and it proves nothing precise.

Fix: advance a `FakeClock` and assert, or run the code inside a `synctest`
bubble and let the virtual clock advance while `synctest.Wait` synchronizes.

### Calling time.Now() deep in production code with no seam

Wrong: `if time.Now().After(deadline) { ... }` buried inside a handler, leaving
no way for a test to control the clock.

Fix: take a `Clock` or `now func() time.Time` parameter, or run the code under
`synctest`. Time must be an input, not an ambient fact.

### Bloating the Clock interface

Wrong: a `Clock` with `Now`, `After`, `Tick`, `NewTimer`, `NewTicker`, `Since`,
`Until`, and more, so every fake is a chore and every test is noisy.

Fix: keep it to the one or two methods the unit actually calls.

### Assuming synctest advances time during real I/O or a held lock

Wrong: doing a real network read or blocking on an externally-held mutex inside a
bubble and expecting the clock to move. Those are not durably blocking, so the
clock stalls and the test hangs.

Fix: keep real I/O out of the bubble; stub it with in-bubble channels, `net.Pipe`,
or `fstest.MapFS`.

### Reading shared state right after starting a goroutine, with no barrier

Wrong: `go worker(); if flushed != want { ... }` — the worker may not have run
yet, so the read races the write.

Fix: `synctest.Wait()` until the worker is durably blocked, then assert.

### Leaking a ticker/timer goroutine

Wrong: starting a ticker loop with no `Ticker.Stop()` and no cancellation, or
calling `Timer.Reset()` on an already-fired channel timer without draining `C`,
leaving a stale value.

Fix: drive the loop with a context and `defer ticker.Stop()`; for a debouncer use
`time.AfterFunc` (no channel to drain). Under `synctest`, a leaked goroutine is
caught as a deadlock.

### Testing only "after enough time" instead of at the boundary

Wrong: advance well past the TTL and check the entry is gone, so a `<` vs `<=`
off-by-one at the exact expiry instant survives.

Fix: assert at `TTL-1ns`, `TTL`, and `TTL+1ns`.

### Granting on the failure/edge path

Wrong: a rate limiter that grants a token at exactly the empty-bucket instant, or
a breaker that lets two probes through in half-open.

Fix: pin the boundary contract with an explicit test at the edge instant and at
the state transition.

### Misusing the bubble's testing.T

Wrong: calling `t.Parallel()`, `t.Run()`, or `t.Deadline()` on the `T` inside a
`synctest.Test` function, or calling `synctest.Wait()` outside a bubble.

Fix: mark the *outer* test parallel before entering the bubble; keep bubble tests
flat and self-contained.

Next: [01-injectable-clock-scheduler.md](01-injectable-clock-scheduler.md)
