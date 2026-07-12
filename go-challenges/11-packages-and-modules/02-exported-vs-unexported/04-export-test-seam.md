# Exercise 4: Inject a Clock Into Black-Box Tests Without Exporting It

The black-box contract test from the previous exercise has a problem: to assert TTL
expiry it needs to control time, but a `Clock` or `SetClock` in the shipped API would
pollute `go doc` forever. The standard-library answer is `export_test.go`: a file in
the production package (so it can touch the unexported `now`) that is compiled only
under `go test`, exposing a thin exported seam the black-box test can call. This
exercise builds that seam and proves it is invisible to a normal build.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
cachekit/                  independent module: example.com/cachekit
  go.mod                   go 1.26
  cache.go                 package cache: exported API + unexported now clock seam
  export_test.go           package cache, test-only: exports SetClock over now
  cmd/
    demo/
      main.go              runnable demo using only the shipped exported API
  contract_test.go         package cache_test: drives TTL via SetClock, no now reference
```

- Files: `cache.go`, `export_test.go`, `cmd/demo/main.go`, `contract_test.go`.
- Implement: a cache with an unexported `now func() time.Time` seam, plus an `export_test.go` (declared `package cache`) that exposes `SetClock` for black-box tests.
- Test: a black-box `package cache_test` that controls time by calling `SetClock`, never touching `now` directly; confirm `SetClock` is absent from the shipped API (`go doc` shows no `SetClock`, and a normal build does not include `export_test.go`).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/02-exported-vs-unexported/04-export-test-seam/cmd/demo
cd go-solutions/11-packages-and-modules/02-exported-vs-unexported/04-export-test-seam
go mod edit -go=1.26
```

### Why the seam lives in export_test.go

The cache reads the clock through an unexported field `now func() time.Time`,
defaulting to `time.Now`. A white-box test could reassign `now` directly, but the
contract test we want is black-box: it should exercise the cache the way a caller does,
and callers cannot see `now`. We also refuse to add an exported `SetClock` to `cache.go`,
because then every consumer would see it in `go doc`, could call it, and could come to
depend on it, and we could never remove it.

`export_test.go` threads the needle. Its name ends in `_test.go`, so the Go toolchain
compiles it only under `go test` and never in a normal build. But it declares
`package cache`, the production package, so it can reference the unexported `now`. In it
we define an exported method `SetClock` that assigns `now`. The result: during `go test`,
the `cache` package (as seen by the linked-in `cache_test` package) gains a `SetClock`
method the contract test can call; during `go build`, `go install`, and `go doc`, that
method does not exist. The seam is real for tests and invisible to the world.

This is exactly how parts of the standard library expose internals to their external
test packages. The name `export_test.go` is a convention, not a keyword, any
`*_test.go` file in the production package works, but the convention signals "test-only
exports" at a glance.

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

// Cache reads the wall clock through the unexported now field. In production now
// is time.Now; a test-only seam in export_test.go can swap it.
type Cache struct {
	mu   sync.RWMutex
	data map[string]entry
	now  func() time.Time
}

func New() *Cache {
	return &Cache{
		data: make(map[string]entry),
		now:  time.Now,
	}
}

func (c *Cache) Get(key string) (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.data[key]
	if !ok {
		return "", ErrNotFound
	}
	if !e.expiresAt.IsZero() && c.now().After(e.expiresAt) {
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
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = c.now().Add(ttl)
	}
	c.data[key] = entry{value: value, expiresAt: expiresAt}
	return nil
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

Create `export_test.go`. It declares `package cache`, so it can reach `now`, but its
`_test.go` name means it is compiled only under `go test`:

```go
package cache

import "time"

// SetClock is a test-only seam. Because this file is named export_test.go it is
// compiled only under `go test`, so SetClock never appears in the package's
// public API or in `go doc`. It lets black-box tests control time without
// exporting now.
func (c *Cache) SetClock(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}
```

### The runnable demo

The demo can only use the shipped API, `SetClock` does not exist for a normal build,
so it uses a real short sleep to show expiry against the wall clock.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/cachekit"
)

func main() {
	c := cache.New()
	_ = c.Set("session", "alice", 40*time.Millisecond)

	if v, err := c.Get("session"); err == nil {
		fmt.Printf("before expiry: %s\n", v)
	}

	time.Sleep(80 * time.Millisecond)

	if _, err := c.Get("session"); errors.Is(err, cache.ErrExpired) {
		fmt.Println("after expiry: expired")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before expiry: alice
after expiry: expired
```

### The black-box test driving the seam

`package cache_test` calls `SetClock` to install a fixed clock, then advances it to
prove TTL expiry deterministically, no sleeping, no flakiness, and without ever naming
the unexported `now`. A second test confirms the pure exported contract with no seam.

Create `contract_test.go`:

```go
package cache_test

import (
	"errors"
	"testing"
	"time"

	"example.com/cachekit"
)

func TestTTLExpiryViaSeam(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := cache.New()
	c.SetClock(func() time.Time { return now }) // test-only seam from export_test.go

	if err := c.Set("k", "v", time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := c.Get("k"); err != nil {
		t.Fatalf("Get right after Set: %v", err)
	}

	now = now.Add(2 * time.Minute) // advance the fixed clock past the TTL
	if _, err := c.Get("k"); !errors.Is(err, cache.ErrExpired) {
		t.Fatalf("after TTL err = %v, want ErrExpired", err)
	}
}

func TestExportedContractWithoutSeam(t *testing.T) {
	t.Parallel()

	c := cache.New()
	if err := c.Set("k", "v", time.Minute); err != nil {
		t.Fatal(err)
	}
	v, err := c.Get("k")
	if err != nil || v != "v" {
		t.Fatalf("Get = %q,%v want v,nil", v, err)
	}
	c.Delete("k")
	if _, err := c.Get("k"); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("after Delete err = %v, want ErrNotFound", err)
	}
}
```

Confirm the seam is invisible to the shipped API. `go doc` reads the buildable package
only, so it never sees `export_test.go`:

```bash
go doc example.com/cachekit Cache
```

The printed method set is `Delete`, `Get`, `Len`, `Set`, with no `SetClock`. A normal
`go build ./...` likewise excludes `export_test.go`, so nothing outside a test can call
`SetClock`, which is exactly the property we wanted: a deterministic time seam for tests
that ships zero public surface.

## Review

The seam is correct when the black-box `TestTTLExpiryViaSeam` controls time entirely
through `SetClock` and never references `now`, and when `go doc` on `Cache` lists only
`Delete`, `Get`, `Len`, `Set`. The discipline is placement: had `SetClock` (or an
exported `now`) lived in `cache.go`, it would be permanent public API, callable and
dependable by every consumer, and impossible to remove; putting it in `export_test.go`
gives tests the hook while keeping the shipped surface closed. Note that `SetClock`
takes the same lock the cache uses, so reassigning the clock is race-free even though
the demo and tests run under `-race`. If a future refactor moved the seam back into the
production file, the tell would be `go doc` suddenly listing `SetClock`, a sign the
public surface just grew.

## Resources

- [`cmd/go`: Test packages and export_test.go](https://pkg.go.dev/cmd/go#hdr-Test_packages) — how `*_test.go` files in the production package are compiled only under test.
- [Go Spec: Exported identifiers](https://go.dev/ref/spec#Exported_identifiers) — why the external test package needs an exported seam to reach an internal.
- [`go doc` command](https://pkg.go.dev/cmd/go#hdr-Show_documentation_for_package_or_symbol) — reads the buildable package, so it never shows test-only exports.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-blackbox-package-api-surface.md](03-blackbox-package-api-surface.md) | Next: [05-repository-interface-unexported-impl.md](05-repository-interface-unexported-impl.md)
