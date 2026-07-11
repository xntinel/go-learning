# Exercise 6: Guard Compound Cache State with RWMutex (and Know When Atomic Fails)

The primitive-choice skill has a sharp edge: a single atomic protects one word, but
the moment an invariant spans two fields it cannot help you. This module builds a
TTL cache whose entry map, size, and hit counter must always agree, shows in prose
why an atomic-only fix tears that invariant, and implements the correct
`sync.RWMutex`-guarded compound update. An invariant test under `-race` proves the
map length, the reported size, and the observed hits stay consistent.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
statcache/                 independent module: example.com/statcache
  go.mod                   module example.com/statcache
  statcache.go             Cache: entries map + size + hits, all guarded by one RWMutex; Get/Set/Evict/Stats
  cmd/
    demo/
      main.go              runnable demo: set, hit, miss, evict; print consistent stats
  statcache_test.go        invariant test under -race: len(entries)==size, hits matches observed hits
```

- Files: `statcache.go`, `cmd/demo/main.go`, `statcache_test.go`.
- Implement: a `Cache` with `entries map[string]entry`, an aggregate `size`, and a `hits` counter, all under one `sync.RWMutex`; `Get`, `Set`, `Evict`, `Stats`.
- Test: concurrent `Get`/`Set`/`Evict`; after `Wait`, assert `len(entries) == reported size` and the hits counter equals observed hits, under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/statcache/cmd/demo
cd ~/go-exercises/statcache
go mod init example.com/statcache
```

### Why a single atomic cannot guard this invariant

The cache maintains three pieces of state that must agree: the `entries` map, a
`size` that must equal `len(entries)`, and a `hits` counter. Suppose you tried to
"avoid the lock" by making `size` an `atomic.Int64`. `Set` would do
`entries[k] = e` and then `size.Add(1)` as two separate steps. Between those two
steps, a concurrent `Stats` reader can observe the map with the new entry but the
old size — the invariant `size == len(entries)` tears. Worse, the map write itself
is unsynchronized and Go will crash with "concurrent map writes". A single atomic
protects one word in isolation; it cannot make a *pair* of updates (map + counter)
appear atomic relative to a reader. That is precisely the shape that demands a
mutex: the critical section spans multiple fields whose relationship is the
invariant.

So `Set` takes the exclusive `Lock`, mutates the map and the size counter together
inside one critical section, and releases; a reader taking the `RLock` therefore
never sees a half-applied update. `Get` is on the read-heavy path but it also
increments `hits`, which is a *write* — so `Get` cannot use `RLock` alone. This is
a classic subtlety: a "read" that mutates a counter is a writer for
synchronization purposes. Here `Get` takes the full `Lock` because it updates
`hits` and may lazily drop an expired entry (which changes the map and size). If
you wanted `hits` to be cheap you could split it into a separate `atomic.Int64`,
but then `hits` would no longer be part of the same consistent snapshot as `size`;
keeping it under the lock is what lets `Stats` return one coherent picture.

`RWMutex` vs `Mutex` here is a real trade-off. `RLock` lets multiple pure readers
(`Stats`, and a hypothetical peek that does not touch `hits`) overlap, which pays
off if reads dominate and the read critical section is non-trivial. But `RWMutex`
has higher per-operation overhead than `Mutex` and can starve writers; for a cache
whose every `Get` is actually a writer, a plain `Mutex` may well be faster. This
module uses `RWMutex` so `Stats` and other pure readers can overlap, and documents
the trade-off honestly.

Create `statcache.go`:

```go
package statcache

import (
	"sync"
	"time"
)

type entry struct {
	value   string
	expires time.Time
}

// Cache holds compound state: the entries map, an aggregate size that must equal
// len(entries), and a hits counter. Because the invariant spans multiple fields,
// a single atomic cannot guard it; one RWMutex protects all three together.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]entry
	size    int
	hits    int
}

func NewCache() *Cache {
	return &Cache{entries: make(map[string]entry)}
}

// Set stores value under key with the given TTL, updating the map and size in a
// single critical section so they never disagree.
func (c *Cache) Set(key, value string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, existed := c.entries[key]; !existed {
		c.size++
	}
	c.entries[key] = entry{value: value, expires: time.Now().Add(ttl)}
}

// Get returns the value if present and unexpired, recording a hit. It takes the
// exclusive lock because it writes hits and may drop an expired entry (which
// changes both the map and size).
func (c *Cache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if !time.Now().Before(e.expires) {
		delete(c.entries, key)
		c.size--
		return "", false
	}
	c.hits++
	return e.value, true
}

// Evict removes key if present, keeping size in step with the map.
func (c *Cache) Evict(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; ok {
		delete(c.entries, key)
		c.size--
	}
}

// Stats returns a coherent snapshot of size and hits under a read lock.
func (c *Cache) Stats() (size, hits int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.size, c.hits
}

// mapLen exposes the raw map length under the read lock, for the invariant test.
func (c *Cache) mapLen() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/statcache"
)

func main() {
	c := statcache.NewCache()
	c.Set("a", "1", time.Minute)
	c.Set("b", "2", time.Minute)

	c.Get("a") // hit
	c.Get("a") // hit
	c.Get("x") // miss (no hit recorded)

	c.Evict("b")

	size, hits := c.Stats()
	fmt.Printf("size=%d hits=%d\n", size, hits)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
size=1 hits=2
```

### Tests

`TestInvariantUnderConcurrency` is the load-bearing test: concurrent `Set`, `Get`,
and `Evict` goroutines hammer the cache, and after `Wait` it asserts the reported
`size` equals the raw `len(entries)` — the invariant that an atomic-only fix would
tear. `TestHitsCountedExactly` drives a known number of successful `Get`s and
asserts the `hits` counter matches, proving the counter update is inside the same
critical section as the lookup.

Create `statcache_test.go`:

```go
package statcache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestInvariantUnderConcurrency(t *testing.T) {
	t.Parallel()

	c := NewCache()
	var wg sync.WaitGroup

	for i := range 100 {
		key := fmt.Sprintf("k%d", i%20)
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Set(key, "v", time.Minute)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Get(key)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%3 == 0 {
				c.Evict(key)
			}
		}()
	}
	wg.Wait()

	size, _ := c.Stats()
	if size != c.mapLen() {
		t.Fatalf("invariant broken: reported size=%d, len(entries)=%d", size, c.mapLen())
	}
	if size < 0 {
		t.Fatalf("size went negative: %d", size)
	}
}

func TestHitsCountedExactly(t *testing.T) {
	t.Parallel()

	c := NewCache()
	c.Set("a", "1", time.Minute)

	const reads = 50
	for range reads {
		if _, ok := c.Get("a"); !ok {
			t.Fatal("Get(a) missed unexpectedly")
		}
	}
	c.Get("missing") // must not count

	_, hits := c.Stats()
	if hits != reads {
		t.Fatalf("hits = %d, want %d", hits, reads)
	}
}

func TestExpiredEntryDropsSize(t *testing.T) {
	t.Parallel()

	c := NewCache()
	c.Set("a", "1", time.Nanosecond)
	time.Sleep(time.Millisecond) // let it expire against the real clock

	if _, ok := c.Get("a"); ok {
		t.Fatal("expected expired entry to be absent")
	}
	if size, _ := c.Stats(); size != 0 {
		t.Fatalf("size = %d after expiring the only entry, want 0", size)
	}
}

func Example() {
	c := NewCache()
	c.Set("a", "1", time.Minute)
	c.Get("a")
	c.Get("a")
	size, hits := c.Stats()
	fmt.Printf("size=%d hits=%d\n", size, hits)
	// Output: size=1 hits=2
}
```

## Review

The cache is correct when `size` always equals `len(entries)` and `hits` matches
the number of successful `Get`s, under a clean `-race` run. The mistake this module
exists to prevent is reaching for an atomic to "avoid the lock" on `size`: because
the invariant spans the map and the counter, an atomic `size` update separate from
the map write tears the invariant (and the unsynchronized map write crashes the
runtime outright). The compound update must live in one critical section. Note the
subtlety that `Get` is a *writer* — it increments `hits` and may drop an expired
entry — so it cannot use `RLock`; only genuinely pure readers like `Stats` take the
read lock. The `RWMutex`-vs-`Mutex` choice is a real trade-off; here reads that do
not touch `hits` can overlap, but for a workload where every `Get` writes, a plain
`Mutex` may be faster.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — reader/writer locking and the starvation trade-off.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the cheaper choice for short, write-heavy critical sections.
- [The Go Memory Model](https://go.dev/ref/mem) — why a multi-field invariant needs one happens-before edge, not two atomics.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-slice-backing-array-aliasing.md](07-slice-backing-array-aliasing.md)
