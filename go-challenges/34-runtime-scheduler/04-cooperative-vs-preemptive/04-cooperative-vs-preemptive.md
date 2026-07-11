# 4. Cooperative vs Preemptive Scheduling

Before Go 1.14 the scheduler was purely cooperative: goroutines yielded only at function calls, channel operations, and system calls. A tight arithmetic loop with no calls could starve other goroutines on the same P. Go 1.14 introduced asynchronous preemption via SIGURG, making the scheduler safe against CPU hogs. This lesson builds a package that measures scheduling gaps — the time a goroutine waits after becoming runnable — as a concrete observable.

## Concepts

### Cooperative Scheduling History

In the original Go scheduler, a goroutine yielded only at explicit yield points: function calls (the compiler inserts preemption checks at function prologues), channel operations, memory allocation, `go` statements, `select`, and `runtime.Gosched()`. Code with a tight arithmetic loop and no function calls could run forever on its P without ever yielding.

### Async Preemption in Go 1.14

Go 1.14 introduced asynchronous preemption. The runtime sends SIGURG to OS threads that have been running the same goroutine for too long (roughly 10ms). The signal handler sets a preemption flag; the goroutine suspends at the next safe point (a point where the GC can inspect the stack). This makes all goroutines preemptible regardless of whether they call functions.

`GODEBUG=asyncpreemptoff=1` disables async preemption. Under this setting, a tight loop can still starve other goroutines on the same P. This setting exists only for diagnosing bugs, never for production.

### Scheduling Latency

Scheduling latency is the gap between when a goroutine becomes runnable (created or unblocked from a channel) and when it first executes. In a lightly loaded system this is nanoseconds. Under contention it can reach hundreds of microseconds. The `Probe` function in this package measures this gap by recording `time.Now()` just before the `go` statement and reading `time.Since(born)` as the first instruction in the goroutine body.

### P99 and MaxGap

Latency distributions are long-tailed. The mean is misleading. P99 (99th percentile) captures the worst-case experience for most goroutines. MaxGap is the absolute worst case. Monitoring both during load tests exposes tail latency that the mean hides.

## Exercises

### Exercise 1: Measure Scheduling Gap Distribution

Call `Probe(200)` and print the max and P99 gap. Then run the same call with `GOMAXPROCS=1` and observe whether the gaps increase.

### Exercise 2: Known-Data P99

Construct a slice of 100 `Sample` values: 99 with Wait=1ms and 1 with Wait=100ms. Compute `P99Gap` and verify it returns 100ms. Check the index calculation: `(100*99)/100 = 99`.

### Exercise 3: MaxGap vs P99 Relationship

Generate a Probe of 1000 goroutines. Assert that `P99Gap <= MaxGap`. Run it multiple times and observe whether MaxGap varies more than P99Gap across runs.

## Common Mistakes

Wrong: Measuring `time.Since(born)` after a channel receive that the goroutine waits on. What happens: the measured gap includes actual blocking time, not just scheduling delay. Fix: Record `born` immediately before the `go` statement and read `time.Since` as the first instruction in the goroutine body.

Wrong: Disabling async preemption (`asyncpreemptoff=1`) in production to avoid SIGURG overhead. What happens: any tight loop blocks all other goroutines on the same P indefinitely. Fix: Never disable async preemption in production; the overhead is sub-microsecond per preemption event.

Wrong: Using `GOMAXPROCS=1` and expecting goroutines to interleave without yield points under Go <1.14. What happens: the first goroutine to run a tight loop starves all others. Fix: Either add explicit `runtime.Gosched()` calls or upgrade to Go 1.14+ where SIGURG handles this.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

Go scheduling moved from purely cooperative (pre-1.14) to hybrid cooperative/preemptive (1.14+). Async preemption via SIGURG eliminates goroutine starvation from tight loops. Scheduling latency — the gap between goroutine creation and first execution — is the key metric for latency-sensitive services. Tail percentiles (P99, max) expose problems the mean hides.

## What's Next

[05. runtime.Gosched](../05-runtime-gosched/05-runtime-gosched.md)

## Resources

- https://go.dev/doc/go1.14#runtime
- https://go.dev/design/24543-non-cooperative-preemption
- https://pkg.go.dev/runtime
- https://www.ardanlabs.com/blog/2018/12/scheduling-in-go-part3.html

---

Create `go.mod`

```go
// go.mod
module example.com/preempt

go 1.26
```

Create `preempt.go`

```go
package preempt

import (
	"runtime"
	"sort"
	"sync"
	"time"
)

// Sample records one scheduling gap: the duration a goroutine
// waited between being created (or unblocked) and actually running.
type Sample struct {
	Wait time.Duration
}

// Probe launches n goroutines. Each records the gap between when the
// go statement fires and when the goroutine body starts executing.
// It waits for all goroutines to complete before returning.
func Probe(n int) []Sample {
	if n <= 0 {
		return nil
	}
	samples := make([]Sample, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		born := time.Now()
		idx := i
		go func() {
			defer wg.Done()
			samples[idx] = Sample{Wait: time.Since(born)}
		}()
	}
	wg.Wait()
	return samples
}

// MaxGap returns the largest Wait in samples, or 0 if empty.
func MaxGap(samples []Sample) time.Duration {
	var max time.Duration
	for _, s := range samples {
		if s.Wait > max {
			max = s.Wait
		}
	}
	return max
}

// P99Gap returns the 99th-percentile Wait, or 0 if samples is empty.
func P99Gap(samples []Sample) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(samples))
	for i, s := range samples {
		sorted[i] = s.Wait
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted) * 99) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// Gosched calls runtime.Gosched, yielding the processor to other goroutines.
// Exported so callers can invoke it without importing "runtime" directly.
func Gosched() {
	runtime.Gosched()
}
```

Create `preempt_test.go`

```go
package preempt

import (
	"testing"
	"time"
)

func TestProbeReturnsNSamples(t *testing.T) {
	t.Parallel()
	got := Probe(50)
	if len(got) != 50 {
		t.Errorf("Probe(50) returned %d samples, want 50", len(got))
	}
}

func TestProbeReturnsEmptyForZero(t *testing.T) {
	t.Parallel()
	got := Probe(0)
	if len(got) != 0 {
		t.Errorf("Probe(0) returned %d samples, want 0", len(got))
	}
}

func TestMaxGapNonNegative(t *testing.T) {
	t.Parallel()
	samples := Probe(20)
	if g := MaxGap(samples); g < 0 {
		t.Errorf("MaxGap < 0: %v", g)
	}
}

func TestP99LeMaxGap(t *testing.T) {
	t.Parallel()
	samples := Probe(100)
	p99 := P99Gap(samples)
	max := MaxGap(samples)
	if p99 > max {
		t.Errorf("P99Gap(%v) > MaxGap(%v)", p99, max)
	}
}

func TestMaxGapOnKnownData(t *testing.T) {
	t.Parallel()
	samples := []Sample{
		{Wait: 10 * time.Millisecond},
		{Wait: 5 * time.Millisecond},
		{Wait: 20 * time.Millisecond},
	}
	got := MaxGap(samples)
	want := 20 * time.Millisecond
	if got != want {
		t.Errorf("MaxGap = %v, want %v", got, want)
	}
}

func TestP99OnKnownData(t *testing.T) {
	t.Parallel()
	// 100 samples: 99 at 1ms, 1 at 100ms
	samples := make([]Sample, 100)
	for i := range samples {
		samples[i] = Sample{Wait: time.Millisecond}
	}
	samples[99] = Sample{Wait: 100 * time.Millisecond}
	got := P99Gap(samples)
	// P99 index = (100*99)/100 = 99 -> the 100ms sample
	if got != 100*time.Millisecond {
		t.Errorf("P99Gap = %v, want 100ms", got)
	}
}

func TestMaxGapEmptyReturnsZero(t *testing.T) {
	t.Parallel()
	if g := MaxGap(nil); g != 0 {
		t.Errorf("MaxGap(nil) = %v, want 0", g)
	}
}

func TestP99EmptyReturnsZero(t *testing.T) {
	t.Parallel()
	if p := P99Gap(nil); p != 0 {
		t.Errorf("P99Gap(nil) = %v, want 0", p)
	}
}
```

Create `example_test.go`

```go
package preempt_test

import (
	"fmt"
	"time"

	"example.com/preempt"
)

func ExampleMaxGap() {
	samples := []preempt.Sample{
		{Wait: 1 * time.Millisecond},
		{Wait: 5 * time.Millisecond},
		{Wait: 2 * time.Millisecond},
	}
	fmt.Println(preempt.MaxGap(samples))
	// Output:
	// 5ms
}
```

Create `cmd/demo/main.go`

```go
package main

import (
	"fmt"
	"runtime"

	"example.com/preempt"
)

func main() {
	fmt.Printf("GOMAXPROCS=%d\n", runtime.GOMAXPROCS(0))

	samples := preempt.Probe(200)
	fmt.Printf("Probed %d goroutines\n", len(samples))
	fmt.Printf("Max scheduling gap: %v\n", preempt.MaxGap(samples))
	fmt.Printf("P99 scheduling gap: %v\n", preempt.P99Gap(samples))
}
```
