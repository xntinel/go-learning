# Exercise 21: Route Events Through Multi-State Workflows with Conditional Transitions

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A workflow engine drives each job through a small state machine — `pending`
→ `executing` → `success` or `failed` — as it receives heterogeneous events
from the runtime: a start signal, a success signal, an error carrying the
underlying failure, and a timeout signal. Whether a given event is legal at
all depends on the workflow's *current* state as much as on the event's own
type, so `Transition` nests a check on `current` inside each type-switch
case rather than treating the event's shape as the whole story.

## What you'll build

```text
state-machine-event-processor/  independent module: example.com/state-machine-event-processor
  go.mod                         go 1.24
  workflowfsm.go                 Transition(current State, event any) (State, error)
  cmd/
    demo/
      main.go                    drives one workflow through a short event sequence
  workflowfsm_test.go             full (state x event) transition matrix, plus concurrency
```

- Files: `workflowfsm.go`, `cmd/demo/main.go`, `workflowfsm_test.go`.
- Implement: `Transition(current State, event any) (State, error)`,
  type-switching on `StartEvent`, `SuccessEvent`, `ErrorEvent`, and
  `TimeoutEvent`, each validated against the states it is legal from.
- Test: an exhaustive table over every `(state, event)` pair (4 states x 4
  event kinds), an `ErrorEvent` carrying a nil error, an unknown event type,
  and many goroutines replaying the same event sequence concurrently to
  prove `Transition` is a pure function.

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/04-type-switch/21-state-machine-event-processor/cmd/demo
cd go-solutions/03-control-flow/04-type-switch/21-state-machine-event-processor
go mod edit -go=1.24
```

The engine's correctness rests on one property: at most one event kind is
legal from any given state, and both terminal states (`Success`, `Failed`)
accept nothing at all. That is exactly what the exhaustive test matrix
verifies — rather than hand-picking a few representative transitions and
hoping the rest behave, the test drives every one of the 16 `(state,
event)` combinations and asserts either the expected next state or
`ErrInvalidTransition`, so adding a fifth event kind later forces this table
to grow explicitly instead of leaving an untested gap. `TimeoutEvent` is the
one event legal from two different states (`Pending` *and* `Executing`),
modeling that a job can time out either before or during execution, and
both paths land on `Failed`. `ErrorEvent` additionally validates that its
carried `Err` is non-nil — an error event with no error is itself a bug at
the call site, and the state machine is the last place to catch it before
the workflow silently transitions to `Failed` with no diagnostic. Finally,
because `Transition` takes no receiver and mutates no shared state, running
the exact same event sequence from many goroutines against independent
`Pending` starting states must always converge on the same terminal state —
which is what the concurrency test proves.

Create `workflowfsm.go`:

```go
package workflowfsm

import (
	"errors"
	"fmt"
)

// ErrInvalidTransition is the sentinel for an event that is not legal from
// the workflow's current state.
var ErrInvalidTransition = errors.New("invalid state transition")

// State is one node of the workflow's state machine.
type State int

const (
	Pending State = iota
	Executing
	Success
	Failed
)

func (s State) String() string {
	switch s {
	case Pending:
		return "pending"
	case Executing:
		return "executing"
	case Success:
		return "success"
	case Failed:
		return "failed"
	default:
		return "unknown"
	}
}

// StartEvent begins execution of a pending workflow.
type StartEvent struct{}

// SuccessEvent reports that the executing step finished cleanly.
type SuccessEvent struct{}

// ErrorEvent reports that the executing step failed with Err.
type ErrorEvent struct{ Err error }

// TimeoutEvent reports that a step did not complete within its deadline.
type TimeoutEvent struct{}

// Transition applies event to current and returns the resulting state, or
// ErrInvalidTransition if the event does not make sense from that state. The
// legality of an event is state-dependent — a StartEvent is only legal from
// Pending, a SuccessEvent only from Executing, and both Success and Failed
// are terminal, accepting nothing further — so the switch on event type
// nests a switch on current inside each case rather than validating shape
// alone.
func Transition(current State, event any) (State, error) {
	switch e := event.(type) {
	case StartEvent:
		if current != Pending {
			return current, fmt.Errorf("%w: start from %s", ErrInvalidTransition, current)
		}
		return Executing, nil
	case SuccessEvent:
		if current != Executing {
			return current, fmt.Errorf("%w: success from %s", ErrInvalidTransition, current)
		}
		return Success, nil
	case ErrorEvent:
		if current != Executing {
			return current, fmt.Errorf("%w: error from %s", ErrInvalidTransition, current)
		}
		if e.Err == nil {
			return current, fmt.Errorf("%w: error event carries nil error", ErrInvalidTransition)
		}
		return Failed, nil
	case TimeoutEvent:
		if current != Pending && current != Executing {
			return current, fmt.Errorf("%w: timeout from %s", ErrInvalidTransition, current)
		}
		return Failed, nil
	default:
		return current, fmt.Errorf("%w: unknown event %T", ErrInvalidTransition, event)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/state-machine-event-processor"
)

func main() {
	state := workflowfsm.Pending
	events := []any{
		workflowfsm.StartEvent{},
		workflowfsm.ErrorEvent{Err: errors.New("downstream unavailable")},
		workflowfsm.StartEvent{}, // illegal: workflow already terminal
	}
	for _, ev := range events {
		next, err := workflowfsm.Transition(state, ev)
		if err != nil {
			fmt.Printf("%T rejected from %s: %v\n", ev, state, err)
			continue
		}
		fmt.Printf("%s -> %s on %T\n", state, next, ev)
		state = next
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
pending -> executing on workflowfsm.StartEvent
executing -> failed on workflowfsm.ErrorEvent
workflowfsm.StartEvent rejected from failed: invalid state transition: start from failed
```

### Tests

The matrix test builds a `map[State]map[int]State` of only the *legal*
transitions and treats every `(state, event)` pair absent from that map as
an expected rejection, which is what makes the table exhaustive without
having to spell out all twelve illegal combinations by hand.

Create `workflowfsm_test.go`:

```go
package workflowfsm

import (
	"errors"
	"fmt"
	"testing"
)

// TestTransitionMatrix exhaustively covers every (state, event) pair so that
// adding a new state or event later forces this table to be extended
// explicitly, instead of a gap being discovered in production.
func TestTransitionMatrix(t *testing.T) {
	t.Parallel()

	states := []State{Pending, Executing, Success, Failed}
	events := []any{StartEvent{}, SuccessEvent{}, ErrorEvent{Err: errBoom}, TimeoutEvent{}}

	wantState := map[State]map[int]State{
		Pending:   {0: Executing, 3: Failed},
		Executing: {1: Success, 2: Failed, 3: Failed},
	}

	for _, from := range states {
		for i, ev := range events {
			from, ev, i := from, ev, i
			t.Run(fmt.Sprintf("%s/%T", from, ev), func(t *testing.T) {
				t.Parallel()
				got, err := Transition(from, ev)
				want, legal := wantState[from][i]
				if !legal {
					if !errors.Is(err, ErrInvalidTransition) {
						t.Fatalf("Transition(%s, %T) err = %v, want ErrInvalidTransition", from, ev, err)
					}
					if got != from {
						t.Fatalf("Transition(%s, %T) = %s on rejection, want unchanged %s", from, ev, got, from)
					}
					return
				}
				if err != nil {
					t.Fatalf("Transition(%s, %T) unexpected error: %v", from, ev, err)
				}
				if got != want {
					t.Fatalf("Transition(%s, %T) = %s, want %s", from, ev, got, want)
				}
			})
		}
	}
}

var errBoom = errors.New("boom")

func TestErrorEventRequiresNonNilErr(t *testing.T) {
	t.Parallel()
	_, err := Transition(Executing, ErrorEvent{Err: nil})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("ErrorEvent with nil Err: got %v, want ErrInvalidTransition", err)
	}
}

func TestUnknownEventType(t *testing.T) {
	t.Parallel()
	_, err := Transition(Pending, "not-an-event")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unknown event: got %v, want ErrInvalidTransition", err)
	}
}

// TestFullLifecycleIsDeterministicUnderConcurrentReplay drives the same
// event sequence through independent state machines from many goroutines to
// prove Transition is a pure function with no shared mutable state: every
// goroutine must land on the same terminal state.
func TestFullLifecycleIsDeterministicUnderConcurrentReplay(t *testing.T) {
	t.Parallel()
	sequence := []any{StartEvent{}, ErrorEvent{Err: errBoom}}

	const workers = 50
	results := make(chan State, workers)
	for i := 0; i < workers; i++ {
		go func() {
			state := Pending
			for _, ev := range sequence {
				var err error
				state, err = Transition(state, ev)
				if err != nil {
					results <- State(-1)
					return
				}
			}
			results <- state
		}()
	}
	for i := 0; i < workers; i++ {
		if got := <-results; got != Failed {
			t.Fatalf("worker %d landed on %s, want %s", i, got, Failed)
		}
	}
}
```

Verify: `go test -race -count=1 ./...`

## Review

The state machine is correct because every event's legality is checked
against `current` before any state change is computed, and the rejection
path always returns the *unchanged* `current` state alongside the error —
callers can safely retry or route on the returned state without first
checking whether the transition succeeded. The exhaustive matrix test is
the load-bearing part of this exercise: a narrower table that only checks
a handful of "obviously legal" and "obviously illegal" transitions would
have let a bug like accepting `SuccessEvent` from `Pending` slip through
unnoticed, since nothing about that specific pair would have seemed worth
testing in isolation. The concurrency test is not testing anything
`workflowfsm.go` does with goroutines or locks — it has none — it is
testing the *absence* of hidden shared state, which is precisely the
property that lets a caller safely run many workflows through the same
`Transition` function in parallel.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [AWS Step Functions: states language](https://docs.aws.amazon.com/step-functions/latest/dg/concepts-states.html)
- [Go: Data Race Detector](https://go.dev/doc/articles/race_detector)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-tenant-quota-enforcer.md](20-tenant-quota-enforcer.md) | Next: [22-distributed-consensus-handler.md](22-distributed-consensus-handler.md)
