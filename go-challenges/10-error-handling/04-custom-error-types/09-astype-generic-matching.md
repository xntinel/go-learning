# Exercise 9: Ergonomic Typed Extraction with errors.AsType[T] (Go 1.26)

Go 1.26 adds `errors.AsType[E error](err error) (E, bool)`, the type-safe
successor to `errors.As`: it returns the typed value directly, with no pointer
target, no reflection, and no runtime pointer-level panic. This module refactors
the extraction pattern from the earlier modules to `AsType`, side by side with an
`errors.As` helper that proves equivalent behavior, and shows the failure modes
`AsType` removes.

This module is fully self-contained: its own module, code, demo, and tests. It
requires Go 1.26 for `errors.AsType`.

## What you'll build

```text
astypeextract/             independent module: example.com/astypeextract
  go.mod                   go 1.26 (errors.AsType needs it)
  astypeextract.go         APIError, RepositoryError; StatusOf via AsType; asVsAsType
  cmd/
    demo/
      main.go              extracts a status and a kind from a wrapped chain
  astypeextract_test.go    AsType hit/miss, As-agreement, zero-on-miss
```

Files: `astypeextract.go`, `cmd/demo/main.go`, `astypeextract_test.go`.
Implement: two typed errors, a `StatusOf` built on `errors.AsType[*APIError]`, and an `asVsAsType` helper showing the equivalent `errors.As` form.
Test: a wrapped chain containing an `*APIError` and a `*RepositoryError` yields the right extraction from each; an absent type returns `(zero, false)`; `AsType` and `errors.As` agree on the same chain.
Verify: `go test -count=1 -race ./...`

Set up the module. `errors.AsType` requires Go 1.26:

```bash
go mod edit -go=1.26
```

### What AsType is and why it is better

`errors.As` writes the matched error through a pointer you supply:

```go
var ae *APIError
if errors.As(err, &ae) { /* use ae */ }
```

You declare a target, pass its address, and `As` uses reflection to check
assignability and write through it. Three things can go wrong: you can pass the
wrong pointer level (`errors.As(err, ae)` instead of `&ae`) and get a *runtime
panic*; the reflection has a measurable cost on hot paths; and the `target any`
argument defeats the compiler's ability to check the type at the call site.

`errors.AsType` is the generic replacement:

```go
if ae, ok := errors.AsType[*APIError](err); ok { /* use ae */ }
```

The type is a *type parameter*, so it is checked at compile time; the value is
*returned*, so there is no target pointer to get wrong and no reflection dance; and
it is documented as allocation-free and several times faster than `As` on typical
chains. The wrong-pointer-level panic simply cannot be written. In new Go 1.26
code, `AsType` is the default for "find a typed error and use it".

The constraint is `E error`: the type parameter must itself satisfy `error`. That
is why `AsType` cannot target a non-error interface (a plain `interface{ Code()
string }`) — that remains the job of `errors.As`, which accepts any interface
target. The decision rule: `AsType` when you know an *error* type at compile time
and want the value; `errors.As` for a non-error interface target, target reuse in a
measured hot loop, or a type known only at run time.

### Equivalent behavior, proven side by side

`asVsAsType` runs both forms on the same error and returns whether they agree, so a
test can assert the migration is behavior-preserving. Both walk the wrapped tree
depth-first; both match the first error whose concrete type is `*APIError`; both
report the same hit/miss. `AsType` just expresses it without the target variable.

### The pre-1.26 fallback

Because `AsType` needs Go 1.26, code that must build on older toolchains keeps the
`errors.As` form. The migration is mechanical and local — replace `var t *T;
errors.As(err, &t)` with `t, ok := errors.AsType[*T](err)` — so a codebase can
adopt it file by file once its minimum toolchain reaches 1.26, leaving `errors.As`
wherever a non-error interface target or a runtime type is genuinely needed.

Create `astypeextract.go`:

```go
// Package astypeextract refactors typed-error extraction to the Go 1.26
// errors.AsType generic, side by side with the equivalent errors.As form.
package astypeextract

import (
	"errors"
	"fmt"
)

// APIError carries an HTTP status. RepositoryError carries a domain kind. Both are
// pointer-receiver error types, so the concrete type in any chain is the pointer.
type APIError struct {
	Status int
	Code   string
}

func (e *APIError) Error() string { return fmt.Sprintf("%s (%d)", e.Code, e.Status) }

type RepositoryError struct {
	Op   string
	Kind string
}

func (e *RepositoryError) Error() string { return fmt.Sprintf("%s: %s", e.Op, e.Kind) }

// StatusOf extracts an *APIError's status from anywhere in err's tree using the
// Go 1.26 errors.AsType generic: no target pointer, compile-time-checked type.
func StatusOf(err error) (int, bool) {
	if ae, ok := errors.AsType[*APIError](err); ok {
		return ae.Status, true
	}
	return 0, false
}

// KindOf extracts a *RepositoryError's kind via AsType.
func KindOf(err error) (string, bool) {
	if re, ok := errors.AsType[*RepositoryError](err); ok {
		return re.Kind, true
	}
	return "", false
}

// asVsAsType runs both errors.AsType and the equivalent errors.As on the same
// error and reports (matched, agree). It proves the migration is behavior-
// preserving: both forms find the same *APIError in the same tree.
func asVsAsType(err error) (matched, agree bool) {
	byType, okType := errors.AsType[*APIError](err)

	var byAs *APIError
	okAs := errors.As(err, &byAs)

	agree = okType == okAs && byType == byAs
	return okType, agree
}
```

### The runnable demo

The demo wraps a `*RepositoryError` inside an `*APIError` inside a `fmt.Errorf`
context, then pulls each typed error out of the single chain with `AsType`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/astypeextract"
)

func main() {
	// A layered chain: repo error wrapped by an API error wrapped by context.
	repoErr := &astypeextract.RepositoryError{Op: "GetUser", Kind: "not_found"}
	apiErr := &astypeextract.APIError{Status: 404, Code: "USER_NOT_FOUND"}
	chain := fmt.Errorf("handler: %w", fmt.Errorf("%w: %w", apiErr, repoErr))

	if status, ok := astypeextract.StatusOf(chain); ok {
		fmt.Printf("http status: %d\n", status)
	}
	if kind, ok := astypeextract.KindOf(chain); ok {
		fmt.Printf("domain kind: %s\n", kind)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
http status: 404
domain kind: not_found
```

### Tests

`TestStatusAndKind` extracts both typed errors from one wrapped tree.
`TestAbsentTypeZero` asserts a missing type returns `(zero, false)`.
`TestAsAgreesWithAsType` asserts the generic and reflective forms produce identical
results on the same chain — the behavior-preserving guarantee.

Create `astypeextract_test.go`:

```go
package astypeextract

import (
	"errors"
	"fmt"
	"testing"
)

func layeredChain() error {
	repoErr := &RepositoryError{Op: "GetUser", Kind: "not_found"}
	apiErr := &APIError{Status: 404, Code: "USER_NOT_FOUND"}
	return fmt.Errorf("handler: %w", fmt.Errorf("%w: %w", apiErr, repoErr))
}

func TestStatusAndKind(t *testing.T) {
	t.Parallel()
	chain := layeredChain()

	status, ok := StatusOf(chain)
	if !ok || status != 404 {
		t.Errorf("StatusOf = %d,%v; want 404,true", status, ok)
	}
	kind, ok := KindOf(chain)
	if !ok || kind != "not_found" {
		t.Errorf("KindOf = %q,%v; want not_found,true", kind, ok)
	}
}

func TestAbsentTypeZero(t *testing.T) {
	t.Parallel()
	// A chain with an APIError but no RepositoryError.
	chain := fmt.Errorf("wrap: %w", &APIError{Status: 500, Code: "INTERNAL"})

	kind, ok := KindOf(chain)
	if ok || kind != "" {
		t.Errorf("KindOf on absent type = %q,%v; want \"\",false", kind, ok)
	}

	// A plain error with neither typed error present.
	if _, ok := StatusOf(errors.New("plain")); ok {
		t.Error("StatusOf found a status where none exists")
	}
}

func TestAsAgreesWithAsType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{"has api error", layeredChain()},
		{"no api error", fmt.Errorf("wrap: %w", &RepositoryError{Op: "X", Kind: "conflict"})},
		{"plain", errors.New("plain")},
		{"nil", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, agree := asVsAsType(tc.err)
			if !agree {
				t.Error("errors.AsType and errors.As disagreed on the same chain")
			}
		})
	}
}

func ExampleStatusOf() {
	chain := fmt.Errorf("wrap: %w", &APIError{Status: 403, Code: "FORBIDDEN"})
	status, ok := StatusOf(chain)
	fmt.Println(status, ok)
	// Output: 403 true
}
```

## Review

`AsType` is the ergonomic and safe successor to `errors.As` for the common case:
`StatusOf` and `KindOf` extract a typed error from a wrapped tree in one expression,
with the type checked at compile time and the value returned rather than written
through a pointer — the wrong-pointer-level panic that `errors.As` allows cannot be
written here. `TestAsAgreesWithAsType` proves the two forms are behavior-preserving
across hit, miss, and nil, so the migration is mechanical and safe. Keep `errors.As`
for the cases `AsType` cannot express — a non-error interface target, a reused hot-loop
target, or a runtime-determined type — and keep it, too, wherever the toolchain is
still below Go 1.26. Run `go test -race` to confirm; the module pins `go 1.26` in its
go.mod so the toolchain provides `errors.AsType`.

## Resources

- [errors: AsType](https://pkg.go.dev/errors#AsType) — the generic signature and its documented equivalence to `As`.
- [proposal: errors.AsType (golang/go #51945)](https://github.com/golang/go/issues/51945) — the design, including why the constraint is `E error`.
- [Go 1.26 release notes](https://go.dev/doc/go1.26) — `errors.AsType` and the surrounding stdlib changes.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-error-slog-logvaluer.md](08-error-slog-logvaluer.md) | Next: [../05-sentinel-errors/00-concepts.md](../05-sentinel-errors/00-concepts.md)
