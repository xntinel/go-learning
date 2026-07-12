# Exercise 8: Token-Bucket Rate Limiter: Allow/Reject Decision

Every API that enforces a quota runs an admission decision per request: is this
caller under their limit? The token bucket is the standard primitive — a bucket
refills at a steady rate and each request costs one token. This module builds
`Allow(key) bool` as a compact refill-then-decide `if`, with a comma-ok map to lazily
create a per-caller bucket and an injected clock so the refill is deterministic.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
tokenbucket/                independent module: example.com/tokenbucket
  go.mod                    go 1.26
  limiter.go                Limiter: New(capacity, refill, clock), Allow(key)
  cmd/
    demo/
      main.go               burst until rejected, advance clock, one more allowed
  limiter_test.go           burst, refill, per-key isolation, cap clamp, -race
```

- Files: `limiter.go`, `cmd/demo/main.go`, `limiter_test.go`.
- Implement: a per-key token bucket `Allow(key) bool` that refills lazily from elapsed time (init-statement `if` computing tokens), then admits with `if b.tokens < 1 { return false }` else consumes; comma-ok map creates a bucket per key.
- Test: a burst of `capacity` requests all allowed, the next rejected; after advancing the clock enough to refill one token, one more allowed; independent keys have independent buckets; tokens never exceed capacity after a long idle gap; concurrent `-race` test showing no over-admission.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/01-if-else-and-init-statements/08-token-bucket-admission/cmd/demo
cd go-solutions/03-control-flow/01-if-else-and-init-statements/08-token-bucket-admission
```

## Refill lazily, then decide

A naive limiter runs a background goroutine that adds tokens on a ticker. The lazy
token bucket is simpler and race-free by construction: it stores, per key, the token
count and the last time it was touched, and it computes the refill *on demand* the
moment a request arrives. That collapses the whole limiter into one function with no
background goroutine.

`Allow` does three things under the lock:

1. Look up or lazily create the bucket: `b, ok := buckets[key]; if !ok { create full }`.
   A new caller starts with a full bucket (`capacity` tokens), so the first burst up
   to capacity is admitted.
2. Refill from elapsed time. `elapsed := now.Sub(b.last)` and
   `b.tokens = min(capacity, b.tokens + elapsed/refillInterval)`. The `min` clamp is
   essential: a caller idle for an hour must not accumulate an hour of tokens and be
   allowed a giant burst — the bucket caps at capacity.
3. Decide: `if b.tokens < 1 { return false }` else consume one token and return true.

The clock is injected (`now func() time.Time`) so the refill is deterministic in
tests: advance the clock by exactly one refill interval and assert exactly one more
request is admitted. The whole method holds one mutex, because the refill computation
and the token decrement are a read-modify-write on shared state; without the lock two
concurrent requests could both read `tokens == 1` and both consume it, over-admitting
past the limit. The concurrency test proves admissions never exceed the tokens
available in the window.

Create `limiter.go`:

```go
package tokenbucket

import (
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a per-key lazy token-bucket rate limiter.
type Limiter struct {
	mu       sync.Mutex
	capacity float64
	refill   time.Duration // time to regain one token
	now      func() time.Time
	buckets  map[string]*bucket
}

// New returns a limiter allowing bursts up to capacity, refilling one token every
// refill interval. Pass time.Now as clock in production.
func New(capacity int, refill time.Duration, clock func() time.Time) *Limiter {
	return &Limiter{
		capacity: float64(capacity),
		refill:   refill,
		now:      clock,
		buckets:  make(map[string]*bucket),
	}
}

// Allow reports whether a request for key is admitted, consuming a token if so.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	t := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.capacity, last: t}
		l.buckets[key] = b
	}

	if elapsed := t.Sub(b.last); elapsed > 0 {
		b.tokens += float64(elapsed) / float64(l.refill)
		if b.tokens > l.capacity {
			b.tokens = l.capacity
		}
		b.last = t
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
```

### The runnable demo

The demo creates a limiter with capacity 3 and a 1-second refill, bursts four
requests (three allowed, one rejected), advances the clock one second, and shows one
more request admitted.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/tokenbucket"
)

func main() {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lim := tokenbucket.New(3, time.Second, func() time.Time { return clock })

	for i := 1; i <= 4; i++ {
		fmt.Printf("burst %d: allow=%v\n", i, lim.Allow("client-1"))
	}

	clock = clock.Add(time.Second) // refill one token
	fmt.Printf("after 1s refill: allow=%v\n", lim.Allow("client-1"))

	fmt.Printf("other key: allow=%v\n", lim.Allow("client-2"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
burst 1: allow=true
burst 2: allow=true
burst 3: allow=true
burst 4: allow=false
after 1s refill: allow=true
other key: allow=true
```

### Tests

The tests drive time through a clock variable. They assert a full burst is admitted
and the next rejected, that advancing one refill interval admits exactly one more,
that two keys have independent buckets, that a long idle gap does not let tokens
exceed capacity, and a concurrent hammer under `-race` never over-admits.

Create `limiter_test.go`:

```go
package tokenbucket

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func clock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestBurstThenReject(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	lim := New(3, time.Second, clock(&now))
	for i := range 3 {
		if !lim.Allow("k") {
			t.Fatalf("burst request %d rejected; want allowed", i)
		}
	}
	if lim.Allow("k") {
		t.Fatal("4th request allowed; want rejected")
	}
}

func TestRefillAdmitsOneMore(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	lim := New(3, time.Second, clock(&now))
	for range 3 {
		lim.Allow("k")
	}
	if lim.Allow("k") {
		t.Fatal("request allowed before refill; want rejected")
	}
	now = now.Add(time.Second)
	if !lim.Allow("k") {
		t.Fatal("request rejected after one refill interval; want allowed")
	}
	if lim.Allow("k") {
		t.Fatal("second request after single refill allowed; want rejected")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	lim := New(1, time.Second, clock(&now))
	if !lim.Allow("a") {
		t.Fatal("first request for a rejected")
	}
	if !lim.Allow("b") {
		t.Fatal("first request for b rejected; keys should be independent")
	}
}

func TestTokensClampedToCapacity(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	lim := New(2, time.Second, clock(&now))
	lim.Allow("k")           // create the bucket
	now = now.Add(time.Hour) // idle a long time
	// Bucket must not exceed capacity: only 2 requests admitted, not thousands.
	admitted := 0
	for range 10 {
		if lim.Allow("k") {
			admitted++
		}
	}
	if admitted != 2 {
		t.Fatalf("admitted %d after long idle; want 2 (clamped to capacity)", admitted)
	}
}

func TestConcurrentNoOverAdmission(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	lim := New(50, time.Hour, clock(&now)) // no refill within the test
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lim.Allow("k") {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 50 {
		t.Fatalf("admitted %d; want exactly 50 (capacity)", admitted.Load())
	}
}
```

## Review

The limiter is correct when a fresh key bursts up to capacity, the next request is
rejected, one refill interval admits exactly one more, keys are isolated, and an idle
bucket clamps at capacity rather than hoarding tokens. The injected clock makes refill
deterministic — never sleep to force a refill. The mistakes to avoid are omitting the
`min` clamp (an idle caller then gets a huge burst), computing the refill and the
decrement outside the lock (two requests both consume the last token — over-admission,
which the concurrency test catches under `-race`), and running a background refill
goroutine when lazy on-demand refill is simpler and race-free. Production limiters
(`golang.org/x/time/rate`) use the same lazy math; the standard library's is worth
reaching for in real services, but building one is how you understand the decision.

## Resources

- [Token bucket algorithm](https://en.wikipedia.org/wiki/Token_bucket)
- [golang.org/x/time/rate.Limiter](https://pkg.go.dev/golang.org/x/time/rate#Limiter)
- [time.Time.Sub](https://pkg.go.dev/time#Time.Sub)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-optimistic-lock-update.md](07-optimistic-lock-update.md) | Next: [09-readiness-health-aggregator.md](09-readiness-health-aggregator.md)
