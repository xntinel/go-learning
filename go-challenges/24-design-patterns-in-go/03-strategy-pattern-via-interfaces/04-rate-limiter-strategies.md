# Exercise 4: A Rate-Limiting Service with Pluggable Algorithms

A rate limiter answers one question — may this request proceed right now? — but production systems answer it with several different algorithms (token bucket, fixed window, sliding window) depending on the traffic shape they must control. This exercise builds a limiting service where the algorithm is one `Limiter` interface, the concrete strategy is chosen once from configuration, and the hot path that decides each request never names a concrete type. Because a limiter is hit by many goroutines at once, every strategy is internally synchronized and the test suite proves it under the race detector.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
ratelimit.go         Limiter interface; TokenBucket, FixedWindow, SlidingWindow;
                     Config and New (config-driven selection)
cmd/
  demo/
    main.go          build each strategy from config, fire a burst, print verdicts
ratelimit_test.go    per-algorithm behavior, config selection, unknown-strategy
                     error, and a -race concurrent Allow() test
```

- Files: `ratelimit.go`, `cmd/demo/main.go`, `ratelimit_test.go`.
- Implement: the `Limiter` interface (`Allow` + `Name`), three concrete limiters, and a `New(cfg, now)` factory that selects one by `cfg.Strategy`.
- Test: token-bucket burst-then-refill, fixed-window reset, sliding-window roll-off, config selection, an unknown strategy erroring, and a concurrent `Allow()` run under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/03-strategy-pattern-via-interfaces/04-rate-limiter-strategies/cmd/demo && cd go-solutions/24-design-patterns-in-go/03-strategy-pattern-via-interfaces/04-rate-limiter-strategies
```

### One interface, three time models

Every limiting algorithm reduces to the same contract: a request arrives, the limiter says yes or no, and a yes consumes whatever capacity that request costs. That contract is the `Limiter` interface — two methods, `Allow` and `Name` — and it is the only thing a caller (an HTTP middleware, a queue consumer) ever depends on:

```go
type Limiter interface {
	Allow() bool
	Name() string
}
```

What differs between the strategies is the *model of time* behind `Allow`, and the differences matter operationally:

- **Token bucket** holds a reservoir of tokens that refills at a steady rate up to a cap. Each request spends one token; an empty bucket denies. It permits short bursts (drain the whole bucket at once) while bounding the long-run average to the refill rate. This is what `golang.org/x/time/rate` implements, and it is the right default when occasional bursts are acceptable.
- **Fixed window** counts requests inside a clock-aligned window (say, 100 per second) and resets the counter when the window rolls over. It is trivial to implement and reason about, but it has a known flaw: a caller can send the full quota at the end of one window and the full quota at the start of the next, briefly doubling the intended rate across the boundary.
- **Sliding window** keeps the timestamps of recent requests and admits a new one only if fewer than the limit fall inside the trailing window measured from *now*. It has no boundary artifact — the window moves continuously with each call — at the cost of remembering individual events.

The key design decision is that all three take their current time from an injected `now func() time.Time` rather than calling `time.Now` directly. Time is the input that makes a limiter's behavior hard to test; injecting it lets the demo and the tests advance a clock by exact amounts and assert exact outcomes, while production passes `nil` and gets the real `time.Now`. This is the same dependency-injection move you would make for any non-deterministic source.

### Selection happens once, at construction

Choosing which algorithm to run is a configuration decision, not a per-request one. `New` takes a `Config` and returns the matching `Limiter`; the `switch` on `cfg.Strategy` lives here, runs once at startup, and is the *only* place a concrete limiter type is named. After construction the system holds a `Limiter` and calls `Allow` — no branch on strategy in the request path. This is the deliberate boundary of the strategy pattern: constructing different concrete types genuinely requires a switch or a registry somewhere, so you confine that knowledge to the factory and keep the hot path strategy-blind. An unknown strategy string returns an error that names the bad value and the valid ones, so a misconfigured deployment fails loudly at boot instead of silently admitting every request.

### Concurrency is part of the contract

A rate limiter exists to be shared: one limiter instance guards an endpoint that many goroutines serve simultaneously. That makes `Allow` a concurrent mutator of shared state — the token count, the window counter, the event log — so each strategy carries a `sync.Mutex` and takes it for the whole read-modify-write of every `Allow`. Without the lock, two goroutines could both read "1 token left," both decrement, and both proceed, admitting two requests against a one-token budget; the race detector would also flag the unsynchronized access. The test suite includes a 500-goroutine `Allow()` storm against a 100-token bucket and asserts that *exactly* 100 are admitted, run under `-race` so an accidental unlocked path fails the build.

Create `ratelimit.go`:

```go
package ratelimit

import (
	"fmt"
	"sync"
	"time"
)

// Limiter is the strategy contract shared by every rate-limiting algorithm:
// Allow answers whether a request may proceed now (and consumes capacity on a
// yes), and Name identifies the active algorithm for logs and metrics.
type Limiter interface {
	Allow() bool
	Name() string
}

// TokenBucket refills tokens at a steady rate up to a capacity; each Allow
// spends one token. It permits bursts up to the capacity while bounding the
// long-run rate to the refill rate.
type TokenBucket struct {
	mu       sync.Mutex
	capacity float64
	refill   float64 // tokens added per second
	tokens   float64
	last     time.Time
	now      func() time.Time
}

// NewTokenBucket starts full at capacity and refills refillPerSec tokens each
// second. A nil now defaults to time.Now.
func NewTokenBucket(capacity, refillPerSec float64, now func() time.Time) *TokenBucket {
	if now == nil {
		now = time.Now
	}
	return &TokenBucket{
		capacity: capacity,
		refill:   refillPerSec,
		tokens:   capacity,
		last:     now(),
		now:      now,
	}
}

func (b *TokenBucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	t := b.now()
	if elapsed := t.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * b.refill
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = t
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func (b *TokenBucket) Name() string { return "token-bucket" }

// FixedWindow admits up to limit requests per fixed window and resets the count
// when the window elapses. Simple, but it can admit up to 2*limit across a
// window boundary.
type FixedWindow struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	start  time.Time
	count  int
	now    func() time.Time
}

// NewFixedWindow admits limit requests per window. A nil now defaults to
// time.Now.
func NewFixedWindow(limit int, window time.Duration, now func() time.Time) *FixedWindow {
	if now == nil {
		now = time.Now
	}
	return &FixedWindow{limit: limit, window: window, start: now(), now: now}
}

func (w *FixedWindow) Allow() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	t := w.now()
	if t.Sub(w.start) >= w.window {
		w.start = t
		w.count = 0
	}
	if w.count < w.limit {
		w.count++
		return true
	}
	return false
}

func (w *FixedWindow) Name() string { return "fixed-window" }

// SlidingWindow remembers recent request times and admits a request only if
// fewer than limit fall inside the trailing window measured from now. It has no
// boundary artifact, at the cost of storing individual events.
type SlidingWindow struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	events []time.Time
	now    func() time.Time
}

// NewSlidingWindow admits limit requests per trailing window. A nil now
// defaults to time.Now.
func NewSlidingWindow(limit int, window time.Duration, now func() time.Time) *SlidingWindow {
	if now == nil {
		now = time.Now
	}
	return &SlidingWindow{limit: limit, window: window, now: now}
}

func (w *SlidingWindow) Allow() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	t := w.now()
	cutoff := t.Add(-w.window)

	// Drop events that have aged out of the trailing window. The in-place
	// filter reuses the backing array since we only write at or behind the
	// read index.
	kept := w.events[:0]
	for _, e := range w.events {
		if e.After(cutoff) {
			kept = append(kept, e)
		}
	}
	w.events = kept

	if len(w.events) < w.limit {
		w.events = append(w.events, t)
		return true
	}
	return false
}

func (w *SlidingWindow) Name() string { return "sliding-window" }

// Config selects and parameterizes a limiting strategy. Strategy is one of
// "token-bucket", "fixed-window", or "sliding-window"; the remaining fields
// apply to the chosen strategy.
type Config struct {
	Strategy string
	Limit    int           // window strategies: requests per window
	Window   time.Duration // window strategies: window length
	Capacity float64       // token bucket: maximum tokens
	Refill   float64       // token bucket: tokens added per second
}

// New builds the Limiter named by cfg.Strategy. Selection happens here, once,
// from configuration; everything downstream depends only on the Limiter
// interface. An unknown strategy is an error, not a silent allow-all.
func New(cfg Config, now func() time.Time) (Limiter, error) {
	switch cfg.Strategy {
	case "token-bucket":
		return NewTokenBucket(cfg.Capacity, cfg.Refill, now), nil
	case "fixed-window":
		return NewFixedWindow(cfg.Limit, cfg.Window, now), nil
	case "sliding-window":
		return NewSlidingWindow(cfg.Limit, cfg.Window, now), nil
	default:
		return nil, fmt.Errorf("ratelimit: unknown strategy %q (have token-bucket, fixed-window, sliding-window)", cfg.Strategy)
	}
}
```

### The runnable demo

The demo builds each strategy from a `Config`, then fires five requests back-to-back against a freshly built limiter on a frozen clock. Each strategy is provisioned to admit three, so the burst pattern is identical — `[true true true false false]` — which is the point: the caller wrote one loop against `Limiter` and the algorithm underneath is interchangeable. A final call with a bogus strategy name shows the factory rejecting misconfiguration.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ratelimit"
)

// clock is a fixed time source so the demo's output is deterministic and
// independent of wall-clock timing.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func main() {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	configs := []ratelimit.Config{
		{Strategy: "token-bucket", Capacity: 3, Refill: 1},
		{Strategy: "fixed-window", Limit: 3, Window: time.Second},
		{Strategy: "sliding-window", Limit: 3, Window: time.Second},
	}
	for _, cfg := range configs {
		clk := &clock{t: base}
		lim, err := ratelimit.New(cfg, clk.now)
		if err != nil {
			fmt.Println("config error:", err)
			continue
		}
		// Five requests against a limiter provisioned for three.
		verdicts := make([]bool, 0, 5)
		for i := 0; i < 5; i++ {
			verdicts = append(verdicts, lim.Allow())
		}
		fmt.Printf("%-15s burst: %v\n", lim.Name(), verdicts)
	}

	// An unknown strategy fails loudly instead of silently allowing traffic.
	if _, err := ratelimit.New(ratelimit.Config{Strategy: "leaky-bucket"}, nil); err != nil {
		fmt.Println()
		fmt.Println(err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
token-bucket    burst: [true true true false false]
fixed-window    burst: [true true true false false]
sliding-window  burst: [true true true false false]

ratelimit: unknown strategy "leaky-bucket" (have token-bucket, fixed-window, sliding-window)
```

### Tests

The tests pin each algorithm's distinct time behavior on a controlled clock, confirm the factory selects by config and rejects an unknown strategy, and prove the token bucket is safe under concurrent `Allow`. The clock is mutex-guarded so even the concurrent test reads time without a data race. `TestTokenBucket_ConcurrentAllowIsRaceFree` is the load-bearing one: 500 goroutines race for 100 tokens and exactly 100 must win, verified under `-race`.

Create `ratelimit_test.go`:

```go
package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testClock is a race-safe manually advanced clock for deterministic tests.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock(t time.Time) *testClock { return &testClock{t: t} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestTokenBucket_BurstThenRefill(t *testing.T) {
	t.Parallel()

	clk := newTestClock(base)
	b := NewTokenBucket(3, 1, clk.Now) // 3 tokens, +1 per second

	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("burst request %d denied, want allowed", i)
		}
	}
	if b.Allow() {
		t.Fatal("4th request allowed, want denied (bucket empty)")
	}

	clk.Advance(2 * time.Second) // refill 2 tokens
	if !b.Allow() || !b.Allow() {
		t.Fatal("after 2s refill, want 2 allowed")
	}
	if b.Allow() {
		t.Fatal("3rd after refill allowed, want denied")
	}
}

func TestFixedWindow_ResetsAtBoundary(t *testing.T) {
	t.Parallel()

	clk := newTestClock(base)
	w := NewFixedWindow(3, time.Second, clk.Now)

	for i := 0; i < 3; i++ {
		if !w.Allow() {
			t.Fatalf("request %d denied, want allowed", i)
		}
	}
	if w.Allow() {
		t.Fatal("over-limit request allowed, want denied")
	}

	clk.Advance(time.Second) // window rolls over
	if !w.Allow() {
		t.Fatal("first request of new window denied, want allowed")
	}
}

func TestSlidingWindow_RollsContinuously(t *testing.T) {
	t.Parallel()

	clk := newTestClock(base)
	w := NewSlidingWindow(3, time.Second, clk.Now)

	for i := 0; i < 3; i++ {
		if !w.Allow() {
			t.Fatalf("request %d denied, want allowed", i)
		}
	}
	if w.Allow() {
		t.Fatal("over-limit request allowed, want denied")
	}

	clk.Advance(500 * time.Millisecond) // events still inside window
	if w.Allow() {
		t.Fatal("request inside window allowed, want denied")
	}

	clk.Advance(600 * time.Millisecond) // total 1.1s: original events expired
	if !w.Allow() {
		t.Fatal("request after events aged out denied, want allowed")
	}
}

func TestNew_SelectsByConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cfg  Config
		want string
	}{
		{Config{Strategy: "token-bucket", Capacity: 1, Refill: 1}, "token-bucket"},
		{Config{Strategy: "fixed-window", Limit: 1, Window: time.Second}, "fixed-window"},
		{Config{Strategy: "sliding-window", Limit: 1, Window: time.Second}, "sliding-window"},
	}
	for _, tc := range cases {
		lim, err := New(tc.cfg, nil)
		if err != nil {
			t.Fatalf("New(%q): unexpected error %v", tc.cfg.Strategy, err)
		}
		if lim.Name() != tc.want {
			t.Errorf("New(%q).Name() = %q, want %q", tc.cfg.Strategy, lim.Name(), tc.want)
		}
	}
}

func TestNew_UnknownStrategyErrors(t *testing.T) {
	t.Parallel()

	lim, err := New(Config{Strategy: "nope"}, nil)
	if err == nil {
		t.Fatal("unknown strategy: want error, got nil")
	}
	if lim != nil {
		t.Errorf("unknown strategy: want nil limiter, got %v", lim)
	}
}

func TestTokenBucket_ConcurrentAllowIsRaceFree(t *testing.T) {
	t.Parallel()

	const capacity = 100
	clk := newTestClock(base) // frozen: no refill during the storm
	b := NewTokenBucket(capacity, 0, clk.Now)

	const goroutines = 500
	var allowed int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if b.Allow() {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&allowed); got != capacity {
		t.Errorf("allowed = %d, want exactly %d", got, capacity)
	}
}
```

## Review

The service is correct when the algorithm is interchangeable behind `Limiter` and every strategy is safe to share. Confirm the request path holds only a `Limiter` and calls `Allow` — no `switch` on strategy survives past `New`, which is where the one allowed switch lives. Confirm `New` returns a diagnostic error for an unknown strategy rather than a usable allow-all limiter; a silent default here would disable rate limiting for a whole deployment because of a typo. Confirm each `Allow` takes the mutex around the entire read-modify-write: the token bucket's "read tokens, refill, decrement" and the windows' "expire, count, increment" must be atomic with respect to other goroutines, which is exactly what the 500-goroutine test verifies under `-race`.

Common mistakes for this feature. The first is calling `time.Now` inside the strategies instead of taking an injected clock — it makes the behavior untestable and forces flaky `time.Sleep`-based tests. The second is locking too little: guarding only the decrement but not the refill read leaves a check-then-act window where two goroutines both see the last token. The third is the fixed-window boundary trap presented as a feature — it is a real limitation, so a deployment that cannot tolerate a 2x burst across the boundary should choose the sliding window, which is why all three ship behind one interface and the choice is config-driven. The fourth is a sliding window that never trims its event slice; without dropping aged-out timestamps the slice grows without bound and the count is wrong.

## Resources

- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) — the standard extended-library token-bucket limiter, the production-grade version of the `TokenBucket` here.
- [Token bucket (Wikipedia)](https://en.wikipedia.org/wiki/Token_bucket) — the algorithm behind burst-tolerant, average-bounded limiting.
- [How we built rate limiting (Cloudflare)](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) — why fixed windows leak at the boundary and how a sliding window fixes it at scale.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock each strategy uses to make `Allow` safe for concurrent callers.

---

Back to [03-strategy-registry.md](03-strategy-registry.md) | Next: [05-payment-routing-service.md](05-payment-routing-service.md)
