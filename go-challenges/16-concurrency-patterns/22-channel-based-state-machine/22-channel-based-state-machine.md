# 22. Channel-Based State Machine

Traditional state machines protect their current-state variable with a mutex. Channel-based
state machines eliminate that mutex entirely: a single goroutine runs the state loop, so
transitions are naturally serialized. Each state is a function that blocks on an event
channel and returns the next state function. This lesson builds an order lifecycle machine
— Pending, Confirmed, Shipped, Delivered, and Cancelled — using the `StateFn` pattern
Rob Pike introduced in his 2011 lexer talk.

```text
statemachine/
  go.mod
  internal/statemachine/statemachine.go
  internal/statemachine/statemachine_test.go
  cmd/demo/main.go
```

## Concepts

### The StateFn Pattern

```go
type StateFn func(ctx context.Context, events <-chan Event) StateFn
```

A state function receives a context and a read-only event channel. It blocks until an event
arrives (or the context is cancelled), processes the event, and returns the next state
function. Returning `nil` terminates the machine. The driver loop is trivial:

```go
for state := initial; state != nil; {
    state = state(ctx, events)
}
```

There is no `switch` over an enum, no shared variable, and no mutex. The goroutine running
the loop is the only writer of the "current state" — it's implicit in the call stack.

### No Shared Mutable State

A mutex-based machine stores `currentState State` and requires `mu.Lock()` around every
read and write. Under high event rate, the mutex becomes a bottleneck and the lock scope
is easy to get wrong (forgetting to unlock on an early return, holding the lock during
I/O). The channel-based approach has no shared state to protect. Events are delivered one
at a time through the channel; the goroutine processes them sequentially.

### Serialized Transitions

Because the state loop runs in a single goroutine, transitions cannot interleave. If two
callers send events concurrently, the channel buffers them and the loop processes them in
order. There is no race between reading the current state and writing the next one.

### Invalid Event Rejection

Each state function is responsible for the events it accepts. If it receives an unexpected
event, it logs the rejection and returns itself — staying in the same state. There is no
centralized dispatcher that must know all valid transitions for all states. Adding a new
state requires only a new function, not changes to a central switch.

### Context Cancellation

Every state function selects on `ctx.Done()` alongside the events channel. When the context
is cancelled, the function returns `nil`, which terminates the loop. This is the correct way
to stop a machine that is waiting for an event that never arrives.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/22-channel-based-state-machine/22-channel-based-state-machine/internal/statemachine go-solutions/16-concurrency-patterns/22-channel-based-state-machine/22-channel-based-state-machine/cmd/demo
cd go-solutions/16-concurrency-patterns/22-channel-based-state-machine/22-channel-based-state-machine
```

### Exercise 1: Types and State Functions

Create `internal/statemachine/statemachine.go`:

```go
package statemachine

import (
	"context"
	"errors"
	"fmt"
)

// EventType names a transition trigger.
type EventType string

const (
	EventConfirm EventType = "Confirm"
	EventShip    EventType = "Ship"
	EventDeliver EventType = "Deliver"
	EventCancel  EventType = "Cancel"
)

// State names a position in the lifecycle.
type State string

const (
	StatePending   State = "Pending"
	StateConfirmed State = "Confirmed"
	StateShipped   State = "Shipped"
	StateDelivered State = "Delivered"
	StateCancelled State = "Cancelled"
)

// Event carries a transition trigger.
type Event struct {
	Type EventType
}

// ErrInvalidTransition is returned by Run when an event is rejected in a state.
var ErrInvalidTransition = errors.New("statemachine: invalid transition")

// Transition records a single state change.
type Transition struct {
	From  State
	Event EventType
	To    State
}

// StateFn is a function that handles events for one state and returns the next StateFn.
// Returning nil terminates the machine.
type StateFn func(ctx context.Context, events <-chan Event) StateFn

// Machine runs an order lifecycle state machine.
type Machine struct {
	History []Transition
	Final   State
}

// Run starts the machine in the initial state and processes events until the
// machine reaches a terminal state or ctx is cancelled.
func (m *Machine) Run(ctx context.Context, events <-chan Event) {
	state := m.pending
	for state != nil {
		state = state(ctx, events)
	}
}

func (m *Machine) record(from, to State, ev EventType) {
	m.History = append(m.History, Transition{From: from, Event: ev, To: to})
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
			fmt.Printf("statemachine: Pending: rejected event %q\n", ev.Type)
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
			fmt.Printf("statemachine: Confirmed: rejected event %q\n", ev.Type)
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
			fmt.Printf("statemachine: Shipped: rejected event %q\n", ev.Type)
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

### Exercise 2: Table-Driven Tests

Create `internal/statemachine/statemachine_test.go`:

```go
package statemachine_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"example.com/statemachine/internal/statemachine"
)

func sendEvents(events chan<- statemachine.Event, types ...statemachine.EventType) {
	for _, t := range types {
		events <- statemachine.Event{Type: t}
	}
}

func TestHappyPath(t *testing.T) {
	t.Parallel()

	events := make(chan statemachine.Event, 4)
	sendEvents(events, statemachine.EventConfirm, statemachine.EventShip, statemachine.EventDeliver)
	close(events)

	var m statemachine.Machine
	m.Run(context.Background(), events)

	if m.Final != statemachine.StateDelivered {
		t.Errorf("Final = %q, want Delivered", m.Final)
	}
	if len(m.History) != 3 {
		t.Errorf("len(History) = %d, want 3; history: %v", len(m.History), m.History)
	}
}

func TestCancelFromPending(t *testing.T) {
	t.Parallel()

	events := make(chan statemachine.Event, 1)
	sendEvents(events, statemachine.EventCancel)
	close(events)

	var m statemachine.Machine
	m.Run(context.Background(), events)

	if m.Final != statemachine.StateCancelled {
		t.Errorf("Final = %q, want Cancelled", m.Final)
	}
}

func TestCancelFromShipped(t *testing.T) {
	t.Parallel()

	events := make(chan statemachine.Event, 3)
	sendEvents(events, statemachine.EventConfirm, statemachine.EventShip, statemachine.EventCancel)
	close(events)

	var m statemachine.Machine
	m.Run(context.Background(), events)

	if m.Final != statemachine.StateCancelled {
		t.Errorf("Final = %q, want Cancelled", m.Final)
	}
}

func TestInvalidEventRejected(t *testing.T) {
	t.Parallel()

	// Ship is invalid in Pending; the machine should stay in Pending then accept Cancel.
	events := make(chan statemachine.Event, 2)
	sendEvents(events, statemachine.EventShip, statemachine.EventCancel)
	close(events)

	var m statemachine.Machine
	m.Run(context.Background(), events)

	// After rejecting Ship, the machine is still in Pending and should transition on Cancel.
	if m.Final != statemachine.StateCancelled {
		t.Errorf("Final = %q, want Cancelled", m.Final)
	}
	// The rejected event must not appear in history.
	for _, tr := range m.History {
		if tr.Event == statemachine.EventShip {
			t.Errorf("Ship appeared in history but should have been rejected: %v", m.History)
		}
	}
}

func TestContextCancellationStopsMachine(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan statemachine.Event) // no events; machine blocks

	done := make(chan struct{})
	var m statemachine.Machine
	go func() {
		m.Run(ctx, events)
		close(done)
	}()

	cancel()
	<-done

	// Machine stopped in Pending without any transitions.
	if len(m.History) != 0 {
		t.Errorf("History = %v, want empty", m.History)
	}
}

// Ensure ErrInvalidTransition is exported and usable (compile-time check).
var _ = errors.New(statemachine.ErrInvalidTransition.Error())

func ExampleMachine_Run() {
	events := make(chan statemachine.Event, 3)
	events <- statemachine.Event{Type: statemachine.EventConfirm}
	events <- statemachine.Event{Type: statemachine.EventShip}
	events <- statemachine.Event{Type: statemachine.EventDeliver}
	close(events)

	var m statemachine.Machine
	m.Run(context.Background(), events)
	fmt.Println(m.Final)
	// Output:
	// Delivered
}
```

### Exercise 3: Demo Binary

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/statemachine/internal/statemachine"
)

func main() {
	events := make(chan statemachine.Event, 5)

	// Happy path: Confirm -> Ship -> Deliver
	events <- statemachine.Event{Type: statemachine.EventConfirm}
	events <- statemachine.Event{Type: statemachine.EventShip}
	events <- statemachine.Event{Type: statemachine.EventDeliver}
	close(events)

	var m statemachine.Machine
	m.Run(context.Background(), events)

	fmt.Printf("Final state: %s\n", m.Final)
	fmt.Println("Transition history:")
	for _, tr := range m.History {
		fmt.Printf("  %s -[%s]-> %s\n", tr.From, tr.Event, tr.To)
	}
}
```

## Common Mistakes

### Protecting currentState With a Mutex

Wrong: `type Machine struct { mu sync.Mutex; current State }` where every event read and
state write holds the mutex.

What happens: callers contend on the mutex; the lock scope must be carefully maintained
across complex transition logic; error-prone early returns can leave the mutex locked.

Fix: run the entire state loop in one goroutine. The goroutine owns `current` implicitly —
it is the active `StateFn` on the call stack. No mutex is needed.

### Returning a New Closure Instead of the Named Method

Wrong: `return func(ctx context.Context, events <-chan Event) StateFn { ... }` inline for
each branch of a large switch.

What happens: each call allocates a new closure; the code becomes hard to read and
impossible to compare by identity (e.g., for testing which state you are in).

Fix: define each state as a method on `*Machine`. Methods are addressable and readable;
`m.pending` is the same function value every time.

### Not Handling Channel Close in State Functions

Wrong: `case ev := <-events:` without checking `ok`.

What happens: if the events channel is closed, the receive returns the zero value
indefinitely, and the machine loops forever processing empty events as if they were real.

Fix: `case ev, ok := <-events: if !ok { return nil }`. A closed channel signals that no
more events will arrive; returning nil terminates the machine cleanly.

### Sending Events Without a Buffer When Running the Machine Synchronously

Wrong: `events := make(chan statemachine.Event)` (unbuffered) and then sending before
`m.Run` is called in a goroutine.

What happens: the send blocks forever because no goroutine is receiving.

Fix: use a buffered channel sized to at least the number of events to send before `Run`
starts, or start `Run` in a goroutine first, then send.

## Verification

From `~/go-exercises/statemachine`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector confirms that the single-goroutine state loop is
truly free of concurrent access to `m.History` and `m.Final`.

## Summary

- `StateFn` is a function type that returns the next `StateFn`; a nil return terminates the loop.
- One goroutine runs the loop; it serializes all transitions without a mutex.
- Each state function handles its own valid events and rejects unknown ones by returning itself.
- Context cancellation is handled uniformly by selecting on `ctx.Done()` in every state function.
- Closing the events channel is a clean shutdown signal; each state function checks the `ok` flag.

## What's Next

Next: [Request Coalescing with Singleflight](../23-request-coalescing-singleflight/23-request-coalescing-singleflight.md).

## Resources

- [Lexical Scanning in Go (Rob Pike, 2011)](https://go.dev/talks/2011/lex.slide)
- [Go Concurrency Patterns (Rob Pike, 2012)](https://go.dev/talks/2012/concurrency.slide)
- [context package](https://pkg.go.dev/context)
- [Communicating Sequential Processes (Hoare, 1978)](https://dl.acm.org/doi/10.1145/359576.359585)
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share)
