# Exercise 16: Typed nil Inside any: A Generic Cache's Get Boundary

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Any small in-process cache that predates generics, or that deliberately
stays untyped so one implementation serves lookups of several unrelated
result types, ends up with the same shape: `Get(key string) (any, bool)`. A
memoized database lookup, a plugin registry keyed by name, a resolved-config
store shared across handlers -- all of them box heterogeneous values behind
`any` and hand the caller back the concrete type it already knows to expect.
That boundary is where nil-vs-empty's least intuitive cousin lives, and it
survives code review because it looks exactly like an ordinary nil check.

An `any` value is a two-word pair: a pointer to a type descriptor and a
pointer to the data. The comparison `v == nil` is true only when *both* words
are nil -- no type recorded, no data recorded. Store a `[]string(nil)` into
that cache -- a legitimate value, meaning "I looked this key up and there
were no results, so don't look it up again" -- and `Get` hands back an `any`
whose type word says "`[]string`" and whose data word happens to be nil. That
`any` is not the nil `any`. `v == nil` on it is false, every time, even
though `v.([]string) == nil` is true underneath. Code that uses `v == nil` to
decide "was anything cached here" gets the wrong answer for exactly the value
that was cached to mean nothing.

This module builds `anycache`, a small bounded cache with that `Get`
boundary, and proves the property directly: a typed nil slice set through
`Set` comes back through `Get` with `ok == true` and `v == nil` false, and
only a type assertion recovers the fact that the slice underneath is nil. The
naive `v == nil` miss check never appears in the cache's own API -- it is not
a mode, not a flag, nothing the package offers as an alternative. It exists
only in the test file, as the thing the tests prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
anycache/                module example.com/anycache
  go.mod                 go 1.24
  cache.go                Cache; NewCache, Get, Set, Len
  cache_test.go           Get/Set table, eviction, capacity edge cases, typed-nil-survives-Get,
                          the isMissNaive contrast, concurrency, ExampleCache_typedNil
```

- Files: `cache.go`, `cache_test.go`.
- Implement: `NewCache(maxEntries int) (*Cache, error)` rejecting a non-positive capacity with `ErrInvalidCapacity`; `(*Cache).Get(key string) (any, bool)` and `(*Cache).Set(key string, value any)`, `Set` evicting the oldest entry by insertion order when a new key would exceed capacity; `(*Cache).Len() int`.
- Test: Get/Set across several concrete types including a nil pointer; overwrite not refreshing eviction position; eviction at capacity; `NewCache` rejecting non-positive capacity; a typed nil `[]string` surviving `Get` with `ok=true` and `v == nil` false; an `isMissNaive` contrast proving a bare `== nil` check is blind to that case while correctly reporting a genuine miss; concurrent `Get`/`Set` from many goroutines; and `ExampleCache_typedNil` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/16-typed-nil-any-kv-store
cd go-solutions/06-collections-arrays-slices-and-maps/05-nil-slices-vs-empty-slices/16-typed-nil-any-kv-store
go mod edit -go=1.24
```

### Why a typed nil boxed into any is never == nil

An interface value -- and `any` is just `interface{}`, an interface with no
methods -- is represented internally as a pair: a pointer to the concrete
type's descriptor, and a pointer (or inline word, for small values) to the
data. `nil` for an interface means both halves are unset: no type, no value.
Assigning a concrete nil into an interface variable sets only the second half
to nil; the first half still records the concrete type, because the compiler
knows exactly what type is being assigned. That is the whole mechanism, and
it applies identically whether the concrete type is a nil pointer, a nil
map, or a nil slice:

```go
var tags []string          // tags == nil is true; ordinary nil slice
var v any = tags           // v == nil is false: v's type word is now []string
v == nil                   // false
v.([]string) == nil        // true -- the value under the type assertion is nil
```

A cache's `Get` method does exactly this assignment on the way out: it reads
a `map[string]any` entry and returns it. If that entry was set from a nil
slice, pointer, or map, the returned `any` carries that type and is not the
nil `any`, no matter how empty the underlying value is. This is why `ok`, the
second return value of a comma-ok map read, is the only signal this package
exposes for "was there an entry" -- and why the module's Get is spelled
`(any, bool)` rather than a single `any` a caller might be tempted to check
against nil.

Create `cache.go`:

```go
// Package anycache implements a small, bounded, in-process cache that stores
// values of any concrete type behind a single any-typed slot -- the shape
// underneath a memoized lookup or a plugin registry.
//
// The package exists to make one detail impossible to get wrong at the call
// site: a value retrieved through Get's any return can be a typed nil (for
// example a nil []string that was deliberately cached to mean "looked this
// up, found nothing"), and a typed nil boxed into any never compares equal to
// the untyped nil. Get's ok result is the only reliable miss signal; see the
// package tests for what a bare == nil check on the value gets wrong.
package anycache

import (
	"errors"
	"fmt"
	"sync"
)

// ErrInvalidCapacity is returned by NewCache when the requested maximum entry
// count is not positive.
var ErrInvalidCapacity = errors.New("anycache: max entries must be positive")

// Cache is a bounded key-value store holding values of any concrete type. It
// evicts the oldest entry, by insertion order, when Set would exceed its
// configured capacity.
//
// A Cache is safe for concurrent use by multiple goroutines.
type Cache struct {
	mu         sync.Mutex
	entries    map[string]any
	order      []string
	maxEntries int
}

// NewCache returns an empty Cache that holds at most maxEntries entries. It
// returns ErrInvalidCapacity if maxEntries is not positive.
func NewCache(maxEntries int) (*Cache, error) {
	if maxEntries <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, maxEntries)
	}
	return &Cache{
		entries:    make(map[string]any, maxEntries),
		order:      make([]string, 0, maxEntries),
		maxEntries: maxEntries,
	}, nil
}

// Get returns the value stored under key and whether it was found. ok is the
// only reliable signal of a miss: the returned value can be a typed nil (a
// nil slice, map, or pointer stored deliberately, e.g. to memoize "looked
// this up, found nothing"), and a typed nil boxed into any is never == nil.
// Callers who know the concrete type should check ok, then type-assert;
// callers must never branch on value == nil to detect a miss.
func (c *Cache) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[key]
	return v, ok
}

// Set stores value under key, overwriting any existing entry for that key
// without changing its position in eviction order. If key is new and the
// cache is already at capacity, Set evicts the oldest entry, by insertion
// order, before inserting.
func (c *Cache) Set(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.entries[key]; exists {
		c.entries[key] = value
		return
	}
	if len(c.entries) >= c.maxEntries {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
	c.entries[key] = value
	c.order = append(c.order, key)
}

// Len reports the number of entries currently stored.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
```

### Using it

Construct one `Cache` with `NewCache`, sized to the working set the process
should keep in memory, and share it across every goroutine that needs it --
`Get` and `Set` both hold an internal mutex, so no external synchronization is
required. The contract a caller must respect is the one this module exists
to teach: `ok` answers "was there an entry," and once `ok` is true, recovering
whether the concrete value itself is nil requires a type assertion the caller
is already positioned to make, because it is the caller who knows what
concrete type it stored under that key.

The module has no `main.go`; a generic cache is a package another service
imports, not a program run on its own. Its executable demonstration is
`ExampleCache_typedNil`: `go test` runs it and compares its standard output
against the `// Output:` comment, so the usage shown here cannot drift away
from the code.

```go
func ExampleCache_typedNil() {
	c, err := NewCache(4)
	if err != nil {
		panic(err)
	}

	c.Set("count", 3)
	c.Set("no-results", []string(nil))

	for _, key := range []string{"count", "no-results", "never-set"} {
		v, ok := c.Get(key)
		fmt.Printf("%s: ok=%v value==nil:%v\n", key, ok, v == nil)
	}

	if v, ok := c.Get("no-results"); ok {
		if tags, isSlice := v.([]string); isSlice {
			fmt.Println("no-results as []string, nil:", tags == nil)
		}
	}

	// Output:
	// count: ok=true value==nil:false
	// no-results: ok=true value==nil:false
	// never-set: ok=false value==nil:true
	// no-results as []string, nil: true
}
```

### Tests

`TestGetSet` covers several concrete types stored and retrieved through the
same `any` slot, including a nil pointer, plus the ordinary miss on an empty
cache. `TestSetOverwriteKeepsPosition` and `TestSetEvictsOldestAtCapacity`
pin the eviction policy: overwriting an existing key changes its value but
not its place in line, so it can still be the next entry evicted.
`TestNewCacheRejectsNonPositiveCapacity` is the constructor's edge case.

`TestTypedNilSurvivesGet` is the property this module is about, checked
directly rather than through a proxy: `Set` a nil `[]string`, `Get` it back,
and assert `ok` is true, `v == nil` is false, and the type-asserted
`[]string` is nil. `TestIsMissNaiveFailsOnTypedNil` is the antipattern
contrast. `isMissNaive` is unexported and unreachable from the package API;
the test shows it wrongly reports "found" (not a miss) for the cached typed
nil -- which is the correct `ok` result read through the wrong lens -- and
then shows the same check correctly identifying a genuine miss, which is
exactly why the bug is easy to miss in review: the naive check looks right
for the common case and only fails on the case that matters most.
`TestCacheIsSafeForConcurrentUse` exercises `Get` and `Set` from many
goroutines at once, matching the concurrency contract stated on `Cache`.

Create `cache_test.go`:

```go
package anycache

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// isMissNaive is how a Get boundary is often checked the first time it is
// written: "if the value is nil, there was nothing here." It is never
// exported and never reachable from the package API; it exists so the tests
// can pin exactly what it gets wrong. An interface value -- and any is an
// interface -- is nil only when both its type and its value are nil. Boxing a
// typed nil slice into any gives the any a non-nil type descriptor, so this
// check reports found for a value that is, underneath, nil.
func isMissNaive(v any) bool {
	return v == nil
}

func TestGetSet(t *testing.T) {
	t.Parallel()

	c, err := NewCache(4)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}

	tests := []struct {
		name string
		key  string
		set  any
	}{
		{name: "int value", key: "count", set: 42},
		{name: "string value", key: "label", set: "gateway"},
		{name: "struct value", key: "point", set: struct{ X, Y int }{1, 2}},
		{name: "nil pointer value", key: "ptr", set: (*int)(nil)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cc, err := NewCache(4)
			if err != nil {
				t.Fatalf("NewCache: %v", err)
			}
			cc.Set(tc.key, tc.set)
			got, ok := cc.Get(tc.key)
			if !ok {
				t.Fatalf("Get(%q) ok = false, want true", tc.key)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.set) {
				t.Fatalf("Get(%q) = %v, want %v", tc.key, got, tc.set)
			}
		})
	}

	if _, ok := c.Get("absent"); ok {
		t.Fatal("Get on empty cache: ok = true, want false")
	}
}

func TestSetOverwriteKeepsPosition(t *testing.T) {
	t.Parallel()

	c, err := NewCache(2)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("a", 100) // overwrite: value changes, insertion position does not
	c.Set("c", 3)   // capacity 2: "a" is still the oldest, so it is evicted

	if _, ok := c.Get("a"); ok {
		t.Fatal("Get(a) ok = true, want false: a should have been evicted (overwrite does not refresh position)")
	}
	if got, ok := c.Get("b"); !ok || got != 2 {
		t.Fatalf("Get(b) = (%v, %v), want (2, true)", got, ok)
	}
	if got, ok := c.Get("c"); !ok || got != 3 {
		t.Fatalf("Get(c) = (%v, %v), want (3, true)", got, ok)
	}
}

func TestSetEvictsOldestAtCapacity(t *testing.T) {
	t.Parallel()

	c, err := NewCache(2)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3) // evicts a

	if _, ok := c.Get("a"); ok {
		t.Fatal("Get(a) ok = true, want false: a should have been evicted")
	}
	if c.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", c.Len())
	}
}

func TestNewCacheRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, -1, -100} {
		if _, err := NewCache(n); !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("NewCache(%d) error = %v, want ErrInvalidCapacity", n, err)
		}
	}
}

// TestTypedNilSurvivesGet is the core of this module: a nil []string boxed
// into any and stored through Set comes back through Get as a value that is
// found (ok is true) but whose == nil comparison against the untyped nil is
// false, because the any now carries a non-nil type descriptor even though
// the slice value underneath it is nil.
func TestTypedNilSurvivesGet(t *testing.T) {
	t.Parallel()

	c, err := NewCache(4)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	c.Set("no-results", []string(nil))

	v, ok := c.Get("no-results")
	if !ok {
		t.Fatal("Get(no-results) ok = false, want true: the key was set")
	}
	if v == nil {
		t.Fatal("v == nil is true; want false, a typed nil []string is not an untyped nil any")
	}

	got, isSlice := v.([]string)
	if !isSlice {
		t.Fatalf("v.(type) is not []string: %T", v)
	}
	if got != nil {
		t.Fatal("the asserted []string is not nil; want the original nil slice preserved")
	}
}

// TestIsMissNaiveFailsOnTypedNil is the antipattern contrast. isMissNaive
// treats value == nil as "nothing was cached", which is exactly the check
// that fails silently for a cached typed-nil slice: it reports false (not a
// miss), so code guarded by "if isMissNaive(v) { compute and cache }" skips
// recomputation and instead falls through to code that expected a real
// value -- even though what was cached was the explicit "no results" marker.
func TestIsMissNaiveFailsOnTypedNil(t *testing.T) {
	t.Parallel()

	c, err := NewCache(4)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	c.Set("no-results", []string(nil))
	v, ok := c.Get("no-results")
	if !ok {
		t.Fatal("Get(no-results) ok = false, want true")
	}

	if isMissNaive(v) {
		t.Fatal("isMissNaive(v) = true for a cached typed-nil slice; the bug this test pins is that it should be true and is not -- prove the naive check is blind to this case")
	}

	// A real miss, by contrast, returns the untyped nil any and ok=false;
	// isMissNaive happens to agree with ok here, which is exactly why the
	// bug is easy to miss in review: the check looks right for a genuine miss.
	missing, ok := c.Get("never-set")
	if ok {
		t.Fatal("Get(never-set) ok = true, want false")
	}
	if !isMissNaive(missing) {
		t.Fatal("isMissNaive(missing) = false for a genuine miss, want true")
	}
}

func TestCacheIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	c, err := NewCache(50)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i)
			c.Set(key, i)
			if v, ok := c.Get(key); !ok || v != i {
				t.Errorf("goroutine %d: Get(%q) = (%v, %v), want (%d, true)", i, key, v, ok, i)
			}
		}(i)
	}
	wg.Wait()
}

// ExampleCache_typedNil is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleCache_typedNil() {
	c, err := NewCache(4)
	if err != nil {
		panic(err)
	}

	c.Set("count", 3)
	c.Set("no-results", []string(nil))

	for _, key := range []string{"count", "no-results", "never-set"} {
		v, ok := c.Get(key)
		fmt.Printf("%s: ok=%v value==nil:%v\n", key, ok, v == nil)
	}

	if v, ok := c.Get("no-results"); ok {
		if tags, isSlice := v.([]string); isSlice {
			fmt.Println("no-results as []string, nil:", tags == nil)
		}
	}

	// Output:
	// count: ok=true value==nil:false
	// no-results: ok=true value==nil:false
	// never-set: ok=false value==nil:true
	// no-results as []string, nil: true
}
```

## Review

`Get` and `Set` are correct when `ok` is the only thing a caller trusts to
mean "was there an entry." A typed nil -- a nil slice, map, or pointer boxed
into `any` -- carries a non-nil type descriptor even though its underlying
value is nil, so `v == nil` is false for it, and any code that uses that
comparison to detect a miss or an intentionally-empty cached result gets the
wrong answer exactly when it matters. `NewCache` rejects a non-positive
capacity with `ErrInvalidCapacity`, checkable with `errors.Is`. `Set` evicts
the oldest entry by insertion order when a new key would exceed capacity, and
overwriting an existing key updates its value without refreshing its
position. `Cache` is safe for concurrent use: `Get` and `Set` both hold an
internal mutex, and `TestCacheIsSafeForConcurrentUse` exercises that under
`-race`. `ExampleCache_typedNil` is the executable documentation: `go test`
verifies its output. Run `go test -count=1 -race ./...`.

## Resources

- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — the exact rule for when an interface value equals nil.
- [Go FAQ: Why is my nil error value not equal to nil?](https://go.dev/doc/faq#nil_error) — the canonical explanation of typed nil inside an interface, using error rather than any.
- [Effective Go: Interfaces and other types](https://go.dev/doc/effective_go#interfaces_and_types) — background on the two-word interface representation.
- [pkg.go.dev: any](https://pkg.go.dev/builtin#any) — the built-in alias for `interface{}` used throughout this module.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-batch-collector-clear-vs-nil-reset.md](15-batch-collector-clear-vs-nil-reset.md) | Next: [17-json-field-state-classifier.md](17-json-field-state-classifier.md)
