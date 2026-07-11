# Exercise 17: Aggregating Health Checks from Multiple Endpoints

**Nivel: Intermedio** — validacion rapida (un test corto).

A `/healthz` endpoint that fans out to several dependencies has to answer two
different questions depending on who's asking: an operator debugging an
incident wants to see the status of *every* dependency at once, while a
readiness gate deciding whether to route traffic to a fresh instance just
wants the fastest possible "no" the moment anything is broken. This module
builds both shapes from the same check list — a full aggregation with
early-continue guards, and a fail-fast variant with early `break` — so the
difference between `continue` and `break` in a `for range` loop is the whole
lesson.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
health/                        module example.com/health
  go.mod                       go 1.24
  health.go                    Check; Result; Report; Aggregate; AggregateFailFast
  health_test.go                 all healthy, one failing, nil Fn, fail-fast short-circuit
  cmd/demo/
    main.go                     runs both aggregators against the same three checks
```

- Files: `health.go`, `health_test.go`, `cmd/demo/main.go`.
- Implement: `Aggregate(checks []Check) Report` — `for range` with early-`continue` guards that records every check and never stops early; `AggregateFailFast(checks []Check) Report` — the same loop with `break` in place of `continue`, stopping at the first failure.
- Test: all healthy; one endpoint failing (confirm every check still ran); a `nil` `Fn` counts as a failure; an empty check list is healthy; fail-fast stops after the first failure and never calls the checks after it.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/health/cmd/demo
cd ~/go-exercises/health
go mod init example.com/health
go mod edit -go=1.24
```

### Why `continue` and `break` are the entire difference

Both functions run the exact same body — guard against a missing `Fn`, run
the check, record the result, flip `Healthy` to false on any failure — so the
only thing that distinguishes "give me the full picture" from "give me the
fastest no" is whether a failing check's loop iteration ends with `continue`
(move to the next check) or `break` (stop looking at anything else). That is
a deliberate demonstration that these two keywords are not interchangeable
"skip this iteration" synonyms: `continue` preserves the AND-aggregation
property (every endpoint gets probed, so the report is exhaustive) while
`break` trades completeness for latency (the moment the answer is already
"unhealthy," there is no value in probing the rest). Using a slice of named
`Check` values instead of a `map[string]func() error` is also deliberate —
map iteration order is randomized in Go, and a health report whose check
order changes on every call is a report a human cannot diff between two
incidents.

Create `health.go`:

```go
package health

import "errors"

// errNilCheck is recorded as a failure when a Check has no Fn set.
var errNilCheck = errors.New("health: check has no Fn")

// Check names a single health probe.
type Check struct {
	Name string
	Fn   func() error
}

// Result is the outcome of one check.
type Result struct {
	Name string
	Err  error
}

// Report is the outcome of aggregating every check.
type Report struct {
	Healthy bool
	Results []Result
}

// Aggregate runs every check and returns the full picture: it never stops
// early, so an operator always sees every endpoint's status in one report.
// Overall health is AND logic -- Healthy is true only if every check passed.
// Each iteration is an early-continue guard: a nil Fn or a failing check is
// recorded and the loop moves straight to the next check, keeping the happy
// path (record success, keep going) unindented.
func Aggregate(checks []Check) Report {
	report := Report{Healthy: true}
	for _, c := range checks {
		if c.Fn == nil {
			report.Results = append(report.Results, Result{Name: c.Name, Err: errNilCheck})
			report.Healthy = false
			continue
		}
		err := c.Fn()
		report.Results = append(report.Results, Result{Name: c.Name, Err: err})
		if err != nil {
			report.Healthy = false
			continue
		}
	}
	return report
}

// AggregateFailFast is Aggregate's short-circuiting twin: it stops probing
// the instant one check fails, instead of running every endpoint regardless.
// This is the right shape for a startup readiness gate, where the caller only
// needs to know "is everything up yet", not a full report of every failure.
func AggregateFailFast(checks []Check) Report {
	report := Report{Healthy: true}
	for _, c := range checks {
		if c.Fn == nil {
			report.Results = append(report.Results, Result{Name: c.Name, Err: errNilCheck})
			report.Healthy = false
			break
		}
		err := c.Fn()
		report.Results = append(report.Results, Result{Name: c.Name, Err: err})
		if err != nil {
			report.Healthy = false
			break
		}
	}
	return report
}
```

### The runnable demo

The demo runs the same three checks — a healthy database, a failing cache,
and a healthy queue — through both aggregators, so the output makes the
`continue`-versus-`break` difference visible: `Aggregate` reports all three,
`AggregateFailFast` stops after the cache fails and never touches the queue.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/health"
)

func main() {
	checks := []health.Check{
		{Name: "database", Fn: func() error { return nil }},
		{Name: "cache", Fn: func() error { return errors.New("connection refused") }},
		{Name: "queue", Fn: func() error { return nil }},
	}

	full := health.Aggregate(checks)
	fmt.Printf("Aggregate: healthy=%v results=%d\n", full.Healthy, len(full.Results))
	for _, r := range full.Results {
		fmt.Printf("  %-10s err=%v\n", r.Name, r.Err)
	}

	fast := health.AggregateFailFast(checks)
	fmt.Printf("AggregateFailFast: healthy=%v results=%d\n", fast.Healthy, len(fast.Results))
	for _, r := range fast.Results {
		fmt.Printf("  %-10s err=%v\n", r.Name, r.Err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
Aggregate: healthy=false results=3
  database   err=<nil>
  cache      err=connection refused
  queue      err=<nil>
AggregateFailFast: healthy=false results=2
  database   err=<nil>
  cache      err=connection refused
```

### Tests

`TestAggregate` is a table covering all-healthy, one-failing, a `nil` `Fn`,
and an empty list, and always checks that every entry produced a result.
`TestAggregateFailFastStopsAtFirstFailure` is the sharpest check: it puts a
call-counting check *after* the failing one and asserts it was never
invoked, proving the loop actually `break`s instead of merely returning early
after finishing the range.

Create `health_test.go`:

```go
package health

import (
	"errors"
	"testing"
)

var errDown = errors.New("endpoint down")

func ok() error   { return nil }
func fail() error { return errDown }

func TestAggregate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		checks      []Check
		wantHealthy bool
		wantResults int
	}{
		{
			name:        "all healthy",
			checks:      []Check{{"a", ok}, {"b", ok}, {"c", ok}},
			wantHealthy: true,
			wantResults: 3,
		},
		{
			name:        "one failing still runs every check",
			checks:      []Check{{"a", ok}, {"b", fail}, {"c", ok}},
			wantHealthy: false,
			wantResults: 3,
		},
		{
			name:        "nil Fn counts as failure",
			checks:      []Check{{"a", ok}, {"b", nil}},
			wantHealthy: false,
			wantResults: 2,
		},
		{
			name:        "empty check list is healthy",
			checks:      nil,
			wantHealthy: true,
			wantResults: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			report := Aggregate(tc.checks)
			if report.Healthy != tc.wantHealthy {
				t.Errorf("Healthy = %v, want %v", report.Healthy, tc.wantHealthy)
			}
			if len(report.Results) != tc.wantResults {
				t.Errorf("len(Results) = %d, want %d", len(report.Results), tc.wantResults)
			}
		})
	}
}

func TestAggregateFailFastStopsAtFirstFailure(t *testing.T) {
	t.Parallel()

	calls := 0
	tracking := func() error {
		calls++
		return nil
	}
	checks := []Check{
		{"a", tracking},
		{"b", fail},
		{"c", tracking}, // must never run
	}

	report := AggregateFailFast(checks)
	if report.Healthy {
		t.Fatal("Healthy = true, want false")
	}
	if len(report.Results) != 2 {
		t.Fatalf("len(Results) = %d, want 2 (stopped at first failure)", len(report.Results))
	}
	if calls != 1 {
		t.Fatalf("tracking called %d times, want 1 (only the first, healthy check)", calls)
	}
}

func TestAggregateFailFastAllHealthyRunsAll(t *testing.T) {
	t.Parallel()

	report := AggregateFailFast([]Check{{"a", ok}, {"b", ok}})
	if !report.Healthy {
		t.Fatal("Healthy = false, want true")
	}
	if len(report.Results) != 2 {
		t.Fatalf("len(Results) = %d, want 2", len(report.Results))
	}
}
```

## Review

Both functions are correct when `Healthy` ends up false if and only if some
check either failed or had a `nil` `Fn`, and `AggregateFailFast` is correct
when it additionally never invokes a check after the first failure. The
common mistake this design avoids is writing one function that takes a
`failFast bool` parameter and branching inside the loop body on every
iteration — that scatters the `continue`/`break` decision across an `if`
check on every pass instead of making it the one structural difference
between two small, obviously-correct functions. Run `go test -count=1 ./...`.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — `break` and `continue` in a `for range`.
- [Kubernetes: Configure Liveness, Readiness and Startup Probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/) — the operational distinction between a full health report and a fast readiness gate this module mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-circuit-breaker-exponential-reset.md](16-circuit-breaker-exponential-reset.md) | Next: [18-config-loader-fallback-chain.md](18-config-loader-fallback-chain.md)
