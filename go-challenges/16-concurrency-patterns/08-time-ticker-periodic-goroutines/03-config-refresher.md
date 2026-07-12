# Exercise 3: A Periodic Config Refresher with Jitter and an Injectable Clock

Real services keep a hot copy of slowly-changing state — feature flags, a routing table, a signing key, a config blob — and reload it on a schedule in the background so request handlers never block on the source of truth. This exercise builds that component the way a senior engineer would: a generic `Refresher[T]` that loads on a jittered cadence, keeps the last good value when a load fails, stops gracefully by cancelling an in-flight load, and is tested deterministically against an injected fake clock with no real sleeps anywhere.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
clock.go                 Clock, Ticker interfaces; SystemClock (real); FakeClock (test-driven)
jitter.go                FixedJitter, RandomJitter helpers
refresher.go             Refresher[T]: New, OnRefresh, Start, Get, Err, Loads, Errors, Stop
cmd/
  demo/
    main.go              drive three deterministic reloads with a FakeClock
refresher_test.go        initial load, tick refreshes, error keeps last good,
                         graceful stop cancels in-flight load, jitter range, idempotent stop
```

- Files: `clock.go`, `jitter.go`, `refresher.go`, `cmd/demo/main.go`, `refresher_test.go`.
- Implement: the `Clock`/`Ticker` interfaces with a real and a fake implementation, the jitter helpers, and `Refresher[T]` with `New`, `OnRefresh`, `Start`, `Get`, `Err`, `Loads`, `Errors`, and `Stop`.
- Test: deterministic tests drive the refresher with `FakeClock.Tick()` and synchronize on an `OnRefresh` callback, asserting cache population, error handling, and graceful cancellation without a single real sleep.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/08-time-ticker-periodic-goroutines/03-config-refresher/cmd/demo && cd go-solutions/16-concurrency-patterns/08-time-ticker-periodic-goroutines/03-config-refresher
```

### Why an injectable clock, and why jitter

Two design decisions separate a toy refresher from a production one.

The first is testability. Code that calls `time.NewTicker` directly can only be driven forward by sleeping in real wall-clock time, which makes its tests slow and, under load, flaky — a `time.Sleep(120 * time.Millisecond)` that "usually" sees three ticks will occasionally see two on a busy CI box. The fix is to depend on a small `Clock` interface instead of the `time` package. The interface exposes exactly what the refresher needs: a `NewTicker` that returns a `Ticker` (a tick channel plus `Stop` and `Reset`) and a `Now`. Production wires in `SystemClock`, which wraps `time.NewTicker` one-for-one. Tests wire in `FakeClock`, whose `Tick()` method delivers exactly one tick to every live ticker with a blocking send, so a test advances the refresher one reload at a time, deterministically, and observes the effect through an `OnRefresh` callback. No sleeps, no tolerance bands, no flakiness.

The second is jitter. If a thousand instances of this service all reload every thirty seconds and were all started by the same deploy, their ticks are phase-aligned and the config source is hit by a synchronized thundering herd every thirty seconds. Perturbing each instance's period by a small random amount spreads the fleet's requests into an approximately constant rate. A fixed-period ticker cannot do this — its cadence is constant by construction — so after each tick the refresher computes `base + jitter()` and calls `ticker.Reset` with that value, re-randomizing every interval. The jitter source is injected as a `func() time.Duration`: production passes `RandomJitter(maxJitter)`, and a deterministic test passes a fixed function (or `nil`, meaning no jitter) so the schedule is reproducible.

### The graceful-stop and last-good-value contracts

Two correctness properties round out the design. First, a failed load must never poison the cache: if the source is briefly unavailable, `Get` should keep returning the last value that loaded cleanly while the error is recorded separately for observability. So `refresh` stores a new value only on success; on error it bumps an error counter and records the error but leaves the cached value untouched. Second, `Stop` must be graceful: the context handed to the load function is derived from a cancellable context that `Stop` cancels, so a load that is mid-flight when shutdown begins is interrupted rather than left to run to completion against a source the process is about to disconnect from. `Stop` cancels, then waits on a `sync.WaitGroup` for the worker to fully return, making it a real synchronization point.

The initial load is synchronous inside `Start`: the cache is populated before `Start` returns, so the first `Get` after `Start` never sees a zero value. The ticker is created in `Start` before the worker goroutine is launched, which matters for the fake clock — it guarantees the ticker is registered with the clock by the time `Start` returns, so a test can call `Tick()` immediately without racing the goroutine's setup.

Create `clock.go`:

```go
package refresher

import "time"

// Ticker is the subset of *time.Ticker the refresher depends on. Abstracting it
// behind an interface lets tests drive ticks deterministically.
type Ticker interface {
	C() <-chan time.Time
	Stop()
	Reset(d time.Duration)
}

// Clock is the source of time and tickers. Production uses SystemClock; tests
// use FakeClock.
type Clock interface {
	Now() time.Time
	NewTicker(d time.Duration) Ticker
}

// SystemClock returns a Clock backed by the real time package.
func SystemClock() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) NewTicker(d time.Duration) Ticker {
	return &systemTicker{t: time.NewTicker(d)}
}

type systemTicker struct{ t *time.Ticker }

func (s *systemTicker) C() <-chan time.Time   { return s.t.C }
func (s *systemTicker) Stop()                  { s.t.Stop() }
func (s *systemTicker) Reset(d time.Duration)  { s.t.Reset(d) }
```

Create the fake clock in the same file. `Tick` uses a blocking send so that when it returns, the worker has provably received the tick; the test then synchronizes on the `OnRefresh` callback to know the reload completed.

Append to `clock.go`:

```go
import "sync"

// FakeClock is a deterministic Clock for tests. Tick delivers one tick to every
// ticker created from it. It performs no real sleeping.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*fakeTicker
}

// NewFakeClock returns a FakeClock at a fixed origin time.
func NewFakeClock() *FakeClock {
	return &FakeClock{now: time.Unix(0, 0).UTC()}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) NewTicker(d time.Duration) Ticker {
	t := &fakeTicker{c: make(chan time.Time)}
	c.mu.Lock()
	c.tickers = append(c.tickers, t)
	c.mu.Unlock()
	return t
}

// Tick delivers exactly one tick to every ticker created from this clock,
// blocking until each receiver has taken it.
func (c *FakeClock) Tick() {
	c.mu.Lock()
	now := c.now
	ts := make([]*fakeTicker, len(c.tickers))
	copy(ts, c.tickers)
	c.mu.Unlock()
	for _, t := range ts {
		t.c <- now
	}
}

type fakeTicker struct{ c chan time.Time }

func (t *fakeTicker) C() <-chan time.Time  { return t.c }
func (t *fakeTicker) Stop()                {}
func (t *fakeTicker) Reset(time.Duration)  {}
```

Create `jitter.go`:

```go
package refresher

import (
	"math/rand/v2"
	"time"
)

// FixedJitter always returns d. Useful for deterministic schedules in tests.
func FixedJitter(d time.Duration) func() time.Duration {
	return func() time.Duration { return d }
}

// RandomJitter returns a function that yields a uniformly random duration in
// [0, max). math/rand/v2's top-level generator is safe for concurrent use, so
// the returned function needs no lock. A non-positive max yields a no-op.
func RandomJitter(max time.Duration) func() time.Duration {
	if max <= 0 {
		return func() time.Duration { return 0 }
	}
	return func() time.Duration { return rand.N(max) }
}
```

Create `refresher.go`:

```go
package refresher

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Refresher keeps a hot copy of a value of type T, reloading it on a jittered
// cadence in the background. A failed reload keeps the last good value. Stop
// cancels any in-flight load and waits for the worker to exit.
type Refresher[T any] struct {
	clock  Clock
	base   time.Duration
	jitter func() time.Duration
	load   func(context.Context) (T, error)
	notify func()

	mu  sync.RWMutex
	val T
	err error

	loads  atomic.Int64
	errors atomic.Int64

	cancel context.CancelFunc
	once   sync.Once
	wg     sync.WaitGroup
}

// New builds a Refresher that reloads every base + jitter() using load. jitter
// may be nil, meaning a fixed base period.
func New[T any](clock Clock, base time.Duration, jitter func() time.Duration, load func(context.Context) (T, error)) *Refresher[T] {
	return &Refresher[T]{clock: clock, base: base, jitter: jitter, load: load}
}

// OnRefresh registers a callback invoked after every reload attempt completes
// (the value is already stored on success). Call it before Start. It is the
// observability/synchronization hook the deterministic tests use.
func (r *Refresher[T]) OnRefresh(fn func()) { r.notify = fn }

func (r *Refresher[T]) period() time.Duration {
	if r.jitter == nil {
		return r.base
	}
	return r.base + r.jitter()
}

func (r *Refresher[T]) refresh(ctx context.Context) {
	r.loads.Add(1)
	v, err := r.load(ctx)
	if err != nil {
		r.errors.Add(1)
		r.mu.Lock()
		r.err = err
		r.mu.Unlock()
	} else {
		r.mu.Lock()
		r.val = v
		r.err = nil
		r.mu.Unlock()
	}
	if r.notify != nil {
		r.notify()
	}
}

// Start performs an initial synchronous load and then launches the background
// worker. After Start returns, Get reflects the initial load.
func (r *Refresher[T]) Start(ctx context.Context) {
	ctx, r.cancel = context.WithCancel(ctx)
	r.refresh(ctx)
	ticker := r.clock.NewTicker(r.period())
	r.wg.Add(1)
	go r.loop(ctx, ticker)
}

func (r *Refresher[T]) loop(ctx context.Context, ticker Ticker) {
	defer r.wg.Done()
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C():
			r.refresh(ctx)
			ticker.Reset(r.period())
		case <-ctx.Done():
			return
		}
	}
}

// Get returns the last successfully loaded value.
func (r *Refresher[T]) Get() T {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.val
}

// Err returns the error from the most recent reload, or nil if it succeeded.
func (r *Refresher[T]) Err() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.err
}

// Loads returns the total number of reload attempts.
func (r *Refresher[T]) Loads() int64 { return r.loads.Load() }

// Errors returns the number of reload attempts that failed.
func (r *Refresher[T]) Errors() int64 { return r.errors.Load() }

// Stop cancels any in-flight load and blocks until the worker has exited. It is
// safe to call more than once.
func (r *Refresher[T]) Stop() {
	r.once.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
	})
	r.wg.Wait()
}
```

### The runnable demo

The demo wires in a `FakeClock` so the output is fully deterministic: an initial load gives `config-v1`, then each manual `Tick` drives exactly one reload to `config-v2` and `config-v3`. The `OnRefresh` callback signals a buffered channel so `main` can wait for each reload to finish storing before it reads `Get`, which is what keeps the printed values race-free and in order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/config-refresher"
)

func main() {
	clock := refresher.NewFakeClock()

	var version atomic.Int64
	load := func(ctx context.Context) (string, error) {
		return fmt.Sprintf("config-v%d", version.Add(1)), nil
	}

	r := refresher.New[string](clock, time.Second, nil, load)

	reloaded := make(chan struct{}, 8)
	r.OnRefresh(func() { reloaded <- struct{}{} })

	r.Start(context.Background())
	<-reloaded
	fmt.Printf("after start:   %q\n", r.Get())

	clock.Tick()
	<-reloaded
	fmt.Printf("after 1 tick:  %q\n", r.Get())

	clock.Tick()
	<-reloaded
	fmt.Printf("after 2 ticks: %q\n", r.Get())

	r.Stop()
	fmt.Printf("loaded %d times total\n", r.Loads())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after start:   "config-v1"
after 1 tick:  "config-v2"
after 2 ticks: "config-v3"
loaded 3 times total
```

### Tests

Every test drives the refresher with `FakeClock.Tick()` and synchronizes on the `OnRefresh` callback; none of them sleeps. `TestInitialLoadPopulates` asserts the cache holds the first value the instant `Start` returns. `TestTickReloads` advances two ticks and asserts the value and load count track them. `TestErrorKeepsLastGood` makes the second load fail and asserts `Get` still returns the first value while `Err` and `Errors` report the failure, then makes the third succeed and asserts recovery. `TestStopCancelsInflightLoad` blocks a load on its context and asserts `Stop` cancels it and returns. `TestJitterHelpers` checks `FixedJitter` and that `RandomJitter` stays in range. `TestStopIsIdempotent` calls `Stop` twice. `TestSystemClock` exercises the real clock's construction without sleeping.

Create `refresher_test.go`:

```go
package refresher

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestInitialLoadPopulates(t *testing.T) {
	t.Parallel()

	clock := NewFakeClock()
	r := New[string](clock, time.Second, nil, func(context.Context) (string, error) {
		return "hello", nil
	})
	r.Start(context.Background())
	defer r.Stop()

	if got := r.Get(); got != "hello" {
		t.Fatalf("Get() = %q, want %q", got, "hello")
	}
	if r.Loads() != 1 {
		t.Fatalf("Loads() = %d, want 1", r.Loads())
	}
}

func TestTickReloads(t *testing.T) {
	t.Parallel()

	clock := NewFakeClock()
	var n atomic.Int64
	reloaded := make(chan struct{}, 8)
	r := New[int64](clock, time.Second, FixedJitter(0), func(context.Context) (int64, error) {
		return n.Add(1), nil
	})
	r.OnRefresh(func() { reloaded <- struct{}{} })

	r.Start(context.Background())
	defer r.Stop()
	<-reloaded
	if got := r.Get(); got != 1 {
		t.Fatalf("after start Get() = %d, want 1", got)
	}

	clock.Tick()
	<-reloaded
	if got := r.Get(); got != 2 {
		t.Fatalf("after tick Get() = %d, want 2", got)
	}

	clock.Tick()
	<-reloaded
	if got := r.Get(); got != 3 {
		t.Fatalf("after 2 ticks Get() = %d, want 3", got)
	}
	if r.Loads() != 3 {
		t.Fatalf("Loads() = %d, want 3", r.Loads())
	}
}

func TestErrorKeepsLastGood(t *testing.T) {
	t.Parallel()

	clock := NewFakeClock()
	var calls atomic.Int64
	reloaded := make(chan struct{}, 8)
	r := New[string](clock, time.Second, nil, func(context.Context) (string, error) {
		switch calls.Add(1) {
		case 2:
			return "", errors.New("source unavailable")
		default:
			return fmt.Sprintf("v%d", calls.Load()), nil
		}
	})
	r.OnRefresh(func() { reloaded <- struct{}{} })

	r.Start(context.Background())
	defer r.Stop()
	<-reloaded
	if got := r.Get(); got != "v1" {
		t.Fatalf("Get() = %q, want v1", got)
	}

	clock.Tick() // load #2 fails
	<-reloaded
	if got := r.Get(); got != "v1" {
		t.Fatalf("after failed load Get() = %q, want last-good v1", got)
	}
	if r.Err() == nil {
		t.Fatal("Err() = nil after a failed load, want error")
	}
	if r.Errors() != 1 {
		t.Fatalf("Errors() = %d, want 1", r.Errors())
	}

	clock.Tick() // load #3 succeeds
	<-reloaded
	if got := r.Get(); got != "v3" {
		t.Fatalf("after recovery Get() = %q, want v3", got)
	}
	if r.Err() != nil {
		t.Fatalf("Err() = %v after a successful load, want nil", r.Err())
	}
}

func TestStopCancelsInflightLoad(t *testing.T) {
	t.Parallel()

	clock := NewFakeClock()
	var calls atomic.Int64
	entered := make(chan struct{}, 1)
	r := New[int](clock, time.Second, nil, func(ctx context.Context) (int, error) {
		if calls.Add(1) == 1 {
			return 1, nil // initial synchronous load
		}
		entered <- struct{}{}
		<-ctx.Done() // block until Stop cancels
		return 0, ctx.Err()
	})

	r.Start(context.Background())
	clock.Tick() // triggers the blocking load
	<-entered

	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not cancel the in-flight load and return")
	}
	if r.Loads() < 2 {
		t.Fatalf("Loads() = %d, want at least 2", r.Loads())
	}
}

func TestJitterHelpers(t *testing.T) {
	t.Parallel()

	if got := FixedJitter(7 * time.Millisecond)(); got != 7*time.Millisecond {
		t.Fatalf("FixedJitter = %v, want 7ms", got)
	}
	j := RandomJitter(100 * time.Millisecond)
	for i := 0; i < 1000; i++ {
		if d := j(); d < 0 || d >= 100*time.Millisecond {
			t.Fatalf("RandomJitter = %v, want [0,100ms)", d)
		}
	}
	if got := RandomJitter(0)(); got != 0 {
		t.Fatalf("RandomJitter(0) = %v, want 0", got)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	t.Parallel()

	clock := NewFakeClock()
	r := New[int](clock, time.Second, nil, func(context.Context) (int, error) {
		return 0, nil
	})
	r.Start(context.Background())
	r.Stop()
	r.Stop() // must not panic
}

func TestSystemClock(t *testing.T) {
	t.Parallel()

	c := SystemClock()
	if c.Now().IsZero() {
		t.Fatal("SystemClock.Now() is zero")
	}
	tk := c.NewTicker(time.Hour)
	tk.Reset(time.Hour)
	if tk.C() == nil {
		t.Fatal("ticker channel is nil")
	}
	tk.Stop()
}
```

## Review

The refresher is correct when four properties hold. The injected `Clock` makes every test deterministic: `FakeClock.Tick()` delivers one tick with a blocking send, and the test synchronizes on `OnRefresh` rather than sleeping, so reloads are observed exactly, never raced. The last-good-value contract holds because `refresh` writes the cached value only on success and merely records the error otherwise, so a transient source failure never blanks the cache. Graceful stop holds because the load receives a context derived from one `Stop` cancels, so a blocked load is interrupted, and `Stop` then waits on the wait group so it is a true synchronization point. Jitter is real and injectable: the period is recomputed as `base + jitter()` and applied with `ticker.Reset` every cycle, with production passing `RandomJitter` and tests passing a fixed function or `nil`.

Common mistakes for this feature. The first is reaching for `time.NewTicker` directly, which forces sleep-based tests that are slow and flaky; depending on the `Clock` interface is what buys deterministic, sub-millisecond tests. The second is overwriting the cache on a failed load, which turns a brief source outage into a service-wide blanking of config — store on success only. The third is a `Stop` that does not cancel the load's context, leaving a shutdown blocked behind a slow in-flight request; deriving the load context from a cancellable one and cancelling it in `Stop` is what makes shutdown graceful. The fourth is creating the ticker inside the worker goroutine instead of in `Start`, which lets a fast test `Tick()` before the ticker is registered and silently lose the tick.

## Resources

- [`time.Ticker.Reset`](https://pkg.go.dev/time#Ticker.Reset) — re-arming the cadence each cycle, which is how the jittered period is applied.
- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) — the cancellation that makes `Stop` interrupt an in-flight load.
- [`math/rand/v2`](https://pkg.go.dev/math/rand/v2) — the concurrency-safe generator behind `RandomJitter`, including the generic `N`.
- [AWS Architecture Blog: Exponential Backoff and Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — why randomized timing beats synchronized fixed timing across a fleet.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-bounded-periodic.md](02-bounded-periodic.md) | Next: [04-metrics-flush-loop.md](04-metrics-flush-loop.md)
