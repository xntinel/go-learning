# Exercise 4: Truncating Text for a varchar(N) Column Without Splitting a Rune

A `varchar(255)` counts *bytes* in most engines, but a naive `s[:255]` can land
inside a multi-byte rune and emit a lone `0xC3`, which then breaks the DB write,
JSON marshaling, or the next consumer. This is the function that saves a title
into a byte-budgeted column without ever producing a broken tail.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
truncate/                   independent module: example.com/truncate
  go.mod                    go 1.25
  internal/truncate/truncate.go ByBytes, ByRunes
  internal/truncate/truncate_test.go boundary, budget, and ellipsis tables
  cmd/demo/main.go          runnable: truncate a title two ways
```

Files: `internal/truncate/truncate.go`, `internal/truncate/truncate_test.go`,
`cmd/demo/main.go`.
Implement: `ByBytes(s string, maxBytes int) string` that trims to a byte budget on
a rune boundary; `ByRunes(s string, maxRunes int) string` that trims to a
code-point budget and appends an ellipsis when it cut.
Test: mid-rune byte budget never yields invalid UTF-8, `ByRunes` counts runes not
bytes and only appends when cutting, plus budget-0 and exact-length edges.
Verify: `go test -count=1 -race ./...`

### Walking back to a rune boundary

`ByBytes` first checks the fast path: if `len(s) <= maxBytes` the whole string
fits and is returned as is. Otherwise it slices to the byte budget and then walks
*backward* to the nearest rune boundary. `utf8.DecodeLastRuneInString` is the key:
on a string that ends mid-rune (its last byte is a continuation byte, or a lead
byte with missing continuations) it returns `(utf8.RuneError, 1)`. That `size ==
1` distinguishes an *incomplete* trailing rune from a legitimately-encoded
`U+FFFD` (which decodes with `size == 3`), so you drop one byte and retry until
the tail decodes cleanly. UTF-8's self-synchronization guarantees this backward
walk terminates within at most three bytes. The result never exceeds `maxBytes`
and always passes `utf8.ValidString`.

`ByRunes` budgets by code point instead — the right unit when a product rule says
"titles show at most 40 characters". It counts runes with
`utf8.RuneCountInString`; if the string is already within budget it is returned
untouched (no ellipsis, because nothing was cut). Otherwise it keeps the first
`maxRunes` runes and appends a single ellipsis rune `…` to signal truncation. The
ellipsis is appended beyond the content budget, which is the common product
convention; the load-bearing detail is that the count is runes, so `café`
truncated to 2 runes is `ca…`, not a byte slice that might split `é`.

Create `internal/truncate/truncate.go`:

```go
package truncate

import (
	"strings"
	"unicode/utf8"
)

// ByBytes trims s to at most maxBytes bytes, backing up to a rune boundary so the
// result is always valid UTF-8 and never ends in a partial multi-byte rune.
func ByBytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	b := s[:maxBytes]
	for len(b) > 0 {
		r, size := utf8.DecodeLastRuneInString(b)
		if r == utf8.RuneError && size <= 1 {
			b = b[:len(b)-1] // drop an incomplete trailing byte and retry
			continue
		}
		break
	}
	return b
}

// ByRunes trims s to at most maxRunes code points, appending a single ellipsis
// rune when (and only when) truncation occurred. A string within budget is
// returned unchanged.
func ByRunes(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	if maxRunes < 0 {
		maxRunes = 0
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= maxRunes {
			break
		}
		b.WriteRune(r)
		n++
	}
	b.WriteRune('…')
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

	"example.com/truncate/internal/truncate"
)

func main() {
	title := "café-menu"
	for _, budget := range []int{4, 5} {
		out := truncate.ByBytes(title, budget)
		fmt.Printf("ByBytes(%q, %d) = %q (len=%d valid=%v)\n",
			title, budget, out, len(out), utf8.ValidString(out))
	}
	fmt.Printf("ByRunes(%q, 2) = %q\n", "café", truncate.ByRunes("café", 2))
	fmt.Printf("ByRunes(%q, 9) = %q\n", title, truncate.ByRunes(title, 9))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ByBytes("café-menu", 4) = "caf" (len=3 valid=true)
ByBytes("café-menu", 5) = "café" (len=5 valid=true)
ByRunes("café", 2) = "ca…"
ByRunes("café-menu", 9) = "café-menu"
```

Budget 4 lands inside `é` (bytes `0xC3 0xA9` at offsets 3-4), so `ByBytes` backs
up to `caf`; budget 5 fits the whole `café`. `ByRunes("café-menu", 9)` is a no-op
because the string is exactly 9 runes.

### Tests

Create `internal/truncate/truncate_test.go`:

```go
package truncate

import (
	"fmt"
	"testing"
	"unicode/utf8"
)

func TestByBytesNeverSplitsRune(t *testing.T) {
	t.Parallel()
	// "café" is c a f 0xC3 0xA9; budget 4 lands inside é.
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"café", 4, "caf"},
		{"café", 5, "café"},
		{"café", 3, "caf"},
		{"café", 100, "café"},
		{"café", 0, ""},
		{"日本語", 4, "日"}, // each rune is 3 bytes
		{"日本語", 6, "日本"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := ByBytes(tc.in, tc.max)
			if got != tc.want {
				t.Fatalf("ByBytes(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if len(got) > tc.max && tc.max >= 0 {
				t.Fatalf("ByBytes(%q, %d) = %q exceeds budget", tc.in, tc.max, got)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("ByBytes(%q, %d) = %q, not valid UTF-8", tc.in, tc.max, got)
			}
		})
	}
}

func TestByRunes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"café-menu", 4, "café…"},
		{"café", 2, "ca…"},
		{"hello", 5, "hello"}, // exact length: no ellipsis
		{"hello", 10, "hello"},
		{"abc", 0, "…"},
		{"日本語テスト", 2, "日本…"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := ByRunes(tc.in, tc.max); got != tc.want {
				t.Fatalf("ByRunes(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

func TestByRunesNoOpUnderBudget(t *testing.T) {
	t.Parallel()
	in := "résumé"
	if got := ByRunes(in, 6); got != in {
		t.Fatalf("ByRunes(%q, 6) = %q, want unchanged", in, got)
	}
}

func ExampleByBytes() {
	fmt.Println(ByBytes("café", 4))
	// Output: caf
}
```

## Review

`ByBytes` is correct when its output never exceeds the byte budget and always
passes `utf8.ValidString`, even when the budget lands mid-rune — the backward walk
with `utf8.DecodeLastRuneInString` is what guarantees that. `ByRunes` is correct
when it counts code points (not bytes), appends the ellipsis only on an actual
cut, and is a no-op under budget. The mistake this exercise exists to prevent is
`s[:n]` on a raw byte offset, which emits a broken tail; the second, subtler one
is confusing the two budgets — bytes for a storage column, runes for a display
rule. Run `go test -race`.

## Resources

- [`utf8.DecodeLastRuneInString`](https://pkg.go.dev/unicode/utf8#DecodeLastRuneInString)
- [`utf8.RuneCountInString`](https://pkg.go.dev/unicode/utf8#RuneCountInString)
- [`utf8.RuneLen`](https://pkg.go.dev/unicode/utf8#RuneLen)
- [Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings)

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-username-rune-validation.md](05-username-rune-validation.md)
