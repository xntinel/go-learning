# Exercise 9: No Tail-Call Optimization — Accumulator Recursion vs. the Loop

Two implementations of a ledger-balance / running-checksum reduction over a large
slice — one accumulator-passing "tail recursive", one a plain loop — plus a
benchmark. The point is concrete: Go performs no tail-call optimization, so the
recursive version grows the stack O(n) and can overflow, while the loop is O(1)
stack. This is the measured reason to prefer iteration for linear reductions.

This module is fully self-contained: its own `go mod init`, both sums, the
benchmark, and the recover demonstration inline.

## What you'll build

```text
notco/                     independent module: example.com/notco
  go.mod                   go 1.26
  ledger.go                SumRecursive (accumulator); SumIterative (loop)
  ledger_test.go           equivalence, recover-does-not-catch note, benchmarks
  cmd/
    demo/
      main.go              both sums agree on a large slice
```

- Files: `ledger.go`, `cmd/demo/main.go`, `ledger_test.go`.
- Implement: `SumRecursive([]int64) int64` in accumulator-passing tail style and `SumIterative([]int64) int64` as a `for` loop.
- Test: both agree on random slices; a benchmark comparing them; an informational test showing an ordinary panic is recoverable (to contrast with a stack overflow, which is not).
- Verify: `go test -count=1 -race ./...`

### Why "tail recursion" buys nothing in Go

In languages with tail-call optimization (Scheme, and with caveats some others),
a function whose recursive call is the last thing it does — passing an accumulator
so there is nothing to do after the call returns — is compiled into a loop that
reuses a single stack frame. `sumAcc(rest, acc+x)` there runs in O(1) stack.

Go does not do this. There is no tail-call optimization in the gc compiler, so
`SumRecursive` allocates one stack frame per element regardless of the accumulator
style. Its stack usage is O(n) in the length of the slice, exactly like naive
non-tail recursion. On a large enough input it grows the goroutine stack until it
hits the 1 GiB cap and the process aborts with `runtime: goroutine stack exceeds
... limit` — a fatal abort that `recover` cannot catch. The accumulator gives you
readable code and nothing else; it does not make the recursion safe.

`SumIterative` is the same reduction as a `for` loop. It uses O(1) stack — one
frame, one accumulator variable, no matter how long the slice — and cannot
overflow. For a linear reduction over a slice this is not a style preference; it is
the correct choice, because the recursive form has a failure mode (stack exhaustion
on large input) that the loop simply does not have. The rule generalizes: reserve
recursion for genuinely branching structure, where depth is the tree height rather
than the element count, and use loops for linear folds.

Both functions compute the identical result on any input, which the equivalence
test pins down. The demo runs both on a 50,000-element slice — deep enough to make
the recursion grow the stack visibly, but well within the cap so it completes — and
confirms they agree. The stack-overflow claim is left as prose and an informational
test rather than an executed crash, because actually triggering it would kill the
test process; the honest way to observe it is to lower `runtime/debug.SetMaxStack`
in a throwaway program and watch `SumRecursive` abort while `SumIterative` keeps
running.

Create `ledger.go`:

```go
package ledger

// SumRecursive reduces nums in accumulator-passing "tail recursive" style. Despite
// the tail form, Go performs no tail-call optimization: this allocates one stack
// frame per element (O(n) stack) and can overflow on a large slice.
func SumRecursive(nums []int64) int64 {
	return sumAcc(nums, 0)
}

func sumAcc(nums []int64, acc int64) int64 {
	if len(nums) == 0 {
		return acc
	}
	return sumAcc(nums[1:], acc+nums[0])
}

// SumIterative reduces nums with a loop: O(1) stack, cannot overflow. This is the
// correct form for a linear reduction.
func SumIterative(nums []int64) int64 {
	var acc int64
	for _, n := range nums {
		acc += n
	}
	return acc
}
```

### The runnable demo

The demo builds a 50,000-entry ledger of amounts, sums it both ways, and confirms
the results agree and match an independently computed total.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/notco"
)

func main() {
	const n = 50_000
	nums := make([]int64, n)
	var want int64
	for i := range n {
		nums[i] = int64(i)
		want += int64(i)
	}

	rec := ledger.SumRecursive(nums)
	itr := ledger.SumIterative(nums)

	fmt.Printf("recursive: %d\n", rec)
	fmt.Printf("iterative: %d\n", itr)
	fmt.Printf("agree: %v\n", rec == itr && rec == want)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
recursive: 1249975000
iterative: 1249975000
agree: true
```

### Tests

`TestEquivalence` checks both functions return the same result on random slices
of varying length, proving they are the same reduction. `TestRecoverDoesNotCatchStackOverflow`
is informational: it shows that `recover` *does* catch an ordinary panic, then
documents in a comment why it would *not* catch a stack overflow (which is why the
test does not trigger one). The benchmarks compare the two implementations; run
them with `go test -bench=.`.

Create `ledger_test.go`:

```go
package ledger

import (
	"fmt"
	"math/rand"
	"testing"
)

func TestEquivalence(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 1, 2, 10, 1000, 5000} {
		nums := make([]int64, n)
		for i := range nums {
			nums[i] = rand.Int63n(1000)
		}
		rec := SumRecursive(nums)
		itr := SumIterative(nums)
		if rec != itr {
			t.Fatalf("n=%d: SumRecursive=%d, SumIterative=%d", n, rec, itr)
		}
	}
}

func TestRecoverDoesNotCatchStackOverflow(t *testing.T) {
	t.Parallel()

	// recover CAN intercept an ordinary panic:
	got := catchPanic(func() { panic("boom") })
	if got == nil {
		t.Fatal("expected to recover an ordinary panic")
	}
	// A stack overflow is different: "runtime: goroutine stack exceeds ... limit"
	// is a FATAL runtime abort, not a panic, so a deferred recover never runs. We
	// do NOT trigger one here because it would kill the test process. The only
	// defense against runaway recursion is to bound depth up front, never to
	// recover after the fact. SumIterative sidesteps the whole failure mode.
}

func catchPanic(f func()) (recovered any) {
	defer func() { recovered = recover() }()
	f()
	return nil
}

func BenchmarkSumRecursive(b *testing.B) {
	nums := makeNums(4096)
	b.ResetTimer()
	for range b.N {
		_ = SumRecursive(nums)
	}
}

func BenchmarkSumIterative(b *testing.B) {
	nums := makeNums(4096)
	b.ResetTimer()
	for range b.N {
		_ = SumIterative(nums)
	}
}

func makeNums(n int) []int64 {
	nums := make([]int64, n)
	for i := range nums {
		nums[i] = int64(i)
	}
	return nums
}

func ExampleSumIterative() {
	fmt.Println(SumIterative([]int64{10, 20, 30}))
	// Output: 60
}
```

## Review

The two functions are the same reduction — `TestEquivalence` proves it across
lengths — so the difference is entirely in stack cost, not result. That is the
lesson: an accumulator-passing "tail recursive" `SumRecursive` looks like it should
be optimized into a loop, but Go has no tail-call optimization, so it consumes O(n)
stack and can abort the process on a large slice, while `SumIterative` uses O(1)
stack and cannot. `TestRecoverDoesNotCatchStackOverflow` makes the second half
concrete: `recover` catches an ordinary panic but not a stack overflow, which is a
fatal abort — so the defense against runaway recursion is bounding depth up front,
never recovering after. The takeaway a senior engineer carries out: use loops for
linear folds, reserve recursion for branching structure where the depth is the
tree height, not the element count.

## Resources

- [Go Specification: Function declarations (no TCO guarantee)](https://go.dev/ref/spec#Function_declarations)
- [runtime/debug.SetMaxStack](https://pkg.go.dev/runtime/debug#SetMaxStack)
- [testing package (benchmarks, testing.B)](https://pkg.go.dev/testing#hdr-Benchmarks)
- [Effective Go: for and range](https://go.dev/doc/effective_go#for)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-generic-tree-fold.md](08-generic-tree-fold.md) | Next: [10-category-tree-breadcrumb-flatten.md](10-category-tree-breadcrumb-flatten.md)
