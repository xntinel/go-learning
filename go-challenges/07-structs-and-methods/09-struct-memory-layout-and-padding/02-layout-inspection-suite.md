# Exercise 2: Inspecting layout with Sizeof, Alignof, and Offsetof

`unsafe.Sizeof`, `unsafe.Alignof`, and `unsafe.Offsetof` are compile-time
constants that let you assert a struct's layout directly in a test. This module
builds the real layout-invariant suite: every field offset is a multiple of that
field's alignment, offsets increase in declaration order, and the struct's
alignment is the maximum of its fields' — replacing the tautological
`Offsetof >= 0` checks that a naive suite (including the original of this lesson)
gets wrong.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test.

## What you'll build

```text
layout/                    independent module: example.com/layout
  go.mod                   go 1.26
  layout.go                type Bad, type Good; Fields() offset/align report
  cmd/
    demo/
      main.go              prints each field's offset and alignment
  layout_test.go           offset % align == 0; monotonic offsets; struct align == max
```

- Files: `layout.go`, `cmd/demo/main.go`, `layout_test.go`.
- Implement: the `Bad`/`Good` types plus a `Fields(name)` accessor returning each field's name, offset, and alignment so the demo and test can read layout without duplicating the `unsafe` expressions.
- Test: assert `offset % align == 0` for every field, that offsets strictly increase in declaration order, and that `Alignof(struct) == max(field aligns)`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/09-struct-memory-layout-and-padding/02-layout-inspection-suite/cmd/demo
cd go-solutions/07-structs-and-methods/09-struct-memory-layout-and-padding/02-layout-inspection-suite
```

### The real invariant, and the bug it replaces

The tempting-but-wrong test is `if unsafe.Offsetof(s.f) < 0 { t.Fatal(...) }`.
`Offsetof` returns `uintptr`, which is unsigned, so an offset can never be
negative: `>= 0` is always true, `< 0` is always false, and the assertion tests
nothing. It compiles, it passes, and it gives false confidence. This exact
pattern shipped in the earlier version of this lesson.

There are three invariants actually worth asserting, and together they pin the
layout:

1. **Alignment**: every field's offset is a multiple of that field's alignment,
   `offset % align == 0`. This is the defining property of correct layout — if it
   failed, the field would be misaligned.
2. **Monotonic order**: because Go lays fields out in declaration order and never
   reorders, offsets strictly increase as you walk the fields in source order. If
   two consecutive offsets were out of order, either Go reordered (it does not)
   or your table is wrong.
3. **Struct alignment**: a struct's alignment equals the maximum alignment of any
   field. For both `Bad` and `Good`, whose widest field is 8-aligned, the struct
   alignment is 8.

To assert these without scattering `unsafe.Offsetof(Bad{}.Flag)` expressions
across the test, `Fields` collects each field's name, offset, and alignment into
a slice once. `Offsetof` needs a literal field selector at compile time, so the
slice is built by naming each field explicitly — that is the correct, readable
way, not `unsafe.Pointer` arithmetic.

Create `layout.go`:

```go
// Package layout inspects struct field layout with the unsafe compile-time
// operators and exposes it as data so tests can assert alignment invariants.
package layout

import "unsafe"

// Bad interleaves small fields between 8-byte fields, forcing interior padding.
type Bad struct {
	Flag   bool
	Count  int64
	Letter byte
	Score  float64
	Active bool
	Total  int32
}

// Good orders fields largest-alignment-first, eliminating interior padding.
type Good struct {
	Score  float64
	Count  int64
	Total  int32
	Letter byte
	Flag   bool
	Active bool
}

// FieldInfo is one field's layout: its name, byte offset within the struct,
// and its alignment.
type FieldInfo struct {
	Name   string
	Offset uintptr
	Align  uintptr
}

// BadFields reports the layout of Bad in declaration order.
func BadFields() []FieldInfo {
	var b Bad
	return []FieldInfo{
		{"Flag", unsafe.Offsetof(b.Flag), unsafe.Alignof(b.Flag)},
		{"Count", unsafe.Offsetof(b.Count), unsafe.Alignof(b.Count)},
		{"Letter", unsafe.Offsetof(b.Letter), unsafe.Alignof(b.Letter)},
		{"Score", unsafe.Offsetof(b.Score), unsafe.Alignof(b.Score)},
		{"Active", unsafe.Offsetof(b.Active), unsafe.Alignof(b.Active)},
		{"Total", unsafe.Offsetof(b.Total), unsafe.Alignof(b.Total)},
	}
}

// GoodFields reports the layout of Good in declaration order.
func GoodFields() []FieldInfo {
	var g Good
	return []FieldInfo{
		{"Score", unsafe.Offsetof(g.Score), unsafe.Alignof(g.Score)},
		{"Count", unsafe.Offsetof(g.Count), unsafe.Alignof(g.Count)},
		{"Total", unsafe.Offsetof(g.Total), unsafe.Alignof(g.Total)},
		{"Letter", unsafe.Offsetof(g.Letter), unsafe.Alignof(g.Letter)},
		{"Flag", unsafe.Offsetof(g.Flag), unsafe.Alignof(g.Flag)},
		{"Active", unsafe.Offsetof(g.Active), unsafe.Alignof(g.Active)},
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"unsafe"

	"example.com/layout"
)

func main() {
	fmt.Printf("Bad (size %d, align %d):\n", unsafe.Sizeof(layout.Bad{}), unsafe.Alignof(layout.Bad{}))
	for _, f := range layout.BadFields() {
		fmt.Printf("  %-7s offset=%2d align=%d\n", f.Name, f.Offset, f.Align)
	}
	fmt.Printf("Good (size %d, align %d):\n", unsafe.Sizeof(layout.Good{}), unsafe.Alignof(layout.Good{}))
	for _, f := range layout.GoodFields() {
		fmt.Printf("  %-7s offset=%2d align=%d\n", f.Name, f.Offset, f.Align)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (on a 64-bit platform):

```
Bad (size 40, align 8):
  Flag    offset= 0 align=1
  Count   offset= 8 align=8
  Letter  offset=16 align=1
  Score   offset=24 align=8
  Active  offset=32 align=1
  Total   offset=36 align=4
Good (size 24, align 8):
  Score   offset= 0 align=8
  Count   offset= 8 align=8
  Total   offset=16 align=4
  Letter  offset=20 align=1
  Flag    offset=21 align=1
  Active  offset=22 align=1
```

### Tests

The suite runs the three invariants over both types via a shared helper. Note
that alignment (`offset % align == 0`) and monotonic ordering are portable — they
hold on 32-bit too — so unlike the size table in Exercise 1 these assertions need
no platform guard.

Create `layout_test.go`:

```go
package layout

import (
	"fmt"
	"testing"
	"unsafe"
)

func checkInvariants(t *testing.T, name string, fields []FieldInfo, structAlign uintptr) {
	t.Helper()

	var maxAlign uintptr = 1
	var prev uintptr
	for i, f := range fields {
		if f.Align == 0 || f.Align&(f.Align-1) != 0 {
			t.Errorf("%s.%s: align = %d, not a power of two", name, f.Name, f.Align)
		}
		if f.Offset%f.Align != 0 {
			t.Errorf("%s.%s: offset %d not a multiple of align %d (misaligned)", name, f.Name, f.Offset, f.Align)
		}
		if i > 0 && f.Offset <= prev {
			t.Errorf("%s.%s: offset %d not strictly greater than previous %d; Go must not reorder", name, f.Name, f.Offset, prev)
		}
		prev = f.Offset
		if f.Align > maxAlign {
			maxAlign = f.Align
		}
	}
	if structAlign != maxAlign {
		t.Errorf("%s: struct align = %d, want max field align %d", name, structAlign, maxAlign)
	}
}

func TestBadLayout(t *testing.T) {
	t.Parallel()
	checkInvariants(t, "Bad", BadFields(), unsafe.Alignof(Bad{}))
}

func TestGoodLayout(t *testing.T) {
	t.Parallel()
	checkInvariants(t, "Good", GoodFields(), unsafe.Alignof(Good{}))
}

func TestFirstFieldOffsetIsZero(t *testing.T) {
	t.Parallel()
	// The first field always starts at offset 0; this is the honest version of
	// the meaningless "offset >= 0" check.
	if got := BadFields()[0].Offset; got != 0 {
		t.Errorf("Bad first field offset = %d, want 0", got)
	}
	if got := GoodFields()[0].Offset; got != 0 {
		t.Errorf("Good first field offset = %d, want 0", got)
	}
}

func ExampleGoodFields() {
	for _, f := range GoodFields() {
		fmt.Printf("%s@%d\n", f.Name, f.Offset)
	}
	// Output:
	// Score@0
	// Count@8
	// Total@16
	// Letter@20
	// Flag@21
	// Active@22
}
```

## Review

The suite is correct when it asserts something that could actually fail: every
offset is a multiple of its field's alignment, offsets strictly increase in
declaration order, and the struct's alignment is the maximum field alignment. The
one mistake this exercise exists to kill is the tautological `Offsetof >= 0`
check — an unsigned value is never negative, so that assertion is dead weight; the
honest "first field is at offset 0" check replaces it. Because alignment and
ordering are architecture-independent, these tests carry no platform guard, in
contrast to the exact-size table of Exercise 1.

## Resources

- [unsafe.Offsetof / Alignof / Sizeof](https://pkg.go.dev/unsafe#Offsetof) — the three compile-time layout operators.
- [Go spec: Size and alignment guarantees](https://go.dev/ref/spec#Size_and_alignment_guarantees) — why offsets are multiples of alignment.
- [uintptr](https://pkg.go.dev/builtin#uintptr) — the unsigned integer type the operators return, which is why `>= 0` is a tautology.

---

Back to [01-field-ordering-record-types.md](01-field-ordering-record-types.md) | Next: [03-reflect-layout-auditor.md](03-reflect-layout-auditor.md)
