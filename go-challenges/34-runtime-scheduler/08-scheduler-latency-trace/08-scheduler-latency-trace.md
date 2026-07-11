# 8. Scheduler Latency Trace

Scheduling latency is the gap between when a goroutine becomes runnable — when it is created or unblocked — and when it actually starts executing. This latency is invisible to most profilers because by the time a goroutine runs, the clock starts. Measuring it requires recording `time.Now()` *before* the `go` statement and comparing it to `time.Now()` inside the goroutine body. The resulting distribution reveals how saturated the scheduler is and how much P-stealing is needed.

## Concepts

### What Scheduling Latency Measures

When you write `go f()`, the new goroutine is placed on the creating P's local run queue. It does not run until a P picks it up. The time between enqueueing and first instruction of `f` is the scheduling latency. Under low load this is nanoseconds; under high load or with GOMAXPROCS=1 it can reach milliseconds.

### Why Latency Distributions Matter

A single average latency hides bimodal behavior: most goroutines may run in under 10 µs, while occasional goroutines sit in the queue for tens of milliseconds when all P's are busy. P99 latency (99th percentile) exposes the tail that an average does not.

### Measurement Bias

Recording `time.Now()` before `go` and inside the goroutine captures scheduling latency plus goroutine creation overhead (a few hundred nanoseconds), plus clock read overhead. For practical latency analysis this is acceptable — the scheduling contribution dominates under any real load. Do not use this technique for sub-microsecond measurements.

### The Role of GOMAXPROCS

With GOMAXPROCS=1 all goroutines share one P, so latency rises sharply as the queue grows. With GOMAXPROCS=runtime.NumCPU(), goroutines can start on any available P, keeping latency low under moderate load. Work stealing further reduces latency by redistributing goroutines from busy P's to idle ones.

## Exercises

Module path: `example.com/latency`. Set up the module:

```go
// go.mod
module example.com/latency

go 1.26
```

### Exercise 1: Implement the latency package

Create `latency.go`:

```go
// latency.go
package latency

import (
	"fmt"
	"sort"
	"time"
)

// Sample holds a single scheduling latency measurement.
type Sample struct {
	Wait time.Duration
}

// Probe launches n goroutines and records the time between goroutine creation
// and when the goroutine body first runs. Returns n samples.
func Probe(n int) []Sample {
	if n <= 0 {
		return nil
	}
	results := make(chan Sample, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		go func() {
			wait := time.Since(start)
			results <- Sample{Wait: wait}
		}()
	}
	samples := make([]Sample, n)
	for i := 0; i < n; i++ {
		samples[i] = <-results
	}
	return samples
}

// Histogram distributes samples into buckets. Each element of buckets is an
// upper bound (exclusive). A sample with Wait < buckets[0] falls into label
// "< <buckets[0]>". Samples that exceed all buckets fall into the overflow
// label ">= <buckets[last]>". Returns a map of label to count.
func Histogram(samples []Sample, buckets []time.Duration) map[string]int {
	h := make(map[string]int)
	if len(buckets) == 0 {
		h[">= 0s"] = len(samples)
		return h
	}
	overflow := fmt.Sprintf(">= %v", buckets[len(buckets)-1])
	for _, s := range samples {
		placed := false
		for _, b := range buckets {
			if s.Wait < b {
				h[fmt.Sprintf("< %v", b)]++
				placed = true
				break
			}
		}
		if !placed {
			h[overflow]++
		}
	}
	return h
}

// P99 returns the 99th percentile Wait duration from samples.
// Returns 0 for an empty slice.
func P99(samples []Sample) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	cp := make([]Sample, len(samples))
	copy(cp, samples)
	sort.Slice(cp, func(i, j int) bool {
		return cp[i].Wait < cp[j].Wait
	})
	idx := int(float64(len(cp)) * 0.99)
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx].Wait
}
```

### Exercise 2: Write the test file

Create `latency_test.go`:

```go
// latency_test.go
package latency

import (
	"testing"
	"time"
)

func TestProbeReturnsNSamples(t *testing.T) {
	t.Parallel()
	got := Probe(100)
	if len(got) != 100 {
		t.Errorf("Probe(100) returned %d samples, want 100", len(got))
	}
}

func TestProbeZero(t *testing.T) {
	t.Parallel()
	got := Probe(0)
	if len(got) != 0 {
		t.Errorf("Probe(0) returned %d samples, want 0", len(got))
	}
}

func TestHistogramSumsToN(t *testing.T) {
	t.Parallel()
	samples := Probe(50)
	buckets := []time.Duration{
		10 * time.Microsecond,
		1 * time.Millisecond,
		10 * time.Millisecond,
	}
	h := Histogram(samples, buckets)
	total := 0
	for _, v := range h {
		total += v
	}
	if total != len(samples) {
		t.Errorf("histogram total = %d, want %d", total, len(samples))
	}
}

func TestP99NonNegative(t *testing.T) {
	t.Parallel()
	samples := Probe(50)
	p := P99(samples)
	if p < 0 {
		t.Errorf("P99 = %v, want >= 0", p)
	}
}

func TestHistogramWithKnownData(t *testing.T) {
	t.Parallel()
	samples := []Sample{
		{Wait: 5 * time.Microsecond},
		{Wait: 50 * time.Microsecond},
		{Wait: 5 * time.Millisecond},
		{Wait: 50 * time.Millisecond},
	}
	buckets := []time.Duration{
		10 * time.Microsecond,
		1 * time.Millisecond,
		10 * time.Millisecond,
	}
	h := Histogram(samples, buckets)

	cases := []struct {
		label string
		want  int
	}{
		{"< 10µs", 1},
		{"< 1ms", 1},
		{"< 10ms", 1},
		{">= 10ms", 1},
	}
	for _, tc := range cases {
		if got := h[tc.label]; got != tc.want {
			t.Errorf("h[%q] = %d, want %d", tc.label, got, tc.want)
		}
	}
}

func TestP99Empty(t *testing.T) {
	t.Parallel()
	if P99(nil) != 0 {
		t.Error("P99(nil) should return 0")
	}
}
```

### Exercise 3: Example and demo

Create `example_test.go`:

```go
// example_test.go
package latency_test

import (
	"example.com/latency"
	"fmt"
	"time"
)

func ExampleHistogram() {
	samples := []latency.Sample{
		{Wait: 5 * time.Microsecond},
		{Wait: 50 * time.Microsecond},
		{Wait: 5 * time.Millisecond},
		{Wait: 50 * time.Millisecond},
	}
	buckets := []time.Duration{
		10 * time.Microsecond,
		1 * time.Millisecond,
		10 * time.Millisecond,
	}
	h := latency.Histogram(samples, buckets)
	fmt.Println(h["< 10µs"])
	fmt.Println(h["< 1ms"])
	fmt.Println(h["< 10ms"])
	fmt.Println(h[">= 10ms"])
	// Output:
	// 1
	// 1
	// 1
	// 1
}
```

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"example.com/latency"
	"fmt"
	"time"
)

func main() {
	const n = 200
	fmt.Printf("probing scheduling latency with %d goroutines...\n", n)
	samples := latency.Probe(n)

	buckets := []time.Duration{
		1 * time.Microsecond,
		10 * time.Microsecond,
		100 * time.Microsecond,
		1 * time.Millisecond,
		10 * time.Millisecond,
	}
	h := latency.Histogram(samples, buckets)

	labels := []string{
		"< 1µs",
		"< 10µs",
		"< 100µs",
		"< 1ms",
		"< 10ms",
		">= 10ms",
	}

	fmt.Println("\nhistogram:")
	for _, label := range labels {
		count := h[label]
		if count > 0 {
			fmt.Printf("  %-12s: %d\n", label, count)
		}
	}
	fmt.Printf("\np99 latency: %v\n", latency.P99(samples))
}
```

## Common Mistakes

**Wrong**: Capturing `time.Now()` inside the goroutine body instead of before the `go` statement.

What happens: You measure zero latency because you start the clock when the goroutine is already running. The whole point is to capture the time *before* the goroutine is scheduled.

**Fix**: Always capture `start := time.Now()` on the line before `go func() { ... }()`, not inside the goroutine.

---

**Wrong**: Building a histogram by printing values and comparing visually rather than by asserting bucket counts in tests.

What happens: Visual output varies between runs (different scheduling latencies), so eye-balled comparisons are not reliable. The test is effectively never failing.

**Fix**: Use `TestHistogramWithKnownData` with a fixed `[]Sample` so the expected counts are fully deterministic.

---

**Wrong**: Computing P99 on the original slice, mutating it via sort.Slice.

What happens: `sort.Slice` sorts in place. If the caller retains the original slice and expects it to be in insertion order, the silent mutation is a bug.

**Fix**: Copy the slice before sorting: `cp := make([]Sample, len(samples)); copy(cp, samples)`.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Run the demo:

```bash
go run ./cmd/demo
```

## Summary

- Scheduling latency is the time between goroutine creation (or unblocking) and first instruction execution.
- Measure it by recording `time.Now()` before `go` and comparing to `time.Now()` at goroutine start.
- Use P99 to expose tail latency hidden by averages.
- `Histogram` with bucket upper bounds reveals the latency distribution shape.
- Under high GOMAXPROCS load, scheduling latency is typically sub-100 µs; under saturation it can reach milliseconds.
- Do not sort the original samples slice — copy first.

## What's Next

Next: [CPU Pinning and NUMA](../09-cpu-pinning-numa/09-cpu-pinning-numa.md).

## Resources

- [time package — Duration](https://pkg.go.dev/time#Duration)
- [sort package — Slice](https://pkg.go.dev/sort#Slice)
- [runtime package — GOMAXPROCS](https://pkg.go.dev/runtime#GOMAXPROCS)
- [Go Execution Tracer](https://pkg.go.dev/runtime/trace)
- [Scheduling in Go Part II: Go Scheduler (Ardan Labs)](https://www.ardanlabs.com/blog/2018/08/scheduling-in-go-part2.html)
