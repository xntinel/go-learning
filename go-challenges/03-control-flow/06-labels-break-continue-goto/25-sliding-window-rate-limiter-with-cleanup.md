# Exercise 25: Sliding-window rate limiter with tail cleanup

**Nivel: Intermedio** — validacion rapida (un test corto).

A public API enforces a per-client request cap using a sliding window rather
than fixed buckets, because fixed buckets let a client burst twice its limit
right across a bucket boundary. The correct fix keeps a history of request
timestamps per client and counts only the ones still inside the trailing
window — but that history has to shrink as time moves forward, or a service
with a million distinct API keys leaks memory forever on keys that made one
request and vanished. This module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
ratewindow/                 independent module: example.com/ratewindow
  go.mod                     go 1.24
  ratewindow.go               Limiter; Allow, Cleanup, Len
  cmd/
    demo/
      main.go                runnable demo: window boundary eviction, then a cleanup sweep
  ratewindow_test.go          table test: window boundary, cutoff-equality edge case, eviction freeing a slot; cleanup removing stale keys and partially trimming live ones
```

- Files: `ratewindow.go`, `cmd/demo/main.go`, `ratewindow_test.go`.
- Implement: `Limiter.Allow(key string, now int64) bool`, counting only timestamps inside `(now-window, now]` and evicting the stale prefix for that key on every call; `Limiter.Cleanup(now int64)`, sweeping every tracked key and deleting any whose entire history is now stale.
- Test: three requests fitting under the limit, a fourth rejected, a timestamp landing exactly on the eviction cutoff, eviction freeing a slot after the window rolls forward, and a cleanup sweep that removes a stale key while leaving a partially-live one trimmed but present.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/06-labels-break-continue-goto/25-sliding-window-rate-limiter-with-cleanup/cmd/demo
cd go-solutions/03-control-flow/06-labels-break-continue-goto/25-sliding-window-rate-limiter-with-cleanup
go mod edit -go=1.24
```

### Why Cleanup needs a labeled continue, not a bare one

`Allow` already evicts a key's own stale entries on every call, but that only
happens when the key is used again. A key that sends exactly `limit-1`
requests and then goes silent — a decommissioned client, a rotated API key —
keeps its slice allocated forever, since nothing ever calls `Allow` for it
again to trigger the trim. `Cleanup` closes that gap by sweeping every
tracked key on a timer, independent of traffic.

The sweep is two loops deep: the outer one walks every key in the map, and
the inner one walks that key's timestamps looking for the first one still
inside the window (timestamps are appended in non-decreasing order, so stale
entries are always a contiguous prefix). The moment the inner loop finds a
live timestamp, two things need to happen together: commit the truncation
for the entries found stale so far, and move on to the *next key* — there is
nothing more this key's remaining entries can tell us, since they are all
live by the same ascending-order argument. A bare `continue` at that point
would only advance to the next timestamp of the *same* key, needlessly
re-checking entries already known to be live before the outer loop's natural
end-of-body advance eventually gets to the next key anyway. `continue keys`
states the real intent directly — abandon this key's inner scan now — and
is fired from inside the per-timestamp loop, one level below the loop it
names.

A key whose inner loop runs to completion without ever finding a live
timestamp has no live entries at all, and is deleted outright — leaving a
key mapped to an empty slice in memory forever would defeat the whole point
of the sweep.

Create `ratewindow.go`:

```go
package ratewindow

import "sync"

// Limiter is a sliding-window rate limiter keyed by an arbitrary string --
// a client ID, an API key, a tenant ID. Each key keeps a history of request
// timestamps in ascending order; a request is allowed only if fewer than
// limit requests fall inside the half-open window (now-window, now].
type Limiter struct {
	mu     sync.Mutex
	limit  int
	window int64 // window width, same unit as the timestamps passed to Allow
	hist   map[string][]int64
}

// NewLimiter builds a Limiter permitting up to limit requests per key inside
// any rolling window of the given width.
func NewLimiter(limit int, window int64) *Limiter {
	return &Limiter{limit: limit, window: window, hist: make(map[string][]int64)}
}

// Allow reports whether a request for key at time now is within the limit,
// after first evicting this key's own stale entries (anything at or before
// now-window). Piggybacking eviction onto every call means a key that keeps
// sending requests never accumulates more history than the window can hold.
func (l *Limiter) Allow(key string, now int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now - l.window
	times := l.hist[key]

	// Timestamps are appended in non-decreasing order, so stale entries form
	// a contiguous prefix. The moment we see one still inside the window,
	// everything after it is too -- there is nothing left worth checking.
	evict := 0
	for _, t := range times {
		if t > cutoff {
			break
		}
		evict++
	}
	if evict > 0 {
		times = times[evict:]
	}

	if len(times) >= l.limit {
		l.hist[key] = times
		return false
	}

	l.hist[key] = append(times, now)
	return true
}

// Cleanup sweeps every tracked key and evicts stale entries even for keys
// that have gone quiet -- without it, a client that sends limit-1 requests
// and then disappears keeps its slice allocated forever, since Allow only
// prunes a key when THAT key is used again. A key left with no live entries
// at all is removed outright, reclaiming the map entry too.
func (l *Limiter) Cleanup(now int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now - l.window

keys:
	for key, times := range l.hist {
		evict := 0
		for _, t := range times {
			if t > cutoff {
				// Everything from here on is still live (ascending order):
				// commit the truncation found so far and move straight to
				// the next key. A bare continue here would only advance to
				// the next timestamp of THIS key -- wasted scanning of
				// entries already known to be live.
				if evict > 0 {
					l.hist[key] = times[evict:]
				}
				continue keys
			}
			evict++
		}
		// Every entry in this key's history was stale.
		delete(l.hist, key)
	}
}

// Len reports how many keys currently have tracked history, for tests and
// monitoring dashboards that watch for unbounded growth.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hist)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ratewindow"
)

func main() {
	lim := ratewindow.NewLimiter(3, 1000) // 3 requests per 1000-unit window

	// Three requests inside the window: all allowed.
	for _, t := range []int64{0, 200, 400} {
		fmt.Println(t, lim.Allow("client-a", t))
	}
	// Fourth, still inside the window relative to all three: rejected.
	fmt.Println(500, lim.Allow("client-a", 500))
	// At t=1001, t=0 has aged out (cutoff=1001-1000=1); 200 and 400 remain,
	// so there is exactly one free slot.
	fmt.Println(1001, lim.Allow("client-a", 1001))

	fmt.Println("tracked keys before cleanup:", lim.Len())
	lim.Cleanup(5000) // long after client-a's last request; everything is stale
	fmt.Println("tracked keys after cleanup:", lim.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
0 true
200 true
400 true
500 false
1001 true
tracked keys before cleanup: 1
tracked keys after cleanup: 0
```

### Tests

`TestAllow` is a table covering a clean run under the limit, a rejection once
the limit is hit, the cutoff-equality boundary (a timestamp exactly at
`now-window` counts as stale, not live), and eviction freeing a slot once the
window has rolled past an old entry. `TestCleanupEvictsStaleKeysAndTrims` and
`TestCleanupPartialTrimKeepsLiveEntries` exercise the sweep itself: a fully
stale key disappears from the map, while a key with one stale and one live
entry is trimmed, not deleted, and its remaining quota is still enforced
correctly afterward.

Create `ratewindow_test.go`:

```go
package ratewindow

import "testing"

func TestAllow(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		limit  int
		window int64
		times  []int64 // requests to replay for "client", in order
		wantOK []bool  // Allow's result for each request in times
	}{
		"three requests inside the window all fit under a limit of three": {
			limit:  3,
			window: 1000,
			times:  []int64{0, 200, 400},
			wantOK: []bool{true, true, true},
		},
		"a fourth request inside the same window is rejected": {
			limit:  3,
			window: 1000,
			times:  []int64{0, 200, 400, 500},
			wantOK: []bool{true, true, true, false},
		},
		"a timestamp exactly at the cutoff boundary is stale, not live": {
			limit:  2,
			window: 1000,
			// window=1000: at t=1000, cutoff=0. t=0 is <= cutoff, so it is
			// evicted and there is room for the new request.
			times:  []int64{0, 1000},
			wantOK: []bool{true, true},
		},
		"eviction frees a slot for a later request after the window rolls": {
			limit:  1,
			window: 100,
			times:  []int64{0, 50, 101},
			wantOK: []bool{true, false, true},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lim := NewLimiter(tc.limit, tc.window)
			for i, ts := range tc.times {
				got := lim.Allow("client", ts)
				if got != tc.wantOK[i] {
					t.Fatalf("Allow(#%d, t=%d) = %v, want %v", i, ts, got, tc.wantOK[i])
				}
			}
		})
	}
}

func TestCleanupEvictsStaleKeysAndTrims(t *testing.T) {
	t.Parallel()

	lim := NewLimiter(5, 1000)
	lim.Allow("stale-client", 0)
	lim.Allow("stale-client", 100)
	lim.Allow("fresh-client", 4500)

	if got := lim.Len(); got != 2 {
		t.Fatalf("tracked keys before cleanup = %d, want 2", got)
	}

	lim.Cleanup(5000) // cutoff=4000: stale-client's entries are both < 4000

	if got := lim.Len(); got != 1 {
		t.Fatalf("tracked keys after cleanup = %d, want 1 (stale-client removed)", got)
	}
	// fresh-client must still be able to consume its remaining budget.
	if !lim.Allow("fresh-client", 4600) {
		t.Fatal("fresh-client should still have quota after cleanup")
	}
}

func TestCleanupPartialTrimKeepsLiveEntries(t *testing.T) {
	t.Parallel()

	lim := NewLimiter(2, 1000)
	lim.Allow("client", 0)
	lim.Allow("client", 900)

	lim.Cleanup(1000) // cutoff=0: only the t=0 entry is stale

	if got := lim.Len(); got != 1 {
		t.Fatalf("tracked keys after partial cleanup = %d, want 1", got)
	}
	// One live entry (t=900) remains, so exactly one more request fits.
	if !lim.Allow("client", 1000) {
		t.Fatal("expected room for one more request after partial trim")
	}
	if lim.Allow("client", 1000) {
		t.Fatal("expected the limit to be enforced again after the trim's slot is used")
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The limiter is correct when eviction is exact at the boundary (`t > cutoff`
is live, `t <= cutoff` is stale) and when `Cleanup` reclaims memory for keys
that have gone silent without touching keys that still have live entries.
The bug this exercise guards against is a `Cleanup` that only ever trims —
never deletes — a key whose history is entirely stale, leaving an
ever-growing map of empty slices behind for every client that was ever seen
once. The partial-trim test is the one to study: it proves the sweep
distinguishes "some entries stale" (trim in place) from "all entries stale"
(delete the key), rather than treating both the same way.

## Resources

- [Go Specification: Continue statements](https://go.dev/ref/spec#Continue_statements) — a labeled `continue` targets the named enclosing `for`.
- [Cloudflare: sliding window rate limiting](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) — why a sliding window avoids the fixed-bucket boundary-burst problem.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the shared history map against concurrent callers.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-pipeline-stage-graceful-shutdown.md](24-pipeline-stage-graceful-shutdown.md) | Next: [26-consistent-hash-shard-routing.md](26-consistent-hash-shard-routing.md)
