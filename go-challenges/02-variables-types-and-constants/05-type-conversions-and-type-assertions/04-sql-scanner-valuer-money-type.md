# Exercise 4: Implement sql.Scanner And driver.Valuer For A Money Type

When you give a column a custom Go type, `database/sql` forces you through a type
boundary: on read it hands your value to `Scan(src any)` as one of a small set of
concrete driver types, and on write it calls `Value()` expecting one of the
canonical `driver.Value` types. This exercise builds a `Cents` money type that
survives that round trip, dispatching on the driver's concrete type with a type
switch.

This module is fully self-contained: its own module, all code inline, its own
demo and tests.

## What you'll build

```text
moneytype/                   independent module: example.com/moneytype
  go.mod                     go 1.26
  money.go                   type Cents; Scan(any) error; Value() (driver.Value, error)
  cmd/
    demo/
      main.go                runnable demo: Value() output fed back into Scan()
  money_test.go              round-trip + per-src-type subtests + unsupported-type error
```

- Files: `money.go`, `cmd/demo/main.go`, `money_test.go`.
- Implement: `Cents` with `Scan(src any) error` handling `int64`, `[]byte`, `string`, and `nil`, and `Value() (driver.Value, error)` returning the canonical `int64`.
- Test: a round trip (`Value` output fed back into `Scan` yields the original), a subtest per src type, a `nil`→zero policy, and error cases for `float64` and a non-numeric `[]byte`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/moneytype/cmd/demo
cd ~/go-exercises/moneytype
go mod init example.com/moneytype
go mod edit -go=1.26
```

### The driver contract Scan must satisfy

`database/sql` decouples your type from any specific driver by passing column
values through a fixed vocabulary. A driver's `Scan` source is always one of:
`int64`, `float64`, `bool`, `[]byte`, `string`, `time.Time`, or `nil` — and which
one you get depends on the driver and the column. A pure-Go SQLite driver might
hand an integer column as `int64`; a driver reading a numeric column as text
hands `[]byte` or `string`; a NULL always arrives as `nil`. So `Scan` cannot
assume a single type: it must be a type switch that accepts every form the value
can legitimately arrive in and rejects the rest with an error rather than a panic.

`Cents` stores an integer count of cents (money in floating point is its own
class of bug, so we never accept `float64` — a driver that hands `float64` for a
money column is a misconfiguration we surface loudly). `Scan` handles `int64`
directly, parses `[]byte` and `string` with `ParseInt`, and treats `nil` (a NULL
column) as zero — a deliberate policy choice you would make `Cents` a pointer or
add a `Valid` flag to change. The `default` case returns an error naming the
unsupported concrete type with `%T`, which is what catches the `float64` a
misconfigured column would send.

`Value` is the easy half: it returns the canonical `int64` form. `driver.Value`
must be one of the same fixed set of types; returning `int64(c)` keeps the column
integer-typed on write, matching how `Scan` reads it back.

Create `money.go`:

```go
// money.go
package moneytype

import (
	"database/sql/driver"
	"fmt"
	"strconv"
)

// Cents is an integer amount of money, stored as an int64 column.
type Cents int64

// Value implements driver.Valuer: the canonical wire form is int64.
func (c Cents) Value() (driver.Value, error) {
	return int64(c), nil
}

// Scan implements sql.Scanner, accepting every concrete type a driver may send
// for an integer money column and rejecting the rest.
func (c *Cents) Scan(src any) error {
	switch v := src.(type) {
	case int64:
		*c = Cents(v)
		return nil
	case []byte:
		return c.parse(string(v))
	case string:
		return c.parse(v)
	case nil:
		*c = 0 // NULL column -> zero; make Cents a pointer to distinguish
		return nil
	default:
		return fmt.Errorf("cannot scan %T into Cents", src)
	}
}

func (c *Cents) parse(s string) error {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("cannot scan %q into Cents: %w", s, err)
	}
	*c = Cents(n)
	return nil
}
```

### The runnable demo

The demo shows the round trip end to end: it computes a `Value`, then scans that
value back and prints the recovered amount, plus a NULL and a text scan.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/moneytype"
)

func main() {
	price := moneytype.Cents(1299)

	v, _ := price.Value() // int64(1299), the driver.Value form
	var back moneytype.Cents
	_ = back.Scan(v)
	fmt.Printf("round trip: %d -> %v -> %d\n", price, v, back)

	var fromText moneytype.Cents
	_ = fromText.Scan([]byte("500")) // driver handed a numeric column as bytes
	fmt.Printf("from bytes: %d\n", fromText)

	var fromNull moneytype.Cents
	_ = fromNull.Scan(nil) // NULL column
	fmt.Printf("from null:  %d\n", fromNull)

	var bad moneytype.Cents
	fmt.Printf("rejected:   %v\n", bad.Scan(3.14))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
round trip: 1299 -> 1299 -> 1299
from bytes: 500
from null:  0
rejected:   cannot scan float64 into Cents
```

### Tests

The round-trip test is the core property: whatever `Value` emits must scan back
to the original. The per-type subtests cover each concrete src, and the error
table covers the unsupported `float64` and a non-numeric `[]byte`.

Create `money_test.go`:

```go
// money_test.go
package moneytype

import "testing"

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	for _, want := range []Cents{0, 1, 1299, -500, 9_223_372_036_854_775_807} {
		v, err := want.Value()
		if err != nil {
			t.Fatalf("Value(%d): %v", want, err)
		}
		var got Cents
		if err := got.Scan(v); err != nil {
			t.Fatalf("Scan(%v): %v", v, err)
		}
		if got != want {
			t.Fatalf("round trip %d -> %d", want, got)
		}
	}
}

func TestScanSources(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  any
		want Cents
	}{
		{"int64", int64(1299), 1299},
		{"bytes", []byte("500"), 500},
		{"string", "-42", -42},
		{"nil is zero", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got Cents
			if err := got.Scan(tt.src); err != nil {
				t.Fatalf("Scan(%v): %v", tt.src, err)
			}
			if got != tt.want {
				t.Fatalf("Scan(%v) = %d, want %d", tt.src, got, tt.want)
			}
		})
	}
}

func TestScanRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  any
	}{
		{"float64", float64(3.14)},
		{"non-numeric bytes", []byte("abc")},
		{"bool", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var c Cents
			if err := c.Scan(tt.src); err == nil {
				t.Fatalf("Scan(%v) = nil error, want rejection", tt.src)
			}
		})
	}
}
```

## Review

The type is correct when `Value` and `Scan` are inverses over the integer domain
and every unsupported src is an error, not a panic. The property test proves the
round trip; the source subtests prove each driver form is handled; the reject
table proves the `default` and the non-numeric parse both fail loudly. The two
policy decisions worth stating explicitly: `float64` is rejected rather than
truncated because money in floating point is a defect class, and `nil` maps to
zero — if a column is nullable and you must distinguish "absent" from "zero", the
correct shape is a `*Cents` field or a `sql.Null`-style wrapper with a `Valid`
bool, and `Scan` sets `Valid = false` on `nil`. Forgetting the `nil` case is the
classic omission: a NULL column would otherwise hit `default` and error on every
nullable read.

## Resources

- [database/sql.Scanner](https://pkg.go.dev/database/sql#Scanner) — the `Scan(src any) error` contract.
- [database/sql/driver.Valuer and Value](https://pkg.go.dev/database/sql/driver#Valuer) — the canonical `driver.Value` types a driver accepts.
- [database/sql/driver.Value](https://pkg.go.dev/database/sql/driver#Value) — the fixed set of concrete types (`int64`, `float64`, `bool`, `[]byte`, `string`, `time.Time`, `nil`).

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-optional-interface-capability-check.md](05-optional-interface-capability-check.md)
