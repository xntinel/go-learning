# Exercise 4: Secret and PII Redactor for Log Output

Before a log line leaves the process it should not carry a bearer token, an email
address, or a card-like digit run. This module builds the redaction layer that
scrubs them, using `ReplaceAllStringFunc` to replace each match with a fixed mask
while preserving the surrounding structure — so `Authorization: Bearer <token>`
becomes `Authorization: Bearer ***REDACTED***`, keeping the shape a human debugger
still needs to read.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
redact/                     independent module: example.com/redact
  go.mod                    go 1.26
  redact.go                 bearer/email/card regexes; Redact via ReplaceAllStringFunc; masks
  cmd/
    demo/
      main.go               runnable demo: redact a noisy log line
  redact_test.go            table-driven: prefix survives, multiple secrets, clean line, no partial leak
```

- Files: `redact.go`, `cmd/demo/main.go`, `redact_test.go`.
- Implement: `Redact(s string) string` applying package-level bearer, email, and card regexes; a `ReplaceAllStringFunc` masker for the bearer token that keeps the `Bearer ` prefix; a length-preserving card mask.
- Test: a bearer token is masked but `Authorization:` and `Bearer ` survive; multiple secrets on one line are all masked; a clean line is returned unchanged; the raw secret never appears in the output; a benchmark justifies package-level compilation.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/redact/cmd/demo
cd ~/go-exercises/redact
go mod init example.com/redact
```

### Why ReplaceAllStringFunc, and why preserve structure

A redactor has to do more than "delete the secret." A debugger reading the scrubbed
log still needs to know *that there was a bearer token there* and where — so the
mask preserves the surrounding structure: the `Authorization:` header name, the
`Bearer ` scheme, and the general shape of the line stay, only the secret value
turns into a fixed mask. That contextual rewrite is exactly what
`ReplaceAllStringFunc(src, func(match string) string)` is for: it hands you each
matched substring and lets you compute its replacement. The bearer masker
re-matches its own capture groups to peel off the `Bearer ` prefix and mask only
the token; the card masker replaces the digit run with a same-length run of `*`,
so column alignment survives.

Contrast this with `ReplaceAllString(src, "$1***")`, which does `$`-expansion on
the replacement string. That is fine for the email case (a fixed replacement with
no `$`), but for anything where the replacement depends on the match — or where
the matched text might itself contain a `$` — the closure form is safer and
clearer. And crucially, the masks are *fixed* text you control, so a partial
secret can never leak through: the output contains the mask, never a slice of the
original token.

The three patterns are compiled once at package level. The benchmark exists to
make the reason concrete: compiling them per call would rebuild three automata on
every log line, which on a hot logging path is pure waste.

Create `redact.go`:

```go
package redact

import (
	"regexp"
	"strings"
)

const mask = "***REDACTED***"

// Package-level patterns: compiled once, safe for concurrent use.
var (
	// bearerRe captures the "Bearer " scheme separately from the token so the
	// masker can keep the scheme and mask only the secret.
	bearerRe = regexp.MustCompile(`(?i)(bearer\s+)([A-Za-z0-9._~+/=-]+)`)
	emailRe  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	cardRe   = regexp.MustCompile(`\b\d{13,19}\b`)
)

// Redact scrubs bearer tokens, emails, and card-like digit runs from s, replacing
// each with a fixed mask while preserving the surrounding structure.
func Redact(s string) string {
	s = bearerRe.ReplaceAllStringFunc(s, maskBearer)
	s = emailRe.ReplaceAllString(s, "[EMAIL]")
	s = cardRe.ReplaceAllStringFunc(s, maskCard)
	return s
}

// maskBearer keeps the "Bearer " scheme and masks only the token.
func maskBearer(m string) string {
	g := bearerRe.FindStringSubmatch(m)
	if len(g) < 3 {
		return mask
	}
	return g[1] + mask
}

// maskCard replaces the digit run with a same-length run of asterisks so column
// alignment survives.
func maskCard(m string) string {
	return strings.Repeat("*", len(m))
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/redact"
)

func main() {
	lines := []string{
		"GET /me Authorization: Bearer eyJhbGciOi.J9secret user=alice@example.com",
		"payment card=4111111111111111 approved",
		"health check ok, no secrets here",
	}
	for _, line := range lines {
		fmt.Println(redact.Redact(line))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /me Authorization: Bearer ***REDACTED*** user=[EMAIL]
payment card=**************** approved
health check ok, no secrets here
```

### Tests

Create `redact_test.go`:

```go
package redact

import (
	"regexp"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bearer keeps prefix",
			in:   "Authorization: Bearer abc.def.ghi",
			want: "Authorization: Bearer ***REDACTED***",
		},
		{
			name: "email",
			in:   "contact alice@example.com now",
			want: "contact [EMAIL] now",
		},
		{
			name: "card same length",
			in:   "card=4111111111111111 ok",
			want: "card=**************** ok",
		},
		{
			name: "multiple secrets one line",
			in:   "Bearer tok123 from bob@corp.io pan 4111111111111111",
			want: "Bearer ***REDACTED*** from [EMAIL] pan ****************",
		},
		{
			name: "clean line unchanged",
			in:   "health check ok",
			want: "health check ok",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Redact(tc.in); got != tc.want {
				t.Fatalf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNoPartialSecretLeaks(t *testing.T) {
	t.Parallel()
	secret := "SuperSecretToken1234567890"
	out := Redact("Authorization: Bearer " + secret)
	if strings.Contains(out, secret) {
		t.Fatalf("full secret leaked: %q", out)
	}
	// No 6+ char slice of the token should survive either.
	for i := 0; i+6 <= len(secret); i++ {
		if strings.Contains(out, secret[i:i+6]) {
			t.Fatalf("partial secret %q leaked in %q", secret[i:i+6], out)
		}
	}
}

func TestCardMaskPreservesLength(t *testing.T) {
	t.Parallel()
	digits := regexp.MustCompile(`\d`)
	in := "4111111111111111"
	out := Redact(in)
	if len(out) != len(in) {
		t.Fatalf("len(out) = %d, want %d", len(out), len(in))
	}
	if digits.MatchString(out) {
		t.Fatalf("digits survived redaction: %q", out)
	}
}

func BenchmarkRedact(b *testing.B) {
	line := "Authorization: Bearer eyJhbGciOi.J9 user=alice@example.com pan 4111111111111111"
	b.ReportAllocs()
	for b.Loop() {
		Redact(line)
	}
}
```

## Review

The redactor is correct when structure survives but the secret does not:
`ReplaceAllStringFunc` lets the bearer masker keep the `Bearer ` scheme (its first
capture group) and replace only the token, while the card masker preserves column
width with a same-length `*` run. Because every mask is fixed text the code owns,
`TestNoPartialSecretLeaks` can assert that not even a six-character slice of the
original token appears in the output — the closure never echoes matched bytes. The
clean-line case returns unchanged, and the benchmark documents why the three
patterns live at package scope rather than being rebuilt per line. The trap to
remember is `$`-expansion: `ReplaceAllString(s, repl)` interprets `$1`/`${name}`
in `repl`, so a replacement that must contain a literal `$` uses
`ReplaceAllLiteralString` or the func form. Run `go test -race` since the shared
patterns are matched from many goroutines.

## Resources

- [`regexp` package](https://pkg.go.dev/regexp) — `ReplaceAllStringFunc`, `ReplaceAllString`, `ReplaceAllLiteralString`, and `$`-expansion rules.
- [OWASP Logging Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html) — what must never reach a log, and why redaction is a boundary control.
- [`strings.Repeat`](https://pkg.go.dev/strings#Repeat) — building the fixed-width mask.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-dynamic-pattern-rule-engine.md](03-dynamic-pattern-rule-engine.md) | Next: [05-regex-path-router.md](05-regex-path-router.md)
