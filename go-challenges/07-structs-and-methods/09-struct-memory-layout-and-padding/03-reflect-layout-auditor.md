# Exercise 3: A reflect-based struct layout auditor that flags wasted padding

The `unsafe` operators only work on statically-named fields. To audit an
*arbitrary* struct at runtime — the way `golang.org/x/tools`'s `fieldalignment`
analyzer does in CI — you use `reflect`. This module builds `AuditLayout`, a
diagnostic that walks any struct type, sums its interior and trailing padding,
and proposes a largest-to-smallest reordering with the size it would achieve.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test.

## What you'll build

```text
layoutaudit/               independent module: example.com/layoutaudit
  go.mod                   go 1.26
  audit.go                 AuditLayout(reflect.Type) (Report, error); ErrNotStruct
  cmd/
    demo/
      main.go              audits a bad struct and prints the report + suggestion
  audit_test.go            Bad padding > Good; OptimalSize(Bad) == Sizeof(Good); error path
```

- Files: `audit.go`, `cmd/demo/main.go`, `audit_test.go`.
- Implement: `AuditLayout(t reflect.Type) (Report, error)` computing per-field offset/size/align, total wasted padding, a suggested largest-alignment-first field order, and the size that order would achieve. Non-struct input returns `ErrNotStruct`.
- Test: assert Bad reports strictly more padding than Good, that Bad's suggested-optimal size equals `Sizeof(Good)`, that a naturally-packed struct reports zero wasted padding, and that a non-struct returns `ErrNotStruct` via `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/layoutaudit/cmd/demo
cd ~/go-exercises/layoutaudit
go mod init example.com/layoutaudit
```

### How the auditor computes wasted padding

`reflect` is the runtime mirror of `unsafe`. `reflect.TypeFor[T]()` yields the
`reflect.Type`; `t.NumField()` and `t.Field(i)` walk the fields;
`StructField.Offset` is the field's byte offset; `f.Type.Size()` and
`f.Type.Align()` are its size and alignment; and `t.Size()` / `t.Align()` are the
whole struct's. That is everything needed to measure padding.

Padding *after* a field is the gap between where that field ends
(`offset + size`) and where the next field begins — or, for the last field, the
struct's total size (that final gap is the trailing padding). Summing those gaps
over all fields gives the total wasted bytes. A struct whose fields already tile
without gaps reports zero.

The suggested layout is the greedy optimum for a no-reorder language: sort the
fields by alignment descending (stably, so equal-alignment fields keep their
relative order), then simulate placement — round the running offset up to each
field's alignment, add its size — and round the final offset up to the struct's
alignment (the maximum field alignment). For the interleaved `Bad` record this
produces exactly the `Good` layout's size, which is the auditor's headline
suggestion: "reorder these fields and this type shrinks from 40 to 24 bytes."

Guarding the input matters: `Offset`, `NumField`, and `Field` panic if called on
a non-struct `reflect.Type`, so `AuditLayout` checks `t.Kind() == reflect.Struct`
first and returns a wrapped sentinel `ErrNotStruct` otherwise, which callers can
match with `errors.Is`.

Create `audit.go`:

```go
// Package layoutaudit is a hand-built analog of the fieldalignment analyzer: it
// reports the wasted padding in a struct type and suggests a tighter field order.
package layoutaudit

import (
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"slices"
)

// ErrNotStruct is returned by AuditLayout when the type is not a struct.
var ErrNotStruct = errors.New("layoutaudit: type is not a struct")

// FieldReport describes one field's placement and the padding that follows it.
type FieldReport struct {
	Name     string
	Offset   uintptr
	Size     uintptr
	Align    uintptr
	PadAfter uintptr // bytes of padding between this field's end and the next field (or struct end)
}

// Report is the full layout audit of a struct type.
type Report struct {
	Type          string
	Size          uintptr
	Align         uintptr
	Fields        []FieldReport
	WastedPadding uintptr  // total interior + trailing padding
	Suggested     []string // field names ordered largest-alignment-first
	OptimalSize   uintptr  // size achievable with the suggested order
}

// alignUp rounds off up to the next multiple of align (a power of two).
func alignUp(off, align uintptr) uintptr {
	if align == 0 {
		return off
	}
	return (off + align - 1) &^ (align - 1)
}

// AuditLayout reports the layout of a struct type: per-field padding, total
// wasted bytes, and a suggested largest-alignment-first reordering with the size
// it would achieve. It returns ErrNotStruct for a non-struct type.
func AuditLayout(t reflect.Type) (Report, error) {
	if t.Kind() != reflect.Struct {
		return Report{}, fmt.Errorf("audit %s: %w", t.Kind(), ErrNotStruct)
	}

	rep := Report{Type: t.String(), Size: t.Size(), Align: uintptr(t.Align())}
	n := t.NumField()
	for i := range n {
		f := t.Field(i)
		next := t.Size()
		if i+1 < n {
			next = t.Field(i + 1).Offset
		}
		size := f.Type.Size()
		pad := next - (f.Offset + size)
		rep.Fields = append(rep.Fields, FieldReport{
			Name:     f.Name,
			Offset:   f.Offset,
			Size:     size,
			Align:    uintptr(f.Type.Align()),
			PadAfter: pad,
		})
		rep.WastedPadding += pad
	}

	// Simulate the greedy largest-alignment-first layout.
	opt := slices.Clone(rep.Fields)
	slices.SortStableFunc(opt, func(a, b FieldReport) int {
		return cmp.Compare(b.Align, a.Align) // descending by alignment
	})
	var off, maxAlign uintptr = 0, 1
	for _, f := range opt {
		if f.Align > maxAlign {
			maxAlign = f.Align
		}
		off = alignUp(off, f.Align)
		off += f.Size
		rep.Suggested = append(rep.Suggested, f.Name)
	}
	rep.OptimalSize = alignUp(off, maxAlign)
	return rep, nil
}
```

### The runnable demo

The demo audits an interleaved struct and prints the field-by-field padding, the
total waste, and the suggested reordering.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"reflect"
	"strings"

	"example.com/layoutaudit"
)

type Bad struct {
	Flag   bool
	Count  int64
	Letter byte
	Score  float64
	Active bool
	Total  int32
}

func main() {
	rep, err := layoutaudit.AuditLayout(reflect.TypeFor[Bad]())
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s: size=%d align=%d wasted=%d bytes\n", rep.Type, rep.Size, rep.Align, rep.WastedPadding)
	for _, f := range rep.Fields {
		fmt.Printf("  %-7s offset=%2d size=%d padAfter=%d\n", f.Name, f.Offset, f.Size, f.PadAfter)
	}
	fmt.Printf("suggested order: %s\n", strings.Join(rep.Suggested, ", "))
	fmt.Printf("optimal size: %d bytes (saves %d)\n", rep.OptimalSize, rep.Size-rep.OptimalSize)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (on a 64-bit platform):

```
main.Bad: size=40 align=8 wasted=17 bytes
  Flag    offset= 0 size=1 padAfter=7
  Count   offset= 8 size=8 padAfter=0
  Letter  offset=16 size=1 padAfter=7
  Score   offset=24 size=8 padAfter=0
  Active  offset=32 size=1 padAfter=3
  Total   offset=36 size=4 padAfter=0
suggested order: Count, Score, Total, Flag, Letter, Active
optimal size: 24 bytes (saves 16)
```

### Tests

The tests feed the auditor `Bad`, `Good`, and a naturally-packed struct, and
assert the three headline properties plus the error path.

Create `audit_test.go`:

```go
package layoutaudit

import (
	"errors"
	"reflect"
	"testing"
	"unsafe"
)

type Bad struct {
	Flag   bool
	Count  int64
	Letter byte
	Score  float64
	Active bool
	Total  int32
}

type Good struct {
	Score  float64
	Count  int64
	Total  int32
	Letter byte
	Flag   bool
	Active bool
}

// Packed already tiles without gaps: two 8-byte fields, zero padding.
type Packed struct {
	A uint64
	B uint64
}

func TestBadWastesMoreThanGood(t *testing.T) {
	t.Parallel()

	bad, err := AuditLayout(reflect.TypeFor[Bad]())
	if err != nil {
		t.Fatalf("AuditLayout(Bad): %v", err)
	}
	good, err := AuditLayout(reflect.TypeFor[Good]())
	if err != nil {
		t.Fatalf("AuditLayout(Good): %v", err)
	}
	if bad.WastedPadding <= good.WastedPadding {
		t.Errorf("wasted: Bad=%d Good=%d; Bad must waste strictly more", bad.WastedPadding, good.WastedPadding)
	}
}

func TestSuggestedOptimalEqualsGoodSize(t *testing.T) {
	t.Parallel()

	bad, err := AuditLayout(reflect.TypeFor[Bad]())
	if err != nil {
		t.Fatalf("AuditLayout(Bad): %v", err)
	}
	if want := unsafe.Sizeof(Good{}); bad.OptimalSize != want {
		t.Errorf("OptimalSize(Bad) = %d, want Sizeof(Good) = %d", bad.OptimalSize, want)
	}
	if bad.OptimalSize >= bad.Size {
		t.Errorf("OptimalSize(Bad) = %d not smaller than actual size %d", bad.OptimalSize, bad.Size)
	}
}

func TestAlreadyOptimalHasNoInteriorGaps(t *testing.T) {
	t.Parallel()

	rep, err := AuditLayout(reflect.TypeFor[Packed]())
	if err != nil {
		t.Fatalf("AuditLayout(Packed): %v", err)
	}
	if rep.WastedPadding != 0 {
		t.Errorf("Packed wasted padding = %d, want 0", rep.WastedPadding)
	}
	// A Good already-good struct's reordering does not shrink it.
	good, err := AuditLayout(reflect.TypeFor[Good]())
	if err != nil {
		t.Fatalf("AuditLayout(Good): %v", err)
	}
	if good.OptimalSize != good.Size {
		t.Errorf("Good OptimalSize = %d, want unchanged %d", good.OptimalSize, good.Size)
	}
}

func TestNonStructReturnsSentinel(t *testing.T) {
	t.Parallel()

	_, err := AuditLayout(reflect.TypeFor[int]())
	if !errors.Is(err, ErrNotStruct) {
		t.Fatalf("AuditLayout(int) error = %v, want ErrNotStruct", err)
	}
}
```

## Review

The auditor is correct when its wasted-padding figure equals the sum of interior
gaps plus trailing padding, and its suggested reordering reproduces the greedy
largest-first optimum — which for `Bad` lands exactly on `Good`'s 24-byte size.
The trap it guards against is calling `NumField`/`Offset` on a non-struct type,
which panics; `AuditLayout` returns a wrapped `ErrNotStruct` instead, matched with
`errors.Is`. This is the same computation `fieldalignment` performs; running that
analyzer in CI is the production version of this exercise.

## Resources

- [reflect.Type](https://pkg.go.dev/reflect#Type) — `Size`, `Align`, `NumField`, `Field`, `FieldAlign`.
- [reflect.StructField](https://pkg.go.dev/reflect#StructField) — the `Offset` field the auditor reads.
- [fieldalignment analyzer](https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/fieldalignment) — the production tool this module reimplements.

---

Back to [02-layout-inspection-suite.md](02-layout-inspection-suite.md) | Next: [04-size-regression-gate.md](04-size-regression-gate.md)
