# Exercise 1: Math Library — Example, Output, and Multi-Line Output

The plainest way to feel the double-duty of a testable example is on a package
whose behavior is trivial to read, so all your attention is on the mechanism. You
build a shared arithmetic helper — the kind of tiny internal package a dozen
services import — and pin its public contract with runnable examples whose
`// Output:` comments the test runner asserts on every CI run.

## What you'll build

```text
mathkit/                    independent module: example.com/mathkit
  go.mod                    go 1.26
  mathkit.go                Add, Sub, Mul (the public contract)
  cmd/
    demo/
      main.go               runnable demo printing each operation
  mathkit_test.go           table-driven Test + Example* with // Output:
```

Files: `mathkit.go`, `cmd/demo/main.go`, `mathkit_test.go`.
Implement: exported `Add`, `Sub`, `Mul` over `int`.
Test: a table-driven `Test`, plus `ExampleAdd`, `ExampleSub`, `ExampleMul`, the multi-line `ExampleAdd_multiple`, and the negative-contract `ExampleMul_negative`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/mathkit/cmd/demo
cd ~/go-exercises/mathkit
go mod init example.com/mathkit
```

## The base rules, made concrete

An example function takes no arguments and returns nothing; the toolchain finds
it by the `Example` prefix. A concluding `// Output:` comment turns it into an
executed test: the runner captures stdout during the call and compares it to the
comment, ignoring leading and trailing whitespace. A multi-line expectation is a
comment block, one line per printed line, as `ExampleAdd_multiple` shows. Each of
these examples is an executable assertion, not merely prose: mutate `Add` to
`a + b + 1` and `ExampleAdd` turns red, which is precisely the property that
keeps the documentation honest.

`ExampleMul_negative` exists to pin a contract that is easy to regress silently:
that `Mul` propagates sign correctly for negative inputs. `Mul(-2, 3)` must
produce `-6`, and the example nails that down as public, executed documentation —
if someone "optimizes" `Mul` into an absolute-value-then-sign routine and gets
the sign wrong, this example catches it.

Create `mathkit.go`:

```go
package mathkit

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a + b
}

// Sub returns a minus b.
func Sub(a, b int) int {
	return a - b
}

// Mul returns the product of a and b, preserving sign for negative inputs.
func Mul(a, b int) int {
	return a * b
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mathkit"
)

func main() {
	fmt.Println("Add(2, 3) =", mathkit.Add(2, 3))
	fmt.Println("Sub(5, 2) =", mathkit.Sub(5, 2))
	fmt.Println("Mul(2, 3) =", mathkit.Mul(2, 3))
	fmt.Println("Mul(-2, 3) =", mathkit.Mul(-2, 3))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Add(2, 3) = 5
Sub(5, 2) = 3
Mul(2, 3) = 6
Mul(-2, 3) = -6
```

### Tests and examples

The table-driven `Test` covers the arithmetic across positive, negative, and
zero inputs; the examples pin the public-facing contract as documentation that
also runs. Note `ExampleAdd_multiple` uses the multi-line `// Output:` block and
`ExampleMul_negative` pins the negative-input result.

Create `mathkit_test.go`:

```go
package mathkit

import (
	"fmt"
	"testing"
)

func TestOps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		op         func(int, int) int
		a, b, want int
	}{
		{"add positive", Add, 2, 3, 5},
		{"add negatives", Add, -4, -6, -10},
		{"sub to zero", Sub, 7, 7, 0},
		{"sub negative result", Sub, 2, 5, -3},
		{"mul positive", Mul, 6, 7, 42},
		{"mul negative", Mul, -2, 3, -6},
		{"mul by zero", Mul, 99, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.op(tt.a, tt.b); got != tt.want {
				t.Errorf("%s: got %d, want %d", tt.name, got, tt.want)
			}
		})
	}
}

func ExampleAdd() {
	fmt.Println(Add(2, 3))
	// Output: 5
}

func ExampleSub() {
	fmt.Println(Sub(5, 2))
	// Output: 3
}

func ExampleMul() {
	fmt.Println(Mul(2, 3))
	// Output: 6
}

func ExampleAdd_multiple() {
	fmt.Println(Add(10, 20))
	fmt.Println(Add(-1, 1))
	// Output:
	// 30
	// 0
}

func ExampleMul_negative() {
	fmt.Println(Mul(-2, 3))
	// Output: -6
}
```

## Review

The examples are correct when each one's captured stdout matches its `// Output:`
comment exactly, whitespace at the ends aside. The way to convince yourself they
are real tests and not decorative prose is to break the code on purpose: change
`Add` to `a + b + 1` and run `go test -run Example` — `ExampleAdd` and
`ExampleAdd_multiple` both fail, proving the comment is an assertion. The most
common miss on a package like this is dropping the `// Output:` line, at which
point the example still compiles and renders in `go doc` but no longer runs, so a
regression passes silently. Keep `gofmt -l` empty and `go vet ./...` clean; run
`go test -v -run Example` to see each example reported as its own `PASS`.

## Resources

- [The Go Blog: Testable Examples in Go](https://go.dev/blog/examples) — the original walkthrough of examples as documentation and tests.
- [testing package — Examples](https://pkg.go.dev/testing#hdr-Examples) — the naming, `// Output:`, and whole-file rules, authoritative.
- [go command — Testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — `-run Example` and how the test binary treats examples.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-money-value-object-method-examples.md](02-money-value-object-method-examples.md)
