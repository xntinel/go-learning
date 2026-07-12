# Exercise 2: Detecting Shared Backing Arrays in a Debug Endpoint

When a request-batching layer produces corrupted output — one batch's bytes
bleeding into the next — the question you need answered is precise: do these two
slices share a backing array? This exercise builds the diagnostic that answers
it, reporting a slice header (pointer identity, length, capacity) and detecting
aliasing between two slices, exactly the tool you would wire behind an internal
`/debug/slices` handler.

This module is fully self-contained: its own `go mod init`, all code inline,
its own demo and tests.

## What you'll build

```text
slicediag/                  independent module: example.com/slicediag
  go.mod
  slicediag.go              type Header; HeaderOf, SharesBacking (unsafe.SliceData)
  cmd/
    demo/
      main.go               runnable demo: report headers, detect aliasing
  slicediag_test.go         sub-slice shares; clone does not; reslice-past-len; nil/empty
```

Files: `slicediag.go`, `cmd/demo/main.go`, `slicediag_test.go`.
Implement: `HeaderOf[E any](s []E) Header` returning `{Data, Len, Cap}`, and `SharesBacking[E any](a, b []E) bool` using `unsafe.SliceData`.
Test: `SharesBacking(s, s[1:3])` is true; against `slices.Clone(s)` is false; a reslice past len is still detected; nil and empty are handled.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/07-slice-internals/02-slice-header-diagnostics/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/07-slice-internals/02-slice-header-diagnostics
```

### Reading the header with unsafe.SliceData

`unsafe.SliceData(s)` returns a pointer to the first element of the backing
array. Its contract has three cases you must handle: for a `nil` slice it returns
`nil`; for a slice with `cap > 0` it returns a pointer to element zero of the
array; for a non-nil slice with `cap == 0` it returns a non-nil pointer to an
unspecified address. We convert that pointer to a `uintptr` purely to *report* and
*compare* addresses — never to convert back to a pointer, which would be an
unsafe-pointer violation. Storing an address as a number for a diagnostic is
fine; the GC does not currently move heap objects, and we never dereference the
number.

`SharesBacking` cannot just compare the two `SliceData` pointers for equality:
`s` and `s[1:3]` share the same array but their first-element pointers differ by
one element. The correct test is **range overlap**. Compute each slice's byte
range `[start, start+cap*elemSize)` over its backing array and check whether the
ranges overlap. This catches a sub-slice (its start lies inside the parent's
range) and a reslice past the original length that still points into the same
array (its capacity extends across the shared region). A `slices.Clone` produces
a disjoint array, so its range does not overlap and the function returns false.

Empty and nil slices are handled up front: a slice with `cap == 0` owns no array
region, so it cannot share one — return false without touching `unsafe`.

Create `slicediag.go`:

```go
package slicediag

import "unsafe"

// Header is a reportable snapshot of a slice's three-word header: the address
// of its backing array, its length, and its capacity. Data is 0 for a nil
// slice. It is meant for diagnostics, not for reconstructing a slice.
type Header struct {
	Data uintptr
	Len  int
	Cap  int
}

// HeaderOf reports the header of s. For a nil slice, Data is 0.
func HeaderOf[E any](s []E) Header {
	return Header{
		Data: uintptr(unsafe.Pointer(unsafe.SliceData(s))),
		Len:  len(s),
		Cap:  cap(s),
	}
}

// SharesBacking reports whether a and b are views into the same backing array,
// i.e. whether a write through one could be visible through the other. It is
// true for a sub-slice or a reslice-past-len, and false for slices with
// independent arrays (such as a clone) and for any slice with zero capacity.
func SharesBacking[E any](a, b []E) bool {
	if cap(a) == 0 || cap(b) == 0 {
		return false
	}
	var zero E
	size := unsafe.Sizeof(zero)
	aStart := uintptr(unsafe.Pointer(unsafe.SliceData(a)))
	bStart := uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	aEnd := aStart + uintptr(cap(a))*size
	bEnd := bStart + uintptr(cap(b))*size
	// Two half-open ranges overlap iff each starts before the other ends.
	return aStart < bEnd && bStart < aEnd
}
```

### The runnable demo

The demo builds a batch buffer, carves out a sub-view, clones it, and prints
what shares storage with what.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"

	"example.com/slicediag"
)

func main() {
	batch := make([]byte, 4, 16) // len 4, cap 16
	view := batch[1:3]           // sub-slice, same array
	clone := slices.Clone(batch) // independent array

	h := slicediag.HeaderOf(batch)
	fmt.Printf("batch header: len=%d cap=%d\n", h.Len, h.Cap)
	fmt.Printf("view shares batch: %v\n", slicediag.SharesBacking(batch, view))
	fmt.Printf("clone shares batch: %v\n", slicediag.SharesBacking(batch, clone))

	past := batch[1:3:16] // reslice whose cap runs past the original len
	fmt.Printf("reslice-past-len shares batch: %v\n", slicediag.SharesBacking(batch, past))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
batch header: len=4 cap=16
view shares batch: true
clone shares batch: false
reslice-past-len shares batch: true
```

### Tests

Create `slicediag_test.go`:

```go
package slicediag

import (
	"fmt"
	"slices"
	"testing"
)

func TestSharesBackingSubSlice(t *testing.T) {
	t.Parallel()
	s := []int{1, 2, 3, 4, 5}
	if !SharesBacking(s, s[1:3]) {
		t.Fatal("a sub-slice must share the backing array")
	}
}

func TestSharesBackingClone(t *testing.T) {
	t.Parallel()
	s := []int{1, 2, 3, 4, 5}
	if SharesBacking(s, slices.Clone(s)) {
		t.Fatal("a clone must not share the backing array")
	}
}

func TestSharesBackingReslicePastLen(t *testing.T) {
	t.Parallel()
	s := make([]int, 2, 8)
	past := s[1:2:8] // cap extends across the shared array beyond original len
	if !SharesBacking(s, past) {
		t.Fatal("a reslice past len still points into the same array")
	}
}

func TestSharesBackingNilAndEmpty(t *testing.T) {
	t.Parallel()
	var nilSlice []int
	empty := []int{}
	s := []int{1, 2, 3}
	if SharesBacking(nilSlice, s) {
		t.Fatal("nil slice shares nothing")
	}
	if SharesBacking(empty, s) {
		t.Fatal("zero-capacity slice shares nothing")
	}
}

func TestHeaderNilHasZeroData(t *testing.T) {
	t.Parallel()
	var nilSlice []int
	if h := HeaderOf(nilSlice); h.Data != 0 || h.Len != 0 || h.Cap != 0 {
		t.Fatalf("HeaderOf(nil) = %+v, want all zero", h)
	}
}

func TestHeaderReportsLenCap(t *testing.T) {
	t.Parallel()
	s := make([]byte, 3, 9)
	if h := HeaderOf(s); h.Len != 3 || h.Cap != 9 {
		t.Fatalf("HeaderOf = len %d cap %d, want len 3 cap 9", h.Len, h.Cap)
	}
	if HeaderOf(s).Data == 0 {
		t.Fatal("a non-empty slice must have a non-zero backing address")
	}
}

func ExampleSharesBacking() {
	s := []int{1, 2, 3, 4}
	fmt.Println(SharesBacking(s, s[2:4]), SharesBacking(s, slices.Clone(s)))
	// Output: true false
}
```

## Review

The diagnostic is correct when `SharesBacking` uses **range overlap**, not
pointer equality: `s` and `s[1:3]` share an array yet have different first-element
pointers, so an equality test would wrongly report false. Overlap catches both a
sub-slice and a reslice whose capacity runs past the original length, while a
`slices.Clone` (a fresh array) correctly reports false. Guard `cap == 0` first so
nil and empty slices — which own no array region — short-circuit before any
`unsafe` call. Treat the `uintptr` addresses as report/compare-only values; never
convert one back to a pointer. Run `go test -race` to confirm the diagnostic is
read-only and race-free.

## Resources

- [pkg.go.dev: unsafe.SliceData](https://pkg.go.dev/unsafe#SliceData) — the exact nil/`cap==0`/`cap>0` contract used here.
- [pkg.go.dev: unsafe.Pointer](https://pkg.go.dev/unsafe#Pointer) — the valid conversions between pointers and `uintptr`.
- [Go blog: Go Slices: usage and internals](https://go.dev/blog/slices-intro) — why sub-slices share storage.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-append-realloc-metrics.md](03-append-realloc-metrics.md)
