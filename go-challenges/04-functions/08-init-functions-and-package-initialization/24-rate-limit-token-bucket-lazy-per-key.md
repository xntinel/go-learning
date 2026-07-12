# Exercise 24: Per-Key Rate Limiters, Lazily Built via sync.OnceValue

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A multi-tenant service that rate-limits per tenant or per user should not
allocate a token bucket for every possible key up front — most keys may
never make a single request. This exercise builds a `Limiter` that lazily
creates one token-bucket `Bucket` per key, the first time that key calls
`Allow`, using `sync.OnceValue` so concurrent first-requests for the same key
never create two competing buckets, and an unused key costs nothing at all.

## What you'll build

```text
ratelimit/                 independent module: example.com/ratelimit
  go.mod                    module example.com/ratelimit
  ratelimit.go                Clock, Bucket (refill math), Limiter (lazy per-key), ManualClock
  cmd/
    demo/
      main.go                 deterministic run using ManualClock, no real time.Sleep
  ratelimit_test.go            capacity/refill table + lazy-independence + concurrent-capacity test
```

Files: `ratelimit.go`, `cmd/demo/main.go`, `ratelimit_test.go`.
Implement: `Bucket.Allow() bool` doing refill-then-consume under one lock; `Limiter.Allow(key string) bool` lazily building each key's `Bucket` via `sync.OnceValue`; `Limiter.Keys() int`; a `ManualClock` for deterministic time.
Test: a bucket allows exactly up to capacity then blocks; refill after an advanced duration restores tokens; two keys have independent buckets and only touched keys are counted; many goroutines hitting one key concurrently are allowed exactly `capacity` times, verified with `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the whole check-then-act must be one critical section

A token bucket's `Allow` does three things in sequence: refill based on
elapsed time, check whether at least one token is available, and if so,
consume it. Protecting only the refill step, or only the decrement, with a
lock — while leaving the "is at least one token available" check outside
it — reopens exactly the race this design exists to prevent: two goroutines
could both observe `tokens >= 1` before either decrements, and both proceed,
handing out more permits than the bucket's capacity allows. `Bucket.Allow`
holds its mutex across the entire refill-check-consume sequence for that
reason; there is no partial-locking version of this method that is still
correct.

`Limiter` adds a second, independent concern: creating a key's `Bucket`
lazily. Using `sync.OnceValue` per key means the (conceptually expensive)
first-touch setup for a key runs exactly once even if many goroutines call
`Allow` with that key simultaneously for the first time — the same
`sync.OnceValue` technique an earlier exercise in this chapter used for
per-dialect connection pools, applied here to per-key rate-limiter state
instead.

Both the demo and the tests use `ManualClock` rather than `time.Now` and
`time.Sleep`, so refill behavior is exact and reproducible: advancing the
clock by precisely two seconds always refills precisely `2 * refillRate`
tokens, with no flakiness from actual scheduling delays.

Create `ratelimit.go`:

```go
// ratelimit.go
// Package ratelimit implements a per-key token bucket rate limiter whose
// per-key state is created lazily, via sync.OnceValue, the first time that
// key is actually seen — so a tenant or user who never makes a request
// never causes an allocation.
package ratelimit

import (
	"sync"
	"time"
)

// Clock returns the current time. Production code passes time.Now; tests
// and the demo pass a ManualClock's Now method so refill math is
// deterministic.
type Clock func() time.Time

// Bucket is a token bucket: it holds at most capacity tokens and refills at
// refillRate tokens per second, computed lazily from elapsed clock time
// rather than a background goroutine.
type Bucket struct {
	mu         sync.Mutex
	capacity   float64
	refillRate float64
	tokens     float64
	last       time.Time
	clock      Clock
}

func newBucket(capacity, refillRate float64, clock Clock) *Bucket {
	return &Bucket{
		capacity:   capacity,
		refillRate: refillRate,
		tokens:     capacity,
		last:       clock(),
		clock:      clock,
	}
}

// Allow reports whether one token is available right now, consuming it if
// so. The full check-then-act — refill, check, decrement — happens under
// one lock, so concurrent callers never both see and consume the same
// token.
func (b *Bucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clock()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(b.capacity, b.tokens+elapsed*b.refillRate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// Limiter lazily creates one Bucket per key using sync.OnceValue, so a key
// that never calls Allow never allocates a bucket.
type Limiter struct {
	mu         sync.Mutex
	capacity   float64
	refillRate float64
	clock      Clock
	buckets    map[string]func() *Bucket
}

// NewLimiter returns a Limiter sharing one capacity/refillRate/clock across
// every key's lazily created bucket.
func NewLimiter(capacity, refillRate float64, clock Clock) *Limiter {
	if clock == nil {
		clock = time.Now
	}
	return &Limiter{
		capacity:   capacity,
		refillRate: refillRate,
		clock:      clock,
		buckets:    make(map[string]func() *Bucket),
	}
}

// Allow reports whether key may proceed right now, lazily creating and
// caching its bucket on first use.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	get, ok := l.buckets[key]
	if !ok {
		get = sync.OnceValue(func() *Bucket {
			return newBucket(l.capacity, l.refillRate, l.clock)
		})
		l.buckets[key] = get
	}
	l.mu.Unlock()
	return get().Allow()
}

// Keys reports how many distinct keys have had a bucket created for them,
// i.e. have had Allow called at least once.
func (l *Limiter) Keys() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// ManualClock is a Clock a caller advances explicitly, used by the demo and
// by tests so refill behavior never depends on real elapsed wall-clock time.
type ManualClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewManualClock returns a ManualClock starting at start.
func NewManualClock(start time.Time) *ManualClock {
	return &ManualClock{now: start}
}

// Now returns the clock's current time. It satisfies the Clock signature.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"time"

	"example.com/ratelimit"
)

func main() {
	clock := ratelimit.NewManualClock(time.Unix(0, 0))
	limiter := ratelimit.NewLimiter(3, 1, clock.Now) // capacity 3, refills 1 token/sec

	for i := 0; i < 4; i++ {
		fmt.Printf("tenant-a request %d allowed: %v\n", i+1, limiter.Allow("tenant-a"))
	}
	fmt.Println("buckets created so far:", limiter.Keys())

	clock.Advance(2 * time.Second)
	fmt.Println("after 2s, tenant-a allowed:", limiter.Allow("tenant-a"))

	fmt.Println("tenant-b (never touched before) allowed:", limiter.Allow("tenant-b"))
	fmt.Println("buckets created so far:", limiter.Keys())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
tenant-a request 1 allowed: true
tenant-a request 2 allowed: true
tenant-a request 3 allowed: true
tenant-a request 4 allowed: false
buckets created so far: 1
after 2s, tenant-a allowed: true
tenant-b (never touched before) allowed: true
buckets created so far: 2
```

### Tests

Create `ratelimit_test.go`:

```go
// ratelimit_test.go
package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBucketAllowsUpToCapacityThenBlocks(t *testing.T) {
	clock := NewManualClock(time.Unix(0, 0))
	b := newBucket(3, 1, clock.Now)

	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("request %d: Allow() = false, want true within capacity", i+1)
		}
	}
	if b.Allow() {
		t.Fatal("4th request: Allow() = true, want false: capacity exhausted")
	}
}

func TestBucketRefillsOverTime(t *testing.T) {
	clock := NewManualClock(time.Unix(0, 0))
	b := newBucket(2, 1, clock.Now) // capacity 2, refills 1 token/sec

	if !b.Allow() || !b.Allow() {
		t.Fatal("expected both initial requests to be allowed")
	}
	if b.Allow() {
		t.Fatal("expected bucket to be empty before any time passes")
	}

	clock.Advance(2 * time.Second) // +2 tokens, capped at capacity 2
	if !b.Allow() || !b.Allow() {
		t.Fatal("expected refill to allow two more requests after 2s")
	}
	if b.Allow() {
		t.Fatal("expected bucket to be empty again after consuming the refill")
	}
}

func TestLimiterLazyPerKeyIndependentBuckets(t *testing.T) {
	clock := NewManualClock(time.Unix(0, 0))
	l := NewLimiter(1, 1, clock.Now)

	if got := l.Keys(); got != 0 {
		t.Fatalf("Keys() before any Allow = %d, want 0", got)
	}

	if !l.Allow("a") {
		t.Fatal("first request for key a should be allowed")
	}
	if l.Allow("a") {
		t.Fatal("second immediate request for key a should be blocked (capacity 1)")
	}
	if got := l.Keys(); got != 1 {
		t.Fatalf("Keys() after touching only key a = %d, want 1", got)
	}

	// key b has never been touched; its bucket must be independent (full),
	// not shared with a's exhausted bucket.
	if !l.Allow("b") {
		t.Fatal("first request for a never-touched key b should be allowed")
	}
	if got := l.Keys(); got != 2 {
		t.Fatalf("Keys() after touching a and b = %d, want 2", got)
	}
}

func TestLimiterConcurrentAllowSameKeyRespectsCapacity(t *testing.T) {
	clock := NewManualClock(time.Unix(0, 0))
	l := NewLimiter(20, 1, clock.Now) // no refill during the test: clock never advances

	const callers = 50
	var allowed int64
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow("shared") {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&allowed); got != 20 {
		t.Fatalf("allowed = %d, want exactly 20 (the bucket capacity)", got)
	}
	if got := l.Keys(); got != 1 {
		t.Fatalf("Keys() = %d, want 1: all goroutines shared one lazily-created bucket", got)
	}
}
```

## Review

`TestLimiterConcurrentAllowSameKeyRespectsCapacity` is the test that would
catch a `Bucket.Allow` that only locked part of its critical section: with
50 goroutines racing to consume from a 20-token bucket and no refill during
the test, `-race` must stay clean and the allowed count must land on exactly
20 — not 19 from a lost update, not 21 from two goroutines both reading
"available" before either decremented. `TestLimiterLazyPerKeyIndependentBuckets`
is the lazy-per-key proof: `Keys()` only ever counts keys that have actually
called `Allow`, and touching one key's bucket to exhaustion has zero effect
on a different, never-touched key.

The mistake to avoid is calling `time.Now()` anywhere in this code path
outside of the production default: every test and the demo route time
through `ManualClock`, which is what makes `TestBucketRefillsOverTime`
deterministic rather than dependent on how much wall-clock time a slow test
runner actually took between two calls.

## Resources

- [sync.OnceValue](https://pkg.go.dev/sync#OnceValue) — lazily builds and caches each key's Bucket exactly once, even under concurrent first access.
- [Wikipedia — Token bucket](https://en.wikipedia.org/wiki/Token_bucket) — the refill/consume algorithm `Bucket.Allow` implements.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-graceful-shutdown-handler-collection-stack.md](23-graceful-shutdown-handler-collection-stack.md) | Next: [25-structured-logging-format-string-parser.md](25-structured-logging-format-string-parser.md)
