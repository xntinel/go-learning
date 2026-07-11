# Exercise 13: From an init()-Built Global to a Constructor for Parallel Tests

**Nivel: Intermedio** — validacion rapida (un test corto).

A word-frequency cache is a small piece of shared state that is tempting to
build once as a package-level global filled by `init()`. This exercise builds
it instead as an explicitly constructed `Cache` type, so every caller — and
every test — gets its own independent instance and can run in parallel
without stepping on anyone else's counts.

## What you'll build

```text
wordcache/                 independent module: example.com/wordcache
  go.mod                    module example.com/wordcache
  wordcache.go               Cache type, New, Add, Count, Len, Merge
  wordcache_test.go          parallel independent-instance proof + Merge test
```

Files: `wordcache.go`, `wordcache_test.go`.
Implement: a `Cache` struct with `New`, `Add`, `Count`, `Len`, and `Merge` — no package-level global, no `init()`.
Test: two `Cache` instances created by parallel subtests never see each other's counts; `Merge` folds one cache's counts into another without mutating its argument.

Set up the module:

```bash
mkdir -p ~/go-exercises/wordcache
cd ~/go-exercises/wordcache
go mod init example.com/wordcache
go mod edit -go=1.24
```

### The anti-pattern this replaces

The tempting starting point (shown only as an illustrative, non-compiled
sketch) is a single shared cache built once at import time:

```text
// WRONG: one global cache, filled by init(), shared by every caller and
// every test in the package.
var globalCache = map[string]int{}

func init() {
	// pretend this seeds globalCache from some fixed source
}
```

Every caller of this package shares the same map. Two tests that each want
to count different words cannot run with `t.Parallel()` without a data race
or cross-contamination, and nothing can reset the cache between tests short
of restarting the process. `New()` fixes this by making construction
explicit: each call allocates its own map, so independence is structural,
not a matter of test discipline.

Create `wordcache.go`:

```go
// Package wordcache counts word occurrences. Each Cache is an independent,
// explicitly constructed instance rather than a single package-level global
// built by init(), so callers — and tests — never share state by accident.
package wordcache

// Cache counts word occurrences.
type Cache struct {
	counts map[string]int
}

// New returns a fresh, empty Cache. Call this instead of relying on a
// shared package-level cache: New's whole point is that every call returns
// an independent instance, so concurrent or parallel tests never step on
// each other's counts.
func New() *Cache {
	return &Cache{counts: make(map[string]int)}
}

// Add records one occurrence of word.
func (c *Cache) Add(word string) {
	c.counts[word]++
}

// Count returns how many times word has been added.
func (c *Cache) Count(word string) int {
	return c.counts[word]
}

// Len returns how many distinct words have been recorded.
func (c *Cache) Len() int {
	return len(c.counts)
}

// Merge folds the counts of other into c and returns c for chaining. Both
// caches remain independent instances; other is left unmodified.
func (c *Cache) Merge(other *Cache) *Cache {
	for w, n := range other.counts {
		c.counts[w] += n
	}
	return c
}
```

Create `wordcache_test.go`:

```go
package wordcache

import "testing"

// TestIndependentInstances proves two Cache instances never share state.
// With a package-level global built by init(), these subtests could not
// safely run in parallel; with New() each gets its own map.
func TestIndependentInstances(t *testing.T) {
	t.Parallel()

	t.Run("alpha", func(t *testing.T) {
		t.Parallel()
		c := New()
		c.Add("go")
		c.Add("go")
		c.Add("test")
		if got := c.Count("go"); got != 2 {
			t.Fatalf("Count(go) = %d, want 2", got)
		}
	})

	t.Run("beta", func(t *testing.T) {
		t.Parallel()
		c := New()
		c.Add("rust")
		if got := c.Count("go"); got != 0 {
			t.Fatalf("Count(go) = %d, want 0 (leaked from another test's cache)", got)
		}
		if got := c.Count("rust"); got != 1 {
			t.Fatalf("Count(rust) = %d, want 1", got)
		}
	})
}

func TestLen(t *testing.T) {
	c := New()
	if got := c.Len(); got != 0 {
		t.Fatalf("Len() = %d on empty cache, want 0", got)
	}
	c.Add("a")
	c.Add("b")
	c.Add("a")
	if got := c.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}
}

func TestMerge(t *testing.T) {
	a := New()
	a.Add("x")
	a.Add("x")

	b := New()
	b.Add("x")
	b.Add("y")

	a.Merge(b)

	if got := a.Count("x"); got != 3 {
		t.Fatalf("Count(x) after merge = %d, want 3", got)
	}
	if got := a.Count("y"); got != 1 {
		t.Fatalf("Count(y) after merge = %d, want 1", got)
	}
	// b must be left unmodified.
	if got := b.Count("x"); got != 1 {
		t.Fatalf("Merge mutated its argument: b.Count(x) = %d, want 1", got)
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`TestIndependentInstances` is the whole point: both subtests call
`t.Parallel()` and each builds its own `Cache` via `New()`, so there is
nothing to synchronize and nothing to race on. That would be impossible to
do safely with a single package-level cache filled by `init()` — either the
tests serialize, or they race on the shared map. Preferring an explicit
constructor over a global is a small change with an outsized payoff: every
consumer, test or otherwise, gets isolation for free.

## Resources

- [testing.T.Parallel](https://pkg.go.dev/testing#T.Parallel) — marks a test to run in parallel with other parallel tests.
- [Go blog — Package names](https://go.dev/blog/package-names) — on designing a package around explicit constructors rather than global state.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-idempotent-init-with-sync-once.md](12-idempotent-init-with-sync-once.md) | Next: [14-regexp-table-cross-validation-at-init.md](14-regexp-table-cross-validation-at-init.md)
