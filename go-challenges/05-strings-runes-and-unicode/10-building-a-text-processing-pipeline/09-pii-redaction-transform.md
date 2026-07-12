# Exercise 9: Redact Emails and Bearer Tokens Before Indexing

Log and text ingestion routinely carries secrets that must never reach a search
index: email addresses, `Bearer` tokens, long hex digests. This module builds a
redaction `Transform` that masks those structured secrets with placeholders using
tightly-scoped regexes — a legitimate regex job, in contrast to the lesson's
warning against regex HTML parsing — while leaving surrounding text intact.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
redact/                   independent module: example.com/redact
  go.mod                  go 1.26
  redact.go               Redact(string) string; the three anchored patterns
  cmd/
    demo/
      main.go             runnable demo over emails, tokens, and hex blobs
  redact_test.go          match, no-match, adjacency, and idempotence tests
```

- Files: `redact.go`, `cmd/demo/main.go`, `redact_test.go`.
- Implement: `Redact(string) string` masking emails (`[REDACTED-EMAIL]`), `Bearer` tokens (`[REDACTED-TOKEN]`), and long hex blobs (`[REDACTED-HEX]`) with `regexp.MustCompile` package-level patterns and `ReplaceAllString`.
- Test: text with an email and an `Authorization: Bearer <token>` fragment is redacted while surrounding words are untouched; no-match input is unchanged; adjacent/multiple matches; `Redact(Redact(s)) == Redact(s)`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/10-building-a-text-processing-pipeline/09-pii-redaction-transform/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/10-building-a-text-processing-pipeline/09-pii-redaction-transform
```

### Regex for token grammars, not for markup

The lesson warns against using a regex as an HTML parser, and that warning stands:
HTML is not a regular language. But an email address, a `Bearer` token, and a hex
digest *are* well-defined token grammars with no recursion, and matching them with
a tightly-scoped regex is exactly right. The distinction is not "regex bad" — it is
"regex for regular grammars, a parser for recursive ones."

Three patterns, each anchored on word boundaries so they do not swallow
surrounding text:

- Email: `\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b` — local part, `@`,
  domain, TLD. Scoped enough to match `alice@example.com` and
  `bob.smith+tag@mail.co.uk` without eating the words around them.
- Bearer token: `(?i)\bBearer\s+[A-Za-z0-9._~+/=\-]+` — the literal `Bearer`
  (case-insensitive) followed by the token characters of an opaque bearer/JWT
  value. It matches the whole `Bearer abc.def-123` fragment, so the secret itself
  is masked, not just the word `Bearer`.
- Hex blob: `\b[A-Fa-f0-9]{32,}\b` — a run of 32 or more hex digits, the shape of a
  digest, API key, or session id.

Redaction runs the three replacements in a fixed order and is order-safe within the
pipeline: the placeholders (`[REDACTED-EMAIL]`, ...) contain no `@`, no `Bearer`,
and no 32-character hex run, so a second pass matches nothing new. That is what
makes `Redact` idempotent — `Redact(Redact(s)) == Redact(s)` — which matters because
a record may be re-ingested and must not accumulate double redactions.

One caution belongs in the code and the review: redaction here is
defense-in-depth, not a license to log secrets. The right primary control is to not
emit secrets into logs and text fields at all; this transform is the safety net for
when something slips through, not the reason it is safe to be careless upstream.

Create `redact.go`:

```go
package redact

import "regexp"

var (
	emailRe  = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	bearerRe = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=\-]+`)
	hexRe    = regexp.MustCompile(`\b[A-Fa-f0-9]{32,}\b`)
)

// Redact masks structured secrets in s with placeholders. It is a defense-in-depth
// safety net, not a substitute for keeping secrets out of logs. Redact is
// idempotent: the placeholders never match the patterns, so a second pass is a
// no-op.
func Redact(s string) string {
	s = emailRe.ReplaceAllString(s, "[REDACTED-EMAIL]")
	s = bearerRe.ReplaceAllString(s, "[REDACTED-TOKEN]")
	s = hexRe.ReplaceAllString(s, "[REDACTED-HEX]")
	return s
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
	inputs := []string{
		"contact alice@example.com or bob.smith+tag@mail.co.uk today",
		"header Authorization: Bearer abc123.DEF-456_tok fetched",
		"digest 0123456789abcdef0123456789abcdef then ok",
		"nothing to redact here",
	}
	for _, in := range inputs {
		fmt.Printf("%q\n  -> %q\n", in, redact.Redact(in))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"contact alice@example.com or bob.smith+tag@mail.co.uk today"
  -> "contact [REDACTED-EMAIL] or [REDACTED-EMAIL] today"
"header Authorization: Bearer abc123.DEF-456_tok fetched"
  -> "header Authorization: [REDACTED-TOKEN] fetched"
"digest 0123456789abcdef0123456789abcdef then ok"
  -> "digest [REDACTED-HEX] then ok"
"nothing to redact here"
  -> "nothing to redact here"
```

### Tests

`TestRedact` is a table over each secret kind plus a no-match row and a
multiple-match row, asserting exact output so surrounding words are proven intact.
`TestIdempotent` runs `Redact` twice and asserts a fixed point — the guarantee that
re-ingestion does not accumulate redactions.

Create `redact_test.go`:

```go
package redact

import (
	"fmt"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, in, want string
	}{
		{
			"email",
			"contact alice@example.com now",
			"contact [REDACTED-EMAIL] now",
		},
		{
			"bearer token",
			"send Authorization: Bearer abc.def-123 header",
			"send Authorization: [REDACTED-TOKEN] header",
		},
		{
			"hex digest",
			"sha 0123456789abcdef0123456789abcdef done",
			"sha [REDACTED-HEX] done",
		},
		{
			"multiple secrets",
			"x@y.io and Bearer zzz and deadbeefdeadbeefdeadbeefdeadbeef00",
			"[REDACTED-EMAIL] and [REDACTED-TOKEN] and [REDACTED-HEX]",
		},
		{
			"no match",
			"plain log line with no secrets",
			"plain log line with no secrets",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Redact(tt.in)
			if got != tt.want {
				t.Fatalf("Redact(%q) = %q, want %q", tt.in, got, tt.want)
			}
			// The literal secret must not survive.
			if strings.Contains(got, "@example.com") {
				t.Fatalf("Redact leaked an email: %q", got)
			}
		})
	}
}

func TestIdempotent(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"alice@example.com and Bearer tok and 0123456789abcdef0123456789abcdef",
		"nothing here",
		"",
	}
	for _, in := range inputs {
		once := Redact(in)
		twice := Redact(once)
		if once != twice {
			t.Fatalf("Redact not idempotent for %q: %q vs %q", in, once, twice)
		}
	}
}

func ExampleRedact() {
	fmt.Println(Redact("ping alice@example.com please"))
	// Output: ping [REDACTED-EMAIL] please
}
```

## Review

The redactor is correct when each pattern masks its secret and leaves the
surrounding text byte-for-byte intact, when a no-match input is returned unchanged,
and when `Redact` is idempotent so re-ingestion cannot double-redact. The trap is
scope creep in the patterns: an over-broad email or token regex starts eating
adjacent words or normal identifiers, so keep the patterns anchored on word
boundaries and test the no-match and adjacency cases. And keep the honest framing —
this is defense-in-depth, a safety net for secrets that slip through, never a
reason to log them in the first place. Run `go test -race` to confirm the shared
compiled patterns are safe under concurrent use (they are; `*regexp.Regexp` is safe
for concurrent use).

## Resources

- [regexp.MustCompile and Regexp.ReplaceAllString](https://pkg.go.dev/regexp#Regexp.ReplaceAllString) — compiling once and substituting matches.
- [regexp/syntax](https://pkg.go.dev/regexp/syntax) — the RE2 syntax and its linear-time guarantees.
- [OWASP Logging Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html) — why keeping secrets out of logs is the primary control, redaction the backstop.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-rune-safe-length-truncation.md](08-rune-safe-length-truncation.md) | Next: [10-composed-ingestion-invariants.md](10-composed-ingestion-invariants.md)
