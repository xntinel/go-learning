# Exercise 4: Fuzz An HTTP Header Line Parser For Panic-Freedom

A reverse proxy reads raw header lines off a socket and splits each into a
canonical key and a value. Those bytes are fully attacker-controlled: embedded
newlines, no colon at all, invalid UTF-8, control characters. This module builds
`ParseHeaderLine` and fuzzes it for the weakest-yet-essential property at an
untrusted boundary — it never panics — plus an output invariant: when it accepts
a line, the key it returns is already in canonical form and the value is trimmed.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
header/                    independent module: example.com/header
  go.mod                   module path
  header.go                ParseHeaderLine(string) (key, value string, ok bool)
  cmd/
    demo/
      main.go              parse a handful of raw lines, print key/value/ok
  header_test.go           TestParseHeaderLineTable, FuzzParseHeaderLine, Example
```

Files: `header.go`, `cmd/demo/main.go`, `header_test.go`.
Implement: `ParseHeaderLine(line string) (key, value string, ok bool)` splitting on
the first colon, rejecting invalid field-name tokens, canonicalizing the key, and
trimming the value.
Test: a table test for the contract; `FuzzParseHeaderLine` proving no panic and
the canonical-key / trimmed-value invariant.
Verify: `go test -race ./...`, then `go test -fuzz=FuzzParseHeaderLine
-fuzztime=2s`.

Set up the module:

```bash
mkdir -p ~/go-exercises/header/cmd/demo
cd ~/go-exercises/header
go mod init example.com/header
```

### Splitting, validating, and canonicalizing

A header line is `field-name : field-value`. `strings.Cut(line, ":")` splits on
the first colon and reports whether one was found — cleaner and allocation-lean
compared to `strings.SplitN`. No colon means not a header line, so `ok` is false.

A raw key must be validated before it is trusted. RFC 7230 restricts a field name
to *token* characters (letters, digits, and a fixed set of punctuation); a space,
a control byte, or a stray newline in the key means the line is malformed and the
parser must reject it rather than emit garbage. Rejecting invalid keys is what
makes `ok == true` meaningful: an accepted line has a real field name. The valid
key is then passed through `textproto.CanonicalMIMEHeaderKey`, which normalizes
`content-type` and `CONTENT-TYPE` alike to `Content-Type` — the same
canonicalization `net/http` applies, so downstream lookups are consistent. The
value is `strings.TrimSpace`d, because header values carry optional leading and
trailing whitespace that is not part of the value.

The fuzz target's first job is trivial to state and vital in practice: *call the
parser and do not crash*. A slice index computed from attacker bytes, a rune
decode that walks off the end — any of these would panic, and a panic in a
per-request parser is a denial of service. Because the body simply calls the
function, any panic fails the test. Its second job is the output invariant: when
`ok` is true, `key == textproto.CanonicalMIMEHeaderKey(key)` (the key is already
canonical, so canonicalizing again is a no-op), `key` is valid UTF-8, and
`strings.TrimSpace(value) == value`.

Create `header.go`:

```go
package header

import (
	"net/textproto"
	"strings"
)

// isTokenByte reports whether c is a valid HTTP field-name character per RFC
// 7230 (a "token" character).
func isTokenByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

// ParseHeaderLine splits a raw header line into a canonicalized key and a trimmed
// value. It returns ok == false when there is no colon, the key is empty, or the
// key contains a non-token byte. It never panics on any input.
func ParseHeaderLine(line string) (key, value string, ok bool) {
	rawKey, rawVal, found := strings.Cut(line, ":")
	if !found || rawKey == "" {
		return "", "", false
	}
	for i := 0; i < len(rawKey); i++ {
		if !isTokenByte(rawKey[i]) {
			return "", "", false
		}
	}
	return textproto.CanonicalMIMEHeaderKey(rawKey), strings.TrimSpace(rawVal), true
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/header"
)

func main() {
	lines := []string{
		"content-type:  application/json  ",
		"X-Request-Id: abc-123",
		"no colon here",
		"bad key: value",
	}
	for _, l := range lines {
		k, v, ok := header.ParseHeaderLine(l)
		if ok {
			fmt.Printf("ok   %q -> %q\n", k, v)
		} else {
			fmt.Printf("skip %q\n", l)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ok   "Content-Type" -> "application/json"
ok   "X-Request-Id" -> "abc-123"
skip "no colon here"
skip "bad key: value"
```

### Tests

Create `header_test.go`:

```go
package header

import (
	"fmt"
	"net/textproto"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseHeaderLineTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    string
		key   string
		value string
		ok    bool
	}{
		{"canonicalizes", "content-type: text/html", "Content-Type", "text/html", true},
		{"trims value", "X-Id:   7  ", "X-Id", "7", true},
		{"no colon", "garbage line", "", "", false},
		{"empty key", ": value", "", "", false},
		{"space in key", "bad key: v", "", "", false},
		{"embedded newline in key", "a\nb: v", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			k, v, ok := ParseHeaderLine(tc.in)
			if ok != tc.ok || k != tc.key || v != tc.value {
				t.Fatalf("ParseHeaderLine(%q) = (%q,%q,%v), want (%q,%q,%v)", tc.in, k, v, ok, tc.key, tc.value, tc.ok)
			}
		})
	}
}

func FuzzParseHeaderLine(f *testing.F) {
	seeds := []string{
		"Content-Type: application/json",
		"x-id:\t42\t",
		"no colon",
		": empty key",
		"a\r\nb: smuggle",
		"key: \xff\xfe not utf8",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		key, value, ok := ParseHeaderLine(line) // a panic here fails the test
		if !ok {
			return
		}
		if key != textproto.CanonicalMIMEHeaderKey(key) {
			t.Fatalf("ParseHeaderLine(%q) key %q is not canonical", line, key)
		}
		if !utf8.ValidString(key) {
			t.Fatalf("ParseHeaderLine(%q) produced non-UTF8 key %q", line, key)
		}
		if strings.TrimSpace(value) != value {
			t.Fatalf("ParseHeaderLine(%q) value %q has untrimmed whitespace", line, value)
		}
	})
}

func Example() {
	k, v, ok := ParseHeaderLine("accept-encoding:  gzip  ")
	fmt.Printf("%q %q %v\n", k, v, ok)
	// Output: "Accept-Encoding" "gzip" true
}
```

## Review

The parser is correct when it accepts exactly the lines with a colon and a valid
token key, canonicalizes that key the way `net/http` does, and trims the value —
and, above all, never panics on any byte sequence. The no-panic property is the
one that matters most operationally: a per-request parser that can be crashed by a
crafted header is a remote denial of service, and fuzzing is how you prove the
crash surface is empty. The output invariant catches the subtler regression where
someone drops the canonicalization and returns the raw key. Note the key
validation is what makes `ok` a real signal rather than "there was a colon
somewhere". Run `go test -race ./...`, then
`go test -fuzz=FuzzParseHeaderLine -fuzztime=2s`.

## Resources

- [`net/textproto.CanonicalMIMEHeaderKey`](https://pkg.go.dev/net/textproto#CanonicalMIMEHeaderKey) — the exact canonicalization `net/http` uses.
- [`strings.Cut`](https://pkg.go.dev/strings#Cut) — split-on-first-separator with a found flag.
- [`unicode/utf8.ValidString`](https://pkg.go.dev/unicode/utf8#ValidString) — the UTF-8 validity check the invariant asserts.

---

Back to [03-differential-ipv4-parser.md](03-differential-ipv4-parser.md) | Next: [05-json-body-decoder-limits.md](05-json-body-decoder-limits.md)
