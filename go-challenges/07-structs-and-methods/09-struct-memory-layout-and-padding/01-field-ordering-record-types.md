# Exercise 1: Field ordering — the poorly-ordered vs well-ordered record

The same six logical fields, declared in two orders, produce two different
memory footprints because Go does not reorder struct fields for you. This module
builds a `record` package exposing a deliberately bad layout and a hand-optimized
good one, and proves with `unsafe.Sizeof` that ordering alone shrinks the record.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test. Nothing here imports another exercise.

## What you'll build

```text
record/                    independent module: example.com/record
  go.mod                   go 1.26
  record.go                type Bad, type Good (same fields, different order)
  cmd/
    demo/
      main.go              prints Sizeof(Bad) and Sizeof(Good) and the delta
  record_test.go           asserts Sizeof(Good) <= Sizeof(Bad); pins amd64/arm64 sizes
```

- Files: `record.go`, `cmd/demo/main.go`, `record_test.go`.
- Implement: two structs, `Bad` (small fields interleaved with large ones) and `Good` (fields ordered largest-alignment-first), modeling the same logical record.
- Test: table-driven `unsafe.Sizeof` comparison; the anchor assertion is `Good <= Bad`, with a comment documenting the exact 64-bit sizes.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/09-struct-memory-layout-and-padding/01-field-ordering-record-types/cmd/demo
cd go-solutions/07-structs-and-methods/09-struct-memory-layout-and-padding/01-field-ordering-record-types
```

### Why interleaving small fields between large ones costs bytes

`Bad` and `Good` carry identical data: a `float64` score, an `int64` count, an
`int32` total, a `byte` letter, and two `bool` flags. They differ only in
declaration order, and because declaration order *is* memory order in Go, that
difference is the whole story.

Walk `Bad` byte by byte on a 64-bit platform. `Flag bool` takes offset 0. The
next field is `Count int64`, which must sit on a multiple of 8, so it lands at
offset 8 — bytes 1..7 are dead padding inserted only to align `Count`. `Letter
byte` takes offset 16. `Score float64` again needs an 8-multiple, so it goes at
offset 24 — bytes 17..23 are padding. `Active bool` takes 32, `Total int32`
needs a 4-multiple so it lands at 36 (bytes 33..35 padding), ending at 40. The
struct's alignment is 8 and 40 is already a multiple of 8, so `Bad` is 40 bytes
for 23 bytes of actual data.

Now `Good`, ordered largest-alignment-first. `Score` at 0, `Count` at 8, `Total`
at 16, `Letter` at 20, `Flag` at 21, `Active` at 22 — the two `bool`s and the
`byte` pack tightly into the tail. End of data is offset 23; trailing padding to
the next multiple of 8 makes `Good` 24 bytes. Same data, 24 versus 40: a 40%
reduction from ordering alone. The rule the good layout follows is mechanical:
put the 8-aligned fields first, then 4-aligned, then the 1-aligned bytes and
bools last, so every field falls naturally onto its boundary and the only
padding left is a little at the very end.

Create `record.go`:

```go
// Package record demonstrates that Go lays out struct fields in declaration
// order and never reorders them, so field ordering is the programmer's lever
// for minimizing padding.
package record

// Bad is a poorly-ordered layout: 1-byte fields (bool, byte) are interleaved
// between 8-byte fields (int64, float64), forcing the compiler to insert
// interior padding before each 8-aligned field. On a 64-bit platform the
// bytes fall as: Flag@0, [pad 1..7], Count@8, Letter@16, [pad 17..23],
// Score@24, Active@32, [pad 33..35], Total@36, giving 40 bytes total.
type Bad struct {
	Flag   bool
	Count  int64
	Letter byte
	Score  float64
	Active bool
	Total  int32
}

// Good is the same logical record ordered largest-alignment-first, so every
// field lands on its natural boundary with no interior padding. On a 64-bit
// platform: Score@0, Count@8, Total@16, Letter@20, Flag@21, Active@22, then a
// single byte of trailing padding to reach a multiple of 8, giving 24 bytes.
type Good struct {
	Score  float64
	Count  int64
	Total  int32
	Letter byte
	Flag   bool
	Active bool
}
```

### The runnable demo

The demo prints both sizes and the delta so you can watch ordering pay off.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"unsafe"

	"example.com/record"
)

func main() {
	bad := unsafe.Sizeof(record.Bad{})
	good := unsafe.Sizeof(record.Good{})
	fmt.Printf("Bad  = %d bytes\n", bad)
	fmt.Printf("Good = %d bytes\n", good)
	fmt.Printf("saved %d bytes per record by ordering alone\n", bad-good)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (on a 64-bit platform):

```
Bad  = 40 bytes
Good = 24 bytes
saved 16 bytes per record by ordering alone
```

### Tests

The anchor test asserts the portable contract — `Good` is never larger than
`Bad` — and, in the same table, documents the exact sizes we computed. The exact
numbers are commented as platform-dependent: they hold on amd64 and arm64 (both
64-bit); a 32-bit platform would shrink the 8-aligned fields and shift the
numbers, but the `Good <= Bad` relationship is invariant.

Create `record_test.go`:

```go
package record

import (
	"fmt"
	"testing"
	"unsafe"
)

func TestGoodIsNoLargerThanBad(t *testing.T) {
	t.Parallel()

	bad := unsafe.Sizeof(Bad{})
	good := unsafe.Sizeof(Good{})
	if good > bad {
		t.Fatalf("Sizeof(Good) = %d > Sizeof(Bad) = %d; good ordering must not be larger", good, bad)
	}
}

func TestDocumentedSizes(t *testing.T) {
	t.Parallel()

	// Exact sizes on a 64-bit platform (amd64, arm64). Documented, not a
	// portable contract: a 32-bit target would differ, but Good <= Bad holds
	// everywhere. Guard so this table only asserts on 64-bit builds.
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("documented sizes are for 64-bit platforms")
	}

	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"Bad", unsafe.Sizeof(Bad{}), 40},
		{"Good", unsafe.Sizeof(Good{}), 24},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("Sizeof(%s) = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}

func ExampleBad() {
	// Same six fields, two orderings, two footprints.
	fmt.Println(unsafe.Sizeof(Bad{}) >= unsafe.Sizeof(Good{}))
	// Output: true
}
```

## Review

The record is correct when the two types carry the same fields and `Good` is
provably no larger than `Bad`; on a 64-bit platform the reduction is 40 to 24
bytes, sixteen bytes saved per value with zero change to the data. The mistake to
avoid is thinking the compiler would have packed `Bad` for you — it will not, so
the `Good` ordering is something you produce by hand or with `fieldalignment`.
Remember the sizes are platform-dependent; assert the *relationship*
(`Good <= Bad`) as the portable contract and treat the exact byte counts as
documented facts about 64-bit layout, which is why the size table skips on a
32-bit build.

## Resources

- [Go spec: Size and alignment guarantees](https://go.dev/ref/spec#Size_and_alignment_guarantees) — the alignment rules Go guarantees.
- [unsafe.Sizeof](https://pkg.go.dev/unsafe#Sizeof) — the compile-time size operator used here.
- [Go data structures](https://research.swtch.com/godata) — Russ Cox on how Go lays values out in memory.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-layout-inspection-suite.md](02-layout-inspection-suite.md)
