# Exercise 3: Round-Trip Enum — MarshalText/UnmarshalText For Config And Wire

A status that lives in a YAML config file or a JSON payload must survive a load
and re-emit as its *name*, never as its `iota` integer. This module adds
`encoding.TextMarshaler`/`TextUnmarshaler` and a `ParseStatus` function, all
driven by a single `name <-> value` table so the two directions cannot drift.

Self-contained module: own `go mod init`, code, demo, and tests.

## What you'll build

```text
statustext/                 independent module: example.com/statustext
  go.mod
  status.go                 Status enum; String(); ParseStatus; MarshalText/UnmarshalText
  cmd/
    demo/
      main.go               marshal a config struct to JSON and back
  status_test.go            round-trip property; sentinel error; json string/number/bogus
```

- Files: `status.go`, `cmd/demo/main.go`, `status_test.go`.
- Implement: `ParseStatus(string) (Status, error)` returning a wrapped `ErrUnknownStatus`, plus `MarshalText`/`UnmarshalText` sharing one canonical name table with `String()`.
- Test: for every valid `Status`, `UnmarshalText(MarshalText(s)) == s`; `ParseStatus` rejects unknown strings, asserted with `errors.Is`; `json.Unmarshal` into a `Status` field accepts `"running"` and rejects `3` and `"bogus"`; a case/whitespace variant table.
- Verify: `go test -count=1 -race ./...`

### One table, three consumers

The design principle is a single source of truth. `statusNames` maps each `Status`
to its canonical lowercase name; `statusValues` is its inverse, built once at
package init. `String()`, `MarshalText`, and `ParseStatus` all read from these two
maps, so adding a status is a one-line change and the human, wire, and parse
representations can never disagree.

`MarshalText` returning `[]byte(name)` is what makes the enum safe on the wire.
Crucially, once a type implements `encoding.TextMarshaler`/`TextUnmarshaler`,
`encoding/json` uses them automatically: `json.Marshal` emits a quoted string, and
`json.Unmarshal` calls `UnmarshalText` for a JSON string — and *rejects a JSON
number outright* with "cannot unmarshal number into Go value of type Status",
because the presence of a `TextUnmarshaler` tells `encoding/json` to expect text.
That single interface therefore closes the "integer leaked onto the wire" hole in
both directions at once. The same is true of YAML libraries, which honor
`encoding.TextUnmarshaler` for scalar nodes.

`UnmarshalText` is necessarily on a pointer receiver — it mutates the receiver —
which is correct and does not affect `String()`'s value-receiver satisfaction of
`fmt.Stringer`. `ParseStatus` normalizes input (trim, lowercase) so `"Running"`,
`" running "`, and `"RUNNING"` all parse, and returns a `Status` plus a wrapped
sentinel `ErrUnknownStatus` for anything else, so callers can branch with
`errors.Is` rather than string-matching the message.

Create `status.go`:

```go
package statustext

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Status is a job lifecycle state. It is stored and transmitted as its name,
// never as this uint8, so reordering the constants cannot corrupt old data.
type Status uint8

const (
	StatusUnknown Status = iota
	StatusPending
	StatusRunning
	StatusSucceeded
	StatusFailed
)

// ErrUnknownStatus is the sentinel returned (wrapped) when a name does not map
// to a known Status. Callers branch on it with errors.Is.
var ErrUnknownStatus = errors.New("unknown status")

// statusNames is the single canonical table. String, MarshalText, and
// ParseStatus all derive from it, so the representations cannot drift.
var statusNames = map[Status]string{
	StatusUnknown:   "unknown",
	StatusPending:   "pending",
	StatusRunning:   "running",
	StatusSucceeded: "succeeded",
	StatusFailed:    "failed",
}

// statusValues is the inverse of statusNames, built once.
var statusValues = func() map[string]Status {
	m := make(map[string]Status, len(statusNames))
	for s, name := range statusNames {
		m[name] = s
	}
	return m
}()

// String returns the canonical name, or Status(N) for an out-of-range value.
func (s Status) String() string {
	if name, ok := statusNames[s]; ok {
		return name
	}
	return "Status(" + strconv.FormatUint(uint64(s), 10) + ")"
}

// ParseStatus maps a name (case-insensitive, surrounding space trimmed) to a
// Status, or returns ErrUnknownStatus wrapped with the offending input.
func ParseStatus(s string) (Status, error) {
	key := strings.ToLower(strings.TrimSpace(s))
	if v, ok := statusValues[key]; ok {
		return v, nil
	}
	return StatusUnknown, fmt.Errorf("%w: %q", ErrUnknownStatus, s)
}

// MarshalText emits the canonical name. encoding/json and YAML use it.
func (s Status) MarshalText() ([]byte, error) {
	name, ok := statusNames[s]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnknownStatus, uint8(s))
	}
	return []byte(name), nil
}

// UnmarshalText parses a name back into the receiver. Pointer receiver because
// it mutates; this does not affect String()'s value-receiver satisfaction.
func (s *Status) UnmarshalText(b []byte) error {
	v, err := ParseStatus(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}
```

### The runnable demo

The demo marshals a small config struct to JSON, then unmarshals it back, proving
the status crosses the boundary as `"running"` and never as `2`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/statustext"
)

type jobConfig struct {
	Name   string            `json:"name"`
	Status statustext.Status `json:"status"`
}

func main() {
	cfg := jobConfig{Name: "nightly-export", Status: statustext.StatusRunning}

	raw, _ := json.Marshal(cfg)
	fmt.Printf("marshaled: %s\n", raw)

	var back jobConfig
	if err := json.Unmarshal(raw, &back); err != nil {
		fmt.Println("unmarshal error:", err)
		return
	}
	fmt.Printf("round-trip: name=%s status=%s\n", back.Name, back.Status)

	err := json.Unmarshal([]byte(`{"name":"x","status":"bogus"}`), &back)
	fmt.Println("bad status:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
marshaled: {"name":"nightly-export","status":"running"}
round-trip: name=nightly-export status=running
bad status: unknown status: "bogus"
```

### Tests

`TestTextRoundTrip` is a property test: marshal then unmarshal every valid status
and require the value is preserved. `TestParseRejectsUnknown` asserts the sentinel
travels through `errors.Is`. `TestJSONField` covers the real boundary: a string
value decodes, a JSON *number* is rejected (the `TextUnmarshaler` makes
`encoding/json` demand text), and a bogus name is rejected with the wrapped
sentinel. `TestParseNormalization` pins the case- and whitespace-insensitivity.

Create `status_test.go`:

```go
package statustext

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestTextRoundTrip(t *testing.T) {
	t.Parallel()
	for s := range statusNames {
		text, err := s.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%v): %v", s, err)
		}
		var got Status
		if err := got.UnmarshalText(text); err != nil {
			t.Fatalf("UnmarshalText(%q): %v", text, err)
		}
		if got != s {
			t.Errorf("round-trip %v -> %q -> %v", s, text, got)
		}
	}
}

func TestParseRejectsUnknown(t *testing.T) {
	t.Parallel()
	_, err := ParseStatus("does-not-exist")
	if !errors.Is(err, ErrUnknownStatus) {
		t.Fatalf("ParseStatus error = %v, want wrap of ErrUnknownStatus", err)
	}
}

func TestParseNormalization(t *testing.T) {
	t.Parallel()
	tests := map[string]Status{
		"running":   StatusRunning,
		"Running":   StatusRunning,
		"  RUNNING": StatusRunning,
		"failed  ":  StatusFailed,
	}
	for in, want := range tests {
		got, err := ParseStatus(in)
		if err != nil || got != want {
			t.Errorf("ParseStatus(%q) = %v,%v; want %v,nil", in, got, err, want)
		}
	}
}

func TestJSONField(t *testing.T) {
	t.Parallel()
	type dto struct {
		Status Status `json:"status"`
	}

	var ok dto
	if err := json.Unmarshal([]byte(`{"status":"running"}`), &ok); err != nil {
		t.Fatalf("valid string rejected: %v", err)
	}
	if ok.Status != StatusRunning {
		t.Fatalf("got %v, want running", ok.Status)
	}

	var num dto
	if err := json.Unmarshal([]byte(`{"status":3}`), &num); err == nil {
		t.Fatal("JSON number accepted; want rejection")
	}

	var bogus dto
	err := json.Unmarshal([]byte(`{"status":"bogus"}`), &bogus)
	if !errors.Is(err, ErrUnknownStatus) {
		t.Fatalf("bogus name error = %v, want wrap of ErrUnknownStatus", err)
	}
}

func ExampleStatus_MarshalText() {
	b, _ := StatusSucceeded.MarshalText()
	fmt.Printf("%s\n", b)
	// Output: succeeded
}
```

## Review

The enum is wire-safe when the integer never appears in either direction and an
unknown name is a typed error rather than a silent zero. The property test is the
core guarantee: marshal-then-unmarshal is the identity on every valid value. The
JSON test encodes the real payoff — because `Status` implements `TextUnmarshaler`,
`encoding/json` both refuses a raw number and routes strings through your
validation, so a client cannot smuggle `3` past you. Keep `statusNames` as the one
place a status is named; deriving `statusValues`, `String`, and `MarshalText` from
it is what prevents the three representations from disagreeing after a future edit.
Note `UnmarshalText` is on a pointer receiver by necessity and that this is
orthogonal to `String()` staying on a value receiver.

## Resources

- [encoding: TextMarshaler / TextUnmarshaler](https://pkg.go.dev/encoding#TextMarshaler) — the interfaces `encoding/json` and YAML honor for scalars.
- [encoding/json: Marshal](https://pkg.go.dev/encoding/json#Marshal) — how a `TextMarshaler` becomes a quoted string and a number is rejected.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching the wrapped sentinel.

---

Back to [02-stringer-value-vs-pointer-and-recursion.md](02-stringer-value-vs-pointer-and-recursion.md) | Next: [04-logvaluer-redaction.md](04-logvaluer-redaction.md)
