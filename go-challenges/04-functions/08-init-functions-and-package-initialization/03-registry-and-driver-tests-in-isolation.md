# Exercise 3: Testing Registration Under Concurrency With Fakes

Explicit registration is what makes a registry testable, and the ultimate proof is
a concurrency test: fan N goroutines into `Register` and assert all N drivers
appear, with the race detector confirming the `RWMutex` is the only thing making it
safe. This exercise builds that test suite against in-package fakes, so it depends
on no real driver at all.

This module bundles its own copy of the registry so it gates on its own.

## What you'll build

```text
regtest/                    independent module: example.com/regtest
  go.mod                    module example.com/regtest
  registry.go               Driver/Conn + Registry with Register/Open/Names (bundled)
  registry_test.go          fakeDriver/fakeConn; duplicate + not-found; concurrent Register under -race
  cmd/demo/main.go          registers fakes from goroutines and prints the sorted set
```

Files: `registry.go`, `registry_test.go`, `cmd/demo/main.go`.
Implement: the same `Registry` contract, bundled locally so this module stands alone.
Test: duplicate `Register` is `ErrDriverExists`; missing `Open` is `ErrDriverNotFound`; N goroutines register N distinct names and `len(Names()) == N` with every name present.
Verify: `go test -count=1 -race ./...`

### Why fakes, and why the concurrency test matters

The registry's whole value proposition is that a test controls what is registered.
This suite leans into that: it never imports `mem` or `null`, it defines a tiny
`fakeDriver`/`fakeConn` in the test file, and it builds exactly the world each test
needs. A duplicate test registers the same name twice and asserts `ErrDriverExists`;
a not-found test opens an unregistered name and asserts `ErrDriverNotFound`. Both
sentinels are matched with `errors.Is`, so they keep working even though `Register`
and `Open` wrap them with `%w` and the driver name.

The centerpiece is `TestConcurrentRegistration`. It spins up N goroutines, each
registering a driver with a distinct name (`driver-0` .. `driver-N-1`), waits on a
`sync.WaitGroup`, and asserts that `Names()` returns all N in sorted order.
Structurally this is the exact scenario the `RWMutex` exists for: concurrent writes
to the same map. Run without a mutex, Go's race detector flags a data race and,
worse, concurrent map writes panic outright with "concurrent map writes". Run under
`go test -race`, this test is the ground-truth proof that the lock is correct — not
a comment claiming it is. Because each goroutine registers a *unique* name, no
duplicate error is expected; the assertion is that all N landed.

Create `registry.go` (bundled base package):

```go
// registry.go
package registry

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
)

var (
	ErrDriverExists   = errors.New("driver already registered")
	ErrDriverNotFound = errors.New("driver not found")
)

type Driver interface {
	Name() string
	Open(spec string) (Conn, error)
}

type Conn interface {
	Read(p []byte) (int, error)
	Close() error
}

type Registry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
}

func New() *Registry {
	return &Registry{drivers: make(map[string]Driver)}
}

func (r *Registry) Register(d Driver) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := d.Name()
	if _, ok := r.drivers[name]; ok {
		return fmt.Errorf("%w: %s", ErrDriverExists, name)
	}
	r.drivers[name] = d
	return nil
}

func (r *Registry) Open(name, spec string) (Conn, error) {
	r.mu.RLock()
	d, ok := r.drivers[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrDriverNotFound, name)
	}
	return d.Open(spec)
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Sorted(maps.Keys(r.drivers))
}
```

### The runnable demo

The demo does the concurrent-registration dance outside a test so you can watch it
succeed: it launches goroutines that each register a fake driver, waits, and prints
the sorted set.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"io"
	"sync"

	"example.com/regtest"
)

type demoDriver struct{ name string }

func (d demoDriver) Name() string                       { return d.name }
func (d demoDriver) Open(string) (registry.Conn, error) { return demoConn{}, nil }

type demoConn struct{}

func (demoConn) Read([]byte) (int, error) { return 0, io.EOF }
func (demoConn) Close() error             { return nil }

func main() {
	reg := registry.New()
	var wg sync.WaitGroup
	for i := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = reg.Register(demoDriver{name: fmt.Sprintf("driver-%d", i)})
		}()
	}
	wg.Wait()
	fmt.Println("registered:", reg.Names())
}
```

Note the import path is `example.com/regtest` but the package name is `registry`,
so the demo refers to it as `registry`.

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
registered: [driver-0 driver-1 driver-2 driver-3 driver-4]
```

### Tests

Create `registry_test.go`:

```go
// registry_test.go
package registry

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
)

// fakeDriver is a minimal in-test Driver; the suite needs no real driver.
type fakeDriver struct{ name string }

func (f fakeDriver) Name() string              { return f.name }
func (f fakeDriver) Open(string) (Conn, error) { return &fakeConn{}, nil }

type fakeConn struct{}

func (c *fakeConn) Read(p []byte) (int, error) { return 0, io.EOF }
func (c *fakeConn) Close() error               { return nil }

func TestDuplicateRegisterFails(t *testing.T) {
	t.Parallel()

	r := New()
	if err := r.Register(fakeDriver{name: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(fakeDriver{name: "x"}); !errors.Is(err, ErrDriverExists) {
		t.Fatalf("duplicate err = %v, want ErrDriverExists", err)
	}
}

func TestOpenMissingFails(t *testing.T) {
	t.Parallel()

	r := New()
	if _, err := r.Open("ghost", ""); !errors.Is(err, ErrDriverNotFound) {
		t.Fatalf("Open(ghost) err = %v, want ErrDriverNotFound", err)
	}
}

func TestConcurrentRegistration(t *testing.T) {
	t.Parallel()

	const n = 64
	r := New()

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Register(fakeDriver{name: fmt.Sprintf("driver-%02d", i)}); err != nil {
				t.Errorf("Register(driver-%02d) error = %v", i, err)
			}
		}()
	}
	wg.Wait()

	names := r.Names()
	if len(names) != n {
		t.Fatalf("len(Names()) = %d, want %d", len(names), n)
	}
	for i := range n {
		want := fmt.Sprintf("driver-%02d", i)
		if !slices.Contains(names, want) {
			t.Fatalf("missing %q from Names()", want)
		}
	}
	if !slices.IsSorted(names) {
		t.Fatalf("Names() not sorted: %v", names)
	}
}
```

## Review

The suite is correct when it depends on no real driver and still fully exercises
the contract — that independence is the payoff of explicit registration. The
duplicate and not-found tests confirm the sentinels flow through `%w`; the match
with `errors.Is` must hold despite the wrapped name.

`TestConcurrentRegistration` is the load-bearing one. It must be run under
`go test -race`: without the `RWMutex`, N goroutines writing the same map either
trips the race detector or panics with "concurrent map writes", and this test is
how you would catch a future change that weakens the locking. Because every
goroutine registers a distinct name, the correct outcome is all N present and
sorted; if you see fewer than N, a write was lost to a race. Keep the names unique
in this test — reusing a name would make some `Register` calls legitimately return
`ErrDriverExists`, muddying the "all N landed" assertion.

## Resources

- [Go: data race detector](https://go.dev/doc/articles/race_detector) — what `-race` catches and how to run it.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — fan-out/wait for the concurrent registration test.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching wrapped sentinel errors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-blank-import-side-effect-registration.md](04-blank-import-side-effect-registration.md)
