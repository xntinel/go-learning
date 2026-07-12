# Exercise 7: Does &T{} Escape? Allocation Cost of Pointer Constructors

Returning `&T{...}` from a constructor usually forces the value onto the heap;
returning `T` by value can keep it on the stack. On a hot path — a metric built
per request — that difference is a measurable allocation. This exercise builds two
constructors for a `Metric` DTO, benchmarks both, and uses `testing.AllocsPerRun`
to assert the value path allocates 0 while the pointer path allocates 1.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
metricalloc/                  independent module: example.com/metricalloc
  go.mod                      go 1.26
  metric.go                   Metric, NewValue() Metric, NewPointer() *Metric
  cmd/
    demo/
      main.go                 runnable demo: build both ways, print the constructed metric
  metric_test.go              AllocsPerRun assertions (0 vs 1) + benchmarks with ReportAllocs
```

Files: `metric.go`, `cmd/demo/main.go`, `metric_test.go`.
Implement: a `Metric` with `NewValue() Metric` (return by value) and
`NewPointer() *Metric` (return `&Metric{...}`).
Test: `testing.AllocsPerRun` asserts the value path allocates 0 and the pointer
path allocates 1; a `b.ReportAllocs()` benchmark for each; an annotated
`-gcflags=-m` comment block the learner can reproduce.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/03-new-vs-composite-literal/07-escape-analysis-constructor-cost/cmd/demo
cd go-solutions/09-pointers/03-new-vs-composite-literal/07-escape-analysis-constructor-cost
```

### Why the pointer escapes

Go's escape analysis decides, at compile time, whether a value can live on the
stack (allocated and freed with the function frame, essentially free) or must live
on the heap (a real allocation plus later GC work). A value returned *by value*
can stay on the stack: `func NewValue() Metric` hands back a copy, and the
original need not outlive the frame. A value whose *address* is returned cannot:
`func NewPointer() *Metric { return &Metric{...} }` returns a pointer whose
pointed-to `Metric` must remain alive after the constructor returns, so the
compiler moves it to the heap — it "escapes." You can see the decision with
`go build -gcflags=-m`, which prints a line like `&Metric{...} escapes to heap`
for the pointer constructor and nothing for the value constructor.

On a cold path this does not matter. On a hot path it does: a `Metric` built once
per request, per row, or per log line multiplies that per-call heap allocation by
your request rate, adding GC pressure you can measure. The senior rule of thumb is
to return small structs by value on hot paths and reserve pointer returns for
values that are large, must be shared/mutated, or must be nil-able. This exercise
makes the cost visible two ways: `b.ReportAllocs()` in a benchmark, and
`testing.AllocsPerRun`, which runs a function many times and returns the average
number of allocations — 0 for the value constructor, 1 for the pointer
constructor. The `//go:noinline` directive on both constructors keeps the compiler
from inlining them away and changing the measurement.

Create `metric.go`:

```go
package metricalloc

// Metric is a small hot-path DTO: the kind built once per request or per row.
type Metric struct {
	Name  string
	Value float64
	Count int64
}

// NewValue returns a Metric by value. The result can stay on the caller's stack;
// it does not force a heap allocation.
//
//go:noinline
func NewValue(name string, value float64) Metric {
	return Metric{Name: name, Value: value, Count: 1}
}

// NewPointer returns a *Metric via &Metric{...}. The pointed-to value must
// outlive this frame, so escape analysis moves it to the heap: one allocation
// per call.
//
//go:noinline
func NewPointer(name string, value float64) *Metric {
	return &Metric{Name: name, Value: value, Count: 1}
}
```

### The runnable demo

The demo builds a metric both ways and prints them; the observable values are
identical, and the difference (where the `Metric` lives) is what the tests measure.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/metricalloc"
)

func main() {
	v := metricalloc.NewValue("requests_total", 42)
	fmt.Printf("value:   %s=%.0f count=%d\n", v.Name, v.Value, v.Count)

	p := metricalloc.NewPointer("requests_total", 42)
	fmt.Printf("pointer: %s=%.0f count=%d\n", p.Name, p.Value, p.Count)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
value:   requests_total=42 count=1
pointer: requests_total=42 count=1
```

To reproduce the escape decision yourself:

```bash
go build -gcflags=-m ./... 2>&1 | grep -i metric
```

You will see a line reporting that the `&Metric{...}` literal in `NewPointer`
escapes to the heap, and no such line for `NewValue`. Annotated:

```text
./metric.go: &Metric{...} escapes to heap        <- NewPointer: the pointer escapes
(no escape line for NewValue's Metric{...})       <- value stays on the stack
```

### Tests

`TestValuePathAllocatesZero` and `TestPointerPathAllocatesOne` use
`testing.AllocsPerRun` to pin the per-call allocation counts. A package-level sink
consumes each result so the compiler cannot optimize the construction away. The
benchmarks report allocations with `b.ReportAllocs()`.

Create `metric_test.go`:

```go
package metricalloc

import (
	"fmt"
	"testing"
)

// Sinks prevent the compiler from optimizing the constructor calls away.
var (
	sinkValue   Metric
	sinkPointer *Metric
)

func TestValuePathAllocatesZero(t *testing.T) {
	avg := testing.AllocsPerRun(1000, func() {
		sinkValue = NewValue("m", 1)
	})
	if avg != 0 {
		t.Fatalf("NewValue allocs/op = %v, want 0 (value stays on stack)", avg)
	}
}

func TestPointerPathAllocatesOne(t *testing.T) {
	avg := testing.AllocsPerRun(1000, func() {
		sinkPointer = NewPointer("m", 1)
	})
	if avg != 1 {
		t.Fatalf("NewPointer allocs/op = %v, want 1 (&Metric{} escapes to heap)", avg)
	}
}

func BenchmarkNewValue(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		sinkValue = NewValue("m", 1)
	}
}

func BenchmarkNewPointer(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		sinkPointer = NewPointer("m", 1)
	}
}

func TestConstructorsAgreeOnFields(t *testing.T) {
	t.Parallel()

	v := NewValue("m", 3.5)
	p := NewPointer("m", 3.5)
	if v != *p {
		t.Fatalf("value %+v != pointer deref %+v", v, *p)
	}
}

// ExampleNewValue shows the value constructor's observable result; the difference
// from NewPointer is where the Metric lives, which the alloc tests measure.
func ExampleNewValue() {
	m := NewValue("requests_total", 42)
	fmt.Printf("%s=%.0f count=%d\n", m.Name, m.Value, m.Count)
	// Output:
	// requests_total=42 count=1
}
```

## Review

The measurement is the lesson: `TestValuePathAllocatesZero` asserts the
value-returning constructor allocates nothing, and `TestPointerPathAllocatesOne`
asserts the pointer-returning constructor allocates exactly once, because
`&Metric{...}` escapes to the heap. Two details make those assertions stable. The
package-level sinks (`sinkValue`, `sinkPointer`) force the results to be used, so
the compiler cannot delete the construction — and storing the *pointer* in a
global is precisely what guarantees the escape. The `//go:noinline` directives
stop the compiler from inlining the constructors into the test loop, which could
otherwise change the allocation decision. Reproduce the reasoning behind the
numbers with `go build -gcflags=-m`; the annotated output is in the demo section.
The takeaway is not "always return by value" — it is that on a hot path the
value-vs-pointer return is an allocation decision you can and should measure.

## Resources

- [testing.AllocsPerRun](https://pkg.go.dev/testing#AllocsPerRun) — average allocations per call.
- [testing.B.ReportAllocs](https://pkg.go.dev/testing#B.ReportAllocs) — per-op allocation counts in benchmarks.
- [Go FAQ: stack or heap](https://go.dev/doc/faq#stack_or_heap) — how the compiler decides, and why you usually should not care until you profile.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-sync-pool-buffer-reuse.md](08-sync-pool-buffer-reuse.md)
