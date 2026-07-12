# Exercise 4: Closures in Loops and Per-Iteration Scope

Closures created inside a loop expose the sharpest consequence of capture-by-pointer. If every iteration binds the loop variable into one shared environment, every closure captures that same environment and, after the loop ends, they all read the variable's final value — the historical JavaScript `var` bug and the pre-1.22 Go loop-variable surprise. The fix is not to copy the value but to give each closure its own scope to point at: create a `NewEnclosedEnvironment` per iteration and bind the loop variable inside it. This exercise builds both versions side by side in a self-contained module, proves the per-iteration version captures distinct values and the shared version captures the final one, and makes the difference a runnable, testable fact rather than a warning.

This module is fully self-contained. It depends on nothing but the standard library, ships its own demo and tests, and imports no other exercise.

## What you'll build

```text
object.go            Object, Integer, Parameter, Function
environment.go       Environment, NewEnvironment, NewEnclosedEnvironment, Get, Set
loopscope.go         MakeClosuresPerIteration, MakeClosuresShared, Captured
loopscope_test.go    per-iteration captures distinct i; shared captures the final i
cmd/
  demo/
    main.go          build both closure sets and print what each captured
```

- Files: `object.go`, `environment.go`, `loopscope.go`, `loopscope_test.go`, `cmd/demo/main.go`.
- Implement: two loop builders, one giving each closure a fresh enclosed scope and one reusing a single scope, plus a `Captured` helper.
- Test: the per-iteration closures capture 0..n-1, the shared closures all capture n-1.
- Verify: `go test -race ./...` then `go run ./cmd/demo`.

### Why one scope per iteration

A closure captures its environment by pointer, so the value it later reports is whatever the captured environment holds at lookup time, not at capture time. When a loop reuses one environment and rebinds `i` on each pass with `Set`, all closures hold a pointer to that single environment. After the loop, the environment's `i` is the last value assigned, so every closure reports it — five closures over a 0..4 loop all report 4. Nothing was copied per closure, so nothing distinguishes them.

Creating a `NewEnclosedEnvironment` at the top of each iteration and binding `i` inside it gives each closure a private scope. Closure number two captures the scope where `i` is 2; closure number four captures the scope where `i` is 4. The cure is structural — distinct scopes — not a value copy, which is the same realization Go landed on in 1.22 when it changed loop variables to be per-iteration, and the behavior JavaScript's `let` already had. Monkey uses per-iteration scopes, so its closures-in-loops behave like Go 1.22 and JavaScript `let`, not like the older shared-variable forms.

### The object system

Create `object.go`:

```go
package loopscope

import "fmt"

// Object is the interface every runtime value implements.
type Object interface {
	Inspect() string
}

// Integer holds a 64-bit integer value.
type Integer struct{ Value int64 }

func (i *Integer) Inspect() string { return fmt.Sprintf("%d", i.Value) }

// Parameter is a named function parameter.
type Parameter struct{ Name string }

// Function captures its parameters, a body, and the defining environment by
// pointer. Two closures that share one *Environment report the same captured
// values; two with distinct environments report independently.
type Function struct {
	Parameters []*Parameter
	Body       string
	Env        *Environment
}
```

### The environment

Create `environment.go`:

```go
package loopscope

// Environment is a linked chain of variable scopes.
type Environment struct {
	store map[string]Object
	outer *Environment
}

// NewEnvironment creates a top-level (global) environment.
func NewEnvironment() *Environment {
	return &Environment{store: make(map[string]Object)}
}

// NewEnclosedEnvironment creates a new scope that extends outer. Calling this
// once per loop iteration is what gives each closure a private scope.
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

### The two loop builders

`MakeClosuresPerIteration` creates a fresh enclosed scope each pass, so each closure captures a distinct `i`. `MakeClosuresShared` reuses one scope and rebinds `i` each pass, so every closure captures the same scope and reports the final value. `Captured` reads back what a closure captured.

Create `loopscope.go`:

```go
package loopscope

// MakeClosuresPerIteration builds n closures, each capturing its own enclosed
// scope that binds "i" to that iteration's value. Each closure reports a
// distinct i. This is the correct behavior.
func MakeClosuresPerIteration(global *Environment, n int) []*Function {
	closures := make([]*Function, n)
	for i := 0; i < n; i++ {
		iter := NewEnclosedEnvironment(global) // new scope per iteration
		iter.Set("i", &Integer{Value: int64(i)})
		closures[i] = &Function{Parameters: []*Parameter{}, Body: "i", Env: iter}
	}
	return closures
}

// MakeClosuresShared reuses ONE enclosed scope for every iteration, rebinding
// "i" each pass. All closures capture the same scope, so after the loop they
// all report the final value. This reproduces the classic loop-capture bug.
func MakeClosuresShared(global *Environment, n int) []*Function {
	closures := make([]*Function, n)
	shared := NewEnclosedEnvironment(global)
	for i := 0; i < n; i++ {
		shared.Set("i", &Integer{Value: int64(i)})
		closures[i] = &Function{Parameters: []*Parameter{}, Body: "i", Env: shared}
	}
	return closures
}

// Captured returns the value of "i" that the closure captured.
func Captured(f *Function) int64 {
	v, _ := f.Env.Get("i")
	return v.(*Integer).Value
}
```

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/loopscope"
)

func main() {
	global := loopscope.NewEnvironment()

	correct := loopscope.MakeClosuresPerIteration(global, 5)
	shared := loopscope.MakeClosuresShared(global, 5)

	fmt.Print("per-iteration captures:")
	for _, f := range correct {
		fmt.Printf(" %d", loopscope.Captured(f))
	}
	fmt.Println()

	fmt.Print("shared captures:")
	for _, f := range shared {
		fmt.Printf(" %d", loopscope.Captured(f))
	}
	fmt.Println()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
per-iteration captures: 0 1 2 3 4
shared captures: 4 4 4 4 4
```

### Tests

The first test asserts the correct behavior: closure `i` captured the value `i`. The second pins the bug so the contrast is explicit and cannot silently drift — every shared closure must report `n-1`, which is the symptom you are learning to avoid.

Create `loopscope_test.go`:

```go
package loopscope

import "testing"

func TestPerIterationScopeCapturesDistinct(t *testing.T) {
	t.Parallel()

	global := NewEnvironment()
	closures := MakeClosuresPerIteration(global, 5)

	for idx, f := range closures {
		if got := Captured(f); got != int64(idx) {
			t.Fatalf("closures[%d] captured %d, want %d (per-iteration scope required)", idx, got, idx)
		}
	}
}

func TestSharedEnvironmentCapturesFinal(t *testing.T) {
	t.Parallel()

	global := NewEnvironment()
	const n = 5
	closures := MakeClosuresShared(global, n)

	// All closures share one scope, so each reports the loop's final value.
	for idx, f := range closures {
		if got := Captured(f); got != int64(n-1) {
			t.Fatalf("closures[%d] captured %d, want %d (shared scope captures the final value)", idx, got, n-1)
		}
	}
}
```

## Review

The behavior is correct when the per-iteration builder produces closures that report 0..n-1 and the shared builder produces closures that all report n-1. The contrast is the lesson: identical-looking loops differ only in whether they allocate a scope per iteration, and that single structural choice decides whether the captured values are distinct or collapsed. Confirm both tests pass under `go test -race ./...`, and read the shared case as the failure mode to recognize in real evaluator code, not as something to ship.

The trap is reaching for a value copy of the loop variable to fix the bug. The real fix is a fresh scope per iteration; capture-by-pointer then makes each closure point at its own value automatically.

## Resources

- [Go 1.22 loop variable change](https://go.dev/blog/loopvar-preview) — Go made loop variables per-iteration for exactly this capture problem; Monkey's per-iteration scope matches it.
- [Go FAQ: closures and goroutines](https://go.dev/doc/faq#closures_and_goroutines) — the canonical statement of the loop-capture surprise.
- [Writing An Interpreter In Go (Thorsten Ball)](https://interpreterbook.com/) — the environment model whose per-iteration scoping produces this behavior.

---

Back to [03-recursive-closures-forward-reference.md](03-recursive-closures-forward-reference.md) | Next: [../07-repl-line-editing/00-concepts.md](../07-repl-line-editing/00-concepts.md)
