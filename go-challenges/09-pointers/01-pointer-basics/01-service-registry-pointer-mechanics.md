# Exercise 1: Service Registry — address, deref, value-copy vs pointer-share

An in-memory service registry is the smallest realistic place to see every core
pointer mechanic at once: taking an address, dereferencing it, guarding a `nil`,
and the decisive difference between mutating a value copy and mutating through a
pointer. This module builds that registry and proves each property with tests.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
registry/                  independent module: example.com/registry
  go.mod                   module example.com/registry
  registry.go              Service{Name,Count}; newPtr, addr, deref, incrementValue, incrementPointer; ErrNilService
  cmd/
    demo/
      main.go              runnable demo: value copy vs pointer share, shared address
  registry_test.go         value-does-not-mutate vs pointer-does-mutate; nil paths; shared address
```

- Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: a `Service{Name, Count}` plus `newPtr`, `addr`, `deref`, `incrementValue`, `incrementPointer`, and the sentinel `ErrNilService`.
- Test: value-copy does not mutate the original; pointer-share does; `addr` returns a pointer to a copy (`p != &original`); writing through `*p` changes the original; nil paths return `ErrNilService`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/01-pointer-basics/01-service-registry-pointer-mechanics/cmd/demo
cd go-solutions/09-pointers/01-pointer-basics/01-service-registry-pointer-mechanics
```

### The five helpers and what each proves

`newPtr(name)` returns a `*Service` — the direct construction of a pointer with
`&Service{...}`, the shape a constructor uses in real code. `addr(s Service)
*Service` takes a value and returns `&s`: because `s` is a parameter, it is a
*copy* of whatever the caller passed, so the returned pointer addresses that copy,
not the caller's variable. That is why the test asserts `p != &original` — proving
the copy is a distinct object even though `*p == original` in value. `deref(p
*Service)` is the guarded dereference: `nil` returns `ErrNilService` instead of
panicking, everything else returns `*p`.

`incrementValue(s Service) Service` and `incrementPointer(s *Service) error` are
the heart of the lesson. `incrementValue` receives a copy, increments the copy's
`Count`, and returns the new value; the caller's original is untouched.
`incrementPointer` receives the address, increments the caller's own `Service`
through the pointer, and returns `nil` (or `ErrNilService` on a nil argument). The
contrast — same operation, opposite effect on the caller — is the value-semantics
vs pointer-semantics decision every backend function makes.

Create `registry.go`:

```go
package registry

import "errors"

// ErrNilService is returned when a helper is asked to work through a nil
// *Service instead of dereferencing it and panicking.
var ErrNilService = errors.New("nil service")

// Service is a registry entry: a name and a request counter.
type Service struct {
	Name  string
	Count int
}

// newPtr constructs a fresh Service and returns a pointer to it.
func newPtr(name string) *Service {
	return &Service{Name: name, Count: 0}
}

// addr takes a Service by value and returns a pointer to that copy. The
// returned pointer never equals the address of the caller's variable.
func addr(s Service) *Service {
	return &s
}

// deref reads the value at p, guarding against a nil pointer.
func deref(p *Service) (Service, error) {
	if p == nil {
		return Service{}, ErrNilService
	}
	return *p, nil
}

// incrementValue increments a copy and returns it; the caller's Service is
// left unchanged (value semantics).
func incrementValue(s Service) Service {
	s.Count++
	return s
}

// incrementPointer increments the caller's own Service through the pointer
// (pointer semantics), or reports ErrNilService for a nil argument.
func incrementPointer(s *Service) error {
	if s == nil {
		return ErrNilService
	}
	s.Count++
	return nil
}
```

### The runnable demo

The demo shows the two increments side by side so the value-vs-pointer split is
visible in output: after `incrementValue` the original still reads its old count;
after `incrementPointer(&s)` the same variable now reads the new count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/registry"
)

func main() {
	svc := registry.NewService("api", 1)

	updated := registry.IncrementValue(svc)
	fmt.Printf("after IncrementValue: original=%d copy=%d\n", svc.Count, updated.Count)

	if err := registry.IncrementPointer(&svc); err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("after IncrementPointer: original=%d\n", svc.Count)

	if err := registry.IncrementPointer(nil); err != nil {
		fmt.Println("nil path:", err)
	}
}
```

Because `cmd/demo` is a separate `package main`, it can only touch exported
identifiers. Add thin exported wrappers over the unexported helpers so the demo has
something to call.

Append to `registry.go`:

```go
// NewService is the exported constructor used by the demo.
func NewService(name string, count int) Service {
	return Service{Name: name, Count: count}
}

// IncrementValue is the exported value-semantics increment.
func IncrementValue(s Service) Service { return incrementValue(s) }

// IncrementPointer is the exported pointer-semantics increment.
func IncrementPointer(s *Service) error { return incrementPointer(s) }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after IncrementValue: original=1 copy=2
after IncrementPointer: original=2
nil path: nil service
```

### Tests

The two core tests are the contrast: `TestIncrementValueDoesNotMutateOriginal`
proves the value copy leaves the caller alone, and
`TestIncrementPointerMutatesOriginal` proves the pointer changes it.
`TestAddrReturnsPointerToCopy` asserts `p != &original` (address identity, not value
equality). `TestPointerAndOriginalShareAddress` writes a whole struct through `*p`
and reads the change back from the original variable.
`TestDerefReturnsValueOrNilError` and the nil path of `incrementPointer` assert
`errors.Is(err, ErrNilService)`. `TestPointerTypeIsDistinctFromValueType` pins the
"a pointer is a distinct type" contract by comparing `%T` formatting.

Create `registry_test.go`:

```go
package registry

import (
	"errors"
	"fmt"
	"testing"
)

func TestNewPtrReturnsPointerToFreshService(t *testing.T) {
	t.Parallel()

	s := newPtr("api")
	if s == nil {
		t.Fatal("newPtr returned nil")
	}
	if s.Name != "api" {
		t.Fatalf("Name = %q, want api", s.Name)
	}
	if s.Count != 0 {
		t.Fatalf("Count = %d, want 0", s.Count)
	}
}

func TestAddrReturnsPointerToCopy(t *testing.T) {
	t.Parallel()

	original := Service{Name: "db", Count: 5}
	p := addr(original)
	if p == nil {
		t.Fatal("addr returned nil")
	}
	if p == &original {
		t.Fatal("addr should return a pointer to a copy, not the original")
	}
	if *p != original {
		t.Fatalf("deref = %+v, want %+v", *p, original)
	}
}

func TestDerefReturnsValueOrNilError(t *testing.T) {
	t.Parallel()

	s := Service{Name: "cache", Count: 7}
	got, err := deref(&s)
	if err != nil {
		t.Fatal(err)
	}
	if got != s {
		t.Fatalf("got %+v, want %+v", got, s)
	}

	if _, err := deref(nil); !errors.Is(err, ErrNilService) {
		t.Fatalf("err = %v, want ErrNilService", err)
	}
}

func TestIncrementValueDoesNotMutateOriginal(t *testing.T) {
	t.Parallel()

	original := Service{Name: "api", Count: 1}
	updated := incrementValue(original)
	if updated.Count != 2 {
		t.Fatalf("updated.Count = %d, want 2", updated.Count)
	}
	if original.Count != 1 {
		t.Fatalf("original.Count = %d, want 1 (value copy must not mutate)", original.Count)
	}
}

func TestIncrementPointerMutatesOriginal(t *testing.T) {
	t.Parallel()

	original := Service{Name: "api", Count: 1}
	if err := incrementPointer(&original); err != nil {
		t.Fatal(err)
	}
	if original.Count != 2 {
		t.Fatalf("original.Count = %d, want 2 (pointer share must mutate)", original.Count)
	}

	if err := incrementPointer(nil); !errors.Is(err, ErrNilService) {
		t.Fatalf("err = %v, want ErrNilService", err)
	}
}

func TestPointerAndOriginalShareAddress(t *testing.T) {
	t.Parallel()

	original := Service{Name: "x", Count: 0}
	p := &original
	if p != &original {
		t.Fatal("pointer should equal the address of the original")
	}
	*p = Service{Name: "y", Count: 1}
	if original.Name != "y" {
		t.Fatalf("original.Name = %q, want y (write through pointer)", original.Name)
	}
}

func TestPointerTypeIsDistinctFromValueType(t *testing.T) {
	t.Parallel()

	var p *Service
	var v Service
	if pt, vt := fmt.Sprintf("%T", p), fmt.Sprintf("%T", v); pt != "*registry.Service" || vt != "registry.Service" {
		t.Fatalf("types = %q and %q, want *registry.Service and registry.Service", pt, vt)
	}
}

func Example() {
	svc := NewService("api", 1)
	updated := IncrementValue(svc)
	_ = IncrementPointer(&svc)
	fmt.Printf("copy=%d original=%d\n", updated.Count, svc.Count)
	// Output: copy=2 original=2
}
```

## Review

The registry is correct when the two increments behave oppositely on the caller:
`incrementValue` returns `2` while the original stays `1`, and `incrementPointer`
drives the original itself to `2`. If both mutate the caller, you accidentally took
a pointer where you meant a value; if neither does, you took a value where you meant
a pointer. `addr` returning `p != &original` is the proof that a value parameter is
a copy — the returned address is a fresh object. The nil guards in `deref` and
`incrementPointer` are what turn a nil argument into a handled `ErrNilService`
rather than a runtime panic; asserting them with `errors.Is` keeps the check robust
even if the error is later wrapped. Run `go test -race` — there is no shared state
here, but it is the habit every module in this lesson keeps.

## Resources

- [Go Language Specification: Pointer types and address operators](https://go.dev/ref/spec#Pointer_types)
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values)
- [Go Code Review Comments: Receiver Type](https://go.dev/wiki/CodeReviewComments#receiver-type)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-distinct-pointer-type-contract.md](02-distinct-pointer-type-contract.md)
