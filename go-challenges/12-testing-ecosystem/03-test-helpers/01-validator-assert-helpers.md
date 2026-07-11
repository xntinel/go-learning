# Exercise 1: Assertion Helpers for a Validation Library

Every backend has a validation layer, and every validation layer's test file
drowns in `if err := Validate(x); err != nil { t.Fatalf(...) }`. The cure is a
pair of assertion helpers that call `t.Helper()` so a failure points at the case
that broke, not at the helper. This module builds a small validator and the two
helpers that make its test suite read like a specification.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
validate/                    independent module: example.com/validate
  go.mod                     go 1.26
  validate.go                ErrInvalid sentinel; Validate rejects len < 3
  cmd/
    demo/
      main.go                runs Validate on a valid and an invalid input
  validate_test.go           assertValid/assertInvalid helpers (t.Helper), table subtests, boundary test
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: `ErrInvalid` (a package-level sentinel) and `Validate(string) error` that wraps `ErrInvalid` with `%w` for inputs shorter than three characters.
- Test: `assertValid`/`assertInvalid` helpers calling `t.Helper()` and matching with `errors.Is`, driven by table-driven subtests, plus `TestValidateExactlyThree` pinning the inclusive boundary.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/validate/cmd/demo
cd ~/go-exercises/validate
go mod init example.com/validate
```

### Why a sentinel wrapped with `%w`

`Validate` does not return `ErrInvalid` directly; it returns
`fmt.Errorf("validate: %w", ErrInvalid)`. The `%w` verb *wraps* the sentinel, so
the returned error carries a human-readable prefix (`validate: invalid input`) yet
still satisfies `errors.Is(err, ErrInvalid)`. This is the contract every caller
in the codebase relies on: they branch on the *sentinel*, not on a fragile string
match. The test helper `assertInvalid` therefore matches with `errors.Is(err,
ErrInvalid)` rather than `err != nil` — it asserts *which* error came back, not
merely that some error did. That distinction is what keeps the test honest when a
future refactor introduces a second error kind.

### Why the helpers call `t.Helper()`

`assertValid(t, "abc")` is one line in the test body. When it fails, you want the
failure message to read `validate_test.go:41: Validate("ab") = ... , want nil`
pointing at line 41 where the *case* lives — not at the `t.Fatalf` line inside the
helper, which is the same for every case and tells you nothing. `t.Helper()`, as
the first statement of each helper, is what buys that: the runner skips the helper
frame when computing the `file:line` prefix. Omit it and every failure in the
whole suite points at the same helper line, and you lose the case that broke.

The two helpers make opposite fatal/continue choices only in what they assert, not
in severity: both use `t.Fatalf` because a validation assertion that fails leaves
nothing useful to check afterward in that subtest. Since each case runs in its own
`t.Run` subtest, one `Fatalf` fails only that subtest, and the siblings still run.

Create `validate.go`:

```go
package validate

import (
	"errors"
	"fmt"
)

// ErrInvalid is the sentinel every caller branches on with errors.Is.
var ErrInvalid = errors.New("invalid input")

// Validate rejects inputs shorter than three characters, wrapping ErrInvalid
// with %w so the sentinel remains matchable while carrying context.
func Validate(input string) error {
	if len(input) < 3 {
		return fmt.Errorf("validate: %q too short: %w", input, ErrInvalid)
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/validate"
)

func main() {
	for _, in := range []string{"abc", "ab"} {
		err := validate.Validate(in)
		switch {
		case err == nil:
			fmt.Printf("%q: valid\n", in)
		case errors.Is(err, validate.ErrInvalid):
			fmt.Printf("%q: %v\n", in, err)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"abc": valid
"ab": validate: "ab" too short: invalid input
```

### The tests

`assertValid` and `assertInvalid` are the load-bearing helpers. Each calls
`t.Helper()` first, runs `Validate`, and reports with `t.Fatalf` echoing the
argument so the message names the offending input. `TestValidateValid` and
`TestValidateInvalid` are table-driven, one `t.Run` subtest per case, each subtest
marked parallel because `Validate` is a pure function sharing no state.
`TestValidateExactlyThree` is a dedicated boundary test that pins the inclusive
contract: length exactly three is valid, length two is not — the off-by-one every
validator gets wrong at least once.

Create `validate_test.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"strconv"
	"testing"
)

// assertValid fails the test if input is rejected. t.Helper() attributes the
// failure to the caller's line, not this line.
func assertValid(t *testing.T, input string) {
	t.Helper()
	if err := Validate(input); err != nil {
		t.Fatalf("Validate(%q) = %v, want nil", input, err)
	}
}

// assertInvalid fails the test unless input is rejected with ErrInvalid.
func assertInvalid(t *testing.T, input string) {
	t.Helper()
	if err := Validate(input); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate(%q) = %v, want ErrInvalid", input, err)
	}
}

func TestValidateValid(t *testing.T) {
	t.Parallel()
	for _, c := range []string{"abc", "hello", "xyzw"} {
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			assertValid(t, c)
		})
	}
}

func TestValidateInvalid(t *testing.T) {
	t.Parallel()
	for _, c := range []string{"", "a", "ab"} {
		t.Run("len="+strconv.Itoa(len(c)), func(t *testing.T) {
			t.Parallel()
			assertInvalid(t, c)
		})
	}
}

// TestValidateExactlyThree pins the inclusive boundary: 3 is valid, 2 is not.
func TestValidateExactlyThree(t *testing.T) {
	t.Parallel()
	// exactly 3 characters is accepted; exactly 2 is rejected.
	assertValid(t, "abc")
	assertInvalid(t, "ab")
}

func ExampleValidate() {
	err := Validate("ab")
	fmt.Println(errors.Is(err, ErrInvalid))
	// Output: true
}
```

## Review

The validator is correct when `Validate` returns `nil` for length ≥ 3 and an
error satisfying `errors.Is(err, ErrInvalid)` otherwise — the `%w` wrap is what
makes that `errors.Is` true, and dropping it to `%v` would break every caller and
`assertInvalid` alike. The helpers are correct when a deliberately broken
`Validate` (say, `len(input) < 4`) makes the failure point at the *subtest's*
line, not the helper's: that is the entire payoff of `t.Helper()`. Confirm it by
temporarily breaking the boundary and reading the `file:line` in the failure. The
boundary test is the specification of the "exactly three" contract; without it a
refactor to `<=` or `<` for the length check could slip through the table cases,
which never probe the exact edge. Run `go test -race -count=1` — there is no
shared state, so the parallel subtests must stay green.

## Resources

- [testing.T.Helper](https://pkg.go.dev/testing#T.Helper) — how helper frames are skipped in failure attribution.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel through `%w`.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `%w` wrapping and the `Is`/`As` model.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-testing-tb-assert-package.md](02-testing-tb-assert-package.md)
