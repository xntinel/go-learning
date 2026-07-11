# 2. GOMAXPROCS and Processor Binding

GOMAXPROCS controls the number of P's (logical processors) the Go scheduler uses simultaneously. Setting it above NumCPU does not increase parallelism for CPU-bound work, but setting it below can artificially serialize goroutines. This lesson builds a testable benchmarking package that makes the CPU-bound vs I/O-bound distinction concrete.

## Concepts

### What GOMAXPROCS Does

A P is a logical processor — a resource that a goroutine needs before it can run. The runtime creates exactly `GOMAXPROCS` P's at startup (or when you call `runtime.GOMAXPROCS(n)`). Only one goroutine executes on each P at a time, but thousands of goroutines can exist simultaneously in wait states.

`runtime.GOMAXPROCS(0)` reads the current value without changing it. `runtime.GOMAXPROCS(n)` sets it to n and returns the previous value. The default since Go 1.5 is `runtime.NumCPU()`.

### CPU-Bound vs I/O-Bound Work

For CPU-bound work — arithmetic, compression, hashing — parallelism is capped at the number of physical cores. Adding more P's than cores gives the OS more threads to schedule but does not produce more actual computation per second. The overhead of extra goroutines and context switches can slow things down.

For I/O-bound work — network calls, disk reads, waiting on channels — goroutines block in the runtime and release their P for others to use. Here, having more runnable goroutines than P's is normal and efficient; GOMAXPROCS has little effect on throughput.

### Container Environments

`runtime.NumCPU()` calls the OS to count CPUs visible to the process. In containers, this reports the host's total CPU count, not the cgroup CPU quota. A container limited to 0.5 CPUs on a 32-core host returns 32 from `NumCPU()`, causing the program to create 32 P's and 32 OS threads competing for half a core. The `uber-go/automaxprocs` package reads the cgroup quota and sets GOMAXPROCS accordingly.

### EqualPartitions and ParallelSum Design

The library separates concerns: `EqualPartitions` produces the work split, `ParallelSum` runs it. The caller controls `GOMAXPROCS` before invoking `ParallelSum`; the library never touches scheduler settings. This makes the functions composable and testable in isolation.

## Exercises

### Exercise 1: Understand SumRange

Read `SumRange` and verify its boundary conditions manually. Confirm that `SumRange(0, 5)` returns 10 (0+1+2+3+4) and that `SumRange(1, 5)` also returns 10 (1+2+3+4). Write down what `SumRange(n, n)` returns for any n.

### Exercise 2: Verify EqualPartitions Coverage

Call `EqualPartitions(10, 3)` and verify that the three ranges cover [0,10) with no gaps and no overlap. Sum the sizes of all returned ranges and confirm they equal n.

### Exercise 3: Benchmark Under Different GOMAXPROCS Values

Wrap `ParallelSum` in a benchmark. Call `runtime.GOMAXPROCS(p)` before the benchmark loop, varying p from 1 to `runtime.NumCPU()`. Observe at which p value throughput stops improving and overhead begins to dominate.

## Common Mistakes

Wrong: Calling `runtime.GOMAXPROCS(n)` inside `ParallelSum` itself. What happens: the function's caller loses control of scheduler settings; tests that set GOMAXPROCS before calling get overridden. Fix: The caller controls GOMAXPROCS; the library function only partitions and sums.

Wrong: Using procs=0 in `EqualPartitions`. What happens: division by zero panic. Fix: Guard with `if procs <= 0 { procs = 1 }`.

Wrong: Assuming `ParallelSum` with procs > NumCPU is faster. What happens: goroutines time-share the same cores; overhead of extra goroutines adds latency. Fix: Benchmark with real data; the sweet spot for CPU-bound work is usually procs == NumCPU.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

GOMAXPROCS sets the number of P's and therefore the maximum parallelism for CPU-bound goroutines. The default of NumCPU() is correct for most workloads. Container environments need cgroup-aware GOMAXPROCS adjustment. The library pattern used here keeps scheduler control in the caller, not the library.

## What's Next

[03. Work Stealing](../03-work-stealing/03-work-stealing.md)

## Resources

- https://pkg.go.dev/runtime#GOMAXPROCS
- https://pkg.go.dev/runtime#NumCPU
- https://github.com/uber-go/automaxprocs
- https://www.ardanlabs.com/blog/2018/08/scheduling-in-go-part2.html

---

Create `go.mod`

```go
// go.mod
module example.com/procbench

go 1.26
```

Create `procbench.go`

```go
package procbench

import (
	"sync"
)

// SumRange returns the sum of integers in [lo, hi).
func SumRange(lo, hi int) int {
	s := 0
	for i := lo; i < hi; i++ {
		s += i
	}
	return s
}

// EqualPartitions splits [0,n) into procs contiguous ranges.
// If n < procs, some ranges may be empty [x,x).
func EqualPartitions(n, procs int) [][2]int {
	if procs <= 0 {
		procs = 1
	}
	parts := make([][2]int, procs)
	size := n / procs
	rem := n % procs
	lo := 0
	for i := range parts {
		extra := 0
		if i < rem {
			extra = 1
		}
		hi := lo + size + extra
		parts[i] = [2]int{lo, hi}
		lo = hi
	}
	return parts
}

// ParallelSum divides [0,n) into procs chunks and sums each in a goroutine.
// The caller controls GOMAXPROCS before calling this function.
func ParallelSum(n, procs int) int {
	parts := EqualPartitions(n, procs)
	results := make([]int, procs)
	var wg sync.WaitGroup
	for i, p := range parts {
		wg.Add(1)
		go func(idx, lo, hi int) {
			defer wg.Done()
			results[idx] = SumRange(lo, hi)
		}(i, p[0], p[1])
	}
	wg.Wait()
	total := 0
	for _, r := range results {
		total += r
	}
	return total
}
```

Create `procbench_test.go`

```go
package procbench

import (
	"testing"
)

func TestSumRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		lo, hi, want int
	}{
		{0, 0, 0},
		{0, 1, 0},
		{0, 5, 10},
		{1, 5, 10},
		{3, 7, 18},
	}
	for _, tc := range cases {
		got := SumRange(tc.lo, tc.hi)
		if got != tc.want {
			t.Errorf("SumRange(%d,%d) = %d, want %d", tc.lo, tc.hi, got, tc.want)
		}
	}
}

func TestEqualPartitionsCoverRange(t *testing.T) {
	t.Parallel()
	cases := []struct{ n, procs int }{{0, 1}, {10, 1}, {10, 3}, {100, 4}, {7, 3}}
	for _, tc := range cases {
		parts := EqualPartitions(tc.n, tc.procs)
		if len(parts) != tc.procs {
			t.Errorf("EqualPartitions(%d,%d) len=%d want %d", tc.n, tc.procs, len(parts), tc.procs)
			continue
		}
		if parts[0][0] != 0 {
			t.Errorf("EqualPartitions(%d,%d): first lo=%d want 0", tc.n, tc.procs, parts[0][0])
		}
		if parts[len(parts)-1][1] != tc.n {
			t.Errorf("EqualPartitions(%d,%d): last hi=%d want %d", tc.n, tc.procs, parts[len(parts)-1][1], tc.n)
		}
		for i := 1; i < len(parts); i++ {
			if parts[i][0] != parts[i-1][1] {
				t.Errorf("EqualPartitions(%d,%d): gap between part %d and %d", tc.n, tc.procs, i-1, i)
			}
		}
	}
}

func TestParallelSumMatchesSequential(t *testing.T) {
	t.Parallel()
	n := 10000
	want := SumRange(0, n)
	for _, procs := range []int{1, 2, 4, 8} {
		got := ParallelSum(n, procs)
		if got != want {
			t.Errorf("ParallelSum(%d,%d) = %d, want %d", n, procs, got, want)
		}
	}
}
```

Create `example_test.go`

```go
package procbench_test

import (
	"fmt"

	"example.com/procbench"
)

func ExampleSumRange() {
	fmt.Println(procbench.SumRange(0, 5))
	// Output:
	// 10
}
```

Create `cmd/demo/main.go`

```go
package main

import (
	"fmt"
	"runtime"
	"time"

	"example.com/procbench"
)

func main() {
	n := 50_000_000
	fmt.Printf("Summing [0,%d) with varying GOMAXPROCS\n", n)
	fmt.Printf("Sequential reference: %d\n\n", procbench.SumRange(0, n))

	for _, procs := range []int{1, 2, runtime.NumCPU()} {
		prev := runtime.GOMAXPROCS(procs)
		start := time.Now()
		got := procbench.ParallelSum(n, procs)
		elapsed := time.Since(start)
		runtime.GOMAXPROCS(prev)
		fmt.Printf("GOMAXPROCS=%d procs=%d result=%d time=%v\n", procs, procs, got, elapsed)
	}
}
```
