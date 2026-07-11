# Exercise 8: The type-Alias Trick to Stop MarshalJSON from Recursing

Every backend that hand-rolls a `MarshalJSON` eventually hits the classic
stack-overflow: the method calls `json.Marshal` on its own receiver, which calls
the method again, forever. The idiomatic fix is a *local type definition* — often
miscalled an "alias" — that strips the method set so `json.Marshal` serializes the
fields directly. This exercise builds an API response type with a computed field
and RFC3339 timestamps, and uses the guard correctly.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
apievent/                 independent module: example.com/apievent
  go.mod                  go 1.24
  event.go                type Event; MarshalJSON/UnmarshalJSON with the guard
  cmd/
    demo/
      main.go             marshals an event, unmarshals it back
  event_test.go           round-trip, computed-field, no-recursion tests
```

- Files: `event.go`, `cmd/demo/main.go`, `event_test.go`.
- Implement: `Event` with custom `MarshalJSON` that adds a computed `display` field and `UnmarshalJSON` that ignores it, both using the local `type noMethods Event` definition.
- Test: `Marshal` produces the augmented JSON without overflowing; `Unmarshal` reconstructs the struct; the computed field appears on output but is ignored on input; the timestamp round-trips as RFC3339.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/apievent/cmd/demo
cd ~/go-exercises/apievent
go mod init example.com/apievent
go mod edit -go=1.24
```

### Why marshaling the receiver recurses, and how the definition breaks it

`encoding/json` checks whether a value's type implements `json.Marshaler`
(a `MarshalJSON() ([]byte, error)` method). If it does, it calls that method
instead of walking the fields. So this is infinite recursion:

```go
func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(e) // e is an Event, which has MarshalJSON: recurse forever
}
```

The guard is to marshal a value of a *different* type that has the same fields but
no `MarshalJSON`. A local type definition gives you exactly that:
`type noMethods Event` creates a new type whose underlying type is the named type
`Event`, and by the method-set rule a defined type over a named type starts with
an **empty** method set. So `noMethods` has `Event`'s fields but none of its
methods; `json.Marshal(noMethods(e))` serializes the fields directly, no recursion.
You then wrap that in an anonymous struct to add computed fields like `display`.

The load-bearing detail, and the reason this exercise exists in a lesson about
aliases: the guard must be a *definition*, not an alias. If you wrote
`type noMethods = Event`, that is a real alias — `noMethods` *is* `Event`, keeps
`MarshalJSON`, and recurses exactly as before. The colloquial name "alias trick" is
a misnomer; the mechanism is a definition, and the one-character `=` difference is
the difference between a fix and a crash.

`UnmarshalJSON` uses the same guard: a `*noMethods` pointer aliased onto the
receiver's memory so `json.Unmarshal` fills the real fields without calling the
custom method. The computed `display` field has no counterpart field on `Event`, so
it is simply ignored on the way in — computed on output, dropped on input.

Create `event.go`:

```go
package apievent

import (
	"encoding/json"
	"time"
)

// Event is an API response type with a custom JSON encoding: it adds a computed
// "display" field on output and encodes At as an RFC3339 timestamp.
type Event struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

// MarshalJSON augments the default encoding with a computed display field.
//
// The local `type noMethods Event` is a DEFINITION, not an alias: it has Event's
// fields but an empty method set, so json.Marshal serializes the fields directly
// instead of re-entering this method and recursing forever.
func (e Event) MarshalJSON() ([]byte, error) {
	type noMethods Event
	return json.Marshal(struct {
		noMethods
		Display string `json:"display"`
	}{
		noMethods: noMethods(e),
		Display:   e.Name + " (" + e.ID + ")",
	})
}

// UnmarshalJSON decodes into the real fields. It uses the same methodless
// definition so json.Unmarshal does not re-enter this method. The computed
// display field has no matching field on Event, so it is ignored on input.
func (e *Event) UnmarshalJSON(data []byte) error {
	type noMethods Event
	aux := struct {
		*noMethods
	}{noMethods: (*noMethods)(e)}
	return json.Unmarshal(data, &aux)
}

// Display is the same computed string the marshaler emits, exposed for tests.
func (e Event) Display() string {
	return e.Name + " (" + e.ID + ")"
}
```

### The wrong version, for contrast

This is the bug the guard prevents. It is shown as an illustrative snippet only
(not part of the built package):

```text
func (e Event) MarshalJSON() ([]byte, error) {
	type noMethods = Event            // an ALIAS: still IS Event
	return json.Marshal(noMethods(e)) // noMethods has MarshalJSON: recurses, crashes
}
```

Change the `=` to a definition and the recursion stops. That single character is
the entire lesson.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"time"

	"example.com/apievent"
)

func main() {
	e := apievent.Event{
		ID:   "evt_1",
		Name: "signup",
		At:   time.Date(2026, 7, 2, 10, 30, 0, 0, time.UTC),
	}

	b, err := json.Marshal(e)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(b))

	var back apievent.Event
	if err := json.Unmarshal(b, &back); err != nil {
		panic(err)
	}
	fmt.Println("round-trip at:", back.At.Format(time.RFC3339))
	fmt.Println("display recomputed:", back.Display())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"id":"evt_1","name":"signup","at":"2026-07-02T10:30:00Z","display":"signup (evt_1)"}
round-trip at: 2026-07-02T10:30:00Z
display recomputed: signup (evt_1)
```

### Tests

The tests confirm the marshaler completes (no recursion), emits the computed
`display` field, and that a round-trip reconstructs the struct while ignoring
`display` on input. The timestamp is asserted to survive as an RFC3339 instant.

Create `event_test.go`:

```go
package apievent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func newEvent() Event {
	return Event{
		ID:   "evt_42",
		Name: "purchase",
		At:   time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC),
	}
}

func TestMarshalAddsComputedField(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(newEvent()) // must not recurse/overflow
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(b)

	for _, want := range []string{`"id":"evt_42"`, `"display":"purchase (evt_42)"`, `"at":"2026-01-15T09:00:00Z"`} {
		if !strings.Contains(got, want) {
			t.Errorf("marshaled JSON %q missing %q", got, want)
		}
	}
}

func TestRoundTripIgnoresComputedField(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(newEvent())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var back Event
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if back.ID != "evt_42" || back.Name != "purchase" {
		t.Fatalf("round-trip lost fields: %+v", back)
	}
	if !back.At.Equal(newEvent().At) {
		t.Errorf("timestamp not preserved: got %s", back.At.Format(time.RFC3339))
	}
}

func TestUnmarshalDropsUnknownDisplay(t *testing.T) {
	t.Parallel()

	// Input carries a display value; it must be ignored, then recomputed.
	in := `{"id":"evt_9","name":"login","at":"2026-03-01T00:00:00Z","display":"attacker-controlled"}`
	var e Event
	if err := json.Unmarshal([]byte(in), &e); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if e.Display() != "login (evt_9)" {
		t.Errorf("Display = %q, want recomputed value", e.Display())
	}
}

func ExampleEvent_MarshalJSON() {
	e := Event{ID: "evt_1", Name: "signup", At: time.Date(2026, 7, 2, 10, 30, 0, 0, time.UTC)}
	b, _ := json.Marshal(e)
	fmt.Println(string(b))
	// Output: {"id":"evt_1","name":"signup","at":"2026-07-02T10:30:00Z","display":"signup (evt_1)"}
}
```

## Review

The encoding is correct when `Marshal` completes and emits the computed `display`,
and when a round-trip restores the fields while ignoring `display` on input. The
mistake the exercise targets is calling `json.Marshal(e)` on the receiver type
directly, which recurses until the stack overflows. The subtler mistake — the one
tied to this lesson — is "fixing" it with `type noMethods = Event`: that is an
alias, keeps the marshaler, and still recurses. It must be a definition
(`type noMethods Event`) so the method set is stripped. Confirm the tests run to
completion; a hang or a stack-overflow panic means the guard is an alias.

## Resources

- [`encoding/json.Marshaler`](https://pkg.go.dev/encoding/json#Marshaler) — the interface whose presence triggers the recursion.
- [`encoding/json.Marshal`](https://pkg.go.dev/encoding/json#Marshal) — how anonymous embedded structs promote fields.
- [`time.Time.Format` with `time.RFC3339`](https://pkg.go.dev/time#Time.Format) — the timestamp layout used.

---

Prev: [07-generic-set-alias-vs-defined.md](07-generic-set-alias-vs-defined.md) | Next: [09-method-set-loss-when-wrapping.md](09-method-set-loss-when-wrapping.md)
