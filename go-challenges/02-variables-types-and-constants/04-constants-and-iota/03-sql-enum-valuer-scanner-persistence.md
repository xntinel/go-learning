# Exercise 3: Persist an Enum to SQL as a Stable Code, Not a Reorderable Ordinal

The repository layer is where an ordinal does the most damage: a column full of
`2`s is a landmine that a `const`-block reorder detonates across every existing
row. This module builds the persistence seam so a `State` stores `'running'`,
not `4` — by implementing `database/sql/driver.Valuer` (write side) and
`sql.Scanner` (read side).

This module is fully self-contained: its own module, its own demo, its own tests.

## What you'll build

```text
sqlenum/                        module: example.com/sqlenum
  go.mod                        go 1.26
  state.go                      State enum + Value()/Scan() for database/sql
  cmd/
    demo/
      main.go                   round-trips a state through Value and Scan
  state_test.go                 Value codes, Scan(string/[]byte/nil), bad type, round-trip
```

Files: `state.go`, `cmd/demo/main.go`, `state_test.go`.
Implement: `Value() (driver.Value, error)` returning the string code and `Scan(src any) error` handling `string`, `[]byte`, and `nil`.
Test: `Value` returns the right code (and errors on `StateUnknown`); `Scan` accepts both `string` and `[]byte`, errors on unknown text and on an unsupported source type; `Value` → `Scan` round-trips.
Verify: `go test -count=1 ./...`

## Why the driver interfaces are the seam

`database/sql` bridges Go values and SQL columns through two small interfaces.
On the way *out*, `driver.Valuer` — `Value() (driver.Value, error)` — converts
your type into a `driver.Value`, which must be one of a fixed set of types
(`int64`, `float64`, `bool`, `[]byte`, `string`, `time.Time`, or `nil`).
Returning the string code means the column stores `'running'`. On the way *in*,
`sql.Scanner` — `Scan(src any) error` — receives whatever the driver produced
for that column and populates your value.

The trap that bites people in production is the `Scan` source type. You might
assume a text column always arrives as a `string`, but many drivers hand back a
text or `VARCHAR` column as `[]byte` instead — and some return `nil` for a NULL
column. A `Scan` that type-switches on `string` only will fail at runtime under
exactly the driver you deploy against. So `Scan` must handle `string`, `[]byte`,
*and* `nil`, and return a clear error for any other source type rather than
panicking on a bad type assertion.

Making `Value` error on `StateUnknown` is a deliberate write-side guard: it
refuses to persist a garbage sentinel, so a bug that left a struct field unset
surfaces as a failed insert instead of a row full of `''`. Because the stored
value is the string, the `const` block underneath is now free to be reordered —
the DB contract does not depend on the ordinal at all, which is the whole point.

Create `state.go`:

```go
package sqlenum

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrUnknownState is returned when a State has no stable string code.
	ErrUnknownState = errors.New("unknown state")
	// ErrScanType is returned when Scan receives an unsupported source type.
	ErrScanType = errors.New("unsupported scan source")
)

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

// Value implements driver.Valuer: it persists the stable string code and
// refuses to write an unknown/garbage state.
func (s State) Value() (driver.Value, error) {
	name, ok := stateToName[s]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnknownState, uint8(s))
	}
	return name, nil
}

// Scan implements sql.Scanner. Drivers return a text column as string, []byte,
// or nil, so all three must be handled; anything else is an error, not a panic.
func (s *State) Scan(src any) error {
	switch v := src.(type) {
	case string:
		return s.parse(v)
	case []byte:
		return s.parse(string(v))
	case nil:
		*s = StateUnknown
		return nil
	default:
		return fmt.Errorf("%w: %T", ErrScanType, src)
	}
}

func (s *State) parse(raw string) error {
	norm := strings.ToLower(strings.TrimSpace(raw))
	for st, name := range stateToName {
		if name == norm {
			*s = st
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrUnknownState, raw)
}
```

## The runnable demo

The demo round-trips a state the way `database/sql` would: `Value` produces the
stored code, and `Scan` reconstructs the state from a driver-style `[]byte`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sqlenum"
)

func main() {
	stored, err := sqlenum.StateSucceeded.Value()
	if err != nil {
		fmt.Println("value error:", err)
		return
	}
	fmt.Printf("stored column value: %v\n", stored)

	// A driver commonly hands a text column back as []byte.
	var got sqlenum.State
	if err := got.Scan([]byte("running")); err != nil {
		fmt.Println("scan error:", err)
		return
	}
	fmt.Printf("scanned state: %s\n", got)

	if err := got.Scan(int64(3)); err != nil {
		fmt.Println("scan bad type:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stored column value: succeeded
scanned state: running
scan bad type: unsupported scan source: int64
```

## Tests

`TestValue` asserts each state's stored code and that `StateUnknown` errors.
`TestScan` is a table over both `string` and `[]byte` inputs plus `nil`, an
unknown text, and an unsupported `int64` — asserting the sentinels with
`errors.Is`. `TestRoundTrip` proves `Value` → `Scan` returns the original state.

Create `state_test.go`:

```go
package sqlenum

import (
	"errors"
	"fmt"
	"testing"
)

func TestValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state State
		want  string
	}{
		{StateQueued, "queued"},
		{StateRunning, "running"},
		{StateSucceeded, "succeeded"},
		{StateFailed, "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			got, err := tt.state.Value()
			if err != nil {
				t.Fatalf("Value: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Value = %v, want %q", got, tt.want)
			}
		})
	}

	if _, err := StateUnknown.Value(); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("Value(StateUnknown) err = %v, want ErrUnknownState", err)
	}
}

func TestScan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		src     any
		want    State
		wantErr error
	}{
		{"string", "queued", StateQueued, nil},
		{"bytes", []byte("running"), StateRunning, nil},
		{"nil", nil, StateUnknown, nil},
		{"unknown text", "paused", 0, ErrUnknownState},
		{"bad type", int64(3), 0, ErrScanType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got State
			err := got.Scan(tt.src)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Scan(%v) err = %v, want %v", tt.src, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Scan(%v): %v", tt.src, err)
			}
			if got != tt.want {
				t.Fatalf("Scan(%v) = %v, want %v", tt.src, got, tt.want)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	for _, want := range []State{StateQueued, StateRunning, StateSucceeded, StateFailed} {
		v, err := want.Value()
		if err != nil {
			t.Fatalf("Value: %v", err)
		}
		var got State
		if err := got.Scan(v); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if got != want {
			t.Fatalf("round-trip = %v, want %v", got, want)
		}
	}
}

func ExampleState_Value() {
	v, _ := StateRunning.Value()
	fmt.Println(v)
	// Output: running
}
```

## Review

The persistence seam is correct when `Value` and `Scan` are inverses over the
stable string and every source type a real driver can produce is handled without
a panic. The `[]byte` case is not optional — a `Scan` that only accepts `string`
compiles and passes a naive test, then fails against a production driver that
returns bytes. The `Value` guard on `StateUnknown` is what keeps a forgotten
field from silently persisting an empty code. Because the contract is the string,
you can reorder or renumber the `const` block freely, and `TestRoundTrip` is what
proves the mapping did not drift.

## Resources

- [database/sql/driver: Valuer and Value](https://pkg.go.dev/database/sql/driver#Valuer)
- [database/sql: Scanner](https://pkg.go.dev/database/sql#Scanner)
- [database/sql/driver: Value](https://pkg.go.dev/database/sql/driver#Value)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-json-enum-textmarshaler-roundtrip.md](02-json-enum-textmarshaler-roundtrip.md) | Next: [04-rbac-bitmask-permission-set.md](04-rbac-bitmask-permission-set.md)
