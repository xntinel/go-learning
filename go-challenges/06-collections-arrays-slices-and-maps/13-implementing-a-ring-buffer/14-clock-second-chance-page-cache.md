# Exercise 14: CLOCK Second-Chance Page Replacement Over a Ring

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every buffer pool needs an eviction policy, and true LRU -- a doubly linked
list re-ordered on every access -- is more machinery than most caches need.
PostgreSQL's buffer manager, and the virtual-memory subsystem of most
operating systems, use a cheaper approximation instead: the CLOCK
(second-chance) algorithm. A fixed ring of frames, each holding one
reference bit, is swept by a single hand. On eviction the hand walks
forward: an occupied frame with its reference bit set gets that bit cleared
and one more lap of safety; the first frame the hand finds with the bit
already clear is the victim. No list re-linking, no per-access pointer
chasing -- just one bit per frame and one moving index, which is the ring
buffer this lesson has been building all along, wearing yet another hat.

The entire value of the algorithm lives in that one bit. Skip checking it and
CLOCK degrades into plain round-robin eviction: the hand evicts whatever
frame it lands on next, regardless of whether that frame was read a
microsecond ago or has sat untouched since it was loaded. A cache that
"forgets" to look at the reference bit passes every simple test -- insert
past capacity, watch something get evicted, done -- because round-robin and
CLOCK produce the exact same eviction under the *first* overflow. The
difference only shows up once a hot key is accessed between two evictions,
which is precisely the situation the reference bit exists to protect, and
precisely the situation a superficial test never constructs.

This module builds `Cache[K, V]`, a fixed-capacity cache using the CLOCK
algorithm over a ring of frames.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
clockcache/               module example.com/clockcache
  go.mod                   go 1.24
  clockcache.go            Cache[K, V]; New, Get, Put, Len, Cap; one sentinel error
  clockcache_test.go        capacity validation, basic hit/miss, update-without-evict,
                           the second-chance sequence, the round-robin contrast,
                           ExampleCache_Get
```

- Files: `clockcache.go`, `clockcache_test.go`.
- Implement: `New[K comparable, V any](capacity int) (*Cache[K, V], error)` rejecting a non-positive capacity with `ErrInvalidCapacity`; `(*Cache[K, V]).Get(key K) (V, bool)` setting the frame's reference bit on a hit; `(*Cache[K, V]).Put(key K, value V)` inserting below capacity, updating in place with a reference-bit refresh, or running one CLOCK sweep to choose a victim once full.
- Test: capacity validation; a miss on an empty cache; inserting below capacity never evicts; updating an existing key never evicts and does set its reference bit; the second-chance sequence -- fill, evict once, `Get` one survivor, evict again -- pinned against a real captured run; the round-robin contrast; and `ExampleCache_Get` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/14-clock-second-chance-page-cache
cd go-solutions/06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/14-clock-second-chance-page-cache
go mod edit -go=1.24
```

### One bit turns a ring into an approximation of recency

Every ring buffer so far in this lesson evicted from a fixed end -- `Pop`
always reads `tail`, `Push` always overwrites at `tail` when full. CLOCK
generalizes that: instead of always evicting the structurally oldest slot, a
single hand sweeps the ring looking for a slot that is *behaviorally* stale,
using one bit of feedback per frame to decide. That bit is set whenever a
frame is touched -- by `Get`, or by the `Put` that inserted it -- and cleared
the moment the hand passes over it while searching for a victim. A frame
survives a sweep exactly once per access: touch it, and it gets one more
full lap of immunity before it can be evicted; touch it again before the
hand comes back around, and it earns another lap.

The bug this design invites is dropping the bit check and keeping only the
moving hand:

```go
func (r *roundRobin) evict() int {
    victim := r.hand
    r.hand = (r.hand + 1) % len(r.frames)   // just... the next slot
    return victim
}
```

This is CLOCK with the one line that makes it CLOCK removed, and the two
implementations are indistinguishable on the very first eviction: both
evict whatever the hand happens to be pointing at, because nothing has had a
chance to earn a second chance yet. The divergence only appears on the
*second* eviction, if something was accessed in between -- exactly the
pattern a real cache workload produces constantly (a hot key read on every
request, cold keys evicted around it) and exactly the pattern a shallow
"insert past capacity, assert something left" test never constructs. Ship
the round-robin version and your hottest keys get evicted exactly as often
as your coldest ones, and the cache buys you nothing over no cache at all.

Create `clockcache.go`:

```go
// Package clockcache implements the CLOCK page-replacement algorithm: a
// fixed-capacity cache that approximates LRU by sweeping one hand around a
// ring of frames instead of maintaining a recency-ordered list.
//
// It exists to get one detail right that a hand-rolled "clock" cache
// routinely gets wrong: eviction must give an in-use frame a second chance
// by clearing its reference bit and moving on, not evict whatever the hand
// is pointing at regardless of that bit. Skipping the bit check turns
// CLOCK into plain round-robin, which evicts hot entries as readily as cold
// ones -- exactly the behavior the reference bit exists to prevent. See the
// package tests for a side-by-side demonstration.
package clockcache

import (
	"errors"
	"fmt"
)

// ErrInvalidCapacity is returned by New when the requested capacity is not
// positive.
var ErrInvalidCapacity = errors.New("clockcache: capacity must be positive")

// frame is one slot in the fixed ring of cached entries.
type frame[K comparable, V any] struct {
	key      K
	value    V
	occupied bool
	ref      bool
}

// Cache is a fixed-capacity key/value cache using the CLOCK (second-chance)
// replacement algorithm: a single hand sweeps a ring of frames, clearing
// each occupied frame's reference bit on the way past and evicting the
// first frame it finds already clear. A Get or an update through Put sets
// the reference bit, giving that frame one more full sweep before it can be
// evicted -- the "second chance" that approximates LRU far more cheaply
// than maintaining a recency-ordered list.
//
// Cache is not safe for concurrent use; the caller must synchronize access,
// for example with the sync.Mutex wrapper pattern used elsewhere in this
// lesson.
type Cache[K comparable, V any] struct {
	frames []frame[K, V]
	index  map[K]int
	hand   int
	size   int
}

// New returns a Cache holding up to capacity entries. It returns
// ErrInvalidCapacity if capacity is not positive.
func New[K comparable, V any](capacity int) (*Cache[K, V], error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacity)
	}
	return &Cache[K, V]{
		frames: make([]frame[K, V], capacity),
		index:  make(map[K]int, capacity),
	}, nil
}

// Len reports how many entries are currently cached.
func (c *Cache[K, V]) Len() int { return c.size }

// Cap reports the maximum number of entries this Cache holds.
func (c *Cache[K, V]) Cap() int { return len(c.frames) }

// Get returns the value stored for key, and true if it was present. A
// successful Get sets that frame's reference bit, giving it a second chance
// against eviction on the next sweep.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	if idx, ok := c.index[key]; ok {
		c.frames[idx].ref = true
		return c.frames[idx].value, true
	}
	var zero V
	return zero, false
}

// Put inserts or updates the value stored for key. Updating an existing key
// also sets its reference bit, exactly like Get. Inserting a new key when
// the cache is at capacity runs one CLOCK sweep to choose a victim frame.
func (c *Cache[K, V]) Put(key K, value V) {
	if idx, ok := c.index[key]; ok {
		c.frames[idx].value = value
		c.frames[idx].ref = true
		return
	}

	if c.size < len(c.frames) {
		idx := c.size
		c.frames[idx] = frame[K, V]{key: key, value: value, occupied: true, ref: true}
		c.index[key] = idx
		c.size++
		return
	}

	idx := c.evictOne()
	delete(c.index, c.frames[idx].key)
	c.frames[idx] = frame[K, V]{key: key, value: value, occupied: true, ref: true}
	c.index[key] = idx
}

// evictOne runs the CLOCK sweep and returns the index of the frame it
// chose to evict, without touching that frame's contents. Every occupied
// frame the hand passes with its reference bit set gets that bit cleared
// and one more chance; the first occupied frame the hand finds with the
// bit already clear is the victim. Because every frame is occupied here
// (evictOne only runs once the cache is full) and a set bit can only be
// cleared, never re-set, during the sweep, at most two full laps around the
// ring are ever needed: the first lap clears every set bit, and the second
// is guaranteed to find one already clear.
func (c *Cache[K, V]) evictOne() int {
	for range 2 * len(c.frames) {
		f := &c.frames[c.hand]
		if !f.ref {
			victim := c.hand
			c.hand = (c.hand + 1) % len(c.frames)
			return victim
		}
		f.ref = false
		c.hand = (c.hand + 1) % len(c.frames)
	}
	// Unreachable given the invariant above: every occupied frame's bit is
	// cleared on the first lap, so the second lap always finds a victim.
	panic("clockcache: sweep failed to find a victim; invariant violated")
}
```

### Using it

Construct one `Cache[K, V]` per pool you want bounded -- a page cache keyed
by block number, a parsed-config cache keyed by file path, a compiled
regular expression cache keyed by pattern -- and call `Get` before doing the
expensive work, `Put` after. The type itself declares it is not safe for
concurrent use: CLOCK's hand and every frame's reference bit are ordinary,
unsynchronized state, exactly like the bare `Ring[T]` this lesson opened
with, and for the same reason -- most callers either own the cache from a
single goroutine or already have a natural place to add the mutex wrapper
pattern from Exercise 4 on top.

The one contract worth internalizing before you rely on this cache under
load is what "second chance" actually buys you: it is an *approximation* of
recency, not a guarantee that the single most recently used entry survives
every eviction. A key accessed once, long enough ago that the hand has
already swept past it and cleared its bit again, is evicted exactly like a
key that was never accessed at all. That is the trade CLOCK makes for O(1)
bookkeeping instead of a recency-ordered list, and it is the right trade for
most production caches.

`ExampleCache_Get` in the test file is the runnable demonstration of this
module: `go test` executes it and compares its stdout against the
`// Output:` comment, so the usage shown there cannot drift from the code.

### Tests

`TestNewRejectsNonPositiveCapacity` and `TestGetMissOnEmptyCache` cover the
boundary cases. `TestPutBelowCapacityNeverEvicts` and
`TestPutUpdatesExistingKeyWithoutEvicting` confirm the two paths that must
never trigger a sweep at all.

`TestSecondChanceProtectsRecentlyAccessedKey` is the heart of the module: a
five-step sequence -- fill to capacity, insert a fourth key (forcing the
first eviction while every reference bit is still set from insertion), `Get`
one of the two survivors, then insert a fifth key (forcing a second
eviction) -- captured from a real run of the package rather than derived by
hand, because CLOCK's exact sweep order is part of what the test is pinning.
The key that received the extra `Get` must survive the second eviction; the
other original survivor must not.

`TestRoundRobinEvictsHotKeyAnyway` is the antipattern contrast.
`roundRobinCache` is unexported and unreachable from the package API; it
runs the identical five-step sequence and shows the key that was just
accessed gets evicted anyway, because nothing in that version ever consults
a reference bit. If a future edit to `evictOne` ever drops the `if !f.ref`
check, `TestSecondChanceProtectsRecentlyAccessedKey` fails immediately.

Create `clockcache_test.go`:

```go
package clockcache

import (
	"fmt"
	"testing"
)

// roundRobinCache is the second-chance cache as it is usually written the
// first time: it evicts whatever frame the hand is pointing at, without
// ever consulting -- or even storing -- a reference bit. It is never
// exported and never reachable from the package API; it exists only so the
// tests can pin what it gets wrong. A key accessed a moment ago is exactly
// as likely to be evicted as one nobody has touched since it was inserted,
// because nothing in this version distinguishes them.
type roundRobinCache struct {
	keys     []string
	occupied []bool
	index    map[string]int
	hand     int
	size     int
}

func newRoundRobinCache(capacity int) *roundRobinCache {
	return &roundRobinCache{
		keys:     make([]string, capacity),
		occupied: make([]bool, capacity),
		index:    make(map[string]int, capacity),
	}
}

func (r *roundRobinCache) get(key string) bool {
	_, ok := r.index[key]
	return ok
}

func (r *roundRobinCache) put(key string) {
	if _, ok := r.index[key]; ok {
		return
	}
	if r.size < len(r.keys) {
		r.keys[r.size] = key
		r.occupied[r.size] = true
		r.index[key] = r.size
		r.size++
		return
	}
	victim := r.hand
	r.hand = (r.hand + 1) % len(r.keys)
	delete(r.index, r.keys[victim])
	r.keys[victim] = key
	r.index[key] = victim
}

func TestNewRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()

	for _, capacity := range []int{0, -1} {
		if _, err := New[string, int](capacity); err == nil {
			t.Errorf("New(%d): want error, got nil", capacity)
		}
	}
}

func TestGetMissOnEmptyCache(t *testing.T) {
	t.Parallel()

	c, err := New[string, int](3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := c.Get("missing"); ok {
		t.Fatal("Get on an empty cache: want miss, got hit")
	}
}

func TestPutBelowCapacityNeverEvicts(t *testing.T) {
	t.Parallel()

	c, err := New[string, int](3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Put("a", 1)
	c.Put("b", 2)
	if c.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", c.Len())
	}
	for k, want := range map[string]int{"a": 1, "b": 2} {
		got, ok := c.Get(k)
		if !ok || got != want {
			t.Fatalf("Get(%q) = %v, %v, want %v, true", k, got, ok, want)
		}
	}
}

func TestPutUpdatesExistingKeyWithoutEvicting(t *testing.T) {
	t.Parallel()

	c, err := New[string, int](2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("a", 99)
	if c.Len() != 2 {
		t.Fatalf("Len() = %d, want 2 (updating a must not evict b)", c.Len())
	}
	if got, ok := c.Get("a"); !ok || got != 99 {
		t.Fatalf("Get(a) = %v, %v, want 99, true", got, ok)
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("Get(b): want hit, got miss")
	}
}

// TestSecondChanceProtectsRecentlyAccessedKey is the whole point of the
// module. Filling the cache and then inserting past capacity twice, with a
// single Get on "b" in between, must evict "a" (never accessed after
// insertion) and then "c" (never accessed at all) while "b" survives
// because that Get set its reference bit and bought it one more sweep. This
// exact sequence was captured from a real run of the package and is not
// hand-derived: the CLOCK sweep order is part of what the test pins.
func TestSecondChanceProtectsRecentlyAccessedKey(t *testing.T) {
	t.Parallel()

	c, err := New[string, int](3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)
	c.Put("d", 4) // first eviction while all three reference bits are set
	c.Get("b")    // give b a second chance before the next eviction
	c.Put("e", 5) // second eviction: must skip b and take c instead

	wantPresent := map[string]bool{"a": false, "b": true, "c": false, "d": true, "e": true}
	for k, want := range wantPresent {
		_, got := c.Get(k)
		if got != want {
			t.Errorf("Get(%q) hit = %v, want %v", k, got, want)
		}
	}
	if c.Len() != c.Cap() {
		t.Fatalf("Len() = %d, want Cap() = %d", c.Len(), c.Cap())
	}
}

// TestRoundRobinEvictsHotKeyAnyway contrasts the naive helper against Cache
// using the identical sequence of operations. Without a reference bit, the
// naive cache evicts whatever the hand is pointing at regardless of how
// recently it was touched, so the key that was just accessed is evicted
// exactly when a real CLOCK cache would have spared it.
func TestRoundRobinEvictsHotKeyAnyway(t *testing.T) {
	t.Parallel()

	naive := newRoundRobinCache(3)
	naive.put("a")
	naive.put("b")
	naive.put("c")
	naive.put("d") // evicts a (hand starts at 0)
	naive.get("b") // no effect: roundRobinCache has no reference bit
	naive.put("e") // evicts b anyway: the hand simply continued to index 1

	if naive.get("b") {
		t.Fatal("roundRobinCache kept b after eviction; the naive/correct contrast no longer holds")
	}

	correct, err := New[string, int](3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	correct.Put("a", 1)
	correct.Put("b", 2)
	correct.Put("c", 3)
	correct.Put("d", 4)
	correct.Get("b")
	correct.Put("e", 5)

	if _, ok := correct.Get("b"); !ok {
		t.Fatal("Cache evicted b despite the intervening Get; the second-chance mechanism failed")
	}
}

// ExampleCache_Get is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleCache_Get() {
	c, err := New[string, int](3)
	if err != nil {
		panic(err)
	}
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)
	c.Put("d", 4) // evicts a
	c.Get("b")    // give b a second chance
	c.Put("e", 5) // evicts c, not b

	for _, k := range []string{"a", "b", "c", "d", "e"} {
		_, ok := c.Get(k)
		fmt.Println(k, ok)
	}

	// Output:
	// a false
	// b true
	// c false
	// d true
	// e true
}
```

## Review

`Cache[K, V]` is correct when a frame accessed since the hand last passed it
survives the next sweep, and only a frame the hand finds with its reference
bit already clear is evicted. `evictOne` gets that right by clearing bits as
it sweeps rather than acting on the first frame it reaches: the bit check --
`if !f.ref { evict } else { f.ref = false; advance }` -- is the entire
algorithm. The mistake this module exists to name is dropping that check and
keeping only the moving hand, which turns CLOCK into round-robin: it evicts
whatever slot is next regardless of how recently it was touched, and a hot
key is exactly as likely to be evicted as a cold one. Around that core, `New`
rejects a non-positive capacity with `ErrInvalidCapacity`, `Put` on an
existing key updates in place and refreshes its reference bit without
triggering a sweep, and the two-lap bound in `evictOne` guarantees the sweep
always terminates because a bit can only be cleared, never re-set, during
one pass. `Cache[K, V]` is explicitly not safe for concurrent use; wrap it
with a mutex, as Exercise 4 does for the plain ring, if multiple goroutines
need it. `ExampleCache_Get` is the executable documentation: `go test`
verifies its output. Run `go test -count=1 -race ./...`.

## Resources

- [PostgreSQL buffer manager: clock sweep](https://www.postgresql.org/docs/current/backend-flow.html) — the production algorithm this module implements a minimal version of.
- [CLOCK page replacement (OSTEP)](https://pages.cs.wisc.edu/~remzi/OSTEP/vm-beyondphys.pdf) — a from-first-principles explanation of the algorithm and why it approximates LRU.
- [Go maps](https://go.dev/blog/maps) — the index map used here for O(1) key-to-frame lookup alongside the ring.
- [Go Wiki: CodeReviewComments](https://go.dev/wiki/CodeReviewComments) — general guidance on documenting concurrency contracts, applied to `Cache`'s "not safe for concurrent use" doc comment.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-sharded-ring-buffers-hash-distribution.md](13-sharded-ring-buffers-hash-distribution.md) | Next: [15-idempotency-key-dedup-window.md](15-idempotency-key-dedup-window.md)
