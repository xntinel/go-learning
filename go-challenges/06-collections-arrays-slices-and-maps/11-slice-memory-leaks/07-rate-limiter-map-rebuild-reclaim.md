# Exercise 7: Reclaim Heap From a Churning Rate-Limiter Map That Never Shrinks

A per-key rate limiter keeps a `map[string]*counter` keyed by client IP or API
key. Over days of uptime the key space churns — new IPs arrive, old ones expire —
and here is the trap: a Go map grows its bucket array but never gives it back.
`delete` removes entries but returns no memory, so the map's footprint is a
permanent high-water mark of every key it ever held at once. This module builds
the limiter, shows that a delete-in-place sweep does not reclaim, and fixes it with
a `Compact` that rebuilds into a fresh map.

## What you'll build

```text
ratelimit/                   independent module: example.com/ratelimit
  go.mod                     go 1.24
  ratelimit.go               type Limiter; New, SetClock, Allow, SweepExpired, Compact, Len, Counts
  cmd/
    demo/
      main.go                allow within limit, expire a window, compact away the dead key
  ratelimit_test.go          allow-semantics, compact-preserves, compact-reclaims-vs-sweep; -race
```

Files: `ratelimit.go`, `cmd/demo/main.go`, `ratelimit_test.go`.
Implement: a fixed-window `Limiter` with `Allow`, a naive `SweepExpired` (delete-in-place), and `Compact` (rebuild into a fresh map).
Test: assert `Allow` limits per window and `Compact` preserves live counts; a `HeapAlloc` delta proving `SweepExpired` does not reclaim and `Compact` does.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

## Delete does not shrink a map; rebuild does

The limiter is a fixed-window counter: `Allow(key)` starts a fresh window the first
time it sees a key (or after the current window elapses) and counts hits within
the window, allowing up to `limit`. The clock is injectable via `SetClock` so tests
and the demo can expire windows deterministically without sleeping.

The memory behavior is the lesson. A Go map's bucket array grows as keys are added
and is *never* shrunk: `delete(m, k)` unlinks an entry, and — as the test in this
module demonstrates on a 200,000-key map — it does not even reliably return the
per-entry memory to the point where a heap reading drops. `SweepExpired`, which
walks the map and deletes every expired key in place, leaves the map's footprint at
its high-water mark. For a limiter facing a churning, high-cardinality key space
(every scanning bot gets a bucket), that footprint only ever grows: the peak
concurrent key count becomes the permanent floor.

`Compact` is the fix. It allocates a *fresh*, size-hinted map, copies only the
still-live entries into it, and swaps it in. The old map — bucket spine and all —
becomes garbage and is collected, so the limiter's footprint drops to what the
live key set actually needs. The live `*counter` values are shared between the old
and new map during the copy, so they survive; only the expired entries and the
oversized old spine are released. This is the general shape of reclaiming any
high-churn map: you cannot shrink it, so you rebuild it and let the old one go.

The two methods are kept side by side so a single test can measure both: fill two
limiters identically, expire everything, sweep one and compact the other, and read
`HeapAlloc`. The swept limiter stays near its full footprint; the compacted one
drops to nearly nothing.

Create `ratelimit.go`:

```go
// Package ratelimit is a fixed-window per-key limiter whose map can be rebuilt to
// reclaim the heap a high-churn key space would otherwise pin forever.
package ratelimit

import (
	"sync"
	"time"
)

type counter struct {
	hits        int
	windowStart time.Time
}

// Limiter allows up to limit hits per key per window.
type Limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	m      map[string]*counter
}

// New returns a Limiter allowing limit hits per window per key.
func New(limit int, window time.Duration) *Limiter {
	return &Limiter{
		limit:  limit,
		window: window,
		now:    time.Now,
		m:      make(map[string]*counter),
	}
}

// SetClock overrides the time source (for tests and deterministic demos).
func (l *Limiter) SetClock(now func() time.Time) { l.now = now }

// Allow records a hit for key and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	t := l.now()
	c, ok := l.m[key]
	if !ok || t.Sub(c.windowStart) >= l.window {
		l.m[key] = &counter{hits: 1, windowStart: t}
		return true
	}
	c.hits++
	return c.hits <= l.limit
}

// SweepExpired deletes expired entries in place. It does NOT return bucket memory:
// the map keeps its high-water footprint. Kept only to contrast with Compact.
func (l *Limiter) SweepExpired() {
	l.mu.Lock()
	defer l.mu.Unlock()

	t := l.now()
	for k, c := range l.m {
		if t.Sub(c.windowStart) >= l.window {
			delete(l.m, k)
		}
	}
}

// Compact rebuilds the map, copying only still-live entries into a fresh,
// right-sized map and dropping the old one. This actually reclaims the heap.
func (l *Limiter) Compact() {
	l.mu.Lock()
	defer l.mu.Unlock()

	t := l.now()
	live := 0
	for _, c := range l.m {
		if t.Sub(c.windowStart) < l.window {
			live++
		}
	}
	fresh := make(map[string]*counter, live)
	for k, c := range l.m {
		if t.Sub(c.windowStart) < l.window {
			fresh[k] = c
		}
	}
	l.m = fresh
}

// Len reports the number of entries currently stored (expired or not).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}

// Counts returns a projected view of key -> hits, for assertions.
func (l *Limiter) Counts() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int, len(l.m))
	for k, c := range l.m {
		out[k] = c.hits
	}
	return out
}
```

## The runnable demo

The demo allows two hits for one key, rejects the third, then advances the clock
past the window and compacts — showing the expired key is both re-allowable and
removed from the map by `Compact`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ratelimit"
)

func main() {
	clock := time.Unix(0, 0)
	l := ratelimit.New(2, time.Minute)
	l.SetClock(func() time.Time { return clock })

	fmt.Println("hit 1 allowed:", l.Allow("1.2.3.4"))
	fmt.Println("hit 2 allowed:", l.Allow("1.2.3.4"))
	fmt.Println("hit 3 allowed:", l.Allow("1.2.3.4"))

	clock = clock.Add(2 * time.Minute) // expire the window
	l.Allow("5.6.7.8")                 // a new key in the new window
	fmt.Println("entries before compact:", l.Len())

	l.Compact()
	fmt.Println("entries after compact: ", l.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hit 1 allowed: true
hit 2 allowed: true
hit 3 allowed: false
entries before compact: 2
entries after compact:  1
```

## Tests

The semantics tests use the injected clock to drive windows deterministically. The
reclamation test fills two 200,000-key limiters identically, expires every entry,
sweeps one and compacts the other, and reads `HeapAlloc` after a forced GC: the
swept limiter stays near its full multi-megabyte footprint (the map did not
shrink), while the compacted one drops to almost nothing. It is non-parallel so
the global `HeapAlloc` reading is clean.

Create `ratelimit_test.go`:

```go
package ratelimit

import (
	"maps"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func readAlloc() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func TestAllowLimitsPerWindow(t *testing.T) {
	t.Parallel()

	clock := time.Unix(0, 0)
	l := New(2, time.Minute)
	l.SetClock(func() time.Time { return clock })

	if !l.Allow("k") || !l.Allow("k") {
		t.Fatal("first two hits must be allowed")
	}
	if l.Allow("k") {
		t.Fatal("third hit in the same window must be rejected")
	}

	clock = clock.Add(time.Minute) // new window
	if !l.Allow("k") {
		t.Fatal("first hit in the new window must be allowed")
	}
}

func TestCompactPreservesLiveCounts(t *testing.T) {
	t.Parallel()

	clock := time.Unix(0, 0)
	l := New(100, time.Minute)
	l.SetClock(func() time.Time { return clock })

	l.Allow("a")
	l.Allow("a")
	l.Allow("b")
	// Move to the next window and touch only "c"; a and b are now expired.
	clock = clock.Add(2 * time.Minute)
	l.Allow("c")

	l.Compact()

	want := map[string]int{"c": 1}
	if got := l.Counts(); !maps.Equal(got, want) {
		t.Fatalf("after Compact Counts = %v, want %v", got, want)
	}
}

func TestCompactReclaimsWhereSweepDoesNot(t *testing.T) {
	const n = 200_000
	fill := func(l *Limiter, at time.Time) {
		l.SetClock(func() time.Time { return at })
		for i := range n {
			l.Allow("key-" + strconv.Itoa(i))
		}
	}

	base := readAlloc()

	// Delete-in-place: the map keeps its high-water footprint.
	t0 := time.Unix(0, 0)
	swept := New(1, time.Minute)
	fill(swept, t0)
	swept.SetClock(func() time.Time { return t0.Add(2 * time.Minute) }) // all expired
	swept.SweepExpired()
	afterSweep := readAlloc()
	runtime.KeepAlive(swept)
	swept = nil

	// Rebuild: the old spine becomes garbage and is reclaimed.
	compacted := New(1, time.Minute)
	fill(compacted, t0)
	compacted.SetClock(func() time.Time { return t0.Add(2 * time.Minute) })
	compacted.Compact()
	afterCompact := readAlloc()
	runtime.KeepAlive(compacted)

	const fourMiB = 4 << 20
	if d := int64(afterSweep) - int64(base); d < fourMiB {
		t.Fatalf("delete-in-place unexpectedly reclaimed: delta %d bytes, want >= %d", d, fourMiB)
	}
	if d := int64(afterCompact) - int64(base); d >= fourMiB {
		t.Fatalf("Compact did not reclaim: delta %d bytes, want < %d", d, fourMiB)
	}
}
```

## Review

The limiter is correct when `Allow` enforces the per-window limit and `Compact`
preserves exactly the live entries (the `maps.Equal` projection proves it). The
reclamation test is the point: after deleting every expired key in place the map's
`HeapAlloc` stays multiple megabytes above baseline — the buckets did not shrink —
while `Compact` rebuilds into a fresh map and the reading drops back to baseline.
The mistake this module exists to prevent is believing `delete` or `clear` hands
map memory back; they do not. For any high-churn, high-cardinality map — rate
limiters, idempotency stores, session tables — periodically rebuild into a fresh,
size-hinted map. Run `go test -race` to confirm `Allow`, `SweepExpired`, and
`Compact` are safe under concurrent traffic.

## Resources

- [`builtin.delete` and `clear`](https://pkg.go.dev/builtin#delete) — remove entries but do not shrink the bucket array.
- [`maps.Copy` and `maps.Equal`](https://pkg.go.dev/maps#Copy) — copying into a fresh map and comparing projected views.
- [`runtime.MemStats`](https://pkg.go.dev/runtime#MemStats) — `HeapAlloc` before and after the rebuild.

---

Back to [06-pool-buffer-reset-cap-guard.md](06-pool-buffer-reset-cap-guard.md) | Next: [08-three-index-frame-window.md](08-three-index-frame-window.md)
