# Exercise 19: A Fixed [1024]uint64 Bitset and the Value-Receiver Mutation Trap

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A Bloom-filter-style bitset is a classic use of a large fixed array: 1024
`uint64` words address 65536 bits, and because the size is known and fixed at
compile time, `[1024]uint64` inside a struct is allocation-free and
cache-resident — exactly what a hot-path membership filter (a duplicate-
request cache, a seen-keys filter in a dedup pipeline) wants. But a large
array embedded in a struct is precisely where the "arrays are values, not
references" rule bites hardest: a method with a *value* receiver operates on
a full copy of that 1024-word array, so any write it makes vanishes the
instant the method returns, and nothing about the call site looks wrong — it
compiles, it runs, it just silently does nothing.

This exercise builds `BitSet`, a package you can drop into a service, with
every mutating method taking a pointer receiver deliberately. The trap this
module is named for — a value-receiver method that silently discards its
own write — never appears anywhere in `bitset.go`. It lives entirely in the
test file, as an unexported helper contrasted directly against the real
`Set` method, so the danger is proven rather than merely described.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
bitset/                        module example.com/bitset
  go.mod                       go 1.24
  bitset.go                    BitSet{bits [1024]uint64}; Set/Clear/Has/TrySet (pointer receivers); PopCount; All (iter.Seq[uint]); Union; WordsPopCount(array by value); ErrIndexRange
  bitset_test.go                setBad contrast, word-boundary bits, copy independence, value-param no-alias, TrySet/Clear/All/Union, ExampleBitSet
```

- Files: `bitset.go`, `bitset_test.go`.
- Implement: `BitSet` wrapping a `[1024]uint64`; `func (b *BitSet) Set(i uint)`, `func (b *BitSet) Clear(i uint)`, and `func (b *BitSet) Has(i uint) bool` with pointer receivers; `func (b *BitSet) TrySet(i uint) error` returning `ErrIndexRange` instead of panicking on an out-of-range index; `func (b *BitSet) PopCount() int` delegating to `WordsPopCount(words [1024]uint64) int`, a helper that takes the backing array by value; `func (b *BitSet) All() iter.Seq[uint]` iterating set bit indices in ascending order; `func (b *BitSet) Union(other *BitSet) BitSet` returning a new, independent set.
- Test: the value-receiver trap, isolated as an unexported `setBad` helper, proven to lose its write while `Set` keeps it; bits at word boundaries (0, 63, 64, 1023\*64+63) set and read correctly without disturbing neighbors; a plain-assignment copy of a `BitSet` is independent of the original; passing the array by value into `WordsPopCount` cannot mutate the caller's `BitSet`; `TrySet` rejects an out-of-range index instead of panicking; `Clear` unsets one bit without touching its neighbors; `All` yields set bits in order and snapshots at call time; `Union` combines two sets without mutating either; `ExampleBitSet` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/bitset
cd ~/go-exercises/bitset
go mod init example.com/bitset
go mod edit -go=1.24
```

### Why a value receiver silently loses the write

`BitSet` holds `bits [1024]uint64` as a plain array field, not a slice.
That is deliberate: a `[]uint64` field would let two `BitSet` values share
the same backing words after a struct copy, which is exactly the bug a
fixed-size array is supposed to prevent. But the same value semantics that
make a `BitSet` copy safely independent also mean that a method receiver of
type `BitSet` (not `*BitSet`) receives its *own* independent copy of the
struct, array field included, every time it is called. A value-receiver
version of `Set` would compute `b.bits[i/64] |= 1 << (i % 64)` correctly —
the arithmetic is not wrong, and the write genuinely happens — but `b`
inside such a method is a temporary that exists only for the duration of
the call, so the write is real but invisible to the caller, who is left
holding the original, untouched `BitSet`:

```go
// The trap, never written into bitset.go: a value receiver.
func (b BitSet) setBad(i uint) {
	b.bits[i/64] |= 1 << (i % 64) // mutates a copy; the caller sees nothing
}
```

This is worse than a typo, because nothing about the call site signals
danger: `bs.setBad(5)` would look identical to `bs.Set(5)`, compile without a
warning, and run without a panic. The only way to catch it is to check the
*effect* — read the bit back and see that it never actually got set — which
is exactly what `TestSetBadSilentlyLosesTheWrite` does in the test file,
against a package-level, unexported `setBad` function kept entirely out of
the component's real API. Every mutating method that actually ships in
`bitset.go` — `Set`, `Clear`, `TrySet` — uses a pointer receiver, `Has` does
too for consistency and to avoid an unnecessary 8 KB copy on every read (a
`[1024]uint64` is exactly 8192 bytes — the exact case where "arrays are
cheap for small arrays" stops being true).

`WordsPopCount` demonstrates the flip side of the same rule used
deliberately, as a feature rather than a trap: it takes `[1024]uint64` *by
value*. Because the parameter is a copy, `WordsPopCount` cannot reach back
into the caller's `BitSet` no matter what it does internally — passing a
large array by value into a read-only helper is a safe, allocation-bounded
way to hand out a snapshot without a lock or a defensive-copy comment. The
cost is a real 8 KB stack copy per call, which is why `PopCount` (the method
most callers actually use) still takes `*BitSet` and only pays that copy
once, at the point where `WordsPopCount` is invoked. `Union` leans on the
same property to return a brand-new, independent `BitSet` by value: since
the underlying array copies element-for-element, the caller gets a value
that shares nothing with either operand.

Create `bitset.go`:

```go
// Package bitset implements a fixed-size bitset backed by a [1024]uint64
// array, the same shape used inside a Bloom filter's underlying bit array.
package bitset

import (
	"errors"
	"fmt"
	"iter"
	"math/bits"
)

// NumWords is the number of uint64 words backing the set.
const NumWords = 1024

// NumBits is the total addressable bit count: 1024 words * 64 bits.
const NumBits = NumWords * 64

// ErrIndexRange is returned by TrySet when the requested index falls
// outside [0, NumBits).
var ErrIndexRange = errors.New("bitset: index out of range")

// BitSet is a fixed 65536-bit set. The zero value is an empty set ready to
// use. Because bits is a true array field, copying a BitSet value by plain
// assignment deep-copies every bit -- there is no shared backing store the
// way there would be with a []uint64 field.
//
// BitSet is not safe for concurrent use: Set, Clear, and Has must not be
// called concurrently on the same BitSet without external synchronization,
// because Set and Clear perform an unsynchronized read-modify-write on a
// single word.
type BitSet struct {
	bits [NumWords]uint64
}

// Set marks bit i as present. i must be in [0, NumBits); Set panics on an
// out-of-range index, the same contract plain array indexing already gives
// every caller of bits[i/64]. Use TrySet for an index computed from input
// you do not trust to be in range.
func (b *BitSet) Set(i uint) {
	b.bits[i/64] |= 1 << (i % 64)
}

// TrySet is the non-panicking counterpart to Set. It validates i is in
// range before mutating, returning ErrIndexRange instead of panicking --
// the shape to reach for when i comes from untrusted input, such as a bit
// position decoded off the wire.
func (b *BitSet) TrySet(i uint) error {
	if i >= NumBits {
		return fmt.Errorf("%w: %d not in [0,%d)", ErrIndexRange, i, NumBits)
	}
	b.Set(i)
	return nil
}

// Clear unmarks bit i. i must be in [0, NumBits); Clear panics on an
// out-of-range index, matching Set's contract.
func (b *BitSet) Clear(i uint) {
	b.bits[i/64] &^= 1 << (i % 64)
}

// Has reports whether bit i is set. i must be in [0, NumBits).
func (b *BitSet) Has(i uint) bool {
	return b.bits[i/64]&(1<<(i%64)) != 0
}

// PopCount returns the number of set bits.
func (b *BitSet) PopCount() int {
	return WordsPopCount(b.bits)
}

// All returns an iterator over every set bit index, in ascending order. The
// iterator reads a private copy of b's words up front, so mutating b while
// ranging over All is safe and never observed mid-iteration -- the standard
// iter.Seq contract of stopping early whenever yield returns false.
func (b *BitSet) All() iter.Seq[uint] {
	words := b.bits
	return func(yield func(uint) bool) {
		for wi, w := range words {
			for w != 0 {
				i := uint(wi)*64 + uint(bits.TrailingZeros64(w))
				if !yield(i) {
					return
				}
				w &= w - 1 // clear the lowest set bit
			}
		}
	}
}

// Union returns a new BitSet containing every bit set in b, other, or both.
// b and other are left unmodified. Because bits is a true array, the
// returned value is a genuinely independent copy: no caller can mutate it
// and see the change reflected in b or other, or vice versa -- the same
// value-copy-as-immutability property that makes a BitSet snapshot safe to
// hand out without a lock.
func (b *BitSet) Union(other *BitSet) BitSet {
	var out BitSet
	for i := range b.bits {
		out.bits[i] = b.bits[i] | other.bits[i]
	}
	return out
}

// WordsPopCount counts the set bits across a [NumWords]uint64 array passed
// BY VALUE. Because arrays are values, words inside this function is an
// independent copy of whatever the caller passed -- WordsPopCount cannot
// mutate the caller's array even if it wanted to, which is exactly the
// property that makes passing a fixed array by value into a read-only
// helper safe: no aliasing, no lock needed, no defensive copy to write.
func WordsPopCount(words [NumWords]uint64) int {
	n := 0
	for _, w := range words {
		n += bits.OnesCount64(w)
	}
	return n
}
```

### Using it

`BitSet` is the whole surface: declare a `var bs BitSet` (the zero value is
ready to use), call `Set`/`Clear` to mutate it and `Has` to query it. Every
mutating and reading method takes `*BitSet`, so always call them through a
variable, never through a value returned from a function — `f().Set(5)`
would not compile, which is the type system enforcing the same discipline
`TestSetBadSilentlyLosesTheWrite` demonstrates by hand. `TrySet` is the
entry point for an index that did not come from your own code, such as a
bit position deserialized off the wire: it returns `ErrIndexRange`
(checkable with `errors.Is`) instead of crashing the process on a bad input.
`All` returns a real `iter.Seq[uint]`, so it works directly in a `for i :=
range bs.All()` loop with no custom iterator type to learn, and `Union`
returns an independent snapshot a caller can keep and mutate freely without
touching either input set — the aliasing contract documented on both
methods.

The module has no `main.go`, because a bitset is a library, not a tool. Its
executable demonstration is `ExampleBitSet`: `go test` runs it and compares
its standard output against the `// Output:` comment, so the usage shown
below cannot drift away from the code.

### Tests

`TestSetBadSilentlyLosesTheWrite` is the test this module exists to write:
it calls the unexported, package-private `setBad` (a plain function taking
`BitSet` by value, never a method on the real type) and asserts the bit did
*not* get set, documenting the trap as an explicit, checked property rather
than a comment someone can ignore. `TestSetMutatesInPlace` is its corrected
counterpart, `Set` itself. `TestSetAndHasAcrossWordBoundary` sweeps bit
indices that land in different words of the array — including the very
first bit, the last bit of the first word, the first bit of the second
word, and the very last addressable bit — to make sure the `i/64`/`i%64`
arithmetic is right at every seam. `TestBitSetCopyIsIndependent` checks the
benign side of the same value semantics: a snapshot taken by plain
assignment cannot be corrupted by later mutations to the original or vice
versa. `TestWordsPopCountValueParamNoAlias` confirms that passing the array
by value into a helper is genuinely safe. `TestTrySetRejectsOutOfRange`,
`TestClearUnsetsWithoutDisturbingNeighbors`,
`TestAllYieldsSetBitsInAscendingOrder`, and
`TestUnionCombinesWithoutMutatingEitherOperand` each pin one of the newer
methods against its documented contract.

Create `bitset_test.go`:

```go
package bitset

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// setBad is the trap this module exists to teach, kept out of bitset.go
// entirely: it takes BitSet BY VALUE, so the write below lands in a
// throwaway copy that is discarded the instant setBad returns. Nothing
// about the call site looks wrong -- it compiles, it runs, it just silently
// does nothing to the caller's BitSet. Contrast with Set, which takes
// *BitSet and mutates the caller's array in place.
func setBad(b BitSet, i uint) {
	b.bits[i/64] |= 1 << (i % 64)
}

// TestSetBadSilentlyLosesTheWrite pins the trap numerically: after setBad
// returns, the bit it tried to set must NOT be visible on the caller's
// BitSet, because setBad only ever touched its own copy.
func TestSetBadSilentlyLosesTheWrite(t *testing.T) {
	t.Parallel()

	var bs BitSet
	setBad(bs, 5)
	if bs.Has(5) {
		t.Fatal("setBad takes BitSet by value: the write must NOT be visible on bs, " +
			"but Has(5) reports true")
	}
}

// TestSetMutatesInPlace is the corrected counterpart: Set has a pointer
// receiver, so the write reaches the caller's BitSet.
func TestSetMutatesInPlace(t *testing.T) {
	t.Parallel()

	var bs BitSet
	bs.Set(5)
	if !bs.Has(5) {
		t.Fatal("Set has a pointer receiver: Has(5) must report true after Set(5)")
	}
}

// TestSetAndHasAcrossWordBoundary exercises bit indices that land in
// different words of the [1024]uint64 array, including the very first and
// very last addressable bit.
func TestSetAndHasAcrossWordBoundary(t *testing.T) {
	t.Parallel()

	cases := []uint{0, 1, 63, 64, 65, 127, NumBits/2 + 1, NumBits - 1}
	for _, i := range cases {
		var bs BitSet
		bs.Set(i)
		if !bs.Has(i) {
			t.Fatalf("Set(%d) then Has(%d) = false, want true", i, i)
		}
		// No other bit in a fresh set should have flipped.
		if i > 0 && bs.Has(i-1) {
			t.Fatalf("Set(%d) unexpectedly also set bit %d", i, i-1)
		}
	}
}

// TestBitSetCopyIsIndependent asserts that copying a BitSet by value (via
// plain assignment) deep-copies the underlying array, so mutating the copy
// never reaches the original -- the same value semantics that make setBad's
// bug possible are what make a defensive snapshot of a BitSet trustworthy.
func TestBitSetCopyIsIndependent(t *testing.T) {
	t.Parallel()

	var original BitSet
	original.Set(10)

	snapshot := original
	snapshot.Set(20)

	if original.Has(20) {
		t.Fatal("mutating the snapshot must not affect the original BitSet")
	}
	if !snapshot.Has(10) {
		t.Fatal("the snapshot must still carry the bits set before it was copied")
	}
}

// TestWordsPopCountValueParamNoAlias proves that passing the [NumWords]uint64
// array by value into WordsPopCount cannot mutate the caller's BitSet: the
// parameter inside the function is an independent copy.
func TestWordsPopCountValueParamNoAlias(t *testing.T) {
	t.Parallel()

	var bs BitSet
	bs.Set(1)
	bs.Set(2)
	bs.Set(3)

	before := bs.PopCount()
	if before != 3 {
		t.Fatalf("PopCount before = %d, want 3", before)
	}

	// Calling WordsPopCount directly with a copy of bs.bits cannot reach
	// back into bs, no matter what the function does internally.
	_ = WordsPopCount(bs.bits)

	after := bs.PopCount()
	if after != before {
		t.Fatalf("PopCount after helper call = %d, want unchanged %d", after, before)
	}
}

// TestTrySetRejectsOutOfRange asserts TrySet returns ErrIndexRange instead
// of panicking when i is outside [0, NumBits), and leaves the set
// untouched.
func TestTrySetRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	var bs BitSet
	if err := bs.TrySet(NumBits); !errors.Is(err, ErrIndexRange) {
		t.Fatalf("TrySet(NumBits) err = %v, want ErrIndexRange", err)
	}
	if bs.PopCount() != 0 {
		t.Fatalf("PopCount after a rejected TrySet = %d, want 0", bs.PopCount())
	}
	if err := bs.TrySet(42); err != nil {
		t.Fatalf("TrySet(42): %v", err)
	}
	if !bs.Has(42) {
		t.Fatal("TrySet(42) succeeded but Has(42) reports false")
	}
}

// TestClearUnsetsWithoutDisturbingNeighbors sets three bits, clears the
// middle one, and checks only that bit changed.
func TestClearUnsetsWithoutDisturbingNeighbors(t *testing.T) {
	t.Parallel()

	var bs BitSet
	bs.Set(10)
	bs.Set(11)
	bs.Set(12)

	bs.Clear(11)

	if bs.Has(11) {
		t.Fatal("Clear(11) did not unset bit 11")
	}
	if !bs.Has(10) || !bs.Has(12) {
		t.Fatal("Clear(11) disturbed a neighboring bit")
	}
}

// TestAllYieldsSetBitsInAscendingOrder checks that All enumerates exactly
// the set bits, in order, and that obtaining the iterator snapshots the
// words up front: mutating bs after calling All (but before ranging over
// it) must not appear in the iteration already in flight.
func TestAllYieldsSetBitsInAscendingOrder(t *testing.T) {
	t.Parallel()

	var bs BitSet
	want := []uint{3, 64, 65, 200, 4000}
	for _, i := range want {
		bs.Set(i)
	}

	seq := bs.All()
	bs.Set(9000) // mutate after obtaining the iterator, before ranging over it

	var got []uint
	for i := range seq {
		got = append(got, i)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("All() = %v, want %v (snapshot taken at All(), not at range time)", got, want)
	}
}

// TestUnionCombinesWithoutMutatingEitherOperand checks the returned set has
// every bit from both operands, and that neither operand changed.
func TestUnionCombinesWithoutMutatingEitherOperand(t *testing.T) {
	t.Parallel()

	var a, b BitSet
	a.Set(1)
	a.Set(2)
	b.Set(2)
	b.Set(3)

	u := a.Union(&b)

	for _, i := range []uint{1, 2, 3} {
		if !u.Has(i) {
			t.Fatalf("Union missing bit %d", i)
		}
	}
	if a.Has(3) {
		t.Fatal("Union mutated a")
	}
	if b.Has(1) {
		t.Fatal("Union mutated b")
	}
}

// ExampleBitSet is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleBitSet() {
	var bs BitSet
	bs.Set(5)
	bs.Set(7)
	bs.Set(1000)
	fmt.Println("has 5:", bs.Has(5))
	fmt.Println("has 6:", bs.Has(6))
	fmt.Println("pop count:", bs.PopCount())

	bs.Clear(7)
	fmt.Println("after Clear(7), pop count:", bs.PopCount())

	var other BitSet
	other.Set(7)
	other.Set(2000)
	union := bs.Union(&other)
	for i := range union.All() {
		fmt.Println("union bit:", i)
	}

	snapshot := bs
	snapshot.Set(9999)
	fmt.Println("original has 9999:", bs.Has(9999))
	fmt.Println("snapshot has 9999:", snapshot.Has(9999))

	// Output:
	// has 5: true
	// has 6: false
	// pop count: 3
	// after Clear(7), pop count: 2
	// union bit: 5
	// union bit: 7
	// union bit: 1000
	// union bit: 2000
	// original has 9999: false
	// snapshot has 9999: true
}
```

## Review

`Set`/`Has`/`Clear` are correct when a bit set at any index in `[0, NumBits)`
reads back true and does not disturb its neighbors, and `setBad` is
"correct" only in the narrow, deliberate sense that it demonstrably fails to
mutate the caller's `BitSet` — that failure is the point, pinned by
`TestSetBadSilentlyLosesTheWrite` so it can never regress. The mistake this
module exists to prevent is exactly the one described in the concepts file:
expecting a value-receiver method on a struct with an array field to mutate
the caller's copy. It does not, ever, for any array size — the bug is
identical whether the array is `[8]byte` or, as here, `[1024]uint64`; the
only thing the large size changes is how expensive the silently-discarded
copy is. `TrySet` gives the API a non-panicking path for indices that come
from outside the process, `All` exposes a real `iter.Seq[uint]` instead of a
hand-rolled iterator type, and `Union` returns an independent value that
leans on the exact same array-copy semantics that make the trap possible in
the first place — proof that the rule cuts both ways. Run `go test -count=1
-race ./...` to confirm the trap, the fix, the word-boundary arithmetic, the
newer methods, and `ExampleBitSet`'s output.

## Resources

- [Go Specification: Method sets](https://go.dev/ref/spec#Method_sets) — how a value versus pointer receiver determines what the method can see and mutate.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — the standard guidance on choosing a pointer receiver for a type with large or mutable state.
- [math/bits](https://pkg.go.dev/math/bits#OnesCount64) — `OnesCount64` and `TrailingZeros64`, the population-count and bit-scan primitives `WordsPopCount` and `All` use.
- [iter package](https://pkg.go.dev/iter) — `iter.Seq`, the standard iterator shape `All` returns instead of a custom type.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-dns-header-12-byte-codec.md](18-dns-header-12-byte-codec.md) | Next: [20-service-mesh-hop-matrix-2d-array.md](20-service-mesh-hop-matrix-2d-array.md)
