# Exercise 4: Driver-Style Registration: Factories and Open-by-Name

Instead of registering live instances, register *constructors* in a package-level
table and build a fresh instance on demand with `Open(name)`. This is the
`database/sql` driver model: plugins self-register from `init()`, and the host
selects a backend by a config string without importing it by name.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
drivers/                  independent module: example.com/drivers
  go.mod                  go 1.25
  drivers.go              Factory type; package-level Register (panic on dup) + Open + Registered
  builtin.go              two backends self-registering from init()
  cmd/
    demo/
      main.go             Open two backends by name, use each independently
  drivers_test.go         distinct-instance, unknown-name, duplicate-panic tests
```

- Files: `drivers.go`, `builtin.go`, `cmd/demo/main.go`, `drivers_test.go`.
- Implement: a package-level registry of `func() (Plugin, error)` factories guarded by a `sync.Mutex`; `Register(name, factory)` that panics on a duplicate name; `Open(name)` that builds a fresh instance; `init()` self-registration.
- Test: register two factories and `Open` each into distinct instances (mutating one does not affect the other); `Open` on an unknown name returns an error; a duplicate `Register` panics.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/drivers/cmd/demo
cd ~/go-exercises/drivers
go mod init example.com/drivers
go mod edit -go=1.25
```

### Known backends versus live backends

Storing a live plugin instance couples "this backend is known to exist" to "this
specific instance is alive and configured." That works when the host builds every
plugin itself, but it breaks two real needs. First, a backend package usually
wants to announce itself the moment it is imported — before the host has decided
whether to use it — which means registering at `init()` time, when no instance
should yet exist. Second, opening the same backend twice (two connection pools,
two configured processors) must yield two *independent* instances, not one shared
mutable object.

The `database/sql` model solves both by registering a constructor rather than an
instance. A `Factory` is a `func() (Plugin, error)`; `Register(name, factory)`
records it in a package-level table; `Open(name)` looks up the factory and calls
it to build a fresh instance every time. A backend file self-registers from its
`init()`:

```go
func init() { drivers.Register("memory", func() (drivers.Plugin, error) { return &memBackend{}, nil }) }
```

The host then imports the backend package for its side effect (often with a blank
import `_ "example.com/drivers/backends/memory"`) and calls
`drivers.Open("memory")`. It never references `memBackend` by type — the decoupling
that lets a plugin ship in its own module.

Two deliberate `database/sql` semantics to copy. First, `Register` **panics** on a
duplicate name: a duplicate driver name is a programming error (two packages
claiming the same name), discoverable at startup, and panicking is the documented
behavior of `sql.Register`. Second, the package-level table is guarded by a
`sync.Mutex` because `init()` functions and concurrent `Open` calls both touch it.

Create `drivers.go`:

```go
package drivers

import (
	"fmt"
	"slices"
	"sync"
)

// Plugin is the minimal contract an opened backend satisfies.
type Plugin interface {
	Name() string
	Process(input string) (string, error)
}

// Factory builds a fresh, independent Plugin instance.
type Factory func() (Plugin, error)

var (
	mu        sync.Mutex
	factories = make(map[string]Factory)
)

// Register records factory under name. It panics if name is already registered
// or factory is nil, matching database/sql.Register semantics: a duplicate
// driver name is a programming error caught at startup.
func Register(name string, factory Factory) {
	mu.Lock()
	defer mu.Unlock()
	if factory == nil {
		panic("drivers: Register factory is nil")
	}
	if _, dup := factories[name]; dup {
		panic("drivers: Register called twice for driver " + name)
	}
	factories[name] = factory
}

// Open builds a fresh instance of the named backend. Each call returns a new,
// independent instance. It errors if the name was never registered.
func Open(name string) (Plugin, error) {
	mu.Lock()
	factory, ok := factories[name]
	mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("drivers: unknown driver %q (forgotten import?)", name)
	}
	return factory()
}

// Registered returns the sorted list of registered driver names.
func Registered() []string {
	mu.Lock()
	defer mu.Unlock()
	names := make([]string, 0, len(factories))
	for name := range factories {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
```

### Two self-registering backends

Each backend registers itself from `init()` and carries its own mutable state so
that two `Open`s are provably independent. The `counter` backend counts how many
times it has processed; two opened counters must count separately.

Create `builtin.go`:

```go
package drivers

import "strconv"

// counter is a backend whose Process count is per-instance mutable state.
type counter struct {
	calls int
}

func (c *counter) Name() string { return "counter" }

func (c *counter) Process(input string) (string, error) {
	c.calls++
	return input + "#" + strconv.Itoa(c.calls), nil
}

// echo is a stateless backend.
type echo struct{}

func (echo) Name() string                      { return "echo" }
func (echo) Process(in string) (string, error) { return in, nil }

func init() {
	Register("counter", func() (Plugin, error) { return &counter{}, nil })
	Register("echo", func() (Plugin, error) { return echo{}, nil })
}
```

### The runnable demo

The demo opens the `counter` backend twice and shows the two instances count
independently — the whole point of factory registration. It then lists the
registered drivers.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/drivers"
)

func main() {
	a, err := drivers.Open("counter")
	if err != nil {
		log.Fatal(err)
	}
	b, err := drivers.Open("counter")
	if err != nil {
		log.Fatal(err)
	}

	// a and b are independent instances with independent state.
	o1, _ := a.Process("x")
	o2, _ := a.Process("y")
	o3, _ := b.Process("z")
	fmt.Println(o1, o2, o3)

	fmt.Println("drivers:", drivers.Registered())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
x#1 y#2 z#1
```

Then a second line lists the registered drivers:

```text
drivers: [counter echo]
```

`b`'s counter is at 1, not 3, because `Open` built it a fresh instance — it does
not share `a`'s state.

### Tests

`TestOpenReturnsDistinctInstances` opens `counter` twice and proves mutating one
does not affect the other. `TestOpenUnknownName` asserts an error for a name that
was never registered. `TestDuplicateRegisterPanics` registers a fresh name twice
and recovers the panic, matching `sql.Register`. Each test that calls `Register`
uses a unique name so tests do not collide on the shared package-level table.

Create `drivers_test.go`:

```go
package drivers

import (
	"strings"
	"testing"
)

func TestOpenReturnsDistinctInstances(t *testing.T) {
	t.Parallel()

	a, err := Open("counter")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open("counter")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Process("x"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Process("x"); err != nil {
		t.Fatal(err)
	}
	// a has processed twice; b is fresh and must report #1.
	got, err := b.Process("x")
	if err != nil {
		t.Fatal(err)
	}
	if got != "x#1" {
		t.Fatalf("second instance = %q, want x#1 (instances share state?)", got)
	}
}

func TestOpenUnknownName(t *testing.T) {
	t.Parallel()

	if _, err := Open("does-not-exist"); err == nil {
		t.Fatal("expected error for unknown driver")
	}
}

func TestDuplicateRegisterPanics(t *testing.T) {
	t.Parallel()

	Register("dup-test", func() (Plugin, error) { return echo{}, nil })

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate Register")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "dup-test") {
			t.Fatalf("panic value = %v, want string mentioning dup-test", r)
		}
	}()
	Register("dup-test", func() (Plugin, error) { return echo{}, nil })
}
```

## Review

The factory model is correct when two `Open` calls on the same name yield
instances with independent state — that is what the distinct-instances test pins,
and it is exactly what registering a live instance would violate. The duplicate
panic and the unknown-name error are the two `database/sql` behaviors to copy
faithfully: a duplicate name is a startup-time programming error and should stop
the program loudly, while an unknown name is a runtime lookup miss and should
return an error the caller can handle. Guard the package-level table with a mutex
because `init()` self-registration and concurrent `Open` both reach it; the gate
runs `-race` and will surface an unguarded map.

## Resources

- [database/sql.Register](https://pkg.go.dev/database/sql#Register) — the exact pattern, including the documented duplicate-name panic.
- [database/sql/driver](https://pkg.go.dev/database/sql/driver) — the driver interface the `sql` package opens by name.
- [Effective Go: The init function](https://go.dev/doc/effective_go#init) — `init()` for self-registration side effects.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-thread-safe-registry.md](03-thread-safe-registry.md) | Next: [05-context-aware-processing.md](05-context-aware-processing.md)
