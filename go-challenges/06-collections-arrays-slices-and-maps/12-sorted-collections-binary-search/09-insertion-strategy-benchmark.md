# Exercise 9: Insert Strategy Trade-off — Binary-Search Insert vs Append+Sort vs Map

The claim "insertion into a sorted slice is O(n), append-then-sort is worse, a map
wins for bulk build but loses order" is only worth trusting if it is measurable.
This exercise puts three sorted-set builders behind one interface, proves they
produce identical output with a differential test, and contrasts them with
`testing.B` benchmarks so the crossover is documented in runnable code rather than
asserted in prose.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
insertbench/                 independent module: example.com/insertbench
  go.mod
  builders.go                Builder interface; BinInsert, AppendSort, MapBuffer
  cmd/
    demo/
      main.go                run all three on one input, show identical output
  builders_test.go           differential correctness + benchmarks (no wall-clock asserts)
```

Files: `builders.go`, `cmd/demo/main.go`, `builders_test.go`.
Implement: a `Builder` interface (`Add(int)`, `Result() []int`) with three implementations — binary-search insert, append-then-sort-each-time, map-then-sort-once.
Test: a differential correctness test running one shuffled input with duplicates through all three and asserting identical sorted, de-duplicated output; benchmarks with `b.ReportAllocs`/`b.ResetTimer` and no wall-clock assertions.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/insertbench/cmd/demo
cd ~/go-exercises/insertbench
go mod init example.com/insertbench
```

### Three strategies, one contract

All three build the same thing — a sorted set of ints — but pay for it
differently:

- `BinInsert` keeps the slice sorted at all times. Each `Add` binary-searches the
  position and `slices.Insert`s (skipping duplicates). The search is O(log n) but
  the shift is O(n), so each `Add` is O(n) and building `n` elements is O(n^2)
  overall — with the crucial property that the slice is queryable *during*
  construction.
- `AppendSort` appends then re-sorts (and compacts) on every `Add`. Each `Add` is
  O(n log n); building `n` elements is O(n^2 log n) — strictly worse than
  `BinInsert`. It exists here as the anti-pattern the concepts file warns against,
  made measurable.
- `MapBuffer` drops each value into a map (O(1) amortized) and sorts once in
  `Result` via `slices.Sorted(maps.Keys(m))`. Building is O(n) plus one
  O(n log n) sort at the end — the fastest for a bulk build — but the set has *no
  order at all* until `Result` is called, so it cannot answer a range or
  floor/ceiling query mid-build.

The interface makes them interchangeable, which is what lets one differential test
prove all three agree and lets the benchmarks compare like for like. The
differential test is the important correctness lever: three independent
implementations that must produce byte-identical output are far more likely to be
correct than any one checked against hand-written expectations.

Create `builders.go`:

```go
package insertbench

import (
	"maps"
	"slices"
)

// Builder incrementally accumulates ints and yields them sorted and de-duplicated.
type Builder interface {
	Add(v int)
	Result() []int
}

// BinInsert keeps its slice sorted on every Add via binary-search insertion.
type BinInsert struct{ data []int }

func (b *BinInsert) Add(v int) {
	pos, found := slices.BinarySearch(b.data, v)
	if found {
		return
	}
	b.data = slices.Insert(b.data, pos, v)
}

func (b *BinInsert) Result() []int { return slices.Clone(b.data) }

// AppendSort appends then re-sorts and compacts on every Add. This is the
// deliberately-wasteful strategy: O(n log n) per insert.
type AppendSort struct{ data []int }

func (a *AppendSort) Add(v int) {
	a.data = append(a.data, v)
	slices.Sort(a.data)
	a.data = slices.Compact(a.data)
}

func (a *AppendSort) Result() []int { return slices.Clone(a.data) }

// MapBuffer buffers into a map and sorts once in Result. Fastest to build, but
// unordered until Result is called.
type MapBuffer struct{ set map[int]struct{} }

func NewMapBuffer() *MapBuffer { return &MapBuffer{set: make(map[int]struct{})} }

func (m *MapBuffer) Add(v int) { m.set[v] = struct{}{} }

func (m *MapBuffer) Result() []int { return slices.Sorted(maps.Keys(m.set)) }

// builders returns a fresh instance of each strategy, keyed by name.
func builders() map[string]func() Builder {
	return map[string]func() Builder{
		"bininsert":  func() Builder { return &BinInsert{} },
		"appendsort": func() Builder { return &AppendSort{} },
		"mapbuffer":  func() Builder { return NewMapBuffer() },
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"

	"example.com/insertbench"
)

func main() {
	input := []int{5, 3, 8, 3, 1, 9, 5, 2, 8, 4}

	results := map[string][]int{}
	strategies := map[string]insertbench.Builder{
		"bininsert":  &insertbench.BinInsert{},
		"appendsort": &insertbench.AppendSort{},
		"mapbuffer":  insertbench.NewMapBuffer(),
	}
	for name, b := range strategies {
		for _, v := range input {
			b.Add(v)
		}
		results[name] = b.Result()
	}

	same := slices.Equal(results["bininsert"], results["appendsort"]) &&
		slices.Equal(results["bininsert"], results["mapbuffer"])
	fmt.Println("result:", results["bininsert"])
	fmt.Println("all strategies agree:", same)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
result: [1 2 3 4 5 8 9]
all strategies agree: true
```

### Tests

`TestDifferentialCorrectness` runs one shuffled, duplicate-laden input through all
three strategies and asserts each `Result` equals the independently computed
sorted-unique reference. The benchmarks measure the incremental-insert workload
(build the whole set element by element) with `b.ReportAllocs`; they make no
timing assertions — a benchmark documents cost, it does not gate on it.

Create `builders_test.go`:

```go
package insertbench

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

// reference computes the sorted, de-duplicated set independently of any Builder.
func reference(input []int) []int {
	out := slices.Clone(input)
	slices.Sort(out)
	return slices.Compact(out)
}

func shuffledInput(n int) []int {
	r := rand.New(rand.NewPCG(1, 2))
	in := make([]int, 0, 2*n)
	for i := range n {
		in = append(in, i, i) // every value twice, to exercise dedup
	}
	r.Shuffle(len(in), func(i, j int) { in[i], in[j] = in[j], in[i] })
	return in
}

func TestDifferentialCorrectness(t *testing.T) {
	t.Parallel()

	input := shuffledInput(200)
	want := reference(input)

	for name, newBuilder := range builders() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := newBuilder()
			for _, v := range input {
				b.Add(v)
			}
			if got := b.Result(); !slices.Equal(got, want) {
				t.Fatalf("%s result mismatch: got %d elems, want %d", name, len(got), len(want))
			}
		})
	}
}

func TestEmptyBuild(t *testing.T) {
	t.Parallel()

	for name, newBuilder := range builders() {
		if got := newBuilder().Result(); len(got) != 0 {
			t.Fatalf("%s empty build = %v, want empty", name, got)
		}
	}
}

func BenchmarkIncrementalBuild(b *testing.B) {
	input := shuffledInput(500)
	for name, newBuilder := range builders() {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				bld := newBuilder()
				for _, v := range input {
					bld.Add(v)
				}
				_ = bld.Result()
			}
		})
	}
}

func Example() {
	var b BinInsert
	for _, v := range []int{5, 3, 8, 3, 1} {
		b.Add(v)
	}
	fmt.Println(b.Result())
	// Output: [1 3 5 8]
}
```

Run the benchmarks (not part of the default `go test` path):

```bash
go test -bench=. -benchmem -run=^$
```

You will see `mapbuffer` build fastest, `bininsert` in the middle, and
`appendsort` slowest and allocating the most — the O(n^2) vs O(n^2 log n) gap made
concrete. The exact numbers vary by machine; the *ordering* of the three is the
lesson.

## Review

The three strategies are correct when they produce identical output, which the
differential test enforces without any hand-written expected slice — the strongest
form of correctness check when you have more than one implementation of the same
contract. The benchmarks exist to document the trade-off, not to assert it:
never put a wall-clock threshold in a test, because it flakes on a busy CI box.
The takeaway to internalize is the crossover — `MapBuffer` wins for a bulk build
but gives up ordering during construction, `BinInsert` keeps the set queryable at
every step for a linear-per-insert price, and `AppendSort` is the one to never
ship. Run `go test -race` for correctness and `go test -bench=.` to see the costs.

## Resources

- [`testing.B`](https://pkg.go.dev/testing#B) — `B.N`, `B.ResetTimer`, `B.ReportAllocs`.
- [`slices.Sorted` and `maps.Keys`](https://pkg.go.dev/slices#Sorted) — collect a map's keys into a sorted slice in one call.
- [Go Wiki: SliceTricks](https://go.dev/wiki/SliceTricks) — the insert, sort, and compact idioms the strategies use.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-invalid-search-guardrails.md](10-invalid-search-guardrails.md)
