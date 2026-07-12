# Exercise 3: Recursive Closures and the Forward-Reference Pattern

A recursive function names itself in its own body: `let fact = fn(n) { if (n < 2) { 1 } else { n * fact(n - 1) } }` references `fact` from inside `fact`. The reason this works in a pointer-based environment is that the function's captured `Env` and the environment the evaluator binds the name into are the same `*Environment`. After `Set("fact", f)`, a lookup of `fact` through the captured environment returns `f`, and because the body only performs that lookup when it runs, the order of creating the function and binding the name does not matter. This exercise builds a self-contained recursive `Function` whose body resolves its own name through its captured environment, proves the binding resolves to the function itself, and shows the order-independence that distinguishes a pointer-based environment from a value-copy one.

This module is fully self-contained. It depends on nothing but the standard library, ships its own demo and tests, and imports no other exercise.

## What you'll build

```text
object.go            Object, Null, Parameter, Body, Function
environment.go       Environment, NewEnvironment, NewEnclosedEnvironment, Get, Set
recursion.go         NewFactorial, Apply
recursion_test.go    name resolves to self; factorial computes; order is irrelevant
cmd/
  demo/
    main.go          build the factorial closure and run it
```

- Files: `object.go`, `environment.go`, `recursion.go`, `recursion_test.go`, `cmd/demo/main.go`.
- Implement: a `Function` whose `Body` resolves its own name through `Env`, `NewFactorial` that wires the forward reference, and `Apply` that runs it.
- Test: the captured environment resolves the function's name to the function itself, the factorial computes correctly, and binding the name after the function is created still works.
- Verify: `go test -race ./...` then `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/40-capstone-language-interpreter/06-closures-first-class-functions/03-recursive-closures-forward-reference/cmd/demo && cd go-solutions/40-capstone-language-interpreter/06-closures-first-class-functions/03-recursive-closures-forward-reference
```

### Why the order does not matter here

The two-step forward-reference convention is: bind the name to a placeholder (`Null`) first, create the function so it captures the environment, then update the binding to point at the function. In an environment that copied its contents at creation time, that ordering would be mandatory — the function would otherwise capture a snapshot taken before the name existed and could never find itself. In this pointer-based design the placeholder is optional, because `Function.Env` and the environment the name is bound into are one live map, and the body resolves the name only at call time. Whether the function value is created before or after `Set("fact", f)`, a later call walks the current map and finds `f`. The placeholder still earns its place: it documents that a forward reference is intended and keeps the code portable to value-copy designs, which is why the implementation below uses it even though it is not strictly required.

To make the recursion observable, the function's `Body` is a Go closure that takes the captured environment and resolves `fact` from it on every recursive step, exactly as a tree-walking evaluator would resolve the identifier in the AST body. The recursion is therefore driven through the environment lookup, not through a Go-level reference to the variable, which is what proves the closure mechanism rather than Go's own scoping.

### The object system

Create `object.go`:

```go
package recursion

// Object is the interface every runtime value implements.
type Object interface {
	Inspect() string
}

// Null is the forward-reference placeholder bound before the function exists.
type Null struct{}

func (n *Null) Inspect() string { return "null" }

// Parameter is a named function parameter.
type Parameter struct{ Name string }

// Body evaluates the function for argument n, using env to resolve free names
// including the function's own name for the recursive call.
type Body func(n int64, env *Environment) int64

// Function captures its parameters, a body, and the defining environment by
// pointer. The pointer is why a name bound into Env after the Function is
// created is still visible to the body at call time.
type Function struct {
	Parameters []*Parameter
	Body       Body
	Env        *Environment
}

func (f *Function) Inspect() string { return "fn" }
```

### The environment

Recursion needs only read and local-write; assignment is the previous exercise's concern.

Create `environment.go`:

```go
package recursion

// Environment is a linked chain of variable scopes.
type Environment struct {
	store map[string]Object
	outer *Environment
}

// NewEnvironment creates a top-level (global) environment.
func NewEnvironment() *Environment {
	return &Environment{store: make(map[string]Object)}
}

// NewEnclosedEnvironment creates a new scope that extends outer.
func NewEnclosedEnvironment(outer *Environment) *Environment {
	env := NewEnvironment()
	env.outer = outer
	return env
}

// Get looks up name, walking the chain toward the global scope.
func (e *Environment) Get(name string) (Object, bool) {
	obj, ok := e.store[name]
	if !ok && e.outer != nil {
		return e.outer.Get(name)
	}
	return obj, ok
}

// Set creates or shadows name in the current scope only.
func (e *Environment) Set(name string, val Object) Object {
	e.store[name] = val
	return val
}
```

### The recursive function

`NewFactorial` wires the forward reference: it binds `fact` to a `Null` placeholder, builds the function whose body looks `fact` up through the captured environment on each recursive step, then updates the binding to the real function. `Apply` starts the recursion.

Create `recursion.go`:

```go
package recursion

// NewFactorial builds the closure for:
//
//	let fact = fn(n) { if (n < 2) { 1 } else { n * fact(n - 1) } }
//
// using the two-step forward reference: bind a placeholder, create the
// function (capturing the environment), then bind the name to the function.
func NewFactorial() *Function {
	env := NewEnvironment()
	env.Set("fact", &Null{}) // forward-reference placeholder

	f := &Function{
		Parameters: []*Parameter{{Name: "n"}},
		Env:        env, // env already contains "fact" (as Null)
	}
	f.Body = func(n int64, e *Environment) int64 {
		if n < 2 {
			return 1
		}
		// Resolve "fact" from the captured environment and recurse through it.
		self, _ := e.Get("fact")
		fn := self.(*Function)
		return n * fn.Body(n-1, fn.Env)
	}

	env.Set("fact", f) // update: "fact" now resolves to f
	return f
}

// Apply runs f against n, starting the recursion in f's captured environment.
func Apply(f *Function, n int64) int64 {
	return f.Body(n, f.Env)
}
```

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/recursion"
)

func main() {
	fact := recursion.NewFactorial()

	// The captured environment resolves "fact" to the function itself, so the
	// body can recurse by name.
	self, _ := fact.Env.Get("fact")
	fmt.Printf("fact resolves to itself in its captured env: %t\n", self == recursion.Object(fact))

	for _, n := range []int64{0, 1, 5} {
		fmt.Printf("fact(%d) = %d\n", n, recursion.Apply(fact, n))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fact resolves to itself in its captured env: true
fact(0) = 1
fact(1) = 1
fact(5) = 120
```

### Tests

The first test pins the forward reference: the captured environment must resolve `fact` to the function itself. The second runs the recursion end to end. The third is the order-independence proof — it creates the function value before binding its name, and the recursion still works because the captured `Env` is a live pointer, not a snapshot.

Create `recursion_test.go`:

```go
package recursion

import "testing"

func TestForwardReferenceResolvesToSelf(t *testing.T) {
	t.Parallel()

	fact := NewFactorial()
	self, ok := fact.Env.Get("fact")
	if !ok {
		t.Fatal("forward reference: fact not found in captured env")
	}
	if self != fact {
		t.Fatal("forward reference: fact does not resolve to the function itself")
	}
}

func TestFactorialComputes(t *testing.T) {
	t.Parallel()

	fact := NewFactorial()
	cases := map[int64]int64{0: 1, 1: 1, 5: 120, 6: 720}
	for n, want := range cases {
		if got := Apply(fact, n); got != want {
			t.Fatalf("fact(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestPointerCaptureOrderIndependent(t *testing.T) {
	t.Parallel()

	// Create the function value BEFORE binding its name. Because Env is a
	// pointer to a shared map, the later Set is still visible to the body,
	// so the recursion resolves "fact" correctly at call time.
	env := NewEnvironment()
	f := &Function{Parameters: []*Parameter{{Name: "n"}}, Env: env}
	f.Body = func(n int64, e *Environment) int64 {
		if n < 2 {
			return 1
		}
		self, _ := e.Get("fact")
		fn := self.(*Function)
		return n * fn.Body(n-1, fn.Env)
	}
	env.Set("fact", f) // bound after creation; pointer capture makes this work

	if got := Apply(f, 5); got != 120 {
		t.Fatalf("fact(5) = %d, want 120 (pointer capture must make order irrelevant)", got)
	}
}
```

## Review

The recursion is correct when the captured environment resolves the function's name to the function itself and the factorial computes the expected values. The order-independence test is the conceptual payoff: building the function before binding its name still produces a working recursion, because the captured `Env` is a shared pointer and the name lookup is deferred to call time. Confirm `go test -race ./...` is clean, and note that the forward-reference placeholder is a convention here, not a requirement — it would become mandatory only if the environment captured a value copy at creation time.

The misconception to discard is that recursion needs the name bound before the function is created, or needs a snapshot of the environment. In this pointer design neither is true; the body walks the live map whenever it runs.

## Resources

- [Writing An Interpreter In Go (Thorsten Ball)](https://interpreterbook.com/) — the evaluation chapter relies on the environment pointer to make recursive `let` bindings resolve.
- [Crafting Interpreters: Closures (Nystrom)](https://craftinginterpreters.com/closures.html) — how recursive and mutually recursive bindings resolve through environments.
- [Go Specification: Function declarations](https://go.dev/ref/spec#Function_declarations) — Go functions may refer to themselves, the language-level analog of this forward reference.

---

Back to [02-mutable-closures-the-counter-pattern.md](02-mutable-closures-the-counter-pattern.md) | Next: [04-closures-in-loops-per-iteration-scope.md](04-closures-in-loops-per-iteration-scope.md)
