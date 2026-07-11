# Exercise 3: Implement sql.Scanner over Driver Value Types

`database/sql` hands your `Scanner` a value typed as `any` that is guaranteed to
be one of exactly seven driver types. A custom column type implements `Scan` by
type-switching over precisely that vocabulary and rejecting anything else. This
is the database boundary, and getting the `[]byte` handling wrong corrupts data
silently.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
nulljson/                    independent module: example.com/nulljson
  go.mod                     go 1.26
  nulljson.go                type NullableJSON: Scan(src any) error, Value() (driver.Value, error)
  cmd/
    demo/
      main.go                scans several driver values, prints the stored JSON
  nulljson_test.go           seven driver types + complex128, []byte/string parity, nil, round-trip, copy
```

- Files: `nulljson.go`, `cmd/demo/main.go`, `nulljson_test.go`.
- Implement: `NullableJSON` implementing `sql.Scanner` (`Scan(src any) error`)
  over the driver value set (`nil`, `int64`, `float64`, `bool`, `[]byte`,
  `string`, `time.Time`) and `driver.Valuer` (`Value() (driver.Value, error)`).
- Test: each of the seven driver types plus an unsupported `complex128`; `[]byte`
  and `string` produce identical results; `nil` maps to the null state; a
  round-trip `Value()` then `Scan()` is identity; a retained `[]byte` is copied.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/nulljson/cmd/demo
cd ~/go-exercises/nulljson
go mod init example.com/nulljson
```

## The fixed driver vocabulary, and the []byte ownership trap

`database/sql/driver.Value` is documented to be one of exactly seven Go types:
`nil`, `int64`, `float64`, `bool`, `[]byte`, `string`, and `time.Time`. A
`Scanner.Scan(src any)` implementation therefore has a *closed* set to switch
over — this is the rare boundary where the type set is truly fixed, which is
exactly when a type switch (over reflection) is the right tool. Anything outside
that set means the driver or a wrapper handed you something lossy, and the
correct response is a loss-of-information error, not a best-effort guess.

`NullableJSON` stores raw JSON bytes plus a `Valid` flag (the nullable pattern
from `sql.NullString`). `Scan` renders each driver type into its JSON form:
`nil` becomes the null state; `[]byte` and `string` are taken as already-JSON and
must produce identical results; `int64`, `float64`, and `bool` are rendered with
`strconv`; `time.Time` becomes a quoted RFC3339 string.

The trap the concepts file warned about lives here: **the driver owns the
`[]byte` it passes to `Scan` and reuses that backing array on the next row.** If
you retain the slice directly, your stored value mutates when the next row is
scanned. You must copy it. `bytes.Clone` (Go 1.20+) is the idiomatic copy;
`append([]byte(nil), src...)` is the older form. The test proves this by mutating
the source slice after `Scan` and asserting the stored value is unchanged.

Create `nulljson.go`:

```go
package nulljson

import (
	"bytes"
	"database/sql/driver"
	"fmt"
	"strconv"
	"time"
)

// NullableJSON is a JSON column that may be SQL NULL. It implements sql.Scanner
// and driver.Valuer over the fixed driver value vocabulary.
type NullableJSON struct {
	Data  []byte // raw JSON when Valid
	Valid bool
}

// Scan implements sql.Scanner. src is one of the driver value types; anything
// else is rejected as a loss of information.
func (n *NullableJSON) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		n.Data, n.Valid = nil, false
		return nil
	case []byte:
		// The driver owns v and reuses it next row; copy before retaining.
		n.Data, n.Valid = bytes.Clone(v), true
		return nil
	case string:
		n.Data, n.Valid = []byte(v), true
		return nil
	case int64:
		n.Data, n.Valid = strconv.AppendInt(nil, v, 10), true
		return nil
	case float64:
		n.Data, n.Valid = strconv.AppendFloat(nil, v, 'g', -1, 64), true
		return nil
	case bool:
		n.Data, n.Valid = strconv.AppendBool(nil, v), true
		return nil
	case time.Time:
		n.Data, n.Valid = strconv.AppendQuote(nil, v.Format(time.RFC3339)), true
		return nil
	default:
		return fmt.Errorf("nulljson: cannot scan %T into NullableJSON", src)
	}
}

// Value implements driver.Valuer. A null column returns a nil driver.Value; a
// valid one returns a fresh copy of the raw JSON as []byte.
func (n NullableJSON) Value() (driver.Value, error) {
	if !n.Valid {
		return nil, nil
	}
	return bytes.Clone(n.Data), nil
}
```

## The runnable demo

The demo scans one of each driver kind through a `NullableJSON` and prints the
stored JSON, showing that heterogeneous driver values all land as canonical JSON.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/nulljson"
)

func main() {
	srcs := []any{
		nil,
		int64(42),
		3.5,
		true,
		[]byte(`{"k":1}`),
		"hello",
		time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC),
	}
	for _, src := range srcs {
		var n nulljson.NullableJSON
		if err := n.Scan(src); err != nil {
			fmt.Printf("%T -> error: %v\n", src, err)
			continue
		}
		if !n.Valid {
			fmt.Printf("%T -> NULL\n", src)
			continue
		}
		fmt.Printf("%T -> %s\n", src, n.Data)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
<nil> -> NULL
int64 -> 42
float64 -> 3.5
bool -> true
[]uint8 -> {"k":1}
string -> hello
time.Time -> "2026-07-02T15:04:05Z"
```

## Tests

The table test feeds each of the seven driver types plus a `complex128` and
asserts the decoded JSON or a typed error. A dedicated test proves `[]byte` and
`string` inputs produce identical `Data`. The nil test asserts the null state.
The round-trip test scans a value, calls `Value()`, scans the result again, and
asserts identity. The copy test mutates the source slice after `Scan` and asserts
the stored bytes did not change — the proof that `Scan` copied.

Create `nulljson_test.go`:

```go
package nulljson

import (
	"bytes"
	"testing"
	"time"
)

func TestScanDriverTypes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		src     any
		want    string
		wantErr bool
	}{
		{"nil", nil, "", false},
		{"int64", int64(42), "42", false},
		{"float64", 3.5, "3.5", false},
		{"bool", true, "true", false},
		{"bytes", []byte(`{"k":1}`), `{"k":1}`, false},
		{"string", "hello", "hello", false},
		{"time", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), `"2026-01-02T03:04:05Z"`, false},
		{"unsupported", complex128(1 + 2i), "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var n NullableJSON
			err := n.Scan(tc.src)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Scan(%T) err = nil, want error", tc.src)
				}
				return
			}
			if err != nil {
				t.Fatalf("Scan(%T): %v", tc.src, err)
			}
			if tc.src == nil {
				if n.Valid {
					t.Fatalf("nil scanned to Valid=true")
				}
				return
			}
			if string(n.Data) != tc.want {
				t.Fatalf("Data = %s, want %s", n.Data, tc.want)
			}
		})
	}
}

func TestBytesAndStringParity(t *testing.T) {
	t.Parallel()
	var a, b NullableJSON
	if err := a.Scan([]byte(`[1,2,3]`)); err != nil {
		t.Fatal(err)
	}
	if err := b.Scan(`[1,2,3]`); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Data, b.Data) {
		t.Fatalf("[]byte gave %s, string gave %s", a.Data, b.Data)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	var in NullableJSON
	if err := in.Scan([]byte(`{"x":true}`)); err != nil {
		t.Fatal(err)
	}
	v, err := in.Value()
	if err != nil {
		t.Fatal(err)
	}
	var out NullableJSON
	if err := out.Scan(v); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in.Data, out.Data) || in.Valid != out.Valid {
		t.Fatalf("round-trip: in=%s out=%s", in.Data, out.Data)
	}
}

func TestScanCopiesBytes(t *testing.T) {
	t.Parallel()
	src := []byte(`{"a":1}`)
	var n NullableJSON
	if err := n.Scan(src); err != nil {
		t.Fatal(err)
	}
	// The driver reuses src for the next row; mutate it to prove Scan copied.
	src[2] = 'X'
	if string(n.Data) != `{"a":1}` {
		t.Fatalf("stored value mutated with source: %s", n.Data)
	}
}
```

## Review

The scanner is correct when every one of the seven driver types decodes to
canonical JSON, when `complex128` (or any non-driver type) yields a typed
loss-of-information error, and when a scanned `[]byte` survives a later mutation
of the source — the copy is not optional. The subtle bugs are two. Forgetting to
copy the `[]byte` gives a value that mutates when the next row is scanned, which
shows up as impossible-to-reproduce data corruption under load. And treating
`string` and `[]byte` as different value shapes (rather than routing both to the
same JSON) makes the column behave differently across drivers, since some pass
text as `string` and some as `[]byte`. Route them to identical results.

## Resources

- [database/sql/driver.Value (allowed driver value types)](https://pkg.go.dev/database/sql/driver#Value)
- [database/sql.Scanner (Scan(src any) contract)](https://pkg.go.dev/database/sql#Scanner)
- [database/sql/driver.Valuer](https://pkg.go.dev/database/sql/driver#Valuer)
- [bytes.Clone](https://pkg.go.dev/bytes#Clone)

---

Prev: [02-json-tree-redactor.md](02-json-tree-redactor.md) | Up: [00-concepts.md](00-concepts.md) | Next: [04-error-retry-classifier.md](04-error-retry-classifier.md)
