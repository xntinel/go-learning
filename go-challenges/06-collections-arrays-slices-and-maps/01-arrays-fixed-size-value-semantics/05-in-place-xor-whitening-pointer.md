# Exercise 5: In-Place Block Whitening with a *[16]byte Pointer Receiver

Mutating a fixed-size buffer in place — the essence of any block transform — only
works if you pass a pointer to the array. A value parameter mutates a copy and the
caller sees nothing. This exercise builds a 16-byte XOR whitening step both ways
and pins the difference: `Whiten(*[16]byte)` mutates the caller's block,
`WhitenCopy([16]byte)` returns a transformed copy and leaves the argument alone.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
whitening/                   independent module: example.com/whitening
  go.mod
  whiten.go                  Whiten(block, key *[16]byte); WhitenCopy(block [16]byte) [16]byte
  cmd/
    demo/
      main.go                runnable demo: in-place vs copy, XOR involution
  whiten_test.go             in-place mutates; copy is pure; involution property; alloc benchmark
```

- Files: `whiten.go`, `cmd/demo/main.go`, `whiten_test.go`.
- Implement: `Whiten(block, key *[16]byte)` XORing key into block in place, and `WhitenCopy(block [16]byte) [16]byte` returning a transformed copy.
- Test: `Whiten` mutates the caller's block; `WhitenCopy` does not; whitening twice with the same key restores the original; a benchmark showing the pointer path is allocation-free.
- Verify: `go test -count=1 -race ./...`

### Why the mutation path needs *[16]byte

`Whiten(block, key *[16]byte)` takes pointers to arrays. The XOR loop writes
`block[i] ^= key[i]`, and because `block` is a pointer the writes land in the
caller's array. Had the signature been `Whiten(block [16]byte, ...)` — value
parameter — the function would receive a 16-byte copy on the stack, XOR into that
copy, and discard it on return; the caller's block would be unchanged. This is the
single most common array mistake, and it is silent: the code compiles, runs, and
does nothing visible. Passing `*[16]byte` is what makes in-place mutation real.

`WhitenCopy(block [16]byte) [16]byte` is the deliberate value-semantics variant: it
takes a copy, transforms it, and returns it. The caller's argument is untouched
because it was copied at the call, and the caller gets the result only through the
return value. This is the right shape when you want an immutable transform — feed a
block in, get a new block out, original preserved.

Two bridges matter for interop. `&arr` produces a `*[16]byte` from an array
variable, so a caller with a plain `[16]byte` can call `Whiten(&block, &key)`
without heap allocation — the pointers refer to the existing stack storage. And
`arr[:]` (or `(&arr)[:]`) produces a `[]byte` view over the array for any
slice-consuming API; the demo uses it to hand the block to a slice function without
copying. Neither bridge allocates: the array already exists, and the pointer or
slice header just refers to it.

The XOR itself has a clean property the tests exploit: XOR is an involution.
`b ^ k ^ k == b`, so applying `Whiten` twice with the same key restores the
original block. That gives a property test that does not depend on any specific
key bytes.

Create `whiten.go`:

```go
package whitening

// BlockSize is the fixed whitening block width in bytes.
const BlockSize = 16

// Whiten XORs key into block in place. Both are *[16]byte, so the writes land in
// the caller's arrays with no copy and no allocation.
func Whiten(block, key *[BlockSize]byte) {
	for i := range block {
		block[i] ^= key[i]
	}
}

// WhitenCopy returns a whitened copy of block, leaving the caller's argument
// untouched. block is passed by value, so it is already an independent copy.
func WhitenCopy(block [BlockSize]byte) [BlockSize]byte {
	key := DefaultKey
	for i := range block {
		block[i] ^= key[i]
	}
	return block
}

// DefaultKey is a fixed whitening key used by WhitenCopy so the value-semantics
// variant is self-contained.
var DefaultKey = [BlockSize]byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
}
```

Note `for i := range block` where `block` is a `*[16]byte`: ranging over an array
pointer iterates the indices of the pointed-to array, so `block[i]` reads and
writes through the pointer. That is a small, idiomatic convenience Go allows only
for arrays and array pointers.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/whitening"
)

func main() {
	key := [16]byte{0xaa, 0xbb, 0xcc, 0xdd}

	// In-place: the caller's block changes.
	block := [16]byte{0x01, 0x02, 0x03, 0x04}
	before := block
	whitening.Whiten(&block, &key)
	fmt.Printf("in-place: changed=%v first4=%#x\n", block != before, block[:4])

	// Involution: whiten again with the same key restores the original.
	whitening.Whiten(&block, &key)
	fmt.Printf("involution restores: %v\n", block == before)

	// Copy variant: the argument is untouched, result is returned.
	orig := [16]byte{0x10, 0x20, 0x30, 0x40}
	out := whitening.WhitenCopy(orig)
	fmt.Printf("copy: arg unchanged=%v result differs=%v\n", orig[0] == 0x10, out != orig)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in-place: changed=true first4=0xabb9cfd9
involution restores: true
copy: arg unchanged=true result differs=true
```

`%#x` on the `block[:4]` byte slice prints the four bytes as one `0x`-prefixed hex
run: `0x01^0xaa 0x02^0xbb 0x03^0xcc 0x04^0xdd` = `ab b9 cf d9`.

### Tests

`TestWhitenMutatesInPlace` XORs a key into a block and asserts the caller's block
reflects the change byte-for-byte. `TestWhitenCopyIsPure` asserts `WhitenCopy`
leaves its argument untouched and returns a different value.
`TestWhitenInvolution` is the property test: two `Whiten` calls with the same key
restore the original, for any block. `BenchmarkWhiten` runs the pointer path under
`b.ReportAllocs` to document zero allocations.

Create `whiten_test.go`:

```go
package whitening

import (
	"testing"
)

func TestWhitenMutatesInPlace(t *testing.T) {
	t.Parallel()

	block := [BlockSize]byte{0x01, 0x02, 0x03}
	key := [BlockSize]byte{0xff, 0x0f, 0xf0}
	Whiten(&block, &key)

	want := [BlockSize]byte{0x01 ^ 0xff, 0x02 ^ 0x0f, 0x03 ^ 0xf0}
	if block != want {
		t.Fatalf("Whiten in place = %#x, want %#x", block, want)
	}
}

func TestWhitenCopyIsPure(t *testing.T) {
	t.Parallel()

	orig := [BlockSize]byte{0x10, 0x20, 0x30}
	out := WhitenCopy(orig)

	if orig != ([BlockSize]byte{0x10, 0x20, 0x30}) {
		t.Fatalf("WhitenCopy mutated its argument: %#x", orig)
	}
	if out == orig {
		t.Fatal("WhitenCopy should return a transformed value")
	}
}

func TestWhitenInvolution(t *testing.T) {
	t.Parallel()

	key := [BlockSize]byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02, 0x03, 0x04}
	for seed := 0; seed < 16; seed++ {
		var block [BlockSize]byte
		for i := range block {
			block[i] = byte(seed*31 + i)
		}
		orig := block
		Whiten(&block, &key)
		Whiten(&block, &key)
		if block != orig {
			t.Fatalf("double whiten did not restore original for seed %d", seed)
		}
	}
}

func BenchmarkWhiten(b *testing.B) {
	block := [BlockSize]byte{1, 2, 3, 4}
	key := [BlockSize]byte{5, 6, 7, 8}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		Whiten(&block, &key)
	}
}
```

## Review

The transform is correct when `Whiten` changes the caller's array and `WhitenCopy`
does not — the two functions differ only in that one takes `*[16]byte` and the
other `[16]byte`, and that single difference is the whole lesson in value vs pointer
semantics. `TestWhitenMutatesInPlace` proves the pointer path writes through to the
caller; `TestWhitenCopyIsPure` proves the value path is isolated;
`TestWhitenInvolution` proves the XOR math independent of any specific key. The
mistake to internalize: a value receiver or value parameter on a mutating method
(`func (b Block) Whiten(...)`) silently operates on a copy and appears to do
nothing — always `*[N]byte` when you intend to mutate. Run `go test -race` and
`go test -bench=. -benchmem` to confirm the pointer path allocates nothing.

## Resources

- [Go Specification: Address operators](https://go.dev/ref/spec#Address_operators) — `&arr` yields `*[N]T`.
- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — `arr[:]` produces a slice over an array's storage.
- [Go Specification: For statements with range clause](https://go.dev/ref/spec#For_range) — ranging over an array pointer.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-header-canonicalize-lookup-table.md](04-header-canonicalize-lookup-table.md) | Next: [06-gcm-nonce-and-tag-verify.md](06-gcm-nonce-and-tag-verify.md)
