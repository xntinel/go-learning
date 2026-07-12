# 4. Bounds Check Elimination

The Go runtime inserts a bounds check before every slice and array index operation. Each check is a compare-and-branch that panics with an index-out-of-range error if the index is invalid. This is correct behavior — buffer overflows cause undefined behavior in C and silent data corruption in languages without memory safety — but in hot loops the overhead is measurable. The compiler's `prove` SSA pass eliminates checks it can prove are unreachable at compile time. This lesson shows which patterns get automatic elimination, which need a hint, and how to confirm the result with compiler diagnostics.

```text
bce/
  go.mod
  bce.go
  bce_test.go
  cmd/demo/main.go
```

## Concepts

### What Bounds Checks Cost

A bounds check compiles to a compare and a conditional branch. On modern pipelined CPUs the branch is well-predicted in steady-state loops, but it still occupies issue slots and retirement budget. In inner loops that process large arrays — image decoders, hash functions, protocol parsers — eliminating redundant checks can yield a 5-20% speedup measurable with `go test -bench`.

The check itself looks like this in the generated machine code (amd64): a `CMP` of the index against the length stored in the slice header, followed by a `JBE` to a `CALL runtime.panicIndex` path. The `prove` pass works on SSA form before code generation; if it can derive a contradiction that makes the panic branch unreachable, it removes the branch entirely.

### The Prove Pass

The `prove` SSA pass (source: `src/cmd/compile/internal/ssa/prove.go`) maintains a set of facts about integer relationships. It starts from dominance edges — if a block is dominated by an `if i < len(s)` guard, every block under it inherits the fact `i < len(s)`. It propagates those facts through integer arithmetic and derives new inequalities. A check `i >= 0 && i < len(s)` that follows from existing facts is proved redundant and removed.

Facts come from four sources:

1. The loop induction variable. In `for i := 0; i < len(s); i++`, the compiler knows `i >= 0` (unsigned comparison) and `i < len(s)` at the access site.
2. Guard clauses. `if len(s) < 4 { return }` adds the fact `len(s) >= 4` to all dominated blocks.
3. Re-slicing. `s = s[:4]` changes the slice header so the compiler sees `len(s) == 4`.
4. The three-index slice form. `s = s[i : i+4 : i+4]` proves `len(s) == cap(s) == 4` tightly.

### Diagnostic Flag

```bash
go build -gcflags='-d=ssa/check_bce/debug=1' ./...
```

This flag makes the compiler print a line for every index operation that still has a bounds check after the prove pass. No output means all checks were eliminated. Output like `Found IsInBounds` or `Found IsSliceInBounds` names the file and line of the surviving check.

### Patterns That Get Automatic BCE

Standard ascending and descending loops are proved automatically:

```go
for i := 0; i < len(s); i++ { _ = s[i] }    // BCE: i < len(s) is loop guard
for i := range s { _ = s[i] }               // BCE: same fact from range
for i := len(s) - 1; i >= 0; i-- { _ = s[i] } // BCE: i < len(s) provable
```

### Patterns That Need a Hint

Multi-element access inside a loop needs the compiler to know that `i+3 < len(s)`. The natural loop guard `i < len(s)` does not prove that, and neither does `i < len(s)-3` — under Go 1.24 the prove pass only eliminates the s[i] check with that guard; s[i+1], s[i+2], and s[i+3] retain checks. Two patterns that actually eliminate all four checks:

**Reslice the window inside the loop:**

```go
// w := s[i : i+4 : i+4] sets len(w) == cap(w) == 4 exactly.
// The prove pass sees len(w) == 4 and eliminates all four checks on w[0..3].
// The loop guard i+4 <= len(s) keeps the reslice expression safe.
i := 0
for ; i+4 <= len(s); i += 4 {
	w := s[i : i+4 : i+4]
	_ = w[0] + w[1] + w[2] + w[3] // BCE: all four checks eliminated
}
```

**Re-slicing a fixed-size chunk:**

```go
if len(s) < 4 {
	return 0
}
s = s[:4] // len(s) == 4 exactly; all four accesses proved safe
return s[0] + s[1] + s[2] + s[3]
```

**Three-index slice for a single window:**

```go
// s[i:i+4:i+4] sets len and cap both to 4; compiler sees len == cap == 4
w := s[i : i+4 : i+4]
return w[0] + w[1] + w[2] + w[3]
```

### Trade-offs and Failure Modes

BCE is a compiler optimization, not a contract. It can regress silently across compiler versions if the prove pass changes. Verify with the diagnostic flag after updating Go. Do not rely on BCE for correctness — rely on it only for performance. If a loop is safety-critical, a guard clause both helps the compiler and makes the invariant explicit in the code, which is a win either way.

Generic functions instantiated with a concrete type may or may not get BCE depending on whether the compiler specializes the body. Modulo-indexed access (`s[i%n]`) is not proved safe because the compiler does not model integer division precisely.

## Exercises

### Exercise 1: Implement the BCE Showcase Functions

Create `bce.go`:

```go
package bce

// SumAscending sums a slice with a plain ascending loop.
// The loop guard `i < len(s)` gives the compiler the fact it needs
// to eliminate the bounds check on s[i].
func SumAscending(s []int) int {
	total := 0
	for i := 0; i < len(s); i++ {
		total += s[i]
	}
	return total
}

// SumDescending sums a slice with a descending loop.
// The compiler proves i < len(s) because i starts at len(s)-1 and
// only decreases, so the check on s[i] is eliminated.
func SumDescending(s []int) int {
	total := 0
	for i := len(s) - 1; i >= 0; i-- {
		total += s[i]
	}
	return total
}

// SumRange uses a range loop, which gives the same BCE guarantee
// as the ascending loop.
func SumRange(s []int) int {
	total := 0
	for i := range s {
		total += s[i]
	}
	return total
}

// SumStridedNaive accesses four elements per iteration without a guard.
// The compiler cannot prove i+3 < len(s) from the loop condition alone,
// so bounds checks survive on all four accesses s[i], s[i+1], s[i+2], and s[i+3].
func SumStridedNaive(s []int) int {
	total := 0
	for i := 0; i+4 <= len(s); i += 4 {
		total += s[i] + s[i+1] + s[i+2] + s[i+3]
	}
	return total
}

// SumStridedBCE uses the three-index reslice form inside the loop to give
// the compiler a tight length fact. w := s[i : i+4 : i+4] sets len(w) == 4
// exactly; the prove pass eliminates all four bounds checks on w[0]..w[3].
// The guard i+4 <= len(s) keeps the reslice safe and is checked once per
// iteration. The tail loop handles 0-3 remaining elements.
func SumStridedBCE(s []int) int {
	total := 0
	i := 0
	for ; i+4 <= len(s); i += 4 {
		w := s[i : i+4 : i+4]
		total += w[0] + w[1] + w[2] + w[3]
	}
	// handle the tail (0-3 remaining elements)
	for ; i < len(s); i++ {
		total += s[i]
	}
	return total
}

// ProcessQuad sums exactly four elements. Re-slicing to length 4 gives
// the compiler a tight bound and eliminates all four checks.
func ProcessQuad(s []int) int {
	if len(s) < 4 {
		return 0
	}
	s = s[:4]
	return s[0] + s[1] + s[2] + s[3]
}

// WindowBCE uses the three-index slice form to prove that a four-element
// window is safe. len(w) == cap(w) == 4 after the reslice.
func WindowBCE(s []int, i int) int {
	if i < 0 || i+4 > len(s) {
		return 0
	}
	w := s[i : i+4 : i+4]
	return w[0] + w[1] + w[2] + w[3]
}
```

### Exercise 2: Test the Functions

Create `bce_test.go`:

```go
package bce

import (
	"testing"
)

var testCases = []struct {
	name string
	s    []int
	want int
}{
	{"empty", []int{}, 0},
	{"one", []int{7}, 7},
	{"two", []int{3, 5}, 8},
	{"four", []int{1, 2, 3, 4}, 10},
	{"five", []int{1, 2, 3, 4, 5}, 15},
	{"eight", []int{1, 2, 3, 4, 5, 6, 7, 8}, 36},
}

func TestSumAscending(t *testing.T) {
	t.Parallel()
	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SumAscending(tc.s); got != tc.want {
				t.Errorf("SumAscending(%v) = %d, want %d", tc.s, got, tc.want)
			}
		})
	}
}

func TestSumDescending(t *testing.T) {
	t.Parallel()
	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SumDescending(tc.s); got != tc.want {
				t.Errorf("SumDescending(%v) = %d, want %d", tc.s, got, tc.want)
			}
		})
	}
}

func TestSumRange(t *testing.T) {
	t.Parallel()
	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SumRange(tc.s); got != tc.want {
				t.Errorf("SumRange(%v) = %d, want %d", tc.s, got, tc.want)
			}
		})
	}
}

func TestSumStridedNaive(t *testing.T) {
	t.Parallel()
	// strided only handles multiples of 4; check those
	cases := []struct {
		s    []int
		want int
	}{
		{[]int{}, 0},
		{[]int{1, 2, 3, 4}, 10},
		{[]int{1, 2, 3, 4, 5, 6, 7, 8}, 36},
	}
	for _, tc := range cases {
		tc := tc
		t.Run("", func(t *testing.T) {
			t.Parallel()
			if got := SumStridedNaive(tc.s); got != tc.want {
				t.Errorf("SumStridedNaive(%v) = %d, want %d", tc.s, got, tc.want)
			}
		})
	}
}

func TestSumStridedBCE(t *testing.T) {
	t.Parallel()
	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SumStridedBCE(tc.s); got != tc.want {
				t.Errorf("SumStridedBCE(%v) = %d, want %d", tc.s, got, tc.want)
			}
		})
	}
}

func TestProcessQuad(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    []int
		want int
	}{
		{[]int{}, 0},
		{[]int{1, 2, 3}, 0}, // fewer than 4 elements -> 0
		{[]int{1, 2, 3, 4}, 10},
		{[]int{1, 2, 3, 4, 5}, 10}, // only first four used
	}
	for _, tc := range cases {
		tc := tc
		t.Run("", func(t *testing.T) {
			t.Parallel()
			if got := ProcessQuad(tc.s); got != tc.want {
				t.Errorf("ProcessQuad(%v) = %d, want %d", tc.s, got, tc.want)
			}
		})
	}
}

func TestWindowBCE(t *testing.T) {
	t.Parallel()
	s := []int{10, 20, 30, 40, 50}
	cases := []struct {
		i    int
		want int
	}{
		{0, 100}, // 10+20+30+40
		{1, 140}, // 20+30+40+50
		{2, 0},   // i+4 == 6 > len(s); out of range -> 0
		{-1, 0},  // negative -> 0
	}
	for _, tc := range cases {
		tc := tc
		t.Run("", func(t *testing.T) {
			t.Parallel()
			if got := WindowBCE(s, tc.i); got != tc.want {
				t.Errorf("WindowBCE(s, %d) = %d, want %d", tc.i, got, tc.want)
			}
		})
	}
}

// ExampleSumAscending shows the basic usage and is verified by go test.
func ExampleSumAscending() {
	s := []int{1, 2, 3, 4, 5}
	sum := SumAscending(s)
	_ = sum
	// The sum is: 15
}

// ExampleProcessQuad shows the re-slicing BCE pattern.
func ExampleProcessQuad() {
	s := []int{10, 20, 30, 40, 99}
	result := ProcessQuad(s)
	_ = result
	// ProcessQuad uses only the first four elements: 10+20+30+40 = 100
}

// Your turn: add TestSumAllFunctionsAgree that calls SumAscending,
// SumDescending, SumRange, and SumStridedBCE on the same input and
// asserts all four return the same value.

func BenchmarkSumAscending(b *testing.B) {
	data := make([]int, 1024)
	for i := range data {
		data[i] = i
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_ = SumAscending(data)
	}
}

func BenchmarkSumStridedNaive(b *testing.B) {
	data := make([]int, 1024)
	for i := range data {
		data[i] = i
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_ = SumStridedNaive(data)
	}
}

func BenchmarkSumStridedBCE(b *testing.B) {
	data := make([]int, 1024)
	for i := range data {
		data[i] = i
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_ = SumStridedBCE(data)
	}
}
```

### Exercise 3: Add the Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bce"
)

func main() {
	s := []int{1, 2, 3, 4, 5, 6, 7, 8}

	fmt.Println("SumAscending:", bce.SumAscending(s))
	fmt.Println("SumDescending:", bce.SumDescending(s))
	fmt.Println("SumRange:", bce.SumRange(s))
	fmt.Println("SumStridedNaive:", bce.SumStridedNaive(s))
	fmt.Println("SumStridedBCE:", bce.SumStridedBCE(s))
	fmt.Println("ProcessQuad:", bce.ProcessQuad(s))
	fmt.Println("WindowBCE(s,2):", bce.WindowBCE(s, 2))
}
```

Run the demo:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Assuming the strided loop guard is enough

Wrong: writing `for i := 0; i+4 <= len(s); i += 4 { total += s[i] + s[i+1] + s[i+2] + s[i+3] }` and expecting zero remaining checks. Although `i+4 <= len(s)` implies `i+3 < len(s)`, the prove pass does not propagate this implication to all four sub-accesses in current Go versions. Adding a pre-loop `if len(s) < 4 { ... }` guard does not help either — the strided loop still emits four surviving checks. Confirm with `go build -gcflags='-d=ssa/check_bce/debug=1' ./...`.

Fix: use `SumStridedBCE`'s pattern — reslice the window inside the loop: `w := s[i : i+4 : i+4]; total += w[0]+w[1]+w[2]+w[3]`. Setting `len(w) == 4` exactly gives the prove pass a tight bound that eliminates all four per-element checks. The loop guard `i+4 <= len(s)` keeps the reslice expression safe.

### Using modulo indexing and expecting BCE

Wrong: `s[i % n]` where `n` is a variable. The compiler's prove pass does not model integer division precisely, so the check survives even if the programmer can reason that the result is in bounds.

Fix: avoid modulo-indexed access in performance-critical inner loops. Use a power-of-two mask (`s[i & mask]`) if the size allows it; that is sometimes provable.

### Relying on BCE without checking the diagnostic

Wrong: restructuring a loop "for BCE" without ever running the diagnostic to confirm the checks were actually eliminated.

Fix: run `go build -gcflags='-d=ssa/check_bce/debug=1' ./...` after every change and confirm the target lines no longer appear in the output. BCE can silently regress across Go versions.

### Forgetting the tail after a strided loop

Wrong: `SumStridedNaive` only accumulates elements whose index is a multiple of four. Elements at indices 4, 5, 6 (for a 7-element slice) are silently skipped.

Fix: after the strided loop, add a tail loop: `for i := len(s) &^ 3; i < len(s); i++ { total += s[i] }`. `SumStridedBCE` includes this tail.

## Verification

From `~/go-exercises/bce`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

To observe bounds check elimination diagnostics:

```bash
go build -gcflags='-d=ssa/check_bce/debug=1' ./...
```

Output naming `bce.go` and a line number identifies each surviving check. In this package, `SumStridedNaive` intentionally retains four checks (lines reporting `IsInBounds` at all four sub-accesses s[i], s[i+1], s[i+2], s[i+3]) — that is the expected naive baseline. `SumStridedBCE` uses the reslice pattern `w := s[i : i+4 : i+4]` and should produce no `IsInBounds` output on its w[0]..w[3] accesses; only a single `IsSliceInBounds` for the reslice expression itself may appear, which is expected and harmless. No output on a given function means all its element checks were eliminated.

Run benchmarks to measure the difference:

```bash
go test -bench=. -benchmem -count=5 ./...
```

Compare `BenchmarkSumStridedNaive` vs `BenchmarkSumStridedBCE` on large inputs. The difference is typically small on modern CPUs (branches are predicted well) but grows on data that causes branch mispredictions.

## Summary

- Go inserts a bounds check before every slice and array index to prevent out-of-range panics.
- The `prove` SSA pass eliminates checks it can prove are unreachable from facts derived from the control flow.
- Standard ascending loops, descending loops, and range loops get automatic BCE.
- Multi-element strided access needs a re-slicing hint: `w := s[i : i+4 : i+4]` gives the prove pass a tight length fact and eliminates all four element checks; a plain loop guard or a pre-loop `if len(s) < 4` guard alone is not sufficient under Go 1.24.
- `s = s[:n]` and `s = s[i:i+n:i+n]` give the compiler a tight length fact.
- `go build -gcflags='-d=ssa/check_bce/debug=1' ./...` shows surviving checks.
- Verify after each change and after each Go upgrade; BCE can regress silently.

## What's Next

Next: [PGO: Profile-Guided Optimization](../05-pgo-profile-guided-optimization/05-pgo-profile-guided-optimization.md).

## Resources

- [Bounds Check Elimination — go101.org/optimizations](https://go101.org/optimizations/5-bce.html)
- [SSA prove pass source](https://github.com/golang/go/blob/master/src/cmd/compile/internal/ssa/prove.go)
- [Go Specification: Index expressions](https://go.dev/ref/spec#Index_expressions)
- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions)
- [cmd/compile documentation](https://pkg.go.dev/cmd/compile)
