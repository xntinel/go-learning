# Exercise 6: Building a Map from a Streaming Source

Loading a table into an in-memory index — DB rows, CSV lines, a Kafka batch — is a
stream-to-map fold. Go 1.23's range-over-func iterators make the producer an
`iter.Seq2[K, V]`, and `maps.Collect` / `maps.Insert` turn that stream into a keyed
index. This module builds the indexer and pins the collision semantics that decide
which duplicate row wins.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It gates alone.

## What you'll build

```text
streamindex/                independent module: example.com/streamindex
  go.mod                    go 1.26
  streamindex.go            Record, rowsSeq, Index (Collect), MergeInto (Insert)
  cmd/
    demo/
      main.go               index a stream, merge a second batch, print result
  streamindex_test.go       last-writer-wins, empty seq, All->Collect round-trip
```

Files: `streamindex.go`, `cmd/demo/main.go`, `streamindex_test.go`.
Implement: `Index(seq iter.Seq2[string, Record]) map[string]Record` via `maps.Collect`; `MergeInto(dst, seq)` via `maps.Insert`.
Test: duplicate keys resolve last-writer-wins; `Collect` of an empty seq is a non-nil empty map; `maps.All` -> `maps.Collect` round-trips to an equal map.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/streamindex/cmd/demo
cd ~/go-exercises/streamindex
go mod init example.com/streamindex
```

## Why iterators, and what collision means here

A producer that yields key/value pairs one at a time is an `iter.Seq2[K, V]` — a
function `func(yield func(K, V) bool)`. This is how you model a `sql.Rows` cursor,
a `bufio.Scanner` over a CSV, or any source you do not want to buffer into a slice
first. `maps.Collect(seq)` drains such a sequence into a fresh `map[K]V`, and
`maps.Insert(dst, seq)` folds it into an existing map. Both are the map-building
duals of `slices.Collect`.

The semantics that matter are collisions. A stream can yield the same key twice — a
row that appears in two batches, a user id seen in two files. Because a map holds
one entry per key, the second yield overwrites the first: last writer wins. This is
usually what you want for an "upsert into an index" (the newest row replaces the
older), but it is a silent behavior, so the test pins it explicitly. If you must
keep every colliding row you collect into a `map[K][]V` by hand instead of using
`maps.Collect`.

`Index` builds a fresh index from a stream with `maps.Collect`. `MergeInto` folds a
second stream into an already-built index with `maps.Insert`, applying the same
last-writer-wins rule against the existing entries. The round-trip property —
`maps.Collect(maps.All(m))` reproduces an equal map — is worth testing because it
confirms `maps.All` yields every entry exactly once and `maps.Collect` reassembles
them faithfully; it is the identity law of the two functions.

Create `streamindex.go`:

```go
package streamindex

import (
	"iter"
	"maps"
)

// Record is one indexed row.
type Record struct {
	Name string
	Seq  int
}

// Index drains a key/value stream into a fresh index. On a duplicate key the
// later pair wins (last-writer-wins), matching an upsert.
func Index(seq iter.Seq2[string, Record]) map[string]Record {
	return maps.Collect(seq)
}

// MergeInto folds a stream into an existing index in place, with the stream's
// entries overwriting existing keys on collision.
func MergeInto(dst map[string]Record, seq iter.Seq2[string, Record]) {
	maps.Insert(dst, seq)
}
```

The `iter.Seq2` these functions consume stands in for a real cursor; in production
it would wrap `sql.Rows.Next` or a `csv.Reader`. The producer's `if !yield(...) {
return }` pattern (shown in the demo and tests below) is the iterator contract:
stop producing when the consumer signals it is done — which `maps.Collect` never
does, but a bounded consumer might.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"iter"
	"maps"
	"slices"

	"example.com/streamindex"
)

func rows(recs ...streamindex.Record) iter.Seq2[string, streamindex.Record] {
	return func(yield func(string, streamindex.Record) bool) {
		for _, r := range recs {
			if !yield(r.Name, r) {
				return
			}
		}
	}
}

func main() {
	idx := streamindex.Index(rows(
		streamindex.Record{Name: "alice", Seq: 1},
		streamindex.Record{Name: "bob", Seq: 2},
		streamindex.Record{Name: "alice", Seq: 3}, // duplicate key, later wins
	))
	fmt.Println("keys:", slices.Sorted(maps.Keys(idx)))
	fmt.Println("alice seq:", idx["alice"].Seq)

	streamindex.MergeInto(idx, rows(
		streamindex.Record{Name: "carol", Seq: 4},
		streamindex.Record{Name: "bob", Seq: 5}, // overwrites existing bob
	))
	fmt.Println("after merge:", slices.Sorted(maps.Keys(idx)))
	fmt.Println("bob seq:", idx["bob"].Seq)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
keys: [alice bob]
alice seq: 3
after merge: [alice bob carol]
bob seq: 5
```

`alice` resolved to `Seq: 3` (the later of the two `alice` rows), and after the
merge `bob` became `Seq: 5` (the merged batch overwrote the existing entry).

### Tests

The tests pin last-writer-wins on both `Index` and `MergeInto`, assert `Collect` of
an empty stream is a non-nil empty map, and confirm the `maps.All` -> `maps.Collect`
round-trip reconstructs an equal map.

Create `streamindex_test.go`:

```go
package streamindex

import (
	"fmt"
	"iter"
	"maps"
	"testing"
)

func seqOf(recs ...Record) iter.Seq2[string, Record] {
	return func(yield func(string, Record) bool) {
		for _, r := range recs {
			if !yield(r.Name, r) {
				return
			}
		}
	}
}

func TestIndexLastWriterWins(t *testing.T) {
	t.Parallel()

	idx := Index(seqOf(
		Record{Name: "a", Seq: 1},
		Record{Name: "a", Seq: 2},
		Record{Name: "b", Seq: 3},
	))
	if len(idx) != 2 {
		t.Fatalf("len(idx) = %d, want 2 (duplicate key collapses)", len(idx))
	}
	if idx["a"].Seq != 2 {
		t.Errorf("idx[a].Seq = %d, want 2 (last writer wins)", idx["a"].Seq)
	}
}

func TestIndexEmptySeq(t *testing.T) {
	t.Parallel()

	idx := Index(seqOf())
	if idx == nil {
		t.Fatal("Index of empty seq returned nil, want non-nil empty map")
	}
	if len(idx) != 0 {
		t.Fatalf("len = %d, want 0", len(idx))
	}
}

func TestMergeIntoOverwrites(t *testing.T) {
	t.Parallel()

	idx := Index(seqOf(Record{Name: "a", Seq: 1}, Record{Name: "b", Seq: 1}))
	MergeInto(idx, seqOf(Record{Name: "b", Seq: 9}, Record{Name: "c", Seq: 2}))

	want := map[string]Record{
		"a": {Name: "a", Seq: 1},
		"b": {Name: "b", Seq: 9},
		"c": {Name: "c", Seq: 2},
	}
	if !maps.Equal(idx, want) {
		t.Fatalf("MergeInto result = %v, want %v", idx, want)
	}
}

func TestAllCollectRoundTrip(t *testing.T) {
	t.Parallel()

	orig := map[string]Record{
		"x": {Name: "x", Seq: 1},
		"y": {Name: "y", Seq: 2},
		"z": {Name: "z", Seq: 3},
	}
	got := maps.Collect(maps.All(orig))
	if !maps.Equal(got, orig) {
		t.Fatalf("round trip = %v, want %v", got, orig)
	}
}

func ExampleIndex() {
	idx := Index(seqOf(Record{Name: "k", Seq: 1}, Record{Name: "k", Seq: 7}))
	fmt.Println(len(idx), idx["k"].Seq)
	// Output: 1 7
}
```

## Review

The module is correct when duplicate keys resolve to the last yielded value on both
`Index` and `MergeInto`, an empty stream yields a non-nil empty map, and the
`maps.All` -> `maps.Collect` round-trip is an identity. The one behavior to
internalize is that collision is silent: if two rows share a key you keep only the
last, which is right for an upsert index and wrong if you needed to accumulate — in
which case collect into a `map[K][]V` yourself instead of `maps.Collect`. The
iterator adapter shows the shape a real `sql.Rows` or `csv.Reader` wrapper takes;
the `if !yield { return }` guard is the non-negotiable part of writing a correct
producer. Run `go test -race`.

## Resources

- [maps package](https://pkg.go.dev/maps) — `Collect`, `Insert`, `All`.
- [iter package](https://pkg.go.dev/iter) — `Seq2` and the yield contract.
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions) — writing a producer.

---

Back to [05-immutable-snapshot-registry.md](05-immutable-snapshot-registry.md) | Next: [07-ttl-cache-sweep.md](07-ttl-cache-sweep.md)
