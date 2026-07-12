# Exercise 4: Scrub Invalid UTF-8 for a Log/Telemetry Pipeline

A log-ingestion path has the opposite policy from an ingest validator: it must never
drop a line. A record with a few bad bytes still has to be JSON-encoded, indexed, and
searched, so the right move is to repair the invalid bytes rather than reject the
record. This module builds that scrubber, contrasting repair with the reject policy
of the previous exercise, and keeps a zero-copy fast path for the common valid case.

## What you'll build

```text
scrub/                     independent module: example.com/scrub
  go.mod                   go 1.26
  scrub.go                 Scrub, ScrubWith
  cmd/
    demo/
      main.go              repairs a bad byte, leaves valid input identical
  scrub_test.go            identity fast path + repair + cross-check vs manual loop
```

Files: `scrub.go`, `cmd/demo/main.go`, `scrub_test.go`.
Implement: `Scrub(s string) string` and `ScrubWith(s, replacement string) string`.
Test: valid input returned identical; `"a\xffb"` becomes `"a�b"`; runs collapse to one replacement; output always valid; cross-check vs a hand-written decode loop.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/04-string-iteration-bytes-vs-runes/04-utf8-scrubber-replace-invalid/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/04-string-iteration-bytes-vs-runes/04-utf8-scrubber-replace-invalid
```

### Repair, and the fast path that matters

The workhorse is `strings.ToValidUTF8(s, replacement)`: it returns a copy of `s`
with each *run* of invalid UTF-8 bytes replaced by the replacement string. "Run" is
the key word — three consecutive bad bytes collapse to a single replacement, not
three. The default replacement is U+FFFD, the Unicode replacement character, which is
exactly what a downstream JSON encoder would substitute anyway; doing it here, once,
at the pipeline edge, means everything after this point can assume valid UTF-8.

`ToValidUTF8` always allocates a new string, even when the input is already valid.
In a log pipeline that is overwhelmingly valid, that copy on every line is pure
waste, so `Scrub` guards with `utf8.ValidString(s)` first and returns the original
string — same backing bytes, no allocation — when nothing needs repair. Only genuinely
corrupt lines pay for a copy. This fast-path-then-repair shape is the same one the
validator used, differing only in what it does on the unhappy path: reject there,
repair here. That difference is the whole policy decision, and it is made per
boundary.

Create `scrub.go`:

```go
// scrub.go
package scrub

import (
	"strings"
	"unicode/utf8"
)

// Replacement is the default stand-in for invalid bytes: the Unicode replacement
// character U+FFFD.
const Replacement = "�"

// Scrub returns s with every run of invalid UTF-8 bytes replaced by U+FFFD.
// Valid input is returned unchanged with no allocation.
func Scrub(s string) string {
	return ScrubWith(s, Replacement)
}

// ScrubWith is Scrub with a caller-chosen replacement string (which may be "").
// Valid input is returned unchanged with no allocation.
func ScrubWith(s, replacement string) string {
	if utf8.ValidString(s) {
		return s // fast path: no copy for the common valid case
	}
	return strings.ToValidUTF8(s, replacement)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"unicode/utf8"

	"example.com/scrub"
)

func main() {
	line := "user=admin msg=he\xffllo run=\xe4\xb8" // stray 0xff and a truncated 中
	fixed := scrub.Scrub(line)

	fmt.Printf("raw valid   = %v\n", utf8.ValidString(line))
	fmt.Printf("fixed       = %q\n", fixed)
	fmt.Printf("fixed valid = %v\n", utf8.ValidString(fixed))

	clean := "user=alice msg=ok"
	fmt.Printf("clean same  = %v\n", scrub.Scrub(clean) == clean)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
raw valid   = false
fixed       = "user=admin msg=he�llo run=�"
fixed valid = true
clean same  = true
```

### Tests

The tests pin the identity fast path, the single-replacement-per-run semantics, and
the invariant that every output is valid UTF-8 — and cross-check `ToValidUTF8`
against a hand-written decode loop so the lesson does not merely trust the stdlib but
demonstrates the exact semantics it relies on.

Create `scrub_test.go`:

```go
// scrub_test.go
package scrub

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestScrubValidIsIdentity(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "ascii", "café 中文", "cjk-ext 𠀀", "already � here"} {
		if got := Scrub(s); got != s {
			t.Errorf("Scrub(%q) = %q, want identity", s, got)
		}
	}
}

func TestScrubRepairs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"single bad byte", "a\xffb", "a�b"},
		{"run collapses to one", "a\xff\xfe\xfdb", "a�b"},
		{"leading bad byte", "\x80ok", "�ok"},
		{"trailing truncated rune", "ok\xe4\xb8", "ok�"},
	}
	for _, tc := range cases {
		got := Scrub(tc.in)
		if got != tc.want {
			t.Errorf("%s: Scrub(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: Scrub output %q is not valid UTF-8", tc.name, got)
		}
	}
}

func TestScrubWithCustomReplacement(t *testing.T) {
	t.Parallel()
	if got := ScrubWith("a\xffb", "?"); got != "a?b" {
		t.Errorf("ScrubWith custom = %q, want a?b", got)
	}
	if got := ScrubWith("a\xffb", ""); got != "ab" {
		t.Errorf("ScrubWith empty (drop) = %q, want ab", got)
	}
}

// scrubRef is an independent implementation that decodes rune by rune, collapsing
// each run of invalid bytes into one replacement. It cross-checks ToValidUTF8's
// documented semantics.
func scrubRef(s, replacement string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString(replacement)
			i++
			for i < len(s) {
				if r2, sz := utf8.DecodeRuneInString(s[i:]); r2 == utf8.RuneError && sz == 1 {
					i++
					continue
				}
				break
			}
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

func TestScrubMatchesReference(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"a\xffb",
		"a\xff\xfe\xfdb",
		"\x80ok",
		"ok\xe4\xb8",
		"clean text 中文",
		"mix é \xff 中 \x80 z",
	}
	for _, s := range inputs {
		if got, want := Scrub(s), scrubRef(s, Replacement); got != want {
			t.Errorf("Scrub(%q) = %q, reference = %q", s, got, want)
		}
	}
}

func ExampleScrub() {
	fmt.Printf("%q\n", Scrub("a\xffb"))
	// Output: "a�b"
}
```

## Review

The scrubber is correct when valid input comes back byte-identical (via the
`ValidString` fast path, so no allocation), every output satisfies `utf8.ValidString`,
and a run of invalid bytes collapses to exactly one replacement — the semantics the
reference loop `scrubRef` demonstrates and `TestScrubMatchesReference` locks in. The
policy contrast with the previous module is the lesson: same fast path, opposite
unhappy-path action. Reject when garbage must not persist; repair when a record must
never be lost. Choosing wrong — repairing at a boundary that should reject, or
rejecting one that must not drop data — is the real mistake, not the mechanics.

## Resources

- [strings.ToValidUTF8](https://pkg.go.dev/strings#ToValidUTF8) — run-based replacement of invalid bytes.
- [unicode/utf8: ValidString, DecodeRuneInString, RuneError](https://pkg.go.dev/unicode/utf8) — the fast-path check and the reference decode.
- [Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings) — how the replacement character arises.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-utf8-input-validation-reject-invalid.md](03-utf8-input-validation-reject-invalid.md) | Next: [05-byte-offset-to-line-column.md](05-byte-offset-to-line-column.md)
