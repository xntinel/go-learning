# Exercise 2: Higher-Order Built-ins via a Callable Interface

`map`, `filter`, and `reduce` take a function as an argument and apply it across an array. The interesting problem is not the fold — it is how a built-in calls a language-level function without importing the evaluator that defines functions. This exercise solves it with a one-method `Callable` interface that both the evaluator's `*Function` and a test-friendly `*BuiltinCallable` satisfy, so the higher-order built-ins depend on the interface and nothing else.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
object.go         the runtime Object types, the Callable interface, BuiltinCallable
registry.go       Builtin, RegisterBuiltin, Dispatch, isError, newError
higherorder.go    map, filter, reduce, isTruthy and their init() registration
cmd/
  demo/
    main.go       map/filter/reduce over a fixed array
higherorder_test.go   callable application, truthiness filtering, error propagation
```

- Files: `object.go`, `registry.go`, `higherorder.go`, `cmd/demo/main.go`, `higherorder_test.go`.
- Implement: the `Callable` interface and `*BuiltinCallable`; `map`, `filter`, `reduce`; the `isTruthy` helper; and the registry framework they dispatch through.
- Test: that the callback is applied to every element, that `filter` keeps truthy results, that `reduce` folds left, and that an error from the callback aborts the whole call.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

### Why an interface, not a concrete function type

The higher-order built-ins must invoke a callback that, in the full interpreter, is a `*Function` carrying an AST body and a closure environment. The built-in package cannot import the evaluator to name that type — the evaluator already imports the built-ins, so naming `*Function` here would close a circular dependency. The `Callable` interface breaks the cycle: it declares exactly one capability, `Call(args ...Object) Object`, and the built-ins program against it. The evaluator's `*Function` implements `Call` by running `Eval` on its body; the `*BuiltinCallable` defined in this module implements it by invoking a wrapped Go function. The built-ins never know or care which one they hold.

That second implementation is what makes the higher-order built-ins testable. A test supplies a `*BuiltinCallable` whose body is an ordinary Go closure — `double`, `isEven`, `add` — and asserts on the result without standing up a parser or an evaluator. The interface is the testability seam.

Create `object.go`:

```go
package higherorder

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
	HASH_OBJ    ObjectType = "HASH"
	ERROR_OBJ   ObjectType = "ERROR"
	BUILTIN_OBJ ObjectType = "BUILTIN"
)

// Object is the runtime value interface shared across the interpreter.
type Object interface {
	Type() ObjectType
	Inspect() string
}

// Callable extends Object with the ability to be invoked with arguments.
// The evaluator's *Function type implements this; so does *BuiltinCallable.
type Callable interface {
	Object
	Call(args ...Object) Object
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

// BuiltinCallable wraps a BuiltinFunction so it satisfies Callable. The
// interpreter uses it to pass built-ins as first-class values; tests use it as
// a controllable callback for the higher-order built-ins.
type BuiltinCallable struct {
	Name string
	Fn   BuiltinFunction
}

func (b *BuiltinCallable) Type() ObjectType           { return BUILTIN_OBJ }
func (b *BuiltinCallable) Inspect() string            { return "builtin " + b.Name }
func (b *BuiltinCallable) Call(args ...Object) Object { return b.Fn(args...) }

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

The same dispatch substrate from the core module. The two helpers the higher-order built-ins lean on are `isError`, used to detect a failed callback and abort, and `newError`, used to report a wrong argument type.

Create `registry.go`:

```go
package higherorder

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
// Pass -1 for an unbounded side.
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

func isError(obj Object) bool {
	return obj != nil && obj.Type() == ERROR_OBJ
}
```

### The higher-order built-ins

Each takes an array and a `Callable`. The defining detail is error handling: after every `fn.Call`, the result is checked with `isError` and returned immediately on failure, so a fault in the callback aborts the whole operation instead of leaving a half-built array. `filter` additionally routes the callback's result through `isTruthy`, the language's truthiness rule — only `null` and `false` are falsy. `reduce` threads an accumulator left to right, calling `fn(acc, el)` and replacing `acc` with each result.

Create `higherorder.go`:

```go
package higherorder

func init() {
	RegisterBuiltin("map", builtinMap, WithArity(2, 2),
		WithDoc("map(arr, fn) – apply fn to each element, return new array"))
	RegisterBuiltin("filter", builtinFilter, WithArity(2, 2),
		WithDoc("filter(arr, fn) – return elements for which fn returns truthy"))
	RegisterBuiltin("reduce", builtinReduce, WithArity(3, 3),
		WithDoc("reduce(arr, initial, fn) – fold arr left with fn(acc, val)"))
}

func builtinMap(args ...Object) Object {
	arr, ok := args[0].(*Array)
	if !ok {
		return newError("map: arg 1: want ARRAY, got %s", args[0].Type())
	}
	fn, ok := args[1].(Callable)
	if !ok {
		return newError("map: arg 2: want callable, got %s", args[1].Type())
	}
	newElems := make([]Object, len(arr.Elements))
	for i, el := range arr.Elements {
		result := fn.Call(el)
		if isError(result) {
			return result
		}
		newElems[i] = result
	}
	return &Array{Elements: newElems}
}

func builtinFilter(args ...Object) Object {
	arr, ok := args[0].(*Array)
	if !ok {
		return newError("filter: arg 1: want ARRAY, got %s", args[0].Type())
	}
	fn, ok := args[1].(Callable)
	if !ok {
		return newError("filter: arg 2: want callable, got %s", args[1].Type())
	}
	newElems := make([]Object, 0)
	for _, el := range arr.Elements {
		result := fn.Call(el)
		if isError(result) {
			return result
		}
		if isTruthy(result) {
			newElems = append(newElems, el)
		}
	}
	return &Array{Elements: newElems}
}

func builtinReduce(args ...Object) Object {
	arr, ok := args[0].(*Array)
	if !ok {
		return newError("reduce: arg 1: want ARRAY, got %s", args[0].Type())
	}
	fn, ok := args[2].(Callable)
	if !ok {
		return newError("reduce: arg 3: want callable, got %s", args[2].Type())
	}
	acc := args[1]
	for _, el := range arr.Elements {
		result := fn.Call(acc, el)
		if isError(result) {
			return result
		}
		acc = result
	}
	return acc
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

The demo constructs a `*BuiltinCallable` for each operation — `double`, `isEven`, `add` — and runs the three built-ins over a fixed array, so the output is deterministic and shows the immutability of the source array (it is reused untouched across all three calls).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/higherorder"
)

func main() {
	arr := &higherorder.Array{Elements: []higherorder.Object{
		&higherorder.Integer{Value: 1},
		&higherorder.Integer{Value: 2},
		&higherorder.Integer{Value: 3},
		&higherorder.Integer{Value: 4},
		&higherorder.Integer{Value: 5},
	}}

	double := &higherorder.BuiltinCallable{
		Name: "double",
		Fn: func(args ...higherorder.Object) higherorder.Object {
			n := args[0].(*higherorder.Integer)
			return &higherorder.Integer{Value: n.Value * 2}
		},
	}
	isEven := &higherorder.BuiltinCallable{
		Name: "isEven",
		Fn: func(args ...higherorder.Object) higherorder.Object {
			return higherorder.BoolObj(args[0].(*higherorder.Integer).Value%2 == 0)
		},
	}
	add := &higherorder.BuiltinCallable{
		Name: "add",
		Fn: func(args ...higherorder.Object) higherorder.Object {
			return &higherorder.Integer{Value: args[0].(*higherorder.Integer).Value + args[1].(*higherorder.Integer).Value}
		},
	}

	fmt.Println("source:", arr.Inspect())
	fmt.Println("map:   ", higherorder.Dispatch("map", arr, double).Inspect())
	fmt.Println("filter:", higherorder.Dispatch("filter", arr, isEven).Inspect())
	fmt.Println("reduce:", higherorder.Dispatch("reduce", arr, &higherorder.Integer{Value: 0}, add).Inspect())
	fmt.Println("source still:", arr.Inspect())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
source: [1, 2, 3, 4, 5]
map:    [2, 4, 6, 8, 10]
filter: [2, 4]
reduce: 15
source still: [1, 2, 3, 4, 5]
```

### Tests

The tests use `*BuiltinCallable` as the controllable callback. `TestBuiltinMapWithCallable` doubles each element and then checks the source array is unchanged. `TestBuiltinFilterWithCallable` keeps the even elements. `TestBuiltinReduceSum` folds to a sum. `TestBuiltinMapPropagatesError` installs a callback that always errors and asserts the whole `map` returns an error — the error-propagation contract made executable.

Create `higherorder_test.go`:

```go
package higherorder

import (
	"fmt"
	"testing"
)

func TestRegistryPopulated(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"map", "filter", "reduce"} {
		if Lookup(name) == nil {
			t.Errorf("built-in %q not registered", name)
		}
	}
}

func TestBuiltinMapWithCallable(t *testing.T) {
	t.Parallel()

	double := &BuiltinCallable{
		Name: "double",
		Fn: func(args ...Object) Object {
			return &Integer{Value: args[0].(*Integer).Value * 2}
		},
	}
	arr := &Array{Elements: []Object{
		&Integer{Value: 1}, &Integer{Value: 2}, &Integer{Value: 3},
	}}
	result := Dispatch("map", arr, double).(*Array)
	want := []int64{2, 4, 6}
	for i, w := range want {
		if result.Elements[i].(*Integer).Value != w {
			t.Fatalf("map[%d] = %d, want %d", i, result.Elements[i].(*Integer).Value, w)
		}
	}
	if arr.Elements[0].(*Integer).Value != 1 {
		t.Fatal("map mutated the original array")
	}
}

func TestBuiltinFilterWithCallable(t *testing.T) {
	t.Parallel()

	isEven := &BuiltinCallable{
		Name: "isEven",
		Fn: func(args ...Object) Object {
			return BoolObj(args[0].(*Integer).Value%2 == 0)
		},
	}
	arr := &Array{Elements: []Object{
		&Integer{Value: 1}, &Integer{Value: 2},
		&Integer{Value: 3}, &Integer{Value: 4},
	}}
	result := Dispatch("filter", arr, isEven).(*Array)
	if len(result.Elements) != 2 {
		t.Fatalf("filter len = %d, want 2", len(result.Elements))
	}
	if result.Elements[0].(*Integer).Value != 2 {
		t.Fatal("filter kept wrong element")
	}
}

func TestBuiltinReduceSum(t *testing.T) {
	t.Parallel()

	add := &BuiltinCallable{
		Name: "add",
		Fn: func(args ...Object) Object {
			return &Integer{Value: args[0].(*Integer).Value + args[1].(*Integer).Value}
		},
	}
	arr := &Array{Elements: []Object{
		&Integer{Value: 1}, &Integer{Value: 2}, &Integer{Value: 3},
	}}
	result := Dispatch("reduce", arr, &Integer{Value: 0}, add)
	if result.(*Integer).Value != 6 {
		t.Fatalf("reduce sum = %d, want 6", result.(*Integer).Value)
	}
}

func TestBuiltinMapPropagatesError(t *testing.T) {
	t.Parallel()

	fail := &BuiltinCallable{
		Name: "fail",
		Fn: func(args ...Object) Object {
			return &Error{Message: "intentional"}
		},
	}
	arr := &Array{Elements: []Object{&Integer{Value: 1}}}
	result := Dispatch("map", arr, fail)
	if result.Type() != ERROR_OBJ {
		t.Fatalf("map should propagate error, got %s", result.Type())
	}
}

// ExampleDispatch folds an array to its sum through reduce.
func ExampleDispatch() {
	add := &BuiltinCallable{
		Name: "add",
		Fn: func(args ...Object) Object {
			return &Integer{Value: args[0].(*Integer).Value + args[1].(*Integer).Value}
		},
	}
	arr := &Array{Elements: []Object{&Integer{Value: 1}, &Integer{Value: 2}, &Integer{Value: 3}}}
	fmt.Println(Dispatch("reduce", arr, &Integer{Value: 0}, add).Inspect())
	// Output: 6
}
```

## Review

The module is correct when the callback is applied to every element, the source array is never mutated, and a callback error aborts the whole operation. Confirm that `map` returns a new array of the same length, that `filter` keeps exactly the elements whose callback result is truthy under the language rule (only `null` and `false` are falsy), that `reduce` folds left with the supplied initial value, and that a callback returning an `*Error` makes the entire call return that error rather than a partial array.

Common mistakes for this feature. Ignoring the callback's return value and always appending swallows errors and produces a half-built result; check `isError` after every `Call`. Naming the evaluator's `*Function` type directly instead of programming against `Callable` reintroduces the circular import the interface exists to prevent. Treating every non-`false` value as falsy, or forgetting that `null` is falsy, makes `filter` disagree with the language's `if`.

## Resources

- [Writing An Interpreter In Go (Thorsten Ball)](https://interpreterbook.com/) — the evaluator chapter shows how `*Function` carries a body and environment, the concrete `Callable` the interface abstracts.
- [Go interfaces](https://go.dev/tour/methods/9) — the single-method interface that decouples the built-ins from the evaluator.
- [Effective Go: interfaces](https://go.dev/doc/effective_go#interfaces) — accepting the narrowest interface a function needs.

---

Back to [01-registry-and-collections.md](01-registry-and-collections.md) | Next: [03-string-builtins.md](03-string-builtins.md)
