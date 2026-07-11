# Exercise 1: The Pointer/Length Calling Convention

Before any bytes cross the boundary, both sides must agree on how a region of
guest memory is named. This exercise builds that agreement as a tiny, pure-Go
codec — the `(pointer, length)` pair, its packed `i64` form, and the bounds check
that turns a guest-supplied region into a trusted one — with no Wasm runtime in
sight, so you can nail the arithmetic before it matters.

This module is fully self-contained. It begins with its own `go mod init`, defines
every symbol it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
abi/                       independent module: example.com/abi
  go.mod                   go 1.26
  abi.go                   PackPtrLen, UnpackPtrLen, CheckBounds, ErrOutOfBounds
  cmd/
    demo/
      main.go              runnable demo: pack a (ptr,len), unpack it back
  abi_test.go              round-trip + bounds table tests, an Example, a boundary test
```

- Files: `abi.go`, `cmd/demo/main.go`, `abi_test.go`.
- Implement: `PackPtrLen(ptr, length uint32) uint64`, `UnpackPtrLen(packed uint64) (ptr, length uint32)`, and `CheckBounds(ptr, length, memSize uint32) error` returning a wrapped `ErrOutOfBounds`.
- Test: table-driven round-trip over edge values asserting no field bleed, agreement with the `api.EncodeU32` spelling, and bounds cases (fits, exactly fills, overflow) asserted with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/abi/cmd/demo
cd ~/go-exercises/abi
go mod init example.com/abi
```

### Why a codec, and why one `i64`

The boundary can only move numbers, so a region of guest memory is described by
two of them: the offset it starts at (its "pointer" into linear memory) and its
byte length. When a guest function *accepts* a region it takes those as two `i32`
parameters and there is nothing to pack. The packing matters when a function must
*return* a region: a single Wasm result is the simplest, most portable shape, so
the convention — the one TinyGo and wazero's own examples use — is to fold the
pair into one `i64` with the pointer in the high 32 bits and the length in the low
32 bits. `PackPtrLen` and `UnpackPtrLen` are the two halves of that fold, and the
only thing that can go wrong is putting a field in the wrong half. That is exactly
why this is worth building and testing on its own: a swapped shift compiles
cleanly, passes a smoke test on small symmetric inputs, and corrupts every real
read.

The equivalence to wazero's helpers is worth internalizing. `api.EncodeU32`
zero-extends a `uint32` into a `uint64` and `api.DecodeU32` truncates it back, so
`api.EncodeU32(ptr)<<32 | api.EncodeU32(length)` is bit-for-bit the same value as
`uint64(ptr)<<32 | uint64(length)`. The test asserts that identity so the plain
arithmetic and the library spelling can never drift.

### Why the bounds check uses `uint64` arithmetic

`CheckBounds` decides whether a `(ptr, length)` a guest handed you actually fits
inside the current memory. The trap is the addition: `ptr + length` in `uint32`
can overflow. A malicious or buggy guest returning `ptr = math.MaxUint32,
length = 2` produces a `uint32` sum of `1`, which sails past a naive `ptr+length <=
memSize` check while pointing 4 GiB out of range. Computing the end offset in
`uint64` — `end := uint64(ptr) + uint64(length)` — cannot wrap for any pair of
32-bit inputs, so the comparison against `memSize` is honest. The check treats a
region that ends exactly at `memSize` as valid (a half-open interval `[ptr, end)`),
which is why `ptr=0, length=memSize` and `ptr=memSize-10, length=10` both pass
while `length=memSize+1` fails.

Create `abi.go`:

```go
package abi

import (
	"errors"
	"fmt"
)

// ErrOutOfBounds is the sentinel for a region that does not fit in linear
// memory. Wrap it with %w so callers can match it via errors.Is.
var ErrOutOfBounds = errors.New("region out of bounds")

// PackPtrLen folds a (pointer, length) pair into a single uint64 with the
// pointer in the high 32 bits and the length in the low 32 bits. This is the
// return convention a guest uses when a function must yield one value.
func PackPtrLen(ptr, length uint32) uint64 {
	return uint64(ptr)<<32 | uint64(length)
}

// UnpackPtrLen reverses PackPtrLen. The pointer is the high half, the length
// the low half; swapping the two silently corrupts every subsequent read.
func UnpackPtrLen(packed uint64) (ptr, length uint32) {
	return uint32(packed >> 32), uint32(packed)
}

// CheckBounds reports whether the half-open region [ptr, ptr+length) fits within
// memSize bytes. The end offset is computed in uint64 so ptr+length cannot wrap
// a uint32 and falsely pass. A region ending exactly at memSize is in bounds.
func CheckBounds(ptr, length, memSize uint32) error {
	end := uint64(ptr) + uint64(length)
	if end > uint64(memSize) {
		return fmt.Errorf("region [%d,%d) does not fit in %d bytes of memory: %w", ptr, end, memSize, ErrOutOfBounds)
	}
	return nil
}
```

### The runnable demo

The demo packs a sample region, prints the packed integer, and unpacks it to show
the two halves survive the round trip untouched.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/abi"
)

func main() {
	const ptr, length = 1024, 5
	packed := abi.PackPtrLen(ptr, length)
	gotPtr, gotLen := abi.UnpackPtrLen(packed)
	fmt.Printf("packed=%d ptr=%d len=%d\n", packed, gotPtr, gotLen)

	if err := abi.CheckBounds(ptr, length, 65536); err != nil {
		fmt.Println("unexpected:", err)
	} else {
		fmt.Println("region fits in one page")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
packed=4398046511109 ptr=1024 len=5
region fits in one page
```

### Tests

The round-trip table sweeps the corners — zero, one, and `math.MaxUint32` in each
field independently — so a field bleeding into the other half fails immediately;
it also asserts the plain-arithmetic pack equals the `api.EncodeU32` spelling and
that `api.DecodeU32` recovers each half. The bounds table covers the region that
fits, the two that exactly fill memory, the one-byte overflow, a pointer past the
end, and the `uint32`-overflow case, each asserted against `ErrOutOfBounds` with
`errors.Is`. The final test is your extension point: it pins the 4 GiB boundary,
where `ptr + length` would wrap in `uint32` and must not.

Create `abi_test.go`:

```go
package abi

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/tetratelabs/wazero/api"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct{ ptr, length uint32 }{
		{0, 0}, {1, 1}, {0, math.MaxUint32}, {math.MaxUint32, 0},
		{math.MaxUint32, math.MaxUint32}, {1024, 5},
	}
	for _, c := range cases {
		packed := PackPtrLen(c.ptr, c.length)
		gotP, gotL := UnpackPtrLen(packed)
		if gotP != c.ptr || gotL != c.length {
			t.Errorf("round trip (%d,%d): got (%d,%d)", c.ptr, c.length, gotP, gotL)
		}
		want := api.EncodeU32(c.ptr)<<32 | api.EncodeU32(c.length)
		if packed != want {
			t.Errorf("(%d,%d): PackPtrLen=%d != api form %d", c.ptr, c.length, packed, want)
		}
		if api.DecodeU32(packed>>32) != c.ptr || api.DecodeU32(packed) != c.length {
			t.Errorf("(%d,%d): api.DecodeU32 disagrees", c.ptr, c.length)
		}
	}
}

func TestBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		ptr, length, mem uint32
		wantErr          bool
	}{
		{"fits", 0, 10, 65536, false},
		{"exactly fills from zero", 0, 65536, 65536, false},
		{"exactly fills at offset", 65526, 10, 65536, false},
		{"exceeds by one", 0, 65537, 65536, true},
		{"ptr past end", 65536, 1, 65536, true},
		{"overflow wrap", math.MaxUint32, 2, 65536, true},
	}
	for _, c := range cases {
		err := CheckBounds(c.ptr, c.length, c.mem)
		if c.wantErr {
			if !errors.Is(err, ErrOutOfBounds) {
				t.Errorf("%s: want ErrOutOfBounds, got %v", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
	}
}

func ExamplePackPtrLen() {
	packed := PackPtrLen(1024, 5)
	ptr, length := UnpackPtrLen(packed)
	fmt.Printf("%d -> ptr=%d len=%d\n", packed, ptr, length)
	// Output: 4398046511109 -> ptr=1024 len=5
}

// Your turn: a guest may return ptr near the top of the address space. Prove the
// bounds check rejects a region whose end lands exactly on the 4 GiB boundary
// (2^32) instead of wrapping a uint32 to a small, falsely-in-bounds value.
func TestBoundaryNoWrap(t *testing.T) {
	t.Parallel()
	err := CheckBounds(1, math.MaxUint32, 65536) // end = 1 + (2^32-1) = 2^32
	if !errors.Is(err, ErrOutOfBounds) {
		t.Fatalf("4 GiB boundary must be rejected, got %v", err)
	}
}
```

## Review

The codec is correct when `UnpackPtrLen(PackPtrLen(p, l))` returns exactly `(p,
l)` for every pair — including the corners where one field is `math.MaxUint32` —
and when the packed value equals `api.EncodeU32(p)<<32 | api.EncodeU32(l)`. If a
corner case bleeds, you have the shift or the truncation on the wrong half:
pointer is the *high* 32 bits, length the *low*. The most common regression is
writing `UnpackPtrLen` as `uint32(packed), uint32(packed>>32)`, which passes for
symmetric inputs like `(5, 5)` and fails the moment the two fields differ, which
is why the table uses asymmetric corners.

`CheckBounds` is correct when it computes the end in `uint64`. Doing the addition
in `uint32` is the subtle bug the "overflow wrap" and boundary tests exist to
catch: `math.MaxUint32 + 2` wraps to `1` and a naive check would call that region
in-bounds. Treat a `false` from a real `Memory.Read`/`Write` (next exercise) the
same way this function treats an oversized region — as a wrapped `ErrOutOfBounds`,
never a silently ignored condition. Run `go test -race` to confirm.

## Resources

- [wazero `api` package](https://pkg.go.dev/github.com/tetratelabs/wazero/api) — `EncodeU32`, `DecodeU32`, and the numeric-only call convention.
- [wazero allocation example (greet.go)](https://github.com/tetratelabs/wazero/blob/main/examples/allocation/tinygo/greet.go) — the `ptr<<32 | size` packing in a real host.
- [WebAssembly Core Spec: numeric types](https://webassembly.github.io/spec/core/syntax/types.html#number-types) — why the boundary carries only i32/i64/f32/f64.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-linear-memory-io.md](02-linear-memory-io.md)
