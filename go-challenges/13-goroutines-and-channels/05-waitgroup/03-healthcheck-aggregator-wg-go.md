# Exercise 3: Multi-Dependency Health Endpoint Using the Go 1.25 wg.Go Method

A readiness endpoint probes every dependency a service needs — database, cache, a
downstream API — and reports an aggregate status plus per-dependency detail. The
probes are independent, so they should run concurrently and the handler should return
in roughly the time of the *slowest* probe, not the sum. This module builds that
aggregator with Go 1.25's `wg.Go`, and writes each probe result into its own slice
slot so no mutex is needed.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
readiness/                 independent module: example.com/readiness
  go.mod                   go 1.25 (wg.Go needs it)
  readiness.go             Dependency, Check, Report; Aggregate probes concurrently
  cmd/
    demo/
      main.go              runnable demo: 3 deps, one failing, prints report
  readiness_test.go        degraded-on-one-failure; concurrency (wall-clock) proof
```

- Files: `readiness.go`, `cmd/demo/main.go`, `readiness_test.go`.
- Implement: `Aggregate(ctx, deps []Dependency, timeout time.Duration) Report` — probe every dependency concurrently with `wg.Go`, each writing `checks[i]`, overall status `degraded` if any probe fails.
- Test: one failing probe marks the report degraded while others still report OK; total wall-clock is ~max(probe), not sum (concurrency proof); race-clean without a mutex.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/05-waitgroup/03-healthcheck-aggregator-wg-go/cmd/demo
cd go-solutions/13-goroutines-and-channels/05-waitgroup/03-healthcheck-aggregator-wg-go
go mod edit -go=1.25
```

### wg.Go removes the Add/Done boilerplate

Go 1.25 added `wg.Go(f func())`. It performs `Add(1)`, runs `f` in a new goroutine,
and calls `Done` when `f` returns — the three-line dance collapses into one call, and
the Add-placement footgun disappears entirely because there is no separate `Add` to
misplace. For fire-and-join goroutines this is the modern spelling.

Each probe writes its result into a pre-indexed slot: `checks[i]` for probe `i`. This
is the disjoint-index idiom — every goroutine touches a distinct memory location, so
there is no data race and no mutex, and the `wg.Wait()` (inside the last `wg.Go`'s
completion, gathered by the outer `Wait`) publishes all the writes. We size the
`checks` slice up front to `len(deps)` so slot `i` exists before any goroutine runs.

The aggregate status is computed *after* the join, by scanning the completed slice.
One failed probe makes the whole report `degraded`; the individual `Check` entries
still carry each dependency's own status and latency, so an operator can see exactly
which dependency is unhealthy.

The context carries a deadline: `Aggregate` derives a timeout so a single hung
dependency cannot stall readiness forever. Each probe receives that context and is
expected to honor it; a probe that ignores cancellation is a bug in the probe, not
the aggregator.

Create `readiness.go`:

```go
package readiness

import (
	"context"
	"sync"
	"time"
)

// Status is the health of a dependency or the overall report.
type Status string

const (
	StatusOK       Status = "ok"
	StatusDegraded Status = "degraded"
)

// Dependency is a named health probe. Probe returns nil when healthy.
type Dependency struct {
	Name  string
	Probe func(ctx context.Context) error
}

// Check is one dependency's probe outcome.
type Check struct {
	Name    string
	Status  Status
	Latency time.Duration
	Err     error
}

// Report is the aggregate readiness result.
type Report struct {
	Status Status
	Checks []Check
}

// Aggregate probes every dependency concurrently and returns a combined report.
// The overall status is degraded if any single probe fails. Each probe is given
// a context derived from ctx with the supplied timeout.
func Aggregate(ctx context.Context, deps []Dependency, timeout time.Duration) Report {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	checks := make([]Check, len(deps))
	var wg sync.WaitGroup
	for i, d := range deps {
		wg.Go(func() {
			start := time.Now()
			err := d.Probe(ctx)
			status := StatusOK
			if err != nil {
				status = StatusDegraded
			}
			checks[i] = Check{
				Name:    d.Name,
				Status:  status,
				Latency: time.Since(start),
				Err:     err,
			}
		})
	}
	wg.Wait()

	overall := StatusOK
	for _, c := range checks {
		if c.Status != StatusOK {
			overall = StatusDegraded
			break
		}
	}
	return Report{Status: overall, Checks: checks}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/readiness"
)

func main() {
	deps := []readiness.Dependency{
		{Name: "postgres", Probe: func(ctx context.Context) error { return nil }},
		{Name: "redis", Probe: func(ctx context.Context) error {
			return errors.New("dial tcp: connection refused")
		}},
		{Name: "billing-api", Probe: func(ctx context.Context) error { return nil }},
	}

	report := readiness.Aggregate(context.Background(), deps, time.Second)
	fmt.Printf("overall: %s\n", report.Status)
	for _, c := range report.Checks {
		if c.Err != nil {
			fmt.Printf("  %s: %s (%v)\n", c.Name, c.Status, c.Err)
			continue
		}
		fmt.Printf("  %s: %s\n", c.Name, c.Status)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
overall: degraded
  postgres: ok
  redis: degraded (dial tcp: connection refused)
  billing-api: ok
```

### Tests

`TestAggregateDegradedOnOneFailure` injects two healthy probes and one failing probe
and asserts the overall status is degraded while the two healthy checks still report
OK and carry the right per-dependency error. `TestAggregateRunsConcurrently` gives
every probe a fixed latency and asserts the total wall-clock is far below the sum —
the concurrency proof — using a generous bound so the test is not flaky on a loaded
machine. The absence of a mutex combined with `-race` proves each slot is written by
exactly one goroutine.

Create `readiness_test.go`:

```go
package readiness

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

var errProbe = errors.New("probe failed")

func TestAggregateDegradedOnOneFailure(t *testing.T) {
	t.Parallel()

	deps := []Dependency{
		{Name: "db", Probe: func(ctx context.Context) error { return nil }},
		{Name: "cache", Probe: func(ctx context.Context) error { return errProbe }},
		{Name: "api", Probe: func(ctx context.Context) error { return nil }},
	}

	report := Aggregate(context.Background(), deps, time.Second)
	if report.Status != StatusDegraded {
		t.Fatalf("overall = %s, want %s", report.Status, StatusDegraded)
	}

	byName := map[string]Check{}
	for _, c := range report.Checks {
		byName[c.Name] = c
	}
	if byName["db"].Status != StatusOK || byName["api"].Status != StatusOK {
		t.Fatalf("healthy deps not OK: db=%s api=%s", byName["db"].Status, byName["api"].Status)
	}
	if !errors.Is(byName["cache"].Err, errProbe) {
		t.Fatalf("cache err = %v, want errProbe", byName["cache"].Err)
	}
}

func TestAggregateAllHealthy(t *testing.T) {
	t.Parallel()

	deps := []Dependency{
		{Name: "a", Probe: func(ctx context.Context) error { return nil }},
		{Name: "b", Probe: func(ctx context.Context) error { return nil }},
	}
	report := Aggregate(context.Background(), deps, time.Second)
	if report.Status != StatusOK {
		t.Fatalf("overall = %s, want %s", report.Status, StatusOK)
	}
}

func TestAggregateRunsConcurrently(t *testing.T) {
	t.Parallel()

	const (
		probes  = 8
		latency = 40 * time.Millisecond
	)
	deps := make([]Dependency, 0, probes)
	for i := range probes {
		deps = append(deps, Dependency{
			Name: fmt.Sprintf("dep-%d", i),
			Probe: func(ctx context.Context) error {
				select {
				case <-time.After(latency):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		})
	}

	start := time.Now()
	report := Aggregate(context.Background(), deps, time.Second)
	elapsed := time.Since(start)

	if report.Status != StatusOK {
		t.Fatalf("overall = %s, want %s", report.Status, StatusOK)
	}
	// Sequential would be probes*latency = 320ms; concurrent is ~latency.
	// A generous bound (half the sequential time) proves concurrency without flaking.
	if elapsed >= probes*latency/2 {
		t.Fatalf("elapsed = %v, want well below sequential %v", elapsed, probes*latency)
	}
}

func ExampleAggregate() {
	deps := []Dependency{
		{Name: "svc", Probe: func(ctx context.Context) error { return nil }},
	}
	report := Aggregate(context.Background(), deps, time.Second)
	fmt.Println(report.Status, report.Checks[0].Name)
	// Output: ok svc
}
```

## Review

The aggregator is correct when the overall status is degraded exactly when at least
one probe fails, each `Check` carries its own dependency's status and error, and the
whole thing is race-clean under `-race` despite having no mutex — because every
goroutine writes a distinct `checks[i]`. The concurrency proof is the wall-clock
test: eight 40 ms probes finish in about 40 ms, not 320 ms.

The point of this module is the `wg.Go` spelling. It is not just shorter; it removes
the failure mode where an `Add` drifts into the wrong goroutine. Remember its one
limit: `wg.Go` does not recover panics, so a probe that panics still crashes the
process — the panic-isolation pattern for that lives in Exercise 10. Keep the slice
pre-sized to `len(deps)` so slot `i` exists before the goroutines run.

## Resources

- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) — the Go 1.25 method that does Add/run/Done.
- [Go 1.25 Release Notes](https://go.dev/doc/go1.25) — the `WaitGroup.Go` addition.
- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) — bounding probe latency.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-bounded-fanout-runner.md](02-bounded-fanout-runner.md) | Next: [04-scatter-gather-indexed-slice.md](04-scatter-gather-indexed-slice.md)
