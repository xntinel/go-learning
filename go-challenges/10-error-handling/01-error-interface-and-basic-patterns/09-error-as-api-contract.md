# Exercise 9: A Token-Bucket Limiter Whose Returned Errors Are Its Contract

The set of errors an exported function can return is part of its API, as binding
as its parameter types. Callers branch on `ErrRateLimited`, `ErrNotFound`,
`ErrCacheMiss`; widen or change that set and you break them with no compile error.
This exercise builds a token-bucket `RateLimiter` whose `Allow` returns exactly one
documented sentinel, and a contract test that pins that promise.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
ratelimit/                   independent module: example.com/ratelimit
  go.mod                     go 1.26
  limiter.go                 ErrRateLimited; RateLimiter with injected now; Allow(key)
  cmd/
    demo/
      main.go                runnable demo: exhaust a bucket, refill, allow again
  limiter_test.go            allow/deny + refill after clock advance + contract test
```

- Files: `limiter.go`, `cmd/demo/main.go`, `limiter_test.go`.
- Implement: a `RateLimiter` with `Allow(key string) error` returning a documented `ErrRateLimited` when the per-key bucket is empty and nil when a token is granted, with the clock injected as a `now func() time.Time`.
- Test: N allowed calls return nil then the next returns `errors.Is(err, ErrRateLimited)`; a refill after a simulated clock advance restores capacity; a contract test asserting the only non-nil error `Allow` ever returns is `ErrRateLimited`.
- Verify: `go test -count=1 -race ./...`

### The returned error set is a versioned promise

`Allow` has a tiny surface — it returns `nil` or `ErrRateLimited` — and that
tininess is the point. A caller writes `if errors.Is(err, ErrRateLimited) { return http.StatusTooManyRequests }`
and treats anything else as a bug. That branch is only safe if the method's error
contract is stable and documented: `Allow` returns `ErrRateLimited` when the bucket
is empty, and no other error. If a later version started returning, say, an
`ErrUnknownKey`, every caller's `else` branch would mishandle it silently. So the
error set is versioned: you document it, and you pin it with a test that asserts
the *only* non-nil error observed across many calls is `ErrRateLimited`. That
contract test is what turns "which errors does this return?" from tribal knowledge
into an enforced invariant.

The limiter itself is a per-key token bucket. Each key gets a bucket that holds up
to `capacity` tokens and refills at `refillPerSec` tokens per second. `Allow` first
refills the bucket based on elapsed time — `min(capacity, tokens + elapsed*rate)`,
clamped so it never exceeds capacity — then either spends a token (return nil) or,
if fewer than one token is available, returns `ErrRateLimited`. The clock is
injected as `now func() time.Time` for the same reason the config loader injected
`getenv`: it makes refill testable without sleeping. A test advances a captured
clock variable by a second and observes capacity return, deterministically and
instantly. A `sync.Mutex` guards the bucket map because a limiter is shared across
request-handling goroutines; the `-race` run proves it.

Create `limiter.go`:

```go
package ratelimit

import (
	"errors"
	"sync"
	"time"
)

// ErrRateLimited is the single non-nil error Allow returns. It is part of the
// exported contract: callers branch on it and must not see any other error.
var ErrRateLimited = errors.New("rate limited")

type bucket struct {
	tokens float64
	last   time.Time
}

// RateLimiter is a per-key token-bucket limiter. now is injected so refill is
// testable without real time.
type RateLimiter struct {
	mu           sync.Mutex
	capacity     float64
	refillPerSec float64
	buckets      map[string]*bucket
	now          func() time.Time
}

// NewRateLimiter builds a limiter with the given capacity and refill rate. If now
// is nil it defaults to time.Now.
func NewRateLimiter(capacity, refillPerSec float64, now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{
		capacity:     capacity,
		refillPerSec: refillPerSec,
		buckets:      make(map[string]*bucket),
		now:          now,
	}
}

// Allow grants one token to key, returning nil, or ErrRateLimited if the bucket
// is empty. It returns no other error: that is the contract.
func (rl *RateLimiter) Allow(key string) error {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	t := rl.now()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.capacity, last: t}
		rl.buckets[key] = b
	}

	elapsed := t.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = min(rl.capacity, b.tokens+elapsed*rl.refillPerSec)
		b.last = t
	}

	if b.tokens < 1 {
		return ErrRateLimited
	}
	b.tokens--
	return nil
}
```

### The runnable demo

The demo exhausts a three-token bucket, shows the next call denied, advances a
captured clock by one second to refill one token, and shows a call allowed again.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/ratelimit"
)

func main() {
	now := time.Unix(0, 0)
	rl := ratelimit.NewRateLimiter(3, 1, func() time.Time { return now })

	for i := range 4 {
		err := rl.Allow("user:1")
		fmt.Printf("call %d: limited=%v\n", i+1, errors.Is(err, ratelimit.ErrRateLimited))
	}

	now = now.Add(time.Second) // refill one token
	err := rl.Allow("user:1")
	fmt.Printf("after refill: limited=%v\n", errors.Is(err, ratelimit.ErrRateLimited))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (four calls exhaust the bucket, then one succeeds after refill):

```
call 1: limited=false
call 2: limited=false
call 3: limited=false
call 4: limited=true
after refill: limited=false
```

### Tests

`TestAllowThenLimit` spends the whole bucket and asserts the next call is
`ErrRateLimited`. `TestRefillAfterClockAdvance` advances the injected clock and
asserts capacity returns. `TestOnlyErrRateLimited` is the contract test: it hammers
the limiter and asserts every non-nil error it ever returned is `ErrRateLimited`,
so a caller branching on that sentinel is safe. `TestConcurrentAllow` runs under
`-race` to prove the mutex guards the bucket map.

Create `limiter_test.go`:

```go
package ratelimit

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAllowThenLimit(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	rl := NewRateLimiter(3, 1, func() time.Time { return now })

	for i := range 3 {
		if err := rl.Allow("k"); err != nil {
			t.Fatalf("call %d = %v, want nil", i+1, err)
		}
	}
	if err := rl.Allow("k"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("call 4 = %v, want ErrRateLimited", err)
	}
}

func TestRefillAfterClockAdvance(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	rl := NewRateLimiter(3, 1, func() time.Time { return now })

	for range 3 {
		_ = rl.Allow("k")
	}
	if err := rl.Allow("k"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected limit before refill, got %v", err)
	}

	now = now.Add(time.Second) // one token refilled
	if err := rl.Allow("k"); err != nil {
		t.Fatalf("after refill = %v, want nil", err)
	}
	if err := rl.Allow("k"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected limit after spending the one refilled token, got %v", err)
	}
}

// TestOnlyErrRateLimited pins the contract: the only non-nil error Allow returns
// is ErrRateLimited, so callers can branch on it exhaustively.
func TestOnlyErrRateLimited(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	rl := NewRateLimiter(2, 1, func() time.Time { return now })

	for i := range 20 {
		if err := rl.Allow("k"); err != nil && !errors.Is(err, ErrRateLimited) {
			t.Fatalf("call %d returned unexpected error %v; contract is ErrRateLimited only", i+1, err)
		}
	}
}

func TestConcurrentAllow(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(1000, 0, func() time.Time { return time.Unix(0, 0) })
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = rl.Allow("shared")
			_ = rl.Allow(string(rune('a' + i%26)))
		}()
	}
	wg.Wait()
}
```

## Review

The limiter is correct when a bucket of capacity N grants exactly N tokens before
denying, and a clock advance of one second at rate one restores exactly one token —
`TestRefillAfterClockAdvance` pins both edges. But the load-bearing test for this
lesson is `TestOnlyErrRateLimited`: it encodes the promise that `Allow` returns
`ErrRateLimited` and nothing else, which is what makes a caller's
`errors.Is(err, ErrRateLimited)` branch exhaustive. Injecting `now` is what lets
the refill assertions be instant and deterministic instead of sleeping real
seconds.

The mistake to avoid is treating the returned error set as a private
implementation detail — adding a new sentinel, or returning a wrapped variant that
`errors.Is` no longer matches, silently breaks every caller that branched on the
old contract. Document the errors a method returns and pin them with a test. Run
`go test -race` to confirm the mutex actually guards the shared bucket map.

## Resources

- [pkg.go.dev: errors.Is](https://pkg.go.dev/errors#Is) — how callers match the sentinel a method's contract promises.
- [Go Blog: Error handling and Go](https://go.dev/blog/error-handling-and-go) — errors as values callers inspect and branch on.
- [pkg.go.dev: time.Time.Sub](https://pkg.go.dev/time#Time.Sub) — computing elapsed time for the token refill.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-defer-close-error-capture.md](08-defer-close-error-capture.md) | Next: [../02-fmt-errorf-and-error-wrapping/00-concepts.md](../02-fmt-errorf-and-error-wrapping/00-concepts.md)
