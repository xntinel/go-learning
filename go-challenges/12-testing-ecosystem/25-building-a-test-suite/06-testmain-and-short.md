# Exercise 6: Suite-Level Fixtures With TestMain And Short Mode

Some setup belongs to the whole test binary, not to any one test: warming a shared
cache, opening a fixture, creating a temp working directory. `TestMain` is the
single per-package hook for that, and its one sharp edge is that `os.Exit` skips
deferred functions, so teardown must be explicit. This module wires a `TestMain`
and guards a slow stress test behind `testing.Short()`.

## What you'll build

```text
mainsuite/                  independent module: example.com/mainsuite
  go.mod
  cache.go                  the cache under test
  cmd/
    demo/
      main.go               runnable demo
  cache_test.go             TestMain fixture + a fast test + a -short-guarded stress test
```

Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
Implement: reuse the cache.
Test: a `TestMain` that warms a shared cache and tears it down around `m.Run`; a fast test that reads the warmed cache; a stress test that `t.Skip`s under `-short`.
Verify: `go test -count=1 -race ./...` and `go test -short ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/25-building-a-test-suite/06-testmain-and-short/cmd/demo
cd go-solutions/12-testing-ecosystem/25-building-a-test-suite/06-testmain-and-short
```

### The TestMain contract, and why teardown cannot defer

`TestMain(m *testing.M)`, when present, is called *instead of* running the tests
directly; nothing runs until you call `m.Run()`, and the process exits with
whatever you pass to `os.Exit`. That gives you a window before and after the entire
suite. The canonical body is: parse flags if you read any (`flag.Parse()`), do
setup, run `code := m.Run()`, do teardown, then `os.Exit(code)`. The order is not
stylistic. `os.Exit` terminates the process immediately and does **not** run
deferred functions, so `defer teardown()` in `TestMain` silently never fires — the
temp directory is never removed, the connection never closed. Teardown must be a
plain call placed *between* `m.Run()` and `os.Exit(code)`, and you must thread the
code through rather than calling `os.Exit(m.Run())` directly, or teardown has
nowhere to go.

Here the fixture is a package-level `shared *Cache` warmed once with entries every
fast test can read. Because setup completes before any test starts and the tests
only *read* the warmed keys, sharing it across parallel tests is safe. The
teardown clears it and records that it ran, which the demo mirrors so you can see
the lifecycle.

The second piece is tiering. A stress test that hammers the cache with many
operations is valuable but slow; you do not want it in the fast inner-loop CI
stage. `testing.Short()` reports whether `-short` was passed, and the convention
is to `t.Skip` at the top of a slow test when it is set. The full suite still runs
the stress test; `go test -short` skips it. There is exactly one `TestMain` per
package, and it owns flag parsing for the binary, which is why `flag.Parse()` lives
there and why `testing.Short()` reads correctly from within the tests.

Create `cache.go`:

```go
package mainsuite

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("cache: key not found")
	ErrExpired  = errors.New("cache: key expired")
)

type entry struct {
	value     []byte
	expiresAt time.Time
}

type Cache struct {
	mu   sync.RWMutex
	data map[string]entry
	now  func() time.Time
}

func New() *Cache {
	return &Cache{data: make(map[string]entry), now: time.Now}
}

func (c *Cache) Get(key string) ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	if !e.expiresAt.IsZero() && c.now().After(e.expiresAt) {
		return nil, ErrExpired
	}
	return e.value, nil
}

func (c *Cache) Set(key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = c.now().Add(ttl)
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
	"fmt"

	"example.com/mainsuite"
)

func main() {
	c := mainsuite.New()
	for i := range 3 {
		c.Set(fmt.Sprintf("warm:%d", i), []byte("v"), 0)
	}
	fmt.Printf("warmed %d entries\n", c.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
warmed 3 entries
```

### The tests

Create `cache_test.go`:

```go
package mainsuite

import (
	"flag"
	"fmt"
	"os"
	"testing"
)

// shared is the suite-wide fixture warmed once by TestMain.
var shared *Cache

func TestMain(m *testing.M) {
	flag.Parse() // read -short, -run, etc. before any test runs

	// setup: warm a shared cache the fast tests read.
	shared = New()
	for i := range 10 {
		shared.Set(fmt.Sprintf("warm:%d", i), []byte("value"), 0)
	}

	code := m.Run()

	// teardown runs here, NOT via defer: os.Exit below skips deferred funcs.
	shared = nil

	os.Exit(code)
}

func TestWarmedFixture(t *testing.T) {
	t.Parallel()
	if got := shared.Len(); got != 10 {
		t.Fatalf("warmed cache Len = %d; want 10", got)
	}
	if v, err := shared.Get("warm:3"); err != nil || string(v) != "value" {
		t.Fatalf("Get(warm:3) = %q, %v; want value, nil", v, err)
	}
}

func TestStress(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping stress test in -short mode")
	}
	c := New()
	const n = 50_000
	for i := range n {
		key := fmt.Sprintf("k:%d", i)
		c.Set(key, []byte("v"), 0)
		if _, err := c.Get(key); err != nil {
			t.Fatalf("Get(%s) err = %v", key, err)
		}
	}
	if got := c.Len(); got != n {
		t.Fatalf("Len = %d; want %d", got, n)
	}
}
```

Run the full suite, then the fast tier:

```bash
go test -count=1 -race ./...
go test -short ./...   # TestStress is skipped
```

Expected output (full run):

```
ok  	example.com/mainsuite	0.8s
```

## Review

`TestMain` is correct when setup runs before `m.Run()`, teardown runs after it as
a plain call, and `os.Exit(code)` propagates the result — never `defer teardown()`,
because `os.Exit` discards deferred functions and the teardown vanishes. The
warmed `shared` cache is safe to read from parallel tests because it is fully built
before any test starts and never mutated afterward; if a test needed to mutate it,
it would take its own instance instead. `testing.Short()` gates the slow
`TestStress` so `go test -short` runs a fast tier while the full suite still
exercises it. There is one `TestMain` per package; put flag parsing there so
`testing.Short()` reads correctly everywhere else.

## Resources

- [`testing.M` and `TestMain`](https://pkg.go.dev/testing#hdr-Main) — the per-package entry point and its contract.
- [`os.Exit`](https://pkg.go.dev/os#Exit) — exits immediately and does not run deferred functions.
- [`testing.Short`](https://pkg.go.dev/testing#Short) — the `-short` flag for tiering slow tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-golden-file-stats-report.md](05-golden-file-stats-report.md) | Next: [07-httptest-cache-aside-handler.md](07-httptest-cache-aside-handler.md)
