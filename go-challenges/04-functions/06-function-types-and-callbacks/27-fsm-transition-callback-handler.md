# Exercise 27: Finite State Machine State Transitions with Before/After Callbacks

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A state machine's transition table says *which* moves are legal; it has no
opinion on *what else* should happen around a move — an audit log entry, a
metric, a policy check that can veto it. This module keeps the table
minimal and pushes all of that into `BeforeTransition`/`AfterTransition`
callbacks, including the check-then-act race that shows up the moment two
goroutines try to fire the same event from the same state at once.

## What you'll build

```text
fsm/                          independent module: example.com/fsm-transition-callback-handler
  go.mod                       go 1.24
  fsm.go                       type BeforeTransition, type AfterTransition, type Machine: AddTransition, OnBefore, OnAfter, Fire, Current
  cmd/
    demo/
      main.go                   runnable demo: approve, a vetoed ship, final state
  fsm_test.go                   table test of transitions, veto keeps state unchanged, after sees committed state, concurrent Fire race (-race)
```

Files: `fsm.go`, `cmd/demo/main.go`, `fsm_test.go`.
Implement: `type BeforeTransition func(from, event, to State) error`, `type AfterTransition func(from, event, to State)`, `Machine` with `AddTransition(from, event, to)`, `OnBefore(h)`, `OnAfter(h)`, `Fire(event) error`, and `Current() State`; a before-hook error vetoes the transition and leaves the state unchanged, an after-hook only runs once the new state is committed.
Test: `Fire` applies a configured transition and updates `Current`; an unconfigured `(state, event)` pair returns `ErrNoTransition` and leaves the state unchanged; a vetoing before-hook returns `ErrTransitionRejected` and leaves the state unchanged; an after-hook observes the state already committed to `to`; a table of sequential transitions; concurrent `Fire` calls from the same starting state under `-race` let exactly one succeed.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/06-function-types-and-callbacks/27-fsm-transition-callback-handler/cmd/demo
cd go-solutions/04-functions/06-function-types-and-callbacks/27-fsm-transition-callback-handler
go mod edit -go=1.24
```

### Why `Fire` re-checks the state after the before-hooks run

`Fire` looks like it should be a single atomic step — look up the target
state, run the hooks, commit — but under concurrent callers it is really
three separate critical sections, and the middle one, running the
before-hooks, cannot hold the mutex: a hook is caller code that might do
its own locking, logging, or even call back into the machine, and holding
`Machine`'s internal lock across that invites a deadlock. So `Fire` takes
the lock only to read `from` and look up `to`, releases it to run the
hooks, then takes it again to commit. That gap is exactly where two
goroutines racing to fire the same event from the same state could both
decide "yes, this transition is legal" and both try to commit it. The fix
is a second check inside the final locked section: if `m.current` no
longer equals the `from` this call started with, someone else already
moved the machine, and this call fails instead of silently clobbering the
other transition's effect. `TestConcurrentFireFromSameStateOnlyOneWins`
proves exactly one of twenty concurrent callers succeeds.

Create `fsm.go`:

```go
// Package fsm implements a small finite state machine whose transitions
// can be observed and vetoed by Before/After callbacks, without the
// transition table itself knowing anything about logging or validation.
package fsm

import (
	"errors"
	"fmt"
	"sync"
)

// State names a machine state. Event names the trigger that moves between
// states.
type State string
type Event string

// BeforeTransition observes a transition about to happen and may veto it
// by returning a non-nil error; the state is not changed when it does.
type BeforeTransition func(from State, event Event, to State) error

// AfterTransition observes a transition that already happened; the
// machine's current state is already to when it runs.
type AfterTransition func(from State, event Event, to State)

var (
	// ErrNoTransition is returned by Fire for an (state, event) pair with
	// no configured transition.
	ErrNoTransition = errors.New("no transition defined")
	// ErrTransitionRejected wraps the error returned by a Before hook.
	ErrTransitionRejected = errors.New("transition rejected by before-hook")
)

type stateEvent struct {
	state State
	event Event
}

// Machine is a table-driven FSM guarded by a mutex, since Fire may be
// called from multiple goroutines and hooks may be registered at any time.
type Machine struct {
	mu          sync.Mutex
	current     State
	transitions map[stateEvent]State
	before      []BeforeTransition
	after       []AfterTransition
}

// New returns a Machine starting in initial.
func New(initial State) *Machine {
	return &Machine{
		current:     initial,
		transitions: make(map[stateEvent]State),
	}
}

// AddTransition configures event to move the machine from `from` to `to`.
func (m *Machine) AddTransition(from State, event Event, to State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transitions[stateEvent{from, event}] = to
}

// OnBefore registers h to observe (and potentially veto) every transition.
func (m *Machine) OnBefore(h BeforeTransition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.before = append(m.before, h)
}

// OnAfter registers h to observe every transition that committed.
func (m *Machine) OnAfter(h AfterTransition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.after = append(m.after, h)
}

// Current returns the machine's current state.
func (m *Machine) Current() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Fire looks up the transition for the current state and event, runs
// every Before hook (any error vetoes the transition and leaves the state
// unchanged), commits the new state, then runs every After hook.
func (m *Machine) Fire(event Event) error {
	m.mu.Lock()
	from := m.current
	to, ok := m.transitions[stateEvent{from, event}]
	befores := append([]BeforeTransition(nil), m.before...)
	afters := append([]AfterTransition(nil), m.after...)
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("%w: state=%s event=%s", ErrNoTransition, from, event)
	}

	for _, h := range befores {
		if err := h(from, event, to); err != nil {
			return fmt.Errorf("%w: %w", ErrTransitionRejected, err)
		}
	}

	m.mu.Lock()
	// Re-check that no concurrent Fire changed the state out from under
	// us between the lookup above and this commit.
	if m.current != from {
		m.mu.Unlock()
		return fmt.Errorf("%w: state changed concurrently", ErrNoTransition)
	}
	m.current = to
	m.mu.Unlock()

	for _, h := range afters {
		h(from, event, to)
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/fsm-transition-callback-handler"
)

func main() {
	m := fsm.New("pending")
	m.AddTransition("pending", "approve", "approved")
	m.AddTransition("pending", "reject", "rejected")
	m.AddTransition("approved", "ship", "shipped")

	m.OnBefore(func(from fsm.State, event fsm.Event, to fsm.State) error {
		if to == "shipped" {
			return errors.New("no inventory reserved")
		}
		return nil
	})
	m.OnAfter(func(from fsm.State, event fsm.Event, to fsm.State) {
		fmt.Printf("transitioned %s -(%s)-> %s\n", from, event, to)
	})

	if err := m.Fire("approve"); err != nil {
		fmt.Println("error:", err)
	}
	if err := m.Fire("ship"); err != nil {
		fmt.Println("error:", err)
	}
	fmt.Println("final state:", m.Current())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
transitioned pending -(approve)-> approved
error: transition rejected by before-hook: no inventory reserved
final state: approved
```

### Tests

Create `fsm_test.go`:

```go
package fsm

import (
	"errors"
	"sync"
	"testing"
)

func TestFireAppliesConfiguredTransition(t *testing.T) {
	t.Parallel()
	m := New("pending")
	m.AddTransition("pending", "approve", "approved")

	if err := m.Fire("approve"); err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if m.Current() != "approved" {
		t.Fatalf("Current() = %s, want approved", m.Current())
	}
}

func TestFireUnknownTransitionErrors(t *testing.T) {
	t.Parallel()
	m := New("pending")
	err := m.Fire("approve")
	if !errors.Is(err, ErrNoTransition) {
		t.Fatalf("err = %v, want ErrNoTransition", err)
	}
	if m.Current() != "pending" {
		t.Fatalf("Current() = %s, want pending (unchanged)", m.Current())
	}
}

func TestBeforeHookRejectingKeepsStateUnchanged(t *testing.T) {
	t.Parallel()
	m := New("pending")
	m.AddTransition("pending", "approve", "approved")
	m.OnBefore(func(from State, event Event, to State) error {
		return errors.New("policy denies this transition")
	})

	err := m.Fire("approve")
	if !errors.Is(err, ErrTransitionRejected) {
		t.Fatalf("err = %v, want ErrTransitionRejected", err)
	}
	if m.Current() != "pending" {
		t.Fatalf("Current() = %s, want pending (unchanged)", m.Current())
	}
}

func TestAfterHookObservesCommittedState(t *testing.T) {
	t.Parallel()
	m := New("pending")
	m.AddTransition("pending", "approve", "approved")

	var seenCurrent State
	m.OnAfter(func(from State, event Event, to State) {
		seenCurrent = m.Current()
	})

	if err := m.Fire("approve"); err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if seenCurrent != "approved" {
		t.Fatalf("after-hook saw Current() = %s, want approved (already committed)", seenCurrent)
	}
}

func TestMultipleTransitionsTableDriven(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		event   Event
		wantTo  State
		wantErr error
	}{
		{name: "approve from pending", event: "approve", wantTo: "approved"},
		{name: "ship after approve", event: "ship", wantTo: "shipped"},
		{name: "unknown event after shipped", event: "reject", wantErr: ErrNoTransition},
	}

	m := New("pending")
	m.AddTransition("pending", "approve", "approved")
	m.AddTransition("approved", "ship", "shipped")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := m.Fire(tc.event)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Fire: %v", err)
			}
			if m.Current() != tc.wantTo {
				t.Fatalf("Current() = %s, want %s", m.Current(), tc.wantTo)
			}
		})
	}
}

func TestConcurrentFireFromSameStateOnlyOneWins(t *testing.T) {
	t.Parallel()
	m := New("pending")
	m.AddTransition("pending", "approve", "approved")

	var mu sync.Mutex
	successes := 0

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Fire("approve"); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if successes != 1 {
		t.Fatalf("successes = %d, want exactly 1", successes)
	}
	if m.Current() != "approved" {
		t.Fatalf("Current() = %s, want approved", m.Current())
	}
}
```

## Review

The machine is correct when three things hold: `Fire` only ever moves the
state along a configured edge, a vetoing before-hook leaves the state
exactly as it found it, and an after-hook never observes a half-committed
transition. `TestFireUnknownTransitionErrors` and
`TestBeforeHookRejectingKeepsStateUnchanged` both check the "leaves state
unchanged" half, which is easy to get wrong if `Fire` commits before
running the hooks instead of after. `TestAfterHookObservesCommittedState`
checks the other easy mistake — calling the after-hook before the write to
`m.current`, which would make "after" a lie. The concurrency test is the
one that matters most for a shared `Machine`: it does not just check that
`-race` stays quiet (a mutex around `m.current` gets you that for free),
it checks the *check-then-act* correctness — that only one of many
simultaneous callers racing the same edge actually wins, which requires
the second locked re-check inside `Fire`, not just a lock around the read
and a separate lock around the write.

## Resources

- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [Go blog: Share Memory By Communicating](https://go.dev/blog/codelab-share)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-database-transaction-hook-callback.md](26-database-transaction-hook-callback.md) | Next: [28-grpc-interceptor-chain-composition.md](28-grpc-interceptor-chain-composition.md)
