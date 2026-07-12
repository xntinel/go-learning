# Exercise 13: Sizing make(map[K]V, n) to Skip Incremental Rehashing

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Loading a reference table — currency codes, a country list, a product
catalog snapshot fetched in one bulk query — into a lookup index is a
one-shot batch job with a property most callers ignore: the exact number of
rows is already known before the indexing loop starts. `make(map[K]V)` with
no size hint starts the map at its smallest bucket size and grows the
backing table incrementally as entries are inserted, reallocating and
rehashing every existing entry each time the table crosses a growth
threshold. When `n` is known up front, that repeated work is pure waste —
`make(map[K]V, n)` sizes the table for `n` entries in one allocation, and the
loop that follows never triggers a single rehash. This exercise proves the
difference is real, not folklore, using `testing.AllocsPerRun`, and wraps it
in a small index type that also catches the mistake a reference-table load
is most likely to hide: a duplicate key silently overwriting an earlier row.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
idxbuild/                     module example.com/idxbuild
  go.mod                      go 1.24
  index.go                    Record, Index; NewIndex, Lookup, Len; two sentinel errors
  index_test.go                 correctness, empty batch, empty/duplicate ID, aliasing,
                               concurrency, AllocsPerRun comparison, ExampleNewIndex
```

- Files: `index.go`, `index_test.go`.
- Implement: `Record{ID string; Value int}`; `NewIndex(records []Record) (*Index, error)` building its backing map with `make(map[string]Record, len(records))` and rejecting an empty `ID` with `ErrEmptyID` or a repeated `ID` with `ErrDuplicateID`; `(*Index).Lookup(id string) (Record, bool)`; `(*Index).Len() int`.
- Test: a full batch indexed and looked up correctly; an empty batch; empty-ID rejection at the first, middle, and last position; duplicate-ID rejection both adjacent and far apart; the index not aliasing the input slice; concurrent `Lookup`/`Len` under `-race`; `testing.AllocsPerRun` over a 4000-record batch showing the hinted build allocates strictly fewer times than an unhinted one; and `ExampleNewIndex` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/13-prealloc-map-index-build
cd go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/13-prealloc-map-index-build
go mod edit -go=1.24
```

### Why a known count changes what make should do

`make(map[K]V)` and `make(map[K]V, n)` build the exact same kind of map and
are interchangeable for correctness — every test that only checks the
resulting entries would pass either way. The difference is entirely about
how the backing hash table gets to its final size. Without a hint, the
runtime starts small and grows the table by doubling (roughly) each time the
load factor crosses its threshold; every growth step allocates a new,
larger table and rehashes every entry already in the map into it. A loop
inserting 4000 records into an unhinted map does not just do 4000 inserts —
it does 4000 inserts *plus* several full rehash passes over an
ever-growing prefix of those entries, because growth is triggered by
insertion count, not known in advance:

```go
byID := make(map[string]Record)   // starts tiny, grows and rehashes repeatedly
for _, r := range records {
    byID[r.ID] = r
}
```

`make(map[K]V, n)` tells the runtime the target size before the first insert,
so it allocates a table sized for `n` entries immediately and every
subsequent insert lands directly, with zero growth events. This is exactly
the same idea as `make([]T, 0, n)` for a slice you are about to `append` to
`n` times — the hint is not a promise the runtime enforces, it is a sizing
input that lets it skip work it would otherwise have to redo. The rule of
thumb: whenever the number of entries is known before the loop that fills the
map — a bulk fetch that reports a row count, a batch job over a fixed slice,
deserializing a length-prefixed wire format — pass it to `make`.

A reference-table load has a second, independent failure mode worth guarding
in the same pass: two rows sharing the same key. A bare `byID[r.ID] = r`
loop makes the second row win silently, with no signal that the first was
ever discarded — a currency code appearing twice in a vendor feed becomes a
silently truncated table instead of a caught data error. `NewIndex` checks
for that before it ever accepts the write.

Create `index.go`:

```go
// Package idxbuild builds a read-only, ID-keyed lookup index from a
// known-size batch of records -- the shape of loading a reference table
// (currency codes, country codes, a product catalog snapshot) after a bulk
// fetch that already reports how many rows it returned.
package idxbuild

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by NewIndex. Callers should test for them with
// errors.Is.
var (
	// ErrEmptyID means a record's ID field was the empty string.
	ErrEmptyID = errors.New("idxbuild: record ID must not be empty")
	// ErrDuplicateID means two records in the batch shared the same ID.
	ErrDuplicateID = errors.New("idxbuild: duplicate record ID")
)

// Record is one row of the source batch, keyed by ID once indexed.
type Record struct {
	ID    string
	Value int
}

// Index is a read-only, ID-keyed lookup built from a batch of Records.
//
// Index is safe for concurrent use by multiple goroutines: it is never
// mutated after NewIndex returns, so any number of readers may call Lookup
// and Len concurrently without synchronization.
type Index struct {
	byID map[string]Record
}

// NewIndex builds an Index from records. It preallocates the backing map
// with make(map[string]Record, len(records)) -- the exact target size is
// already known before the loop starts, so the table is sized once and the
// insert loop below never triggers an incremental growth-and-rehash step.
//
// NewIndex returns ErrEmptyID if any record's ID is empty, and
// ErrDuplicateID if two records share an ID: silently letting the second
// overwrite the first would hide a data-integrity bug in the source batch
// instead of surfacing it.
//
// Each Record is copied into the index by value, so the returned Index
// never aliases records: mutating the input slice after NewIndex returns
// has no effect on the index.
func NewIndex(records []Record) (*Index, error) {
	byID := make(map[string]Record, len(records))
	for i, r := range records {
		if r.ID == "" {
			return nil, fmt.Errorf("%w: record %d", ErrEmptyID, i)
		}
		if _, exists := byID[r.ID]; exists {
			return nil, fmt.Errorf("%w: %q at record %d", ErrDuplicateID, r.ID, i)
		}
		byID[r.ID] = r
	}
	return &Index{byID: byID}, nil
}

// Lookup returns the record stored under id and whether it was present.
func (idx *Index) Lookup(id string) (Record, bool) {
	r, ok := idx.byID[id]
	return r, ok
}

// Len reports the number of records in the index.
func (idx *Index) Len() int {
	return len(idx.byID)
}
```

### Using it

Call `NewIndex` once, right after the bulk fetch that produced `records`,
while the exact count is still in hand — that is the only moment the size
hint is worth anything. The returned `*Index` is read-only from that point
on: there is no `Set` or `Add`, so once construction succeeds there is
nothing left that could invalidate the "never grows again" guarantee, and
`Lookup`/`Len` can be called from as many goroutines as the service needs
without a mutex. A caller that needs to reload the table on a schedule
simply builds a fresh `Index` and swaps the pointer.

The module has no `main.go`, because an index builder is a library, not a
tool. Its executable demonstration is `ExampleNewIndex`: `go test` runs it
and compares its standard output against the `// Output:` comment, so the
usage shown below cannot drift away from the code.

### Tests

`TestNewIndexBuildsCompleteIndex` and `TestNewIndexOnEmptyBatch` are the
correctness baseline: a full batch indexes and looks up exactly right, and a
`nil` batch produces a valid, empty, non-panicking `Index`.
`TestNewIndexRejectsEmptyID` and `TestNewIndexRejectsDuplicateID` are tables
covering the position the bad record appears at — first, middle, last, and
adjacent versus far-apart duplicates — because an off-by-one in the loop
that reports the failing index would otherwise hide in only one of those
shapes. `TestNewIndexDoesNotAliasInputSlice` pins the aliasing contract from
`NewIndex`'s doc comment: mutating `records` after construction must never
be visible through `Lookup`. `TestIndexConcurrentLookups` drives `Lookup` and
`Len` from many goroutines under `-race`, holding `Index` to the concurrency
contract its doc comment promises.

`TestPreallocReducesAllocations` is where the sizing claim gets proven:
`buildIndexNoHint` is unexported and lives only in the test file, doing
exactly what an unhinted `make(map[string]Record)` loop does, and the test
runs both it and `NewIndex` through `testing.AllocsPerRun` over the same
4000-record batch. It asserts only the property `hinted < noHint`, never an
exact count — the runtime's growth curve is not a documented contract and
has changed between Go releases — and it deliberately skips `t.Parallel`,
because `testing.AllocsPerRun` panics if it runs from a parallel test.

Create `index_test.go`:

```go
package idxbuild

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// makeRecords builds n deterministic records with IDs "rec-0000".."rec-NNNN".
func makeRecords(n int) []Record {
	records := make([]Record, n)
	for i := range n {
		records[i] = Record{ID: fmt.Sprintf("rec-%04d", i), Value: i * 2}
	}
	return records
}

// buildIndexNoHint is the version most engineers write first: it builds the
// exact same index but without telling make how many entries to expect. The
// map starts at its smallest bucket size and grows incrementally, so the
// runtime repeatedly allocates a larger backing table and rehashes every
// existing entry as the map crosses each growth threshold. It is never
// exported and never reachable from the package API; it exists only so the
// allocation test below can pin the cost NewIndex's size hint avoids.
func buildIndexNoHint(records []Record) map[string]Record {
	byID := make(map[string]Record)
	for _, r := range records {
		byID[r.ID] = r
	}
	return byID
}

func TestNewIndexBuildsCompleteIndex(t *testing.T) {
	t.Parallel()

	records := makeRecords(50)
	idx, err := NewIndex(records)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if idx.Len() != len(records) {
		t.Fatalf("Len() = %d, want %d", idx.Len(), len(records))
	}
	for _, r := range records {
		got, ok := idx.Lookup(r.ID)
		if !ok || got != r {
			t.Fatalf("Lookup(%q) = (%+v, %v), want (%+v, true)", r.ID, got, ok, r)
		}
	}
}

func TestNewIndexOnEmptyBatch(t *testing.T) {
	t.Parallel()

	idx, err := NewIndex(nil)
	if err != nil {
		t.Fatalf("NewIndex(nil): %v", err)
	}
	if idx.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", idx.Len())
	}
	if _, ok := idx.Lookup("anything"); ok {
		t.Fatal("Lookup on an empty index should report absent")
	}
}

func TestNewIndexRejectsEmptyID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []Record
	}{
		{"empty ID first", []Record{{ID: ""}, {ID: "b"}}},
		{"empty ID in the middle", []Record{{ID: "a"}, {ID: ""}, {ID: "c"}}},
		{"empty ID last", []Record{{ID: "a"}, {ID: "b"}, {ID: ""}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := NewIndex(tc.records); !errors.Is(err, ErrEmptyID) {
				t.Fatalf("err = %v, want ErrEmptyID", err)
			}
		})
	}
}

func TestNewIndexRejectsDuplicateID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []Record
	}{
		{"duplicate adjacent", []Record{{ID: "a", Value: 1}, {ID: "a", Value: 2}}},
		{"duplicate far apart", []Record{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "a"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := NewIndex(tc.records); !errors.Is(err, ErrDuplicateID) {
				t.Fatalf("err = %v, want ErrDuplicateID", err)
			}
		})
	}
}

// TestNewIndexDoesNotAliasInputSlice proves mutating records after NewIndex
// returns never changes the built Index: each Record is a plain value with
// no pointer or slice fields, so storing it in the map copies it, and the
// backing array behind records is never retained.
func TestNewIndexDoesNotAliasInputSlice(t *testing.T) {
	t.Parallel()

	records := []Record{{ID: "usd", Value: 1}}
	idx, err := NewIndex(records)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	records[0].Value = 999
	got, ok := idx.Lookup("usd")
	if !ok || got.Value != 1 {
		t.Fatalf("Lookup(usd) = (%+v, %v), want ({usd 1}, true); mutating the input slice must not affect the index", got, ok)
	}
}

// TestIndexConcurrentLookups drives Lookup and Len from many goroutines at
// once, under -race: Index's doc comment promises safety for concurrent use
// because it is never mutated after NewIndex returns, and this is what
// holds it to that.
func TestIndexConcurrentLookups(t *testing.T) {
	t.Parallel()

	records := makeRecords(200)
	idx, err := NewIndex(records)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := records[i%len(records)]
			got, ok := idx.Lookup(r.ID)
			if !ok || got != r {
				t.Errorf("Lookup(%q) = (%+v, %v), want (%+v, true)", r.ID, got, ok, r)
			}
			if idx.Len() != len(records) {
				t.Errorf("Len() = %d, want %d", idx.Len(), len(records))
			}
		}(i)
	}
	wg.Wait()
}

// TestPreallocReducesAllocations proves the production claim: sizing the
// map up front with make(map[K]V, n) causes fewer allocations than letting
// it grow incrementally, because the incremental path repeatedly allocates
// a larger backing table and rehashes existing entries as it crosses each
// growth threshold. Only the property exact < noHint is asserted -- never a
// specific count -- because the runtime's growth curve is not a documented
// contract and has changed between Go releases.
//
// This test deliberately does not call t.Parallel: testing.AllocsPerRun
// panics when run from a parallel test, because a concurrent goroutine
// allocating in the background would corrupt its measurement.
func TestPreallocReducesAllocations(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation counting is slow under -short")
	}

	records := makeRecords(4000)

	hintedAllocs := testing.AllocsPerRun(20, func() {
		_, _ = NewIndex(records)
	})
	noHintAllocs := testing.AllocsPerRun(20, func() {
		buildIndexNoHint(records)
	})

	if !(hintedAllocs < noHintAllocs) {
		t.Fatalf("allocations: hinted = %v, no-hint = %v; want hinted strictly fewer", hintedAllocs, noHintAllocs)
	}
}

// ExampleNewIndex is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleNewIndex() {
	records := []Record{
		{ID: "usd", Value: 1},
		{ID: "eur", Value: 2},
		{ID: "gbp", Value: 3},
	}

	idx, err := NewIndex(records)
	if err != nil {
		panic(err)
	}
	fmt.Println("len:", idx.Len())

	if r, ok := idx.Lookup("eur"); ok {
		fmt.Printf("eur: %+v\n", r)
	}
	if _, ok := idx.Lookup("jpy"); !ok {
		fmt.Println("jpy: not found")
	}

	if _, err := NewIndex([]Record{{ID: "usd"}, {ID: "usd"}}); errors.Is(err, ErrDuplicateID) {
		fmt.Println("duplicate rejected:", err)
	}

	// Output:
	// len: 3
	// eur: {ID:eur Value:2}
	// jpy: not found
	// duplicate rejected: idxbuild: duplicate record ID: "usd" at record 1
}
```

## Review

`NewIndex` is correct index-building; the size hint is about the cost of
getting there, and the duplicate/empty-ID checks are about the correctness
of what gets there. `TestPreallocReducesAllocations` is the test that would
catch a regression where someone "simplifies" `NewIndex` back to a bare
`make(map[string]Record)` — the correctness tests would still pass, silently,
while the whole point of the size hint quietly disappeared; it asserts only
`hinted < noHint`, never a specific allocation count, because the runtime's
growth curve is not part of Go's contract. The duplicate-ID guard exists
because a bulk load has no other place to catch a repeated key: a bare
`byID[r.ID] = r` loop makes corruption invisible. `Index` never exposes a
mutator once built, so `Lookup` and `Len` are safe for any number of
concurrent readers with no locking. Run `go test -count=1 -race ./...`.

## Resources

- [testing.AllocsPerRun](https://pkg.go.dev/testing#AllocsPerRun) — the measurement this exercise's allocation test is built on.
- [Go Specification: Make](https://go.dev/ref/spec#Making_slices,_maps_and_channels) — the size-hint argument's effect on `make` for maps.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-delete-during-range-safe-sweep.md](12-delete-during-range-safe-sweep.md) | Next: [14-map-of-slices-append-grouping.md](14-map-of-slices-append-grouping.md)
