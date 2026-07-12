# Exercise 2: Table-Driven Parallel Tests Under the Race Detector

The test is the artifact a senior actually maintains. This module writes the
canonical Go test — table-driven, parallel subtests, sentinel errors asserted
with `errors.Is`, floats compared with an epsilon — and runs it the way CI does:
under `-race`, `-count=1`, and `-shuffle=on`, with `-run` to focus a single case.

## What you'll build

```text
table-tests/                   module example.com/table-tests
  go.mod
  internal/
    circle/
      circle.go                Area(radius) (float64, error); ErrNegativeRadius
      circle_test.go           table-driven, t.Parallel subtests, errors.Is, epsilon
  cmd/
    demo/
      main.go                  exercises circle.Area over a few radii
```

- Files: `internal/circle/circle.go`, `internal/circle/circle_test.go`, `cmd/demo/main.go`.
- Implement: the `circle.Area` library.
- Test: a table-driven `TestArea` with parallel subtests, including a `radius: 2.5` case, asserting the sentinel with `errors.Is` and the math with an epsilon.
- Verify: `go run ./cmd/demo` prints the areas; `go test -count=1 -race -shuffle=on ./...` passes; `go test -run TestArea/negative` runs exactly one subtest.

### The shape and why each flag matters

A table-driven test names each case in a struct slice and runs it as a subtest
with `t.Run(tc.name, ...)`. The names are what let `-run TestArea/negative`
select one case by path. Each subtest calls `t.Parallel()`, so the runtime holds
them until the parent returns and then runs them concurrently — which is exactly
why the suite must be run under `-race`: the detector instruments memory access
and reports any unsynchronized sharing between those concurrent subtests. `Area`
is pure and shares nothing, so it is race-free; the point of `-race` is that if a
future change introduced shared state, the parallel table would surface it.

Since Go 1.22 the loop variable `tc` is scoped per iteration, so the old
`tc := tc` shadow is unnecessary and omitted. Two assertion rules are
non-negotiable. Errors are matched with `errors.Is(err, ErrNegativeRadius)`, not
by comparing strings, so the assertion keeps working when the error is wrapped
with `%w` further up the stack. Floats are compared with
`math.Abs(got-want) > epsilon`, never `==`, because floating-point arithmetic is
not exact — `math.Pi * 6.25` computed two ways can differ in the last bit.

Three run flags complete the CI contract. `-count=1` disables the test cache so a
green result reflects this run, not a cached one. `-shuffle=on` randomizes test
order and prints the seed, so a hidden ordering dependency fails loudly instead
of passing by luck. `-run` filters by name for focused local iteration.

Create `internal/circle/circle.go`:

```go
package circle

import (
	"errors"
	"math"
)

// ErrNegativeRadius is returned when the radius is negative.
var ErrNegativeRadius = errors.New("radius must not be negative")

// Area returns the area of a circle with the given radius.
func Area(radius float64) (float64, error) {
	if radius < 0 {
		return 0, ErrNegativeRadius
	}
	return math.Pi * radius * radius, nil
}
```

### A demo that exercises the exported API

The demo runs the same `circle.Area` the table drives, over a small set of radii
including the negative case, so you can watch the sentinel path without reading a
test. It uses `errors.Is` exactly as the test does — the production call site and
the test assert the error the same way.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"os"

	"example.com/table-tests/internal/circle"
)

func main() {
	for _, r := range []float64{2.5, 5, -1} {
		area, err := circle.Area(r)
		if err != nil {
			if errors.Is(err, circle.ErrNegativeRadius) {
				fmt.Printf("radius %.1f: %v\n", r, err)
				continue
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("radius %.1f has area %.5f\n", r, area)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
radius 2.5 has area 19.63495
radius 5.0 has area 78.53982
radius -1.0: radius must not be negative
```

Create `internal/circle/circle_test.go`:

```go
package circle

import (
	"errors"
	"math"
	"testing"
)

func TestArea(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		radius  float64
		want    float64
		wantErr error
	}{
		{name: "zero", radius: 0, want: 0},
		{name: "unit", radius: 1, want: math.Pi},
		{name: "two-and-a-half", radius: 2.5, want: math.Pi * 6.25},
		{name: "five", radius: 5, want: 25 * math.Pi},
		{name: "negative", radius: -1, wantErr: ErrNegativeRadius},
	}

	const epsilon = 1e-9
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Area(tc.radius)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Area(%v) err = %v, want %v", tc.radius, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Area(%v) unexpected err = %v", tc.radius, err)
			}
			if math.Abs(got-tc.want) > epsilon {
				t.Fatalf("Area(%v) = %v, want %v (+/-%v)", tc.radius, got, tc.want, epsilon)
			}
		})
	}
}
```

Run the full CI form:

```bash
go test -count=1 -race -shuffle=on ./...
```

```text
ok  	example.com/table-tests/internal/circle	0.259s
```

Focus a single subtest by its `/`-joined path:

```bash
go test -run 'TestArea/negative' -v ./internal/circle
```

```text
=== RUN   TestArea
=== RUN   TestArea/negative
--- PASS: TestArea (0.00s)
PASS
ok  	example.com/table-tests/internal/circle	0.259s
```

Only the `negative` subtest ran; the parent `TestArea` still reports so its
setup executes, but no other case was selected.

### Why -shuffle=on is not optional

`-shuffle=on` exists because tests that pass in declaration order but depend on
that order are a real, common bug. Consider two tests sharing a package variable:

```go
// Illustrative only — do NOT add this to the module. It fails under -shuffle.
var shared int

func TestSeed(t *testing.T)    { shared = 42 }
func TestUseSeed(t *testing.T) {
	if shared != 42 {
		t.Fatalf("shared = %d, want 42 (TestSeed must run first)", shared)
	}
}
```

In declaration order this passes. Under a shuffle seed that runs `TestUseSeed`
first it fails, and the seed is printed so you can reproduce it:

```text
-test.shuffle 2
--- FAIL: TestUseSeed (0.00s)
    order_test.go:13: shared = 0, want 42 (TestSeed must run first)
```

`-shuffle=on` picks a random seed each run (printing something like
`-test.shuffle 1782997803715301000`); `-shuffle=<n>` reproduces a specific
ordering. The `TestArea` table above shares nothing, so it is order-independent
and passes under every seed — which is the property `-shuffle=on` verifies.

## Review

The suite is correct when `go test -count=1 -race -shuffle=on ./...` passes and
`-run TestArea/negative` narrows to exactly one subtest. The traps this exercise
drills are the assertion ones: comparing floats with `==` (use an epsilon), and
matching errors by string instead of `errors.Is` (which breaks the moment the
error is wrapped). The order-dependent example is illustrative and deliberately
excluded from the module — the whole point is that the shipped table is immune to
it. Run the suite a few times; a stable green under `-shuffle=on` and `-race` is
the signal that the tests are independent and race-free.

## Resources

- [testing package](https://pkg.go.dev/testing) — `T.Parallel`, `T.Run`, and how parallel subtests are scheduled.
- [Command go — testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — `-race`, `-count`, `-run`, `-shuffle`, and the test cache.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel error rather than comparing strings.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-package-graph-and-package-mode.md](01-package-graph-and-package-mode.md) | Next: [03-testable-examples-as-docs.md](03-testable-examples-as-docs.md)
