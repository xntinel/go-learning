# Exercise 1: Release a versioned internal pricing library

The unit that carries a version is a module, and the smallest honest module is a
library with a clear contract and a sentinel error. Here you build
`example.com/billing/pricing` as it would be released at `v0.1.0`: integer money
math with a division that refuses a zero divisor, extended with a `DivMod` for the
quotient-and-remainder split that prorated billing needs.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
pricing/                    independent module: example.com/billing/pricing
  go.mod                    module path == import prefix; released as v0.1.0
  pricing.go                Add, Sub, Mul, Div, DivMod; ErrDivideByZero sentinel
  cmd/
    demo/
      main.go               runnable: prorate a charge with Div and DivMod
  pricing_test.go           table-driven happy paths + errors.Is on the sentinel
```

- Files: `pricing.go`, `cmd/demo/main.go`, `pricing_test.go`.
- Implement: `Add`, `Sub`, `Mul`, `Div(a, b) (int, error)`, and `DivMod(a, b) (q, r int, err error)`, all rejecting a zero divisor with the package sentinel `ErrDivideByZero`.
- Test: subtests for each happy path, `TestDivRejectsZero` and `TestDivModRejectsZero` via `errors.Is`, and `TestDivMod` asserting `DivMod(10, 3) == (3, 1, nil)`.
- Verify: `go test -count=1 -race ./...`

Set up the module. The module path is the import prefix for every package inside
it, and it is the name you will tag as `v0.1.0`:

```bash
mkdir -p ~/go-exercises/pricing/cmd/demo
cd ~/go-exercises/pricing
go mod init example.com/billing/pricing
go mod edit -go=1.26
```

### Why a sentinel error, and why v0

Division is the only operation with a precondition — a zero divisor has no answer —
so it is the only one that returns an `error`. The error is a package-level
*sentinel*: `var ErrDivideByZero = errors.New(...)`, declared once and returned by
value, so a caller can branch on the *cause* with `errors.Is(err, ErrDivideByZero)`
rather than string-matching the message. That is the contract you are versioning:
callers wrote `errors.Is(err, pricing.ErrDivideByZero)`, and that line must keep
working across every `v0.x` and `v1.x` release. Change the sentinel's identity and
you have broken consumers as surely as if you renamed a function — which is exactly
why it lives in the public surface and why moving it later would be a v2 change.

`DivMod` returns both the quotient and the remainder because prorating a charge is
a single division you do not want to do twice: split a 100-unit monthly fee across
3 tenants and you owe each `q` units with `r` units of rounding to reconcile,
`DivMod(100, 3) == (33, 1, nil)`. Doing `a/b` and `a%b` as two calls invites the
divisor-check to drift out of sync between them; one function, one guard, one
sentinel.

Releasing this at `v0.1.0` is deliberate. v0 is the "still stabilizing" line: the
module path carries no `/vN` suffix, and the compatibility promise is weaker, so you
can refine the surface before committing to v1. The suffix rule only begins at v2 —
that is Exercise 3.

Create `pricing.go`:

```go
package pricing

import "errors"

// ErrDivideByZero is the sentinel returned by Div and DivMod when the divisor is
// zero. Callers branch on it with errors.Is; its identity is part of the module's
// public contract and must survive every v0.x and v1.x release.
var ErrDivideByZero = errors.New("pricing: divide by zero")

// Add returns a + b.
func Add(a, b int) int { return a + b }

// Sub returns a - b.
func Sub(a, b int) int { return a - b }

// Mul returns a * b.
func Mul(a, b int) int { return a * b }

// Div returns the integer quotient a / b, or ErrDivideByZero if b is zero.
func Div(a, b int) (int, error) {
	if b == 0 {
		return 0, ErrDivideByZero
	}
	return a / b, nil
}

// DivMod returns the quotient q and remainder r of a / b in one call, so a
// prorated split does not divide twice. It returns ErrDivideByZero if b is zero.
func DivMod(a, b int) (q, r int, err error) {
	if b == 0 {
		return 0, 0, ErrDivideByZero
	}
	return a / b, a % b, nil
}
```

### The runnable demo

The demo prorates a 100-unit monthly charge across three tenants: `DivMod` gives
the per-tenant amount and the leftover, and a guarded `Div` shows the zero-divisor
path returning the sentinel rather than panicking.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/billing/pricing"
)

func main() {
	const monthly, tenants = 100, 3

	q, r, err := pricing.DivMod(monthly, tenants)
	if err != nil {
		fmt.Println("prorate failed:", err)
		return
	}
	fmt.Printf("each tenant owes %d, remainder %d to reconcile\n", q, r)

	if _, err := pricing.Div(monthly, 0); errors.Is(err, pricing.ErrDivideByZero) {
		fmt.Println("guarded zero divisor:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
each tenant owes 33, remainder 1 to reconcile
guarded zero divisor: pricing: divide by zero
```

### Tests

The tests are table-driven for the happy paths and assert the *cause* of a failure
with `errors.Is`, never the message text. `TestDivMod` pins the contract the brief
names — `DivMod(10, 3) == (3, 1, nil)` — and the two zero-divisor tests prove both
guards route through the same sentinel.

Create `pricing_test.go`:

```go
package pricing

import (
	"errors"
	"fmt"
	"testing"
)

func TestArithmetic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"add", Add(2, 3), 5},
		{"sub", Sub(5, 2), 3},
		{"mul", Mul(2, 3), 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.got != tc.want {
				t.Fatalf("%s = %d, want %d", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestDiv(t *testing.T) {
	t.Parallel()
	got, err := Div(10, 2)
	if err != nil {
		t.Fatalf("Div(10, 2) unexpected error: %v", err)
	}
	if got != 5 {
		t.Fatalf("Div(10, 2) = %d, want 5", got)
	}
}

func TestDivRejectsZero(t *testing.T) {
	t.Parallel()
	if _, err := Div(10, 0); !errors.Is(err, ErrDivideByZero) {
		t.Fatalf("Div(10, 0) err = %v, want ErrDivideByZero", err)
	}
}

func TestDivMod(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		a, b         int
		wantQ, wantR int
	}{
		{"exact", 10, 5, 2, 0},
		{"with-remainder", 10, 3, 3, 1},
		{"prorate-100-3", 100, 3, 33, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, r, err := DivMod(tc.a, tc.b)
			if err != nil {
				t.Fatalf("DivMod(%d, %d) unexpected error: %v", tc.a, tc.b, err)
			}
			if q != tc.wantQ || r != tc.wantR {
				t.Fatalf("DivMod(%d, %d) = (%d, %d), want (%d, %d)", tc.a, tc.b, q, r, tc.wantQ, tc.wantR)
			}
		})
	}
}

func TestDivModRejectsZero(t *testing.T) {
	t.Parallel()
	if _, _, err := DivMod(10, 0); !errors.Is(err, ErrDivideByZero) {
		t.Fatalf("DivMod(10, 0) err = %v, want ErrDivideByZero", err)
	}
}

func ExampleDivMod() {
	q, r, _ := DivMod(100, 3)
	fmt.Printf("q=%d r=%d\n", q, r)
	// Output: q=33 r=1
}
```

## Review

The library is correct when division is the only fallible operation and both
`Div` and `DivMod` route a zero divisor through the *same* `ErrDivideByZero`, so a
caller's `errors.Is(err, pricing.ErrDivideByZero)` holds regardless of which one it
called. The tests prove exactly that: the happy-path table pins the arithmetic,
`TestDivMod` pins `(3, 1)` for `DivMod(10, 3)` and `(33, 1)` for the 100-over-3
prorate, and the two rejection tests assert the sentinel by identity, not by
message. The trap to avoid is treating the sentinel as an implementation detail —
its identity is the versioned contract; a v0.x or v1.x release that swaps it for a
freshly constructed error silently breaks every consumer that branched on it.

## Resources

- [Go Modules Reference: versions](https://go.dev/ref/mod#versions) — how a tagged version maps to a module release.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching a wrapped error against a sentinel by identity.
- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — sentinel errors and the `%w`/`Is` model.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-inspect-the-module-graph.md](02-inspect-the-module-graph.md)
