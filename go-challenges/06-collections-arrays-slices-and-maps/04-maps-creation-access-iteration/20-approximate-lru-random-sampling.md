# Exercise 20: Approximate LRU Eviction by Random Sampling

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A true LRU cache needs more than a map: it needs a doubly linked list
threaded through every entry so a hit can move that entry to the front in
O(1), and it needs to touch that list on every single read, not just every
write. That is real overhead -- extra memory per entry, and a data structure
that must be mutated (and therefore lock-protected) even on the read path,
which is usually the hot path. Redis's answer, under `maxmemory-policy
allkeys-lru`, is to skip the list entirely: on eviction, sample a small,
fixed number of random keys from the whole keyspace and evict whichever one
of those few has gone the longest without being touched. It is not exact --
a key outside the sample can be staler than the one evicted -- but it turns
an O(1)-per-read bookkeeping cost into nothing, at the price of an
occasionally suboptimal eviction.

This module builds that scheme as a package you can drop into a service,
over a plain `map[string]entry`: `Set` and `Get` stamp an access time, and
eviction samples `K` keys and drops the oldest of the sample. The one
subtlety worth teaching is where the "randomness" is allowed to come from --
sampling straight off a `range` of the map would make the eviction depend
on Go's own iteration-order randomization on top of the injected random
source, which defeats reproducibility even with a fixed seed. This module
sorts the key list before sampling from it, so the *only* source of
randomness left is the one you actually control.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
approxlru/               module example.com/approxlru
  go.mod                 go 1.24
  approxlru.go           Clock; Cache (bound, sampleSize, injected rng + clock); Get/Set/Len/Keys
  approxlru_test.go      eviction picks oldest of a full sample, bound never exceeded, seeded
                         reproducibility, -race, Example
```

- Files: `approxlru.go`, `approxlru_test.go`.
- Implement: `Cache` holding `map[string]entry` (value plus `lastAccess int64`), a `bound`, a `sampleSize`, an injected `*rand.Rand`, and an injected `Clock func() int64`; `Get(key) (string, bool)` (touches access time on a hit), `Set(key, value string)` (evicts before inserting if the key is new and the cache is at its bound), `Len() int`, and `Keys() []string` (sorted, for inspection); the private `evictLocked` collects `slices.Sorted(maps.Keys(c.entries))`, samples `sampleSize` distinct positions from that sorted list via the injected `rng`, and deletes whichever sampled key has the smallest `lastAccess`.
- Test: eviction with `sampleSize` equal to the full cache size deterministically picks the true oldest key; updating an existing key never triggers eviction; the cache's `Len()` never exceeds its bound across 200 churning inserts; a lookup on an empty cache reports absent; two independently constructed caches given the same seed and the same clock sequence end up holding the identical key set after an identical operation sequence; concurrent `Set`/`Get` calls are race-free; `Example` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/20-approximate-lru-random-sampling
cd go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/20-approximate-lru-random-sampling
go mod edit -go=1.24
```

### Why sampling has to sort first, and how the two injected dependencies make it testable

The eviction question is simple to state: "of these `sampleSize` random
keys, which has the oldest access time?" The subtlety is entirely in how
"these random keys" gets chosen. The tempting shortcut is to range the map
directly and take the first `sampleSize` entries the iterator happens to
produce -- but that reintroduces exactly the nondeterminism this whole
lesson keeps coming back to: Go re-randomizes map range order on every
single range, so "the first `sampleSize` entries of a range" is not a
sample controlled by your seeded `rand.Rand` at all, it is controlled by
the runtime's own iteration seed, which you cannot fix or reproduce.

The fix is to collect the map's keys into a slice and sort it --
`slices.Sorted(maps.Keys(c.entries))` -- *before* any sampling happens. A
sorted slice has one specific order, fixed and repeatable, so the only
place randomness is allowed to enter the picture is the deliberate call to
`c.rng.Intn(len(keys))` that picks which sorted positions make up the
sample. Given the same map contents and the same `rand.Rand` state, that
call always picks the same positions, which means the same keys, which
means the same eviction. This is the same discipline the rest of this
lesson has been building toward: never let map iteration order leak into a
decision that needs to be reproducible, and sorting first is almost always
the one-line fix.

The second injected dependency, `Clock`, exists for the same reason a
sliding-window rate limiter or a TTL cache injects time rather than calling
`time.Now()` directly: `lastAccess` has to be comparable and controllable in
a test without a real `time.Sleep`. A test can hand the cache a clock that
returns a plain incrementing counter, giving every `Set` and `Get` a
distinct, ordered "timestamp" with no wall-clock dependency and no
flakiness under CI load. Together, a seeded `*rand.Rand` plus an injected
`Clock` make an inherently probabilistic eviction policy fully
deterministic and unit-testable -- which is the whole point of injecting
both rather than reaching for `math/rand`'s global functions or `time.Now`
inside the cache's own logic.

Create `approxlru.go`:

```go
// Package approxlru implements a bounded cache with approximate LRU
// eviction: instead of a full LRU list, it samples a handful of random
// keys on eviction and evicts whichever of those few is oldest, the same
// trade-off Redis makes under maxmemory-policy allkeys-lru.
package approxlru

import (
	"maps"
	"math/rand"
	"slices"
	"sync"
)

// Clock returns the current logical time used to stamp entry access
// times. Production code passes a wrapper around time.Now().UnixNano;
// tests and demos inject a deterministic sequence so eviction decisions
// are reproducible.
type Clock func() int64

// entry is one cached value plus the logical time it was last read or
// written.
type entry struct {
	value      string
	lastAccess int64
}

// Cache is a bounded key-value store with sampled, approximate LRU
// eviction. A real LRU needs an O(1)-per-access doubly linked list
// alongside the map, extra memory per entry, and a lock held on every
// single read to keep that list correct. Approximate LRU skips the list
// entirely: eviction samples sampleSize random keys and drops whichever
// one of those few has the oldest access time. It is not exact -- a key
// outside the sample can be older than the one evicted -- but it bounds
// memory with O(1) overhead per entry and keeps reads cheap, which is why
// Redis defaults to it at scale instead of a true LRU list.
//
// Cache is safe for concurrent use by multiple goroutines.
type Cache struct {
	mu         sync.Mutex
	entries    map[string]entry
	bound      int
	sampleSize int
	rng        *rand.Rand
	clock      Clock
}

// New returns an empty Cache that holds at most bound entries, sampling
// sampleSize keys per eviction. rng and clock are injected: the same rng
// seed and the same clock sequence always evict the same keys in the same
// order, which is what makes eviction testable without relying on wall-clock
// time or the package's own unseeded randomness.
func New(bound, sampleSize int, rng *rand.Rand, clock Clock) *Cache {
	return &Cache{
		entries:    make(map[string]entry),
		bound:      bound,
		sampleSize: sampleSize,
		rng:        rng,
		clock:      clock,
	}
}

// Get returns the value for key and refreshes its access time on a hit --
// the same touch a real LRU list performs by moving the entry to the
// front.
func (c *Cache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return "", false
	}
	e.lastAccess = c.clock()
	c.entries[key] = e
	return e.value, true
}

// Set inserts or updates key. If key is new and the cache is already at
// its bound, Set evicts one entry first via sampled approximate LRU;
// updating an existing key never triggers eviction.
func (c *Cache) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.bound {
		c.evictLocked()
	}
	c.entries[key] = entry{value: value, lastAccess: c.clock()}
}

// Len returns the number of entries currently cached.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Keys returns the currently cached keys, sorted, for inspection in tests
// and demos.
func (c *Cache) Keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(maps.Keys(c.entries))
}

// evictLocked samples up to sampleSize keys and deletes whichever has the
// oldest lastAccess. It must run under c.mu.
//
// Keys are collected and sorted before sampling, not read straight off a
// map range: range order is randomized by design, so picking sample
// positions with the injected rng directly against range order would make
// the "same" seed pick a different sample on every run, even with
// identical cache contents. Sorting first gives the key list a fixed,
// repeatable order, so the injected rng -- not map iteration -- is the
// only source of randomness left, and that is what makes eviction
// reproducible.
func (c *Cache) evictLocked() {
	keys := slices.Sorted(maps.Keys(c.entries))
	n := c.sampleSize
	if n > len(keys) {
		n = len(keys)
	}

	var (
		oldestKey   string
		oldestFound bool
		oldestAt    int64
	)
	sampled := make(map[int]bool, n)
	for len(sampled) < n {
		idx := c.rng.Intn(len(keys))
		if sampled[idx] {
			continue
		}
		sampled[idx] = true

		key := keys[idx]
		access := c.entries[key].lastAccess
		if !oldestFound || access < oldestAt {
			oldestKey, oldestFound, oldestAt = key, true, access
		}
	}
	delete(c.entries, oldestKey)
}
```

### Using it

`New` requires the caller to supply both `rng` and `clock` explicitly --
there is no zero-value `Cache` that works, which is the point: a cache that
silently fell back to `time.Now()` and an unseeded global `math/rand` would
be just as functional in production and impossible to pin down in a test.
Production code passes `rand.New(rand.NewSource(time.Now().UnixNano()))` and
a thin wrapper around `time.Now().UnixNano`; tests pass a fixed seed and an
incrementing counter. `Cache` is safe for concurrent use by multiple
goroutines, guarded internally by a single `sync.Mutex` -- but the injected
`Clock` is not automatically safe just because `Cache` is: if a caller
shares one non-trivial clock closure across goroutines (as opposed to the
default `time.Now` wrapper, which already is), that closure needs its own
synchronization, exactly as `TestConcurrentAccess` demonstrates. `Keys()`
returns a fresh, sorted slice on every call; it never aliases the cache's
internal map, so a caller may hold onto it freely.

The module has no `main.go`, because a cache is a package you embed in a
service, not a tool you run standalone. Its executable demonstration is
`Example`: `go test` runs it and compares its standard output against the
`// Output:` comment, so the usage shown below cannot drift away from the
code. It inserts six sessions into a bound-4 cache with a sample size of 2
and a fixed seed, printing the key set after every `Set` so you can watch
eviction kick in once the cache fills.

### Tests

`TestEvictionPicksOldestOfSample` sets `sampleSize` equal to the cache's
`bound`, which makes the "sample" the entire cache and turns the policy
into exact LRU for that one test -- the cleanest way to prove the
oldest-of-sample logic itself is correct, independent of sampling luck.
`TestUpdatingExistingKeyNeverEvicts` guards an easy-to-miss edge: `Set` on
a key that already exists must never trigger eviction, even when the cache
is already full, because the total entry count is not changing.
`TestCacheNeverExceedsBound` churns 200 inserts across a rotating pool of
20 keys through a bound-5 cache and asserts `Len()` never exceeds 5 at any
point, not just at the end. `TestGetMissingKey` is the trivial absent-key
case. `TestSeededRunIsReproducible` is the test that justifies the whole
injected-dependency design: it runs the identical 30-insert sequence
through two independently constructed caches built with the same seed and
the same clock, and asserts they end up holding the exact same key set --
a version of `evictLocked` that sampled straight off map range order would
fail this test intermittently. `TestConcurrentAccess` runs 16 goroutines
performing interleaved `Set`/`Get` calls under `-race`, with the clock
itself wrapped in its own mutex since `Clock` is an injected closure with
no concurrency guarantee of its own. `Example` closes the loop as the
runnable demonstration.

Create `approxlru_test.go`:

```go
package approxlru

import (
	"fmt"
	"math/rand"
	"slices"
	"sync"
	"testing"
)

// fakeClock is a deterministic, injectable Clock: each call returns a
// strictly increasing logical time, with no dependency on time.Now.
type fakeClock struct {
	t int64
}

func (f *fakeClock) now() int64 {
	f.t++
	return f.t
}

func TestEvictionPicksOldestOfSample(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{}
	// sampleSize equals bound, so the "sample" is the whole cache and
	// eviction must pick the true oldest key deterministically.
	c := New(3, 3, rand.New(rand.NewSource(1)), clk.now)

	c.Set("a", "va") // oldest
	c.Set("b", "vb")
	c.Set("c", "vc")
	c.Set("d", "vd") // cache is full: must evict "a"

	if _, ok := c.Get("a"); ok {
		t.Fatal("oldest key \"a\" should have been evicted")
	}
	for _, k := range []string{"b", "c", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("key %q should still be present after eviction", k)
		}
	}
	if got := c.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}
}

func TestUpdatingExistingKeyNeverEvicts(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{}
	c := New(2, 2, rand.New(rand.NewSource(2)), clk.now)

	c.Set("a", "v1")
	c.Set("b", "v2")
	c.Set("a", "v1-updated") // updates a, cache already full but a exists

	if got := c.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2 (update must not evict)", got)
	}
	v, ok := c.Get("a")
	if !ok || v != "v1-updated" {
		t.Fatalf("Get(a) = %q, %v, want v1-updated, true", v, ok)
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("Get(b) should still be present")
	}
}

func TestCacheNeverExceedsBound(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{}
	c := New(5, 2, rand.New(rand.NewSource(7)), clk.now)

	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("key-%d", i%20)
		c.Set(key, "v")
		if got := c.Len(); got > 5 {
			t.Fatalf("iteration %d: Len() = %d, want at most 5", i, got)
		}
	}
}

func TestGetMissingKey(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{}
	c := New(2, 2, rand.New(rand.NewSource(3)), clk.now)
	if _, ok := c.Get("ghost"); ok {
		t.Fatal("Get on an empty cache should report absent")
	}
}

// TestSeededRunIsReproducible runs the identical sequence of operations on
// two fresh caches built with the same seed and the same clock sequence,
// and asserts they end up holding the exact same set of keys. This is the
// contract that makes sampled eviction testable at all: nondeterministic
// eviction (e.g. sampling straight off map range order) would make this
// test flaky.
func TestSeededRunIsReproducible(t *testing.T) {
	t.Parallel()

	run := func() []string {
		clk := &fakeClock{}
		c := New(4, 2, rand.New(rand.NewSource(99)), clk.now)
		for i := 0; i < 30; i++ {
			key := fmt.Sprintf("key-%d", i%10)
			c.Set(key, "v")
		}
		return c.Keys()
	}

	first := run()
	for i := 0; i < 5; i++ {
		got := run()
		if !slices.Equal(got, first) {
			t.Fatalf("run %d: Keys() = %v, want %v (same as run 0)", i, got, first)
		}
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{}
	var mu sync.Mutex // guards clk, which is not itself safe for concurrent use
	c := New(8, 3, rand.New(rand.NewSource(5)), func() int64 {
		mu.Lock()
		defer mu.Unlock()
		return clk.now()
	})

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				key := fmt.Sprintf("key-%d-%d", g, i%5)
				c.Set(key, "v")
				c.Get(key)
				if got := c.Len(); got > 8 {
					t.Errorf("Len() = %d, want at most 8", got)
				}
			}
		}(g)
	}
	wg.Wait()
}

// Example inserts six sessions into a bound-4 cache with a sample size of
// 2 and a fixed seed, printing the key set after every Set so you can
// watch eviction kick in once the cache fills.
func Example() {
	clk := &fakeClock{}
	c := New(4, 2, rand.New(rand.NewSource(42)), clk.now)

	for _, key := range []string{"session-a", "session-b", "session-c", "session-d", "session-e", "session-f"} {
		c.Set(key, "payload")
		fmt.Printf("after Set(%s): keys=%v len=%d\n", key, c.Keys(), c.Len())
	}

	fmt.Println("---")
	fmt.Println("final keys:", c.Keys())

	// Output:
	// after Set(session-a): keys=[session-a] len=1
	// after Set(session-b): keys=[session-a session-b] len=2
	// after Set(session-c): keys=[session-a session-b session-c] len=3
	// after Set(session-d): keys=[session-a session-b session-c session-d] len=4
	// after Set(session-e): keys=[session-a session-c session-d session-e] len=4
	// after Set(session-f): keys=[session-c session-d session-e session-f] len=4
	// ---
	// final keys: [session-c session-d session-e session-f]
}
```

## Review

The eviction policy is correct exactly when it evicts the oldest key of
whichever sample it drew -- `TestEvictionPicksOldestOfSample` pins that down
unambiguously by setting `sampleSize` to the full cache size, removing
sampling luck from the equation entirely. `TestCacheNeverExceedsBound` is
the invariant that actually matters in production: the cache staying within
its memory bound across churn, regardless of which specific key gets
evicted on any given call. The design's central lesson is
`TestSeededRunIsReproducible`: two identically-seeded, identically-clocked
caches given the identical operation sequence must land on the identical
key set, and that only holds because `evictLocked` sorts the key list
before sampling from it -- sampling straight off `range c.entries` would
make this test flaky even with a fixed `rand.Rand` seed, since the seed
alone does not control map iteration order. `Example` is the executable
documentation: `go test` verifies its output, watching the cache fill and
then start evicting once it hits its bound. Run
`go test -count=1 -race ./...`, including `TestConcurrentAccess`, before
trusting the cache under real concurrent load.

## Resources

- [math/rand.Rand](https://pkg.go.dev/math/rand#Rand) — the injected, seedable random source behind reproducible sampling.
- [maps.Keys](https://pkg.go.dev/maps#Keys) and [slices.Sorted](https://pkg.go.dev/slices#Sorted) — pin the key list into a fixed order before `evictLocked` samples from it.
- [Redis: eviction policies](https://redis.io/docs/latest/develop/reference/eviction/) — the production system whose `allkeys-lru` approximate-sampling design this module reimplements over a plain Go map.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guards both the cache's map and, in the concurrency test, the injected clock closure itself.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-dependency-dag-cycle-detection.md](19-dependency-dag-cycle-detection.md) | Next: [../05-nil-slices-vs-empty-slices/00-concepts.md](../05-nil-slices-vs-empty-slices/00-concepts.md)
