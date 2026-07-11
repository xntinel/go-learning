# Exercise 5: Debug a Failing Test with dlv test

A test that fails only on one subtest is the ideal reproducer: it already isolates
the input that breaks. `dlv test` compiles the test binary and drops you into a
debugging session on it, so you can break on the failing case, step into the
assertion, and inspect the test-case struct to see exactly why `got` differs from
`want`. This module debugs a boundary bug in a grading function.

This module is fully self-contained: its own `go mod init`, demo, and test.

## What you'll build

```text
gradedbg/                  independent module: example.com/gradedbg
  go.mod                   go 1.24
  grade/
    grade.go               Letter(ctx, score) (string, error)  (correct boundaries)
  cmd/
    demo/
      main.go              prints letters for a few scores
  grade/grade_test.go      table-driven test using t.Context(); errors.Is on cancel
```

- Files: `grade/grade.go`, `cmd/demo/main.go`, `grade/grade_test.go`.
- Implement: `Letter(ctx context.Context, score int) (string, error)` mapping scores to A–F with correct `>=` boundaries, honoring `ctx.Err()`.
- Test: table-driven over scores including boundaries, plus a cancelled-context case asserting `errors.Is(err, context.Canceled)`.
- Verify: `go test -count=1 -race ./...`, then a `dlv test --init` session that breaks on the boundary case and prints `tc`.

Set up the module:

```bash
mkdir -p ~/go-exercises/gradedbg/grade ~/go-exercises/gradedbg/cmd/demo
cd ~/go-exercises/gradedbg
go mod init example.com/gradedbg
go mod edit -go=1.24
```

### The boundary bug and dlv test

Here is the version with the bug — a strict `>` where the contract needs `>=`, so
a score of exactly 90 grades as B instead of A. It is illustrative; do not save it:

```go
package grade

import "context"

// BUGGY: strict > misses the boundary. A score of exactly 90 returns "B".
func Letter(ctx context.Context, score int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch {
	case score > 90:
		return "A", nil
	case score > 80:
		return "B", nil
	case score > 70:
		return "C", nil
	case score > 60:
		return "D", nil
	default:
		return "F", nil
	}
}
```

The table test below has a `boundary_A` case with `score: 90, want: "A"`, and only
that subtest fails on the buggy version. `dlv test` compiles the test binary with
debug info and starts a session on it, so you break on the grading logic and watch
the wrong branch get taken:

```bash
dlv test ./grade
```

`break grade.TestLetter` would stop at the test function's entry, but the loop
variable `tc` is not in scope there — it only exists inside the subtest closure.
So break on the call line inside the loop instead, and use a condition (as in
Exercise 2) to land directly on the `boundary_A` case regardless of subtest order:

```text
(dlv) break grade/grade_test.go:29
Breakpoint 1 set at 0x... for example.com/gradedbg/grade.TestLetter.func1() ./grade/grade_test.go:29
(dlv) condition 1 tc.score == 90
(dlv) continue
> example.com/gradedbg/grade.TestLetter.func1() ./grade/grade_test.go:29 (hits goroutine(9):1 total:1)
(dlv) print tc
example.com/gradedbg/grade.struct { name string; score int; want string } {name: "boundary_A", score: 90, want: "A"}
(dlv) break grade/grade.go:11
Breakpoint 2 set at 0x... for example.com/gradedbg/grade.Letter() ./grade/grade.go:11
(dlv) condition 2 score == 90
(dlv) continue
> example.com/gradedbg/grade.Letter() ./grade/grade.go:11 (hits goroutine(9):1 total:1)
(dlv) print score
90
(dlv) next
> example.com/gradedbg/grade.Letter() ./grade/grade.go:13
```

The stop shows `score` is 90 and execution falls past `case score > 90` — because
`90 > 90` is false — into the next case at line 13, returning "B". That is the
bug: the boundary needs `>=`. `print tc` shows the whole test-case struct so you
know which subtest you are in, and the conditions on both breakpoints jump you
straight to the `score == 90` case without stepping through the others.

### The fix

Create `grade/grade.go`:

```go
package grade

import "context"

// Letter maps a score to a letter grade using inclusive lower bounds. It honors
// ctx: a cancelled or expired context returns its error and no grade.
func Letter(ctx context.Context, score int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch {
	case score >= 90:
		return "A", nil
	case score >= 80:
		return "B", nil
	case score >= 70:
		return "C", nil
	case score >= 60:
		return "D", nil
	default:
		return "F", nil
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/gradedbg/grade"
)

func main() {
	ctx := context.Background()
	for _, score := range []int{95, 90, 82, 70, 59} {
		letter, err := grade.Letter(ctx, score)
		if err != nil {
			fmt.Printf("%d: error %v\n", score, err)
			continue
		}
		fmt.Printf("%d: %s\n", score, letter)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
95: A
90: A
82: B
70: C
59: F
```

### The test: boundaries and a cancelled context

Create `grade/grade_test.go`:

```go
package grade

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestLetter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		score int
		want  string
	}{
		{name: "high_A", score: 95, want: "A"},
		{name: "boundary_A", score: 90, want: "A"},
		{name: "boundary_B", score: 80, want: "B"},
		{name: "mid_C", score: 75, want: "C"},
		{name: "boundary_D", score: 60, want: "D"},
		{name: "fail_F", score: 59, want: "F"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Letter(t.Context(), tc.score)
			if err != nil {
				t.Fatalf("Letter(%d) unexpected err: %v", tc.score, err)
			}
			if got != tc.want {
				t.Fatalf("Letter(%d) = %q; want %q", tc.score, got, tc.want)
			}
		})
	}
}

func TestLetterCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel before the call

	if _, err := Letter(ctx, 95); !errors.Is(err, context.Canceled) {
		t.Fatalf("Letter with cancelled ctx err = %v; want context.Canceled", err)
	}
}

func ExampleLetter() {
	g, _ := Letter(context.Background(), 90)
	fmt.Println(g)
	// Output: A
}
```

`t.Context()` (Go 1.24+) returns a context tied to the test's lifetime, which is
cleaner than constructing a `context.Background()` per case; the cancelled-context
test derives a child from it and cancels immediately to assert the error path with
`errors.Is`.

### Scripted: prove the breakpoint fired on the boundary case

```bash
cat > /tmp/grade.dlv <<'EOF'
break grade/grade_test.go:29
condition 1 tc.score == 90
continue
print tc.name
print tc.score
quit
EOF

dlv test ./grade --init /tmp/grade.dlv 2>&1 | tee /tmp/grade.out
grep -q 'tc.score = 90' /tmp/grade.out && echo OK
```

The condition makes the breakpoint fire only on the `boundary_A` iteration, so
`print tc.score` shows `90` — the value the boundary bug mishandled — regardless
of the order the parallel subtests happen to run in. A CI job asserts the marker
with the `grep`.

## Review

The grader is correct when every boundary uses an inclusive lower bound, so a
score equal to a cutoff earns the higher grade, and when a cancelled context short
-circuits to its error — the table and the cancellation test pin both. `dlv test`
is the right entry point because it builds the test binary with debug info for
you; `break TestLetter` stops at the test, `print tc` reveals which subtest is
live, and stepping into `Letter` shows the wrong branch on the buggy version. The
mistake to avoid is debugging the shipped binary instead of the test: the failing
subtest is the reproducer, so run `dlv test` on the package and let the table
drive you to the exact input.

## Resources

- [`dlv test` usage](https://github.com/go-delve/delve/blob/master/Documentation/usage/dlv_test.md) — compiling and debugging a package's tests.
- [`testing.T.Context`](https://pkg.go.dev/testing#T.Context) — the per-test context added in Go 1.24.
- [Delve CLI command reference](https://github.com/go-delve/delve/blob/master/Documentation/cli/README.md) — `break` on a symbol, `clear`, `print`, `next`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-goroutine-inspection.md](06-goroutine-inspection.md)
