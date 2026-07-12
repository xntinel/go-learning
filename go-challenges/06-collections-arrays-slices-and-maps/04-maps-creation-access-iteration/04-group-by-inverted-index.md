# Exercise 4: Grouping and Inverted Indexes: map[string][]T with lazy append

Aggregating a stream of records into buckets — logs by level, audit events by
actor, documents by tag — is the daily work of a reporting or search layer. This
exercise builds `GroupBy` over `map[string][]Record` and an inverted index
`map[string][]DocID`, exploiting that appending to the nil slice fetched from a
missing key just works, and emitting keys in sorted order for stable reports.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
logindex/                  independent module: example.com/logindex
  go.mod
  index.go                 Record; GroupBy, InvertedIndex, SortedKeys, RecordIDs
  cmd/
    demo/
      main.go              runnable demo: group records by level, index by tag
  index_test.go            grouping, empty input, sorted keys, new-value bucket, inverted index
```

- Files: `index.go`, `cmd/demo/main.go`, `index_test.go`.
- Implement: `GroupBy(records, key)` returning `map[string][]Record`, `InvertedIndex(records)` returning `map[string][]string` (tag to sorted doc IDs), and `SortedKeys` for stable output.
- Test: grouping produces expected buckets, empty input yields an empty map, output keys are sorted and stable, a new field value creates a fresh bucket, the inverted index maps each tag to all doc IDs.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/04-group-by-inverted-index/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/04-group-by-inverted-index
```

### Why append to a missing key needs no setup

The core move is `m[k] = append(m[k], v)`. When `k` is absent, `m[k]` reads the
value type's zero — for a slice value that is `nil` — and `append(nil, v)`
allocates a fresh one-element slice, which is then stored back under `k`. So the
first record for a new group key transparently creates the group; you never write
`if _, ok := m[k]; !ok { m[k] = []Record{} }`. This is the same zero-value trick
that makes `m[k]++` work for counters, applied to slices. Both `GroupBy` and
`InvertedIndex` lean on it: every bucket springs into existence on first touch.

There is a subtlety worth naming. A nil slice value and a present-but-empty slice
value are different states that `len` cannot distinguish (both are `0`), but
comma-ok can: `recs, ok := groups[k]` tells you whether the key exists at all.
For grouping you rarely need that distinction — an absent key and an empty group
are both "no records" — but it is the reason a grouping map never stores empty
slices: a key exists only once at least one record landed in it.

`GroupBy` takes a key function so it can group by any field (level, service,
actor) without duplication. `InvertedIndex` maps each tag to the IDs of every
record carrying it, sorting the ID lists so the index is deterministic.
`SortedKeys` collects and sorts a map's keys with `slices.Sorted(maps.Keys(m))`
so reports iterate in a stable order rather than the map's randomized one.

Create `index.go`:

```go
package logindex

import (
	"maps"
	"slices"
)

// Record is one log or audit entry.
type Record struct {
	ID      string
	Service string
	Level   string
	Tags    []string
}

// GroupBy buckets records by the string key produced by key. A new key value
// creates a fresh bucket on first touch, because append to a nil slice works.
func GroupBy(records []Record, key func(Record) string) map[string][]Record {
	groups := make(map[string][]Record)
	for _, r := range records {
		k := key(r)
		groups[k] = append(groups[k], r)
	}
	return groups
}

// InvertedIndex maps each tag to the sorted IDs of every record carrying it.
func InvertedIndex(records []Record) map[string][]string {
	index := make(map[string][]string)
	for _, r := range records {
		for _, tag := range r.Tags {
			index[tag] = append(index[tag], r.ID)
		}
	}
	for tag := range index {
		slices.Sort(index[tag])
	}
	return index
}

// SortedKeys returns the keys of any string-keyed map in ascending order, for
// stable reporting over a map's randomized iteration order.
func SortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// RecordIDs returns the IDs of records in order.
func RecordIDs(records []Record) []string {
	ids := make([]string, len(records))
	for i, r := range records {
		ids[i] = r.ID
	}
	return ids
}
```

### The runnable demo

The demo groups a fixed set of records by level and builds a tag index, printing
both in sorted key order so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/logindex"
)

func main() {
	records := []logindex.Record{
		{ID: "1", Service: "auth", Level: "info", Tags: []string{"login"}},
		{ID: "2", Service: "auth", Level: "error", Tags: []string{"login", "failure"}},
		{ID: "3", Service: "billing", Level: "info", Tags: []string{"charge"}},
		{ID: "4", Service: "billing", Level: "error", Tags: []string{"charge", "failure"}},
	}

	groups := logindex.GroupBy(records, func(r logindex.Record) string { return r.Level })
	fmt.Println("group by level:")
	for _, level := range logindex.SortedKeys(groups) {
		fmt.Printf("  %s: %v\n", level, logindex.RecordIDs(groups[level]))
	}

	index := logindex.InvertedIndex(records)
	fmt.Println("tag index:")
	for _, tag := range logindex.SortedKeys(index) {
		fmt.Printf("  %s: %v\n", tag, index[tag])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
group by level:
  error: [2 4]
  info: [1 3]
tag index:
  charge: [3 4]
  failure: [2 4]
  login: [1 2]
```

Both sections print in sorted key order every run: `SortedKeys` sorts the map keys
before iterating, and `InvertedIndex` sorts each tag's ID list, so nothing leaks
the map's randomized order.

### Tests

`TestGroupByLevel` pins the buckets for a fixed input. `TestGroupByEmptyInput`
proves an empty stream yields an empty (non-nil) map. `TestNewValueCreatesBucket`
shows a record with a never-seen level lands in a fresh bucket. `TestSortedKeys`
proves the report order is deterministic. `TestInvertedIndex` checks each tag maps
to all and only its doc IDs, sorted. `TestAbsentBucket` uses comma-ok to
distinguish an absent group from a present one.

Create `index_test.go`:

```go
package logindex

import (
	"fmt"
	"slices"
	"testing"
)

func sample() []Record {
	return []Record{
		{ID: "1", Service: "auth", Level: "info", Tags: []string{"login"}},
		{ID: "2", Service: "auth", Level: "error", Tags: []string{"login", "failure"}},
		{ID: "3", Service: "billing", Level: "info", Tags: []string{"charge"}},
		{ID: "4", Service: "billing", Level: "error", Tags: []string{"charge", "failure"}},
	}
}

func byLevel(r Record) string { return r.Level }

func TestGroupByLevel(t *testing.T) {
	t.Parallel()

	groups := GroupBy(sample(), byLevel)
	if got := RecordIDs(groups["info"]); !slices.Equal(got, []string{"1", "3"}) {
		t.Fatalf("info group = %v, want [1 3]", got)
	}
	if got := RecordIDs(groups["error"]); !slices.Equal(got, []string{"2", "4"}) {
		t.Fatalf("error group = %v, want [2 4]", got)
	}
}

func TestGroupByEmptyInput(t *testing.T) {
	t.Parallel()

	groups := GroupBy(nil, byLevel)
	if groups == nil {
		t.Fatal("GroupBy(nil) should return a non-nil empty map")
	}
	if len(groups) != 0 {
		t.Fatalf("len = %d, want 0", len(groups))
	}
}

func TestNewValueCreatesBucket(t *testing.T) {
	t.Parallel()

	recs := append(sample(), Record{ID: "5", Level: "warn", Tags: []string{"slow"}})
	groups := GroupBy(recs, byLevel)
	if got := RecordIDs(groups["warn"]); !slices.Equal(got, []string{"5"}) {
		t.Fatalf("warn group = %v, want [5]", got)
	}
}

func TestSortedKeys(t *testing.T) {
	t.Parallel()

	groups := GroupBy(sample(), byLevel)
	if got := SortedKeys(groups); !slices.Equal(got, []string{"error", "info"}) {
		t.Fatalf("SortedKeys = %v, want [error info]", got)
	}
}

func TestInvertedIndex(t *testing.T) {
	t.Parallel()

	index := InvertedIndex(sample())
	want := map[string][]string{
		"login":   {"1", "2"},
		"failure": {"2", "4"},
		"charge":  {"3", "4"},
	}
	for tag, ids := range want {
		if got := index[tag]; !slices.Equal(got, ids) {
			t.Fatalf("index[%q] = %v, want %v", tag, got, ids)
		}
	}
}

func TestAbsentBucket(t *testing.T) {
	t.Parallel()

	groups := GroupBy(sample(), byLevel)
	if _, ok := groups["debug"]; ok {
		t.Fatal("absent group should report ok=false via comma-ok")
	}
}

func ExampleGroupBy() {
	groups := GroupBy(sample(), byLevel)
	fmt.Println(RecordIDs(groups["info"]))
	// Output: [1 3]
}
```

## Review

The grouping is correct when every record lands in exactly one bucket keyed by the
key function and no bucket is ever pre-initialized: `append(groups[k], r)` on a
missing key reads a nil slice, appends, and stores the fresh slice, so the first
record creates the group. The inverted index is correct when every tag maps to the
sorted IDs of exactly the records carrying it. Determinism comes from `SortedKeys`
and the per-tag `slices.Sort`, not from the map — ranging the map directly would
produce a different order each run and flaky golden tests. The comma-ok test
documents that a grouping map only ever holds keys that received at least one
record, so an absent key is genuinely "no records", not an empty bucket.

## Resources

- [Go blog: Go maps in action](https://go.dev/blog/maps) — grouping and inverted-index idioms.
- [append](https://pkg.go.dev/builtin#append) — appending to a nil slice allocates a fresh one.
- [maps.Keys](https://pkg.go.dev/maps#Keys) and [slices.Sorted](https://pkg.go.dev/slices#Sorted) — deterministic key ordering.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-set-membership-allowlist.md](03-set-membership-allowlist.md) | Next: [05-maps-package-config-snapshots.md](05-maps-package-config-snapshots.md)
