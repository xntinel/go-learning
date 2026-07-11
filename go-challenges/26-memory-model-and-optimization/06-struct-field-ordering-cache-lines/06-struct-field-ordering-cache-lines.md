# 6. Struct Field Ordering and Cache Lines

Struct layout is part API design and part performance engineering. This lesson builds a small layout-reporting library that compares the same logical record in two field orders, validates report requests with sentinel errors, and keeps the measurement code inside tests instead of a print-only program.

```text
layoutcheck/
  go.mod
  layout.go
  layout_test.go
  cmd/demo/main.go
```

## Concepts

### Alignment Creates Padding

Each field has an alignment requirement. Go lays out struct fields in source order and inserts padding when the next field must start at a more aligned address. The struct size is also rounded so arrays and slices of that struct keep every element properly aligned.

### Source Order Is Preserved

The compiler does not reorder fields to save space. That is important because field order can be observable through reflection, struct tags, documentation, and unsafe layout inspection. If you want a smaller layout, you must choose the order yourself.

### Cache Effects Compound In Slices

For one object, a few padding bytes rarely matter. For millions of elements in a slice, object size controls how many elements fit in cache. A smaller hot struct can reduce memory bandwidth and cache misses, but only measurements on the actual workload can prove a speedup.

### Layout Is Not The Only Design Constraint

Do not reorder fields blindly. Public struct fields are part of the API, and related fields may be easier to read when grouped semantically. Optimize layout for internal hot-path structs where memory density matters.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/layoutcheck/cmd/demo
cd ~/go-exercises/layoutcheck
go mod init layoutcheck
```

This is a library package. It exposes reports and leaves unsafe details behind small exported functions.

### Exercise 1: Implement Layout Reports

Create `layout.go`:

```go
package layoutcheck

import (
	"errors"
	"fmt"
	"unsafe"
)

var ErrUnknownLayout = errors.New("unknown layout")

type PoorRecord struct {
	Active  bool
	Balance float64
	Deleted bool
	Age     int32
	ID      int64
	Locked  bool
}

type CompactRecord struct {
	Balance float64
	ID      int64
	Age     int32
	Active  bool
	Deleted bool
	Locked  bool
}

type FieldOffset struct {
	Name   string
	Offset uintptr
}

type Report struct {
	Name    string
	Size    uintptr
	Align   uintptr
	Offsets []FieldOffset
}

func Layout(name string) (Report, error) {
	switch name {
	case "poor":
		var r PoorRecord
		return Report{
			Name:  "poor",
			Size:  unsafe.Sizeof(r),
			Align: unsafe.Alignof(r),
			Offsets: []FieldOffset{
				{Name: "Active", Offset: unsafe.Offsetof(r.Active)},
				{Name: "Balance", Offset: unsafe.Offsetof(r.Balance)},
				{Name: "Deleted", Offset: unsafe.Offsetof(r.Deleted)},
				{Name: "Age", Offset: unsafe.Offsetof(r.Age)},
				{Name: "ID", Offset: unsafe.Offsetof(r.ID)},
				{Name: "Locked", Offset: unsafe.Offsetof(r.Locked)},
			},
		}, nil
	case "compact":
		var r CompactRecord
		return Report{
			Name:  "compact",
			Size:  unsafe.Sizeof(r),
			Align: unsafe.Alignof(r),
			Offsets: []FieldOffset{
				{Name: "Balance", Offset: unsafe.Offsetof(r.Balance)},
				{Name: "ID", Offset: unsafe.Offsetof(r.ID)},
				{Name: "Age", Offset: unsafe.Offsetof(r.Age)},
				{Name: "Active", Offset: unsafe.Offsetof(r.Active)},
				{Name: "Deleted", Offset: unsafe.Offsetof(r.Deleted)},
				{Name: "Locked", Offset: unsafe.Offsetof(r.Locked)},
			},
		}, nil
	default:
		return Report{}, fmt.Errorf("layout %q: %w", name, ErrUnknownLayout)
	}
}

func SavedBytesPerRecord() uintptr {
	return unsafe.Sizeof(PoorRecord{}) - unsafe.Sizeof(CompactRecord{})
}

func EstimatedSavedBytes(records int) (uintptr, error) {
	if records < 0 {
		return 0, fmt.Errorf("records %d: %w", records, ErrUnknownLayout)
	}
	return uintptr(records) * SavedBytesPerRecord(), nil
}
```

`Layout` uses real `unsafe.Sizeof`, `unsafe.Alignof`, and `unsafe.Offsetof` calls. The exported report type keeps the demo and tests away from direct unsafe operations.

### Exercise 2: Test Sizes, Offsets, And Errors

Create `layout_test.go`:

```go
package layoutcheck

import (
	"errors"
	"fmt"
	"testing"
)

func TestLayoutReports(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		wantSize uintptr
		wantName string
	}{
		{name: "poor", wantSize: 40, wantName: "poor"},
		{name: "compact", wantSize: 24, wantName: "compact"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			report, err := Layout(tt.name)
			if err != nil {
				t.Fatalf("Layout() error = %v", err)
			}
			if report.Name != tt.wantName || report.Size != tt.wantSize || report.Align != 8 {
				t.Fatalf("report = %+v", report)
			}
			if len(report.Offsets) != 6 {
				t.Fatalf("offset count = %d, want 6", len(report.Offsets))
			}
		})
	}
}

func TestSavedBytesPerRecord(t *testing.T) {
	t.Parallel()

	if got := SavedBytesPerRecord(); got != 16 {
		t.Fatalf("SavedBytesPerRecord() = %d, want 16", got)
	}
}

func TestEstimatedSavedBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records int
		want    uintptr
	}{
		{name: "none", records: 0, want: 0},
		{name: "many", records: 10, want: 160},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := EstimatedSavedBytes(tt.records)
			if err != nil {
				t.Fatalf("EstimatedSavedBytes() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("EstimatedSavedBytes() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fn   func() error
		want error
	}{
		{name: "unknown layout", fn: func() error { _, err := Layout("missing"); return err }, want: ErrUnknownLayout},
		{name: "negative records", fn: func() error { _, err := EstimatedSavedBytes(-1); return err }, want: ErrUnknownLayout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := tt.fn(); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func ExampleLayout() {
	poor, _ := Layout("poor")
	compact, _ := Layout("compact")
	fmt.Println(poor.Size, compact.Size, SavedBytesPerRecord())
	// Output: 40 24 16
}
```

The expected sizes are for the standard Go implementations on common 64-bit architectures, where `float64` and `int64` align to 8 bytes. If you are targeting unusual architectures, verify the numbers with the tests rather than copying assumptions.

### Exercise 3: Add The Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"layoutcheck"
)

func main() {
	poor, err := layoutcheck.Layout("poor")
	if err != nil {
		log.Fatal(err)
	}
	compact, err := layoutcheck.Layout("compact")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("poor=%d compact=%d saved=%d\n", poor.Size, compact.Size, layoutcheck.SavedBytesPerRecord())
}
```

## Common Mistakes

### Reordering Public Structs Without Considering API Cost

Wrong: changing exported field order in a public type only to save padding.

Fix: use compact internal structs for hot data paths. Public structs should prioritize compatibility and clarity unless the layout is already part of the design.

### Measuring One Value Instead Of A Slice Workload

Wrong: assuming a 16-byte saving matters because a single `unsafe.Sizeof` result is smaller.

Fix: multiply by the number of records and benchmark the actual traversal. The lesson's `EstimatedSavedBytes` makes the multiplication explicit.

### Forgetting That Unsafe Size Excludes Referenced Data

Wrong: treating `unsafe.Sizeof([]byte{})` as the size of the backing array.

Fix: remember that `unsafe.Sizeof` reports the descriptor size for slices, strings, interfaces, maps, and channels, not the memory they refer to.

## Verification

Run this from `~/go-exercises/layoutcheck`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test that checks `Layout("compact")` reports `Balance` at offset `0` and `ID` at offset `8`.

## Summary

- Go preserves struct field order and inserts padding to satisfy alignment.
- `unsafe.Sizeof`, `unsafe.Alignof`, and `unsafe.Offsetof` expose the actual compiled layout.
- Compact field ordering matters most for large slices of hot structs.
- Layout optimization is a trade-off against API stability and readability.

## What's Next

Next: [String Interning](../07-string-interning/07-string-interning.md).

## Resources

- [Go Specification: Size and alignment guarantees](https://go.dev/ref/spec#Size_and_alignment_guarantees)
- [unsafe package](https://pkg.go.dev/unsafe)
- [fieldalignment analyzer](https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/fieldalignment)
