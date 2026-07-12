# Exercise 3: Lock the Public Contract With a Black-Box _test Package

A white-box test in `package cache` can reach anything; a black-box test in
`package cache_test` sees only what a real caller sees. The black-box test is the
one a senior writes to lock the public contract: if a refactor of the internals
breaks it, the refactor broke callers too. This exercise builds a TTL cache and its
black-box contract test, and makes the visibility boundary executable by documenting
the exact compiler error you get when the test tries to touch an unexported name.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
cachelib/                  independent module: example.com/cachelib
  go.mod                   go 1.26
  cache.go                 package cache: exported New/Set/Get/Delete/Len; entry/data/set unexported
  cmd/
    demo/
      main.go              runnable demo using only the exported API
  contract_test.go         package cache_test: exercises ONLY the exported surface
```

- Files: `cache.go`, `cmd/demo/main.go`, `contract_test.go`.
- Implement: a cache whose exported surface is `New`, `Set`, `Get`, `Delete`, `Len` plus sentinel errors, with `entry`, `data`, and `set` unexported.
- Test: a black-box test in `package cache_test` that imports the module path and asserts behavior through the exported API only, with a documented compile-failure snippet proving an unexported reference does not compile.
- Verify: `go test -count=1 -race ./...` (the black-box package compiles separately; a passing build proves the contract test touches only exported names).

Set up the module:

```bash
go mod edit -go=1.26
```

### Why a separate _test package is the contract

Go lets two packages live in one directory: the package under test (`package cache`)
and its external test package (`package cache_test`). They compile as two distinct
packages, and `go test` links them together. The external package imports the package
under test by its module path and can therefore reference only its exported identifiers,
exactly like any other consumer. That constraint is the feature: a black-box test is a
compiled, runnable statement of "here is everything a caller is allowed to do, and here
is what it must observe". If a future maintainer renames `data`, splits `entry`, or
replaces the map with a slab allocator, the black-box test still compiles and passes,
because it never mentioned any of those. The day the test stops compiling or passing is
the day the change would have broken a real downstream caller, and you find out at
`go test` time instead of in an incident.

The compile-failure part is what turns this from a convention into knowledge. Below the
contract test we document the exact lines that do not compile from `package cache_test`
and the errors the compiler emits, so the boundary is not "trust me, it is private" but
"here is the diagnostic you will see if you cross it". Those lines stay commented,
because a file that does not compile cannot be part of a passing test.

Create `cache.go`:

```go
package cache

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("cache: key not found")
	ErrExpired  = errors.New("cache: entry expired")
)

type entry struct {
	value     string
	expiresAt time.Time
}

// Cache exposes only New/Set/Get/Delete/Len. data, entry, and set are unexported,
// so a black-box test cannot reach them and neither can any other caller.
type Cache struct {
	mu   sync.RWMutex
	data map[string]entry
}

func New() *Cache {
	return &Cache{data: make(map[string]entry)}
}

func (c *Cache) Get(key string) (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.data[key]
	if !ok {
		return "", ErrNotFound
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		return "", ErrExpired
	}
	return e.value, nil
}

func (c *Cache) Set(key, value string, ttl time.Duration) error {
	if key == "" {
		return errors.New("cache: empty key")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set(key, value, ttl)
	return nil
}

func (c *Cache) set(key, value string, ttl time.Duration) {
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}
	c.data[key] = entry{value: value, expiresAt: expiresAt}
}

func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, key)
}

func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/cachelib"
)

func main() {
	c := cache.New()
	_ = c.Set("a", "1", time.Minute)
	_ = c.Set("b", "2", time.Minute)

	v, _ := c.Get("a")
	fmt.Printf("a -> %s\n", v)
	fmt.Printf("len -> %d\n", c.Len())

	c.Delete("a")
	if _, err := c.Get("a"); errors.Is(err, cache.ErrNotFound) {
		fmt.Println("a -> not found after delete")
	}
}
```

The import path is `example.com/cachelib`, but the package it declares is named
`cache`, so the code refers to it as `cache.New`. Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
a -> 1
len -> 2
a -> not found after delete
```

### The black-box contract test

`package cache_test` imports the module path and drives only the exported surface.
It stores values, reads them, matches the exported sentinels with `errors.Is`, checks
`Len`, and deletes, all without a single reference to `data`, `entry`, or `set`. The
commented block records what happens if you try.

Create `contract_test.go`:

```go
package cache_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"example.com/cachelib"
)

func TestExportedContract(t *testing.T) {
	t.Parallel()

	c := cache.New()

	if err := c.Set("k", "v", time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := c.Get("k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v != "v" {
		t.Fatalf("Get = %q, want v", v)
	}
	if n := c.Len(); n != 1 {
		t.Fatalf("Len = %d, want 1", n)
	}

	c.Delete("k")
	if _, err := c.Get("k"); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("after Delete err = %v, want ErrNotFound", err)
	}
}

func TestEmptyKeyRejected(t *testing.T) {
	t.Parallel()

	c := cache.New()
	if err := c.Set("", "v", 0); err == nil {
		t.Fatal("expected error for empty key")
	}
}

// The following references DO NOT COMPILE from package cache_test, which is the
// whole point: the visibility boundary is enforced by the compiler. Uncomment
// any line to see the exact diagnostic.
//
//	c := cache.New()
//	_ = c.data          // c.data undefined (type *cache.Cache has no field or method data)
//	c.set("k", "v", 0)  // c.set undefined (type *cache.Cache has no field or method set)
//	var e cache.entry   // undefined: cache.entry

func ExampleCache() {
	c := cache.New()
	_ = c.Set("answer", "42", time.Minute)
	v, _ := c.Get("answer")
	fmt.Println(v)
	// Output: 42
}
```

## Review

The contract test is correct when it exercises the full exported surface, `New`, `Set`,
`Get`, `Delete`, `Len`, and the two sentinels, while never naming an internal, and it
proves its point precisely because it still compiles: a `package cache_test` file that
compiled while touching `data` or `set` would mean those names were exported. The
commented block is not decoration; it records the exact compiler errors
(`undefined`, `has no field or method`) so the boundary is executable knowledge rather
than a comment asking for trust. Keep white-box and black-box tests as complementary
tools: this module's black-box test pins the public contract, while a white-box test
(Exercise 1) covers the internals; a mature package ships both, and a refactor that
breaks the black-box test is a refactor that broke callers.

## Resources

- [`cmd/go`: Test packages (external _test package)](https://pkg.go.dev/cmd/go#hdr-Test_packages) — how `package foo_test` compiles separately and links under `go test`.
- [Go Spec: Exported identifiers](https://go.dev/ref/spec#Exported_identifiers) — why the external test package can reach only capitalized names.
- [`testing`](https://pkg.go.dev/testing) — the test harness that links the two packages together.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-json-field-visibility.md](02-json-field-visibility.md) | Next: [04-export-test-seam.md](04-export-test-seam.md)
