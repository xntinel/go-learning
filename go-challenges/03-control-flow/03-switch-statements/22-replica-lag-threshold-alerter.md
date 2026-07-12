# Exercise 22: Monitor Database Replica Lag With Threshold Cascades

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A leader that reads its own replica's lag has to decide, every time it
checks: serve reads locally, degrade to admittedly-stale reads, or reject
queries outright until the replica catches up. This module makes that
decision with a tagless switch on lag ranges, and instruments it with a
second tagless switch that uses `fallthrough` to record every severity
threshold a single reading crossed — because a reading bad enough to
reject is, by definition, also bad enough to have crossed the degrade
threshold, and a monitoring dashboard that only counts the worst case loses
that information. It is self-contained: its own `go mod init`, code, demo,
and test.

## What you'll build

```text
replicalag/                 independent module: example.com/replica-lag-threshold-alerter
  go.mod                     go 1.24
  replicalag.go               package replicalag; Metrics; Evaluate(lagMs, *Metrics) string
  cmd/demo/main.go            runnable demo over a rising sequence of lag readings
  replicalag_test.go          table over threshold boundaries plus an accumulation test across repeated calls
```

- Implement: `Evaluate(lagMs int, m *Metrics) string` — a tagless switch that decides the action ("serve", "degrade", "reject") without fallthrough, and a second tagless switch that cascades threshold-crossing counts and log lines with `fallthrough`.
- Test: a table over both threshold boundaries and the readings between and beyond them, plus a test that accumulates a rising sequence of readings and asserts crossings never reset and reject crossings never exceed degrade crossings.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Two switches, two different jobs, the same predicates

`Evaluate` reasons about the same lag thresholds twice, in two switches that
look almost identical — and that repetition is deliberate, not an
oversight. The first switch decides the action, and it must return exactly
one answer per reading: no `fallthrough`, because "serve" and "degrade" and
"reject" are mutually exclusive outcomes, and falling through from `reject`
into `degrade` would silently overwrite `action` with the wrong value. The
second switch instruments the same reading, and here `fallthrough` is
exactly right, for the same reason the permission-cascade and
queue-priority exercises use it: a reading of 1500ms trips the reject case,
which increments `RejectCrossings`, logs it, and then falls through into
the degrade case, incrementing `DegradeCrossings` too — because that
reading really did also exceed the degrade threshold, and a dashboard that
only reflects the worst bucket can't answer "how often has lag gone above
200ms at all," only "how often has it gone above 1000ms." Trying to make
one switch do both jobs — return the action *and* cascade the metrics —
would require the decision switch to fall through, which breaks the
mutual-exclusivity that `action` depends on.

Create `replicalag.go`:

```go
// Package replicalag decides how a leader should serve reads given a
// replica's current lag, and instruments every severity threshold the lag
// has crossed using a tagless switch with fallthrough: exceeding the
// reject threshold necessarily also crosses the degrade threshold, so both
// counters increment for the same reading.
package replicalag

import "fmt"

// Metrics accumulates threshold-crossing counts and a human-readable log
// across repeated calls to Evaluate, as a real leader would across a
// monitoring loop.
type Metrics struct {
	ServeChecks      int
	DegradeCrossings int
	RejectCrossings  int
	Log              []string
}

// Evaluate decides how to serve reads given the current replica lag, in
// milliseconds, and updates m to reflect every threshold this reading
// crossed. It returns one of "serve", "degrade", or "reject".
func Evaluate(lagMs int, m *Metrics) string {
	var action string
	switch {
	case lagMs >= 1000:
		action = "reject"
	case lagMs >= 200:
		action = "degrade"
	default:
		action = "serve"
	}

	// Instrumentation cascades independently of the decision above: a
	// reading that trips "reject" is, by definition, also a reading that
	// would have tripped "degrade" on its own, so both crossings are
	// counted for the same event via fallthrough.
	switch {
	case lagMs >= 1000:
		m.RejectCrossings++
		m.Log = append(m.Log, fmt.Sprintf("reject threshold crossed: lag=%dms", lagMs))
		fallthrough
	case lagMs >= 200:
		m.DegradeCrossings++
		m.Log = append(m.Log, fmt.Sprintf("degrade threshold crossed: lag=%dms", lagMs))
		fallthrough
	default:
		m.ServeChecks++
	}

	return action
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	replicalag "example.com/replica-lag-threshold-alerter"
)

func main() {
	m := &replicalag.Metrics{}

	readings := []int{50, 199, 200, 750, 1000, 1500}
	for _, lag := range readings {
		action := replicalag.Evaluate(lag, m)
		fmt.Printf("lag=%5dms -> %s\n", lag, action)
	}

	fmt.Printf("\nserveChecks=%d degradeCrossings=%d rejectCrossings=%d\n",
		m.ServeChecks, m.DegradeCrossings, m.RejectCrossings)
	fmt.Println("log:")
	for _, line := range m.Log {
		fmt.Println(" ", line)
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
lag=   50ms -> serve
lag=  199ms -> serve
lag=  200ms -> degrade
lag=  750ms -> degrade
lag= 1000ms -> reject
lag= 1500ms -> reject

serveChecks=6 degradeCrossings=4 rejectCrossings=2
log:
  degrade threshold crossed: lag=200ms
  degrade threshold crossed: lag=750ms
  reject threshold crossed: lag=1000ms
  degrade threshold crossed: lag=1000ms
  reject threshold crossed: lag=1500ms
  degrade threshold crossed: lag=1500ms
```

### Tests

`TestEvaluateSingleReading` runs a table over both threshold boundaries
(199ms vs. exactly 200ms, and exactly 1000ms) and asserts the action and
the per-call crossing counts for each. `TestEvaluateAccumulatesAcrossCalls`
drives the same rising sequence the demo uses through one shared `Metrics`,
the way a real monitoring loop reuses one struct across many checks, and
asserts three things a dashboard depends on: crossings accumulate rather
than reset between calls, `RejectCrossings` never exceeds
`DegradeCrossings` (the invariant the fallthrough cascade exists to
guarantee), and the log grew by exactly the expected number of lines.

Create `replicalag_test.go`:

```go
package replicalag

import "testing"

func TestEvaluateSingleReading(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                    string
		lagMs                   int
		wantAction              string
		wantDegrade, wantReject int
	}{
		{"well under degrade threshold", 50, "serve", 0, 0},
		{"one ms below degrade threshold", 199, "serve", 0, 0},
		{"exactly at degrade threshold", 200, "degrade", 1, 0},
		{"between degrade and reject", 750, "degrade", 1, 0},
		{"exactly at reject threshold", 1000, "reject", 1, 1},
		{"well past reject threshold", 1500, "reject", 1, 1},
	}

	for _, tc := range tests {
		m := &Metrics{}
		got := Evaluate(tc.lagMs, m)
		if got != tc.wantAction {
			t.Errorf("%s: Evaluate(%d) = %q, want %q", tc.name, tc.lagMs, got, tc.wantAction)
		}
		if m.ServeChecks != 1 {
			t.Errorf("%s: ServeChecks = %d, want 1 (every reading is checked)", tc.name, m.ServeChecks)
		}
		if m.DegradeCrossings != tc.wantDegrade {
			t.Errorf("%s: DegradeCrossings = %d, want %d", tc.name, m.DegradeCrossings, tc.wantDegrade)
		}
		if m.RejectCrossings != tc.wantReject {
			t.Errorf("%s: RejectCrossings = %d, want %d", tc.name, m.RejectCrossings, tc.wantReject)
		}
	}
}

// TestEvaluateAccumulatesAcrossCalls drives a sequence of readings through
// the same Metrics, the way a real monitoring loop would, and checks that
// crossings accumulate rather than reset, and that a reject-level reading
// always counts as a degrade crossing too.
func TestEvaluateAccumulatesAcrossCalls(t *testing.T) {
	t.Parallel()

	m := &Metrics{}
	readings := []int{50, 199, 200, 750, 1000, 1500}
	for _, lag := range readings {
		Evaluate(lag, m)
	}

	if m.ServeChecks != 6 {
		t.Errorf("ServeChecks = %d, want 6", m.ServeChecks)
	}
	if m.DegradeCrossings != 4 {
		t.Errorf("DegradeCrossings = %d, want 4", m.DegradeCrossings)
	}
	if m.RejectCrossings != 2 {
		t.Errorf("RejectCrossings = %d, want 2", m.RejectCrossings)
	}
	if m.RejectCrossings > m.DegradeCrossings {
		t.Fatalf("RejectCrossings (%d) > DegradeCrossings (%d): a reject reading must also count as a degrade crossing",
			m.RejectCrossings, m.DegradeCrossings)
	}
	if len(m.Log) != 6 {
		t.Errorf("len(Log) = %d, want 6", len(m.Log))
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The alerter is correct when the decision switch always returns exactly one
action per reading (no fallthrough there), when the instrumentation switch
cascades so that a reject-level reading is counted in both `RejectCrossings`
and `DegradeCrossings`, and when repeated calls against the same `Metrics`
accumulate instead of overwriting. Carry this forward: when a single event
needs both a mutually-exclusive decision and a cumulative record of every
threshold it crossed, don't force one switch to do both — a plain switch
for the decision and a `fallthrough` switch for the cascade, evaluated
against the same predicates, is clearer than either overloading `action`
with fallthrough or hand-rolling the cascade with nested `if` statements.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless switch form and the fallthrough statement.
- [PostgreSQL: Replication Lag](https://www.postgresql.org/docs/current/hot-standby.html) — the kind of lag metric this exercise classifies.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-api-auth-scheme-classifier.md](21-api-auth-scheme-classifier.md) | Next: [23-sharding-key-router-with-hashing.md](23-sharding-key-router-with-hashing.md)
