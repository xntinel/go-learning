# 2. unsafe.Sizeof, Alignof, and Offsetof

`unsafe.Sizeof`, `unsafe.Alignof`, and `unsafe.Offsetof` let a Go package inspect the exact layout that the compiler chose for a type. This lesson builds a layout package that compares two record shapes and proves where padding appears.

## Concepts

### Size Is Not The Sum Of Fields

Struct fields are placed at offsets that satisfy each field's alignment. A `bool` before an `int64` usually creates padding because the `int64` must start on an aligned address.

### Offsets Are Compile-Time Facts

`unsafe.Offsetof(x.Field)` reports the byte offset of a field. That is useful for binary formats and C interop, but it binds the code to a concrete Go type layout.

### Reordering Can Save Memory

Grouping wider fields first often reduces padding. The correct lesson is not "always reorder"; it is "measure layout before depending on it".

## Exercises

### Exercise 1: Describe Two Struct Layouts

Create `layout.go`:

```go
package layout

import (
	"errors"
	"fmt"
	"unsafe"
)

var ErrUnknownLayout = errors.New("unknown layout")

type Wasteful struct {
	Flag bool
	ID   int64
	Code int16
}

type Packed struct {
	ID   int64
	Code int16
	Flag bool
}

type Report struct {
	Name      string
	Size      uintptr
	Align     uintptr
	Offsets   map[string]uintptr
	FieldSize uintptr
}

func Describe(name string) (Report, error) {
	switch name {
	case "wasteful":
		var v Wasteful
		return Report{
			Name:      name,
			Size:      unsafe.Sizeof(v),
			Align:     unsafe.Alignof(v),
			FieldSize: unsafe.Sizeof(v.Flag) + unsafe.Sizeof(v.ID) + unsafe.Sizeof(v.Code),
			Offsets: map[string]uintptr{
				"Flag": unsafe.Offsetof(v.Flag),
				"ID":   unsafe.Offsetof(v.ID),
				"Code": unsafe.Offsetof(v.Code),
			},
		}, nil
	case "packed":
		var v Packed
		return Report{
			Name:      name,
			Size:      unsafe.Sizeof(v),
			Align:     unsafe.Alignof(v),
			FieldSize: unsafe.Sizeof(v.Flag) + unsafe.Sizeof(v.ID) + unsafe.Sizeof(v.Code),
			Offsets: map[string]uintptr{
				"ID":   unsafe.Offsetof(v.ID),
				"Code": unsafe.Offsetof(v.Code),
				"Flag": unsafe.Offsetof(v.Flag),
			},
		}, nil
	default:
		return Report{}, fmt.Errorf("%w: %s", ErrUnknownLayout, name)
	}
}

func PaddingBytes(r Report) uintptr {
	return r.Size - r.FieldSize
}
```

### Exercise 2: Add Example And Demo

Create `example_test.go`:

```go
package layout

import "fmt"

func ExampleDescribe() {
	r, _ := Describe("packed")
	fmt.Println(r.Offsets["ID"], r.Offsets["Code"], r.Offsets["Flag"])
	// Output: 0 8 10
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/layout"
)

func main() {
	r, err := layout.Describe("wasteful")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s size=%d padding=%d\n", r.Name, r.Size, layout.PaddingBytes(r))
}
```

### Exercise 3: Test Layout Contracts

Create `layout_test.go`:

```go
package layout

import (
	"errors"
	"testing"
)

func TestDescribe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		wantID  uintptr
		wantErr error
	}{
		{name: "wasteful", wantID: 8},
		{name: "packed", wantID: 0},
		{name: "missing", wantErr: ErrUnknownLayout},
	}

	for _, tt := range tests {
		r, err := Describe(tt.name)
		if !errors.Is(err, tt.wantErr) {
			t.Fatalf("%s: err = %v, want %v", tt.name, err, tt.wantErr)
		}
		if tt.wantErr != nil {
			continue
		}
		if r.Offsets["ID"] != tt.wantID {
			t.Fatalf("%s: ID offset = %d, want %d", tt.name, r.Offsets["ID"], tt.wantID)
		}
	}
}

func TestPackedUsesLessPadding(t *testing.T) {
	t.Parallel()

	w, err := Describe("wasteful")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Describe("packed")
	if err != nil {
		t.Fatal(err)
	}
	if PaddingBytes(p) >= PaddingBytes(w) {
		t.Fatalf("packed padding = %d, wasteful padding = %d", PaddingBytes(p), PaddingBytes(w))
	}
}
```

## Common Mistakes

### Assuming Field Sizes Add Up To Struct Size

Wrong: calculate `1 + 8 + 2` and assume the struct is 11 bytes.

Fix: measure `unsafe.Sizeof` and account for padding.

### Depending On Layout Without Tests

Wrong: write binary data using guessed offsets.

Fix: pin important offsets with tests like `TestDescribe`.

### Treating Reordering As Always Safe

Wrong: reorder exported struct fields in a public API without checking users.

Fix: use reordering for internal structs or version the binary format explicitly.

## Verification

Run this from `~/go-exercises/layout`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add a test that verifies `packed` has `Flag` after `Code`.

## Summary

- `Sizeof` reports the complete value size, including padding.
- `Alignof` reports the alignment requirement.
- `Offsetof` reports field positions inside a struct.
- Reordering fields can reduce padding, but only when the layout is not an external contract.

## What's Next

Next: [Type Punning](../03-type-punning/03-type-punning.md).

## Resources

- [unsafe package](https://pkg.go.dev/unsafe)
- [Go Specification: Size and alignment guarantees](https://go.dev/ref/spec#Size_and_alignment_guarantees)
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types)
