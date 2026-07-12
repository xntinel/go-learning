# Exercise 1: The Closed-Twice State Machine — Read the Failure, Fix the Guard

A connection lifecycle state machine shipped with a bug that passed review: a
closed connection can be "closed" again, and the code accepts the second `Close()`
as a valid no-op. You will run the bug-reproduction test, read the `<nil>`
failure, form a hypothesis from the transition table, and fix it with a same-state
guard.

## What you'll build

```text
connstate/                        module example.com/connstate
  go.mod
  internal/conn/
    state.go                      State, Connection, allowed table, Transition; typed errors
    state_test.go                 4 lifecycle/rejection tests + TestRejectsUnknownState + Example
  cmd/connstate/
    main.go                       CLI harness: drives connect|authenticate|close from os.Args
```

- Files: `internal/conn/state.go`, `internal/conn/state_test.go`, `cmd/connstate/main.go`.
- Implement: a `Connection` with a `map[State]map[State]bool` transition table, `Transition(to)` guarded by a `from == to` short-circuit, and `ErrInvalidTransition` / `ErrUnknownState` sentinels wrapped with `%w`.
- Test: the four lifecycle tests, `TestRejectsUnknownState` for an out-of-range source state, and an `Example`.
- Verify: `go test -count=1 -race ./...` and `go build ./...`.

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/01-connection-state-machine-closed-twice/internal/conn go-solutions/03-control-flow/10-control-flow-debugging-challenge/01-connection-state-machine-closed-twice/cmd/connstate
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/01-connection-state-machine-closed-twice
```

### The artifact and the planted bug

The state machine enforces a strict lifecycle: `New → Connected → Authenticated →
Closed`. Every transition is a precondition, encoded once in a
`map[State]map[State]bool` table so the rest of the system never defends against
invalid states. The version that shipped looks correct — until you notice the
table carries `StateClosed: {StateClosed: true}` and `Transition` has no guard
above the table lookup:

```go
var allowed = map[State]map[State]bool{
	StateNew:           {StateConnected: true},
	StateConnected:     {StateAuthenticated: true, StateClosed: true},
	StateAuthenticated: {StateClosed: true},
	StateClosed:        {StateClosed: true}, // the table permits closed -> closed
}

func (c *Connection) Transition(to State) error {
	from := c.State
	allowedTargets, ok := allowed[from]
	if !ok {
		return fmt.Errorf("%w: from %s", ErrUnknownState, from)
	}
	if !allowedTargets[to] { // for closed -> closed this lookup is true
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	c.State = to
	return nil
}
```

For a second `Close()` on an already-closed connection, `from` is `StateClosed`,
`to` is `StateClosed`, `allowed[StateClosed]` is `{StateClosed: true}`, and the
lookup returns `true`. The function accepts the transition and returns `nil`. From
the caller's view, closing a closed connection is a successful no-op. That is the
bug, and it is exactly the kind that survives review because the happy path is
untouched.

Run the reproduction test and read the failure:

```bash
go test -count=1 -race -run TestClosedToClosedIsRejected -v ./internal/conn
```

```text
=== RUN   TestClosedToClosedIsRejected
    state_test.go:47: Close on closed = <nil>, want ErrInvalidTransition
--- FAIL: TestClosedToClosedIsRejected (0.00s)
FAIL
```

`Close on closed = <nil>` is the data: the second `Close()` returned no error. The
hypothesis is immediate — the table lets `Closed → Closed` through and there is no
guard above it. The fix is a `from == to` short-circuit at the top of
`Transition`, which rejects same-state transitions for *every* terminal state, not
just `StateClosed`, so a future `StateTerminated` needs no new table edit.

Create `internal/conn/state.go` with the guard in place:

```go
package conn

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidTransition = errors.New("invalid state transition")
	ErrUnknownState      = errors.New("unknown state")
)

// State is a point in the connection lifecycle.
type State uint8

const (
	StateNew State = iota
	StateConnected
	StateAuthenticated
	StateClosed
)

func (s State) String() string {
	switch s {
	case StateNew:
		return "new"
	case StateConnected:
		return "connected"
	case StateAuthenticated:
		return "authenticated"
	case StateClosed:
		return "closed"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(s))
	}
}

// Connection carries the current lifecycle state.
type Connection struct {
	State State
}

// allowed is the transition table: allowed[from][to] reports whether from -> to
// is a legal edge. It is the single source of truth for the lifecycle.
var allowed = map[State]map[State]bool{
	StateNew: {
		StateConnected: true,
	},
	StateConnected: {
		StateAuthenticated: true,
		StateClosed:        true,
	},
	StateAuthenticated: {
		StateClosed: true,
	},
	StateClosed: {
		StateClosed: true,
	},
}

// New returns a connection in the initial state.
func New() *Connection {
	return &Connection{State: StateNew}
}

// Transition moves the connection to state to. A same-state transition is
// rejected outright (the guard), an unknown source state wraps ErrUnknownState,
// and any edge absent from the table wraps ErrInvalidTransition.
func (c *Connection) Transition(to State) error {
	from := c.State
	if from == to {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	allowedTargets, ok := allowed[from]
	if !ok {
		return fmt.Errorf("%w: from %s", ErrUnknownState, from)
	}
	if !allowedTargets[to] {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	c.State = to
	return nil
}

func (c *Connection) Connect() error      { return c.Transition(StateConnected) }
func (c *Connection) Authenticate() error { return c.Transition(StateAuthenticated) }
func (c *Connection) Close() error        { return c.Transition(StateClosed) }
```

The `StateClosed: {StateClosed: true}` entry is now dead code — the guard fires
before the table is consulted — but leaving it keeps the table honest about the
model's terminal state; removing it is equally correct.

### The CLI harness

The `cmd/connstate` binary makes the bug observable from a real program: it drives
the state machine from command-line steps and exits non-zero on the first invalid
transition.

Create `cmd/connstate/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"os"

	"example.com/connstate/internal/conn"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: connstate [connect|authenticate|close]...")
		os.Exit(2)
	}
	c := conn.New()
	steps := map[string]func() error{
		"connect":      c.Connect,
		"authenticate": c.Authenticate,
		"close":        c.Close,
	}
	for _, arg := range os.Args[1:] {
		fn, ok := steps[arg]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown step %q\n", arg)
			os.Exit(2)
		}
		if err := fn(); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL %s: %v (state=%s)\n", arg, err, c.State)
			if errors.Is(err, conn.ErrInvalidTransition) {
				os.Exit(1)
			}
			os.Exit(1)
		}
		fmt.Printf("OK   %s (state=%s)\n", arg, c.State)
	}
}
```

Run it:

```bash
go run ./cmd/connstate connect close close
```

Expected output (with the fix applied):

```text
OK   connect (state=connected)
OK   close (state=closed)
FAIL close: invalid state transition: closed -> closed (state=closed)
exit status 1
```

Before the fix the same command printed three `OK` lines and exited 0 — the bug,
observable end to end.

### Tests

`TestClosedToClosedIsRejected` is the bug reproducer: it drives the happy path,
tries to close again, and asserts both that the second close returns
`ErrInvalidTransition` and that the state did not change. `TestInvalidTransitions`
pins three single-transition rejections, each from a setup that reaches the right
starting state. `TestRejectsUnknownState` sets an out-of-range source state and
asserts the `!ok` branch wraps `ErrUnknownState` — a distinct error from an
in-table rejection.

Create `internal/conn/state_test.go`:

```go
package conn

import (
	"errors"
	"fmt"
	"testing"
)

func TestNewConnectionStartsInNew(t *testing.T) {
	t.Parallel()

	c := New()
	if c.State != StateNew {
		t.Fatalf("State = %s, want new", c.State)
	}
}

func TestHappyPathLifecycle(t *testing.T) {
	t.Parallel()

	c := New()
	if err := c.Connect(); err != nil {
		t.Fatalf("Connect() = %v", err)
	}
	if err := c.Authenticate(); err != nil {
		t.Fatalf("Authenticate() = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if c.State != StateClosed {
		t.Fatalf("State = %s, want closed", c.State)
	}
}

func TestClosedToClosedIsRejected(t *testing.T) {
	t.Parallel()

	c := New()
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Close on closed = %v, want ErrInvalidTransition", err)
	}
	if c.State != StateClosed {
		t.Fatalf("State = %s, want closed (Close on closed must not transition)", c.State)
	}
}

func TestInvalidTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		setup  func(*Connection)
		action func(*Connection) error
	}{
		{"authenticate from new", func(c *Connection) {}, (*Connection).Authenticate},
		{"close from new", func(c *Connection) {}, (*Connection).Close},
		{"connect from authenticated", func(c *Connection) { _ = c.Connect(); _ = c.Authenticate() }, (*Connection).Connect},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := New()
			tc.setup(c)
			if err := tc.action(c); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("action err = %v, want ErrInvalidTransition", err)
			}
		})
	}
}

func TestRejectsUnknownState(t *testing.T) {
	t.Parallel()

	c := New()
	c.State = State(99) // a source state absent from the transition table
	if err := c.Transition(StateClosed); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("Transition from unknown = %v, want ErrUnknownState", err)
	}
}

func ExampleConnection() {
	c := New()
	_ = c.Connect()
	_ = c.Close()
	err := c.Close() // second close: rejected by the same-state guard
	fmt.Println(err)
	// Output: invalid state transition: closed -> closed
}
```

## Review

The state machine is correct when `Transition` accepts an edge only if the source
and target differ and the table contains that edge, and rejects everything else
with the right sentinel. The proof is `TestClosedToClosedIsRejected`: the second
`Close()` must return `ErrInvalidTransition` and leave the state unchanged. The
distinction `TestRejectsUnknownState` pins matters in practice — an unknown source
state is an internal-consistency failure (`ErrUnknownState`), while a disallowed
edge from a known state is a caller error (`ErrInvalidTransition`), and callers
key retry and alerting logic off which one they see. Read the failure line before
editing, assert with `errors.Is` against sentinels rather than string-matching,
and keep the transition table the single source of truth: adding a state means
adding its edges in lockstep.

## Resources

- [errors.Is](https://pkg.go.dev/errors#Is) — matching against sentinel errors through `%w` wrapping.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `%w`, `Is`, and `As`.
- [Go Specification: Errors](https://go.dev/ref/spec#Errors) — the `error` interface and error values.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-bounded-retry-off-by-one.md](02-bounded-retry-off-by-one.md)
