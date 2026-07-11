# Exercise 10: Nil Embedded Pointer and Constructor Discipline

Pointer embedding shares state, which is exactly what you want for a client or a
connection — but it comes with a hazard that value embedding does not: the zero
value has a nil embedded pointer, and any promoted method call then panics. This
exercise contrasts the two and shows the constructor discipline that prevents it.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
svc/                       independent module: example.com/svc
  go.mod                   go 1.26
  svc.go                   Service embeds *Client; New guarantees non-nil; value-embed contrast
  cmd/
    demo/
      main.go              runnable demo: build via New, call the promoted method
  svc_test.go              New yields a usable client; zero-value promoted call panics
```

- Files: `svc.go`, `cmd/demo/main.go`, `svc_test.go`.
- Implement: a `Service` embedding `*Client` whose promoted `Ping` panics if the pointer is nil; a `New` constructor that validates its input and guarantees a non-nil embedded client; a value-embedding `Defaults` type whose zero value is usable, for contrast.
- Test: a `Service` from `New` has a working promoted `Ping`; the zero-value `Service` panics on a promoted call (asserted with `recover`); `New` rejects invalid input.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/svc/cmd/demo
cd ~/go-exercises/svc
go mod init example.com/svc
```

### Why the zero value bites, and how the constructor fixes it

When you embed `*Client`, the embedded field is a pointer, and the zero value of a
pointer is nil. So `var s Service` produces a `Service` whose embedded `*Client` is
nil. `Client`'s methods are still promoted onto `Service` — `s.Ping()` compiles —
but calling one dereferences the nil pointer inside the method (`c.base` where `c`
is nil) and panics with a runtime nil-pointer error. Nothing at compile time warns
you; the type looks complete. This is the single most common embedding bug in
production: a struct assembled field-by-field (or left as its zero value) with the
embedded pointer never set, discovered only when the first promoted call runs.

Value embedding does not have this problem. Embed `Defaults` (a value) and the zero
`Service`-like struct has a fully usable `Defaults` with its own zero fields; a
promoted method just operates on those zero values. That is the trade-off from the
concepts file made concrete: value embedding gives a usable zero value, pointer
embedding gives shared state at the cost of a nil-able zero value.

The fix for pointer embedding is constructor discipline. `New` is the only
sanctioned way to build a `Service`: it validates its inputs (a blank base URL is
rejected with a wrapped sentinel), constructs the `*Client`, and returns a
`*Service` whose embedded pointer is guaranteed non-nil. Callers who go through
`New` can never hold a `Service` that panics on `Ping`. The same discipline applies
to embedded interfaces (previous exercise) for the same reason: initialize the
embedded dependency at construction time and reject the invalid case.

Create `svc.go`:

```go
package svc

import (
	"errors"
	"fmt"
)

// ErrMissingConfig is returned by New when a required dependency is absent.
var ErrMissingConfig = errors.New("missing config")

// Client is the shared, single-instance dependency. Its methods dereference the
// receiver, so calling one on a nil *Client panics.
type Client struct {
	base string
}

// Ping is promoted onto any type that embeds *Client.
func (c *Client) Ping() string {
	return "pong from " + c.base
}

// Defaults is a small value component. Embedded by value, its zero value is
// usable, which is the contrast to the nil-able *Client.
type Defaults struct {
	Timeout int
}

// Retries is promoted from a value-embedded Defaults and works on the zero value.
func (d Defaults) Retries() int {
	if d.Timeout == 0 {
		return 3
	}
	return d.Timeout / 10
}

// Service embeds *Client (shared state, nil-able zero value) and Defaults by
// value (usable zero value). A zero Service has a nil *Client: Ping would panic.
type Service struct {
	*Client
	Defaults
	Name string
}

// New is the only sanctioned constructor. It validates input and guarantees the
// embedded *Client is non-nil, so a Service from New never panics on Ping.
func New(name, base string) (*Service, error) {
	if base == "" {
		return nil, fmt.Errorf("%w: base url", ErrMissingConfig)
	}
	return &Service{
		Client: &Client{base: base},
		Name:   name,
	}, nil
}
```

### The runnable demo

The demo builds a `Service` the right way, through `New`, and calls both a
pointer-embedded promoted method (`Ping`) and a value-embedded one (`Retries`).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/svc"
)

func main() {
	s, err := svc.New("billing", "https://api.internal")
	if err != nil {
		panic(err)
	}
	fmt.Println(s.Ping())                // promoted from *Client
	fmt.Println("retries:", s.Retries()) // promoted from value Defaults

	_, err = svc.New("broken", "")
	fmt.Println("empty base rejected:", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pong from https://api.internal
retries: 3
empty base rejected: true
```

### Tests

`TestNewYieldsUsableClient` builds via `New` and asserts the promoted `Ping`
returns the expected string — the embedded pointer is live. `TestZeroValuePanics`
constructs a zero-value `Service` (bypassing `New`) and asserts that calling the
promoted `Ping` panics, caught with `recover`; this is the exact production bug the
constructor prevents. `TestValueEmbedZeroIsUsable` shows the contrast: a zero
`Service`'s value-embedded `Retries` works without panicking. `TestNewRejectsEmptyBase`
asserts `New` returns the wrapped sentinel for a blank base.

Create `svc_test.go`:

```go
package svc

import (
	"errors"
	"testing"
)

func TestNewYieldsUsableClient(t *testing.T) {
	t.Parallel()
	s, err := New("billing", "https://api.internal")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Ping(); got != "pong from https://api.internal" {
		t.Fatalf("Ping() = %q", got)
	}
}

func TestZeroValuePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("promoted Ping on a zero-value Service (nil *Client) should panic")
		}
	}()
	var s Service // embedded *Client is nil
	_ = s.Ping()
}

func TestValueEmbedZeroIsUsable(t *testing.T) {
	t.Parallel()
	var s Service // embedded Defaults is a usable zero value
	if got := s.Retries(); got != 3 {
		t.Fatalf("Retries() on zero value = %d, want 3", got)
	}
}

func TestNewRejectsEmptyBase(t *testing.T) {
	t.Parallel()
	_, err := New("x", "")
	if !errors.Is(err, ErrMissingConfig) {
		t.Fatalf("New with empty base err = %v, want ErrMissingConfig", err)
	}
}
```

## Review

The service is correct when a value built through `New` can never have a nil
embedded pointer: `New` validates and initializes, so `TestNewYieldsUsableClient`
sees a live `Ping`. `TestZeroValuePanics` documents the danger the constructor
exists to remove — a zero `Service` compiles, looks complete, and panics on the
first promoted call — while `TestValueEmbedZeroIsUsable` shows why value embedding
does not share that hazard. The rule that falls out: pointer-embedded dependencies
must be initialized (and validated) in a constructor, and the zero value of such a
type should be treated as unusable. When a usable zero value matters more than
shared state, value-embed instead.

## Resources

- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — embedded fields, including embedding a pointer type.
- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — pointer embedding and method promotion.
- [Go Specification: Method values](https://go.dev/ref/spec#Method_values) — how a method value binds its receiver, the mechanism behind the nil-pointer panic.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../09-struct-memory-layout-and-padding/00-concepts.md](../09-struct-memory-layout-and-padding/00-concepts.md)
