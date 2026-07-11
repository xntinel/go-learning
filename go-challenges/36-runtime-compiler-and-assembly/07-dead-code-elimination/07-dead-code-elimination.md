# 7. Dead Code Elimination

Dead code elimination (DCE) in Go operates at two distinct levels that are easy to conflate. The compiler's SSA backend removes unreachable basic blocks and unused computations within a function — triggered by constant conditions, inlining, and the `prove` pass. The linker removes entire functions and types unreachable from the program's entry points using reachability analysis. A third optional tool (`golang.org/x/tools/cmd/deadcode`) applies Rapid Type Analysis (RTA) to find functions the linker keeps but that can never be called through any dynamic dispatch path.

The hard part is knowing which level eliminates what, and how to write code that cooperates with the optimizer. Interface values, reflection, and `go:linkname` can all prevent linker DCE by creating implicit references that static analysis cannot see.

```text
dce/
  go.mod
  dce.go
  dce_test.go
  cmd/demo/main.go
```

## Concepts

### Compiler-Level DCE: Constant Conditions and Unreachable Blocks

The Go compiler converts each function to SSA form and then runs a sequence of passes. One of the first is `generic deadcode`, which removes any basic block it can prove will never execute. A `const bool` guard is the simplest trigger:

```go
const debug = false

func process(data []byte) int {
	result := len(data)
	if debug {
		// This entire block becomes an unreachable SSA block.
		// The compiler's deadcode pass removes it before codegen.
		// If fmt was only imported here, it is eliminated too.
		_ = data
	}
	return result
}
```

Because `debug` is a compile-time constant, the compiler's `prove` pass establishes that the `if`-branch is statically false. The `generic deadcode` pass removes the block entirely — including any imports that were only referenced inside it. If `fmt` was only used inside the `if debug` block, it disappears from the object file.

Observe the effect with `GOSSAFUNC`:

```bash
GOSSAFUNC=process go build -o /dev/null .
```

Open the generated `ssa.html` and compare the `start` block with the `generic deadcode` block: the dead branch is gone.

### Linker-Level DCE: Reachability from main.main

The Go linker performs reachability analysis starting from `main.main` (and any `init` functions). Any function not transitively reachable from those roots is dropped from the final binary. This is why importing a large package like `encoding/json` does not bloat your binary if you only call `json.Marshal` — only `json.Marshal` and the functions it transitively calls are linked in.

The practical consequence: exported functions in a library package that are never called are eliminated by the linker. Exported functions in the `main` package are kept because the linker cannot prove external code will not call them.

Verify symbol presence with `go tool nm`:

```bash
go build -o mybinary ./cmd/demo
go tool nm mybinary | grep FunctionName
```

If a function name does not appear in the output, the linker eliminated it.

### Build Constraints and Conditional Compilation

Build constraints (the `//go:build` line, introduced in Go 1.16) select which source files are compiled. Files excluded by a constraint are never parsed, so their code never enters the compiler — this is file-level conditional compilation, stronger than function-level DCE.

The pattern for a debug-logging pair:

```go
//go:build debug

package dce // file: log_debug.go — only compiled with -tags debug

import "fmt"

func logf(format string, args ...any) { fmt.Printf(format+"\n", args...) }
```

```go
//go:build !debug

package dce // file: log_release.go — compiled by default

func logf(string, ...any) {} // no-op; call sites and this body are both eliminated
```

Build with and without the tag:

```bash
go build -tags debug -o bin-debug ./cmd/demo
go build -o bin-release ./cmd/demo
ls -l bin-debug bin-release
```

The release binary is smaller because `fmt` is never referenced.

### What Defeats Linker DCE

Three common patterns prevent the linker from eliminating code:

1. Interface values. When you assign a concrete type to an interface, the linker must keep all methods of that type because any interface call might dispatch to them at runtime.

2. Reflection. `reflect.TypeOf(v)` creates an implicit reference to the full type descriptor, including method sets.

3. `go:linkname`. The `//go:linkname` directive creates a symbol alias visible only to the linker; static analysis cannot see it.

The `golang.org/x/tools/cmd/deadcode` tool uses RTA to find functions that survive linker DCE but are still unreachable through any actual call path, accounting for interface dispatch.

### The deadcode Tool

Install and run:

```bash
go install golang.org/x/tools/cmd/deadcode@latest
deadcode .
```

The tool reports functions unreachable from any `main` or `init` entry point. The `-whylive=pkg.FuncName` flag prints the call-graph path keeping a function alive; `-test` includes test binaries in the analysis.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/dce/cmd/demo
cd ~/go-exercises/dce
go mod init example.com/dce
```

This is a library package verified by `go test`. There is no `main` in the library itself.

### Exercise 1: A Package that Demonstrates Both DCE Levels

Create `dce.go`:

```go
// dce.go
package dce

// Verbosity is a compile-time constant. When false, the compiler's
// SSA dead-code pass removes every branch guarded by it.
const Verbosity = false

// Compute returns the sum of values in data.
// When Verbosity is false the entire diagnostic branch is eliminated
// at compile time; no dead code reaches the linker.
func Compute(data []int) int {
	total := 0
	for _, v := range data {
		total += v
	}
	if Verbosity {
		// In a real program this block would call fmt.Printf.
		// With Verbosity = false the block is an unreachable SSA
		// node and is removed before code generation.
		noop(total)
	}
	return total
}

// noop exists only to give the Verbosity block a call site without
// importing fmt. The linker eliminates it because no reachable code
// calls it when Verbosity is false.
func noop(int) {}

// Greeter is an interface whose implementations demonstrate linker DCE.
type Greeter interface {
	Greet() string
}

// HelloGreeter is instantiated by cmd/demo. Because cmd/demo passes a
// statically-known HelloGreeter{} to Dispatch, the compiler can devirtualize
// the interface call and inline Greet — no method symbol is emitted for it in
// the final binary. The deadcode tool does not report it as unreachable.
type HelloGreeter struct{}

// Greet returns "hello".
func (HelloGreeter) Greet() string { return "hello" }

// GoodbyeGreeter is never instantiated outside tests.
// The deadcode tool flags GoodbyeGreeter.Greet as unreachable in the
// main binary because no code ever creates a GoodbyeGreeter value
// in cmd/demo.
type GoodbyeGreeter struct{}

// Greet returns "goodbye".
func (GoodbyeGreeter) Greet() string { return "goodbye" }

// Dispatch calls g.Greet via the Greeter interface. With the statically-known
// concrete type in cmd/demo, the compiler devirtualizes the call; neither
// HelloGreeter.Greet nor GoodbyeGreeter.Greet appears as a linker symbol.
// The deadcode tool (not nm) is the right instrument for this distinction:
// it flags GoodbyeGreeter.Greet as unreachable while HelloGreeter is live.
func Dispatch(g Greeter) string {
	return g.Greet()
}
```

### Exercise 2: Test the Observable Behavior

Create `dce_test.go`:

```go
// dce_test.go
package dce

import (
	"fmt"
	"testing"
)

func TestComputeEmpty(t *testing.T) {
	t.Parallel()
	if got := Compute(nil); got != 0 {
		t.Fatalf("Compute(nil) = %d, want 0", got)
	}
}

func TestComputeSum(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data []int
		want int
	}{
		{"single", []int{7}, 7},
		{"positive", []int{1, 2, 3, 4}, 10},
		{"mixed", []int{-3, 3, -1, 1}, 0},
		{"large", []int{100, 200, 300}, 600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Compute(tc.data); got != tc.want {
				t.Errorf("Compute(%v) = %d, want %d", tc.data, got, tc.want)
			}
		})
	}
}

func TestDispatchHelloGreeter(t *testing.T) {
	t.Parallel()
	g := HelloGreeter{}
	if got := Dispatch(g); got != "hello" {
		t.Fatalf("Dispatch(HelloGreeter{}) = %q, want %q", got, "hello")
	}
}

func TestDispatchGoodbyeGreeter(t *testing.T) {
	t.Parallel()
	// GoodbyeGreeter is never instantiated in production code, but it is a
	// valid Greeter and must behave correctly when the test exercises it.
	g := GoodbyeGreeter{}
	if got := Dispatch(g); got != "goodbye" {
		t.Fatalf("Dispatch(GoodbyeGreeter{}) = %q, want %q", got, "goodbye")
	}
}

func ExampleCompute() {
	fmt.Println(Compute([]int{1, 2, 3}))
	// Output: 6
}

func ExampleDispatch() {
	fmt.Println(Dispatch(HelloGreeter{}))
	// Output: hello
}
```

Your turn: add `TestComputeNegativeOnly` that passes `[]int{-5, -3, -2}` and asserts the result is `-10`.

### Exercise 3: A Runnable Demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/dce"
)

func main() {
	data := []int{10, 20, 30, 40}
	sum := dce.Compute(data)
	fmt.Printf("sum: %d\n", sum)

	greeting := dce.Dispatch(dce.HelloGreeter{})
	fmt.Printf("greeting: %s\n", greeting)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sum: 100
greeting: hello
```

## Common Mistakes

### Confusing Compiler DCE with Linker DCE

Wrong: assuming that a `const false` guard causes the function itself to disappear from the binary.

What happens: the compiler removes the unreachable block inside the function, but the function's symbol still exists in the object file. The linker removes a function only when nothing in the reachable call graph references it.

Fix: to eliminate a function from the binary entirely, ensure no call site references it (linker DCE), or use a build constraint to exclude the file that defines it.

### Using a Variable Instead of a Constant for a Guard

Wrong:

```go
var debug = false // set via flag or env var

func process(data []byte) int {
	if debug { // NOT eliminated: debug is not a compile-time constant
		// ...
	}
	return len(data)
}
```

What happens: the compiler cannot prove the condition is always false, so the block is compiled in, `fmt` remains imported, and the branch is evaluated at runtime.

Fix: use `const debug = false`. If runtime control is needed, use a build tag to select between a file with a real body and a file with a no-op.

### Expecting Interface Methods to Be Eliminated

Wrong: assigning a concrete type to an interface and expecting the linker to eliminate methods on that type that are never called at a specific call site.

What happens: the linker treats the type's full method set as reachable the moment any value of that type is assigned to an interface anywhere in the binary.

Fix: if binary size matters, avoid unnecessary interface indirection. The `deadcode` tool identifies methods that survive the linker but are never actually dispatched.

### Using Binary Size as the Sole Proof of DCE

Wrong: comparing binary sizes and concluding DCE fired based on a size difference.

What happens: binary size is affected by DWARF info, string literals, alignment padding, and the `-ldflags='-s -w'` strip flags — all independent of DCE.

Fix: use `go tool nm binary | grep FunctionName` to confirm whether a specific symbol is present or absent. For interface-method reachability, prefer `deadcode ./cmd/...` — inlining and devirtualization can remove symbols from `nm` output even when an implementation is logically live.

## Verification

From `~/go-exercises/dce`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. Then confirm the DCE analysis with the `deadcode` tool:

```bash
go install golang.org/x/tools/cmd/deadcode@latest
deadcode ./cmd/demo
```

Expected output includes a line such as:

```
dce.go:51:23: unreachable func: GoodbyeGreeter.Greet
```

`GoodbyeGreeter.Greet` is reported as unreachable because no code path in
`cmd/demo` creates a `GoodbyeGreeter` value. `HelloGreeter.Greet` is not
reported because it is live.

Note: `go tool nm` is not reliable for this particular distinction. When
`cmd/demo` passes a statically-known `HelloGreeter{}` to `Dispatch`, the
compiler devirtualizes the interface call and inlines `Greet` — so neither
`HelloGreeter.Greet` nor `GoodbyeGreeter.Greet` appears as a linker symbol in
the binary. Use `nm` to verify the presence or absence of whole functions that
are not subject to inlining/devirtualization; use `deadcode` for RTA-based
reachability of interface implementations.

## Summary

- Compiler DCE removes unreachable SSA blocks within a function; `const false` conditions are the clearest trigger.
- Linker DCE removes entire functions not reachable from `main.main` or `init`; only the live call graph is linked.
- Build constraints (`//go:build`) exclude files before compilation; this is file-level conditional compilation, stronger than function-level DCE.
- Interface values, reflection, and `go:linkname` create implicit references that defeat linker DCE.
- `go tool nm binary | grep Name` checks whether a symbol survived the linker; devirtualization or inlining can remove symbols that RTA considers live, so use `deadcode` for interface-method reachability.
- `golang.org/x/tools/cmd/deadcode` uses RTA to find functions the linker keeps but that are unreachable through any real dispatch path.

## What's Next

Next: [runtime.SetFinalizer](../08-runtime-setfinalizer/08-runtime-setfinalizer.md).

## Resources

- [Finding unreachable functions with deadcode](https://go.dev/blog/deadcode) — Go Blog, explains RTA and the deadcode tool in detail
- [Introduction to the Go compiler's SSA backend](https://go.dev/src/cmd/compile/internal/ssa/README) — covers the deadcode pass and how to use GOSSAFUNC
- [Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — official reference for //go:build syntax and boolean operators
- [deadcode command](https://pkg.go.dev/golang.org/x/tools/cmd/deadcode) — pkg.go.dev reference for flags and output format
- [Go Compiler Optimizations wiki](https://go.dev/wiki/CompilerOptimizations) — lists all documented optimization passes including SSA phases
