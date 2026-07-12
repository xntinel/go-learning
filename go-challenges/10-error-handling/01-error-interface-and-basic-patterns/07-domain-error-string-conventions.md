# Exercise 7: A Payments Domain Error That Follows Go Error-String Style

`Error() string` is the single method of the `error` interface, and its output is
an API and log concern, not a UI string. Go has firm conventions for that output:
lowercase first letter, no trailing punctuation or newline, and enough context to
be actionable once a caller wraps it. This exercise builds a `PaymentError` and a
self-linting test that mechanically enforces those conventions.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
paymenterr/                  independent module: example.com/paymenterr
  go.mod                     go 1.26
  payment.go                 PaymentError{AmountCents, Reason}; Error() string
  cmd/
    demo/
      main.go                runnable demo: render one payment error, wrap it
  payment_test.go            convention-lint test + compile-time assertion + determinism
```

- Files: `payment.go`, `cmd/demo/main.go`, `payment_test.go`.
- Implement: a `PaymentError` type carrying an amount (in cents) and a reason, with `Error() string` producing a message that follows Go conventions and includes the amount.
- Test: a convention-lint test asserting the message starts lowercase, has no trailing `.`/`!`/`?`/newline, and includes the amount; a compile-time `var _ error = (*PaymentError)(nil)` assertion; a determinism test that fixed inputs yield a stable string.
- Verify: `go test -count=1 -race ./...`

### Why the message string is an interface concern, not a UI string

The output of `Error()` lands in three places: server logs, structured-logging
fields, and the message a *caller* produces when it wraps the error with
`fmt.Errorf("charge order 42: %w", err)`. All three are read by engineers and
tools, never by an end user. That is why the conventions exist. A lowercase first
letter means the message reads correctly mid-sentence after a wrapping prefix:
`charge order 42: card declined, amount 4999 cents` rather than the jarring
`charge order 42: Card declined...`. No trailing period means the same — a period
in the middle of a wrapped chain (`charge order 42: card declined.: timeout`)
reads as broken. No newline means the line stays one log record instead of
splitting into two. And the message must carry actionable context — here the
amount — so an operator can act on the log line without opening a debugger.

Because these are conventions the compiler cannot enforce, the reliable way to
keep them is a test that lints the produced string. `PaymentError.Error()` is
built with `fmt.Sprintf` from the amount and reason, and the lint test scans the
result: first rune not uppercase (via `unicode.IsUpper`), no trailing sentence
punctuation or newline (via `strings.HasSuffix` against each forbidden suffix),
and the amount present (via `strings.Contains`). A `var _ error = (*PaymentError)(nil)`
line is a compile-time assertion that the type satisfies `error` — if someone
renames `Error` or changes its signature, the package stops compiling rather than
failing subtly at a call site. The determinism test pins that fixed inputs yield a
byte-identical string, so logs and any test that greps them stay stable.

Create `payment.go`:

```go
package paymenterr

import "fmt"

// PaymentError describes a failed charge. AmountCents avoids float rounding by
// storing money as an integer number of cents.
type PaymentError struct {
	AmountCents int64
	Reason      string
}

// Compile-time assertion that *PaymentError satisfies error. If Error's name or
// signature drifts, the package fails to compile here.
var _ error = (*PaymentError)(nil)

// Error follows Go conventions: lowercase first word, no trailing punctuation or
// newline, and it includes the amount so a log line is actionable on its own.
func (e *PaymentError) Error() string {
	return fmt.Sprintf("payment declined: %s, amount %d cents", e.Reason, e.AmountCents)
}
```

### The runnable demo

The demo renders one payment error directly and once more wrapped by a caller, so
the lowercase-and-no-period conventions are visible in a real concatenated chain.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/paymenterr"
)

func main() {
	err := &paymenterr.PaymentError{AmountCents: 4999, Reason: "insufficient funds"}
	fmt.Println(err.Error())

	wrapped := fmt.Errorf("charge order 42: %w", err)
	fmt.Println(wrapped.Error())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
payment declined: insufficient funds, amount 4999 cents
charge order 42: payment declined: insufficient funds, amount 4999 cents
```

### Tests

The convention-lint test is the substance: it renders a `PaymentError` and asserts
each rule mechanically, so a future edit that capitalizes the message or appends a
period fails immediately. The determinism test renders the same error twice and
compares. The compile-time assertion lives in `payment.go`, and a small runtime
test also confirms the interface satisfaction for completeness.

Create `payment_test.go`:

```go
package paymenterr

import (
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestErrorStringFollowsConventions(t *testing.T) {
	t.Parallel()

	e := &PaymentError{AmountCents: 4999, Reason: "insufficient funds"}
	msg := e.Error()

	first, _ := utf8.DecodeRuneInString(msg)
	if unicode.IsUpper(first) {
		t.Fatalf("message %q starts with an uppercase letter", msg)
	}
	for _, suffix := range []string{".", "!", "?", "\n"} {
		if strings.HasSuffix(msg, suffix) {
			t.Fatalf("message %q ends with forbidden %q", msg, suffix)
		}
	}
	if !strings.Contains(msg, strconv.FormatInt(e.AmountCents, 10)) {
		t.Fatalf("message %q omits the amount %d", msg, e.AmountCents)
	}
}

func TestErrorStringIsDeterministic(t *testing.T) {
	t.Parallel()

	e := &PaymentError{AmountCents: 100, Reason: "card expired"}
	if e.Error() != e.Error() {
		t.Fatal("Error() is not deterministic for fixed inputs")
	}
}

func TestSatisfiesErrorInterface(t *testing.T) {
	t.Parallel()

	var err error = &PaymentError{AmountCents: 1, Reason: "x"}
	if err.Error() == "" {
		t.Fatal("Error() returned an empty string")
	}
}
```

## Review

The type is correct when it satisfies `error` (the compile-time assertion proves
it) and its message obeys every convention (the lint test proves that). The value
of linting the string rather than eyeballing it is that the conventions survive
future edits: the day someone "improves" the message by capitalizing it, the test
goes red. Keeping money as integer cents rather than a float avoids a whole class
of rounding bugs in the amount the message reports.

The mistakes to avoid: capitalizing or period-terminating the message (it reads
badly the moment a caller wraps it) and treating `Error()` output as a UI string
(it is for logs and operators; user-facing text is a separate rendering concern).
Include actionable context — the amount here — so the log line stands on its own.

## Resources

- [Go Code Review Comments: Error Strings](https://go.dev/wiki/CodeReviewComments#error-strings) — lowercase, no trailing punctuation, reads well when wrapped.
- [pkg.go.dev: builtin error](https://pkg.go.dev/builtin#error) — `Error() string` is the whole interface.
- [pkg.go.dev: unicode.IsUpper](https://pkg.go.dev/unicode#IsUpper) and [strings.HasSuffix](https://pkg.go.dev/strings#HasSuffix) — the primitives the lint test uses.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-request-validation-guard-clauses.md](06-request-validation-guard-clauses.md) | Next: [08-defer-close-error-capture.md](08-defer-close-error-capture.md)
