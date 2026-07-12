# Exercise 4: A JSONB Column Type Implementing `driver.Valuer` and `sql.Scanner`

The database boundary is where `any` is at its most disciplined: `driver.Value` is
`any` narrowed to a closed set of six dynamic types. This module builds a custom
`Attributes` type that round-trips as a JSON column by implementing `driver.Valuer`
(the write side) and `sql.Scanner` (the read side), handling every representation a
real driver might hand back.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
jsonbcol/                  independent module: example.com/jsonbcol
  go.mod                   go 1.26
  jsonbcol.go              type Attributes; Value() (driver.Valuer); Scan(src any) (sql.Scanner)
  cmd/
    demo/
      main.go              runnable demo: marshal to a column, scan []byte and string back
  jsonbcol_test.go         round-trip, []byte + string + nil src, typed error for int64 src
```

- Files: `jsonbcol.go`, `cmd/demo/main.go`, `jsonbcol_test.go`.
- Implement: `Attributes` (a `map[string]string`) with `Value() (driver.Value, error)` returning JSON `[]byte`, and `Scan(src any) error` accepting `[]byte`, `string`, and `nil`.
- Test: `Value()` then `Scan()` reconstruct an equal value; `Scan` accepts both `[]byte` and `string`, treats `nil` as empty without error, and returns a typed error (not a panic) for an `int64` src.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/02-empty-interface-and-any/04-sql-driver-valuer-scanner/cmd/demo
cd go-solutions/08-interfaces/02-empty-interface-and-any/04-sql-driver-valuer-scanner
go mod edit -go=1.26
```

### The closed set, and why Scan must accept several types

`database/sql/driver` documents `driver.Value` as one of exactly these dynamic
types: `nil`, `int64`, `float64`, `bool`, `[]byte`, `string`, and `time.Time`. A
type that wants to be stored in a column implements `driver.Valuer`: its `Value()`
must return one of those. For a JSON/JSONB column the natural choice is `[]byte`
(the marshaled JSON) — `database/sql` will hand that to the driver, which writes it
to the column.

Reading back is where the discipline matters. `sql.Scanner.Scan(src any)` receives
whatever the driver produced for that column, and drivers differ: the standard
`lib/pq` hands a `text`/`jsonb` column back as `[]byte`, some drivers hand it back
as `string`, and a SQL `NULL` arrives as `nil`. A correct `Scan` handles all three:
`[]byte` and `string` both unmarshal, `nil` resets to the zero value without error,
and anything else — an `int64`, say — returns a typed error rather than panicking on
a bad assertion. Two rules are easy to get wrong. First, `nil` is `NULL`, not an
error: a nullable column is normal. Second, you must not retain the `[]byte` src
past the return of `Scan` — the driver owns and reuses that buffer — so you either
copy it or, as here, unmarshal out of it immediately (unmarshaling copies the bytes
it needs into the destination map).

Create `jsonbcol.go`:

```go
package jsonbcol

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// ErrUnsupportedScan is returned by Scan for a src type outside the driver.Value
// set that Attributes knows how to decode.
var ErrUnsupportedScan = fmt.Errorf("jsonbcol: unsupported Scan source type")

// Attributes is a string map that round-trips as a JSON/JSONB column.
type Attributes map[string]string

// Value marshals the map to JSON []byte, one of the allowed driver.Value types.
func (a Attributes) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("jsonbcol: marshal: %w", err)
	}
	return b, nil
}

// Scan reconstructs the map from a column value. It accepts the two byte-ish
// representations drivers use ([]byte and string) and treats a NULL (nil) as an
// empty map. Any other src type is a typed error, never a panic. It does not retain
// the []byte src: json.Unmarshal copies what it needs into the destination.
func (a *Attributes) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*a = Attributes{}
		return nil
	case []byte:
		return a.unmarshal(v)
	case string:
		return a.unmarshal([]byte(v))
	default:
		return fmt.Errorf("%w: %T", ErrUnsupportedScan, src)
	}
}

func (a *Attributes) unmarshal(data []byte) error {
	if len(data) == 0 {
		*a = Attributes{}
		return nil
	}
	m := Attributes{}
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("jsonbcol: unmarshal: %w", err)
	}
	*a = m
	return nil
}
```

### The runnable demo

The demo marshals a value with `Value()`, then scans it back from both a `[]byte`
and a `string` (to show the two driver representations round-trip identically), and
finally scans a `nil` to show a NULL column becomes an empty map.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/jsonbcol"
)

func main() {
	orig := jsonbcol.Attributes{"role": "admin", "team": "payments"}

	v, _ := orig.Value()
	fmt.Printf("stored as %T: %s\n", v, v)

	var fromBytes jsonbcol.Attributes
	_ = fromBytes.Scan(v) // v is []byte
	fmt.Println("from []byte:", fromBytes["role"], fromBytes["team"])

	var fromString jsonbcol.Attributes
	_ = fromString.Scan(string(v.([]byte)))
	fmt.Println("from string:", fromString["role"], fromString["team"])

	var fromNull jsonbcol.Attributes
	_ = fromNull.Scan(nil)
	fmt.Println("from NULL len:", len(fromNull))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stored as []uint8: {"role":"admin","team":"payments"}
from []byte: admin payments
from string: admin payments
from NULL len: 0
```

### Tests

`TestRoundTrip` marshals with `Value()` and scans the result back, asserting
`reflect.DeepEqual` equality. `TestScanSourceTypes` is table-driven over the src
representations a driver might use — `[]byte`, `string`, and `nil` — asserting each
reconstructs correctly. `TestScanRejectsUnsupported` proves an `int64` src returns
`ErrUnsupportedScan` via `errors.Is`, not a panic. `TestScanDoesNotRetainSrc`
mutates the source buffer after `Scan` and proves the stored map is unaffected,
which is the "do not retain the driver's `[]byte`" contract.

Create `jsonbcol_test.go`:

```go
package jsonbcol

import (
	"errors"
	"reflect"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	orig := Attributes{"role": "admin", "team": "payments"}

	v, err := orig.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	b, ok := v.([]byte)
	if !ok {
		t.Fatalf("Value returned %T, want []byte", v)
	}

	var got Attributes
	if err := got.Scan(b); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !reflect.DeepEqual(got, orig) {
		t.Fatalf("round-trip = %v, want %v", got, orig)
	}
}

func TestScanSourceTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  any
		want Attributes
	}{
		{"bytes", []byte(`{"k":"v"}`), Attributes{"k": "v"}},
		{"string", `{"k":"v"}`, Attributes{"k": "v"}},
		{"nil is empty", nil, Attributes{}},
		{"empty bytes", []byte{}, Attributes{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got Attributes
			if err := got.Scan(tc.src); err != nil {
				t.Fatalf("Scan(%v): %v", tc.src, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Scan(%v) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

func TestScanRejectsUnsupported(t *testing.T) {
	t.Parallel()

	var a Attributes
	err := a.Scan(int64(7))
	if err == nil {
		t.Fatal("Scan(int64) returned nil error")
	}
	if !errors.Is(err, ErrUnsupportedScan) {
		t.Fatalf("Scan(int64) error = %v, want ErrUnsupportedScan", err)
	}
}

func TestScanDoesNotRetainSrc(t *testing.T) {
	t.Parallel()

	// A driver reuses its buffer. Mutating src after Scan must not corrupt the map.
	src := []byte(`{"k":"v"}`)
	var a Attributes
	if err := a.Scan(src); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for i := range src {
		src[i] = 'x'
	}
	if a["k"] != "v" {
		t.Fatalf("map corrupted after src mutation: %v", a)
	}
}
```

## Review

The type is correct at the boundary when `Value()` returns one of the six allowed
`driver.Value` types (here `[]byte`) and `Scan` accepts every representation a real
driver might produce: `[]byte`, `string`, and `nil` for `NULL`. The
`TestScanRejectsUnsupported` case proves the failure path is a typed error matchable
with `errors.Is`, not a panic — a `Scan` that asserts `src.([]byte)` unconditionally
crashes on the first driver that returns a `string`. `TestScanDoesNotRetainSrc` pins
the subtle rule that the driver owns the `[]byte`: unmarshaling out of it immediately
(rather than storing it) is what keeps the map safe after the driver reuses its
buffer. Run `go test -race` to confirm the round-trip and every src type behave.

## Resources

- [`database/sql/driver.Valuer`](https://pkg.go.dev/database/sql/driver#Valuer) — `Value()` and the allowed `driver.Value` types.
- [`database/sql/driver.Value`](https://pkg.go.dev/database/sql/driver#Value) — the closed set of dynamic types.
- [`database/sql.Scanner`](https://pkg.go.dev/database/sql#Scanner) — `Scan(src any)` and the note that the src is only valid until `Scan` returns.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-dynamic-config-coercion.md](05-dynamic-config-coercion.md)
