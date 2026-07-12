# 6. cgo Performance Overhead

Every cgo call pays a fixed overhead of roughly 50-200 ns on modern hardware regardless of how much work the C function does. The cost comes from four sources that stack on top of each other: switching from Go's growable goroutine stack to a fixed C-compatible stack, notifying the Go scheduler that the goroutine is entering a blocking operation, setting up signal masks, and the `cgocall` trampoline itself. For a C function that takes 5 ns, this overhead is 10-40x the work. For a C function that processes a 100,000-element array, the overhead is noise.

Knowing the crossover point — the amount of work per cgo call at which the overhead is justified — is the practical skill this lesson builds. You measure the overhead directly, then apply batching to amortize it, then observe how cgo pins goroutines to OS threads and limits concurrency scaling.

```text
cgoperf/
  go.mod
  cbridge.go
  batch_test.go
  parallel_test.go
```

## Concepts

### What the cgo Boundary Costs

When Go calls a C function, the runtime must:

1. **Save the goroutine state** and switch to a C-compatible stack. Go goroutines use a segmented/growable stack starting at a few kilobytes; C assumes a large fixed stack (typically 1-8 MB). The switch involves a `MOVQ` of the stack pointer and bookkeeping in the `m` (machine/thread) struct.
2. **Inform the scheduler** that this goroutine is about to enter a long-running (from the scheduler's perspective) operation. The scheduler may create a new OS thread so other goroutines can run.
3. **Configure signal handling** so that C signals are not delivered to the Go runtime's signal handler unexpectedly.
4. **Call the C function** through the `cgocall` assembly trampoline.

On return, all four steps are reversed. The `go tool pprof` CPU profile labels this cost `runtime.cgocall`.

### Thread Pinning and GOMAXPROCS

A goroutine making a cgo call is pinned to its OS thread (`m`) for the duration of the call. If 1000 goroutines all call cgo simultaneously, the Go runtime must create up to 1000 OS threads to service them — far more than `GOMAXPROCS` (typically the number of CPU cores). Thread creation is expensive, context switching between OS threads at that scale saturates the scheduler, and the per-thread stack memory adds up quickly.

Pure Go goroutines do not have this problem: the scheduler multiplexes many goroutines onto a small number of OS threads.

### Batching: Amortizing the Fixed Cost

If you need to process N elements and each element requires a C operation, the worst strategy is N cgo calls of 1 element each. The overhead is paid N times. The best strategy is 1 cgo call over N elements (a batch). The overhead is paid once; the per-element work in C is performed at C speed.

The ratio of per-element work to per-call overhead determines the breakeven:

- At N=10, a simple operation (say, squaring an integer): overhead dominates; cgo is 10-50x slower than pure Go.
- At N=1000: the work amortizes the overhead; cgo and pure Go are roughly comparable.
- At N=10000 with a computation the C compiler vectorizes (SIMD): C may be faster.

For simple arithmetic that the Go compiler can also vectorize, the breakeven is in the thousands of elements. For operations that require a specialized C library (FFT, compression, encryption), the breakeven is much lower because the C code is algorithmically superior.

### When cgo Is Worth It

Use cgo when:
- A C library provides capability Go cannot replicate at the same quality (SQLite, OpenSSL, BLAS, libz, libpng).
- The C function operates on large buffers (kilobytes to megabytes) so the fixed overhead is proportionally small.
- You can batch: call C once for many elements rather than once per element.

Do not use cgo when:
- The C function is trivial arithmetic replicable in pure Go.
- You call it more than ~1M times per second from the same goroutine (the ~100 ns overhead becomes ~100 ms per second).
- You need to cross the boundary from thousands of concurrent goroutines.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/28-unsafe-and-cgo/06-cgo-performance-overhead/06-cgo-performance-overhead
cd go-solutions/28-unsafe-and-cgo/06-cgo-performance-overhead/06-cgo-performance-overhead
```

This module requires a C compiler (`gcc` or `clang`). Check availability:

```bash
go env CGO_ENABLED   # must print 1
cc --version         # must print a version
```

### Exercise 1: The C Bridge

Go rejects `import "C"` in test files. All cgo declarations belong in a regular
`.go` file in the same package; test files call the exported Go wrappers.

Create `cbridge.go`:

```go
package cgoperf

/*
#include <stdint.h>

// noop: no work, pure boundary-crossing cost
static int32_t cgo_noop(void) { return 0; }

// add: minimal arithmetic — measures overhead + 1 op
static int32_t cgo_add(int32_t a, int32_t b) { return a + b; }

// sum_n: O(n) loop — lets you vary work per call
static int32_t cgo_sum_n(int32_t n) {
    int32_t total = 0;
    for (int32_t i = 0; i < n; i++) total += i;
    return total;
}

// process_one: per-element transform
static int32_t process_one(int32_t x) { return x * x + 1; }

// process_batch: batch transform — called once for N elements
static void process_batch(const int32_t *in, int32_t *out, int n) {
    for (int i = 0; i < n; i++) out[i] = in[i] * in[i] + 1;
}
*/
import "C"

import "unsafe"

// Go wrappers — test files call these, never the C symbols directly.

func cgoNoop() int32              { return int32(C.cgo_noop()) }
func cgoAdd(a, b int32) int32     { return int32(C.cgo_add(C.int32_t(a), C.int32_t(b))) }
func cgoSumN(n int32) int32       { return int32(C.cgo_sum_n(C.int32_t(n))) }
func cgoProcessOne(x int32) int32 { return int32(C.process_one(C.int32_t(x))) }
func cgoProcessBatch(in, out []int32) {
	C.process_batch(
		(*C.int32_t)(unsafe.Pointer(&in[0])),
		(*C.int32_t)(unsafe.Pointer(&out[0])),
		C.int(len(in)),
	)
}

// Pure Go equivalents for comparison.

func goNoop() int32          { return 0 }
func goAdd(a, b int32) int32 { return a + b }
func goSumN(n int32) int32 {
	var total int32
	for i := int32(0); i < n; i++ {
		total += i
	}
	return total
}
func goBatchProcess(in []int32) []int32 {
	out := make([]int32, len(in))
	for i, x := range in {
		out[i] = x*x + 1
	}
	return out
}
```

### Exercise 2: Measure Raw Call Overhead

Create `batch_test.go`:

```go
package cgoperf

import "testing"

// BenchmarkCgoNoop and BenchmarkGoNoop measure the raw boundary-crossing cost.
// The Go noop will inline to nearly nothing; the ratio is the overhead.
func BenchmarkCgoNoop(b *testing.B) {
	var r int32
	for i := 0; i < b.N; i++ {
		r = cgoNoop()
	}
	_ = r
}

func BenchmarkGoNoop(b *testing.B) {
	var r int32
	for i := 0; i < b.N; i++ {
		r = goNoop()
	}
	_ = r
}

func BenchmarkCgoAdd(b *testing.B) {
	var r int32
	for i := 0; i < b.N; i++ {
		r = cgoAdd(3, 4)
	}
	_ = r
}

func BenchmarkGoAdd(b *testing.B) {
	var r int32
	for i := 0; i < b.N; i++ {
		r = goAdd(3, 4)
	}
	_ = r
}

// Work-per-call amortization.

func BenchmarkCgoSum10(b *testing.B) {
	for i := 0; i < b.N; i++ {
		cgoSumN(10)
	}
}
func BenchmarkGoSum10(b *testing.B) {
	for i := 0; i < b.N; i++ {
		goSumN(10)
	}
}

func BenchmarkCgoSum1000(b *testing.B) {
	for i := 0; i < b.N; i++ {
		cgoSumN(1000)
	}
}
func BenchmarkGoSum1000(b *testing.B) {
	for i := 0; i < b.N; i++ {
		goSumN(1000)
	}
}

func BenchmarkCgoSum10000(b *testing.B) {
	for i := 0; i < b.N; i++ {
		cgoSumN(10000)
	}
}
func BenchmarkGoSum10000(b *testing.B) {
	for i := 0; i < b.N; i++ {
		goSumN(10000)
	}
}

// Batching: 1 cgo call vs N cgo calls for N=1000 elements.

func BenchmarkCgoOneByOne1000(b *testing.B) {
	data := make([]int32, 1000)
	for i := range data {
		data[i] = int32(i)
	}
	out := make([]int32, 1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range data {
			out[j] = cgoProcessOne(data[j])
		}
	}
	_ = out
}

func BenchmarkCgoBatch1000(b *testing.B) {
	data := make([]int32, 1000)
	for i := range data {
		data[i] = int32(i)
	}
	out := make([]int32, 1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cgoProcessBatch(data, out)
	}
	_ = out
}

func BenchmarkGoBatch1000(b *testing.B) {
	data := make([]int32, 1000)
	for i := range data {
		data[i] = int32(i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = goBatchProcess(data)
	}
}
```

### Exercise 3: Concurrency Scaling

Create `parallel_test.go`:

```go
package cgoperf

import "testing"

// BenchmarkCgoParallel and BenchmarkGoParallel show how cgo overhead
// interacts with GOMAXPROCS. Run with -cpu=1,2,4,8 to vary parallelism.
func BenchmarkCgoParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			cgoAdd(1, 2)
		}
	})
}

func BenchmarkGoParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			goAdd(1, 2)
		}
	})
}
```

Run the benchmarks:

```bash
# Raw overhead
go test -bench='BenchmarkCgoNoop|BenchmarkGoNoop' -benchmem -count=5

# Work-per-call amortization
go test -bench='BenchmarkCgoSum|BenchmarkGoSum' -benchmem

# Batching: compare one-by-one vs batch vs pure Go
go test -bench='Benchmark.*1000' -benchmem

# Concurrency scaling
go test -bench='BenchmarkCgoParallel|BenchmarkGoParallel' -cpu=1,2,4,8 -benchmem
```

### Exercise 4: Interpreting Results

The benchmark numbers will vary by CPU model and OS, but the ratios are consistent:

| Benchmark pair | Expected ratio | Explanation |
|---|---|---|
| CgoNoop / GoNoop | 50-200x | Pure boundary-crossing cost (~100 ns vs sub-ns) |
| CgoSum10 / GoSum10 | 10-50x | Overhead dominates 10 iterations of simple work |
| CgoSum1000 / GoSum1000 | ~1-3x | Work begins to amortize the overhead |
| CgoSum10000 / GoSum10000 | 0.5-2x | Near parity; C loop may compile more aggressively |
| CgoOneByOne1000 / CgoBatch1000 | 100-1000x | 1000 boundary crossings vs 1 |
| GoBatch1000 / CgoBatch1000 | 0.5-2x | Pure Go is competitive for simple arithmetic |

The parallel benchmarks show cgo scaling less well with increasing `-cpu`: each cgo call pins one OS thread, so `-cpu=8` with cgo requires 8 OS threads active simultaneously versus the Go scheduler multiplexing 8 goroutines.

Profile the cgo overhead directly:

```bash
go test -bench=BenchmarkCgoNoop -cpuprofile=cpu.out -count=5
go tool pprof -top cpu.out
```

Look for `runtime.cgocall` and `runtime.exitsyscall` in the profile to see the boundary-crossing cost separated from the C function work.

## Common Mistakes

### Calling cgo Per Element Instead of Per Batch

Wrong:

```go
for _, x := range data {
	result = append(result, C.process_one(C.int32_t(x))) // 1 cgo call per element
}
```

What happens: for 1000 elements, 1000 boundary crossings × ~100 ns = ~100 µs just in overhead. The C function's actual work is irrelevant at this scale.

Fix: batch the data and call through the Go wrapper once:

```go
cgoProcessBatch(data, out) // 1 boundary crossing for all 1000 elements
```

### Assuming C Is Always Faster

Wrong: replacing a pure-Go function with a cgo call because "C is faster". For simple arithmetic on small inputs, cgo is 10-100x slower due to overhead.

Fix: benchmark both versions at your actual batch size. Use cgo only when the C library provides a capability or algorithmic advantage Go cannot match (SIMD intrinsics, a complex algorithm in a well-maintained library, hardware access).

### Ignoring CGO_ENABLED=0

Wrong: testing a package that uses cgo with `CGO_ENABLED=0` and being surprised it does not compile.

Fix: provide a pure-Go fallback behind a build tag if the package needs to work without a C compiler:

```go
//go:build !cgo
```

The Go standard library itself does this for `net` and `os/user`.

### Spawning Many Goroutines That Each Make cgo Calls

Wrong:

```go
for i := 0; i < 10000; i++ {
	go func() {
		cgoAdd(1, 2) // each goroutine pins an OS thread for the call duration
	}()
}
```

What happens: the runtime creates up to 10000 OS threads, each holding a stack of 1-8 MB. Memory and scheduler pressure spike.

Fix: use a worker pool with a bounded number of goroutines making cgo calls. The pool controls the maximum thread count.

## Verification

From `~/go-exercises/cgoperf`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -bench=BenchmarkCgoNoop -benchmem -count=3
go test -bench='Benchmark.*1000' -benchmem
```

The first four commands must complete cleanly. Verify these observations from the benchmark output:

- `BenchmarkCgoNoop` is at least 50x slower than `BenchmarkGoNoop` (the raw boundary cost).
- `BenchmarkCgoOneByOne1000` is at least 50x slower than `BenchmarkCgoBatch1000` (batching works).
- `BenchmarkGoBatch1000` and `BenchmarkCgoBatch1000` are within 3x of each other (pure Go is competitive for simple arithmetic).

Note: this module requires `CGO_ENABLED=1` and a working C compiler. It will not compile offline with `CGO_ENABLED=0`. Validated by §15 prose/code consistency + gofmt + go vet on the extractable Go portions.

## Summary

- Every cgo call has a fixed overhead of ~50-200 ns: stack switch, scheduler notification, signal setup, and the `cgocall` trampoline.
- The overhead is constant regardless of how much work the C function does. Amortize it by batching: one cgo call over N elements instead of N calls over 1 element.
- cgo calls pin goroutines to OS threads. Many concurrent cgo calls require many OS threads, which limits concurrency scaling.
- Use cgo when C provides a capability or algorithmic advantage Go cannot replicate. Benchmark before assuming C is faster for simple operations.
- `CGO_ENABLED=0` eliminates cgo entirely and is the right choice for pure-Go builds, cross-compilation targets without a C toolchain, or high-security deployments.

## What's Next

Next: [unsafe.Slice and unsafe.String](../07-unsafe-slice-and-string/07-unsafe-slice-and-string.md).

## Resources

- [cmd/cgo reference](https://pkg.go.dev/cmd/cgo) — the authoritative cgo documentation
- [Dave Cheney: cgo is not Go](https://dave.cheney.net/2016/01/18/cgo-is-not-go) — tradeoffs and when to avoid it
- [Go wiki: cgo](https://go.dev/wiki/cgo) — best practices including batching and thread limits
- [testing package: Benchmarks](https://pkg.go.dev/testing#hdr-Benchmarks) — how to write and interpret Go benchmarks
- [Go blog: Profiling Go Programs](https://go.dev/blog/pprof) — using pprof to find cgocall overhead
