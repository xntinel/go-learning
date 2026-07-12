# Exercise 3: Clock-injected token-bucket rate limiter tested at refill boundaries

A token-bucket limiter is the standard way a backend caps request rate: a bucket
of `capacity` tokens refills at a steady rate, each admitted request spends one,
and an empty bucket rejects. Its correctness lives entirely at the refill
boundary — exactly when does the next token become available — so it is the
perfect artifact for testing time at the edge. Inject a `Clock`, drain the bucket,
advance the fake clock by precise fractions of a refill interval, and assert the
grant happens at the boundary instant and not a nanosecond early.

## What you'll build

```text
tokenbucket/                   independent module: example.com/tokenbucket
  go.mod
  limiter.go                   Clock (Now); RealClock; FakeClock; Limiter (Allow) with lazy refill
  cmd/
    demo/
      main.go                  drain a bucket, advance a fake clock, watch a token return
  limiter_test.go              drain/reject, boundary refill, partial-interval, -race
```

Files: `limiter.go`, `cmd/demo/main.go`, `limiter_test.go`.
Implement: a concurrency-safe `Limiter` with `capacity` and a one-token-per-`interval` refill computed lazily from elapsed time via an injected `Clock`; `Allow() bool`.
Test: inject `FakeClock` — drain the bucket and assert further `Allow()==false`; advance exactly one interval and assert exactly one grant; advance a partial interval and assert no premature grant; run concurrent `Allow()` under `-race`.
Verify: `go test -count=1 -race ./...`

### Lazy refill: the design that makes the boundary exact

A naive limiter would run a background goroutine that adds a token every
interval. That is wasteful (a goroutine per limiter) and imprecise (the token
appears whenever the goroutine happens to be scheduled). The production design is
*lazy refill*: the limiter stores the number of tokens as a float and the instant
it was last updated. On each `Allow()`, it reads `clock.Now()`, computes how much
time elapsed since the last update, converts that to a fractional number of tokens
(`elapsed / interval`), adds them (capped at `capacity`), and records the new
instant. Then, if it holds at least one whole token, it spends one and admits;
otherwise it rejects.

This makes the grant a pure function of elapsed time, which is exactly what makes
it testable to the nanosecond. Tokens are a `float64` so that a partial interval
contributes a partial token: advancing half an interval adds `0.5` tokens, not
enough to admit; a second half brings the total to exactly `1.0`, which admits.
Because `float64(interval)/float64(interval)` is exactly `1.0` and `0.5 + 0.5` is
exactly `1.0` in binary floating point, the boundary assertions are exact rather
than approximate.

The `>= 1` threshold is the boundary contract. A bucket holding exactly `1.0`
token admits; a bucket holding `0.999...` does not. The test pins this at the
refill instant: at `interval-1ns` the bucket holds just under a token and rejects;
at `interval` it holds exactly one and admits.

Create `limiter.go`:

```go
package tokenbucket

import (
	"sync"
	"time"
)

// Clock is the minimal time surface the limiter reads.
type Clock interface {
	Now() time.Time
}

// RealClock forwards to the standard library.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// FakeClock is a test clock advanced by hand, safe for concurrent use.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{now: t}
}

func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Limiter is a concurrency-safe token bucket. It starts full and refills one
// token per interval, computed lazily from the injected clock on each Allow.
type Limiter struct {
	mu       sync.Mutex
	clock    Clock
	capacity float64
	interval time.Duration
	tokens   float64
	last     time.Time
}

// NewLimiter builds a full bucket of the given capacity that refills one token
// every interval, reading time from clock.
func NewLimiter(clock Clock, capacity int, interval time.Duration) *Limiter {
	return &Limiter{
		clock:    clock,
		capacity: float64(capacity),
		interval: interval,
		tokens:   float64(capacity),
		last:     clock.Now(),
	}
}

// Allow refills lazily from elapsed time, then admits if at least one whole
// token is available, spending it. Otherwise it rejects.
func (l *Limiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock.Now()
	elapsed := now.Sub(l.last)
	l.last = now
	l.tokens += float64(elapsed) / float64(l.interval)
	if l.tokens > l.capacity {
		l.tokens = l.capacity
	}
	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	return false
}
```

### The runnable demo

The demo builds a one-token bucket refilling every 100ms on a `FakeClock`, spends
the initial token, shows the next call rejected, advances 100ms, and shows the
token returned — all with no real time passing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/tokenbucket"
)

func main() {
	fc := tokenbucket.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	lim := tokenbucket.NewLimiter(fc, 1, 100*time.Millisecond)

	fmt.Printf("first call: %v\n", lim.Allow())
	fmt.Printf("second call (empty): %v\n", lim.Allow())
	fc.Advance(100 * time.Millisecond)
	fmt.Printf("after +100ms: %v\n", lim.Allow())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first call: true
second call (empty): false
after +100ms: true
```

### Tests

`TestDrainThenReject` spends the initial capacity and asserts the bucket is empty.
`TestRefillBoundary` is the heart: after draining, it advances `interval-1ns` and
asserts no grant, then the final `1ns` to reach exactly `interval` and asserts one
grant — pinning the `>= 1` boundary. `TestPartialIntervalNoGrant` advances two
half-intervals and proves the grant appears only once a full token accrues.
`TestConcurrentAllow` hammers `Allow()` from many goroutines under `-race` with a
fixed clock to prove the mutex guards the lazy refill.

Create `limiter_test.go`:

```go
package tokenbucket

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestDrainThenReject(t *testing.T) {
	t.Parallel()
	fc := NewFakeClock(epoch)
	lim := NewLimiter(fc, 3, time.Second)
	for i := range 3 {
		if !lim.Allow() {
			t.Fatalf("Allow #%d = false on a full bucket of 3", i+1)
		}
	}
	if lim.Allow() {
		t.Fatal("Allow granted on a drained bucket")
	}
}

func TestRefillBoundary(t *testing.T) {
	t.Parallel()
	fc := NewFakeClock(epoch)
	lim := NewLimiter(fc, 1, 100*time.Millisecond)

	if !lim.Allow() {
		t.Fatal("first Allow on full bucket = false")
	}
	if lim.Allow() {
		t.Fatal("Allow granted on empty bucket")
	}

	fc.Advance(100*time.Millisecond - time.Nanosecond)
	if lim.Allow() {
		t.Fatal("Allow granted 1ns before the refill instant")
	}

	fc.Advance(time.Nanosecond) // now exactly one interval since drain
	if !lim.Allow() {
		t.Fatal("Allow denied at the exact refill instant")
	}
}

func TestPartialIntervalNoGrant(t *testing.T) {
	t.Parallel()
	fc := NewFakeClock(epoch)
	lim := NewLimiter(fc, 1, 100*time.Millisecond)
	lim.Allow() // drain

	fc.Advance(50 * time.Millisecond)
	if lim.Allow() {
		t.Fatal("Allow granted after only half a refill interval")
	}
	fc.Advance(50 * time.Millisecond) // total one interval
	if !lim.Allow() {
		t.Fatal("Allow denied after two half-intervals accrued a full token")
	}
}

func TestConcurrentAllow(t *testing.T) {
	t.Parallel()
	lim := NewLimiter(NewFakeClock(epoch), 100, time.Second)
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lim.Allow()
		}()
	}
	wg.Wait()
}

func ExampleLimiter_Allow() {
	fc := NewFakeClock(epoch)
	lim := NewLimiter(fc, 1, time.Second)
	fmt.Println(lim.Allow())
	fmt.Println(lim.Allow())
	fc.Advance(time.Second)
	fmt.Println(lim.Allow())
	// Output:
	// true
	// false
	// true
}
```

## Review

The limiter is correct when a grant is a pure function of elapsed time against the
`>= 1` threshold: draining leaves zero tokens, `interval-1ns` accrues just under
one and rejects, `interval` accrues exactly one and admits, and two half-intervals
sum to exactly one. If `TestRefillBoundary` flakes it means a token is being
granted early — a `>` where a `>=` belongs, or a background refiller adding tokens
off-schedule. The design mistake to avoid is the background-goroutine refiller: it
is wasteful and its grant instant is non-deterministic, which is exactly the
property that makes the boundary untestable. Lazy refill keyed off the injected
clock is both cheaper and exactly testable. Run `go test -race` to confirm the
mutex actually serializes the read-modify-write of `tokens` and `last`.

## Resources

- [`time.Time.Sub`](https://pkg.go.dev/time#Time.Sub) — elapsed duration between two instants, the basis of lazy refill.
- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) — the stdlib-adjacent production limiter; its `Limiter` uses the same lazy-token model.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — guarding the read-modify-write under concurrency.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-backoff-retry-injected-sleep.md](02-backoff-retry-injected-sleep.md) | Next: [04-ttl-cache-expiry.md](04-ttl-cache-expiry.md)
