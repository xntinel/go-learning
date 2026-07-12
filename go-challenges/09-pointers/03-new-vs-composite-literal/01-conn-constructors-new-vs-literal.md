# Exercise 1: Connection Constructors: new(Conn) vs &Conn{...}

Every connection pool has a constructor. This one has two, on purpose: `New`
returns `new(Conn)` (a pointer to the zero value) and `NewWithHost` returns
`&Conn{Host, Port}` (a composite literal that initializes fields). Building both
side by side pins the contract that, for a struct, `new(T)` and `&T{}` are the
same allocation, and shows when each reads better.

This module is fully self-contained: its own module, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
connpool/                     independent module: example.com/connpool
  go.mod                      go 1.26
  pool.go                     Conn, New (new), NewWithHost (&Conn{...}), IsZero, Close, Healthy, ErrClosed
  cmd/
    demo/
      main.go                 runnable demo: build both ways, close, observe health
  pool_test.go                the four preserved tests + the new==&Conn{} equivalence test
```

Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
Implement: a `Conn` with `New()` using `new(Conn)`, `NewWithHost(host, port)`
using `&Conn{...}`, plus `IsZero`, `Close`, `Healthy`, and an `ErrClosed`
sentinel; `Close` is nil-receiver-safe.
Test: `New` returns a non-nil zero-value pointer; `NewWithHost` sets fields;
`Close` is idempotent-with-`ErrClosed` and flips `Healthy` false; `Close` on a
nil `*Conn` returns `ErrClosed` without panicking; `New()` equals `&Conn{}`
field-by-field.
Verify: `go test -count=1 -race ./...`

### Why two constructors

The two constructors exist to make the equivalence concrete. `New` calls
`new(Conn)`, which allocates a `Conn`, zeroes every field, and returns the `*Conn`.
`NewWithHost` writes `&Conn{Host: host, Port: port}`, which allocates a `Conn`,
sets those two fields, leaves the rest zero, and returns the `*Conn`. The machine
does the same thing in both cases; the difference is what the code says. `new(Conn)`
announces "I want the zero value." `&Conn{Host: host, Port: port}` announces "I am
constructing a connection with these fields." That is the whole rule: use `new`
(or `var c Conn`) for the zero value, use the composite literal the moment you
have a field to set.

The `closed` field is unexported, so `Healthy` and `Close` gate on it and the
zero value of a `Conn` is a live (not-yet-closed) connection. `Close` is written
to be safe on a nil receiver: a method with a pointer receiver can be called on a
nil pointer, and as long as it does not dereference nil it will not panic. Here a
nil `*Conn` is treated as already unusable, so `Close` returns `ErrClosed`. That
nil-safety is what lets `defer c.Close()` be correct even on a path where `c` was
never assigned.

Create `pool.go`:

```go
package connpool

import "errors"

// ErrClosed is returned by Close on a connection that is already closed or nil.
var ErrClosed = errors.New("connection closed")

// Conn is a single pooled connection. Its zero value is a live, unconfigured
// connection: closed is false, so Healthy reports true.
type Conn struct {
	Host   string
	Port   int
	closed bool
}

// New returns a pointer to the zero value of Conn using new(Conn). It is exactly
// equivalent to returning &Conn{}; new states the "zero value" intent.
func New() *Conn {
	return new(Conn)
}

// NewWithHost returns a *Conn with Host and Port initialized, using a composite
// literal. The literal allocates and initializes in one expression.
func NewWithHost(host string, port int) *Conn {
	return &Conn{Host: host, Port: port}
}

// IsZero reports whether the connection holds only zero-value fields.
func (c *Conn) IsZero() bool {
	return c.Host == "" && c.Port == 0 && !c.closed
}

// Close marks the connection closed. It is idempotent in the sense that a second
// Close (or a Close on a nil *Conn) returns ErrClosed rather than panicking.
func (c *Conn) Close() error {
	if c == nil || c.closed {
		return ErrClosed
	}
	c.closed = true
	return nil
}

// Healthy reports whether the connection is usable: non-nil and not closed.
func (c *Conn) Healthy() bool {
	return c != nil && !c.closed
}
```

### The runnable demo

The demo builds a connection both ways, closes one, and prints the observable
health so you can see the zero-value form and the initialized form behave as
described.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/connpool"
)

func main() {
	zero := connpool.New()
	fmt.Printf("new(Conn): host=%q port=%d healthy=%v isZero=%v\n",
		zero.Host, zero.Port, zero.Healthy(), zero.IsZero())

	c := connpool.NewWithHost("db.local", 5432)
	fmt.Printf("&Conn{...}: host=%q port=%d healthy=%v isZero=%v\n",
		c.Host, c.Port, c.Healthy(), c.IsZero())

	if err := c.Close(); err != nil {
		fmt.Println("first close:", err)
	}
	fmt.Printf("after close: healthy=%v\n", c.Healthy())

	if err := c.Close(); errors.Is(err, connpool.ErrClosed) {
		fmt.Println("second close:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
new(Conn): host="" port=0 healthy=true isZero=true
&Conn{...}: host="db.local" port=5432 healthy=true isZero=false
after close: healthy=false
second close: connection closed
```

### Tests

The four preserved tests fix the behavior of the two constructors and of `Close`,
and `TestNewAndEmptyLiteralEqual` pins the equivalence contract directly:
`New()` dereferenced must equal `&Conn{}` dereferenced, field for field.

Create `pool_test.go`:

```go
package connpool

import (
	"errors"
	"fmt"
	"testing"
)

func TestNewReturnsZeroValuePointer(t *testing.T) {
	t.Parallel()

	c := New()
	if c == nil {
		t.Fatal("New returned nil")
	}
	if c.Host != "" {
		t.Fatalf("Host = %q, want empty", c.Host)
	}
	if c.Port != 0 {
		t.Fatalf("Port = %d, want 0", c.Port)
	}
	if c.closed {
		t.Fatal("closed should be false for a fresh Conn")
	}
	if !c.IsZero() {
		t.Fatal("IsZero = false, want true for fresh Conn")
	}
}

func TestNewWithHostInitializesFields(t *testing.T) {
	t.Parallel()

	c := NewWithHost("db.local", 5432)
	if c == nil {
		t.Fatal("NewWithHost returned nil")
	}
	if c.Host != "db.local" {
		t.Fatalf("Host = %q, want db.local", c.Host)
	}
	if c.Port != 5432 {
		t.Fatalf("Port = %d, want 5432", c.Port)
	}
	if c.IsZero() {
		t.Fatal("IsZero = true, want false for initialized Conn")
	}
}

func TestCloseMarksConnectionAsClosed(t *testing.T) {
	t.Parallel()

	c := NewWithHost("db", 5432)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if c.Healthy() {
		t.Fatal("Healthy = true, want false after Close")
	}
	if err := c.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close err = %v, want ErrClosed", err)
	}
}

func TestCloseOnNilDoesNotPanic(t *testing.T) {
	t.Parallel()

	var c *Conn
	if err := c.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Close err = %v, want ErrClosed", err)
	}
	if c.Healthy() {
		t.Fatal("nil Conn should not be Healthy")
	}
}

// TestNewAndEmptyLiteralEqual pins the core contract: new(Conn) and &Conn{}
// produce the same value.
func TestNewAndEmptyLiteralEqual(t *testing.T) {
	t.Parallel()

	fromNew := New()
	fromLiteral := &Conn{}

	if *fromNew != *fromLiteral {
		t.Fatalf("new(Conn) = %+v, &Conn{} = %+v; want equal", *fromNew, *fromLiteral)
	}
	if fromNew == fromLiteral {
		t.Fatal("distinct allocations must have distinct addresses")
	}
}

func ExampleNewWithHost() {
	c := NewWithHost("cache.local", 6379)
	fmt.Printf("%s:%d healthy=%v\n", c.Host, c.Port, c.Healthy())
	// Output: cache.local:6379 healthy=true
}
```

## Review

The lesson is correct when `New()` and `&Conn{}` are equal in every field but are
distinct allocations, which `TestNewAndEmptyLiteralEqual` asserts on both counts.
`Close` is correct when the second call and the nil-receiver call both return
`ErrClosed` through `errors.Is` rather than panicking; the pointer receiver is
what makes the nil-receiver call legal, and the guard `c == nil || c.closed` is
what makes it safe. The trap this exercise inoculates against is writing
`new(Conn)` and then assigning `Host` and `Port` on the next lines, where
`NewWithHost`'s single composite literal is both shorter and atomic; and the
reverse, writing `&Conn{}` where `new(Conn)` states the zero-value intent more
plainly. Run `go test -race` to confirm nothing here trips the detector, and
`go vet` to confirm the nil-safe method is well formed.

## Resources

- [Go Specification: Allocation (new)](https://go.dev/ref/spec#Allocation) — the exact definition of what `new(T)` returns.
- [Go Specification: Composite literals](https://go.dev/ref/spec#Composite_literals) — `&T{...}` and field elision.
- [Effective Go: Allocation with new](https://go.dev/doc/effective_go#allocation_new) — `new` versus `make`, and when each is idiomatic.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-zero-value-ready-batch.md](02-zero-value-ready-batch.md)
