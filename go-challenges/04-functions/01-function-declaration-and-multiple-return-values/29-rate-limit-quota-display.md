# Exercise 29: Token-Bucket Rate Limiter With Quota Status

**Nivel: Intermedio** — validacion rapida (un test corto).

A rate limiter that only returns `bool` forces every caller to guess how
close a client is to being throttled and when it is safe to retry. Real API
gateways expose that as `X-RateLimit-Remaining` and `X-RateLimit-Reset`
headers. This exercise builds a token-bucket `Limiter.Check(key) (allowed
bool, tokensRemaining int, resetAt time.Time)` so a handler can both enforce
the limit and hand the client exactly the backpressure information it needs
to back off correctly.

This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
ratelimit/                 independent module: example.com/rate-limit-quota-display
  go.mod                   go 1.24
  ratelimit.go             package ratelimit; Clock; Limiter; NewLimiter; Check(key) (allowed,tokensRemaining,resetAt)
  cmd/
    demo/
      main.go              four rapid requests against a 3-token bucket
  ratelimit_test.go         drain-then-throttle sequence with a fake clock; resetAt math; independent per-key buckets
```

- Files: `ratelimit.go`, `cmd/demo/main.go`, `ratelimit_test.go`.
- Implement: `(*Limiter).Check(key string) (allowed bool, tokensRemaining int, resetAt time.Time)` refilling a per-key token bucket continuously based on elapsed time from an injected `Clock`, consuming one token per allowed request, and reporting the time the bucket will be full again.
- Test: a bucket of 3 tokens allows exactly 3 requests back-to-back and throttles the 4th; advancing the fake clock by one refill interval allows exactly one more request; `resetAt` equals `now + (capacity - tokensRemaining) / refillPerSecond`; two different keys never share a bucket.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ratelimit/cmd/demo
cd ~/go-exercises/ratelimit
go mod init example.com/rate-limit-quota-display
go mod edit -go=1.24
```

### Why `bool` alone starves the client of the information it needs

A client that gets back a bare `false` from a rate limiter has exactly one
strategy available: guess a retry delay, or hammer the endpoint again
immediately. Neither is good — guessing too short wastes a request and
counts against the client's own reputation with the server; guessing too
long leaves throughput on the table once the bucket has actually refilled.
`resetAt` removes the guessing entirely: the client sleeps until that exact
instant and then retries with a real chance of succeeding.
`tokensRemaining` matters even on the *allowed* path — a client watching
this number count down toward zero across several calls can proactively
slow itself down before it ever gets throttled, which is exactly what a
well-behaved API consumer respecting `X-RateLimit-Remaining` does today.

The token bucket itself refills continuously rather than resetting in
discrete windows (unlike a fixed-window counter), computed from elapsed
time since the bucket's last touch: `tokens = min(capacity, tokens +
elapsedSeconds * refillPerSecond)`. Driving that elapsed-time computation
from an injected `Clock` rather than `time.Now()` is what makes "advance by
exactly one second and expect exactly one more allowed request" a
deterministic assertion instead of a race against the test runner's actual
scheduling.

Create `ratelimit.go`:

```go
package ratelimit

import (
	"sync"
	"time"
)

// Clock abstracts time.Now so the limiter's refill math can be tested with
// exact, controlled time steps instead of real elapsed wall-clock time.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type bucket struct {
	tokens     float64
	lastRefill time.Time
}

// Limiter is a per-key token-bucket rate limiter. Capacity tokens refill
// continuously at refillPerSecond tokens/second, and each allowed request
// consumes exactly one token. It is safe for concurrent use.
type Limiter struct {
	mu              sync.Mutex
	capacity        float64
	refillPerSecond float64
	clock           Clock
	buckets         map[string]*bucket
}

// NewLimiter builds a limiter with capacity tokens per key, refilling at
// refillPerSecond tokens/second, using the real wall clock.
func NewLimiter(capacity int, refillPerSecond float64) *Limiter {
	return newLimiter(capacity, refillPerSecond, realClock{})
}

func newLimiter(capacity int, refillPerSecond float64, clock Clock) *Limiter {
	return &Limiter{
		capacity:        float64(capacity),
		refillPerSecond: refillPerSecond,
		clock:           clock,
		buckets:         make(map[string]*bucket),
	}
}

// Check consumes one token for key if one is available. It reports whether
// the request is allowed, how many whole tokens remain for the client to
// spend (for a quota-display header), and the time at which the bucket
// will be full again (for backpressure: "come back no sooner than this").
func (l *Limiter) Check(key string) (allowed bool, tokensRemaining int, resetAt time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.capacity, lastRefill: now}
		l.buckets[key] = b
	}

	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens = min(l.capacity, b.tokens+elapsed*l.refillPerSecond)
		b.lastRefill = now
	}

	if b.tokens >= 1 {
		b.tokens--
		allowed = true
	}

	secondsToFull := (l.capacity - b.tokens) / l.refillPerSecond
	resetAt = now.Add(time.Duration(secondsToFull * float64(time.Second)))

	return allowed, int(b.tokens), resetAt
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	ratelimit "example.com/rate-limit-quota-display"
)

func main() {
	limiter := ratelimit.NewLimiter(3, 1) // 3 tokens, refilling at 1/second

	for i := 1; i <= 4; i++ {
		allowed, remaining, _ := limiter.Check("client-42")
		fmt.Printf("request %d: allowed=%t tokensRemaining=%d\n", i, allowed, remaining)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request 1: allowed=true tokensRemaining=2
request 2: allowed=true tokensRemaining=1
request 3: allowed=true tokensRemaining=0
request 4: allowed=false tokensRemaining=0
```

### Tests

Create `ratelimit_test.go`:

```go
package ratelimit

import (
	"testing"
	"time"
)

// fakeClock lets the test advance time by exact, deterministic steps
// instead of sleeping for real durations.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func TestCheckTokenBucketSequence(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	limiter := newLimiter(3, 1, clock) // 3 tokens, refill 1/second

	cases := []struct {
		name          string
		advance       time.Duration
		wantAllowed   bool
		wantRemaining int
	}{
		{name: "1st request consumes a token", wantAllowed: true, wantRemaining: 2},
		{name: "2nd request consumes a token", wantAllowed: true, wantRemaining: 1},
		{name: "3rd request drains the bucket", wantAllowed: true, wantRemaining: 0},
		{name: "4th request is throttled", wantAllowed: false, wantRemaining: 0},
		{name: "after 1s, one token refilled and spent", advance: time.Second, wantAllowed: true, wantRemaining: 0},
		{name: "immediately throttled again", wantAllowed: false, wantRemaining: 0},
	}

	for _, tc := range cases {
		clock.Advance(tc.advance)
		allowed, remaining, _ := limiter.Check("client-1")
		if allowed != tc.wantAllowed {
			t.Fatalf("%s: allowed = %t, want %t", tc.name, allowed, tc.wantAllowed)
		}
		if remaining != tc.wantRemaining {
			t.Fatalf("%s: tokensRemaining = %d, want %d", tc.name, remaining, tc.wantRemaining)
		}
	}
}

func TestCheckResetAtReflectsTimeToFull(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(2000, 0)}
	limiter := newLimiter(3, 1, clock)

	// Drain the bucket completely.
	for i := 0; i < 3; i++ {
		if allowed, _, _ := limiter.Check("client-2"); !allowed {
			t.Fatalf("drain request %d: want allowed", i)
		}
	}

	allowed, remaining, resetAt := limiter.Check("client-2")
	if allowed {
		t.Fatal("want the request to be throttled once the bucket is empty")
	}
	if remaining != 0 {
		t.Fatalf("tokensRemaining = %d, want 0", remaining)
	}
	want := clock.now.Add(3 * time.Second)
	if !resetAt.Equal(want) {
		t.Fatalf("resetAt = %v, want %v (3 empty slots / 1 token per second)", resetAt, want)
	}
}

func TestCheckIndependentKeys(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(3000, 0)}
	limiter := newLimiter(1, 1, clock)

	if allowed, _, _ := limiter.Check("a"); !allowed {
		t.Fatal("key a: want first request allowed")
	}
	if allowed, _, _ := limiter.Check("a"); allowed {
		t.Fatal("key a: want second request throttled")
	}
	if allowed, _, _ := limiter.Check("b"); !allowed {
		t.Fatal("key b: a different key must have its own untouched bucket")
	}
}
```

## Review

`Check` is correct when the three returns stay in lockstep: `allowed`
reflects whether a token was actually consumed this call, `tokensRemaining`
reflects the bucket's state immediately after that consumption (not
before), and `resetAt` is derived from the same post-consumption state, not
recomputed from stale numbers. `TestCheckTokenBucketSequence` is the
load-bearing test — it walks the exact sequence a client hits in practice:
drain the bucket, get throttled, wait out one refill interval, get exactly
one more request through.

The mistake to avoid is refilling the bucket based on a fixed tick (a
background goroutine incrementing tokens every second) instead of lazily
computing the refill from elapsed time on each `Check` call. A tick-based
design either needs a goroutine per bucket (which never gets cleaned up
for keys that stop appearing) or a global sweep that still has to do the
same elapsed-time math this lazy version does directly — with none of the
extra bookkeeping.

## Resources

- [Wikipedia: Token bucket](https://en.wikipedia.org/wiki/Token_bucket) — the refill-and-consume model this limiter implements.
- [golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate) — the standard Go token-bucket limiter; `Reserve`/`Allow` mirror this exercise's `Check`.
- [IETF draft: RateLimit header fields](https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers) — the `remaining`/`reset` vocabulary this exercise's return values expose.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-postgres-pool-acquire-metrics.md](28-postgres-pool-acquire-metrics.md) | Next: [30-s3-object-metadata-lookup.md](30-s3-object-metadata-lookup.md)
