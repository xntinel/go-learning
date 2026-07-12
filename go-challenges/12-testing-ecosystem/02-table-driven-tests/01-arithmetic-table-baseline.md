# Exercise 1: The Canonical Pattern: A Pure-Function Arithmetic Library

The table-driven pattern is easiest to see on a pure function, where the outcome
is a plain value and nothing hides in state or time. This module builds a tiny
arithmetic package and tests it with the canonical shape — a slice of
`{name, a, b, want}` cases, one `t.Run` subtest each, `t.Parallel` on both the
parent and every subtest — then proves the payoff by adding a large-number case as
a single new row.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
arith/                     independent module: example.com/arith
  go.mod                   go 1.26
  arith.go                 Add, Sub, Mul (pure int functions)
  cmd/
    demo/
      main.go              runnable demo printing a few results
  arith_test.go            table-driven TestAdd/TestSub/TestMul + large-number row
```

- Files: `arith.go`, `cmd/demo/main.go`, `arith_test.go`.
- Implement: `Add`, `Sub`, `Mul` over `int`.
- Test: a `{name,a,b,want}` table per operation, each case a `t.Run` subtest with `t.Parallel`, a `Fatalf` that echoes the inputs, and a large-number row appended to `TestAdd`.
- Verify: `go test -count=1 -race ./...`

### Why this shape is the template

Everything later in the lesson — the HTTP status matrix, the validator, the
error-to-status mapper — reuses the exact structure you write here: an anonymous
struct with a `name` field first, then the inputs, then the expected outcome; a
loop that runs each element as a named subtest; a failure message that reprints the
case's inputs so the output is self-diagnosing. Starting on pure integer functions
lets you see the skeleton with nothing else in the frame.

Two properties are worth naming explicitly. The `name` field is not decoration: it
is what `go test -run TestAdd/mixed` filters on and what the failure line prints,
so it earns its place in the struct. And the failure uses `t.Fatalf("Add(%d, %d) =
%d, want %d", ...)` rather than `t.Fatal("wrong")` — inside a loop the inputs are
the only way to know which values broke, and echoing them is the difference between
a self-explanatory failure and one that forces you to read the table.

The `t.Parallel()` call appears twice on purpose: once on the parent so `TestAdd`,
`TestSub`, and `TestMul` overlap, and once inside each subtest so the rows of one
table overlap too. Because the functions are pure and each case is a fresh subtest
scope, there is no shared state to corrupt, so parallelism is free here — it is a
habit worth forming on the simple case before it matters on the ones with
recorders and buffers.

Create `arith.go`:

```go
package arith

// Add returns a + b.
func Add(a, b int) int {
	return a + b
}

// Sub returns a - b.
func Sub(a, b int) int {
	return a - b
}

// Mul returns a * b.
func Mul(a, b int) int {
	return a * b
}
```

### The runnable demo

`cmd/demo` is a separate `package main`, so it can touch only the exported API —
which for this module is exactly `Add`, `Sub`, `Mul`. It exists to give you a
runnable artifact and an expected-output block; the real verification is the test.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/arith"
)

func main() {
	fmt.Println("Add(2, 3)       =", arith.Add(2, 3))
	fmt.Println("Sub(5, 3)       =", arith.Sub(5, 3))
	fmt.Println("Mul(4, 6)       =", arith.Mul(4, 6))
	fmt.Println("Add(1e6, 2e6)   =", arith.Add(1_000_000, 2_000_000))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Add(2, 3)       = 5
Sub(5, 3)       = 2
Mul(4, 6)       = 24
Add(1e6, 2e6)   = 3000000
```

### The tests

Each test declares its table inline, loops with `t.Run`, and asserts `got !=
want` with an input-echoing `Fatalf`. The `large` row in `TestAdd` is the whole
point of the pattern made concrete: extending the contract to "Add handles
million-scale operands" cost exactly one line, not a new function. An `Example`
with an `// Output:` comment doubles as documentation the `go` tool verifies.

Create `arith_test.go`:

```go
package arith

import (
	"fmt"
	"testing"
)

func TestAdd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b int
		want int
	}{
		{"pos", 2, 3, 5},
		{"neg", -1, -1, -2},
		{"zero", 0, 0, 0},
		{"mixed", -1, 1, 0},
		{"large", 1_000_000, 2_000_000, 3_000_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Add(tc.a, tc.b); got != tc.want {
				t.Fatalf("Add(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestSub(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b int
		want int
	}{
		{"pos", 5, 3, 2},
		{"neg", 3, 5, -2},
		{"zero", 0, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Sub(tc.a, tc.b); got != tc.want {
				t.Fatalf("Sub(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestMul(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b int
		want int
	}{
		{"pos", 2, 3, 6},
		{"neg", -2, 3, -6},
		{"zero", 0, 5, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Mul(tc.a, tc.b); got != tc.want {
				t.Fatalf("Mul(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func ExampleAdd() {
	fmt.Println(Add(1_000_000, 2_000_000))
	// Output: 3000000
}
```

## Review

The suite is correct when each operation's table names every case, runs it as a
subtest, and prints the inputs on failure. The signal that the pattern is working
is that `go test -run TestAdd/large` selects exactly one case — proof the names are
filter targets, not comments. If you find yourself copying a test function to add a
case, you have left the pattern; the fix is always a new row.

Keep the `Fatalf` echoing the arguments: a bare `t.Fatal` inside the loop would
report which named case failed but not the values, which is half the diagnosis.
And note the two `t.Parallel()` calls are safe here only because the functions are
pure and no fixture is shared — that property is what later modules must preserve
deliberately with fresh recorders and buffers, and it is worth seeing it hold for
free once before it costs effort. Run `go test -race` to confirm the parallel
subtests share nothing.

## Resources

- [testing package](https://pkg.go.dev/testing) — `T.Run`, `T.Parallel`, `T.Fatalf`.
- [Go Wiki: TableDrivenTests](https://go.dev/wiki/TableDrivenTests) — the canonical description of the pattern.
- [Go Blog: Using Subtests and Sub-benchmarks](https://go.dev/blog/subtests) — why `t.Run` names matter for `-run` filtering.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-http-handler-status-matrix.md](02-http-handler-status-matrix.md)
