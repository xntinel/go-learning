# Exercise 8: Guard Against Enum Drift with an Exhaustiveness and Round-Trip Test

The most common enum bug is not in the enum — it is in the code that forgot to
keep up with it. Someone adds `StatePaused`, ships it, and it serializes as
`"unknown"` because nobody updated `String` and `ParseState`. This module builds
the defensive discipline that catches that drift in CI: a trailing sentinel
constant that counts the states, plus a round-trip test that every real state
maps to a distinct code and parses back.

This module is fully self-contained: its own module, its own demo, its own tests.

## What you'll build

```text
enumguard/                      module: example.com/enumguard
  go.mod                        go 1.26
  state.go                      State enum with a maxState sentinel, String/ParseState/Terminal
  cmd/
    demo/
      main.go                   prints every state and whether it is terminal
  state_test.go                 round-trip, no "unknown", distinct codes, terminal set
```

Files: `state.go`, `cmd/demo/main.go`, `state_test.go`.
Implement: a `State` enum ending in a `maxState` sentinel, plus `String`, `ParseState`, `Terminal`, and an `allStates()` helper bounded by the sentinel.
Test: every real state has a non-`"unknown"` `String`, `ParseState(String())` round-trips, the codes are distinct, and `Terminal` is true only for the end states.
Verify: `go test -count=1 ./...`

## Why a sentinel and a round-trip test

An enum defined with `iota` has no built-in notion of "how many values are
there" — the language does not give you a list to range over. The trick is to add
a trailing constant that captures the count:

```go
const (
	StateUnknown State = iota
	StateQueued
	StateRunning
	StateSucceeded
	StateFailed
	maxState // == 5; not a real state, just the count
)
```

Because `iota` increments per line, `maxState` equals the number of const specs
before it. It is not an operational state — it is a bound. `allStates()` ranges
`StateQueued` up to (but not including) `maxState`, yielding exactly the real
operational states. Now the enum is *iterable*, which is what makes an
exhaustiveness test possible.

The test itself asserts two properties for every state the sentinel bounds.
First, `String()` must not return `"unknown"` — a real state that stringifies to
the unknown sentinel means someone added a constant without adding its case.
Second, `ParseState(s.String())` must return `s` with no error — the round-trip
proves the string mapping is a true bijection, so serialization and
deserialization agree. Add a third check that the codes are distinct, and a
future copy-paste that maps two states to the same string is caught too.

The payoff is operational. When a teammate adds `StatePaused` between
`StateFailed` and `maxState` but forgets the `String`/`ParseState` cases,
`maxState` bumps to `6`, `allStates()` now includes the new value, and the
round-trip test fails immediately with a clear message — in CI, on the pull
request, not months later as a stream of `"unknown"` states in production
telemetry. The sentinel is what makes the test notice the new value without
anyone remembering to update the test.

Create `state.go`:

```go
package enumguard

import (
	"fmt"
	"strings"
)

type State uint8

const (
	StateUnknown State = iota
	StateQueued
	StateRunning
	StateSucceeded
	StateFailed
	maxState // sentinel: count of states, not an operational value
)

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

// ParseState is the inverse of String for the operational states.
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

func (s State) Terminal() bool {
	return s == StateSucceeded || s == StateFailed
}

// allStates returns every real operational state, bounded by the maxState
// sentinel so a newly-added constant is automatically included.
func allStates() []State {
	states := make([]State, 0, int(maxState)-1)
	for s := StateQueued; s < maxState; s++ {
		states = append(states, s)
	}
	return states
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/enumguard"
)

func main() {
	for _, name := range []string{"queued", "running", "succeeded", "failed"} {
		s, err := enumguard.ParseState(name)
		if err != nil {
			fmt.Println("parse error:", err)
			return
		}
		fmt.Printf("%-10s terminal=%v\n", s, s.Terminal())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
queued     terminal=false
running    terminal=false
succeeded  terminal=true
failed     terminal=true
```

## Tests

`TestExhaustiveRoundTrip` ranges every state from the sentinel-bounded
`allStates()` and asserts a non-`"unknown"` code, a clean `ParseState` round-trip,
and distinctness. `TestTerminalSet` pins exactly which states are terminal, so a
future edit that marks the wrong state terminal is caught.

Create `state_test.go`:

```go
package enumguard

import (
	"testing"
)

func TestExhaustiveRoundTrip(t *testing.T) {
	t.Parallel()

	seen := make(map[string]State)
	for _, s := range allStates() {
		code := s.String()
		if code == "unknown" {
			t.Fatalf("state %d has no String mapping (got %q)", s, code)
		}
		if prev, dup := seen[code]; dup {
			t.Fatalf("code %q maps to both %d and %d", code, prev, s)
		}
		seen[code] = s

		got, err := ParseState(code)
		if err != nil {
			t.Fatalf("ParseState(%q): %v", code, err)
		}
		if got != s {
			t.Fatalf("round-trip: ParseState(%q) = %d, want %d", code, got, s)
		}
	}

	if len(seen) != int(maxState)-1 {
		t.Fatalf("mapped %d states, want %d (sentinel drift)", len(seen), int(maxState)-1)
	}
}

func TestTerminalSet(t *testing.T) {
	t.Parallel()

	terminal := map[State]bool{
		StateSucceeded: true,
		StateFailed:    true,
	}
	for _, s := range allStates() {
		if got := s.Terminal(); got != terminal[s] {
			t.Fatalf("Terminal(%s) = %v, want %v", s, got, terminal[s])
		}
	}
}
```

## Review

The guard is correct when adding a state and forgetting its mapping *fails a
test*. The sentinel `maxState` makes `allStates()` self-updating, so the round-trip
test automatically covers any new value; the `"unknown"` check and the
distinctness map are what turn a missing or duplicated case into a failure. This
is cheap insurance: a few lines of test that convert a silent
serialize-as-unknown bug into a red build. Pair it with the `String`/`ParseState`
pattern from earlier modules and enum drift stops being an operational risk.

## Resources

- [Go Specification: Iota](https://go.dev/ref/spec#Iota)
- [fmt: Stringer](https://pkg.go.dev/fmt#Stringer)
- [Effective Go: Constants and iota](https://go.dev/doc/effective_go#constants)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-untyped-const-boundaries-overflow.md](07-untyped-const-boundaries-overflow.md) | Next: [09-http-retry-classifier-constants.md](09-http-retry-classifier-constants.md)
