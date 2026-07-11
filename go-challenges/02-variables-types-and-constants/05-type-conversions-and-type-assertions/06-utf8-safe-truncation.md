# Exercise 6: Truncate A String To A Byte Budget Without Corrupting UTF-8

Storing a field in a fixed-width `varchar` or bounding a log line means enforcing
a *byte* budget on a string that is a sequence of variable-width UTF-8 runes.
The naive `s[:max]` can slice through the middle of a multi-byte code point and
produce invalid UTF-8. This exercise builds a truncation that backs off to a rune
boundary so the result is always valid.

This module is fully self-contained: its own module, all code inline, its own
demo and tests.

## What you'll build

```text
utf8trunc/                   independent module: example.com/utf8trunc
  go.mod                     go 1.26
  truncate.go                TruncateBytes(s string, max int) string (rune-aware)
  cmd/
    demo/
      main.go                runnable demo: naive vs safe truncation of a multi-byte string
  truncate_test.go           ASCII cut, mid-rune backoff, wide-char/CJK, identity, max==0
```

- Files: `truncate.go`, `cmd/demo/main.go`, `truncate_test.go`.
- Implement: `TruncateBytes(s string, max int) string` returning the longest valid-UTF-8 prefix of `s` whose length in bytes is at most `max`.
- Test: exact ASCII cut, a budget landing mid-rune (result shorter and `utf8.ValidString==true`), an wide-char/CJK case, `max` larger than `len(s)` (identity), and `max==0`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/utf8trunc/cmd/demo
cd ~/go-exercises/utf8trunc
go mod init example.com/utf8trunc
go mod edit -go=1.26
```

### Why byte slicing corrupts and how to back off

A string in Go is bytes; UTF-8 encodes a rune in one to four of them. `"é"` is
two bytes (`0xC3 0xA9`), `"世"` is three, a `"𠮷"` ideograph is four. Slicing `s[:max]`
counts bytes with no regard for rune boundaries, so if `max` falls between the
lead byte and the continuation bytes of a rune, the prefix ends with an
incomplete sequence — invalid UTF-8. Some databases reject it outright; others
store the mojibake and you discover it in a support ticket.

The fix is to take the byte-bounded prefix and then trim any trailing bytes that
form an incomplete rune. `unicode/utf8.DecodeLastRuneInString(b)` returns the
last rune and its byte width; when the trailing bytes are not a valid complete
rune it returns `(utf8.RuneError, 1)` — the sentinel width `1` signalling "one
invalid byte", as opposed to a real `U+FFFD` replacement character which decodes
with its full width. So the loop removes one trailing byte at a time while the
last rune decodes as `RuneError` with width at most 1, stopping as soon as the
prefix ends on a complete rune. The result is the longest valid prefix within the
budget. The `max <= 0` and `max >= len(s)` cases short-circuit: an empty budget
yields the empty string, and a budget at least the full length is the identity.

Note this walks bytes, not `[]rune(s)`: converting to `[]rune` would allocate a
whole new slice and re-encode the entire string just to drop a few trailing
bytes. Backing off from the end touches at most three bytes and allocates
nothing.

Create `truncate.go`:

```go
// truncate.go
package utf8trunc

import "unicode/utf8"

// TruncateBytes returns the longest prefix of s that is valid UTF-8 and at most
// max bytes long. It never splits a multi-byte rune.
func TruncateBytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	b := s[:max]
	for len(b) > 0 {
		r, size := utf8.DecodeLastRuneInString(b)
		if r == utf8.RuneError && size <= 1 {
			// Incomplete trailing byte of a split rune: drop it and retry.
			b = b[:len(b)-1]
			continue
		}
		break
	}
	return b
}
```

### The runnable demo

The demo truncates a string whose byte budget lands in the middle of a
multi-byte rune, printing both the corrupt naive result's validity and the safe
result.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"unicode/utf8"

	"example.com/utf8trunc"
)

func main() {
	s := "café" // 5 bytes: c a f + é(2 bytes)
	const budget = 4

	naive := s[:budget] // cuts é in half
	fmt.Printf("naive: %q valid=%v\n", naive, utf8.ValidString(naive))

	safe := utf8trunc.TruncateBytes(s, budget)
	fmt.Printf("safe:  %q valid=%v\n", safe, utf8.ValidString(safe))

	wide := "hi𠮷"
	fmt.Printf("wide budget 3: %q\n", utf8trunc.TruncateBytes(wide, 3))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
naive: "caf\xc3" valid=false
safe:  "caf" valid=true
```

The wide-char line prints after; its 4-byte rune does not fit a 3-byte budget, so it
is dropped:

```
wide budget 3: "hi"
```

### Tests

Each test asserts the exact result and, for the mid-rune cases, that the output
is valid UTF-8 and no longer than the budget.

Create `truncate_test.go`:

```go
// truncate_test.go
package utf8trunc

import (
	"fmt"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"ascii exact", "hello", 3, "hel"},
		{"ascii identity", "hello", 5, "hello"},
		{"max over length", "hi", 10, "hi"},
		{"max zero", "hello", 0, ""},
		{"mid rune backoff", "café", 4, "caf"}, // é is 2 bytes at [3:5]
		{"cjk backoff", "世界", 4, "世"},          // each rune is 3 bytes
		{"wide char dropped", "hi𠮷", 3, "hi"},  // this ideograph is 4 bytes
		{"wide char fits", "hi𠮷", 6, "hi𠮷"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := TruncateBytes(tt.s, tt.max)
			if got != tt.want {
				t.Fatalf("TruncateBytes(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
			}
			if len(got) > tt.max && tt.max > 0 {
				t.Fatalf("result %q exceeds budget %d", got, tt.max)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("result %q is not valid UTF-8", got)
			}
		})
	}
}

func ExampleTruncateBytes() {
	fmt.Println(TruncateBytes("café", 4))
	// Output: caf
}
```

## Review

The function is correct when its output is always valid UTF-8, never longer than
`max` bytes, and equal to `s` when the budget already covers it. The load-bearing
detail is the `size <= 1` guard on `RuneError`: a genuine `U+FFFD` in the input
decodes with width 3 and must be kept, while a split rune's trailing bytes decode
as `RuneError` width 1 and must be dropped — testing only `r == utf8.RuneError`
would wrongly strip a legitimate replacement character. The test asserts
`utf8.ValidString(got)` on every case precisely so a regression to naive slicing
fails loudly. For a *character* budget rather than a byte budget the tool changes
to `utf8.RuneCountInString` plus a rune-index walk, but the byte budget is what a
`varchar(n)` and most log limits actually impose.

## Resources

- [unicode/utf8.DecodeLastRuneInString](https://pkg.go.dev/unicode/utf8#DecodeLastRuneInString) — last-rune decoding and the `RuneError`/size-1 signal.
- [unicode/utf8.ValidString](https://pkg.go.dev/unicode/utf8#ValidString) — checking a string is well-formed UTF-8.
- [unicode/utf8.RuneCountInString](https://pkg.go.dev/unicode/utf8#RuneCountInString) — counting runes for a character budget.
- [Strings, bytes, runes and characters in Go](https://go.dev/blog/strings) — the Go blog on UTF-8 and string internals.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-zero-copy-bytes-string-hotpath.md](07-zero-copy-bytes-string-hotpath.md)
