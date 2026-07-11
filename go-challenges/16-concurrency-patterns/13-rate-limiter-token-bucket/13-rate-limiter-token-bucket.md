# 13. Rate Limiter: Token Bucket From Stdlib

A token bucket rate limiter caps the long-term rate of events while permitting short bursts. A bucket holds at most `B` tokens; each event consumes one token; tokens refill continuously at rate `R` per second. The algorithm is simple to implement in stdlib: store the last refill time, compute how many tokens have accrued since then, clamp to the burst cap, and decide whether the request can proceed. `golang.org/x/time/rate` uses this same algorithm internally; building it from `sync.Mutex` and `time.Now()` makes the mechanics visible.

```text
ratelimit/
  go.mod
  internal/ratelimit/ratelimit.go
  internal/ratelimit/ratelimit_test.go
  internal/ratelimit/example_test.go
  cmd/ratelimitdemo/main.go
```

The package exposes `Limiter` with `Allow`, `AllowN`, and `WaitN`. Tests pin the burst contract, the steady-state refill, and context cancellation. The implementation uses only stdlib.

## Concepts

### Two Knobs: Rate And Burst

Rate is events per second (a float64). Burst is the maximum number of tokens the bucket holds at once. At start-up and after an idle period, the bucket is full (burst tokens available). Consecutive events drain the bucket; it refills at `rate` tokens per second. A burst of 10 means 10 back-to-back events are allowed; after that, the caller must wait `1/rate` seconds per event.

The refill is continuous, not tick-based. The implementation records the time of the last token grant and computes the fractional token increase on every call:

```
available = min(burst, stored + elapsed * rate)
```

This avoids the "thundering herd at tick boundaries" problem that discrete refill windows create.

### Non-Blocking vs Blocking

`Allow()` consumes one token and returns immediately: `nil` if a token was available, `ErrRateLimited` otherwise. This is the right primitive for "drop if throttled" use cases.

`WaitN(ctx, n)` blocks until `n` tokens are available or the context is cancelled. It computes the exact wait time (`n / rate`), sleeps, and confirms. Context cancellation surfaces as `ErrRateLimited` wrapping `ctx.Err()`, so callers use `errors.Is` to distinguish the two.

### Why The Wait Is Exact, Not Polling

A polling loop (`for { Allow(); sleep(short) }`) wastes CPU and is inaccurate. The correct approach computes the wait analytically: if the bucket has `have` tokens and we need `n`, the wait is `max(0, (n-have)/rate)`. After sleeping, the bucket will have enough tokens (assuming no concurrent drain). The implementation re-checks under the mutex in case a concurrent call consumed tokens during the sleep.

### Sentinel Errors Enable `errors.Is` Chains

`ErrRateLimited` is the package sentinel. `WaitN` wraps it with `fmt.Errorf("%w: %w", ErrRateLimited, ctx.Err())` so callers can assert both:

```go
errors.Is(err, ratelimit.ErrRateLimited) // true
errors.Is(err, context.Canceled)         // true
```

This is the Go 1.20+ multi-wrap pattern. Both unwrap targets are reachable through the chain.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/ratelimit/internal/ratelimit ~/go-exercises/ratelimit/cmd/ratelimitdemo
cd ~/go-exercises/ratelimit
go mod init example.com/ratelimit
```

### Exercise 1: The Token Bucket

Create `internal/ratelimit/ratelimit.go`:

```go
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrRateLimited  = errors.New("rate limited")
	ErrInvalidRate  = errors.New("rate must be positive")
	ErrInvalidBurst = errors.New("burst must be positive")
)

// Limiter is a token bucket rate limiter. It allows up to Burst events
// immediately, then refills at Rate tokens per second. Allow is
// non-blocking; WaitN blocks until tokens are available or ctx is done.
type Limiter struct {
	mu     sync.Mutex
	rate   float64   // tokens per second
	burst  int       // maximum token count
	tokens float64   // current token count (fractional)
	last   time.Time // time of last token update
}

// New returns a Limiter with the given rate (events per second) and burst size.
// Both must be positive.
func New(rate float64, burst int) (*Limiter, error) {
	if rate <= 0 {
		return nil, fmt.Errorf("ratelimit: %w", ErrInvalidRate)
	}
	if burst <= 0 {
		return nil, fmt.Errorf("ratelimit: %w", ErrInvalidBurst)
	}
	return &Limiter{
		rate:   rate,
		burst:  burst,
		tokens: float64(burst),
		last:   time.Now(),
	}, nil
}

// Rate returns the configured rate in tokens per second.
func (l *Limiter) Rate() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rate
}

// Burst returns the configured burst size.
func (l *Limiter) Burst() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.burst
}

// Allow consumes one token and returns nil, or returns ErrRateLimited if no
// token is available. It never blocks.
func (l *Limiter) Allow() error {
	return l.AllowN(1)
}

// AllowN consumes n tokens and returns nil, or returns ErrRateLimited if
// fewer than n tokens are available. It never blocks.
func (l *Limiter) AllowN(n int) error {
	if n <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.advance(time.Now())
	if l.tokens < float64(n) {
		return ErrRateLimited
	}
	l.tokens -= float64(n)
	return nil
}

// WaitN blocks until n tokens are available or ctx is cancelled. On
// cancellation, it returns ErrRateLimited wrapping ctx.Err().
func (l *Limiter) WaitN(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	for {
		l.mu.Lock()
		l.advance(time.Now())
		wait := l.waitDuration(n)
		if wait == 0 {
			l.tokens -= float64(n)
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			return fmt.Errorf("ratelimit: %w: %w", ErrRateLimited, ctx.Err())
		case <-time.After(wait):
		}
	}
}

// advance refills tokens based on elapsed time since last call.
// Must be called with l.mu held.
func (l *Limiter) advance(now time.Time) {
	elapsed := now.Sub(l.last).Seconds()
	l.tokens += elapsed * l.rate
	if l.tokens > float64(l.burst) {
		l.tokens = float64(l.burst)
	}
	l.last = now
}

// waitDuration returns how long to wait for n tokens to become available.
// Must be called with l.mu held.
func (l *Limiter) waitDuration(n int) time.Duration {
	need := float64(n) - l.tokens
	if need <= 0 {
		return 0
	}
	return time.Duration(need/l.rate*float64(time.Second)) + time.Millisecond
}
```

`advance` is the core of the token bucket: it computes the fractional token increase since the last call, adds it to the stored count, and clamps to burst. `waitDuration` computes the exact sleep needed. The `+time.Millisecond` in `waitDuration` adds a small margin to avoid a re-check loop when floating-point rounding leaves the bucket fractionally short.

### Exercise 2: Test The Contract

Create `internal/ratelimit/ratelimit_test.go`:

```go
package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestNewRejectsZeroRate(t *testing.T) {
	t.Parallel()

	_, err := New(0, 5)
	if !errors.Is(err, ErrInvalidRate) {
		t.Fatalf("err = %v, want ErrInvalidRate", err)
	}
}

func TestNewRejectsNegativeRate(t *testing.T) {
	t.Parallel()

	_, err := New(-1, 5)
	if !errors.Is(err, ErrInvalidRate) {
		t.Fatalf("err = %v, want ErrInvalidRate", err)
	}
}

func TestNewRejectsZeroBurst(t *testing.T) {
	t.Parallel()

	_, err := New(1, 0)
	if !errors.Is(err, ErrInvalidBurst) {
		t.Fatalf("err = %v, want ErrInvalidBurst", err)
	}
}

func TestAllowConsumesBurstThenRejects(t *testing.T) {
	t.Parallel()

	l, err := New(1, 3)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := l.Allow(); err != nil {
			t.Fatalf("Allow %d: err = %v, want nil", i, err)
		}
	}

	if err := l.Allow(); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Allow after burst: err = %v, want ErrRateLimited", err)
	}
}

func TestAllowNZeroIsNoOp(t *testing.T) {
	t.Parallel()

	l, err := New(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Exhaust the burst.
	l.Allow()
	// AllowN(0) must not consume tokens or return an error.
	if err := l.AllowN(0); err != nil {
		t.Fatalf("AllowN(0): err = %v, want nil", err)
	}
}

func TestAllowNConsumesExactlyN(t *testing.T) {
	t.Parallel()

	l, err := New(1, 10)
	if err != nil {
		t.Fatal(err)
	}

	if err := l.AllowN(7); err != nil {
		t.Fatalf("AllowN(7): err = %v, want nil", err)
	}
	if err := l.AllowN(4); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("AllowN(4) after 7 used: err = %v, want ErrRateLimited", err)
	}
	if err := l.AllowN(3); err != nil {
		t.Fatalf("AllowN(3) for remaining 3: err = %v, want nil", err)
	}
}

func TestAllowRefillsAtRate(t *testing.T) {
	t.Parallel()

	// 20 tokens/sec = 1 token every 50ms
	l, err := New(20, 1)
	if err != nil {
		t.Fatal(err)
	}

	if err := l.Allow(); err != nil {
		t.Fatalf("first Allow: err = %v, want nil", err)
	}
	if err := l.Allow(); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("immediate second Allow: err = %v, want ErrRateLimited", err)
	}

	time.Sleep(60 * time.Millisecond)

	if err := l.Allow(); err != nil {
		t.Fatalf("Allow after refill: err = %v, want nil", err)
	}
}

func TestWaitNBlocksForTokens(t *testing.T) {
	t.Parallel()

	// 1 token per 30ms
	l, err := New(1.0/0.030, 1)
	if err != nil {
		t.Fatal(err)
	}

	if err := l.Allow(); err != nil {
		t.Fatalf("first Allow: err = %v, want nil", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := l.WaitN(ctx, 1); err != nil {
		t.Fatalf("WaitN: err = %v, want nil", err)
	}
	elapsed := time.Since(start)
	if elapsed < 20*time.Millisecond {
		t.Fatalf("WaitN returned in %s, expected >= 20ms", elapsed)
	}
}

func TestWaitNRespectsContextCancel(t *testing.T) {
	t.Parallel()

	// Rate so slow that WaitN will block for much longer than the cancel delay.
	l, err := New(0.1, 1) // 1 token per 10 seconds
	if err != nil {
		t.Fatal(err)
	}
	l.Allow() // exhaust the token

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- l.WaitN(ctx, 1)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, ErrRateLimited) {
			t.Fatalf("err = %v, want ErrRateLimited", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled in chain", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("WaitN did not return after cancel")
	}
}

func TestAccessors(t *testing.T) {
	t.Parallel()

	l, err := New(10.0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Rate(); got != 10.0 {
		t.Fatalf("Rate() = %v, want 10.0", got)
	}
	if got := l.Burst(); got != 5 {
		t.Fatalf("Burst() = %d, want 5", got)
	}
}

func TestLimiterIsRaceFree(t *testing.T) {
	t.Parallel()

	l, err := New(1000, 100)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Allow()
		}()
	}
	wg.Wait()
}
```

`TestAllowConsumesBurstThenRejects` is the core test: it drains the burst of 3 and asserts the 4th call is rejected. `TestAllowRefillsAtRate` pins the steady-state: after 60ms at 20 tokens/sec, a token has accrued. `TestWaitNRespectsContextCancel` proves the multi-wrap: `errors.Is` reaches both `ErrRateLimited` and `context.Canceled` through the chain.

Your turn: add `TestAllowNLargerThanBurst` that calls `l.AllowN(burst+1)` on a fresh limiter and asserts `ErrRateLimited` is returned (no burst can satisfy a request larger than the bucket size).

### Exercise 3: Example For go doc

Create `internal/ratelimit/example_test.go`:

```go
package ratelimit_test

import (
	"fmt"

	"example.com/ratelimit/internal/ratelimit"
)

func ExampleLimiter_Allow() {
	l, _ := ratelimit.New(1, 2)
	fmt.Println(l.Allow()) // first token
	fmt.Println(l.Allow()) // second token (burst)
	fmt.Println(l.Allow()) // burst exhausted
	// Output:
	// <nil>
	// <nil>
	// rate limited
}
```

### Exercise 4: Runnable Demo

Create `cmd/ratelimitdemo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/ratelimit/internal/ratelimit"
)

func main() {
	// 20 events/sec, burst of 3.
	l, err := ratelimit.New(20, 3)
	if err != nil {
		fmt.Println("init:", err)
		return
	}

	fmt.Printf("rate=%.0f/s burst=%d\n", l.Rate(), l.Burst())

	for i := 0; i < 8; i++ {
		if err := l.Allow(); err != nil {
			if errors.Is(err, ratelimit.ErrRateLimited) {
				fmt.Printf("event %d: rate limited\n", i)
				time.Sleep(60 * time.Millisecond)
				continue
			}
			fmt.Printf("event %d: %v\n", i, err)
			return
		}
		fmt.Printf("event %d: ok\n", i)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	for i := 0; i < 3; i++ {
		if err := l.WaitN(ctx, 1); err != nil {
			fmt.Println("wait:", err)
			return
		}
		fmt.Printf("waited event %d\n", i)
	}
}
```

Run it:

```bash
go run ./cmd/ratelimitdemo
```

The demo fires 8 events at a burst of 3 (events 0-2 succeed, events 3-7 rate limit and sleep), then waits for 3 more tokens.

## Common Mistakes

### Polling Instead Of Sleeping The Exact Duration

Wrong: a loop that calls `Allow()` every millisecond until it returns nil.

What happens: the CPU spins; the rate limiter is taxed far more than the events it limits; the effective rate is blurry because the sleep may land mid-refill.

Fix: compute `waitDuration` analytically and sleep exactly that long. `WaitN` does this; callers should use `WaitN` instead of `for { Allow(); sleep(short) }`.

### Not Clamping Tokens To Burst

Wrong: `tokens += elapsed * rate` without `if tokens > burst { tokens = burst }`.

What happens: after a long idle period the bucket holds thousands of tokens. The first caller drains them all, causing a burst far larger than configured.

Fix: clamp to `burst` on every `advance` call. The lesson's implementation does this.

### Ignoring The Burst Knob

Wrong: `New(10, 1)` for a service that receives 20 requests in a page load.

What happens: 19 of those requests are rate-limited, even though the long-term average of 10/s is fine.

Fix: set burst to the size of the largest realistic burst. The rate controls steady-state; the burst absorbs natural spikes.

### Using Allow When WaitN Is The Right Primitive

Wrong: a loop that calls `Allow()` and `sleep(1/rate)` when it returns false.

What happens: the sleep is a guess; the bucket may have refilled earlier or later; the effective rate drifts.

Fix: use `WaitN(ctx, n)`. The exact wait duration is computed analytically.

## Verification

From `~/go-exercises/ratelimit`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The timing tests (`TestAllowRefillsAtRate`, `TestWaitNBlocksForTokens`) use generous bounds. If they are flaky on a heavily loaded machine, increase the sleep multiplier.

## Summary

- A token bucket holds at most `burst` tokens, refilled continuously at `rate` tokens per second.
- `Allow` is non-blocking; `WaitN` blocks for the exact duration needed and respects context cancellation.
- Clamp the stored token count to `burst` after every refill to prevent idle-period credit explosions.
- Wrap the sentinel `ErrRateLimited` and `ctx.Err()` with `%w` so callers can use `errors.Is` on both.
- `golang.org/x/time/rate` uses the same algorithm; building it from stdlib makes the mechanics visible.

## What's Next

Next: [Circuit Breaker Pattern](../14-circuit-breaker-pattern/14-circuit-breaker-pattern.md).

## Resources

- [Token bucket on Wikipedia](https://en.wikipedia.org/wiki/Token_bucket)
- [pkg.go.dev: golang.org/x/time/rate source](https://cs.opensource.google/go/x/time/+/refs/tags/v0.5.0:rate/rate.go)
- [Go Blog: Rate limiting](https://go.dev/blog/rate-limiting)
- [errors package](https://pkg.go.dev/errors)
