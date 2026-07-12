# Exercise 2: Serialize a Status Enum Safely Across a JSON API Boundary

The moment a job status appears in a JSON response, its representation becomes a
contract with every client. This module builds the request/response DTO seam:
a `State` enum that decodes from and encodes to a stable lowercase string —
`"running"`, never the raw ordinal `2` — by implementing `encoding.TextMarshaler`
and `encoding.TextUnmarshaler`, so `encoding/json` uses them automatically.

This module is fully self-contained: its own module, its own demo, its own tests.

## What you'll build

```text
statusjson/                     module: example.com/statusjson
  go.mod                        go 1.26
  status.go                     State enum + MarshalText/UnmarshalText, Job DTO
  cmd/
    demo/
      main.go                   marshal a Job, unmarshal one, reject an unknown status
  status_test.go                round-trip table, unknown-error, absent-field-is-Unknown
```

Files: `status.go`, `cmd/demo/main.go`, `status_test.go`.
Implement: `State` with `MarshalText() ([]byte, error)` and `UnmarshalText([]byte) error`, plus a `Job` struct with a `State` field.
Test: each valid state round-trips (struct → JSON → struct); an unknown status is an error and does not default to a valid state; an absent field decodes to `StateUnknown`.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/04-constants-and-iota/02-json-enum-textmarshaler-roundtrip/cmd/demo
cd go-solutions/02-variables-types-and-constants/04-constants-and-iota/02-json-enum-textmarshaler-roundtrip
```

## Why default int marshaling is a latent bug

If you do nothing, `encoding/json` marshals a `State uint8` as its numeric
value: `{"state":2}`. That number is the `iota` ordinal — an internal encoding
that reorders freely — now frozen into every payload a client stores, caches, or
logs. Insert `StatePaused` between `StateQueued` and `StateRunning` next quarter
and every `2` in the wild silently changes meaning. The API contract broke, and
nothing failed to compile.

Implementing `encoding.TextMarshaler` (`MarshalText() ([]byte, error)`) and
`encoding.TextUnmarshaler` (`UnmarshalText([]byte) error`) fixes this at the
source. `encoding/json` checks for these interfaces before falling back to
default encoding: if a type implements `TextMarshaler`, JSON emits the returned
text as a quoted string, so `StateRunning` becomes `"running"`. On the way in,
`UnmarshalText` receives the unquoted bytes and maps them back. The string is
now the contract; the ordinal is free to move.

Two correctness details matter. First, `MarshalText` must *error* on a value with
no string form (like `StateUnknown`) rather than emit `"unknown"` — you do not
want to serialize a garbage state into a payload. Second, `UnmarshalText` must
*reject* an unrecognized string with an error, not silently leave the field at
its zero value or default it to a real state. Both errors are wrapped with `%w`
around a sentinel so callers can match them with `errors.Is`.

One modern note: Go 1.24 added `encoding.TextAppender` (an `AppendText` method)
as a lower-allocation companion to `TextMarshaler`; `encoding/json` will use it
when present. It is optional here — `TextMarshaler` alone is the contract that
makes the enum serialize as a string.

Create `status.go`:

```go
package statusjson

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnknownState is the sentinel for a State that has no stable string form.
var ErrUnknownState = errors.New("unknown state")

type State uint8

const (
	StateUnknown State = iota
	StateQueued
	StateRunning
	StateSucceeded
	StateFailed
)

var stateToName = map[State]string{
	StateQueued:    "queued",
	StateRunning:   "running",
	StateSucceeded: "succeeded",
	StateFailed:    "failed",
}

func (s State) String() string {
	if name, ok := stateToName[s]; ok {
		return name
	}
	return "unknown"
}

// MarshalText emits the stable string code, erroring on a state with no code so
// a garbage value cannot be serialized into a payload.
func (s State) MarshalText() ([]byte, error) {
	name, ok := stateToName[s]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnknownState, uint8(s))
	}
	return []byte(name), nil
}

// UnmarshalText parses a stable string code, rejecting anything unrecognized
// instead of defaulting to a valid state.
func (s *State) UnmarshalText(text []byte) error {
	raw := strings.ToLower(strings.TrimSpace(string(text)))
	for st, name := range stateToName {
		if name == raw {
			*s = st
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrUnknownState, raw)
}

// Job is an API DTO. Because State implements TextMarshaler/TextUnmarshaler,
// encoding/json serializes the state as a string, not its ordinal.
type Job struct {
	ID    string `json:"id"`
	State State  `json:"state"`
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/statusjson"
)

func main() {
	out, err := json.Marshal(statusjson.Job{ID: "job-42", State: statusjson.StateRunning})
	if err != nil {
		fmt.Println("marshal error:", err)
		return
	}
	fmt.Println(string(out))

	var back statusjson.Job
	if err := json.Unmarshal([]byte(`{"id":"job-7","state":"succeeded"}`), &back); err != nil {
		fmt.Println("unmarshal error:", err)
		return
	}
	fmt.Printf("%s -> %s\n", back.ID, back.State)

	err = json.Unmarshal([]byte(`{"id":"job-9","state":"paused"}`), &back)
	fmt.Println("decode paused:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"id":"job-42","state":"running"}
job-7 -> succeeded
decode paused: unknown state: "paused"
```

## Tests

`TestRoundTrip` marshals a `Job` for each valid state, asserts the exact quoted
JSON, unmarshals it back, and asserts equality — proving struct → JSON → struct
stability. `TestUnknownStringIsError` drives `UnmarshalText` directly and asserts
`errors.Is(err, ErrUnknownState)` while confirming the field is not silently set
to a valid state. `TestAbsentFieldIsUnknown` proves a missing `state` key decodes
to the zero value, `StateUnknown`.

Create `status_test.go`:

```go
package statusjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state State
		json  string
	}{
		{StateQueued, `{"id":"j","state":"queued"}`},
		{StateRunning, `{"id":"j","state":"running"}`},
		{StateSucceeded, `{"id":"j","state":"succeeded"}`},
		{StateFailed, `{"id":"j","state":"failed"}`},
	}

	for _, tt := range tests {
		t.Run(tt.state.String(), func(t *testing.T) {
			t.Parallel()

			out, err := json.Marshal(Job{ID: "j", State: tt.state})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(out) != tt.json {
				t.Fatalf("Marshal = %s, want %s", out, tt.json)
			}

			var back Job
			if err := json.Unmarshal(out, &back); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if back.State != tt.state {
				t.Fatalf("round-trip State = %v, want %v", back.State, tt.state)
			}
		})
	}
}

func TestUnknownStringIsError(t *testing.T) {
	t.Parallel()

	s := StateQueued
	err := s.UnmarshalText([]byte("paused"))
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("UnmarshalText err = %v, want ErrUnknownState", err)
	}
	if s != StateQueued {
		t.Fatalf("field mutated to %v on failed decode; want it left as StateQueued", s)
	}
}

func TestMarshalUnknownIsError(t *testing.T) {
	t.Parallel()

	_, err := StateUnknown.MarshalText()
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("MarshalText(StateUnknown) err = %v, want ErrUnknownState", err)
	}
}

func TestAbsentFieldIsUnknown(t *testing.T) {
	t.Parallel()

	var job Job
	if err := json.Unmarshal([]byte(`{"id":"j"}`), &job); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if job.State != StateUnknown {
		t.Fatalf("absent state decoded to %v, want StateUnknown", job.State)
	}
}

func ExampleState_MarshalText() {
	out, _ := json.Marshal(Job{ID: "j", State: StateFailed})
	fmt.Println(string(out))
	// Output: {"id":"j","state":"failed"}
}
```

## Review

The DTO is correct when a state survives struct → JSON → struct unchanged and the
JSON is always the stable string, never a number. The two guards that make it
production-safe are the errors: `MarshalText` refusing to serialize an unnamed
state, and `UnmarshalText` refusing an unrecognized one. Note that when the field
mutation matters — a failed decode must not corrupt an existing value — the test
drives `UnmarshalText` directly, because that is where the invariant lives. If a
future rename changes `"running"` to `"in_progress"`, `TestRoundTrip` fails
immediately, which is the point: the string mapping is a contract under test.

## Resources

- [encoding: TextMarshaler / TextUnmarshaler / TextAppender](https://pkg.go.dev/encoding#TextMarshaler)
- [encoding/json: Marshal](https://pkg.go.dev/encoding/json#Marshal)
- [Go Specification: Iota](https://go.dev/ref/spec#Iota)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-workflow-states-and-permissions.md](01-workflow-states-and-permissions.md) | Next: [03-sql-enum-valuer-scanner-persistence.md](03-sql-enum-valuer-scanner-persistence.md)
