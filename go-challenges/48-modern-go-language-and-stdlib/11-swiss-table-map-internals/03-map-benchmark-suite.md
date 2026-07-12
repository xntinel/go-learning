# Exercise 3: Benchmark Maps Correctly with testing.B.Loop

A "map regression" report is worthless if the benchmark measured nothing. This
exercise builds a benchmark suite for the operations whose curves changed in Go 1.24
— lookup hit versus miss, assignment into a presized versus a grown map, and
full-map iteration — using the Go 1.24 `for b.Loop()` form, and keeps the measurable
logic in pure helpers so it can be unit-tested without running the benchmarks.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
mapbench/                  independent module: example.com/mapbench
  go.mod                   go 1.24 (b.Loop)
  mapbench.go              BuildPresized, BuildGrown, Lookup, SumValues, SortedKeys,
                           bytesPerEntry helper + metric unit const
  mapbench_test.go         unit tests for the helpers + benchmarks using b.Loop, Example
  cmd/
    demo/
      main.go              runnable demo exercising the helpers
```

- Files: `mapbench.go`, `mapbench_test.go`, `cmd/demo/main.go`.
- Implement: pure builder/lookup/iterate helpers plus a `bytesPerEntry` helper and a metric-unit constant; benchmarks that are thin wrappers over the helpers using `for b.Loop()`, `b.ReportAllocs`, and `b.ReportMetric`.
- Test: unit tests asserting the builders agree and `Lookup` returns hit/miss correctly (table-driven, `t.Parallel`), plus a test that the metric unit ends in `/op` and `bytesPerEntry` computes correctly; an `Example` with a fixed `// Output`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/48-modern-go-language-and-stdlib/11-swiss-table-map-internals/03-map-benchmark-suite/cmd/demo
cd go-solutions/48-modern-go-language-and-stdlib/11-swiss-table-map-internals/03-map-benchmark-suite
go mod edit -go=1.24
```

### Why for b.Loop() and not for i := 0; i < b.N; i++

The classic benchmark loop has two failure modes that make it lie. First, if the loop
body computes a result and does not use it, the compiler is entitled to delete the
whole computation as dead code, and the benchmark reports an impossibly fast time for
work that never ran. Second, any setup done before the loop still counts toward the
measured time unless you remember to call `b.ResetTimer`, and any teardown after it
skews the result unless you `StopTimer`. The Go 1.24 `for b.Loop()` form fixes both:
the loop runs an internally chosen number of iterations, it automatically excludes
setup before the first `Loop()` call and teardown after the last from the timing, and
— crucially — the compiler is required to keep the loop body, so it cannot be
optimized away. It is the correct default for new benchmarks; the `b.N` form is now
legacy.

Even with `b.Loop`, discipline in the body matters: accumulate lookup results into a
sink variable the loop reads, so the intent (do the lookup) is unambiguous and the
work is anchored. `b.ReportAllocs` adds allocation counts to the output;
`b.ReportMetric(value, unit)` adds a custom column. By convention a per-operation
metric's unit ends in `/op` so tooling that aggregates benchmark output treats it as
per-iteration.

### Testable helpers, thin benchmarks

A benchmark is not run by `go test` unless you pass `-bench`, so a benchmark alone
proves nothing in the gate. The fix is to put every piece of real logic in a plain,
exported helper — `BuildPresized`, `BuildGrown`, `Lookup`, `SumValues` — and make each
benchmark a two-line wrapper around one. Now the helpers get ordinary table-driven
unit tests (which the gate runs), and the benchmarks only have to compile and, when
you do run them, exercise the same code the tests already validated. The custom
bytes-per-entry metric is computed by a pure `bytesPerEntry` function the test checks
directly, so the number reported in a benchmark is the same number the test asserts.

Create `mapbench.go`:

```go
package mapbench

import (
	"cmp"
	"slices"
)

// bytesPerEntryUnit is the unit reported to b.ReportMetric. Per-operation metric
// units end in "/op" by convention so benchmark tooling aggregates them correctly.
const bytesPerEntryUnit = "bytes-per-entry/op"

// BuildPresized returns a map of n entries built with a presized allocation.
// Values are i*2 so tests can check contents cheaply.
func BuildPresized(n int) map[int]int {
	m := make(map[int]int, n)
	for i := range n {
		m[i] = i * 2
	}
	return m
}

// BuildGrown returns the same map built without presizing (it grows and rehashes).
func BuildGrown(n int) map[int]int {
	m := make(map[int]int)
	for i := range n {
		m[i] = i * 2
	}
	return m
}

// Lookup is a thin wrapper over a map read so the benchmark body is anchored.
func Lookup(m map[int]int, k int) (int, bool) {
	v, ok := m[k]
	return v, ok
}

// SumValues iterates the whole map. Used by the iteration benchmark.
func SumValues(m map[int]int) int {
	sum := 0
	for _, v := range m {
		sum += v
	}
	return sum
}

// SortedKeys returns the map's keys in ascending order — the idiom for
// deterministic output over a map, whose iteration order is randomized.
func SortedKeys(m map[int]int) []int {
	ks := make([]int, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.SortFunc(ks, func(a, b int) int { return cmp.Compare(a, b) })
	return ks
}

// bytesPerEntry converts a measured heap size and entry count into a per-entry
// figure. Kept pure so a test can assert the exact value a benchmark reports.
func bytesPerEntry(n int, heapBytes uint64) float64 {
	if n == 0 {
		return 0
	}
	return float64(heapBytes) / float64(n)
}
```

### The runnable demo

The demo exercises the helpers with fully deterministic output — sorted keys and
fixed lookups — so it doubles as a sanity check that the code the benchmarks wrap
behaves correctly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mapbench"
)

func main() {
	m := mapbench.BuildPresized(10)
	fmt.Println("len:", len(m))
	fmt.Println("sorted keys:", mapbench.SortedKeys(m))

	v, ok := mapbench.Lookup(m, 4)
	fmt.Println("lookup 4:", v, ok)
	_, ok = mapbench.Lookup(m, 100)
	fmt.Println("lookup 100 present:", ok)

	fmt.Println("sum of values:", mapbench.SumValues(m))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
len: 10
sorted keys: [0 1 2 3 4 5 6 7 8 9]
lookup 4: 8 true
lookup 100 present: false
sum of values: 90
```

### Tests and benchmarks

The unit tests validate the helpers the benchmarks depend on: `TestBuilders` asserts
presized and grown builds produce identical, correct contents; `TestLookup` is a
table of hit/miss cases; `TestMetric` checks the unit ends in `/op` and that
`bytesPerEntry` computes exactly. The benchmarks are thin `for b.Loop()` wrappers;
they are not run by `go test` without `-bench`, but they must compile and, when run,
measure real work. The iteration benchmark reports the custom metric to demonstrate
`b.ReportMetric`. Run the benchmarks once with `go test -run=^$ -bench=. -benchtime=1x`
to confirm they execute.

Create `mapbench_test.go`:

```go
package mapbench

import (
	"fmt"
	"strings"
	"testing"
)

func TestBuilders(t *testing.T) {
	t.Parallel()
	builders := []struct {
		name  string
		build func(int) map[int]int
	}{
		{"presized", BuildPresized},
		{"grown", BuildGrown},
	}
	for _, tc := range builders {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := tc.build(1000)
			if len(m) != 1000 {
				t.Fatalf("len = %d, want 1000", len(m))
			}
			for i := range 1000 {
				if m[i] != i*2 {
					t.Fatalf("m[%d] = %d, want %d", i, m[i], i*2)
				}
			}
		})
	}
}

func TestLookup(t *testing.T) {
	t.Parallel()
	m := BuildPresized(10)
	tests := []struct {
		k      int
		wantV  int
		wantOK bool
	}{
		{0, 0, true},
		{5, 10, true},
		{9, 18, true},
		{10, 0, false},
		{-1, 0, false},
	}
	for _, tc := range tests {
		v, ok := Lookup(m, tc.k)
		if v != tc.wantV || ok != tc.wantOK {
			t.Errorf("Lookup(%d) = %d,%v; want %d,%v", tc.k, v, ok, tc.wantV, tc.wantOK)
		}
	}
}

func TestMetric(t *testing.T) {
	t.Parallel()
	if !strings.HasSuffix(bytesPerEntryUnit, "/op") {
		t.Errorf("metric unit %q must end in /op", bytesPerEntryUnit)
	}
	if got := bytesPerEntry(1000, 16000); got != 16 {
		t.Errorf("bytesPerEntry(1000, 16000) = %v, want 16", got)
	}
	if got := bytesPerEntry(0, 999); got != 0 {
		t.Errorf("bytesPerEntry(0, _) = %v, want 0", got)
	}
}

func Example() {
	m := BuildPresized(5)
	fmt.Println(len(m))
	fmt.Println(SortedKeys(m))
	v, ok := Lookup(m, 3)
	fmt.Println(v, ok)
	fmt.Println(SumValues(m))
	// Output:
	// 5
	// [0 1 2 3 4]
	// 6 true
	// 20
}

const benchN = 10000

func BenchmarkLookupHit(b *testing.B) {
	m := BuildPresized(benchN)
	b.ReportAllocs()
	var sink int
	for b.Loop() {
		v, _ := Lookup(m, benchN/2)
		sink += v
	}
	_ = sink
}

func BenchmarkLookupMiss(b *testing.B) {
	m := BuildPresized(benchN)
	b.ReportAllocs()
	var hits int
	for b.Loop() {
		if _, ok := Lookup(m, -1); ok {
			hits++
		}
	}
	if hits != 0 {
		b.Fatalf("miss lookup reported %d hits", hits)
	}
}

func BenchmarkAssignPresized(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		m := BuildPresized(benchN)
		_ = m
	}
}

func BenchmarkAssignGrown(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		m := BuildGrown(benchN)
		_ = m
	}
}

func BenchmarkIterate(b *testing.B) {
	m := BuildPresized(benchN)
	var sink int
	for b.Loop() {
		sink += SumValues(m)
	}
	_ = sink
	// Report a custom per-entry metric alongside the timing.
	b.ReportMetric(bytesPerEntry(benchN, uint64(benchN)*16), bytesPerEntryUnit)
}
```

## Review

The suite is correct when the helpers the benchmarks wrap are themselves tested: if
`BuildPresized` and `BuildGrown` disagree, or `Lookup` mis-reports a miss, the
benchmark numbers are meaningless no matter how carefully timed. That is why the real
logic lives in pure functions with table-driven tests and the benchmarks are thin
wrappers — the gate runs the tests, not the benchmarks, so the correctness proof has
to sit in the tests.

The mistakes here are about measurement integrity. Do not write `for i := 0; i < b.N;
i++` with an unused result — the compiler may delete the body and the benchmark
measures nothing; `for b.Loop()` prevents that and also excludes setup and teardown
from timing without manual `ResetTimer`/`StopTimer`. Accumulate results into a sink
the loop reads, so the work is anchored. Keep the metric computation in a pure helper
the test checks, so the number a benchmark reports is the number a test has verified,
and keep the unit ending in `/op` so downstream tooling aggregates it per-iteration.
Run `go test -race` for the correctness gate, and `go test -run=^$ -bench=.
-benchtime=1x` to confirm the benchmarks execute.

## Resources

- [More predictable benchmarking with testing.B.Loop (The Go Blog)](https://go.dev/blog/testing-b-loop) — why `b.Loop` replaces the `b.N` loop and how it defeats dead-code elimination.
- [`testing` package](https://pkg.go.dev/testing) — `B.Loop`, `B.ReportAllocs`, and `B.ReportMetric` signatures.
- [How Go 1.24's Swiss Tables saved us hundreds of gigabytes (Datadog Engineering)](https://www.datadoghq.com/blog/engineering/go-swiss-tables/) — a production account of measuring the map change at scale.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-map-memory-footprint.md](02-map-memory-footprint.md) | Next: [../12-encoding-json-v2/00-concepts.md](../12-encoding-json-v2/00-concepts.md)
