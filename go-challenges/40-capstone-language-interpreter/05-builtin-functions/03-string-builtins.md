# Exercise 3: String Built-ins

Strings are the most-used data type in scripts, so the standard library exposes a full slate of string operations: splitting and joining, trimming, case folding, substring tests, replacement, and a `{}`-placeholder formatter. Each is a thin, type-guarded wrapper over a `strings` package function — the lesson is in the uniform error shape and in `format`, the one operation that is not a direct delegation.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
object.go        the runtime Object types, singletons, BoolObj
registry.go      Builtin, RegisterBuiltin, Dispatch, checkArgs, newError
strings.go       split, join, trim, upper, lower, contains, replace, format, ...
cmd/
  demo/
    main.go      upper / split / join / format over fixed strings
strings_test.go  delegation correctness, the non-string-element join error, format cases
```

- Files: `object.go`, `registry.go`, `strings.go`, `cmd/demo/main.go`, `strings_test.go`.
- Implement: `split`, `join`, `trim`, `trimLeft`, `trimRight`, `upper`, `lower`, `contains`, `replace`, `startsWith`, `endsWith`, `format`, and the registry framework they dispatch through.
- Test: that each delegates to the right `strings` function, that `join` rejects a non-string element, and that `format` substitutes, passes through literal text, and leaves an unmatched `{}` intact.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/40-capstone-language-interpreter/05-builtin-functions/03-string-builtins/cmd/demo && cd go-solutions/40-capstone-language-interpreter/05-builtin-functions/03-string-builtins
```

### What the wrappers share, and where format differs

Eleven of the twelve built-ins are mechanical: assert each argument is a `*String` (or, for `join`, a `*Array` of strings), call the matching `strings` function, and wrap the result. The value is consistency — every type failure produces the same `name: arg N: want STRING, got T` shape, so a script author learns one error format and recognizes it everywhere. `split` returns an `*Array` of `*String`; `join` walks an array and fails with a precise element index if any entry is not a string; the boolean tests (`contains`, `startsWith`, `endsWith`) return the `BoolObj` singleton.

`format` is the one with real logic. It scans the template for `{}` pairs and substitutes the `Inspect()` of successive arguments. Two edge cases define its behavior: literal text passes through untouched, and a `{}` with no remaining argument is left verbatim rather than erroring — a forgiving choice that makes partial templates debuggable instead of fatal. The scan is a manual byte loop because it must distinguish the two-byte sequence `{}` from any other character without a regular expression.

Create `object.go`:

```go
package strbuiltins

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

The same dispatch substrate. The string built-ins use `newError` for their type guards and rely on `Dispatch` to enforce the registered arity before the handler runs.

Create `registry.go`:

```go
package strbuiltins

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

### The string built-ins

Create `strings.go`:

```go
package strbuiltins

import "strings"

func init() {
	RegisterBuiltin("split", builtinSplit, WithArity(2, 2),
		WithDoc("split(str, delim) – split str on delim, return array of strings"))
	RegisterBuiltin("join", builtinJoin, WithArity(2, 2),
		WithDoc("join(arr, delim) – join string array with delim"))
	RegisterBuiltin("trim", builtinTrim, WithArity(1, 1),
		WithDoc("trim(str) – remove leading and trailing Unicode whitespace"))
	RegisterBuiltin("trimLeft", builtinTrimLeft, WithArity(1, 1),
		WithDoc("trimLeft(str) – remove leading ASCII whitespace"))
	RegisterBuiltin("trimRight", builtinTrimRight, WithArity(1, 1),
		WithDoc("trimRight(str) – remove trailing ASCII whitespace"))
	RegisterBuiltin("upper", builtinUpper, WithArity(1, 1),
		WithDoc("upper(str) – convert to uppercase"))
	RegisterBuiltin("lower", builtinLower, WithArity(1, 1),
		WithDoc("lower(str) – convert to lowercase"))
	RegisterBuiltin("contains", builtinContains, WithArity(2, 2),
		WithDoc("contains(str, substr) – return boolean"))
	RegisterBuiltin("replace", builtinReplace, WithArity(3, 3),
		WithDoc("replace(str, old, new) – replace all occurrences"))
	RegisterBuiltin("startsWith", builtinStartsWith, WithArity(2, 2),
		WithDoc("startsWith(str, prefix) – return boolean"))
	RegisterBuiltin("endsWith", builtinEndsWith, WithArity(2, 2),
		WithDoc("endsWith(str, suffix) – return boolean"))
	RegisterBuiltin("format", builtinFormat, WithArity(1, -1),
		WithDoc("format(tmpl, args...) – replace {} placeholders with args"))
}

func builtinSplit(args ...Object) Object {
	s, ok := args[0].(*String)
	if !ok {
		return newError("split: arg 1: want STRING, got %s", args[0].Type())
	}
	delim, ok := args[1].(*String)
	if !ok {
		return newError("split: arg 2: want STRING, got %s", args[1].Type())
	}
	parts := strings.Split(s.Value, delim.Value)
	elems := make([]Object, len(parts))
	for i, p := range parts {
		elems[i] = &String{Value: p}
	}
	return &Array{Elements: elems}
}

func builtinJoin(args ...Object) Object {
	arr, ok := args[0].(*Array)
	if !ok {
		return newError("join: arg 1: want ARRAY, got %s", args[0].Type())
	}
	delim, ok := args[1].(*String)
	if !ok {
		return newError("join: arg 2: want STRING, got %s", args[1].Type())
	}
	parts := make([]string, len(arr.Elements))
	for i, el := range arr.Elements {
		s, ok := el.(*String)
		if !ok {
			return newError("join: element %d: want STRING, got %s", i, el.Type())
		}
		parts[i] = s.Value
	}
	return &String{Value: strings.Join(parts, delim.Value)}
}

func builtinTrim(args ...Object) Object {
	s, ok := args[0].(*String)
	if !ok {
		return newError("trim: arg 1: want STRING, got %s", args[0].Type())
	}
	return &String{Value: strings.TrimSpace(s.Value)}
}

func builtinTrimLeft(args ...Object) Object {
	s, ok := args[0].(*String)
	if !ok {
		return newError("trimLeft: arg 1: want STRING, got %s", args[0].Type())
	}
	return &String{Value: strings.TrimLeft(s.Value, " \t\n\r\v\f")}
}

func builtinTrimRight(args ...Object) Object {
	s, ok := args[0].(*String)
	if !ok {
		return newError("trimRight: arg 1: want STRING, got %s", args[0].Type())
	}
	return &String{Value: strings.TrimRight(s.Value, " \t\n\r\v\f")}
}

func builtinUpper(args ...Object) Object {
	s, ok := args[0].(*String)
	if !ok {
		return newError("upper: arg 1: want STRING, got %s", args[0].Type())
	}
	return &String{Value: strings.ToUpper(s.Value)}
}

func builtinLower(args ...Object) Object {
	s, ok := args[0].(*String)
	if !ok {
		return newError("lower: arg 1: want STRING, got %s", args[0].Type())
	}
	return &String{Value: strings.ToLower(s.Value)}
}

func builtinContains(args ...Object) Object {
	s, ok := args[0].(*String)
	if !ok {
		return newError("contains: arg 1: want STRING, got %s", args[0].Type())
	}
	sub, ok := args[1].(*String)
	if !ok {
		return newError("contains: arg 2: want STRING, got %s", args[1].Type())
	}
	return BoolObj(strings.Contains(s.Value, sub.Value))
}

func builtinReplace(args ...Object) Object {
	s, ok := args[0].(*String)
	if !ok {
		return newError("replace: arg 1: want STRING, got %s", args[0].Type())
	}
	old, ok := args[1].(*String)
	if !ok {
		return newError("replace: arg 2: want STRING, got %s", args[1].Type())
	}
	repl, ok := args[2].(*String)
	if !ok {
		return newError("replace: arg 3: want STRING, got %s", args[2].Type())
	}
	return &String{Value: strings.ReplaceAll(s.Value, old.Value, repl.Value)}
}

func builtinStartsWith(args ...Object) Object {
	s, ok := args[0].(*String)
	if !ok {
		return newError("startsWith: arg 1: want STRING, got %s", args[0].Type())
	}
	prefix, ok := args[1].(*String)
	if !ok {
		return newError("startsWith: arg 2: want STRING, got %s", args[1].Type())
	}
	return BoolObj(strings.HasPrefix(s.Value, prefix.Value))
}

func builtinEndsWith(args ...Object) Object {
	s, ok := args[0].(*String)
	if !ok {
		return newError("endsWith: arg 1: want STRING, got %s", args[0].Type())
	}
	suffix, ok := args[1].(*String)
	if !ok {
		return newError("endsWith: arg 2: want STRING, got %s", args[1].Type())
	}
	return BoolObj(strings.HasSuffix(s.Value, suffix.Value))
}

// builtinFormat replaces {} placeholders in tmpl with the Inspect() of
// successive arguments. A {} with no remaining argument is left intact.
func builtinFormat(args ...Object) Object {
	tmpl, ok := args[0].(*String)
	if !ok {
		return newError("format: arg 1: want STRING, got %s", args[0].Type())
	}
	var sb strings.Builder
	argIdx := 1
	s := tmpl.Value
	for i := 0; i < len(s); {
		if i+1 < len(s) && s[i] == '{' && s[i+1] == '}' {
			if argIdx < len(args) {
				sb.WriteString(args[argIdx].Inspect())
				argIdx++
			} else {
				sb.WriteString("{}")
			}
			i += 2
		} else {
			sb.WriteByte(s[i])
			i++
		}
	}
	return &String{Value: sb.String()}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/strbuiltins"
)

func main() {
	fmt.Println("upper: ", strbuiltins.Dispatch("upper", &strbuiltins.String{Value: "hello"}).Inspect())

	parts := strbuiltins.Dispatch("split", &strbuiltins.String{Value: "a,b,c"}, &strbuiltins.String{Value: ","})
	fmt.Println("split: ", parts.Inspect())

	arr := &strbuiltins.Array{Elements: []strbuiltins.Object{
		&strbuiltins.String{Value: "a"},
		&strbuiltins.String{Value: "b"},
		&strbuiltins.String{Value: "c"},
	}}
	fmt.Println("join:  ", strbuiltins.Dispatch("join", arr, &strbuiltins.String{Value: "-"}).Inspect())

	tmpl := &strbuiltins.String{Value: "Hello, {}! You have {} messages."}
	out := strbuiltins.Dispatch("format", tmpl, &strbuiltins.String{Value: "World"}, &strbuiltins.Integer{Value: 3})
	fmt.Println("format:", out.Inspect())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
upper:  HELLO
split:  [a, b, c]
join:   a-b-c
format: Hello, World! You have 3 messages.
```

### Tests

Create `strings_test.go`:

```go
package strbuiltins

import (
	"fmt"
	"testing"
)

func TestRegistryPopulated(t *testing.T) {
	t.Parallel()

	required := []string{
		"split", "join", "trim", "trimLeft", "trimRight",
		"upper", "lower", "contains", "replace",
		"startsWith", "endsWith", "format",
	}
	for _, name := range required {
		if Lookup(name) == nil {
			t.Errorf("built-in %q not registered", name)
		}
	}
}

func TestBuiltinSplit(t *testing.T) {
	t.Parallel()

	result := Dispatch("split", &String{Value: "a,b,c"}, &String{Value: ","}).(*Array)
	if len(result.Elements) != 3 {
		t.Fatalf("split len = %d, want 3", len(result.Elements))
	}
	if result.Elements[1].(*String).Value != "b" {
		t.Fatal("split element 1 wrong")
	}
}

func TestBuiltinJoin(t *testing.T) {
	t.Parallel()

	arr := &Array{Elements: []Object{
		&String{Value: "hello"}, &String{Value: "world"},
	}}
	result := Dispatch("join", arr, &String{Value: " "})
	if result.(*String).Value != "hello world" {
		t.Fatalf("join = %q, want %q", result.(*String).Value, "hello world")
	}
}

func TestBuiltinJoinNonStringElement(t *testing.T) {
	t.Parallel()

	arr := &Array{Elements: []Object{&String{Value: "a"}, &Integer{Value: 1}}}
	result := Dispatch("join", arr, &String{Value: ","})
	if result.Type() != ERROR_OBJ {
		t.Fatalf("join with non-string element should error, got %s", result.Type())
	}
}

func TestBuiltinTrimFunctions(t *testing.T) {
	t.Parallel()

	s := &String{Value: "  hello  "}
	if Dispatch("trim", s).(*String).Value != "hello" {
		t.Fatal("trim wrong")
	}
	if Dispatch("trimLeft", s).(*String).Value != "hello  " {
		t.Fatal("trimLeft wrong")
	}
	if Dispatch("trimRight", s).(*String).Value != "  hello" {
		t.Fatal("trimRight wrong")
	}
}

func TestBuiltinStringCaseFunctions(t *testing.T) {
	t.Parallel()

	s := &String{Value: "Hello World"}
	if Dispatch("upper", s).(*String).Value != "HELLO WORLD" {
		t.Fatal("upper wrong")
	}
	if Dispatch("lower", s).(*String).Value != "hello world" {
		t.Fatal("lower wrong")
	}
}

func TestBuiltinContainsStartsEnds(t *testing.T) {
	t.Parallel()

	s := &String{Value: "foobar"}
	if Dispatch("contains", s, &String{Value: "oba"}).(*Boolean).Value != true {
		t.Fatal("contains wrong")
	}
	if Dispatch("startsWith", s, &String{Value: "foo"}).(*Boolean).Value != true {
		t.Fatal("startsWith wrong")
	}
	if Dispatch("endsWith", s, &String{Value: "bar"}).(*Boolean).Value != true {
		t.Fatal("endsWith wrong")
	}
}

func TestBuiltinReplace(t *testing.T) {
	t.Parallel()

	result := Dispatch("replace",
		&String{Value: "aabbcc"},
		&String{Value: "b"},
		&String{Value: "X"},
	)
	if result.(*String).Value != "aaXXcc" {
		t.Fatalf("replace = %q, want %q", result.(*String).Value, "aaXXcc")
	}
}

func TestBuiltinFormat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tmpl string
		args []Object
		want string
	}{
		{
			"Hello, {}!",
			[]Object{&String{Value: "World"}},
			"Hello, World!",
		},
		{
			"{} + {} = {}",
			[]Object{&Integer{Value: 1}, &Integer{Value: 2}, &Integer{Value: 3}},
			"1 + 2 = 3",
		},
		{
			"no placeholders",
			nil,
			"no placeholders",
		},
		{
			"extra {} placeholder",
			nil,
			"extra {} placeholder",
		},
	}
	for _, tc := range cases {
		t.Run(tc.tmpl, func(t *testing.T) {
			t.Parallel()
			args := []Object{&String{Value: tc.tmpl}}
			args = append(args, tc.args...)
			result := Dispatch("format", args...)
			if result.(*String).Value != tc.want {
				t.Fatalf("format = %q, want %q", result.(*String).Value, tc.want)
			}
		})
	}
}

// ExampleDispatch upper-cases a string through the registry.
func ExampleDispatch() {
	fmt.Println(Dispatch("upper", &String{Value: "monkey"}).Inspect())
	// Output: MONKEY
}
```

## Review

The module is correct when each built-in delegates to the matching `strings` function and reports type errors in the uniform shape. Confirm that `split` returns an `*Array` of `*String`, that `join` fails with a precise element index when an entry is not a string, that the boolean predicates return the shared `BoolObj` singleton, and that `format` substitutes successive arguments, passes literal text through, and leaves a `{}` with no matching argument untouched rather than erroring.

Common mistakes for this feature. Using `strings.Replace` with a count instead of `strings.ReplaceAll` replaces only the first occurrence. Reaching for a regular expression in `format` is both slower and wrong for literal braces; the two-byte scan is deliberate. Returning a fresh `&Boolean{}` from the predicates instead of `BoolObj` discards the singleton and allocates needlessly. And remember `len`/`split` operate on bytes, so a multibyte delimiter splits on the exact byte sequence, which is what `strings.Split` already does.

## Resources

- [`strings` package](https://pkg.go.dev/strings) — `Split`, `Join`, `TrimSpace`, `TrimLeft`, `TrimRight`, `ToUpper`, `ToLower`, `Contains`, `ReplaceAll`, `HasPrefix`, `HasSuffix`.
- [`strings.Builder`](https://pkg.go.dev/strings#Builder) — the zero-allocation buffer `format` accumulates into.
- [Writing An Interpreter In Go (Thorsten Ball)](https://interpreterbook.com/) — the built-in chapter's approach to wrapping host-language functions as language built-ins.

---

Back to [02-higher-order-builtins.md](02-higher-order-builtins.md) | Next: [04-type-conversion-builtins.md](04-type-conversion-builtins.md)
