# Exercise 7: Mask PII Keeping the Last N Characters (Rune-Aware Redaction)

Audit logs and support tooling show the last few digits of a card, account, or phone
and mask the rest. "The rest" must be counted in runes: a byte-based mask miscounts on
any non-ASCII value and can slice a multi-byte rune into invalid UTF-8. This module
builds a rune-aware redactor that never leaks more than the intended tail.

## What you'll build

```text
redact/                    independent module: example.com/redact
  go.mod                   go 1.26
  redact.go                Redact, MaskEmail
  cmd/
    demo/
      main.go              masks a card number and an email
  redact_test.go           digit tails, CJK, keepLast bounds, email, valid UTF-8
```

Files: `redact.go`, `cmd/demo/main.go`, `redact_test.go`.
Implement: `Redact(s string, keepLast int, mask rune) string`, `MaskEmail(s string) string`.
Test: card keeps last 4; CJK masks the right count and stays valid UTF-8; `keepLast >= runeCount` returns input; `keepLast == 0` fully masks; `MaskEmail` preserves first char and domain.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/redact/cmd/demo
cd ~/go-exercises/redact
go mod init example.com/redact
```

### Count in runes, or leak

The redaction rule is "reveal the last `keepLast` characters, mask everything before".
The unit is characters — code points — so the implementation converts to `[]rune`
once and works in that space. Converting to `[]rune` is an O(n) allocation, which for a
short token (a card, a phone, an email) is negligible and buys correctness: indexing a
`[]rune` by character is exact, whereas byte indexing would miscount the split point on
any accented or CJK value and could cut a multi-byte rune, emitting invalid UTF-8 into
your logs.

Two guards make the function safe by construction. If `keepLast >= len(runes)`, the
whole value would be revealed, so return the input unchanged — masking nothing is
correct, and it also means the function never fabricates a longer string. If
`keepLast <= 0`, mask everything. In the general case, write `len(runes) - keepLast`
copies of the mask rune, then the final `keepLast` runes verbatim. Because the count of
revealed runes is `min(keepLast, len(runes))`, the function can never leak more than
`keepLast` characters — the security invariant a redactor lives or dies by.

`MaskEmail` is the same idea shaped to an address: keep the first character of the local
part and the entire `@domain`, mask the rest of the local part. Support staff can tell
two masked addresses apart by first letter and domain without seeing the identity.
`strings.LastIndexByte(s, '@')` locates the domain (a `@` is ASCII, so a byte index is
exact and also a valid rune boundary); a missing or leading `@` falls back to masking
everything.

Create `redact.go`:

```go
// redact.go
package redact

import "strings"

// Redact reveals only the last keepLast runes of s and replaces every earlier
// rune with mask. If keepLast is at least the rune count, s is returned
// unchanged; if keepLast <= 0, every rune is masked. The result never reveals
// more than keepLast characters and is always valid UTF-8.
func Redact(s string, keepLast int, mask rune) string {
	runes := []rune(s)
	n := len(runes)
	if keepLast >= n {
		return s
	}
	if keepLast < 0 {
		keepLast = 0
	}
	var b strings.Builder
	b.Grow(len(s))
	for range n - keepLast {
		b.WriteRune(mask)
	}
	for _, r := range runes[n-keepLast:] {
		b.WriteRune(r)
	}
	return b.String()
}

// MaskEmail keeps the first character of the local part and the full @domain,
// masking the rest of the local part with '*'. Input without a usable '@' is
// fully masked.
func MaskEmail(s string) string {
	at := strings.LastIndexByte(s, '@')
	if at <= 0 {
		return Redact(s, 0, '*')
	}
	local, domain := s[:at], s[at:]
	lr := []rune(local)
	if len(lr) <= 1 {
		return local + domain
	}
	var b strings.Builder
	b.Grow(len(s))
	b.WriteRune(lr[0])
	for range len(lr) - 1 {
		b.WriteByte('*')
	}
	b.WriteString(domain)
	return b.String()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/redact"
)

func main() {
	fmt.Println(redact.Redact("4111111111111234", 4, '*'))
	fmt.Println(redact.Redact("中文字号1234", 4, '*'))
	fmt.Println(redact.MaskEmail("alice@example.com"))
	fmt.Println(redact.MaskEmail("q@corp.example"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
************1234
****1234
a****@example.com
q@corp.example
```

### Tests

The tests cover the digit tail, a CJK value (asserting both the right mask count and
that the output is valid UTF-8), the two `keepLast` extremes, and the email shape, plus
the security invariant that the number of revealed characters never exceeds `keepLast`.

Create `redact_test.go`:

```go
// redact_test.go
package redact

import (
	"fmt"
	"testing"
	"unicode/utf8"
)

// revealedRunes counts how many runes of masked were left unmasked.
func revealedRunes(masked string, mask rune) int {
	n := 0
	for _, r := range masked {
		if r != mask {
			n++
		}
	}
	return n
}

func TestRedact(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in       string
		keepLast int
		want     string
	}{
		{"card keeps last 4", "4111111111111234", 4, "************1234"},
		{"cjk masks correct count", "中文字号1234", 4, "****1234"},
		{"keepLast equals length", "abcd", 4, "abcd"},
		{"keepLast exceeds length", "abcd", 9, "abcd"},
		{"keepLast zero masks all", "secret", 0, "******"},
		{"keepLast negative masks all", "secret", -3, "******"},
	}
	for _, tc := range cases {
		got := Redact(tc.in, tc.keepLast, '*')
		if got != tc.want {
			t.Errorf("%s: Redact(%q,%d) = %q, want %q", tc.name, tc.in, tc.keepLast, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: result %q is not valid UTF-8", tc.name, got)
		}
	}
}

func TestRedactNeverLeaksMoreThanKeepLast(t *testing.T) {
	t.Parallel()
	inputs := []string{"4111111111111234", "中文字号1234", "café-token-99", "x"}
	for _, s := range inputs {
		for keep := 0; keep <= 6; keep++ {
			got := Redact(s, keep, '*')
			revealed := revealedRunes(got, '*')
			total := utf8.RuneCountInString(s)
			bound := keep
			if bound > total {
				bound = total
			}
			if revealed > bound {
				t.Errorf("Redact(%q,%d) revealed %d runes, bound %d", s, keep, revealed, bound)
			}
		}
	}
}

func TestMaskEmail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"alice@example.com", "a****@example.com"},
		{"q@corp.example", "q@corp.example"},
		{"joão@example.org", "j***@example.org"},
		{"noatsign", "********"},
	}
	for _, tc := range cases {
		if got := MaskEmail(tc.in); got != tc.want {
			t.Errorf("MaskEmail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func ExampleRedact() {
	fmt.Println(Redact("中文字号1234", 4, '*'))
	// Output: ****1234
}
```

## Review

The redactor is correct when the count of revealed runes never exceeds `keepLast`
(the leak bound, asserted directly over many inputs and `keepLast` values), when a CJK
value masks the right number of *characters* and the output is valid UTF-8, and when
`keepLast >= len(runes)` returns the input rather than an oddly-longer string.
Converting to `[]rune` once is the deliberate choice: an O(n) allocation on a short
token in exchange for character-exact indexing that byte arithmetic cannot give you
here. The mistake to avoid is masking by byte length — `len(s) - keepLast*avgWidth`
or slicing `s[:len(s)-keepLast]` — which miscounts on non-ASCII and can split a rune,
writing invalid UTF-8 into the very logs you are trying to make safe.

## Resources

- [strings.Builder, LastIndexByte](https://pkg.go.dev/strings) — allocation-lean assembly and locating the domain.
- [unicode/utf8: RuneCountInString, ValidString](https://pkg.go.dev/unicode/utf8) — the character count and the validity invariant.
- [Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings) — why `[]rune` indexing differs from byte indexing.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-streaming-rune-decoder.md](06-streaming-rune-decoder.md) | Next: [08-rune-count-limit-validator.md](08-rune-count-limit-validator.md)
