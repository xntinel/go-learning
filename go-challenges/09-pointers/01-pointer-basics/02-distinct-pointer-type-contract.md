# Exercise 2: Pin the type contract — *Service is not Service

The lesson prose says "a pointer is a distinct type"; this module turns that claim
into machine-checked assertions with `reflect`, and documents the illegal implicit
conversion as a negative case the compiler enforces. It is the smallest possible
module — its whole job is to pin an invariant precisely.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
ptrtype/                   independent module: example.com/ptrtype
  go.mod                   module example.com/ptrtype
  ptrtype.go               Service{Name}; kindOf, elemOf using reflect
  cmd/
    demo/
      main.go              prints the reflected kind and element type
  ptrtype_test.go          asserts Kind==Pointer, Elem==Service, distinct-from-value
```

- Files: `ptrtype.go`, `cmd/demo/main.go`, `ptrtype_test.go`.
- Implement: `kindOf(any) reflect.Kind` and `elemOf(any) reflect.Type` wrapping `reflect.TypeOf` and `reflect.Type.Elem`.
- Test: `reflect.TypeOf(p).Kind() == reflect.Pointer`; `reflect.TypeOf(p).Elem() == reflect.TypeOf(Service{})`; a `*Service` type is not the `Service` type.
- Verify: `go test -count=1 -race ./...`

### Why reflect proves what the compiler already knows

The compiler enforces the type distinction at build time — you simply cannot write
`var p *Service = Service{}` — but a build error is not something a test can assert
on. `reflect` lets you inspect the type at runtime and make positive assertions.
`reflect.TypeOf(p)` on a `*Service` returns a `reflect.Type` whose `Kind()` is
`reflect.Pointer` (the modern name; `reflect.Ptr` is the retained older alias for
the same constant). Calling `.Elem()` on that pointer type returns the type it
points to — `Service` — so `reflect.TypeOf(p).Elem() == reflect.TypeOf(Service{})`
is a runtime statement of "`*Service` points to `Service`". `elemOf` is only valid
on a pointer (or slice/array/map/chan) type; calling `.Elem()` on a non-pointer
`reflect.Type` panics, which is itself a way the type system tells you a value type
has no element.

The negative case — that assigning a `Service` to a `*Service` without `&` is
illegal — cannot be *compiled* in this module (it would fail the build), so it is
carried as documentation. The commented block below shows exactly what the compiler
rejects; keeping it as a comment means the lesson text carries the negative case
without breaking the gate.

Create `ptrtype.go`:

```go
package ptrtype

import "reflect"

// Service is a stand-in registry entry; only its type identity matters here.
type Service struct {
	Name string
}

// kindOf reports the reflect.Kind of a value's dynamic type. For a *Service it
// is reflect.Pointer; for a Service it is reflect.Struct.
func kindOf(v any) reflect.Kind {
	return reflect.TypeOf(v).Kind()
}

// elemOf returns the element type a pointer type points to. It is only valid
// on a pointer (or slice/array/map/chan) type; on a plain value type
// reflect.Type.Elem panics, which is why callers pass a pointer.
func elemOf(v any) reflect.Type {
	return reflect.TypeOf(v).Elem()
}

// The following is illegal Go and is kept as documentation of the negative
// case the compiler enforces. Uncommenting it fails the build:
//
//	var p *Service = Service{} // cannot use Service{} (value of struct type
//	                           // Service) as *Service value in variable
//	                           // declaration
//
// The fix is to take the address: var p *Service = &Service{}.
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ptrtype"
)

func main() {
	p := &ptrtype.Service{Name: "api"}
	var v ptrtype.Service

	fmt.Printf("kind of *Service: %s\n", ptrtype.KindOf(p))
	fmt.Printf("kind of  Service: %s\n", ptrtype.KindOf(v))
	fmt.Printf("*Service points to: %s\n", ptrtype.ElemOf(p))
}
```

`cmd/demo` is a separate package, so expose the helpers through exported wrappers.

Append to `ptrtype.go`:

```go
// KindOf is the exported wrapper over kindOf.
func KindOf(v any) reflect.Kind { return kindOf(v) }

// ElemOf is the exported wrapper over elemOf.
func ElemOf(v any) reflect.Type { return elemOf(v) }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
kind of *Service: ptr
kind of  Service: struct
*Service points to: ptrtype.Service
```

Note the `reflect.Kind` of a pointer prints as `ptr` (the `Kind`'s string form),
even though the constant is named `reflect.Pointer`.

### Tests

`TestPointerKindIsPointer` asserts `kindOf(p) == reflect.Pointer`.
`TestElemOfPointerIsValueType` asserts `elemOf(p) == reflect.TypeOf(Service{})`.
`TestPointerTypeIsNotValueType` asserts the two `reflect.Type` values are not equal
— `*Service` and `Service` are distinct identities.
`TestTwoPointerValuesShareTypeButNotIdentity` builds two `*Service` over different
underlying structs and asserts they have the same type yet are not `==` (different
addresses), separating "same type" from "same object".

Create `ptrtype_test.go`:

```go
package ptrtype

import (
	"fmt"
	"reflect"
	"testing"
)

func TestPointerKindIsPointer(t *testing.T) {
	t.Parallel()

	p := &Service{Name: "api"}
	if k := kindOf(p); k != reflect.Pointer {
		t.Fatalf("kindOf(p) = %s, want reflect.Pointer", k)
	}
	if k := kindOf(Service{}); k != reflect.Struct {
		t.Fatalf("kindOf(Service{}) = %s, want reflect.Struct", k)
	}
}

func TestElemOfPointerIsValueType(t *testing.T) {
	t.Parallel()

	p := &Service{Name: "api"}
	if got, want := elemOf(p), reflect.TypeOf(Service{}); got != want {
		t.Fatalf("elemOf(p) = %s, want %s", got, want)
	}
}

func TestPointerTypeIsNotValueType(t *testing.T) {
	t.Parallel()

	var p *Service
	var v Service
	pt, vt := reflect.TypeOf(p), reflect.TypeOf(v)
	if pt == vt {
		t.Fatalf("*Service and Service reflect to the same type %s; they must differ", pt)
	}
}

func TestTwoPointerValuesShareTypeButNotIdentity(t *testing.T) {
	t.Parallel()

	a := &Service{Name: "api"}
	b := &Service{Name: "db"}
	if reflect.TypeOf(a) != reflect.TypeOf(b) {
		t.Fatal("two *Service values must share the same type")
	}
	if a == b {
		t.Fatal("two *Service over distinct structs must not be equal pointers")
	}
}

func Example() {
	p := &Service{Name: "api"}
	fmt.Println(kindOf(p), elemOf(p))
	// Output: ptr ptrtype.Service
}
```

## Review

The invariant is pinned when three assertions hold together: the pointer's `Kind`
is `reflect.Pointer`, its `Elem` is exactly the value type, and the pointer type is
not equal to the value type. The fourth test separates the two axes learners often
conflate — two `*Service` share a *type* (so they are interchangeable in a
signature) yet are not equal as *values* unless they hold the same address. The
compile-error negative case lives in a comment on purpose: it is real Go that the
compiler rejects, so it cannot be assembled, but documenting it keeps the "you need
`&`" contract visible. `reflect.Pointer` and `reflect.Ptr` are the same constant;
prefer `reflect.Pointer` in new code.

## Resources

- [`reflect.TypeOf` and `reflect.Type`](https://pkg.go.dev/reflect#TypeOf) — `Kind`, `Elem`, and type identity.
- [`reflect.Kind` constants](https://pkg.go.dev/reflect#Kind) — `Pointer` (and the `Ptr` alias).
- [Go Language Specification: Pointer types](https://go.dev/ref/spec#Pointer_types)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-service-registry-pointer-mechanics.md](01-service-registry-pointer-mechanics.md) | Next: [03-optional-config-fields-with-pointers.md](03-optional-config-fields-with-pointers.md)
