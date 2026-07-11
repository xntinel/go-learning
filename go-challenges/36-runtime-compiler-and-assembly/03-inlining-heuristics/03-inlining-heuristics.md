# 3. Inlining Heuristics

Go's inliner replaces a call site with the body of the callee. The hard part is that the decision is made at compile time using a cost model — not by measuring actual runtime frequency. Getting the cost model wrong means writing functions that are larger than the budget, inadvertently preventing optimizations that compound (escape analysis, constant folding) downstream. This lesson teaches you to read inlining decisions from the compiler, understand the budget arithmetic, and write functions that stay inlineable while remaining readable.

```text
inlheur/
  go.mod
  math.go
  math_test.go
  cmd/demo/main.go
```

## Concepts

### The Cost Model and the 80-Node Budget

The `gc` compiler assigns each function a cost measured in abstract "AST nodes". A function whose cost stays below the **inline budget of 80** is eligible for inlining at its call sites; one that exceeds it is not. The cost is not a line count or a byte count — it is closer to an operation count: each statement, sub-expression, and language construct increments the counter by a fixed amount.

The key numbers from the compiler source (`cmd/compile/internal/inline`):

| Construct | Approximate cost |
|-----------|-----------------|
| Simple expression or assignment | 1 |
| Call to a non-inlinable function | 57 |
| Call through a function parameter | 17 |
| Closure literal | 15 (deducted from budget) |
| `panic` | 1 (panic path is treated as cold) |
| Loop body | scales with body size |

A call to a function that is itself **not** inlinable costs 57 nodes (`inlineExtraCallCost`). That leaves only 23 nodes of budget for everything else if the function makes one such call. However, when the callee **is** inlinable, mid-stack inlining substitutes the cheaper inlined body rather than charging 57 nodes — which is why `Quadruple`, which calls `double` twice, reports a cost of 14, not 114.

### What the `-m` Flag Shows

`-gcflags='-m'` asks the compiler to print its optimization decisions. The relevant lines for inlining:

- `can inline f` — `f` is under budget and will be inlined wherever it is called from the same compilation unit.
- `inlining call to f` — the compiler substituted `f`'s body at this call site.
- `cannot inline f: <reason>` — `f` exceeded the budget or hit a hard disqualifier.

Adding a second `-m` (`-gcflags='-m -m'`) produces the per-function cost:

```
./math.go:12:6: can inline add with cost 4 as:
```

That cost number is the one being compared against 80.

### Hard Disqualifiers

Some constructs unconditionally prevent inlining regardless of the node count:

- `select` statement
- `go` statement (goroutine spawn)
- `defer` statement (before Go 1.22 this was always a hard stop; from 1.22, open-coded defer can be inlined)
- `recover()` call
- `//go:noinline` directive on the function

These are not "expensive" in the node-count sense — they are structurally forbidden by the inliner's implementation at the time this corpus was written.

### Mid-Stack Inlining (Go 1.12+)

Before Go 1.12 only leaf functions (functions that make no calls) were inlined. Since 1.12 the compiler can inline a function that itself calls other functions, as long as the total cost of the inlining chain stays within budget. This is called mid-stack inlining.

Practical effect: a small wrapper that calls a small helper can be fully collapsed at the call site. For example, `quadruple -> double -> double` can be flattened into a single multiply.

### Why Inlining Matters Beyond Call Overhead

The call overhead (argument marshaling, stack frame setup, return) is the least important effect. The more important effect is that inlining exposes callee code to the caller's optimizer:

1. **Escape analysis**: a pointer returned from an inlined function can be proven not to escape and therefore placed on the stack instead of the heap. Without inlining, the compiler sees only the function signature and must assume the pointer escapes.
2. **Constant propagation**: if the inlined body receives a known constant, the compiler can evaluate branches at compile time and eliminate dead code.
3. **Bounds check elimination**: after inlining, a bounds check that was inside the callee can be seen in context and eliminated if the caller already checked the index.

### `//go:noinline` and When to Use It

`//go:noinline` is a compiler directive (not a build constraint; it must appear on the line immediately before the `func` keyword). It tells the compiler not to inline that function regardless of cost.

Use cases:

- Benchmarking: prevent the inliner from eliminating a function you are measuring.
- Debugging: keep a function visible in profiles and stack traces.
- Intentional slowdown: keep a hot path visible in pprof so you can find it.

Do not use `//go:noinline` as a general "this function is complex" marker. The compiler's cost model already handles that.

### PGO and Hot-Function Inlining

Profile-guided optimization (Go 1.21+) can inline functions that exceed the static 80-node budget if profiling shows the call is hot. The budget boost for PGO-hot functions can reach 2000 nodes. This makes PGO particularly powerful for medium-sized functions that the static heuristic conservatively excludes.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/inlheur/cmd/demo
cd ~/go-exercises/inlheur
go mod init example.com/inlheur
```

This is a library (`package inlheur`), not a program. You verify it with `go test`.

### Exercise 1: Functions Across the Budget Boundary

Create `math.go`:

```go
// math.go
package inlheur

// add is a trivial leaf function. Cost: ~4 nodes. Well under budget.
func add(a, b int) int {
	return a + b
}

// clamp has branches but no calls. Cost: ~10 nodes. Still under budget.
func clamp(val, lo, hi int) int {
	if val < lo {
		return lo
	}
	if val > hi {
		return hi
	}
	return val
}

// double is a leaf helper used by Quadruple.
func double(x int) int {
	return x * 2
}

// Quadruple calls double twice. Because double is itself inlineable,
// mid-stack inlining can flatten this into a single multiply.
// Exported so cmd/demo can call it.
func Quadruple(x int) int {
	return double(double(x))
}

// sumSlice contains a range loop over a slice. The loop body is small,
// but loops add to the cost. Verify whether this crosses 80 with -gcflags='-m -m'.
func sumSlice(nums []int) int {
	s := 0
	for _, v := range nums {
		s += v
	}
	return s
}

// Clamp is the exported wrapper so cmd/demo and external tests can call it.
func Clamp(val, lo, hi int) int {
	return clamp(val, lo, hi)
}

// SumSlice is the exported form of sumSlice.
func SumSlice(nums []int) int {
	return sumSlice(nums)
}
```

Check inlining decisions without running the full test suite yet:

```bash
go build -gcflags='-m' ./...
go build -gcflags='-m -m' ./... 2>&1 | grep -E 'can inline|cannot inline|cost'
```

You should see `can inline add`, `can inline clamp`, `can inline double`, and `can inline Quadruple`. Look at the reported cost for `sumSlice` — it will be close to or above the budget depending on your Go version.

### Exercise 2: The `//go:noinline` Directive and What It Costs

Append to `math.go`:

```go
// AddNoInline is identical to add but explicitly prevents inlining.
// Use this to benchmark the call overhead that inlining eliminates.
//
//go:noinline
func AddNoInline(a, b int) int {
	return a + b
}
```

Build again and confirm the compiler reports `cannot inline AddNoInline: marked go:noinline`. This message requires two `-m` flags:

```bash
go build -gcflags='-m -m' ./... 2>&1 | grep 'cannot inline'
```

### Exercise 3: Tests, Benchmarks, and an Example

Create `math_test.go`:

```go
// math_test.go
package inlheur

import (
	"fmt"
	"testing"
)

func TestAdd(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b, want int
	}{
		{1, 2, 3},
		{0, 0, 0},
		{-1, 1, 0},
		{100, -50, 50},
	}
	for _, tc := range cases {
		got := add(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("add(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestClamp(t *testing.T) {
	t.Parallel()

	cases := []struct {
		val, lo, hi, want int
	}{
		{5, 0, 10, 5},   // in range
		{-3, 0, 10, 0},  // below lo
		{15, 0, 10, 10}, // above hi
		{0, 0, 0, 0},    // lo == hi
	}
	for _, tc := range cases {
		got := clamp(tc.val, tc.lo, tc.hi)
		if got != tc.want {
			t.Errorf("clamp(%d, %d, %d) = %d, want %d", tc.val, tc.lo, tc.hi, got, tc.want)
		}
	}
}

func TestQuadruple(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in, want int
	}{
		{1, 4},
		{0, 0},
		{-3, -12},
		{7, 28},
	}
	for _, tc := range cases {
		got := Quadruple(tc.in)
		if got != tc.want {
			t.Errorf("Quadruple(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSumSlice(t *testing.T) {
	t.Parallel()

	cases := []struct {
		nums []int
		want int
	}{
		{[]int{1, 2, 3, 4}, 10},
		{[]int{}, 0},
		{[]int{-1, 1}, 0},
		{[]int{100}, 100},
	}
	for _, tc := range cases {
		got := sumSlice(tc.nums)
		if got != tc.want {
			t.Errorf("sumSlice(%v) = %d, want %d", tc.nums, got, tc.want)
		}
	}
}

// ExampleQuadruple documents the observable behavior and is verified by go test.
func ExampleQuadruple() {
	fmt.Println(Quadruple(5))
	// Output: 20
}

// BenchmarkAdd measures the inlined add path.
func BenchmarkAdd(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = add(i, i+1)
	}
}

// BenchmarkAddNoInline measures the same arithmetic through a non-inlined call.
// The difference shows the real call overhead on your machine.
func BenchmarkAddNoInline(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = AddNoInline(i, i+1)
	}
}
```

Your turn: add `TestDouble` that checks `double(0) == 0`, `double(5) == 10`, and `double(-3) == -6`.

### Exercise 4: The Demo Program

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/inlheur"
)

func main() {
	fmt.Println("Quadruple(7) =", inlheur.Quadruple(7))
	fmt.Println("Clamp(15, 0, 10) =", inlheur.Clamp(15, 0, 10))
	fmt.Println("SumSlice([1 2 3 4 5]) =", inlheur.SumSlice([]int{1, 2, 3, 4, 5}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Quadruple(7) = 28
Clamp(15, 0, 10) = 10
SumSlice([1 2 3 4 5]) = 15
```

## Common Mistakes

### Relying on line count to predict inlining

Wrong: "This function is only 5 lines, so it will inline."

What happens: a 5-line function with one call to a non-inlinable function starts at cost 57 (`inlineExtraCallCost`) before counting the rest of the body. If those 5 lines hold two such calls, the cost is already 114 — over budget. Note that this penalty applies only when the callee is itself not inlinable; when both the wrapper and the callee are under budget, mid-stack inlining substitutes the callee's actual (cheaper) body, as demonstrated by `Quadruple` above.

Fix: always check with `-gcflags='-m -m'` and look at the reported cost. Line count is not the cost model.

### Using `//go:noinline` to "mark" complex functions

Wrong: adding `//go:noinline` to a function that already exceeds 80 nodes, thinking this is explicit documentation.

What happens: nothing changes for that function (it was already not inlined), but you add a directive that confuses readers and prevents the inliner from inlining the function even if a future Go version raises the budget or the function shrinks.

Fix: leave the directive off unless you have a concrete reason (benchmarking, debugging, keeping a symbol visible in profiles).

### Expecting `defer` to inline (before Go 1.22)

Wrong: a function with `defer` is expected to inline because it is small.

What happens: before Go 1.22, any `defer` unconditionally prevented inlining. The compiler reports `cannot inline: has defer`. From Go 1.22, open-coded defers can be inlined, but not all defers qualify.

Fix: check your Go version. Use `-gcflags='-m'` to see the actual decision.

### Forgetting that inlining is a module-compilation decision

Wrong: expecting a function in package `foo` to be inlined when called from package `bar` in a different module, compiled separately.

What happens: the inliner operates per-compilation-unit. For cross-package inlining to work, both packages must be compiled together (e.g., `go build ./...` from a workspace). Separately-compiled archives do not expose the function body to the caller.

Fix: the standard `go build` and `go test` commands compile dependency chains together, so cross-package inlining works. The mistake only appears in unusual build systems that compile packages independently.

## Verification

From `~/go-exercises/inlheur`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must exit cleanly. Then inspect the inlining report:

```bash
go build -gcflags='-m -m' ./... 2>&1 | grep -E 'can inline|cannot inline'
go build -gcflags='-m -m' ./... 2>&1 | grep 'cost'
```

Confirm that `add`, `clamp`, `double`, and `Quadruple` all appear in the "can inline" list, and that `AddNoInline` appears in the "cannot inline" list with the reason "marked go:noinline". The `cannot inline` / `marked go:noinline` diagnostic only appears under `-gcflags='-m -m'`; a single `-m` does not emit it.

Run the benchmark comparison:

```bash
go test -bench=BenchmarkAdd -benchmem -count=5 ./...
```

On most hardware you will see `BenchmarkAddNoInline` 2-5x slower than `BenchmarkAdd` for this trivial case. The gap is the raw call overhead the inliner eliminates.

## Summary

- The inlining budget is 80 AST nodes; a function above this threshold is not inlined at static call sites.
- A call to a non-inlinable function costs approximately 57 nodes (`inlineExtraCallCost`), leaving only 23 for everything else in a one-call wrapper. When the callee is itself inlinable, mid-stack inlining substitutes the cheaper inlined body instead of charging 57 nodes.
- `-gcflags='-m'` prints "can inline" and "inlining call to" messages; `-gcflags='-m -m'` adds the per-function cost.
- Hard disqualifiers: `select`, `go`, `recover`, and (before Go 1.22) `defer`. These prevent inlining regardless of node count.
- Mid-stack inlining (Go 1.12+) allows non-leaf functions to be inlined when the callee chain is itself inlineable.
- The most important effect of inlining is not call-overhead elimination but the downstream optimizations it unlocks: escape analysis, constant propagation, and bounds-check elimination.
- `//go:noinline` has legitimate uses in benchmarking and profiling but should not be used as documentation of complexity.
- PGO (Go 1.21+) can override the static budget for functions identified as hot by a CPU profile.

## What's Next

Next: [Bounds Check Elimination](../04-bounds-check-elimination/04-bounds-check-elimination.md).

## Resources

- [Go Compiler Optimizations wiki — Function Inlining](https://go.dev/wiki/CompilerOptimizations#function-inlining)
- [cmd/compile package documentation](https://pkg.go.dev/cmd/compile) — covers `-m`, `-l`, `-N` flags and `//go:noinline`
- [Mid-stack inlining design document](https://github.com/golang/proposal/blob/master/design/19348-midstack-inlining.md)
- [Profile-Guided Optimization in Go 1.21](https://go.dev/blog/pgo) — explains PGO-driven inlining and its effect on escape analysis
- [cmd/compile/internal/inline source](https://github.com/golang/go/tree/master/src/cmd/compile/internal/inline) — authoritative budget constants and heuristic logic
