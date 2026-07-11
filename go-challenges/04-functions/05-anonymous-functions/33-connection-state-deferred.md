# Exercise 33: Connection Resilience Tracking via Deferred Closure State Monitor

**Nivel: Intermedio** — validacion rapida (un test corto).

Deciding whether a connection has just degraded, just recovered, or hasn't
changed state at all is easy to get wrong if the transition logic is
duplicated at every call site that makes an attempt. This module builds
`Monitor.Attempt`, which runs an operation and defers a closure that reads
the named return `err` to update state, then reports whether *this specific
attempt* changed anything via a second named return, `transitioned`.

This module is fully self-contained. Nothing here imports another
exercise.

## What you'll build

```text
connstate/                    module example.com/connstate
  go.mod
  connstate.go                  State, Monitor, NewMonitor, Attempt (deferred state update)
  connstate_test.go               success stays up, full transition sequence, recovery resets
  cmd/demo/main.go              a sequence of successes and failures through Attempt
```

- Files: `connstate.go`, `connstate_test.go`, `cmd/demo/main.go`.
- Implement: `Monitor{consecutiveFailures, degradeThreshold, downThreshold, state}`; `Attempt(op) (transitioned bool, err error)` deferring a closure that updates `state` from `err` and sets `transitioned`.
- Test: a lone success stays up with no transition; a scripted sequence of failures and a recovery drives `up -> degraded -> down -> up`; recovery resets the failure counter, not just the state.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/connstate/cmd/demo
cd ~/go-exercises/connstate
go mod init example.com/connstate
go mod edit -go=1.24
```

### The deferred closure decides state from the named return, then reports itself

`Attempt` records `prev := m.state` before running anything, then defers a
closure and only afterward calls `err = op()`. The closure reads `err`
after `op` has actually run — exactly the guard-clause-over-a-named-return
idiom this chapter uses for cleanup, applied here to state transitions
instead: on failure it increments `consecutiveFailures` and escalates
`state` through `StateDegraded` to `StateDown` once the respective
thresholds are crossed; on any success it resets the counter and snaps
straight back to `StateUp`. The closure's very last line,
`transitioned = m.state != prev`, is what makes `transitioned` trustworthy:
it compares against the state captured *before* `op` ran, not some
approximation, so a caller can tell "this attempt is the one that tipped us
into `StateDown`" apart from "we were already down and stayed down" — the
difference between an event worth alerting on and routine, already-known
bad news.

Create `connstate.go`:

```go
package connstate

import "fmt"

// State is a connection's resilience state.
type State int

const (
	StateUp State = iota
	StateDegraded
	StateDown
)

func (s State) String() string {
	switch s {
	case StateUp:
		return "up"
	case StateDegraded:
		return "degraded"
	case StateDown:
		return "down"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// Monitor tracks a single connection's resilience state across attempts.
type Monitor struct {
	consecutiveFailures int
	degradeThreshold    int
	downThreshold       int
	state               State
}

// NewMonitor returns a Monitor that moves to StateDegraded after
// degradeThreshold consecutive failures and to StateDown after
// downThreshold.
func NewMonitor(degradeThreshold, downThreshold int) *Monitor {
	return &Monitor{degradeThreshold: degradeThreshold, downThreshold: downThreshold}
}

// State reports the monitor's current state.
func (m *Monitor) State() State { return m.state }

// Attempt runs op and, via a deferred closure, updates the monitor's state
// from the outcome. The closure reads the named return err only after op
// has run, so it sees the real final outcome, and it writes the named
// return transitioned as its very last act, reporting whether this
// attempt actually changed the monitor's state -- the signal a caller
// needs to decide whether to emit a degradation or recovery alert.
func (m *Monitor) Attempt(op func() error) (transitioned bool, err error) {
	prev := m.state
	defer func() {
		if err != nil {
			m.consecutiveFailures++
			switch {
			case m.consecutiveFailures >= m.downThreshold:
				m.state = StateDown
			case m.consecutiveFailures >= m.degradeThreshold:
				m.state = StateDegraded
			}
		} else {
			m.consecutiveFailures = 0
			m.state = StateUp
		}
		transitioned = m.state != prev
	}()

	err = op()
	return
}
```

### The runnable demo

The demo scripts six attempts — a success, four failures, and a recovery —
against a monitor that degrades at 2 consecutive failures and goes down at
4.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/connstate"
)

func main() {
	m := connstate.NewMonitor(2, 4) // degrade at 2 consecutive failures, down at 4

	fail := errors.New("dial timeout")
	attempts := []error{nil, fail, fail, fail, fail, nil}

	for i, want := range attempts {
		transitioned, err := m.Attempt(func() error { return want })
		fmt.Printf("attempt %d: state=%s transitioned=%v err=%v\n", i, m.State(), transitioned, err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempt 0: state=up transitioned=false err=<nil>
attempt 1: state=up transitioned=false err=dial timeout
attempt 2: state=degraded transitioned=true err=dial timeout
attempt 3: state=degraded transitioned=false err=dial timeout
attempt 4: state=down transitioned=true err=dial timeout
attempt 5: state=up transitioned=true err=<nil>
```

### Tests

`TestAttemptSuccessKeepsStateUp` checks the very first success reports no
transition. `TestAttemptSequenceDrivesTransitions` scripts the same six-step
sequence as the demo and asserts the exact state and `transitioned` value
at every step. `TestAttemptRecoveryResetsFailureCount` checks that after a
recovery, a single subsequent failure does not immediately re-degrade the
connection — proving the counter was reset to zero, not merely
decremented.

Create `connstate_test.go`:

```go
package connstate

import (
	"errors"
	"testing"
)

func TestAttemptSuccessKeepsStateUp(t *testing.T) {
	t.Parallel()
	m := NewMonitor(2, 4)
	transitioned, err := m.Attempt(func() error { return nil })
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if transitioned {
		t.Fatal("transitioned = true on the very first success, want false (already up)")
	}
	if m.State() != StateUp {
		t.Fatalf("State() = %v, want up", m.State())
	}
}

func TestAttemptSequenceDrivesTransitions(t *testing.T) {
	t.Parallel()
	m := NewMonitor(2, 4)
	fail := errors.New("dial timeout")

	cases := []struct {
		op               error
		wantState        State
		wantTransitioned bool
	}{
		{nil, StateUp, false},        // 1: still up
		{fail, StateUp, false},       // 1 consecutive failure: below degrade threshold
		{fail, StateDegraded, true},  // 2 consecutive failures: degrades
		{fail, StateDegraded, false}, // 3: already degraded
		{fail, StateDown, true},      // 4 consecutive failures: goes down
		{nil, StateUp, true},         // recovers immediately on success
	}

	for i, tc := range cases {
		op := tc.op
		transitioned, err := m.Attempt(func() error { return op })
		if (err != nil) != (op != nil) {
			t.Fatalf("step %d: err = %v, want matching op %v", i, err, op)
		}
		if m.State() != tc.wantState {
			t.Fatalf("step %d: State() = %v, want %v", i, m.State(), tc.wantState)
		}
		if transitioned != tc.wantTransitioned {
			t.Fatalf("step %d: transitioned = %v, want %v", i, transitioned, tc.wantTransitioned)
		}
	}
}

func TestAttemptRecoveryResetsFailureCount(t *testing.T) {
	t.Parallel()
	m := NewMonitor(2, 4)
	fail := errors.New("boom")

	m.Attempt(func() error { return fail })
	m.Attempt(func() error { return fail }) // now degraded, 2 consecutive failures
	m.Attempt(func() error { return nil })  // recovers, resets counter to 0

	// A single subsequent failure must not re-degrade immediately, proving
	// the counter actually reset instead of just being decremented.
	transitioned, _ := m.Attempt(func() error { return fail })
	if transitioned {
		t.Fatal("transitioned = true after only 1 failure post-recovery, want false")
	}
	if m.State() != StateUp {
		t.Fatalf("State() = %v, want up (1 failure after reset must not degrade)", m.State())
	}
}
```

## Review

`Attempt` is correct when `transitioned` is `true` exactly on the calls
that actually change `State()`, and never on calls that merely confirm an
existing state. The recovery-resets-the-counter test is the one that would
catch a common shortcut: setting `state` back to `StateUp` on success while
forgetting to zero `consecutiveFailures`, which would leave a connection
one failure away from re-degrading immediately after it just recovered.
Reading `err` from the named return inside the defer — rather than
threading a local `ok bool` through `Attempt`'s body — is what keeps the
transition logic in exactly one place regardless of how many future
call sites `Attempt` gets.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Effective Go: Defer](https://go.dev/doc/effective_go#defer)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-dlq-handler-literal.md](32-dlq-handler-literal.md) | Next: [34-dns-cache-afterfunc.md](34-dns-cache-afterfunc.md)
