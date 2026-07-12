# Exercise 1: Model Workflow States and RBAC Permissions with Constants

Every job-orchestration service needs a state machine and an authorization
model, and both are naturally expressed with constants. This module builds the
foundational `workflow` package: a mutually-exclusive `State` enum, a
combinable `Permission` bitmask, and the page-size limits a list endpoint
clamps against. It is the artifact every later module in this lesson extends.

This module is fully self-contained: its own module, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
workflow/                       module: example.com/workflow
  go.mod                        go 1.26
  workflow.go                   State enum, Permission bitmask, page limits, helpers
  cmd/
    demo/
      main.go                   parses a state, tests a permission set, clamps a page size
  workflow_test.go              zero-value, parse/terminal, bitmask, clamp, paused-error
```

Files: `workflow.go`, `cmd/demo/main.go`, `workflow_test.go`.
Implement: `State uint8` with `StateUnknown = iota`, `Permission uint16` with `1 << iota`, `DefaultPageSize`/`MaxPageSize`, and `ParseState`, `State.String`, `State.Terminal`, `Has`, `ClampPageSize`.
Test: zero value is `StateUnknown`, `ParseState` with whitespace/case, `Terminal`, bitmask membership, `ClampPageSize`, and `ParseState("paused")` returning an error.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/04-constants-and-iota/01-workflow-states-and-permissions/cmd/demo
cd go-solutions/02-variables-types-and-constants/04-constants-and-iota/01-workflow-states-and-permissions
```

## Why two kinds of constant in one package

The package holds two constant families that look identical at the byte level —
both are small unsigned integers — but encode opposite domain shapes.

`State` is an enum: a job is in exactly one state at a time. The values are
distinct labels, and their numeric order is an implementation detail. Critically,
`StateUnknown` sits at `iota == 0` so the zero value is a non-operational
sentinel: a `Job` struct whose `State` field was never assigned reads as
`Unknown`, not as a legitimately-queued job. That single choice turns a whole
class of "forgot to set the field" bugs into a detectable, rejectable state.

`Permission` is a bitmask: a principal can hold any combination of `Read`,
`Write`, and `Admin` simultaneously, so each is a distinct power of two via
`1 << iota` (`1`, `2`, `4`). Membership is tested by masking: `all & required
== required` is true only when every required bit is present. Combining
permissions with OR (`PermissionRead | PermissionWrite`) is meaningful; the same
operation on `State` values would be nonsense, which is exactly why the two must
never be mixed.

`DefaultPageSize` and `MaxPageSize` are untyped integer constants — plain
list-endpoint limits. `ClampPageSize` folds a caller-supplied size into the valid
range: non-positive falls back to the default, oversized is capped at the max.
This is the guard every paginated repository method runs before it touches the
database.

`ParseState` normalizes external input (trimming whitespace, lowercasing) before
matching, and returns `StateUnknown` with a non-nil error for anything it does
not recognize — so a bad string from an API request becomes an explicit error,
never a silent `Unknown` that flows downstream.

Create `workflow.go`:

```go
package workflow

import (
	"fmt"
	"strings"
)

// State is a mutually-exclusive workflow state. The zero value is reserved for
// StateUnknown so a forgotten struct field cannot masquerade as a real state.
type State uint8

const (
	StateUnknown State = iota
	StateQueued
	StateRunning
	StateSucceeded
	StateFailed
)

// Permission is an RBAC flag. Each value is a distinct power of two, so a
// permission set is any bitwise-OR combination of them.
type Permission uint16

const (
	PermissionRead Permission = 1 << iota
	PermissionWrite
	PermissionAdmin
)

// Page-size limits for list endpoints.
const (
	DefaultPageSize = 100
	MaxPageSize     = 500
)

// ParseState normalizes and parses an external state string. Unknown input is
// an explicit error, never a silent StateUnknown.
func ParseState(raw string) (State, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "queued":
		return StateQueued, nil
	case "running":
		return StateRunning, nil
	case "succeeded":
		return StateSucceeded, nil
	case "failed":
		return StateFailed, nil
	default:
		return StateUnknown, fmt.Errorf("unknown state: %q", raw)
	}
}

func (s State) String() string {
	switch s {
	case StateQueued:
		return "queued"
	case StateRunning:
		return "running"
	case StateSucceeded:
		return "succeeded"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Terminal reports whether the state is an end state a job cannot leave.
func (s State) Terminal() bool {
	return s == StateSucceeded || s == StateFailed
}

// Has reports whether all required permission bits are present in the set.
func Has(all Permission, required Permission) bool {
	return all&required == required
}

// ClampPageSize folds a caller-supplied size into [1, MaxPageSize], defaulting
// a non-positive size to DefaultPageSize.
func ClampPageSize(size int) int {
	if size <= 0 {
		return DefaultPageSize
	}
	if size > MaxPageSize {
		return MaxPageSize
	}
	return size
}
```

## The runnable demo

The demo mirrors a request path: parse a state string from an API call, check a
principal's permission set, and clamp a client-supplied page size.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/workflow"
)

func main() {
	s, err := workflow.ParseState(" Running ")
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}
	fmt.Printf("state=%s terminal=%v\n", s, s.Terminal())

	done, _ := workflow.ParseState("succeeded")
	fmt.Printf("state=%s terminal=%v\n", done, done.Terminal())

	grant := workflow.PermissionRead | workflow.PermissionWrite
	fmt.Printf("read=%v admin=%v\n",
		workflow.Has(grant, workflow.PermissionRead),
		workflow.Has(grant, workflow.PermissionAdmin))

	fmt.Printf("clamp(999)=%d\n", workflow.ClampPageSize(999))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
state=running terminal=false
state=succeeded terminal=true
read=true admin=false
clamp(999)=500
```

## Tests

The tests observe constant values through behavior rather than by printing raw
numbers, which keeps the ordinals an implementation detail. `TestParseStateAndTerminal`
is a table with subtasks covering whitespace and case normalization;
`TestPermissionsAreBitmasks` proves OR-combined membership; `TestClampPageSize`
covers the boundaries; and `TestParseStatePausedIsError` locks in that an
unrecognized state is a real error, not a silent `Unknown`.

Create `workflow_test.go`:

```go
package workflow

import (
	"fmt"
	"testing"
)

func TestStateZeroValueIsUnknown(t *testing.T) {
	t.Parallel()

	var state State
	if state != StateUnknown {
		t.Fatalf("zero State = %v, want StateUnknown", state)
	}
	if got := state.String(); got != "unknown" {
		t.Fatalf("String() = %q, want %q", got, "unknown")
	}
}

func TestParseStateAndTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw      string
		want     State
		terminal bool
	}{
		{raw: "queued", want: StateQueued, terminal: false},
		{raw: " RUNNING ", want: StateRunning, terminal: false},
		{raw: "succeeded", want: StateSucceeded, terminal: true},
		{raw: "failed", want: StateFailed, terminal: true},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()

			got, err := ParseState(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("ParseState(%q) = %v, want %v", tt.raw, got, tt.want)
			}
			if got.Terminal() != tt.terminal {
				t.Fatalf("Terminal() = %v, want %v", got.Terminal(), tt.terminal)
			}
		})
	}
}

func TestParseStatePausedIsError(t *testing.T) {
	t.Parallel()

	got, err := ParseState("paused")
	if err == nil {
		t.Fatal("ParseState(\"paused\") err = nil, want non-nil")
	}
	if got != StateUnknown {
		t.Fatalf("ParseState(\"paused\") = %v, want StateUnknown", got)
	}
}

func TestPermissionsAreBitmasks(t *testing.T) {
	t.Parallel()

	readWrite := PermissionRead | PermissionWrite
	if !Has(readWrite, PermissionRead) {
		t.Fatal("readWrite should include read")
	}
	if !Has(readWrite, PermissionWrite) {
		t.Fatal("readWrite should include write")
	}
	if Has(readWrite, PermissionAdmin) {
		t.Fatal("readWrite should not include admin")
	}
}

func TestClampPageSize(t *testing.T) {
	t.Parallel()

	tests := map[int]int{
		-1:  DefaultPageSize,
		0:   DefaultPageSize,
		50:  50,
		999: MaxPageSize,
	}

	for input, want := range tests {
		if got := ClampPageSize(input); got != want {
			t.Fatalf("ClampPageSize(%d) = %d, want %d", input, got, want)
		}
	}
}

func ExampleState_String() {
	fmt.Println(StateRunning, StateSucceeded)
	// Output: running succeeded
}
```

## Review

The package is correct when `State` behavior is a pure function of the constant
identity — `String`, `Terminal`, and `ParseState` agree on the same four
operational states, and the zero value is `unknown` everywhere. The most
important structural choice is reserving `iota == 0` for `StateUnknown`: without
it, the zero value is `queued` and a struct with a forgotten field is a silent
bug rather than a caught one. Keep `State` and `Permission` conceptually apart —
one is a single value, the other a set — and never OR two `State` values. Run
`go vet ./...` and `go test -race ./...` to confirm the package is clean.

## Resources

- [Go Specification: Constants](https://go.dev/ref/spec#Constants)
- [Go Specification: Iota](https://go.dev/ref/spec#Iota)
- [Effective Go: Constants and iota](https://go.dev/doc/effective_go#constants)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-json-enum-textmarshaler-roundtrip.md](02-json-enum-textmarshaler-roundtrip.md)
