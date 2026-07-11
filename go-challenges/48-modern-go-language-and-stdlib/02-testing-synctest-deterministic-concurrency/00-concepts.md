# Deterministic Concurrency with testing/synctest — Concepts

Time-dependent concurrent code is the worst thing to test. A cache that expires
entries after a TTL, a background sweeper on a ticker, a retry with backoff — to
test them honestly you either sleep real wall-clock seconds (slow, and still
racy) or you thread a `Clock` interface through every call site just so the test
can inject a fake. Go 1.25 stabilized `testing/synctest` (experimental in 1.24),
which gives a third option: run the code in a "bubble" where the `time` package
uses a fake clock that only advances when every goroutine is durably blocked, and
where `synctest.Wait` lets you deterministically synchronize with background
goroutines. You test the real `time.Now`/`time.Sleep` code with no clock
abstraction, and a two-second test finishes in microseconds. This file is the
conceptual foundation; read it once and you have everything you need to reason
through each of the five independent exercises that follow.

## Concepts

### The bubble and the fake clock

`synctest.Test(t, func(t *testing.T) { ... })` runs the function in an isolated
bubble. Inside it, the `time` package is virtualized: `time.Now` starts at
midnight UTC 2000-01-01, and the clock only advances when every goroutine in the
bubble is *durably blocked*. So `time.Sleep(2 * time.Second)` does not wait two
real seconds — it parks the goroutine, the bubble sees all goroutines blocked,
and it jumps the clock forward to wake it. The test of a TTL expiry runs at full
speed and is deterministic *in time*: there is no real-time slack to flake on.
(One caveat to keep honest: synctest makes *time* deterministic, not goroutine
*scheduling* — the order in which runnable goroutines run is still up to the
scheduler, so your assertions must not depend on that order. `synctest.Wait` is
the tool for pinning down a background goroutine before you read its effect.)

The key payoff for design: you do **not** need a `Clock` interface. The code
under test calls `time.Now()` and `time.Sleep()` directly, exactly as it does in
production; the bubble swaps the clock underneath it. The clock-injection pattern
that exists only to make tests deterministic can be deleted.

### "Durably blocked" is the whole contract

A goroutine is durably blocked when it is blocked and can *only* be unblocked by
another goroutine in the same bubble: blocked on a send/receive on a channel
created inside the bubble, a `sync.WaitGroup`, a `sync.Cond.Wait`, or
`time.Sleep`. Several waits are deliberately **excluded** and do *not* count as
durably blocked: locking a `sync.Mutex` or `sync.RWMutex`, real network or file
I/O, syscalls, and a channel fed by a goroutine started outside the bubble. The
reason for excluding the mutex is subtle but important: a goroutine blocked
acquiring a lock held by code outside the bubble would otherwise wrongly look
durably blocked, so synctest never treats lock acquisition as durable. If any
goroutine is blocked on one of these excluded waits, the bubble's clock cannot
advance and the test deadlocks. That is why the rule is: no real network, no real
I/O, no external processes — use in-memory fakes, and do not expect the clock to
move while a goroutine waits on a lock.

### How time advances, precisely

The clock is frozen whenever *any* goroutine in the bubble is runnable. It jumps
forward — to the next scheduled timer — only once every goroutine is durably
blocked. A consequence worth internalizing: a goroutine spinning in a busy loop
(or blocked on something external) never becomes durably blocked, so the clock
never moves and the test hangs. "Make every goroutine block on something
in-bubble" is not just good style here; it is what lets virtual time work at all.

### synctest.Wait synchronizes with background goroutines

`synctest.Wait()` blocks the current goroutine until every *other* goroutine in
the bubble is durably blocked. This is how you remove the race between "I advanced
the clock" and "the background goroutine reacted to it". After
`time.Sleep(2*time.Second)` wakes the janitor's ticker, calling `synctest.Wait()`
guarantees the janitor has finished its sweep and parked again before your
assertion reads the cache. Without it, the assertion might run before the sweep.

### Timeouts, timers, and select are virtualized too

`time.After`, `time.NewTimer`, `time.Tick`, and the timers behind
`context.WithTimeout`/`WithDeadline` all run on the bubble clock. That means you
can test the *timeout branch* of a `select` deterministically and instantly — no
"sleep slightly longer than the timeout and hope". The one trap: when a `select`
picks its timeout case, the goroutine feeding the other case is still running.
Give it somewhere to go (a buffered channel) so it can finish and exit; otherwise
it blocks forever and the bubble never drains. The deadline exercise shows this
directly.

### Each bubble is isolated

Every call to `synctest.Test` gets its own bubble with its own clock, both
starting at the same instant. Tests do not interfere through shared virtual time,
so the *outer* tests may run with `t.Parallel()`. Only the inner bubble function
is restricted.

### What you must not do inside a bubble

The `*testing.T` handed to your bubble function is restricted: `T.Run`,
`T.Parallel`, and `T.Deadline` must not be called inside it (the *outer* test can
still call `t.Parallel()`). `T.Cleanup` is honored and runs inside the bubble.
`synctest.Test` waits for every goroutine in the bubble to exit before returning,
so a goroutine you start must be able to stop — a leaked goroutine that never
returns turns into a reported deadlock, not a silent leak.

### From the 1.24 experiment to the 1.25 API

In Go 1.24 the package was behind `GOEXPERIMENT=synctest` and the entry point was
`synctest.Run(func())`. Go 1.25 stabilized it (no GOEXPERIMENT) and added
`synctest.Test(t, func(*testing.T))`, which wires the bubble to the test's `T`
(so `T.Context`, `T.Cleanup`, and failures work). Prefer `synctest.Test`; that is
what these exercises use. It requires Go 1.25+, so each exercise pins `go 1.25`.

### When not to reach for synctest

`synctest` is a testing tool, and a narrow one. It does not replace a `Clock`
abstraction when you need to *control* time in production (e.g. a feature that
fast-forwards), only when you need to do so in a test. It requires Go 1.25+, so
code that must build on older toolchains still benefits from clock injection. And
it cannot test a path that does real I/O or calls into a third-party library that
blocks on syscalls — those goroutines never become durably blocked and hang the
bubble; you must supply an in-memory fake (a channel, `net.Pipe`, `fstest.MapFS`)
instead. Use it for the time-and-goroutine logic you own; keep real integration
tests for the real I/O.

## Common Mistakes

### Asserting on a background goroutine without Wait

Wrong: advance the clock with `time.Sleep`, then immediately read the shared
state. The background goroutine may not have reacted yet.

Fix: call `synctest.Wait()` after the sleep. It returns only once every other
bubble goroutine is durably blocked, so the reaction is complete.

### Bounding a load that cannot be cancelled

Wrong: giving a load a timeout but no way to stop it (no context). After the
timeout fires the load goroutine keeps running; under synctest, if it is still
blocked when the bubble's root goroutine exits, virtual time stops and the test
reports a deadlock over the leaked goroutine.

Fix: pass a context to the load and have it watch `ctx.Done`, so it exits at the
same instant the deadline fires; buffer the result channel (size 1) so the loser
of the `select` can still send and exit.

### Doing real I/O inside a bubble

Wrong: opening a real socket or reading an `os.Pipe` inside `synctest.Test`. That
goroutine is blocked on something outside the bubble, so it is never durably
blocked, the clock cannot advance, and the test deadlocks.

Fix: use in-memory fakes (`net.Pipe`, a channel, `fstest.MapFS`). Keep the bubble
self-contained.

### Calling t.Parallel or t.Run inside the bubble

Wrong: `synctest.Test(t, func(t *testing.T) { t.Parallel(); ... })`.

Fix: those calls are forbidden on the bubble's `T`. Mark the *outer* test
parallel instead, before entering the bubble.

### Leaking a background goroutine

Wrong: starting a ticker/sweeper goroutine with no way to stop it. `synctest.Test`
waits for all bubble goroutines to exit and reports a deadlock if one never does.

Fix: drive it with a context and `defer cancel()`, so the goroutine returns when
the bubble function ends.

Next: [01-ttl-cache.md](01-ttl-cache.md)
