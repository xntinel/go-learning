# Exercise 34: Sliding Window Rate Limiter — Deferred Window Rotation Enables Smooth Token Distribution

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A naive fixed-window rate limiter resets its counter to zero at every
window boundary — every 10 seconds, say — which has a well-known flaw: a
client that spends its whole limit in the last millisecond of one window
and its whole limit again in the first millisecond of the next has just
sent twice the configured rate in a two-millisecond span, and the
limiter never noticed. A continuous sliding window avoids that by never
fully discarding the previous window's count the instant a new one
starts; instead it weights that count by how much of the previous
window still temporally overlaps the current instant, so the effective
allowance decays smoothly across the boundary instead of snapping. This
module builds that weighting, computing window rotation lazily — on the
calling request itself, with no background ticker — every time `Allow`
is called. The module is fully self-contained: its own `go mod init`,
all code inline, its own demo and tests.

## What you'll build

```text
ratelimiter/                  independent module: example.com/sliding-window-rate-limiter-continuous
  go.mod                       go 1.24
  ratelimiter.go                SlidingWindow (New, Allow)
  cmd/
    demo/
      main.go                  runnable demo: burst fills a window, boundary-crossing requests weighted
  ratelimiter_test.go           sequence table across window boundaries; concurrent-load case under -race
```

- Files: `ratelimiter.go`, `cmd/demo/main.go`, `ratelimiter_test.go`.
- Implement: `SlidingWindow` with `New(limit int, window time.Duration, now func() time.Time) *SlidingWindow` and `Allow() bool`.
- Test: a sequential table driving requests across a window boundary, plus a concurrent-callers case proving the limit is never exceeded.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why `now` is read exactly once, at the top, under no lock yet

`Allow` calls `rl.now()` a single time, before it even takes the mutex,
and every calculation below — whether a window boundary was crossed, how
much overlap the previous window still contributes — reuses that one
value. This matters for the same reason the concepts lesson insists a
deferred closure must read its captured state at execution time rather
than freezing it early, just inverted: here the risk is reading the
clock *too many times within one call* instead of too few times across
calls. If `Allow` called `rl.now()` again partway through — say, once to
decide whether to rotate the window and again afterward to compute the
overlap fraction — a window boundary could fall in the gap between
those two reads under real concurrent load, and the same logical request
would end up reasoning about two different instants: rotating based on
one "now" and weighting based on another, an internally inconsistent
decision. Reading the clock once and passing that single value through
the whole function is what keeps one `Allow` call atomic with respect to
time, no matter how the mutex-protected section beneath it is
structured.

Create `ratelimiter.go`:

```go
package ratelimiter

import (
	"sync"
	"time"
)

// SlidingWindow implements a continuous sliding-window rate limiter: it
// approximates a true sliding window by weighting the previous fixed
// window's count by how much of it still overlaps the current window,
// instead of resetting to zero at each discrete window boundary. That
// weighting is what gives smooth token distribution -- a burst that lands
// right at a window boundary in a naive fixed-window limiter would get to
// spend the whole limit twice, once in each adjacent window; the weighted
// overlap here prevents that.
type SlidingWindow struct {
	now    func() time.Time
	limit  int
	window time.Duration

	mu          sync.Mutex
	windowStart time.Time
	prevCount   int
	currCount   int
}

// New returns a limiter allowing at most limit requests per window
// duration, using now as its clock -- inject a fixed or fake clock in
// tests and demos for reproducible results, time.Now in production.
func New(limit int, window time.Duration, now func() time.Time) *SlidingWindow {
	return &SlidingWindow{limit: limit, window: window, now: now}
}

// Allow reports whether one more request fits under the limit right now.
// Window rotation is computed lazily, here, on the calling goroutine's own
// request -- there is no background ticker rotating windows on a
// schedule. Reading rl.now() exactly once, right at the top, and reusing
// that single value for every calculation below is what keeps a single
// call internally consistent: if it were read again partway through, a
// window boundary crossed in between the two reads could make the same
// call see two different, inconsistent instants.
func (rl *SlidingWindow) Allow() bool {
	now := rl.now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	elapsedSinceStart := now.Sub(rl.windowStart)
	switch {
	case rl.windowStart.IsZero():
		rl.windowStart = now
	case elapsedSinceStart >= 2*rl.window:
		// More than one full window has passed with no requests at all:
		// the previous window's count is entirely stale.
		rl.prevCount = 0
		rl.currCount = 0
		rl.windowStart = now
	case elapsedSinceStart >= rl.window:
		// Exactly one window boundary crossed: what was "current" becomes
		// "previous", and a fresh window starts now.
		rl.prevCount = rl.currCount
		rl.currCount = 0
		rl.windowStart = rl.windowStart.Add(rl.window)
		elapsedSinceStart = now.Sub(rl.windowStart)
	}

	overlap := float64(rl.window-elapsedSinceStart) / float64(rl.window)
	if overlap < 0 {
		overlap = 0
	}
	weighted := float64(rl.prevCount)*overlap + float64(rl.currCount)

	if weighted >= float64(rl.limit) {
		return false
	}
	rl.currCount++
	return true
}
```

### The runnable demo

A limit of 4 requests per 10-second window. The first four requests
(t=0s..3s) fill window one exactly; the fifth (t=4s) is rejected.
Crossing into the next window at t=11s, the previous window's count of
4 is weighted by how much of that window's 10 seconds still overlaps —
at t=11s that overlap is 90%, so `4 * 0.9 = 3.6` is still under the
limit of 4 and the request is allowed; by t=12s the overlap has decayed
enough that the weighted total tips over the limit.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/sliding-window-rate-limiter-continuous"
)

func main() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	clock := func() time.Time { return now }

	rl := ratelimiter.New(4, 10*time.Second, clock)

	offsets := []time.Duration{
		0, 1 * time.Second, 2 * time.Second, 3 * time.Second, // fills window 1
		4 * time.Second, // 5th request in window 1: rejected
		11 * time.Second, 12 * time.Second, 13 * time.Second, 14 * time.Second, 15 * time.Second,
	}

	for i, off := range offsets {
		now = base.Add(off)
		allowed := rl.Allow()
		fmt.Printf("request %d at t=%s: allowed=%v\n", i, off, allowed)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
request 0 at t=0s: allowed=true
request 1 at t=1s: allowed=true
request 2 at t=2s: allowed=true
request 3 at t=3s: allowed=true
request 4 at t=4s: allowed=false
request 5 at t=11s: allowed=true
request 6 at t=12s: allowed=false
request 7 at t=13s: allowed=true
request 8 at t=14s: allowed=false
request 9 at t=15s: allowed=false
```

Note requests 5, 7, and 9 do not form an obvious pattern at a glance —
that is the sliding window working as designed: each decision depends on
the *current* weighted overlap of the previous window's count, which
shifts continuously with time rather than jumping only at discrete
boundaries.

### Tests

`TestAllowSequenceWithinAndAcrossWindows` drives exactly the demo's
sequence as a table, asserting each individual `Allow` result.
`TestAllowNeverExceedsLimitUnderConcurrentLoad` fixes the clock so every
concurrent caller observes the identical instant, then races 100
goroutines against a limit of 20 — under `-race`, this proves the
mutex-protected check-and-increment never lets more than exactly the
configured limit through, regardless of scheduling.

Create `ratelimiter_test.go`:

```go
package ratelimiter

import (
	"sync"
	"testing"
	"time"
)

func TestAllowSequenceWithinAndAcrossWindows(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	clock := func() time.Time { return now }

	rl := New(4, 10*time.Second, clock)

	tests := []struct {
		offset time.Duration
		want   bool
	}{
		{0, true},
		{1 * time.Second, true},
		{2 * time.Second, true},
		{3 * time.Second, true},
		{4 * time.Second, false},
		{11 * time.Second, true},
		{12 * time.Second, false},
		{13 * time.Second, true},
		{14 * time.Second, false},
		{15 * time.Second, false},
	}

	for _, tc := range tests {
		now = base.Add(tc.offset)
		if got := rl.Allow(); got != tc.want {
			t.Errorf("Allow() at t=%s = %v, want %v", tc.offset, got, tc.want)
		}
	}
}

// TestAllowNeverExceedsLimitUnderConcurrentLoad races callers, all sharing
// one fixed instant, against a limiter's mutex-protected check-and-
// increment. Because every caller sees the same clock reading, this
// reduces to "the limit is never exceeded" regardless of how the
// scheduler interleaves the concurrent Allow calls -- exactly what -race
// exists to catch a regression in.
func TestAllowNeverExceedsLimitUnderConcurrentLoad(t *testing.T) {
	fixed := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return fixed }

	const limit = 20
	rl := New(limit, time.Second, clock)

	const callers = 100
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allowedCount int

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rl.Allow() {
				mu.Lock()
				allowedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowedCount != limit {
		t.Fatalf("allowed = %d concurrent callers, want exactly %d (the configured limit)", allowedCount, limit)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

`Allow` is correct when its accept/reject decision is always consistent
with a single instant in time, and when concurrent callers never push
the effective rate above the configured limit no matter how their calls
interleave. Reading `rl.now()` exactly once per call, before the lock is
even taken, and threading that one value through the rotation-and-weight
calculation is what guarantees the first property; the mutex around the
check-and-increment is what guarantees the second. The mistake this
design avoids is calling the clock more than once inside a single
`Allow` — deciding whether to rotate based on one reading and computing
the overlap weight from a second, later reading — which reintroduces
exactly the kind of internal inconsistency a sliding window is supposed
to eliminate, just moved from the window-boundary level down to the
level of a single request.

## Resources

- [Cloudflare Blog: How we built rate limiting capable of scaling to millions of domains](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) — production discussion of the sliding-window-counter approximation this exercise implements.
- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — `defer rl.mu.Unlock()` releasing the lock on every return path out of `Allow`.
- [pkg.go.dev: golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate) — the standard Go token-bucket limiter; a useful contrast to this exercise's window-based approach.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-bloom-filter-dynamic-hashfunc-finalization.md](33-bloom-filter-dynamic-hashfunc-finalization.md) | Next: [../08-panic-and-recover/00-concepts.md](../08-panic-and-recover/00-concepts.md)
