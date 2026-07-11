# Exercise 1: StateFn Order Lifecycle

The cleanest way to make a state machine concurrency-safe is to delete the shared state. In the `StateFn` pattern each state is a function that the loop calls, so the "current state" is just which function is on the call stack — owned implicitly by the one goroutine running the loop. This exercise builds an order lifecycle (Pending, Confirmed, Shipped, Delivered, Cancelled) on that pattern, with no mutex and no `current` field.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
lifecycle.go         State, EventType, Event, Transition, Rejection, StateFn, Machine, Run
cmd/
  demo/
    main.go          drive an order through Confirm -> Ship -> Deliver, reject one illegal event
lifecycle_test.go    happy path, cancel branches, illegal-event rejection, context cancellation
```

- Files: `lifecycle.go`, `cmd/demo/main.go`, `lifecycle_test.go`.
- Implement: `Machine` with `Run(ctx, events)`, one method per state returning the next `StateFn`, and `History`/`Rejected`/`Final` fields.
- Test: the happy path reaches Delivered with three transitions, Cancel works from Pending and Shipped, an illegal event is recorded in `Rejected` and never in `History`, and a cancelled context stops a waiting machine.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p order-lifecycle/cmd/demo && cd order-lifecycle
go mod init example.com/order-lifecycle
```

### Why state-as-a-function, and what it buys

A mutex-guarded machine stores `current State` and locks it on every read and write. The lock has to cover the whole read-decide-write step or two callers both read `Pending` and both advance; the scope leaks on early returns; and the single lock is the point everything contends on. The `StateFn` pattern removes the variable. The state type is recursive — a state function returns the next state function:

```text
type StateFn func(ctx context.Context, events <-chan Event) StateFn
```

The loop is three lines and the only writer of "where we are" is the goroutine running it:

```text
for state := m.pending; state != nil; {
	state = state(ctx, events)
}
```

Each state function blocks on the events channel, handles exactly the events it allows, and returns the next state's function. An illegal event is not a crash and not a silent advance: the function records it in `Rejected` and returns *itself*, so the machine stays put and keeps going. Returning `nil` ends the machine, which is how terminal states (Delivered, Cancelled) stop the loop. Two guards make the loop robust against a misbehaving sender: every state selects on `ctx.Done()` so a cancelled context unblocks a machine waiting for an event that never arrives, and every receive uses the comma-ok form so a closed channel ends the machine cleanly instead of spinning on the zero value forever.

There is no `ErrInvalidTransition` sentinel and nothing returned from `Run`: rejections are data on the machine (`m.Rejected`), which is both honest about what the code does and directly testable. Recording each accepted move in `m.History` gives an audit trail; `m.Final` always holds the state the machine settled in.

Create `lifecycle.go`:

```go
package lifecycle

import "context"

// EventType names a transition trigger.
type EventType string

const (
	EventConfirm EventType = "Confirm"
	EventShip    EventType = "Ship"
	EventDeliver EventType = "Deliver"
	EventCancel  EventType = "Cancel"
)

// State names a position in the order lifecycle.
type State string

const (
	StatePending   State = "Pending"
	StateConfirmed State = "Confirmed"
	StateShipped   State = "Shipped"
	StateDelivered State = "Delivered"
	StateCancelled State = "Cancelled"
)

// Event carries a transition trigger into the machine.
type Event struct {
	Type EventType
}

// Transition records a single accepted state change.
type Transition struct {
	From  State
	Event EventType
	To    State
}

// Rejection records an event that was not valid in the state it arrived in.
type Rejection struct {
	State State
	Event EventType
}

// StateFn handles events for one state and returns the next StateFn.
// Returning nil terminates the machine.
type StateFn func(ctx context.Context, events <-chan Event) StateFn

// Machine runs an order lifecycle. It holds no shared mutable state: every
// field below is written only by the single goroutine running Run.
type Machine struct {
	History  []Transition
	Rejected []Rejection
	Final    State
}

// Run starts the machine in the Pending state and processes events until a
// terminal state is reached, the events channel is closed, or ctx is cancelled.
func (m *Machine) Run(ctx context.Context, events <-chan Event) {
	for state := m.pending; state != nil; {
		state = state(ctx, events)
	}
}

func (m *Machine) record(from, to State, ev EventType) {
	m.History = append(m.History, Transition{From: from, Event: ev, To: to})
}

func (m *Machine) reject(state State, ev EventType) {
	m.Rejected = append(m.Rejected, Rejection{State: state, Event: ev})
}

func (m *Machine) pending(ctx context.Context, events <-chan Event) StateFn {
	m.Final = StatePending
	select {
	case <-ctx.Done():
		return nil
	case ev, ok := <-events:
		if !ok {
			return nil
		}
		switch ev.Type {
		case EventConfirm:
			m.record(StatePending, StateConfirmed, ev.Type)
			return m.confirmed
		case EventCancel:
			m.record(StatePending, StateCancelled, ev.Type)
			return m.cancelled
		default:
			m.reject(StatePending, ev.Type)
			return m.pending
		}
	}
}

func (m *Machine) confirmed(ctx context.Context, events <-chan Event) StateFn {
	m.Final = StateConfirmed
	select {
	case <-ctx.Done():
		return nil
	case ev, ok := <-events:
		if !ok {
			return nil
		}
		switch ev.Type {
		case EventShip:
			m.record(StateConfirmed, StateShipped, ev.Type)
			return m.shipped
		case EventCancel:
			m.record(StateConfirmed, StateCancelled, ev.Type)
			return m.cancelled
		default:
			m.reject(StateConfirmed, ev.Type)
			return m.confirmed
		}
	}
}

func (m *Machine) shipped(ctx context.Context, events <-chan Event) StateFn {
	m.Final = StateShipped
	select {
	case <-ctx.Done():
		return nil
	case ev, ok := <-events:
		if !ok {
			return nil
		}
		switch ev.Type {
		case EventDeliver:
			m.record(StateShipped, StateDelivered, ev.Type)
			return m.delivered
		case EventCancel:
			m.record(StateShipped, StateCancelled, ev.Type)
			return m.cancelled
		default:
			m.reject(StateShipped, ev.Type)
			return m.shipped
		}
	}
}

func (m *Machine) delivered(ctx context.Context, events <-chan Event) StateFn {
	m.Final = StateDelivered
	return nil // terminal state
}

func (m *Machine) cancelled(ctx context.Context, events <-chan Event) StateFn {
	m.Final = StateCancelled
	return nil // terminal state
}
```

The terminal states `delivered` and `cancelled` ignore their parameters and return `nil` immediately; they exist so the transition into them is uniform with every other state. Notice that `m.Final` is set at the top of each state function, so the machine always reports the state it is currently sitting in even if it blocks there waiting for the next event.

### The runnable demo

The demo drives one order through the happy path but slips an illegal `Deliver` in while the order is only Confirmed, to show that the machine rejects it (records it in `Rejected`) and stays in Confirmed rather than jumping ahead. The events are buffered and the channel is closed before `Run`, so the machine drains the buffer and stops on its own.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/order-lifecycle"
)

func main() {
	events := make(chan lifecycle.Event, 4)
	events <- lifecycle.Event{Type: lifecycle.EventConfirm}
	events <- lifecycle.Event{Type: lifecycle.EventDeliver} // illegal while Confirmed: rejected
	events <- lifecycle.Event{Type: lifecycle.EventShip}
	events <- lifecycle.Event{Type: lifecycle.EventDeliver}
	close(events)

	var m lifecycle.Machine
	m.Run(context.Background(), events)

	fmt.Printf("Final state: %s\n", m.Final)
	fmt.Println("Transitions:")
	for _, tr := range m.History {
		fmt.Printf("  %s -[%s]-> %s\n", tr.From, tr.Event, tr.To)
	}
	fmt.Println("Rejected events:")
	for _, r := range m.Rejected {
		fmt.Printf("  %s rejected in %s\n", r.Event, r.State)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Final state: Delivered
Transitions:
  Pending -[Confirm]-> Confirmed
  Confirmed -[Ship]-> Shipped
  Shipped -[Deliver]-> Delivered
Rejected events:
  Deliver rejected in Confirmed
```

### Tests

The tests pin the lifecycle's contract. The happy path must reach Delivered in exactly three transitions. Cancel must work from Pending and from Shipped. An illegal event must land in `Rejected` and never in `History`, and the machine must still accept a later legal event. A cancelled context must stop a machine that is blocked waiting for an event, leaving its history empty. Running them under `-race` is the real proof that the single-goroutine loop never shares `History`, `Rejected`, or `Final`.

Create `lifecycle_test.go`:

```go
package lifecycle_test

import (
	"context"
	"fmt"
	"testing"

	"example.com/order-lifecycle"
)

func sendAll(events chan<- lifecycle.Event, types ...lifecycle.EventType) {
	for _, t := range types {
		events <- lifecycle.Event{Type: t}
	}
}

func TestHappyPath(t *testing.T) {
	t.Parallel()

	events := make(chan lifecycle.Event, 3)
	sendAll(events, lifecycle.EventConfirm, lifecycle.EventShip, lifecycle.EventDeliver)
	close(events)

	var m lifecycle.Machine
	m.Run(context.Background(), events)

	if m.Final != lifecycle.StateDelivered {
		t.Errorf("Final = %q, want Delivered", m.Final)
	}
	if len(m.History) != 3 {
		t.Errorf("len(History) = %d, want 3; history: %v", len(m.History), m.History)
	}
	if len(m.Rejected) != 0 {
		t.Errorf("Rejected = %v, want empty", m.Rejected)
	}
}

func TestCancelFromPending(t *testing.T) {
	t.Parallel()

	events := make(chan lifecycle.Event, 1)
	sendAll(events, lifecycle.EventCancel)
	close(events)

	var m lifecycle.Machine
	m.Run(context.Background(), events)

	if m.Final != lifecycle.StateCancelled {
		t.Errorf("Final = %q, want Cancelled", m.Final)
	}
}

func TestCancelFromShipped(t *testing.T) {
	t.Parallel()

	events := make(chan lifecycle.Event, 3)
	sendAll(events, lifecycle.EventConfirm, lifecycle.EventShip, lifecycle.EventCancel)
	close(events)

	var m lifecycle.Machine
	m.Run(context.Background(), events)

	if m.Final != lifecycle.StateCancelled {
		t.Errorf("Final = %q, want Cancelled", m.Final)
	}
}

func TestIllegalEventRejected(t *testing.T) {
	t.Parallel()

	// Ship is illegal in Pending: it must be rejected, then Cancel accepted.
	events := make(chan lifecycle.Event, 2)
	sendAll(events, lifecycle.EventShip, lifecycle.EventCancel)
	close(events)

	var m lifecycle.Machine
	m.Run(context.Background(), events)

	if m.Final != lifecycle.StateCancelled {
		t.Errorf("Final = %q, want Cancelled", m.Final)
	}
	if len(m.Rejected) != 1 || m.Rejected[0].Event != lifecycle.EventShip || m.Rejected[0].State != lifecycle.StatePending {
		t.Errorf("Rejected = %v, want one Ship rejected in Pending", m.Rejected)
	}
	for _, tr := range m.History {
		if tr.Event == lifecycle.EventShip {
			t.Errorf("Ship appeared in history but should have been rejected: %v", m.History)
		}
	}
}

func TestContextCancellationStopsMachine(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan lifecycle.Event) // unbuffered, no events: the machine blocks

	done := make(chan struct{})
	var m lifecycle.Machine
	go func() {
		m.Run(ctx, events)
		close(done)
	}()

	cancel()
	<-done

	if len(m.History) != 0 {
		t.Errorf("History = %v, want empty", m.History)
	}
	if m.Final != lifecycle.StatePending {
		t.Errorf("Final = %q, want Pending", m.Final)
	}
}

func ExampleMachine_Run() {
	events := make(chan lifecycle.Event, 3)
	events <- lifecycle.Event{Type: lifecycle.EventConfirm}
	events <- lifecycle.Event{Type: lifecycle.EventShip}
	events <- lifecycle.Event{Type: lifecycle.EventDeliver}
	close(events)

	var m lifecycle.Machine
	m.Run(context.Background(), events)
	fmt.Println(m.Final)
	// Output:
	// Delivered
}
```

## Review

The machine is correct when the only writer of `History`, `Rejected`, and `Final` is the goroutine inside `Run`, which is what makes the `-race` run clean: the state is the function on the call stack, not a shared field. Confirm that an illegal event records a `Rejection` and returns the same state function — the machine stays put and the rejected event never reaches `History` — and that a legal event after a rejection still advances. Confirm that closing the events channel ends the machine (the comma-ok receive returns `nil`) and that a cancelled context ends a machine blocked on an empty channel, both without a panic.

Common mistakes for this pattern. The first is guarding a `current State` field with a mutex; the `StateFn` loop has no such field to guard, so do not add one. The second is dropping the comma-ok check on the receive: a closed channel then yields the zero `Event` forever and the loop spins. The third is treating an illegal event as fatal — panicking or returning an error from `Run`; an illegal event is expected input, so the state function records it and returns itself. The fourth is sending several events into an unbuffered channel from the same goroutine before `Run` is started elsewhere, which deadlocks on the first send; buffer the channel or start `Run` in its own goroutine first.

## Resources

- [Lexical Scanning in Go (Rob Pike, 2011)](https://go.dev/talks/2011/lex.slide) — the talk that introduced the `stateFn`-returns-`stateFn` pattern this exercise is built on.
- [Go Concurrency Patterns (Rob Pike, 2012)](https://go.dev/talks/2012/concurrency.slide) — channels as the serialisation primitive behind a single-goroutine loop.
- [`context` package](https://pkg.go.dev/context) — `Done()` and cancellation, used to stop a machine waiting on an event that never arrives.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-session-lifecycle-owning-goroutine.md](02-session-lifecycle-owning-goroutine.md)
