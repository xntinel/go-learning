# Exercise 28: Operation Latency and Outcome Percentile Recording

A latency histogram is only as trustworthy as its worst discipline problem:
one code path that measures and records, and every other path that forgets.
This exercise builds an `Instrument` wrapper that times an operation with an
injected clock and, via a deferred closure that reads the named `err`
result, records exactly one observation — latency plus outcome — no matter
which of the wrapped function's return statements produced the result.

**Nivel: Avanzado** — validacion normal (tabla de percentiles + prueba concurrente con `-race`).

## What you'll build

```text
metric/                     independent module: example.com/metric
  go.mod
  metric.go                 Clock; Histogram (Observe/Percentile); Instrument (deferred record)
  cmd/demo/
    main.go                 runnable demo: a fake clock, a success and an error call
  metric_test.go             success/error outcome recording, percentile table, concurrent Instrument calls
```

- Files: `metric.go`, `cmd/demo/main.go`, `metric_test.go`.
- Implement: `Instrument(clock Clock, h *Histogram, fn func() error) (err error)` whose deferred closure takes a second clock reading and calls `h.Observe` with the elapsed duration and an outcome derived from `err`.
- Test: a table of exact durations checked against `Percentile` at 0/50/100; success and error outcomes recorded correctly; concurrent `Instrument` calls under `-race` land in the histogram without corruption.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/28-operation-duration-percentile-metric/cmd/demo
cd go-solutions/04-functions/02-named-return-values/28-operation-duration-percentile-metric
go mod edit -go=1.24
```

### One measurement, one record, keyed on the named error

```go
func Instrument(clock Clock, h *Histogram, fn func() error) (err error) {
	start := clock()
	defer func() {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		h.Observe(clock().Sub(start), outcome)
	}()

	err = fn()
	return
}
```

`Instrument` takes its first clock reading before `fn` runs, then registers
a deferred closure that takes the second one. Because `err` is a named
result, that closure runs after `fn`'s return value has been copied into
`err`, so it can look at the final outcome and record it alongside the
elapsed duration — one `Observe` call, always, regardless of whether `fn`
succeeded, failed, or (were this production code) panicked. `clock` is a
`func() time.Time` rather than a hard-coded `time.Now`: production wires
`time.Now`, tests wire a fake that advances by a fixed step on every call, so
the recorded durations are exact numbers to assert on instead of "probably
around a few milliseconds."

Create `metric.go`:

```go
package metric

import (
	"sort"
	"sync"
	"time"
)

// Clock returns the current time. Production code passes time.Now; tests
// pass a fake that advances on a fixed schedule, so latency assertions never
// depend on wall-clock timing.
type Clock func() time.Time

// Observation is one recorded operation outcome.
type Observation struct {
	Duration time.Duration
	Outcome  string
}

// Histogram collects Observations under a mutex so concurrent Instrument
// calls can record safely.
type Histogram struct {
	mu  sync.Mutex
	obs []Observation
}

// Observe records one sample.
func (h *Histogram) Observe(d time.Duration, outcome string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.obs = append(h.obs, Observation{Duration: d, Outcome: outcome})
}

// Count returns how many samples have been recorded.
func (h *Histogram) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.obs)
}

// CountByOutcome returns how many recorded samples carry the given outcome.
func (h *Histogram) CountByOutcome(outcome string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, o := range h.obs {
		if o.Outcome == outcome {
			n++
		}
	}
	return n
}

// Percentile returns the duration at percentile p (0..100) using
// nearest-rank interpolation over the durations recorded so far. It panics if
// no samples have been recorded.
func (h *Histogram) Percentile(p float64) time.Duration {
	h.mu.Lock()
	durations := make([]time.Duration, len(h.obs))
	for i, o := range h.obs {
		durations[i] = o.Duration
	}
	h.mu.Unlock()

	if len(durations) == 0 {
		panic("metric: Percentile called on an empty histogram")
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	rank := int(p/100*float64(len(durations)-1) + 0.5)
	if rank < 0 {
		rank = 0
	}
	if rank >= len(durations) {
		rank = len(durations) - 1
	}
	return durations[rank]
}

// Instrument runs fn, timing it with clock, and records the elapsed duration
// plus outcome ("success" or "error") into h.
//
// err is a named result: the deferred closure runs after fn's return value
// has been copied into err, so it can read the final outcome, take a second
// clock reading, and emit exactly one Observe call regardless of which of
// fn's return statements produced err — including a future one nobody has
// written yet.
func Instrument(clock Clock, h *Histogram, fn func() error) (err error) {
	start := clock()
	defer func() {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		h.Observe(clock().Sub(start), outcome)
	}()

	err = fn()
	return
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/metric"
)

func main() {
	h := &metric.Histogram{}

	// A fake clock that advances by a fixed step on every call, so the
	// recorded durations are exact and reproducible.
	var now time.Time
	step := 10 * time.Millisecond
	clock := func() time.Time {
		t := now
		now = now.Add(step)
		return t
	}

	_ = metric.Instrument(clock, h, func() error { return nil })
	err := metric.Instrument(clock, h, func() error { return errors.New("boom") })

	fmt.Println("last error:", err)
	fmt.Println("count:", h.Count())
	fmt.Println("success count:", h.CountByOutcome("success"))
	fmt.Println("error count:", h.CountByOutcome("error"))
	fmt.Println("p100 duration:", h.Percentile(100))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
last error: boom
count: 2
success count: 1
error count: 1
p100 duration: 10ms
```

### Tests

Create `metric_test.go`:

```go
package metric

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func fakeClock(step time.Duration) Clock {
	var mu sync.Mutex
	now := time.Time{}
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := now
		now = now.Add(step)
		return t
	}
}

func TestInstrumentRecordsSuccessDuration(t *testing.T) {
	t.Parallel()

	h := &Histogram{}
	clock := fakeClock(5 * time.Millisecond)

	err := Instrument(clock, h, func() error { return nil })
	if err != nil {
		t.Fatalf("Instrument: unexpected error: %v", err)
	}
	if h.Count() != 1 {
		t.Fatalf("Count() = %d, want 1", h.Count())
	}
	if h.CountByOutcome("success") != 1 {
		t.Fatalf("CountByOutcome(success) = %d, want 1", h.CountByOutcome("success"))
	}
	if got := h.Percentile(100); got != 5*time.Millisecond {
		t.Fatalf("Percentile(100) = %v, want 5ms", got)
	}
}

func TestInstrumentRecordsErrorOutcome(t *testing.T) {
	t.Parallel()

	h := &Histogram{}
	clock := fakeClock(5 * time.Millisecond)
	wantErr := errors.New("boom")

	err := Instrument(clock, h, func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("Instrument err = %v, want %v", err, wantErr)
	}
	if h.CountByOutcome("error") != 1 {
		t.Fatalf("CountByOutcome(error) = %d, want 1", h.CountByOutcome("error"))
	}
	if h.CountByOutcome("success") != 0 {
		t.Fatalf("CountByOutcome(success) = %d, want 0", h.CountByOutcome("success"))
	}
}

func TestPercentileNearestRank(t *testing.T) {
	t.Parallel()

	h := &Histogram{}
	// Feed exact, known durations directly so the percentile math is a
	// table of concrete cases rather than derived from timing.
	for _, d := range []time.Duration{10, 20, 30, 40, 50} {
		h.Observe(d*time.Millisecond, "success")
	}

	cases := []struct {
		p    float64
		want time.Duration
	}{
		{0, 10 * time.Millisecond},
		{50, 30 * time.Millisecond},
		{100, 50 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := h.Percentile(tc.p); got != tc.want {
			t.Errorf("Percentile(%v) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestInstrumentConcurrentRecordingIsRaceFree(t *testing.T) {
	t.Parallel()

	h := &Histogram{}
	clock := fakeClock(1 * time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Instrument(clock, h, func() error { return nil })
		}()
	}
	wg.Wait()

	if h.Count() != 50 {
		t.Fatalf("Count() = %d, want 50", h.Count())
	}
}
```

## Review

`Instrument` is correct when every call — success or error — produces
exactly one `Observe`, and the outcome label matches the named `err` the
deferred closure saw. The percentile table pins down the nearest-rank math
against exact, hand-picked durations rather than timing-derived ones, and the
concurrent test proves the histogram's mutex makes concurrent `Instrument`
calls safe under `-race`. The mistake to avoid is calling `h.Observe` from
inside `fn` itself, at the call site, instead of from the deferred closure —
that means every call site must remember to record both outcomes, and the
one that only handles the success path silently produces a histogram with no
error observations at all.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [`time.Time` and durations](https://pkg.go.dev/time#Time)
- [Prometheus: Histograms and summaries](https://prometheus.io/docs/practices/histograms/)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-topic-subscription-unsubscribe-on-handler-error.md](27-topic-subscription-unsubscribe-on-handler-error.md) | Next: [29-request-trace-context-propagation.md](29-request-trace-context-propagation.md)
