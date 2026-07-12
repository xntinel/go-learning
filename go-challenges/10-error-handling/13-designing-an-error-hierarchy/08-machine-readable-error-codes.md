# Exercise 8: A Stable Error-Code Catalog via a Coder Interface

Human-facing messages get reworded; a client that matches on `err.Error() == "user
not found"` breaks the day someone edits the string. This exercise gives every
category a stable machine-readable code through a small `Coder` interface, extracts
it with `errors.As` (and the Go 1.25 generic `errors.AsType`), and uses it to power
a JSON envelope and a structured log field that survive rewording and wrapping.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
error-codes/                       module example.com/error-codes
  go.mod
  codes.go                         Coder iface; CodedError; NotFound/Exists/Invalid; CodeOf; CodeOfTyped; Envelope
  cmd/demo/main.go                 code survives wrapping; no-coder defaults; JSON envelope
  codes_test.go                    code after %w wrap; INTERNAL default; As vs AsType agree; reworded msg same code
```

- Files: `codes.go`, `cmd/demo/main.go`, `codes_test.go`.
- Implement: a `Coder` interface (`Code() string`), a `CodedError` implementing it, constructors returning `USER_NOT_FOUND`/`USER_EXISTS`/`USER_INVALID`, a `CodeOf` via `errors.As`, a `CodeOfTyped` via `errors.AsType`, and an `Envelope`.
- Test: `CodeOf(NotFound(...))` is `USER_NOT_FOUND` even after `fmt.Errorf("svc: %w", ...)`; an error with no `Coder` yields `INTERNAL`; a reworded message does not change the code; `errors.As` and `errors.AsType` agree.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/13-designing-an-error-hierarchy/08-machine-readable-error-codes/cmd/demo
cd go-solutions/10-error-handling/13-designing-an-error-hierarchy/08-machine-readable-error-codes
```

### The code is the contract, not the prose

A machine-readable code decouples the wire contract from the message text. The
`Coder` interface is deliberately tiny — a single `Code() string` — and every domain
error implements it. `CodeOf(err)` extracts the code from anywhere in the chain and
defaults to `INTERNAL` when nothing implements `Coder`, so an unmapped or foreign
error still produces a valid envelope. Because the extraction walks the chain,
wrapping with `%w` at three boundaries does not change the code, and neither does
rewording the message — the client and the log pipeline see a stable identifier no
matter how the human-facing text evolves.

There are two ways to do the extraction, and the difference is instructive.
`CodeOf` uses `errors.As(err, &c)` where `c` is a `Coder`. This works even though
`Coder` is *not* an error interface: `errors.As` accepts a pointer to *any*
interface as its target, and matches the first error in the chain whose type is
assignable to it. `CodeOfTyped` uses the generic `errors.AsType[E error](err)
(E, bool)` added in Go 1.25. It is the cleaner spelling — no zero target to declare
first — but it carries a constraint: the type parameter `E` must itself satisfy
`error`. So you can write `errors.AsType[*CodedError](err)` (the concrete type
implements `error`), but you *cannot* write `errors.AsType[Coder](err)`, because
`Coder` has no `Error()` method and fails the `E error` constraint. When your target
is a non-error interface, `errors.As` with a pointer to the interface is the tool;
when it is a concrete error type, `errors.AsType` is the ergonomic one. Both find
the same `*CodedError` here, so both return the same code.

The `Envelope` ties it together: `NewEnvelope(err)` puts the stable `Code` and the
current `Message` into a JSON body, so the API's machine contract (the code) and its
human hint (the message) travel together but evolve independently.

Create `codes.go`:

```go
package codes

import (
	"errors"
	"fmt"
)

// Coder is the stable wire contract: a machine-readable code that survives
// message rewording and wrapping. Note it is NOT an error interface itself.
type Coder interface {
	Code() string
}

// CodedError is a domain error that carries a stable code alongside its message
// and an optional wrapped cause.
type CodedError struct {
	code  string
	msg   string
	cause error
}

func (e *CodedError) Error() string {
	if e.cause != nil {
		return e.msg + ": " + e.cause.Error()
	}
	return e.msg
}

func (e *CodedError) Code() string  { return e.code }
func (e *CodedError) Unwrap() error { return e.cause }

func NotFound(id string) *CodedError {
	return &CodedError{code: "USER_NOT_FOUND", msg: fmt.Sprintf("user %q not found", id)}
}

func Exists(id string) *CodedError {
	return &CodedError{code: "USER_EXISTS", msg: fmt.Sprintf("user %q already exists", id)}
}

func Invalid(id, field string) *CodedError {
	return &CodedError{code: "USER_INVALID", msg: fmt.Sprintf("user %q invalid: field %s", id, field)}
}

// CodeOf extracts the stable code from any error in err's chain via errors.As on
// the Coder interface, defaulting to INTERNAL. Because the target is a non-error
// interface, this uses errors.As with a pointer to the interface.
func CodeOf(err error) string {
	var c Coder
	if errors.As(err, &c) {
		return c.Code()
	}
	return "INTERNAL"
}

// CodeOfTyped is the same lookup via the generic errors.AsType on the concrete
// type. AsType's type parameter must itself satisfy error, so it targets
// *CodedError (which implements error), not the Coder interface (which does not).
func CodeOfTyped(err error) string {
	if ce, ok := errors.AsType[*CodedError](err); ok {
		return ce.Code()
	}
	return "INTERNAL"
}

// Envelope is the JSON body an API returns for an error.
type Envelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func NewEnvelope(err error) Envelope {
	return Envelope{Code: CodeOf(err), Message: err.Error()}
}
```

### The runnable demo

The demo shows the code surviving a two-level `%w` wrap through both extractors, an
unmapped error defaulting to `INTERNAL`, and the JSON envelope carrying code and
message together.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/error-codes"
)

func main() {
	wrapped := fmt.Errorf("svc: load: %w", codes.NotFound("u9"))
	fmt.Printf("code (As):      %s\n", codes.CodeOf(wrapped))
	fmt.Printf("code (AsType):  %s\n", codes.CodeOfTyped(wrapped))

	plain := errors.New("disk full")
	fmt.Printf("no coder:       %s\n", codes.CodeOf(plain))

	env := codes.NewEnvelope(codes.Invalid("u9", "email"))
	fmt.Printf("envelope:       code=%s message=%q\n", env.Code, env.Message)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
code (As):      USER_NOT_FOUND
code (AsType):  USER_NOT_FOUND
no coder:       INTERNAL
envelope:       code=USER_INVALID message="user \"u9\" invalid: field email"
```

### Tests

The tests pin the contract: the code is stable across a `%w` wrap, a no-coder error
defaults to `INTERNAL`, a reworded message keeps the same code (the whole point of
decoupling wire from prose), and `errors.As` and `errors.AsType` agree.

Create `codes_test.go`:

```go
package codes

import (
	"errors"
	"fmt"
	"testing"
)

func TestCodeSurvivesWrapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"not found", NotFound("u1"), "USER_NOT_FOUND"},
		{"exists", Exists("u1"), "USER_EXISTS"},
		{"invalid", Invalid("u1", "email"), "USER_INVALID"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wrapped := fmt.Errorf("handler: svc: %w", tc.err)
			if got := CodeOf(wrapped); got != tc.want {
				t.Errorf("CodeOf(wrapped) = %s; want %s", got, tc.want)
			}
			if got := CodeOfTyped(wrapped); got != tc.want {
				t.Errorf("CodeOfTyped(wrapped) = %s; want %s", got, tc.want)
			}
		})
	}
}

func TestNoCoderDefaultsInternal(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrap: %w", errors.New("raw failure"))
	if got := CodeOf(err); got != "INTERNAL" {
		t.Fatalf("CodeOf = %s; want INTERNAL", got)
	}
	if got := CodeOfTyped(err); got != "INTERNAL" {
		t.Fatalf("CodeOfTyped = %s; want INTERNAL", got)
	}
}

func TestRewordedMessageKeepsCode(t *testing.T) {
	t.Parallel()
	// Two errors with the same code but different messages must share a code.
	a := &CodedError{code: "USER_NOT_FOUND", msg: "user not found"}
	b := &CodedError{code: "USER_NOT_FOUND", msg: "no such account, please sign up"}
	if CodeOf(a) != CodeOf(b) {
		t.Fatalf("reworded message changed the code: %s vs %s", CodeOf(a), CodeOf(b))
	}
	if a.Error() == b.Error() {
		t.Fatal("test setup wrong: messages should differ")
	}
}

func TestAsAndAsTypeAgree(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("ctx: %w", Exists("u7"))
	if CodeOf(err) != CodeOfTyped(err) {
		t.Fatalf("As=%s AsType=%s; want equal", CodeOf(err), CodeOfTyped(err))
	}
}

func Example() {
	env := NewEnvelope(fmt.Errorf("svc: %w", NotFound("u1")))
	fmt.Println(env.Code)
	// Output: USER_NOT_FOUND
}
```

## Review

The catalog is correct when the code is invariant under the two things that change
most in a codebase: wrapping and wording. `CodeOf` walking the chain gives you
wrap-invariance, and storing the code separately from the message gives you
word-invariance — a reworded `msg` must yield the same `Code()`. The `errors.As`
versus `errors.AsType` distinction is the real lesson: `AsType` is the cleaner
generic spelling but its type parameter must satisfy `error`, so it targets the
concrete `*CodedError`, while a non-error interface target like `Coder` still needs
`errors.As(err, &c)`. Both find the same value here. Ship the code on the envelope
and the log field; keep the message for humans; never make a client parse
`Error()`.

## Resources

- [`errors.As`](https://pkg.go.dev/errors#As) — extraction, including targeting a non-error interface via a pointer.
- [`errors.AsType`](https://pkg.go.dev/errors#AsType) — the Go 1.25 generic extractor and its `E error` constraint.
- [Go 1.25 release notes](https://go.dev/doc/go1.25) — the addition of `errors.AsType`.

---

Back to [07-context-error-distinction.md](07-context-error-distinction.md) | Next: [09-wrap-once-log-once.md](09-wrap-once-log-once.md)
