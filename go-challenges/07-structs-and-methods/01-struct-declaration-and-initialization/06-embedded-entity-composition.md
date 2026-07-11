# Exercise 6: Sharing ID and Timestamps via an Embedded Base Entity

Almost every domain type in a backend carries the same three fields: an ID and
`CreatedAt`/`UpdatedAt` timestamps. Copy-pasting them into `Order`, `Invoice`,
`Customer` is the inheritance instinct — but Go has no inheritance. It has
embedding, which promotes the fields and methods of a base `Entity` into each
type that embeds it. This module builds that base and embeds it into two domain
types.

Fully self-contained: own `go mod init`, inline code, own demo and tests.

## What you'll build

```text
entity/                     independent module: example.com/entity
  go.mod                    go 1.24
  entity.go                 base Entity{ID, CreatedAt, UpdatedAt}; Touch; Order; Invoice
  cmd/
    demo/
      main.go               builds an Order, touches it, prints promoted fields
  entity_test.go            promotion, Touch through pointer, shadowing rule
```

- Files: `entity.go`, `cmd/demo/main.go`, `entity_test.go`.
- Implement: a base `Entity{ID string; CreatedAt, UpdatedAt time.Time}` with a `Touch()` method, embedded into `Order` and `Invoice`; a `LegacyRecord` that shadows the promoted `ID` with its own field to demonstrate the collision rule.
- Test: `o.ID`/`o.CreatedAt` resolve to the embedded `Entity` fields; `o.Touch()` updates `o.UpdatedAt`; a shadowing outer field wins over the promoted one while `rec.Entity.ID` still reaches the inner one; a composite literal initializes the embedded struct.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/entity/cmd/demo
cd ~/go-exercises/entity
go mod init example.com/entity
go mod edit -go=1.24
```

### Embedding is composition, not inheritance

You embed a struct by declaring a field with a type and **no name**: `Entity`
inside `Order`. The compiler then *promotes* `Entity`'s fields and methods to
`Order`, so `o.ID`, `o.CreatedAt`, and `o.Touch()` all work even though they are
declared on `Entity`. This looks like inheritance but is not: there is no subtype
relationship (an `*Order` is not an `*Entity`), and promotion is a name-resolution
rule layered over a plain has-a field. `Order` *has an* `Entity`; the promotion is
sugar for `o.Entity.ID`.

Two rules matter in practice.

**Explicit initialization in a literal.** Embedding does not give you a shortcut
in composite literals — you name the embedded type as the field:
`Order{Entity: Entity{ID: "o1", CreatedAt: now}, Total: 500}`. The embedded field's
name *is* its type name.

**Shadowing.** If the outer struct declares a field with the same name as a
promoted one, the outer field **wins** for the unqualified selector, and the
promoted one is shadowed. `LegacyRecord` below embeds `Entity` (which has `ID`) and
also declares its own `ID` — so `rec.ID` is `LegacyRecord`'s field, and you reach
the embedded one only explicitly as `rec.Entity.ID`. This is a real bug source: an
accidental name collision means you read the wrong field silently.

`Touch()` has a **pointer receiver**, so when it is promoted onto `Order`, calling
`o.Touch()` (on an addressable `*Order`) mutates the embedded `Entity`'s
`UpdatedAt` in place — promotion of a pointer-receiver method still reaches back
into the real embedded value, not a copy.

Create `entity.go`:

```go
package entity

import "time"

// Entity is the base every domain type embeds: identity plus audit timestamps.
type Entity struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Touch stamps UpdatedAt with the given instant. Pointer receiver: it mutates.
func (e *Entity) Touch(now time.Time) {
	e.UpdatedAt = now
}

// Order embeds Entity, so it gets ID, CreatedAt, UpdatedAt, and Touch for free.
type Order struct {
	Entity
	Total int
}

// Invoice also embeds Entity: the base is shared by composition, not inheritance.
type Invoice struct {
	Entity
	AmountDue int
}

// LegacyRecord embeds Entity but also declares its own ID, which SHADOWS the
// promoted Entity.ID. rec.ID is this field; rec.Entity.ID is the embedded one.
type LegacyRecord struct {
	Entity
	ID string // shadows Entity.ID
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/entity"
)

func main() {
	created := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	o := entity.Order{
		Entity: entity.Entity{ID: "o-1001", CreatedAt: created},
		Total:  4200,
	}

	// Promoted fields, no Entity qualifier needed.
	fmt.Printf("id=%s created=%s total=%d\n", o.ID, o.CreatedAt.Format(time.RFC3339), o.Total)

	// Promoted pointer-receiver method mutates the embedded Entity in place.
	updated := time.Date(2026, 1, 3, 9, 0, 0, 0, time.UTC)
	o.Touch(updated)
	fmt.Printf("updated=%s\n", o.UpdatedAt.Format(time.RFC3339))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id=o-1001 created=2026-01-02T15:04:05Z total=4200
updated=2026-01-03T09:00:00Z
```

### Tests

`TestPromotedFields` proves `o.ID` and `o.CreatedAt` resolve to the embedded
fields. `TestTouchMutatesThroughPointer` proves the promoted pointer-receiver
method updates the real `UpdatedAt`. `TestShadowingRule` is the subtle one: it
sets the outer `ID` and the inner `Entity.ID` to different values and asserts the
unqualified `rec.ID` returns the outer one while `rec.Entity.ID` returns the inner
one.

Create `entity_test.go`:

```go
package entity

import (
	"fmt"
	"testing"
	"time"
)

func TestPromotedFields(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	o := Order{Entity: Entity{ID: "o1", CreatedAt: created}, Total: 100}

	if o.ID != "o1" {
		t.Fatalf("o.ID = %q, want o1 (promoted from Entity)", o.ID)
	}
	if !o.CreatedAt.Equal(created) {
		t.Fatalf("o.CreatedAt = %v, want %v", o.CreatedAt, created)
	}
	// The promoted field is the same storage as o.Entity.ID.
	if o.ID != o.Entity.ID {
		t.Fatal("promoted o.ID must be o.Entity.ID")
	}
}

func TestTouchMutatesThroughPointer(t *testing.T) {
	t.Parallel()
	o := Order{Entity: Entity{ID: "o1"}}
	if !o.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt should start zero")
	}
	now := time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC)
	o.Touch(now)
	if !o.UpdatedAt.Equal(now) {
		t.Fatalf("Touch did not update embedded UpdatedAt: %v", o.UpdatedAt)
	}
}

func TestInvoiceSharesBase(t *testing.T) {
	t.Parallel()
	inv := Invoice{Entity: Entity{ID: "inv1"}, AmountDue: 999}
	if inv.ID != "inv1" || inv.AmountDue != 999 {
		t.Fatalf("inv = %+v", inv)
	}
}

func TestShadowingRule(t *testing.T) {
	t.Parallel()
	rec := LegacyRecord{
		Entity: Entity{ID: "inner"},
		ID:     "outer",
	}
	if rec.ID != "outer" {
		t.Fatalf("rec.ID = %q, want outer (outer field shadows promoted)", rec.ID)
	}
	if rec.Entity.ID != "inner" {
		t.Fatalf("rec.Entity.ID = %q, want inner (explicit reaches embedded)", rec.Entity.ID)
	}
}

func ExampleOrder() {
	o := Order{Entity: Entity{ID: "o1"}, Total: 50}
	fmt.Println(o.ID, o.Total)
	// Output: o1 50
}
```

## Review

Embedding is correct when it reads as composition: `Order` and `Invoice` each
*have an* `Entity` and get its fields and `Touch` promoted, with no inheritance and
no subtype relationship. The composite-literal rule is the one people forget — you
initialize the embedded value by naming its type (`Entity: Entity{...}`), not with
a positional shortcut. The shadowing rule is the trap: an outer field that
accidentally shares a name with a promoted one silently wins, and you read the
wrong value; when it is deliberate (a legacy type overriding an ID), reach the
inner field explicitly through the embedded type name. And because `Touch` has a
pointer receiver, promotion still mutates the real embedded `Entity` — value
semantics would have touched a copy. Run `go test -race` and `go vet`.

## Resources

- [Effective Go: embedding](https://go.dev/doc/effective_go#embedding) — field and method promotion, and how it differs from inheritance.
- [Go Spec: struct types (embedded fields)](https://go.dev/ref/spec#Struct_types) — the promotion and shadowing rules.
- [Go Spec: selectors](https://go.dev/ref/spec#Selectors) — how `o.ID` resolves through embedded fields.
- [Go Code Review Comments: interfaces / embedding](https://go.dev/wiki/CodeReviewComments) — idiomatic use of composition.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-struct-field-alignment-and-padding.md](07-struct-field-alignment-and-padding.md)
