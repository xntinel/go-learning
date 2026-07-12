# Exercise 20: Event-Sourcing Replay with Per-Event Recovery and Checkpoint Tracking

**Nivel: Intermedio** — validacion rapida (un test corto).

Rebuilding an aggregate by replaying its event log is only useful if one bad
event — a schema drift, a handler that assumed a field would always be
present — cannot stop the replay from reaching events after it. Losing
those later events would mean losing real, valid history. This module
builds `Replay`, which applies every event to a `State` in order, isolates
each event's panic with its own recover, and keeps a monotonically
advancing `Checkpoint` so the replay never rewinds and never gets stuck —
a failed event is skipped, not retried, and every later success still moves
the checkpoint forward. It is fully self-contained: its own module, demo,
and tests.

## What you'll build

```text
eventreplay/                 independent module: example.com/eventreplay
  go.mod                     go 1.24
  eventreplay.go              Event, State, ApplyFunc, Failure, Replay, applyOne
  cmd/
    demo/
      main.go                runnable demo: 5 events, one panics mid-replay
  eventreplay_test.go          skip-and-continue, ordinary error as failure, no-failure run
```

Files: `eventreplay.go`, `cmd/demo/main.go`, `eventreplay_test.go`.
Implement: `Replay(state *State, events []Event, apply ApplyFunc) []Failure` that applies each `Event` via a per-event `applyOne` recover boundary, advancing `state.Checkpoint` and `state.Applied` only on a clean apply.
Test: five events where the third panics; assert the checkpoint still reaches the last event's position, `Applied` skips exactly the failed one, and the failure wraps the original sentinel via `errors.Is`; an ordinary returned error (no panic) is recorded the same way; a fully clean run advances every position with no failures.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the checkpoint only ever moves forward, and never on the failed event itself

`state.Checkpoint` and `state.Applied` are updated in exactly one place —
right after `applyOne` returns with no error — never inside `applyOne`
itself and never speculatively before the apply call completes. This is
what preserves causality: if event 3 fails, `Checkpoint` stays wherever
event 2 left it until event 4 succeeds and pushes it forward again. The
checkpoint is monotonic by construction (it only ever receives a later
event's position, in log order, never an earlier one), so a caller
persisting `Checkpoint` after each `Replay` call can safely resume from it
later without worrying the replay ever moved backward or double-applied
something already committed.

The recover boundary in `applyOne` is deliberately the *only* thing standing
between one event's bug and the rest of the log being lost. Wrapping the
whole `Replay` loop in one recover would mean event 3's panic unwinds out of
the loop entirely — events 4 and 5, which have nothing to do with event 3's
bug, would simply never be applied, silently truncating the aggregate's
history. Putting the recover one event wide, re-armed on every loop
iteration, is what lets replay treat a bad event as "skip it, note it,
keep going" rather than "the whole rebuild is compromised."

Create `eventreplay.go`:

```go
package eventreplay

import "fmt"

// Event is one record from the append-only log, replayed in position order.
type Event struct {
	Position int
	Type     string
	Data     string
}

// State is the aggregate being rebuilt by replay. Checkpoint tracks the
// highest position successfully applied so far; Applied records the
// positions that actually mutated state, in the order they were applied.
type State struct {
	Applied    []int
	Checkpoint int
}

// ApplyFunc mutates state for one event. It may return an ordinary error or
// panic; both leave state untouched by that event (the caller is expected
// not to partially mutate state before failing).
type ApplyFunc func(*State, Event) error

// Failure records one event that did not apply, whether it panicked or
// returned an error.
type Failure struct {
	Position int
	Event    string
	Err      error
}

// Replay applies every event to state in order. If one event's handler
// panics, the panic is recovered and recorded as a Failure carrying the
// event's position and the original panic detail — then replay continues
// from the next event. Checkpoint only ever advances on a successful apply,
// so it never rewinds and never gets stuck: a failed event is skipped, not
// retried, and every later event that succeeds still pushes the checkpoint
// forward. Causality is preserved because Applied and Checkpoint are only
// ever updated after apply returns cleanly, in strict log order.
func Replay(state *State, events []Event, apply ApplyFunc) []Failure {
	var failures []Failure
	for _, ev := range events {
		if err := applyOne(state, ev, apply); err != nil {
			failures = append(failures, Failure{Position: ev.Position, Event: ev.Type, Err: err})
			continue
		}
		state.Checkpoint = ev.Position
		state.Applied = append(state.Applied, ev.Position)
	}
	return failures
}

// applyOne is the recover boundary: exactly one event's worth of untrusted
// application logic.
func applyOne(state *State, ev Event, apply ApplyFunc) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = fmt.Errorf("event %d (%s) panicked: %w", ev.Position, ev.Type, e)
				return
			}
			err = fmt.Errorf("event %d (%s) panicked: %v", ev.Position, ev.Type, r)
		}
	}()
	return apply(state, ev)
}
```

### The runnable demo

Five events replay in order; the `withdraw` event at position 3 panics on
corrupted data, and the checkpoint still reaches position 5.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/eventreplay"
)

func main() {
	events := []eventreplay.Event{
		{Position: 1, Type: "account-opened", Data: "ok"},
		{Position: 2, Type: "deposit", Data: "ok"},
		{Position: 3, Type: "withdraw", Data: "corrupt"},
		{Position: 4, Type: "deposit", Data: "ok"},
		{Position: 5, Type: "account-closed", Data: "ok"},
	}

	apply := func(s *eventreplay.State, ev eventreplay.Event) error {
		if ev.Data == "corrupt" {
			panic(errors.New("withdraw amount unparseable"))
		}
		return nil
	}

	state := &eventreplay.State{}
	failures := eventreplay.Replay(state, events, apply)

	fmt.Printf("checkpoint: %d\n", state.Checkpoint)
	fmt.Printf("applied: %v\n", state.Applied)
	for _, f := range failures {
		fmt.Printf("failure at %d (%s): %v\n", f.Position, f.Event, f.Err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
checkpoint: 5
applied: [1 2 4 5]
failure at 3 (withdraw): event 3 (withdraw) panicked: withdraw amount unparseable
```

### Tests

`TestReplaySkipsPanickingEventAndKeepsGoing` asserts the checkpoint reaches
the last event's position despite the panic, `Applied` skips exactly the
failed one, and `errors.Is` still reaches the original sentinel through the
wrapping. `TestReplayOrdinaryErrorAlsoRecordedAsFailure` confirms a plain
returned error (no panic) is recorded the same way. `TestReplayNoFailures`
covers a fully clean run.

Create `eventreplay_test.go`:

```go
package eventreplay

import (
	"errors"
	"testing"
)

func TestReplaySkipsPanickingEventAndKeepsGoing(t *testing.T) {
	sentinel := errors.New("negative balance assumption violated")
	events := []Event{
		{Position: 1, Type: "deposit", Data: "100"},
		{Position: 2, Type: "deposit", Data: "50"},
		{Position: 3, Type: "withdraw", Data: "bad"}, // panics
		{Position: 4, Type: "deposit", Data: "25"},
		{Position: 5, Type: "deposit", Data: "10"},
	}

	apply := func(s *State, ev Event) error {
		if ev.Data == "bad" {
			panic(sentinel)
		}
		return nil
	}

	state := &State{}
	failures := Replay(state, events, apply)

	if len(failures) != 1 {
		t.Fatalf("len(failures) = %d, want 1", len(failures))
	}
	if failures[0].Position != 3 {
		t.Fatalf("failure position = %d, want 3", failures[0].Position)
	}
	if !errors.Is(failures[0].Err, sentinel) {
		t.Fatalf("failure error %v does not wrap the sentinel", failures[0].Err)
	}

	if state.Checkpoint != 5 {
		t.Fatalf("Checkpoint = %d, want 5 (later successes must still advance it)", state.Checkpoint)
	}
	wantApplied := []int{1, 2, 4, 5}
	if len(state.Applied) != len(wantApplied) {
		t.Fatalf("Applied = %v, want %v", state.Applied, wantApplied)
	}
	for i, pos := range wantApplied {
		if state.Applied[i] != pos {
			t.Fatalf("Applied[%d] = %d, want %d", i, state.Applied[i], pos)
		}
	}
}

func TestReplayOrdinaryErrorAlsoRecordedAsFailure(t *testing.T) {
	events := []Event{
		{Position: 1, Type: "a", Data: "ok"},
		{Position: 2, Type: "b", Data: "reject"},
		{Position: 3, Type: "c", Data: "ok"},
	}
	apply := func(s *State, ev Event) error {
		if ev.Data == "reject" {
			return errors.New("business rule violated")
		}
		return nil
	}

	state := &State{}
	failures := Replay(state, events, apply)

	if len(failures) != 1 || failures[0].Position != 2 {
		t.Fatalf("failures = %+v, want one failure at position 2", failures)
	}
	if state.Checkpoint != 3 {
		t.Fatalf("Checkpoint = %d, want 3", state.Checkpoint)
	}
}

func TestReplayNoFailures(t *testing.T) {
	events := []Event{{Position: 1}, {Position: 2}}
	state := &State{}
	failures := Replay(state, events, func(*State, Event) error { return nil })
	if failures != nil {
		t.Fatalf("failures = %v, want nil", failures)
	}
	if state.Checkpoint != 2 {
		t.Fatalf("Checkpoint = %d, want 2", state.Checkpoint)
	}
}
```

## Review

`Replay` is correct when the checkpoint reaches the last successfully
applied event regardless of how many earlier events failed, and when a
failed event is recorded without ever mutating `state.Applied` or
`state.Checkpoint` for that position. The recover in `applyOne` is
re-armed every iteration by the surrounding loop, the same discipline this
chapter applies to jobs and batch items; the event-sourcing-specific rule is
that `Checkpoint` and `Applied` are only ever touched *after* `applyOne`
returns cleanly, never inside the recover itself, which is what keeps
causality intact — a caller resuming replay from a persisted `Checkpoint`
later can trust it reflects only events that actually, successfully,
mutated state.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the per-event recover boundary this replay loop relies on.
- [fmt.Errorf and %w](https://pkg.go.dev/fmt#Errorf) — preserving a panicking event handler's original error.
- [Martin Fowler: Event Sourcing](https://martinfowler.com/eaaDev/EventSourcing.html) — the replay-from-log pattern this module implements defensively.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-request-trace-boundary.md](19-request-trace-boundary.md) | Next: [21-circuit-breaker-state-machine.md](21-circuit-breaker-state-machine.md)
