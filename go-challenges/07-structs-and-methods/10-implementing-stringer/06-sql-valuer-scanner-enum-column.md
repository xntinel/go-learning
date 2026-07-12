# Exercise 6: Persisting An Enum — driver.Valuer And sql.Scanner For A DB Column

The database is where the raw-`iota` bug does the most damage: store the integer,
reorder the constants a year later, and every historical row silently changes
meaning with no error. This module makes `Status` a `TEXT` column via
`driver.Valuer` (write the name) and `sql.Scanner` (read the name), and explicitly
rejects the integer so the corruption cannot happen.

Self-contained module: own `go mod init`, code, demo, and tests.

## What you'll build

```text
statussql/                  independent module: example.com/statussql
  go.mod
  status.go                 Status enum; Value() driver.Value; Scan(any) error
  cmd/
    demo/
      main.go               round-trips a status through Value/Scan; rejects an int
  status_test.go            round-trip; string/[]byte/nil; int/float rejected; interface asserts
```

- Files: `status.go`, `cmd/demo/main.go`, `status_test.go`.
- Implement: `Value() (driver.Value, error)` emitting the `String()` name; `Scan(src any) error` accepting `string`, `[]byte`, and `nil` (mapped to `StatusUnknown`) and rejecting numeric types with a typed error.
- Test: `Scan(Value(s))` reconstructs each `s`; `Scan` handles `string`, `[]byte`, and `nil`; a numeric `src` is rejected with a wrapped sentinel; `var _ driver.Valuer` and `var _ sql.Scanner` assertions.
- Verify: `go test -count=1 -race ./...`

### The storage contract

`database/sql` moves values across the driver boundary through two interfaces.
`driver.Valuer` (`Value() (driver.Value, error)`) converts a Go value into one of
the handful of types a driver understands — `int64`, `float64`, `bool`, `[]byte`,
`string`, `time.Time`, or `nil`. `sql.Scanner` (`Scan(src any) error`) converts a
value coming back from the database into the Go type; `src` arrives as one of those
same driver types, and *which* one depends on the driver — the same `TEXT` column
may surface as a `string` from one driver and a `[]byte` from another, so a correct
`Scan` must handle both.

The deliberate design choice is that `Value()` returns the *name*, a `string`, not
`uint8(s)`. That is what decouples the stored representation from the declaration
order: the column holds `"running"`, and no reordering of the constants can change
what `"running"` means. The mirror-image choice is that `Scan` *rejects* numeric
`src` values with a typed error. If a legacy column or a careless migration ever
tries to feed an integer into this type, the code fails loudly instead of silently
trusting a positional value — the failure is the feature. `nil` (SQL `NULL`) maps
to `StatusUnknown` by contract, so a nullable column has a defined zero.

Two sentinels make the failures branchable: `ErrUnknownStatus` for a name that is
not in the table, and `ErrUnsupportedType` for a `src` Go type the column should
never carry.

Create `status.go`:

```go
package statussql

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
)

// Status is a job lifecycle state persisted as its TEXT name, never as this
// uint8. Reordering the constants below cannot corrupt stored rows.
type Status uint8

const (
	StatusUnknown Status = iota
	StatusPending
	StatusRunning
	StatusSucceeded
	StatusFailed
)

var (
	// ErrUnknownStatus is returned (wrapped) for a name not in the table.
	ErrUnknownStatus = errors.New("unknown status")
	// ErrUnsupportedType is returned (wrapped) when Scan receives a Go type the
	// column should never hold, notably a raw integer.
	ErrUnsupportedType = errors.New("unsupported scan type")
)

var statusNames = map[Status]string{
	StatusUnknown:   "unknown",
	StatusPending:   "pending",
	StatusRunning:   "running",
	StatusSucceeded: "succeeded",
	StatusFailed:    "failed",
}

var statusValues = func() map[string]Status {
	m := make(map[string]Status, len(statusNames))
	for s, name := range statusNames {
		m[name] = s
	}
	return m
}()

func (s Status) String() string {
	if name, ok := statusNames[s]; ok {
		return name
	}
	return "Status(" + strconv.FormatUint(uint64(s), 10) + ")"
}

// Value implements driver.Valuer, storing the stable name as a string.
func (s Status) Value() (driver.Value, error) {
	name, ok := statusNames[s]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnknownStatus, uint8(s))
	}
	return name, nil
}

// Scan implements sql.Scanner. It accepts a string or []byte name and nil
// (NULL -> StatusUnknown), and rejects any other Go type, including integers, so
// a positional value can never be trusted as a status.
func (s *Status) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*s = StatusUnknown
		return nil
	case string:
		return s.scanName(v)
	case []byte:
		return s.scanName(string(v))
	default:
		return fmt.Errorf("%w: %T into Status", ErrUnsupportedType, src)
	}
}

func (s *Status) scanName(name string) error {
	v, ok := statusValues[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownStatus, name)
	}
	*s = v
	return nil
}
```

### The runnable demo

The demo round-trips a status through the driver boundary the way `database/sql`
would: `Value()` produces what the driver stores, and `Scan()` reconstructs the Go
value from what the driver returns (shown as both `string` and `[]byte`). It then
shows an integer being rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/statussql"
)

func main() {
	stored, _ := statussql.StatusRunning.Value()
	fmt.Printf("stored value: %q (%T)\n", stored, stored)

	var fromString statussql.Status
	_ = fromString.Scan("succeeded")
	fmt.Printf("scanned string: %s\n", fromString)

	var fromBytes statussql.Status
	_ = fromBytes.Scan([]byte("failed"))
	fmt.Printf("scanned bytes: %s\n", fromBytes)

	var fromNull statussql.Status
	_ = fromNull.Scan(nil)
	fmt.Printf("scanned NULL: %s\n", fromNull)

	var bad statussql.Status
	err := bad.Scan(int64(2))
	fmt.Printf("scan int rejected: %v\n", errors.Is(err, statussql.ErrUnsupportedType))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stored value: "running" (string)
scanned string: succeeded
scanned bytes: failed
scanned NULL: unknown
scan int rejected: true
```

### Tests

`TestRoundTrip` is the core guarantee: `Scan(Value(s))` reconstructs every status.
`TestScanSourceTypes` covers the three legitimate `src` shapes a driver can hand
you. `TestScanRejectsNumbers` proves the integer is refused with the typed sentinel
— the assertion that a future constant reordering cannot silently reinterpret a
stored value. The interface assertions pin the two contracts at compile time.

Create `status_test.go`:

```go
package statussql

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
)

var (
	_ driver.Valuer = Status(0)
	_ sql.Scanner   = (*Status)(nil)
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	for s := range statusNames {
		val, err := s.Value()
		if err != nil {
			t.Fatalf("Value(%v): %v", s, err)
		}
		var got Status
		if err := got.Scan(val); err != nil {
			t.Fatalf("Scan(%v): %v", val, err)
		}
		if got != s {
			t.Errorf("round-trip %v -> %v -> %v", s, val, got)
		}
	}
}

func TestScanSourceTypes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  any
		want Status
	}{
		{"string", "running", StatusRunning},
		{"bytes", []byte("failed"), StatusFailed},
		{"null", nil, StatusUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got Status
			if err := got.Scan(tc.src); err != nil {
				t.Fatalf("Scan(%v): %v", tc.src, err)
			}
			if got != tc.want {
				t.Errorf("Scan(%v) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

func TestScanRejectsNumbers(t *testing.T) {
	t.Parallel()
	for _, src := range []any{int64(2), float64(2), 2} {
		var got Status
		err := got.Scan(src)
		if !errors.Is(err, ErrUnsupportedType) {
			t.Errorf("Scan(%v %T) error = %v, want wrap of ErrUnsupportedType", src, src, err)
		}
	}
}

func TestScanRejectsUnknownName(t *testing.T) {
	t.Parallel()
	var got Status
	err := got.Scan("bogus")
	if !errors.Is(err, ErrUnknownStatus) {
		t.Fatalf("Scan(bogus) error = %v, want wrap of ErrUnknownStatus", err)
	}
}

func ExampleStatus_Value() {
	v, _ := StatusSucceeded.Value()
	fmt.Printf("%q\n", v)
	// Output: "succeeded"
}
```

## Review

The column is safe when the stored bytes are self-describing — a name, not a
position — and when a value that cannot be a valid status is rejected rather than
coerced. `TestRoundTrip` proves the write/read identity; `TestScanRejectsNumbers`
proves the safety property that motivates the whole design. Handle both `string`
and `[]byte` in `Scan`, because you do not control which one a given driver
delivers for a `TEXT` column. Decide the `NULL` contract explicitly — here `NULL`
becomes `StatusUnknown`; a stricter service might return an error instead — and
document it, because a silent wrong default is exactly the class of bug this
module exists to prevent. `Value()` and `String()` share the name table, so the
stored form and the logged form never disagree.

## Resources

- [database/sql/driver: Valuer](https://pkg.go.dev/database/sql/driver#Valuer) — the write side and the allowed `driver.Value` types.
- [database/sql: Scanner](https://pkg.go.dev/database/sql#Scanner) — the read side and the `src` types a driver can pass.
- [database/sql/driver: Value](https://pkg.go.dev/database/sql/driver#Value) — the exact set of types a driver understands.

---

Back to [05-fmt-formatter-verbs-and-flags.md](05-fmt-formatter-verbs-and-flags.md) | Next: [07-json-api-enum-field.md](07-json-api-enum-field.md)
