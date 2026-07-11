# Exercise 5: Mutex-guarded token-bucket rate limiter

A per-endpoint or per-tenant rate limiter is a token bucket: it holds a balance
of tokens that refills at a fixed rate up to a burst ceiling, and each admitted
request spends one. This module builds that limiter with a mutex guarding the
refill-and-deduct — the entire critical section — and a deterministic clock so
the tests assert exact behavior without sleeping.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
tokenbucket/                 independent module: example.com/tokenbucket
  go.mod                     go 1.26
  limiter.go                 type Limiter; New, Allow
  cmd/
    demo/
      main.go                runnable demo: drain the burst, refill, allow again
  limiter_test.go            burst-then-deny, exact refill, concurrent budget, -race
```

- Files: `limiter.go`, `cmd/demo/main.go`, `limiter_test.go`.
- Implement: a `Limiter` (mutex + `tokens float64` + `lastRefill time.Time` + rate/burst) with `Allow() bool` that lazily refills based on elapsed time, then deducts a token.
- Test: an injected clock; a burst of `Allow()` up to capacity returns true then false; advancing the clock refills exactly `rate*elapsed`; concurrent `Allow()` never exceeds the budget under `-race`.
- Verify: `go test -count=1 -race ./...`

```bash
mkdir -p ~/go-exercises/tokenbucket/cmd/demo
cd ~/go-exercises/tokenbucket
go mod init example.com/tokenbucket
```

### Lazy refill under the lock, and why the whole thing is the critical section

There is no background goroutine dripping tokens into the bucket. Instead `Allow`
refills lazily: on each call it measures the time since the last refill, adds
`elapsed * rate` tokens (capped at `burst`), records the new refill instant, and
then decides whether at least one token is available to spend. That whole
sequence — read the clock, compute the refill, update `tokens` and `lastRefill`,
compare, and deduct — must be one atomic critical section. If two goroutines
interleaved a check ("is there a token?") with the deduct ("take it"), both could
see the last token and both spend it, admitting one request too many. So the
mutex wraps the entire body of `Allow`, and the section is intentionally short:
a subtraction and a comparison, no I/O.

`tokens` is a `float64` on purpose. A rate of 5 tokens per second over 200 ms is
1 token; over 100 ms it is half a token. Fractional accounting lets the limiter
honor a rate that does not divide evenly into request arrivals, accumulating
partial tokens across calls instead of rounding them away. The cap at `burst`
keeps an idle bucket from hoarding unbounded credit — after a long quiet period
the bucket holds exactly `burst`, so a sudden spike is allowed up to the burst
size and no more.

As in the cache, the clock is injected. A fixed clock (no time advancing) makes
the concurrent-budget test exact: with `elapsed == 0` no refill happens, so
`burst` goroutines racing on `Allow` yield exactly `burst` admissions and no
more — the invariant a rate limiter exists to guarantee.

Create `limiter.go`:

```go
package tokenbucket

import (
	"sync"
	"time"
)

// Limiter is a token-bucket rate limiter safe for concurrent use. It refills
// lazily on each Allow based on elapsed time; there is no background goroutine.
type Limiter struct {
	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
	rate       float64 // tokens added per second
	burst      float64 // maximum tokens the bucket can hold
	now        func() time.Time
}

// New returns a Limiter that adds rate tokens per second up to burst, starting
// full. rate and burst must be positive.
func New(rate, burst float64) *Limiter {
	return newWithClock(rate, burst, time.Now)
}

func newWithClock(rate, burst float64, now func() time.Time) *Limiter {
	return &Limiter{
		tokens:     burst,
		lastRefill: now(),
		rate:       rate,
		burst:      burst,
		now:        now,
	}
}

// Allow refills the bucket for the elapsed time, then admits one request if a
// token is available, deducting it. The refill-check-deduct is one critical
// section so no two callers can spend the same token.
func (l *Limiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	elapsed := now.Sub(l.lastRefill).Seconds()
	l.lastRefill = now
	l.tokens += elapsed * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	return false
}
```

### The runnable demo

The demo uses a small burst of 3 and a slow rate so you can watch the bucket
drain, deny, and — after a real sleep long enough to refill one token — admit
again.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/tokenbucket"
)

func main() {
	l := tokenbucket.New(10, 3) // 10 tokens/sec, burst of 3

	for i := range 4 {
		fmt.Printf("burst call %d: %v\n", i+1, l.Allow())
	}

	time.Sleep(150 * time.Millisecond) // ~1.5 tokens refill

	fmt.Printf("after refill: %v\n", l.Allow())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
burst call 1: true
burst call 2: true
burst call 3: true
burst call 4: false
after refill: true
```

### Tests

`TestBurstThenDeny` drains a full bucket with a frozen clock: the first `burst`
calls are admitted, the next is denied. `TestExactRefill` advances the clock one
second at `rate=5` and asserts exactly five more admissions become available.
`TestConcurrentBudget` freezes the clock and races `Allow` from many goroutines,
counting admissions with an `atomic.Int64`; because no time passes, the total
admitted must equal exactly `burst` — the guarantee the limiter exists to make.

Create `limiter_test.go`:

```go
package tokenbucket

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func TestBurstThenDeny(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(0, 0)}
	l := newWithClock(5, 3, clk.Now)

	for i := range 3 {
		if !l.Allow() {
			t.Fatalf("burst call %d denied; bucket should be full", i+1)
		}
	}
	if l.Allow() {
		t.Fatal("call past burst admitted; bucket should be empty")
	}
}

func TestExactRefill(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(0, 0)}
	l := newWithClock(5, 3, clk.Now)

	for range 3 { // drain the burst
		l.Allow()
	}
	clk.Advance(time.Second) // refill 5*1 = 5 tokens, capped at burst=3

	admitted := 0
	for range 10 {
		if l.Allow() {
			admitted++
		}
	}
	if admitted != 3 {
		t.Fatalf("after 1s refill admitted %d, want 3 (capped at burst)", admitted)
	}
}

func TestConcurrentBudget(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(0, 0)} // frozen: no refill during the race
	const burst = 50
	l := newWithClock(5, burst, clk.Now)

	var admitted atomic.Int64
	var wg sync.WaitGroup
	const goroutines = 200
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if l.Allow() {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := admitted.Load(); got != burst {
		t.Fatalf("admitted %d with a frozen clock, want exactly %d", got, burst)
	}
}
```

## Review

The limiter is correct when the refill-check-deduct is one continuous critical
section: split it and two callers spend the same last token, admitting one over
budget. The concurrent-budget test is the proof — with a frozen clock, exactly
`burst` requests are admitted no matter how many goroutines race, and any excess
means the check and the deduct were not atomic. The exact-refill test pins that
tokens accrue at `rate*elapsed` and are capped at `burst`.

The traps: refilling with an integer count instead of a `float64` silently drops
sub-token credit and slows the effective rate below the configured one; and
capping *after* the deduct instead of before lets an idle bucket admit more than
`burst` on the first spike. Keep the whole body under the lock and keep it short —
a rate limiter is on every request's path, so a slow critical section here is a
service-wide tail-latency cost. Run `go test -race`.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — guarding the refill-and-deduct.
- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) — the production token-bucket limiter this models.
- [`time` package](https://pkg.go.dev/time) — `time.Now`, `Time.Sub`, `Duration.Seconds`.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — proving the concurrent budget holds.

---

Back to [04-ttl-cache.md](04-ttl-cache.md) | Next: [06-idempotency-dedupe-store.md](06-idempotency-dedupe-store.md)
