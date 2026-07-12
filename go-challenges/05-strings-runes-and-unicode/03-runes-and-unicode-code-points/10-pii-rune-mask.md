# Exercise 10: PII Masking: Redact a Token Keeping the Last N Runes

A logging middleware that emits an API key or an account id must redact it first,
showing only the last few characters for support triage. Count by *bytes* and you
can split a multi-byte tail down the middle, emitting invalid UTF-8 into your log
pipeline. This is the mask helper that counts code points.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
redact/                     independent module: example.com/redact
  go.mod                    go 1.25
  internal/redact/redact.go Mask(s, keep) string
  internal/redact/redact_test.go keep/short/multibyte tables + valid-UTF8 check
  cmd/demo/main.go          runnable: mask an API key and an account id
```

Files: `internal/redact/redact.go`, `internal/redact/redact_test.go`,
`cmd/demo/main.go`.
Implement: `Mask(s string, keep int) string` that replaces all but the last `keep`
runes with a fixed mask rune, counting by code point.
Test: keep=4 masks the prefix, keep>=len returns the input unchanged, keep=0 masks
fully, a multi-byte tail stays intact and valid, empty string.
Verify: `go test -count=1 -race ./...`

### Counting the tail by runes, not bytes

`Mask` keeps the last `keep` *code points* and replaces everything before them
with a mask character. The subtlety is finding where the kept tail begins: you
cannot slice `keep` bytes off the end, because the last `keep` runes may be more
than `keep` bytes. So walk backward with `utf8.DecodeLastRuneInString`, `keep`
times, each step subtracting the decoded rune's `size` from the index. The
resulting index is the byte position where the kept tail starts, on a rune
boundary, so `s[idx:]` is always valid UTF-8 and never a split rune.

The masked region is `n - keep` runes wide, where `n = utf8.RuneCountInString(s)`.
The mask rune is ASCII `*`, so `strings.Repeat("*", n-keep)` produces exactly one
star per masked code point — the mask length matches the *rune* count of what was
hidden, not its byte length, which is the right invariant (an observer should not
learn that the hidden part was multi-byte).

Two policies are pinned. When `keep >= n` there is nothing to hide, so the input
is returned unchanged — masking a value shorter than the reveal window would leak
it entirely, so returning it as-is is the honest behavior for the caller to reason
about (and a caller that wants a hard floor can check length first). When `keep <=
0` the whole value is masked. A `café` masked with `keep=2` keeps `fé` — two runes,
three bytes — intact and valid.

Create `internal/redact/redact.go`:

```go
package redact

import (
	"strings"
	"unicode/utf8"
)

const maskRune = '*'

// Mask replaces all but the last keep code points of s with maskRune, counting
// by rune so a multi-byte tail is never split. If keep >= the rune count, s is
// returned unchanged; if keep <= 0, s is fully masked. The output is always valid
// UTF-8.
func Mask(s string, keep int) string {
	n := utf8.RuneCountInString(s)
	if keep < 0 {
		keep = 0
	}
	if keep >= n {
		return s
	}

	// Walk back keep runes to find where the kept tail begins.
	idx := len(s)
	for range keep {
		_, size := utf8.DecodeLastRuneInString(s[:idx])
		idx -= size
	}

	var b strings.Builder
	b.Grow((n - keep) + (len(s) - idx))
	b.WriteString(strings.Repeat(string(maskRune), n-keep))
	b.WriteString(s[idx:])
	return b.String()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"unicode/utf8"

	"example.com/redact/internal/redact"
)

func main() {
	secrets := []struct {
		label string
		value string
		keep  int
	}{
		{"api key", "sk_live_ABCD", 4},
		{"account", "acct_9f2c", 0},
		{"unicode", "café", 2},
		{"short", "xy", 4},
	}
	for _, s := range secrets {
		out := redact.Mask(s.value, s.keep)
		fmt.Printf("%-8s %q -> %q (valid=%v)\n", s.label, s.value, out, utf8.ValidString(out))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
api key  "sk_live_ABCD" -> "********ABCD" (valid=true)
account  "acct_9f2c" -> "*********" (valid=true)
unicode  "café" -> "**fé" (valid=true)
short    "xy" -> "xy" (valid=true)
```

`sk_live_ABCD` is 12 runes; keeping 4 masks the 8-rune prefix into eight stars.
`café` keeps `fé` (the two-rune, three-byte tail) intact. `xy` with `keep=4` is
shorter than the reveal window, so it is returned unchanged per the documented
policy.

### Tests

Create `internal/redact/redact_test.go`:

```go
package redact

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMask(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		keep int
		want string
	}{
		{"api key keep 4", "sk_live_ABCD", 4, "********ABCD"},
		{"keep zero", "secret", 0, "******"},
		{"keep ge len", "short", 10, "short"},
		{"keep eq len", "abcd", 4, "abcd"},
		{"multibyte tail", "café", 2, "**fé"},
		{"all multibyte", "日本語鍵", 1, "***鍵"},
		{"empty", "", 4, ""},
		{"negative keep", "abc", -1, "***"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Mask(tc.in, tc.keep)
			if got != tc.want {
				t.Fatalf("Mask(%q, %d) = %q, want %q", tc.in, tc.keep, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("Mask(%q, %d) = %q, not valid UTF-8", tc.in, tc.keep, got)
			}
		})
	}
}

func TestMaskLengthMatchesRuneCount(t *testing.T) {
	t.Parallel()
	// The number of mask runes must equal the rune count of the hidden region,
	// regardless of how many bytes those runes occupy.
	in := "café-münchen" // multi-byte runes in the masked prefix
	keep := 3
	got := Mask(in, keep)
	wantMask := utf8.RuneCountInString(in) - keep
	if n := strings.Count(got, "*"); n != wantMask {
		t.Fatalf("Mask(%q, %d) has %d mask runes, want %d", in, keep, n, wantMask)
	}
}

func ExampleMask() {
	fmt.Println(Mask("sk_live_ABCD", 4))
	// Output: ********ABCD
}
```

## Review

`Mask` is correct when the mask count equals the rune count of the hidden region
(never its byte count), when the kept tail is a whole number of runes and the
output is always valid UTF-8, and when the two edge policies hold: `keep >= n`
returns the input, `keep <= 0` masks everything. The mistake it prevents is
slicing the tail by bytes, which can split a multi-byte rune and emit invalid
UTF-8 into a log sink that then rejects or mojibakes the line. Run `go test -race`.

## Resources

- [`utf8.DecodeLastRuneInString`](https://pkg.go.dev/unicode/utf8#DecodeLastRuneInString)
- [`utf8.RuneCountInString`](https://pkg.go.dev/unicode/utf8#RuneCountInString)
- [`strings.Repeat`](https://pkg.go.dev/strings#Repeat)
- [Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings)

---

Back to [00-concepts.md](00-concepts.md) | Next: [../04-string-iteration-bytes-vs-runes/00-concepts.md](../04-string-iteration-bytes-vs-runes/00-concepts.md)
