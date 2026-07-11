# 2. Loop Variable Semantic Change — Concepts

For more than a decade the single most reported bug in Go was not a runtime panic or a subtle deadlock; it was a `for` loop that captured its iteration variable and every closure or goroutine in the loop ended up seeing the same final value. Go 1.22 fixed this at the language level by giving loop variables per-iteration scope. The change is small to state and large in consequence: code that was wrong for ten years is now correct, an entire defensive idiom is now obsolete, and the behavior of a loop now depends on the `go` directive in your module's `go.mod`. This file is the conceptual foundation for the exercises, which prove the new semantics with closures, goroutines under the race detector, and pointer capture.

## Concepts

### Per-loop versus per-iteration scope

Consider the canonical range loop `for _, v := range items`. The question the language has to answer is: how many distinct variables named `v` exist when the loop runs ten times? Before Go 1.22 the answer was one. The variable `v` was declared a single time, at the top of the loop, and each iteration assigned the next element into that same storage. After Go 1.22 the answer is ten. Each iteration gets its own fresh `v`, and the element value is copied into that iteration's private variable.

The same rule applies to the three-clause form `for i := 0; i < n; i++`. Before Go 1.22 there was one `i` shared across the whole loop. After Go 1.22 each iteration receives a fresh `i` whose value is copied from the previous iteration; the loop condition and post-statement operate on that per-iteration copy, and the result is copied forward to seed the next iteration. The observable counting behavior is identical — the loop still runs the same number of times with the same values — but the *identity* of `i` is now per-iteration, which is exactly what closures and address-of operations care about.

The distinction is invisible to ordinary loop bodies that only read the variable and finish. It becomes visible the instant the variable's lifetime is extended past the iteration: by a closure that captures it, by a goroutine that reads it, or by taking its address. In all three cases the old semantics leaked the single shared variable into the future, where its value kept changing under the consumer's feet.

### Why closures saw the last value

A closure captures variables by reference, not by value. Under the old per-loop semantics, every closure created inside `for _, v := range items` captured a reference to the one and only `v`. By the time those closures were finally called — after the loop had finished — `v` held the last element. All of them returned it. The classic symptom was a slice of functions that all printed `items[len(items)-1]`, or a map of handlers that all responded as the final route.

The decade-old workaround was a shadowing redeclaration: `v := v` as the first line of the loop body. This created a new variable in the iteration's block scope that shadowed the loop variable, and the closure captured that block-scoped copy instead. Under Go 1.22 the loop variable is *already* a fresh per-iteration variable, so the shadow assignment is redundant. New code should not write it, and `go vet` no longer needs to warn about the pattern it was designed to catch.

### Why goroutines were worse: a genuine data race

A goroutine launched inside a loop has the same capture problem as a closure, with one extra hazard: timing. Under the old semantics, `go func() { use(i) }()` captured the shared `i`, and the goroutine typically did not run until after the loop had advanced or finished, so it observed a later value. That alone produced wrong results. But it was also a true data race in the memory-model sense: the loop's post-statement wrote to `i` on the main goroutine while the launched goroutines read `i` concurrently, with no synchronization between them. The race detector (`go test -race`) flagged it as a write/read data race, not merely a logic bug.

Under Go 1.22 each iteration's variable is distinct and is never written again after that iteration begins, so the goroutine reads a value that no one else mutates. The same source code that raced before is now race-free and produces one distinct value per iteration. This is the most consequential part of the change: it converts a category of code that was *undefined behavior* into well-defined, correct behavior, with no edit to the loop.

### Taking addresses: iteration pointers are still not element pointers

Per-iteration scope fixes the sharing problem but does not change what `&v` points at. When you write `&v` inside a range loop, you take the address of the iteration's copy of the element, not the address of the element inside the backing array. Under the old semantics this was a notorious trap because all the `&v` pointers were equal — they all pointed at the one shared variable — so a slice of them aliased a single value that ended up holding the last element. Under Go 1.22 the pointers are now distinct and each points at a live, correct copy, which removes the aliasing bug.

What it does *not* do is make `&v` a pointer into the original slice. Mutating through `&v` mutates the iteration copy and leaves the caller's slice untouched. When the intent is to hand back pointers that alias the caller's elements — so that writing through them updates the original slice — the correct form is still index addressing: `&items[i]`. The per-iteration change improved the safety of value-copy pointers; it did not collapse the real and deliberate difference between a pointer to a copy and a pointer to an element.

### The change is gated by the module language version

This is a language-version change, not a toolchain flag, and that distinction governs when it takes effect. The behavior is selected by the `go` directive in your module's `go.mod`. A module that declares `go 1.22` or later gets per-iteration scope; a module that declares `go 1.21` or earlier keeps per-loop scope *even when compiled with a current Go toolchain*. The compiler reads the language version and applies the matching loop semantics. Per-file `//go:build go1.21` style version selection can also pin individual files. The practical upshot is that upgrading the toolchain does not silently change your loops; bumping the `go` line in `go.mod` does. Every exercise here declares `go 1.26`, so every one of them runs under the new per-iteration semantics.

A second practical note: the Go team designed this as a rare backward-incompatible change precisely because the old behavior caused far more bugs than it prevented. They ran the change against a large corpus and found vanishingly few programs that depended on the old sharing, and those that did were almost always already buggy. The migration story is therefore "bump the `go` line and your latent loop bugs disappear," not "audit every loop."

## Common Mistakes

### Cargo-culting the `v := v` shadow assignment

Wrong: opening every range loop body in a Go 1.22+ module with `v := v` out of habit, because old code and old tutorials did it.

What happens: nothing breaks, but the line is dead weight. It declares a variable that shadows an already-per-iteration variable, adding noise and implying to a reader that the loop variable is still shared when it is not. In `t.Parallel()` subtests the old `tc := tc` line is the most common instance of this fossil.

Fix: delete it in modules declaring `go 1.22` or later. Keep an explicit copy only when it serves a different, stated purpose (for example, copying out of a large struct element to avoid repeated field reloads).

### Assuming the toolchain version controls the behavior

Wrong: believing that installing Go 1.22+ changes how your existing module's loops behave.

What happens: a module still declaring `go 1.21` keeps per-loop scope under a Go 1.26 toolchain, so a loop you expected to be fixed still leaks its variable, and a bug you thought was resolved persists.

Fix: the behavior is selected by the `go` directive in `go.mod`, not by the installed toolchain. Bump the `go` line to `1.22` or later to opt in.

### Confusing an iteration pointer with an element pointer

Wrong: assuming that after Go 1.22, `&v` inside `for _, v := range items` points into the original slice because the pointers are now distinct.

What happens: the pointers are distinct and safe to store, but each points at the iteration's *copy*. Writing through one updates the copy, not the caller's slice, so a function that was meant to expose mutable references to the original elements silently mutates throwaway storage.

Fix: use `&items[i]` with the index form when the pointer must alias the original slice element. Reserve `&v` for when a pointer to an independent copy is what you actually want.

### Treating the goroutine fix as merely cosmetic

Wrong: thinking the old goroutine-in-loop problem was "just" a logic bug that printed the wrong value.

What happens: you underestimate it. The old pattern was a real data race — concurrent unsynchronized read and write of the shared loop variable — which is undefined behavior, not a deterministic wrong answer. Reasoning about it as "always prints the last value" is itself incorrect, because a race has no guaranteed outcome.

Fix: understand that Go 1.22 removed the race by giving each iteration its own variable. The race detector confirms it: the same code that reported a data race before now runs clean under `go test -race`.

---

Next: [01-closures-over-loop-variables.md](01-closures-over-loop-variables.md)
