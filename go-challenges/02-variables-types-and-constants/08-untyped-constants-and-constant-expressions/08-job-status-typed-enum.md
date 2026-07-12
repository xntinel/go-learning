# Exercise 8: Job Status Enum As A Typed API Contract

A worker's job status is a state machine: queued, running, done, failed. This
module defines `Status` as a *defined type* with `iota` constants, a `Stringer`, an
`IsTerminal` check, a validating `Parse`, and `encoding.TextMarshaler` /
`TextUnmarshaler` for JSON and config round-trips. It is the deliberate contrast to
the untyped-constant modules: here the *type* is the contract, so a raw `int` must
not be accepted as a status.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
jobstatus/                    independent module: example.com/jobstatus
  go.mod                      go 1.26
  status.go                   type Status int; iota consts; String, IsTerminal, Parse,
                              MarshalText, UnmarshalText
  cmd/
    demo/
      main.go                 parses, round-trips through JSON, checks terminality
  status_test.go              round-trip every status; Parse rejects unknowns; defensive String
```

Files: `status.go`, `cmd/demo/main.go`, `status_test.go`.
Implement: `type Status int` with `StatusQueued..StatusFailed` via `iota`; a
`Stringer`; `IsTerminal`; `Parse(string) (Status, error)`; `MarshalText` /
`UnmarshalText`.
Test: `MarshalText`/`UnmarshalText` round-trip for every status; `Parse` rejects
unknown strings; `String` is defensive for out-of-range; `IsTerminal` only for
done/failed.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/08-untyped-constants-and-constant-expressions/08-job-status-typed-enum/cmd/demo
cd go-solutions/02-variables-types-and-constants/08-untyped-constants-and-constant-expressions/08-job-status-typed-enum
```

### The type is the contract

`type Status int` with `iota` constants gives each state a distinct value
(`StatusQueued = 0`, `StatusRunning = 1`, `StatusDone = 2`, `StatusFailed = 3`), but
the crucial part is the defined type. A function `func Transition(to Status)` will
not accept a bare `2` — the caller must pass a `Status` — which is exactly what
stops an invalid state from being smuggled into the machine as an integer. This is
the mirror image of the earlier byte-limit modules: there, untypedness was the goal
so one number could adapt to many contexts; here, the type is an invariant, so raw
ints are refused. That refusal is documented, not executed, because its value is
that the offending call does not compile.

`String` maps each value to its canonical text and returns a defensive
`"Status(N)"` for an out-of-range value rather than panicking or returning
`""` — a status read from a corrupt record should stringify to something
diagnosable. `Parse` is the inverse and validates: an unknown string is a
sentinel-wrapped error, not a silent zero value. `MarshalText`/`UnmarshalText`
implement `encoding.TextMarshaler`/`TextUnmarshaler`, which `encoding/json` uses
automatically, so a `Status` field round-trips through JSON and text configs as its
canonical name (`"running"`), not as the brittle integer `1`.

Create `status.go`:

```go
package jobstatus

import (
	"errors"
	"fmt"
)

// Status is a job's lifecycle state. The defined type is the contract: a bare
// int cannot be passed where a Status is expected.
type Status int

const (
	StatusQueued Status = iota
	StatusRunning
	StatusDone
	StatusFailed
)

// ErrUnknownStatus is returned by Parse for an unrecognized status string.
var ErrUnknownStatus = errors.New("unknown job status")

// statusText is the canonical text for each status, indexed by value.
var statusText = [...]string{
	StatusQueued:  "queued",
	StatusRunning: "running",
	StatusDone:    "done",
	StatusFailed:  "failed",
}

// String returns the canonical text, or a defensive Status(N) for an
// out-of-range value.
func (s Status) String() string {
	if s < 0 || int(s) >= len(statusText) {
		return fmt.Sprintf("Status(%d)", int(s))
	}
	return statusText[s]
}

// IsTerminal reports whether the job has reached a final state.
func (s Status) IsTerminal() bool {
	return s == StatusDone || s == StatusFailed
}

// Parse converts canonical text to a Status, erroring on anything unknown.
func Parse(text string) (Status, error) {
	for i, name := range statusText {
		if name == text {
			return Status(i), nil
		}
	}
	return 0, fmt.Errorf("%w: %q", ErrUnknownStatus, text)
}

// MarshalText implements encoding.TextMarshaler; an out-of-range value errors
// rather than emitting garbage.
func (s Status) MarshalText() ([]byte, error) {
	if s < 0 || int(s) >= len(statusText) {
		return nil, fmt.Errorf("%w: %d", ErrUnknownStatus, int(s))
	}
	return []byte(statusText[s]), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Status) UnmarshalText(text []byte) error {
	parsed, err := Parse(string(text))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/jobstatus"
)

type job struct {
	ID     string           `json:"id"`
	Status jobstatus.Status `json:"status"`
}

func main() {
	j := job{ID: "j-1", Status: jobstatus.StatusRunning}
	blob, _ := json.Marshal(j)
	fmt.Printf("marshaled: %s\n", blob)

	var back job
	_ = json.Unmarshal([]byte(`{"id":"j-2","status":"failed"}`), &back)
	fmt.Printf("unmarshaled: id=%s status=%s terminal=%v\n", back.ID, back.Status, back.Status.IsTerminal())

	if _, err := jobstatus.Parse("paused"); err != nil {
		fmt.Println("parse paused:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
marshaled: {"id":"j-1","status":"running"}
unmarshaled: id=j-2 status=failed terminal=true
parse paused: unknown job status: "paused"
```

### Tests

`TestRoundTrip` marshals every status and unmarshals it back, asserting identity.
`TestParse` checks each canonical string and rejects an unknown one with the
sentinel. `TestStringDefensive` proves an out-of-range value stringifies to
`Status(N)`. `TestIsTerminal` pins terminality to done/failed only.

Create `status_test.go`:

```go
package jobstatus

import (
	"errors"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	for _, s := range []Status{StatusQueued, StatusRunning, StatusDone, StatusFailed} {
		text, err := s.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%v) error: %v", s, err)
		}
		var back Status
		if err := back.UnmarshalText(text); err != nil {
			t.Fatalf("UnmarshalText(%q) error: %v", text, err)
		}
		if back != s {
			t.Fatalf("round-trip %v -> %q -> %v", s, text, back)
		}
	}
}

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text string
		want Status
	}{
		{"queued", StatusQueued},
		{"running", StatusRunning},
		{"done", StatusDone},
		{"failed", StatusFailed},
	}
	for _, tt := range tests {
		got, err := Parse(tt.text)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", tt.text, err)
		}
		if got != tt.want {
			t.Fatalf("Parse(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}

	if _, err := Parse("paused"); !errors.Is(err, ErrUnknownStatus) {
		t.Fatalf("Parse(paused) err = %v, want ErrUnknownStatus", err)
	}
}

func TestStringDefensive(t *testing.T) {
	t.Parallel()

	if got := Status(99).String(); got != "Status(99)" {
		t.Fatalf("Status(99).String() = %q, want Status(99)", got)
	}
	if got := Status(-1).String(); got != "Status(-1)" {
		t.Fatalf("Status(-1).String() = %q, want Status(-1)", got)
	}
	if _, err := Status(99).MarshalText(); !errors.Is(err, ErrUnknownStatus) {
		t.Fatalf("MarshalText(99) err = %v, want ErrUnknownStatus", err)
	}
}

func TestIsTerminal(t *testing.T) {
	t.Parallel()

	tests := map[Status]bool{
		StatusQueued:  false,
		StatusRunning: false,
		StatusDone:    true,
		StatusFailed:  true,
	}
	for s, want := range tests {
		if got := s.IsTerminal(); got != want {
			t.Errorf("%v.IsTerminal() = %v, want %v", s, got, want)
		}
	}
}
```

## Review

The enum is correct when every status round-trips through text as its canonical
name, `Parse` rejects anything not in `statusText`, `String` is defensive for
corrupt values, and `IsTerminal` is true only for done and failed. The defined
`Status` type is the whole point: it makes the status a contract that resists being
replaced by a raw `int`, preventing an invalid state from entering the machine.
Implementing `TextMarshaler`/`TextUnmarshaler` rather than marshaling the raw
integer is what keeps stored jobs readable and stable across schema changes.

## Resources

- [encoding.TextMarshaler / TextUnmarshaler](https://pkg.go.dev/encoding#TextMarshaler) — the interfaces `encoding/json` uses for the round-trip.
- [Go Language Specification: Iota](https://go.dev/ref/spec#Iota) — numbering the enum members.
- [fmt.Stringer](https://pkg.go.dev/fmt#Stringer) — the canonical-text rendering.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-rate-limiter-capacity-config.md](07-rate-limiter-capacity-config.md) | Next: [09-compile-time-invariants.md](09-compile-time-invariants.md)
