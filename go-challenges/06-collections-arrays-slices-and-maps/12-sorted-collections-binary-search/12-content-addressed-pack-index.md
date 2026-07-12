# Exercise 12: Content-Addressed Pack Index With a Byte-Slice Comparator

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A content-addressed store like restic or a git packfile never asks "where is
object 7"; it asks "where is the blob whose SHA-256 hash is this exact
32-byte value". The store answers that question millions of times per
backup or `git fsync`, and the answer lives in a small in-memory index: for
every blob, which pack file holds it and at what byte offset. Building that
index once and searching it fast is the entire performance story of a
content-addressed system — get it wrong and a backup that should take
seconds scans gigabytes of pack headers instead.

Every other module in this lesson searches a slice whose key is a
`cmp.Ordered` scalar — an int, a string, a timestamp — where `<` is already
the right comparator. A content hash is a `[]byte`, and `[]byte` has no `<`
operator at all: comparing byte slices means comparing them lexicographically
byte by byte, which is exactly what `bytes.Compare` does and exactly what
`slices.BinarySearchFunc`'s comparator argument exists to plug in. This is
the one comparator shape the lesson has not yet exercised, and it is the
shape every content-addressed index in production actually uses.

This module builds `packindex`, a package that holds (hash, location) pairs
sorted by hash and answers `Lookup(hash)` in O(log n) via
`bytes.Compare`-based binary search. The naive alternative — scanning every
entry with `bytes.Equal` — is not part of that API. It lives only in the
tests, where it belongs, as the thing the tests prove too slow to ship.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
packindex/               module example.com/packindex
  go.mod                 go 1.24
  index.go               Location, Entry, PackIndex; NewPackIndex, Lookup, Len;
                         three sentinel errors
  index_test.go          lookup table, empty index, three constructor guards,
                         hash-cloning aliasing, the linear-scan contrast,
                         concurrency, ExamplePackIndex_Lookup
```

- Files: `index.go`, `index_test.go`.
- Implement: `NewPackIndex(entries []Entry) (*PackIndex, error)` cloning every `Hash` and rejecting `ErrEmptyHash`, `ErrNotSorted`, or `ErrDuplicateHash`; `(*PackIndex).Lookup(hash []byte) (Location, bool)` via `slices.BinarySearchFunc` with `bytes.Compare`; `(*PackIndex).Len() int`.
- Test: the first/middle/last entry, an unknown hash, an empty query hash, an empty index, the three constructor rejections, mutating the caller's original hash slice after construction leaving the index untouched, a comparison-count property showing the binary search costs strictly fewer comparisons than a linear `bytes.Equal` scan, concurrent `Lookup` calls, and `ExamplePackIndex_Lookup` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### `bytes.Compare` is the comparator; a linear scan is the failure mode

`slices.BinarySearchFunc(s, target, cmp)` needs a `cmp` that returns
negative, zero, or positive the way `cmp.Compare` does for ordered scalars —
but a `[]byte` has no natural `<`, so the comparator has to be
`bytes.Compare` itself: negative when the element's hash sorts before the
target, zero on an exact match, positive when it sorts after. Once the index
is genuinely sorted under that same comparator, a lookup among a million
entries costs about twenty comparisons.

The alternative that shows up in a first draft looks correct and passes
every functional test, because it *is* functionally correct — it is just
the wrong complexity class:

```go
func lookupSlow(entries []Entry, hash []byte) (Location, bool) {
    for _, e := range entries {
        if bytes.Equal(e.Hash, hash) {   // one full-length comparison per entry
            return e.Location, true
        }
    }
    return Location{}, false
}
```

At ten objects this is unmeasurable. At ten million objects — a real restic
repository after a few years of backups — it means reading and comparing
every hash in the index for every single blob fetch, turning an O(log n)
lookup into an O(n) one and a fast restore into one that times out. The fix
is not a smarter loop; it is sorting the index once, by the same comparator
the search will use, and never scanning past `log2(n)` entries again.

Create `index.go`:

```go
// Package packindex implements the in-memory index a content-addressed pack
// file store -- restic, git's packfiles -- needs to answer "which pack and
// offset holds this blob" without scanning every entry per lookup.
//
// The key is a content hash: a []byte, not a cmp.Ordered scalar, so ordering
// and searching both go through bytes.Compare rather than the ordinary "<"
// operator. That is the one comparator shape this package exists to teach.
package packindex

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
)

// Sentinel errors returned by NewPackIndex. Callers should test for them
// with errors.Is rather than by comparing error strings.
var (
	// ErrEmptyHash means an entry had a zero-length hash.
	ErrEmptyHash = errors.New("packindex: entry has empty hash")
	// ErrNotSorted means the input entries were not sorted by Hash.
	ErrNotSorted = errors.New("packindex: entries not sorted by hash")
	// ErrDuplicateHash means two entries carried the same hash.
	ErrDuplicateHash = errors.New("packindex: duplicate hash")
)

// Location identifies where a blob lives: which pack file, and the byte
// range within it.
type Location struct {
	PackID uint32
	Offset int64
	Length int64
}

// Entry is one (hash, location) pair as given to NewPackIndex.
type Entry struct {
	Hash     []byte
	Location Location
}

// entry is the internal, defensively-copied form of Entry.
type entry struct {
	hash []byte
	loc  Location
}

// PackIndex maps content hashes to the pack Location holding that content,
// via binary search over a table sorted by hash.
//
// A PackIndex is immutable after construction: NewPackIndex clones every
// hash, and no method mutates the stored entries. It is therefore safe for
// concurrent use by multiple goroutines without external synchronization.
type PackIndex struct {
	entries []entry
}

// NewPackIndex builds a PackIndex from entries, which must already be
// sorted by Hash under bytes.Compare and must not contain a zero-length or
// duplicate hash. Each Hash is cloned, so mutating the caller's original
// byte slices after this call cannot corrupt the index.
//
// It returns ErrEmptyHash, ErrNotSorted, or ErrDuplicateHash for input it
// cannot index. A nil or empty entries is valid and produces an empty
// PackIndex.
func NewPackIndex(entries []Entry) (*PackIndex, error) {
	es := make([]entry, len(entries))
	for i, e := range entries {
		if len(e.Hash) == 0 {
			return nil, fmt.Errorf("%w: at index %d", ErrEmptyHash, i)
		}
		es[i] = entry{hash: bytes.Clone(e.Hash), loc: e.Location}
	}
	if !slices.IsSortedFunc(es, func(a, b entry) int { return bytes.Compare(a.hash, b.hash) }) {
		return nil, ErrNotSorted
	}
	for i := 1; i < len(es); i++ {
		if bytes.Equal(es[i-1].hash, es[i].hash) {
			return nil, fmt.Errorf("%w: %x", ErrDuplicateHash, es[i].hash)
		}
	}
	return &PackIndex{entries: es}, nil
}

// Lookup reports the Location of the blob identified by hash, and whether
// it was found. It runs in O(log n) via slices.BinarySearchFunc with
// bytes.Compare as the comparator, never a linear scan.
func (idx *PackIndex) Lookup(hash []byte) (Location, bool) {
	i, found := slices.BinarySearchFunc(idx.entries, hash, func(e entry, target []byte) int {
		return bytes.Compare(e.hash, target)
	})
	if !found {
		return Location{}, false
	}
	return idx.entries[i].loc, true
}

// Len reports the number of entries held in the index.
func (idx *PackIndex) Len() int { return len(idx.entries) }
```

### Using it

`NewPackIndex` runs once, when a pack file or a whole repository index is
loaded, with entries already produced in sorted order by whatever wrote the
index to disk. `Lookup` is the hot path, called once per blob fetch, and it
never mutates `PackIndex`, so a single value can be shared by every
goroutine reading from the store concurrently — that is the contract
`TestPackIndexIsSafeForConcurrentUse` holds it to.

The cloning in `NewPackIndex` closes a real aliasing hole: a caller that
builds `Entry.Hash` from a reused buffer (a common pattern when streaming
pack headers) must be free to overwrite that buffer the moment
`NewPackIndex` returns. `TestNewPackIndexClonesHashes` pins that by zeroing
the original hash slice after construction and confirming `Lookup` still
finds the entry by its original value.

`ExamplePackIndex_Lookup` is the runnable demonstration of this module: `go
test` executes it and compares its stdout against the `// Output:` comment,
so the usage shown here cannot drift away from the code.

### Tests

`TestLookup` is the table: first, middle, and last entry in a 50-entry
index, an unknown hash, and an empty query hash, each checked against the
`(Location, bool)` pair `Lookup` returns. `TestLookupEmptyIndex` and the
three `TestNewPackIndexRejects*` tests cover the constructor's edge cases
and its three sentinel errors.

`TestBinarySearchComparesFewerThanLinearScan` is the heart of the module.
`lookupBuggyLinear` is unexported and unreachable from the package API; it
counts every `bytes.Equal` comparison a sequential scan makes, and a second
helper, `binarySearchCompares`, counts comparisons through the same
`bytes.Compare`-based algorithm `Lookup` uses. The test asserts a property —
`binary < linear` — never an exact count, because the precise number of
comparisons a binary search makes is an algorithmic fact, not a timing
measurement, and stays true across toolchains without depending on wall
clock at all.

Create `index_test.go`:

```go
package packindex

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// hashOf returns a deterministic 32-byte SHA-256 content hash for label.
func hashOf(label string) []byte {
	sum := sha256.Sum256([]byte(label))
	return sum[:]
}

// buildEntries returns n entries with deterministic, distinct hashes,
// sorted by Hash the way NewPackIndex requires.
func buildEntries(n int) []Entry {
	entries := make([]Entry, n)
	for i := 0; i < n; i++ {
		entries[i] = Entry{
			Hash:     hashOf(fmt.Sprintf("blob-%08d", i)),
			Location: Location{PackID: uint32(i % 4), Offset: int64(i) * 1024, Length: 512},
		}
	}
	slices.SortFunc(entries, func(a, b Entry) int { return bytes.Compare(a.Hash, b.Hash) })
	return entries
}

// lookupBuggyLinear is the mistake this module exists to prevent: a
// sequential bytes.Equal scan across every entry. It is never exported and
// never reachable from the package API; it exists so the tests can pin how
// many comparisons it costs relative to the real binary search.
func lookupBuggyLinear(entries []Entry, hash []byte) (Location, bool, int) {
	compares := 0
	for _, e := range entries {
		compares++
		if bytes.Equal(e.Hash, hash) {
			return e.Location, true, compares
		}
	}
	return Location{}, false, compares
}

// binarySearchCompares mirrors PackIndex.Lookup's own algorithm -- the same
// bytes.Compare-based binary search -- but counts how many comparisons it
// performs, so the O(log n) claim can be pinned as a property instead of
// asserted by fiat.
func binarySearchCompares(entries []Entry, hash []byte) int {
	compares := 0
	_, _ = slices.BinarySearchFunc(entries, hash, func(e Entry, target []byte) int {
		compares++
		return bytes.Compare(e.Hash, target)
	})
	return compares
}

func TestLookup(t *testing.T) {
	t.Parallel()

	entries := buildEntries(50)
	idx, err := NewPackIndex(entries)
	if err != nil {
		t.Fatalf("NewPackIndex: %v", err)
	}

	tests := []struct {
		name   string
		hash   []byte
		want   Location
		wantOK bool
	}{
		{name: "first entry", hash: entries[0].Hash, want: entries[0].Location, wantOK: true},
		{name: "middle entry", hash: entries[25].Hash, want: entries[25].Location, wantOK: true},
		{name: "last entry", hash: entries[49].Hash, want: entries[49].Location, wantOK: true},
		{name: "unknown hash", hash: hashOf("never-indexed"), wantOK: false},
		{name: "empty query hash", hash: []byte{}, wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := idx.Lookup(tc.hash)
			if ok != tc.wantOK {
				t.Fatalf("Lookup(%x) ok = %v, want %v", tc.hash, ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("Lookup(%x) = %+v, want %+v", tc.hash, got, tc.want)
			}
		})
	}
}

func TestLookupEmptyIndex(t *testing.T) {
	t.Parallel()

	idx, err := NewPackIndex(nil)
	if err != nil {
		t.Fatalf("NewPackIndex(nil): %v", err)
	}
	if _, ok := idx.Lookup(hashOf("anything")); ok {
		t.Fatalf("Lookup on empty index reported found")
	}
	if idx.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", idx.Len())
	}
}

func TestNewPackIndexRejectsEmptyHash(t *testing.T) {
	t.Parallel()

	entries := []Entry{{Hash: nil, Location: Location{}}}
	if _, err := NewPackIndex(entries); !errors.Is(err, ErrEmptyHash) {
		t.Fatalf("NewPackIndex error = %v, want ErrEmptyHash", err)
	}
}

func TestNewPackIndexRejectsUnsorted(t *testing.T) {
	t.Parallel()

	entries := buildEntries(5)
	entries[0], entries[4] = entries[4], entries[0]
	if _, err := NewPackIndex(entries); !errors.Is(err, ErrNotSorted) {
		t.Fatalf("NewPackIndex error = %v, want ErrNotSorted", err)
	}
}

func TestNewPackIndexRejectsDuplicateHash(t *testing.T) {
	t.Parallel()

	h := hashOf("dup")
	entries := []Entry{
		{Hash: h, Location: Location{PackID: 1}},
		{Hash: h, Location: Location{PackID: 2}},
	}
	if _, err := NewPackIndex(entries); !errors.Is(err, ErrDuplicateHash) {
		t.Fatalf("NewPackIndex error = %v, want ErrDuplicateHash", err)
	}
}

// TestNewPackIndexClonesHashes pins the aliasing contract: mutating the
// caller's original Hash slice after construction must not corrupt the
// index's stored entries.
func TestNewPackIndexClonesHashes(t *testing.T) {
	t.Parallel()

	original := hashOf("mutate-me")
	entries := []Entry{{Hash: original, Location: Location{PackID: 7}}}
	idx, err := NewPackIndex(entries)
	if err != nil {
		t.Fatalf("NewPackIndex: %v", err)
	}

	want := bytes.Clone(original)
	for i := range original {
		original[i] = 0
	}

	got, ok := idx.Lookup(want)
	if !ok {
		t.Fatalf("Lookup(%x) not found after caller mutated its own hash slice", want)
	}
	if got.PackID != 7 {
		t.Fatalf("Lookup(%x) = %+v, want PackID 7", want, got)
	}
}

// TestBinarySearchComparesFewerThanLinearScan is the heart of the module:
// it pins that the byte-slice binary search costs strictly fewer
// comparisons than a linear bytes.Equal scan over the same data, which is
// the property that makes a pack index usable at ten million objects
// instead of ten. No timing is involved; both counts come from an explicit
// comparator call counter, so the result is deterministic.
func TestBinarySearchComparesFewerThanLinearScan(t *testing.T) {
	t.Parallel()

	entries := buildEntries(4096)
	target := entries[len(entries)-1].Hash // worst case for the linear scan

	linear := binarySearchComparesLinear(entries, target)
	binary := binarySearchCompares(entries, target)

	if !(binary < linear) {
		t.Fatalf("comparisons: binary = %d, linear = %d; want binary < linear", binary, linear)
	}
}

// binarySearchComparesLinear counts the comparisons lookupBuggyLinear makes
// finding target, without needing its Location result.
func binarySearchComparesLinear(entries []Entry, target []byte) int {
	_, _, compares := lookupBuggyLinear(entries, target)
	return compares
}

func TestPackIndexIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	entries := buildEntries(200)
	idx, err := NewPackIndex(entries)
	if err != nil {
		t.Fatalf("NewPackIndex: %v", err)
	}

	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		i := i
		go func() {
			defer func() { done <- struct{}{} }()
			want := entries[i*10%len(entries)]
			got, ok := idx.Lookup(want.Hash)
			if !ok || got != want.Location {
				t.Errorf("Lookup(%x) = %+v, %v; want %+v, true", want.Hash, got, ok, want.Location)
			}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

// ExamplePackIndex_Lookup is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment
// below.
func ExamplePackIndex_Lookup() {
	entries := []Entry{
		{Hash: hashOf("blob-a"), Location: Location{PackID: 1, Offset: 0, Length: 128}},
		{Hash: hashOf("blob-b"), Location: Location{PackID: 1, Offset: 128, Length: 256}},
	}
	slices.SortFunc(entries, func(a, b Entry) int { return bytes.Compare(a.Hash, b.Hash) })

	idx, err := NewPackIndex(entries)
	if err != nil {
		panic(err)
	}

	if loc, ok := idx.Lookup(hashOf("blob-b")); ok {
		fmt.Printf("blob-b: pack=%d offset=%d length=%d\n", loc.PackID, loc.Offset, loc.Length)
	}
	if _, ok := idx.Lookup(hashOf("blob-missing")); !ok {
		fmt.Println("blob-missing: not found")
	}

	// Output:
	// blob-b: pack=1 offset=128 length=256
	// blob-missing: not found
}
```

## Review

`Lookup` is correct when it finds a hash in O(log n) using the same
`bytes.Compare` comparator the index was sorted with — never by scanning.
That comparator is the one shape this lesson had not yet used: a `[]byte`
key has no `<` operator, so ordering, sorting, and searching all have to go
through an explicit comparator function instead of `cmp.Ordered`. Around
that core, `NewPackIndex` clones every hash so a caller's reused buffer can
never corrupt the index after construction, and it refuses `ErrEmptyHash`,
`ErrNotSorted`, or `ErrDuplicateHash` rather than let `Lookup` silently
mis-answer a query. `PackIndex` is immutable after construction and
therefore safe to share across goroutines. The comparison-count test shows,
without any timing, why the naive `bytes.Equal` scan is not a smaller
version of the same idea but a different complexity class entirely — the
gap between the two only widens as a repository grows. Run
`go test -count=1 -race ./...` to confirm the lookup table, the constructor
guards, the aliasing guarantee, the comparison-count property, and the
concurrent-use property.

## Resources

- [`bytes.Compare`](https://pkg.go.dev/bytes#Compare) — the lexicographic comparator this index sorts and searches with.
- [`slices.BinarySearchFunc`](https://pkg.go.dev/slices#BinarySearchFunc) — the generic search that accepts a comparator for keys with no natural `<`.
- [`bytes.Clone`](https://pkg.go.dev/bytes#Clone) — how `NewPackIndex` severs aliasing to the caller's original hash slices.
- [restic design: the index](https://restic.readthedocs.io/en/stable/100_references.html#the-index) — a production content-addressed store documenting the same pack/offset lookup this module models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-dns-rrset-equal-range.md](11-dns-rrset-equal-range.md) | Next: [13-sliding-window-rate-limiter.md](13-sliding-window-rate-limiter.md)
