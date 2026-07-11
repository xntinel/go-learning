# Exercise 9: Prune Expired Cache Entries Safely During Iteration

A TTL cache needs a periodic sweep that walks its entries and drops the expired ones.
The spec is precise about mutating a map mid-range, and the two directions are not
symmetric: deleting during a range is defined and safe, adding is not. This module
builds the sweep both by hand and with `maps.DeleteFunc`, and pins the rules with tests.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
sweep/                     independent module: example.com/sweep
  go.mod                   go 1.26
  sweep.go                 type Cache; Set, Get, Len, SweepLoop, SweepFunc, ExpiredKeys
  cmd/
    demo/
      main.go              inserts entries with mixed deadlines, sweeps, prints survivors
  sweep_test.go            exact-expiry sweep, empty/nil no-op, equivalence, Example
```

- Files: `sweep.go`, `cmd/demo/main.go`, `sweep_test.go`.
- Implement: a `Cache` with `Set(key, ttl)`, `Get`, `Len`, and two equivalent sweeps —
  `SweepLoop` (manual delete-in-range) and `SweepFunc` (`maps.DeleteFunc`).
- Test: a sweep removes exactly the expired keys; sweeping empty/nil is a no-op; a sweep
  that expires everything leaves `Len` 0; the two sweeps agree.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/sweep/cmd/demo
cd ~/go-exercises/sweep
go mod init example.com/sweep
```

### The spec's rule, stated exactly

From the Go spec on `for range` over a map: the iteration order is unspecified, and
entries may be deleted or added during iteration. The two cases differ:

- **Deleting** the entry at the current key — or *any* key — during the range is
  well-defined: an entry that has not yet been reached and is deleted before it is
  reached will not be produced. So a loop that ranges and calls `delete(m, k)` on the
  keys it decides are expired is correct, in-place, and needs no separate key slice.
- **Adding** an entry during the range is permitted but its effect is *unspecified*: the
  new entry may or may not be produced by that same range. You must never rely on it.

That asymmetry is the whole design rule for a sweeper. Because *delete-in-range* is
safe, the expiry sweep can walk the map once and delete expired keys as it goes — no
"collect keys into a slice, then delete" two-pass dance is needed for correctness (that
pattern is only needed when you must *add*). Because *add-in-range* is unspecified, the
insert path (`Set`) must not run concurrently against a sweep on the same map; in this
single-goroutine cache the sweep and the inserts simply never overlap.

`SweepLoop` shows the manual form; `SweepFunc` shows `maps.DeleteFunc(m, pred)`, which
is exactly that loop packaged — it ranges the map and deletes every entry for which the
predicate returns true. They are equivalent, and the test proves it by running both on
identical inputs and comparing survivors. `ExpiredKeys` returns the sorted list of keys
a sweep *would* remove, for deterministic reporting.

Create `sweep.go`:

```go
package sweep

import (
	"maps"
	"slices"
	"time"
)

// Cache is a single-goroutine TTL cache. Entries store an absolute expiry.
type Cache struct {
	items map[string]time.Time // key -> expiry instant
}

// New returns an empty Cache.
func New() *Cache {
	return &Cache{items: make(map[string]time.Time)}
}

// Set stores key with an expiry ttl from now.
func (c *Cache) Set(key string, ttl time.Duration) {
	c.items[key] = time.Now().Add(ttl)
}

// Get reports whether key is present and unexpired as of now.
func (c *Cache) Get(key string) bool {
	exp, ok := c.items[key]
	return ok && time.Now().Before(exp)
}

// Len reports how many entries are stored, expired or not.
func (c *Cache) Len() int { return len(c.items) }

// SweepLoop removes every entry expired as of now, deleting in place during the
// range. Delete-in-range is defined by the spec; no separate key slice is needed.
func (c *Cache) SweepLoop(now time.Time) int {
	removed := 0
	for k, exp := range c.items {
		if !now.Before(exp) { // expired: now >= exp
			delete(c.items, k)
			removed++
		}
	}
	return removed
}

// SweepFunc removes expired entries using maps.DeleteFunc — the packaged form of
// the delete-in-range loop in SweepLoop.
func (c *Cache) SweepFunc(now time.Time) {
	maps.DeleteFunc(c.items, func(_ string, exp time.Time) bool {
		return !now.Before(exp)
	})
}

// ExpiredKeys returns the sorted keys that a sweep at now would remove. It only
// reads the map, so it is safe to call for reporting before sweeping.
func (c *Cache) ExpiredKeys(now time.Time) []string {
	var expired []string
	for k, exp := range c.items {
		if !now.Before(exp) {
			expired = append(expired, k)
		}
	}
	slices.Sort(expired)
	return expired
}
```

### The runnable demo

The demo uses explicit deadlines relative to a fixed `now` so the output is
deterministic, then sweeps and prints the survivors in sorted order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"
	"time"

	"example.com/sweep"
)

func main() {
	c := sweep.New()
	c.Set("fresh-a", time.Hour)
	c.Set("fresh-b", time.Hour)
	c.Set("stale-a", -time.Minute) // already expired
	c.Set("stale-b", -time.Second) // already expired

	now := time.Now()
	fmt.Println("expired:", c.ExpiredKeys(now))
	removed := c.SweepLoop(now)
	fmt.Println("removed:", removed)

	var survivors []string
	c.SweepFunc(now) // second sweep is a no-op; used here only to show idempotence
	for _, k := range []string{"fresh-a", "fresh-b", "stale-a", "stale-b"} {
		if c.Get(k) {
			survivors = append(survivors, k)
		}
	}
	slices.Sort(survivors)
	fmt.Println("survivors:", survivors)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
expired: [stale-a stale-b]
removed: 2
survivors: [fresh-a fresh-b]
```

### Tests

The tests fix `now` and use relative deadlines so expiry is deterministic without any
real sleeping. They pin that a sweep removes exactly the expired set, that sweeping an
empty and a nil map is a safe no-op, that expiring everything leaves `Len` 0, and that
`SweepLoop` and `SweepFunc` agree on the survivors.

Create `sweep_test.go`:

```go
package sweep

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"
)

func seed(now time.Time) *Cache {
	c := New()
	c.items["keep-1"] = now.Add(time.Hour)
	c.items["keep-2"] = now.Add(time.Minute)
	c.items["gone-1"] = now.Add(-time.Hour)
	c.items["gone-2"] = now // now is not Before now -> expired
	return c
}

func TestSweepRemovesExactlyExpired(t *testing.T) {
	t.Parallel()

	now := time.Now()
	c := seed(now)

	if got, want := c.ExpiredKeys(now), []string{"gone-1", "gone-2"}; !slices.Equal(got, want) {
		t.Fatalf("ExpiredKeys = %v, want %v", got, want)
	}
	if removed := c.SweepLoop(now); removed != 2 {
		t.Fatalf("SweepLoop removed %d, want 2", removed)
	}
	if c.Len() != 2 {
		t.Fatalf("Len after sweep = %d, want 2", c.Len())
	}
	if !c.Get("keep-1") || !c.Get("keep-2") {
		t.Fatal("a fresh entry was swept")
	}
}

func TestSweepEmptyAndNilIsNoOp(t *testing.T) {
	t.Parallel()

	now := time.Now()

	empty := New()
	if removed := empty.SweepLoop(now); removed != 0 {
		t.Fatalf("SweepLoop on empty removed %d, want 0", removed)
	}
	empty.SweepFunc(now) // must not panic

	// A nil backing map: delete-in-range and maps.DeleteFunc are both no-ops.
	nilCache := &Cache{items: nil}
	if removed := nilCache.SweepLoop(now); removed != 0 {
		t.Fatalf("SweepLoop on nil removed %d, want 0", removed)
	}
	nilCache.SweepFunc(now) // must not panic
}

func TestSweepEverythingLeavesZero(t *testing.T) {
	t.Parallel()

	now := time.Now()
	c := New()
	c.items["a"] = now.Add(-time.Second)
	c.items["b"] = now.Add(-time.Second)

	c.SweepFunc(now)
	if c.Len() != 0 {
		t.Fatalf("Len = %d after expiring all, want 0", c.Len())
	}
}

func TestSweepLoopAndFuncAgree(t *testing.T) {
	t.Parallel()

	now := time.Now()
	c1 := seed(now)
	c2 := seed(now)

	c1.SweepLoop(now)
	c2.SweepFunc(now)

	if !maps.Equal(c1.items, c2.items) {
		t.Fatalf("SweepLoop and SweepFunc disagree:\n loop = %v\n func = %v", c1.items, c2.items)
	}
}

func ExampleCache_ExpiredKeys() {
	now := time.Now()
	c := New()
	c.items["live"] = now.Add(time.Hour)
	c.items["dead"] = now.Add(-time.Hour)
	fmt.Println(c.ExpiredKeys(now))
	// Output: [dead]
}
```

## Review

The sweep is correct because it exploits the one mutation the spec guarantees during a
range: deleting keys. `SweepLoop` deletes in place with no auxiliary slice, and
`maps.DeleteFunc` in `SweepFunc` is that exact loop with the predicate factored out —
`TestSweepLoopAndFuncAgree` proves they are interchangeable. The asymmetry is the lesson:
you may delete during a range, but you may not rely on adds appearing, which is why this
cache is single-goroutine and its `Set` never runs concurrently with a sweep. Nil and
empty sweeps are no-ops because ranging and deleting a nil map are both defined. Run
`go test -race`.

## Resources

- [Go Specification: For statements with range clause](https://go.dev/ref/spec#For_range) — the delete-allowed, add-unspecified rule.
- [`maps.DeleteFunc`](https://pkg.go.dev/maps#DeleteFunc) — the packaged delete-in-range sweep.
- [`time.Time.Before`](https://pkg.go.dev/time#Time.Before) — the expiry comparison.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-iteration-order-guardrail.md](10-iteration-order-guardrail.md)
