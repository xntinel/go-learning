# Exercise 15: Rate Limiter with Sliding Window and Call History

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A fixed-window rate limiter has a well-known flaw: a client can burst up to
`2*limit` calls straddling a window boundary. A sliding window fixes this
by keeping the actual history of recent call timestamps in a closure and
counting how many still fall inside the trailing window on every call —
no bucket to reset, no boundary to exploit.

## What you'll build

```text
throttle/                    independent module: example.com/throttle
  go.mod                     go 1.24
  throttle.go                type Clock, Limiter; func NewSlidingWindow
  throttle_test.go           limit enforcement, partial expiry, canceled ctx, concurrency
  cmd/demo/
    main.go                  drives a fake clock through a burst and a window slide
```

- Files: `throttle.go`, `throttle_test.go`, `cmd/demo/main.go`.
- Implement: `Clock func() time.Time`, `Limiter func(ctx context.Context) (bool, error)`, and `NewSlidingWindow(limit int, window time.Duration, clock Clock) Limiter`.
- Test: calls are admitted up to `limit` within the window; a partially expired history admits exactly as many new calls as have expired; an already-canceled context is rejected with its error before touching history; concurrent callers never admit more than `limit` total.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/throttle/cmd/demo
cd ~/go-exercises/throttle
go mod init example.com/throttle
go mod edit -go=1.24
```

### Why the history lives in the closure, not a counter

A naive limiter keeps one integer counter and resets it every `window`
duration. That has the boundary-burst flaw: 100 calls at `0.99s` and 100
more at `1.01s` both land inside the limit if the window resets exactly at
`1.0s`, even though only 20ms separates them. A sliding window avoids this
by tracking *when* each admitted call happened, not just *how many*. Every
call first prunes timestamps older than `now - window` from the front of
the slice — the history is kept in nondecreasing order, so pruning is a
single forward scan, not a full pass — and then admits the new call only if
fewer than `limit` timestamps survive the prune.

The history and its mutex are variables captured by the closure `NewSlidingWindow`
returns; there is no `Limiter` type with exported fields, so the only way to
observe or corrupt the count is through the closure itself. `clock` is
injected rather than calling `time.Now()` directly so a test can freeze,
advance, or straddle a window boundary exactly, which is the entire point
of testing a time-based limiter without real sleeps.

The context check happens before the lock is even taken: a canceled
context should never count against the caller's quota, and checking first
means a canceled request never mutates the history.

Create `throttle.go`:

```go
package throttle

import (
	"context"
	"sync"
	"time"
)

// Clock returns the current time. Production wires this to time.Now;
// tests wire it to a fake so the window boundary is exact and repeatable.
type Clock func() time.Time

// Limiter reports whether a call is allowed right now. It returns an error
// only if ctx is already done; a denied call under a live context returns
// (false, nil), not an error, since throttling is an expected outcome.
type Limiter func(ctx context.Context) (bool, error)

// NewSlidingWindow returns a Limiter that allows at most limit calls within
// any trailing duration of length window. It works by keeping a history of the
// timestamps of allowed calls in a closure: on every call it first drops
// timestamps older than now-window, then admits the call only if fewer
// than limit timestamps remain.
func NewSlidingWindow(limit int, window time.Duration, clock Clock) Limiter {
	var mu sync.Mutex
	var history []time.Time

	return func(ctx context.Context) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}

		mu.Lock()
		defer mu.Unlock()

		now := clock()
		cutoff := now.Add(-window)

		// history is kept in nondecreasing order, so the first entries
		// still at-or-after cutoff mark where the live window begins.
		i := 0
		for i < len(history) && history[i].Before(cutoff) {
			i++
		}
		history = history[i:]

		if len(history) >= limit {
			return false, nil
		}
		history = append(history, now)
		return true, nil
	}
}
```

### The runnable demo

The demo drives a fake clock manually: three quick calls fill the window,
a fourth is denied, and advancing the clock past the window frees up room
again — all without a single real sleep.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/throttle"
)

func main() {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	clock := func() time.Time { return now }

	limit := throttle.NewSlidingWindow(3, time.Second, clock)
	ctx := context.Background()

	// Three calls in quick succession: all allowed.
	for i := range 3 {
		ok, _ := limit(ctx)
		fmt.Printf("t=%s call=%d allowed=%v\n", now.Sub(start), i, ok)
	}

	// A fourth call still inside the one-second window: denied.
	ok, _ := limit(ctx)
	fmt.Printf("t=%s call=%d allowed=%v\n", now.Sub(start), 3, ok)

	// Advance past the window: the oldest calls expire and admit again.
	now = now.Add(1100 * time.Millisecond)
	ok, _ = limit(ctx)
	fmt.Printf("t=%s call=%d allowed=%v\n", now.Sub(start), 4, ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
t=0s call=0 allowed=true
t=0s call=1 allowed=true
t=0s call=2 allowed=true
t=0s call=3 allowed=false
t=1.1s call=4 allowed=true
```

The three calls at `t=0` fill the window of 3; the fourth call, still at
`t=0`, is denied. Advancing the clock by 1.1s pushes every prior timestamp
past the one-second window, so the next call finds an empty history and is
admitted.

### Tests

`TestSlidingWindowAdmitsUpToLimit` drives a sequence of clock advances
through a table, checking admission at each step, including the exact
moment the window slides past the earlier entries. `TestSlidingWindowPartialExpiry`
proves the history expires one entry at a time rather than all-or-nothing:
with two live entries 600ms apart, advancing 500ms more expires only the
older one, so exactly one new call is admitted, not two.
`TestSlidingWindowRespectsCanceledContext` proves a canceled context is
rejected before it can consume quota. `TestSlidingWindowConcurrentCallsRespectLimit`
freezes the clock so every goroutine sees the same instant, fires 50
concurrent callers at a limiter of 10, and asserts the mutex serializes the
check-then-act so exactly 10 — never more — are admitted.

Create `throttle_test.go`:

```go
package throttle

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSlidingWindowAdmitsUpToLimit(t *testing.T) {
	t.Parallel()

	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	clock := func() time.Time { return now }
	limit := NewSlidingWindow(2, time.Second, clock)
	ctx := context.Background()

	tests := []struct {
		advance time.Duration
		want    bool
	}{
		{0, true},                       // 1st call in window
		{0, true},                       // 2nd call in window
		{0, false},                      // 3rd call: limit reached
		{1100 * time.Millisecond, true}, // window slid past both, room again
		{0, true},                       // one live entry, room for one more
		{0, false},                      // limit reached again
	}

	for i, tc := range tests {
		now = now.Add(tc.advance)
		got, err := limit(ctx)
		if err != nil {
			t.Fatalf("call %d: unexpected error %v", i, err)
		}
		if got != tc.want {
			t.Fatalf("call %d: allowed = %v, want %v", i, got, tc.want)
		}
	}
}

func TestSlidingWindowPartialExpiry(t *testing.T) {
	t.Parallel()

	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	clock := func() time.Time { return now }
	limit := NewSlidingWindow(2, time.Second, clock)
	ctx := context.Background()

	first, _ := limit(ctx) // t=0
	if !first {
		t.Fatal("first call should be allowed")
	}

	now = now.Add(600 * time.Millisecond)
	second, _ := limit(ctx) // t=600ms
	if !second {
		t.Fatal("second call should be allowed")
	}

	now = now.Add(500 * time.Millisecond) // t=1100ms: first (t=0) has expired
	third, _ := limit(ctx)
	if !third {
		t.Fatal("third call should be allowed once the first entry expires")
	}

	fourth, _ := limit(ctx) // still t=1100ms, second (t=600ms) still counts
	if fourth {
		t.Fatal("fourth call should be denied: two live entries remain")
	}
}

func TestSlidingWindowRespectsCanceledContext(t *testing.T) {
	t.Parallel()

	clock := func() time.Time { return time.Unix(0, 0) }
	limit := NewSlidingWindow(5, time.Second, clock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ok, err := limit(ctx)
	if ok {
		t.Fatal("allowed = true, want false for a canceled context")
	}
	if err == nil {
		t.Fatal("err = nil, want context.Canceled")
	}
}

func TestSlidingWindowConcurrentCallsRespectLimit(t *testing.T) {
	t.Parallel()

	clock := func() time.Time { return time.Unix(0, 0) } // frozen: all calls land in one instant
	const limit = 10
	limiter := NewSlidingWindow(limit, time.Second, clock)
	ctx := context.Background()

	var mu sync.Mutex
	allowed := 0

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := limiter(ctx)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != limit {
		t.Fatalf("allowed = %d, want exactly %d", allowed, limit)
	}
}
```

## Review

The limiter is correct when the prune-then-count happens atomically under
one lock: checking the history length and appending the new timestamp must
happen as one critical section, or two goroutines could both see room for
one more and both admit, overshooting `limit`. The concurrency test pins
exactly that — 50 goroutines racing against a frozen clock must never admit
more than 10. Injecting `Clock` is what makes the partial-expiry test
possible at all: with `time.Now()` baked in, asserting "exactly one entry
expired after advancing 500ms past a 600ms-old entry" would be flaky by
construction. Checking `ctx.Err()` before taking the lock keeps a canceled
caller from ever consuming quota it will not use.

## Resources

- [context package](https://pkg.go.dev/context) — `Err`, `WithCancel`, cooperative cancellation.
- [time package](https://pkg.go.dev/time) — `Time.Before`, `Time.Add`, `Duration` arithmetic.
- [Cloudflare Blog: How we built rate limiting capable of scaling to millions of domains](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) — sliding window vs fixed window counters in production.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-fallback-lookup-combinator.md](14-fallback-lookup-combinator.md) | Next: [16-event-debouncer-deadline-coalesce.md](16-event-debouncer-deadline-coalesce.md)
