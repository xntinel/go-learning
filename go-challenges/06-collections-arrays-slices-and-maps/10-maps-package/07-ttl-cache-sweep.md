# Exercise 7: TTL Cache Eviction Sweep with DeleteFunc

Every TTL cache needs a janitor: a periodic pass that removes entries whose
expiry has passed, so memory does not grow without bound. That sweep is
`maps.DeleteFunc` applied under a lock — one in-place traversal that prunes the
expired entries and returns how many it removed. This module builds the cache and
its sweep and tests it against a fixed clock.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It gates alone.

## What you'll build

```text
sweepcache/                 independent module: example.com/sweepcache
  go.mod                    go 1.26
  sweepcache.go             Cache (Mutex), Set, Get, Len, Sweep
  cmd/
    demo/
      main.go               set entries with mixed expiries, sweep, print evicted
  sweepcache_test.go        fixed-clock sweep count, empty-map no-op, -race Get/Set/Sweep
```

Files: `sweepcache.go`, `cmd/demo/main.go`, `sweepcache_test.go`.
Implement: `Cache` guarded by `sync.Mutex`; `Set(key, value, expiry)`, `Get`, `Len`, `Sweep(now) int`.
Test: a fixed-clock sweep removes only expired entries and returns the right count; a sweep of an empty map is a no-op; concurrent `Get`/`Set`/`Sweep` under `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/sweepcache/cmd/demo
cd ~/go-exercises/sweepcache
go mod init example.com/sweepcache
```

## Why DeleteFunc-in-place beats rebuild-on-every-get

A cache with lazy expiry — `Get` treats an entry as gone once it is past its
deadline but leaves it in the map — leaks memory: an entry that is set and never
read again sits there forever. The janitor closes that leak. The naive janitor
allocates a new map and copies the survivors on every pass; on a large cache swept
frequently, that is a full-size allocation and a GC churn each time. The right
primitive is `maps.DeleteFunc(m, pred)`, which walks the map once and deletes the
expired entries in place — no new allocation, and deleting during the traversal is
explicitly allowed by the language.

`Sweep(now)` takes the mutex, counts and deletes with `maps.DeleteFunc` where
`now` is not before the entry's expiry, and returns the evicted count. Taking
`now` as a parameter rather than calling `time.Now()` inside makes the sweep
testable against a fixed clock: the test passes an exact instant and asserts
precisely which entries went. In production a background goroutine on a
`time.Ticker` calls `Sweep(time.Now())` under that same lock.

The sweep runs under the write lock because it mutates the shared map;
`maps.DeleteFunc` does no locking of its own, so a concurrent `Set` during the
sweep would be a data race. `Get` and `Set` take the same mutex, so the three
compose safely — which the `-race` test verifies.

Create `sweepcache.go`:

```go
package sweepcache

import (
	"maps"
	"sync"
	"time"
)

type entry[V any] struct {
	value   V
	expires time.Time
}

// Cache is a concurrency-safe TTL cache with an explicit sweep for eviction.
type Cache[K comparable, V any] struct {
	mu    sync.Mutex
	items map[K]entry[V]
}

func New[K comparable, V any]() *Cache[K, V] {
	return &Cache[K, V]{items: make(map[K]entry[V])}
}

// Set stores value under key with an absolute expiry instant.
func (c *Cache[K, V]) Set(key K, value V, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[V]{value: value, expires: expires}
}

// Get returns the value if present and not yet expired as of now.
func (c *Cache[K, V]) Get(key K, now time.Time) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok || !now.Before(e.expires) {
		var zero V
		return zero, false
	}
	return e.value, true
}

// Len reports the number of stored entries, expired or not.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Sweep evicts every entry whose expiry is at or before now, in place, and
// returns the number evicted. This is the background janitor's per-tick work.
func (c *Cache[K, V]) Sweep(now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	before := len(c.items)
	maps.DeleteFunc(c.items, func(_ K, e entry[V]) bool {
		return !now.Before(e.expires)
	})
	return before - len(c.items)
}
```

The eviction predicate `!now.Before(e.expires)` reads as "now is not before the
expiry," i.e. the deadline has arrived or passed — the same condition `Get` uses to
treat an entry as absent, so a swept entry and a lazily-expired one agree exactly.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/sweepcache"
)

func main() {
	base := time.Unix(1000, 0)
	c := sweepcache.New[string, string]()
	c.Set("short", "a", base.Add(10*time.Second))
	c.Set("medium", "b", base.Add(60*time.Second))
	c.Set("long", "c", base.Add(600*time.Second))

	fmt.Println("stored:", c.Len())

	// Sweep 30 seconds in: only "short" has expired.
	evicted := c.Sweep(base.Add(30 * time.Second))
	fmt.Println("evicted at +30s:", evicted)
	fmt.Println("remaining:", c.Len())

	if _, ok := c.Get("medium", base.Add(30*time.Second)); ok {
		fmt.Println("medium still live")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stored: 3
evicted at +30s: 1
remaining: 2
medium still live
```

### Tests

`TestSweepEvictsOnlyExpired` sets entries with mixed expiries, sweeps at a fixed
instant, and asserts both the evicted count and which keys survived.
`TestSweepEmptyIsNoOp` sweeps an empty cache and asserts it returns zero.
`TestConcurrentGetSetSweep` runs the three operations across goroutines under
`-race`.

Create `sweepcache_test.go`:

```go
package sweepcache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSweepEvictsOnlyExpired(t *testing.T) {
	t.Parallel()

	base := time.Unix(0, 0)
	c := New[string, int]()
	c.Set("a", 1, base.Add(10*time.Second))
	c.Set("b", 2, base.Add(20*time.Second))
	c.Set("c", 3, base.Add(30*time.Second))

	// Sweep at +20s: a and b are at/past expiry, c survives.
	evicted := c.Sweep(base.Add(20 * time.Second))
	if evicted != 2 {
		t.Fatalf("Sweep evicted %d, want 2", evicted)
	}
	if c.Len() != 1 {
		t.Fatalf("Len after sweep = %d, want 1", c.Len())
	}
	if _, ok := c.Get("c", base.Add(20*time.Second)); !ok {
		t.Error("c should have survived the sweep")
	}
	if _, ok := c.Get("a", base.Add(20*time.Second)); ok {
		t.Error("a should have been evicted")
	}
}

func TestSweepEmptyIsNoOp(t *testing.T) {
	t.Parallel()

	c := New[string, int]()
	if n := c.Sweep(time.Now()); n != 0 {
		t.Fatalf("Sweep of empty cache = %d, want 0", n)
	}
}

func TestConcurrentGetSetSweep(t *testing.T) {
	t.Parallel()

	base := time.Unix(0, 0)
	c := New[int, int]()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			c.Set(i, i, base.Add(time.Duration(i)*time.Second))
		}()
		go func() {
			defer wg.Done()
			c.Get(i, base)
		}()
		go func() {
			defer wg.Done()
			c.Sweep(base.Add(50 * time.Second))
		}()
	}
	wg.Wait()
}

func ExampleCache_Sweep() {
	base := time.Unix(0, 0)
	c := New[string, int]()
	c.Set("x", 1, base.Add(5*time.Second))
	c.Set("y", 2, base.Add(50*time.Second))
	fmt.Println(c.Sweep(base.Add(10 * time.Second)))
	// Output: 1
}
```

## Review

The sweep is correct when it evicts exactly the entries whose deadline has arrived —
`!now.Before(e.expires)`, the same boundary `Get` uses — and returns the true count,
which `before - len` computes without a separate tally. The design choices to
defend: `maps.DeleteFunc` prunes in place so the janitor allocates nothing, and the
sweep holds the write lock because `DeleteFunc` offers no synchronization of its own
and a concurrent `Set` would race. Taking `now` as a parameter is what makes the
eviction boundary testable to the second. Run `go test -race` to confirm the three
operations compose safely.

## Resources

- [maps package](https://pkg.go.dev/maps) — `DeleteFunc`, in-place pruning.
- [sync package](https://pkg.go.dev/sync) — `Mutex`.
- [time package](https://pkg.go.dev/time) — `Time.Before`, `Ticker` for the real janitor.

---

Back to [06-streaming-map-collect.md](06-streaming-map-collect.md) | Next: [08-set-operations-scopes.md](08-set-operations-scopes.md)
