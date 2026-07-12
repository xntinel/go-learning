# Exercise 3: Reject Invalid UTF-8 at the Ingest Boundary Before Persisting

A Go string can hold bytes that are not valid UTF-8, so an HTTP write path that
copies a request field straight into a text column is one bad byte away from a
failed insert or a mangled JSON re-encode deep in the stack. This module builds the
ingest guard that rejects invalid UTF-8 up front, with a sentinel error and the byte
offset of the first bad byte for diagnostics.

## What you'll build

```text
utf8ingest/                independent module: example.com/utf8ingest
  go.mod                   go 1.26
  validate.go              ValidateText, ErrInvalidUTF8, InvalidUTF8Error
  cmd/
    demo/
      main.go              accepts good input, reports offset on bad
  validate_test.go         accept valid; reject four malformations at right offset
```

Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
Implement: `ValidateText(field string, raw []byte) error` wrapping `ErrInvalidUTF8`.
Test: accept ASCII/Latin/CJK/emoji; reject `0xff`, overlong NUL, truncated sequence, lone continuation; assert `errors.Is` and offset.
Verify: `go test -count=1 -race ./...`

### Fast path, then locate the first bad byte

The common case at ingest is valid input, so the function opens with the O(n)
`utf8.Valid`, which returns true after a single scan and allocates nothing — no
decoded copy of the body is made. Only when validation fails do you spend the second
pass to locate the offending byte, because a good error names *where* the problem is.

To find the first invalid byte, decode rune by rune over the slice with
`utf8.DecodeRune`, advancing by each rune's `size`. The failure signature is the one
from the concepts file: `r == utf8.RuneError && size == 1`. Testing `size == 1` is
essential — a legitimate U+FFFD already present in valid input decodes as
`(RuneError, 3)` and must not be flagged. When the signature hits, the current
offset is the first bad byte; wrap it into a typed error.

The malformations this rejects are the classic families:

- a stray byte no UTF-8 rune can start with, e.g. `0xff`;
- an overlong encoding, e.g. `0xC0 0x80` — a two-byte spelling of NUL that UTF-8
  forbids because every code point must use its shortest form (overlongs are a
  historical security hole, used to smuggle `/` or NUL past naive filters);
- a truncated sequence, e.g. the first two bytes of a three-byte rune with the third
  missing;
- a lone continuation byte, e.g. `0x80`, with no lead byte before it.

The error carries the field name and offset and wraps a package sentinel, so callers
can branch on `errors.Is(err, ErrInvalidUTF8)` (map to HTTP 400) while logs still get
the precise offset.

Create `validate.go`:

```go
// validate.go
package utf8ingest

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// ErrInvalidUTF8 is the sentinel every rejection wraps, so callers can branch
// with errors.Is without depending on the message.
var ErrInvalidUTF8 = errors.New("invalid UTF-8")

// InvalidUTF8Error reports which field failed and the byte offset of the first
// invalid byte.
type InvalidUTF8Error struct {
	Field  string
	Offset int
}

func (e *InvalidUTF8Error) Error() string {
	return fmt.Sprintf("field %q: invalid UTF-8 at byte offset %d", e.Field, e.Offset)
}

// Unwrap ties the typed error to the sentinel for errors.Is.
func (e *InvalidUTF8Error) Unwrap() error { return ErrInvalidUTF8 }

// ValidateText reports nil if raw is well-formed UTF-8, or an *InvalidUTF8Error
// (wrapping ErrInvalidUTF8) naming the first invalid byte offset. The happy path
// is a single utf8.Valid scan with no allocation.
func ValidateText(field string, raw []byte) error {
	if utf8.Valid(raw) {
		return nil
	}
	// Invalid: locate the first bad byte for the diagnostic.
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && size == 1 {
			return &InvalidUTF8Error{Field: field, Offset: i}
		}
		i += size
	}
	// utf8.Valid said false, so the loop must have found the bad byte; this is
	// defensive and should be unreachable.
	return &InvalidUTF8Error{Field: field, Offset: len(raw)}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"errors"
	"fmt"

	"example.com/utf8ingest"
)

func main() {
	good := []byte("café 中文 �e")
	if err := utf8ingest.ValidateText("bio", good); err != nil {
		fmt.Println("unexpected:", err)
	} else {
		fmt.Println("bio: accepted")
	}

	bad := []byte("ab\xffcd") // 0xff at offset 2
	err := utf8ingest.ValidateText("bio", bad)
	fmt.Println("bad:", err)
	fmt.Println("is ErrInvalidUTF8:", errors.Is(err, utf8ingest.ErrInvalidUTF8))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
bio: accepted
bad: field "bio": invalid UTF-8 at byte offset 2
is ErrInvalidUTF8: true
```

### Tests

The tests accept every well-formed family and reject each malformation at the exact
offset, asserting both the `errors.Is` contract and the reported position, so a
regression that returns the wrong offset — or false-positives on a real U+FFFD — is
caught.

Create `validate_test.go`:

```go
// validate_test.go
package utf8ingest

import (
	"errors"
	"fmt"
	"testing"
)

func TestValidateTextAccepts(t *testing.T) {
	t.Parallel()
	good := []string{
		"",
		"plain ascii",
		"café",
		"中文字符",
		"cjk-ext 𠀀 ok",
		"� is a real character", // legitimate U+FFFD, must be accepted
	}
	for _, s := range good {
		if err := ValidateText("f", []byte(s)); err != nil {
			t.Errorf("ValidateText(%q) = %v, want nil", s, err)
		}
	}
}

func TestValidateTextRejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		raw        []byte
		wantOffset int
	}{
		{"stray 0xff", []byte{'a', 0xff, 'b'}, 1},
		{"overlong NUL", []byte{0xc0, 0x80}, 0},
		{"truncated 3-byte", []byte("ok\xe4\xb8"), 2}, // missing final byte of 中
		{"lone continuation", []byte{0x80}, 0},
	}
	for _, tc := range cases {
		err := ValidateText("payload", tc.raw)
		if err == nil {
			t.Fatalf("%s: got nil, want error", tc.name)
		}
		if !errors.Is(err, ErrInvalidUTF8) {
			t.Errorf("%s: errors.Is(err, ErrInvalidUTF8) = false", tc.name)
		}
		var ie *InvalidUTF8Error
		if !errors.As(err, &ie) {
			t.Fatalf("%s: err is not *InvalidUTF8Error", tc.name)
		}
		if ie.Offset != tc.wantOffset {
			t.Errorf("%s: offset = %d, want %d", tc.name, ie.Offset, tc.wantOffset)
		}
		if ie.Field != "payload" {
			t.Errorf("%s: field = %q, want payload", tc.name, ie.Field)
		}
	}
}

func ExampleValidateText() {
	err := ValidateText("name", []byte{0xff})
	fmt.Println(err)
	fmt.Println(errors.Is(err, ErrInvalidUTF8))
	// Output:
	// field "name": invalid UTF-8 at byte offset 0
	// true
}
```

## Review

The guard is correct when it accepts every valid string — including one that
legitimately contains U+FFFD — and rejects each malformation at the byte offset a
human would point to. The two facts that make it right: the happy path is a single
`utf8.Valid` call that allocates nothing, and the failure test is `r ==
utf8.RuneError && size == 1`, never `r == utf8.RuneError` alone (which would flag
real replacement characters). Wrapping `ErrInvalidUTF8` via `Unwrap` lets a handler
map any rejection to a 400 with `errors.Is` while logs keep the offset. Reject is one
policy; when the boundary must not drop records, the next module repairs instead.

## Resources

- [unicode/utf8: Valid, DecodeRune, RuneError](https://pkg.go.dev/unicode/utf8) — the scan and the exact invalid-byte signature.
- [errors: Is, As, Unwrap](https://pkg.go.dev/errors) — sentinel wrapping and typed-error extraction.
- [The Unicode Standard, Conformance (D92, overlong forms)](https://www.unicode.org/versions/latest/) — why overlong encodings are ill-formed.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-rune-safe-truncate-to-byte-budget.md](02-rune-safe-truncate-to-byte-budget.md) | Next: [04-utf8-scrubber-replace-invalid.md](04-utf8-scrubber-replace-invalid.md)
