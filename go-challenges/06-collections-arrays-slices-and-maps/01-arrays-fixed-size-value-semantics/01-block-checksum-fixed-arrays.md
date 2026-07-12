# Exercise 1: A Fixed-Size Block Checksum with [16]byte Blocks and a [32]byte Digest

A checksum whose input block is a `[16]byte` and whose output is a `[32]byte`
digest makes the array size the specification: the compiler will not let a block
be fifteen bytes or a digest be thirty-one. This exercise builds that checksum,
processing data one 16-byte block at a time into an `[8]uint64` mixing state with
zero per-block allocation, and pins its behavior with a table of deterministic
tests.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
blockchecksum/               independent module: example.com/blockchecksum
  go.mod
  checksum.go                Block [16]byte, Digest [32]byte, Sum, mixBlock, mixStateIntoDigest
  cmd/
    demo/
      main.go                runnable demo: hash a few payloads, print hex digests
  checksum_test.go           determinism, short/exact/multi-block, distinct inputs, size, empty-input
```

- Files: `checksum.go`, `cmd/demo/main.go`, `checksum_test.go`.
- Implement: `Sum(data []byte) Digest` that mixes 16-byte blocks into an `[8]uint64` state and folds the state into a `[32]byte` digest, allocation-free per block.
- Test: determinism, short/padded input, exact one block, multiple blocks, distinct inputs, `len(Digest) == DigestSize`, and `Sum(nil) == Sum([]byte{})`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/01-arrays-fixed-size-value-semantics/01-block-checksum-fixed-arrays/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/01-arrays-fixed-size-value-semantics/01-block-checksum-fixed-arrays
```

### Why the array sizes are the spec

`Block` is `[16]byte` and `Digest` is `[32]byte`. These are named array types, so
the block size and digest size are welded into the types themselves. `Sum` cannot
return a digest of the wrong length; a future refactor cannot silently change the
block size without the compiler noticing every call site. That is the entire point
of using arrays here rather than `[]byte` with a length comment.

The algorithm is deliberately not a real cryptographic hash — it is a stable,
deterministic mixing function that the tests can pin. It exists to demonstrate the
array mechanics: `Sum` walks `data` in 16-byte strides, copying each stride into a
reused `Block` value (`copy(block[:], data[i:])` bridges the array to the
slice-consuming `copy`), padding any short final block deterministically, and
folding it into the `[8]uint64` state via `mixBlock`. Because `block` and `state`
are arrays declared once and passed by pointer where mutation is needed, the inner
loop allocates nothing. At the end, `mixStateIntoDigest` writes the first four
state words into the digest with `binary.LittleEndian.PutUint64` over `d[i*8:...]`
— again bridging the array to a slice with no allocation.

Note the two receivers-by-pointer: `mixBlock(state *[8]uint64, ...)` and
`mixStateIntoDigest(state *[8]uint64, d *Digest)` take pointers precisely because
they mutate the caller's state and digest. If `mixBlock` took `state [8]uint64` by
value it would mix a copy and throw the result away — the value-semantics trap that
the whole lesson is about.

Create `checksum.go`:

```go
package checksum

import (
	"encoding/binary"
)

// BlockSize is the number of bytes Sum consumes per mixing round.
const BlockSize = 16

// DigestSize is the fixed length of a Sum result.
const DigestSize = 32

// Block is one fixed-size input block. The size is part of the type.
type Block [BlockSize]byte

// Digest is the fixed-size checksum output. The size is part of the type.
type Digest [DigestSize]byte

// Sum returns a deterministic, non-cryptographic checksum of data. It processes
// data one 16-byte block at a time into an [8]uint64 state, allocating nothing
// per block, then folds the state into a [32]byte digest.
func Sum(data []byte) Digest {
	var d Digest
	state := [8]uint64{
		0x0123456789abcdef, 0xfedcba9876543210,
		0xaaaa5555aaaa5555, 0x5555aaaa5555aaaa,
		0xdeadbeefcafebabe, 0x0123456789abcdef,
		0xfedcba9876543210, 0xaaaaaaaaaaaaaaaa,
	}

	var block Block
	for i := 0; i < len(data); i += BlockSize {
		n := copy(block[:], data[i:])
		for j := n; j < BlockSize; j++ {
			block[j] = byte(j + i)
		}
		mixBlock(&state, block, i)
	}

	mixStateIntoDigest(&state, &d)
	return d
}

func mixBlock(state *[8]uint64, block Block, offset int) {
	for i := range 2 {
		state[i] ^= binary.LittleEndian.Uint64(block[i*8:i*8+8]) ^ uint64(offset)
	}
	for i := range 8 {
		state[i] = (state[i] << 13) | (state[i] >> 51)
		state[i] *= 0x9e3779b97f4a7c15
		state[(i+1)%8] += state[i]
	}
}

func mixStateIntoDigest(state *[8]uint64, d *Digest) {
	for i := range 4 {
		binary.LittleEndian.PutUint64(d[i*8:i*8+8], state[i])
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/blockchecksum"
)

func main() {
	inputs := []string{"", "hi", "hello world"}
	for _, in := range inputs {
		d := checksum.Sum([]byte(in))
		fmt.Printf("Sum(%q) = %x\n", in, d)
	}
	fmt.Printf("digest size = %d bytes\n", checksum.DigestSize)
}
```

Note the import path is the module (`example.com/blockchecksum`) but the package
name is `checksum`, so the demo refers to it as `checksum.Sum`.

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Sum("") = efcdab89674523011032547698badcfe5555aaaa5555aaaaaaaa5555aaaa5555
Sum("hi") = 773eb31ad389798e7b1a1111bbae8391e90999482fe6ee306820745a51e7b73d
Sum("hello world") = 7890bdba451e0b7100a6a95b2dc50f35430f3305ed62433f0fda0c56906f33ba
```

The empty input never enters the block loop, so its digest is just the folded
initial state — a fixed, non-zero value. The digests are stable across runs and
distinct across inputs, which the tests below assert rigorously.

### Tests

The tests are a table of the properties that define a correct checksum.
`TestSumIsDeterministic` is the core contract: equal input yields equal digest.
`TestSumDistinguishesDifferentInputs` proves the digest is not a constant.
`TestDigestHasExpectedSize` proves the array type enforces the size — `len(d)` is
`DigestSize` by construction. `TestSumIsEmptyInputStable` pins the empty-input
contract so a future "skip the loop when data is empty" optimization cannot change
behavior: `Sum(nil)` and `Sum([]byte{})` must return the same digest.

Create `checksum_test.go`:

```go
package checksum

import (
	"testing"
)

func TestSumIsDeterministic(t *testing.T) {
	t.Parallel()

	d1 := Sum([]byte("hello world"))
	d2 := Sum([]byte("hello world"))
	if d1 != d2 {
		t.Fatalf("Sum should be deterministic: %x vs %x", d1, d2)
	}
}

func TestSumHandlesShortInput(t *testing.T) {
	t.Parallel()

	d := Sum([]byte("hi"))
	if d == (Digest{}) {
		t.Fatal("Sum should not be the zero digest for non-empty input")
	}
}

func TestSumHandlesExactBlock(t *testing.T) {
	t.Parallel()

	exact := make([]byte, BlockSize)
	for i := range exact {
		exact[i] = byte(i)
	}
	d := Sum(exact)
	if d == (Digest{}) {
		t.Fatal("Sum should not be the zero digest")
	}
}

func TestSumHandlesMultipleBlocks(t *testing.T) {
	t.Parallel()

	data := make([]byte, BlockSize*3)
	for i := range data {
		data[i] = byte(i)
	}
	d1 := Sum(data)
	d2 := Sum(data)
	if d1 != d2 {
		t.Fatal("Sum should be deterministic across multiple blocks")
	}
}

func TestSumDistinguishesDifferentInputs(t *testing.T) {
	t.Parallel()

	d1 := Sum([]byte("hello"))
	d2 := Sum([]byte("world"))
	if d1 == d2 {
		t.Fatal("Sum should distinguish different inputs")
	}
}

func TestDigestHasExpectedSize(t *testing.T) {
	t.Parallel()

	d := Sum([]byte("x"))
	if got := len(d); got != DigestSize {
		t.Fatalf("digest length = %d, want %d", got, DigestSize)
	}
}

func TestSumIsEmptyInputStable(t *testing.T) {
	t.Parallel()

	if Sum(nil) != Sum([]byte{}) {
		t.Fatal("Sum(nil) and Sum([]byte{}) must return the same digest")
	}
}
```

## Review

The checksum is correct when the digest is a pure, deterministic function of the
input bytes: equal inputs collide, distinct inputs (with overwhelming probability)
do not, and the empty input is stable across `nil` and `[]byte{}`. The array types
carry the real lesson: `Block [16]byte` and `Digest [32]byte` make the sizes a
compile-time contract, and `TestDigestHasExpectedSize` is a live assertion that the
type enforces it. The two mixing helpers take `*[8]uint64` and `*Digest` because
they mutate; had they taken the arrays by value, `Sum` would fold every block into
a discarded copy and return the untouched initial state — the value-semantics trap
in its purest form. Run `go test -race` to confirm the code is clean, then reread
`mixBlock` and convince yourself the `&state` and `&d` pointers are load-bearing.

## Resources

- [Go Specification: Array types](https://go.dev/ref/spec#Array_types) — the length is part of the type.
- [encoding/binary](https://pkg.go.dev/encoding/binary) — `LittleEndian.Uint64` and `PutUint64` over a byte slice view of an array.
- [Go blog: Arrays, slices (and strings)](https://go.dev/blog/slices) — arrays vs slices and the mechanics of copying.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-content-addressed-dedup-store.md](02-content-addressed-dedup-store.md)
