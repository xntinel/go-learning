# Exercise 15: A Sorted Key Index With a Lazy iter.Seq2 Range Scan

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every LSM-tree storage engine -- LevelDB, RocksDB, Pebble (CockroachDB's Go
engine), bbolt -- keeps a small sorted index in memory that maps keys to
where their data actually lives, and every read starts by searching that
index. The index itself is simple: sorted keys, binary search. The
interesting engineering decision is what a *range scan* over that index
returns. A function that returns `[]Entry` must build the whole window
before the caller sees a single element, which is fine for ten keys and a
real cost for a compaction scanning millions of them, or for a caller who
only wanted the first match and is about to `break` out of the loop anyway.

Go 1.23 added exactly the shape this problem wants: `iter.Seq2[K, V]`, a
function type that `range` can iterate directly, and that stops the instant
the loop body returns `false` from its implicit `break`. This module builds
`ssindex.Index`, a sorted key/value index whose `Range` method returns an
`iter.Seq2` instead of a slice, so that scanning a window and stopping early
are the same operation instead of "materialize everything, then maybe use
some of it."

The trap this module isolates is subtle because it never panics or returns
the wrong answer: a `Range` implementation that quietly falls back to
building a slice internally still *compiles* against the `iter.Seq2` return
type and still returns *correct* results. It just throws away the entire
performance reason to have chosen the interface. The tests here pin the
actual behavioral difference -- how much work happens before the caller's
first iteration -- not just the returned values.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
ssindex/                 module example.com/ssindex
  go.mod                 go 1.24
  ssindex.go             Entry, Index; New, Get, Range
  ssindex_test.go        New/Get/Range tables, half-open window, early-break
                         contrast, aliasing, concurrency, ExampleIndex_Range
```

- Files: `ssindex.go`, `ssindex_test.go`.
- Implement: `New(entries []Entry) (*Index, error)` validating that `entries` is sorted by `Key` in strictly increasing order, rejecting an unsorted input with `ErrUnsorted` and a duplicate key with `ErrDuplicateKey`; `(*Index).Get(key string) (value []byte, ok bool)` via `slices.BinarySearch`; `(*Index).Range(lo, hi string) iter.Seq2[string, []byte]` yielding every entry in the half-open window `[lo, hi)` lazily, located by two `sort.Search` calls.
- Test: `New` over nil/single/unsorted/duplicate entries; `Get` over present, absent, and an empty index; `Range` over a table of half-open windows including both empty-window shapes; the early-break contrast against an unexported `rangeSliceNaive` helper; the aliasing contract on yielded values; `Index` safe for concurrent use; and `ExampleIndex_Range` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/12-sorted-collections-binary-search/15-sorted-key-index-lazy-range-scan
cd go-solutions/06-collections-arrays-slices-and-maps/12-sorted-collections-binary-search/15-sorted-key-index-lazy-range-scan
go mod edit -go=1.24
```

### A slice-returning range scan can't stop early; an iter.Seq2 one can

The naive version of a range scan looks entirely reasonable and is what most
engineers write first:

```go
func rangeSliceNaive(idx *Index, lo, hi string) []Entry {
    var out []Entry
    for i, k := range idx.keys {
        if k >= lo && k < hi {
            out = append(out, Entry{Key: k, Value: idx.vals[i]})
        }
    }
    return out
}
```

Nothing here is wrong in the sense of returning bad data -- the contents are
correct. The problem is structural: the function must finish visiting every
matching entry before it can return anything at all, because a `[]Entry` is
a value, not a process. A caller who writes `for _, e := range
rangeSliceNaive(idx, lo, hi) { use(e); break }` still paid the full cost of
building the whole window, even though it only ever looked at the first
element.

`iter.Seq2[K, V]` is a function type, `func(yield func(K, V) bool)`. Calling
it doesn't compute anything up front; it runs the loop body and calls
`yield` once per element, and when `yield` returns `false` -- which the
compiler generates automatically when a `range`-over-func loop body executes
`break`, `return`, or hits an error -- the iterator function returns
immediately, no matter how much of the window it hasn't visited yet.
`Range` below computes only the two boundary indices with `sort.Search`
before it starts yielding; everything else happens exactly as many times as
the caller actually asks for.

Create `ssindex.go`:

```go
// Package ssindex is a small, immutable, sorted key index in the shape used
// by the block index of an LSM-tree storage engine (LevelDB, RocksDB,
// Pebble, bbolt): string keys, kept sorted, searched by binary search, and
// scanned in ranges through a lazy iterator rather than a materialized
// slice.
//
// The package demonstrates iter.Seq2 as the interface a range scan should
// implement instead of a hand-rolled Next()/Value() cursor or a function
// that returns []Entry: a slice-returning scan must build the whole window
// before the caller sees the first element, while iter.Seq2 yields one pair
// at a time and stops the moment the caller's loop breaks.
package ssindex

import (
	"errors"
	"fmt"
	"iter"
	"slices"
	"sort"
)

// Sentinel errors returned by New. Callers should test for them with
// errors.Is rather than comparing error strings.
var (
	// ErrUnsorted means the entries were not supplied in strictly increasing
	// key order.
	ErrUnsorted = errors.New("ssindex: entries must be sorted by key")
	// ErrDuplicateKey means the same key appeared more than once.
	ErrDuplicateKey = errors.New("ssindex: duplicate key")
)

// Entry is one key/value pair as supplied to New.
type Entry struct {
	Key   string
	Value []byte
}

// Index is an immutable, sorted key index.
//
// An Index is safe for concurrent use by multiple goroutines: nothing about
// it changes after New returns, so reads never race.
//
// Values returned by Get and yielded by Range alias the backing storage
// this Index was built from. A caller that needs to retain or mutate a
// value beyond the current call must copy it, for example with bytes.Clone.
type Index struct {
	keys []string
	vals [][]byte
}

// New builds an Index from entries, which must already be sorted by Key in
// strictly increasing order. New returns ErrUnsorted if they are not, and
// ErrDuplicateKey if the same key appears twice. A nil or empty entries
// slice is not an error; it produces a valid, empty Index.
//
// New does not sort entries itself: sorting is the caller's responsibility,
// and validating the invariant here catches a build-time mistake before it
// becomes a silent wrong answer at query time.
func New(entries []Entry) (*Index, error) {
	keys := make([]string, len(entries))
	vals := make([][]byte, len(entries))
	for i, e := range entries {
		keys[i] = e.Key
		vals[i] = e.Value
	}
	for i := 1; i < len(keys); i++ {
		switch {
		case keys[i] == keys[i-1]:
			return nil, fmt.Errorf("%w: %q at position %d", ErrDuplicateKey, keys[i], i)
		case keys[i] < keys[i-1]:
			return nil, fmt.Errorf("%w: %q at position %d follows %q", ErrUnsorted, keys[i], i, keys[i-1])
		}
	}
	return &Index{keys: keys, vals: vals}, nil
}

// Len reports the number of entries in the index.
func (idx *Index) Len() int { return len(idx.keys) }

// Get returns the value stored under key and reports whether it was found.
// The returned slice aliases the Index's backing storage; see the Index doc
// comment for the aliasing contract.
func (idx *Index) Get(key string) (value []byte, ok bool) {
	pos, found := slices.BinarySearch(idx.keys, key)
	if !found {
		return nil, false
	}
	return idx.vals[pos], true
}

// Range returns an iterator over every entry with a key in the half-open
// interval [lo, hi). It locates the window with two binary searches -- the
// same lower-bound and upper-bound pair used throughout this lesson -- and
// then yields entries one at a time without ever materializing the window as
// a slice. The values yielded alias the Index's backing storage.
//
// The iterator stops as soon as the caller's range-over-func loop breaks (or
// the yield function returns false for any other reason), so a caller that
// only wants the first match out of a large window pays for exactly one
// entry, not the whole window.
func (idx *Index) Range(lo, hi string) iter.Seq2[string, []byte] {
	return func(yield func(string, []byte) bool) {
		start := sort.Search(len(idx.keys), func(i int) bool { return idx.keys[i] >= lo })
		end := sort.Search(len(idx.keys), func(i int) bool { return idx.keys[i] >= hi })
		for i := start; i < end; i++ {
			if !yield(idx.keys[i], idx.vals[i]) {
				return
			}
		}
	}
}
```

### Using it

`New` is the only place an `Index` is built, and it is the only place
sortedness is checked -- every read afterward trusts that invariant instead
of re-verifying it, which is what makes `Get` and `Range` cheap. Because
nothing mutates an `Index` after construction, a single value can be shared
across every goroutine serving reads without a mutex, exactly as
`TestIndexIsSafeForConcurrentUse` holds it to.

The aliasing contract matters as much as the concurrency one: `Get` and
`Range` hand back slices that point directly into the `Index`'s own storage.
A caller that needs a value to outlive the current call, or that wants to
mutate it, must copy it first. `TestRangeValuesAliasBackingStorage` pins the
other side of that contract -- mutating a yielded value is visible on the
Index's next read -- so the doc comment cannot silently drift from the code.

The module has no `main.go`; a sorted index is a library, not a tool. Its
executable demonstration is `ExampleIndex_Range`: `go test` runs it and
compares its standard output against the `// Output:` comment below.

```go
func ExampleIndex_Range() {
	idx, err := New([]Entry{
		{Key: "block-000010", Value: []byte("offset:0")},
		{Key: "block-000020", Value: []byte("offset:4096")},
		{Key: "block-000030", Value: []byte("offset:8192")},
		{Key: "block-000040", Value: []byte("offset:12288")},
	})
	if err != nil {
		panic(err)
	}

	fmt.Println("point lookup:")
	if v, ok := idx.Get("block-000020"); ok {
		fmt.Println(" ", "block-000020", "->", string(v))
	}

	fmt.Println("range [block-000015, block-000035):")
	for k, v := range idx.Range("block-000015", "block-000035") {
		fmt.Println(" ", k, "->", string(v))
	}

	fmt.Println("first match only, then break:")
	for k := range idx.Range("block-000000", "block-999999") {
		fmt.Println(" ", k)
		break
	}

	// Output:
	// point lookup:
	//   block-000020 -> offset:4096
	// range [block-000015, block-000035):
	//   block-000020 -> offset:4096
	//   block-000030 -> offset:8192
	// first match only, then break:
	//   block-000010
}
```

The third block is the point of the module: `Range("block-000000",
"block-999999")` spans the entire index, but the loop breaks after the
first entry, and `TestRangeStopsAtFirstBreak` proves that only one entry was
ever visited to produce it -- the same call over `rangeSliceNaive` cannot
make that claim, since building its return value requires visiting every
matching entry regardless of what the caller does with them.

### Tests

`TestNew` is the construction table: nil entries producing a valid empty
index, a single entry, and the two ways sortedness can be violated --
strictly decreasing and duplicated. `TestGet` covers present keys, absent
keys, and an empty index. `TestRangeHalfOpenWindow` is the half-open
interval table this lesson builds on throughout: a middle window, a lower
bound below the first key, an upper bound past the last, both shapes of an
empty window (`lo == hi` and `lo > hi`), and a window with no overlap at
all.

`TestRangeStopsAtFirstBreak` is the test that matters most: it counts how
many entries `Range` actually yields before an immediate `break`, and
separately builds the same window with `rangeSliceNaive`, asserting the
naive helper's result always has every matching entry -- it structurally
cannot do otherwise. `TestRangeValuesAliasBackingStorage` pins the aliasing
contract by mutating a yielded value and observing the change through a
subsequent `Get`. `TestIndexIsSafeForConcurrentUse` runs `Get` and `Range`
from multiple goroutines against a shared `Index` under `-race`.

Create `ssindex_test.go`:

```go
package ssindex

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func entries(pairs ...string) []Entry {
	es := make([]Entry, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		es = append(es, Entry{Key: pairs[i], Value: []byte(pairs[i+1])})
	}
	return es
}

// rangeSliceNaive is the range scan as it is usually written first: build
// the whole window into a slice, then let the caller loop over it. It is
// never exported and never reachable from Index's API; it exists only so
// the tests below can pin what it costs relative to Range's iter.Seq2 form.
func rangeSliceNaive(idx *Index, lo, hi string) []Entry {
	var out []Entry
	for i, k := range idx.keys {
		if k >= lo && k < hi {
			out = append(out, Entry{Key: k, Value: idx.vals[i]})
		}
	}
	return out
}

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []Entry
		wantLen int
		wantErr error
	}{
		{name: "nil entries produce an empty index", entries: nil, wantLen: 0},
		{name: "single entry", entries: entries("a", "1"), wantLen: 1},
		{name: "unsorted entries rejected", entries: entries("b", "1", "a", "2"), wantErr: ErrUnsorted},
		{name: "duplicate key rejected", entries: entries("a", "1", "a", "2"), wantErr: ErrDuplicateKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idx, err := New(tc.entries)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("New error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if idx.Len() != tc.wantLen {
				t.Fatalf("Len() = %d, want %d", idx.Len(), tc.wantLen)
			}
		})
	}
}

func TestGet(t *testing.T) {
	t.Parallel()

	idx, err := New(entries("a", "1", "c", "3", "e", "5"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		key    string
		want   string
		wantOK bool
	}{
		{key: "a", want: "1", wantOK: true},
		{key: "c", want: "3", wantOK: true},
		{key: "e", want: "5", wantOK: true},
		{key: "b", wantOK: false},
		{key: "z", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			v, ok := idx.Get(tc.key)
			if ok != tc.wantOK {
				t.Fatalf("Get(%q) ok = %v, want %v", tc.key, ok, tc.wantOK)
			}
			if ok && string(v) != tc.want {
				t.Fatalf("Get(%q) = %q, want %q", tc.key, v, tc.want)
			}
		})
	}
	empty, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	if _, ok := empty.Get("anything"); ok {
		t.Fatal("Get on empty index found something")
	}
}

func TestRangeHalfOpenWindow(t *testing.T) {
	t.Parallel()

	idx, err := New(entries("a", "1", "b", "2", "c", "3", "d", "4", "e", "5"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name     string
		lo, hi   string
		wantKeys []string
	}{
		{name: "middle window", lo: "b", hi: "d", wantKeys: []string{"b", "c"}},
		{name: "lo below first key", lo: "", hi: "c", wantKeys: []string{"a", "b"}},
		{name: "hi past last key", lo: "d", hi: "z", wantKeys: []string{"d", "e"}},
		{name: "empty window when lo == hi", lo: "c", hi: "c", wantKeys: nil},
		{name: "empty window when lo > hi", lo: "d", hi: "b", wantKeys: nil},
		{name: "no overlap past the end", lo: "x", hi: "z", wantKeys: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for k := range idx.Range(tc.lo, tc.hi) {
				got = append(got, k)
			}
			if len(got) != len(tc.wantKeys) {
				t.Fatalf("Range(%q,%q) = %v, want %v", tc.lo, tc.hi, got, tc.wantKeys)
			}
			for i, k := range tc.wantKeys {
				if got[i] != k {
					t.Fatalf("Range(%q,%q)[%d] = %q, want %q", tc.lo, tc.hi, i, got[i], k)
				}
			}
		})
	}
}

// TestRangeStopsAtFirstBreak is the heart of the module: a caller that
// breaks after the first result touches exactly one entry, however large
// the window is, because Range yields lazily instead of building a slice.
// rangeSliceNaive cannot offer that -- it must finish the whole window
// before the caller's loop even runs, so it always visits every match.
func TestRangeStopsAtFirstBreak(t *testing.T) {
	t.Parallel()

	es := entries("a", "1", "b", "2", "c", "3", "d", "4", "e", "5")
	idx, err := New(es)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	visited := 0
	for range idx.Range("a", "z") {
		visited++
		break
	}
	if visited != 1 {
		t.Fatalf("visited = %d after an immediate break, want 1", visited)
	}

	naive := rangeSliceNaive(idx, "a", "z")
	if len(naive) != len(es) {
		t.Fatalf("rangeSliceNaive visited %d entries building its result, want %d (it cannot stop early)", len(naive), len(es))
	}
}

// TestRangeValuesAliasBackingStorage pins the aliasing contract documented
// on Index: a value yielded by Range shares storage with the Index, so
// mutating it through the caller's reference is visible on the next read.
func TestRangeValuesAliasBackingStorage(t *testing.T) {
	t.Parallel()

	idx, err := New(entries("a", "1"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, v := range idx.Range("a", "b") {
		v[0] = 'X'
	}
	got, _ := idx.Get("a")
	if string(got) != "X" {
		t.Fatalf("Get(%q) = %q after mutating the yielded value, want %q", "a", got, "X")
	}
}

func TestIndexIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	es := make([]Entry, 50)
	for i := range es {
		es[i] = Entry{Key: fmt.Sprintf("k%04d", i), Value: []byte(fmt.Sprintf("v%d", i))}
	}
	idx, err := New(es)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			key := fmt.Sprintf("k%04d", g*5)
			if _, ok := idx.Get(key); !ok {
				t.Errorf("Get(%q) not found", key)
			}
			count := 0
			for range idx.Range("k0000", "k0050") {
				count++
			}
			if count != 50 {
				t.Errorf("Range count = %d, want 50", count)
			}
		}(g)
	}
	wg.Wait()
}

// ExampleIndex_Range is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleIndex_Range() {
	idx, err := New([]Entry{
		{Key: "block-000010", Value: []byte("offset:0")},
		{Key: "block-000020", Value: []byte("offset:4096")},
		{Key: "block-000030", Value: []byte("offset:8192")},
		{Key: "block-000040", Value: []byte("offset:12288")},
	})
	if err != nil {
		panic(err)
	}

	fmt.Println("point lookup:")
	if v, ok := idx.Get("block-000020"); ok {
		fmt.Println(" ", "block-000020", "->", string(v))
	}

	fmt.Println("range [block-000015, block-000035):")
	for k, v := range idx.Range("block-000015", "block-000035") {
		fmt.Println(" ", k, "->", string(v))
	}

	fmt.Println("first match only, then break:")
	for k := range idx.Range("block-000000", "block-999999") {
		fmt.Println(" ", k)
		break
	}

	// Output:
	// point lookup:
	//   block-000020 -> offset:4096
	// range [block-000015, block-000035):
	//   block-000020 -> offset:4096
	//   block-000030 -> offset:8192
	// first match only, then break:
	//   block-000010
}
```

## Review

The index is correct when `New` rejects anything that is not strictly
sorted -- `ErrUnsorted` for out-of-order keys, `ErrDuplicateKey` for a
repeated one -- so `Get` and `Range` never have to re-check the invariant
they depend on. The mechanism worth taking away is what `Range` returns:
`iter.Seq2[string, []byte]` instead of `[]Entry`. A slice-returning scan and
an `iter.Seq2`-returning scan can return byte-for-byte identical data and
still differ enormously in what they cost a caller who only needs part of
the result, because a slice is a finished value and an `iter.Seq2` is a
resumable computation that the caller's own loop drives one step at a time.
The trap is that a `Range` method can be implemented by materializing a
slice internally and handing it out one element at a time -- it compiles,
it returns correct values, and it throws away the entire reason to have
chosen the interface, which is exactly why the tests here measure how much
work happens before the first result rather than only checking the result
itself. Values returned by `Get` and yielded by `Range` alias the `Index`'s
storage, and the `Index` itself never changes after `New` returns, so it is
safe to share across goroutines without a mutex. Run
`go test -count=1 -race ./...`.

## Resources

- [`iter.Seq2`](https://pkg.go.dev/iter#Seq2) — the two-value iterator function type `Range` implements.
- [Go blog: Range over Function Types](https://go.dev/blog/range-functions) — how `range`-over-func desugars to calls into `yield`, and how `break` maps to `yield` returning `false`.
- [`sort.Search`](https://pkg.go.dev/sort#Search) — the lower-bound primitive used to locate both ends of the half-open window.
- [`slices.BinarySearch`](https://pkg.go.dev/slices#BinarySearch) — the point-lookup form `Get` uses.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-ttl-expiry-sweep.md](14-ttl-expiry-sweep.md) | Next: [16-wal-tail-reconciliation-compact.md](16-wal-tail-reconciliation-compact.md)
