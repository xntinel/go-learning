# Exercise 27: Reject Requests When System Load Exceeds Thresholds

**Nivel: Intermedio** — validacion rapida (un test corto).

Every service eventually meets a load spike it cannot fully absorb, and
the difference between a graceful degradation and a full outage is whether
something is actively shedding load before the process falls over. This
module gates request admission on three independent signals — queue depth,
p99 latency, and error rate — classifying the system's state and choosing
`ADMIT`, `QUEUE`, or `REJECT` accordingly, with a specific reason string
attached so an on-call engineer reading the log line knows exactly which
signal tripped. It is self-contained: its own `go mod init`, code, demo,
and test.

## What you'll build

```text
loadshed/                  independent module: example.com/load-shedding-admission-controller
  go.mod                    go 1.24
  loadshed.go                package loadshed; Metrics; Thresholds; Default; Decision; Decide(Metrics, Thresholds) (Decision, string)
  cmd/demo/main.go           runnable demo over seven representative snapshots
  loadshed_test.go           table covering healthy, each degraded trigger, and each critical trigger
```

- Implement: `Decide(m Metrics, t Thresholds) (Decision, string)` — a tagless switch checking critical-level compound predicates before degraded-level ones, returning both the decision and the specific reason.
- Test: a table over the healthy path, each of the three degraded triggers, each of the three critical triggers, and the priority ordering when multiple signals are critical at once.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/loadshed/cmd/demo
cd ~/go-exercises/loadshed
go mod init example.com/load-shedding-admission-controller
go mod edit -go=1.24
```

### Why three signals instead of one, and why this order

A load shedder watching only queue depth misses the service that is
still accepting work quickly but returning wrong answers under load — a
climbing error rate with a shallow queue, which happens when downstream
capacity has already collapsed and requests are failing fast rather than
piling up. A shedder watching only latency misses a queue that is deep but
still draining within its SLO because the requests inside it are cheap.
Each signal catches a failure mode the other two do not, which is why
`Decide` checks all three, and why it checks them worst-tier-first: a
critical error rate rejects the request even if queue depth and latency
both happen to look fine in the same snapshot, because a critical error
rate means the system is already failing work, and admitting more of it
compounds the failure instead of helping.

Within a tier, error rate is checked before queue depth and latency
because it is the most direct signal that downstream capacity is gone —
a queue can be deep because of a transient traffic spike that is still
being served correctly, but an elevated error rate means requests are
actively failing right now. This ordering is a judgment call specific to
this kind of service, and it is exactly the sort of decision the concepts
file insists must be commented as deliberate, not left as an accident of
how the cases happened to get typed.

Create `loadshed.go`:

```go
// Package loadshed decides whether an incoming request should be admitted,
// queued, or rejected outright, based on three independent load signals:
// queue depth, tail latency, and error rate. A service that only sheds load
// on one signal (say, queue depth) keeps admitting requests while p99
// latency or the error rate silently climbs past the point of no return;
// checking all three, worst-first, is what makes a load shedder trustworthy
// under a real incident.
package loadshed

// Metrics is a point-in-time snapshot of the signals an admission
// controller inspects before letting a new request in.
type Metrics struct {
	QueueDepth       int     // requests currently waiting for a worker
	P99LatencyMillis float64 // observed p99 latency over the last sampling window
	ErrorRatePercent float64 // percentage of recent requests that errored
}

// Thresholds is the pair of cutoffs (a Degraded level and a worse Critical
// level) applied to each of the three signals.
type Thresholds struct {
	QueueDepthDegraded, QueueDepthCritical int
	P99LatencyDegraded, P99LatencyCritical float64
	ErrorRateDegraded, ErrorRateCritical   float64
}

// Default mirrors thresholds a mid-sized HTTP service might actually run:
// queueing starts to matter in the low hundreds, tail latency degrades
// past half a second and turns critical past two seconds, and an error
// rate above 10% is a signal something downstream is already failing.
var Default = Thresholds{
	QueueDepthDegraded: 200,
	QueueDepthCritical: 1000,
	P99LatencyDegraded: 500,
	P99LatencyCritical: 2000,
	ErrorRateDegraded:  10,
	ErrorRateCritical:  50,
}

// Decision is the admission outcome.
type Decision string

const (
	Admit  Decision = "ADMIT"
	Queue  Decision = "QUEUE"
	Reject Decision = "REJECT"
)

// Decide classifies a Metrics snapshot against t and returns both the
// admission decision and the specific reason a caller (or an on-call
// engineer reading the log line) needs to know which signal tripped.
// Critical-level signals are checked before degraded-level ones, and
// within each tier the order itself is a judgment call about which signal
// is the most reliable early indicator of an outage in this kind of
// service; error rate is checked first because a climbing error rate means
// downstream capacity is already gone, which is a stronger signal than a
// queue that is merely deep but still draining.
func Decide(m Metrics, t Thresholds) (Decision, string) {
	switch {
	case m.ErrorRatePercent >= t.ErrorRateCritical:
		return Reject, "error rate critical"
	case m.QueueDepth >= t.QueueDepthCritical:
		return Reject, "queue depth critical"
	case m.P99LatencyMillis >= t.P99LatencyCritical:
		return Reject, "p99 latency critical"
	case m.ErrorRatePercent >= t.ErrorRateDegraded:
		return Queue, "error rate degraded"
	case m.QueueDepth >= t.QueueDepthDegraded:
		return Queue, "queue depth degraded"
	case m.P99LatencyMillis >= t.P99LatencyDegraded:
		return Queue, "p99 latency degraded"
	default:
		return Admit, "healthy"
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	loadshed "example.com/load-shedding-admission-controller"
)

func main() {
	snapshots := []loadshed.Metrics{
		{QueueDepth: 10, P99LatencyMillis: 80, ErrorRatePercent: 0.5},
		{QueueDepth: 250, P99LatencyMillis: 120, ErrorRatePercent: 1},
		{QueueDepth: 50, P99LatencyMillis: 650, ErrorRatePercent: 2},
		{QueueDepth: 50, P99LatencyMillis: 120, ErrorRatePercent: 15},
		{QueueDepth: 1200, P99LatencyMillis: 120, ErrorRatePercent: 2},
		{QueueDepth: 50, P99LatencyMillis: 2500, ErrorRatePercent: 2},
		{QueueDepth: 50, P99LatencyMillis: 120, ErrorRatePercent: 60},
	}

	for _, m := range snapshots {
		decision, reason := loadshed.Decide(m, loadshed.Default)
		fmt.Printf("%+v -> %s (%s)\n", m, decision, reason)
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
{QueueDepth:10 P99LatencyMillis:80 ErrorRatePercent:0.5} -> ADMIT (healthy)
{QueueDepth:250 P99LatencyMillis:120 ErrorRatePercent:1} -> QUEUE (queue depth degraded)
{QueueDepth:50 P99LatencyMillis:650 ErrorRatePercent:2} -> QUEUE (p99 latency degraded)
{QueueDepth:50 P99LatencyMillis:120 ErrorRatePercent:15} -> QUEUE (error rate degraded)
{QueueDepth:1200 P99LatencyMillis:120 ErrorRatePercent:2} -> REJECT (queue depth critical)
{QueueDepth:50 P99LatencyMillis:2500 ErrorRatePercent:2} -> REJECT (p99 latency critical)
{QueueDepth:50 P99LatencyMillis:120 ErrorRatePercent:60} -> REJECT (error rate critical)
```

### Tests

`TestDecide` runs a table covering the fully healthy path, each of the
three degraded triggers in isolation, each of the three critical triggers
in isolation, and a case where every signal is simultaneously critical to
confirm error rate wins the ordering.

Create `loadshed_test.go`:

```go
package loadshed

import "testing"

func TestDecide(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		m          Metrics
		wantDec    Decision
		wantReason string
	}{
		{
			name:       "well within every threshold admits",
			m:          Metrics{QueueDepth: 10, P99LatencyMillis: 80, ErrorRatePercent: 0.5},
			wantDec:    Admit,
			wantReason: "healthy",
		},
		{
			name:       "queue depth degraded queues",
			m:          Metrics{QueueDepth: 250, P99LatencyMillis: 120, ErrorRatePercent: 1},
			wantDec:    Queue,
			wantReason: "queue depth degraded",
		},
		{
			name:       "p99 latency degraded queues",
			m:          Metrics{QueueDepth: 50, P99LatencyMillis: 650, ErrorRatePercent: 2},
			wantDec:    Queue,
			wantReason: "p99 latency degraded",
		},
		{
			name:       "error rate degraded queues, and is checked before the milder signals",
			m:          Metrics{QueueDepth: 250, P99LatencyMillis: 650, ErrorRatePercent: 15},
			wantDec:    Queue,
			wantReason: "error rate degraded",
		},
		{
			name:       "queue depth critical rejects",
			m:          Metrics{QueueDepth: 1200, P99LatencyMillis: 120, ErrorRatePercent: 2},
			wantDec:    Reject,
			wantReason: "queue depth critical",
		},
		{
			name:       "p99 latency critical rejects",
			m:          Metrics{QueueDepth: 50, P99LatencyMillis: 2500, ErrorRatePercent: 2},
			wantDec:    Reject,
			wantReason: "p99 latency critical",
		},
		{
			name:       "error rate critical rejects even if queue and latency are also critical",
			m:          Metrics{QueueDepth: 1200, P99LatencyMillis: 2500, ErrorRatePercent: 60},
			wantDec:    Reject,
			wantReason: "error rate critical",
		},
	}

	for _, tc := range tests {
		gotDec, gotReason := Decide(tc.m, Default)
		if gotDec != tc.wantDec || gotReason != tc.wantReason {
			t.Errorf("%s: Decide(%+v) = (%s, %q), want (%s, %q)",
				tc.name, tc.m, gotDec, gotReason, tc.wantDec, tc.wantReason)
		}
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The controller is correct when a critical-level signal on any one metric
rejects the request regardless of what the other two metrics look like,
when the degraded tier is only reached once every critical check has
already failed, and when the returned reason string always names the
specific signal that decided the outcome rather than a generic "overload."
Carry this forward: when a switch dispatches on several independent
compound predicates that can each independently demand the harshest
outcome, order the harshest tier first in its entirety before considering
any milder tier, and never let a switch collapse "what happened" into a
single boolean when the caller downstream (a log line, an alert, an
on-call engineer) needs to know which specific condition fired.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless (expressionless) switch form.
- [Google SRE Workbook: Handling Overload](https://sre.google/sre-book/handling-overload/) — load shedding and graceful degradation in production systems.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-sliding-window-rate-limiter.md](26-sliding-window-rate-limiter.md) | Next: [28-cron-schedule-expression-matcher.md](28-cron-schedule-expression-matcher.md)
