# Exercise 22: Manage Connection Pool with LRU Eviction Under Concurrent Access

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A connection pool for a database or upstream service must cap how many idle
connections it keeps open, and the natural policy is to evict the
least-recently-used ones once the pool is over capacity. Multiple goroutines
touch the pool concurrently — one per request thread — while a background
janitor periodically ranges the pool to decide what to evict, so the pool's
internal map must be safely mutated while it is being read and ranged from
other goroutines at the same time. The module is fully self-contained: its
own `go mod init`, no external dependencies.

## What you'll build

```text
pool/                       independent module: example.com/connection-pool-lru-eviction
  go.mod                    go 1.24
  pool.go                   type Pool; Touch(id, now); EvictLRU(keep) []string
  cmd/
    demo/
      main.go               runnable demo: four connections, evict down to two
  pool_test.go              table test: eviction ordering + edge cases; concurrent Touch/EvictLRU under -race
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: `Pool.Touch(id string, now time.Time)` and
  `Pool.EvictLRU(keep int) []string`, both holding `p.mu` for the duration of
  their map access.
- Test: one table covering under-capacity (no-op), normal eviction, and
  keep-zero (evict everything); one concurrency test running `Touch` and
  `EvictLRU` from many goroutines under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Ranging under lock: the map is never exposed, only walked while held

The concurrency line from this lesson's concepts is simple to state and easy
to violate under pressure: a single goroutine mutating a map it is ranging is
fine, but any other goroutine reading or writing that map concurrently is a
data race, full stop — the runtime can even panic with "concurrent map
iteration and map write" without `-race` ever running. `EvictLRU` respects
this by taking `p.mu` before it ranges `p.conns` to build the `entries`
slice, and holding it through the subsequent `delete` calls; nothing outside
that critical section ever sees `p.conns` mid-range. `Touch` takes the same
mutex before its own map access, so the two methods are mutually exclusive
by construction — there is no window where one goroutine's `range` overlaps
another's `Touch` write.

The eviction policy itself is a two-step range: first, range the map once
under lock to snapshot every connection's `(id, lastAccess)` pair into an
ordinary slice — the map itself is never sorted, since maps have no order to
sort. Then sort that slice by `lastAccess` ascending (oldest first) and
delete the oldest `len(entries) - keep` entries directly from the map, still
under the same lock. Doing the sort on a snapshot slice rather than trying
to find "the minimum" repeatedly by re-ranging the map is both simpler and
correct: the map does not change mid-sort because the lock is still held.

Create `pool.go`:

```go
package pool

import (
	"sort"
	"sync"
	"time"
)

// conn is one idle connection tracked by the pool.
type conn struct {
	lastAccess time.Time
}

// Pool tracks idle connections keyed by ID and evicts the least-recently-used
// ones on demand. All methods are safe for concurrent use.
type Pool struct {
	mu      sync.Mutex
	conns   map[string]*conn
	evicted int
}

// New builds an empty Pool.
func New() *Pool {
	return &Pool{conns: make(map[string]*conn)}
}

// Touch marks id as accessed at now, adding it to the pool if it is not
// already tracked.
func (p *Pool) Touch(id string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	c, ok := p.conns[id]
	if !ok {
		c = &conn{}
		p.conns[id] = c
	}
	c.lastAccess = now
}

// EvictLRU ranges the pool under lock to find every connection's last-access
// time, then evicts the least-recently-used connections until at most keep
// remain. It returns the evicted IDs, sorted for a deterministic result.
func (p *Pool) EvictLRU(keep int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.conns) <= keep {
		return nil
	}

	type entry struct {
		id   string
		last time.Time
	}
	entries := make([]entry, 0, len(p.conns))
	for id, c := range p.conns {
		entries = append(entries, entry{id: id, last: c.lastAccess})
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].last.Equal(entries[j].last) {
			return entries[i].last.Before(entries[j].last)
		}
		return entries[i].id < entries[j].id
	})

	toEvict := len(entries) - keep
	evicted := make([]string, 0, toEvict)
	for i := 0; i < toEvict; i++ {
		id := entries[i].id
		delete(p.conns, id)
		evicted = append(evicted, id)
	}
	p.evicted += len(evicted)

	sort.Strings(evicted)
	return evicted
}

// Len reports how many connections the pool currently tracks.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// EvictedCount reports the cumulative number of connections evicted over the
// pool's lifetime, a metric a caller can export.
func (p *Pool) EvictedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.evicted
}
```

### The runnable demo

The demo touches four connections at increasing timestamps, then evicts down
to two, which must remove the two oldest.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/connection-pool-lru-eviction"
)

func main() {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := pool.New()

	p.Touch("conn-a", base)
	p.Touch("conn-b", base.Add(1*time.Second))
	p.Touch("conn-c", base.Add(2*time.Second))
	p.Touch("conn-d", base.Add(3*time.Second))

	evicted := p.EvictLRU(2)
	fmt.Printf("evicted=%v remaining=%d total_evicted=%d\n", evicted, p.Len(), p.EvictedCount())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
evicted=[conn-a conn-b] remaining=2 total_evicted=2
```

### Tests

The table drives `EvictLRU` through an under-capacity no-op, a normal
eviction, and the `keep == 0` edge case (evict everything). A separate
concurrency test runs many goroutines calling `Touch` and `EvictLRU` at the
same time and must pass under `-race`.

Create `pool_test.go`:

```go
package pool

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestEvictLRU(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		touch map[string]time.Duration
		keep  int
		want  []string
	}{
		{
			name:  "under capacity evicts nothing",
			touch: map[string]time.Duration{"a": 0},
			keep:  5,
			want:  nil,
		},
		{
			name: "evicts oldest until keep remain",
			touch: map[string]time.Duration{
				"conn-a": 0,
				"conn-b": 1 * time.Second,
				"conn-c": 2 * time.Second,
				"conn-d": 3 * time.Second,
			},
			keep: 2,
			want: []string{"conn-a", "conn-b"},
		},
		{
			name: "keep zero evicts everything",
			touch: map[string]time.Duration{
				"only": 0,
			},
			keep: 0,
			want: []string{"only"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := New()
			// Touch in a stable order derived from the duration so timestamps
			// are deterministic regardless of map iteration order here in
			// the test itself.
			type kv struct {
				id string
				d  time.Duration
			}
			var ordered []kv
			for id, d := range tc.touch {
				ordered = append(ordered, kv{id, d})
			}
			for _, e := range ordered {
				p.Touch(e.id, base.Add(e.d))
			}

			got := p.EvictLRU(tc.keep)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("EvictLRU(%d) = %v, want %v", tc.keep, got, tc.want)
			}
			if p.Len() != len(tc.touch)-len(tc.want) {
				t.Errorf("Len() = %d, want %d", p.Len(), len(tc.touch)-len(tc.want))
			}
		})
	}
}

func TestPoolConcurrentTouchAndEvict(t *testing.T) {
	t.Parallel()

	p := New()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "conn-" + string(rune('A'+i%26))
			p.Touch(id, base.Add(time.Duration(i)*time.Millisecond))
		}(i)
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.EvictLRU(5)
		}()
	}
	wg.Wait()

	if p.Len() > 26 {
		t.Fatalf("Len() = %d, want <= 26 distinct connection IDs", p.Len())
	}
}
```

Run it:

```bash
go test -count=1 -race ./...
```

## Review

The pool is correct when `EvictLRU` always removes the connections with the
oldest `lastAccess` first, never removes more than needed to reach `keep`,
and never races with a concurrent `Touch`. The bug this design specifically
avoids is releasing the lock between "decide what to evict" and "actually
evict it" — if `EvictLRU` computed the LRU candidates under lock, released
it, then reacquired it to delete them, a concurrent `Touch` could update one
of those candidates in between, and the eviction would then throw away a
connection that was just used. Holding `p.mu` for the entire read-decide-
mutate sequence is what makes the eviction atomic with respect to `Touch`.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [Go Specification: For statements (range over map, concurrency)](https://go.dev/ref/spec#For_range)
- [The Go Memory Model](https://go.dev/ref/mem) — why unsynchronized concurrent map access is undefined behavior.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-sql-column-projection-iterator.md](21-sql-column-projection-iterator.md) | Next: [23-multi-layer-cache-invalidation.md](23-multi-layer-cache-invalidation.md)
