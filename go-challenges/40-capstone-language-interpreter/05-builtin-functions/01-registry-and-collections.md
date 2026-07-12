# Exercise 1: The Registry and Collection Built-ins

This is the core module every later exercise is a variation on. It builds the object system the built-ins operate over, the registry that maps a name to a handler with an arity contract, the `Dispatch` entry point that validates arguments before any handler runs, and the first category of built-ins: the immutable collection operations `len`, `push`, `pop`, `first`, `last`, `rest`, and `zip`. Get this right and the rest of the lesson is "register another handler".

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
object.go         the runtime Object types, singletons, HashKeyOf
registry.go       Builtin, RegisterBuiltin, Dispatch, checkArgs, checkType, newError
collections.go    len, push, pop, first, last, rest, zip and their init() registration
cmd/
  demo/
    main.go       exercise the collection built-ins through Dispatch
collections_test.go   arity, type, immutability, and edge-case coverage
```

- Files: `object.go`, `registry.go`, `collections.go`, `cmd/demo/main.go`, `collections_test.go`.
- Implement: the `Object` interface and its concrete types; `RegisterBuiltin`, `WithArity`, `WithDoc`, `Lookup`, `Dispatch`, `checkArgs`, `checkType`, `newError`, `isError`; and the seven collection handlers.
- Test: arity failures, wrong-type failures, the immutability of `push`/`pop`/`rest`, and the empty-array edge cases for `first`/`last`/`rest`.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/40-capstone-language-interpreter/05-builtin-functions/01-registry-and-collections/cmd/demo && cd go-solutions/40-capstone-language-interpreter/05-builtin-functions/01-registry-and-collections
```

### The object system

The built-ins do not know anything about the evaluator; they operate on a small interface, `Object`, and a handful of concrete types. In a real interpreter this lives in a shared `object` package that both the evaluator and the built-ins import. For a standalone module it is reproduced here. `Inspect` is the printable form; `Type` returns a named constant used in error messages and in the `type` built-in. Singletons for `null`, `true`, and `false` avoid allocating a fresh object on every call that returns one of them.

Create `object.go`:

```go
package builtins

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

// HashKey is the canonical map key for Hash entries.
type HashKey struct {
	Type  ObjectType
	Value string
}

// HashPair holds one key-value pair in a Hash.
type HashPair struct {
	Key   Object
	Value Object
}

// Hash maps hashable keys to objects.
type Hash struct{ Pairs map[HashKey]HashPair }

func (h *Hash) Type() ObjectType { return HASH_OBJ }
func (h *Hash) Inspect() string {
	parts := make([]string, 0, len(h.Pairs))
	for _, p := range h.Pairs {
		parts = append(parts, p.Key.Inspect()+": "+p.Value.Inspect())
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Error is a runtime error value that propagates without panicking.
type Error struct{ Message string }

func (e *Error) Type() ObjectType { return ERROR_OBJ }
func (e *Error) Inspect() string  { return "ERROR: " + e.Message }

// BuiltinCallable wraps a BuiltinFunction so it satisfies Callable.
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

// HashKeyOf returns the canonical HashKey for o and reports whether o is
// hashable. Only Integer, String, and Boolean are hashable.
func HashKeyOf(o Object) (HashKey, bool) {
	switch v := o.(type) {
	case *Integer:
		return HashKey{Type: INTEGER_OBJ, Value: fmt.Sprintf("%d", v.Value)}, true
	case *String:
		return HashKey{Type: STRING_OBJ, Value: v.Value}, true
	case *Boolean:
		return HashKey{Type: BOOLEAN_OBJ, Value: fmt.Sprintf("%t", v.Value)}, true
	}
	return HashKey{}, false
}
```

### The registry framework

The registry is the heart of the lesson. `RegisterBuiltin` records a name, a handler, and an arity contract built from functional options. `Dispatch` is the single entry point: it looks the name up, validates the argument count with `checkArgs`, and only then calls the handler. Because arity is checked once, centrally, no handler ever repeats it — and because a failed check returns an `*Error` value, a bad call never panics. `checkType` and `isError` are shared helpers the handlers reach for.

Create `registry.go`:

```go
package builtins

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
// It returns an *Error if name is unknown or arity is wrong.
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

// checkType asserts that args[i] has type want and returns a descriptive
// *Error on mismatch, nil on success.
func checkType(fnName string, args []Object, i int, want ObjectType) *Error {
	if got := args[i].Type(); got != want {
		return newError("%s: arg %d: want %s, got %s", fnName, i+1, want, got)
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

### The collection built-ins

These seven handlers are the canonical demonstration of functional purity. `len` is overloaded across string, array, and hash; on a string it is a byte count, matching Go's own `len`. `push`, `pop`, and `rest` each allocate a fresh slice and copy, never touching the argument's backing array. `first` and `last` return an element or the `null` singleton on an empty array. `zip` pairs two arrays element-wise, truncating to the shorter length. Each handler assumes the arity is already correct because `Dispatch` checked it.

Create `collections.go`:

```go
package builtins

func init() {
	RegisterBuiltin("len", builtinLen, WithArity(1, 1),
		WithDoc("len(obj) – byte length of a string; element count of an array or hash"))
	RegisterBuiltin("push", builtinPush, WithArity(2, -1),
		WithDoc("push(arr, val...) – return a new array with val(s) appended"))
	RegisterBuiltin("pop", builtinPop, WithArity(1, 1),
		WithDoc("pop(arr) – return a new array with the last element removed"))
	RegisterBuiltin("first", builtinFirst, WithArity(1, 1),
		WithDoc("first(arr) – return the first element or null"))
	RegisterBuiltin("last", builtinLast, WithArity(1, 1),
		WithDoc("last(arr) – return the last element or null"))
	RegisterBuiltin("rest", builtinRest, WithArity(1, 1),
		WithDoc("rest(arr) – return all elements except the first, or null"))
	RegisterBuiltin("zip", builtinZip, WithArity(2, 2),
		WithDoc("zip(arr1, arr2) – combine two arrays element-wise into pairs"))
}

func builtinLen(args ...Object) Object {
	switch obj := args[0].(type) {
	case *String:
		return &Integer{Value: int64(len(obj.Value))}
	case *Array:
		return &Integer{Value: int64(len(obj.Elements))}
	case *Hash:
		return &Integer{Value: int64(len(obj.Pairs))}
	default:
		return newError("len: unsupported type %s", args[0].Type())
	}
}

func builtinPush(args ...Object) Object {
	arr, ok := args[0].(*Array)
	if !ok {
		return newError("push: arg 1: want ARRAY, got %s", args[0].Type())
	}
	newElems := make([]Object, len(arr.Elements), len(arr.Elements)+len(args)-1)
	copy(newElems, arr.Elements)
	newElems = append(newElems, args[1:]...)
	return &Array{Elements: newElems}
}

func builtinPop(args ...Object) Object {
	arr, ok := args[0].(*Array)
	if !ok {
		return newError("pop: arg 1: want ARRAY, got %s", args[0].Type())
	}
	if len(arr.Elements) == 0 {
		return NullVal
	}
	newElems := make([]Object, len(arr.Elements)-1)
	copy(newElems, arr.Elements[:len(arr.Elements)-1])
	return &Array{Elements: newElems}
}

func builtinFirst(args ...Object) Object {
	arr, ok := args[0].(*Array)
	if !ok {
		return newError("first: arg 1: want ARRAY, got %s", args[0].Type())
	}
	if len(arr.Elements) == 0 {
		return NullVal
	}
	return arr.Elements[0]
}

func builtinLast(args ...Object) Object {
	arr, ok := args[0].(*Array)
	if !ok {
		return newError("last: arg 1: want ARRAY, got %s", args[0].Type())
	}
	if len(arr.Elements) == 0 {
		return NullVal
	}
	return arr.Elements[len(arr.Elements)-1]
}

func builtinRest(args ...Object) Object {
	arr, ok := args[0].(*Array)
	if !ok {
		return newError("rest: arg 1: want ARRAY, got %s", args[0].Type())
	}
	if len(arr.Elements) == 0 {
		return NullVal
	}
	newElems := make([]Object, len(arr.Elements)-1)
	copy(newElems, arr.Elements[1:])
	return &Array{Elements: newElems}
}

func builtinZip(args ...Object) Object {
	a, ok1 := args[0].(*Array)
	b, ok2 := args[1].(*Array)
	if !ok1 {
		return newError("zip: arg 1: want ARRAY, got %s", args[0].Type())
	}
	if !ok2 {
		return newError("zip: arg 2: want ARRAY, got %s", args[1].Type())
	}
	length := len(a.Elements)
	if len(b.Elements) < length {
		length = len(b.Elements)
	}
	pairs := make([]Object, length)
	for i := 0; i < length; i++ {
		pairs[i] = &Array{Elements: []Object{a.Elements[i], b.Elements[i]}}
	}
	return &Array{Elements: pairs}
}
```

### The runnable demo

Because `cmd/demo` is a separate `package main`, it can only touch the exported API: the object constructors, `Registry`, and `Dispatch`. The demo prints how many built-ins are registered, then walks the immutable collection operations on a fixed array so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/builtins"
)

func main() {
	fmt.Printf("%d collection built-ins registered\n", len(builtins.Registry))

	arr := &builtins.Array{Elements: []builtins.Object{
		&builtins.Integer{Value: 1},
		&builtins.Integer{Value: 2},
		&builtins.Integer{Value: 3},
		&builtins.Integer{Value: 4},
		&builtins.Integer{Value: 5},
	}}
	fmt.Println("array:", arr.Inspect())
	fmt.Println("len:  ", builtins.Dispatch("len", arr).Inspect())
	fmt.Println("first:", builtins.Dispatch("first", arr).Inspect())
	fmt.Println("last: ", builtins.Dispatch("last", arr).Inspect())
	fmt.Println("rest: ", builtins.Dispatch("rest", arr).Inspect())
	fmt.Println("push: ", builtins.Dispatch("push", arr, &builtins.Integer{Value: 6}).Inspect())
	fmt.Println("pop:  ", builtins.Dispatch("pop", arr).Inspect())

	a := &builtins.Array{Elements: []builtins.Object{&builtins.Integer{Value: 1}, &builtins.Integer{Value: 2}}}
	b := &builtins.Array{Elements: []builtins.Object{&builtins.String{Value: "a"}, &builtins.String{Value: "b"}}}
	fmt.Println("zip:  ", builtins.Dispatch("zip", a, b).Inspect())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
7 collection built-ins registered
array: [1, 2, 3, 4, 5]
len:   5
first: 1
last:  5
rest:  [2, 3, 4, 5]
push:  [1, 2, 3, 4, 5, 6]
pop:   [1, 2, 3, 4]
zip:   [[1, a], [2, b]]
```

### Tests

The tests are the real verification — there is no program output to eyeball in a library. They cover three things the registry promises: arity failures return errors, wrong-type arguments return errors, and the collection operations never mutate their input. The immutability checks are the load-bearing ones: each calls the operation and then asserts the original's length is unchanged.

Create `collections_test.go`:

```go
package builtins

import (
	"fmt"
	"testing"
)

func TestRegistryPopulated(t *testing.T) {
	t.Parallel()

	required := []string{"len", "push", "pop", "first", "last", "rest", "zip"}
	for _, name := range required {
		if Lookup(name) == nil {
			t.Errorf("built-in %q not registered", name)
		}
	}
}

func TestDispatchUnknownReturnsError(t *testing.T) {
	t.Parallel()

	result := Dispatch("doesNotExist")
	if result.Type() != ERROR_OBJ {
		t.Fatalf("want ERROR, got %s", result.Type())
	}
}

func TestBuiltinLen(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		arg  Object
		want int64
	}{
		{"string", &String{Value: "hello"}, 5},
		{"empty string", &String{Value: ""}, 0},
		{"array", &Array{Elements: []Object{&Integer{Value: 1}, &Integer{Value: 2}}}, 2},
		{"empty array", &Array{Elements: []Object{}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := Dispatch("len", tc.arg)
			got, ok := result.(*Integer)
			if !ok {
				t.Fatalf("want Integer, got %T (%s)", result, result.Inspect())
			}
			if got.Value != tc.want {
				t.Fatalf("len = %d, want %d", got.Value, tc.want)
			}
		})
	}
}

func TestBuiltinLenWrongType(t *testing.T) {
	t.Parallel()

	result := Dispatch("len", &Integer{Value: 5})
	if result.Type() != ERROR_OBJ {
		t.Fatalf("want ERROR for len(INTEGER), got %s", result.Type())
	}
}

func TestBuiltinLenArityError(t *testing.T) {
	t.Parallel()

	result := Dispatch("len")
	if result.Type() != ERROR_OBJ {
		t.Fatalf("want ERROR for len() with no args, got %s", result.Type())
	}
}

func TestBuiltinPushReturnsCopy(t *testing.T) {
	t.Parallel()

	orig := &Array{Elements: []Object{&Integer{Value: 1}}}
	result := Dispatch("push", orig, &Integer{Value: 2})
	arr, ok := result.(*Array)
	if !ok {
		t.Fatalf("push returned %T", result)
	}
	if len(arr.Elements) != 2 {
		t.Fatalf("len(arr) = %d, want 2", len(arr.Elements))
	}
	if len(orig.Elements) != 1 {
		t.Fatal("push mutated the original array")
	}
}

func TestBuiltinPopReturnsCopy(t *testing.T) {
	t.Parallel()

	orig := &Array{Elements: []Object{&Integer{Value: 1}, &Integer{Value: 2}}}
	result := Dispatch("pop", orig)
	arr, ok := result.(*Array)
	if !ok {
		t.Fatalf("pop returned %T", result)
	}
	if len(arr.Elements) != 1 {
		t.Fatalf("len(arr) = %d, want 1", len(arr.Elements))
	}
	if len(orig.Elements) != 2 {
		t.Fatal("pop mutated the original array")
	}
}

func TestBuiltinPopEmptyReturnsNull(t *testing.T) {
	t.Parallel()

	result := Dispatch("pop", &Array{Elements: []Object{}})
	if result.Type() != NULL_OBJ {
		t.Fatalf("pop(empty) = %s, want NULL", result.Type())
	}
}

func TestBuiltinFirstLastRestEdgeCases(t *testing.T) {
	t.Parallel()

	empty := &Array{Elements: []Object{}}

	if Dispatch("first", empty).Type() != NULL_OBJ {
		t.Fatal("first(empty) should be null")
	}
	if Dispatch("last", empty).Type() != NULL_OBJ {
		t.Fatal("last(empty) should be null")
	}
	if Dispatch("rest", empty).Type() != NULL_OBJ {
		t.Fatal("rest(empty) should be null")
	}

	arr := &Array{Elements: []Object{
		&Integer{Value: 10}, &Integer{Value: 20}, &Integer{Value: 30},
	}}
	if got := Dispatch("first", arr).(*Integer).Value; got != 10 {
		t.Fatalf("first = %d, want 10", got)
	}
	if got := Dispatch("last", arr).(*Integer).Value; got != 30 {
		t.Fatalf("last = %d, want 30", got)
	}
	rest := Dispatch("rest", arr).(*Array)
	if len(rest.Elements) != 2 {
		t.Fatalf("rest len = %d, want 2", len(rest.Elements))
	}
}

func TestBuiltinZip(t *testing.T) {
	t.Parallel()

	a := &Array{Elements: []Object{&Integer{Value: 1}, &Integer{Value: 2}}}
	b := &Array{Elements: []Object{&String{Value: "a"}, &String{Value: "b"}}}
	result := Dispatch("zip", a, b).(*Array)
	if len(result.Elements) != 2 {
		t.Fatalf("zip len = %d, want 2", len(result.Elements))
	}
	pair0 := result.Elements[0].(*Array)
	if pair0.Elements[0].(*Integer).Value != 1 {
		t.Fatal("zip pair 0 element 0 wrong")
	}
	if pair0.Elements[1].(*String).Value != "a" {
		t.Fatal("zip pair 0 element 1 wrong")
	}
}

// ExampleDispatch shows the Dispatch function looking up and calling len.
func ExampleDispatch() {
	arr := &Array{Elements: []Object{
		&Integer{Value: 10}, &Integer{Value: 20}, &Integer{Value: 30},
	}}
	fmt.Println(Dispatch("len", arr).Inspect())
	// Output: 3
}
```

## Review

The module is correct when the registry enforces the contract and the collection operations are pure. Confirm that `Dispatch` on an unknown name and on a wrong argument count both return an `*Error` rather than panicking, that `len` is a byte count on strings and an element count on arrays and hashes, and that `push`, `pop`, and `rest` leave their input array's length unchanged — the immutability tests are the ones that catch the most damaging regression. `first`, `last`, and `rest` must return the `null` singleton on an empty array, never index out of range.

Common mistakes for this module. Writing `arr.Elements = append(arr.Elements, ...)` in `push` mutates every binding that shares the array; allocate and copy instead. Re-checking `len(args)` inside a handler duplicates what `Dispatch` already guarantees and lets the two error messages drift apart. Decoding UTF-8 in `len` would make it O(n) and disagree with the byte-oriented string operations layered on top of it; keep it a byte count.

## Resources

- [Writing An Interpreter In Go (Thorsten Ball)](https://interpreterbook.com/) — the chapter on built-in functions introduces this exact registry and arity-checking pattern.
- [`strings` package](https://pkg.go.dev/strings) — `Join`, used by `Array.Inspect` and `Hash.Inspect`.
- [Go slices: usage and internals](https://go.dev/blog/slices-intro) — why `make`-plus-`copy` is required to return a new array without aliasing the original's backing storage.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-higher-order-builtins.md](02-higher-order-builtins.md)
