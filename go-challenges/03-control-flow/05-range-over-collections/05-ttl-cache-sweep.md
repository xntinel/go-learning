# Exercise 5: TTL Cache Sweep — safely deleting map entries during iteration

An in-memory cache eventually has to reclaim expired entries, and the natural place
to do it is a periodic sweep that ranges the map and deletes what has expired. This
module builds that sweep and settles a question people are often unsure about:
deleting from a map *during* its own `range` is defined and safe in Go. It also
draws the hard line — you must not let another goroutine write the map while a
sweep ranges it — with an `RWMutex` and a race-tested concurrent path.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
ttlsweep/                   independent module: example.com/ttlsweep
  go.mod                    go 1.24
  ttlsweep.go               Cache; Set, Get, Len, Sweep(now); RWMutex-guarded
  cmd/
    demo/
      main.go               runnable demo: seed entries, sweep at a fixed time, print survivors
  ttlsweep_test.go          mixed-expiry sweep, all-expired, empty-map, concurrent Sweep+Get (-race)
```

- Files: `ttlsweep.go`, `cmd/demo/main.go`, `ttlsweep_test.go`.
- Implement: `Cache` with `Set(key, value, expiresAt)`, `Get(key, now)`, `Len()`, and `Sweep(now)` that deletes expired entries in a `for k := range m { delete(m, k) }` loop under a write lock.
- Test: mixed expiries (only expired removed, live retained), all-expired, empty-map, and concurrent Sweep-while-Getters under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Delete-in-place during range is legal

The Go spec is explicit: the iteration variables of a `range` over a map may be
mutated, and entries may be deleted during iteration. If you delete a key the loop
has not yet reached, that key will simply not be produced. So the direct sweep is
correct and idiomatic:

```go
for k, e := range c.items {
	if !now.Before(e.expiresAt) { // now >= expiresAt: expired
		delete(c.items, k)
	}
}
```

There is a second, equivalent pattern — collect the doomed keys into a slice while
ranging, then delete them in a follow-up loop — which some teams prefer for clarity
or when the delete condition is expensive to evaluate. Both are safe *for a single
goroutine*. This exercise uses the delete-in-place form because it allocates nothing
and reads directly.

The addition rule is the asymmetric one: adding a key during a map range may or may
not surface the new key in that same range, so you never rely on it. Sweeping only
deletes, so this never bites us.

### The concurrency line is where it gets dangerous

Delete-in-place is safe only because the sweep is the *only* goroutine touching the
map during the range. The instant a second goroutine writes the map while the sweep
ranges it, you have a data race: the runtime can panic with "concurrent map
iteration and map write" even without `-race`, and `-race` flags it deterministically.
Real caches are read and written concurrently, so the sweep must hold a write lock
and readers must hold a read lock.

We use `sync.RWMutex`: `Get` takes `RLock` (many readers proceed together), and
`Sweep` and `Set` take the full `Lock` (exclusive). While `Sweep` holds the write
lock, no reader can be mid-range, so the delete-in-place loop is safe. The concurrent
test drives many `Get`s against a running `Sweep` under `-race` to prove the locking
is correct.

Note `Get` and `Sweep` both take an explicit `now time.Time` so expiry is a pure
function of the argument — the tests pass a fixed instant and assert exactly, with
no dependence on the wall clock.

Create `ttlsweep.go`:

```go
package ttlsweep

import (
	"sync"
	"time"
)

type entry struct {
	value     string
	expiresAt time.Time
}

// Cache is a concurrency-safe string cache with per-entry expiry. A Sweep reclaims
// expired entries; Get treats an expired entry as absent.
type Cache struct {
	mu    sync.RWMutex
	items map[string]entry
}

func New() *Cache {
	return &Cache{items: make(map[string]entry)}
}

// Set stores value under key, expiring at expiresAt.
func (c *Cache) Set(key, value string, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry{value: value, expiresAt: expiresAt}
}

// Get returns the value if present and not expired as of now.
func (c *Cache) Get(key string, now time.Time) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.items[key]
	if !ok || !now.Before(e.expiresAt) {
		return "", false
	}
	return e.value, true
}

// Len reports the number of stored entries, expired or not.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// Sweep deletes every entry expired as of now and returns how many it removed. It
// deletes in place during the range, which is safe under the exclusive lock.
func (c *Cache) Sweep(now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := 0
	for k, e := range c.items {
		if !now.Before(e.expiresAt) {
			delete(c.items, k)
			removed++
		}
	}
	return removed
}
```

### The runnable demo

The demo seeds three entries with different expiries, sweeps at a fixed instant, and
prints how many were removed and how many remain.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ttlsweep"
)

func main() {
	base := time.Unix(1000, 0).UTC()
	c := ttlsweep.New()
	c.Set("a", "1", base.Add(10*time.Second)) // expires later: survives
	c.Set("b", "2", base.Add(-5*time.Second)) // already expired
	c.Set("c", "3", base.Add(-1*time.Second)) // already expired

	removed := c.Sweep(base)
	fmt.Printf("removed=%d remaining=%d\n", removed, c.Len())

	if v, ok := c.Get("a", base); ok {
		fmt.Printf("a=%s survived\n", v)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
removed=2 remaining=1
a=1 survived
```

### Tests

The mixed-expiry test seeds live and expired entries, sweeps at a fixed instant,
and asserts only the expired keys were removed and the live ones remain readable.
The edge tests cover an all-expired cache (swept to empty) and an empty cache
(sweep is a no-op returning 0). The concurrency test runs a `Sweep` loop while many
goroutines `Get` under `-race`, proving the `RWMutex` prevents the concurrent
iteration/write race and no panic occurs.

Create `ttlsweep_test.go`:

```go
package ttlsweep

import (
	"sync"
	"testing"
	"time"
)

func fixedNow() time.Time { return time.Unix(1000, 0).UTC() }

func TestSweepRemovesOnlyExpired(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	c := New()
	c.Set("live1", "a", now.Add(time.Minute))
	c.Set("live2", "b", now.Add(time.Second))
	c.Set("dead1", "c", now.Add(-time.Second))
	c.Set("dead2", "d", now) // expiresAt == now counts as expired

	removed := c.Sweep(now)
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
	if _, ok := c.Get("live1", now); !ok {
		t.Fatal("live1 should survive")
	}
	if _, ok := c.Get("dead1", now); ok {
		t.Fatal("dead1 should be gone")
	}
}

func TestSweepAllExpired(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	c := New()
	c.Set("x", "1", now.Add(-time.Second))
	c.Set("y", "2", now.Add(-time.Hour))

	if removed := c.Sweep(now); removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0", c.Len())
	}
}

func TestSweepEmptyMap(t *testing.T) {
	t.Parallel()
	c := New()
	if removed := c.Sweep(fixedNow()); removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
}

func TestConcurrentSweepAndGet(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	c := New()
	for i := range 200 {
		key := string(rune('A' + i%26))
		exp := now.Add(time.Duration(i%3) * time.Second) // some expired, some live
		c.Set(key, "v", exp)
	}

	var wg sync.WaitGroup
	// Readers.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				c.Get(string(rune('A'+i%26)), now)
			}
		}()
	}
	// Sweeper.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 50 {
			c.Sweep(now)
		}
	}()
	wg.Wait()
}
```

## Review

The sweep is correct when it removes exactly the entries whose `expiresAt` is at or
before `now` and leaves the rest readable. The conceptual takeaway is that
`delete(m, k)` inside `for k := range m` is legal Go, not a hack — so you do not
need the collect-then-delete dance unless you prefer it. The real hazard is
concurrency: a sweep that ranges the map while another goroutine writes it is a data
race that can panic in production, which the `RWMutex` and the `-race` concurrent
test guard against. Run `go test -race`; a green run is the proof the locking holds.

## Resources

- [Go Specification: For statements (range, map deletion)](https://go.dev/ref/spec#For_range)
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex)
- [The delete builtin](https://pkg.go.dev/builtin#delete)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-worker-pool-fan-in.md](04-worker-pool-fan-in.md) | Next: [06-retry-backoff-counted-loop.md](06-retry-backoff-counted-loop.md)
