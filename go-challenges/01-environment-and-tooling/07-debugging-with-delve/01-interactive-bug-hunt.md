# Exercise 1: Hunt an Off-by-One Under Delve

The fastest way to learn a debugger is to point it at a real defect. This module
builds a summing library with a classic off-by-one, reproduces the wrong answer,
then uses an interactive `dlv debug` session to inspect the loop variables and
locate the bug before fixing it with a range loop and proving the fix with a
race-clean test.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test. Nothing here imports another exercise.

## What you'll build

```text
bughunt/                   independent module: example.com/bughunt
  go.mod                   go 1.24
  sum/
    sum.go                 func Total([]int) int  (range-based, correct)
  cmd/
    demo/
      main.go              parses args, prints sum(...) = N
  sum/sum_test.go          table-driven TestTotal + Example
```

- Files: `sum/sum.go`, `cmd/demo/main.go`, `sum/sum_test.go`.
- Implement: `Total(nums []int) int` that sums every element (no off-by-one).
- Test: table-driven `TestTotal` (empty, single, five, negative) plus an `Example`.
- Verify: `go test -count=1 -race ./...`, then a scripted `dlv` session asserting `total = 150`.

Set up the module:

```bash
go mod edit -go=1.24
```

### The bug, and how the debugger reveals it

Start from the version a tired engineer actually writes. The loop bound reads
`i < len(nums)-1`, which stops one element short so the last number is never
added. This block is illustrative — do not save it as the package file yet:

```go
package sum

// BUGGY: the loop bound skips the final element.
func Total(nums []int) int {
	total := 0
	for i := 0; i < len(nums)-1; i++ {
		total += nums[i]
	}
	return total
}
```

For `{10, 20, 30, 40, 50}` this returns `100`, not `150`. A print statement would
tell you the answer is wrong but not why. Delve tells you why. Build the buggy
binary under the debugger — `dlv debug` injects `-gcflags='all=-N -l'` so every
variable is visible — and pass the program's arguments after the `--` separator:

```bash
dlv debug ./cmd/demo -- 10 20 30 40 50
```

Inside the REPL, set a breakpoint on the accumulation line and run to it. The
address and package path in the output depend on your build; the values are what
matter:

```text
(dlv) break sum/sum.go:7
Breakpoint 1 set at 0x... for example.com/bughunt/sum.Total() ./sum/sum.go:7
(dlv) continue
> example.com/bughunt/sum.Total() ./sum/sum.go:7 (hits goroutine(1):1 total:1)
(dlv) print nums
[]int len: 5, cap: 5, [10,20,30,40,50]
(dlv) print len(nums)
5
```

Now watch the loop bound. Step across iterations with `next`, printing `i` and
`total` each time, and note the last index the loop ever reaches:

```text
(dlv) print i
0
(dlv) continue
> ... ./sum/sum.go:7 (hits goroutine(1):4 total:4)
(dlv) print i
3
(dlv) print total
60
```

The breakpoint fires four times, never five, and `i` tops out at `3` — index `4`,
holding `50`, is never visited. The condition `i < len(nums)-1` is the culprit.
That is the whole diagnosis: the debugger showed you the loop stopping one
iteration early, in the running program, without a single added print.

### The fix

A range loop cannot express this off-by-one, which is why it is the idiomatic
form. Save the corrected package file:

Create `sum/sum.go`:

```go
package sum

// Total returns the sum of every element in nums. Using range over the slice
// removes any chance of an off-by-one on the loop bound.
func Total(nums []int) int {
	total := 0
	for _, n := range nums {
		total += n
	}
	return total
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"strconv"

	"example.com/bughunt/sum"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: demo N1 [N2 ...]")
		os.Exit(2)
	}

	nums := make([]int, 0, len(os.Args)-1)
	for _, a := range os.Args[1:] {
		n, err := strconv.Atoi(a)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		nums = append(nums, n)
	}

	fmt.Printf("sum(%v) = %d\n", nums, sum.Total(nums))
}
```

Run it:

```bash
go run ./cmd/demo 10 20 30 40 50
```

Expected output:

```text
sum([10 20 30 40 50]) = 150
```

### Prove the fix with a test

The test is the durable version of the debugging session: it pins the contract so
the off-by-one can never come back. The `five` case is the one that failed on the
buggy version.

Create `sum/sum_test.go`:

```go
package sum

import (
	"fmt"
	"testing"
)

func TestTotal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []int
		want int
	}{
		{name: "empty", in: []int{}, want: 0},
		{name: "single", in: []int{42}, want: 42},
		{name: "five", in: []int{10, 20, 30, 40, 50}, want: 150},
		{name: "negative", in: []int{-1, -2, -3}, want: -6},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Total(tc.in); got != tc.want {
				t.Fatalf("Total(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func ExampleTotal() {
	fmt.Println(Total([]int{10, 20, 30, 40, 50}))
	// Output: 150
}
```

Run the gate:

```bash
gofmt -l .
go vet ./...
go build ./...
go test -count=1 -race ./...
```

### Make the debugger output a CI artifact

The same session runs non-interactively. Build with `-N -l` so `dlv exec` sees
every variable, then replay a script that stops at the return and prints `total`:

```bash
go build -gcflags='all=-N -l' -o /tmp/bughunt ./cmd/demo

cat > /tmp/bughunt.dlv <<'EOF'
break sum/sum.go:10
continue
print total
quit
EOF

dlv exec /tmp/bughunt --init /tmp/bughunt.dlv -- 10 20 30 40 50 2>&1 | tee /tmp/bughunt.out
grep -q 'total = 150' /tmp/bughunt.out && echo OK
```

Line 10 is the `return total` line in the fixed file; stopping there and printing
`total` proves the loop summed all five elements. The `grep` turns the debugger's
output into a pass/fail signal a pipeline can assert on.

## Review

The library is correct when `Total` is a pure fold over the slice: it visits
every index exactly once and adds nothing else. The debugging proof is that the
breakpoint on the accumulation line fires as many times as there are elements —
five for the fixed loop, four for the buggy one — which is the observable
signature of the off-by-one. If you build the binary yourself for `dlv exec` and
forget `-gcflags='all=-N -l'`, `print total` may report `<optimized out>`; that
is not a bug in your code, it is the optimizer eliding the variable, and the flags
restore it. Do not forget the `--` before the program's arguments, or Delve
parses `10 20 30 40 50` as its own flags and fails to start.

## Resources

- [Delve CLI command reference](https://github.com/go-delve/delve/blob/master/Documentation/cli/README.md) — every REPL command, including `break`, `continue`, `next`, `print`.
- [`dlv debug` usage](https://github.com/go-delve/delve/blob/master/Documentation/usage/dlv_debug.md) — flags, the `--` separator, and how it injects `-N -l`.
- [Go slices: usage and internals](https://go.dev/blog/slices-intro) — the slice header (len/cap/backing array) Delve prints for you.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-conditional-breakpoints.md](02-conditional-breakpoints.md)
