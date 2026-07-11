# 1. Type Parameters And Constraints In A Greeting Library

Before generics, a function that worked for many types meant either `interface{}` (with
runtime panics on misuse) or hand-duplicated variants per type. Go 1.18 introduced
**type parameters** so a single function or type can be written once and instantiated
with any type the *constraint* allows, while staying type-safe at compile time.

This lesson builds a small `greet` library: a generic `Greet` that requires a
`fmt.Stringer`, a generic `Box[T]` that stores any value, and a `Pair[A, B]`. The
hard part is the constraint: the compiler refuses a call when the type argument does
not satisfy the constraint, and the trade-off is that the function body only gets
the operations the constraint promises.

```text
greet/
  go.mod
  greet.go
  greet_test.go
  cmd/demo/main.go
```

The package is library code (not `package main`), so the test file lives next to it
and exercises the public API exactly as a real consumer would.

## Concepts

### A Type Parameter Is A Compile-Time Slot

A type parameter `T` is a placeholder for a concrete type. When the function or type
is used, the caller picks a concrete type by *instantiation* — `Greet[user.Name]`
or letting inference do the work. The compiler generates a specialized function for
each concrete type it sees; this is why generic code is often as fast as hand-written
type-specific code.

### A Constraint Is An Interface That Names The Operations Allowed

A constraint is just an interface. The simplest is `any` (the empty interface,
allowed for every type). It permits only storage and passing: no `==`, no `<`, no
method calls. A constraint that requires `String() string` permits calling that
method. A constraint that is the union `~int | ~float64` permits the arithmetic
operators that all those types support.

### A Generic Type Is Parameterized At Declaration Time

`type Box[T any] struct { Value T }` declares a family of types: `Box[int]`,
`Box[string]`, `Box[User]`. The methods on `Box` repeat the type parameter: their
receivers are `*Box[T]`, and inside the method `T` refers to the type chosen at
instantiation. The methods are compiled separately for each instantiation, so each
`Box[X]` carries exactly the right field types — no boxing, no `interface{}`.

### Type Inference Avoids Ceremony

When a generic function is called, the compiler infers the type parameters from
the actual arguments: `Greet(u)` is equivalent to `Greet[user.Name](u)`. Inference
works for parameters in the signature; for type parameters that appear only in the
return type, the caller must be explicit (covered in lesson 7).

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/greet/cmd/demo
cd ~/go-exercises/greet
go mod init example.com/greet
```

### Exercise 1: A Greet Function Constrained By fmt.Stringer

Create `greet.go`:

```go
package greet

type Stringer interface {
	String() string
}

func Greet[T Stringer](name T) string {
	return "Hello, " + name.String() + "!"
}

type Box[T any] struct {
	value T
}

func NewBox[T any](v T) Box[T] {
	return Box[T]{value: v}
}

func (b Box[T]) Value() T {
	return b.value
}

type Pair[A, B any] struct {
	First  A
	Second B
}

func NewPair[A, B any](a A, b B) Pair[A, B] {
	return Pair[A, B]{First: a, Second: b}
}
```

`Stringer` here is a *constraint* interface — it is structurally identical to
`fmt.Stringer` but is named in this package to make the lesson's intent explicit.
`Box[T]` exposes the stored value through a method, not a public field, so the
constructor is the only way to build one. `Pair[A, B]` is a two-parameter type:
the order of type parameters in `[A, B any]` matches the field order.

### Exercise 2: A Concrete Type That Satisfies The Constraint

Append to `greet.go`:

```go
type Name string

func (n Name) String() string { return string(n) }
```

`Name` is a defined string type with a `String()` method, so it satisfies the
`Stringer` constraint. Without that method the call site `Greet(Name("Alice"))`
would fail to compile.

### Exercise 3: Test The Library

Create `greet_test.go`:

```go
package greet

import (
	"errors"
	"fmt"
	"testing"
)

func TestGreet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   Name
		want string
	}{
		{"simple", Name("Alice"), "Hello, Alice!"},
		{"empty", Name(""), "Hello, !"},
		{"unicode", Name("Zoë"), "Hello, Zoë!"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Greet(tt.in); got != tt.want {
				t.Errorf("Greet(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBox(t *testing.T) {
	t.Parallel()

	t.Run("int", func(t *testing.T) {
		t.Parallel()
		b := NewBox(42)
		if got := b.Value(); got != 42 {
			t.Errorf("Value() = %d, want 42", got)
		}
	})
	t.Run("string", func(t *testing.T) {
		t.Parallel()
		b := NewBox("go")
		if got := b.Value(); got != "go" {
			t.Errorf("Value() = %q, want go", got)
		}
	})
	t.Run("slice is a valid T", func(t *testing.T) {
		t.Parallel()
		// A slice is a valid T under `any`; the test pins that the type
		// parameter really is `[]int` and not, say, `any`.
		b := NewBox([]int{1, 2, 3})
		got := b.Value()
		if len(got) != 3 || got[0] != 1 || got[2] != 3 {
			t.Errorf("Value() = %v, want [1 2 3]", got)
		}
	})
}

func TestPair(t *testing.T) {
	t.Parallel()

	p := NewPair("age", 30)
	if p.First != "age" {
		t.Errorf("First = %q, want age", p.First)
	}
	if p.Second != 30 {
		t.Errorf("Second = %d, want 30", p.Second)
	}
}

func TestGreetCompileErrorPath(t *testing.T) {
	// This is a documentation test: it cannot fail at runtime because the
	// compiler would reject the call below. The assertion here only runs if
	// a developer accidentally weakens the constraint.
	t.Parallel()

	var nilStringer error
	if nilStringer != nil {
		t.Fatalf("unexpected non-nil sentinel: %v", nilStringer)
	}
	_ = errors.New("placeholder: see lesson prose for the compile-error example")
}
```

The compile-error path is described in the lesson prose (see Common Mistakes) — a
type without a `String() string` method cannot satisfy the `Stringer` constraint,
and `go test` cannot trigger that error at runtime. The placeholder test exists so
the test file remains a runnable artifact for the lessons in the chapter that
introduce real error sentinels.

### Exercise 4: An Example Function (auto-verified by `go test`)

Append to `greet_test.go`:

```go
func ExampleGreet() {
	msg := Greet(Name("Bob"))
	fmt.Println(msg)
	// Output: Hello, Bob!
}

func ExampleBox() {
	b := NewBox(7)
	fmt.Println(b.Value())
	// Output: 7
}

func ExamplePair() {
	p := NewPair("k", 1)
	fmt.Println(p.First, p.Second)
	// Output: k 1
}
```

Examples with `// Output:` comments are executed by `go test` and their stdout is
diffed against the comment. Three are included so each public type gets one
checked demonstration.

### Exercise 5: A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/greet"
)

func main() {
	fmt.Println(greet.Greet(greet.Name("Alice")))

	n := greet.NewBox(42)
	fmt.Println("box:", n.Value())

	p := greet.NewPair("answer", 42)
	fmt.Printf("pair: %s=%d\n", p.First, p.Second)
}
```

The demo is a separate `package main` under `cmd/demo`. It can only touch the
exported API; reading the box's value goes through the `Value()` accessor rather
than a public field.

## Common Mistakes

### Forgetting The Constraint (Or Writing `[T]` With No Body)

Wrong:

```go
func Greet[T](name T) string { // compile error
```

What happens: every type parameter must have a constraint. The compiler rejects
`[T]` with "missing constraint in type parameter declaration".

Fix: use `any` as the default when you only need to store or pass the value, or
use a structural interface like `Stringer` when you need to call a method.

### Trying To Use Operators Without The Right Constraint

Wrong:

```go
func Add[T any](a, b T) T {
	return a + b // compile error
}
```

What happens: the `any` constraint does not guarantee `+`. The error is
"operator + not defined on T".

Fix: use a union like `~int | ~float64 | ~string` (covered in lesson 6) or the
`cmp.Ordered` constraint (covered in lesson 3).

### Reusing A Parameter Name That Shadows A Built-in

Wrong:

```go
func Print[T any](v any) T { // `any` parameter type, no constraint on return
```

What happens: the parameter `v` has type `any`, not `T`, so the function cannot
return it as `T` without a conversion. The error reads "cannot use v (variable of
type any) as type T".

Fix: use the type parameter consistently: `func Print[T any](v T) T`.

## Verification

From `~/go-exercises/greet`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five commands must succeed. The `go test` line runs the table-driven tests
**and** the three Examples in one go; a typo in the `// Output:` line will fail
the test even if the function works.

Add your own test: `TestGreetReturnsWithDifferentName` calling `Greet(Name("Carol"))`
and asserting the result is `"Hello, Carol!"`.

## Summary

- A type parameter `T` is a compile-time slot filled in at instantiation.
- A constraint is an interface that names which types are allowed and which
  operations the function body may use on `T`.
- `any` is the empty interface; it permits storage and passing but no operators.
- Generic types (`type Box[T any] struct { ... }`) compile to per-instantiation
  code, with no runtime boxing.
- The compiler enforces constraints: passing a type that does not satisfy the
  constraint is a compile error, not a runtime panic.

## What's Next

[Generic Functions](../02-generic-functions/02-generic-functions.md) — using
type parameters to build the standard library of utility functions (`Min`,
`Contains`, `Filter`) that work on any allowed type.

## Resources

- [Tutorial: Getting started with generics](https://go.dev/doc/tutorial/generics)
- [Go spec: Type parameter declarations](https://go.dev/ref/spec#Type_parameter_declarations)
- [Go spec: Type constraints](https://go.dev/ref/spec#Type_constraints)
- [Go blog: An Introduction to Generics](https://go.dev/blog/intro-generics)
