# Exercise 5: Math Built-ins

The math built-ins wrap the `math` and `math/rand/v2` packages, but two design rules give them more character than a plain delegation. First, type preservation: `abs` returns an INTEGER for an INTEGER and a FLOAT for a FLOAT, so it does not silently widen integer arithmetic to floating point. Second, domain guards: `sqrt` of a negative and `log` of a non-positive return an `*Error` instead of a `NaN` that would poison every later computation. A small `toFloat64` helper lets the float-only functions accept either numeric type.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
object.go     the runtime Integer, Float, Error types
registry.go   Builtin, RegisterBuiltin, Dispatch, newError
math.go       abs, min, max, floor, ceil, round, sqrt, pow, log, sin, cos, random, randomInt
cmd/
  demo/
    main.go   deterministic abs / sqrt / pow / rounding output
math_test.go  type preservation, rounding, domain guards, random ranges
```

- Files: `object.go`, `registry.go`, `math.go`, `cmd/demo/main.go`, `math_test.go`.
- Implement: `abs`, `min`, `max`, `floor`, `ceil`, `round`, `sqrt`, `pow`, `log`, `sin`, `cos`, `random`, `randomInt`, the `toFloat64` helper, and the registry framework.
- Test: that `abs` preserves the argument's type, that `floor`/`ceil`/`round` return integers, that `sqrt(-1)` and a reversed `randomInt` range error, and that `random`/`randomInt` stay in range.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

### Type preservation, domain guards, and the float bridge

The defining decision is `abs`. A naive implementation converts to `float64`, takes `math.Abs`, and returns a FLOAT — which means `abs(-5)` is `5` the float, and an expression that was pure integer arithmetic suddenly carries floating-point values. The correct `abs` type-switches: INTEGER in, INTEGER out (negating the `int64` directly); FLOAT in, FLOAT out (via `math.Abs`). `min` and `max` follow the same instinct: when both arguments are integers they compare and return an integer, preserving the type; only a mixed or float pair falls back to a float comparison.

The float-only functions — `floor`, `ceil`, `round`, `sqrt`, `pow`, `log`, `sin`, `cos` — accept either numeric type through `toFloat64`, which converts an INTEGER or a FLOAT to `float64` and reports failure for anything else. `floor`, `ceil`, and `round` then return an INTEGER (they produce whole numbers); the rest return a FLOAT. The domain guards live here too: `sqrt` of a negative number and `log` of a value `<= 0` would yield `NaN`/`-Inf` in Go, which propagate silently and make every dependent result garbage, so both return an explicit `*Error`. The randomness built-ins use `math/rand/v2`, which is automatically seeded — `random()` is a float in `[0, 1)` and `randomInt(lo, hi)` is an inclusive integer range, erroring if `lo > hi`.

Create `object.go`:

```go
package mathbuiltins

import "fmt"

// ObjectType names the runtime kind of an Object.
type ObjectType string

const (
	INTEGER_OBJ ObjectType = "INTEGER"
	FLOAT_OBJ   ObjectType = "FLOAT"
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

// Error is a runtime error value that propagates without panicking.
type Error struct{ Message string }

func (e *Error) Type() ObjectType { return ERROR_OBJ }
func (e *Error) Inspect() string  { return "ERROR: " + e.Message }
```

### The registry framework

Create `registry.go`:

```go
package mathbuiltins

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

### The math built-ins

Create `math.go`:

```go
package mathbuiltins

import (
	"math"
	"math/rand/v2"
)

func init() {
	RegisterBuiltin("abs", builtinAbs, WithArity(1, 1),
		WithDoc("abs(n) – absolute value; INTEGER in, INTEGER out; FLOAT in, FLOAT out"))
	RegisterBuiltin("min", builtinMin, WithArity(2, 2),
		WithDoc("min(a, b) – return the smaller of two numbers"))
	RegisterBuiltin("max", builtinMax, WithArity(2, 2),
		WithDoc("max(a, b) – return the larger of two numbers"))
	RegisterBuiltin("floor", builtinFloor, WithArity(1, 1),
		WithDoc("floor(f) – round down, return INTEGER"))
	RegisterBuiltin("ceil", builtinCeil, WithArity(1, 1),
		WithDoc("ceil(f) – round up, return INTEGER"))
	RegisterBuiltin("round", builtinRound, WithArity(1, 1),
		WithDoc("round(f) – round to nearest, return INTEGER"))
	RegisterBuiltin("sqrt", builtinSqrt, WithArity(1, 1),
		WithDoc("sqrt(f) – square root, always return FLOAT"))
	RegisterBuiltin("pow", builtinPow, WithArity(2, 2),
		WithDoc("pow(base, exp) – raise base to exp, return FLOAT"))
	RegisterBuiltin("log", builtinLog, WithArity(1, 1),
		WithDoc("log(f) – natural logarithm, return FLOAT"))
	RegisterBuiltin("sin", builtinSin, WithArity(1, 1),
		WithDoc("sin(f) – sine in radians, return FLOAT"))
	RegisterBuiltin("cos", builtinCos, WithArity(1, 1),
		WithDoc("cos(f) – cosine in radians, return FLOAT"))
	RegisterBuiltin("random", builtinRandom, WithArity(0, 0),
		WithDoc("random() – return a float in [0, 1)"))
	RegisterBuiltin("randomInt", builtinRandomInt, WithArity(2, 2),
		WithDoc("randomInt(min, max) – return a random integer in [min, max]"))
}

func toFloat64(obj Object) (float64, bool) {
	switch o := obj.(type) {
	case *Integer:
		return float64(o.Value), true
	case *Float:
		return o.Value, true
	}
	return 0, false
}

func builtinAbs(args ...Object) Object {
	switch obj := args[0].(type) {
	case *Integer:
		v := obj.Value
		if v < 0 {
			v = -v
		}
		return &Integer{Value: v}
	case *Float:
		return &Float{Value: math.Abs(obj.Value)}
	default:
		return newError("abs: want INTEGER or FLOAT, got %s", args[0].Type())
	}
}

func builtinMin(args ...Object) Object {
	aInt, aIsInt := args[0].(*Integer)
	bInt, bIsInt := args[1].(*Integer)
	if aIsInt && bIsInt {
		if aInt.Value <= bInt.Value {
			return aInt
		}
		return bInt
	}
	a, okA := toFloat64(args[0])
	b, okB := toFloat64(args[1])
	if !okA || !okB {
		return newError("min: want INTEGER or FLOAT, got %s and %s",
			args[0].Type(), args[1].Type())
	}
	if a <= b {
		return args[0]
	}
	return args[1]
}

func builtinMax(args ...Object) Object {
	aInt, aIsInt := args[0].(*Integer)
	bInt, bIsInt := args[1].(*Integer)
	if aIsInt && bIsInt {
		if aInt.Value >= bInt.Value {
			return aInt
		}
		return bInt
	}
	a, okA := toFloat64(args[0])
	b, okB := toFloat64(args[1])
	if !okA || !okB {
		return newError("max: want INTEGER or FLOAT, got %s and %s",
			args[0].Type(), args[1].Type())
	}
	if a >= b {
		return args[0]
	}
	return args[1]
}

func builtinFloor(args ...Object) Object {
	f, ok := toFloat64(args[0])
	if !ok {
		return newError("floor: want INTEGER or FLOAT, got %s", args[0].Type())
	}
	return &Integer{Value: int64(math.Floor(f))}
}

func builtinCeil(args ...Object) Object {
	f, ok := toFloat64(args[0])
	if !ok {
		return newError("ceil: want INTEGER or FLOAT, got %s", args[0].Type())
	}
	return &Integer{Value: int64(math.Ceil(f))}
}

func builtinRound(args ...Object) Object {
	f, ok := toFloat64(args[0])
	if !ok {
		return newError("round: want INTEGER or FLOAT, got %s", args[0].Type())
	}
	return &Integer{Value: int64(math.Round(f))}
}

func builtinSqrt(args ...Object) Object {
	f, ok := toFloat64(args[0])
	if !ok {
		return newError("sqrt: want INTEGER or FLOAT, got %s", args[0].Type())
	}
	if f < 0 {
		return newError("sqrt: argument must be non-negative, got %g", f)
	}
	return &Float{Value: math.Sqrt(f)}
}

func builtinPow(args ...Object) Object {
	base, okB := toFloat64(args[0])
	exp, okE := toFloat64(args[1])
	if !okB || !okE {
		return newError("pow: want INTEGER or FLOAT args")
	}
	return &Float{Value: math.Pow(base, exp)}
}

func builtinLog(args ...Object) Object {
	f, ok := toFloat64(args[0])
	if !ok {
		return newError("log: want INTEGER or FLOAT, got %s", args[0].Type())
	}
	if f <= 0 {
		return newError("log: argument must be positive, got %g", f)
	}
	return &Float{Value: math.Log(f)}
}

func builtinSin(args ...Object) Object {
	f, ok := toFloat64(args[0])
	if !ok {
		return newError("sin: want INTEGER or FLOAT, got %s", args[0].Type())
	}
	return &Float{Value: math.Sin(f)}
}

func builtinCos(args ...Object) Object {
	f, ok := toFloat64(args[0])
	if !ok {
		return newError("cos: want INTEGER or FLOAT, got %s", args[0].Type())
	}
	return &Float{Value: math.Cos(f)}
}

func builtinRandom(_ ...Object) Object {
	return &Float{Value: rand.Float64()}
}

func builtinRandomInt(args ...Object) Object {
	lo, okL := args[0].(*Integer)
	hi, okH := args[1].(*Integer)
	if !okL {
		return newError("randomInt: arg 1: want INTEGER, got %s", args[0].Type())
	}
	if !okH {
		return newError("randomInt: arg 2: want INTEGER, got %s", args[1].Type())
	}
	if lo.Value > hi.Value {
		return newError("randomInt: min (%d) must be <= max (%d)", lo.Value, hi.Value)
	}
	n := hi.Value - lo.Value + 1
	return &Integer{Value: lo.Value + rand.Int64N(n)}
}
```

### The runnable demo

The demo prints only deterministic results — the random built-ins are registered and tested, but printing their values would make the output unstable, so they are exercised in the tests instead.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mathbuiltins"
)

func main() {
	fmt.Println("abs(-5):     ", mathbuiltins.Dispatch("abs", &mathbuiltins.Integer{Value: -5}).Inspect())
	fmt.Println("abs(-2.5):   ", mathbuiltins.Dispatch("abs", &mathbuiltins.Float{Value: -2.5}).Inspect())
	fmt.Println("sqrt(16):    ", mathbuiltins.Dispatch("sqrt", &mathbuiltins.Integer{Value: 16}).Inspect())
	fmt.Println("pow(2, 10):  ", mathbuiltins.Dispatch("pow", &mathbuiltins.Integer{Value: 2}, &mathbuiltins.Integer{Value: 10}).Inspect())
	fmt.Println("floor(2.9):  ", mathbuiltins.Dispatch("floor", &mathbuiltins.Float{Value: 2.9}).Inspect())
	fmt.Println("ceil(2.1):   ", mathbuiltins.Dispatch("ceil", &mathbuiltins.Float{Value: 2.1}).Inspect())
	fmt.Println("round(2.5):  ", mathbuiltins.Dispatch("round", &mathbuiltins.Float{Value: 2.5}).Inspect())
	fmt.Println("min(3, 7):   ", mathbuiltins.Dispatch("min", &mathbuiltins.Integer{Value: 3}, &mathbuiltins.Integer{Value: 7}).Inspect())
	fmt.Println("max(3, 7):   ", mathbuiltins.Dispatch("max", &mathbuiltins.Integer{Value: 3}, &mathbuiltins.Integer{Value: 7}).Inspect())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
abs(-5):      5
abs(-2.5):    2.5
sqrt(16):     4
pow(2, 10):   1024
floor(2.9):   2
ceil(2.1):    3
round(2.5):   3
min(3, 7):    3
max(3, 7):    7
```

### Tests

The random built-ins are tested by sampling many draws and asserting every result lands in range — a probabilistic but reliable check — plus the reversed-range error path. The deterministic functions are checked against known values.

Create `math_test.go`:

```go
package mathbuiltins

import (
	"fmt"
	"testing"
)

func TestRegistryPopulated(t *testing.T) {
	t.Parallel()

	required := []string{
		"abs", "min", "max", "floor", "ceil", "round",
		"sqrt", "pow", "log", "sin", "cos", "random", "randomInt",
	}
	for _, name := range required {
		if Lookup(name) == nil {
			t.Errorf("built-in %q not registered", name)
		}
	}
}

func TestBuiltinAbsPreservesType(t *testing.T) {
	t.Parallel()

	if Dispatch("abs", &Integer{Value: -5}).(*Integer).Value != 5 {
		t.Fatal("abs(-5 INTEGER) should be 5 INTEGER")
	}
	if Dispatch("abs", &Float{Value: -2.5}).(*Float).Value != 2.5 {
		t.Fatal("abs(-2.5 FLOAT) should be 2.5 FLOAT")
	}
}

func TestBuiltinMinMax(t *testing.T) {
	t.Parallel()

	if Dispatch("min", &Integer{Value: 3}, &Integer{Value: 7}).(*Integer).Value != 3 {
		t.Fatal("min wrong")
	}
	if Dispatch("max", &Integer{Value: 3}, &Integer{Value: 7}).(*Integer).Value != 7 {
		t.Fatal("max wrong")
	}
}

func TestBuiltinFloorCeilRound(t *testing.T) {
	t.Parallel()

	cases := []struct {
		fn   string
		arg  float64
		want int64
	}{
		{"floor", 2.9, 2},
		{"ceil", 2.1, 3},
		{"round", 2.5, 3},
		{"round", 2.4, 2},
	}
	for _, tc := range cases {
		t.Run(tc.fn, func(t *testing.T) {
			t.Parallel()
			result := Dispatch(tc.fn, &Float{Value: tc.arg})
			if result.(*Integer).Value != tc.want {
				t.Fatalf("%s(%g) = %d, want %d", tc.fn, tc.arg, result.(*Integer).Value, tc.want)
			}
		})
	}
}

func TestBuiltinSqrt(t *testing.T) {
	t.Parallel()

	result := Dispatch("sqrt", &Integer{Value: 16})
	if result.(*Float).Value != 4.0 {
		t.Fatalf("sqrt(16) = %g, want 4.0", result.(*Float).Value)
	}
	if Dispatch("sqrt", &Integer{Value: -1}).Type() != ERROR_OBJ {
		t.Fatal("sqrt(-1) should return error")
	}
}

func TestBuiltinPowAndLog(t *testing.T) {
	t.Parallel()

	if Dispatch("pow", &Integer{Value: 2}, &Integer{Value: 10}).(*Float).Value != 1024.0 {
		t.Fatal("pow(2, 10) should be 1024.0")
	}
	if Dispatch("log", &Integer{Value: 1}).(*Float).Value != 0.0 {
		t.Fatal("log(1) should be 0.0")
	}
	if Dispatch("log", &Integer{Value: 0}).Type() != ERROR_OBJ {
		t.Fatal("log(0) should return error")
	}
}

func TestBuiltinRandomInRange(t *testing.T) {
	t.Parallel()

	for i := 0; i < 100; i++ {
		result := Dispatch("random")
		f := result.(*Float).Value
		if f < 0 || f >= 1 {
			t.Fatalf("random() = %g, not in [0,1)", f)
		}
	}
}

func TestBuiltinRandomInt(t *testing.T) {
	t.Parallel()

	for i := 0; i < 50; i++ {
		result := Dispatch("randomInt", &Integer{Value: 1}, &Integer{Value: 6})
		v := result.(*Integer).Value
		if v < 1 || v > 6 {
			t.Fatalf("randomInt(1,6) = %d, not in [1,6]", v)
		}
	}
	if Dispatch("randomInt", &Integer{Value: 5}, &Integer{Value: 1}).Type() != ERROR_OBJ {
		t.Fatal("randomInt with min > max should error")
	}
}

// ExampleDispatch raises 2 to the 10th power.
func ExampleDispatch() {
	fmt.Println(Dispatch("pow", &Integer{Value: 2}, &Integer{Value: 10}).Inspect())
	// Output: 1024
}
```

## Review

The module is correct when types are preserved and bad domains are rejected. Confirm that `abs` returns an INTEGER for an INTEGER and a FLOAT for a FLOAT, that `min`/`max` keep an integer pair integer, that `floor`/`ceil`/`round` return integers, that `sqrt`/`pow`/`log`/`sin`/`cos` return floats, that `sqrt` of a negative and `log` of a non-positive return an `*Error`, and that `random` and `randomInt` stay in range across many draws while a reversed `randomInt` range errors.

Common mistakes for this feature. Converting everything to float in `abs` widens integer arithmetic and changes the type of an expression. Returning a `NaN` from `sqrt(-1)` instead of an error lets the poison value propagate undetected through every later operation. Using `math/rand` (v1) without seeding produces the same sequence every run; `math/rand/v2` is auto-seeded and is the right choice. Forgetting the `+1` in `randomInt`'s range makes the maximum unreachable.

## Resources

- [`math` package](https://pkg.go.dev/math) — `Abs`, `Floor`, `Ceil`, `Round`, `Sqrt`, `Pow`, `Log`, `Sin`, `Cos`.
- [`math/rand/v2` package](https://pkg.go.dev/math/rand/v2) — `Float64` and `Int64N`, automatically seeded since Go 1.22.
- [Writing An Interpreter In Go (Thorsten Ball)](https://interpreterbook.com/) — the object model these numeric types extend.

---

Back to [04-type-conversion-builtins.md](04-type-conversion-builtins.md) | Next: [06-hash-builtins.md](06-hash-builtins.md)
