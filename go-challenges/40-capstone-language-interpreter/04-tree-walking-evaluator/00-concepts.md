# 4. Tree-Walking Evaluator — Concepts

A tree-walking evaluator gives meaning to an abstract syntax tree by recursing over it and producing runtime values. Every node type maps to a concrete action: an integer literal produces an Integer value, an infix node evaluates both sides and applies the operator, a call expression creates a new scope and evaluates the function body. The mechanism is a single recursive function from `(Node, Environment)` to `Object`, and a type switch on the node is the entire dispatch table. What makes it subtle is not the recursion but the discipline around three things: propagating runtime errors as values instead of panicking, knowing exactly where control-flow signals such as `return` are unwrapped, and giving closures an environment whose lifetime outlives the call that created it. This file is the conceptual foundation. Read it once and you will have everything you need to reason through each exercise, which build the evaluator as independent, self-contained Go modules: a core that handles expressions, variables, functions, and closures; a loop module that builds the signal-propagation machinery for `break` and `continue`; and an array module that adds indexing.

## Concepts

### The Object System

Every value the interpreter produces — an integer, a boolean, a function, an error — implements one small interface:

```go
type Object interface {
	Type() ObjectType
	Inspect() string
}
```

`Type()` returns a string constant (`"INTEGER"`, `"ERROR"`, `"FUNCTION"`, ...) that drives dispatch without reflection: the evaluator can ask any value what kind it is and branch on the answer. `Inspect()` produces the human-readable form used by the REPL and by test assertions. Representing each language type as its own concrete Go struct — `Integer{Value int64}`, `Boolean{Value bool}`, `String{Value string}` — is more verbose than wrapping everything in a single `any`, but it makes every type error visible to the Go compiler, keeps the common integer path allocation-light, and lets a type switch recover the concrete type when an operator needs the underlying `int64` or `string`.

A few values are allocated once and shared as package-level singletons: `TRUE`, `FALSE`, and `NULL`. Sharing them means boolean equality can be tested with pointer `==` rather than a field comparison, and it guarantees there is exactly one canonical null. The control-flow signals below follow the same pattern.

### Control-Flow Signals and ReturnValue Unwrap Timing

`return`, `break`, and `continue` are not errors and not ordinary values; they are signals that must bubble up through nested block evaluation until the right handler catches and consumes them. Each is its own wrapper type, and the entire correctness of the evaluator depends on unwrapping each at exactly one level:

```
ReturnValue    wraps a value; unwrapped at the function-call boundary (applyFunction)
               and at the top level (evalProgram)
BreakSignal    a singleton; consumed at the loop boundary (evalWhileExpression)
ContinueSignal a singleton; consumed at the loop boundary to start the next iteration
```

The pattern has two cooperating pieces. `evalBlockStatement` walks the statements of a block and, the instant a statement produces a `ReturnValue`, an `Error`, a `BreakSignal`, or a `ContinueSignal`, it returns that object immediately **without unwrapping it**. It is a relay, not a consumer. The consumers sit one level up: `evalProgram` unwraps a `ReturnValue` because a top-level return ends the whole program; `applyFunction` unwraps a `ReturnValue` after evaluating the function body because that is the function boundary; `evalWhileExpression` consumes `BreakSignal` and `ContinueSignal` because the loop is where those mean something.

Why the relay matters: imagine `return 10` sitting inside an `if` block, inside a `while` body, inside a function. If `evalBlockStatement` unwrapped the `ReturnValue` the moment it saw it, the `return` would escape only the innermost `if` block and the function would keep running. By passing the wrapped signal upward untouched, the `return` survives every intermediate block and is finally unwrapped at `applyFunction`, exiting the whole function as written. Forgetting to unwrap at the right level — or unwrapping too early — is the single most common evaluator defect.

### The Environment and Lexical Scoping

An `Environment` is a `map[string]Object` plus a pointer to an enclosing (outer) scope. `Get` looks in the innermost map and, on a miss, walks outward through the chain until it finds the name or runs out of scopes. `Set` always writes to the innermost scope and never to an outer one, which prevents action at a distance: a binding created inside a function cannot silently overwrite a caller's variable.

Closures fall out of this design almost for free. When the evaluator meets a function literal it builds a `Function` object that captures a pointer to the environment **as it exists at definition time** — not the environment of whoever later calls it. When the function is called, `applyFunction` creates a fresh enclosed environment whose outer pointer is that captured environment, binds the parameters in it, and evaluates the body there:

```
outerEnv:  {adder: <Function captured here>}
            └── closureEnv: {x: 5}        captured when the inner fn was defined
                      └── callEnv: {y: 3}  created fresh on each call
```

When the inner function looks up `x`, lookup walks `callEnv` → `closureEnv` and finds it. The closure keeps working long after `adder` returned because Go's garbage collector keeps `closureEnv` alive for exactly as long as the `Function` object that points at it is reachable. A fresh enclosed environment per call is also what makes recursion correct: each activation of `fact(n)` gets its own `n`, so the recursive descent does not stomp on the caller's binding.

### Error Propagation via isError

Runtime errors — type mismatch, undefined variable, division by zero, wrong arity — are returned as an `*Error` object, never raised as a Go panic. The evaluator stays a pure function from `(Node, Environment)` to `Object`: no globals mutated, no panics, no channels. The price is a check after every child evaluation that could fail:

```go
right := Eval(node.Right, env)
if isError(right) {
	return right
}
```

`isError` is a one-line helper that returns true when an object's `Type()` is `ERROR`. The rule is mechanical: immediately after every `Eval` call whose result might be an error, check `isError` and short-circuit. This is verbose but it is what makes errors propagate correctly. Consider `(1/0) + 5`: the left operand evaluates to a division-by-zero error; if the evaluator did not check before evaluating the right operand and calling the infix handler, it would try to add `5` to an error value, the type switch would fall through to "unknown operator", and the original, meaningful error would be masked by a confusing second one. Checking `isError` early makes the first error win and stops evaluation cleanly. Because the evaluator never panics, each error case is testable in isolation: build the smallest node that triggers it, call `Eval`, assert you got an `*Error`.

### Monkey Truthiness

This evaluator uses Monkey's truthiness rule, which is the Ruby rule, not Python's: **only `false` and `null` are falsy; every other value is truthy, including `0` and the empty string `""`**. An `if (0) { ... }` takes the consequence branch; an `if ("") { ... }` does too. The helper is a three-case type switch: `*Null` is false, `*Boolean` returns its own field, everything else is true. This is a deliberate language-design choice, not an implementation accident, so it belongs in a test (`TestTruthiness`) that pins `0` and `""` as truthy. Pinning it in a test means a future refactor cannot silently regress the rule into C-style or Python-style falsiness.

## Common Mistakes

### Unwrapping ReturnValue at the Wrong Level

Unwrapping `ReturnValue` inside `evalBlockStatement` instead of leaving it wrapped causes `return` to escape only the nearest block rather than the whole function. A `return 10` inside a nested `if` exits the `if` and execution continues in the function body. The fix is the relay discipline: `evalBlockStatement` returns the `ReturnValue` untouched; only `applyFunction` (and `evalProgram` at the top level) unwraps it.

### Forgetting isError After a Child Eval

Writing `left := Eval(n.Left, env)` and then immediately `right := Eval(n.Right, env)` without checking `isError(left)` between them lets an error from the left operand flow as a value into the infix handler. The handler does not recognize the error type, falls through to "unknown operator", and reports a misleading message that hides the real failure. Check `isError` immediately after each `Eval` call that can fail, before using its result.

### Capturing the Caller's Environment in a Closure

Building the `Function` object with, or calling `applyFunction` against, the caller's environment instead of the environment captured at definition time breaks closures. The inner function then sees the caller's bindings rather than the ones in scope where it was written, so `adder(5)(10)` fails with "identifier not found: x" because `x` lives in the definition scope, not the call site. `applyFunction` must always build the enclosed environment from the `Function`'s own captured `Env`.

### Sharing One Environment Across Recursive Calls

Reusing a single environment for every recursive call, instead of creating a fresh enclosed environment per call, means each activation overwrites the same `n` binding. The recursion then reads whatever `n` was written most recently rather than the value belonging to its own frame, and the result is wrong. The fix is the same enclosed-environment rule: every call gets `NewEnclosedEnvironment(fn.Env)` and binds its parameters there.

### Treating 0 or "" as Falsy

Importing C or Python intuition and making `0` or the empty string falsy silently changes the language. Under Monkey truthiness those are truthy; only `false` and `null` are falsy. Encode the rule in `isTruthy` as an explicit three-case switch and pin it with a test so the contract cannot drift.

Next: [01-tree-walking-evaluator.md](01-tree-walking-evaluator.md)
