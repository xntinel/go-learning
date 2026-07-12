# Exercise 2: Fixed-Window Rate Limiter: map[string]counter with comma-ok window checks

A per-client fixed-window rate limiter is the shape behind API-key and per-IP
throttling. This exercise builds one over a `map[string]counter`, exploiting that
the zero value of an absent counter is already the correct starting point, and
uses an injected clock so window rollover is tested deterministically without
sleeping.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
ratelimit/                 independent module: example.com/ratelimit
  go.mod
  limiter.go               type Limiter; New, NewWithClock, Allow, Prune (map[string]counter, injected clock)
  cmd/
    demo/
      main.go              runnable demo: N pass, N+1 rejected, window resets, clients independent
  limiter_test.go          under/over limit, window reset, client isolation, prune, -race
```

- Files: `limiter.go`, `cmd/demo/main.go`, `limiter_test.go`.
- Implement: `Allow(clientID) bool` that increments the client's counter in the current wall-clock window and reports whether the request is at or under the limit; `Prune()` to drop counters from past windows.
- Test: N requests pass and N+1 is rejected, a new window resets the count, distinct clients are independent, `Prune` drops stale windows — all with an injected clock — plus concurrent `Allow` under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/02-fixed-window-rate-limiter/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/02-fixed-window-rate-limiter
```

### Why the zero value is the starting counter, and why the clock is injected

The limiter keeps `map[string]counter`, where a `counter` records the window it
belongs to and how many requests it has seen in that window. The map exploits a
core map fact: reading an absent key yields the value type's zero, and here the
zero `counter{}` is *already* the correct state for a client that has never been
seen — its `windowStart` is the zero `time.Time`, which never equals a real
truncated window, so the first `Allow` naturally resets it to the current window
with count `0` before incrementing. No per-client initialization is needed;
`l.counters[clientID]` on a missing key does the right thing.

Windows are keyed by truncating the current time: `now.Truncate(window)` maps
every instant to the start of its window bucket, so all requests in the same
minute share one bucket and the next minute is a distinct one. `Allow` reads the
client's counter with comma-ok, resets it when the stored `windowStart` no longer
equals the current bucket (comma-ok distinguishes "never seen" from "seen in an
old window", though both take the reset branch), increments, writes back, and
returns whether the count is within the limit.

The clock is the second design point. Testing a wall-clock rate limiter by
sleeping past a window is slow and flaky under CI load. Instead the limiter reads
time through an injected `now func() time.Time`. `New` wires it to `time.Now` for
production; `NewWithClock` lets a test (or the demo) supply a controllable clock,
so window rollover is asserted deterministically by advancing a variable, not by
waiting.

Create `limiter.go`:

```go
package ratelimit

import (
	"sync"
	"time"
)

type counter struct {
	windowStart time.Time
	count       int
}

// Limiter is a per-client fixed-window rate limiter. It admits up to limit
// requests per window per client. It is safe for concurrent use.
type Limiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	now      func() time.Time
	counters map[string]counter
}

// New returns a Limiter that admits limit requests per window per client,
// reading the wall clock through time.Now.
func New(limit int, window time.Duration) *Limiter {
	return NewWithClock(limit, window, time.Now)
}

// NewWithClock is New with an injected clock, for deterministic tests.
func NewWithClock(limit int, window time.Duration, now func() time.Time) *Limiter {
	return &Limiter{
		limit:    limit,
		window:   window,
		now:      now,
		counters: make(map[string]counter),
	}
}

// Allow records one request for clientID in the current window and reports
// whether it is within the limit. Requests beyond the limit return false but
// are still counted (the window must roll over before the client is admitted).
func (l *Limiter) Allow(clientID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket := l.now().Truncate(l.window)
	c, ok := l.counters[clientID]
	if !ok || !c.windowStart.Equal(bucket) {
		c = counter{windowStart: bucket, count: 0}
	}
	c.count++
	l.counters[clientID] = c
	return c.count <= l.limit
}

// Prune drops counters whose window is not the current one, reclaiming memory
// for clients that went idle. It returns the number of counters removed.
func (l *Limiter) Prune() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket := l.now().Truncate(l.window)
	removed := 0
	for id, c := range l.counters {
		if !c.windowStart.Equal(bucket) {
			delete(l.counters, id)
			removed++
		}
	}
	return removed
}

// Len reports how many client counters are currently tracked.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.counters)
}
```

### The runnable demo

The demo injects a controllable clock so its output is deterministic: it fires
four requests against a limit of three (the fourth is rejected), advances the
clock past the window to see the count reset, and shows a second client is
tracked independently.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ratelimit"
)

func main() {
	cur := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return cur }

	l := ratelimit.NewWithClock(3, time.Minute, clock)

	for i := 1; i <= 4; i++ {
		fmt.Printf("api-key-1 req %d: allowed=%v\n", i, l.Allow("api-key-1"))
	}

	cur = cur.Add(time.Minute) // roll into the next window
	fmt.Printf("after window: allowed=%v\n", l.Allow("api-key-1"))
	fmt.Printf("api-key-2 first: allowed=%v\n", l.Allow("api-key-2"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
api-key-1 req 1: allowed=true
api-key-1 req 2: allowed=true
api-key-1 req 3: allowed=true
api-key-1 req 4: allowed=false
after window: allowed=true
api-key-2 first: allowed=true
```

The fourth request is rejected because the client already used its three-per-minute
budget; advancing the clock one minute rolls into a fresh window and the count
resets, so the next request is admitted. `api-key-2` has its own counter and is
unaffected by `api-key-1`'s traffic.

### Tests

The tests drive time by mutating a variable the injected clock closes over, so
every window-dependent assertion is deterministic. `TestUnderAndOverLimit` proves
N pass and N+1 is rejected. `TestNewWindowResets` advances the clock and shows the
count starts over. `TestClientsIndependent` proves two clients do not share a
budget. `TestPruneDropsStaleWindows` proves `Prune` removes only past-window
counters. `TestConcurrentAllow` hammers `Allow` from many goroutines under
`-race` to prove the mutex guards the map.

Create `limiter_test.go`:

```go
package ratelimit

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func fixedClock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestUnderAndOverLimit(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewWithClock(3, time.Minute, fixedClock(&now))

	for i := 1; i <= 3; i++ {
		if !l.Allow("c") {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	if l.Allow("c") {
		t.Fatal("4th request should be rejected")
	}
}

func TestNewWindowResets(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewWithClock(2, time.Minute, fixedClock(&now))

	l.Allow("c")
	l.Allow("c")
	if l.Allow("c") {
		t.Fatal("should be rejected within the window")
	}

	now = now.Add(time.Minute) // new window
	if !l.Allow("c") {
		t.Fatal("new window should reset the count")
	}
}

func TestClientsIndependent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewWithClock(1, time.Minute, fixedClock(&now))

	if !l.Allow("a") {
		t.Fatal("a's first request should be allowed")
	}
	if !l.Allow("b") {
		t.Fatal("b's first request should be allowed independently of a")
	}
	if l.Allow("a") {
		t.Fatal("a's second request should be rejected")
	}
}

func TestPruneDropsStaleWindows(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewWithClock(5, time.Minute, fixedClock(&now))

	l.Allow("stale")
	now = now.Add(time.Minute) // move to a new window
	l.Allow("fresh")

	if got := l.Prune(); got != 1 {
		t.Fatalf("Prune removed %d, want 1 (the stale client)", got)
	}
	if got := l.Len(); got != 1 {
		t.Fatalf("Len after prune = %d, want 1", got)
	}
}

func TestConcurrentAllow(t *testing.T) {
	t.Parallel()

	l := New(1000, time.Minute)
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.Allow(fmt.Sprintf("client-%d", i%10))
		}(i)
	}
	wg.Wait()

	if got := l.Len(); got != 10 {
		t.Fatalf("Len() = %d, want 10 distinct clients", got)
	}
}

func ExampleLimiter_Allow() {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewWithClock(1, time.Minute, func() time.Time { return now })
	fmt.Println(l.Allow("c"), l.Allow("c"))
	// Output: true false
}
```

## Review

The limiter is correct when the admit decision is a pure function of the client's
count within the current truncated window: the first `limit` requests in a window
return true, the rest return false, and crossing into the next window resets the
count. The zero-value trick is load-bearing — `l.counters[clientID]` on an unseen
client yields `counter{}`, whose zero `windowStart` never equals a real bucket, so
the reset branch initializes it correctly without any explicit setup. The clock
injection is what makes the window tests deterministic; a version that called
`time.Now()` directly could only be tested by sleeping, which flakes. `Prune`
drops exactly the counters not in the current window, bounding memory for a limiter
that would otherwise accumulate a counter per client forever. Run `go test -race`
to confirm the mutex serializes concurrent `Allow` across clients.

## Resources

- [time.Time.Truncate](https://pkg.go.dev/time#Time.Truncate) — mapping an instant to its window bucket.
- [Go Specification: Map types](https://go.dev/ref/spec#Map_types) — zero-value reads and delete semantics.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the counter map.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-ttl-cache.md](01-ttl-cache.md) | Next: [03-set-membership-allowlist.md](03-set-membership-allowlist.md)
