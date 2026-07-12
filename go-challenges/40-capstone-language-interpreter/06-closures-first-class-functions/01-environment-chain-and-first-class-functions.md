# Exercise 1: The Environment Chain and First-Class Functions

A closure is a function value paired with the environment in which it was defined. This exercise builds the layer beneath the evaluator that makes that pairing real: an object system whose `Function` type stores a pointer to its defining environment, and an `Environment` type that resolves names by walking a chain of scopes. The single pointer in `Function.Env` is the entire trick — because it is a pointer and not a copy, the function sees the live contents of the scope it closed over, including changes made after the function object was created. The tests prove the closure invariants at this layer without a parser or AST: capture by pointer, lexical shadowing, free-variable resolution through the chain, independent factory captures, and correct call-time environment extension.

This module is fully self-contained. It depends on nothing but the standard library, ships its own demo and tests, and imports no other exercise.

## What you'll build

```text
object.go            Object, ObjectType, Integer, Boolean, Null, ReturnValue, Error, Parameter, Function
environment.go       Environment, NewEnvironment, NewEnclosedEnvironment, Get, Set, Update
evaluator.go         extendFunctionEnv, unwrapReturnValue
evaluator_test.go    closure invariants at the environment layer
cmd/
  demo/
    main.go          enclosed-scope resolution and independent function factories
```

- Files: `object.go`, `environment.go`, `evaluator.go`, `evaluator_test.go`, `cmd/demo/main.go`.
- Implement: the `Object` interface and its values, `Environment` with `Get`/`Set`/`Update`, `NewEnclosedEnvironment`, and the call-time helpers `extendFunctionEnv` and `unwrapReturnValue`.
- Test: capture by pointer, shadowing, chain resolution, factory independence, parameter binding plus arity checking, and return-value unwrapping.
- Verify: `go test -race ./...` then `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/40-capstone-language-interpreter/06-closures-first-class-functions/01-environment-chain-and-first-class-functions/cmd/demo && cd go-solutions/40-capstone-language-interpreter/06-closures-first-class-functions/01-environment-chain-and-first-class-functions
```

### The object system

Every runtime value implements `Object`. The type that matters for closures is `Function`: it holds its parameters, its body, and — the load-bearing field — a pointer to the `*Environment` that was alive when the function literal was evaluated. Storing a pointer rather than a copy is what makes a Monkey function a closure, exactly as Go's own function literals capture their free variables by reference. The `Body` field is a `string` here so the package compiles standalone; in the full interpreter it is an `*ast.BlockStatement`.

Create `object.go`:

```go
package closures

import (
	"fmt"
	"strings"
)

// ObjectType names a Monkey runtime value kind.
type ObjectType string

const (
	INTEGER_OBJ  ObjectType = "INTEGER"
	BOOLEAN_OBJ  ObjectType = "BOOLEAN"
	NULL_OBJ     ObjectType = "NULL"
	RETURN_OBJ   ObjectType = "RETURN_VALUE"
	ERROR_OBJ    ObjectType = "ERROR"
	FUNCTION_OBJ ObjectType = "FUNCTION"
)

// Object is the interface every runtime value implements.
type Object interface {
	Type() ObjectType
	Inspect() string
}

// Integer holds a 64-bit integer value.
type Integer struct{ Value int64 }

func (i *Integer) Type() ObjectType { return INTEGER_OBJ }
func (i *Integer) Inspect() string  { return fmt.Sprintf("%d", i.Value) }

// Boolean holds a true/false value.
type Boolean struct{ Value bool }

func (b *Boolean) Type() ObjectType { return BOOLEAN_OBJ }
func (b *Boolean) Inspect() string  { return fmt.Sprintf("%t", b.Value) }

// Null represents the absence of a meaningful value.
type Null struct{}

func (n *Null) Type() ObjectType { return NULL_OBJ }
func (n *Null) Inspect() string  { return "null" }

// ReturnValue wraps a value being returned from a function so that it
// propagates up the evaluation stack without being treated as a normal value.
type ReturnValue struct{ Value Object }

func (rv *ReturnValue) Type() ObjectType { return RETURN_OBJ }
func (rv *ReturnValue) Inspect() string  { return rv.Value.Inspect() }

// Error carries a runtime error message.
type Error struct{ Message string }

func (e *Error) Type() ObjectType { return ERROR_OBJ }
func (e *Error) Inspect() string  { return "ERROR: " + e.Message }

// Parameter is a named function parameter.
type Parameter struct{ Name string }

// Function captures its parameters, a body, and the defining environment.
// Env is stored as a pointer, not a copy: all code that shares this
// *Environment sees the same mutations made after the closure was created.
// In a real interpreter the Body field is *ast.BlockStatement; string is
// used here so this package compiles standalone without an ast dependency.
type Function struct {
	Parameters []*Parameter
	Body       string
	Env        *Environment
}

func (f *Function) Type() ObjectType { return FUNCTION_OBJ }
func (f *Function) Inspect() string {
	params := make([]string, len(f.Parameters))
	for i, p := range f.Parameters {
		params[i] = p.Name
	}
	return fmt.Sprintf("fn(%s) { %s }", strings.Join(params, ", "), f.Body)
}
```

### The environment chain

An `Environment` is a node with a `store` map and an `outer` pointer; the global scope has a nil `outer`, every nested scope points at its parent. The three mutating methods have deliberately different reach. `Get` reads and walks the chain, so the nearest binding wins (lexical shadowing). `Set` writes to the current scope only and never touches an outer scope, which is why a `Set` in an inner scope shadows rather than mutates. `Update` walks the chain like `Get` but writes into the scope that owns the name, which is the primitive this dialect's assignment operator `=` evaluates to. Keeping `Set` and `Update` distinct is the whole reason mutable closures are possible in the next exercise.

Create `environment.go`:

```go
package closures

import "fmt"

// Environment is a linked chain of variable scopes. The outer pointer is
// nil for the global scope and non-nil for every enclosed scope created
// by a function call or a block statement.
type Environment struct {
	store map[string]Object
	outer *Environment
}

// NewEnvironment creates a top-level (global) environment.
func NewEnvironment() *Environment {
	return &Environment{store: make(map[string]Object)}
}

// NewEnclosedEnvironment creates a new scope that extends outer.
// Function calls use this so the callee has its own local store while
// still resolving free variables through the outer chain.
func NewEnclosedEnvironment(outer *Environment) *Environment {
	env := NewEnvironment()
	env.outer = outer
	return env
}

// Get looks up name, walking the chain toward the global scope.
// The first scope that contains name wins (lexical shadowing).
func (e *Environment) Get(name string) (Object, bool) {
	obj, ok := e.store[name]
	if !ok && e.outer != nil {
		return e.outer.Get(name)
	}
	return obj, ok
}

// Set creates or shadows name in the current scope only.
// It does not modify any outer scope, even if name exists there.
func (e *Environment) Set(name string, val Object) Object {
	e.store[name] = val
	return val
}

// Update walks the chain and mutates the scope that first declared name.
// This implements assignment to a captured (outer-scope) variable, which is
// necessary for mutable closures like the counter pattern.
// It returns an error if name is not found anywhere in the chain.
func (e *Environment) Update(name string, val Object) error {
	if _, ok := e.store[name]; ok {
		e.store[name] = val
		return nil
	}
	if e.outer != nil {
		return e.outer.Update(name, val)
	}
	return fmt.Errorf("closures: assignment to undefined variable %q", name)
}
```

### Extending the function's environment at call time

The single most important rule of the call path: extend `f.Env`, the function's defining environment, never the caller's. Parameters bind in a fresh enclosed scope; free variables in the body resolve through that scope's `outer` chain to the captured environment. Extending the caller's environment instead would let the callee see the caller's locals and would silently break lexical scoping. `unwrapReturnValue` is the companion helper that consumes the `*ReturnValue` signal at the function boundary so the caller receives the inner value, not the wrapper.

Create `evaluator.go`:

```go
package closures

import "fmt"

// extendFunctionEnv creates the execution environment for a function call.
// It extends the function's DEFINING environment (f.Env), not the caller's
// environment. This enforces lexical scoping: the callee sees names from
// where the function was defined, never names from the call site.
func extendFunctionEnv(f *Function, args []Object) (*Environment, error) {
	if len(args) != len(f.Parameters) {
		return nil, fmt.Errorf(
			"closures: wrong number of arguments: want %d, got %d",
			len(f.Parameters), len(args),
		)
	}
	env := NewEnclosedEnvironment(f.Env) // critical: f.Env, not callerEnv
	for i, param := range f.Parameters {
		env.Set(param.Name, args[i])
	}
	return env, nil
}

// unwrapReturnValue extracts the value from a ReturnValue wrapper.
// A ReturnValue propagates through block evaluation (loops, if-else) but
// must be unwrapped at the function boundary so the caller receives the
// inner value, not the signal object.
func unwrapReturnValue(obj Object) Object {
	if rv, ok := obj.(*ReturnValue); ok {
		return rv.Value
	}
	return obj
}
```

### The demo

The demo uses only the exported environment API to show two things the evaluator does for real: resolving a free variable through the chain at call time, and producing independent closures from a factory. There is no counter here — mutation is the next exercise — only the read side of closures.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/closures"
)

func main() {
	// A function value captures its defining environment by pointer. Calling it
	// creates a new scope enclosed in that captured environment: the parameter
	// binds locally; the free variable resolves up the chain.
	// Simulate calling fn(x) { x + base } with x = 3 and a captured base = 100.
	global := closures.NewEnvironment()
	global.Set("base", &closures.Integer{Value: 100})

	callEnv := closures.NewEnclosedEnvironment(global)
	callEnv.Set("x", &closures.Integer{Value: 3})
	x, _ := callEnv.Get("x")       // local parameter
	base, _ := callEnv.Get("base") // free variable, resolved through the chain
	fmt.Printf("param x=%s, captured base=%s\n", x.Inspect(), base.Inspect())

	// Function factory: let makeAdder = fn(n) { fn(x) { n + x } }.
	// Each call to makeAdder captures an independent environment.
	makeAdder := func(n int64) *closures.Function {
		env := closures.NewEnclosedEnvironment(global)
		env.Set("n", &closures.Integer{Value: n})
		return &closures.Function{
			Parameters: []*closures.Parameter{{Name: "x"}},
			Body:       "n + x",
			Env:        env,
		}
	}
	add5 := makeAdder(5)
	add10 := makeAdder(10)
	n5, _ := add5.Env.Get("n")
	n10, _ := add10.Env.Get("n")
	fmt.Printf("add5 captures n=%s, add10 captures n=%s\n", n5.Inspect(), n10.Inspect())

	// Mutating one factory's captured env must not affect the other.
	add5.Env.Set("n", &closures.Integer{Value: 99})
	n10again, _ := add10.Env.Get("n")
	fmt.Printf("after mutating add5, add10 still n=%s\n", n10again.Inspect())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
param x=3, captured base=100
add5 captures n=5, add10 captures n=10
after mutating add5, add10 still n=10
```

### Tests

The tests prove the closure invariants at the environment layer. Capture by pointer is asserted by mutating a variable after a `Function` is built and checking the function sees the new value. Factory independence is asserted by mutating one closure's captured scope and checking the other is untouched. The call-path tests confirm `extendFunctionEnv` binds parameters, resolves free variables through the chain, and rejects arity mismatches.

Create `evaluator_test.go`:

```go
package closures

import (
	"fmt"
	"testing"
)

func TestEnvironmentGetSet(t *testing.T) {
	t.Parallel()

	env := NewEnvironment()
	env.Set("x", &Integer{Value: 42})

	val, ok := env.Get("x")
	if !ok {
		t.Fatal("Get: x not found")
	}
	if val.(*Integer).Value != 42 {
		t.Fatalf("Get: got %s, want 42", val.Inspect())
	}
}

func TestEnclosedScopeShadowsOuter(t *testing.T) {
	t.Parallel()

	outer := NewEnvironment()
	outer.Set("x", &Integer{Value: 10})

	inner := NewEnclosedEnvironment(outer)
	inner.Set("x", &Integer{Value: 20}) // shadow, not mutation of outer

	got, _ := inner.Get("x")
	if got.(*Integer).Value != 20 {
		t.Fatalf("inner x: got %s, want 20", got.Inspect())
	}
	got, _ = outer.Get("x")
	if got.(*Integer).Value != 10 {
		t.Fatalf("outer x after inner Set: got %s, want 10 (Set must not mutate outer)", got.Inspect())
	}
}

func TestGetResolvesFreeThroughChain(t *testing.T) {
	t.Parallel()

	global := NewEnvironment()
	global.Set("n", &Integer{Value: 7})

	callEnv := NewEnclosedEnvironment(global)
	// n is not in callEnv; Get must walk to global.
	got, ok := callEnv.Get("n")
	if !ok {
		t.Fatal("Get: n not found through chain")
	}
	if got.(*Integer).Value != 7 {
		t.Fatalf("Get through chain: got %s, want 7", got.Inspect())
	}
}

func TestUpdateMutatesOwnerScope(t *testing.T) {
	t.Parallel()

	outer := NewEnvironment()
	outer.Set("count", &Integer{Value: 0})

	inner := NewEnclosedEnvironment(outer)
	if err := inner.Update("count", &Integer{Value: 1}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, _ := outer.Get("count")
	if got.(*Integer).Value != 1 {
		t.Fatalf("count after Update from inner: got %s, want 1", got.Inspect())
	}
}

func TestUpdateRejectsUndefined(t *testing.T) {
	t.Parallel()

	env := NewEnvironment()
	if err := env.Update("missing", &Integer{Value: 0}); err == nil {
		t.Fatal("Update to undefined: expected error, got nil")
	}
}

func TestFunctionCapturesEnvironmentByPointer(t *testing.T) {
	t.Parallel()

	// Simulate: let x = 10; let f = fn() { x }; x = 99
	// f must see x = 99 because the environment is captured by pointer.
	env := NewEnvironment()
	env.Set("x", &Integer{Value: 10})

	f := &Function{Parameters: []*Parameter{}, Body: "x", Env: env}

	// Mutate x after f was created.
	env.Set("x", &Integer{Value: 99})

	got, ok := f.Env.Get("x")
	if !ok {
		t.Fatal("closure env: x not found")
	}
	if got.(*Integer).Value != 99 {
		t.Fatalf("closure sees stale value: got %s, want 99 (must capture by pointer)", got.Inspect())
	}
}

func TestFunctionFactoriesAreIndependent(t *testing.T) {
	t.Parallel()

	// Simulate: let makeAdder = fn(n) { fn(x) { n + x } }
	// makeAdder(5) and makeAdder(10) must produce closures with
	// independent environments; mutating one must not affect the other.
	makeAdder := func(n int64) *Function {
		env := NewEnvironment()
		env.Set("n", &Integer{Value: n})
		return &Function{
			Parameters: []*Parameter{{Name: "x"}},
			Body:       "n + x",
			Env:        env,
		}
	}

	add5 := makeAdder(5)
	add10 := makeAdder(10)

	v5, _ := add5.Env.Get("n")
	v10, _ := add10.Env.Get("n")
	if v5.(*Integer).Value != 5 {
		t.Fatalf("add5 n = %s, want 5", v5.Inspect())
	}
	if v10.(*Integer).Value != 10 {
		t.Fatalf("add10 n = %s, want 10", v10.Inspect())
	}

	// Mutating add5's captured env must not affect add10's.
	add5.Env.Set("n", &Integer{Value: 99})
	v10again, _ := add10.Env.Get("n")
	if v10again.(*Integer).Value != 10 {
		t.Fatalf("add10 n after add5 mutation: got %s, want 10", v10again.Inspect())
	}
}

func TestExtendFunctionEnvBindsParametersAndResolvesCaptures(t *testing.T) {
	t.Parallel()

	global := NewEnvironment()
	global.Set("captured", &Integer{Value: 77})

	f := &Function{
		Parameters: []*Parameter{{Name: "a"}, {Name: "b"}},
		Body:       "a + b + captured",
		Env:        global,
	}

	callEnv, err := extendFunctionEnv(f, []Object{
		&Integer{Value: 3},
		&Integer{Value: 4},
	})
	if err != nil {
		t.Fatalf("extendFunctionEnv: %v", err)
	}

	a, _ := callEnv.Get("a")
	b, _ := callEnv.Get("b")
	captured, _ := callEnv.Get("captured") // free variable resolved through chain

	if a.(*Integer).Value != 3 {
		t.Fatalf("a = %s, want 3", a.Inspect())
	}
	if b.(*Integer).Value != 4 {
		t.Fatalf("b = %s, want 4", b.Inspect())
	}
	if captured.(*Integer).Value != 77 {
		t.Fatalf("captured = %s, want 77", captured.Inspect())
	}
}

func TestExtendFunctionEnvArityMismatch(t *testing.T) {
	t.Parallel()

	f := &Function{
		Parameters: []*Parameter{{Name: "x"}},
		Body:       "x",
		Env:        NewEnvironment(),
	}

	if _, err := extendFunctionEnv(f, []Object{}); err == nil {
		t.Fatal("extendFunctionEnv: expected error for too few args, got nil")
	}
	if _, err := extendFunctionEnv(f, []Object{&Integer{Value: 1}, &Integer{Value: 2}}); err == nil {
		t.Fatal("extendFunctionEnv: expected error for too many args, got nil")
	}
}

func TestUnwrapReturnValue(t *testing.T) {
	t.Parallel()

	inner := &Integer{Value: 42}
	wrapped := &ReturnValue{Value: inner}

	if got := unwrapReturnValue(wrapped); got != inner {
		t.Fatalf("unwrapReturnValue(ReturnValue): got %v, want inner Integer", got)
	}
	if got := unwrapReturnValue(inner); got != inner {
		t.Fatalf("unwrapReturnValue(non-ReturnValue): got %v, want same object", got)
	}
}

func ExampleNewEnclosedEnvironment() {
	outer := NewEnvironment()
	outer.Set("x", &Integer{Value: 10})

	inner := NewEnclosedEnvironment(outer)
	val, _ := inner.Get("x") // x is not in inner; walks to outer
	fmt.Println(val.Inspect())
	// Output: 10
}
```

## Review

The module is correct when every closure invariant holds at the environment layer. Capture by pointer means a `Function` built before a mutation still observes the new value through `f.Env`, and two factory-produced closures hold independent scopes so mutating one leaves the other unchanged. Lexical shadowing means a `Set` in an inner scope creates a local binding without disturbing the outer one, while `Get` still resolves a free variable up the chain. The call path is correct when `extendFunctionEnv` extends `f.Env` (not the caller), binds every parameter, resolves captured names through the chain, and rejects an argument count that does not match the parameter list. Confirm `unwrapReturnValue` returns the inner value for a wrapper and the object itself otherwise, and that `go test -race ./...` stays clean.

The dangerous mistake to watch for is extending the caller's environment instead of `f.Env`: it passes for functions defined and called in one scope and fails only across scopes, which is exactly where closures are supposed to work.

## Resources

- [Writing An Interpreter In Go (Thorsten Ball)](https://interpreterbook.com/) — the closures chapter builds this exact `Function`-plus-`Environment` design.
- [Go Tour: Function closures](https://go.dev/tour/moretypes/25) — Go's own closures capture free variables by reference, which is what `Function.Env` mirrors.
- [Go Specification: Function literals](https://go.dev/ref/spec#Function_literals) — the language rule for capturing variables in a function literal.
- [Crafting Interpreters: Closures (Nystrom)](https://craftinginterpreters.com/closures.html) — the environment-chain model from a second interpreter.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-mutable-closures-the-counter-pattern.md](02-mutable-closures-the-counter-pattern.md)
