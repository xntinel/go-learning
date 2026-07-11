# 6. Closures and First-Class Functions — Concepts

Closures are the mechanism that turns the Monkey interpreter into a real language rather than a calculator. A closure is a function paired with the lexical environment in which it was defined; when the function later runs, it sees the names from its defining scope no matter where the call happens. The concept is short to state and easy to get subtly wrong to implement, because three properties have to hold at once: the defining environment is captured by pointer (not copied), name resolution walks an environment chain, and assignment can mutate a variable that lives in an outer scope. Those three are exactly what separates an interpreter that appears to support closures from one that actually does. This file is the conceptual foundation for the lesson; the exercises build each property in isolation as self-contained Go modules, so you can prove an invariant holds at the environment layer without needing a full parser or evaluator wired up.

## Concepts

### Go Function Values Are Already Closures

A Go function literal `func(x int) int { return x + n }` closes over `n` from the enclosing scope. The compiler's escape analysis notices that `n` outlives the declaring frame and heap-allocates it, so the returned function keeps a live reference to the same `n`, not a snapshot of its value. Capture is by reference: if the outer code mutates `n` after the literal is created, the function sees the new value. This is the single most important fact to internalize, because the interpreter you build mirrors it exactly.

A Monkey `Function` object stores a pointer to the `*Environment` that was alive when the function literal was evaluated, never a copy of that environment's contents. The consequence is identical to Go's: a mutation made to the captured environment after the function object exists is visible through the function's stored pointer. Capturing by snapshot would mean the closure freezes the values it saw at definition time and never observes later changes — wrong for both recursive functions (which reference a name bound after the literal) and for mutable counters (which depend on seeing the latest value). Capturing by pointer is what makes both work, and it is free in Go because a struct field of type `*Environment` is already a reference.

### The Environment Chain

Every scope in the interpreter is an `*Environment` node holding a `store` map and an `outer` pointer. The global scope has a nil `outer`; every nested scope — created at a function call or a block — has `outer` pointing at its parent. A new scope is made by `NewEnclosedEnvironment(outer)`, which is the moment the chain link is fixed. Because the link is set once, at creation, and never rewritten, the language has lexical (static) scoping: a name resolves through the chain that existed where the function was written, not the chain that exists where it is called.

```text
global: { x: 1, adder: <Function> }
                ^
call:   { y: 5 }   outer -> global
```

Three methods give the chain its behavior, and their differences are the heart of the lesson. `Get` reads: it looks in the current node and, on a miss, walks toward global, so the first scope that holds the name wins (this is what lexical shadowing means). `Set` writes only to the current node; it never touches an outer scope even if the name already exists there, so `Set` in an inner scope creates a shadow that hides the outer binding. `Update` walks the chain like `Get` but writes: it finds the scope that actually declared the name and mutates that scope in place, returning an error if the name exists nowhere. The distinction between `Set` (always local, shadows) and `Update` (finds the owner, mutates) is the line between a closure that captures a value and a closure that can change one.

### Captured Environment vs. Enclosed Environment

Two distinct environments take part in every call, and conflating them is the classic closure bug. The captured environment is `f.Env`: the environment alive when the function literal was evaluated. It is what makes the function a closure, and it is fixed for the life of the function object. The enclosed environment is a fresh scope created at call time, whose `outer` pointer is set to `f.Env`; parameters are bound into it and the body runs in it. Every call produces a new enclosed environment, so two concurrent calls to the same function have independent parameter bindings while sharing the same captured outer scope.

The rule that the call-time helper must follow is: extend `f.Env`, not the caller's environment. Binding parameters into a scope whose outer is the caller would let the callee see the caller's locals and would break lexical scoping outright — a function defined at global scope but called from inside another function would suddenly resolve free names against the caller's frame. The fix is one line (`NewEnclosedEnvironment(f.Env)`), and getting it wrong is invisible for simple tests and catastrophic for any program that defines a function in one scope and calls it from another.

### Mutable Closures Require Update, Not Set

Canonical Monkey, from Thorsten Ball's book, has only immutable `let` bindings: there is no way to reassign a name, so the environment never needs an `Update` operation. This curriculum's dialect extends the language with an assignment operator `=`, parsed as a right-associative infix in the Pratt-parser lesson. Assignment is exactly the construct that needs to reach across the chain and mutate an outer binding, and `Update` is the environment-layer primitive it evaluates to.

The counter pattern is the canonical motivation. Given `let count = 0` in an outer scope and an inner function `fn() { count = count + 1; count }`, each call must increment the same `count` that lives outside. If the assignment compiled to `Set` on the call's environment, it would create a brand-new `count` local to that call, leaving the outer one at zero; every invocation would read the outer zero, write a fresh local one, and the counter would never advance past one. Compiling assignment to `Update` instead walks the chain, finds the `count` declared in the outer scope, and mutates it in place, so the second call sees the value the first one wrote. Mutable closures are therefore not a feature of the function object at all; they are a property of choosing `Update` over `Set` when assignment touches a captured name.

### Recursive Closures and the Forward-Reference Pattern

In `let fact = fn(n) { if (n < 2) { 1 } else { n * fact(n - 1) } }` the function body names `fact`, which is the very binding being defined. The reason this works in a pointer-based environment is that `f.Env` and the environment the evaluator calls `Set("fact", f)` on are the same `*Environment`: after that `Set`, `f.Env.Get("fact")` returns `f` whether the function object was created before or after the `Set`, because both operations touch one live map. There is no "too late" — the body looks up `fact` only when it runs, long after the binding is in place.

A two-step forward-reference convention is still worth knowing: bind the name to a placeholder (such as `Null`) first, create the function so it captures the environment, then update the binding to point at the function. In this pointer-based design that placeholder step is optional, because the lookup is deferred and the map is shared. It becomes mandatory only in environment designs that copy values at creation time, where the function would otherwise capture a snapshot that does not yet contain the name. Showing the pattern documents intent and keeps the code portable to those designs.

### Closures in Loops: Per-Iteration Scope

If a loop reuses one environment across iterations and binds the loop variable into it each time, every closure created in the loop captures that same environment and, after the loop ends, they all observe the variable's final value. That is the historical JavaScript `var` bug and the pre-1.22 Go loop-variable surprise. The fix is to create a `NewEnclosedEnvironment` at the start of each iteration and bind the loop variable inside it, so each closure captures a distinct scope holding that iteration's value. Monkey uses per-iteration scopes, which matches JavaScript's `let` and Go 1.22's redefined loop semantics, and it is a direct corollary of capture-by-pointer: the cure is not to copy the value but to give each closure its own scope to point at.

## Common Mistakes

### Using Set Instead of Update for Outer-Scope Mutation

The inner closure assigns with `env.Set("count", newVal)` on its own call environment. That creates a fresh `count` local to the call, shadowing the outer binding the counter depends on. Every call reads the outer `count` (the local does not exist yet), increments it, and writes a new local shadow, so the outer value never moves and the counter is stuck at one. Compile assignment to `Update` instead: it walks the chain, finds the scope that declared `count`, and mutates it in place, so the next call sees the updated value.

### Extending the Caller's Environment Instead of the Defining Environment

The call helper builds `NewEnclosedEnvironment(callerEnv)` rather than `NewEnclosedEnvironment(f.Env)`. The callee can now see the caller's locals, and any closure that captured a different defining scope resolves free names against the wrong frame. This is the most destructive closure bug because it produces correct results for functions defined and called in the same scope and wrong results only for the cross-scope calls that closures exist to support, so simple tests pass and real programs fail. Always extend the function's stored environment.

### Sharing One Environment Across Loop Iterations

The loop calls `Set` on a single environment each pass before creating each closure. After the loop, all closures share that environment and read the loop variable's final value. Create a `NewEnclosedEnvironment` per iteration and bind the loop variable inside it, so each closure captures a distinct scope with that iteration's value.

### Assuming Recursion Needs a Snapshot or That Order Matters

Because `Function.Env` is a pointer and not a value copy, both orderings — create the function then bind the name, or bind a placeholder then create then rebind — reach the same live map, so `f.Env.Get("fact")` returns the function after the binding regardless of sequence. The mistake is to reason as if the function captured a snapshot and then conclude that recursion is impossible or that the binding must precede the function; in this design the lookup is deferred to call time and always walks the current map. The forward-reference placeholder is a readability and portability convention here, not a correctness requirement, though it is mandatory in value-copy environment designs.

### Forgetting to Unwrap ReturnValue at the Function Boundary

A `return` inside a body produces a `*ReturnValue` wrapper so the signal can propagate up through nested blocks (loops, `if`/`else`) without being mistaken for an ordinary value. That wrapper must be unwrapped at the function boundary before the caller receives it; otherwise the caller compares a `*ReturnValue` against an `*Integer` and gets a type mismatch. Unwrap once, where the call returns, and let only the inner value escape to the caller.

---

Next: [01-environment-chain-and-first-class-functions.md](01-environment-chain-and-first-class-functions.md)
