# Exercise 7: Shrinking a Hot Event Struct by Reordering Fields

When a struct is allocated millions of times per second — a request event, a log
record, a trace span — its in-memory size is a real cost. The compiler inserts
padding bytes to satisfy each field's alignment, and a wasteful field order pays
for padding that a better order eliminates. This module measures an `Event`
struct with `unsafe.Sizeof`/`unsafe.Offsetof`, then reorders its fields to shrink
it.

Fully self-contained: own `go mod init`, inline code, own demo and tests.

## What you'll build

```text
padding/                    independent module: example.com/padding
  go.mod                    go 1.24
  event.go                  Wasteful (bad order) and Packed (good order) event structs
  cmd/
    demo/
      main.go               prints Sizeof of both and bytes saved
  event_test.go             asserts Sizeof(Wasteful) > Sizeof(Packed); offset contiguity
```

- Files: `event.go`, `cmd/demo/main.go`, `event_test.go`.
- Implement: two structs with the same fields (`bool`, `int64`, `bool`, `int32`) in a wasteful order and a packed largest-first order.
- Test: `unsafe.Sizeof(Wasteful{})` is strictly greater than `unsafe.Sizeof(Packed{})`; the packed struct has no internal padding, confirmed with `unsafe.Offsetof`. Assertions target 64-bit (amd64/arm64) and document the platform dependence.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/01-struct-declaration-and-initialization/07-struct-field-alignment-and-padding/cmd/demo
cd go-solutions/07-structs-and-methods/01-struct-declaration-and-initialization/07-struct-field-alignment-and-padding
go mod edit -go=1.24
```

### Why field order changes the size

Each type has an alignment: an `int64` must start on an address that is a multiple
of 8, an `int32` on a multiple of 4, a `bool` on any byte. The compiler lays out
fields in declaration order and inserts **padding** bytes wherever the next field
would otherwise be misaligned. The whole struct is then padded at the end to a
multiple of its largest field's alignment, so that arrays of the struct stay
aligned.

Take `Event` with fields `Active bool`, `Timestamp int64`, `Deleted bool`,
`Code int32`, declared in that wasteful order, on a 64-bit platform:

```
offset  0: Active    (bool, 1 byte)
offset  1: <padding, 7 bytes>   so Timestamp lands on an 8-byte boundary
offset  8: Timestamp (int64, 8 bytes)
offset 16: Deleted   (bool, 1 byte)
offset 17: <padding, 3 bytes>   so Code lands on a 4-byte boundary
offset 20: Code      (int32, 4 bytes)
offset 24: total size (already a multiple of 8)
```

That is **24 bytes** for 14 bytes of actual data — 10 bytes of pure padding. Now
order the same fields largest-alignment-first — `Timestamp int64`, `Code int32`,
`Active bool`, `Deleted bool`:

```
offset  0: Timestamp (int64, 8 bytes)
offset  8: Code      (int32, 4 bytes)
offset 12: Active    (bool, 1 byte)
offset 13: Deleted   (bool, 1 byte)
offset 14: <padding, 2 bytes>   tail padding to a multiple of 8
offset 16: total size
```

Now **16 bytes**: zero internal padding, only 2 bytes of unavoidable tail padding
(the struct must be a multiple of 8 because it contains an `int64`). Reordering
saved 8 bytes per event — a third of the struct — with no change to behavior. At
a few million events per second that is measurable memory bandwidth and GC
pressure.

The rule of thumb: **declare fields from largest alignment to smallest**. Measure,
do not guess: `unsafe.Sizeof` gives the total, `unsafe.Alignof` the alignment,
and `unsafe.Offsetof` the offset of a named field so you can see exactly where the
padding is. And keep the optimization where it pays — a config struct allocated
once does not care; a hot per-request struct does. Sizes are platform-dependent
(on 32-bit `386`, `int64` aligns to 4, changing the layout), so these assertions
target 64-bit amd64/arm64.

An aside for FFI: when a struct must match a C struct's layout across cgo or a
syscall, mark it with a `structs.HostLayout` field (Go 1.23+). That tells the
compiler to use the host platform's C ABI layout rules instead of Go's own, so the
bytes line up with the C side. It does not change pure-Go layout; it is a
correctness marker for the FFI boundary.

Create `event.go`:

```go
package event

// Wasteful declares its fields in a padding-heavy order: a bool before an int64
// forces 7 bytes of padding, and another bool before an int32 forces 3 more.
// On 64-bit platforms this struct is 24 bytes.
type Wasteful struct {
	Active    bool
	Timestamp int64
	Deleted   bool
	Code      int32
}

// Packed holds the SAME fields ordered largest-alignment-first, eliminating all
// internal padding. On 64-bit platforms this struct is 16 bytes.
type Packed struct {
	Timestamp int64
	Code      int32
	Active    bool
	Deleted   bool
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"unsafe"

	"example.com/padding"
)

func main() {
	w := unsafe.Sizeof(event.Wasteful{})
	p := unsafe.Sizeof(event.Packed{})
	fmt.Printf("wasteful=%d bytes\n", w)
	fmt.Printf("packed=%d bytes\n", p)
	fmt.Printf("saved=%d bytes per event\n", w-p)
}
```

Run it (on a 64-bit machine):

```bash
go run ./cmd/demo
```

Expected output:

```
wasteful=24 bytes
packed=16 bytes
saved=8 bytes per event
```

### Tests

The tests assert the size relationship and prove *where* the savings came from.
`TestPackedIsSmaller` asserts `Sizeof(Wasteful) > Sizeof(Packed)` — the portable,
always-true part. `TestExactSizes64Bit` pins the exact 24 and 16 on 64-bit.
`TestNoInternalPadding` uses `unsafe.Offsetof` to prove the packed fields are
contiguous (each field's offset equals the previous field's offset plus its size),
so the only padding left is the 2-byte tail.

Create `event_test.go`:

```go
package event

import (
	"testing"
	"unsafe"
)

func TestPackedIsSmaller(t *testing.T) {
	t.Parallel()
	if unsafe.Sizeof(Wasteful{}) <= unsafe.Sizeof(Packed{}) {
		t.Fatalf("expected Wasteful (%d) > Packed (%d)",
			unsafe.Sizeof(Wasteful{}), unsafe.Sizeof(Packed{}))
	}
}

func TestExactSizes64Bit(t *testing.T) {
	t.Parallel()
	// These exact values hold on 64-bit platforms (amd64, arm64), where int64
	// aligns to 8. On 32-bit 386 the layout differs.
	if got := unsafe.Sizeof(Wasteful{}); got != 24 {
		t.Fatalf("Sizeof(Wasteful) = %d, want 24 (64-bit)", got)
	}
	if got := unsafe.Sizeof(Packed{}); got != 16 {
		t.Fatalf("Sizeof(Packed) = %d, want 16 (64-bit)", got)
	}
}

func TestNoInternalPadding(t *testing.T) {
	t.Parallel()
	// Fields are contiguous: each offset equals the prior offset plus prior size.
	if off := unsafe.Offsetof(Packed{}.Timestamp); off != 0 {
		t.Fatalf("Timestamp offset = %d, want 0", off)
	}
	if off := unsafe.Offsetof(Packed{}.Code); off != 8 {
		t.Fatalf("Code offset = %d, want 8 (right after 8-byte Timestamp)", off)
	}
	if off := unsafe.Offsetof(Packed{}.Active); off != 12 {
		t.Fatalf("Active offset = %d, want 12 (right after 4-byte Code)", off)
	}
	if off := unsafe.Offsetof(Packed{}.Deleted); off != 13 {
		t.Fatalf("Deleted offset = %d, want 13 (right after 1-byte Active)", off)
	}
	// Sum of field sizes is 14; the struct is 16, so exactly 2 bytes are tail
	// padding and zero bytes are internal padding.
	sum := unsafe.Sizeof(Packed{}.Timestamp) + unsafe.Sizeof(Packed{}.Code) +
		unsafe.Sizeof(Packed{}.Active) + unsafe.Sizeof(Packed{}.Deleted)
	if sum != 14 {
		t.Fatalf("field size sum = %d, want 14", sum)
	}
	if tail := unsafe.Sizeof(Packed{}) - sum; tail != 2 {
		t.Fatalf("tail padding = %d, want 2", tail)
	}
}
```

## Review

The optimization is correct when the packed struct is provably smaller with the
same fields and the same behavior: `Sizeof(Packed) < Sizeof(Wasteful)`, and
`Offsetof` shows the packed fields are contiguous so no byte is wasted between
them. The honest caveat the tests encode: even the packed struct keeps 2 bytes of
*tail* padding, because a struct containing an `int64` must be a multiple of 8 —
you cannot always reach zero padding, only zero *internal* padding. Apply this
only where it pays (hot, high-cardinality allocations), measure with `unsafe`
rather than guessing, and remember the numbers are platform-dependent. For structs
crossing an FFI or syscall boundary, `structs.HostLayout` is the correctness knob,
a separate concern from this size optimization. Run `go test -race` and `go vet`.

## Resources

- [`unsafe` package](https://pkg.go.dev/unsafe) — `Sizeof`, `Alignof`, `Offsetof` and their compile-time-constant semantics.
- [`structs` package: HostLayout](https://pkg.go.dev/structs#HostLayout) — marking a struct to match the host C ABI.
- [Go Spec: size and alignment guarantees](https://go.dev/ref/spec#Size_and_alignment_guarantees) — the alignment rules the compiler follows.
- [Go 101: memory layout](https://go101.org/article/memory-layout.html) — a detailed treatment of struct padding.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-anonymous-and-nested-struct-literals.md](08-anonymous-and-nested-struct-literals.md)
