# Exercise 1: Baseline coverage — profile a small library end-to-end

Before coverage can be a signal in a pipeline you have to know the mechanics cold:
run a suite under `-cover`, write the profile with `-coverprofile`, read it
per-function with `go tool cover -func`, and verify the whole thing is race-safe
under `-race -covermode=atomic`. This module builds a small arithmetic library —
`Add`, `Sub`, `Mul`, `Div` with a sentinel error — and drives every one of those
tools against it, so the workflow is muscle memory before the harder exercises.

This module is fully self-contained: its own `go mod init`, its own demo, and its
own tests. Nothing here imports another exercise.

## What you'll build

```text
mathcov/                   independent module: example.com/mathcov
  go.mod
  math.go                  Add, Sub, Mul, Div; ErrDivideByZero sentinel
  cmd/
    demo/
      main.go              runnable demo exercising each operation
  math_test.go             table-driven tests, errors.Is on the zero branch, Example
```

- Files: `math.go`, `cmd/demo/main.go`, `math_test.go`.
- Implement: `Add`, `Sub`, `Mul` returning `int`, and `Div(a, b int) (int, error)` returning `ErrDivideByZero` (wrapped) when `b == 0`.
- Test: table-driven happy-path cases, the `b == 0` error branch asserted with `errors.Is`, and the `Div(_, 1)` branch pinned explicitly.
- Verify: `go test -count=1 -race -covermode=atomic -cover ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/mathcov/cmd/demo
cd ~/go-exercises/mathcov
go mod init example.com/mathcov
```

### The library and its one interesting branch

Three of the four operations are total functions: for every pair of `int`s they
return an `int`, so a single happy-path assertion each covers them fully. `Div`
is the interesting one: it has a branch. When the divisor is zero it returns a
package-level sentinel, `ErrDivideByZero`, so callers can classify the failure
with `errors.Is` rather than string-matching a message. The sentinel is declared
once with `errors.New` and returned directly here; a real repository layer would
wrap it with `fmt.Errorf("...: %w", ErrDivideByZero)` to add context while keeping
it matchable — the same `errors.Is` pattern you assert against in the test.

Coverage on this library is a small, honest 100%: the `b == 0` branch is the only
thing that can be missed, and one test drives it. That is what "coverage as a
diagnostic" looks like at the smallest scale — the profile's job is to tell you
the zero-divisor branch is or is not exercised, nothing more grand.

Create `math.go`:

```go
package mathcov

import "errors"

// ErrDivideByZero is returned by Div when the divisor is zero. Callers classify
// it with errors.Is rather than matching the message text.
var ErrDivideByZero = errors.New("divide by zero")

// Add returns a + b.
func Add(a, b int) int { return a + b }

// Sub returns a - b.
func Sub(a, b int) int { return a - b }

// Mul returns a * b.
func Mul(a, b int) int { return a * b }

// Div returns a / b, or ErrDivideByZero when b is zero.
func Div(a, b int) (int, error) {
	if b == 0 {
		return 0, ErrDivideByZero
	}
	return a / b, nil
}
```

### The runnable demo

The demo exercises each operation and shows the sentinel classification in action,
so `go run ./cmd/demo` produces deterministic output you can diff.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/mathcov"
)

func main() {
	fmt.Println("add:", mathcov.Add(2, 3))
	fmt.Println("sub:", mathcov.Sub(5, 2))
	fmt.Println("mul:", mathcov.Mul(4, 6))

	q, err := mathcov.Div(10, 2)
	fmt.Println("div:", q, err)

	if _, err := mathcov.Div(10, 0); errors.Is(err, mathcov.ErrDivideByZero) {
		fmt.Println("div by zero rejected")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
add: 5
sub: 3
mul: 24
div: 5 <nil>
div by zero rejected
```

### The tests

The tests are table-driven. The arithmetic table covers the three total
functions; a separate `Div` table covers both the happy path and the
`Div(_, 1)` case (the divide-by-one path is implicitly covered by any non-zero
divisor, but pinning it explicitly documents intent). `TestDivRejectsZero`
asserts the error branch with `errors.Is`, which is the load-bearing test for
coverage — it is the only thing that drives the `b == 0` block. Every test uses
`t.Parallel()`; these are pure functions with no shared state, so parallelism is
free and exercises the atomic counters under `-race`.

Create `math_test.go`:

```go
package mathcov

import (
	"errors"
	"fmt"
	"testing"
)

func TestArithmetic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		op   func(int, int) int
		a, b int
		want int
	}{
		{"add", Add, 2, 3, 5},
		{"add-negative", Add, -4, 1, -3},
		{"sub", Sub, 5, 2, 3},
		{"mul", Mul, 4, 6, 24},
		{"mul-by-zero", Mul, 7, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.op(tt.a, tt.b); got != tt.want {
				t.Errorf("%s(%d,%d) = %d, want %d", tt.name, tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestDiv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		a, b    int
		want    int
		wantErr error
	}{
		{"exact", 10, 2, 5, nil},
		{"truncates", 7, 2, 3, nil},
		{"by-one", 10, 1, 10, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Div(tt.a, tt.b)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Div(%d,%d) err = %v, want %v", tt.a, tt.b, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Div(%d,%d) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestDivRejectsZero(t *testing.T) {
	t.Parallel()
	if _, err := Div(10, 0); !errors.Is(err, ErrDivideByZero) {
		t.Fatalf("Div(10,0) err = %v, want ErrDivideByZero", err)
	}
}

func ExampleDiv() {
	q, err := Div(9, 3)
	fmt.Println(q, err)
	// Output: 3 <nil>
}
```

`ExampleDiv` needs `fmt`, so add it to the test file's imports alongside
`errors`. `go test` runs the `Example` and compares its stdout against the
`// Output:` comment, so a regression in `Div` is caught by the example just as it
is by the table tests.

### Running coverage against the library

With the code in place, run the four tools in sequence. First the headline number:

```bash
go test -cover ./...
```

Expected output:

```
ok      example.com/mathcov     0.002s  coverage: 100.0% of statements
```

Write a profile and read it per-function:

```bash
go test -coverprofile=cover.out ./...
go tool cover -func=cover.out
```

Expected output (line numbers depend on your file):

```
example.com/mathcov/math.go:10:  Add             100.0%
example.com/mathcov/math.go:13:  Sub             100.0%
example.com/mathcov/math.go:16:  Mul             100.0%
example.com/mathcov/math.go:19:  Div             100.0%
total:                           (statements)    100.0%
```

Open the HTML view to see the source painted green (no red because coverage is
complete here):

```bash
go tool cover -html=cover.out
```

Finally, the race-safe verification — this is the form a CI job runs. `-race`
forces the counters to atomic mode automatically; passing `-covermode=atomic`
makes that explicit:

```bash
go test -count=1 -race -covermode=atomic -cover ./...
```

Expected output:

```
ok      example.com/mathcov     0.30s   coverage: 100.0% of statements
```

## Review

The library is correct when `Div` returns `ErrDivideByZero` for exactly `b == 0`
and a truncating integer quotient otherwise, and when every caller can classify
the failure with `errors.Is` against the exported sentinel. The coverage workflow
is correct when `go tool cover -func` reports `100.0%` on the `total:` line and
the `-html` view shows no red — meaning the only branch that can be missed, the
zero divisor, is driven by `TestDivRejectsZero`.

The mistake to avoid at this scale is reading the 100% as a badge. It is not: it
says only that each statement ran, which for four tiny functions is a low bar. The
value here is the *habit* — write the profile, read `-func`, glance at `-html`,
and verify under `-race -covermode=atomic` so the number is trustworthy. Do not
hardcode `-covermode=set` with `-race`; that reintroduces the counter race the
atomic default protects against. Run `go test -race` to confirm the parallel
subtests are clean.

## Resources

- [Testing flags (`-cover`, `-covermode`, `-coverprofile`)](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — the authoritative flag reference.
- [`go tool cover`](https://pkg.go.dev/cmd/cover) — `-func`, `-html`, and the profile format.
- [The cover story](https://go.dev/blog/cover) — the original design of Go's coverage tooling.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-covermode-atomic-race-cache.md](02-covermode-atomic-race-cache.md)
