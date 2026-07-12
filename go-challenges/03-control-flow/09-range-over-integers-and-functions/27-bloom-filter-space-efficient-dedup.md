# Exercise 27: Bloom Filter Space-Efficient Dedup Combinator — Distinct with Configurable False-Positive Rate

**Nivel: Intermedio** — validacion rapida (un test corto).

A high-volume event stream needs its duplicates dropped -- the same trace ID
resubmitted by a retrying client, the same idempotency key replayed by an
at-least-once queue -- but a `map[string]struct{}` that tracks every distinct
key ever seen grows without bound and eventually becomes the largest
allocation in the process. A Bloom filter trades that unbounded growth for a
fixed-size bit array: memory is `O(m)` regardless of how many distinct items
pass through, at the cost of an occasional false positive that makes a
genuinely new item look already-seen. This exercise is an independent module
with its own `go mod init`.

## What you'll build

```text
bloom/                     independent module: example.com/bloom-filter-space-efficient-dedup
  go.mod                    module example.com/bloom-filter-space-efficient-dedup
  bloom.go                  Filter, New, MightContain, Add, Distinct
  cmd/
    demo/
      main.go               runnable demo: 10 trace IDs with repeats deduped to 7
  bloom_test.go              exact dedup, no false negatives, upstream-stop, false positive, panics
```

Implement: `New(m, k uint) *Filter`, `(*Filter) MightContain(item string) bool`, `(*Filter) Add(item string)`, and `Distinct(f *Filter, src iter.Seq[string]) iter.Seq[string]` yielding each item only the first time `f` reports it as unseen.
Test: a stream with repeats yields only the first occurrence of each item, in order; a fresh filter never reports an item as contained before it is added; a consumer break stops the source at the right call count; an intentionally undersized filter demonstrates a real false-positive drop; `New` panics on a non-positive bit count or hash count.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

A textbook Bloom filter runs `k` independent hash functions per item, but
computing `k` real hashes per item is `k` times the work for no real benefit
over a mathematically equivalent shortcut: the Kirsch-Mitzenmacher technique
derives `k` positions from a single 64-bit hash by splitting it into two
32-bit halves `h1, h2` and computing `position(i) = (h1 + i*h2) mod m`. This
is what `positions` does, and it is why this filter only ever calls
`hash/fnv` once per item regardless of `k`. The asymmetry that defines a
Bloom filter's contract is directional: `MightContain` returning `false` is a
hard guarantee the item was never added (zero false negatives), but
`MightContain` returning `true` might be a hash collision rather than a real
prior sighting (a bounded false-positive rate that shrinks as `m` grows
relative to the number of distinct items). `Distinct` treats a `true` result
as "already seen" unconditionally, which means a colliding new item is
silently dropped instead of yielded -- a shrunk-but-nonzero error rate is
the whole point of trading a bitmap for an exact set.

Create `bloom.go`:

```go
package bloom

import (
	"hash/fnv"
	"iter"
)

// Filter is a fixed-size Bloom filter: an m-bit array checked and set at k
// positions per item. It never reports a false negative -- if MightContain
// returns false the item was definitely never Added -- but it can report a
// false positive: two different items can hash to the same k positions and
// the filter will claim the second one was already seen. That asymmetry is
// exactly the trade a deduplication combinator is willing to make in
// exchange for O(m) memory instead of O(distinct items) memory.
type Filter struct {
	bits []uint64
	m    uint
	k    uint
}

// New creates a Filter with an m-bit array checked at k positions per item.
// It panics if m or k is less than 1: a zero-bit filter cannot store
// anything, and zero hash positions would make every item vacuously "seen".
func New(m, k uint) *Filter {
	if m < 1 {
		panic("bloom: m must be >= 1")
	}
	if k < 1 {
		panic("bloom: k must be >= 1")
	}
	return &Filter{bits: make([]uint64, (m+63)/64), m: m, k: k}
}

// positions computes the k bit positions for item using the Kirsch-Mitzenmacher
// technique: a single 64-bit hash is split into two 32-bit halves h1, h2, and
// position i is (h1 + i*h2) mod m. This gives k effectively-independent
// positions from one hash computation instead of running k separate hash
// functions, which is the standard space/speed trade-off for production
// Bloom filters.
func (f *Filter) positions(item string) []uint {
	h := fnv.New64a()
	h.Write([]byte(item))
	sum := h.Sum64()
	h1 := uint32(sum >> 32)
	h2 := uint32(sum)
	if h2 == 0 {
		h2 = 1 // avoid every position collapsing onto h1 alone
	}
	pos := make([]uint, f.k)
	for i := uint(0); i < f.k; i++ {
		combined := uint64(h1) + uint64(i)*uint64(h2)
		pos[i] = uint(combined % uint64(f.m))
	}
	return pos
}

// MightContain reports whether every one of item's k bit positions is
// already set. A true result means "probably already added, or a false
// positive collision"; a false result means "definitely never added."
func (f *Filter) MightContain(item string) bool {
	for _, p := range f.positions(item) {
		word, bit := p/64, p%64
		if f.bits[word]&(1<<bit) == 0 {
			return false
		}
	}
	return true
}

// Add sets item's k bit positions.
func (f *Filter) Add(item string) {
	for _, p := range f.positions(item) {
		word, bit := p/64, p%64
		f.bits[word] |= 1 << bit
	}
}

// Distinct wraps src and yields each item only the first time f reports it
// was not already present, then records it in f. Because f is a Bloom
// filter rather than a map[string]struct{}, memory is bounded by m bits
// regardless of how many distinct items pass through -- the price is that a
// hash collision can make a genuinely new item look already-seen and get
// silently dropped instead of yielded, which is the false-positive rate the
// caller accepted by choosing m and k. Passing the same *Filter into two
// calls to Distinct lets duplicate detection span multiple streams, which
// is what cross-cluster or multi-shard dedup needs.
func Distinct(f *Filter, src iter.Seq[string]) iter.Seq[string] {
	return func(yield func(string) bool) {
		for item := range src {
			if f.MightContain(item) {
				continue
			}
			f.Add(item)
			if !yield(item) {
				return
			}
		}
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bloom-filter-space-efficient-dedup"
)

func main() {
	traceIDs := []string{
		"trace-a1", "trace-b2", "trace-a1", "trace-c3", "trace-d4",
		"trace-b2", "trace-e5", "trace-f6", "trace-c3", "trace-g7",
	}

	src := func(yield func(string) bool) {
		for _, id := range traceIDs {
			if !yield(id) {
				return
			}
		}
	}

	f := bloom.New(64, 3)
	for id := range bloom.Distinct(f, src) {
		fmt.Println(id)
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
trace-a1
trace-b2
trace-c3
trace-d4
trace-e5
trace-f6
trace-g7
```

Ten trace IDs with three repeats (`trace-a1`, `trace-b2`, `trace-c3` each
appear twice) collapse to the seven distinct IDs, each printed once, in the
order of its first appearance. A 64-bit filter with 3 hash positions has
enough headroom for ten items that no collision occurs here -- see the test
suite for what happens when `m` is deliberately undersized.

### Tests

Create `bloom_test.go`:

```go
package bloom

import (
	"fmt"
	"iter"
	"testing"
)

func sliceSeq(items []string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for _, item := range items {
			if !yield(item) {
				return
			}
		}
	}
}

func TestDistinctYieldsFirstSightingOnly(t *testing.T) {
	t.Parallel()

	items := []string{"a", "b", "a", "c", "b", "d", "a"}
	f := New(1024, 4)

	var got []string
	for item := range Distinct(f, sliceSeq(items)) {
		got = append(got, item)
	}

	want := []string{"a", "b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestFilterNeverFalseNegative(t *testing.T) {
	t.Parallel()

	f := New(1024, 4)
	items := []string{"x1", "x2", "x3", "x4", "x5"}
	for _, item := range items {
		if f.MightContain(item) {
			t.Fatalf("MightContain(%q) = true before Add: false positive on a fresh filter", item)
		}
		f.Add(item)
		if !f.MightContain(item) {
			t.Fatalf("MightContain(%q) = false right after Add: Bloom filters must never false-negative", item)
		}
	}
}

func TestDistinctStopsUpstreamOnBreak(t *testing.T) {
	t.Parallel()

	calls := 0
	src := func(yield func(string) bool) {
		for i := 0; i < 1000; i++ {
			calls++
			if !yield(fmt.Sprintf("item-%d", i)) {
				return
			}
		}
	}

	f := New(4096, 4)
	kept := 0
	for range Distinct(f, src) {
		kept++
		if kept == 3 {
			break
		}
	}
	if kept != 3 {
		t.Fatalf("kept = %d, want 3", kept)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3: the source must stop, not run to completion", calls)
	}
}

func TestUndersizedFilterCanFalsePositive(t *testing.T) {
	t.Parallel()

	// A deliberately tiny filter (32 bits, 4 hash positions) documents the
	// trade-off: "item-1" is a genuinely distinct string, but it collides
	// with the bits "item-0" already set, so Distinct silently drops it
	// instead of yielding it. This is the accepted cost of O(m) memory
	// instead of an exact O(distinct items) set.
	f := New(32, 4)
	src := sliceSeq([]string{"item-0", "item-1"})

	var got []string
	for item := range Distinct(f, src) {
		got = append(got, item)
	}

	want := []string{"item-0"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("got = %v, want %v (this filter size is intentionally undersized to force the collision)", got, want)
	}
}

func TestNewPanicsOnInvalidArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		m, k uint
	}{
		{"zero bits", 0, 4},
		{"zero hashes", 64, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			New(tc.m, tc.k)
		})
	}
}
```

## Review

`TestUndersizedFilterCanFalsePositive` is the test that earns its place in
this suite: it is not testing a bug, it is pinning down the documented
trade-off with a real, reproducible collision so a future change to
`positions` cannot silently shift the false-positive rate without a test
noticing. The common mistake when adopting a Bloom filter for deduplication
is sizing `m` and `k` once at prototype time against a small sample and never
revisiting them as volume grows -- the false-positive rate is a function of
how full the bit array gets, so a filter that was 99.9% accurate at launch
degrades quietly as more distinct items pass through it, with no error, no
panic, just a rising rate of legitimate events dropped as "duplicates."
Production deployments size `m` from an expected item count and acceptable
error rate up front, or roll the filter over on a schedule.

## Resources

- [`iter.Seq` documentation](https://pkg.go.dev/iter#Seq)
- [Bloom, "Space/Time Trade-offs in Hash Coding with Allowable Errors" (1970)](https://dl.acm.org/doi/10.1145/362686.362692)
- [Kirsch & Mitzenmacher, "Less Hashing, Same Performance: Building a Better Bloom Filter"](https://www.eecs.harvard.edu/~michaelm/postscripts/rsa2008.pdf)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-consistent-hashing-partition-iterator.md](26-consistent-hashing-partition-iterator.md) | Next: [28-cron-expression-iterator.md](28-cron-expression-iterator.md)
