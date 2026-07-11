# Exercise 1: Token Bucket Backend

The token bucket is the burst-tolerant half of the engine: it lets a fresh client fire a short spike at line rate, then settles to a sustained `rate`. This exercise wraps `golang.org/x/time/rate.Limiter` in a small backend that returns the timing metadata the HTTP headers need, and tests its refill behaviour under virtual time so a "10 tokens per second" claim is asserted in microseconds.

This module is fully self-contained: its own `go mod init`, every type defined inline, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
tokenbucket.go        TokenBucket, New, TryAllow (ReserveN + CancelAt)
cmd/
  demo/
    main.go           drive a full bucket to exhaustion, print allowed/remaining
tokenbucket_test.go   burst-then-deny, synctest refill, -race concurrency
```

- Files: `tokenbucket.go`, `cmd/demo/main.go`, `tokenbucket_test.go`.
- Implement: `TokenBucket` with `New(ratePerSec float64, burst int) *TokenBucket` and `TryAllow(now time.Time) (allowed bool, remaining int, resetAt time.Time)`.
- Test: burst up to `burst` then deny, refill after a virtual sleep under `synctest`, and a `-race` concurrency stress.
- Verify: `go test -count=1 -race ./...`

Set up the module. The token bucket pulls in `golang.org/x/time/rate`:

```bash
mkdir -p ~/go-exercises/tokenbucket/cmd/demo
cd ~/go-exercises/tokenbucket
go mod init example.com/token-bucket
go mod edit -go=1.26
go get golang.org/x/time@latest
```

### Why ReserveN, not Allow

`golang.org/x/time/rate.Limiter` is a battle-tested token bucket: concurrency-safe, no background goroutine, tokens computed lazily from elapsed time at each call. Every method takes a `time.Time`, which is exactly why it is easy to test — you pass it the clock, so a `synctest` bubble can drive its refill deterministically.

The interesting choice is `ReserveN(now, 1)` over the simpler `Allow()`. `Allow` answers a single yes/no and discards everything else; to fill in `X-RateLimit-Reset` you would then call `TokensAt` separately, which races the next consumer and gives you an inconsistent snapshot. `ReserveN` instead consumes the token *and* hands back a `*Reservation` whose `DelayFrom(now)` tells you precisely when the token would be available. Three cases fall out of that one call:

- `!r.OK()` — `n` exceeds `burst`; this limiter can never serve the request. Deny with a best-effort reset estimate.
- `delay > 0` — the tokens are in the future. You want to deny rather than block, so you must give the reserved token back with `r.CancelAt(now)`; skipping that cancel leaves the bucket's internal count permanently wrong.
- `delay == 0` — servable now; report the remaining tokens via `TokensAt(now)` and a reset one token-interval out.

`remaining` is read from `TokensAt(now)`, floored at zero and truncated to an int, so the header reflects the budget left after this request.

Create `tokenbucket.go`:

```go
package tokenbucket

import (
	"math"
	"time"

	"golang.org/x/time/rate"
)

// TokenBucket wraps golang.org/x/time/rate.Limiter. The limiter starts full
// (burst tokens available) and refills at ratePerSec tokens per second. It is
// safe for concurrent use; token accumulation is computed lazily from the
// elapsed time at each TryAllow call, so no background goroutine is needed.
type TokenBucket struct {
	lim      *rate.Limiter
	ratePerS float64
}

// New returns a TokenBucket that refills at ratePerSec tokens per second and
// holds at most burst tokens.
func New(ratePerSec float64, burst int) *TokenBucket {
	return &TokenBucket{
		lim:      rate.NewLimiter(rate.Limit(ratePerSec), burst),
		ratePerS: ratePerSec,
	}
}

// TryAllow attempts to consume one token at instant now. It uses ReserveN
// rather than Allow so that it can compute an accurate resetAt time for the
// X-RateLimit-Reset header.
func (tb *TokenBucket) TryAllow(now time.Time) (allowed bool, remaining int, resetAt time.Time) {
	r := tb.lim.ReserveN(now, 1)
	if !r.OK() {
		// n exceeds burst; the request can never be served by this limiter.
		// resetAt is a best-effort estimate.
		reset := now.Add(time.Duration(float64(time.Second) / math.Max(tb.ratePerS, 1e-9)))
		return false, 0, reset
	}
	delay := r.DelayFrom(now)
	if delay > 0 {
		// Tokens will be available in `delay`; deny now and return the token.
		r.CancelAt(now)
		rem := int(math.Max(0, tb.lim.TokensAt(now)))
		return false, rem, now.Add(delay)
	}
	rem := int(math.Max(0, tb.lim.TokensAt(now)))
	var reset time.Time
	if tb.ratePerS > 0 {
		reset = now.Add(time.Duration(float64(time.Second) / tb.ratePerS))
	}
	return true, rem, reset
}
```

### The runnable demo

The demo drives one bucket with a single fixed instant — every call shares the same `now`, so no refill happens between them — and watches the burst of three drain to a deny on the fourth. Because nothing depends on the wall clock, the output is identical on every run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/token-bucket"
)

func main() {
	tb := tokenbucket.New(1, 3) // 1 token/sec, burst 3
	now := time.Now()

	fmt.Println("rate=1 tok/s, burst=3 (bucket starts full)")
	for i := 1; i <= 4; i++ {
		ok, rem, _ := tb.TryAllow(now)
		fmt.Printf("  request %d: allowed=%v  remaining=%d\n", i, ok, rem)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
rate=1 tok/s, burst=3 (bucket starts full)
  request 1: allowed=true  remaining=2
  request 2: allowed=true  remaining=1
  request 3: allowed=true  remaining=0
  request 4: allowed=false  remaining=0
```

### Tests

The first test pins the burst contract at one instant: three allows, then a deny, with no clock movement. The refill test is the one that needs virtual time — it runs inside a `synctest` bubble, exhausts a one-token bucket, sleeps 200 ms of *virtual* time (which returns instantly), and asserts the bucket has refilled. Driving the limiter with `time.Now()` inside the bubble is what makes that assertion deterministic: there is no scheduler slack, so a 200 ms refill at 10 tokens/sec is exact. The concurrency test runs outside any bubble and exists to prove the limiter's internal mutex holds under `-race`.

Create `tokenbucket_test.go`:

```go
package tokenbucket

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestAllowsBurstThenDenies(t *testing.T) {
	t.Parallel()
	tb := New(1, 3) // 1 tok/sec, burst 3
	now := time.Now()
	for i := range 3 {
		ok, _, _ := tb.TryAllow(now)
		if !ok {
			t.Fatalf("request %d should be allowed (burst=3)", i+1)
		}
	}
	if ok, _, _ := tb.TryAllow(now); ok {
		t.Fatal("4th immediate request should be denied: burst exhausted")
	}
}

func TestRefillsOverVirtualTime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		tb := New(10, 1) // 10 tok/sec, burst 1
		if ok, _, _ := tb.TryAllow(time.Now()); !ok {
			t.Fatal("first request should be allowed")
		}
		if ok, _, _ := tb.TryAllow(time.Now()); ok {
			t.Fatal("second immediate request should be denied: no tokens")
		}
		// At 10 tok/sec, 200 ms of virtual time yields a refilled token.
		time.Sleep(200 * time.Millisecond)
		if ok, _, _ := tb.TryAllow(time.Now()); !ok {
			t.Fatal("request after virtual refill should be allowed")
		}
	})
}

func TestReturnsResetAtOnDeny(t *testing.T) {
	t.Parallel()
	tb := New(2, 1) // 2 tok/sec, burst 1
	now := time.Now()
	tb.TryAllow(now) // consume the only token
	ok, _, resetAt := tb.TryAllow(now)
	if ok {
		t.Fatal("should be denied")
	}
	if resetAt.Before(now) {
		t.Fatalf("resetAt %v should be at or after now %v", resetAt, now)
	}
}

func TestConcurrentTryAllow(t *testing.T) {
	t.Parallel()
	tb := New(1000, 1000)
	now := time.Now()
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tb.TryAllow(now)
		}()
	}
	wg.Wait()
}
```

## Review

The backend is correct when a deny always returns the reservation. The mistake this design exists to prevent is calling `ReserveN`, seeing `delay > 0`, and returning without `r.CancelAt(now)` — the token stays committed and every later count is off by one. `TestAllowsBurstThenDenies` proves the burst ceiling holds at a single instant, and `TestRefillsOverVirtualTime` proves a token genuinely comes back after the refill interval; the latter is only deterministic because the bubble virtualizes the clock you feed into `TryAllow`. Read `remaining` from `TokensAt(now)` rather than tracking it yourself, and never substitute `Allow()` when you need the reset time. The `-race` run on `TestConcurrentTryAllow` is the proof that the limiter is safe to share across request goroutines without an external lock.

## Resources

- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) — the `Limiter`, `Reservation`, `ReserveN`, `DelayFrom`, and `TokensAt` used here.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — the virtual-time bubble that makes the refill assertion deterministic.
- [Testing concurrent code with testing/synctest](https://go.dev/blog/synctest) — why driving time through the clock you pass in beats injecting a fake `Clock`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-sliding-window.md](02-sliding-window.md)
