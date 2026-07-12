# Exercise 5: Storing &slice[i] Then Appending — Dangling Pointers After Reallocation

An ingest buffer that indexes `*Sample` into a growing `[]Sample` backing array is
a time bomb: the first `append` past capacity reallocates, and every `&buf[i]` you
captured before the grow now points at the orphaned old array. This module makes
the divergence executable, then builds two designs that survive growth.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
ingestbuf/                    independent module: example.com/ingestbuf
  go.mod                      go 1.24
  buffer.go                   Sample; ReservedBuffer (slices.Grow) and PointerBuffer ([]*Sample)
  buffer_test.go              proves append invalidates interior pointers; reserved & pointer designs stay valid
  cmd/demo/main.go            runnable demo showing capacity growth and pointer divergence
```

Files: `buffer.go`, `buffer_test.go`, `cmd/demo/main.go`.
Implement: a hazard demonstration capturing `&buf[0]` before a reallocation; a
`ReservedBuffer` that `slices.Grow`s up front so interior pointers stay valid; a
`PointerBuffer` storing `[]*Sample` with independently allocated elements.
Test: captured interior pointer does NOT observe a post-reallocation write;
reserved-capacity pointers stay valid; the `[]*Sample` design survives growth.
Verify: `go test -count=1 -race ./...`

### Why the interior pointer goes stale

A slice is a header `{ptr, len, cap}` over a backing array. `&buf[i]` is a pointer
*into* that backing array. As long as the array stays put, the pointer is valid.
But `append` past `cap` cannot grow the existing array in place — it allocates a
new, larger array, copies the elements over, and returns a header pointing at the
new array. Your captured `&buf[i]` still points at the *old* array, which is now
unreferenced by the live slice. From that moment the two diverge: the live buffer
mutates the new array, the stale pointer reads the old one, and neither sees the
other's writes. There is no error, no nil, no panic — just two copies drifting
apart. This is precisely the bug in an aggregation buffer that does
`idx[key] = &buf[len(buf)-1]` and then keeps appending.

Two robust designs. First, reserve capacity before taking any interior pointer:
`buf = slices.Grow(buf, n)` guarantees room for `n` more elements without
reallocating, so appends within that reserve keep the same backing array and the
interior pointers stay valid. (`slices.Grow(s, n)` returns a slice with
`cap >= len(s)+n`; if the current cap already suffices it is a no-op.) You must
still not exceed the reserve. Second, sidestep interior pointers entirely: store
`[]*Sample` where each `*Sample` is independently heap-allocated
(`s := &Sample{...}; buf = append(buf, s)`). Growing the outer `[]*Sample` copies
*pointers*, never the `Sample` objects, so every `*Sample` a caller holds stays
valid no matter how the slice grows. The pointer-slice design is the safer default
for anything you index into.

Create `buffer.go`:

```go
package ingestbuf

import "slices"

type Sample struct {
	Key   string
	Value int64
}

// growPastCap appends n samples to buf, forcing at least one reallocation when
// buf starts full. It returns the grown slice and the capacity it started with.
func growPastCap(buf []Sample, n int) ([]Sample, int) {
	start := cap(buf)
	for i := range n {
		buf = append(buf, Sample{Key: "k", Value: int64(i)})
	}
	return buf, start
}

// ReservedBuffer reserves capacity up front with slices.Grow so interior
// pointers taken after Reserve remain valid across appends within the reserve.
type ReservedBuffer struct {
	buf []Sample
}

func NewReservedBuffer(reserve int) *ReservedBuffer {
	return &ReservedBuffer{buf: slices.Grow([]Sample(nil), reserve)}
}

// Append adds a sample and returns a stable interior pointer to it. Valid only
// while appends stay within the reserved capacity.
func (b *ReservedBuffer) Append(s Sample) *Sample {
	b.buf = append(b.buf, s)
	return &b.buf[len(b.buf)-1]
}

func (b *ReservedBuffer) Cap() int { return cap(b.buf) }

// PointerBuffer stores independently allocated *Sample. Growth copies pointers,
// never Sample objects, so held pointers never dangle.
type PointerBuffer struct {
	buf []*Sample
}

func NewPointerBuffer() *PointerBuffer { return &PointerBuffer{} }

// Append allocates a fresh Sample and returns its pointer.
func (b *PointerBuffer) Append(s Sample) *Sample {
	p := &s // fresh allocation, independent of the slice's backing array
	b.buf = append(b.buf, p)
	return p
}

func (b *PointerBuffer) At(i int) *Sample { return b.buf[i] }
func (b *PointerBuffer) Len() int         { return len(b.buf) }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ingestbuf"
)

func main() {
	// The pointer-slice design: held pointers survive growth.
	pb := ingestbuf.NewPointerBuffer()
	first := pb.Append(ingestbuf.Sample{Key: "cpu", Value: 1})
	for i := range 1000 {
		pb.Append(ingestbuf.Sample{Key: "fill", Value: int64(i)})
	}
	first.Value = 99 // mutate through the held pointer
	fmt.Printf("pointer buffer: first.Value=%d at(0).Value=%d len=%d\n",
		first.Value, pb.At(0).Value, pb.Len())

	// The reserved-capacity design: capacity is stable, pointers valid.
	rb := ingestbuf.NewReservedBuffer(4)
	p := rb.Append(ingestbuf.Sample{Key: "mem", Value: 10})
	rb.Append(ingestbuf.Sample{Key: "mem", Value: 20})
	p.Value = 11
	fmt.Printf("reserved buffer: cap=%d p.Value=%d\n", rb.Cap(), p.Value)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pointer buffer: first.Value=99 at(0).Value=99 len=1001
reserved buffer: cap=4 p.Value=11
```

### Tests

`TestAppendInvalidatesInteriorPointers` is the hazard proof: capture `&buf[0]`,
append enough to force reallocation (asserting `cap` changed), mutate through
`buf[0]`, and assert the captured pointer did *not* observe it — the pointers now
reference different arrays. `TestReservedCapacityKeepsPointersStable` grows up
front and appends within the reserve, asserting a held pointer still tracks the
live element. `TestIndependentAllocationIsSafe` grows the `[]*Sample` past many
reallocations and asserts an early pointer still aliases the live element.

Create `buffer_test.go`:

```go
package ingestbuf

import "testing"

func TestAppendInvalidatesInteriorPointers(t *testing.T) {
	t.Parallel()

	buf := make([]Sample, 1, 1) // len == cap: next append must reallocate
	buf[0] = Sample{Key: "k0", Value: 0}
	stale := &buf[0]
	startCap := cap(buf)

	buf, _ = growPastCap(buf, 8)
	if cap(buf) == startCap {
		t.Fatalf("expected reallocation; cap stayed %d", startCap)
	}

	buf[0].Value = 42 // mutate the LIVE backing array
	if stale.Value == 42 {
		t.Fatal("captured interior pointer observed the post-reallocation write; it should point at the stale array")
	}
	if stale.Value != 0 {
		t.Fatalf("stale.Value = %d, want 0 (unchanged old array)", stale.Value)
	}
}

func TestReservedCapacityKeepsPointersStable(t *testing.T) {
	t.Parallel()

	b := NewReservedBuffer(8)
	startCap := b.Cap()
	p := b.Append(Sample{Key: "first", Value: 1})
	for i := range 6 { // stay within the reserve
		b.Append(Sample{Key: "fill", Value: int64(i)})
	}
	if b.Cap() != startCap {
		t.Fatalf("cap changed from %d to %d within reserve", startCap, b.Cap())
	}

	b.buf[0].Value = 100 // mutate the live element 0
	if p.Value != 100 {
		t.Fatalf("reserved interior pointer stale: p.Value = %d, want 100", p.Value)
	}
}

func TestIndependentAllocationIsSafe(t *testing.T) {
	t.Parallel()

	b := NewPointerBuffer()
	first := b.Append(Sample{Key: "first", Value: 1})
	for i := range 1000 { // force many []*Sample reallocations
		b.Append(Sample{Key: "fill", Value: int64(i)})
	}

	b.At(0).Value = 7 // mutate through the slice
	if first.Value != 7 {
		t.Fatalf("held *Sample diverged after growth: first.Value = %d, want 7", first.Value)
	}
	if b.Len() != 1001 {
		t.Fatalf("Len = %d, want 1001", b.Len())
	}
}
```

## Review

The hazard is that `&buf[i]` looks like a stable handle and is not: it is only as
stable as the backing array, which `append` is free to replace.
`TestAppendInvalidatesInteriorPointers` pins the exact failure — after a
reallocation the captured pointer and the live slice reference different arrays, so
a write to one is invisible to the other. The two fixes address it from opposite
ends: `slices.Grow` keeps the array from moving so the interior pointer stays
valid (as long as you do not exceed the reserve), and `[]*Sample` never takes an
interior pointer at all, so growth is irrelevant to the pointers callers hold.
Prefer the pointer-slice design whenever you build an index into a buffer; reserve
capacity only when you have a hard bound and want the cache locality of a value
slice.

## Resources

- [`slices.Grow`](https://pkg.go.dev/slices#Grow) — reserve capacity so subsequent appends do not reallocate.
- [Go Blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices) — how append reallocates and copies the backing array.
- [Go spec: Appending to and copying slices](https://go.dev/ref/spec#Appending_and_copying_slices) — the defined behavior of `append`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-nil-pointer-map-lookup-guard.md](06-nil-pointer-map-lookup-guard.md)
