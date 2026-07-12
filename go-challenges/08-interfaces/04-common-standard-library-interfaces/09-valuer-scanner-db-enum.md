# Exercise 9: driver.Valuer and sql.Scanner for a DB Enum Column

Mapping a domain enum to a SQL column without leaking driver types into the domain
is the job of two interfaces: `driver.Valuer` writes the value out, `sql.Scanner`
reads it back. The traps are the restricted `driver.Value` set on the write side
and the `string`-versus-`[]byte`-versus-`NULL` ambiguity on the read side. This
module builds both for an `OrderStatus` stored as a TEXT column.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
dbenum/                     independent module: example.com/dbenum
  go.mod
  dbenum.go                 OrderStatus with Value() and Scan(); String() for display
  cmd/
    demo/
      main.go               stores a status, scans it back from a []byte source
  dbenum_test.go            Value type, string/[]byte/nil scan, unknown errors, round-trip
```

- Files: `dbenum.go`, `cmd/demo/main.go`, `dbenum_test.go`.
- Implement: `Value() (driver.Value, error)` returning a string, and `Scan(src any) error` handling `string`, `[]byte`, and `nil`, on the `OrderStatus` type.
- Test: `Value()` returns a type in `driver.Value`'s allowed set and never panics; `Scan` handles both `string` and `[]byte`; `Scan(nil)` sets a defined invalid state; `Scan` of an unknown value errors; a `Value`/`Scan` round-trip is the identity.
- Verify: `go test -count=1 -race ./...`

### The restricted write set and the ambiguous read

`driver.Value` is not `any` — `database/sql` accepts only `nil`, `int64`,
`float64`, `bool`, `[]byte`, `string`, and `time.Time`. Return anything else from
`Value()` — a bare `int`, a `uint64` that overflows `int64`, a custom struct — and
the failure surfaces at query time, deep inside `database/sql`, not at compile
time. Storing an enum as its stable name means `Value()` returns a `string`, which
is in the set and is exactly what a TEXT column wants. `Value()` also validates:
an out-of-range status returns an error rather than writing garbage.

The read side is where drivers disagree. The `database/sql` docs are explicit that
a TEXT column can come back as `string` from one driver and `[]byte` from another,
and a NULL always arrives as `nil`. A `Scan` that only handles `string` panics or
errors the moment you swap drivers or hit a NULL. The correct implementation
type-switches: `string` and `[]byte` both become the name; `nil` maps to a defined
`StatusInvalid` zero value rather than a panic; any other concrete type is a clear
error naming the type. After extracting the name, it looks it up in the known set
and errors on anything unrecognized — the DB should never hold a status the domain
does not know, and if it does you want to hear about it.

Create `dbenum.go`:

```go
package dbenum

import (
	"database/sql/driver"
	"fmt"
)

// OrderStatus is a domain enum stored as a TEXT column.
type OrderStatus int

const (
	StatusInvalid OrderStatus = iota // zero value / NULL
	StatusPending
	StatusPaid
	StatusShipped
	StatusDelivered
	StatusCancelled
)

var statusNames = map[OrderStatus]string{
	StatusPending:   "pending",
	StatusPaid:      "paid",
	StatusShipped:   "shipped",
	StatusDelivered: "delivered",
	StatusCancelled: "cancelled",
}

var statusByName = func() map[string]OrderStatus {
	m := make(map[string]OrderStatus, len(statusNames))
	for k, v := range statusNames {
		m[v] = k
	}
	return m
}()

func (s OrderStatus) String() string {
	if name, ok := statusNames[s]; ok {
		return name
	}
	return "invalid"
}

// Value implements driver.Valuer, returning a string (an allowed driver.Value).
func (s OrderStatus) Value() (driver.Value, error) {
	name, ok := statusNames[s]
	if !ok {
		return nil, fmt.Errorf("cannot store invalid order status %d", int(s))
	}
	return name, nil
}

// Scan implements sql.Scanner, handling the string/[]byte/NULL ambiguity that
// different drivers produce for a TEXT column.
func (s *OrderStatus) Scan(src any) error {
	if src == nil {
		*s = StatusInvalid
		return nil
	}
	var name string
	switch v := src.(type) {
	case string:
		name = v
	case []byte:
		name = string(v)
	default:
		return fmt.Errorf("cannot scan %T into OrderStatus", src)
	}
	v, ok := statusByName[name]
	if !ok {
		return fmt.Errorf("unknown order status %q from database", name)
	}
	*s = v
	return nil
}
```

### The runnable demo

The demo takes the value a driver would store (`Value()`), then scans a status
back from a `[]byte` source — the representation many drivers use for TEXT — and
prints it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/dbenum"
)

func main() {
	stored, _ := dbenum.StatusShipped.Value()
	fmt.Printf("stored: %v (%T)\n", stored, stored)

	var back dbenum.OrderStatus
	if err := back.Scan([]byte("shipped")); err != nil {
		fmt.Println("scan error:", err)
		return
	}
	fmt.Printf("scanned from []byte: %s\n", back)

	var fromNull dbenum.OrderStatus
	_ = fromNull.Scan(nil)
	fmt.Printf("scanned from NULL: %s\n", fromNull)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stored: shipped (string)
scanned from []byte: shipped
scanned from NULL: invalid
```

### Tests

`TestValueIsAllowedType` asserts `Value()` returns a `string` (in the `driver.Value`
set) and errors on an invalid status. `TestScanSources` is table-driven over
`string`, `[]byte`, `nil`, an unknown name, and an unsupported type.
`TestRoundTrip` runs `Value` then `Scan` for every valid status and asserts the
identity.

Create `dbenum_test.go`:

```go
package dbenum

import (
	"database/sql/driver"
	"fmt"
	"testing"
)

func TestValueIsAllowedType(t *testing.T) {
	t.Parallel()

	v, err := StatusPaid.Value()
	if err != nil {
		t.Fatal(err)
	}
	// driver.Value must be one of a fixed set; a string is allowed.
	if _, ok := v.(string); !ok {
		t.Fatalf("Value() returned %T, want string (an allowed driver.Value)", v)
	}
	// The value must be usable as a driver.Value without panic.
	var _ driver.Value = v

	if _, err := OrderStatus(99).Value(); err == nil {
		t.Fatal("Value() of an invalid status must error, not write garbage")
	}
}

func TestScanSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		src     any
		want    OrderStatus
		wantErr bool
	}{
		{"from string", "pending", StatusPending, false},
		{"from bytes", []byte("shipped"), StatusShipped, false},
		{"from nil", nil, StatusInvalid, false},
		{"unknown name", "teleported", StatusInvalid, true},
		{"unsupported type", 42, StatusInvalid, true},
	}
	for _, tc := range tests {
		var s OrderStatus
		err := s.Scan(tc.src)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected error, got nil", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if s != tc.want {
			t.Errorf("%s: Scan = %v, want %v", tc.name, s, tc.want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	for status := range statusNames {
		v, err := status.Value()
		if err != nil {
			t.Fatalf("Value(%v): %v", status, err)
		}
		var back OrderStatus
		if err := back.Scan(v); err != nil {
			t.Fatalf("Scan(%v): %v", v, err)
		}
		if back != status {
			t.Fatalf("round-trip: %v -> %v -> %v", status, v, back)
		}
	}
}

func ExampleOrderStatus_Value() {
	v, _ := StatusDelivered.Value()
	fmt.Printf("%v\n", v)
	// Output: delivered
}
```

## Review

The mapping is correct when `Value()` only ever returns a member of the
`driver.Value` set (a `string` here) and errors on an invalid enum, and when
`Scan` handles `string`, `[]byte`, and `nil` and rejects both unknown names and
unsupported types. The two mistakes this defends against are real production
outages: returning a disallowed type from `Value()` (which blows up at query time)
and a `Scan` that only handles `string` (which breaks the day you change drivers
or read a NULL). The round-trip test proves the pair is a faithful codec. In a real
model you would embed `OrderStatus` as a struct field and `database/sql` calls
these methods automatically on `Query`/`Exec`. Run `go test -race`.

## Resources

- [database/sql/driver.Valuer and Value](https://pkg.go.dev/database/sql/driver#Valuer) — the restricted write-side type set.
- [sql.Scanner](https://pkg.go.dev/database/sql#Scanner) — the read-side interface and the `string`/`[]byte`/NULL contract.
- [sql.NullString](https://pkg.go.dev/database/sql#NullString) — the stdlib pattern for nullable scalar columns.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-sort-interface-multikey.md](08-sort-interface-multikey.md) | Next: [10-http-handler-readiness.md](10-http-handler-readiness.md)
