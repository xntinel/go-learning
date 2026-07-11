# Exercise 2: Assert Allocation Behavior with AllocsPerRun

`go build -gcflags=-m` tells you what the compiler decided *today*. It does not
stop a refactor from silently moving a zero-alloc constructor onto the heap next
month. This module turns the compiler's claim into an executable regression
guard: a test that pins `BuildStack` to zero heap allocations and `BuildHeap` to
at least one, so an accidental escape fails CI instead of a flame graph.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
allocguard/                   independent module: example.com/allocguard
  go.mod                      go 1.26
  logx.go                     Entry; BuildStack (value), BuildHeap (*Entry, noinline);
                              package-level sinks to control escape in the guard
  cmd/
    demo/
      main.go                 prints measured allocs/op for both constructors
  logx_test.go                AllocsPerRun guards + a benchmark using b.Loop
```

Files: `logx.go`, `cmd/demo/main.go`, `logx_test.go`.
Implement: `BuildStack` (returns `Entry`), `BuildHeap` (returns `*Entry`, pinned
with `//go:noinline`), and typed sinks so the guard measures the real escape.
Test: `AllocsPerRun(1000, ...)` asserting `BuildStack == 0` and `BuildHeap >= 1`,
a reusable `assertMaxAllocs` helper, and a `b.Loop`/`ReportAllocs` benchmark.
Verify: `go test -count=1 -race ./...`, then `go test -bench=. -benchmem ./...`.

Set up the module:

```bash
mkdir -p ~/go-exercises/allocguard/cmd/demo
cd ~/go-exercises/allocguard
go mod init example.com/allocguard
```

### Why a sink, and why noinline

`testing.AllocsPerRun(runs, f)` runs `f` a few times to warm up, then `runs`
times, and returns the average number of heap allocations per call as measured by
`runtime.MemStats.Mallocs`. It is the executable form of the escape question: if
`f` performs no heap allocation, it returns `0`.

Two details make the measurement honest. First, the result of the constructor must
be *consumed*, or the compiler may optimize the whole call away and every path
reports zero — meaningless. We consume it by assigning to a package-level sink.
The choice of sink type is the subtle part: a value sink (`var sinkVal Entry`)
receives a copy and does not itself force a heap allocation, so `BuildStack` stays
at zero; a pointer sink (`var sinkPtr *Entry`) stores the address `BuildHeap`
returns, which is exactly the escape we want to measure. Assigning to a global is
what makes the escape observable rather than optimized into nothing.

Second, `BuildHeap` carries `//go:noinline`. Without it the compiler may inline
`BuildHeap` into the test loop and, seeing that the pointer only flows to a store,
merge the analysis in a way that can mask the allocation. `//go:noinline` pins the
function as a real call boundary so its escape behavior is the behavior you assert.
This is the standard technique for isolating a single function's allocation in a
test.

The `assertMaxAllocs` helper generalizes the guard: give it a name, a budget, and
a closure, and it fails the test with a clear message if the average exceeds the
budget. That one helper is what you drop next to any hot-path constructor to
freeze its allocation contract.

Create `logx.go`:

```go
package logx

import "time"

// Entry is a log record built two ways to contrast stack and heap allocation.
type Entry struct {
	Time    time.Time
	Level   string
	Message string
}

// SinkVal and SinkPtr consume constructor results in the allocation guard so the
// compiler cannot optimize the calls away. SinkVal takes a value copy (no heap
// allocation); SinkPtr stores an escaping address.
var (
	SinkVal Entry
	SinkPtr *Entry
)

// BuildStack returns an Entry by value; the local does not escape.
func BuildStack(level, msg string) Entry {
	return Entry{Time: time.Now(), Level: level, Message: msg}
}

// BuildHeap returns a pointer to a local; the value is moved to the heap.
// noinline pins the escape so the allocation guard measures it in isolation.
//
//go:noinline
func BuildHeap(level, msg string) *Entry {
	e := Entry{Time: time.Now(), Level: level, Message: msg}
	return &e
}
```

### The runnable demo

The demo measures both constructors with `testing.AllocsPerRun` and prints the
result. It is unusual to import `testing` from a `main`, but it is exactly the
point of this module: the allocation count is a first-class, printable number.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"testing"

	"example.com/allocguard"
)

func main() {
	stack := testing.AllocsPerRun(1000, func() {
		logx.SinkVal = logx.BuildStack("info", "hello")
	})
	heap := testing.AllocsPerRun(1000, func() {
		logx.SinkPtr = logx.BuildHeap("info", "hello")
	})
	fmt.Printf("BuildStack allocs/op: %.0f\n", stack)
	fmt.Printf("BuildHeap  allocs/op: %.0f\n", heap)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
BuildStack allocs/op: 0
BuildHeap  allocs/op: 1
```

### Tests

`TestBuildStackIsZeroAlloc` is the regression guard that matters: if a future
edit makes `BuildStack` escape, this test fails. `TestBuildHeapAllocates` asserts
the pointer constructor allocates at least once, documenting the cost.
`assertMaxAllocs` is the reusable helper. The benchmark uses `for b.Loop()`
(Go 1.24+), which runs the body an automatically-chosen number of times and is
the modern replacement for `for i := 0; i < b.N; i++`; with `b.ReportAllocs()` it
prints `allocs/op` and `B/op`.

Create `logx_test.go`:

```go
package logx

import "testing"

// assertMaxAllocs fails if f averages more than budget heap allocations per call.
func assertMaxAllocs(t *testing.T, name string, budget float64, f func()) {
	t.Helper()
	got := testing.AllocsPerRun(1000, f)
	if got > budget {
		t.Errorf("%s: allocs/op = %.2f, want <= %.0f (an escape regressed)", name, got, budget)
	}
}

func TestBuildStackIsZeroAlloc(t *testing.T) {
	assertMaxAllocs(t, "BuildStack", 0, func() {
		SinkVal = BuildStack("info", "hello")
	})
}

func TestBuildHeapAllocates(t *testing.T) {
	got := testing.AllocsPerRun(1000, func() {
		SinkPtr = BuildHeap("info", "hello")
	})
	if got < 1 {
		t.Errorf("BuildHeap allocs/op = %.2f, want >= 1 (pointer return must escape)", got)
	}
}

func BenchmarkBuildStack(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		SinkVal = BuildStack("info", "hello")
	}
}

func BenchmarkBuildHeap(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		SinkPtr = BuildHeap("info", "hello")
	}
}
```

## Review

The guard is correct when `TestBuildStackIsZeroAlloc` passes with a budget of
exactly `0` and `TestBuildHeapAllocates` sees at least one allocation — the
compiler's `-gcflags=-m` verdict, now enforced by CI. The two subtleties are the
ones that make or break the measurement: consume the result through a sink so the
call is not optimized away, and match the sink type to what you are measuring (a
value sink for the stack path, a pointer sink for the heap path). Run
`go test -bench=. -benchmem` to see the same contract as `allocs/op`. The mistake
to avoid is asserting a raw allocation count copied from one machine's
`-gcflags=-m` run; assert the *contract* (zero, or a small budget) so the guard is
portable across Go versions and the race detector, which can shift absolute counts.

## Resources

- [testing.AllocsPerRun](https://pkg.go.dev/testing#AllocsPerRun) — average heap allocations per call.
- [testing.B.Loop](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop.
- [testing.B.ReportAllocs](https://pkg.go.dev/testing#B.ReportAllocs) — reporting `allocs/op` and `B/op`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-log-pipeline-stack-vs-heap.md](01-log-pipeline-stack-vs-heap.md) | Next: [03-interface-boxing-in-hot-logger.md](03-interface-boxing-in-hot-logger.md)
