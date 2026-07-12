# Exercise 8: Table-Driven Sub-Benchmarks to Detect O(n^2) Scaling

A benchmark at a single input size tells you a cost; a benchmark swept across sizes
tells you a *complexity class*. This module uses `b.Run` to build sub-benchmarks over
sizes 100, 1k, 10k, and 100k for two implementations of a repository lookup — a linear
scan over a slice and a map lookup — and reads the `ns/op` curve to expose the linear
scan's superlinear growth against the map's flat cost.

## What you'll build

```text
lookup/                    independent module: example.com/lookup
  go.mod                   go 1.24
  lookup.go                type Record; LinearFind([]Record, string) (Record, bool);
                           BuildIndex([]Record) map[string]Record; MapFind(index, key)
  cmd/
    demo/
      main.go              runnable demo: build data, find via both, print
  lookup_test.go           TestEquivalence (both agree); size-swept b.Run sub-benchmarks; Example
```

- Files: `lookup.go`, `cmd/demo/main.go`, `lookup_test.go`.
- Implement: a linear scan and a map-based lookup over a slice of records.
- Test: both return identical results for the same keys, plus size-swept sub-benchmarks.
- Verify: `go test -count=1 -race ./...` then `go test -bench=. -benchmem`.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/05-benchmarks/08-table-driven-benchmarks-scaling/cmd/demo
cd go-solutions/12-testing-ecosystem/05-benchmarks/08-table-driven-benchmarks-scaling
go mod edit -go=1.24
```

### Reading a curve instead of a number

`LinearFind` walks the slice comparing keys until it matches — O(n) per lookup.
`MapFind` reads a prebuilt `map[string]Record` — O(1) per lookup. Both are correct and
return the same record for the same key; the correctness test proves that. The
interesting fact is invisible at any single size and obvious across a sweep.

`BenchmarkLookup` nests two groups of sub-benchmarks with `b.Run`: for each size in
{100, 1k, 10k, 100k} it benchmarks a worst-case linear lookup (a key that forces a full
scan) and a map lookup of the same key. Because the sub-benchmark name encodes the size
(`linear/size=10000`), the output is one labeled line per size, and reading down the
`ns/op` column is the whole point: at the larger sizes the linear scan's time grows in
proportion to size (10x the records, ~10x the time), climbing by hundreds of times
across the sweep, while the map's time stays flat. If the linear cost grew *faster* than proportionally —
10x input costing 100x time — that superlinear signature would flag an accidental
O(n^2), which is exactly the regression this technique catches in a repository query
path (a nested scan, an N+1 lookup).

The map lookup pays an index-build cost once (`BuildIndex`), which the benchmark does
before the timed loop so only the lookup is measured — the same setup-exclusion
discipline from Exercise 4. The comparison is therefore honest: repeated lookups against
a prebuilt index versus repeated linear scans.

Create `lookup.go`:

```go
package lookup

// Record is a row in a small in-memory repository.
type Record struct {
	Key   string
	Value int
}

// LinearFind scans records for the one whose Key equals key. O(n) per call.
func LinearFind(records []Record, key string) (Record, bool) {
	for i := range records {
		if records[i].Key == key {
			return records[i], true
		}
	}
	return Record{}, false
}

// BuildIndex builds a key->Record map for O(1) lookups. Later duplicate keys win.
func BuildIndex(records []Record) map[string]Record {
	index := make(map[string]Record, len(records))
	for i := range records {
		index[records[i].Key] = records[i]
	}
	return index
}

// MapFind reads the prebuilt index. O(1) per call.
func MapFind(index map[string]Record, key string) (Record, bool) {
	r, ok := index[key]
	return r, ok
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lookup"
)

func main() {
	records := []lookup.Record{
		{Key: "a", Value: 1},
		{Key: "b", Value: 2},
		{Key: "c", Value: 3},
	}
	index := lookup.BuildIndex(records)

	lin, _ := lookup.LinearFind(records, "b")
	mp, _ := lookup.MapFind(index, "b")
	fmt.Printf("linear: %s=%d\n", lin.Key, lin.Value)
	fmt.Printf("map:    %s=%d\n", mp.Key, mp.Value)

	_, ok := lookup.MapFind(index, "z")
	fmt.Printf("missing z present=%v\n", ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
linear: b=2
map:    b=2
missing z present=false
```

### Tests

`TestEquivalence` builds a record set and asserts `LinearFind` and `MapFind` agree for
every key plus a missing key — the correctness precondition before comparing their
speed. `BenchmarkLookup` sweeps the sizes with nested `b.Run` calls.

Create `lookup_test.go`:

```go
package lookup

import (
	"fmt"
	"strconv"
	"testing"
)

func makeRecords(n int) []Record {
	records := make([]Record, n)
	for i := range records {
		records[i] = Record{Key: strconv.Itoa(i), Value: i}
	}
	return records
}

func TestEquivalence(t *testing.T) {
	t.Parallel()
	records := makeRecords(500)
	index := BuildIndex(records)

	keys := []string{"0", "1", "250", "499", "missing"}
	for _, k := range keys {
		lr, lok := LinearFind(records, k)
		mr, mok := MapFind(index, k)
		if lok != mok || lr != mr {
			t.Errorf("key %q: linear=(%v,%v) map=(%v,%v)", k, lr, lok, mr, mok)
		}
	}
}

func BenchmarkLookup(b *testing.B) {
	sizes := []int{100, 1_000, 10_000, 100_000}
	for _, size := range sizes {
		records := makeRecords(size)
		index := BuildIndex(records)
		worstKey := strconv.Itoa(size - 1) // forces a full scan for the linear variant

		b.Run("linear/size="+strconv.Itoa(size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = LinearFind(records, worstKey)
			}
		})
		b.Run("map/size="+strconv.Itoa(size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = MapFind(index, worstKey)
			}
		})
	}
}

func ExampleLinearFind() {
	records := []Record{{Key: "x", Value: 9}}
	r, ok := LinearFind(records, "x")
	fmt.Println(r.Value, ok)
	// Output: 9 true
}
```

Run the sweep; read the `ns/op` column down each group:

```bash
go test -bench=. -benchmem
```

```text
BenchmarkLookup/linear/size=100-8         68509     318.8 ns/op
BenchmarkLookup/map/size=100-8          2833557       7.5 ns/op
BenchmarkLookup/linear/size=1000-8        10000    2151 ns/op
BenchmarkLookup/map/size=1000-8         4139654       5.2 ns/op
BenchmarkLookup/linear/size=10000-8        1867   12959 ns/op
BenchmarkLookup/map/size=10000-8        5088518       4.9 ns/op
BenchmarkLookup/linear/size=100000-8        187  129257 ns/op
BenchmarkLookup/map/size=100000-8       3286432       6.9 ns/op
PASS
```

## Review

`TestEquivalence` makes the two implementations interchangeable in behavior, which is
what licenses comparing them purely on speed. The benchmark lesson is in the shape of
the numbers: the linear variant's `ns/op` scales in lockstep with size — roughly 10x
per 10x of input, the fingerprint of O(n) — while the map variant is flat near 5 ns
regardless of size, the fingerprint of O(1). Encoding the size in the `b.Run` name is
what makes the sweep readable as a curve rather than a pile of numbers. The senior use
of this technique is regression detection: if a future change made the "map" path scan
(a lost index, a fallback branch), its flat line would bend upward across sizes and the
sweep would catch a complexity regression that a single-size benchmark would miss
entirely.

## Resources

- [`testing.B.Run`](https://pkg.go.dev/testing#B.Run) — nested sub-benchmarks for sweeping sizes or variants.
- [`testing.B.Loop`](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 loop used in each sub-benchmark.
- [Go maps in action](https://go.dev/blog/maps) — the O(1) hash-map semantics the map variant relies on.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-custom-metrics-reportmetric.md](07-custom-metrics-reportmetric.md) | Next: [09-allocation-regression-strings-builder.md](09-allocation-regression-strings-builder.md)
