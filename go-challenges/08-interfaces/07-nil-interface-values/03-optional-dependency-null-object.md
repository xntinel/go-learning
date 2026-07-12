# Exercise 3: Optional telemetry dependency with a no-op default

Metrics recording is optional: production wires a real recorder, tests and edge
deployments pass nil. This module builds a `Service` that takes a
`MetricsRecorder` and normalizes a nil to a `nopRecorder` at the boundary, so the
hot path calls `s.metrics.IncCounter` unconditionally with zero nil checks and
zero allocation on the no-op path.

## What you'll build

```text
optmetrics/                independent module: example.com/optmetrics
  go.mod                   go 1.26
  service.go               MetricsRecorder; nopRecorder; Service; NewService; Handle
  cmd/
    demo/
      main.go              real recorder vs nil (nop) side by side
  service_test.go          no-panic on nil; spy records on real path; alloc benchmark
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: a `MetricsRecorder` interface (`IncCounter`/`ObserveLatency`), a `nopRecorder`, and `NewService(m MetricsRecorder)` that maps nil to `nopRecorder{}`.
- Test: `NewService(nil).Handle(ctx)` runs without panic; a spy recorder proves the real path increments; a benchmark reports allocations on the no-op path.
- Verify: `go test -count=1 -race ./...`

### Why a no-op default beats a nil check

`Service.Handle` is a hot path — it runs on every request. If metrics were
optional via a nil `MetricsRecorder`, every metric call would need an
`if s.metrics != nil` guard, and one forgotten guard is a panic. The Null Object
removes the branch: `NewService` substitutes `nopRecorder{}` for nil once, and
`Handle` calls `s.metrics.IncCounter("requests")` and
`s.metrics.ObserveLatency(...)` unconditionally.

The no-op is not just safe, it is free. `nopRecorder`'s methods have empty
bodies; the compiler inlines them to nothing, and because they take a `string`
and a `time.Duration` by value and do nothing with them, the no-op path
allocates zero times. That is what makes it acceptable to leave the metric calls
on the hot path in a build that has no recorder wired: there is no cost to pay.
The benchmark in the test file pins this with `b.ReportAllocs()`.

Create `service.go`:

```go
package optmetrics

import (
	"context"
	"time"
)

// MetricsRecorder is the optional telemetry contract. Production supplies a real
// recorder; tests and edge deployments can leave it nil.
type MetricsRecorder interface {
	IncCounter(name string)
	ObserveLatency(name string, d time.Duration)
}

// nopRecorder is the Null Object: it satisfies MetricsRecorder with no-op
// methods that allocate nothing.
type nopRecorder struct{}

func (nopRecorder) IncCounter(name string) {}

func (nopRecorder) ObserveLatency(name string, d time.Duration) {}

// Service records telemetry on its hot path without ever branching on nil.
type Service struct {
	metrics MetricsRecorder
}

// NewService normalizes a nil recorder to nopRecorder{} at the boundary.
func NewService(m MetricsRecorder) *Service {
	if m == nil {
		m = nopRecorder{}
	}
	return &Service{metrics: m}
}

// Handle simulates request handling: it counts the request and observes how long
// the work took, calling the recorder unconditionally.
func (s *Service) Handle(ctx context.Context) error {
	s.metrics.IncCounter("requests")
	start := time.Now()
	if err := ctx.Err(); err != nil {
		s.metrics.IncCounter("cancelled")
		return err
	}
	s.metrics.ObserveLatency("handle", time.Since(start))
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"example.com/optmetrics"
)

// counting is a tiny real recorder used only for the demo.
type counting struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *counting) IncCounter(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[name]++
}

func (c *counting) ObserveLatency(name string, d time.Duration) {}

func main() {
	ctx := context.Background()

	rec := &counting{counts: map[string]int{}}
	realSvc := optmetrics.NewService(rec)
	_ = realSvc.Handle(ctx)
	_ = realSvc.Handle(ctx)
	fmt.Println("real recorder requests:", rec.counts["requests"])

	// nil recorder: normalized to the no-op, runs clean and records nothing.
	quiet := optmetrics.NewService(nil)
	if err := quiet.Handle(ctx); err != nil {
		fmt.Println("unexpected error:", err)
	}
	fmt.Println("nil recorder handled without panic")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
real recorder requests: 2
nil recorder handled without panic
```

### Tests

Create `service_test.go`:

```go
package optmetrics

import (
	"context"
	"sync"
	"testing"
	"time"
)

// spyRecorder records what the service reported, to prove the real path works.
type spyRecorder struct {
	mu        sync.Mutex
	counters  map[string]int
	latencies int
}

func newSpy() *spyRecorder {
	return &spyRecorder{counters: map[string]int{}}
}

func (s *spyRecorder) IncCounter(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters[name]++
}

func (s *spyRecorder) ObserveLatency(name string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latencies++
}

func TestNilRecorderDoesNotPanic(t *testing.T) {
	t.Parallel()

	s := NewService(nil)
	if err := s.Handle(context.Background()); err != nil {
		t.Fatalf("Handle returned %v; want nil", err)
	}
}

func TestRealRecorderReceivesMetrics(t *testing.T) {
	t.Parallel()

	spy := newSpy()
	s := NewService(spy)
	if err := s.Handle(context.Background()); err != nil {
		t.Fatalf("Handle returned %v; want nil", err)
	}

	spy.mu.Lock()
	defer spy.mu.Unlock()
	if spy.counters["requests"] != 1 {
		t.Fatalf("requests counter = %d; want 1", spy.counters["requests"])
	}
	if spy.latencies != 1 {
		t.Fatalf("latency observations = %d; want 1", spy.latencies)
	}
}

func TestCancelledContextIsCounted(t *testing.T) {
	t.Parallel()

	spy := newSpy()
	s := NewService(spy)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Handle(ctx); err == nil {
		t.Fatal("expected a context error")
	}

	spy.mu.Lock()
	defer spy.mu.Unlock()
	if spy.counters["cancelled"] != 1 {
		t.Fatalf("cancelled counter = %d; want 1", spy.counters["cancelled"])
	}
}

// BenchmarkNopHandle documents that the no-op path allocates nothing. Run it
// with: go test -bench=NopHandle -benchmem
func BenchmarkNopHandle(b *testing.B) {
	s := NewService(nil)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = s.Handle(ctx)
	}
}
```

## Review

The pattern is correct when `Handle` never branches on nil. `NewService`
substitutes `nopRecorder{}` for a nil interface, so the hot path calls the
recorder unconditionally — `TestNilRecorderDoesNotPanic` proves the no-op path
is safe, and `TestRealRecorderReceivesMetrics` proves a wired recorder actually
sees the increments and observations. The no-op path is free: `nopRecorder`'s
empty methods inline away and allocate nothing, which `BenchmarkNopHandle` with
`b.ReportAllocs()` demonstrates. The mistake to avoid is using a nil recorder as
the "no telemetry" sentinel and guarding every call site; one missed guard is a
panic on the very path metrics were supposed to observe.

## Resources

- [Effective Go: Interfaces and methods](https://go.dev/doc/effective_go#interfaces) — the method-set contract a Null Object satisfies.
- [`testing.B.ReportAllocs`](https://pkg.go.dev/testing#B.ReportAllocs) — reporting per-op allocations in a benchmark.
- [`context` package](https://pkg.go.dev/context) — `context.Context` and cancellation.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-reflect-nil-guard-di-container.md](04-reflect-nil-guard-di-container.md)
