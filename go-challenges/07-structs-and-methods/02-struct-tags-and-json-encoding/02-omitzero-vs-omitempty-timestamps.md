# Exercise 2: Fix a Timestamp That Never Gets Omitted — omitzero vs omitempty

The single most surprising `encoding/json` behavior in production is that
`omitempty` does not omit a zero `time.Time`. An audit or event payload with an
optional `CompletedAt time.Time` tagged `omitempty` ships
`"completed_at":"0001-01-01T00:00:00Z"` for every not-yet-completed record — a
timestamp that lies. This module reproduces the trap, then fixes it with `omitzero`
(Go 1.24), and shows that `omitzero` drives omission through an `IsZero()` method
you can define on your own types.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
auditjson/                     independent module: example.com/auditjson
  go.mod                       go 1.24 (omitzero landed in 1.24)
  audit/
    event.go                   BadEvent (omitempty trap), Event (omitzero), Window with IsZero
  cmd/
    demo/
      main.go                  marshal both, show the trap vs the fix
  audit/event_test.go          assert the trap key IS present, the fix key is ABSENT
```

Files: `audit/event.go`, `cmd/demo/main.go`, `audit/event_test.go`.
Implement: `BadEvent{CompletedAt omitempty}`, `Event{CompletedAt omitzero, Window omitzero}`, and a `Window` value type with `IsZero() bool`.
Test: zero `time.Time` under `omitempty` keeps the key; under `omitzero` drops it; a non-zero `Window` appears while a zero one is dropped via `IsZero`.
Verify: `go test -count=1 -race ./...`

Set up the module. `omitzero` requires Go 1.24+:

```bash
mkdir -p go-solutions/07-structs-and-methods/02-struct-tags-and-json-encoding/02-omitzero-vs-omitempty-timestamps/audit go-solutions/07-structs-and-methods/02-struct-tags-and-json-encoding/02-omitzero-vs-omitempty-timestamps/cmd/demo
cd go-solutions/07-structs-and-methods/02-struct-tags-and-json-encoding/02-omitzero-vs-omitempty-timestamps
go mod edit -go=1.24
```

## Why omitempty betrays time.Time

`omitempty` omits a field only when its value is JSON-empty: `false`, `0`, `""`,
`nil`, or an empty slice/map/array. That test is about the *JSON* shape of the
value, and it does not know that a `time.Time` has its own concept of "unset". A
zero `time.Time` is a non-nil struct with a non-empty marshaled form
(`"0001-01-01T00:00:00Z"`), so `omitempty` looks at it, decides it is not empty,
and keeps it. The same is true of a zero nested struct: `omitempty` never omits a
struct value, because a struct is not one of the JSON-empty kinds.

`omitzero`, added in Go 1.24, fixes exactly this. It omits a field when the value
equals the zero value for its type, and — the important part — if the type has a
method `IsZero() bool`, it calls that method to decide. `time.Time` already
implements `IsZero()`, so `CompletedAt time.Time` tagged `omitzero` disappears
when unset. For your own types you implement `IsZero()` and get the same control:
here `Window` reports zero when both of its instants are zero, so an event with no
processing window drops the `window` key entirely instead of emitting a nested
object full of epoch-zero timestamps.

The rule to internalize: for an optional `time.Time` or an optional nested struct,
`omitempty` is wrong and `omitzero` (or a pointer) is right. `omitempty` is only
correct for the JSON-empty kinds it was designed for — strings, numbers, bools,
slices, maps.

Create `audit/event.go`:

```go
// audit/event.go
package audit

import "time"

// BadEvent shows the trap: omitempty does NOT omit a zero time.Time, so an
// uncompleted event still serializes completed_at as 0001-01-01T00:00:00Z.
type BadEvent struct {
	ID          string    `json:"id"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

// Window is a processing window. It is zero when both instants are zero, which
// omitzero detects via this IsZero method.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// IsZero reports whether the window is unset. omitzero calls it.
func (w Window) IsZero() bool {
	return w.Start.IsZero() && w.End.IsZero()
}

// Event is the fixed version: omitzero omits a zero time.Time (via time.Time's
// own IsZero) and a zero Window (via the IsZero method above).
type Event struct {
	ID          string    `json:"id"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
	Window      Window    `json:"window,omitzero"`
}
```

## The runnable demo

The demo marshals an uncompleted `BadEvent` (the trap fires) and an uncompleted
`Event` (the fix works), then a completed `Event` to show the key returns when the
value is real.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"encoding/json"
	"fmt"
	"time"

	"example.com/auditjson/audit"
)

func main() {
	bad, _ := json.Marshal(audit.BadEvent{ID: "e1"})
	fmt.Printf("omitempty (trap): %s\n", bad)

	good, _ := json.Marshal(audit.Event{ID: "e1"})
	fmt.Printf("omitzero (fix):   %s\n", good)

	done, _ := json.Marshal(audit.Event{
		ID:          "e2",
		CompletedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Window: audit.Window{
			Start: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
			End:   time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		},
	})
	fmt.Printf("completed:        %s\n", done)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
omitempty (trap): {"id":"e1","completed_at":"0001-01-01T00:00:00Z"}
omitzero (fix):   {"id":"e1"}
completed:        {"id":"e2","completed_at":"2024-01-15T10:30:00Z","window":{"start":"2024-01-15T10:00:00Z","end":"2024-01-15T10:30:00Z"}}
```

## Tests

`TestOmitemptyKeepsZeroTime` documents the trap: it asserts the `completed_at`
key **is** present on a zero-time `BadEvent`. `TestOmitzeroDropsZeroTime` asserts
the key is **absent** on the fixed `Event`. `TestOmitzeroDropsZeroWindow` and
`TestOmitzeroKeepsSetWindow` prove that `omitzero` on `Window` is driven by the
`IsZero()` method: dropped when both instants are zero, kept when either is set.

Create `audit/event_test.go`:

```go
// audit/event_test.go
package audit

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestOmitemptyKeepsZeroTime(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(BadEvent{ID: "e1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"completed_at"`) {
		t.Fatalf("omitempty unexpectedly omitted zero time: %s", data)
	}
	if !strings.Contains(string(data), `0001-01-01T00:00:00Z`) {
		t.Fatalf("expected epoch-zero timestamp on the wire: %s", data)
	}
}

func TestOmitzeroDropsZeroTime(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(Event{ID: "e1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"completed_at"`) {
		t.Fatalf("omitzero should drop zero time: %s", data)
	}
}

func TestOmitzeroDropsZeroWindow(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(Event{ID: "e1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"window"`) {
		t.Fatalf("omitzero should drop zero Window via IsZero: %s", data)
	}
}

func TestOmitzeroKeepsSetWindow(t *testing.T) {
	t.Parallel()
	e := Event{
		ID: "e2",
		Window: Window{
			Start: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		},
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"window"`) {
		t.Fatalf("a Window with a set Start must not be omitted: %s", data)
	}
}

func TestWindowIsZero(t *testing.T) {
	t.Parallel()
	if !(Window{}).IsZero() {
		t.Fatal("empty Window should report IsZero() == true")
	}
	set := Window{End: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)}
	if set.IsZero() {
		t.Fatal("Window with a set End should report IsZero() == false")
	}
}

func ExampleEvent_omitzero() {
	b, _ := json.Marshal(Event{ID: "e1"})
	fmt.Println(string(b))
	// Output: {"id":"e1"}
}
```

## Review

The fix is correct when the zero-value `Event` marshals to `{"id":"e1"}` with no
`completed_at` and no `window`, while the same-shaped `BadEvent` under `omitempty`
still leaks the epoch-zero timestamp. `omitzero` earns its keep precisely on
`time.Time` and nested structs, the two kinds `omitempty` was never able to omit,
and the `Window.IsZero()` method shows the general mechanism: any type can define
its own notion of "unset" and have `omitzero` honor it. The one thing to remember
is the version floor — `omitzero` is Go 1.24+; on an older toolchain the tag is
silently ignored, so pin the language version and prefer a `*time.Time` pointer if
you must support pre-1.24 builds.

## Resources

- [Go 1.24 release notes: encoding/json omitzero](https://go.dev/doc/go1.24) — the `omitzero` tag option and `IsZero` dispatch.
- [`encoding/json.Marshal`](https://pkg.go.dev/encoding/json#Marshal) — the definition of `omitempty` (JSON-empty values only).
- [`time.Time.IsZero`](https://pkg.go.dev/time#Time.IsZero) — why a zero `time.Time` is detectable by `omitzero` but not by `omitempty`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-api-response-tags-and-omitempty.md](01-api-response-tags-and-omitempty.md) | Next: [03-strict-decoding-disallow-unknown-fields.md](03-strict-decoding-disallow-unknown-fields.md)
