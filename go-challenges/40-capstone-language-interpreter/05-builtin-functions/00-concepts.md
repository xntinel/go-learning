# 5. Built-in Functions — Concepts

Built-in functions are the seam between the interpreter's abstract value system and the host runtime. The design challenge is not writing `strings.ToUpper` — it is building a registry that catches arity and type errors before they reach function bodies, and ensuring every built-in returns a new object rather than mutating its argument so the interpreter's value semantics hold across all collection operations. The higher-order built-ins (`map`, `filter`, `reduce`) expose a second challenge: invoking a language-level callable from within Go code without importing the full evaluator. This file is the conceptual foundation. Read it once and every exercise that follows — each a small, self-contained Go module that builds one category of built-ins on top of the same registry substrate — becomes a variation on the ideas below.

## Concepts

### The Registry: Name-to-Handler Dispatch

The evaluator reaches for a name that is not in the environment and asks a registry: "do you know this?" The registry is a `map[string]*Builtin`. A `*Builtin` wraps the function pointer with metadata — minimum and maximum argument count, a documentation string — so every call site gets the same arity check for free, without repeating it inside each function body. The map gives O(1) lookup; the metadata separates the contract from the implementation.

Registration looks the same everywhere in the codebase:

```go
RegisterBuiltin("len", builtinLen,
	WithArity(1, 1),
	WithDoc("len(obj) – byte length of a string; element count of an array or hash"))
```

`RegisterBuiltin` is called from `init()`, so the registry is populated before `main` runs. A functional-options signature (`opts ...BuiltinOption`) keeps the call site backward-compatible: adding a new metadata field later means adding a new `With...` option, never editing every existing registration. The function pointer and its contract are registered together, in one place, which is the whole point — the contract cannot drift away from the handler because they are declared on the same line.

### Arity and Type Guards: Fail Fast, Fail Clearly

Every public entry point — the `Dispatch` function — validates argument count before the handler body runs. `checkArgs` compares `len(args)` against `MinArgs` and `MaxArgs`, where `-1` means "no bound on this side". When the check fails, `Dispatch` returns an `*Error` object — a value the evaluator can propagate — rather than panicking. The handler body then runs only when the count is already known good, so its type assertions for the validated positions are guaranteed to have an argument to assert against.

Type errors follow two patterns. Built-ins that accept exactly one type — `push`, `pop`, `first`, `last`, `rest`, and the string operations — do an inline type assertion and build an error with a uniform shape: function name, 1-based argument index, expected type, actual type. Passing a STRING to `push` yields `push: arg 1: want ARRAY, got STRING`. Built-ins that accept several types — `len`, `abs`, the numeric helpers — use a type switch; when no case matches they emit a simpler message. Passing an INTEGER to `len` yields:

```
len: unsupported type INTEGER
```

The shared rule is that errors are returned, never thrown. A panic would unwind past the evaluator and crash the REPL; an `*Error` value flows back through the same return path as a correct result, and the evaluator decides what to do with it. This is what lets a single malformed call inside a deeply nested expression surface as a clean diagnostic instead of a stack trace.

### Functional Purity: Always Return New Objects

Arrays and hashes are immutable at the language level. `push(arr, val)` must return a *new* array and must never append to the original. The correct shape is to allocate, copy, then extend:

```go
newElems := make([]Object, len(arr.Elements), len(arr.Elements)+len(args)-1)
copy(newElems, arr.Elements)
newElems = append(newElems, args[1:]...)
return &Array{Elements: newElems}
```

If you write `arr.Elements = append(arr.Elements, ...)` instead, any other variable that points to the same `Array` observes the mutation. In the interpreter this silently corrupts the value semantics of every binding that held the array before the push. The rule is absolute: never modify a field of a received object; always construct a new one. The same discipline governs `pop`, `rest`, `merge`, `delete`, and `set` on hashes — each returns a freshly allocated container that shares element pointers with the original but never the backing slice or map. Because the elements themselves are treated as immutable too, sharing their pointers is safe; only the container is rebuilt.

### Higher-Order Built-ins and the Callable Interface

`map`, `filter`, and `reduce` accept a function as an argument. In the full interpreter that function is a `*Function` value carrying an AST body and a closure environment. But the built-in package must not import the evaluator — that would create a circular dependency, since the evaluator imports the built-ins. The resolution is a narrow interface:

```go
type Callable interface {
	Object
	Call(args ...Object) Object
}
```

`*Function` (defined in the evaluator) implements `Callable` by delegating to `Eval`. `*BuiltinCallable` (defined alongside the built-ins) wraps a plain `BuiltinFunction` so built-ins can be passed as first-class values and, just as usefully, so tests can supply a controllable callback. The higher-order built-ins only ever see `Callable`; they are decoupled from the evaluator's internals and depend on nothing but the one method they call.

When a higher-order built-in invokes the callback and the result is an error, it short-circuits immediately rather than continuing the fold:

```go
result := fn.Call(el)
if isError(result) {
	return result
}
```

This is why error propagation is part of the contract: a type error or a division-by-zero inside the callback must abort the whole `map` and surface as the result, not vanish into a half-filled array.

### I/O Isolation: The Injected Writer

`print` and any future I/O built-in write to `Output`, a package-level `io.Writer` that defaults to `os.Stdout`. Tests replace it with a `bytes.Buffer` to capture output without spawning a subprocess:

```go
var Output io.Writer = os.Stdout
```

The same principle — inject the boundary so the test can stand in for the outside world — applies to anything that touches external state. `readFile` and `writeFile` call `os.ReadFile` and `os.WriteFile` directly; tests exercise them against a real temporary directory created with `t.TempDir()`, which the test framework cleans up automatically. The injected writer is the cheaper, in-process version of the same idea, and it is what makes `print` assertable in a unit test instead of a brittle process-capture harness.

### len Is a Byte Count on Strings

`len` is overloaded across three types, and the string case is the one that surprises people. On an array it returns the element count; on a hash it returns the pair count; on a string it returns the number of *bytes*, not runes. This mirrors Go's own `len("é")`, which is 2 for a two-byte UTF-8 character, not 1. The choice is deliberate and worth stating in the doc string: a built-in that silently decoded UTF-8 would make string indexing and `len` disagree, and would cost an O(n) scan on every call. If the language later wants a rune count it adds a separate `runeLen` built-in rather than redefining `len`. Keeping `len` a byte count keeps it O(1) and consistent with the substring and slice operations layered on top of it.

## Common Mistakes

### Mutating the Input Array

Wrong: writing `arr.Elements = append(arr.Elements, val)` inside `push`.

What happens: any other variable holding a pointer to the same `Array` sees the new element. In the interpreter this silently corrupts the value semantics of every variable that was bound to the array before the push, and the corruption surfaces far from its cause.

Fix: allocate a new slice with `make`, copy the original, append the new elements, and wrap the result in a new `&Array{}`. The standard test for this calls the function and then checks `len(orig.Elements)` — if it changed, the function mutated its input.

### Not Propagating Errors From Higher-Order Callbacks

Wrong: ignoring the return value of `fn.Call(el)` and always appending the element to the result.

What happens: a type error or a division-by-zero inside the callback silently disappears. The `map` call succeeds and returns a partially computed array with a nil or garbage slot, and the failure is discovered much later as a confusing nil dereference.

Fix: after every `fn.Call`, check `isError(result)` and return the error immediately. A dedicated test installs a callback that always returns an `*Error` and asserts the whole call returns an error.

### Duplicating the Arity Check Inside the Handler

Wrong: re-checking `len(args)` inside the handler body even though `Dispatch` already validated it against the registered arity.

What happens: the manual check is harmless until it diverges from the registered contract — one path reports "want at most 1" (the registry) and another reports "want 1 arg" (the handler), so the same misuse produces different messages depending on how the function was reached.

Fix: register the arity with `WithArity` and rely on `Dispatch` to enforce it. Inside the handler, assume the count is valid and proceed straight to the type checks.

### Comparing Hash Iteration Order in Tests

Wrong: calling `keys(h)` on a hash with several entries and comparing the result directly against a fixed-order string.

What happens: Go randomizes map iteration order at runtime, so the test passes locally and fails intermittently in CI.

Fix: assert membership rather than order — check that each expected key is present in the returned array — or sort the result before comparing. Never depend on the order a hash yields its keys or values.

---

Next: [01-registry-and-collections.md](01-registry-and-collections.md)
