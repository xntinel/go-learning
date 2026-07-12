# 1. Reading SSA Output

The Go compiler converts source code to Static Single Assignment (SSA) form before generating machine code. SSA is the intermediate representation where the bulk of optimization happens: dead code elimination, constant folding, bounds-check elimination, and inlining all operate on SSA values. Learning to read SSA output lets you verify that expected optimizations fire, trace why the compiler generates particular code, and diagnose performance problems that are invisible at the source level.

The hard part of this lesson is not generating the output — that is a single environment variable — but reading the dense notation: values with unique IDs, explicit memory-threading, phi nodes that merge control flow paths, and dozens of numbered passes with names like `opt`, `nilcheckelim`, and `prove`.

```text
ssareader/
  go.mod
  analysis.go
  analysis_test.go
  cmd/demo/main.go
```

## Concepts

### Static Single Assignment Form

In SSA, each value is assigned exactly once. The Go compiler's SSA README states:

> "A value mainly consists of a unique identifier, an operator, a type, and some arguments."

A concrete value looks like this in the HTML output:

```
v4 = Add64 <int> v2 v3
```

`v4` is the unique ID, `Add64` is the operation, `<int>` is the type, and `v2 v3` are the argument values. Because every value is defined once, the compiler can reason about uses freely: if `v4` has no uses, it is dead.

The special `memory` type threads through all load and store operations to preserve ordering:

```
v10 = Store <mem> {int} v6 v8 v1
v14 = Store <mem> {int} v7 v8 v10   // depends on v10's memory
```

This ordering chain means the compiler cannot legally reorder two stores, even when both target different addresses.

### Basic Blocks

A basic block is a maximal straight-line code sequence with a single entry point and a single exit point. In SSA notation, blocks are named `b1`, `b2`, ... and their exit instructions name the successors:

```
b1:
  v1 = InitMem <mem>
  v6 = Arg <bool> {b}
  If v6 -> b2 b3      // branch: true -> b2, false -> b3

b2: <- b1
  Ret v11

b3: <- b1
  Ret v15
```

The `<- b1` annotation records the predecessors of each block.

### Phi Nodes

When two control-flow paths merge, the same Go variable may have different values depending on which path was taken. A Phi node selects the correct value:

```
b4: <- b2 b3
  v20 = Phi <int> v8 v12    // v8 if from b2, v12 if from b3
```

Phi nodes appear only at join points. If a loop has a counter that starts at zero and is incremented each iteration, the back-edge join has a Phi that selects between the initial value (from the entry path) and the incremented value (from the loop body).

### Compilation Phases

`GOSSAFUNC=FuncName go build` writes `ssa.html` to the current directory. The HTML shows the function at every pass as a column in a wide table. Passes run sequentially on one function at a time. Notable groups:

| Phase group | Example pass names | What happens |
|---|---|---|
| Construction | `number lines`, `early phielim` | Build initial SSA from AST |
| Optimization | `opt`, `nilcheckelim`, `prove`, `sccp` | Machine-independent rewrites |
| Lowering | `lower`, `late lower` | Replace abstract ops with arch ops |
| Register allocation | `regalloc` | Assign values to physical registers |
| Final | `schedule`, `layout`, `trim` | Order blocks and instructions |

The `lower` pass is the architectural pivot: before it, operations like `Add64` are abstract; after it, they are AMD64-specific (or arm64-specific, etc.). You can see this shift by comparing columns in the HTML.

### Reading the HTML Interactively

Open `ssa.html` in a browser. Click any value ID (e.g., `v20`) to highlight every definition and use of that value across all phases. This makes it easy to trace how a value is transformed — or eliminated — as passes run. Append `+` to the function name to dump plain text to stdout instead of generating HTML:

```bash
GOSSAFUNC=sumSlice+ go build -o /dev/null .
```

## Exercises

### Exercise 1: The Package Under Analysis

The functions in this package are what you will inspect with `GOSSAFUNC`. They are designed to surface distinct SSA patterns: a loop accumulator (induction variable + phi), a conditional merge (phi from an if-else), and arithmetic that constant folding can eliminate.

Create `analysis.go`:

```go
// analysis.go
package ssareader

// SumSlice accumulates the elements of nums.
// In SSA this produces a loop with a Phi node that merges
// the initial accumulator (0) with the incremented value.
func SumSlice(nums []int) int {
	total := 0
	for _, n := range nums {
		total += n
	}
	return total
}

// ConditionalAdd demonstrates a Phi node at an if-else join point.
// When add is true the result is a+b; otherwise it is a.
// The two branches converge at a single return block where a Phi
// selects between them.
func ConditionalAdd(a, b int, add bool) int {
	result := a
	if add {
		result = a + b
	}
	return result
}

// ConstantFold returns a product of two untyped constants.
// Because both operands are untyped constants, the Go spec requires
// the compiler to evaluate the expression at typecheck time, before
// SSA is constructed. As a result, the multiplication is never present
// in SSA at all: the very first column ("start") already contains only
// a Const64 [42] with no Mul op anywhere.
func ConstantFold() int {
	const x = 6
	const y = 7
	return x * y
}

// BoundsCheckElim accesses a slice at fixed indices that can be
// proven safe by the "prove" pass. The prove pass marks the
// IsInBounds ops redundant (and logs "Proved IsInBounds" under
// -d=ssa/prove/debug=1), but the ops are physically removed by
// later deadcode/lower passes, not by prove itself.
func BoundsCheckElim(s []int) int {
	if len(s) < 3 {
		return 0
	}
	// After the len(s) >= 3 guard, prove knows all three accesses
	// are safe and marks their IsInBounds checks for elimination.
	return s[0] + s[1] + s[2]
}
```

### Exercise 2: Tests That Verify the Contract

Create `analysis_test.go`. These tests do not inspect SSA directly — the compiler is not observable at test time — but they verify the observable contract of the functions so any optimization that breaks semantics is caught immediately.

```go
// analysis_test.go
package ssareader

import (
	"fmt"
	"testing"
)

func TestSumSlice(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []int
		want int
	}{
		{"nil", nil, 0},
		{"empty", []int{}, 0},
		{"single", []int{5}, 5},
		{"positive", []int{1, 2, 3, 4, 5}, 15},
		{"negatives", []int{-1, -2, 3}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SumSlice(tc.in)
			if got != tc.want {
				t.Fatalf("SumSlice(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestConditionalAdd(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b int
		add  bool
		want int
	}{
		{"add true", 10, 20, true, 30},
		{"add false", 10, 20, false, 10},
		{"zero b add true", 5, 0, true, 5},
		{"negatives", -3, -4, true, -7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ConditionalAdd(tc.a, tc.b, tc.add)
			if got != tc.want {
				t.Fatalf("ConditionalAdd(%d, %d, %v) = %d, want %d",
					tc.a, tc.b, tc.add, got, tc.want)
			}
		})
	}
}

func TestConstantFold(t *testing.T) {
	t.Parallel()

	if got := ConstantFold(); got != 42 {
		t.Fatalf("ConstantFold() = %d, want 42", got)
	}
}

func TestBoundsCheckElim(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []int
		want int
	}{
		{"nil returns 0", nil, 0},
		{"short returns 0", []int{1, 2}, 0},
		{"exact three", []int{1, 2, 3}, 6},
		{"longer", []int{10, 20, 30, 99}, 60},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := BoundsCheckElim(tc.in)
			if got != tc.want {
				t.Fatalf("BoundsCheckElim(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func ExampleSumSlice() {
	fmt.Println(SumSlice([]int{1, 2, 3, 4, 5}))
	// Output: 15
}

func ExampleConditionalAdd() {
	fmt.Println(ConditionalAdd(10, 20, true))
	fmt.Println(ConditionalAdd(10, 20, false))
	// Output:
	// 30
	// 10
}

func ExampleConstantFold() {
	fmt.Println(ConstantFold())
	// Output: 42
}
```

The `Example` functions are auto-verified by `go test`; if the output changes, the test fails.

Your turn: add `TestSumSliceAllNegative` that calls `SumSlice([]int{-10, -20, -30})` and asserts the result is `-60`.

### Exercise 3: A Runnable Demo

Create `cmd/demo/main.go`. This touches only exported API and shows the functions behaving correctly at runtime before you inspect their SSA:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/ssareader"
)

func main() {
	fmt.Println("SumSlice:", ssareader.SumSlice([]int{1, 2, 3, 4, 5}))
	fmt.Println("ConditionalAdd(10,20,true):", ssareader.ConditionalAdd(10, 20, true))
	fmt.Println("ConditionalAdd(10,20,false):", ssareader.ConditionalAdd(10, 20, false))
	fmt.Println("ConstantFold:", ssareader.ConstantFold())
	fmt.Println("BoundsCheckElim([1,2,3,4]):", ssareader.BoundsCheckElim([]int{1, 2, 3, 4}))
}
```

Run it with:

```bash
go run ./cmd/demo
```

### Exercise 4: Inspect the SSA

From `~/go-exercises/ssareader`, generate SSA for each function:

```bash
GOSSAFUNC=SumSlice go build -o /dev/null .
```

Open `ssa.html` in a browser. Work through these observations:

1. Locate the `start` column. Find the `Phi` node in the loop body. Note its two arguments: the initial value (from the function entry path) and the incremented value (from the loop back-edge).
2. Switch to the `opt` column. Compare the number of values — dead stores and redundant copies are gone.
3. In `ConditionalAdd`, find the `If` block at the top of the HTML. Trace both successor blocks (the `add == true` path and the `add == false` path) to their join point. The join block contains a `Phi` selecting between the two results.
4. For `ConstantFold`, open the `start` column. Confirm that there is already a `Const64 [42]` and no `Mul` or `Mul64` op anywhere in the output. With untyped constant operands, the Go spec mandates compile-time evaluation before SSA is constructed, so the multiplication never enters SSA at any phase.
5. For `BoundsCheckElim`, find the `IsInBounds` ops in the `start` column (there are three, one per slice access). In the `prove` column they are still present — `prove` marks them redundant but does not delete them. Compare `prove` with a later column such as `lower` or `deadcode` to see them actually disappear. To watch `prove` reason about the checks, rebuild with `-d=ssa/prove/debug=1`; it will print "Proved IsInBounds" for each access.

To dump plain text instead of HTML (useful for scripting or CI):

```bash
GOSSAFUNC=SumSlice+ go build -o /dev/null .
```

To restrict the output to specific passes:

```bash
GOSSAFUNC="SumSlice:opt,prove" go build -o /dev/null .
```

## Common Mistakes

### Targeting a Lowercase Function

Wrong: `GOSSAFUNC=sumSlice go build` on an unexported function inside a package named `ssareader`.

What happens: the compiler matches function names case-sensitively against the symbol table. For an unexported function `sumSlice`, the symbol appears as `ssareader.sumSlice`; the plain name `sumSlice` does match it, but if you use `GOSSAFUNC=SumSlice` on an unexported function, you get no output and no error — the file is simply not generated.

Fix: match the exact name, including capitalization, as it appears in the source. Use `GOSSAFUNC=funcName` for unexported and `GOSSAFUNC=PackageSuffix.FuncName` for package-qualified matches.

### Running `go run` Instead of `go build`

Wrong: `GOSSAFUNC=SumSlice go run main.go`

What happens: `go run` compiles and executes immediately; `ssa.html` may still be generated, but the working directory is a temporary build directory, not your project directory. The file appears somewhere under `/tmp`, not next to your source.

Fix: use `go build -o /dev/null .` to trigger compilation in your working directory, which is where `ssa.html` is always written.

### Misreading a Phi Node's Arguments

Wrong: assuming the first argument of a `Phi` is always the "initial" value and the second is always the "loop update".

What happens: the argument order reflects the predecessor block order in the function's block list, not lexical source order. In some functions the back-edge block is listed before the entry-edge block, reversing the argument order.

Fix: read the predecessor labels on the Phi's containing block (`b4: <- b1 b3`) to determine which argument comes from which predecessor, then match the argument to its source block.

### Expecting SSA Values to Match Go Variable Names

Wrong: looking for a value named `total` or `result` in the SSA output.

What happens: SSA values are numbered (`v2`, `v3`, ...), not named after Go variables. Variable names appear only as position annotations in the HTML tooltip or in the `debug` column, not as value identifiers.

Fix: follow the data flow numerically. Start from the `Arg` or `Const` ops that supply the initial values, then trace the uses by clicking on value IDs in the HTML.

## Verification

From `~/go-exercises/ssareader`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. Then generate SSA for at least one function and open the HTML:

```bash
GOSSAFUNC=SumSlice go build -o /dev/null .
open ssa.html    # macOS; use xdg-open on Linux
```

Confirm `ssa.html` is present and non-empty, and that the first column is named `start` and the last is named `genssa`.

## Summary

- `GOSSAFUNC=FuncName go build` writes `ssa.html` to the working directory; append `+` for plain-text stdout output.
- SSA values are numbered (`v1`, `v2`, ...) and defined exactly once; each carries an Op, a type, and argument value IDs.
- The `memory` type threads through all loads and stores, preventing illegal reordering.
- Basic blocks (`b1`, `b2`, ...) are straight-line sequences; an `If` block has two successors; a `Ret` block has none.
- Phi nodes appear at control-flow join points and select one of several incoming values based on which predecessor was taken.
- Key phase boundary: before `lower`, ops are architecture-independent; after `lower`, they are arch-specific (AMD64, arm64, ...).
- Notable optimization passes: `opt` (algebraic rewrites), `nilcheckelim` (remove proven-safe nil checks), `prove` (marks bounds checks redundant; physical removal happens in later deadcode/lower passes), `sccp` (sparse conditional constant propagation).

## What's Next

Next: [Compiler Optimization Passes](../02-compiler-optimization-passes/02-compiler-optimization-passes.md).

## Resources

- [Introduction to the Go compiler's SSA backend (README)](https://go.dev/src/cmd/compile/internal/ssa/README) — canonical specification of values, blocks, passes, and GOSSAFUNC.
- [cmd/compile/internal/ssa/compile.go](https://github.com/golang/go/blob/master/src/cmd/compile/internal/ssa/compile.go) — the authoritative ordered list of all SSA passes.
- [Looking at your program's structure in Go 1.7 (Paul Smith)](https://www.pauladamsmith.com/blog/2016/08/go-1.7-ssa.html) — practical walkthrough of reading `ssa.html` with annotated screenshots.
- [Static single-assignment form (Wikipedia)](https://en.wikipedia.org/wiki/Static_single_assignment_form) — background on the SSA property, phi nodes, and their role in compiler optimization.
