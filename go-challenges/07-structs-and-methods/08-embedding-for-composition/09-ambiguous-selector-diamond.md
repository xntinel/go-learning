# Exercise 9: Diamond Embedding and Ambiguous Selectors

Compose two mixins that happen to share a field or method name and the bare
selector becomes a compile error: "ambiguous selector". This is a real problem the
moment you combine mixins from two different packages. This exercise ships the
resolved type and shows exactly what the unresolved version would fail to compile.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
mixins/                    independent module: example.com/mixins
  go.mod                   go 1.26
  mixins.go                TimestampMixin + AuditMixin both expose ID/String; Record disambiguates
  cmd/
    demo/
      main.go              runnable demo: print a Record via its explicit String
  mixins_test.go           explicit method wins; qualified access reaches each source
```

- Files: `mixins.go`, `cmd/demo/main.go`, `mixins_test.go`.
- Implement: two mixins each declaring an `ID` field and a `String()` method; a `Record` embedding both, with an explicit `String()` and `EntityID()` that select one source and thereby disambiguate.
- Test: `Record.String()` returns the chosen source; qualified access (`r.TimestampMixin.ID`, `r.AuditMixin.ID`) reaches each; `Record` satisfies `fmt.Stringer`.
- Verify: `go test -count=1 -race ./...`

### Equal-depth collisions do not promote

Promotion is decided by depth, and there is a tie-breaker only when depths differ:
a shallower member hides a deeper one. When two members share a name at the *same*
depth — both reachable at depth 1 because they come from two different directly
embedded types — there is no tie-breaker, and Go refuses to promote either. The
bare selector `r.ID` or `r.String()` is then a compile error, not a runtime
surprise: "ambiguous selector r.ID". Nothing silently wins; the program does not
build. `TimestampMixin` and `AuditMixin` here both declare an `ID` field and a
`String()` method, so a `Record` embedding both has an ambiguous `ID` and an
ambiguous `String()`.

There are two ways to resolve it. The first is to qualify: `r.TimestampMixin.ID`
and `r.AuditMixin.ID` name the specific embedded field, and both compile because
you told Go which one you meant. That is fine internally but leaves the outer
type's bare `ID`/`String()` unusable. The second, better for consumers, is to
declare an explicit member on the outer type at depth 0: a `func (r Record) String()`
shadows both ambiguous depth-1 candidates because depth 0 beats depth 1, and inside
it you pick the source you want (`r.TimestampMixin.String()`). Now `r.String()`
compiles and is unambiguous, and `Record` cleanly satisfies `fmt.Stringer` again.
`EntityID()` does the same for the `ID` collision.

The reason this matters in practice: mixins composed from two packages you do not
control can collide on a common name like `ID`, `Name`, or `String`, and the fix is
always the same — qualify, or declare an explicit disambiguating member.

Create `mixins.go`:

```go
package mixins

// TimestampMixin contributes an ID and a String representation.
type TimestampMixin struct {
	ID string
}

func (t TimestampMixin) String() string {
	return "timestamp:" + t.ID
}

// AuditMixin also contributes an ID and a String representation. Composing both
// into one type makes the bare ID and String() ambiguous.
type AuditMixin struct {
	ID string
}

func (a AuditMixin) String() string {
	return "audit:" + a.ID
}

// Record embeds both mixins. Because ID and String() collide at equal depth, the
// bare selectors r.ID and r.String() would NOT compile (see the note below).
// Record resolves the ambiguity with explicit depth-0 members that pick a source.
type Record struct {
	TimestampMixin
	AuditMixin
}

// String disambiguates the two promoted String() methods by choosing one source.
// This depth-0 method shadows both depth-1 candidates, so Record is a fmt.Stringer.
func (r Record) String() string {
	return r.TimestampMixin.String()
}

// EntityID disambiguates the two ID fields by qualifying through one embedded field.
func (r Record) EntityID() string {
	return r.TimestampMixin.ID
}

// AuditID exposes the other source explicitly, qualified through AuditMixin.
func (r Record) AuditID() string {
	return r.AuditMixin.ID
}
```

Without the explicit members, these bare selectors are compile errors — shown as
an illustration only (they do not build, so they are not part of the package):

```text
r := Record{}
_ = r.ID        // compile error: ambiguous selector r.ID
_ = r.String()  // compile error: ambiguous selector r.String
// Resolve by qualifying: r.TimestampMixin.ID, r.AuditMixin.ID
```

### The runnable demo

The demo builds a `Record`, sets each mixin's ID, and prints it. `fmt.Println` uses
the explicit `String()` because `Record` satisfies `fmt.Stringer`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mixins"
)

func main() {
	r := mixins.Record{
		TimestampMixin: mixins.TimestampMixin{ID: "ts-1"},
		AuditMixin:     mixins.AuditMixin{ID: "au-9"},
	}
	fmt.Println(r) // uses the explicit String()
	fmt.Println("entity:", r.EntityID())
	fmt.Println("audit:", r.AuditID())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
timestamp:ts-1
entity: ts-1
audit: au-9
```

### Tests

`TestExplicitStringWins` asserts `r.String()` returns the `TimestampMixin` form,
proving the depth-0 method resolved the ambiguity. `TestQualifiedAccess` reaches
each embedded field through its qualifier and asserts both are independently
accessible. `TestSatisfiesStringer` pins that `Record` still implements
`fmt.Stringer` after the disambiguation.

Create `mixins_test.go`:

```go
package mixins

import (
	"fmt"
	"testing"
)

func TestExplicitStringWins(t *testing.T) {
	t.Parallel()
	r := Record{
		TimestampMixin: TimestampMixin{ID: "ts-1"},
		AuditMixin:     AuditMixin{ID: "au-9"},
	}
	if got := r.String(); got != "timestamp:ts-1" {
		t.Fatalf("String() = %q, want timestamp:ts-1", got)
	}
}

func TestQualifiedAccess(t *testing.T) {
	t.Parallel()
	r := Record{
		TimestampMixin: TimestampMixin{ID: "ts-1"},
		AuditMixin:     AuditMixin{ID: "au-9"},
	}
	if r.TimestampMixin.ID != "ts-1" {
		t.Errorf("TimestampMixin.ID = %q, want ts-1", r.TimestampMixin.ID)
	}
	if r.AuditMixin.ID != "au-9" {
		t.Errorf("AuditMixin.ID = %q, want au-9", r.AuditMixin.ID)
	}
	if r.EntityID() != "ts-1" || r.AuditID() != "au-9" {
		t.Errorf("accessors = %q,%q; want ts-1,au-9", r.EntityID(), r.AuditID())
	}
}

func TestSatisfiesStringer(t *testing.T) {
	t.Parallel()
	var s fmt.Stringer = Record{TimestampMixin: TimestampMixin{ID: "x"}}
	if s.String() != "timestamp:x" {
		t.Fatalf("Stringer.String() = %q, want timestamp:x", s.String())
	}
}
```

## Review

The type is correct when every selector on `Record` is unambiguous: the explicit
`String()` and the `EntityID`/`AuditID` accessors resolve what the bare `r.String()`
and `r.ID` could not, and `TestSatisfiesStringer` confirms the depth-0 method
restored the `fmt.Stringer` implementation. The lesson is that equal-depth
collisions do not silently pick a winner — they fail to compile — so composing
mixins that share a name forces a deliberate choice: qualify through the specific
embedded field, or declare an explicit outer member. Reach for the explicit member
when consumers should call a clean `r.String()`; reach for qualification when the
access is internal and rare.

## Resources

- [Go Specification: Selectors](https://go.dev/ref/spec#Selectors) — the depth rules and the definition of an ambiguous selector.
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — embedded fields and promotion at equal depth.
- [`fmt.Stringer`](https://pkg.go.dev/fmt#Stringer) — the interface the explicit `String()` restores.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-nil-embedded-pointer-panic.md](10-nil-embedded-pointer-panic.md)
