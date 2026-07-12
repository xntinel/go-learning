# 4. Benchmarking Methodology

Good benchmarks measure a stable workload and exclude setup that is not part of the operation under study. This lesson builds a small search library, tests its correctness, and includes benchmark functions that compare linear and binary search across input sizes.

```text
benchmethod/
  go.mod
  search.go
  search_test.go
  cmd/demo/main.go
```

## Concepts

### A Benchmark Must Measure One Workload

If setup changes with `b.N`, the benchmark is measuring both the code and a moving target. Generate input before the timed loop. Keep the timed loop focused on the operation being compared.

The package exposes `GenerateSorted` so benchmarks can create stable sorted data once, then benchmark only `LinearSearch` and `BinarySearch`.

### Correctness Comes Before Timing

Benchmarking a wrong implementation is just measuring a bug. Ordinary tests should prove that both implementations agree before benchmark results are trusted.

The test file uses table-driven tests for validation and agreement. The benchmarks are secondary artifacts: useful for measurement, but not a replacement for correctness tests.

### Sub-Benchmarks Make Comparisons Legible

`b.Run` records each size and algorithm under a separate benchmark name. That makes output easier to compare and gives `benchstat` structured names to group.

The lesson's benchmark names include both algorithm and input size, such as `BenchmarkSearch/linear/1000`.

### Repetition And Statistics Beat Single Runs

One benchmark run is noisy. Use `-count` to collect repeated results, then compare with `benchstat` when evaluating a change. For allocation-sensitive code, add `-benchmem` or call `b.ReportAllocs`.

Go 1.24 introduced `b.Loop`, which excludes setup before the loop and cleanup after the loop. This lesson uses `b.Loop` for benchmark bodies and still discusses `ResetTimer`, `StopTimer`, and `StartTimer` because older code and per-iteration setup still use them.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/26-memory-model-and-optimization/04-benchmarking-methodology/04-benchmarking-methodology/cmd/demo
cd go-solutions/26-memory-model-and-optimization/04-benchmarking-methodology/04-benchmarking-methodology
```

### Exercise 1: Build The Search Library

Create `search.go`:

```go
package benchmethod

import (
	"errors"
	"fmt"
	"sort"
)

var (
	ErrBadSize  = errors.New("size must be positive")
	ErrUnsorted = errors.New("input must be sorted")
)

type Result struct {
	Size        int
	Target      int
	LinearFound bool
	BinaryFound bool
}

func GenerateSorted(size int) ([]int, error) {
	if size <= 0 {
		return nil, fmt.Errorf("generate sorted: %w: got %d", ErrBadSize, size)
	}
	data := make([]int, size)
	for i := range data {
		data[i] = i * 2
	}
	return data, nil
}

func LinearSearch(sorted []int, target int) bool {
	for _, value := range sorted {
		if value == target {
			return true
		}
		if value > target {
			return false
		}
	}
	return false
}

func BinarySearch(sorted []int, target int) bool {
	idx := sort.SearchInts(sorted, target)
	return idx < len(sorted) && sorted[idx] == target
}

func Compare(sorted []int, target int) (Result, error) {
	if !sort.IntsAreSorted(sorted) {
		return Result{}, fmt.Errorf("compare: %w", ErrUnsorted)
	}
	return Result{
		Size:        len(sorted),
		Target:      target,
		LinearFound: LinearSearch(sorted, target),
		BinaryFound: BinarySearch(sorted, target),
	}, nil
}
```

### Exercise 2: Test Correctness And Validation

Create `search_test.go`:

```go
package benchmethod

import (
	"errors"
	"fmt"
	"testing"
)

func TestGenerateSortedValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		size    int
		wantErr error
	}{
		{name: "zero", size: 0, wantErr: ErrBadSize},
		{name: "negative", size: -1, wantErr: ErrBadSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := GenerateSorted(tc.size)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestCompareRejectsUnsortedInput(t *testing.T) {
	t.Parallel()

	_, err := Compare([]int{2, 1, 4}, 1)
	if !errors.Is(err, ErrUnsorted) {
		t.Fatalf("err = %v, want ErrUnsorted", err)
	}
}

func TestSearchImplementationsAgree(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		size   int
		target int
		found  bool
	}{
		{name: "first", size: 10, target: 0, found: true},
		{name: "middle", size: 10, target: 8, found: true},
		{name: "missing odd", size: 10, target: 9, found: false},
		{name: "past end", size: 10, target: 99, found: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, err := GenerateSorted(tc.size)
			if err != nil {
				t.Fatal(err)
			}
			result, err := Compare(data, tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if result.LinearFound != tc.found || result.BinaryFound != tc.found {
				t.Fatalf("result = %+v, want found=%v", result, tc.found)
			}
		})
	}
}

func BenchmarkSearch(b *testing.B) {
	sizes := []int{100, 1_000, 10_000}
	for _, size := range sizes {
		data, err := GenerateSorted(size)
		if err != nil {
			b.Fatal(err)
		}
		target := data[len(data)-1]
		b.Run(fmt.Sprintf("linear/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				LinearSearch(data, target)
			}
		})
		b.Run(fmt.Sprintf("binary/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				BinarySearch(data, target)
			}
		})
	}
}

func BenchmarkCompareWithPerIterationSetup(b *testing.B) {
	for b.Loop() {
		data, err := GenerateSorted(1_000)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := Compare(data, 998); err != nil {
			b.Fatal(err)
		}
	}
}

func ExampleCompare() {
	data, _ := GenerateSorted(5)
	result, _ := Compare(data, 6)
	fmt.Printf("size=%d target=%d linear=%v binary=%v\n", result.Size, result.Target, result.LinearFound, result.BinaryFound)
	// Output: size=5 target=6 linear=true binary=true
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"benchmethod"
)

func main() {
	data, err := benchmethod.GenerateSorted(10)
	if err != nil {
		log.Fatal(err)
	}
	result, err := benchmethod.Compare(data, 18)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("size=%d target=%d linear=%v binary=%v\n", result.Size, result.Target, result.LinearFound, result.BinaryFound)
}
```

After the verification gate passes, run the benchmark comparison:

```bash
go test -bench=BenchmarkSearch -benchmem -count=10 > results.txt
go install golang.org/x/perf/cmd/benchstat@latest
benchstat results.txt
```

Use `b.ResetTimer` when setup must happen once before a `b.N` loop in older benchmark style. Use `b.StopTimer` and `b.StartTimer` only when each iteration needs setup that should not be measured; timer calls have overhead.

## Common Mistakes

### Letting The Workload Change With b.N

Wrong: pass `b.N` as the input size and make every benchmark run measure a different problem.

Fix: choose explicit sizes and use sub-benchmarks. `BenchmarkSearch` uses fixed input sizes.

### Benchmarking Without Agreement Tests

Wrong: compare fast and slow functions without proving they return the same answers.

Fix: test correctness first. `TestSearchImplementationsAgree` proves both implementations agree on found and missing targets.

### Reading Too Much Into One Run

Wrong: claim a performance win from one `go test -bench` run.

Fix: collect repeated runs with `-count` and compare with `benchstat`.

## Verification

Run this from `~/go-exercises/benchmethod`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test with `target := -2` and assert that both search implementations report `false`.

## Summary

- Stable benchmarks keep setup separate from the measured operation.
- Correctness tests must pass before benchmark numbers matter.
- Sub-benchmarks make algorithm and size comparisons readable.
- `b.ReportAllocs` and `-benchmem` expose allocation costs.
- Use repeated runs and `benchstat` for performance conclusions.

## What's Next

Next: [Escape Analysis](../05-escape-analysis/05-escape-analysis.md).

## Resources

- [testing package: Benchmarks](https://pkg.go.dev/testing#hdr-Benchmarks)
- [testing.B documentation](https://pkg.go.dev/testing#B)
- [benchstat command](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat)
- [Go benchmark data format](https://go.dev/design/14313-benchmark-format)
