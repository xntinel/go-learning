# Exercise 26: Sliding Window Rate Limiting With Concurrent Client Dispatch

**Nivel: Intermedio** — validacion rapida (un test corto).

A fixed-bucket rate limiter ("100 requests per minute, reset on the
minute") has a well-known hole: a client can send 100 requests in the last
second of one window and another 100 in the first second of the next,
bursting 200 requests in two seconds while never technically exceeding the
stated limit. A sliding window log closes that hole by tracking each
client's actual recent request timestamps and pruning them against a
moving cutoff instead of a fixed clock boundary. This module builds that
limiter, with a soft "burst" allowance between outright admission and
outright rejection, and a shared `Limiter` instance that many concurrent
clients dispatch requests against safely. It is self-contained: its own
`go mod init`, code, demo, and test.

## What you'll build

```text
slidingwindow/             independent module: example.com/sliding-window-rate-limiter
  go.mod                    go 1.24
  slidingwindow.go           package slidingwindow; Decision; Limiter; New(limit, burst, window) *Limiter; Allow(client, now) Decision
  cmd/demo/main.go           runnable demo: sequential single-client requests, then concurrent dispatch
  slidingwindow_test.go      sequential window table, client independence, concurrent same-client dispatch
```

- Implement: `Allow(client string, now time.Time) Decision` — prunes each client's timestamp history against `now - window`, then a switch-with-init over the pruned count decides `PASS` / `QUEUED` (burst allowance) / `RATE_LIMITED`.
- Test: a sequential table crossing the pass/queue/reject boundaries and the window reset, a check that clients are independent, and a concurrent-dispatch subtest asserting the aggregate outcome under the mutex.
- Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/slidingwindow/cmd/demo
cd ~/go-exercises/slidingwindow
go mod init example.com/sliding-window-rate-limiter
go mod edit -go=1.24
```

### Why a switch-with-init over the pruned count

`Allow` first prunes each client's history down to the timestamps still
inside the window, then has to make a three-way decision based on how many
survived. Binding that count with `switch count := len(kept); {` keeps the
name scoped to exactly the three cases that need it — nothing before the
switch needs it, and nothing after `Allow` returns should be able to touch
it. The tagless form is the right one here because the decision is a pair
of ordered numeric thresholds (`count < limit`, `count < limit+burst`), not
an equality test against a closed set of values.

The caller supplies `now` explicitly rather than `Allow` calling
`time.Now()` internally. This matters twice: it lets a batch of concurrent
callers share one exact instant so the aggregate outcome of a race is
computable in a test, and it lets the sequential table below drive the
window boundary to the millisecond without a real sleep. A limiter that
reads the clock itself is much harder to test at the boundary — you either
sleep in the test (slow and still slightly imprecise) or you don't test the
boundary at all.

The mutex-protected map is a shared resource: every client's history lives
in the same `map[string][]time.Time` under one `sync.Mutex`, so two
goroutines dispatching for different clients still serialize on the map
access itself (Go maps are not safe for concurrent read/write, regardless
of whether the keys differ), and two goroutines dispatching for the *same*
client serialize on both the map and the correctness of the count they see
— the classic case where "read the count, then decide" must happen inside
one critical section, never as two separate locked steps.

Create `slidingwindow.go`:

```go
// Package slidingwindow implements request admission control using a
// sliding window log: each client's own recent request timestamps are kept
// and pruned against a moving cutoff, rather than a fixed-bucket counter
// that resets on a clock boundary and lets a burst straddle two buckets
// slip through at double the intended rate.
package slidingwindow

import (
	"sync"
	"time"
)

// Decision is the outcome of an admission check.
type Decision string

const (
	Pass        Decision = "PASS"
	Queued      Decision = "QUEUED"
	RateLimited Decision = "RATE_LIMITED"
)

// Limiter admits up to limit requests per client within window, plus an
// additional burst requests that are admitted but flagged Queued rather
// than rejected outright -- a soft overflow zone many production limiters
// use so a client that's one request over quota degrades instead of
// failing hard. Requests beyond limit+burst are RateLimited.
type Limiter struct {
	mu      sync.Mutex
	limit   int
	burst   int
	window  time.Duration
	history map[string][]time.Time
}

// New builds a Limiter admitting limit requests per window per client, with
// burst additional requests queued instead of rejected.
func New(limit, burst int, window time.Duration) *Limiter {
	return &Limiter{
		limit:   limit,
		burst:   burst,
		window:  window,
		history: make(map[string][]time.Time),
	}
}

// Allow classifies one request from client arriving at now. The caller
// supplies now explicitly -- rather than Allow calling time.Now() itself --
// so a batch of concurrent callers can share one instant and so tests can
// drive the window boundary exactly.
func (l *Limiter) Allow(client string, now time.Time) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-l.window)
	prior := l.history[client]
	kept := make([]time.Time, 0, len(prior))
	for _, t := range prior {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	switch count := len(kept); {
	case count < l.limit:
		l.history[client] = append(kept, now)
		return Pass
	case count < l.limit+l.burst:
		l.history[client] = append(kept, now)
		return Queued
	default:
		l.history[client] = kept
		return RateLimited
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"time"

	slidingwindow "example.com/sliding-window-rate-limiter"
)

func main() {
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	limiter := slidingwindow.New(3, 2, time.Minute)

	fmt.Println("-- single client, sequential requests --")
	for i := 0; i < 6; i++ {
		at := start.Add(time.Duration(i) * 10 * time.Second)
		decision := limiter.Allow("client-a", at)
		fmt.Printf("request %d at +%ds -> %s\n", i+1, i*10, decision)
	}

	fmt.Println("-- concurrent dispatch, one client, five simultaneous requests --")
	now := start.Add(time.Hour)
	const n = 5
	decisions := make([]slidingwindow.Decision, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			decisions[i] = limiter.Allow("shared-client", now)
		}(i)
	}
	wg.Wait()

	var passCount, queuedCount, rateLimitedCount int
	for _, d := range decisions {
		switch d {
		case slidingwindow.Pass:
			passCount++
		case slidingwindow.Queued:
			queuedCount++
		case slidingwindow.RateLimited:
			rateLimitedCount++
		}
	}
	fmt.Printf("pass=%d queued=%d rate_limited=%d (out of %d concurrent requests)\n", passCount, queuedCount, rateLimitedCount, n)

	fmt.Println("-- concurrent dispatch, independent clients --")
	other := limiter.Allow("client-other", now)
	fmt.Printf("client-other -> %s\n", other)
}
```

Run `go run ./cmd/demo`, expected output:

```
-- single client, sequential requests --
request 1 at +0s -> PASS
request 2 at +10s -> PASS
request 3 at +20s -> PASS
request 4 at +30s -> QUEUED
request 5 at +40s -> QUEUED
request 6 at +50s -> RATE_LIMITED
-- concurrent dispatch, one client, five simultaneous requests --
pass=3 queued=2 rate_limited=0 (out of 5 concurrent requests)
-- concurrent dispatch, independent clients --
client-other -> PASS
```

### Tests

`TestAllowSequentialWindow` drives the pass, burst-queue, and hard-reject
boundaries plus a full window reset. `TestAllowClientsAreIndependent`
confirms one client's history never affects another's count.
`TestAllowConcurrentDispatchSameClient` fires five goroutines at the same
client and same instant; per-goroutine attribution isn't deterministic, but
the aggregate — exactly three `Pass` and exactly two `Queued` — is,
because the mutex serializes every check-and-record into one consistent
sequence regardless of scheduling order.

Create `slidingwindow_test.go`:

```go
package slidingwindow

import (
	"sync"
	"testing"
	"time"
)

func TestAllowSequentialWindow(t *testing.T) {
	t.Parallel()

	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New(2, 1, time.Minute)

	tests := []struct {
		name   string
		offset time.Duration
		want   Decision
	}{
		{"first request passes", 0, Pass},
		{"second request within limit passes", 10 * time.Second, Pass},
		{"third request uses the burst allowance", 20 * time.Second, Queued},
		{"fourth request exceeds limit plus burst", 30 * time.Second, RateLimited},
		{"request after the window fully elapses passes again", 2 * time.Minute, Pass},
	}

	for _, tc := range tests {
		got := l.Allow("client", start.Add(tc.offset))
		if got != tc.want {
			t.Errorf("%s: Allow() = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestAllowClientsAreIndependent(t *testing.T) {
	t.Parallel()

	now := time.Now()
	l := New(1, 0, time.Minute)

	if got := l.Allow("a", now); got != Pass {
		t.Fatalf("client a first request = %s, want Pass", got)
	}
	if got := l.Allow("b", now); got != Pass {
		t.Fatalf("client b first request = %s, want Pass (independent of a)", got)
	}
	if got := l.Allow("a", now); got != RateLimited {
		t.Fatalf("client a second request = %s, want RateLimited", got)
	}
}

// TestAllowConcurrentDispatchSameClient fires five simultaneous requests for
// the same client at a limiter configured for limit=3, burst=2. Which
// goroutine receives which decision is not deterministic, but the aggregate
// outcome is: the mutex serializes every check-and-record, so exactly three
// requests pass and exactly two land in the burst queue, never more.
func TestAllowConcurrentDispatchSameClient(t *testing.T) {
	t.Parallel()

	l := New(3, 2, time.Minute)
	now := time.Now()

	const n = 5
	decisions := make([]Decision, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			decisions[i] = l.Allow("shared-client", now)
		}(i)
	}
	wg.Wait()

	var passCount, queuedCount, rateLimitedCount int
	for _, d := range decisions {
		switch d {
		case Pass:
			passCount++
		case Queued:
			queuedCount++
		case RateLimited:
			rateLimitedCount++
		}
	}
	if passCount != 3 || queuedCount != 2 || rateLimitedCount != 0 {
		t.Fatalf("got pass=%d queued=%d rateLimited=%d, want pass=3 queued=2 rateLimited=0",
			passCount, queuedCount, rateLimitedCount)
	}
}
```

Verify with:

```bash
go test -race -count=1 ./...
```

## Review

The limiter is correct when pruning happens against `now - window` on
every call (not on a timer), when the burst allowance sits strictly
between `limit` and `limit+burst` rather than blurring into either
neighbor, and when concurrent dispatch against the same client never lets
the aggregate admitted count exceed `limit+burst` no matter how the
goroutines interleave. Carry this forward: whenever a switch's decision
depends on a value computed just before it (a pruned count, a parsed
field, a resolved tier), reach for the switch-with-init form so that value
never outlives the branch that needs it, and whenever multiple goroutines
share one piece of state, the read that decides the outcome and the write
that records it belong in the same critical section.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the switch init-statement and the tagless form.
- [Cloudflare: How to build a rate limiter](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) — sliding window vs. fixed window in a production rate limiter.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding shared state across concurrent client dispatch.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-protocol-version-router-with-cascading-fallback.md](25-protocol-version-router-with-cascading-fallback.md) | Next: [27-load-shedding-admission-controller.md](27-load-shedding-admission-controller.md)
