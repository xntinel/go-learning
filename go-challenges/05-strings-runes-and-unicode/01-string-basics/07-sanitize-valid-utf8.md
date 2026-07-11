# Exercise 7: Sanitize untrusted text to valid UTF-8 before logging or persisting

Untrusted bytes written straight into a log or a database column are a real
hazard: invalid UTF-8 corrupts records, and an embedded newline forges a log
line. This module builds the sanitizer that sits in front of persistence — it
repairs invalid UTF-8 and strips control characters — and asserts the output is
always well-formed as an invariant.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
sanitize/                   independent module: example.com/sanitize
  go.mod                    go 1.26
  sanitize.go               SanitizeForStorage
  cmd/
    demo/
      main.go               sanitize a forged log-injection string
  sanitize_test.go          replacement/strip table + always-valid invariant
```

Files: `sanitize.go`, `cmd/demo/main.go`, `sanitize_test.go`.
Implement: `SanitizeForStorage(s string) string`.
Test: valid input unchanged, invalid UTF-8 replaced by U+FFFD, CR/LF stripped so
a forged log line cannot be injected, and an invariant that the output is always
valid UTF-8 across a set of hostile inputs.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/sanitize/cmd/demo
cd ~/go-exercises/sanitize
go mod init example.com/sanitize
```

## Two failure modes, two steps

The sanitizer defends against two distinct attacks that share a root cause —
trusting client bytes.

Encoding corruption: a `string` from the network can contain byte sequences that
are not valid UTF-8. Persist them and a later reader (a JSON encoder, a database
driver, another service) may error, silently drop the row, or mangle it — a poison
record. `strings.ToValidUTF8(s, "�")` rewrites each run of invalid bytes to
the Unicode replacement character U+FFFD, guaranteeing the result is well-formed.

Log injection: if the text is written into a line-oriented log, an embedded `\n`
lets an attacker forge an entire log entry — `evil\n2024-01-15T00:00:00Z INFO
admin logged in` becomes a fake second line in the log. The defense is to drop
control characters (newlines, carriage returns, NUL, escape sequences) before the
text reaches the log. `strings.Map` with a mapping function that returns `-1` for
control runes drops them with no replacement; other runes pass through. We keep
ordinary spaces (a name may legitimately contain them) but drop everything
`unicode.IsControl` classifies, and also the DEL and C1 range it covers.

Order matters: repair the encoding first so `strings.Map` iterates over
well-formed runes, then strip control characters. The result is guaranteed valid
UTF-8 and single-line, safe to log or store.

Create `sanitize.go`:

```go
package sanitize

import (
	"strings"
	"unicode"
)

// SanitizeForStorage makes untrusted text safe to log or persist: invalid UTF-8
// is replaced with U+FFFD, and control characters (including CR/LF, which enable
// log injection) are dropped. Ordinary printable runes and spaces pass through.
func SanitizeForStorage(s string) string {
	// Step 1: guarantee well-formed UTF-8.
	s = strings.ToValidUTF8(s, "�")

	// Step 2: drop control characters. A mapping that returns -1 removes the rune.
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
```

## The runnable demo

The demo feeds in a string that tries to inject a second log line and contains an
invalid byte, and prints the sanitized single-line result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sanitize"
)

func main() {
	forged := "user set name to bob\n2024-01-15T00:00:00Z INFO admin promoted \xff"
	clean := sanitize.SanitizeForStorage(forged)
	fmt.Printf("raw   : %q\n", forged)
	fmt.Printf("clean : %q\n", clean)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
raw   : "user set name to bob\n2024-01-15T00:00:00Z INFO admin promoted \xff"
clean : "user set name to bob2024-01-15T00:00:00Z INFO admin promoted �"
```

## Tests

The table covers the identity case (valid text is untouched), UTF-8 repair, and
CR/LF stripping. The most important test is the invariant: across a set of hostile
inputs — invalid bytes, lone continuation bytes, embedded newlines and NULs — the
output must *always* satisfy `utf8.ValidString`. That is the property the rest of
the system relies on.

Create `sanitize_test.go`:

```go
package sanitize

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeForStorage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"valid unchanged", "café résumé", "café résumé"},
		{"strip newline", "line1\nline2", "line1line2"},
		{"strip crlf", "a\r\nb", "ab"},
		{"strip nul", "a\x00b", "ab"},
		{"replace invalid", "bad\xffend", "bad�end"},
		{"keep spaces", "two  spaces", "two  spaces"},
		{"log injection", "ok\n2024 INFO forged", "ok2024 INFO forged"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := SanitizeForStorage(tc.in); got != tc.want {
				t.Fatalf("SanitizeForStorage(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestOutputAlwaysValidUTF8(t *testing.T) {
	t.Parallel()

	hostile := []string{
		"\xff\xfe\xfd",
		"prefix\xffsuffix",
		"\x80\x80\x80", // lone continuation bytes
		"a\r\nb\x00c",
		strings.Repeat("\xf0", 100), // truncated 4-byte lead bytes
		"normal text",
		"",
	}
	for _, in := range hostile {
		out := SanitizeForStorage(in)
		if !utf8.ValidString(out) {
			t.Fatalf("SanitizeForStorage(%q) = %q, which is not valid UTF-8", in, out)
		}
		if strings.ContainsAny(out, "\r\n\x00") {
			t.Fatalf("SanitizeForStorage(%q) = %q still contains control chars", in, out)
		}
	}
}

func ExampleSanitizeForStorage() {
	fmt.Printf("%q\n", SanitizeForStorage("evil\ninjected\xff"))
	// Output: "evilinjected�"
}
```

## Review

The sanitizer is correct when its output is, without exception, valid UTF-8 and
free of control characters — that is the invariant `TestOutputAlwaysValidUTF8`
enforces across hostile inputs. Repairing the encoding before stripping means
`strings.Map` always walks well-formed runes; returning `-1` from the mapping is
how a rune is dropped with no replacement. The log-injection case is the concrete
motivation: an embedded newline that would forge a second log entry is removed
before the text can reach the log. Keep ordinary spaces, which are legitimate
content, and drop only what `unicode.IsControl` flags. Run `go test -race`.

## Resources

- [strings.ToValidUTF8 (pkg.go.dev)](https://pkg.go.dev/strings#ToValidUTF8)
- [strings.Map (pkg.go.dev)](https://pkg.go.dev/strings#Map)
- [unicode.IsControl (pkg.go.dev)](https://pkg.go.dev/unicode#IsControl)
- [OWASP: Log Injection](https://owasp.org/www-community/attacks/Log_Injection)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-rune-length-validation.md](06-rune-length-validation.md) | Next: [08-scope-tokenizer.md](08-scope-tokenizer.md)
