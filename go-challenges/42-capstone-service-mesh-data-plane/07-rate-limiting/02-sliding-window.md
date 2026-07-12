# Exercise 2: Sliding Window Backend

The sliding window is the strict half of the engine: no burst tolerance, just "at most `maxReqs` requests in any `window`-long period." This exercise builds it as a ring buffer of time slots — pure standard library, no external dependency — and tests its rollover under virtual time so a one-second window is asserted in microseconds.

This module is fully self-contained: its own `go mod init`, every type defined inline, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
slidingwindow.go      SlidingWindow, New, TryAllow (ring buffer of slots)
cmd/
  demo/
    main.go           fill a window to its limit, print allowed/remaining
slidingwindow_test.go allow-up-to-max, synctest rollover, remaining, -race
```

- Files: `slidingwindow.go`, `cmd/demo/main.go`, `slidingwindow_test.go`.
- Implement: `SlidingWindow` with `New(maxReqs int, window time.Duration, numSlots int) *SlidingWindow` and `TryAllow(now time.Time) (allowed bool, remaining int, resetAt time.Time)`.
- Test: allow exactly `maxReqs` then deny, reset after the window passes (under `synctest`), remaining decreases, and a `-race` concurrency stress.
- Verify: `go test -count=1 -race ./...`

Set up the module. This backend is pure stdlib, so no `go get` is needed:

```bash
go mod edit -go=1.26
```

### Why a ring buffer of slots

An exact sliding window remembers every request timestamp and, on each call, counts how many fall inside the last `window`. That is O(maxReqs) memory per client — unacceptable when a proxy tracks millions of clients. The approximation here trades a little accuracy for fixed memory: the window is cut into `numSlots` equal sub-periods held in a fixed-size array used as a ring buffer. Each slot carries a `start` instant and a `count`. A slot whose `start` is older than `window` is stale; its count does not belong to the current period and is evicted on sight.

`TryAllow` does three things under the slot mutex. It walks every slot, zeroing any that is empty or expired and summing the live ones into `total`; it also tracks the earliest live slot so it can report when the window will next free a slot (`resetAt`). If `total` has already reached `maxReqs`, it denies without recording — a denied request must not consume a slot, or the limit would tighten itself. Otherwise it maps `now` to a ring index, and if that slot belongs to an older sub-period it resets the slot before reusing it, then increments the count and reports the remaining budget.

The slot index is `(now / slotDur) mod numSlots`, and the slot's canonical start is `now.Truncate(slotDur)`. Comparing the stored `start` against that truncated value is how a slot recognizes it has wrapped around to a new sub-period and must reset rather than accumulate onto stale counts.

Create `slidingwindow.go`:

```go
package slidingwindow

import (
	"sync"
	"time"
)

// SlidingWindow implements an approximate sliding window using a ring buffer of
// fixed-size time buckets. Expired buckets (older than window) are evicted on
// every TryAllow call; no background goroutine is needed.
type SlidingWindow struct {
	mu      sync.Mutex
	slots   []swSlot
	slotDur time.Duration // window / numSlots
	maxReqs int
	window  time.Duration
}

type swSlot struct {
	start time.Time // zero means the slot is empty / evicted
	count int
}

// New returns a SlidingWindow that allows at most maxReqs requests per window,
// approximated with numSlots ring-buffer buckets (defaulting to 10).
func New(maxReqs int, window time.Duration, numSlots int) *SlidingWindow {
	if numSlots <= 0 {
		numSlots = 10
	}
	return &SlidingWindow{
		slots:   make([]swSlot, numSlots),
		slotDur: window / time.Duration(numSlots),
		maxReqs: maxReqs,
		window:  window,
	}
}

// TryAllow counts active requests at instant now and either records one more
// (allowed) or rejects (denied). It is internally serialized with sw.mu.
func (sw *SlidingWindow) TryAllow(now time.Time) (allowed bool, remaining int, resetAt time.Time) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	cutoff := now.Add(-sw.window)
	total := 0
	var earliestStart time.Time

	for i := range sw.slots {
		s := &sw.slots[i]
		if s.start.IsZero() || !s.start.After(cutoff) {
			// Empty or expired slot: evict.
			s.start = time.Time{}
			s.count = 0
			continue
		}
		total += s.count
		if earliestStart.IsZero() || s.start.Before(earliestStart) {
			earliestStart = s.start
		}
	}

	// resetAt is when the oldest live slot expires.
	if !earliestStart.IsZero() {
		resetAt = earliestStart.Add(sw.window)
	} else {
		resetAt = now.Add(sw.window)
	}

	if total >= sw.maxReqs {
		return false, 0, resetAt
	}

	// Record in the current slot.
	idx := sw.slotIndex(now)
	cur := &sw.slots[idx]
	slotStart := now.Truncate(sw.slotDur)
	if cur.start != slotStart {
		// Slot belongs to an earlier period; reset before use.
		cur.start = slotStart
		cur.count = 0
	}
	cur.count++
	return true, sw.maxReqs - total - 1, resetAt
}

// slotIndex maps a time to a ring-buffer index.
func (sw *SlidingWindow) slotIndex(t time.Time) int {
	ns := sw.slotDur.Nanoseconds()
	if ns <= 0 {
		return 0
	}
	return int((t.UnixNano() / ns) % int64(len(sw.slots)))
}
```

### The runnable demo

The demo drives one window with a single fixed instant — every call shares the same `now`, so nothing rolls over between them — and watches the third request fill the limit and the fourth get denied. No wall-clock dependency means identical output every run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/sliding-window"
)

func main() {
	sw := slidingwindow.New(3, time.Second, 10) // 3 requests per second
	now := time.Now()

	fmt.Println("maxReqs=3, window=1s")
	for i := 1; i <= 4; i++ {
		ok, rem, _ := sw.TryAllow(now)
		fmt.Printf("  request %d: allowed=%v  remaining=%d\n", i, ok, rem)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
maxReqs=3, window=1s
  request 1: allowed=true  remaining=2
  request 2: allowed=true  remaining=1
  request 3: allowed=true  remaining=0
  request 4: allowed=false  remaining=0
```

### Tests

The allow-up-to-max test fills a window at one instant and confirms the next request is denied. The rollover test is the one that needs virtual time: inside a `synctest` bubble it fills a 100 ms window, confirms the deny, sleeps 110 ms of *virtual* time, and asserts the next request is allowed because every slot from the first burst is now older than the window. Feeding `time.Now()` into `TryAllow` inside the bubble makes that boundary exact. The remaining test pins the monotonic decrease, and the concurrency test exercises the slot mutex under `-race`.

Create `slidingwindow_test.go`:

```go
package slidingwindow

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestAllowsUpToMax(t *testing.T) {
	t.Parallel()
	sw := New(3, time.Second, 10)
	now := time.Now()
	for i := range 3 {
		ok, _, _ := sw.TryAllow(now)
		if !ok {
			t.Fatalf("request %d should be allowed (maxReqs=3)", i+1)
		}
	}
	if ok, _, _ := sw.TryAllow(now); ok {
		t.Fatal("4th request in same window should be denied")
	}
}

func TestResetsAfterVirtualWindow(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		sw := New(2, 100*time.Millisecond, 10)
		sw.TryAllow(time.Now())
		sw.TryAllow(time.Now())
		if ok, _, _ := sw.TryAllow(time.Now()); ok {
			t.Fatal("window full: should be denied")
		}
		// Move past the window in virtual time: the first burst's slots expire.
		time.Sleep(110 * time.Millisecond)
		if ok, _, _ := sw.TryAllow(time.Now()); !ok {
			t.Fatal("should be allowed after the window expired")
		}
	})
}

func TestRemainingDecreases(t *testing.T) {
	t.Parallel()
	sw := New(5, time.Second, 10)
	now := time.Now()
	_, rem0, _ := sw.TryAllow(now)
	_, rem1, _ := sw.TryAllow(now)
	if rem1 >= rem0 {
		t.Fatalf("remaining should decrease: got %d then %d", rem0, rem1)
	}
}

func TestConcurrentTryAllow(t *testing.T) {
	t.Parallel()
	sw := New(1000, time.Second, 10)
	now := time.Now()
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sw.TryAllow(now)
		}()
	}
	wg.Wait()
}
```

## Review

The window is correct when a denied request never records a slot — return `false` *before* the increment, or each rejected request would consume budget and the limit would ratchet down. `TestAllowsUpToMax` guards that the ceiling is exactly `maxReqs`. The rollover is correct when a slot resets the moment its `start` no longer matches `now.Truncate(slotDur)`; `TestResetsAfterVirtualWindow` proves the whole window frees up once `window` has passed, and it is deterministic only because the bubble virtualizes the clock you pass into `TryAllow`. Remember the accuracy/memory trade: more slots sharpen the boundary at the cost of memory per client, and ten is a reasonable default. The `-race` run on `TestConcurrentTryAllow` confirms the slot mutex actually serializes concurrent callers.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock that serializes slot reads and writes.
- [`time.Time.Truncate`](https://pkg.go.dev/time#Time.Truncate) — snapping an instant to a slot boundary so a slot can detect it has wrapped.
- [Cloudflare: how we built rate limiting](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) — the sliding-window approximation and its error bounds under real traffic.

---

Back to [01-token-bucket.md](01-token-bucket.md) | Next: [03-rule-engine.md](03-rule-engine.md)
