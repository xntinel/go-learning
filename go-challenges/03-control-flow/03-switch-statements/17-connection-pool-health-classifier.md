# Exercise 17: Classify Connection Pool Health With Fail-Closed Default

**Nivel: Intermedio** — validacion rapida (un test corto).

Every backend service that talks to a database ships a periodic health
check, and the check is only useful if it tells the truth when the pool is
actually in trouble. This module classifies a connection pool's stats
snapshot — active connections, idle connections, waiting goroutines, and the
configured max — into `healthy`, `degraded`, or `unhealthy` with a tagless
switch, defaulting to the worst-case answer whenever the numbers look
missing or corrupt. It is self-contained: its own `go mod init`, code, demo,
and test.

## What you'll build

```text
connpool/                  independent module: example.com/connection-pool-health-classifier
  go.mod                    go 1.24
  connpool.go                package connpool; Stats; Classify(Stats) string
  cmd/demo/main.go           runnable demo over six representative snapshots
  connpool_test.go           table over healthy, degraded, unhealthy, and malformed input
```

- Implement: `Classify(s Stats) string` — a tagless switch mixing numeric range checks (integer cross-multiplication for the 80% threshold, to avoid float rounding) with boolean conditions, defaulting to `"unhealthy"`.
- Test: a table covering the healthy path, both degraded triggers, three ways to be unhealthy, and two shapes of malformed input.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/03-switch-statements/17-connection-pool-health-classifier/cmd/demo
cd go-solutions/03-control-flow/03-switch-statements/17-connection-pool-health-classifier
go mod edit -go=1.24
```

### Why integer cross-multiplication instead of a float ratio

The obvious way to check "80% utilized" is
`float64(s.Active)/float64(s.Max) >= 0.8`, and it is the wrong habit to build:
floating-point division introduces rounding that makes the boundary itself
untestable — you can never be fully sure whether `8.0/10.0 >= 0.8` behaves
identically to `80.0/100.0 >= 0.8` across every combination of pool sizes a
caller configures. Since both operands here are always non-negative integers,
`s.Active*10 >= s.Max*8` expresses the exact same 80% threshold with no
rounding at all, and it is the kind of substitution worth reaching for
whenever a switch's predicate is a ratio over integer counters.

The ordering matters, and it is deliberately fail-closed at every level: an
unconfigured pool (`Max <= 0`) and corrupted counters (any negative field)
are checked *before* anything else, because a ratio computed against a zero
or garbage `Max` is meaningless, not just wrong. Waiting callers and full
saturation are checked next and both resolve straight to `unhealthy` — a
blocked goroutine is worse than "elevated utilization," so those cases must
precede the milder `degraded` checks or a saturated pool with zero waiters
this instant would misreport as merely degraded.

Create `connpool.go`:

```go
// Package connpool classifies database connection pool health snapshots
// into Healthy, Degraded, or Unhealthy using a tagless switch, defaulting
// conservatively whenever the reported stats are missing or out of range.
package connpool

// Stats is a single point-in-time snapshot of a connection pool's counters.
type Stats struct {
	Active  int // connections currently checked out by callers
	Idle    int // connections sitting in the pool, ready to reuse
	Waiting int // goroutines blocked waiting for a connection right now
	Max     int // configured maximum pool size
}

// Classify grades a Stats snapshot. Anything that looks like a
// misconfigured or corrupt report — a non-positive Max, a negative counter —
// resolves to Unhealthy rather than Healthy, because a monitoring system
// that defaults an unreadable metric to "fine" hides the exact outage it
// exists to catch.
func Classify(s Stats) string {
	switch {
	case s.Max <= 0:
		return "unhealthy" // pool isn't configured at all
	case s.Active < 0 || s.Idle < 0 || s.Waiting < 0:
		return "unhealthy" // corrupt or missing counters
	case s.Waiting > 0:
		return "unhealthy" // callers are already blocked on a connection
	case s.Active >= s.Max:
		return "unhealthy" // fully saturated, next request will block
	case s.Active*10 >= s.Max*8:
		return "degraded" // at or above 80% utilization
	case s.Idle == 0:
		return "degraded" // no idle headroom even under 80% utilization
	default:
		return "healthy"
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	connpool "example.com/connection-pool-health-classifier"
)

func main() {
	snapshots := []connpool.Stats{
		{Active: 2, Idle: 8, Waiting: 0, Max: 10},
		{Active: 9, Idle: 1, Waiting: 0, Max: 10},
		{Active: 5, Idle: 0, Waiting: 0, Max: 10},
		{Active: 3, Idle: 2, Waiting: 1, Max: 10},
		{Active: 10, Idle: 0, Waiting: 0, Max: 10},
		{Active: 1, Idle: 1, Waiting: 0, Max: 0},
	}

	for _, s := range snapshots {
		fmt.Printf("%+v -> %s\n", s, connpool.Classify(s))
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
{Active:2 Idle:8 Waiting:0 Max:10} -> healthy
{Active:9 Idle:1 Waiting:0 Max:10} -> degraded
{Active:5 Idle:0 Waiting:0 Max:10} -> degraded
{Active:3 Idle:2 Waiting:1 Max:10} -> unhealthy
{Active:10 Idle:0 Waiting:0 Max:10} -> unhealthy
{Active:1 Idle:1 Waiting:0 Max:0} -> unhealthy
```

### Tests

`TestClassify` runs a table over the healthy path, the exact 80% boundary
(and one point below it, to prove the boundary isn't off-by-one), the
zero-idle degraded trigger, waiting callers, full saturation, an
over-saturated pool, an unconfigured pool, and a negative counter.

Create `connpool_test.go`:

```go
package connpool

import "testing"

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		s    Stats
		want string
	}{
		{"low utilization, idle headroom", Stats{Active: 2, Idle: 8, Waiting: 0, Max: 10}, "healthy"},
		{"just under 80% with idle", Stats{Active: 7, Idle: 3, Waiting: 0, Max: 10}, "healthy"},
		{"exactly 80% utilization", Stats{Active: 8, Idle: 2, Waiting: 0, Max: 10}, "degraded"},
		{"no idle headroom below 80%", Stats{Active: 5, Idle: 0, Waiting: 0, Max: 10}, "degraded"},
		{"callers waiting", Stats{Active: 3, Idle: 2, Waiting: 1, Max: 10}, "unhealthy"},
		{"fully saturated", Stats{Active: 10, Idle: 0, Waiting: 0, Max: 10}, "unhealthy"},
		{"active exceeds max", Stats{Active: 12, Idle: 0, Waiting: 0, Max: 10}, "unhealthy"},
		{"unconfigured pool", Stats{Active: 1, Idle: 1, Waiting: 0, Max: 0}, "unhealthy"},
		{"negative counter", Stats{Active: -1, Idle: 1, Waiting: 0, Max: 10}, "unhealthy"},
	}

	for _, tc := range tests {
		if got := Classify(tc.s); got != tc.want {
			t.Errorf("%s: Classify(%+v) = %q, want %q", tc.name, tc.s, got, tc.want)
		}
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The classifier is correct when every unhealthy-implying condition — waiting
callers, saturation, corrupt or missing metrics — is checked before the
milder degraded checks, and when a caller can never observe "healthy" from a
pool that is actually blocking requests. Carry this forward: when a switch's
predicate is a ratio over integer counters, cross-multiply instead of
dividing into a float, and on any health or capacity classifier, order the
worst-case conditions first and let the default (or, as here, the final
`case`) be the one genuinely safe outcome.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless (expressionless) switch form.
- [database/sql: DBStats](https://pkg.go.dev/database/sql#DBStats) — the real-world shape of the counters this exercise classifies.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-circuit-breaker-state-machine.md](16-circuit-breaker-state-machine.md) | Next: [18-cache-eviction-policy-router.md](18-cache-eviction-policy-router.md)
