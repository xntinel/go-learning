# Exercise 1: A JSON Value Wrapper over `any` with a Type-Switch Stringer

The canonical shape of a boundary value is a small type that wraps `any` and hands
back its content through safe, typed accessors. This module builds a `Value` that
holds an opaque `any`, renders it with a type switch that covers every JSON-ish
dynamic type, and exposes comma-ok accessors that recover an `int64` or a `string`
without ever panicking on the wrong type.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
jsonvalue/                 independent module: example.com/jsonvalue
  go.mod                   go 1.26
  value.go                 type Value wraps any; String() via type switch; AsInt/AsString comma-ok
  cmd/
    demo/
      main.go              runnable demo: format scalars and a nested slice
  value_test.go            table-driven Stringer, recursive slice, AsInt/AsString accept+reject
```

- Files: `value.go`, `cmd/demo/main.go`, `value_test.go`.
- Implement: a `Value` wrapping `any`, a `String()` `Stringer` built from a type switch over `nil`/`string`/`bool`/the integer family/the float family/`[]byte`/`[]Value`, and `AsInt`/`AsString` comma-ok accessors.
- Test: table-driven formatting per dynamic type, recursive `[]Value` formatting (including a nested slice), `AsInt` accepting the whole integer family, and `AsInt`/`AsString` rejecting the wrong type without panicking.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why a type switch, and why comma-ok accessors

A `Value` is opaque by design: it holds whatever a JSON decoder or a config loader
produced, and its job is to render or extract that content safely. `String()` is a
`fmt.Stringer`, so anything that formats a `Value` — `fmt.Println`, a `%s` verb,
a log line — routes through one method. Inside it, a type switch gives each case a
`v` already typed to that case, which is exactly what formatting needs: `%d` for
integers, `%g` for floats, a literal `"null"` for `nil`, `string(x)` for a
`[]byte`. The `[]Value` case is the interesting one: it recurses, formatting each
element through the same `String()`, so a slice of values — including a slice of
slices — renders correctly. The `default` case is the safety net that catches any
dynamic type the switch did not name and formats it with `%v` rather than dropping
it silently.

`AsInt` and `AsString` are the extraction half. They use the comma-ok assertion
form, which is the production default: on a type mismatch they return the zero
value and `false`, never a panic. `AsInt` deliberately accepts the entire signed
integer family — `int`, `int8`, `int16`, `int32`, `int64` — and normalizes to
`int64`, because a value that arrived as any of those is semantically an integer
and a caller should not care which width the boundary happened to use. A caller
that asks for an int from a value holding a string simply gets `false` and handles
it, which is how a boundary type must behave.

Create `value.go`:

```go
package jsonvalue

import (
	"fmt"
	"strings"
)

// Value wraps an opaque any produced at a boundary (a decoded JSON value, a
// config leaf) and exposes safe, typed access to it.
type Value struct {
	data any
}

// New boxes v into a Value.
func New(v any) Value {
	return Value{data: v}
}

// String renders the value. It is a fmt.Stringer: every dynamic type the boundary
// can produce has an explicit case, and the default case formats the unknown
// defensively rather than dropping it.
func (v Value) String() string {
	switch x := v.data.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int, int8, int16, int32, int64:
		return fmt.Sprintf("%d", x)
	case float32, float64:
		return fmt.Sprintf("%g", x)
	case []byte:
		return string(x)
	case []Value:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = e.String()
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		return fmt.Sprintf("%v", x)
	}
}

// AsInt reports the value as an int64 if its dynamic type is any signed integer
// type, else (0, false). It never panics on a mismatch.
func (v Value) AsInt() (int64, bool) {
	switch x := v.data.(type) {
	case int:
		return int64(x), true
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	default:
		return 0, false
	}
}

// AsString reports the value as a string if its dynamic type is string, else
// ("", false).
func (v Value) AsString() (string, bool) {
	s, ok := v.data.(string)
	return s, ok
}
```

### The runnable demo

The demo formats a few scalar values and one nested slice, so you can watch the
recursive `String()` render a slice of slices in one line.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/jsonvalue"
)

func main() {
	fmt.Println(jsonvalue.New("alice"))
	fmt.Println(jsonvalue.New(int64(42)))
	fmt.Println(jsonvalue.New(nil))

	nested := jsonvalue.New([]jsonvalue.Value{
		jsonvalue.New("a"),
		jsonvalue.New([]jsonvalue.Value{jsonvalue.New(1), jsonvalue.New(true)}),
	})
	fmt.Println(nested)

	if n, ok := jsonvalue.New(int32(7)).AsInt(); ok {
		fmt.Printf("as int: %d\n", n)
	}
	if _, ok := jsonvalue.New("x").AsInt(); !ok {
		fmt.Println("string is not an int: ok=false")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alice
42
null
[a,[1,true]]
as int: 7
string is not an int: ok=false
```

### Tests

The table-driven `TestValueStringForCommonTypes` pins the formatting for every
scalar dynamic type. `TestValueStringForSlice` and `TestValueStringForNestedSlice`
pin the recursive `[]Value` contract — the nested case is the one that proves
`String()` truly recurses rather than special-casing one level.
`TestAsIntAcceptsIntegerTypes` walks the whole signed-integer family, and
`TestAsIntRejectsNonInteger` / `TestAsStringRejectsNonString` prove the comma-ok
accessors return `false` instead of panicking on a mismatch.

Create `value_test.go`:

```go
package jsonvalue

import (
	"fmt"
	"testing"
)

func TestValueStringForCommonTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, "null"},
		{"string", "hello", "hello"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"int", int(42), "42"},
		{"int64", int64(42), "42"},
		{"float64", float64(3.5), "3.5"},
		{"bytes", []byte("raw"), "raw"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := New(tc.in).String()
			if got != tc.want {
				t.Fatalf("New(%v).String() = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValueStringForSlice(t *testing.T) {
	t.Parallel()

	v := New([]Value{New("a"), New(1), New(true)})
	want := "[a,1,true]"
	if got := v.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestValueStringForNestedSlice(t *testing.T) {
	t.Parallel()

	// A slice of values, one of which is itself a slice of values, one of which
	// is again a slice — three levels deep pins the recursion.
	v := New([]Value{
		New("a"),
		New([]Value{
			New(1),
			New([]Value{New(true), New("z")}),
		}),
	})
	want := "[a,[1,[true,z]]]"
	if got := v.String(); got != want {
		t.Fatalf("nested String() = %q, want %q", got, want)
	}
}

func TestAsIntAcceptsIntegerTypes(t *testing.T) {
	t.Parallel()

	tests := map[any]int64{
		int(42):   42,
		int8(42):  42,
		int16(42): 42,
		int32(42): 42,
		int64(42): 42,
	}
	for in, want := range tests {
		got, ok := New(in).AsInt()
		if !ok {
			t.Fatalf("AsInt(%v) returned false", in)
		}
		if got != want {
			t.Fatalf("AsInt(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestAsIntRejectsNonInteger(t *testing.T) {
	t.Parallel()

	if _, ok := New("hello").AsInt(); ok {
		t.Fatal("AsInt on string should return false")
	}
	if _, ok := New(nil).AsInt(); ok {
		t.Fatal("AsInt on nil should return false")
	}
	if _, ok := New(3.5).AsInt(); ok {
		t.Fatal("AsInt on float should return false")
	}
}

func TestAsStringAcceptsString(t *testing.T) {
	t.Parallel()

	got, ok := New("hello").AsString()
	if !ok {
		t.Fatal("AsString on string should return true")
	}
	if got != "hello" {
		t.Fatalf("AsString = %q, want hello", got)
	}
}

func TestAsStringRejectsNonString(t *testing.T) {
	t.Parallel()

	if _, ok := New(42).AsString(); ok {
		t.Fatal("AsString on int should return false")
	}
}

func ExampleValue_String() {
	v := New([]Value{New("a"), New(1), New(true)})
	fmt.Println(v)
	// Output: [a,1,true]
}
```

## Review

The `Value` is correct when `String()` has an explicit case for every dynamic type
the boundary can produce and a `default` that formats the rest defensively rather
than dropping it — the nested-slice test is the proof that the `[]Value` case truly
recurses. The accessors are correct when a mismatch yields `false`, never a panic:
`AsInt` on a string, a `nil`, or a float must all return `false`, which is why the
comma-ok form is mandatory here. The most common mistake is reaching for the
panic-form assertion `v.data.(int64)` in an accessor "because it should be an int"
— one unexpected boundary value then crashes the caller instead of letting it
handle a clean `false`. The second is forgetting the whole integer family: a value
that arrived as `int32` is still an integer, and a boundary type that only accepts
`int64` rejects perfectly valid data. Run `go test -race` to confirm the type
switch and accessors behave under the full table.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches) — case bodies typed to each case.
- [Go Specification: Type assertions](https://go.dev/ref/spec#Type_assertions) — the comma-ok form and when the single-return form panics.
- [`fmt.Stringer`](https://pkg.go.dev/fmt#Stringer) — the interface `String()` satisfies.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-decode-arbitrary-json.md](02-decode-arbitrary-json.md)
