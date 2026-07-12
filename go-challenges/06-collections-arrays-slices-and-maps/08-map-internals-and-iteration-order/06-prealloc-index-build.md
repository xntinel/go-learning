# Exercise 6: Build an In-Memory Repository Index with a Capacity Hint

Turning a slice of records into a `map[ID]Record` for O(1) lookup is one of the most
common things a repository layer does. Build it without a size hint and you pay for
repeated table splits and rehashes as the map grows; build it with `make(map, len)` and
the map is sized once. This module builds the index and *measures* the difference with a
benchmark rather than asserting the win from folklore.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo, tests, and benchmarks. Nothing here imports any other exercise.

## What you'll build

```text
index/                     independent module: example.com/index
  go.mod                   go 1.26
  index.go                 type Record; BuildHinted, BuildUnhinted, Lookup
  cmd/
    demo/
      main.go              indexes a few records, looks two up
  index_test.go            correctness + BenchmarkBuildHinted / BuildUnhinted / Lookup
```

- Files: `index.go`, `cmd/demo/main.go`, `index_test.go`.
- Implement: `BuildHinted(records) map[int]Record` using `make(map, len(records))`,
  `BuildUnhinted` using a bare literal, and `Lookup(index, id) (Record, bool)`.
- Test: one entry per unique ID, last-writer-wins on duplicate IDs, comma-ok false for
  missing; both builders produce equal contents; benchmarks with `b.Loop` and
  `b.ReportAllocs`.
- Verify: `go test -count=1 -race ./...` and `go test -bench=. -benchmem`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/06-prealloc-index-build/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/06-prealloc-index-build
```

### Why the capacity hint pays

A map grows by crossing load-factor thresholds: each time it fills past the threshold,
the runtime allocates more table capacity and rehashes the affected entries. Building an
index from a slice of known length `n` without a hint starts the map small and walks it
through several of those grow-and-rehash steps, each of which allocates and moves
entries. `make(map[int]Record, n)` tells the runtime up front roughly how many entries
are coming, so it allocates enough Swiss tables in one shot and the inserts land in a
structure that never has to split during the build. The saving is fewer allocations and
no rehash work — visible directly in `-benchmem`'s allocs/op column.

The hint is advisory, not a limit: a map built with `make(map, n)` can still hold more
than `n` entries (it just grows again past that point), and both builders produce the
*same* final map for the same input. The hint changes the build cost, never the
contents — which is exactly why a correctness test asserts the two builders agree while
a benchmark measures that they cost differently.

Duplicate IDs are last-writer-wins: `m[r.ID] = r` overwrites, so if two records share an
ID the later one in the slice survives, and the final map has one entry per *unique* ID.
That is the natural repository semantic (a later row supersedes an earlier one) and the
test pins it.

Create `index.go`:

```go
package index

// Record is a stored row keyed by ID.
type Record struct {
	ID   int
	Name string
}

// BuildHinted indexes records by ID, presizing the map with make(map, len) so the
// build does not repeatedly grow and rehash. Duplicate IDs are last-writer-wins.
func BuildHinted(records []Record) map[int]Record {
	m := make(map[int]Record, len(records))
	for _, r := range records {
		m[r.ID] = r
	}
	return m
}

// BuildUnhinted indexes the same records with no capacity hint, so the map grows
// through several rehash steps. It produces the same contents as BuildHinted.
func BuildUnhinted(records []Record) map[int]Record {
	m := map[int]Record{}
	for _, r := range records {
		m[r.ID] = r
	}
	return m
}

// Lookup returns the record for id with presence semantics.
func Lookup(index map[int]Record, id int) (Record, bool) {
	r, ok := index[id]
	return r, ok
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/index"
)

func main() {
	records := []index.Record{
		{ID: 1, Name: "alice"},
		{ID: 2, Name: "bob"},
		{ID: 1, Name: "alice-v2"}, // duplicate ID: later wins
	}
	idx := index.BuildHinted(records)

	fmt.Println("size:", len(idx))
	if r, ok := index.Lookup(idx, 1); ok {
		fmt.Println("id 1:", r.Name)
	}
	if _, ok := index.Lookup(idx, 99); !ok {
		fmt.Println("id 99: not found")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
size: 2
id 1: alice-v2
id 99: not found
```

### Tests and benchmarks

The correctness test pins one entry per unique ID, last-writer-wins, presence semantics,
and that both builders agree. The benchmarks use `b.Loop` (Go 1.24), which runs the body
an appropriate number of times, keeps it from being optimized away, and excludes setup
before the loop from the timing automatically; `b.ReportAllocs()` adds the allocs/op
column where the hint's win shows up.

Create `index_test.go`:

```go
package index

import (
	"maps"
	"testing"
)

func sample(n int) []Record {
	records := make([]Record, n)
	for i := range n {
		records[i] = Record{ID: i, Name: "r"}
	}
	return records
}

func TestBuildIndexesByID(t *testing.T) {
	t.Parallel()

	idx := BuildHinted([]Record{{1, "a"}, {2, "b"}, {3, "c"}})
	if len(idx) != 3 {
		t.Fatalf("len = %d, want 3", len(idx))
	}
	if r, ok := Lookup(idx, 2); !ok || r.Name != "b" {
		t.Fatalf("Lookup(2) = %+v,%v; want {2 b},true", r, ok)
	}
	if _, ok := Lookup(idx, 999); ok {
		t.Fatal("Lookup(999) should be absent")
	}
}

func TestDuplicateIDLastWriterWins(t *testing.T) {
	t.Parallel()

	idx := BuildHinted([]Record{{1, "first"}, {1, "second"}})
	if len(idx) != 1 {
		t.Fatalf("len = %d, want 1 (unique IDs)", len(idx))
	}
	if idx[1].Name != "second" {
		t.Fatalf("id 1 = %q, want second", idx[1].Name)
	}
}

func TestBuildersAgree(t *testing.T) {
	t.Parallel()

	records := sample(1000)
	if !maps.Equal(BuildHinted(records), BuildUnhinted(records)) {
		t.Fatal("hinted and unhinted builds differ; the hint must not change contents")
	}
}

func BenchmarkBuildHinted(b *testing.B) {
	records := sample(10000)
	b.ReportAllocs()
	for b.Loop() {
		_ = BuildHinted(records)
	}
}

func BenchmarkBuildUnhinted(b *testing.B) {
	records := sample(10000)
	b.ReportAllocs()
	for b.Loop() {
		_ = BuildUnhinted(records)
	}
}

func BenchmarkLookup(b *testing.B) {
	idx := BuildHinted(sample(10000))
	b.ReportAllocs()
	var sink Record
	for b.Loop() {
		sink, _ = Lookup(idx, 4242)
	}
	_ = sink
}
```

Run the benchmarks:

```bash
go test -bench=. -benchmem
```

On the hinted build you should see materially fewer allocs/op than the unhinted one —
the rehash allocations the hint avoids. The exact numbers are machine-dependent, which
is why the test asserts *equal contents* and lets the benchmark *report* the cost rather
than hard-coding a threshold that would flake across machines.

## Review

The index is correct when it has one entry per unique ID with last-writer-wins and
comma-ok reporting absence — all pinned by the correctness test. The performance claim is
kept honest by construction: `TestBuildersAgree` proves the hint changes cost, not
contents, and the benchmark (not an assertion) is where you observe the allocation drop.
`b.Loop` is the Go 1.24 benchmark form; it subsumes the older `b.ResetTimer()` before a
manual `for range b.N` loop and prevents the compiler from eliding the build. Run
`go test -race` for the correctness test and `go test -bench=. -benchmem` to see the
hint pay off.

## Resources

- [Go blog: Faster Go maps with Swiss Tables](https://go.dev/blog/swisstable) — why growth and load factor cost what they do.
- [`testing.B.Loop`](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop.
- [`testing.B.ReportAllocs`](https://pkg.go.dev/testing#B.ReportAllocs) — the allocs/op column.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-concurrent-counter-map.md](07-concurrent-counter-map.md)
