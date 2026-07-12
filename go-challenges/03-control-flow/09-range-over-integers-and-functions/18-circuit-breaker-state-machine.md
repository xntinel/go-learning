# Exercise 18: Circuit Breaker — Fault Tolerance State Machine Transitioning Closed/Open/Half-Open

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

When a downstream dependency starts failing, retrying every call at full
volume only makes the outage worse -- it piles load onto a service that is
already struggling and burns the caller's own resources waiting on timeouts
that were never going to succeed. A circuit breaker fixes this by tracking
consecutive failures and, once they cross a threshold, failing calls fast
without even attempting them, then cautiously probing with a single call
once a cooldown window has passed. Expressing the breaker's decision stream
as an `iter.Seq[Decision]` driven by a `Seq2[time.Time, error]` of call
outcomes turns the whole state machine into something a table-driven test
can replay deterministically. This exercise is an independent module with
its own `go mod init`.

## What you'll build

```text
breaker/                  independent module: example.com/circuit-breaker-state-machine
  go.mod                   module example.com/circuit-breaker-state-machine
  breaker.go               State, Decision, Breaker, New
  cmd/
    demo/
      main.go              runnable demo: open, fail-fast, half-open recovery
  breaker_test.go          opens-then-recovers, reopens-on-failed-probe, early-stop, panic
```

Implement: `New(failureThreshold int, resetTimeout time.Duration) *Breaker` and `(*Breaker) Decisions(calls iter.Seq2[time.Time, error]) iter.Seq[Decision]` yielding one `Decision{Allowed, State}` per call; panics if `failureThreshold < 1` or `resetTimeout <= 0`.
Test: 2 consecutive failures open the breaker, a call inside the timeout is denied fail-fast, a call past the timeout is let through as a half-open probe and closes the breaker on success; a failed probe reopens it; a consumer break stops after one decision; invalid construction panics.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/09-range-over-integers-and-functions/18-circuit-breaker-state-machine/cmd/demo
cd go-solutions/03-control-flow/09-range-over-integers-and-functions/18-circuit-breaker-state-machine
go mod edit -go=1.24
```

The three states and their transitions are: `Closed` lets every call through
and resets its failure counter on success, accumulating failures until
`failureThreshold` is hit, at which point it flips to `Open` and records the
timestamp it opened at. `Open` denies every call without running it, until a
call's timestamp is at least `resetTimeout` past `openedAt`, at which point
the breaker moves to `HalfOpen` and lets that one call through as a probe.
`HalfOpen` closes the breaker (and resets the failure count) on a successful
probe, or reopens it -- restarting the cooldown from the probe's own
timestamp -- on a failed one. The subtlety worth internalizing is that the
open-to-half-open check happens *before* the call is processed: a call that
arrives past the deadline is the one that gets treated as the probe, not a
call after it, so the state transition and the call's own outcome are
resolved in the same step. Driving the cooldown off the timestamps carried
alongside each call, rather than `time.Now()`, is what makes the whole
schedule replayable with hand-built `time.Time` values instead of real
sleeping.

Create `breaker.go`:

```go
package breaker

import (
	"iter"
	"time"
)

// State is one of the three circuit breaker states.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Decision is the breaker's verdict for one call attempt: whether the call
// was let through, and the state the breaker is in after processing it.
type Decision struct {
	Allowed bool
	State   State
}

// Breaker is a circuit breaker: after failureThreshold consecutive failures
// it opens and fails every call fast, without letting it run, until
// resetTimeout has elapsed since it opened; it then allows exactly one probe
// call through in the half-open state, closing again on success or
// reopening on failure.
type Breaker struct {
	failureThreshold int
	resetTimeout     time.Duration

	state    State
	failures int
	openedAt time.Time
}

// New creates a Breaker. failureThreshold must be >= 1 and resetTimeout must
// be > 0.
func New(failureThreshold int, resetTimeout time.Duration) *Breaker {
	if failureThreshold < 1 {
		panic("breaker: failureThreshold must be >= 1")
	}
	if resetTimeout <= 0 {
		panic("breaker: resetTimeout must be > 0")
	}
	return &Breaker{failureThreshold: failureThreshold, resetTimeout: resetTimeout, state: Closed}
}

// Decisions consumes calls -- (timestamp, result) pairs already produced by
// the caller running the real operation -- and yields the breaker's
// decision for each: whether the call was allowed through, and the state
// after processing it. Driving the open-to-half-open transition off the
// call timestamps rather than a real clock is what makes the state machine
// deterministic to test: a fake clock advances by constructing later
// time.Time values, not by sleeping.
func (b *Breaker) Decisions(calls iter.Seq2[time.Time, error]) iter.Seq[Decision] {
	return func(yield func(Decision) bool) {
		for t, err := range calls {
			if b.state == Open {
				if t.Sub(b.openedAt) >= b.resetTimeout {
					b.state = HalfOpen
				} else {
					if !yield(Decision{Allowed: false, State: b.state}) {
						return
					}
					continue
				}
			}

			// Closed or HalfOpen: let the call run and observe the outcome.
			if err == nil {
				b.failures = 0
				b.state = Closed
			} else {
				b.failures++
				if b.state == HalfOpen || b.failures >= b.failureThreshold {
					b.state = Open
					b.openedAt = t
				}
			}
			if !yield(Decision{Allowed: true, State: b.state}) {
				return
			}
		}
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"iter"
	"time"

	"example.com/circuit-breaker-state-machine"
)

var errBoom = errors.New("boom")

func main() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	b := breaker.New(2, time.Second)

	type call struct {
		t   time.Time
		err error
	}
	calls := []call{
		{base, errBoom},
		{base, errBoom},
		{base.Add(500 * time.Millisecond), errBoom},
		{base.Add(1100 * time.Millisecond), nil},
		{base.Add(1200 * time.Millisecond), errBoom},
	}

	seq := iter.Seq2[time.Time, error](func(yield func(time.Time, error) bool) {
		for _, c := range calls {
			if !yield(c.t, c.err) {
				return
			}
		}
	})

	n := 0
	for d := range b.Decisions(seq) {
		fmt.Printf("call %d: allowed=%v state=%s\n", n, d.Allowed, d.State)
		n++
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
call 0: allowed=true state=closed
call 1: allowed=true state=open
call 2: allowed=false state=open
call 3: allowed=true state=closed
call 4: allowed=true state=closed
```

Calls 0 and 1 both fail at the same instant, so the second one crosses the
threshold of 2 and opens the breaker. Call 2, 500ms later, is still inside
the 1-second cooldown and is denied without running. Call 3, 1100ms after
the breaker opened, is past the cooldown, so it is treated as the half-open
probe, succeeds, and closes the breaker. Call 4 fails again, but since the
breaker just closed, one failure is not yet enough to reopen it against a
threshold of 2.

### Tests

Two scenarios matter beyond the happy path: a probe that succeeds and one
that fails, since they lead to opposite next states.

Create `breaker_test.go`:

```go
package breaker

import (
	"errors"
	"iter"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

type call struct {
	t   time.Time
	err error
}

func callsOf(cs []call) iter.Seq2[time.Time, error] {
	return func(yield func(time.Time, error) bool) {
		for _, c := range cs {
			if !yield(c.t, c.err) {
				return
			}
		}
	}
}

func TestBreakerOpensThenRecoversOnSuccessfulProbe(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	b := New(2, time.Second)

	calls := []call{
		{base, errBoom},                              // 1st failure: still closed
		{base, errBoom},                              // 2nd failure: opens
		{base.Add(500 * time.Millisecond), errBoom},   // inside timeout: fail fast
		{base.Add(1100 * time.Millisecond), nil},      // past timeout: half-open probe succeeds
		{base.Add(1200 * time.Millisecond), errBoom},  // closed again, first failure only
	}

	var got []Decision
	for d := range b.Decisions(callsOf(calls)) {
		got = append(got, d)
	}

	want := []Decision{
		{Allowed: true, State: Closed},
		{Allowed: true, State: Open},
		{Allowed: false, State: Open},
		{Allowed: true, State: Closed},
		{Allowed: true, State: Closed},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d decisions, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("decision[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestBreakerReopensOnFailedProbe(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	b := New(1, time.Second)

	calls := []call{
		{base, errBoom},                             // opens after 1 failure
		{base.Add(1100 * time.Millisecond), errBoom}, // half-open probe fails: reopens
		{base.Add(1200 * time.Millisecond), nil},     // still inside new timeout: fail fast
	}

	var got []Decision
	for d := range b.Decisions(callsOf(calls)) {
		got = append(got, d)
	}

	want := []Decision{
		{Allowed: true, State: Open},
		{Allowed: true, State: Open},
		{Allowed: false, State: Open},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d decisions, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("decision[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestBreakerStopsEarly(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	b := New(1, time.Second)

	calls := []call{{base, errBoom}, {base, errBoom}, {base, errBoom}}
	count := 0
	for range b.Decisions(callsOf(calls)) {
		count++
		break
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestNewPanicsOnInvalidArgs(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for failureThreshold < 1")
		}
	}()
	New(0, time.Second)
}
```

## Review

The transition that is easiest to get wrong is deciding whether a call
qualifies as the half-open probe. Checking `t.Sub(b.openedAt) >= resetTimeout`
*before* processing the call's own outcome, rather than after, is what
guarantees the very call that crossed the deadline is the one treated as the
probe -- get that ordering backwards and the breaker either probes one call
too early (before the cooldown truly elapsed) or denies one call too many
(the first call after the cooldown gets fail-fasted instead of tried). The
other detail that matters is that a failed probe reopens the breaker with a
fresh `openedAt` set to the probe's own timestamp, not the original one --
otherwise the cooldown would not actually restart, and a persistently
failing dependency would get probed every single call instead of being
properly quarantined again.

## Resources

- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html)
- [`iter.Seq2` documentation](https://pkg.go.dev/iter#Seq2)
- [Microsoft Azure Architecture: Circuit Breaker pattern](https://learn.microsoft.com/en-us/azure/architecture/patterns/circuit-breaker)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-connection-pool-lease-iterator.md](17-connection-pool-lease-iterator.md) | Next: [19-health-check-scheduler.md](19-health-check-scheduler.md)
