# Exercise 3: A Self-Redacting Secret Type via fmt.Formatter

An API key, a DB password, or a card number stored in a plain `string` leaks the
moment anything logs it with `%v` or wraps it in an error. This exercise builds a
`Secret` type that implements `fmt.Formatter` so that `%v`, `%s`, and `%q` always
render `[REDACTED]`, with one deliberate escape hatch to read the real value.

This module is fully self-contained: its own `go mod init`, code, demo, and tests.

## What you'll build

```text
secret/                    independent module: example.com/secret
  go.mod                   go 1.24
  secret.go                type Secret; Format(fmt.State, rune); Reveal(); Last4()
  cmd/
    demo/
      main.go              runnable demo: redaction in log lines and errors
  secret_test.go           redaction/never-leak/width-flag/reveal/verb-fuzz tests
```

- Files: `secret.go`, `cmd/demo/main.go`, `secret_test.go`.
- Implement: `Secret` (a string) implementing `Format(f fmt.State, verb rune)` so `%v`/`%s`/`%q` emit `[REDACTED]`, honoring width and the `-` flag for padding, plus `Reveal()` (the true value) and `Last4()` (masked card form).
- Test: `%v`/`%s`/`%q` all yield the redacted token; the secret string never appears in `Sprintf` output nor in a wrapped `fmt.Errorf`; `%-20s` pads the token; `Reveal()` returns the true value; a fuzz-style sweep asserting no verb leaks the plaintext.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why Formatter, not Stringer

A `Stringer` only intercepts `%v` and `%s`. `%q` on a `Stringer` still quotes the
*underlying string* — so `%q` would happily print the real secret. Since a leak
through any verb is unacceptable, `Secret` must intercept *every* verb, which is
exactly what `fmt.Formatter` is for. `Format(f fmt.State, verb rune)` is called for
every verb applied to the value and is fully responsible for the output; the
default reflection path never runs. That total control is the safety property: a
verb the method does not explicitly handle can be made to emit a harmless marker
rather than fall through to the raw bytes.

`fmt.State` is the writer and the flag/width/precision oracle. It implements
`io.Writer` (so `io.WriteString(f, ...)` sends bytes to the output), and exposes
`Width() (int, bool)`, `Precision() (int, bool)`, and `Flag(c int) bool`. Honoring
width matters for real ops output: `%-20s` in an aligned table must still pad the
`[REDACTED]` token to 20 columns, left-justified, or the column breaks. The method
reads `Width()` and the `-` flag and pads the *redacted* token itself — the secret
never participates in the width calculation because the secret never appears.

There is one honest caveat that separates a real understanding from a naive one:
`fmt` special-cases `%T` and `%p` and resolves them *before* consulting
`Formatter`, so those two verbs are the one place a `Formatter` cannot intercept.
`%T` is harmless — it prints only the type name (`secret.Secret`), never the value.
`%p` is not: on a value type it produces a "bad verb" rendering
`%!p(secret.Secret=sk-live-...)` that embeds the value, so a caller who writes
`%p` on a `Secret` value *can* leak it. The lesson is that `Formatter` is powerful
defense-in-depth for every normal rendering verb, but it is not a total guarantee
against a caller deliberately using `%p`; credential types should still never be
handed to `%p`, and code review plus the `Reveal`-only escape hatch remain part of
the defense. The tests below sweep the rendering verbs (all redacted), assert `%T`
exposes only the type name, and document the `%p` bypass explicitly rather than
pretending it does not exist.

The escape hatch is a plain method, not a verb: `Reveal()` returns the true string,
and `Last4()` returns a masked card form (`****1234`). Making the reveal an
explicit method call means it can never happen by accident through a rendering
format string; a grep for `.Reveal()` finds every intentional exposure.

Create `secret.go`:

```go
package secret

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

const redacted = "[REDACTED]"

// Secret is a credential (API key, password, card number) that redacts itself in
// all fmt output. The real value is reachable only through Reveal or Last4.
type Secret string

// Format implements fmt.Formatter. For %v, %s, and %q it writes the redacted
// token (quoted for %q), honoring width and the '-' (left-justify) flag. Any
// other verb yields a harmless marker rather than leaking the value.
func (s Secret) Format(f fmt.State, verb rune) {
	switch verb {
	case 'v', 's', 'q':
		out := redacted
		if verb == 'q' {
			out = strconv.Quote(redacted)
		}
		writePadded(f, out)
	default:
		fmt.Fprintf(f, "%%!%c(secret)", verb)
	}
}

// Reveal returns the true secret value. This is the single intentional escape
// hatch; a search for .Reveal() enumerates every place a secret is exposed.
func (s Secret) Reveal() string { return string(s) }

// Last4 returns a masked form showing only the last four characters, e.g.
// "****1234". Fewer than four characters redact fully.
func (s Secret) Last4() string {
	r := []rune(s)
	if len(r) < 4 {
		return redacted
	}
	return "****" + string(r[len(r)-4:])
}

// writePadded emits out, honoring the State's width and '-' flag.
func writePadded(f fmt.State, out string) {
	w, ok := f.Width()
	if !ok || w <= len(out) {
		io.WriteString(f, out)
		return
	}
	pad := strings.Repeat(" ", w-len(out))
	if f.Flag('-') {
		io.WriteString(f, out)
		io.WriteString(f, pad)
	} else {
		io.WriteString(f, pad)
		io.WriteString(f, out)
	}
}
```

### The runnable demo

The demo shows the two places a credential leaks in real code: a structured log
line and a wrapped error. Both render `[REDACTED]`; only the explicit `Last4()`
reveals anything, and only the last four digits.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/secret"
)

func main() {
	apiKey := secret.Secret("sk-live-abcdef1234567890")
	card := secret.Secret("4111111111111234")

	fmt.Printf("auth key=%v\n", apiKey)
	fmt.Printf("auth key quoted=%q\n", apiKey)

	err := fmt.Errorf("charge failed with key %s", apiKey)
	fmt.Println(err)

	fmt.Printf("card=%s last4=%s\n", card, card.Last4())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
auth key=[REDACTED]
auth key quoted="[REDACTED]"
charge failed with key [REDACTED]
card=[REDACTED] last4=****1234
```

### Tests

The tests prove the safety property from several angles. `TestVerbsRedact` checks
`%v`/`%s`/`%q` all redact. `TestNeverLeaks` is the important one: it formats the
secret with a table of verbs and asserts the plaintext substring never appears in
any output, and separately that a wrapped `fmt.Errorf` does not contain it — this
is the fuzz-style guarantee that no verb combination leaks. `TestWidthAndFlag`
checks `%-20s` left-pads and `%20s` right-pads the token. `TestReveal` confirms the
intentional escape hatch still works.

Create `secret_test.go`:

```go
package secret

import (
	"fmt"
	"strings"
	"testing"
)

const plaintext = "sk-live-abcdef1234567890"

func TestVerbsRedact(t *testing.T) {
	t.Parallel()

	s := Secret(plaintext)
	tests := []struct {
		verb string
		want string
	}{
		{"%v", "[REDACTED]"},
		{"%s", "[REDACTED]"},
		{"%q", `"[REDACTED]"`},
	}
	for _, tt := range tests {
		t.Run(tt.verb, func(t *testing.T) {
			t.Parallel()
			if got := fmt.Sprintf(tt.verb, s); got != tt.want {
				t.Fatalf("Sprintf(%q) = %q, want %q", tt.verb, got, tt.want)
			}
		})
	}
}

// TestNeverLeaks asserts no rendering verb and no wrapped error exposes the
// plaintext. It sweeps the verbs that fmt routes through Format.
func TestNeverLeaks(t *testing.T) {
	t.Parallel()

	s := Secret(plaintext)
	verbs := []string{"%v", "%s", "%q", "%d", "%x", "%X", "%#v", "%+v", "%c", "%o"}
	for _, v := range verbs {
		out := fmt.Sprintf(v, s)
		if strings.Contains(out, plaintext) {
			t.Fatalf("verb %q leaked plaintext: %q", v, out)
		}
	}
	err := fmt.Errorf("failed with credential %s: %w", s, fmt.Errorf("timeout"))
	if strings.Contains(err.Error(), plaintext) {
		t.Fatalf("wrapped error leaked plaintext: %q", err.Error())
	}
}

// TestTypeVerbSafe documents that %T is resolved by fmt before Formatter and
// exposes only the type name, never the value.
func TestTypeVerbSafe(t *testing.T) {
	t.Parallel()

	out := fmt.Sprintf("%T", Secret(plaintext))
	if strings.Contains(out, plaintext) {
		t.Fatalf("%%T leaked plaintext: %q", out)
	}
	if out != "secret.Secret" {
		t.Fatalf("%%T = %q, want secret.Secret", out)
	}
}

// TestPointerVerbBypassesFormatter documents the one leak fmt.Formatter cannot
// stop: %p is resolved before Format, so a value passed to %p is rendered with a
// bad-verb form that embeds the value. Credential types must never reach %p.
func TestPointerVerbBypassesFormatter(t *testing.T) {
	t.Parallel()

	out := fmt.Sprintf("%p", Secret(plaintext))
	if !strings.Contains(out, plaintext) {
		t.Fatalf("expected %%p to bypass Format and expose the value, got %q", out)
	}
}

func TestWidthAndFlag(t *testing.T) {
	t.Parallel()

	s := Secret(plaintext)
	if got := fmt.Sprintf("%-20s|", s); got != "[REDACTED]          |" {
		t.Fatalf("left-justified = %q", got)
	}
	if got := fmt.Sprintf("%20s|", s); got != "          [REDACTED]|" {
		t.Fatalf("right-justified = %q", got)
	}
}

func TestReveal(t *testing.T) {
	t.Parallel()

	s := Secret(plaintext)
	if got := s.Reveal(); got != plaintext {
		t.Fatalf("Reveal() = %q, want %q", got, plaintext)
	}
	card := Secret("4111111111111234")
	if got := card.Last4(); got != "****1234" {
		t.Fatalf("Last4() = %q, want ****1234", got)
	}
	if got := Secret("ab").Last4(); got != "[REDACTED]" {
		t.Fatalf("short Last4() = %q, want [REDACTED]", got)
	}
}

func Example() {
	s := Secret("hunter2")
	fmt.Printf("password=%s\n", s)
	// Output: password=[REDACTED]
}
```

## Review

The type is safe when `Format` handles every rendering verb: the string-family
verbs emit the redacted token, and every other rendering verb emits a marker
instead of falling through to the raw bytes. The proof is `TestNeverLeaks` sweeping
a range of verbs and a wrapped error and finding the plaintext in none of them. The
honest limit is `%p`: `fmt` resolves `%p` (and `%T`) before it consults
`Formatter`, so `%p` on a value leaks via a bad-verb rendering — `%T` is
type-only and safe, but a credential must never be handed to `%p`. That is
defense-in-depth, not an absolute guarantee, which is why the `Reveal`-only escape
hatch and review still matter. Honoring `Width()` and the `-` flag is what lets a
redacted secret sit inside an aligned table without breaking the column. The escape hatch is a method (`Reveal`/`Last4`), never a verb,
so exposure is always an explicit, greppable call. The trap to avoid is reaching
for `Stringer` instead of `Formatter`: `Stringer` leaves `%q` free to quote and
print the real value. Run `go test -race`; the type is immutable and the test
confirms redaction.

## Resources

- [`fmt.Formatter`](https://pkg.go.dev/fmt#Formatter) — the `Format(State, rune)` interface that overrides all verbs.
- [`fmt.State`](https://pkg.go.dev/fmt#State) — `Write`, `Width`, `Precision`, and `Flag` for honoring width/flags.
- [`strconv.Quote`](https://pkg.go.dev/strconv#Quote) — the safe quoting used for the `%q` branch.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-error-wrapping-verbs.md](04-error-wrapping-verbs.md)
