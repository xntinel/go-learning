# Exercise 7: Per-client token-bucket limiter registry for an API gateway

An API gateway throttles each client independently — per API key, per source IP
— which means a map from client ID to rate-limiter state that every single
request must consult. This exercise applies the structure-lock-vs-value-lock
pattern to that map: an `RWMutex` guards the registry's *shape*, a small
per-bucket `sync.Mutex` guards each client's *state*, and the steady-state
`Allow` path for a known client never touches the registry write lock at all.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
keylimiter/                  independent module: example.com/keylimiter
  go.mod                     module example.com/keylimiter
  keylimiter.go              type Limiter (RWMutex + map[string]*bucket); Allow, Len
  cmd/
    demo/
      main.go                runnable demo: burst admitted, overflow denied
  keylimiter_test.go         burst/refill table tests, exact-count concurrency tests
```

Files: `keylimiter.go`, `cmd/demo/main.go`, `keylimiter_test.go`.
Implement: `Limiter` with `Allow(clientID) bool` — registry lookup under `RLock`, per-bucket `sync.Mutex` for the token math, `Lock` + double-check only to register a first-seen client; a hand-rolled token bucket with capacity, refill rate, and an injectable clock.
Test: a cold bucket admits exactly its burst capacity then denies; advancing the injected clock refills proportionally (table test); concurrent goroutines hammering distinct clients produce exact per-client counts under `-race`; a concurrent first hit on one new client creates exactly one bucket.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/keylimiter/cmd/demo
cd ~/go-exercises/keylimiter
go mod init example.com/keylimiter
```

### Two locks, two jobs

A naive registry puts one mutex around everything: every `Allow` from every
client serializes on it, and the limiter itself becomes the bottleneck it was
supposed to prevent. The senior topology separates concerns. The `RWMutex`
protects only the *map structure* — which client IDs have buckets. Each bucket
carries its own `sync.Mutex` protecting its token count and refill timestamp.
`Allow` for a known client is then: `RLock`, map lookup, `RUnlock`, lock *that
client's* bucket, do the token math, unlock. Two clients never contend with
each other (different bucket mutexes), and readers of the map never contend at
all. The registry write lock is touched exactly once per client lifetime — on
first sight, to insert the bucket — via the same release-then-`Lock`-then-
re-check dance as the read-through cache: between `RUnlock` and `Lock` another
goroutine may have registered the same client, and without the double-check you
would overwrite its bucket, silently resetting tokens it had already spent
(which means admitting more traffic than the limit allows — the test pins this).

Why a plain `Mutex` per bucket instead of an `RWMutex`? Because every `Allow`
*mutates* the bucket (it spends a token or refreshes the refill timestamp);
there is no read-only path to share, so the cheaper lock wins — the empirical
rule from the concepts file applied at small scale.

### The token bucket in four lines of arithmetic

Each bucket holds up to `capacity` tokens; a request costs one; tokens refill
continuously at `refillPerSec`. On every `Allow`, the bucket first credits
itself for the time since its last refill — `tokens = min(capacity,
tokens + elapsed.Seconds()*refillPerSec)` — then spends a token if at least one
is available. Continuous refill (fractions accumulate) is what the production
limiter `golang.org/x/time/rate` does; this hand-rolled version keeps the same
semantics with stdlib only so the locking topology stays in the foreground. The
clock is a `now func() time.Time` field: production uses `time.Now`, tests
advance a fake by exact durations and assert exact admission counts with no
sleeps and no flakiness. The `min` builtin (Go 1.21+) caps the credit — without
the cap, an idle client would bank unlimited burst.

Create `keylimiter.go`:

```go
package keylimiter

import (
	"sync"
	"time"
)

// bucket is one client's token-bucket state. Its own mutex guards the fields,
// so two clients never contend with each other.
type bucket struct {
	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
}

// Limiter is a per-client token-bucket registry. The RWMutex guards only the
// map structure; each bucket's mutex guards its own token math.
type Limiter struct {
	mu           sync.RWMutex
	buckets      map[string]*bucket
	capacity     float64
	refillPerSec float64
	now          func() time.Time
}

// New returns a Limiter where every client may burst up to capacity requests
// and thereafter refills at refillPerSec tokens per second.
func New(capacity int, refillPerSec float64) *Limiter {
	return newWithClock(capacity, refillPerSec, time.Now)
}

// newWithClock injects the clock so tests control refill without sleeping.
func newWithClock(capacity int, refillPerSec float64, now func() time.Time) *Limiter {
	return &Limiter{
		buckets:      make(map[string]*bucket),
		capacity:     float64(capacity),
		refillPerSec: refillPerSec,
		now:          now,
	}
}

// Allow reports whether one request from clientID may proceed, spending one
// token if so. For a known client it takes only the registry read lock plus
// that client's bucket mutex; the registry write lock is touched only the
// first time a client is seen.
func (l *Limiter) Allow(clientID string) bool {
	l.mu.RLock()
	b := l.buckets[clientID]
	l.mu.RUnlock()

	if b == nil {
		b = l.register(clientID)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := l.now()
	if elapsed := now.Sub(b.lastRefill); elapsed > 0 {
		b.tokens = min(l.capacity, b.tokens+elapsed.Seconds()*l.refillPerSec)
		b.lastRefill = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// register inserts a full bucket for a first-seen client. The re-check under
// the write lock is load-bearing: another goroutine may have registered the
// same client between our RUnlock and Lock, and overwriting its bucket would
// reset tokens it already spent.
func (l *Limiter) register(clientID string) *bucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[clientID]; ok {
		return b
	}
	b := &bucket{tokens: l.capacity, lastRefill: l.now()}
	l.buckets[clientID] = b
	return b
}

// Len reports how many distinct clients currently have a bucket.
func (l *Limiter) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.buckets)
}
```

### The runnable demo

A gateway admitting a burst of five requests from one client against a
capacity of three: the burst drains the bucket, the overflow is denied, and the
registry has tracked exactly one client.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/keylimiter"
)

func main() {
	l := keylimiter.New(3, 1) // burst 3, refill 1 token/s

	for i := 1; i <= 5; i++ {
		verdict := "denied"
		if l.Allow("10.0.0.1") {
			verdict = "allowed"
		}
		fmt.Printf("req %d for 10.0.0.1: %s\n", i, verdict)
	}
	fmt.Printf("distinct clients tracked: %d\n", l.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
req 1 for 10.0.0.1: allowed
req 2 for 10.0.0.1: allowed
req 3 for 10.0.0.1: allowed
req 4 for 10.0.0.1: denied
req 5 for 10.0.0.1: denied
distinct clients tracked: 1
```

### Tests

`TestColdBucketBurst` pins the burst contract with a frozen clock: exactly
`capacity` admissions, then denial. `TestRefillProportional` is the table test:
drain the bucket, advance the injected clock by an exact duration, and assert
the exact number of requests re-admitted — elapsed times refill rate, capped at
capacity. `TestConcurrentClientsExactCounts` uses refill rate zero so the math
is exact under concurrency: 32 goroutines hammer 8 distinct clients and each
client admits *exactly* its capacity, proving buckets are independent and no
tokens are lost or duplicated. `TestConcurrentFirstHit` releases 64 goroutines
at one brand-new client simultaneously: the double-check must leave exactly one
bucket and exactly `capacity` total admissions — a lost double-check overwrites
the bucket and over-admits.

Create `keylimiter_test.go`:

```go
package keylimiter

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestColdBucketBurst(t *testing.T) {
	t.Parallel()
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newWithClock(5, 10, func() time.Time { return frozen })

	admitted := 0
	for range 20 {
		if l.Allow("api-key-1") {
			admitted++
		}
	}
	if admitted != 5 {
		t.Fatalf("cold bucket admitted %d requests, want exactly capacity 5", admitted)
	}
}

func TestRefillProportional(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// capacity 10, refill 2 tokens/s: admitted after drain = floor(elapsed*2), capped at 10.
	tests := []struct {
		name    string
		elapsed time.Duration
		want    int
	}{
		{"no time passed", 0, 0},
		{"half second refills one", 500 * time.Millisecond, 1},
		{"one second refills two", time.Second, 2},
		{"three seconds refill six", 3 * time.Second, 6},
		{"long idle caps at capacity", 10 * time.Second, 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := base
			l := newWithClock(10, 2, func() time.Time { return now })

			for range 10 { // drain the initial burst
				if !l.Allow("client") {
					t.Fatal("initial burst denied before capacity was reached")
				}
			}
			now = now.Add(tc.elapsed)

			admitted := 0
			for range 20 {
				if l.Allow("client") {
					admitted++
				}
			}
			if admitted != tc.want {
				t.Fatalf("after %v idle, admitted %d, want %d", tc.elapsed, admitted, tc.want)
			}
		})
	}
}

func TestConcurrentClientsExactCounts(t *testing.T) {
	t.Parallel()
	const clients, perClient, capacity = 8, 4, 8
	l := New(capacity, 0) // refill 0: totals must be exact

	var counts [clients]atomic.Int64
	var wg sync.WaitGroup
	for c := range clients {
		id := fmt.Sprintf("client-%d", c)
		for range perClient {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 100 {
					if l.Allow(id) {
						counts[c].Add(1)
					}
				}
			}()
		}
	}
	wg.Wait()

	for c := range clients {
		if got := counts[c].Load(); got != capacity {
			t.Errorf("client-%d admitted %d requests, want exactly %d", c, got, capacity)
		}
	}
	if got := l.Len(); got != clients {
		t.Fatalf("registry tracks %d clients, want %d", got, clients)
	}
}

func TestConcurrentFirstHit(t *testing.T) {
	t.Parallel()
	const goroutines, capacity = 64, 8
	l := New(capacity, 0)

	var admitted atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if l.Allow("new-client") {
				admitted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := l.Len(); got != 1 {
		t.Fatalf("registry has %d buckets for one client, want 1", got)
	}
	if got := admitted.Load(); got != capacity {
		t.Fatalf("first-hit stampede admitted %d, want exactly %d (bucket overwritten?)", got, capacity)
	}
}

func Example() {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newWithClock(2, 1, func() time.Time { return now })

	fmt.Println(l.Allow("10.0.0.1"))
	fmt.Println(l.Allow("10.0.0.1"))
	fmt.Println(l.Allow("10.0.0.1"))

	now = now.Add(time.Second) // one token refills
	fmt.Println(l.Allow("10.0.0.1"))
	// Output:
	// true
	// true
	// false
	// true
}
```

## Review

The design point is the lock topology, not the arithmetic: the registry
`RWMutex` is only about which clients exist, each bucket's `Mutex` is only about
that client's tokens, and `Allow` on the steady path holds them one at a time,
briefly, never nested the dangerous way (bucket mutex is acquired only after the
registry lock is released). The classic mistakes: skipping the double-check in
`register` (a first-hit stampede overwrites a bucket that already spent tokens
and the service over-admits — `TestConcurrentFirstHit` catches it), doing the
token math under the registry `RLock` (mutation under a read lock — race), and
holding the registry `Lock` while computing refill (serializes all clients for
no reason). Note also the deliberate choice of a plain `Mutex` per bucket:
`Allow` always writes, so a per-bucket `RWMutex` would be pure overhead. In
production you would reach for `golang.org/x/time/rate.Limiter` per client and
keep exactly this registry pattern around it. Verify with
`go test -count=1 -race ./...` — every count assertion is exact, so any lost
update or duplicated bucket fails loudly.

## Resources

- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) — the production token-bucket limiter this module hand-rolls.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the structure lock; `RLock` for lookup, `Lock` + re-check for insert.
- [Token bucket](https://en.wikipedia.org/wiki/Token_bucket) — the algorithm: burst capacity plus continuous refill.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-single-flight-refresh-with-trylock.md](08-single-flight-refresh-with-trylock.md)
