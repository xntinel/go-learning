# Exercise 27: Bloom Filter for Space-Efficient Deduplication

**Nivel: Intermedio** — validacion rapida (un test corto).

A deduplication service that ingests billions of event UUIDs cannot keep an
exact set of every ID it has ever seen — a `map[string]struct{}` of a
billion 36-byte strings is tens of gigabytes before you even count the map's
own overhead. A Bloom filter trades that exactness for a fixed, small bit
vector: it can never produce a false negative ("definitely not seen" is
always right), but it can produce a bounded rate of false positives ("probably
seen" is sometimes wrong) once the vector starts to fill up. This module
builds a Bloom filter over a plain `[]uint64` bit vector, ranges each item's
hash-derived bit positions to set and test membership, and ranges a stream of
events to deduplicate them while tracking the filter's estimated false-
positive rate as it fills. The module is fully self-contained: its own
`go mod init`, no external dependencies.

## What you'll build

```text
bloom/                      independent module: example.com/bloom-filter-false-positive-dedup
  go.mod                    go 1.24
  bloom.go                  type Filter; Add, MightContain, Dedup, EstimatedFalsePositiveRate
  cmd/
    demo/
      main.go               runnable demo: dedup a small event stream, report fp_rate
  bloom_test.go              table test: membership after Add + Dedup counts + fp-rate growth
```

- Files: `bloom.go`, `cmd/demo/main.go`, `bloom_test.go`.
- Implement: `Filter.Add`, `Filter.MightContain`, `Filter.Dedup`, and
  `Filter.EstimatedFalsePositiveRate`, all built on ranging the k hash-probe
  positions per item.
- Test: a membership table (before/after `Add`), a `Dedup` count case, and a
  false-positive-rate sanity case.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### One item, k probes: why range replaces k separate hash functions

The textbook definition of a Bloom filter calls for k *independent* hash
functions, but implementing and calling k of them per item is wasteful and,
worse, easy to get subtly correlated if you are not careful about seeding.
The Kirsch-Mitzenmacher trick sidesteps this: compute exactly two independent
hashes, `a` and `b`, and derive the k probe positions as
`(a + i*b) mod m` for `i` in `0..k` — mathematically this behaves like k
independent uniform hash functions for the purposes of the false-positive
bound, at the cost of two hash computations instead of k. `indices` returns
that slice of k positions, and both `Add` (which ranges them to set bits) and
`MightContain` (which ranges them to test bits) share the exact same
derivation, which is the property that makes the filter sound: if `Add` set
bit `p`, `MightContain`'s range over the identical `indices` call will find
`p` set again for that same item.

`MightContain`'s range has an early-exit that `Add`'s does not need: the
moment any one of the k bits is unset, the item was *definitely* never added,
and the loop returns `false` immediately without checking the rest — a
single missing bit is proof of absence. Only when every one of the k bits
survives the full range does the function return `true`, and even then that
`true` is a probabilistic "probably", never a certainty, because another
item's k bits could have coincidentally set the same positions. That gap
between "every bit set" and "definitely this item" is exactly the false-
positive rate `EstimatedFalsePositiveRate` computes from `k`, `n` (items
added), and `m` (total bits) — it rises as `n/m` grows, which is the signal
that tells an operator when to resize the filter rather than keep absorbing
a climbing false-positive rate silently.

Create `bloom.go`:

```go
package bloom

import (
	"hash/fnv"
	"math"
)

// Filter is a space-efficient probabilistic set: MightContain never returns a
// false negative, but it can return a false positive once the bit vector
// fills up. That trade-off is what makes it viable for deduplicating
// billions of items on hardware that could never hold them all as an exact
// set.
type Filter struct {
	words []uint64 // bit vector, 64 bits per word
	m     uint64   // total number of bits
	k     int      // number of hash functions (index probes per item)
	n     uint64   // number of items added, tracked for the FP-rate estimate
}

// New builds a Filter with m bits and k hash probes per item. Both are fixed
// for the filter's lifetime: m controls memory, k controls how sharply the
// false-positive rate rises as the filter fills.
func New(m uint64, k int) *Filter {
	return &Filter{
		words: make([]uint64, (m+63)/64),
		m:     m,
		k:     k,
	}
}

// indices computes the k bit positions item hashes to, using the
// Kirsch-Mitzenmacher double-hashing trick: two independent hashes h1, h2
// combine as h1 + i*h2 to simulate k independent hash functions without
// actually computing k of them.
func (f *Filter) indices(item string) []uint64 {
	h1 := fnv.New64a()
	h1.Write([]byte(item))
	a := h1.Sum64()

	h2 := fnv.New64()
	h2.Write([]byte(item))
	b := h2.Sum64()

	idx := make([]uint64, f.k)
	for i := 0; i < f.k; i++ {
		idx[i] = (a + uint64(i)*b) % f.m
	}
	return idx
}

func (f *Filter) setBit(pos uint64) {
	f.words[pos/64] |= 1 << (pos % 64)
}

func (f *Filter) getBit(pos uint64) bool {
	return f.words[pos/64]&(1<<(pos%64)) != 0
}

// Add sets item's k bits, ranging every probe position so a later
// MightContain for this exact item can never be a false negative.
func (f *Filter) Add(item string) {
	for _, pos := range f.indices(item) {
		f.setBit(pos)
	}
	f.n++
}

// MightContain reports whether item was possibly added before. False means
// definitely not added; true means probably added, with a false-positive
// probability that grows with how full the filter is.
func (f *Filter) MightContain(item string) bool {
	for _, pos := range f.indices(item) {
		if !f.getBit(pos) {
			return false
		}
	}
	return true
}

// EstimatedFalsePositiveRate returns the standard Bloom filter false-positive
// estimate (1 - e^(-k*n/m))^k for the filter's current fill level.
func (f *Filter) EstimatedFalsePositiveRate() float64 {
	if f.n == 0 {
		return 0
	}
	exponent := -float64(f.k) * float64(f.n) / float64(f.m)
	return math.Pow(1-math.Exp(exponent), float64(f.k))
}

// Dedup ranges events in order, adding each one not already possibly present
// and reporting it as unique; an event the filter reports as already present
// is counted as a duplicate and dropped. Because MightContain can false-
// positive, a small fraction of genuinely new events may be incorrectly
// dropped as duplicates — the caller accepts that risk in exchange for
// bounded memory, and EstimatedFalsePositiveRate tells them how often.
func (f *Filter) Dedup(events []string) (unique []string, duplicates int) {
	for _, e := range events {
		if f.MightContain(e) {
			duplicates++
			continue
		}
		f.Add(e)
		unique = append(unique, e)
	}
	return unique, duplicates
}
```

### The runnable demo

The demo dedups a small stream with three repeated event IDs interleaved,
then reports the filter's estimated false-positive rate at that fill level.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bloom-filter-false-positive-dedup"
)

func main() {
	f := bloom.New(1024, 4)

	events := []string{
		"evt-a1b2", "evt-c3d4", "evt-a1b2", "evt-e5f6",
		"evt-c3d4", "evt-g7h8", "evt-a1b2", "evt-i9j0",
	}

	unique, dupes := f.Dedup(events)
	fmt.Printf("unique=%d duplicates=%d\n", len(unique), dupes)
	fmt.Printf("fp_rate=%.2e\n", f.EstimatedFalsePositiveRate())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
unique=5 duplicates=3
fp_rate=1.40e-07
```

### Tests

The membership table proves the no-false-negative guarantee (unset before
`Add`, always set after), a dedicated `Dedup` case checks the unique/
duplicate split against a stream with known repeats, and a growth case
checks the estimated false-positive rate is exactly zero on an empty filter
and strictly between 0 and 1 once items have been added.

Create `bloom_test.go`:

```go
package bloom

import "testing"

func TestMightContain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		item string
	}{
		{name: "short key", item: "evt-1"},
		{name: "uuid-like key", item: "f47ac10b-58cc-4372-a567-0e02b2c3d479"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := New(1024, 4)

			if f.MightContain(tc.item) {
				t.Fatalf("MightContain(%q) = true before Add, want false", tc.item)
			}
			f.Add(tc.item)
			if !f.MightContain(tc.item) {
				t.Fatalf("MightContain(%q) = false after Add, want true (no false negatives allowed)", tc.item)
			}
		})
	}
}

func TestDedup(t *testing.T) {
	t.Parallel()

	f := New(1024, 4)
	events := []string{"a", "b", "a", "c", "b", "a"}

	unique, dupes := f.Dedup(events)

	wantUnique := []string{"a", "b", "c"}
	if len(unique) != len(wantUnique) {
		t.Fatalf("Dedup() unique = %v, want %v", unique, wantUnique)
	}
	for i, u := range unique {
		if u != wantUnique[i] {
			t.Fatalf("Dedup() unique[%d] = %q, want %q", i, u, wantUnique[i])
		}
	}
	if dupes != 3 {
		t.Fatalf("Dedup() duplicates = %d, want 3", dupes)
	}
}

func TestEstimatedFalsePositiveRateGrowsWithLoad(t *testing.T) {
	t.Parallel()

	f := New(256, 3)
	if rate := f.EstimatedFalsePositiveRate(); rate != 0 {
		t.Fatalf("EstimatedFalsePositiveRate() on empty filter = %v, want 0", rate)
	}

	for i := 0; i < 50; i++ {
		f.Add(string(rune('a')) + string(rune(i)))
	}
	rate := f.EstimatedFalsePositiveRate()
	if rate <= 0 || rate >= 1 {
		t.Fatalf("EstimatedFalsePositiveRate() after 50 adds = %v, want a value in (0, 1)", rate)
	}
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

The filter is correct when `MightContain` never returns `false` for an item
that was `Add`ed (no false negatives, ever) and when its false-positive rate
tracks the standard formula as `n` grows relative to `m`. The bug this
design specifically avoids is computing k genuinely separate hashes per
item, which either costs k hash computations per operation or, if done
carelessly with k weak seeded variants of the same hash, produces correlated
probes that inflate the real false-positive rate above what the formula
predicts — the double-hashing derivation in `indices` gives the same
statistical guarantee with exactly two hash computations, no matter how
large k is.

## Resources

- [Bloom, "Space/Time Trade-offs in Hash Coding with Allowable Errors" (1970)](https://dl.acm.org/doi/10.1145/362686.362692)
- [Kirsch and Mitzenmacher, "Less Hashing, Same Performance" (2006)](https://www.eecs.harvard.edu/~michaelm/postscripts/rsa2008.pdf) — the double-hashing trick `indices` implements.
- [hash/fnv](https://pkg.go.dev/hash/fnv)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-consistent-hashing-ring-sharding.md](26-consistent-hashing-ring-sharding.md) | Next: [28-request-coalescing-singleflight.md](28-request-coalescing-singleflight.md)
