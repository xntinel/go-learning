# Exercise 2: Module Cache with Circular-Import Detection

A module system needs three guarantees: a file is evaluated at most once, a re-entrant import is caught before it recurses forever, and a transient failure never poisons the cache. This exercise builds the structure that owns all three — a cache keyed by absolute path, with the evaluation function injected so the cache itself stays free of the lexer, parser, and evaluator. Injecting the loader is what makes the circular-import behavior testable in isolation: a test can hand the cache a loader that re-imports its own path and assert the cycle is caught, with no `.mk` files and no real evaluation anywhere in sight.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
module/
  cache.go            Cache, NewCache, Resolve, Len, Loader, sentinel errors
  cache_test.go       cache hit, circular detection, error-not-cached, path norm
cmd/
  demo/
    main.go           resolve twice (one load) then trip a self-import cycle
```

- Files: `module/cache.go`, `module/cache_test.go`, `cmd/demo/main.go`.
- Implement: `Loader`, `Cache`, `NewCache`, `(*Cache).Resolve`, `(*Cache).Len`, and the `ErrCircularImport` / `ErrModuleNotFound` sentinels.
- Test: `cache_test.go` proves a second resolve hits the cache, a self-importing loader returns `ErrCircularImport`, a failed load is not cached, equivalent paths normalize to one entry, and `Len` counts only successes.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/40-capstone-language-interpreter/08-full-interpreter-monkey/02-module-cache-circular-import/module go-solutions/40-capstone-language-interpreter/08-full-interpreter-monkey/02-module-cache-circular-import/cmd/demo
cd go-solutions/40-capstone-language-interpreter/08-full-interpreter-monkey/02-module-cache-circular-import
```

### The three asymmetric rules

`Resolve` looks linear but encodes three rules that are deliberately not symmetric, and the asymmetry is the whole correctness argument. First, a cache hit short-circuits before any work: if the path is already in the entries map, return it and never call the loader again. Second, a pending mark is set before evaluation and cleared after it on every exit path; that mark is the cycle detector. Third, the entries map is written only when the loader returned no error, while the pending mark is cleared whether the loader succeeded or failed.

The pending mark is what turns an unbounded recursion into a bounded error. When the loader for `a.mk` triggers an import of `b.mk` which imports `a.mk` again, the second resolve for `a.mk` finds the path still marked pending — its first evaluation has not finished — and returns `ErrCircularImport` immediately instead of recursing. The mark must be cleared on the failure path too, because a mark left behind by a failed load would make every subsequent import of that path falsely report a cycle. A `defer`-style unconditional delete after setting the mark is the clean way to guarantee this across success, error, and panic.

Not caching failed loads is the rule that keeps a long-running REPL usable. If `os.ReadFile` returns a transient error on the first import and the cache stored that nil result, fixing the file on disk would not help — the next import would return the stale failure. By writing to entries only on success, a failed load simply falls through uncached and the next attempt re-evaluates from scratch.

The evaluation runs outside the lock on purpose. The loader for one file may itself call `Resolve` for a nested import, and if `Resolve` held the lock across the loader call, that nested re-entry would deadlock on the same mutex. Holding the lock only around the map reads and writes — and releasing it across the loader call — lets nested imports proceed while still protecting the maps from concurrent REPL goroutines.

Create `module/cache.go`:

```go
// Package module implements the Monkey language import/module system.
// It resolves source files by path, evaluates them once, and caches
// the result so that repeated imports of the same file are O(1).
package module

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
)

// ErrCircularImport is returned when an import chain visits the same file
// more than once before the first evaluation has completed.
var ErrCircularImport = errors.New("circular import")

// ErrModuleNotFound is returned when the resolved absolute path cannot be
// read by the Loader.
var ErrModuleNotFound = errors.New("module not found")

// Loader reads and evaluates the source at absPath, returning an opaque
// result object (typically an *object.Hash in the full interpreter). The
// Loader itself may call Cache.Resolve to handle nested imports.
type Loader func(absPath string) (any, error)

// Cache stores evaluated module results keyed by absolute file path.
// The Monkey evaluator is single-threaded; the mutex guards the cache
// against concurrent REPL goroutines or future parallel evaluation.
type Cache struct {
	mu      sync.Mutex
	entries map[string]any  // absPath -> result; populated after successful eval
	pending map[string]bool // absPath -> true while the file is being evaluated
}

// NewCache returns an empty, ready-to-use Cache.
func NewCache() *Cache {
	return &Cache{
		entries: make(map[string]any),
		pending: make(map[string]bool),
	}
}

// Resolve loads the module at absPath, using load to evaluate it.
// On the first call for a given path it evaluates and caches the result.
// On subsequent calls it returns the cached value without re-evaluating.
// It returns ErrCircularImport (wrapped with %w) when the import chain
// visits absPath before its own evaluation has completed.
//
// Failed loads are not cached: a transient error from load on call N does
// not prevent a successful evaluation on call N+1.
func (c *Cache) Resolve(absPath string, load Loader) (any, error) {
	absPath = filepath.Clean(absPath)

	c.mu.Lock()
	if v, ok := c.entries[absPath]; ok {
		c.mu.Unlock()
		return v, nil
	}
	if c.pending[absPath] {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrCircularImport, filepath.Base(absPath))
	}
	c.pending[absPath] = true
	c.mu.Unlock()

	// Evaluate outside the lock so nested Resolve calls from the loader
	// (imports within the file being loaded) do not deadlock.
	result, err := load(absPath)

	c.mu.Lock()
	delete(c.pending, absPath) // clear regardless of outcome
	if err == nil {
		c.entries[absPath] = result
	}
	c.mu.Unlock()

	return result, err
}

// Len returns the number of successfully cached modules.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
```

### The runnable demo

The demo exercises both behaviors the cache exists for. It resolves the same path twice with a loader that counts its calls, showing the second resolve served from the cache with no second load, and then it hands the cache a loader that imports its own path to trip the cycle detector. The injected loader is the whole trick: no files, no evaluation, just the cache's contract laid bare.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/module-cache/module"
)

func main() {
	c := module.NewCache()

	calls := 0
	loader := func(_ string) (any, error) {
		calls++
		return "compiled", nil
	}
	c.Resolve("/lib/math.mk", loader)
	c.Resolve("/lib/math.mk", loader)
	fmt.Printf("two resolves: loader calls=%d cached=%d\n", calls, c.Len())

	var selfImport module.Loader
	selfImport = func(p string) (any, error) { return c.Resolve(p, selfImport) }
	_, err := c.Resolve("/lib/loop.mk", selfImport)
	fmt.Printf("circular import detected=%t\n", errors.Is(err, module.ErrCircularImport))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
two resolves: loader calls=1 cached=1
circular import detected=true
```

### Tests

The tests cover each of the three rules plus path normalization. The hit test counts loader calls to prove the second resolve is free. The circular test uses a loader that re-resolves its own path and asserts the wrapped sentinel is reachable through `errors.Is`. The error test fails the first load and succeeds the second, proving the failure was not cached. The normalization test sends two paths that differ only by a redundant `./` and asserts they collapse to one entry. The `Len` test confirms a failed load does not inflate the count.

Create `module/cache_test.go`:

```go
package module

import (
	"errors"
	"fmt"
	"testing"
)

func TestCacheHitsOnSecondLoad(t *testing.T) {
	t.Parallel()

	c := NewCache()
	calls := 0
	loader := func(_ string) (any, error) {
		calls++
		return "result", nil
	}

	v1, err := c.Resolve("/a/b.mk", loader)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := c.Resolve("/a/b.mk", loader)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("loader called %d times, want 1", calls)
	}
	if v1 != v2 {
		t.Fatalf("cached value mismatch: %v != %v", v1, v2)
	}
}

func TestCacheDetectsDirectCircularImport(t *testing.T) {
	t.Parallel()

	c := NewCache()
	var loader Loader
	loader = func(p string) (any, error) {
		// The loader tries to import the same path it is currently loading.
		return c.Resolve(p, loader)
	}
	_, err := c.Resolve("/a/b.mk", loader)
	if !errors.Is(err, ErrCircularImport) {
		t.Fatalf("err = %v, want ErrCircularImport", err)
	}
}

func TestCacheDoesNotCacheErrors(t *testing.T) {
	t.Parallel()

	c := NewCache()
	calls := 0
	loader := func(_ string) (any, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("transient read error")
		}
		return "ok", nil
	}

	if _, err := c.Resolve("/a.mk", loader); err == nil {
		t.Fatal("expected error on first load")
	}
	v, err := c.Resolve("/a.mk", loader)
	if err != nil || v != "ok" {
		t.Fatalf("second load: v=%v err=%v", v, err)
	}
	if calls != 2 {
		t.Fatalf("loader calls = %d, want 2", calls)
	}
}

func TestCacheNormalizesPath(t *testing.T) {
	t.Parallel()

	c := NewCache()
	calls := 0
	loader := func(_ string) (any, error) {
		calls++
		return "module", nil
	}
	// These paths differ only by a redundant "./" and must hit the same entry.
	c.Resolve("/a/./b.mk", loader)
	c.Resolve("/a/b.mk", loader)
	if calls != 1 {
		t.Fatalf("loader called %d times for equivalent paths, want 1", calls)
	}
}

func TestCacheLenCounts(t *testing.T) {
	t.Parallel()

	c := NewCache()
	ok := func(_ string) (any, error) { return "x", nil }
	bad := func(_ string) (any, error) { return nil, errors.New("fail") }

	c.Resolve("/a.mk", ok)
	c.Resolve("/b.mk", ok)
	c.Resolve("/c.mk", bad) // must not increment Len

	if got := c.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
}

func ExampleCache_Resolve() {
	c := NewCache()
	loader := func(_ string) (any, error) { return "hello", nil }
	v, err := c.Resolve("/lib.mk", loader)
	if err != nil {
		panic(err)
	}
	fmt.Println(v)
	// Output:
	// hello
}
```

## Review

The cache is correct when the three asymmetric rules all hold together: a second resolve of a cached path never calls the loader, a re-entrant resolve returns a wrapped `ErrCircularImport` reachable through `errors.Is`, and a failed load leaves no entry so the next attempt re-evaluates. Confirm that `filepath.Clean` collapses equivalent paths to a single entry and that `Len` counts successes only. The two details most easily lost are clearing the pending mark on the failure path — without it, one failed import permanently blocks the path — and running the loader outside the lock, without which a nested import deadlocks the cache against itself. Reading `Len` or resolving concurrently must stay clean under `go test -race ./...`.

## Resources

- [Writing An Interpreter In Go, Thorsten Ball](https://interpreterbook.com/) — the evaluator and environment a real loader would drive.
- [pkg.go.dev/path/filepath](https://pkg.go.dev/path/filepath) — `filepath.Clean` and `filepath.Base` used in path normalization and cycle messages.
- [pkg.go.dev/sync](https://pkg.go.dev/sync) — `sync.Mutex` guarding the cache maps.
- [go.dev/blog/go1.13-errors](https://go.dev/blog/go1.13-errors) — `fmt.Errorf("%w", ...)` and `errors.Is` behind the sentinel errors.

---

Back to [01-object-interning-pool.md](01-object-interning-pool.md) | Next: [03-cli-argument-parser.md](03-cli-argument-parser.md)
