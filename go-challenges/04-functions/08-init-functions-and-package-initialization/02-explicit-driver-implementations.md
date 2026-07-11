# Exercise 2: Drivers That Implement the Interface Without Self-Registration

A registry is only half a plugin system; the other half is the drivers that
satisfy its interface. This exercise builds two real drivers — an in-memory one
and a null one — as independent subpackages that import the registry to implement
`Driver`/`Conn`, and wires them explicitly from `main`. That base-package +
subpackage + explicit-wiring split is the structural answer to why a registry and
its drivers can never form an import cycle.

This module is fully self-contained: it bundles its own copy of the registry
package so it builds and tests on its own.

## What you'll build

```text
driverkit/                     independent module: example.com/driverkit
  go.mod                       module example.com/driverkit
  internal/registry/registry.go   Driver/Conn interfaces + Registry (bundled)
  drivers/mem/mem.go           in-memory driver: Open(spec) reads spec bytes to EOF
  drivers/mem/mem_test.go      reads to io.EOF, rejects empty spec, interface assertion
  drivers/null/null.go         null driver: Read returns EOF, double Close errors
  drivers/null/null_test.go    EOF on read, double-close error, interface assertion
  cmd/demo/main.go             registers mem and null explicitly, opens both
```

Files: `internal/registry/registry.go`, `drivers/mem/mem.go`, `drivers/mem/mem_test.go`, `drivers/null/null.go`, `drivers/null/null_test.go`, `cmd/demo/main.go`.
Implement: `mem` and `null` drivers implementing `registry.Driver`/`registry.Conn` via `New()` constructors, with no self-registration.
Test: `mem.Conn` reads its spec to `io.EOF` and rejects an empty spec; `null.Conn` returns `io.EOF` on read and errors on double `Close`; compile-time `var _ registry.Driver` assertions pin conformance.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/driverkit/internal/registry ~/go-exercises/driverkit/drivers/mem ~/go-exercises/driverkit/drivers/null ~/go-exercises/driverkit/cmd/demo
cd ~/go-exercises/driverkit
go mod init example.com/driverkit
```

### Why the dependency arrow points one way

The registry defines `Driver` and `Conn`. Each driver imports the registry to
satisfy those interfaces. The dependency arrow points from driver to registry, and
never back — the registry knows nothing about `mem` or `null`. If it did — if the
registry imported the drivers to register them in its own `init()` — the drivers'
import of the registry would close a cycle, and the compiler would reject the whole
program. This is not a style preference; it is a hard constraint that forces the
correct layering. The registry is the stable base; drivers are leaves that depend
on it; and the only place that knows about both is the wiring layer in `main`.

Because registration is explicit, each driver exposes a `New()` constructor and
does exactly nothing at import time — no `init()`, no global side effect. A test
of the `mem` driver constructs it directly and never touches the registry at all.
The compile-time assertion `var _ registry.Driver = (*Driver)(nil)` in each driver
package pins interface conformance at build time: if a method signature drifts, the
package stops compiling rather than failing mysteriously when someone tries to
register it.

The `mem` driver's `Conn` streams the spec bytes through `Read`, advancing a
position cursor and returning `io.EOF` once drained — the standard `io.Reader`
contract. It rejects an empty spec at `Open`, because an empty in-memory source is
almost always a wiring mistake. The `null` driver is the `/dev/null` of drivers:
`Read` returns `io.EOF` immediately (it has nothing to give), and `Close` is
idempotent-guarded so a double `Close` returns an error, which surfaces
double-free bugs in calling code.

Create `internal/registry/registry.go` (the bundled base package):

```go
// internal/registry/registry.go
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

// Driver is the contract each driver subpackage implements.
type Driver interface {
	Name() string
	Open(spec string) (Conn, error)
}

// Conn is the minimal connection a driver returns.
type Conn interface {
	Read(p []byte) (int, error)
	Close() error
}

// Registry maps driver name to Driver. It imports no driver, so no cycle can
// form: the dependency arrow points from driver to registry only.
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

Create `drivers/mem/mem.go`:

```go
// drivers/mem/mem.go
package mem

import (
	"errors"
	"io"

	"example.com/driverkit/internal/registry"
)

// ErrEmptySpec is returned when Open is called without a source.
var ErrEmptySpec = errors.New("mem driver requires a non-empty spec")

// Conn streams the spec bytes through Read until io.EOF.
type Conn struct {
	data []byte
	pos  int
}

func (c *Conn) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := copy(p, c.data[c.pos:])
	c.pos += n
	return n, nil
}

func (c *Conn) Close() error { return nil }

// Driver is a stateless factory. It self-registers with nothing: callers wire
// it explicitly via registry.Register(mem.New()).
type Driver struct{}

// compile-time proof that *Driver satisfies registry.Driver.
var _ registry.Driver = (*Driver)(nil)

func New() *Driver { return &Driver{} }

func (d *Driver) Name() string { return "mem" }

func (d *Driver) Open(spec string) (registry.Conn, error) {
	if spec == "" {
		return nil, ErrEmptySpec
	}
	return &Conn{data: []byte(spec)}, nil
}
```

Create `drivers/null/null.go`:

```go
// drivers/null/null.go
package null

import (
	"errors"
	"io"

	"example.com/driverkit/internal/registry"
)

// ErrDoubleClose is returned when Close is called twice.
var ErrDoubleClose = errors.New("null conn already closed")

// Conn discards everything: Read is always io.EOF, Close is guarded.
type Conn struct {
	closed bool
}

func (c *Conn) Read(p []byte) (int, error) {
	if c.closed {
		return 0, errors.New("read on closed conn")
	}
	return 0, io.EOF
}

func (c *Conn) Close() error {
	if c.closed {
		return ErrDoubleClose
	}
	c.closed = true
	return nil
}

type Driver struct{}

var _ registry.Driver = (*Driver)(nil)

func New() *Driver { return &Driver{} }

func (d *Driver) Name() string { return "null" }

func (d *Driver) Open(spec string) (registry.Conn, error) {
	return &Conn{}, nil
}
```

### The runnable demo

`main` is the only place that imports both the registry and the drivers. It
registers each driver explicitly, so the active set is visible in the source, not
hidden in an `init()`.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"io"

	"example.com/driverkit/drivers/mem"
	"example.com/driverkit/drivers/null"
	"example.com/driverkit/internal/registry"
)

func main() {
	reg := registry.New()
	for _, d := range []registry.Driver{mem.New(), null.New()} {
		if err := reg.Register(d); err != nil {
			fmt.Println("register:", err)
			return
		}
	}
	fmt.Println("drivers:", reg.Names())

	conn, err := reg.Open("mem", "hello driver")
	if err != nil {
		fmt.Println("open mem:", err)
		return
	}
	data, _ := io.ReadAll(conn)
	fmt.Printf("mem read: %s\n", data)
	conn.Close()

	nconn, _ := reg.Open("null", "")
	n, err := nconn.Read(make([]byte, 8))
	fmt.Printf("null read: n=%d eof=%v\n", n, err == io.EOF)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
drivers: [mem null]
mem read: hello driver
null read: n=0 eof=true
```

### Tests

Each driver is tested in isolation, constructed with its own `New()` and never
touching the registry's state — proof that explicit construction keeps a driver
independently testable.

Create `drivers/mem/mem_test.go`:

```go
// drivers/mem/mem_test.go
package mem

import (
	"errors"
	"io"
	"testing"
)

func TestMemConnReadsUntilEOF(t *testing.T) {
	t.Parallel()

	conn, err := New().Open("hello world")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll error = %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("read = %q, want %q", got, "hello world")
	}
}

func TestMemConnRejectsEmptySpec(t *testing.T) {
	t.Parallel()

	_, err := New().Open("")
	if !errors.Is(err, ErrEmptySpec) {
		t.Fatalf("Open(\"\") err = %v, want ErrEmptySpec", err)
	}
}

func TestMemDriverName(t *testing.T) {
	t.Parallel()

	if got := New().Name(); got != "mem" {
		t.Fatalf("Name() = %q, want mem", got)
	}
}
```

Create `drivers/null/null_test.go`:

```go
// drivers/null/null_test.go
package null

import (
	"errors"
	"io"
	"testing"
)

func TestNullConnReadIsEOF(t *testing.T) {
	t.Parallel()

	conn, err := New().Open("anything")
	if err != nil {
		t.Fatal(err)
	}
	n, err := conn.Read(make([]byte, 8))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read = (%d, %v), want (0, io.EOF)", n, err)
	}
}

func TestNullConnRejectsDoubleClose(t *testing.T) {
	t.Parallel()

	conn, err := New().Open("")
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("first Close error = %v", err)
	}
	if err := conn.Close(); !errors.Is(err, ErrDoubleClose) {
		t.Fatalf("second Close err = %v, want ErrDoubleClose", err)
	}
}
```

## Review

The drivers are correct when each satisfies `registry.Driver` (the `var _` lines
prove it at compile time) and does nothing at import time. The structural claim to
internalize is the import direction: `mem` and `null` import `registry`, and
`registry` imports neither, so no cycle is possible and the wiring lives only in
`main`. If you were tempted to add `registry.Default.Register(New())` inside a
driver's `init()`, you would both hide the active set from callers and — the moment
the registry tried to know its drivers — risk the cycle the compiler forbids.

Confirm the `io.EOF` contracts hold: `mem.Conn` must return `(0, io.EOF)` once its
buffer is drained (so `io.ReadAll` terminates), and `null.Conn.Read` must return
`io.EOF` immediately. The double-close guard on `null.Conn` should surface a real
`ErrDoubleClose`, matched with `errors.Is`, so a caller that closes twice learns of
the bug instead of silently succeeding.

## Resources

- [Effective Go: interfaces and methods](https://go.dev/doc/effective_go#interfaces_and_types) — how a type satisfies an interface implicitly.
- [io.EOF and io.Reader](https://pkg.go.dev/io#Reader) — the read contract `mem` and `null` implement.
- [database/sql/driver](https://pkg.go.dev/database/sql/driver) — the real-world driver/conn interface split this exercise models.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-registry-and-driver-tests-in-isolation.md](03-registry-and-driver-tests-in-isolation.md)
