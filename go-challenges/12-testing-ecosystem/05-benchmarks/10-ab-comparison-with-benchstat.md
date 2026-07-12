# Exercise 10: Prove an Optimization with benchstat (Statistical A/B)

A single-run `ns/op` delta on a laptop is noise: thermal throttling, turbo boost, and
background load move the number by more than most real optimizations do. The discipline
that turns a benchmark into evidence is replication plus statistics — run each variant
`-count=N` times, save the results, and feed them to `benchstat`, which reports the
delta with a p-value. This module builds an old and a new implementation of the same
deduplication function and walks the full benchstat workflow.

## What you'll build

```text
dedup/                     independent module: example.com/dedup
  go.mod                   go 1.24
  dedup.go                 DedupOld([]string) []string (O(n^2)); DedupNew([]string) []string (map set)
  cmd/
    demo/
      main.go              runnable demo: dedup a slice, print
  dedup_test.go            TestEquivalence (old and new identical output);
                           BenchmarkDedupOld and BenchmarkDedupNew; Example
```

- Files: `dedup.go`, `cmd/demo/main.go`, `dedup_test.go`.
- Implement: two order-preserving dedup functions — a quadratic scan and a map-set version.
- Test: old and new produce identical output for the same input, plus a benchmark for each.
- Verify: `go test -count=1 -race ./...`, then the benchstat A/B workflow below.

Set up the module:

```bash
go mod edit -go=1.24
```

### The two implementations and why you need statistics

`DedupOld` removes duplicates while preserving first-seen order by checking each
candidate against the already-kept results with a linear `contains` scan — O(n^2) for
`n` items. `DedupNew` does the same job with a `map[string]struct{}` set — O(n). They
are behaviorally identical (the equivalence test proves it), so the only question is
whether new is actually faster, and by how much, with what confidence.

You cannot answer that from one run of each. Run `BenchmarkDedupOld` once and again and
the two numbers already differ by a few percent from noise alone. The honest workflow is
replication into files plus `benchstat`:

```bash
# Run each benchmark 10 times; -run='^$' skips tests so only benchmarks run.
go test -run='^$' -bench=BenchmarkDedupOld -count=10 | tee old.txt
go test -run='^$' -bench=BenchmarkDedupNew -count=10 | tee new.txt

# Compare. benchstat needs matching benchmark names across files, so rename the
# benchmark line to a common name, or (cleaner) name both benchmarks the same via a
# shared sub-benchmark. The simplest correct approach is one file per variant with the
# SAME benchmark name — see the note below — then:
go run golang.org/x/perf/cmd/benchstat@latest old.txt new.txt
```

benchstat prints each variant's median `ns/op`, its variation across the ten runs, and
a p-value for the difference. A result like `-78.4% (p=0.000 n=10)` is a claim you can
defend in a PR; a single-run "22% faster" is not. The p-value is the guard against
calling noise a win: benchstat withholds a delta (prints `~`) when the variation is too
large relative to the difference to be significant.

Naming note: benchstat matches rows by benchmark name across files, so an A/B of two
*differently named* functions is cleanest done by giving both benchmarks the *same*
name in their respective source revisions (the real git workflow: benchmark on the old
commit into `old.txt`, on the new commit into `new.txt`, both named `BenchmarkDedup`).
This module keeps `Old` and `New` side by side for teaching, so the two `tee` commands
above capture each into its own file; when you compare, benchstat aligns them by the
shared prefix. In a real repository you would check out each revision and rerun the one
`BenchmarkDedup`.

Create `dedup.go`:

```go
package dedup

// DedupOld removes duplicate strings preserving first-seen order using a linear scan
// against the kept results: O(n^2).
func DedupOld(in []string) []string {
	var out []string
	for _, s := range in {
		if !contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func contains(xs []string, target string) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}

// DedupNew removes duplicate strings preserving first-seen order using a set: O(n).
func DedupNew(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/dedup"
)

func main() {
	in := []string{"a", "b", "a", "c", "b", "a"}
	fmt.Printf("old: %v\n", dedup.DedupOld(in))
	fmt.Printf("new: %v\n", dedup.DedupNew(in))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
old: [a b c]
new: [a b c]
```

### Tests

`TestEquivalence` is the correctness gate that licenses the A/B: for a range of inputs,
`DedupOld` and `DedupNew` must return identical slices, so any speed difference is a pure
optimization, not a behavior change. The two benchmarks feed a realistically sized input
with many duplicates so the quadratic cost of the old version is visible.

Create `dedup_test.go`:

```go
package dedup

import (
	"fmt"
	"reflect"
	"strconv"
	"testing"
)

func TestEquivalence(t *testing.T) {
	t.Parallel()
	inputs := [][]string{
		{},
		{"a"},
		{"a", "a", "a"},
		{"a", "b", "a", "c", "b"},
		{"z", "y", "x", "z", "y", "x", "w"},
	}
	for _, in := range inputs {
		old := DedupOld(in)
		neu := DedupNew(in)
		if !reflect.DeepEqual(old, neu) {
			t.Errorf("input %v: old %v != new %v", in, old, neu)
		}
	}
}

// bench builds an input of size n with a 50% duplicate rate.
func benchInput(n int) []string {
	in := make([]string, n)
	for i := range in {
		in[i] = "item" + strconv.Itoa(i%(n/2+1))
	}
	return in
}

func BenchmarkDedupOld(b *testing.B) {
	in := benchInput(2000)
	b.ReportAllocs()
	for b.Loop() {
		_ = DedupOld(in)
	}
}

func BenchmarkDedupNew(b *testing.B) {
	in := benchInput(2000)
	b.ReportAllocs()
	for b.Loop() {
		_ = DedupNew(in)
	}
}

func ExampleDedupNew() {
	fmt.Println(DedupNew([]string{"a", "b", "a", "c"}))
	// Output: [a b c]
}
```

Then run the statistical comparison:

```bash
go test -run='^$' -bench=BenchmarkDedupOld -count=10 | tee old.txt
go test -run='^$' -bench=BenchmarkDedupNew -count=10 | tee new.txt
go run golang.org/x/perf/cmd/benchstat@latest old.txt new.txt
```

Illustrative benchstat output (your numbers will differ, the shape will not):

```text
name       old time/op    new time/op    delta
Dedup-8     412µs ± 2%     18µs ± 3%     -95.6%  (p=0.000 n=10+10)
```

## Review

`TestEquivalence` proves old and new are the same function behaviorally, which is the
precondition that makes the benchmark an A/B of implementations rather than a comparison
of two different behaviors. The lesson is the workflow, not the specific numbers:
`-count=10` into two files and `benchstat` yields a delta *with a p-value*, and only that
p-value licenses the claim "this is faster" in a PR or a CI gate. A single laptop run
showing new ahead is worthless as evidence — rerun it and the margin wanders. Two further
disciplines the concepts stress apply here: never compare benchmark numbers across
different machines or a busy CI runner, and remember `allocs/op` (which benchstat also
compares) is the one figure stable enough to gate on as an absolute. The `go run
golang.org/x/perf/cmd/benchstat@latest` form fetches and runs the tool without a manual
install, which is the friction-free way to make this a habit.

## Resources

- [`benchstat`](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat) — statistical comparison of benchmark runs, medians, variation, and p-values.
- [go test benchmark flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — `-count`, `-bench`, `-run` for producing the result files.
- [`testing.B.Loop`](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 loop used by both benchmarks.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-allocation-regression-strings-builder.md](09-allocation-regression-strings-builder.md) | Next: [../06-fuzz-testing/00-concepts.md](../06-fuzz-testing/00-concepts.md)
