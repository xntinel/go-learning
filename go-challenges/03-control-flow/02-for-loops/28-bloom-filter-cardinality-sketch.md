# Exercise 28: Bloom Filter Construction for Set Membership

**Nivel: Intermedio** — validacion rapida (un test corto).

An LSM-tree storage engine checking whether a key might exist in an
on-disk SSTable before paying for a disk seek is the textbook use case for
a Bloom filter: a fixed-size bit array that can say "definitely not
present" for free, and "maybe present" for a small, bounded false-positive
rate, all without storing a single key. Building one is two nested loops —
for each item, mark one bit per hash function — and the entire correctness
argument rests on that inner loop's bit-index arithmetic never running past
the end of the array.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
bloom/                         module example.com/bloom
  go.mod                       go 1.24
  bloom.go                     Filter; New(m,k); NewFromItems(items,m,k); (*Filter).Add; (*Filter).MightContain
  bloom_test.go                   no false negatives, empty filter, add-then-query, degenerate m=1, index bounds
  cmd/demo/
    main.go                      six SSTable keys loaded into a filter, then a query for a key never added
```

- Files: `bloom.go`, `bloom_test.go`, `cmd/demo/main.go`.
- Implement: `NewFromItems(items []string, m, k int) *Filter` — an outer `for` over items, an inner `for i := 0; i < k; i++` over hash functions, each inner iteration computing `idx := (h1 + i*h2) % m` and setting one bit; `(*Filter).MightContain(item string) bool` — the same inner loop, returning `false` the instant a required bit is unset.
- Test: every added item still reports present; an empty filter rejects everything; add-then-query round-trips for several `(m, k)` combinations; a single-bit filter (`m=1`) deterministically matches anything once one item is added; a sweep of sizes and hash counts never panics from an out-of-range bit index.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/bloom/cmd/demo
cd ~/go-exercises/bloom
go mod init example.com/bloom
go mod edit -go=1.24
```

### Why double hashing turns two hash functions into k

A real Bloom filter needs `k` independent hash functions, and implementing
`k` genuinely different hash algorithms by hand does not scale as `k`
grows. The standard shortcut — Kirsch-Mitzenmacher double hashing — derives
all `k` positions from just two real hashes: `idx_i = (h1 + i*h2) mod m`
for `i` in `0..k-1`. This module's inner loop, `for i := 0; i < f.k;
i++`, is that formula made literal: each pass through it is one simulated
hash function, and the loop's bound `k` is the caller's chosen number of
hash functions, not a fixed constant. The two real hashes come from FNV-1
and FNV-1a over the same input, which are cheap, allocation-free, and
already in the standard library — good enough for this use case, where the
goal is spreading bits pseudo-randomly, not cryptographic security.

The place this design earns its "off-by-one safety" framing is the modulo
in that same formula. `idx` is computed as `(h1 + uint64(i)*h2) %
uint64(len(f.bits))` on every single iteration — never once as a raw sum
that happens to fit, and never cached across calls to `Add`. Any version
that computes the modulo once and reuses it, or that omits the modulo for
`i == 0` on the theory that "the first hash is already in range," breaks
the moment `h1` alone happens to exceed `m`. `TestIndexNeverOutOfRange`
exists specifically to catch that class of bug: it exercises a spread of
tiny and large `m` values against several `k` values and simply asserts
nothing panics, because an out-of-range index is the only way this
particular mistake manifests.

Create `bloom.go`:

```go
package bloom

import "hash/fnv"

// Filter is a Bloom filter: a fixed-size bit array plus k hash functions,
// used to test set membership in constant space at the cost of a bounded
// false-positive rate and no false negatives ever.
type Filter struct {
	bits []bool
	k    int
}

// New creates an empty filter with m bits and k hash functions per item.
// Both are clamped to at least 1: a zero-size filter or zero hash functions
// would make MightContain meaningless (either an immediate panic from a
// modulo by zero, or a filter that can never record anything).
func New(m, k int) *Filter {
	if m < 1 {
		m = 1
	}
	if k < 1 {
		k = 1
	}
	return &Filter{bits: make([]bool, m), k: k}
}

// NewFromItems builds a filter of m bits and k hash functions and loads it
// with items in one pass. The outer loop iterates the items; the inner loop
// iterates the k hash functions for the current item; each inner iteration
// sets exactly one bit. Nesting this way (rather than calling Add in a
// separate loop) is what makes the "off-by-one in bit arithmetic" concern
// visible in one place: idx is always computed modulo len(f.bits), so no
// combination of item and hash index can ever index past the end of the
// bit array.
func NewFromItems(items []string, m, k int) *Filter {
	f := New(m, k)
	for _, item := range items {
		h1, h2 := hashPair(item)
		for i := 0; i < f.k; i++ {
			idx := (h1 + uint64(i)*h2) % uint64(len(f.bits))
			f.bits[idx] = true
		}
	}
	return f
}

// Add records item in the filter by setting one bit per hash function,
// using the same double-hashing scheme as NewFromItems.
func (f *Filter) Add(item string) {
	h1, h2 := hashPair(item)
	for i := 0; i < f.k; i++ {
		idx := (h1 + uint64(i)*h2) % uint64(len(f.bits))
		f.bits[idx] = true
	}
}

// MightContain reports whether item may have been added. A false result is
// certain: item was never added. A true result is not certain: item may
// never have been added and all k of its bits happen to be set by other
// items (a false positive). The loop returns the instant any required bit
// is unset, since one missing bit is enough to prove absence; only an item
// whose every bit is set survives to the final "true".
func (f *Filter) MightContain(item string) bool {
	h1, h2 := hashPair(item)
	for i := 0; i < f.k; i++ {
		idx := (h1 + uint64(i)*h2) % uint64(len(f.bits))
		if !f.bits[idx] {
			return false
		}
	}
	return true
}

// hashPair derives two independent 64-bit hashes of item using FNV-1 and
// FNV-1a. Combining them as h1 + i*h2 (Kirsch-Mitzenmacher double hashing)
// simulates k independent hash functions from just these two, which is the
// standard technique for avoiding k separate hash implementations.
func hashPair(item string) (h1, h2 uint64) {
	a := fnv.New64a()
	a.Write([]byte(item))
	sum1 := a.Sum64()

	b := fnv.New64()
	b.Write([]byte(item))
	sum2 := b.Sum64()
	if sum2 == 0 {
		sum2 = 1 // a zero second hash would collapse double hashing to always probing bit h1
	}
	return sum1, sum2
}
```

### The runnable demo

The demo loads six SSTable-style keys into a 1024-bit filter with 4 hash
functions, confirms every one still reports present, and then queries a
key that was never added.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bloom"
)

func main() {
	keys := []string{
		"user:1001", "user:1002", "user:1003",
		"order:5001", "order:5002",
		"session:abc123",
	}

	f := bloom.NewFromItems(keys, 1024, 4)

	fmt.Println("keys known to be in the SSTable:")
	for _, k := range keys {
		fmt.Printf("  %-16s might contain = %v\n", k, f.MightContain(k))
	}

	probe := "user:9999"
	fmt.Printf("\nkey never added:\n  %-16s might contain = %v\n", probe, f.MightContain(probe))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
keys known to be in the SSTable:
  user:1001        might contain = true
  user:1002        might contain = true
  user:1003        might contain = true
  order:5001       might contain = true
  order:5002       might contain = true
  session:abc123   might contain = true

key never added:
  user:9999        might contain = false
```

### Tests

`TestNoFalseNegatives` is the filter's one non-negotiable guarantee: every
added item must always report present. `TestEmptyFilterRejectsEverything`
checks the trivial but important zero-items baseline.
`TestAddThenMightContainSameItem` round-trips across a table of `(m, k)`
combinations. `TestSingleBitFilterAlwaysMatches` pins the degenerate `m=1`
case to a deterministic outcome (every item collapses to bit 0), and
`TestIndexNeverOutOfRange` sweeps a range of sizes and hash counts purely
to confirm nothing panics — the concrete symptom an off-by-one in the bit
arithmetic would produce.

Create `bloom_test.go`:

```go
package bloom

import "testing"

func TestNoFalseNegatives(t *testing.T) {
	t.Parallel()

	items := []string{"user:1", "user:2", "user:3", "order:100", "order:101", "shard-a", "shard-b"}
	f := NewFromItems(items, 256, 4)

	for _, item := range items {
		if !f.MightContain(item) {
			t.Fatalf("MightContain(%q) = false, want true (a Bloom filter must never false-negative)", item)
		}
	}
}

func TestEmptyFilterRejectsEverything(t *testing.T) {
	t.Parallel()

	f := New(256, 4)

	for _, item := range []string{"anything", "nothing-added-yet", ""} {
		if f.MightContain(item) {
			t.Fatalf("MightContain(%q) = true, want false on an empty filter", item)
		}
	}
}

func TestAddThenMightContainSameItem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		m, k int
		item string
	}{
		{name: "typical size", m: 128, k: 3, item: "key-42"},
		{name: "single hash function", m: 64, k: 1, item: "key-7"},
		{name: "many hash functions", m: 512, k: 10, item: "key-99"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := New(tc.m, tc.k)
			f.Add(tc.item)
			if !f.MightContain(tc.item) {
				t.Fatalf("MightContain(%q) = false after Add, want true", tc.item)
			}
		})
	}
}

// TestSingleBitFilterAlwaysMatches pins down the degenerate m=1 case: every
// hash index collapses to bit 0 regardless of the item or hash function
// index, so once any item is added, MightContain reports true for every
// possible item. This is a deterministic property of the modulo arithmetic,
// not a probabilistic one, and it is exactly the kind of boundary an
// off-by-one in the index computation (for example forgetting the modulo,
// or using len(f.bits)+1) would get wrong in the opposite direction: a panic
// instead of a safe, if uselessly permissive, filter.
func TestSingleBitFilterAlwaysMatches(t *testing.T) {
	t.Parallel()

	f := New(1, 1)
	f.Add("only-item")

	for _, item := range []string{"only-item", "completely-different", ""} {
		if !f.MightContain(item) {
			t.Fatalf("MightContain(%q) = false, want true (m=1 means every item maps to bit 0)", item)
		}
	}
}

// TestIndexNeverOutOfRange adds and queries many items across a range of m
// and k combinations. Its assertion is that the calls do not panic: a
// bit-index computation that ever produces idx >= len(f.bits) would index
// out of range and crash, which is the concrete failure mode of an
// off-by-one in the double-hashing arithmetic.
func TestIndexNeverOutOfRange(t *testing.T) {
	t.Parallel()

	sizes := []int{1, 2, 7, 64, 1000}
	hashCounts := []int{1, 2, 5, 16}

	for _, m := range sizes {
		for _, k := range hashCounts {
			f := New(m, k)
			for i := 0; i < 50; i++ {
				item := itemName(i)
				f.Add(item)
				f.MightContain(item)
			}
		}
	}
}

func itemName(i int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	return "item-" + string(alphabet[i%len(alphabet)]) + "-" + string(alphabet[(i/len(alphabet))%len(alphabet)])
}
```

## Review

`MightContain` is correct when it never returns `false` for an item that
was actually added, and both `Add` and `MightContain` are correct only
because they compute the exact same `idx` formula for the exact same
`(item, i)` pair — any drift between the two (say, `Add` using `i` from
`0` to `k` inclusive while `MightContain` stops at `k-1`) would either
leave a bit unset that a later query expects, or query a bit that was
never actually part of the item's fingerprint. The common mistake this
design avoids is computing the bit index without the modulo "just for the
first hash function," on the assumption that a single FNV hash always fits
inside a reasonably sized bit array — it does not, and the very first
production key whose hash exceeds `len(f.bits)` panics the process. Run
`go test -count=1 ./...`.

## Resources

- [Bloom filter (Wikipedia)](https://en.wikipedia.org/wiki/Bloom_filter) — the data structure and its false-positive-rate math.
- [Less Hashing, Same Performance: Building a Better Bloom Filter (Kirsch & Mitzenmacher)](https://www.eecs.harvard.edu/~michaelm/postscripts/rsa2008.pdf) — the double-hashing technique `hashPair` implements.
- [hash/fnv package](https://pkg.go.dev/hash/fnv) — the two independent hash functions this module combines.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-vector-clock-causality-propagation.md](27-vector-clock-causality-propagation.md) | Next: [29-watermark-stream-windowing-emission.md](29-watermark-stream-windowing-emission.md)
