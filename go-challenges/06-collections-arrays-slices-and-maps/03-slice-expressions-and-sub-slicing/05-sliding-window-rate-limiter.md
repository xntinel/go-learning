# Exercise 5: Sliding-Window Rate Limiter That Trims Expired Timestamps by Reslicing

A per-key rate limiter keeps a sorted `[]time.Time` of recent hits and, on each
call, drops timestamps older than the window before deciding allow or deny. If you
trim by advancing an integer `head` index into a slice you keep appending to — and
never physically drop the expired prefix — the backing array grows without bound
for any hot key: a real, slow heap climb that pprof shows as `[]time.Time`. This
exercise builds the limiter and adds the `slices.Clone` compaction that reclaims the
dead prefix.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
ratelimit/                 independent module: example.com/ratelimit
  go.mod                   go 1.24
  ratelimit.go             type Limiter; New(limit, window, now); Allow(key)
  cmd/
    demo/
      main.go              runnable demo with an injected clock
  ratelimit_test.go        allow/deny test, expiry test, bounded-growth test, -race test
```

- Files: `ratelimit.go`, `cmd/demo/main.go`, `ratelimit_test.go`.
- Implement: `Allow(key)` that finds the first fresh timestamp with `sort.Search`,
  advances a `head` index, compacts the dead prefix with `slices.Clone` when it
  grows large, and denies once the live count reaches the limit.
- Test: allow up to the limit inside the window, deny beyond it, older hits expire
  and free capacity, the backing array stays bounded under a long hot-key run, and
  `-race` is clean under concurrent keys.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/03-slice-expressions-and-sub-slicing/05-sliding-window-rate-limiter/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/03-slice-expressions-and-sub-slicing/05-sliding-window-rate-limiter
go mod edit -go=1.24
```

## Why head-index trimming leaks, and how compaction fixes it

Each key holds its hit timestamps in a slice `ts`, sorted ascending (they are
appended in time order), plus an integer `head` marking where the live window
begins. On each `Allow`:

1. Compute `cutoff = now - window`; everything at or before it has expired.
2. Over the live sub-slice `ts[head:]`, `sort.Search` finds the first index strictly
   after `cutoff`. Because the slice is sorted and the predicate monotonic, this is
   a clean binary search.
3. Advance the head: `head += i`. Nothing is copied, nothing is freed — the `head`
   expired timestamps still sit in `ts[0:head]`, physically present in the backing
   array, just logically ignored.
4. Deny if `len(ts) - head >= limit`; otherwise append `now` and allow.

Step 3 is the trap. For a key hit steadily, the live count `len(ts) - head` hovers
near the limit, but `head` marches forward forever and `append` keeps extending the
tail. The expired prefix `ts[0:head]` is never removed, so `ts` — and its backing
array — grows for the entire lifetime of the process. A single hot key can accrete
a multi-thousand-element `[]time.Time` whose live window is only a handful of
entries. Because the whole slice stays referenced, the GC cannot reclaim any of the
dead prefix. This is the "never resetting the head" leak, and it is silent: the
limiter's decisions stay correct while memory climbs.

The fix is periodic compaction. When the dead prefix `head` grows past a small
multiple of the limit, replace the slice with a right-sized copy of just the live
window: `ts = slices.Clone(ts[head:]); head = 0`. `slices.Clone` allocates a fresh
array holding exactly the live timestamps (`cap == len`), the oversized old array
becomes collectable, and the head resets to zero. Compacting only when the prefix is
large keeps the amortized cost negligible while capping memory at roughly the live
window. (Reslicing forward with `ts = ts[head:]` would also shrink the header, but
`Clone` is what actually detaches and frees the old array immediately rather than
waiting for the next `append` reallocation.)

The clock is injected as `now func() time.Time` so the tests drive virtual time and
are fully deterministic — no real sleeping, no flakiness — which is the standard way
to test time-dependent logic that must also build on any toolchain.

Create `ratelimit.go`:

```go
package ratelimit

import (
	"slices"
	"sort"
	"sync"
	"time"
)

// window holds one key's hit timestamps (sorted ascending) and the index where
// the live, unexpired portion begins.
type window struct {
	ts   []time.Time
	head int
}

// Limiter is a per-key sliding-window rate limiter.
type Limiter struct {
	mu    sync.Mutex
	limit int
	dur   time.Duration
	now   func() time.Time
	keys  map[string]*window
}

// New builds a Limiter allowing at most limit hits per key within dur. now is
// injected so tests can drive virtual time; pass time.Now in production.
func New(limit int, dur time.Duration, now func() time.Time) *Limiter {
	return &Limiter{
		limit: limit,
		dur:   dur,
		now:   now,
		keys:  make(map[string]*window),
	}
}

// Allow reports whether a hit on key is permitted now, recording it if so.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	cutoff := now.Add(-l.dur)

	w := l.keys[key]
	if w == nil {
		w = &window{}
		l.keys[key] = w
	}

	// First live index strictly after cutoff; advance the head past expired hits.
	live := w.ts[w.head:]
	i := sort.Search(len(live), func(i int) bool { return live[i].After(cutoff) })
	w.head += i

	// Compact the dead prefix so it cannot pin an ever-growing array.
	if w.head > 2*l.limit {
		w.ts = slices.Clone(w.ts[w.head:])
		w.head = 0
	}

	if len(w.ts)-w.head >= l.limit {
		return false
	}
	w.ts = append(w.ts, now)
	return true
}

// Cap reports the backing-array capacity for a key (for tests/observability).
func (l *Limiter) Cap(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w := l.keys[key]; w != nil {
		return cap(w.ts)
	}
	return 0
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ratelimit"
)

func main() {
	clock := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	lim := ratelimit.New(3, time.Second, func() time.Time { return clock })

	// Three hits in the same instant: all allowed, the fourth denied.
	for i := 1; i <= 4; i++ {
		fmt.Printf("hit %d: %v\n", i, lim.Allow("user-42"))
	}

	// Advance past the window: the old hits expire, so we may hit again.
	clock = clock.Add(2 * time.Second)
	fmt.Printf("after window: %v\n", lim.Allow("user-42"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hit 1: true
hit 2: true
hit 3: true
hit 4: false
after window: true
```

## Tests

The tests drive a fake clock so every assertion is exact. `TestAllowThenDeny` pins
the limit at a single instant; `TestExpiry` advances past the window and checks the
old hits are gone; `TestBoundedGrowth` hammers one key for far more calls than the
limit while advancing the clock each call, and asserts the backing array stays
bounded — which only holds because compaction runs. `TestConcurrentKeys` exercises
the mutex under `-race`.

Create `ratelimit_test.go`:

```go
package ratelimit

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func fixedClock() (*time.Time, func() time.Time) {
	t := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	return &t, func() time.Time { return t }
}

func TestAllowThenDeny(t *testing.T) {
	t.Parallel()
	_, now := fixedClock()
	lim := New(3, time.Second, now)

	for i := range 3 {
		if !lim.Allow("k") {
			t.Fatalf("hit %d denied inside limit", i+1)
		}
	}
	if lim.Allow("k") {
		t.Fatal("4th hit allowed past limit")
	}
}

func TestExpiry(t *testing.T) {
	t.Parallel()
	clk, now := fixedClock()
	lim := New(2, time.Second, now)

	if !lim.Allow("k") || !lim.Allow("k") {
		t.Fatal("first two hits should be allowed")
	}
	if lim.Allow("k") {
		t.Fatal("third hit should be denied inside window")
	}

	*clk = clk.Add(2 * time.Second) // both hits now expired
	if !lim.Allow("k") {
		t.Fatal("hit should be allowed after window slides past old hits")
	}
}

func TestBoundedGrowth(t *testing.T) {
	t.Parallel()
	clk, now := fixedClock()
	limit := 5
	lim := New(limit, time.Second, now)

	// Hit steadily, advancing the clock each call. Without compaction the dead
	// prefix would grow the array for the whole run; with it, cap stays small.
	for range 100000 {
		*clk = clk.Add(10 * time.Millisecond)
		lim.Allow("hot")
	}

	if c := lim.Cap("hot"); c > 8*limit {
		t.Fatalf("backing array grew unbounded: cap=%d (limit=%d)", c, limit)
	}
}

func TestConcurrentKeys(t *testing.T) {
	t.Parallel()
	_, now := fixedClock()
	lim := New(10, time.Minute, now)

	var wg sync.WaitGroup
	for k := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", k)
			for range 20 {
				lim.Allow(key)
			}
		}()
	}
	wg.Wait()
}
```

## Review

The limiter is correct when, at any instant, it allows a hit exactly while fewer
than `limit` timestamps lie within the trailing `window`, and denies otherwise. The
expiry test proves the head advances the window; the bounded-growth test proves the
compaction reclaims the dead prefix — delete the `slices.Clone` branch and
`Cap("hot")` climbs into the thousands and the test fails, which is the whole point.
The subtle wrong turn is believing that advancing `head` "removes" the expired
timestamps; it only hides them, and a long-lived hot key drags an ever-growing array
behind a constant-size window. Compact when the dead prefix grows, and reason about
`cap` (the array) versus the live count (`len - head`). Run `go test -race` to
confirm the per-key map is safely shared.

## Resources

- [`sort.Search`](https://pkg.go.dev/sort#Search)
- [`slices.Clone`](https://pkg.go.dev/slices#Clone)
- [`slices.Clip`](https://pkg.go.dev/slices#Clip)
- [`time.Time.After`](https://pkg.go.dev/time#Time.After)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-filter-connections-in-place.md](04-filter-connections-in-place.md) | Next: [06-csv-line-field-slicing.md](06-csv-line-field-slicing.md)
