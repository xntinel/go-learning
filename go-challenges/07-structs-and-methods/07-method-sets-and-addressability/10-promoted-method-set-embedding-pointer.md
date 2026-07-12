# Exercise 10: Embedded Field Receiver and the Outer Type's Method Set

Embedding promotes the inner type's methods onto the outer type — but which
promotions reach the value and which reach only the pointer follows the same
asymmetry as everything else in this lesson. Embed a value and a promoted pointer
method lands only in `*Outer`'s method set; embed a pointer and it lands in both.
That decides whether `Outer` or only `*Outer` satisfies a `Handler` interface.
This module makes that difference concrete and bridges into the embedding chapter.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
promote/                       independent module: example.com/promote
  go.mod                       module path + go directive
  promote.go                   BaseController (pointer Handle); value-embed and pointer-embed outers; guards
  cmd/
    demo/
      main.go                  call promoted Handle through the Handler interface
  promote_test.go              runtime call through the interface; documented failing guards
```

- Files: `promote.go`, `cmd/demo/main.go`, `promote_test.go`.
- Implement: `BaseController` with a pointer-receiver `Handle`; `ValueEmbed` embedding `BaseController` (value); `PointerEmbed` embedding `*BaseController`; compile-time guards recording which of `Outer`/`*Outer` satisfy `Handler`.
- Test: call the promoted `Handle` through the `Handler` interface and assert behavior; document the failing guard combinations as commented lines quoting the compiler error.
- Verify: `go build ./...`, `go vet ./...`, `go test -count=1 -race ./...`.

### How the embed kind decides the promoted method set

`BaseController.Handle` is a pointer method: it records the request and returns a
response, and it mutates the controller's served-count, so it takes `*BaseController`.
It is in the method set of `*BaseController` only.

Now embed it two ways. `ValueEmbed` embeds `BaseController` by value. Promotion of
a pointer method through a value embed reaches `*ValueEmbed`'s method set but not
`ValueEmbed`'s — to call the promoted `Handle` the compiler must form
`&outer.BaseController`, which requires the outer to be addressable, i.e. a
`*ValueEmbed`. So `*ValueEmbed` satisfies `Handler` and a plain `ValueEmbed` does
not:

```text
cannot use ValueEmbed{} (value of type ValueEmbed) as Handler value ...
ValueEmbed does not implement Handler (method Handle has pointer receiver)
```

`PointerEmbed` embeds `*BaseController` — a pointer. Promotion through a pointer
embed reaches both `PointerEmbed` and `*PointerEmbed`, because the embedded field
is already a pointer, so no address of the outer is needed to call `Handle`. Both
`PointerEmbed` and `*PointerEmbed` satisfy `Handler`.

The practical rule for wiring a controller behind an interface: if the base's
methods have pointer receivers (they usually do — controllers hold state), either
use `*Outer` everywhere, or embed the base as a pointer so the value type also
satisfies the interface. Pin the decision with compile-time guards next to the
types, so a wrong embed kind fails the build here instead of at the injection site.

Create `promote.go`:

```go
package promote

import "fmt"

// Handler is the interface a router depends on.
type Handler interface {
	Handle(req string) string
}

// BaseController holds state and mutates it in Handle, so Handle is a pointer
// method and lives in the method set of *BaseController only.
type BaseController struct {
	served int
}

func (b *BaseController) Handle(req string) string {
	b.served++
	return fmt.Sprintf("handled %q (#%d)", req, b.served)
}

// Served exposes the count for tests/demo.
func (b *BaseController) Served() int { return b.served }

// ValueEmbed embeds BaseController by VALUE. The promoted pointer method Handle
// reaches *ValueEmbed's method set only, so *ValueEmbed satisfies Handler and a
// ValueEmbed value does not.
type ValueEmbed struct {
	BaseController
	Route string
}

// PointerEmbed embeds *BaseController. The promoted method reaches both
// PointerEmbed and *PointerEmbed, so both satisfy Handler.
type PointerEmbed struct {
	*BaseController
	Route string
}

// NewPointerEmbed returns a ready PointerEmbed (as a value) with its embedded
// pointer initialized.
func NewPointerEmbed(route string) PointerEmbed {
	return PointerEmbed{BaseController: &BaseController{}, Route: route}
}

// Compile-time guards recording which types satisfy Handler.
var (
	_ Handler = (*ValueEmbed)(nil)   // OK: pointer form has the promoted method
	_ Handler = PointerEmbed{}       // OK: pointer embed promotes to the value too
	_ Handler = (*PointerEmbed)(nil) // OK
)

// The value-embed VALUE form does NOT satisfy Handler. Uncommenting fails with:
//   cannot use ValueEmbed{} (value of type ValueEmbed) as Handler value in
//   variable declaration: ValueEmbed does not implement Handler (method Handle
//   has pointer receiver)
// var _ Handler = ValueEmbed{}
```

### The runnable demo

The demo calls the promoted `Handle` through the `Handler` interface for both a
`*ValueEmbed` and a `PointerEmbed` value, showing both routes reach the base's
state.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/promote"
)

func serve(h promote.Handler, reqs ...string) {
	for _, r := range reqs {
		fmt.Println(h.Handle(r))
	}
}

func main() {
	// *ValueEmbed satisfies Handler; a ValueEmbed value would not compile here.
	serve(&promote.ValueEmbed{Route: "/v"}, "a", "b")

	// PointerEmbed (a value) satisfies Handler because it embeds a pointer.
	serve(promote.NewPointerEmbed("/p"), "c")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
handled "a" (#1)
handled "b" (#2)
handled "c" (#1)
```

### Tests

The tests call the promoted method through the `Handler` interface for both embed
kinds and assert the base's state advances. The failing guard (a `ValueEmbed`
value used as `Handler`) is documented as a commented line quoting the compiler
error, so the suite stays green.

Create `promote_test.go`:

```go
package promote

import (
	"strings"
	"testing"
)

func TestValueEmbedPointerSatisfiesHandler(t *testing.T) {
	t.Parallel()
	// *ValueEmbed satisfies Handler; the value form would not compile.
	var h Handler = &ValueEmbed{Route: "/v"}
	got := h.Handle("req")
	if !strings.Contains(got, "handled") {
		t.Fatalf("Handle returned %q", got)
	}

	// The value form fails to compile:
	//   var _ Handler = ValueEmbed{}
	// ValueEmbed does not implement Handler (method Handle has pointer receiver)
}

func TestPointerEmbedValueSatisfiesHandler(t *testing.T) {
	t.Parallel()
	// A PointerEmbed VALUE satisfies Handler because it embeds *BaseController.
	pe := NewPointerEmbed("/p")
	var h Handler = pe
	h.Handle("one")
	h.Handle("two")

	if pe.Served() != 2 {
		t.Fatalf("served = %d, want 2 (state shared through the embedded pointer)", pe.Served())
	}
}

func TestPromotedMethodMutatesBase(t *testing.T) {
	t.Parallel()
	ve := &ValueEmbed{Route: "/v"}
	ve.Handle("x")
	ve.Handle("y")
	if ve.Served() != 2 {
		t.Fatalf("served = %d, want 2", ve.Served())
	}
}
```

## Review

The wiring is correct when the right form satisfies `Handler`: `*ValueEmbed` does
(the value-embedded pointer method promotes to the pointer only), while a
`PointerEmbed` value does too (the pointer embed promotes to both). The three
compile-time guards encode exactly that, and the runtime tests prove the promoted
`Handle` reaches the base's state through the interface — including that a
`PointerEmbed` value shares its base's counter, because the embedded field is a
pointer.

The mistake this module exists to prevent is embedding the base by value when the
promoted method you need has a pointer receiver, then discovering the outer *value*
type does not satisfy the interface. Fix it by using `*Outer`, or by embedding
`*Base` so the value type satisfies it too. Keep the guards beside the types so the
wrong combination fails at the definition. This is the method-set half of
embedding; the next chapter takes up composition through embedding in full. Run
`go build`, `go vet`, and `go test -race`.

## Resources

- [Go Language Specification: Struct types (embedding)](https://go.dev/ref/spec#Struct_types) — how embedding promotes fields and methods.
- [Go Language Specification: Method sets](https://go.dev/ref/spec#Method_sets) — the promotion rules for value versus pointer embeds.
- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — idiomatic composition through embedded types.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../08-embedding-for-composition/00-concepts.md](../08-embedding-for-composition/00-concepts.md)
