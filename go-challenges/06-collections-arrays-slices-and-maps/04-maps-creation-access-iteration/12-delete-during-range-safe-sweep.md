# Exercise 12: Deleting During Range: the one map mutation the spec guarantees

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A session cache, the kind that sits in front of a slower session store or
just holds short-lived tokens in memory, needs a periodic reaper: walk every
entry, drop the ones whose TTL has expired. The natural way to write that is
a single `for range` loop that deletes as it goes — and a lot of engineers
hesitate to write exactly that, because "don't mutate a collection while
iterating it" is such a deeply ingrained rule from other languages that they
reach for a more defensive two-pass version instead, collecting expired keys
first and deleting them afterward. Go's map is the one place that instinct is
unnecessary: the specification explicitly carves out delete-during-range as
safe and well-defined. This exercise builds that reaper as a small TTL cache,
proves the direct sweep agrees with the cautious two-pass version at every
boundary, and states exactly which map mutation the spec does *not* extend
the same guarantee to.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
ttlsweep/                     module example.com/ttlsweep
  go.mod                      go 1.24
  sweep.go                    Cache; New, Set, Get, Sweep, Len; two sentinel errors
  sweep_test.go                 boundary sweep table, Get-without-Sweep staleness,
                               collect-then-delete contrast, concurrency, ExampleCache
```

- Files: `sweep.go`, `sweep_test.go`.
- Implement: `New(now func() int64) (*Cache, error)` rejecting a nil clock with `ErrNilClock`; `(*Cache).Set(key, value string, ttl int64) error` rejecting a non-positive `ttl` with `ErrNonPositiveTTL`; `(*Cache).Get(key string) (string, bool)` treating an expired-but-unswept entry as absent; `(*Cache).Sweep() int` removing every expired entry in one delete-during-range pass and returning the count removed; `(*Cache).Len() int`.
- Test: a sweep table across nothing-expired, some-expired, everything-expired, and the boundary where `now` lands exactly on an expiry; `Get` reporting an expired entry absent before `Sweep` has run; the collect-then-delete contrast agreeing with `Sweep` at every boundary; an empty-cache sweep; concurrent `Set`/`Get`/`Sweep` under `-race`; and `ExampleCache` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/12-delete-during-range-safe-sweep
cd go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/12-delete-during-range-safe-sweep
go mod edit -go=1.24
```

### What the spec actually guarantees about mutating a map mid-range

The Go specification is precise about this, and the precision matters more
than the folk wisdom. Quoting the relevant guarantee: for a `for range`
statement over a map, "the iteration order over maps is not specified... If a
map entry is created during iteration, that entry may be produced during the
iteration or may be skipped... If an entry is deleted during iteration, it
will not be produced." Two different mutations, two different guarantees.
**Deletion** is fully deterministic: whether you delete the entry the loop is
currently on, or one it hasn't reached yet, the net effect is well-defined —
that entry simply will not be yielded (again). **Insertion** during a range
is legal Go (it will not panic or corrupt the table) but the *outcome* is
explicitly unspecified: the new entry might show up later in the same range,
or it might not, and the language makes no promise either way. That
asymmetry is the one thing to memorize: delete-during-range is safe and
deterministic, insert-during-range is safe but nondeterministic.

`Cache.Sweep` leans directly on the delete guarantee: it deletes `key` the
moment it decides `key` is expired, in the same iteration that produced it,
and the spec promises this cannot skip a sibling entry or revisit a deleted
one. The more cautious pattern many engineers default to out of habit —
collect expired keys first, mutate after the range has fully finished — is
not wrong, just unnecessary here: it costs an extra slice allocation to buy a
guarantee the language already gives you for free, so it never appears in
`Cache`'s API at all; the test file isolates it purely as a contrast.

`Sweep` also sidesteps the insert side of the asymmetry structurally rather
than by discipline. It holds `Cache`'s mutex for its entire pass, and so does
`Set` — so no `Set` call from another goroutine can ever land in the middle
of a `Sweep` range. The unspecified insert-during-range outcome the spec
warns about simply has no way to occur against this map: the lock that makes
`Cache` safe for concurrent use is the same lock that keeps every range over
`c.entries` from ever overlapping a write to it.

Create `sweep.go`:

```go
// Package ttlsweep implements a TTL cache with a periodic reaper, the same
// shape as a session cache sitting in front of a slower store: entries carry
// an absolute expiry, Get treats an expired-but-not-yet-reaped entry as
// absent, and Sweep drops everything that has aged out in one pass over the
// map.
package ttlsweep

import (
	"errors"
	"fmt"
	"sync"
)

// Sentinel errors returned by New and Set. Callers should test for them
// with errors.Is.
var (
	// ErrNilClock means New was called without a clock function.
	ErrNilClock = errors.New("ttlsweep: clock function must not be nil")
	// ErrNonPositiveTTL means Set was called with a ttl of zero or less.
	ErrNonPositiveTTL = errors.New("ttlsweep: ttl must be positive")
)

// entry is one cached value and its absolute expiry, in the same time unit
// the Cache's clock reports (typically Unix milliseconds).
type entry struct {
	value     string
	expiresAt int64
}

// Cache is a TTL-bounded string cache with an explicit reaper. Time never
// comes from the wall clock inside Cache's logic: every expiry decision is
// made against the value returned by the now function supplied to New, so
// tests can drive the cache through time deterministically.
//
// Cache is safe for concurrent use by multiple goroutines. Because Sweep and
// Set both hold the same mutex for their entire duration, no Set can ever
// interleave with an in-progress Sweep -- the insert-during-range case the
// Go spec leaves unspecified for a bare map range never arises here, not
// because it was handled, but because the lock rules it out structurally.
type Cache struct {
	mu      sync.Mutex
	entries map[string]entry
	now     func() int64
}

// New returns an empty Cache whose expiry decisions are driven by now. now
// must report the current time in the same unit ttl values passed to Set
// are expressed in (typically Unix milliseconds). New returns ErrNilClock
// if now is nil.
func New(now func() int64) (*Cache, error) {
	if now == nil {
		return nil, ErrNilClock
	}
	return &Cache{entries: make(map[string]entry), now: now}, nil
}

// Set stores value under key with the given ttl, measured from the clock's
// current reading. It returns ErrNonPositiveTTL if ttl is not positive.
func (c *Cache) Set(key, value string, ttl int64) error {
	if ttl <= 0 {
		return fmt.Errorf("%w: got %d", ErrNonPositiveTTL, ttl)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = entry{value: value, expiresAt: c.now() + ttl}
	return nil
}

// Get returns key's value and true if key is present and has not expired.
// An entry whose expiry has passed but has not yet been reaped by Sweep is
// reported as absent: Get never returns a stale value, regardless of
// whether Sweep has run recently.
func (c *Cache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || e.expiresAt <= c.now() {
		return "", false
	}
	return e.value, true
}

// Sweep removes every entry whose expiry is at or before the clock's
// current reading, deleting each expired key while still ranging over the
// backing map. The Go specification guarantees this is safe and
// well-defined: deleting the entry for the current key, or any key not yet
// produced by the iterator, simply means that key will not be produced
// later in this range. It returns the number of entries removed.
func (c *Cache) Sweep() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	removed := 0
	for key, e := range c.entries {
		if e.expiresAt <= now {
			delete(c.entries, key)
			removed++
		}
	}
	return removed
}

// Len reports the number of entries currently stored, including any that
// have expired but have not yet been reaped by Sweep.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
```

### Using it

Construct one `Cache` per process with a clock function — `time.Now().UnixMilli`
in production, a fixed or advancing closure in tests — and call `Set` on
writes, `Get` on reads, and `Sweep` from a background ticker to reclaim
expired entries. `Get` and `Sweep` agree on what "expired" means (the same
`<=` boundary against the same clock), so a caller never observes a value
`Get` has already started rejecting but `Sweep` has not yet removed; the only
difference is whether the backing map entry is still physically present,
which `Len` exposes. `Cache` is safe to share across every goroutine that
reads or writes it, including the reaper goroutine calling `Sweep`.

The module has no `main.go`, because a TTL cache is a library, not a tool.
Its executable demonstration is `ExampleCache`: `go test` runs it and
compares its standard output against the `// Output:` comment, so the usage
shown below cannot drift away from the code.

### Tests

`TestSweepRemovesExpiredEntries` is the boundary table: nothing expired, some
expired, everything expired, and `now` landing exactly on an entry's expiry.
`TestGetReflectsExpiryWithoutSweep` pins the staleness contract: `Get`
already reports an expired key absent before any `Sweep` has run, while
`Len` still counts it, proving the two methods use the same boundary
independently rather than `Get` secretly depending on `Sweep` having been
called first.

`TestBothSweepStrategiesAgree` is the load-bearing test for the spec
guarantee: `sweepCollectThenDelete`, unexported and reachable only from this
test file, redoes the reap with the two-pass collect-then-delete pattern
against an identical seed, and the test asserts it lands on the exact same
result as `Cache.Sweep`'s direct delete-during-range loop at every boundary
`now` value. If a future Go release, or a misreading of the spec, ever made
delete-during-range unsafe, this is the test that would start flaking first.
`TestSweepOnEmptyCacheIsNoop` covers the base case, and
`TestCacheConcurrentAccess` drives `Set`, `Get`, and `Sweep` from many
goroutines at once under `-race`, holding `Cache` to the concurrency contract
its doc comment promises.

Create `sweep_test.go`:

```go
package ttlsweep

import (
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"
)

// clockAt returns a clock function pinned to a fixed instant, for tests that
// need Set to compute a deterministic expiresAt.
func clockAt(t int64) func() int64 {
	return func() int64 { return t }
}

// seed returns a fresh entries map, independent per call, so each test (and
// each sweep strategy) starts from an identical unmutated input.
func seed() map[string]entry {
	return map[string]entry{
		"sess-1": {value: "a", expiresAt: 1000},
		"sess-2": {value: "b", expiresAt: 2000},
		"sess-3": {value: "c", expiresAt: 3000},
		"sess-4": {value: "d", expiresAt: 4000},
		"sess-5": {value: "e", expiresAt: 5000},
		"sess-6": {value: "f", expiresAt: 6000},
	}
}

// sweepCollectThenDelete is the more cautious pattern many engineers reach
// for out of habit around mutating a collection mid-iteration: collect the
// expired keys first, delete them in a second pass afterward. It produces
// the same result as Cache.Sweep's delete-during-range loop, at the cost of
// an extra slice allocation, and it is never exported: Cache only ever does
// the direct, single-pass version, because the Go spec already guarantees
// that one is safe.
func sweepCollectThenDelete(entries map[string]entry, now int64) int {
	var expired []string
	for key, e := range entries {
		if e.expiresAt <= now {
			expired = append(expired, key)
		}
	}
	for _, key := range expired {
		delete(entries, key)
	}
	return len(expired)
}

func TestNewRejectsNilClock(t *testing.T) {
	t.Parallel()

	if _, err := New(nil); !errors.Is(err, ErrNilClock) {
		t.Fatalf("New(nil) err = %v, want ErrNilClock", err)
	}
}

func TestSetRejectsNonPositiveTTL(t *testing.T) {
	t.Parallel()

	c, err := New(clockAt(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, ttl := range []int64{0, -1} {
		if err := c.Set("k", "v", ttl); !errors.Is(err, ErrNonPositiveTTL) {
			t.Fatalf("Set(ttl=%d) err = %v, want ErrNonPositiveTTL", ttl, err)
		}
	}
}

func TestGetReflectsExpiryWithoutSweep(t *testing.T) {
	t.Parallel()

	now := int64(1000)
	c, err := New(func() int64 { return now })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Set("k", "v", 500); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if got, ok := c.Get("k"); !ok || got != "v" {
		t.Fatalf("Get before expiry = (%q, %v), want (v, true)", got, ok)
	}

	now = 1500 // exactly at expiresAt (1000+500): expired, boundary inclusive
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get at the expiry boundary should report absent")
	}
	// The entry is still physically present until Sweep runs.
	if c.Len() != 1 {
		t.Fatalf("Len() = %d, want 1 (expired but not yet reaped)", c.Len())
	}
}

func TestSweepRemovesExpiredEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		now  int64
		want map[string]entry
	}{
		{"nothing expired", 500, seed()},
		{"some expired", 3500, map[string]entry{
			"sess-4": {value: "d", expiresAt: 4000},
			"sess-5": {value: "e", expiresAt: 5000},
			"sess-6": {value: "f", expiresAt: 6000},
		}},
		{"everything expired", 9999, map[string]entry{}},
		{"boundary exactly on an expiry", 4000, map[string]entry{
			"sess-5": {value: "e", expiresAt: 5000},
			"sess-6": {value: "f", expiresAt: 6000},
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := &Cache{entries: seed(), now: clockAt(tc.now)}
			c.Sweep()

			if !maps.Equal(c.entries, tc.want) {
				t.Fatalf("after Sweep at now=%d = %v, want %v", tc.now, c.entries, tc.want)
			}
		})
	}
}

// TestBothSweepStrategiesAgree proves the delete-during-range guarantee the
// package relies on: an identical seed, swept once by Cache.Sweep's direct
// delete-during-range loop and once by the unexported collect-then-delete
// contrast, must land on the same result at every now value tested.
func TestBothSweepStrategiesAgree(t *testing.T) {
	t.Parallel()

	nows := []int64{500, 3500, 9999, 4000}
	for _, now := range nows {
		a := seed()
		b := seed()

		c := &Cache{entries: a, now: clockAt(now)}
		c.Sweep()
		sweepCollectThenDelete(b, now)

		if !maps.Equal(a, b) {
			t.Fatalf("now=%d: delete-during-range=%v, collect-then-delete=%v", now, a, b)
		}
	}
}

func TestSweepOnEmptyCacheIsNoop(t *testing.T) {
	t.Parallel()

	c, err := New(clockAt(1000))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if removed := c.Sweep(); removed != 0 {
		t.Fatalf("Sweep on empty cache removed = %d, want 0", removed)
	}
}

// TestCacheConcurrentAccess drives Set, Get, and Sweep from many goroutines
// at once, under -race: Cache's doc comment promises safety for concurrent
// use, and this is what holds it to that.
func TestCacheConcurrentAccess(t *testing.T) {
	t.Parallel()

	now := int64(0)
	c, err := New(func() int64 { return now })
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = c.Set("k", "v", 1000)
			c.Get("k")
			c.Sweep()
		}(i)
	}
	wg.Wait()
}

// ExampleCache is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleCache() {
	now := int64(0)
	c, err := New(func() int64 { return now })
	if err != nil {
		panic(err)
	}

	_ = c.Set("sess-1", "alice", 1000) // expires at 1000
	_ = c.Set("sess-2", "bob", 5000)   // expires at 5000

	now = 2000 // sess-1 has expired, sess-2 has not
	_, ok1 := c.Get("sess-1")
	_, ok2 := c.Get("sess-2")
	fmt.Println("sess-1 present:", ok1)
	fmt.Println("sess-2 present:", ok2)

	removed := c.Sweep()
	fmt.Println("swept:", removed)
	fmt.Println("remaining:", c.Len())

	// Output:
	// sess-1 present: false
	// sess-2 present: true
	// swept: 1
	// remaining: 1
}
```

## Review

`Sweep` is correct exactly when every entry with `expiresAt <= now` is gone
and every entry with `expiresAt > now` survives untouched, and it gets there
in one pass because the spec's delete-during-range guarantee makes that
safe. `TestBothSweepStrategiesAgree` is the test that actually proves the
guarantee, not just asserts it in prose — the two-pass
`sweepCollectThenDelete` never appears in `Cache`'s API because it buys
nothing beyond what the direct loop already gets for free. The one mutation
this exercise deliberately never attempts against a live range is *inserting*
during it: the spec leaves that outcome unspecified, and `Cache` avoids the
question entirely by holding its mutex across the whole of `Sweep`, so no
`Set` can land mid-range in the first place. Run
`go test -count=1 -race ./...`.

## Resources

- [Go Specification: For statements — range clause](https://go.dev/ref/spec#For_range) — the exact wording on delete- and insert-during-range.
- [Go Specification: Delete built-in function](https://go.dev/ref/spec#Deletion_of_map_elements) — `delete` is a no-op on an absent key and has no return value.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the lock that keeps `Sweep` and `Set` from ever overlapping.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-comma-ok-vs-zero-value-flags.md](11-comma-ok-vs-zero-value-flags.md) | Next: [13-prealloc-map-index-build.md](13-prealloc-map-index-build.md)
