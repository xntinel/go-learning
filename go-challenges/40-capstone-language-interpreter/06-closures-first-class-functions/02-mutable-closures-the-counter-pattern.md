# Exercise 2: Mutable Closures and the Counter Pattern

Canonical Monkey has only immutable `let` bindings, so its environment never needs to reassign a name. This curriculum's dialect adds an assignment operator `=`, and that one addition is what makes the counter pattern — a closure that increments a variable living in an outer scope — possible. The whole feature rests on a single distinction at the environment layer: assignment must compile to `Update`, which walks the chain and mutates the scope that owns the name, not to `Set`, which always writes locally and would silently shadow the outer binding. This exercise builds a self-contained environment with both methods and a `Counter` that proves the difference, then pins the contract with a test that fails if `Set` ever leaks into the assignment path.

This module is fully self-contained. It depends on nothing but the standard library, ships its own demo and tests, and imports no other exercise.

## What you'll build

```text
object.go            Object, Integer
environment.go       Environment, NewEnvironment, NewEnclosedEnvironment, Get, Set, Update
counter.go           Counter, NewCounter, Inc
counter_test.go      counter advances; Update mutates the owner; Set does not
cmd/
  demo/
    main.go          run the counter and print 1, 2, 3
```

- Files: `object.go`, `environment.go`, `counter.go`, `counter_test.go`, `cmd/demo/main.go`.
- Implement: `Environment` with `Get`/`Set`/`Update`, and a `Counter` whose `Inc` increments a captured variable through the chain with `Update`.
- Test: the counter advances across calls, `Update` mutates the declaring scope, `Update` on an unknown name errors, and `Set` in an inner scope does not mutate the outer binding.
- Verify: `go test -race ./...` then `go run ./cmd/demo`.

### Why Update, not Set

Picture the Monkey program the counter models:

```text
let count = 0
let inc = fn() { count = count + 1; count }
```

`count` is declared in an outer scope; `inc` runs in a fresh scope enclosed in that outer one. When `inc` executes `count = count + 1`, it must reach back and change the same `count` the outer scope owns. If assignment wrote with `Set` on the call's environment, it would create a brand-new `count` local to the call: the inner scope had no `count` before, so `Set` adds one there, shadowing the outer binding. Every call would then read the outer zero, compute one, and store a fresh local one, leaving the outer `count` permanently at zero and the counter stuck at one.

`Update` fixes this by walking the chain exactly like `Get` and writing into the first scope that already holds the name. The first `Inc` finds `count` in the outer scope and sets it to one; the second `Inc` finds the same, now-incremented binding and sets it to two. The mutable closure is therefore not a property of the function object at all — it is the consequence of choosing `Update` over `Set` when an assignment touches a captured name.

### The object system

Only one value type is needed here.

Create `object.go`:

```go
package counter

import "fmt"

// Object is the interface every runtime value implements.
type Object interface {
	Inspect() string
}

// Integer holds a 64-bit integer value.
type Integer struct{ Value int64 }

func (i *Integer) Inspect() string { return fmt.Sprintf("%d", i.Value) }
```

### The environment with Set and Update

Create `environment.go`:

```go
package counter

import "fmt"

// Environment is a linked chain of variable scopes. outer is nil for the
// global scope and non-nil for every enclosed scope.
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

// Set creates or shadows name in the current scope only. It never modifies
// an outer scope, even if name already exists there.
func (e *Environment) Set(name string, val Object) Object {
	e.store[name] = val
	return val
}

// Update walks the chain and mutates the scope that first declared name.
// This is the primitive the assignment operator "=" evaluates to, and the
// reason a closure can change a captured variable. It returns an error if
// name is not declared anywhere in the chain.
func (e *Environment) Update(name string, val Object) error {
	if _, ok := e.store[name]; ok {
		e.store[name] = val
		return nil
	}
	if e.outer != nil {
		return e.outer.Update(name, val)
	}
	return fmt.Errorf("counter: assignment to undefined variable %q", name)
}
```

### The counter

`Counter` models the `let count = 0; let inc = fn() {...}` program: `count` is bound in an outer scope and `inc`'s body runs in an enclosed scope. `Inc` reads `count` through the chain, computes the next value, and writes it back with `Update` so the outer binding actually moves.

Create `counter.go`:

```go
package counter

// Counter models the Monkey program:
//
//	let count = 0
//	let inc = fn() { count = count + 1; count }
//
// The "count" binding lives in the outer scope; inc's body runs in an
// enclosed scope. Inc reaches the outer "count" through the chain and mutates
// it with Update, never shadowing it with Set.
type Counter struct {
	outer *Environment
	inner *Environment
	name  string
}

// NewCounter binds "count" to start in an outer scope and prepares the
// enclosed scope that inc's body would run in.
func NewCounter(start int64) *Counter {
	outer := NewEnvironment()
	outer.Set("count", &Integer{Value: start})
	return &Counter{
		outer: outer,
		inner: NewEnclosedEnvironment(outer),
		name:  "count",
	}
}

// Inc evaluates "count = count + 1" from the enclosed scope and returns the
// new value. Update mutates the outer binding; the next call sees it.
func (c *Counter) Inc() int64 {
	cur, _ := c.inner.Get(c.name) // resolves through the chain to outer
	next := cur.(*Integer).Value + 1
	_ = c.inner.Update(c.name, &Integer{Value: next}) // mutates outer's count
	got, _ := c.inner.Get(c.name)
	return got.(*Integer).Value
}
```

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/counter"
)

func main() {
	c := counter.NewCounter(0)
	fmt.Println(c.Inc()) // 1
	fmt.Println(c.Inc()) // 2
	fmt.Println(c.Inc()) // 3
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
1
2
3
```

### Tests

The first test proves the counter advances: three `Inc` calls return 1, 2, 3, which can only happen if `Update` mutated the shared outer binding. `TestUpdateMutatesOwnerScope` isolates that mutation, and `TestUpdateRejectsUndefined` checks the error path. `TestSetDoesNotMutateOuter` is the contract that mutable closures depend on: a `Set` from the inner scope must leave the outer binding untouched, so that the only way to move an outer variable is `Update`.

Create `counter_test.go`:

```go
package counter

import "testing"

func TestCounterAdvances(t *testing.T) {
	t.Parallel()

	c := NewCounter(0)
	for i, want := range []int64{1, 2, 3} {
		if got := c.Inc(); got != want {
			t.Fatalf("Inc call %d: got %d, want %d (Update must mutate the outer binding)", i+1, got, want)
		}
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
		t.Fatalf("outer count after inner Update: got %s, want 1", got.Inspect())
	}
}

func TestUpdateRejectsUndefined(t *testing.T) {
	t.Parallel()

	env := NewEnvironment()
	if err := env.Update("missing", &Integer{Value: 0}); err == nil {
		t.Fatal("Update to undefined: expected error, got nil")
	}
}

func TestSetDoesNotMutateOuter(t *testing.T) {
	t.Parallel()

	// Pins the Set vs Update contract: Set in an inner scope must shadow,
	// never mutate the outer binding. If this fails, the counter is relying
	// on accidental behavior.
	outer := NewEnvironment()
	outer.Set("count", &Integer{Value: 0})
	inner := NewEnclosedEnvironment(outer)

	inner.Set("count", &Integer{Value: 99}) // shadow, not mutation

	got, _ := outer.Get("count")
	if got.(*Integer).Value != 0 {
		t.Fatalf("outer count after inner Set: got %s, want 0 (Set must not mutate outer)", got.Inspect())
	}
	shadow, _ := inner.Get("count")
	if shadow.(*Integer).Value != 99 {
		t.Fatalf("inner count after Set: got %s, want 99 (shadow visible locally)", shadow.Inspect())
	}
}
```

## Review

The counter is correct when three calls to `Inc` return 1, 2, 3, which is only possible if `Update` reached the outer `count` and changed it in place. The two environment tests pin the mechanism directly: `Update` from an inner scope mutates the outer owner, and `Update` on a name that exists nowhere returns an error rather than silently creating a binding. The contract test is the one that matters most — a `Set` in the inner scope must shadow and leave the outer binding at its original value, because the moment assignment is allowed to fall back to `Set` the counter stops advancing. Confirm `go test -race ./...` is clean.

The trap is wiring assignment to `Set`: the counter then resets to one on every call because each `inc` writes a fresh local `count` instead of mutating the captured one.

## Resources

- [Writing An Interpreter In Go (Thorsten Ball)](https://interpreterbook.com/) — the book's `Environment` has only `Get`/`Set`; this dialect adds `Update` for assignment.
- [Go Tour: Function closures](https://go.dev/tour/moretypes/25) — the Fibonacci closure mutates captured state across calls, the same pattern as the counter.
- [Go Specification: Assignments](https://go.dev/ref/spec#Assignments) — the semantics of assigning to an existing variable, which `Update` models.

---

Back to [01-environment-chain-and-first-class-functions.md](01-environment-chain-and-first-class-functions.md) | Next: [03-recursive-closures-forward-reference.md](03-recursive-closures-forward-reference.md)
