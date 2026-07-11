# Exercise 3: Rejecting Invalid UTF-8 at the API Ingest Boundary

A Postgres `text` column will reject a string that is not valid UTF-8, and it
will do so deep inside your write path with an opaque driver error. The senior
move is to validate at ingest: return a clean `400` that names the offending
field and byte offset, or repair the bytes for best-effort storage — before the
value ever reaches the database.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
ingest/                     independent module: example.com/ingest
  go.mod                    go 1.25
  internal/ingest/ingest.go ValidateUTF8, SanitizeUTF8, ErrInvalidUTF8
  internal/ingest/ingest_test.go raw-byte tables + offset assertions
  cmd/demo/main.go          runnable: validate and sanitize sample fields
```

Files: `internal/ingest/ingest.go`, `internal/ingest/ingest_test.go`,
`cmd/demo/main.go`.
Implement: `ValidateUTF8(field, s string) error` returning a field-and-offset
error wrapping `ErrInvalidUTF8`; `SanitizeUTF8(s string) string` replacing
invalid sequences with `U+FFFD`.
Test: lone continuation byte, truncated sequence, overlong encoding, valid
multi-byte; assert offsets and that sanitized output is valid UTF-8.
Verify: `go test -count=1 -race ./...`

### Why validate here, and how to find the offset

`utf8.ValidString(s)` answers the yes/no question in one pass, but a `400` that
just says "invalid UTF-8" is useless to the client. To name the *first* bad byte
you decode rune by rune: `utf8.DecodeRuneInString(s[i:])` returns
`(utf8.RuneError, 1)` exactly when the bytes at `i` are not a valid encoding, so
the offset of the first such `i` is the position to report. A valid rune advances
`i` by its real width; an invalid one would advance by 1, so you stop at the first
one and report `i`. The error wraps a package-level sentinel with `%w` so a
handler can branch on `errors.Is(err, ErrInvalidUTF8)` to map it to `400` while
still surfacing the field and offset in the message.

`SanitizeUTF8` is the other half of the policy: when you would rather store
best-effort than reject, `strings.ToValidUTF8(s, "�")` replaces each maximal
invalid subsequence with the replacement character, guaranteeing the result
passes `utf8.ValidString`. The two functions encode the two real policies — reject
at the boundary, or repair and store — and a service picks one per field (reject a
username, repair a free-text log line).

The invalid inputs to reason about: a lone `0x80` is a continuation byte with no
lead, invalid on its own. `\xe2\x82` is the first two bytes of the three-byte euro
sign `€` (`\xe2\x82\xac`) — a *truncated* sequence. `\xc0\xaf` is an *overlong*
encoding of `/`, forbidden because UTF-8 requires the shortest form. All three
must be rejected; a valid multi-byte string like `café` must pass.

Set up the module:

```bash
mkdir -p ~/go-exercises/ingest/internal/ingest ~/go-exercises/ingest/cmd/demo
cd ~/go-exercises/ingest
go mod init example.com/ingest
```

Create `internal/ingest/ingest.go`:

```go
package ingest

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrInvalidUTF8 is the sentinel a handler maps to HTTP 400.
var ErrInvalidUTF8 = errors.New("invalid UTF-8")

// ValidateUTF8 reports the first byte offset at which field s is not valid
// UTF-8, wrapping ErrInvalidUTF8. It returns nil for valid input.
func ValidateUTF8(field, s string) error {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return fmt.Errorf("field %q: %w at byte offset %d", field, ErrInvalidUTF8, i)
		}
		i += size
	}
	return nil
}

// SanitizeUTF8 returns s with every invalid byte sequence replaced by U+FFFD,
// so the result is always valid UTF-8 and safe to store.
func SanitizeUTF8(s string) string {
	return strings.ToValidUTF8(s, "�")
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"unicode/utf8"

	"example.com/ingest/internal/ingest"
)

func main() {
	fields := map[string]string{
		"title":   "café",
		"bio":     "bad\x80byte",
		"comment": "euro\xe2\x82",
	}
	for _, name := range []string{"title", "bio", "comment"} {
		s := fields[name]
		if err := ingest.ValidateUTF8(name, s); err != nil {
			clean := ingest.SanitizeUTF8(s)
			fmt.Printf("%s: reject (%v); sanitized=%q valid=%v\n",
				name, err, clean, utf8.ValidString(clean))
		} else {
			fmt.Printf("%s: accept %q\n", name, s)
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
title: accept "café"
bio: reject (field "bio": invalid UTF-8 at byte offset 3); sanitized="bad�byte" valid=true
comment: reject (field "comment": invalid UTF-8 at byte offset 4); sanitized="euro�" valid=true
```

### Tests

Create `internal/ingest/ingest_test.go`:

```go
package ingest

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidateUTF8(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		wantErr bool
		offset  int
	}{
		{"valid ascii", "hello", false, 0},
		{"valid multibyte", "café résumé", false, 0},
		{"lone continuation", "\x80", true, 0},
		{"continuation midway", "ab\x80cd", true, 2},
		{"truncated sequence", "euro\xe2\x82", true, 4},
		{"overlong slash", "\xc0\xaf", true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateUTF8("f", tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidUTF8) {
					t.Fatalf("ValidateUTF8(%q) err = %v, want ErrInvalidUTF8", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateUTF8(%q) = %v, want nil", tc.in, err)
			}
		})
	}
}

func TestValidateUTF8Offset(t *testing.T) {
	t.Parallel()
	// The reported offset must be the first bad byte, so it stays correct even
	// after valid multi-byte runes precede it.
	err := ValidateUTF8("bio", "café\x80")
	if !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("err = %v, want ErrInvalidUTF8", err)
	}
	if got := err.Error(); !strings.Contains(got, "offset 5") {
		t.Fatalf("error %q, want it to mention byte offset 5", got)
	}
}

func TestSanitizeUTF8(t *testing.T) {
	t.Parallel()
	tests := []string{"café", "bad\x80byte", "euro\xe2\x82", "\xff\xfe", ""}
	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			out := SanitizeUTF8(in)
			if !utf8.ValidString(out) {
				t.Fatalf("SanitizeUTF8(%q) = %q, not valid UTF-8", in, out)
			}
		})
	}
}

func TestSanitizePreservesValidRunes(t *testing.T) {
	t.Parallel()
	if got := SanitizeUTF8("café"); got != "café" {
		t.Fatalf("SanitizeUTF8(café) = %q, want café unchanged", got)
	}
}

func ExampleValidateUTF8() {
	fmt.Println(ValidateUTF8("name", "bad\x80"))
	// Output: field "name": invalid UTF-8 at byte offset 3
}
```

`café\x80` places the bad byte after a valid multi-byte run, so `café` (4 runes,
5 bytes) means the `0x80` is at byte offset 5 — the test pins that the offset is
byte-based and correct past multi-byte runes.

## Review

`ValidateUTF8` is correct when it accepts every valid string and, for an invalid
one, names the first bad byte's offset while wrapping `ErrInvalidUTF8` so a
handler can branch with `errors.Is`. The offset must be byte-based — reporting a
rune index here would misalign with what the storage layer sees. `SanitizeUTF8`
is correct when its output always passes `utf8.ValidString` and leaves valid runes
untouched. The trap is trusting `range` to preserve untrusted bytes: it silently
substitutes `U+FFFD`, so if you must *reject* rather than repair, validate
explicitly first. Run `go test -race`.

## Resources

- [`utf8.ValidString`](https://pkg.go.dev/unicode/utf8#ValidString)
- [`strings.ToValidUTF8`](https://pkg.go.dev/strings#ToValidUTF8)
- [`utf8.DecodeRuneInString`](https://pkg.go.dev/unicode/utf8#DecodeRuneInString)
- [The UTF-8 encoding (RFC 3629)](https://datatracker.ietf.org/doc/html/rfc3629)

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-rune-safe-truncate-varchar.md](04-rune-safe-truncate-varchar.md)
