# 2. Compiler Optimization Passes

The Go compiler transforms your source through a pipeline of roughly 50 named SSA (Static Single Assignment) passes before emitting machine code. The passes are grouped into phases — early canonicalization, generic optimization, architecture-specific lowering, register allocation, and cleanup — and each phase leaves observable fingerprints in the binary and in the compiler's `-m` output. Understanding what fires, when, and why lets you write code the compiler can reason about, diagnose unexpected allocations, and interpret performance differences between semantically equivalent programs.

The hard part is not memorizing pass names. It is learning to write code in a form the compiler can analyze statically — closed over constants, no aliased pointers, no opaque interfaces hiding the concrete type — so that passes like nil check elimination, common subexpression elimination, and escape analysis can do their jobs.

```text
optpasses/
  go.mod
  optpasses.go
  optpasses_test.go
  cmd/demo/main.go
```

## Concepts

### The SSA Pipeline

The compiler converts the type-checked AST to SSA form and then runs a fixed sequence of named passes. The names come from `cmd/compile/internal/ssa/compile.go`. The pipeline order matters: later passes rely on invariants established by earlier ones.

Key groups in order:

1. **Early canonicalization** (`early phielim and copyelim`, `early deadcode`, `short circuit`): converts away phi nodes introduced by naive SSA construction, collapses trivial copies, and removes blocks made unreachable by constant conditions.
2. **Generic optimization** (`opt`, `zero arg cse`, `generic cse`, `prove`, `nilcheckelim`, `phiopt`): the main machine-independent optimization phase. `opt` applies thousands of algebraic rewrite rules from `_gen/*.rules`; `generic cse` merges redundant computations; `prove` propagates range and sign facts through the block graph; `nilcheckelim` removes nil checks the prover has proved redundant.
3. **Lowering** (`lower`, `late lower`): rewrites generic `SSA` opcodes into architecture-specific ones. On amd64, a multiply-by-power-of-two becomes a shift, a load-then-shift may become a single `MOVBQZX`.
4. **Scheduling and register allocation** (`schedule`, `regalloc`): order values inside blocks to minimize register pressure and assign hardware registers. This is where the abstract SSA graph becomes a concrete instruction sequence.
5. **Cleanup** (`dse`, `branchelim`, `memcombine`, `trim`): dead store elimination, conditional-move promotion, adjacent memory access fusion, and removal of empty basic blocks.

You observe the pipeline by setting `GOSSAFUNC=<name>` before `go build`. The compiler writes `ssa.html` to the current directory, containing a column for every pass. Each column shows the SSA of the function after that pass. Diffing adjacent columns reveals exactly what a pass did.

### Constant Folding and Propagation

Go's spec requires constant expressions to be evaluated at compile time with arbitrary precision. The `opt` pass extends this: it applies rewrite rules that turn operations on constants into constants. A chain `a := 3; b := a*4; return b+1` is reduced to `return 13` in the `opt` phase — the value `13` appears as the sole `OpConstInt64` in the function's final SSA.

`-gcflags='-N'` disables the `opt` pass (and others). With `-N`, every intermediate value is kept as a distinct SSA value and spilled to the stack, making the comparison between optimized and unoptimized output stark.

### Common Subexpression Elimination (CSE)

The `generic cse` pass identifies SSA values with the same opcode and the same operands and replaces duplicate uses with a single value. For:

```go
func Totals(a, b int) (int, int) {
	product := a * b
	return product + 1, product + 2
}
```

`a*b` is computed once and its result reused — not computed twice. The pass works on the SSA graph, so it sees through register names; you do not have to introduce a manual temporary.

### Nil Check Elimination

Every pointer dereference in Go is guarded by an implicit nil check unless the `nilcheckelim` or `prove` pass can prove the pointer is non-nil. After the first dereference of a pointer in a straight-line block, all subsequent dereferences of the same pointer in the same block are provably safe and their nil checks are removed.

```go
func SumFields(p *Point) int {
	x := p.X // nil check here
	y := p.Y // nil check eliminated
	return x + y
}
```

`-gcflags='-m=2'` does not report nil checks directly, but `GOSSAFUNC` shows the `OpNilCheck` opcodes disappearing between the `nilcheckelim` pass column and the previous one.

### Escape Analysis

Escape analysis is not an SSA pass — it runs earlier, on the AST/IR — but its output feeds the SSA pipeline because values that do not escape are stack-allocated, avoiding GC pressure. Run `go build -gcflags='-m'` to see escape decisions. A variable "escapes to heap" when the compiler cannot prove its lifetime is bounded by the calling frame: it is returned by pointer, stored in an interface, or passed to a closure that outlives the frame.

The `prove` pass in SSA propagates the facts produced by escape analysis downstream, enabling further nil check elimination and bounds check elimination on values known to be stack-resident.

### Strength Reduction

The `lower` pass replaces expensive operations with cheaper ones: `x*8` becomes `x<<3`, `x/4` becomes `x>>2` (for non-negative `x` proven so by `prove`), and `x%8` becomes `x&7`. This is architecture-specific: on arm64 the multiplier may stay if a fused multiply-add is cheaper; on amd64 the shift is almost always preferred.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/36-runtime-compiler-and-assembly/02-compiler-optimization-passes/02-compiler-optimization-passes/cmd/demo
cd go-solutions/36-runtime-compiler-and-assembly/02-compiler-optimization-passes/02-compiler-optimization-passes
```

This is a library with a small demo binary. Verify it with `go test`.

### Exercise 1: Functions That Expose Optimization Targets

Create `optpasses.go`:

```go
// optpasses.go
package optpasses

// Point is a simple 2D coordinate used across exercises.
type Point struct {
	X, Y int
}

// FoldedConstant returns a value computed entirely from constants.
// The compiler reduces the entire body to a single OpConstInt64 during the
// "opt" pass. Inspect with: GOSSAFUNC=FoldedConstant go build -o /dev/null .
func FoldedConstant() int {
	a := 10
	b := 20
	c := a + b   // 30
	d := c * 2   // 60
	return d + 1 // 61
}

// Totals demonstrates common subexpression elimination (CSE).
// The product a*b is computed once; the "generic cse" pass merges the two
// would-be multiplications into one SSA value reused in both sums.
func Totals(a, b int) (int, int) {
	product := a * b
	return product + 1, product + 2
}

// SumFields demonstrates nil check elimination.
// The first dereference of p (reading p.X) establishes that p != nil.
// The "nilcheckelim" pass removes the implicit nil check on p.Y.
func SumFields(p *Point) int {
	x := p.X
	y := p.Y
	return x + y
}

// ShiftReduction demonstrates strength reduction.
// On amd64, the "lower" pass replaces *8 with <<3 and /4 with >>2.
// The /4 shift is an arithmetic right shift; it rounds toward negative infinity
// for negative inputs, which differs from integer division. The compiler only
// applies the substitution when the sign can be determined or when it matches
// the architecture's division semantics.
func ShiftReduction(x int) int {
	a := x * 8 // becomes x << 3 in lowered SSA
	b := x / 4 // becomes x >> 2 (arithmetic) on amd64
	return a + b
}

// DeadBranch contains a branch on a compile-time constant.
// The "early deadcode" pass removes the unreachable else-branch entirely.
// The function compiles to a single return statement.
func DeadBranch(x int) int {
	const always = true
	if always {
		return x + 1
	}
	return x * 999 // removed by early deadcode
}

// Escape vs stack allocation: NewPoint allocates on the heap because it
// returns a *Point that escapes to the caller.
// NewValue allocates on the stack because the value does not escape.
// Observe with: go build -gcflags='-m' .
func NewPoint(x, y int) *Point {
	return &Point{X: x, Y: y} // escapes to heap
}

func NewValue(x, y int) int {
	p := Point{X: x, Y: y} // stays on stack
	return p.X + p.Y
}
```

### Exercise 2: Tests That Pin the Functional Contract

Create `optpasses_test.go`:

```go
// optpasses_test.go
package optpasses

import (
	"fmt"
	"testing"
)

func TestFoldedConstant(t *testing.T) {
	t.Parallel()

	if got := FoldedConstant(); got != 61 {
		t.Fatalf("FoldedConstant() = %d, want 61", got)
	}
}

func TestTotals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b   int
		wantLo int
		wantHi int
	}{
		{3, 4, 13, 14},
		{0, 0, 1, 2},
		{-1, 5, -4, -3},
	}
	for _, tc := range cases {
		lo, hi := Totals(tc.a, tc.b)
		if lo != tc.wantLo || hi != tc.wantHi {
			t.Errorf("Totals(%d,%d) = (%d,%d), want (%d,%d)",
				tc.a, tc.b, lo, hi, tc.wantLo, tc.wantHi)
		}
	}
}

func TestSumFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		p    *Point
		want int
	}{
		{&Point{1, 2}, 3},
		{&Point{0, 0}, 0},
		{&Point{-5, 3}, -2},
	}
	for _, tc := range cases {
		if got := SumFields(tc.p); got != tc.want {
			t.Errorf("SumFields(%+v) = %d, want %d", tc.p, got, tc.want)
		}
	}
}

func TestShiftReduction(t *testing.T) {
	t.Parallel()

	cases := []struct {
		x    int
		want int
	}{
		{4, 4*8 + 4/4}, // 32 + 1 = 33
		{0, 0},
		{8, 8*8 + 8/4},    // 64 + 2 = 66
		{16, 16*8 + 16/4}, // 128 + 4 = 132
	}
	for _, tc := range cases {
		if got := ShiftReduction(tc.x); got != tc.want {
			t.Errorf("ShiftReduction(%d) = %d, want %d", tc.x, got, tc.want)
		}
	}
}

func TestDeadBranch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		x    int
		want int
	}{
		{0, 1},
		{10, 11},
		{-1, 0},
	}
	for _, tc := range cases {
		if got := DeadBranch(tc.x); got != tc.want {
			t.Errorf("DeadBranch(%d) = %d, want %d", tc.x, got, tc.want)
		}
	}
}

func TestNewValue(t *testing.T) {
	t.Parallel()

	if got := NewValue(3, 4); got != 7 {
		t.Fatalf("NewValue(3,4) = %d, want 7", got)
	}
}

func TestNewPoint(t *testing.T) {
	t.Parallel()

	p := NewPoint(5, 6)
	if p == nil || p.X != 5 || p.Y != 6 {
		t.Fatalf("NewPoint(5,6) = %+v", p)
	}
}

func ExampleFoldedConstant() {
	fmt.Println(FoldedConstant())
	// Output: 61
}

func ExampleTotals() {
	lo, hi := Totals(3, 4)
	fmt.Println(lo, hi)
	// Output: 13 14
}

func ExampleDeadBranch() {
	fmt.Println(DeadBranch(10))
	// Output: 11
}
```

### Exercise 3: Demo Binary

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/optpasses"
)

func main() {
	fmt.Println("FoldedConstant:", optpasses.FoldedConstant())

	lo, hi := optpasses.Totals(6, 7)
	fmt.Println("Totals(6,7):", lo, hi)

	p := optpasses.NewPoint(3, 4)
	fmt.Println("SumFields:", optpasses.SumFields(p))

	fmt.Println("ShiftReduction(4):", optpasses.ShiftReduction(4))
	fmt.Println("DeadBranch(10):", optpasses.DeadBranch(10))
	fmt.Println("NewValue(3,4):", optpasses.NewValue(3, 4))
}
```

Run with:

```bash
go run ./cmd/demo
```

Your turn: add a function `CSEDemo(a, b, c int) int` that computes `(a+b)*c + (a+b)` and a test `TestCSEDemo` proving its result equals `(a+b)*c + (a+b)` for several inputs. The compiler merges the two `a+b` subexpressions via `generic cse`.

## Common Mistakes

### Expecting Constant Folding on Non-Constant Inputs

Wrong: writing `func f(a, b int) int { return a + b }` and assuming the compiler folds it. Constant folding only fires when operands are compile-time constants. The `opt` pass cannot fold a function parameter because the value is not known at compile time.

What happens: the SSA shows an `OpAdd64` with two `OpArg` operands — no folding.

Fix: use `const` declarations or untyped constant literals. Variables initialized from `const` expressions are treated as constants by the compiler through constant propagation.

### Using `-gcflags='-N -l'` and Wondering Why SSA Output Changed

Wrong: running `GOSSAFUNC=f go build -gcflags='-N -l' .` and comparing to the non-`-N` run, then concluding the pass "didn't work".

What happens: `-N` disables the `opt` pass and others. The SSA columns are still produced but many passes are skipped entirely, so the "before"/"after" comparison is between different sets of passes.

Fix: inspect the two runs separately. The `-N -l` run shows the unoptimized baseline; the normal run shows the optimized result. Do not diff them pass-by-pass — diff the final SSA value between the two runs.

### Misreading Strength Reduction for Negative Inputs

Wrong: assuming `x/4` compiled to `x>>2` always produces the same result as `/4` for negative `x`.

What happens: arithmetic right shift rounds toward negative infinity; integer division rounds toward zero. For `x = -7`: `-7/4 = -1` (toward zero) but `-7>>2 = -2` (toward negative infinity). The compiler only substitutes the shift when it can prove `x >= 0` via the `prove` pass, or when the target ABI's signed division instruction produces the same result.

Fix: trust the compiler. If you write `x/4`, the compiled output is always correct. Do not hand-replace `/4` with `>>2` for signed integers — the semantics differ for negative values.

### Assuming Every Dereference After the First Is Nil-Check-Free

Wrong: expecting that `p.Field` after `_ = *p` always removes the nil check in all builds.

What happens: with `-gcflags='-N'`, `nilcheckelim` is disabled and every dereference retains its nil check. Also, if control flow merges between the first dereference and the second (e.g., an intervening function call that could reassign `p`), the prover may not eliminate the check.

Fix: inspect the SSA with `GOSSAFUNC` to confirm elimination. Do not assume; measure.

## Verification

From `~/go-exercises/optpasses`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. To observe optimization decisions:

```bash
# See escape analysis: which allocations go to heap
go build -gcflags='-m' .

# See inlining decisions (more verbose)
go build -gcflags='-m=2' .

# View SSA HTML for a specific function (opens ssa.html in cwd)
GOSSAFUNC=FoldedConstant go build -o /dev/null .

# Compare optimized vs. unoptimized SSA
GOSSAFUNC=Totals go build -o /dev/null .
GOSSAFUNC=Totals go build -gcflags='-N -l' -o /dev/null .
```

## Summary

- The Go compiler SSA pipeline contains roughly 50 named passes grouped into canonicalization, generic optimization, lowering, scheduling, and cleanup phases.
- The `opt` pass applies algebraic rewrite rules; `generic cse` merges redundant computations; `nilcheckelim` and `prove` eliminate redundant nil checks; `lower` performs strength reduction.
- `GOSSAFUNC=<name>` writes `ssa.html` showing every pass column; diff adjacent columns to see what a specific pass did.
- `-gcflags='-N'` disables optimizations and `-gcflags='-l'` disables inlining; use both to see the unoptimized baseline.
- `go build -gcflags='-m'` reports escape analysis decisions; a value that escapes to the heap costs an allocation and GC pressure.
- Write code in forms the compiler can analyze statically: constant expressions, no aliased pointers, concrete types where possible.

## What's Next

Next: [Inlining Heuristics](../03-inlining-heuristics/03-inlining-heuristics.md).

## Resources

- [cmd/compile SSA passes list](https://github.com/golang/go/blob/master/src/cmd/compile/internal/ssa/compile.go) -- the authoritative ordered list of all SSA passes
- [cmd/compile README](https://github.com/golang/go/blob/master/src/cmd/compile/README.md) -- overview of the compiler pipeline phases
- [SSA rewrite rules source](https://github.com/golang/go/tree/master/src/cmd/compile/internal/ssa/_gen) -- the `.rules` files that drive the `opt` pass
- [Go specification: Constant expressions](https://go.dev/ref/spec#Constant_expressions) -- the language-level guarantee that constant expressions are evaluated at compile time
- [pkg.go.dev/cmd/compile](https://pkg.go.dev/cmd/compile) -- compiler flags reference including `-m`, `-N`, `-l`, and `-S`
