# Exercise 3: When errors.As Still Wins — A Migration and Trade-off Study

`errors.AsType` is the better default, but "migrate every `errors.As`" is wrong
advice. This exercise builds the thing `AsType` makes elegant — a generic
field-extraction combinator — and, in the same module, the thing it *cannot*
express: matching into an interface that does not embed `error`, which stays the
job of `errors.As`. The deliverable is a decision rule, backed by code.

This module is fully self-contained: its own module, the helpers, a demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
fielderr/                  independent module: example.com/fielderr
  go.mod                   go 1.26 (errors.AsType needs it)
  fielderr.go              Field[E,T] combinator (AsType) + CodeOf (errors.As)
  cmd/
    demo/
      main.go              extracts a path via AsType, a code via As
  fielderr_test.go         extraction tests + pointer/value silent-miss + Example
```

Files: `fielderr.go`, `cmd/demo/main.go`, `fielderr_test.go`.
Implement: `Field[E error, T any](err, get)` built on `errors.AsType`; `PathOf` over it; a non-error `Coder` interface and `CodeOf` built on `errors.As`.
Test: extraction over wrapped trees, the silent pointer/value no-match, the generic combinator with a second field, and an `Example`.
Verify: `go test -count=1 -race ./...`

Set up the module. `errors.AsType` requires Go 1.26:

```bash
mkdir -p ~/go-exercises/fielderr/cmd/demo
cd ~/go-exercises/fielderr
go mod init example.com/fielderr
go mod edit -go=1.26
```

### Where AsType wins: a generic extraction combinator

Because `AsType` *returns* the typed value instead of writing through a pointer,
it composes. You can write a single generic helper that matches a type and, on a
hit, applies a function to the matched value:

```go
func Field[E error, T any](err error, get func(E) T) (T, bool) {
	if e, ok := errors.AsType[E](err); ok {
		return get(e), true
	}
	var zero T
	return zero, false
}
```

`Field(err, func(pe *fs.PathError) string { return pe.Path })` extracts the path
from anywhere in the tree in one expression. This combinator is awkward to build
on `errors.As`: `As` needs a concrete addressable target variable, which you
cannot name generically without a second type parameter and an out-pointer dance,
and its `target any` argument defeats the type inference that makes `Field`'s
call sites read cleanly. When the shape of the work is "find a typed error and
pull a value out of it", `AsType` is strictly the better tool, and `Field` is the
idiom to reach for.

### Where errors.As still wins: a non-error interface target

Now the case that forces `As`. Suppose your errors expose a stable machine code
through a small interface that is deliberately *not* an error:

```go
type Coder interface {
	Code() string
}
```

`Coder` has no `Error` method, so it does not satisfy `AsType`'s `E error`
constraint — `errors.AsType[Coder](err)` does not compile. `errors.As`, by
contrast, accepts *any* interface type as its target: `var c Coder;
errors.As(err, &c)` walks the tree and, on the first error whose dynamic type
implements `Coder`, sets `c`. This is not a niche curiosity. Cross-cutting
capability interfaces (`interface{ Temporary() bool }`, `interface{ Fields()
[]slog.Attr }`, a `Coder`) are a real pattern, and matching them is precisely the
job that remains with `errors.As`. If you blanket-migrate to `AsType`, this call
site stops compiling and you have lost a capability the language still supports.

Two more retention cases are worth stating even though they are not shown as
separate functions here. First, a hot loop that reuses one preallocated target:
`var pe *fs.PathError` declared once and passed to `errors.As` on each iteration
avoids re-establishing a target per call; the value-returning `AsType` is fine but
does not offer that reuse knob. Second, a target type known only at run time (you
hold a `reflect.Type` or an `any`, not a compile-time `E`): `AsType` needs its
type as a parameter, so `As` with a dynamically constructed pointer is the only
option. The decision rule: reach for `AsType` when you know the type at compile
time and want the value back; keep `As` for a non-error interface target, for
target reuse in a measured hot path, or for a runtime-determined type.

Create `fielderr.go`:

```go
// Package fielderr shows where errors.AsType shines (a generic field-extraction
// combinator) and where errors.As is still the right tool (matching into an
// interface that does not embed error).
package fielderr

import (
	"errors"
	"io/fs"
)

// Field extracts a value of type T from the first error in err's tree that
// matches type E, by applying get to it. It returns (zero, false) when no such
// error is present. This combinator is only clean because AsType RETURNS the
// typed value: the getter can be applied inline, with no out-pointer to declare.
func Field[E error, T any](err error, get func(E) T) (T, bool) {
	if e, ok := errors.AsType[E](err); ok {
		return get(e), true
	}
	var zero T
	return zero, false
}

// PathOf returns the filesystem path from an *fs.PathError anywhere in err's
// tree. It is a one-liner over Field, matching the exact wrapped pointer type.
func PathOf(err error) (string, bool) {
	return Field(err, func(pe *fs.PathError) string { return pe.Path })
}

// Coder is a NON-error interface: it has no Error method. An error type may also
// implement it to expose a machine-readable code. Because Coder does not embed
// error, it does NOT satisfy the [E error] constraint of AsType, so extracting
// it is a job for errors.As, whose target may be any interface type.
type Coder interface {
	Code() string
}

// CodeOf returns the code from the first error in err's tree that implements the
// non-error Coder interface. This is the case AsType cannot express: it must use
// errors.As with an interface target.
func CodeOf(err error) (string, bool) {
	var c Coder
	if errors.As(err, &c) {
		return c.Code(), true
	}
	return "", false
}

// APIError is a domain error that also carries a stable machine code, so it
// implements both error and Coder.
type APIError struct {
	code    string
	message string
}

func (e *APIError) Error() string { return e.message }
func (e *APIError) Code() string  { return e.code }

// NewAPIError constructs an APIError with a code and message.
func NewAPIError(code, message string) *APIError {
	return &APIError{code: code, message: message}
}
```

### The runnable demo

The demo puts both tools side by side: `PathOf` (built on `AsType`) pulls a path
out of a wrapped `*fs.PathError`, and `CodeOf` (built on `As`) pulls a machine
code out through the non-error `Coder` interface.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io/fs"

	"example.com/fielderr"
)

func main() {
	// AsType shines: extract a field from a typed error via the generic helper.
	pe := &fs.PathError{Op: "open", Path: "/etc/app/config.yaml", Err: fs.ErrNotExist}
	wrapped := fmt.Errorf("load config: %w", pe)

	if path, ok := fielderr.PathOf(wrapped); ok {
		fmt.Printf("failed path: %s\n", path)
	}

	// As is still required: match into a non-error interface (Coder).
	apiErr := fielderr.NewAPIError("ACCOUNT_LOCKED", "account is locked")
	wrapped2 := fmt.Errorf("authorize: %w", apiErr)

	if code, ok := fielderr.CodeOf(wrapped2); ok {
		fmt.Printf("machine code: %s\n", code)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
failed path: /etc/app/config.yaml
machine code: ACCOUNT_LOCKED
```

### Tests

`TestPathOf` and `TestFieldGeneric` prove the `AsType` combinator extracts fields
from wrapped trees and reports `(zero, false)` on a miss and on `nil`.
`TestCodeOf` proves the `errors.As` path reaches a non-error interface — the case
that would not compile with `AsType`. `TestPointerValueMismatch` is the important
one: `valErr` has a *value* receiver, so both `valErr` and `*valErr` satisfy
`error`; the error is wrapped as `*valErr`, and asking `AsType[valErr]` compiles
and *silently returns false*, while `AsType[*valErr]` matches. That silent miss is
the most common quiet bug, and the test locks the behavior in.

Create `fielderr_test.go`:

```go
package fielderr

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"
)

// valErr has a VALUE receiver, so both valErr and *valErr satisfy error. This
// is what makes a pointer/value mismatch a SILENT no-match rather than a compile
// error.
type valErr struct{ msg string }

func (e valErr) Error() string { return e.msg }

func TestPathOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		wantPath string
		wantOK   bool
	}{
		{
			name:     "wrapped path error",
			err:      fmt.Errorf("load: %w", &fs.PathError{Op: "open", Path: "/etc/x", Err: fs.ErrNotExist}),
			wantPath: "/etc/x",
			wantOK:   true,
		},
		{
			name:     "no path error present",
			err:      errors.New("plain"),
			wantPath: "",
			wantOK:   false,
		},
		{
			name:     "nil error",
			err:      nil,
			wantPath: "",
			wantOK:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := PathOf(tc.err)
			if ok != tc.wantOK || got != tc.wantPath {
				t.Errorf("PathOf() = %q,%v; want %q,%v", got, ok, tc.wantPath, tc.wantOK)
			}
		})
	}
}

func TestCodeOf(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("authorize: %w", NewAPIError("ACCOUNT_LOCKED", "locked"))
	code, ok := CodeOf(err)
	if !ok || code != "ACCOUNT_LOCKED" {
		t.Fatalf("CodeOf() = %q,%v; want ACCOUNT_LOCKED,true", code, ok)
	}

	if _, ok := CodeOf(errors.New("no code here")); ok {
		t.Fatal("CodeOf() found a code where none exists")
	}
}

// TestPointerValueMismatch demonstrates the single most common silent failure:
// the error is wrapped as *valErr, but asking AsType for the value type valErr
// compiles (value receiver) and silently returns false.
func TestPointerValueMismatch(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("op: %w", &valErr{msg: "boom"})

	if _, ok := errors.AsType[valErr](err); ok {
		t.Error("AsType[valErr] unexpectedly matched a *valErr tree")
	}
	if _, ok := errors.AsType[*valErr](err); !ok {
		t.Error("AsType[*valErr] failed to match the wrapped pointer")
	}
}

// TestFieldGeneric shows the combinator working with a different type parameter.
func TestFieldGeneric(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("x: %w", &fs.PathError{Op: "stat", Path: "/tmp/y", Err: fs.ErrPermission})
	op, ok := Field(err, func(pe *fs.PathError) string { return pe.Op })
	if !ok || op != "stat" {
		t.Fatalf("Field(Op) = %q,%v; want stat,true", op, ok)
	}
}

func ExampleCodeOf() {
	err := fmt.Errorf("authorize: %w", NewAPIError("RATE_LIMITED", "slow down"))
	code, ok := CodeOf(err)
	fmt.Println(code, ok)
	// Output: RATE_LIMITED true
}
```

## Review

The rule this exercise teaches is the whole point: use `AsType` when you know the
type at compile time and want the value back — the `Field` combinator is the
clean expression of that — and keep `errors.As` for a non-error interface target,
for reusing one target across a measured hot loop, or for a type known only at run
time. The failure mode to internalize is the silent pointer/value miss: because a
value-receiver error type makes both `T` and `*T` satisfy `error`, `AsType[T]`
against a wrapped `*T` compiles and returns `(zero, false)` with no diagnostic, so
match the exact wrapped form (the pointer, for a struct error). Confirm with
`go test -race ./...`; `TestPointerValueMismatch` proves the miss, `TestCodeOf`
proves the `As`-only capability, and the demo shows both tools doing their proper
jobs.

## Resources

- [errors package (AsType vs As)](https://pkg.go.dev/errors) — the doc note that As is equivalent to AsType but sets a target and does not require it to implement error.
- [Go source: errors/wrap.go](https://go.dev/src/errors/wrap.go) — the AsType and As implementations and their doc comments.
- [proposal: errors.AsType (golang/go #51945)](https://github.com/golang/go/issues/51945) — the design discussion, including why the constraint is `E error`.
- [Type-safe error checking with errors.AsType](https://antonz.org/accepted/errors-astype/) — a walkthrough of the feature and its trade-offs.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-retry-classification-interface-match.md](02-retry-classification-interface-match.md) | Next: [../15-new-expr-initialized-allocation/00-concepts.md](../15-new-expr-initialized-allocation/00-concepts.md)
