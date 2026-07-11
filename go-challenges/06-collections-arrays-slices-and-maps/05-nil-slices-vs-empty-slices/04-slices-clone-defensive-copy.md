# Exercise 4: Defensive copy in a cache getter to stop backing-array aliasing

A getter that returns the cache's internal slice hands the caller a live pointer
into shared state. One `append` or one index write later, the cache is corrupted
— and under concurrency it is a data race, not just a surprising mutation. This
exercise builds the cache both ways so you can see the bug, then fixes the getter
with `slices.Clone` and pins the missing-key contract.

This module is fully self-contained: its own `go mod init`, its own `cache`
package, its own demo and tests.

## What you'll build

```text
origincache/                  independent module: example.com/origincache
  go.mod
  cache/cache.go              OriginCache: Set (clones input), GetShared (aliases), Get (clones)
  cache/cache_test.go         independence, aliasing-of-GetShared, missing-key nil, -race test
  cmd/demo/main.go            mutates a Get result, shows the cache is untouched
```

Files: `cache/cache.go`, `cache/cache_test.go`, `cmd/demo/main.go`.
Implement: `OriginCache` with `Set` (stores a clone), `Get` (returns a clone;
missing key returns nil), and a `GetShared` that returns the internal slice to
demonstrate the pitfall.
Test: mutating a `Get` result does not affect a second `Get`; `GetShared` aliases
storage; a missing key returns nil; a `-race` test with concurrent `Get` plus
local mutation.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/origincache/cache ~/go-exercises/origincache/cmd/demo
cd ~/go-exercises/origincache
go mod init example.com/origincache
```

### Why returning the internal slice is a bug

A slice value is a header pointing at a backing array. When a getter returns the
slice it stored, it is not returning a copy of the data — it is returning a
second header pointing at the *same* backing array the cache still holds. The
caller can now write `got[0] = ...` straight through into the cache's storage, or
`append` into spare capacity the cache was relying on. The cache never consented
to that mutation and has no way to detect it. When two goroutines do this at once
— one reading, one mutating the shared array — it is a textbook data race that
`go test -race` will flag.

The fix is to make the boundary hand out data the caller cannot use to reach
back in. `slices.Clone` allocates a fresh backing array and copies the elements,
so the returned slice is fully independent; the caller may mutate or append to it
with no effect on the cache. Clone on the write side too: `Set` stores
`slices.Clone(origins)` so that a caller mutating the slice it passed in cannot
corrupt the stored value after the fact. Both edges are sealed.

The nil case is a deliberate contract. `slices.Clone(nil)` returns nil, so a
`Get` on a missing key returns nil — and callers must read that as "no such key",
which is distinct from a key that is mapped to an empty allow-list (a non-nil
length-zero slice). Documenting that distinction is the point: nil means absent,
`[]` means "known, and empty."

Create `cache/cache.go`:

```go
package cache

import (
	"slices"
	"sync"
)

// OriginCache maps a tenant key to its list of allowed CORS origins. It is read
// far more often than written, so it uses an RWMutex.
type OriginCache struct {
	mu sync.RWMutex
	m  map[string][]string
}

// New returns an empty cache.
func New() *OriginCache {
	return &OriginCache{m: make(map[string][]string)}
}

// Set stores a defensive copy of origins, so a later mutation of the caller's
// slice cannot reach into the cache's stored value.
func (c *OriginCache) Set(key string, origins []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = slices.Clone(origins)
}

// GetShared returns the internal slice directly. It is the PITFALL: a caller
// that appends to or indexes into the result mutates the cache's storage, and
// concurrent callers race. Kept here only to demonstrate the bug in tests.
func (c *OriginCache) GetShared(key string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.m[key]
}

// Get returns a defensive copy the caller may freely mutate. A missing key
// yields nil, because slices.Clone(nil) is nil; callers must treat nil as
// "no such key", distinct from a key mapped to an empty allow-list.
func (c *OriginCache) Get(key string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.m[key])
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/origincache/cache"
)

func main() {
	c := cache.New()
	c.Set("acme", []string{"https://acme.test"})

	// A caller mutates its copy; the cache is untouched.
	got := c.Get("acme")
	got = append(got, "https://evil.test")
	got[0] = "https://tampered.test"

	fmt.Printf("caller sees: %v\n", got)
	fmt.Printf("cache still: %v\n", c.Get("acme"))
	fmt.Printf("missing key nil: %v\n", c.Get("nope") == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
caller sees: [https://tampered.test https://evil.test]
cache still: [https://acme.test]
missing key nil: true
```

### Tests

`TestGetReturnsIndependentCopy` mutates and appends to a `Get` result and asserts
a second `Get` is unchanged — the core guarantee. `TestGetSharedAliasesStorage`
proves the opposite for `GetShared`, pinning exactly why cloning is necessary.
`TestSetClonesInput` seals the write side. `TestMissingKeyReturnsNil` documents
the absent-vs-empty contract. `TestConcurrentGetAndLocalMutation` runs under
`-race`: fifty goroutines each `Get` and mutate a local copy, and the cache must
survive intact, which it does only because each got its own backing array.

Create `cache/cache_test.go`:

```go
package cache

import (
	"slices"
	"sync"
	"testing"
)

func TestGetReturnsIndependentCopy(t *testing.T) {
	t.Parallel()
	c := New()
	c.Set("acme", []string{"https://a.test", "https://b.test"})

	first := c.Get("acme")
	first[0] = "https://tampered.test"
	first = append(first, "https://extra.test")

	second := c.Get("acme")
	want := []string{"https://a.test", "https://b.test"}
	if !slices.Equal(second, want) {
		t.Fatalf("second Get = %v, want %v (mutation leaked)", second, want)
	}
}

func TestGetSharedAliasesStorage(t *testing.T) {
	t.Parallel()
	c := New()
	c.Set("acme", []string{"https://a.test", "https://b.test"})

	shared := c.GetShared("acme")
	shared[0] = "https://tampered.test"

	if got := c.GetShared("acme")[0]; got != "https://tampered.test" {
		t.Fatalf("GetShared did not alias; got %q", got)
	}
}

func TestMissingKeyReturnsNil(t *testing.T) {
	t.Parallel()
	c := New()
	if got := c.Get("absent"); got != nil {
		t.Fatalf("Get(absent) = %v, want nil", got)
	}
}

func TestSetClonesInput(t *testing.T) {
	t.Parallel()
	c := New()
	in := []string{"https://a.test"}
	c.Set("acme", in)
	in[0] = "https://tampered.test" // mutate caller's slice after Set

	if got := c.Get("acme")[0]; got != "https://a.test" {
		t.Fatalf("Set did not clone input; got %q", got)
	}
}

func TestConcurrentGetAndLocalMutation(t *testing.T) {
	t.Parallel()
	c := New()
	c.Set("acme", []string{"https://a.test", "https://b.test"})

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := c.Get("acme")
			if len(local) > 0 {
				local[0] = "https://mutated.test"
			}
			local = append(local, "https://more.test")
			_ = local
		}()
	}
	wg.Wait()

	want := []string{"https://a.test", "https://b.test"}
	if !slices.Equal(c.Get("acme"), want) {
		t.Fatalf("cache corrupted under concurrency: %v", c.Get("acme"))
	}
}
```

## Review

The getter is correct when a caller can do anything to the returned slice —
index-write, append, truncate — and a later `Get` still sees the original data,
proven by `TestGetReturnsIndependentCopy` passing while `TestGetSharedAliasesStorage`
demonstrates the bug the clone avoids. Clone on both edges: `Set` so an
after-the-fact mutation of the caller's input cannot leak in, `Get` so the
caller's mutation cannot leak out. The missing-key contract is a nil slice,
distinct from an empty allow-list, and the `-race` test is what proves the
defensive copy is what makes concurrent readers safe rather than merely lucky.

## Resources

- [slices.Clone](https://pkg.go.dev/slices#Clone) — allocates a fresh backing array; Clone(nil) is nil.
- [slices.Equal](https://pkg.go.dev/slices#Equal) — value comparison used throughout the tests.
- [Go Data Race Detector](https://go.dev/doc/articles/race_detector) — what `-race` catches and how.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-json-patch-tristate-field.md](05-json-patch-tristate-field.md)
