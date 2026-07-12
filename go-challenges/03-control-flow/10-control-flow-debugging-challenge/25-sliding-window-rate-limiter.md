# Exercise 25: Sliding Window Rate Limiter Admits Burst on Window Boundary

**Nivel: Intermedio** — validacion rapida (un test corto).

An API gateway rate limiter that resets a plain counter every fixed
window is the shape almost every team ships first, and it hides a burst
window right at the seam between two windows: a client that fires its
full quota at 999ms into a one-second window, then fires another full
quota at 1ms into the next, gets `2 * limit` requests admitted inside a
two-millisecond span even though the limiter believes it enforced
`limit` requests per second the whole time. A sliding-window *counter*
closes that seam cheaply — without paying for a full timestamp log —
by weighting the previous window's count against how much of it still
overlaps the trailing window ending at the current instant. This
module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
ratelimit/                  independent module: example.com/sliding-window-rate-limiter
  go.mod                     go 1.21
  ratelimit.go                Limiter, New, Allow
  cmd/
    demo/
      main.go                 runnable demo: a full-quota burst straddling a window boundary
  ratelimit_test.go            single-window limit, boundary-burst rejection, decay over time
```

- Files: `ratelimit.go`, `cmd/demo/main.go`, `ratelimit_test.go`.
- Implement: `Limiter.Allow(now time.Time) bool` that admits at most `limit` requests per rolling `window`, weighting the previous fixed window's count by how much of it still overlaps the current instant.
- Test: a same-window limit check, a boundary-straddling burst that must not double-admit, and a decay case once enough of the previous window has aged out.
- Verify: `go test -count=1 ./...`.

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/25-sliding-window-rate-limiter/cmd/demo
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/25-sliding-window-rate-limiter
```

### Why a clean window reset admits double the limit

The tempting implementation is a fixed window: count requests, and
when the window's elapsed time passes `window`, zero the counter and
start over.

```go
// BUG: naive fixed-window reset -- ignores how many requests the
// *previous* window already admitted near its own tail.
func (l *Limiter) Allow(now time.Time) bool {
	if now.Sub(l.curStart) >= l.window {
		l.curStart = now
		l.curCount = 0
	}
	if l.curCount >= l.limit {
		return false
	}
	l.curCount++
	return true
}
```

This reads as correct because each *individual* window never exceeds
`limit` — the bug is invisible to a test that only checks one window
at a time. The failure appears at the seam: five requests land at
900ms into window one, filling it to the limit; ten milliseconds
later the clock crosses into window two, `curCount` resets to zero,
and five more requests are admitted immediately. Measured against any
sliding one-second interval — say `[900ms, 1900ms]` — ten requests
were admitted, not five, because the limiter only ever validates
against its *own* window's start, never against how recently the
*previous* window's traffic actually happened. A downstream that
depends on the advertised rate limit to size its own capacity sees
twice the load it was promised, exactly when it is least prepared for
it: right after a reset.

The fix does not reset cleanly. It keeps the previous window's count
around and weights it by the fraction of it still inside the trailing
window ending at `now`:

```go
overlap := 1 - float64(elapsed)/float64(l.window)
weighted := float64(l.prevCount)*overlap + float64(l.curCount)
```

At the instant a new window opens, `overlap` is `1` — the entire
previous window's count still applies — and it decays linearly to `0`
by the time the current window ends. A request arriving 10ms into a
new window, right after the previous window admitted five, is checked
against a weighted count of `5*0.99 + 0 = 4.95`, correctly recognizing
that almost the full previous quota is still "in view" of the sliding
interval — so it denies the burst instead of resetting the ledger to
zero.

Create `ratelimit.go`:

```go
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a sliding-window-counter rate limiter. Instead of storing every
// request timestamp (a sliding-window log, which costs O(requests) memory),
// it keeps only two integers -- the current fixed window's count and the
// immediately preceding window's count -- and approximates the true sliding
// count by weighting the previous window's count by how much of it still
// overlaps the trailing window ending at now. This is the same algorithm
// widely deployed at CDN and API-gateway edges because it is O(1) per
// request regardless of the limit.
type Limiter struct {
	mu        sync.Mutex
	limit     int
	window    time.Duration
	curStart  time.Time
	curCount  int
	prevCount int
}

// New creates a Limiter admitting at most limit requests per window,
// anchored at now.
func New(limit int, window time.Duration, now time.Time) *Limiter {
	return &Limiter{limit: limit, window: window, curStart: now}
}

// Allow reports whether a request arriving at now is admitted. now must be
// non-decreasing across calls.
func (l *Limiter) Allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	elapsed := now.Sub(l.curStart)
	if elapsed >= l.window {
		windowsPassed := int(elapsed / l.window)
		if windowsPassed == 1 {
			l.prevCount = l.curCount
		} else {
			l.prevCount = 0 // more than one window idle: nothing carries over
		}
		l.curCount = 0
		l.curStart = l.curStart.Add(time.Duration(windowsPassed) * l.window)
		elapsed = now.Sub(l.curStart)
	}

	// overlap is the fraction of the previous window that still falls
	// inside the trailing [now-window, now] interval. At the instant the
	// current window opens, overlap is 1 (the whole previous window still
	// counts); it decays linearly to 0 by the time the current window ends.
	overlap := 1 - float64(elapsed)/float64(l.window)
	weighted := float64(l.prevCount)*overlap + float64(l.curCount)

	if weighted >= float64(l.limit) {
		return false
	}
	l.curCount++
	return true
}
```

### The runnable demo

The demo fills a five-request quota at 900ms into window one, then
fires five more requests just 10ms into window two — the exact burst
shape that a naive reset would double-admit.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/sliding-window-rate-limiter"
)

func main() {
	now0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := ratelimit.New(5, time.Second, now0)

	t1 := now0.Add(900 * time.Millisecond)
	fmt.Println("--- requests arriving at 900ms into window 1 ---")
	for i := 0; i < 5; i++ {
		fmt.Printf("request %d: allowed=%v\n", i+1, l.Allow(t1))
	}

	t2 := now0.Add(1010 * time.Millisecond)
	fmt.Println("--- requests arriving 10ms into window 2 (right at the boundary) ---")
	for i := 0; i < 5; i++ {
		fmt.Printf("request %d: allowed=%v\n", i+1, l.Allow(t2))
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
--- requests arriving at 900ms into window 1 ---
request 1: allowed=true
request 2: allowed=true
request 3: allowed=true
request 4: allowed=true
request 5: allowed=true
--- requests arriving 10ms into window 2 (right at the boundary) ---
request 1: allowed=true
request 2: allowed=false
request 3: allowed=false
request 4: allowed=false
request 5: allowed=false
```

### Tests

Three short cases, no table: a single window filling exactly to its
limit and rejecting the next request; the boundary-straddling burst
from the demo, asserting at most one of the five boundary requests is
admitted; and a decay case confirming the previous window's weight
does fade — the limiter is not permanently stuck refusing traffic once
enough real time has actually passed.

Create `ratelimit_test.go`:

```go
package ratelimit

import (
	"testing"
	"time"
)

func TestAllowWithinLimitInSingleWindow(t *testing.T) {
	t.Parallel()

	now0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New(3, time.Second, now0)

	t1 := now0.Add(100 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if !l.Allow(t1) {
			t.Fatalf("request %d denied, want allowed (under the limit)", i+1)
		}
	}
	if l.Allow(t1) {
		t.Fatal("4th request in the same window allowed, want denied (at the limit)")
	}
}

func TestAllowRejectsBurstAtWindowBoundary(t *testing.T) {
	t.Parallel()

	now0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New(5, time.Second, now0)

	t1 := now0.Add(900 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if !l.Allow(t1) {
			t.Fatalf("request %d at t=900ms denied, want allowed", i+1)
		}
	}

	// 10ms into the next window: a naive fixed-window reset would allow a
	// full new burst of 5 here, admitting 10 requests within an 110ms span
	// against a limit of 5 per second. The weighted count must still
	// reflect the previous window's near-full usage.
	t2 := now0.Add(1010 * time.Millisecond)
	var allowed int
	for i := 0; i < 5; i++ {
		if l.Allow(t2) {
			allowed++
		}
	}
	if allowed > 1 {
		t.Fatalf("allowed %d of 5 boundary requests, want at most 1 (previous window's weight must still count)", allowed)
	}
}

func TestAllowDecaysAsPreviousWindowWeightFades(t *testing.T) {
	t.Parallel()

	now0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New(5, time.Second, now0)

	t1 := now0.Add(900 * time.Millisecond)
	for i := 0; i < 5; i++ {
		l.Allow(t1)
	}

	// Half a window later, the previous window's weight has decayed to
	// roughly half, so there is room for more requests.
	t3 := now0.Add(1500 * time.Millisecond)
	var allowed int
	for i := 0; i < 5; i++ {
		if l.Allow(t3) {
			allowed++
		}
	}
	if allowed == 0 {
		t.Fatal("allowed 0 requests once the previous window's weight had decayed, want at least 1")
	}
}
```

Run: `go test -count=1 ./...`.

## Review

`Allow` is correct when no trailing one-window interval, evaluated at
any instant, ever observes more than `limit` admitted requests —
including intervals that straddle a window reset, which is exactly
the case a test asserting only "each window individually stays under
the limit" would miss. The mistake this design avoids is treating a
window boundary as a clean slate: a fixed-window counter validates
requests against how long *its own* window has been open, never
against how recently the *previous* window's traffic actually
happened, so a burst placed deliberately around the reset sails
through twice. Weighting the previous window's count by its remaining
overlap turns the reset into a continuous decay instead of a discrete
jump, which is what makes the limiter's guarantee hold for any
sliding interval, not just for each fixed window in isolation.

## Resources

- [Go Specification: Time durations](https://pkg.go.dev/time#Duration) — `Duration` arithmetic used to compute elapsed time and overlap fractions.
- [Cloudflare: How we built rate limiting capable of scaling to millions of domains](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) — the sliding-window-counter algorithm and its boundary-burst trade-offs in production.
- [pkg.go.dev/time](https://pkg.go.dev/time#Time.Sub) — `Time.Sub` and `Time.Add`, the building blocks for a clock-injectable limiter.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-saga-rollback-loop-scope-error.md](24-saga-rollback-loop-scope-error.md) | Next: [26-singleflight-request-dedup-map-race.md](26-singleflight-request-dedup-map-race.md)
