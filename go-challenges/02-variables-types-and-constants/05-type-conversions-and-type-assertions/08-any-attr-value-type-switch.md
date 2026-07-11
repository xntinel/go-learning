# Exercise 8: Encode Structured-Log Attributes With A Value Type Switch

A structured logger accepts attribute values as `any` and must render each one
into a typed, quoted output. The dispatch is a type switch over the dynamic
type — exactly what `slog.Value` performs internally. This exercise builds that
encoder, handling the concrete kinds, the `time.Time` and `error` and
`fmt.Stringer` interface cases, and a reflection-free default.

This module is fully self-contained: its own module, all code inline, its own
demo and tests.

## What you'll build

```text
attrenc/                     independent module: example.com/attrenc
  go.mod                     go 1.26
  attrenc.go                 EncodeAttr(v any) string via a type switch
  cmd/
    demo/
      main.go                runnable demo: encode a mix of attribute values
  attrenc_test.go            table mapping each concrete type -> expected encoding
```

- Files: `attrenc.go`, `cmd/demo/main.go`, `attrenc_test.go`.
- Implement: `EncodeAttr(v any) string` dispatching via a type switch over `string`, `bool`, the integer kinds, `float64`/`float32`, `time.Time`, `[]byte`, `error`, `fmt.Stringer`, and a default.
- Test: a table mapping each input concrete type to its expected encoded string, including a `Stringer`, an `error`, a `time.Time`, an unhandled struct hitting the default, and several integer kinds.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/attrenc/cmd/demo
cd ~/go-exercises/attrenc
go mod init example.com/attrenc
go mod edit -go=1.26
```

### The type switch, and why case order matters

`switch v := val.(type)` binds `v` to the matched dynamic type in each case, and
the cases are evaluated top to bottom with the first match winning. That ordering
rule is the whole design here. Concrete cases (`string`, `int64`, `time.Time`)
must precede the *interface* cases (`error`, `fmt.Stringer`), because a concrete
type frequently also satisfies an interface: a `time.Time` has a `String()`
method, so it is a `fmt.Stringer`; if `case fmt.Stringer` came first it would
capture the `time.Time` and you would lose the RFC3339 formatting. So `time.Time`
is listed before `fmt.Stringer`, and `error` (more specific intent than a generic
`Stringer`) before it too.

Each branch produces a quoted, unambiguous rendering using `strconv` rather than
`fmt` where it can, because `strconv.Quote`, `strconv.FormatInt`,
`strconv.FormatBool`, and `strconv.FormatFloat` are the exact primitives a
logging library uses for speed and predictable escaping. The integer kinds are
listed separately (`int`, `int8`, ... `int64` and the unsigned set) because Go
has no "any integer" case in a type switch — each concrete width is its own type,
and grouping them in one `case int, int64, uint32:` would leave `v` typed as
`any` again, defeating the purpose. Here they are grouped by signedness and
converted to the widest form for formatting. The `default` uses `fmt.Sprintf`
without reflection-heavy `%+v` field-walking as a last resort for a type the
logger does not specifically know.

Create `attrenc.go`:

```go
// attrenc.go
package attrenc

import (
	"fmt"
	"strconv"
	"time"
)

// EncodeAttr renders a structured-log attribute value into a quoted string,
// dispatching on its dynamic type. Case order puts concrete types before the
// error/Stringer interface cases so a time.Time is not captured as a Stringer.
func EncodeAttr(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return strconv.Quote(x)
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.FormatInt(int64(x), 10)
	case int8:
		return strconv.FormatInt(int64(x), 10)
	case int16:
		return strconv.FormatInt(int64(x), 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint:
		return strconv.FormatUint(uint64(x), 10)
	case uint8:
		return strconv.FormatUint(uint64(x), 10)
	case uint16:
		return strconv.FormatUint(uint64(x), 10)
	case uint32:
		return strconv.FormatUint(uint64(x), 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case time.Time:
		return strconv.Quote(x.Format(time.RFC3339))
	case []byte:
		return strconv.Quote(string(x))
	case error:
		return strconv.Quote(x.Error())
	case fmt.Stringer:
		return strconv.Quote(x.String())
	default:
		return strconv.Quote(fmt.Sprintf("%v", x))
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/attrenc"
)

func main() {
	ts := time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC)
	values := []any{
		"hello",
		true,
		int64(42),
		uint32(7),
		3.5,
		ts,
		[]byte("raw"),
		errors.New("boom"),
	}
	for _, v := range values {
		fmt.Printf("%-10T %s\n", v, attrenc.EncodeAttr(v))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
string     "hello"
bool       true
int64      42
uint32     7
float64    3.5
time.Time  "2026-07-02T15:04:05Z"
[]uint8    "raw"
*errors.errorString "boom"
```

### Tests

The table maps each concrete input to its expected encoded string, covering the
integer kinds, a `time.Time`, an `error`, a `fmt.Stringer`, and an unhandled
struct that must hit the default.

Create `attrenc_test.go`:

```go
// attrenc_test.go
package attrenc

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// role is a fmt.Stringer used to exercise the Stringer case.
type role int

func (r role) String() string { return fmt.Sprintf("role#%d", int(r)) }

// point has no special methods; it must fall to the default branch.
type point struct{ X, Y int }

func TestEncodeAttr(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC)
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, "null"},
		{"string", "hi", `"hi"`},
		{"string with quote", `a"b`, `"a\"b"`},
		{"bool", true, "true"},
		{"int", int(-5), "-5"},
		{"int64", int64(42), "42"},
		{"uint32", uint32(7), "7"},
		{"float64", 3.5, "3.5"},
		{"time", ts, `"2026-07-02T15:04:05Z"`},
		{"bytes", []byte("raw"), `"raw"`},
		{"error", errors.New("boom"), `"boom"`},
		{"stringer", role(3), `"role#3"`},
		{"default struct", point{1, 2}, `"{1 2}"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := EncodeAttr(tt.in); got != tt.want {
				t.Fatalf("EncodeAttr(%#v) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func ExampleEncodeAttr() {
	fmt.Println(EncodeAttr(int64(1024)))
	fmt.Println(EncodeAttr("path/to/file"))
	// Output:
	// 1024
	// "path/to/file"
}
```

## Review

The encoder is correct when every input maps to the intended rendering and case
order guarantees the interface cases never shadow a concrete case. The `time`
test is the proof: it must render as an RFC3339 timestamp, which only happens
because `case time.Time` precedes `case fmt.Stringer` — reorder those two and the
test fails as the `time.Time` is captured by the broader `Stringer` case. The
integer kinds are enumerated rather than merged into one case so `v` keeps a
concrete type in each branch; that is a genuine limitation of the type switch, not
a stylistic choice. The default branch exists so an unknown type degrades to a
readable value instead of panicking, which is what a real logger must do with the
arbitrary values callers pass it.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches) — the `switch v := x.(type)` form and case semantics.
- [log/slog.Value](https://pkg.go.dev/log/slog#Value) — how the standard structured logger models and dispatches attribute values.
- [strconv.Quote](https://pkg.go.dev/strconv#Quote) — producing a double-quoted, escaped Go string literal.
- [time.Time.Format and RFC3339](https://pkg.go.dev/time#Time.Format) — formatting a timestamp with a reference layout.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-typed-config-from-env.md](09-typed-config-from-env.md)
