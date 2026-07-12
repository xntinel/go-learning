# Exercise 33: Bloom Filter — Deferred Hash Function Count Finalization After All Inserts

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A bloom filter's false-positive rate depends on three numbers: the bit
array size, the element count, and the number of independent hash
functions it uses per element. The first is usually fixed ahead of time
by a memory budget; the second is almost never known in advance — a
deduplication filter for a streaming ingest job finds out how many
distinct keys it saw only after the batch finishes. Choosing the hash
function count *before* the element count is known means guessing, and
guessing wrong wastes either memory (too few hash functions, too many
false positives) or CPU on every future lookup (too many hash functions
for how few elements actually went in). This module defers that choice
until `Finalize`, after every `Add` call has already happened, and
computes the filter's packed digest in the same deferred step so it is
never redone. The module is fully self-contained: its own `go mod
init`, all code inline, its own demo and tests.

## What you'll build

```text
bloom/                        independent module: example.com/bloom-filter-dynamic-hashfunc-finalization
  go.mod                       go 1.24
  bloom.go                      Filter (Add, Finalize, MightContain, K, WriteDigest)
  cmd/
    demo/
      main.go                  runnable demo: 50 inserts, adaptive k, digest length
  bloom_test.go                 table over k-selection at different element counts; no-false-negative and panic cases
```

- Files: `bloom.go`, `cmd/demo/main.go`, `bloom_test.go`.
- Implement: `Filter` with `New(bits, maxK int) *Filter`, `Add(item string)`, `Finalize()`, `MightContain(item string) bool`, `K() int`, `WriteDigest(w io.Writer) (err error)`.
- Test: a table over element counts checking the chosen `k`, a no-false-negative sweep, an `Add`-after-`Finalize` panic case, and a digest-stability case.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/07-defer-semantics-and-ordering/33-bloom-filter-dynamic-hashfunc-finalization/cmd/demo
cd go-solutions/03-control-flow/07-defer-semantics-and-ordering/33-bloom-filter-dynamic-hashfunc-finalization
go mod edit -go=1.24
```

### Why the digest computation is a deferred closure inside Finalize, not a lazy check inside WriteDigest

`Finalize` decides `k` from `len(f.inserted)`, sets every bit every
inserted element needs at that `k`, and only then packs those bits into
a byte digest. That packing step is written as a closure deferred at the
top of `Finalize`, which means it executes last, after the bit-setting
loop above it — the exact instant after which `f.bits` is guaranteed
never to change again, because `Add` panics on any call once
`f.finalized` is true (which this same deferred closure also sets, as
its final act). Two things make this the right shape. First, `WriteDigest`
never has to answer "has the bit array changed since I last packed it";
it simply reads `f.digest`, computed exactly once. Second — and this is
the sharper lesson — writing this as an *argument*-form defer,
`defer computeDigest(f.bits, f.k)`, would be a genuine, subtle bug: at
the point that `defer` statement executes (the top of the function,
before `k` has been computed below), `f.k` still holds its zero value,
and the argument form would freeze *that* zero forever, silently
producing a digest built for one hash function no matter what `k`
`Finalize` actually settled on. The closure form defers reading `f.k`
along with the packing call, so it observes the real, computed value at
the moment it actually runs.

Create `bloom.go`:

```go
package bloom

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"math"
)

// Filter is a bloom filter whose hash-function count k is not fixed at
// construction time. Add just records candidate elements; Finalize decides
// k from the final element count and sets every bit those elements need,
// after which no more elements can be added.
type Filter struct {
	bits     []bool
	maxK     int
	inserted []string

	finalized bool
	k         int
	digest    []byte
}

// New returns a filter with the given bit-array size and an upper bound on
// how many hash functions it will ever use.
func New(bits, maxK int) *Filter {
	return &Filter{bits: make([]bool, bits), maxK: maxK}
}

// Add records an element. Panics if called after Finalize -- a bloom
// filter whose hash-function count has already been chosen based on a
// final element count cannot safely accept more elements.
func (f *Filter) Add(item string) {
	if f.finalized {
		panic("bloom: Add called after Finalize")
	}
	f.inserted = append(f.inserted, item)
}

func hashN(item string, n int, m int) int {
	h := fnv.New64a()
	h.Write([]byte(item))
	binary.Write(h, binary.LittleEndian, int64(n))
	return int(h.Sum64() % uint64(m))
}

// Finalize computes the optimal hash-function count from the final
// element count -- k = round((m/n) * ln2), clamped to [1, maxK] -- then
// sets exactly those k bit positions for every inserted element. It is
// safe to call more than once: the guard at the top makes every call
// after the first a no-op.
//
// The digest is computed in a closure deferred until the very end of
// Finalize -- by which point every bit this filter will ever set has
// already been set above. Computing it here, rather than lazily inside
// WriteDigest, means WriteDigest never has to ask "has the bit array
// changed since I last packed it": Finalize's own completion is the only
// signal that matters, and it is also the point after which f.bits is
// guaranteed never to change again. Writing this as `defer
// computeDigest(f.bits, f.k)` -- capturing f.k as a plain argument -- would
// be a real bug: at the point the defer statement runs (the top of this
// function), f.k still holds its zero value, and the argument form would
// freeze that zero instead of the value computed below. The closure form
// reads f.k at the moment it actually executes, after the assignment.
func (f *Filter) Finalize() {
	if f.finalized {
		return
	}

	n := len(f.inserted)
	k := f.maxK
	if n > 0 {
		k = int(math.Round(float64(len(f.bits)) / float64(n) * math.Ln2))
		if k < 1 {
			k = 1
		}
		if k > f.maxK {
			k = f.maxK
		}
	}
	f.k = k

	for _, item := range f.inserted {
		for i := 0; i < k; i++ {
			f.bits[hashN(item, i, len(f.bits))] = true
		}
	}

	defer func() {
		packed := make([]byte, (len(f.bits)+7)/8)
		for i, b := range f.bits {
			if b {
				packed[i/8] |= 1 << uint(i%8)
			}
		}
		f.digest = packed
		f.finalized = true
	}()
}

// MightContain reports whether item could have been added -- false
// negatives are impossible, false positives are possible. Finalize runs
// first if it has not already.
func (f *Filter) MightContain(item string) bool {
	f.Finalize()
	for i := 0; i < f.k; i++ {
		if !f.bits[hashN(item, i, len(f.bits))] {
			return false
		}
	}
	return true
}

// K returns the finalized hash-function count, finalizing first if needed.
func (f *Filter) K() int {
	f.Finalize()
	return f.k
}

// WriteDigest finalizes the filter (if needed) and writes its packed bit
// array to w.
func (f *Filter) WriteDigest(w io.Writer) (err error) {
	f.Finalize()
	if _, err = w.Write(f.digest); err != nil {
		return fmt.Errorf("bloom: write digest: %w", err)
	}
	return nil
}
```

### The runnable demo

1024 bits, a cap of 20 hash functions, 50 inserted elements. The formula
`k = round((m/n) * ln2)` picks 14 — below the cap, so the adaptive
choice actually matters here rather than just clamping to the ceiling.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"

	"example.com/bloom-filter-dynamic-hashfunc-finalization"
)

func main() {
	f := bloom.New(1024, 20)

	const n = 50
	for i := 1; i <= n; i++ {
		f.Add(fmt.Sprintf("order-%d", i))
	}

	fmt.Printf("hash functions chosen after %d inserts: k=%d (maxK=20)\n", n, f.K())

	fmt.Printf("MightContain(order-1) = %v\n", f.MightContain("order-1"))
	fmt.Printf("MightContain(order-50) = %v\n", f.MightContain("order-50"))
	fmt.Printf("MightContain(order-999) = %v (never inserted)\n", f.MightContain("order-999"))

	var buf bytes.Buffer
	if err := f.WriteDigest(&buf); err != nil {
		fmt.Println("digest write error:", err)
	}
	fmt.Printf("digest length: %d bytes\n", buf.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
hash functions chosen after 50 inserts: k=14 (maxK=20)
MightContain(order-1) = true
MightContain(order-50) = true
MightContain(order-999) = false (never inserted)
digest length: 128 bytes
```

`order-999` happens to test negative here; a bloom filter never
guarantees that for an arbitrary un-inserted key, but it never produces
a false *negative* for a key that was actually added, which is the
property the tests below check directly.

### Tests

`TestFinalizeChoosesKFromFinalElementCount` is a table over element
counts, checking the clamped-to-maxK case, the formula-fits-under-maxK
case, and the zero-elements edge case. `TestMightContainNeverFalseNegative`
sweeps 100 inserted elements and asserts every single one still reports
`true`. `TestAddAfterFinalizePanics` and `TestWriteDigestIsStableAcrossCalls`
cover the two structural guarantees `Finalize`'s deferred closure is
responsible for.

Create `bloom_test.go`:

```go
package bloom

import (
	"bytes"
	"fmt"
	"testing"
)

func TestFinalizeChoosesKFromFinalElementCount(t *testing.T) {
	tests := []struct {
		name    string
		bits    int
		maxK    int
		inserts int
		wantK   int
	}{
		{"few elements: formula exceeds maxK, clamps", 256, 8, 5, 8},
		{"many elements: formula fits within maxK", 1024, 20, 50, 14},
		{"zero elements: k stays at maxK", 512, 6, 0, 6},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := New(tc.bits, tc.maxK)
			for i := 0; i < tc.inserts; i++ {
				f.Add(fmt.Sprintf("item-%d", i))
			}
			if got := f.K(); got != tc.wantK {
				t.Errorf("K() = %d, want %d", got, tc.wantK)
			}
		})
	}
}

func TestMightContainNeverFalseNegative(t *testing.T) {
	f := New(2048, 10)
	items := make([]string, 100)
	for i := range items {
		items[i] = fmt.Sprintf("elem-%d", i)
		f.Add(items[i])
	}
	for _, it := range items {
		if !f.MightContain(it) {
			t.Fatalf("MightContain(%q) = false, want true (no false negatives allowed)", it)
		}
	}
}

func TestAddAfterFinalizePanics(t *testing.T) {
	f := New(64, 4)
	f.Add("a")
	f.Finalize()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("want panic when Add is called after Finalize")
		}
	}()
	f.Add("b")
}

func TestWriteDigestIsStableAcrossCalls(t *testing.T) {
	f := New(64, 4)
	f.Add("a")
	f.Add("b")

	var buf1, buf2 bytes.Buffer
	if err := f.WriteDigest(&buf1); err != nil {
		t.Fatalf("first WriteDigest: %v", err)
	}
	if err := f.WriteDigest(&buf2); err != nil {
		t.Fatalf("second WriteDigest: %v", err)
	}
	if !bytes.Equal(buf1.Bytes(), buf2.Bytes()) {
		t.Fatalf("digest differs between calls: %x vs %x", buf1.Bytes(), buf2.Bytes())
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Finalize` is correct when the hash-function count and the packed
digest are both derived from the complete, final set of inserted
elements — never from a partial view frozen at some earlier point.
Deferring the digest-packing closure to the true end of `Finalize`, and
having it read `f.k` and `f.bits` at execution time rather than at the
`defer` statement, is what guarantees that. The mistake this design
avoids is the argument-form defer — `defer computeDigest(f.bits, f.k)`
written at the top of the function — which would silently capture `f.k`
at its zero value, before the formula below ever computes the real one,
producing a digest that looks valid (right length, no error) but
corresponds to a hash-function count of zero instead of the one the
filter actually uses for lookups.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — a deferred closure's free variables are read when the closure body executes, not when the defer statement runs.
- [Bloom filter calculator and formula reference](https://hur.st/bloomfilter/) — the `k = (m/n) * ln 2` optimal hash-count formula this exercise implements.
- [hash/fnv](https://pkg.go.dev/hash/fnv) — the hash family used to derive `k` independent bit positions per element from one base hash plus a salt.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-gossip-protocol-peer-broadcast.md](32-gossip-protocol-peer-broadcast.md) | Next: [34-sliding-window-rate-limiter-continuous.md](34-sliding-window-rate-limiter-continuous.md)
