# 8. Full Monkey Language Interpreter — Concepts

The previous seven lessons in this chapter produced isolated components: a lexer, a Pratt parser, an AST, a tree-walking evaluator, built-in functions, closures, and a REPL. This capstone is about wiring them into a shipping interpreter — a `monkey` binary with subcommands, a module/import system that detects circular dependencies, an integer interning pool that cuts GC pressure in tight loops, and a CLI dispatch layer that turns a process argument into a pipeline stage. The hard problems here are architectural rather than algorithmic: keeping the evaluator's core signature stable while threading a module cache through every `import(...)`, proving the interning and dispatch contracts with tests that do not depend on the whole interpreter tree, and being honest about which features are part of canonical Monkey and which are capstone extensions. Read this file once and you will have the conceptual map for each exercise, which builds one of these concerns as an independent, self-contained Go module.

## Concepts

### The Integration Architecture: A Pipeline of Passes

The complete Monkey interpreter is a pipeline of composable passes, each consuming the output of the previous one:

```text
source text
  -> Lexer      (produces a token stream)
  -> Parser     (produces an *ast.Program)
  -> Evaluator  (walks the AST, mutates the environment, returns an object.Object)
  -> REPL       (drives the whole pipeline on stdin/stdout)
```

The value of stating the architecture as a pipeline is that each stage has a single, narrow contract with its neighbor: the lexer hands the parser tokens and nothing else, the parser hands the evaluator a tree and nothing else. That separation is what made it possible to build and test each stage in isolation across the previous seven lessons, and it is exactly what keeps the integration tractable — wiring the binary is mostly a matter of connecting outputs to inputs, not rewriting any stage.

The evaluator is the integration point where the module system, the object pool, and the call machinery all meet. The single most important design decision in the whole capstone is to keep the evaluator's core signature unchanged — `Eval(node ast.Node, env *object.Environment) object.Object` — even after adding modules. Anything the evaluator needs that is not a node or an environment (the module cache, a profiler, a call stack) is injected at construction time through an evaluator value (`evaluator.New(cache)`), never threaded as an extra parameter through every recursive `Eval` call. Threading a parameter would force a rewrite of every `Eval` call site and break every unit test written against the lesson-4 signature; injecting at construction leaves all of those tests passing untouched. This is the difference between an integration that lands cleanly and one that ripples through every file.

### The Module System and Circular-Import Detection

A module is a source file that, when evaluated, produces a hash of its top-level `let` bindings. The evaluator turns `import("std/math")` into a single call into the module cache, which owns the entire resolve-and-cache protocol:

```text
import("std/math")
  1. Resolve to an absolute path:  /usr/share/monkey/std/math.mk
  2. Cache hit?  Return the cached hash object immediately.
  3. Mark the path "pending" so a re-entrant import can be detected.
  4. Read, lex, parse, and evaluate the file in a fresh child environment.
  5. Collect the top-level lets into a hash object.
  6. Store it in the cache, clear the pending mark, and return.
```

Path resolution follows three rules in order: relative to the importing file's own directory first, then each directory named in a `MONKEY_PATH` environment variable (colon-separated, like the shell `PATH`), then the interpreter's built-in standard-library directory. Resolving relative to the importing file rather than the process working directory is the rule most people get wrong, and it is the difference between an import that works from every invocation directory and one that works only from the project root.

Circular-import detection falls out of one mechanism: an in-flight set. Before a file is evaluated, its absolute path is marked "pending"; if a second resolve for that same path arrives while the first has not yet finished, the chain is circular and an error is returned at once instead of recursing forever. The subtlety that makes this correct under failure is that the pending mark must be cleared on every exit path — success, error, and panic alike — because a mark left behind by a failed load would make every later import of that path falsely report a cycle. A second, equally important rule: failed loads are deliberately not cached. A transient read error on the first attempt must not poison the cache, so a later import in the same long-running REPL session can still succeed once the condition is fixed. The cache stores results only on success and marks pending unconditionally; those two asymmetric rules are the whole of the module system's correctness.

The cache is a single-threaded structure by intent — the Monkey evaluator runs on one goroutine — but it carries a mutex so that a future parallel evaluator or a REPL that spawns goroutines does not corrupt the maps. The flush of a file's evaluation happens outside the lock so that nested imports (a file importing another file) do not deadlock by re-entering the cache while the lock is held.

### Object Interning and the Integer Pool

The evaluator allocates an object for every literal and every intermediate result. The expression `1 + 2` allocates three integer objects: the two operands and the sum. Iterating over an array of 100,000 integers therefore produces on the order of 300,000 allocations per pass, which is real GC pressure for a language whose whole appeal is scripting speed.

Integer interning removes most of those allocations with a fixed pool. A package-level array is initialized once at startup to hold the integer objects for every value in a small range — this lesson uses [-5, 256] — and the constructor returns a pointer into that array for any value in range and a fresh heap allocation for everything else. Because the array is a Go value type rather than a slice of pointers, its elements are contiguous and the garbage collector scans them as a single object: the pool costs the GC nothing to trace. The choice of range is empirical, not arbitrary: [-5, 256] covers every ASCII byte value and the loop counters that dominate real programs. CPython interns exactly this range; Ruby's MRI uses a wider one. Larger pools give diminishing returns because integer literals in real code are heavily skewed toward small magnitudes.

The same singleton idea applies to the other small, immutable values. There is exactly one true object, one false object, and one null object for the entire interpreter, created once and shared. This turns every boolean test in the evaluator from a struct comparison into a pointer comparison (`result == object.True`), which is both faster and clearer. The one trap interning creates is that pointer equality is a valid equality test only inside the pool: two separately allocated objects for the value 1000 are distinct pointers with equal values, so any general equality path must compare the underlying value, using the pointer check only as a fast path when both operands are known to be pooled.

### CLI Dispatch: Turning an Argument into a Pipeline Stage

The `monkey` binary dispatches on its first argument, and each subcommand exposes a different slice of the same pipeline:

```text
run <file>      lex, parse, evaluate; silent on success     0 ok, 1 runtime error
repl            interactive prompt                           0 on Ctrl-D
fmt <file>      parse, then pretty-print the AST             0 ok, 1 parse error
ast <file>      print the AST for debugging                  0 ok, 1 parse error
tokens <file>   print the raw token stream                   0 ok, 1 read error
test <file>     run all test_ functions, report pass/fail    0 all pass, 1 any fail
```

The dispatch layer deserves to be a package of its own, free of every interpreter internal, for one reason: exit-code discipline is a contract a shell depends on, and a contract that small and that load-bearing should be tested without standing up a lexer. The convention is that usage errors (no subcommand, an unknown subcommand, a missing file argument) exit 2, runtime errors exit 1, and success exits 0. That three-way split is what lets a script chain `monkey run a.mk && monkey run b.mk` and distinguish "the script you asked for failed" from "you typed the command wrong." Parsing arguments into a small typed config, and mapping each error class to its exit code, is a pure function over a string slice — trivially testable, and the safest place in the whole binary to be precise.

### What Is Canonical Monkey, and What Is a Capstone Extension

Three features are worth describing because the capstone binary is the natural place to build them, but it is important to be honest that they are extensions beyond both the base interpreter of lessons 1-7 and the canonical Monkey language. Thorsten Ball's book ships none of them; the earlier lessons did not build them; they are design you add while wiring the full binary, each independently useful and each optional.

Tail-call optimization converts a self-call in tail position into a loop. A recursive Monkey function builds one Go stack frame per call, so a deeply recursive accumulator (`sum(n-1, acc+n)` to a depth of 10,000) overflows the goroutine stack. TCO detects that the last expression of a body is a call whose callee is the current function, and instead of recursing into `Eval` it rebinds the parameters in place and jumps back to the top of the body through a Go `for` loop, holding stack depth constant. It applies only to direct self-calls in tail position; mutual recursion needs a trampoline, which is a stretch goal.

Stack-trace error reporting attaches a slice of call frames (function name, source file, line) to the error object, pushed on call and popped on return, so a type error raised three modules deep renders as a most-recent-call-first trace instead of a bare one-line message. It turns an error from a fact into a story about where the fact came from.

The `try(fn)` built-in gives Monkey programs error handling without terminating the interpreter: it calls a user-supplied zero-argument function and returns a hash describing success (`{"ok": true, "value": ...}`) or failure (`{"ok": false, "error": ...}`), using Go's `recover` for panics and an error-object check for evaluator-level errors. None of these three are required to run canonical Monkey; they are the capstone's invitation to go further.

## Common Mistakes

### Leaving the pending mark set when a load fails

Marking a path pending and then returning early on a load error without clearing the mark permanently poisons that path: the first failed import marks it "in progress" forever, and every later import of the same path returns a circular-import error even though no cycle exists. Clear the pending mark unconditionally, ideally with a `defer delete(c.pending, absPath)` placed immediately after setting it, so success, error, and panic all unmark the path.

### Caching failed imports

Writing the result into the cache when the load returned an error caches a missing file or a parse failure permanently. Fixing the file on disk and re-running the import in a live REPL session still returns the stale, broken entry. Write to the entries map only when the error is nil; let failed loads fall through uncached so the next attempt re-evaluates.

### Comparing interned integers with == outside the pool range

Pointer equality is a correct value test only inside the pool. Two separate constructions of the value 1000 return distinct pointers, so comparing them with `==` reports them unequal even though their values match. Compare the underlying value field for any integer that might be outside the pool; reserve the pointer comparison for a fast path where both operands are known to be pooled (or are the boolean and null singletons).

### A default subcommand case that falls through to exec

Adding a default branch to the subcommand switch that runs the unrecognized argument as a system command turns a typo into arbitrary command execution — a real vulnerability if the binary ever runs with elevated privileges or behind a web interface. The default case must return an unknown-command error and exit 2. Never execute a user-controlled string as a shell command.

### Resolving import paths against the working directory

Calling `filepath.Abs` on the import string resolves it relative to the process working directory, so `import("utils.mk")` from `/home/user/project/src/main.mk` finds `/home/user/utils.mk` instead of the sibling file. The import then works from some invocation directories and silently fails from others. Resolve relative to the directory of the importing file: join the import path onto `filepath.Dir(importingFilePath)` first, then make it absolute.

Next: [01-object-interning-pool.md](01-object-interning-pool.md)
