# Exercise 9: Normalize database/sql driver.Value Rows Into Domain Types

A dynamic SQL query hands you back rows as `[]driver.Value` — `any` drawn from a
small, closed set of concrete types. Turning those into domain values means a type
switch over exactly that set, and the reverse direction (domain value to storable
column) is a `driver.Valuer`. Both are the same seam as the JSON walker, but the
closed set is different and worth memorizing.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
rowscan/                    independent module: example.com/rowscan
  go.mod                    module path
  rowscan.go                Cell; NormalizeCell/NormalizeRow over driver.Value; Money (driver.Valuer)
  cmd/
    demo/
      main.go               runnable demo normalizing a mixed row
  rowscan_test.go           one of each driver.Value type, out-of-set error, Valuer round-trip
```

Files: `rowscan.go`, `cmd/demo/main.go`, `rowscan_test.go`.
Implement: `Cell`, `NormalizeCell(driver.Value) (Cell, error)` and `NormalizeRow`, plus a `Money` domain type implementing `driver.Valuer`.
Test: a row with one of each `driver.Value` type plus `nil`; an out-of-set type (`int`) hitting the typed-error default; the `Valuer` round-tripping a domain type to an allowed value.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/rowscan/cmd/demo
cd ~/go-exercises/rowscan
go mod init example.com/rowscan
```

### The closed set, both directions

`database/sql/driver.Value` is documented as `any` restricted to exactly seven
concrete types: `nil`, `int64`, `float64`, `bool`, `[]byte`, `string`, and
`time.Time`. A driver that yields a dynamic row promises every element is one of
those, so `NormalizeCell` type-switches over precisely that set and puts anything
else in a `default` that returns `ErrUnsupportedType`. The out-of-set case is not
paranoia: a buggy or third-party driver that returns a bare `int` (not `int64`)
would otherwise be silently mishandled; the typed error makes it a diagnosable
failure. Note the deliberate absence of an `int` case — just like JSON's `float64`
rule, the integer type here is `int64` and only `int64`.

Each arm converts to a stable textual form: `[]byte` becomes a `string` (the common
case of a text column arriving as bytes), and `time.Time` is formatted as
`RFC3339`. The `Cell` records both a `Kind` tag and the `Str` value so a caller can
route on kind without re-asserting.

The reverse direction is `driver.Valuer`: a domain type implements
`Value() (driver.Value, error)` to convert itself into one of the seven allowed
shapes when the `database/sql` layer stores it. `Money` below holds integer cents
and returns an `int64` from `Value()`, so a monetary domain type round-trips
through the driver boundary without losing precision to `float64`. The pair
`NormalizeCell` and `Money.Value` are the two halves of the same closed-set
contract: read a `driver.Value` into a domain concept, write a domain concept back
into a `driver.Value`.

Create `rowscan.go`:

```go
package rowscan

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ErrUnsupportedType is returned when a column is not one of the seven
// driver.Value concrete types.
var ErrUnsupportedType = errors.New("unsupported driver value type")

// Cell is a normalized column: a kind tag and a stable string form.
type Cell struct {
	Kind string
	Str  string
}

// NormalizeCell converts one driver.Value into a Cell. It switches over the exact
// closed set (nil, int64, float64, bool, []byte, string, time.Time); anything else
// is ErrUnsupportedType. There is no int case: SQL integers arrive as int64.
func NormalizeCell(v driver.Value) (Cell, error) {
	switch x := v.(type) {
	case nil:
		return Cell{Kind: "null", Str: ""}, nil
	case int64:
		return Cell{Kind: "int", Str: strconv.FormatInt(x, 10)}, nil
	case float64:
		return Cell{Kind: "float", Str: strconv.FormatFloat(x, 'g', -1, 64)}, nil
	case bool:
		return Cell{Kind: "bool", Str: strconv.FormatBool(x)}, nil
	case []byte:
		return Cell{Kind: "bytes", Str: string(x)}, nil
	case string:
		return Cell{Kind: "string", Str: x}, nil
	case time.Time:
		return Cell{Kind: "time", Str: x.Format(time.RFC3339)}, nil
	default:
		return Cell{}, fmt.Errorf("%w: %T", ErrUnsupportedType, v)
	}
}

// NormalizeRow normalizes every column, annotating errors with the column index.
func NormalizeRow(row []driver.Value) ([]Cell, error) {
	cells := make([]Cell, len(row))
	for i, v := range row {
		c, err := NormalizeCell(v)
		if err != nil {
			return nil, fmt.Errorf("column %d: %w", i, err)
		}
		cells[i] = c
	}
	return cells, nil
}

// Money is a domain type stored as integer cents. It implements driver.Valuer so
// database/sql can store it as an int64 without float rounding.
type Money struct {
	Cents int64
}

func (m Money) Value() (driver.Value, error) {
	return m.Cents, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"database/sql/driver"
	"fmt"
	"time"

	"example.com/rowscan"
)

func main() {
	row := []driver.Value{
		int64(42),
		3.5,
		true,
		[]byte("hello"),
		"world",
		time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC),
		nil,
	}
	cells, err := rowscan.NormalizeRow(row)
	if err != nil {
		fmt.Println("normalize error:", err)
		return
	}
	for _, c := range cells {
		fmt.Printf("%s=%s\n", c.Kind, c.Str)
	}

	// Reverse direction: a domain type -> an allowed driver.Value.
	v, _ := rowscan.Money{Cents: 1099}.Value()
	fmt.Printf("money stored as %T = %v\n", v, v)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
int=42
float=3.5
bool=true
bytes=hello
string=world
time=2026-07-02T15:04:05Z
null=
money stored as int64 = 1099
```

### Tests

The table covers one of each `driver.Value` type. `TestRejectsOutOfSetType` feeds a
bare `int` — not a `driver.Value` — and asserts `ErrUnsupportedType`, guarding the
`default`. `TestMoneyValuerRoundTrips` proves the `Valuer` output is itself a valid
`driver.Value` that `NormalizeCell` reads back.

Create `rowscan_test.go`:

```go
package rowscan

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestNormalizeCell(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name     string
		in       driver.Value
		wantKind string
		wantStr  string
	}{
		{"nil", nil, "null", ""},
		{"int64", int64(42), "int", "42"},
		{"float64", 3.5, "float", "3.5"},
		{"bool", true, "bool", "true"},
		{"bytes", []byte("hi"), "bytes", "hi"},
		{"string", "hey", "string", "hey"},
		{"time", ts, "time", "2026-01-02T03:04:05Z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := NormalizeCell(tc.in)
			if err != nil {
				t.Fatalf("NormalizeCell(%s): %v", tc.name, err)
			}
			if c.Kind != tc.wantKind || c.Str != tc.wantStr {
				t.Fatalf("NormalizeCell(%s) = %+v, want kind=%s str=%s", tc.name, c, tc.wantKind, tc.wantStr)
			}
		})
	}
}

func TestRejectsOutOfSetType(t *testing.T) {
	t.Parallel()
	// A bare int is NOT a driver.Value; only int64 is allowed.
	_, err := NormalizeCell(int(5))
	if !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("err = %v, want ErrUnsupportedType", err)
	}

	// NormalizeRow annotates the failing column index.
	_, err = NormalizeRow([]driver.Value{"ok", int(5)})
	if !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("row err = %v, want ErrUnsupportedType", err)
	}
}

func TestMoneyValuerRoundTrips(t *testing.T) {
	t.Parallel()
	v, err := Money{Cents: 1099}.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if _, ok := v.(int64); !ok {
		t.Fatalf("Value returned %T, want int64", v)
	}
	c, err := NormalizeCell(v)
	if err != nil {
		t.Fatalf("NormalizeCell of Valuer output: %v", err)
	}
	if c.Kind != "int" || c.Str != "1099" {
		t.Fatalf("round-tripped cell = %+v, want int/1099", c)
	}
}

func ExampleNormalizeCell() {
	c, _ := NormalizeCell([]byte("payload"))
	fmt.Printf("%s %s\n", c.Kind, c.Str)
	// Output: bytes payload
}
```

## Review

The normalizer is correct when its type switch covers the seven `driver.Value`
concrete types and only those, with `int64` (never `int`) as the integer case, and
when an out-of-set value produces `ErrUnsupportedType` rather than a silent miss.
The `Valuer` closes the loop: `Money.Value` emits an `int64` that `NormalizeCell`
reads straight back, so the domain type survives a round trip through the driver
boundary with no float rounding. The recurring lesson across this exercise and the
JSON walker is the same — when a boundary hands you `any` from a closed set, switch
over the set exactly and make the `default` a typed error. Run `go test -race` to
confirm.

## Resources

- [database/sql/driver.Value](https://pkg.go.dev/database/sql/driver#Value) — the seven allowed concrete types.
- [database/sql/driver.Valuer](https://pkg.go.dev/database/sql/driver#Valuer)
- [time.Time.Format](https://pkg.go.dev/time#Time.Format) — the RFC3339 layout.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-loggable-value-detection.md](08-loggable-value-detection.md) | Next: [../04-common-standard-library-interfaces/00-concepts.md](../04-common-standard-library-interfaces/00-concepts.md)
