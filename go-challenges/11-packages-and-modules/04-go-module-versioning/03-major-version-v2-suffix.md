# Exercise 3: Ship a breaking change as a v2 module without breaking v1

A breaking change is one consumers must opt into, and Semantic Import Versioning
makes the opt-in the import statement itself: v2 carries a `/v2` suffix in its path,
so it is a *different* import from v1 and the two link into one binary side by side.
Here you build both — a v1 `Div` returning a sentinel, a v2 `Div` returning a
structured result and a rich error — and prove the suffix rule with
`module.SplitPathVersion`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
pricing/                    independent module: example.com/billing/pricing
  go.mod                    the v1 module path (no suffix)
  pricing.go                v1: Div(a, b) (int, error) with ErrDivideByZero
  v2/
    pricing.go              v2 package, import path example.com/billing/pricing/v2
  cmd/
    demo/
      main.go               runnable: v1 and v2 side by side in one binary
  pricing_test.go           asserts both signatures + module.SplitPathVersion("/v2")
```

- Files: `pricing.go`, `v2/pricing.go`, `cmd/demo/main.go`, `pricing_test.go`.
- Implement: v1 `Div(a, b) (int, error)` with the `ErrDivideByZero` sentinel; v2 `Div(a, b) (Quotient, error)` returning a `Quotient{Value, Remainder}` and a typed `*DivError`.
- Test: the v2 signature via `example.com/billing/pricing/v2`, and `module.SplitPathVersion("example.com/billing/pricing/v2")` yielding `pathMajor == "/v2"`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/04-go-module-versioning/03-major-version-v2-suffix/v2 go-solutions/11-packages-and-modules/04-go-module-versioning/03-major-version-v2-suffix/cmd/demo
cd go-solutions/11-packages-and-modules/04-go-module-versioning/03-major-version-v2-suffix
go mod edit -go=1.26
```

### The path is the import, so v2 is a different module

In a real release, v2 is its own module: a nested `go.mod` at `pricing/v2/go.mod`
declaring `module example.com/billing/pricing/v2`, tagged `v2.0.0`. Consumers who
want the new API import `example.com/billing/pricing/v2/...`; consumers still on the
old API import `example.com/billing/pricing/...` unchanged. Because the two module
paths differ, the compiler treats them as unrelated modules and links both into the
same binary — there is no flag day, no coordinated repo-wide upgrade.

This exercise models that layout as a `v2/` subdirectory of a single module so the
whole thing gates as one unit, but the mechanism it demonstrates is identical: the
package under `v2/` has import path `example.com/billing/pricing/v2`, distinct from
the root package's `example.com/billing/pricing`, and both are importable at once.
The rule you must internalize is the string-level fact that `module.SplitPathVersion`
encodes: a v2+ path splits into a prefix and a `/vN` pathMajor, and a v0/v1 path has
an empty pathMajor. Omit the suffix on a real v2 module and the toolchain rejects it
with `module declares its path as X but was required as X/v2` — you told it two
contradictory things about one path.

The v2 API is a genuine breaking change, which is *why* it needs a new major: v1's
`Div` returns `(int, error)` and a package-level sentinel; v2's `Div` returns a
`Quotient` struct (value plus remainder) and a typed `*DivError` carrying the
operands. A consumer cannot silently upgrade — the return type changed — so the new
import path is the correct, explicit gate.

Create `pricing.go` (the v1 surface):

```go
package pricing

import "errors"

// ErrDivideByZero is the v1 sentinel. v1's contract is (int, error).
var ErrDivideByZero = errors.New("pricing: divide by zero")

// Div is the v1 contract: an integer quotient and a sentinel error.
func Div(a, b int) (int, error) {
	if b == 0 {
		return 0, ErrDivideByZero
	}
	return a / b, nil
}
```

Create `v2/pricing.go` (the v2 surface — import path `example.com/billing/pricing/v2`):

```go
package pricing

import "fmt"

// Quotient is the v2 result. The breaking change from v1 is the return type:
// Div now yields the remainder alongside the value.
type Quotient struct {
	Value     int
	Remainder int
}

// DivError is the v2 rich error type that replaces the v1 sentinel; it carries
// the operands for diagnostics.
type DivError struct {
	Dividend int
	Divisor  int
}

func (e *DivError) Error() string {
	return fmt.Sprintf("pricing/v2: cannot divide %d by %d", e.Dividend, e.Divisor)
}

// Div is the v2 contract: a structured Quotient and a typed error.
func Div(a, b int) (Quotient, error) {
	if b == 0 {
		return Quotient{}, &DivError{Dividend: a, Divisor: b}
	}
	return Quotient{Value: a / b, Remainder: a % b}, nil
}
```

### The runnable demo

The demo imports both majors under aliases and uses them in one `main` — the
concrete proof that v1 and v2 coexist.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	pricingv1 "example.com/billing/pricing"
	pricingv2 "example.com/billing/pricing/v2"
)

func main() {
	if _, err := pricingv1.Div(10, 0); errors.Is(err, pricingv1.ErrDivideByZero) {
		fmt.Println("v1:", err)
	}

	q, _ := pricingv2.Div(10, 3)
	fmt.Printf("v2: value=%d remainder=%d\n", q.Value, q.Remainder)

	if _, err := pricingv2.Div(10, 0); err != nil {
		fmt.Println("v2:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
v1: pricing: divide by zero
v2: value=3 remainder=1
v2: pricing/v2: cannot divide 10 by 0
```

### Tests

The tests assert both signatures against their distinct import paths and pin the
suffix rule at the string level with `golang.org/x/mod/module` and `.../semver` —
the same functions the toolchain uses internally to enforce SIV.

Create `pricing_test.go`:

```go
package pricing

import (
	"errors"
	"fmt"
	"testing"

	pricingv2 "example.com/billing/pricing/v2"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

func TestV1DivSentinel(t *testing.T) {
	t.Parallel()
	if _, err := Div(10, 0); !errors.Is(err, ErrDivideByZero) {
		t.Fatalf("v1 Div(10, 0) err = %v, want ErrDivideByZero", err)
	}
}

func TestV2DivRichError(t *testing.T) {
	t.Parallel()
	q, err := pricingv2.Div(10, 3)
	if err != nil {
		t.Fatalf("v2 Div(10, 3) unexpected error: %v", err)
	}
	if q.Value != 3 || q.Remainder != 1 {
		t.Fatalf("v2 Div(10, 3) = %+v, want {Value:3 Remainder:1}", q)
	}

	_, err = pricingv2.Div(10, 0)
	var de *pricingv2.DivError
	if !errors.As(err, &de) {
		t.Fatalf("v2 Div(10, 0) err = %v, want *DivError", err)
	}
	if de.Divisor != 0 || de.Dividend != 10 {
		t.Fatalf("DivError = %+v, want {Dividend:10 Divisor:0}", de)
	}
}

func TestSplitPathVersion(t *testing.T) {
	t.Parallel()
	prefix, pathMajor, ok := module.SplitPathVersion("example.com/billing/pricing/v2")
	if !ok {
		t.Fatal("SplitPathVersion reported the v2 path invalid")
	}
	if prefix != "example.com/billing/pricing" {
		t.Fatalf("prefix = %q, want example.com/billing/pricing", prefix)
	}
	if pathMajor != "/v2" {
		t.Fatalf("pathMajor = %q, want /v2", pathMajor)
	}
}

func TestV1PathHasNoSuffix(t *testing.T) {
	t.Parallel()
	_, pathMajor, ok := module.SplitPathVersion("example.com/billing/pricing")
	if !ok || pathMajor != "" {
		t.Fatalf("v1 path yielded pathMajor=%q ok=%v, want empty/true", pathMajor, ok)
	}
	if got := semver.Major("v2.0.0"); got != "v2" {
		t.Fatalf("semver.Major(v2.0.0) = %q, want v2", got)
	}
}

func Example() {
	q, _ := pricingv2.Div(100, 7)
	fmt.Printf("%d r%d\n", q.Value, q.Remainder)
	// Output: 14 r2
}
```

## Review

The design is correct when v1 and v2 are reachable through *different* import paths
and both compile into the demo binary at once — that is Semantic Import Versioning
working, not a workaround. v2 earns its major because the return type changed:
`(int, error)` became `(Quotient, error)` with a typed `*DivError`, a change no
consumer can absorb silently, so the new import path is the explicit opt-in.
`TestSplitPathVersion` pins the mechanical rule the toolchain enforces — a v2+ path
splits off a `/v2` pathMajor while a v1 path has none — and `semver.Major` confirms
`v2.0.0` lives on the `v2` line. The mistake to avoid is releasing the v2 API under
the un-suffixed path; the compiler rejects it precisely because the path is the
import, and the path still said v1.

## Resources

- [Go blog: Go Modules: v2 and Beyond](https://go.dev/blog/v2-go-modules) — the canonical guide to releasing a v2+ module.
- [`module.SplitPathVersion`](https://pkg.go.dev/golang.org/x/mod/module#SplitPathVersion) — splitting a path into prefix and `/vN`.
- [Go Modules Reference: Major version suffixes](https://go.dev/ref/mod#major-version-suffixes) — the normative rule.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-runtime-build-version-endpoint.md](04-runtime-build-version-endpoint.md)
