# Exercise 1: The Driver Registry as a Plain Library (No init)

The heart of every plugin system — SQL drivers, image decoders, storage backends —
is a name-keyed registry. This exercise builds that registry as a plain,
concurrency-safe library with zero `init()` and no default driver list, so the set
of active drivers is always something a caller (or a test) wires explicitly.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
registry/                  independent module: example.com/registry
  go.mod                   module example.com/registry
  registry.go              Driver, Conn interfaces; Registry with Register/Open/Names; sentinels
  cmd/
    demo/
      main.go              wires a tiny inline driver explicitly and opens a conn
  registry_test.go         table tests: routing, ErrDriverNotFound, sorted Names, fresh per test
```

Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
Implement: a `Registry` keyed by driver name with `New`, `Register`, `Open`, `Names`, and `ErrDriverExists`/`ErrDriverNotFound` sentinels wrapped with `%w`.
Test: register-then-`Open` routes to the right driver; `Open` of an unknown name is `ErrDriverNotFound` via `errors.Is`; `Names` is sorted; each `New()` is an independent world.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/registry/cmd/demo
cd ~/go-exercises/registry
go mod init example.com/registry
```

### Why there is no init and no default list

A registry is tempting to seed. You could imagine the package shipping a global
`Default` registry and having each driver register into it from `init()`. That is
exactly the design this exercise refuses, and the refusal is the lesson. A global
registry filled by `init()` means the set of active drivers is decided by the
import graph — invisible at the call site, impossible for a test to reset, and
different in a test binary that forgot a blank import. A plain `Registry` returned
by `New()` inverts all of that: the caller holds the registry, decides what goes
in it, and a test gets a fresh empty one on every `New()`.

The type is a `map[string]Driver` guarded by a `sync.RWMutex`. `Register` takes
the write lock and refuses a duplicate name with `ErrDriverExists`; `Open` takes
the read lock, looks up the driver, and delegates to its `Open`, returning
`ErrDriverNotFound` for an unknown name. Both sentinels are wrapped with `%w` so a
caller can match them with `errors.Is` while still seeing the offending name in
the message. `Names` returns a sorted slice via `slices.Sorted(maps.Keys(...))` so
the output is deterministic — a registry that returns names in map-iteration order
produces flaky tests and confusing logs.

The `RWMutex` (rather than a plain `Mutex`) reflects the real access pattern:
registration happens a handful of times at startup, but `Open` and `Names` are
read paths that can run concurrently. Reads take `RLock`, so many `Open` calls
proceed in parallel; only `Register` serializes. Under the race detector this
matters — the map must never be read and written concurrently without
synchronization.

Create `registry.go`:

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

// Sentinel errors. Callers match these with errors.Is; the messages carry the
// offending driver name because Register/Open wrap them with %w.
var (
	ErrDriverExists   = errors.New("driver already registered")
	ErrDriverNotFound = errors.New("driver not found")
)

// Driver is the contract a plugin implements. The registry stores drivers by
// Name and delegates connection creation to Open.
type Driver interface {
	Name() string
	Open(spec string) (Conn, error)
}

// Conn is the minimal connection a driver hands back.
type Conn interface {
	Read(p []byte) (int, error)
	Close() error
}

// Registry is a concurrency-safe map of driver name to Driver. It has no
// package-level global and no init(): callers construct one with New and wire
// drivers into it explicitly, which keeps the active set visible and testable.
type Registry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
}

// New returns an empty registry. Each call is an independent world, so a test
// never inherits state from another.
func New() *Registry {
	return &Registry{drivers: make(map[string]Driver)}
}

// Register adds d under its Name. A duplicate name returns ErrDriverExists
// rather than silently overwriting, mirroring the database/sql contract that a
// mis-wired registration must fail loudly.
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

// Open finds the named driver and delegates to its Open. An unknown name
// returns ErrDriverNotFound.
func (r *Registry) Open(name, spec string) (Conn, error) {
	r.mu.RLock()
	d, ok := r.drivers[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrDriverNotFound, name)
	}
	return d.Open(spec)
}

// Names returns the registered driver names in sorted order, so callers and
// logs see a deterministic list rather than map-iteration order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Sorted(maps.Keys(r.drivers))
}
```

### The runnable demo

The demo is the wiring layer: a `package main` that defines one tiny driver,
registers it explicitly, and opens a connection. Because `cmd/demo` is a separate
`package main`, it can only touch the exported API — which is the point. There is
no hidden `init()` doing registration behind its back; the `Register` call is right
there in `main`.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"errors"
	"fmt"
	"io"

	"example.com/registry"
)

// echoDriver is a trivial driver defined and wired by the application itself.
type echoDriver struct{}

func (echoDriver) Name() string { return "echo" }

func (echoDriver) Open(spec string) (registry.Conn, error) {
	return &echoConn{data: []byte(spec)}, nil
}

type echoConn struct {
	data []byte
	pos  int
}

func (c *echoConn) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := copy(p, c.data[c.pos:])
	c.pos += n
	return n, nil
}

func (c *echoConn) Close() error { return nil }

func main() {
	reg := registry.New()
	if err := reg.Register(echoDriver{}); err != nil {
		fmt.Println("register:", err)
		return
	}

	fmt.Println("drivers:", reg.Names())

	conn, err := reg.Open("echo", "hello")
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	defer conn.Close()

	buf := make([]byte, 16)
	n, _ := conn.Read(buf)
	fmt.Printf("read: %s\n", buf[:n])

	if _, err := reg.Open("postgres", ""); errors.Is(err, registry.ErrDriverNotFound) {
		fmt.Println("open postgres:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
drivers: [echo]
read: hello
open postgres: driver not found: postgres
```

### Tests

The tests exercise the contract on a fresh `New()` each time, which is only
possible because there is no global. `TestOpenRoutesToRegisteredDriver` proves a
registered driver's `Open` is the one invoked. `TestOpenUnknownIsNotFound` asserts
the sentinel travels through `%w` and is recoverable with `errors.Is`.
`TestNamesSorted` pins deterministic ordering. `TestRegisterRejectsDuplicate`
checks the loud-failure contract. `TestFreshRegistryIsIndependent` demonstrates
that two registries share nothing — the property a global would destroy.

Create `registry_test.go`:

```go
// registry_test.go
package registry

import (
	"errors"
	"io"
	"testing"
)

// stubDriver is an in-test Driver; it returns a stubConn from Open.
type stubDriver struct{ name string }

func (d stubDriver) Name() string { return d.name }

func (d stubDriver) Open(spec string) (Conn, error) {
	return &stubConn{spec: spec}, nil
}

type stubConn struct {
	spec string
	done bool
}

func (c *stubConn) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF
	}
	n := copy(p, c.spec)
	c.done = true
	return n, nil
}

func (c *stubConn) Close() error { return nil }

func TestOpenRoutesToRegisteredDriver(t *testing.T) {
	t.Parallel()

	r := New()
	if err := r.Register(stubDriver{name: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(stubDriver{name: "b"}); err != nil {
		t.Fatal(err)
	}

	conn, err := r.Open("b", "payload")
	if err != nil {
		t.Fatalf("Open(b) error = %v", err)
	}
	buf := make([]byte, 16)
	n, _ := conn.Read(buf)
	if got := string(buf[:n]); got != "payload" {
		t.Fatalf("routed conn read = %q, want %q", got, "payload")
	}
}

func TestOpenUnknownIsNotFound(t *testing.T) {
	t.Parallel()

	r := New()
	_, err := r.Open("missing", "")
	if !errors.Is(err, ErrDriverNotFound) {
		t.Fatalf("Open(missing) err = %v, want ErrDriverNotFound", err)
	}
}

func TestRegisterRejectsDuplicate(t *testing.T) {
	t.Parallel()

	r := New()
	if err := r.Register(stubDriver{name: "x"}); err != nil {
		t.Fatal(err)
	}
	err := r.Register(stubDriver{name: "x"})
	if !errors.Is(err, ErrDriverExists) {
		t.Fatalf("duplicate Register err = %v, want ErrDriverExists", err)
	}
}

func TestNamesSorted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"already sorted", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"reversed", []string{"z", "m", "a"}, []string{"a", "m", "z"}},
		{"empty", nil, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := New()
			for _, n := range tc.in {
				if err := r.Register(stubDriver{name: n}); err != nil {
					t.Fatal(err)
				}
			}
			got := r.Names()
			if len(got) != len(tc.want) {
				t.Fatalf("Names() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Names() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestFreshRegistryIsIndependent(t *testing.T) {
	t.Parallel()

	r1 := New()
	if err := r1.Register(stubDriver{name: "only-in-r1"}); err != nil {
		t.Fatal(err)
	}
	r2 := New()
	if _, err := r2.Open("only-in-r1", ""); !errors.Is(err, ErrDriverNotFound) {
		t.Fatalf("r2 saw r1's driver; got err = %v", err)
	}
}
```

## Review

The registry is correct when the active set of drivers is exactly what a caller
registered — nothing more, nothing less — and when a fresh `New()` shares nothing
with any other. That property is what the absence of `init()` and of a package
global buys you: `TestFreshRegistryIsIndependent` would be impossible to write
against a design where drivers self-register into a shared default. Confirm the
sentinels flow through `%w` by checking that `errors.Is(err, ErrDriverNotFound)`
holds even though the message also carries the driver name; if a plain
`errors.New` were returned instead, the match would fail. Run `go test -race` to
prove the `RWMutex` actually guards the map — the read paths (`Open`, `Names`) and
the write path (`Register`) must never touch the map concurrently unsynchronized.

The trap to avoid is reintroducing a global "for convenience". The moment a
package ships `var Default = New()` and drivers call `Default.Register` from
`init()`, tests can no longer reset the world and the active set becomes an
import-graph accident. Keep the registry a value the caller owns.

## Resources

- [database/sql: Register](https://pkg.go.dev/database/sql#Register) — the canonical name-keyed driver registry this exercise mirrors.
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — read/write locking for the many-readers, few-writers access pattern.
- [slices.Sorted and maps.Keys](https://pkg.go.dev/slices#Sorted) — deterministic sorted output from a map's keys (Go 1.23+).

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-explicit-driver-implementations.md](02-explicit-driver-implementations.md)
