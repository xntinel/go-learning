# Exercise 1: Token-Bucket Rate Limiter with an Injected Clock

A rate limiter is where all three `for` forms meet: a counted loop for a bounded
budget, a condition to decide, and an infinite `for {}` inside `Wait` whose only
exits are a token becoming available or the caller's context being cancelled. This
module builds a real in-memory token bucket the production way — the clock is
injected once in the constructor, `refill` is the single place time math lives, and
the whole thing is tested by advancing a fake clock, never by sleeping.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
ratelimit/                          module example.com/ratelimit
  go.mod
  internal/limiter/
    limiter.go                      New, Allow, Wait(ctx), Reserve(n), Tokens; refill is the only time math
    limiter_test.go                 fakeClock-driven suite, -race concurrency test
  cmd/ratelimit-demo/
    main.go                         exercises Allow/Tokens/Wait against an injected, hand-advanced clock
```

- Files: `internal/limiter/limiter.go`, `internal/limiter/limiter_test.go`, `cmd/ratelimit-demo/main.go`.
- Implement: `New(perSecond, burst, clock)`, `Allow() bool`, `Wait(ctx) error` (infinite `for {}` returning on token or ctx cancel), `Reserve(n) (time.Duration, error)`, `Tokens() float64`, and an unexported `refill()`.
- Test: a `fakeClock` whose `Advance` moves virtual time; assert burst-then-block, refill-at-rate, `Wait` returns on a token, `Wait` returns `context.Canceled`, `Reserve` exhaustion and `ErrInvalidN`, and concurrent `Allow` respects burst under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ratelimit/internal/limiter ~/go-exercises/ratelimit/cmd/ratelimit-demo
cd ~/go-exercises/ratelimit
go mod init example.com/ratelimit
```

### Why the clock is injected once, and refill is the only time math

A token bucket holds two pieces of state: a fractional count of `tokens` and the
`lastRefill` timestamp. Every operation that reads the bucket must first bring it
up to date — add the tokens that have regenerated since `lastRefill` — and that is
the *only* place time arithmetic should appear. Concentrating it in one
`refill()` method is what keeps the classic hand-rolled-limiter bug (mutating token
state and computing elapsed time in the same tangled expression) from ever
appearing: every public method is "lock, `refill()`, then decide."

The clock is a `Clock func() time.Time` taken once in `New`. There is deliberately
no `SetClock` setter — a limiter that can swap its clock at runtime has hidden
mutable state that races under concurrent requests. Taking the clock at
construction makes the tests deterministic (a `fakeClock` advanced by exact
amounts) and keeps the limiter race-free.

`Wait` is the infinite-`for` form. It cannot be a counted loop (the number of
waits is unknown) and it cannot be a plain condition loop (it must block on a timer
between checks). So it is `for { refill; if token { return }; wait-on-timer-or-ctx
}`, and its two correct exits are: a token became available (return `nil`) or the
context was cancelled (`Stop` the timer, return `ctx.Err()`). The timer is a
`time.NewTimer` that is stopped on the cancel path — never `time.After` in a loop,
which would leak a timer per iteration.

Create `internal/limiter/limiter.go`:

```go
package limiter

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	// ErrExhausted means the bucket did not have enough tokens for the request.
	ErrExhausted = errors.New("rate limit exhausted")
	// ErrInvalidN means a non-positive token count was requested.
	ErrInvalidN = errors.New("n must be positive")
)

// Clock returns the current time. Injected once so tests can advance it.
type Clock func() time.Time

// Limiter is a concurrency-safe token bucket. Tokens regenerate at refillRate
// per second up to maxTokens; each request spends one or more tokens.
type Limiter struct {
	mu         sync.Mutex
	tokens     float64
	maxTokens  float64
	refillRate float64
	lastRefill time.Time
	clock      Clock
}

// New builds a limiter that regenerates perSecond tokens per second and holds at
// most burst tokens. A nil clock falls back to time.Now.
func New(perSecond float64, burst int, clock Clock) *Limiter {
	if clock == nil {
		clock = time.Now
	}
	if perSecond <= 0 {
		perSecond = 1
	}
	if burst <= 0 {
		burst = 1
	}
	return &Limiter{
		tokens:     float64(burst),
		maxTokens:  float64(burst),
		refillRate: perSecond,
		lastRefill: clock(),
		clock:      clock,
	}
}

// refill is the single place time math lives: it credits the tokens that have
// regenerated since lastRefill, capped at maxTokens. Callers hold l.mu.
func (l *Limiter) refill() {
	now := l.clock()
	elapsed := now.Sub(l.lastRefill)
	if elapsed <= 0 {
		return
	}
	l.tokens += float64(elapsed) / float64(time.Second) * l.refillRate
	if l.tokens > l.maxTokens {
		l.tokens = l.maxTokens
	}
	l.lastRefill = now
}

// Allow spends one token if available and reports whether it did.
func (l *Limiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refill()
	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	return false
}

// Wait blocks until a token is available or ctx is cancelled. Its infinite loop
// exits by returning: nil on a token, ctx.Err() on cancel.
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		l.refill()
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		wait := time.Duration(float64(time.Second) / l.refillRate)
		l.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Reserve spends n tokens if available, returning (0, nil). Otherwise it spends
// nothing and returns how long the caller must wait plus ErrExhausted. A
// non-positive n is rejected with ErrInvalidN.
func (l *Limiter) Reserve(n int) (time.Duration, error) {
	if n <= 0 {
		return 0, ErrInvalidN
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refill()
	needed := float64(n)
	if l.tokens >= needed {
		l.tokens -= needed
		return 0, nil
	}
	missing := needed - l.tokens
	wait := time.Duration(missing / l.refillRate * float64(time.Second))
	return wait, ErrExhausted
}

// Tokens reports the current token count after refilling.
func (l *Limiter) Tokens() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refill()
	return l.tokens
}
```

### The runnable demo

The demo injects a hand-advanced `demoClock` rather than `time.Now`, so the run is
fully reproducible: the exact same token values print on every machine, with no
wall-clock jitter. It fires six requests 250 ms apart at 2 tokens/second with a
burst of 3 — each 250 ms step credits exactly 0.5 tokens, so the values land on
clean halves — then a `Wait` with a short real deadline that times out. Advancing
the clock at the top of each iteration (so the final iteration's refill happens at
the clock's last position) leaves no pending credit for `Wait` to claim, which is
why the trailing `Wait` blocks on its timer until the 250 ms context deadline
fires. A real client would pass the request's own context and a real clock.

Create `cmd/ratelimit-demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/ratelimit/internal/limiter"
)

// demoClock is advanced by hand so the demo prints a reproducible run instead of
// depending on wall-clock jitter.
type demoClock struct{ now time.Time }

func (c *demoClock) Now() time.Time { return c.now }

func main() {
	clock := &demoClock{now: time.Unix(0, 0).UTC()}
	l := limiter.New(2, 3, clock.Now)

	for i := range 6 {
		if i > 0 {
			clock.now = clock.now.Add(250 * time.Millisecond)
		}
		ok := l.Allow()
		fmt.Printf("attempt %d: allowed=%v tokens=%.2f\n", i, ok, l.Tokens())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); err != nil {
		fmt.Printf("wait canceled: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/ratelimit-demo
```

Expected output (deterministic — the demo drives an injected clock in fixed 250 ms
steps, so these values are identical on every machine):

```text
attempt 0: allowed=true tokens=2.00
attempt 1: allowed=true tokens=1.50
attempt 2: allowed=true tokens=1.00
attempt 3: allowed=true tokens=0.50
attempt 4: allowed=true tokens=0.00
attempt 5: allowed=false tokens=0.50
wait canceled: context deadline exceeded
```

Attempt 0 spends one of the three burst tokens, and each subsequent 250 ms step
credits 0.5 more, so the bucket drains at a net 0.5 tokens per attempt: 2.00, 1.50,
1.00, 0.50, 0.00. Attempt 5 finds only 0.50 tokens — less than the one it needs —
so it is denied. The trailing `Wait` then blocks: the clock is frozen at its final
position, so no new tokens accrue, and the 250 ms context deadline fires before a
token becomes available. A real client would pass the request's own context and a
real clock.

### Tests

The fake clock is the whole test tool: the suite advances virtual time by exact
amounts and asserts exact outcomes, never sleeping to wait for a refill.
`TestWaitReturnsWhenTokenAvailable` parks a goroutine inside `Wait`, advances the
clock, and asserts the wait returns `nil` — the deterministic version of "a token
showed up." `TestConcurrentAllowRespectsBurst` is the real `-race` test: twenty
goroutines race on a burst of five, and exactly five must win.

Create `internal/limiter/limiter_test.go`:

```go
package limiter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(0, 0).UTC()}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestAllowConsumesBurstThenBlocks(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := New(10, 3, clock.Now)

	for i := range 3 {
		if !l.Allow() {
			t.Fatalf("Allow #%d should succeed on a fresh burst of 3", i)
		}
	}
	if l.Allow() {
		t.Fatal("fourth Allow should fail when the bucket is empty")
	}
}

func TestAllowRefillsAtRate(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := New(1, 1, clock.Now)

	if !l.Allow() {
		t.Fatal("first Allow should succeed")
	}
	if l.Allow() {
		t.Fatal("second Allow should fail (bucket empty)")
	}
	clock.Advance(500 * time.Millisecond)
	if l.Allow() {
		t.Fatal("Allow after 500ms at 1/s should still fail (need a full second)")
	}
	clock.Advance(500 * time.Millisecond)
	if !l.Allow() {
		t.Fatal("Allow after a full second at 1/s should succeed")
	}
}

func TestWaitReturnsWhenTokenAvailable(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := New(10, 1, clock.Now)
	if !l.Allow() {
		t.Fatal("first Allow should succeed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- l.Wait(ctx)
	}()

	clock.Advance(150 * time.Millisecond)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait() = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after a token became available")
	}
}

func TestWaitReturnsContextErrorOnCancel(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := New(1, 1, clock.Now)
	if !l.Allow() {
		t.Fatal("first Allow should succeed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- l.Wait(ctx)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after cancel")
	}
}

func TestReserveReportsWaitWhenExhausted(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := New(2, 2, clock.Now)

	wait, err := l.Reserve(2)
	if err != nil || wait != 0 {
		t.Fatalf("Reserve(2) on a full bucket = (%v, %v), want (0, nil)", wait, err)
	}

	wait, err = l.Reserve(2)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("Reserve(2) on empty bucket err = %v, want ErrExhausted", err)
	}
	if wait <= 0 {
		t.Fatalf("expected a positive wait, got %v", wait)
	}
}

func TestReserveRejectsInvalidN(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := New(1, 1, clock.Now)

	if _, err := l.Reserve(0); !errors.Is(err, ErrInvalidN) {
		t.Fatalf("Reserve(0) err = %v, want ErrInvalidN", err)
	}
	if _, err := l.Reserve(-1); !errors.Is(err, ErrInvalidN) {
		t.Fatalf("Reserve(-1) err = %v, want ErrInvalidN", err)
	}
}

func TestConcurrentAllowRespectsBurst(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := New(5, 5, clock.Now)

	var allowed int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow() {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&allowed); got != 5 {
		t.Fatalf("allowed = %d, want exactly 5 (the burst)", got)
	}
}

func ExampleLimiter_Allow() {
	clock := newFakeClock()
	l := New(1, 2, clock.Now)
	fmt.Println(l.Allow(), l.Allow(), l.Allow())
	// Output: true true false
}
```

## Review

The limiter is correct when every public method is "lock, `refill()`, then decide"
and `refill()` is the only code that reads the clock or touches `lastRefill`. If
you find yourself computing elapsed time anywhere else, you have reintroduced the
tangle that causes double-spend. The clock is taken once in `New` and never
mutated; there is no setter, because a swappable clock is a data race. `Wait`'s
infinite loop has exactly two exits — a token (`return nil`) and cancellation
(`Stop` the timer, `return ctx.Err()`) — and it uses `time.NewTimer` per wait, not
`time.After`, so nothing leaks. The proof of concurrency safety is
`TestConcurrentAllowRespectsBurst` under `-race`: exactly five of twenty racing
`Allow` calls win, and the race detector stays silent. Run
`go test -count=1 -race ./...` to confirm.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the counted, condition, and infinite forms.
- [context package](https://pkg.go.dev/context) — `Context.Done` and `Context.Err`, the cancel path `Wait` honors.
- [time.NewTimer](https://pkg.go.dev/time#NewTimer) — the cancelable, stoppable timer used inside `Wait`.
- [Token bucket algorithm](https://en.wikipedia.org/wiki/Token_bucket) — the refill-then-spend model this implements.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-exponential-backoff-retry.md](02-exponential-backoff-retry.md)
