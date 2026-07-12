# Exercise 5: Bounding Memory — Per-Shard LRU Eviction

An unbounded cache is a memory leak with a nicer name: every distinct key ever
seen stays resident until it happens to expire, and a scan of ten million ids
takes your service with it. This exercise gives each shard a capacity and
evicts the least-recently-used entry on overflow, using the textbook
`map` + `container/list` combination — and confronts the real cost: LRU
bookkeeping destroys the read-lock optimization.

## What you'll build

```text
lrucache/                        independent module: example.com/lrucache
  go.mod
  cache/
    cache.go                     lruEntry (key, value, expiresAt),
                                 shard: items map[string]*list.Element + order *list.List,
                                 Cache[V]: New(shards, perShardCap),
                                 Set (evict from Back on overflow), Get (MoveToFront,
                                 full Lock), Delete, Size, Evictions
    cache_test.go                deterministic single-shard eviction tests,
                                 recency-protects-a-key test, overwrite-no-evict,
                                 concurrent churn bound under -race
  cmd/
    demo/
      main.go                    capacity-3 shard: insert 4, watch the LRU victim go
```

- Files: `cache/cache.go`, `cache/cache_test.go`, `cmd/demo/main.go`.
- Implement: per-shard capacity with strict LRU eviction from the list back, recency updates on both `Get` and `Set`, and an atomic eviction counter.
- Test: single-shard deterministic evictions (capacity 3), a `Get` that saves a key from eviction, overwrite not evicting, and a concurrent churn test asserting `Size <= capacity*shards`.
- Verify: `go test -count=1 -race ./...`

### The data structure: a map into a list

Strict LRU needs two lookups to be O(1): "find by key" and "find the oldest".
The classic answer is a `map[string]*list.Element` for the first and a
`container/list.List` ordered by recency for the second. Each `Element.Value`
carries a `*lruEntry` holding the key, the value, and the TTL deadline — the
key must live *in* the entry because eviction discovers the victim through the
list (`order.Back()`) and then needs its key to delete from the map.

Every operation maintains one invariant: the list front is the most recently
used entry, the back is the least. `Get` on a hit calls `MoveToFront`; `Set`
on a new key calls `PushFront`, on an existing key updates in place and calls
`MoveToFront`. When `len(items)` exceeds the shard's capacity, `Set` removes
`order.Back()` from both structures and increments the eviction counter — an
`atomic.Int64`, so metrics never widen any lock's critical section.

### The price: Get is a write now

In the plain TTL cache, `Get` took `RLock` and any number of readers proceeded
in parallel. Here `Get` calls `MoveToFront`, which *splices the list* — a
mutation. A read-locked `Get` doing that is a data race: two concurrent
readers moving elements corrupt the list's pointers, and `go test -race`
flags it immediately. So `Get` takes the exclusive `Lock`, and the 99%-read
workload that scaled beautifully in exercise 1 now serializes per shard.

This is not an implementation wart; it is the fundamental trade-off of strict
LRU, and it is why high-performance caches abandon it. Redis samples a few
random keys and evicts the approximately-oldest; ristretto tracks frequency
sketches (TinyLFU) and buffers recency updates so reads stay lock-free. Strict
LRU per shard — what you build here — is the honest middle ground: exact
recency, bounded memory, and contention capped by the shard count rather than
the whole cache.

One more approximation to be explicit about: capacity is *per shard*, so the
global bound is `capacity x shards` only if keys hash evenly. A hot shard
evicts while a cold one sits half-empty. Production systems accept this — the
bound still holds as a maximum, which is what the OOM killer cares about.

Create `cache/cache.go`:

```go
package cache

import (
	"container/list"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"
)

type lruEntry[V any] struct {
	key       string
	value     V
	expiresAt time.Time
}

func (e *lruEntry[V]) expired(now time.Time) bool {
	return !e.expiresAt.IsZero() && now.After(e.expiresAt)
}

type shard[V any] struct {
	mu    sync.Mutex // exclusive: Get mutates LRU order, so RWMutex buys nothing
	items map[string]*list.Element
	order *list.List // front = most recent, back = least recent
	cap   int
}

// Cache is a lock-striped LRU+TTL cache. Each shard holds at most cap
// entries; inserting into a full shard evicts its least-recently-used
// entry. Get refreshes recency, which is why it takes the write lock.
type Cache[V any] struct {
	shards    []*shard[V]
	numShards uint32
	evictions atomic.Int64
}

// New builds a cache with numShards shards of perShardCap entries each.
// The global bound is numShards*perShardCap, reached only under an even
// key distribution.
func New[V any](numShards, perShardCap int) *Cache[V] {
	if numShards < 1 {
		numShards = 1
	}
	if perShardCap < 1 {
		perShardCap = 1
	}
	shards := make([]*shard[V], numShards)
	for i := range shards {
		shards[i] = &shard[V]{
			items: make(map[string]*list.Element),
			order: list.New(),
			cap:   perShardCap,
		}
	}
	return &Cache[V]{shards: shards, numShards: uint32(numShards)}
}

func (c *Cache[V]) shardFor(key string) *shard[V] {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return c.shards[h.Sum32()%c.numShards]
}

// Set stores value under key. A non-positive TTL means "no expiration".
// If the shard is over capacity after an insert, the least-recently-used
// entry is evicted.
func (c *Cache[V]) Set(key string, value V, ttl time.Duration) {
	s := c.shardFor(key)
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if el, ok := s.items[key]; ok {
		// Overwrite in place: no eviction, recency refreshed.
		e := el.Value.(*lruEntry[V])
		e.value = value
		e.expiresAt = expiresAt
		s.order.MoveToFront(el)
		return
	}

	el := s.order.PushFront(&lruEntry[V]{key: key, value: value, expiresAt: expiresAt})
	s.items[key] = el

	if len(s.items) > s.cap {
		victim := s.order.Back()
		if victim != nil {
			s.order.Remove(victim)
			delete(s.items, victim.Value.(*lruEntry[V]).key)
			c.evictions.Add(1)
		}
	}
}

// Get returns the value for key and refreshes its recency; false if
// missing or expired. Expired entries are removed on sight.
func (c *Cache[V]) Get(key string) (V, bool) {
	s := c.shardFor(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	el, ok := s.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	e := el.Value.(*lruEntry[V])
	if e.expired(time.Now()) {
		s.order.Remove(el)
		delete(s.items, key)
		var zero V
		return zero, false
	}
	s.order.MoveToFront(el)
	return e.value, true
}

// Delete removes the entry for key. It is a no-op if the key is absent.
func (c *Cache[V]) Delete(key string) {
	s := c.shardFor(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		s.order.Remove(el)
		delete(s.items, key)
	}
}

// Size returns the number of non-expired entries across all shards.
func (c *Cache[V]) Size() int {
	now := time.Now()
	total := 0
	for _, s := range c.shards {
		s.mu.Lock()
		for _, el := range s.items {
			if !el.Value.(*lruEntry[V]).expired(now) {
				total++
			}
		}
		s.mu.Unlock()
	}
	return total
}

// Evictions reports how many entries have been evicted for capacity.
func (c *Cache[V]) Evictions() int64 {
	return c.evictions.Load()
}
```

Note two deliberate choices. The shard mutex is a plain `sync.Mutex`, not an
`RWMutex` — since `Get` needs exclusive access anyway, `RWMutex` would add
cost for nothing. And this `Get` removes expired entries on sight (it already
holds the write lock, so the delete is free), which the read-locked cache of
exercise 1 could not do.

### The demo

One shard, capacity three, four inserts — and then a recency save: reading
`a` before inserting `e` makes `b` the victim instead.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/lrucache/cache"
)

func main() {
	c := cache.New[int](1, 3) // one shard: deterministic eviction order

	c.Set("a", 1, time.Hour)
	c.Set("b", 2, time.Hour)
	c.Set("c", 3, time.Hour)
	c.Set("d", 4, time.Hour) // over capacity: evicts a (least recently used)

	_, aAlive := c.Get("a")
	fmt.Printf("after inserting d: a alive=%v evictions=%d\n", aAlive, c.Evictions())

	c.Get("b")               // touch b: it is now most recent
	c.Set("e", 5, time.Hour) // evicts c, not b

	_, bAlive := c.Get("b")
	_, cAlive := c.Get("c")
	fmt.Printf("after touching b and inserting e: b alive=%v c alive=%v\n", bAlive, cAlive)
	fmt.Printf("size=%d evictions=%d\n", c.Size(), c.Evictions())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after inserting d: a alive=false evictions=1
after touching b and inserting e: b alive=true c alive=false
size=3 evictions=2
```

### Tests

Determinism comes from construction, not sleeps: `New(1, 3)` puts every key in
one shard, so eviction order is exactly the LRU order and the tests can assert
which key died. The churn test is the capacity proof: 8 goroutines write 500
distinct keys each into a 4-shard, 16-per-shard cache, and at *no point* can
`Size` exceed 64 — asserted after the storm, with the race detector guarding
the during.

Create `cache/cache_test.go`:

```go
package cache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()

	c := New[int](1, 3)
	c.Set("a", 1, time.Hour)
	c.Set("b", 2, time.Hour)
	c.Set("c", 3, time.Hour)
	c.Set("d", 4, time.Hour) // a is LRU: evicted

	if _, ok := c.Get("a"); ok {
		t.Fatal("a survived; want it evicted as least recently used")
	}
	for _, k := range []string{"b", "c", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%s was evicted; want it live", k)
		}
	}
	if got := c.Evictions(); got != 1 {
		t.Fatalf("Evictions = %d, want 1", got)
	}
}

func TestGetRefreshesRecency(t *testing.T) {
	t.Parallel()

	c := New[int](1, 3)
	c.Set("a", 1, time.Hour)
	c.Set("b", 2, time.Hour)
	c.Set("c", 3, time.Hour)

	if _, ok := c.Get("a"); !ok { // a is now most recent
		t.Fatal("Get(a) missed unexpectedly")
	}
	c.Set("d", 4, time.Hour) // b is now LRU: evicted

	if _, ok := c.Get("a"); !ok {
		t.Fatal("a was evicted despite being recently read")
	}
	if _, ok := c.Get("b"); ok {
		t.Fatal("b survived; want it evicted as the true LRU")
	}
}

func TestOverwriteDoesNotEvict(t *testing.T) {
	t.Parallel()

	c := New[int](1, 3)
	c.Set("a", 1, time.Hour)
	c.Set("b", 2, time.Hour)
	c.Set("c", 3, time.Hour)
	c.Set("b", 20, time.Hour) // overwrite: still 3 entries, no eviction

	if got := c.Evictions(); got != 0 {
		t.Fatalf("Evictions = %d, want 0 (overwrite must not evict)", got)
	}
	if v, ok := c.Get("b"); !ok || v != 20 {
		t.Fatalf("Get(b) = %d,%v, want 20,true", v, ok)
	}
	if got := c.Size(); got != 3 {
		t.Fatalf("Size = %d, want 3", got)
	}
}

func TestExpiredEntryRemovedOnGet(t *testing.T) {
	t.Parallel()

	c := New[int](1, 3)
	c.Set("k", 1, 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get(k) after expiry: ok=true")
	}
	if got := c.Size(); got != 0 {
		t.Fatalf("Size = %d, want 0 (expired entry removed on Get)", got)
	}
}

func TestConcurrentChurnRespectsCapacity(t *testing.T) {
	t.Parallel()

	const shards, perShard = 4, 16
	c := New[int](shards, perShard)

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				key := fmt.Sprintf("k-%d-%d", g, i)
				c.Set(key, i, time.Hour)
				c.Get(key)
			}
		}()
	}
	wg.Wait()

	if got, bound := c.Size(), shards*perShard; got > bound {
		t.Fatalf("Size = %d exceeds capacity bound %d", got, bound)
	}
	if c.Evictions() == 0 {
		t.Fatal("no evictions under churn far beyond capacity; bound not enforced")
	}
}

func ExampleCache() {
	c := New[string](1, 2)
	c.Set("a", "1", time.Hour)
	c.Set("b", "2", time.Hour)
	c.Set("c", "3", time.Hour) // evicts a
	_, ok := c.Get("a")
	fmt.Println(ok)
	// Output: false
}
```

Run the gate:

```bash
gofmt -l . && go vet ./... && go test -count=1 -race ./...
```

## Review

The invariant to internalize: map and list always agree — every map value is a
live list element and every list element's key is in the map. All four
mutation sites (`Set` insert, `Set` overwrite, `Get` expiry-removal, eviction)
must update both structures under the same lock acquisition, or you get the
two classic corruptions: a map entry pointing at a removed element (panic on
`MoveToFront`), or a list element whose key was deleted (a zombie that can
never be evicted by key).

The mistake this module exists to teach: trying to keep `RLock` on `Get`.
`MoveToFront` writes; the race detector catches it under concurrent readers,
and the fix is the exclusive lock — which is precisely the cost that pushes
production caches toward sampled or frequency-based eviction. Also note
`Element.Value` is `any`: the `.(*lruEntry[V])` assertions are safe here only
because this package owns every insertion; that is an argument for keeping the
LRU internals unexported. Confirm with `go test -count=1 -race ./...` and
check the eviction counter in the churn test — a bound you never actually hit
is a bound you never actually tested.

## Resources

- [`container/list`](https://pkg.go.dev/container/list) — `PushFront`, `MoveToFront`, `Back`, `Remove`, and the `Element.Value any` field.
- [ristretto](https://github.com/hypermodeinc/ristretto) — a production cache that rejects strict LRU for TinyLFU precisely because of the read-path write cost.
- [Redis key eviction](https://redis.io/docs/latest/develop/reference/eviction/) — approximated LRU via sampling, the other escape from this trade-off.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-singleflight-loader.md](04-singleflight-loader.md) | Next: [06-ttl-jitter-negative-caching.md](06-ttl-jitter-negative-caching.md)
