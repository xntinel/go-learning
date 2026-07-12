# Exercise 1: The Sum Library: Your First Passing Test

The smallest possible package with a real test. The point is not arithmetic — it
is to install the four rules every later module reuses: the `_test.go` suffix, the
`func TestXxx(*testing.T)` signature, `t.Parallel`, and the four-command gate.

## What you'll build

```text
sumtest/                   independent module: example.com/sumtest
  go.mod
  sum.go                   func Sum(a, b int) int
  sum_test.go              TestSum, TestSumWithNegatives, ExampleSum
  cmd/
    demo/
      main.go              runnable demo printing a couple of sums
```

- Files: `sum.go`, `sum_test.go`, `cmd/demo/main.go`.
- Implement: `Sum(a, b int) int` returning `a + b`.
- Test: `TestSum` pins `Sum(2, 3) == 5`; `TestSumWithNegatives` pins `Sum(-1, 1) == 0`.
- Verify: `gofmt -l .`, `go vet ./...`, `go test -count=1 -race ./...`.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/01-your-first-test/01-sum-library-first-test/cmd/demo
cd go-solutions/12-testing-ecosystem/01-your-first-test/01-sum-library-first-test
```

### Why this trivial function earns a test

`Sum` is deterministic and side-effect-free — the ideal shape for a first test.
It reads no clock, no environment, no network; the same inputs always yield the
same output. That is precisely what makes the test trustworthy and instant, and
it is the property you preserve in every module that follows.

`TestSum` computes `got := Sum(2, 3)`, names `want := 5`, and compares with `!=`.
On mismatch it calls `t.Fatalf` with a got-then-want message using `%d` for the
integers. `Fatalf` is defensible here because there is only one assertion — there
is nothing after it to accumulate — so aborting on failure costs nothing.
`TestSumWithNegatives` pins a second point of the contract, that adding `-1` and
`1` yields `0`, guarding against a future "optimization" that mishandles signs.

`t.Parallel()` marks each test as safe to run concurrently with other parallel
tests. It is safe here because the tests share no mutable state and touch no
global; that is the only condition under which `t.Parallel` is correct, and it
holds for every pure-function test in this lesson.

Create `sum.go`:

```go
package sum

// Sum returns the sum of two integers.
func Sum(a, b int) int {
	return a + b
}
```

### The runnable demo

`cmd/demo` is a separate `package main`, so it can only touch the exported API —
which for this package is just `Sum`. That constraint is the point of a demo: it
exercises the package the way a real caller would.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sumtest"
)

func main() {
	fmt.Printf("Sum(2, 3) = %d\n", sum.Sum(2, 3))
	fmt.Printf("Sum(-1, 1) = %d\n", sum.Sum(-1, 1))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Sum(2, 3) = 5
Sum(-1, 1) = 0
```

### The tests

Create `sum_test.go`:

```go
package sum

import (
	"fmt"
	"testing"
)

func TestSum(t *testing.T) {
	t.Parallel()

	got := Sum(2, 3)
	want := 5
	if got != want {
		t.Fatalf("Sum(2, 3) = %d, want %d", got, want)
	}
}

func TestSumWithNegatives(t *testing.T) {
	t.Parallel()

	got := Sum(-1, 1)
	want := 0
	if got != want {
		t.Fatalf("Sum(-1, 1) = %d, want %d", got, want)
	}
}

func ExampleSum() {
	fmt.Println(Sum(40, 2))
	// Output: 42
}
```

## Review

The test is correct when it actually runs and pins the value. Confirm the
mechanics with the failure-mode drill: rename `TestSum` to `SumTest` and run
`go test` — it prints `ok` with no test executed, proving why the `Test` prefix
is load-bearing. Restore the name. Then break `Sum` to `a - b` and confirm the
run goes red with a legible `Sum(2, 3) = -1, want 5` line; that message is the
artifact a future reader relies on. The `ExampleSum` function is verified by
`go test` against its `// Output:` comment, so it is a real assertion, not a
comment. Gate with `gofmt -l .`, `go vet ./...`, and
`go test -count=1 -race ./...`; all three must be clean.

## Resources

- [testing package](https://pkg.go.dev/testing) — `T`, `Errorf`, `Fatalf`, `Parallel`, and the `TestXxx` signature.
- [cmd/go: Test packages](https://pkg.go.dev/cmd/go#hdr-Test_packages) — how `go test` compiles and runs the test binary.
- [Effective Go: Testing](https://go.dev/doc/effective_go#testing) — the idioms behind a Go test.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-retryable-http-status-classifier.md](02-retryable-http-status-classifier.md)
