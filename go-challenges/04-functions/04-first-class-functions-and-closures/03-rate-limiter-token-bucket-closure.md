# Exercise 3: Token-Bucket Rate Limiter Returned as an Allow Closure

A rate limiter is the canonical example of stateful private state living inside a
closure, and of the clock-injection technique that makes time-dependent code
testable. `NewLimiter(rate, burst, now)` returns an `Allow` closure — a
`func() bool` — that captures the token count, the last-refill time, a mutex, and
an injected clock. Because the clock is a parameter, the test advances a fake
clock instead of sleeping, and asserts refill behavior in microseconds.

This module is fully self-contained.

## What you'll build

```text
ratelimiter/               independent module: example.com/ratelimiter
  go.mod                   go 1.26
  limiter.go               NewLimiter returns func() bool (token bucket)
  cmd/
    demo/
      main.go              drains a burst, advances a fake clock, refills
  limiter_test.go          burst-then-deny, refill, concurrency under -race
```

- Files: `limiter.go`, `cmd/demo/main.go`, `limiter_test.go`.
- Implement: `NewLimiter(ratePerSec float64, burst int, now func() time.Time) func() bool`, a lazy-refill token bucket guarded by a mutex, reading the clock only through the injected `now`.
- Test: first `burst` calls return true then false; advancing the fake clock refills up to `burst`; many goroutines under `-race` grant no more than capacity when the clock is frozen.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ratelimiter/cmd/demo
cd ~/go-exercises/ratelimiter
go mod init example.com/ratelimiter
```

### Why the clock is a parameter

The token bucket holds `tokens` (a float, so partial refills accumulate) and
`last`, the instant it last recomputed. Each `Allow` call reads the clock,
computes elapsed seconds since `last`, adds `elapsed * ratePerSec` tokens (capped
at `burst`), and then spends one token if at least one is available. Refill is
*lazy*: there is no background goroutine ticking; the bucket catches up on demand
whenever it is asked. That keeps the whole limiter a single closure over a few
variables.

The design decision that makes this testable is taking `now func() time.Time` as
a parameter instead of calling `time.Now()` inside. Production passes
`time.Now`. A test passes a closure over a mutable `time.Time` variable and
advances it by reassigning the variable — no sleeping, no wall-clock flakiness.
When the fake clock is *frozen* (always returns the same instant), elapsed is
always zero, so no tokens ever refill and the bucket grants exactly `burst`
times; that determinism is what lets the concurrency test assert an exact grant
count under `-race`.

The captured state — `tokens`, `last` — is shared across every goroutine that
calls `Allow`, so it must be guarded. The mutex lives inside the closure's
environment; `Allow` locks it for the read-modify-write of the token count. The
closure is the limiter, the captured variables are its fields, and the mutex is
what makes the whole thing concurrency-safe.

Create `limiter.go`:

```go
package ratelimiter

import (
	"sync"
	"time"
)

// NewLimiter returns an Allow closure implementing a token-bucket rate limiter.
// The bucket starts full with burst tokens and refills at ratePerSec tokens per
// second, capped at burst. Refill is computed lazily from the elapsed time on
// each call. now is injected so tests advance a fake clock instead of sleeping.
func NewLimiter(ratePerSec float64, burst int, now func() time.Time) func() bool {
	var (
		mu     sync.Mutex
		tokens = float64(burst)
		last   = now()
	)
	return func() bool {
		mu.Lock()
		defer mu.Unlock()

		t := now()
		elapsed := t.Sub(last).Seconds()
		last = t
		tokens += elapsed * ratePerSec
		if tokens > float64(burst) {
			tokens = float64(burst)
		}

		if tokens >= 1 {
			tokens--
			return true
		}
		return false
	}
}
```

### The runnable demo

The demo uses a fake clock so the output is deterministic. It drains a burst of
three, sees the next two calls denied, then advances the clock one second (which
refills two tokens at a rate of two per second) and sees two grants followed by a
denial.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ratelimiter"
)

func main() {
	now := time.Now()
	clock := func() time.Time { return now }
	allow := ratelimiter.NewLimiter(2, 3, clock)

	for i := range 5 {
		fmt.Printf("call %d: %v\n", i+1, allow())
	}

	now = now.Add(time.Second) // refill 2 tokens (rate = 2/sec)
	fmt.Printf("after 1s: %v\n", allow())
	fmt.Printf("after 1s: %v\n", allow())
	fmt.Printf("after 1s: %v\n", allow())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
call 1: true
call 2: true
call 3: true
call 4: false
call 5: false
after 1s: true
after 1s: true
after 1s: false
```

### Tests

Create `limiter_test.go`:

```go
package ratelimiter

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock returns a clock closure over t plus an advance function.
func fakeClock(start time.Time) (func() time.Time, func(time.Duration)) {
	var mu sync.Mutex
	cur := start
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		cur = cur.Add(d)
	}
	return now, advance
}

func TestBurstThenDeny(t *testing.T) {
	t.Parallel()
	now, _ := fakeClock(time.Unix(0, 0))
	allow := NewLimiter(1, 3, now)

	for i := range 3 {
		if !allow() {
			t.Fatalf("call %d denied; want allowed within burst", i+1)
		}
	}
	if allow() {
		t.Fatal("call 4 allowed; want denied after burst exhausted")
	}
}

func TestRefillAfterElapsedTime(t *testing.T) {
	t.Parallel()
	now, advance := fakeClock(time.Unix(0, 0))
	allow := NewLimiter(10, 3, now)

	for range 3 {
		allow()
	}
	if allow() {
		t.Fatal("bucket should be empty")
	}

	advance(time.Second) // 10 tokens/sec refill, capped at burst=3
	granted := 0
	for range 5 {
		if allow() {
			granted++
		}
	}
	if granted != 3 {
		t.Fatalf("granted = %d after refill, want 3 (capped at burst)", granted)
	}
}

func TestConcurrentNoDoubleSpend(t *testing.T) {
	t.Parallel()
	now, _ := fakeClock(time.Unix(0, 0)) // frozen: no refill during the test
	const burst = 50
	allow := NewLimiter(1, burst, now)

	var granted atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if allow() {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := granted.Load(); got != burst {
		t.Fatalf("granted = %d, want exactly %d (no double-spend, no refill)", got, burst)
	}
}

func ExampleNewLimiter() {
	now := time.Unix(0, 0)
	allow := NewLimiter(1, 1, func() time.Time { return now })
	fmt.Println(allow())
	fmt.Println(allow())
	// Output:
	// true
	// false
}
```

## Review

The limiter is correct when it grants exactly `burst` tokens before the clock
moves, refills at the configured rate up to the cap, and never over-grants under
concurrency. The clock-injection technique is the transferable lesson: because
`now` is a parameter, `TestRefillAfterElapsedTime` advances a fake second and
observes the refill instantly, and `TestConcurrentNoDoubleSpend` freezes the
clock so the exact grant count is deterministic — assertions that would be slow
and flaky if the code called `time.Now()` directly. The mutex guards the captured
`tokens`/`last`, which is why the concurrency test is clean under `-race`; remove
it and the detector fires. Run `go test -race`.

## Resources

- [pkg.go.dev: time.Time.Sub](https://pkg.go.dev/time#Time.Sub) — elapsed duration between two instants.
- [pkg.go.dev: sync/atomic Int64](https://pkg.go.dev/sync/atomic#Int64) — the grant counter in the concurrency test.
- [pkg.go.dev: golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate) — the production token-bucket limiter this exercise mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-middleware-chain-compose.md](02-middleware-chain-compose.md) | Next: [04-retry-with-backoff-higher-order.md](04-retry-with-backoff-higher-order.md)
