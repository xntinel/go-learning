# Exercise 10: State Machine On Top Of Stringer — Validated Transitions With Readable Errors

The payoff of a good `String()` is operator-readable diagnostics. This final module
builds a transition table for the `Status` enum — `Pending -> Running ->
Succeeded/Failed`, terminal states rejecting further moves — whose error messages
use `String()` to read like `illegal transition running -> pending`, and whose
error type carries the `from`/`to` for programmatic handling.

Self-contained module: own `go mod init`, code, demo, and tests.

## What you'll build

```text
statemachine/               independent module: example.com/statemachine
  go.mod
  status.go                 Status enum + String() + transition table + TransitionError
  cmd/
    demo/
      main.go               drives a job through legal and illegal transitions
  status_test.go            transition table; TransitionError message + errors.As; terminal rejection
```

- Files: `status.go`, `cmd/demo/main.go`, `status_test.go`.
- Implement: an `allowed` transition table, a `Transition(from, to) error` that returns a typed `*TransitionError` (embedding both `String()` names) for illegal moves, and `IsTerminal`.
- Test: a `(from, to)` table asserting legal moves succeed and illegal ones fail; the error message reads `illegal transition <from> -> <to>`; `errors.As` extracts the `from`/`to`; terminal states reject everything.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/statemachine/cmd/demo
cd ~/go-exercises/statemachine
go mod init example.com/statemachine
```

### Tying Stringer to domain-error UX

The state machine is a `map[Status][]Status`: the set of states each state may move
to. `Pending` can go to `Running` (a worker picked it up); `Running` can go to
`Succeeded` or `Failed`; the terminal states go nowhere. `Transition(from, to)`
consults the table and, on an illegal move, returns a `*TransitionError` — a typed
error, not a formatted string, so a caller can branch on it with `errors.As` and
recover the `from`/`to` for metrics or retry logic. Its `Error()` method builds the
human message *through* `String()`: `fmt.Sprintf("illegal transition %s -> %s",
e.From, e.To)` yields `illegal transition running -> pending`. This is the whole
point of the lesson made concrete — the same `String()` that formats a log line
formats the error an on-call engineer reads at 3am.

A design note on the terminal check: rather than special-casing terminal states in
`Transition`, they fall out of the table naturally — `Succeeded` and `Failed` have
empty transition slices, so every move from them is illegal and returns the same
readable error. `IsTerminal` is kept as a separate query for callers (a scheduler
deciding whether to stop polling), and it agrees with the table by construction.

`maps.Clone` gives callers a defensive copy of the table if they want to inspect it
without mutating the package's state — a small but real API-hygiene detail for a
shared registry.

Create `status.go`:

```go
package statemachine

import (
	"fmt"
	"maps"
	"strconv"
)

// Status is a job lifecycle state.
type Status uint8

const (
	StatusUnknown Status = iota
	StatusPending
	StatusRunning
	StatusSucceeded
	StatusFailed
)

func (s Status) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusRunning:
		return "running"
	case StatusSucceeded:
		return "succeeded"
	case StatusFailed:
		return "failed"
	default:
		return "unknown(" + strconv.FormatUint(uint64(s), 10) + ")"
	}
}

// IsTerminal reports whether the job can no longer change state.
func (s Status) IsTerminal() bool {
	return s == StatusSucceeded || s == StatusFailed
}

// allowed is the transition table: the states each state may move to. Terminal
// states have no outgoing transitions.
var allowed = map[Status][]Status{
	StatusUnknown:   {StatusPending},
	StatusPending:   {StatusRunning, StatusFailed},
	StatusRunning:   {StatusSucceeded, StatusFailed},
	StatusSucceeded: {},
	StatusFailed:    {},
}

// TransitionError is returned for an illegal transition. It carries the states
// so callers can branch with errors.As, and formats them through String().
type TransitionError struct {
	From Status
	To   Status
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("illegal transition %s -> %s", e.From, e.To)
}

// Transition validates a move from one status to another, returning a
// *TransitionError if it is not permitted.
func Transition(from, to Status) error {
	for _, next := range allowed[from] {
		if next == to {
			return nil
		}
	}
	return &TransitionError{From: from, To: to}
}

// Table returns a defensive copy of the transition table for inspection.
func Table() map[Status][]Status {
	return maps.Clone(allowed)
}
```

### The runnable demo

The demo drives a job through its happy path and then attempts an illegal move, so
both the success and the readable error are visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/statemachine"
)

func main() {
	path := []struct{ from, to statemachine.Status }{
		{statemachine.StatusPending, statemachine.StatusRunning},
		{statemachine.StatusRunning, statemachine.StatusSucceeded},
		{statemachine.StatusSucceeded, statemachine.StatusRunning}, // illegal: terminal
		{statemachine.StatusRunning, statemachine.StatusPending},   // illegal: backwards
	}
	for _, m := range path {
		if err := statemachine.Transition(m.from, m.to); err != nil {
			fmt.Printf("%s -> %s: %v\n", m.from, m.to, err)
		} else {
			fmt.Printf("%s -> %s: ok\n", m.from, m.to)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pending -> running: ok
running -> succeeded: ok
succeeded -> running: illegal transition succeeded -> running
running -> pending: illegal transition running -> pending
```

### Tests

`TestTransitions` tables legal and illegal `(from, to)` pairs. `TestErrorMessage`
pins the human string. `TestErrorAs` proves the typed error survives wrapping and
that `errors.As` recovers the `from`/`to`. `TestTerminalRejectsAll` asserts every
move out of a terminal state is illegal.

Create `status_test.go`:

```go
package statemachine

import (
	"errors"
	"fmt"
	"testing"
)

func TestTransitions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		from, to Status
		ok       bool
	}{
		{StatusPending, StatusRunning, true},
		{StatusPending, StatusFailed, true},
		{StatusRunning, StatusSucceeded, true},
		{StatusRunning, StatusFailed, true},
		{StatusRunning, StatusPending, false},
		{StatusSucceeded, StatusRunning, false},
		{StatusFailed, StatusPending, false},
		{StatusPending, StatusSucceeded, false},
	}
	for _, tc := range tests {
		err := Transition(tc.from, tc.to)
		if tc.ok && err != nil {
			t.Errorf("Transition(%s, %s) = %v, want nil", tc.from, tc.to, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("Transition(%s, %s) = nil, want error", tc.from, tc.to)
		}
	}
}

func TestErrorMessage(t *testing.T) {
	t.Parallel()
	err := Transition(StatusRunning, StatusPending)
	if got, want := err.Error(), "illegal transition running -> pending"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestErrorAs(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("apply job update: %w", Transition(StatusSucceeded, StatusRunning))
	var te *TransitionError
	if !errors.As(wrapped, &te) {
		t.Fatalf("errors.As did not find *TransitionError in %v", wrapped)
	}
	if te.From != StatusSucceeded || te.To != StatusRunning {
		t.Fatalf("recovered from=%s to=%s, want succeeded/running", te.From, te.To)
	}
}

func TestTerminalRejectsAll(t *testing.T) {
	t.Parallel()
	for _, term := range []Status{StatusSucceeded, StatusFailed} {
		if !term.IsTerminal() {
			t.Errorf("%s should be terminal", term)
		}
		for _, to := range []Status{StatusPending, StatusRunning, StatusSucceeded, StatusFailed} {
			if err := Transition(term, to); err == nil {
				t.Errorf("Transition(%s, %s) = nil, want error (terminal)", term, to)
			}
		}
	}
}

func ExampleTransition() {
	fmt.Println(Transition(StatusRunning, StatusPending))
	// Output: illegal transition running -> pending
}
```

## Review

This module closes the loop the chapter opened: a domain enum whose four
representations are consistent, here put to work in a state machine whose errors are
readable *because* `String()` is good. The `*TransitionError` being a typed value,
not a bare `fmt.Errorf`, is what lets a caller both show the human message and
branch programmatically with `errors.As` — you get UX and control from one error.
Letting terminal states fall out of the table (empty slices) rather than
special-casing them keeps `Transition` uniform and the error identical for every
illegal move. Keep the table the single source of truth and expose only a defensive
copy via `Table()`, so no caller can mutate the machine's rules by accident.

## Resources

- [errors.As](https://pkg.go.dev/errors#As) — recovering a typed error through wrapping.
- [Effective Go: errors](https://go.dev/doc/effective_go#errors) — building error values that carry structured detail.
- [maps.Clone](https://pkg.go.dev/maps#Clone) — a defensive copy of the transition table.

---

Back to [09-go-generate-stringer.md](09-go-generate-stringer.md) | Next: [../11-builder-pattern-for-complex-structs/00-concepts.md](../11-builder-pattern-for-complex-structs/00-concepts.md)
