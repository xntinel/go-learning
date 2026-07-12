# Exercise 5: A Batch Indexer Taking Stable Addresses With &records[i]

Building a `map[string]*Record` index over a `[]Record` is a routine batch job — a
lookup table into rows loaded from a database or a file. The trap is taking the
address of the range *value* (`&v`) instead of the slice element (`&records[i]`): the
former points at a per-iteration copy, so mutating through the index never touches
the backing slice. This exercise builds the correct indexer and contrasts it with
the buggy one under the race detector.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
recordindex/              independent module: example.com/recordindex
  go.mod
  index.go                Record; IndexByID (correct, &records[i]); indexByValueBug
  cmd/
    demo/
      main.go             build the index, mutate through it, print the slice
  index_test.go           correct pointers distinct + write-through; bug reproduced; -race
```

Files: `index.go`, `cmd/demo/main.go`, `index_test.go`.
Implement: a `Record` struct, `IndexByID([]Record) map[string]*Record` using `&records[i]`, and `indexByValueBug([]Record) map[string]*Record` using `&v` from range to demonstrate the trap.
Test: the correct index's pointers are distinct and write through to the slice; the buggy index's pointers all alias one copy (all point to the last element); run under `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/05-pointers-to-structs/05-slice-index-address-indexer/cmd/demo
cd go-solutions/09-pointers/05-pointers-to-structs/05-slice-index-address-indexer
```

### `&records[i]` addresses the element; `&v` addresses a copy

A range loop over a slice of structs, `for i, v := range records`, binds `v` to a
*copy* of each element. Go 1.22 made `v` a fresh variable per iteration, which fixed
the notorious pre-1.22 bug where every `&v` was the *same* address (the one reused
loop variable), so a `map[string]*Record` built with `&v` had every entry pointing at
the final element. But 1.22 did not make `&v` point at the slice element — it points
at that iteration's copy. So a post-1.22 `&v` index has *distinct* pointers, but each
points at a throwaway copy that the loop discards, not at `records[i]`. Mutating
through such a pointer changes a copy nobody else can see; the backing slice is
untouched.

To index *into* the slice you must take `&records[i]`. The element of a slice is
addressable (unlike a map element), and `&records[i]` is a stable pointer into the
backing array: writing `idx[id].Field = x` mutates `records[i]` in place, and a
later read of `records[i]` sees it. That write-through is exactly what an index is
for — it lets you find and update a row by key without a linear scan.

To make the trap concrete, this exercise keeps the buggy variant using `&v` and a
test that reproduces its failure mode. On Go 1.22+ the buggy index's pointers are
distinct (per-iteration copies) but *none* of them write through to the slice — that
is the observable bug the test pins. (If this ran on a pre-1.22 toolchain the same
`&v` code would additionally collapse every pointer onto the last element; the write
through failure is the version-independent symptom.)

Create `index.go`:

```go
package recordindex

type Record struct {
	ID   string
	Name string
	Hits int
}

// IndexByID builds a lookup from ID to a pointer INTO the backing slice, using
// &records[i]. Mutations through the returned pointers write through to records.
func IndexByID(records []Record) map[string]*Record {
	idx := make(map[string]*Record, len(records))
	for i := range records {
		idx[records[i].ID] = &records[i]
	}
	return idx
}

// indexByValueBug is the WRONG version: &v addresses the range loop's per-iteration
// copy, not records[i]. Mutations through these pointers do not reach the slice.
// Kept only to demonstrate the trap; do not use this shape.
func indexByValueBug(records []Record) map[string]*Record {
	idx := make(map[string]*Record, len(records))
	for _, v := range records {
		idx[v.ID] = &v
	}
	return idx
}
```

### The runnable demo

The demo builds the correct index, bumps a record's `Hits` through the index, and
prints the backing slice to show the write went through.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/recordindex"
)

func main() {
	records := []recordindex.Record{
		{ID: "a", Name: "alpha"},
		{ID: "b", Name: "bravo"},
		{ID: "c", Name: "charlie"},
	}

	idx := recordindex.IndexByID(records)
	idx["b"].Hits += 5 // write through the index into the slice

	for _, r := range records {
		fmt.Printf("%s %s hits=%d\n", r.ID, r.Name, r.Hits)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
a alpha hits=0
b bravo hits=5
c charlie hits=0
```

### Tests

`TestIndexWritesThrough` mutates a record through the correct index and asserts the
backing slice changed — the property that defines a real index. `TestIndexPointsAtElements`
asserts each `idx[id]` equals `&records[i]` (address identity). `TestValueBugDoesNotWriteThrough`
reproduces the trap: mutating through the buggy index leaves the slice unchanged.
Running under `-race` confirms the correct index shares the backing array safely for
reads.

Create `index_test.go`:

```go
package recordindex

import (
	"testing"
)

func sample() []Record {
	return []Record{
		{ID: "a", Name: "alpha"},
		{ID: "b", Name: "bravo"},
		{ID: "c", Name: "charlie"},
	}
}

func TestIndexWritesThrough(t *testing.T) {
	t.Parallel()
	records := sample()
	idx := IndexByID(records)

	idx["b"].Hits = 9

	if records[1].Hits != 9 {
		t.Fatalf("write through index did not reach slice: records[1].Hits = %d, want 9", records[1].Hits)
	}
}

func TestIndexPointsAtElements(t *testing.T) {
	t.Parallel()
	records := sample()
	idx := IndexByID(records)

	for i := range records {
		if idx[records[i].ID] != &records[i] {
			t.Fatalf("idx[%q] does not point at &records[%d]", records[i].ID, i)
		}
	}
	// Distinct elements -> distinct pointers.
	if idx["a"] == idx["b"] {
		t.Fatal("distinct records must have distinct pointers")
	}
}

func TestValueBugDoesNotWriteThrough(t *testing.T) {
	t.Parallel()
	records := sample()
	bug := indexByValueBug(records)

	bug["b"].Hits = 9 // mutates a per-iteration copy, not the slice

	if records[1].Hits != 0 {
		t.Fatalf("expected the &v bug to NOT write through; records[1].Hits = %d", records[1].Hits)
	}
}

func TestValueBugPointersAreNotSliceElements(t *testing.T) {
	t.Parallel()
	records := sample()
	bug := indexByValueBug(records)

	for i := range records {
		if bug[records[i].ID] == &records[i] {
			t.Fatalf("bug index unexpectedly points at &records[%d]; the &v trap should not", i)
		}
	}
}
```

## Review

The indexer is correct when a mutation through `idx[id]` is visible in the backing
slice and each `idx[id]` is the address of the matching element (`&records[i]`). The
write-through test is the decisive one: an index whose pointers do not reach the
slice is not an index, it is a map of orphaned copies.

The mistake is `&v` in a range loop. On Go 1.22+ it no longer aliases every entry to
the last element, but it still points at a per-iteration copy, so it silently fails
to write through — a bug the compiler will not flag and tests must catch. Always take
`&records[i]` (index form) to point into a slice. Note that you cannot do the
analogous `&m[k]` for a map: map elements are not addressable because rehashing can
move them, so a map of structs must be indexed differently (store `map[K]*V` from the
start, or keep the structs in a slice and index that).

## Resources

- [Go Wiki: LoopvarExperiment](https://go.dev/wiki/LoopvarExperiment) — the Go 1.22 per-iteration loop variable change and what it does and does not fix.
- [Go Specification: Address operators](https://go.dev/ref/spec#Address_operators) — what is addressable, including slice elements but not map elements.
- [Effective Go: Slices](https://go.dev/doc/effective_go#slices) — slice/backing-array semantics behind `&records[i]`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-lru-cache-pointer-nodes.md](06-lru-cache-pointer-nodes.md)
