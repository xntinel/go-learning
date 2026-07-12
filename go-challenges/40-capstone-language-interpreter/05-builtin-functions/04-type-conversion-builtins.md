# Exercise 4: Type-Conversion Built-ins

A dynamically typed language needs explicit conversions: parse a string into a number, render a number as a string, coerce a value to a boolean, ask what type a value is. These six built-ins — `type`, `int`, `float`, `str`, `bool`, `isNull` — are where the runtime's type tags meet user intent. The subtle ones are `int`, which truncates a float and parses a string but returns an error on garbage, and `bool`, which applies the language's truthiness rule rather than a numeric test.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
object.go      the runtime Object types, singletons, BoolObj
registry.go    Builtin, RegisterBuiltin, Dispatch, newError
convert.go     type, int, float, str, bool, isNull, isTruthy and registration
cmd/
  demo/
    main.go    a "42" -> int -> float round-trip and a type probe
convert_test.go  truncation, parse failure, truthiness coercion
```

- Files: `object.go`, `registry.go`, `convert.go`, `cmd/demo/main.go`, `convert_test.go`.
- Implement: `type`, `int`, `float`, `str`, `bool`, `isNull`, the `isTruthy` helper, and the registry framework.
- Test: that `int` truncates a float and errors on a non-numeric string, that `float` parses and errors symmetrically, that `str` renders any value, and that `bool`/`isNull` follow the truthiness and null rules.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/40-capstone-language-interpreter/05-builtin-functions/04-type-conversion-builtins/cmd/demo && cd go-solutions/40-capstone-language-interpreter/05-builtin-functions/04-type-conversion-builtins
```

### Conversions that can fail, and conversions that cannot

`type`, `str`, `bool`, and `isNull` are total: every value has a type name, a string rendering, a truthiness, and a null-ness, so these never error. `int` and `float` are partial: a string argument may not be a valid number, so they return an `*Error` rather than a silent zero. This is the important design choice — `int("notanumber")` must surface a diagnostic, not produce `0` and hide the bug. The parse goes through `strconv.ParseInt` / `strconv.ParseFloat`, and a parse error is translated into the language's error value.

The two coercions that do not parse are still worth attention. `int` on a float truncates toward zero (`int(3.9)` is `3`, matching Go's `int64(3.9)`), not rounds — rounding is a separate `round` built-in. `bool` does not test "is this nonzero"; it applies `isTruthy`, the same rule the evaluator uses for `if`, where only `null` and `false` are falsy and every other value — including `0` and `""` — is truthy. Keeping `bool` aligned with `if` is what stops `bool(x)` and `if (x)` from disagreeing.

Create `object.go`:

```go
package convert

import (
	"fmt"
	"strings"
)

// ObjectType names the runtime kind of an Object.
type ObjectType string

const (
	INTEGER_OBJ ObjectType = "INTEGER"
	FLOAT_OBJ   ObjectType = "FLOAT"
	STRING_OBJ  ObjectType = "STRING"
	BOOLEAN_OBJ ObjectType = "BOOLEAN"
	NULL_OBJ    ObjectType = "NULL"
	ARRAY_OBJ   ObjectType = "ARRAY"
	ERROR_OBJ   ObjectType = "ERROR"
)

// Object is the runtime value interface shared across the interpreter.
type Object interface {
	Type() ObjectType
	Inspect() string
}

// Integer holds an int64 value.
type Integer struct{ Value int64 }

func (i *Integer) Type() ObjectType { return INTEGER_OBJ }
func (i *Integer) Inspect() string  { return fmt.Sprintf("%d", i.Value) }

// Float holds a float64 value.
type Float struct{ Value float64 }

func (f *Float) Type() ObjectType { return FLOAT_OBJ }
func (f *Float) Inspect() string  { return fmt.Sprintf("%g", f.Value) }

// String holds a Go string value.
type String struct{ Value string }

func (s *String) Type() ObjectType { return STRING_OBJ }
func (s *String) Inspect() string  { return s.Value }

// Boolean holds a bool value.
type Boolean struct{ Value bool }

func (b *Boolean) Type() ObjectType { return BOOLEAN_OBJ }
func (b *Boolean) Inspect() string  { return fmt.Sprintf("%t", b.Value) }

// Null represents the absence of a value.
type Null struct{}

func (n *Null) Type() ObjectType { return NULL_OBJ }
func (n *Null) Inspect() string  { return "null" }

// Array holds an ordered list of Objects.
type Array struct{ Elements []Object }

func (a *Array) Type() ObjectType { return ARRAY_OBJ }
func (a *Array) Inspect() string {
	parts := make([]string, len(a.Elements))
	for i, e := range a.Elements {
		parts[i] = e.Inspect()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// Error is a runtime error value that propagates without panicking.
type Error struct{ Message string }

func (e *Error) Type() ObjectType { return ERROR_OBJ }
func (e *Error) Inspect() string  { return "ERROR: " + e.Message }

// Singletons avoid allocating new booleans and nulls on every built-in call.
var (
	NullVal  = &Null{}
	TrueVal  = &Boolean{Value: true}
	FalseVal = &Boolean{Value: false}
)

// BoolObj returns the singleton Boolean for v.
func BoolObj(v bool) *Boolean {
	if v {
		return TrueVal
	}
	return FalseVal
}
```

### The registry framework

Create `registry.go`:

```go
package convert

import "fmt"

// BuiltinFunction is the signature every built-in satisfies.
type BuiltinFunction func(args ...Object) Object

// Builtin holds a function with its arity contract and documentation.
type Builtin struct {
	Name    string
	MinArgs int // -1 means no lower bound
	MaxArgs int // -1 means no upper bound (variadic)
	Doc     string
	Fn      BuiltinFunction
}

// BuiltinOption modifies a Builtin at registration time.
type BuiltinOption func(*Builtin)

// WithArity sets the inclusive [min, max] argument-count bounds.
func WithArity(min, max int) BuiltinOption {
	return func(b *Builtin) { b.MinArgs = min; b.MaxArgs = max }
}

// WithDoc attaches a one-line documentation string.
func WithDoc(doc string) BuiltinOption {
	return func(b *Builtin) { b.Doc = doc }
}

// Registry maps built-in names to their Builtin descriptors.
var Registry = make(map[string]*Builtin)

// RegisterBuiltin adds fn to the Registry under name.
func RegisterBuiltin(name string, fn BuiltinFunction, opts ...BuiltinOption) {
	b := &Builtin{Name: name, MinArgs: -1, MaxArgs: -1, Fn: fn}
	for _, opt := range opts {
		opt(b)
	}
	Registry[name] = b
}

// Lookup returns the Builtin for name, or nil if not registered.
func Lookup(name string) *Builtin { return Registry[name] }

// Dispatch validates arity, then calls the named built-in.
func Dispatch(name string, args ...Object) Object {
	b, ok := Registry[name]
	if !ok {
		return newError("undefined builtin: %q", name)
	}
	if err := checkArgs(b, args); err != nil {
		return err
	}
	return b.Fn(args...)
}

// checkArgs validates len(args) against b's arity contract.
func checkArgs(b *Builtin, args []Object) *Error {
	n := len(args)
	if b.MinArgs >= 0 && n < b.MinArgs {
		return newError("%s: want at least %d arg(s), got %d", b.Name, b.MinArgs, n)
	}
	if b.MaxArgs >= 0 && n > b.MaxArgs {
		return newError("%s: want at most %d arg(s), got %d", b.Name, b.MaxArgs, n)
	}
	return nil
}

func newError(format string, a ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, a...)}
}
```

### The conversion built-ins

Create `convert.go`:

```go
package convert

import "strconv"

func init() {
	RegisterBuiltin("type", builtinType, WithArity(1, 1),
		WithDoc("type(obj) – return the type name as a string"))
	RegisterBuiltin("int", builtinInt, WithArity(1, 1),
		WithDoc("int(obj) – convert string or float to integer"))
	RegisterBuiltin("float", builtinFloat, WithArity(1, 1),
		WithDoc("float(obj) – convert string or integer to float"))
	RegisterBuiltin("str", builtinStr, WithArity(1, 1),
		WithDoc("str(obj) – convert any value to its string representation"))
	RegisterBuiltin("bool", builtinBool, WithArity(1, 1),
		WithDoc("bool(obj) – convert to boolean using truthiness rules"))
	RegisterBuiltin("isNull", builtinIsNull, WithArity(1, 1),
		WithDoc("isNull(obj) – return true if obj is null"))
}

func builtinType(args ...Object) Object {
	return &String{Value: string(args[0].Type())}
}

func builtinInt(args ...Object) Object {
	switch obj := args[0].(type) {
	case *Integer:
		return obj
	case *Float:
		return &Integer{Value: int64(obj.Value)}
	case *String:
		n, err := strconv.ParseInt(obj.Value, 10, 64)
		if err != nil {
			return newError("int: cannot convert %q to INTEGER", obj.Value)
		}
		return &Integer{Value: n}
	default:
		return newError("int: unsupported type %s", args[0].Type())
	}
}

func builtinFloat(args ...Object) Object {
	switch obj := args[0].(type) {
	case *Float:
		return obj
	case *Integer:
		return &Float{Value: float64(obj.Value)}
	case *String:
		f, err := strconv.ParseFloat(obj.Value, 64)
		if err != nil {
			return newError("float: cannot convert %q to FLOAT", obj.Value)
		}
		return &Float{Value: f}
	default:
		return newError("float: unsupported type %s", args[0].Type())
	}
}

func builtinStr(args ...Object) Object {
	return &String{Value: args[0].Inspect()}
}

func builtinBool(args ...Object) Object {
	return BoolObj(isTruthy(args[0]))
}

func builtinIsNull(args ...Object) Object {
	return BoolObj(args[0].Type() == NULL_OBJ)
}

func isTruthy(obj Object) bool {
	switch o := obj.(type) {
	case *Null:
		return false
	case *Boolean:
		return o.Value
	default:
		return true
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/convert"
)

func main() {
	n := &convert.String{Value: "42"}
	asInt := convert.Dispatch("int", n)
	asFloat := convert.Dispatch("float", asInt)
	fmt.Println("\"42\" -> int:  ", asInt.Inspect())
	fmt.Println("\"42\" -> float:", asFloat.Inspect())

	fmt.Println("type(42):   ", convert.Dispatch("type", &convert.Integer{Value: 42}).Inspect())
	fmt.Println("str(42):    ", convert.Dispatch("str", &convert.Integer{Value: 42}).Inspect())
	fmt.Println("bool(null): ", convert.Dispatch("bool", convert.NullVal).Inspect())
	fmt.Println("bool(0):    ", convert.Dispatch("bool", &convert.Integer{Value: 0}).Inspect())
	fmt.Println("isNull(null):", convert.Dispatch("isNull", convert.NullVal).Inspect())

	bad := convert.Dispatch("int", &convert.String{Value: "notanumber"})
	fmt.Println("int(\"notanumber\"):", bad.Inspect())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"42" -> int:   42
"42" -> float: 42
type(42):    INTEGER
str(42):     42
bool(null):  false
bool(0):     true
isNull(null): true
int("notanumber"): ERROR: int: cannot convert "notanumber" to INTEGER
```

### Tests

Create `convert_test.go`:

```go
package convert

import (
	"fmt"
	"testing"
)

func TestRegistryPopulated(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"type", "int", "float", "str", "bool", "isNull"} {
		if Lookup(name) == nil {
			t.Errorf("built-in %q not registered", name)
		}
	}
}

func TestBuiltinType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		obj  Object
		want string
	}{
		{&Integer{Value: 1}, "INTEGER"},
		{&Float{Value: 1.5}, "FLOAT"},
		{&String{Value: "x"}, "STRING"},
		{&Boolean{Value: true}, "BOOLEAN"},
		{NullVal, "NULL"},
		{&Array{Elements: nil}, "ARRAY"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			result := Dispatch("type", tc.obj)
			if result.(*String).Value != tc.want {
				t.Fatalf("type = %q, want %q", result.(*String).Value, tc.want)
			}
		})
	}
}

func TestBuiltinIntConversion(t *testing.T) {
	t.Parallel()

	if Dispatch("int", &String{Value: "42"}).(*Integer).Value != 42 {
		t.Fatal("int(string) wrong")
	}
	if Dispatch("int", &Float{Value: 3.9}).(*Integer).Value != 3 {
		t.Fatal("int(float) wrong: should truncate")
	}
	result := Dispatch("int", &String{Value: "notanumber"})
	if result.Type() != ERROR_OBJ {
		t.Fatal("int(invalid string) should return error")
	}
}

func TestBuiltinFloatConversion(t *testing.T) {
	t.Parallel()

	if Dispatch("float", &Integer{Value: 5}).(*Float).Value != 5.0 {
		t.Fatal("float(integer) wrong")
	}
	if Dispatch("float", &String{Value: "3.14"}).(*Float).Value != 3.14 {
		t.Fatal("float(string) wrong")
	}
	if Dispatch("float", &String{Value: "bad"}).Type() != ERROR_OBJ {
		t.Fatal("float(invalid) should error")
	}
}

func TestBuiltinStr(t *testing.T) {
	t.Parallel()

	if Dispatch("str", &Integer{Value: 42}).(*String).Value != "42" {
		t.Fatal("str(42) wrong")
	}
	if Dispatch("str", NullVal).(*String).Value != "null" {
		t.Fatal("str(null) wrong")
	}
}

func TestBuiltinBoolAndIsNull(t *testing.T) {
	t.Parallel()

	if Dispatch("bool", NullVal).(*Boolean).Value != false {
		t.Fatal("bool(null) should be false")
	}
	if Dispatch("bool", &Integer{Value: 1}).(*Boolean).Value != true {
		t.Fatal("bool(1) should be true")
	}
	if Dispatch("isNull", NullVal).(*Boolean).Value != true {
		t.Fatal("isNull(null) should be true")
	}
	if Dispatch("isNull", &Integer{Value: 0}).(*Boolean).Value != false {
		t.Fatal("isNull(0) should be false")
	}
}

// ExampleDispatch parses a string to an integer.
func ExampleDispatch() {
	fmt.Println(Dispatch("int", &String{Value: "42"}).Inspect())
	// Output: 42
}
```

## Review

The module is correct when the total conversions never error and the partial ones fail loudly. Confirm that `type` returns the runtime tag for every value, that `str` renders any value through its `Inspect`, that `int` truncates a float toward zero and returns an `*Error` on a non-numeric string, that `float` parses and errors symmetrically, and that `bool` follows the truthiness rule (only `null` and `false` are falsy, so `bool(0)` is true) while `isNull` tests the null tag exactly.

Common mistakes for this feature. Returning `0` from `int("abc")` instead of an error hides the parse failure and corrupts arithmetic downstream. Making `bool` a nonzero test rather than `isTruthy` makes `bool(0)` false and breaks agreement with `if`. Rounding instead of truncating in `int(float)` silently disagrees with Go's own `int64(f)` and with the separate `round` built-in.

## Resources

- [`strconv` package](https://pkg.go.dev/strconv) — `ParseInt` and `ParseFloat`, and the `*NumError` they return on bad input.
- [Go spec: conversions](https://go.dev/ref/spec#Conversions) — float-to-integer conversion truncates toward zero, the behavior `int` mirrors.
- [Writing An Interpreter In Go (Thorsten Ball)](https://interpreterbook.com/) — the truthiness rule used by `if` and reused here by `bool`.

---

Back to [03-string-builtins.md](03-string-builtins.md) | Next: [05-math-builtins.md](05-math-builtins.md)
