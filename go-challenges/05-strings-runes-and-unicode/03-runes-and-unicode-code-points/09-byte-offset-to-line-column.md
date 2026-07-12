# Exercise 9: Parser Error Positions: Mapping a Byte Offset to Line and Rune Column

`encoding/json` and most scanners report an error position as a *byte* offset.
A user-facing config-validation message wants a 1-based line and a column — and
the column must count *runes*, or a line containing `café` reports a column that
is off by one for every multi-byte rune before the error.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
pos/                        independent module: example.com/pos
  go.mod                    go 1.25
  internal/pos/pos.go       LineCol(src, byteOffset) (line, col int)
  internal/pos/pos_test.go  first-line, post-newline, multi-byte column tables
  cmd/demo/main.go          runnable: map a JSON error offset to line:col
```

Files: `internal/pos/pos.go`, `internal/pos/pos_test.go`, `cmd/demo/main.go`.
Implement: `LineCol(src string, byteOffset int) (line, col int)` returning a
1-based line and a 1-based rune column.
Test: offset in the first line, offset after a newline, offset on a line with a
multi-byte rune, offset at a newline, offset past end clamped.
Verify: `go test -count=1 -race ./...`

### Line by bytes, column by runes

The line number is a byte operation: count the `\n` bytes before the offset and
add one. `strings.Count(src[:off], "\n") + 1` does exactly that, and newlines are
single ASCII bytes so there is no rune subtlety on the line axis.

The column is where the rune-vs-byte distinction bites. The column is the position
*within the current line*, and a user counts characters, not bytes. So find where
the current line starts — `strings.LastIndexByte(src[:off], '\n') + 1`, which is
`0` when there is no preceding newline because `LastIndexByte` returns `-1` — and
then count the *runes* between that line start and the offset with
`utf8.RuneCountInString(src[lineStart:off])`, adding one for a 1-based column. On a
line reading `abcafé` with the offset at the end, the byte length is 7 (the `é` is
two bytes) but the rune count is 6, so the column is 7, not 8. Reporting the byte
count here would point the caret one column too far to the right on every line
that contains a multi-byte rune before the error.

Two edge cases round it out. An offset that lands exactly on a `\n` still belongs
to the line the newline ends — it is the column just past the last visible
character — so no special casing is needed; the formula produces it. And an offset
past the end of the source (or negative) is clamped into `[0, len(src)]` so a
malformed report from an upstream scanner yields a valid position instead of a
panic.

Create `internal/pos/pos.go`:

```go
package pos

import (
	"strings"
	"unicode/utf8"
)

// LineCol converts a byte offset (as reported by encoding/json or a scanner)
// into a 1-based line and a 1-based RUNE column. The offset is clamped into
// [0, len(src)]. The line is counted by newline bytes; the column counts runes
// within the current line, so a multi-byte rune before the offset does not skew
// it.
func LineCol(src string, byteOffset int) (line, col int) {
	if byteOffset < 0 {
		byteOffset = 0
	}
	if byteOffset > len(src) {
		byteOffset = len(src)
	}
	prefix := src[:byteOffset]
	line = strings.Count(prefix, "\n") + 1
	lineStart := strings.LastIndexByte(prefix, '\n') + 1 // 0 when no newline
	col = utf8.RuneCountInString(prefix[lineStart:]) + 1
	return line, col
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/pos/internal/pos"
)

func main() {
	// A config with an accented comment on line 2 and an injected NUL error.
	src := "name = ok\n# café ->\x00 bad\nport = 8080"
	off := strings.IndexByte(src, 0) // byte offset of the NUL on line 2
	line, col := pos.LineCol(src, off)
	fmt.Printf("error at byte %d -> line %d, column %d\n", off, line, col)

	line, col = pos.LineCol(src, 0)
	fmt.Printf("start -> line %d, column %d\n", line, col)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
error at byte 20 -> line 2, column 10
start -> line 1, column 1
```

Line 2 is `# café ->` then the NUL; the `é` is two bytes, so although the NUL sits
at byte offset 20, its rune column is 10 — the rune count is what makes the caret
land on the right character rather than drifting right by the extra `é` byte.

### Tests

Create `internal/pos/pos_test.go`:

```go
package pos

import (
	"fmt"
	"strings"
	"testing"
)

func TestLineCol(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		src      string
		off      int
		wantLine int
		wantCol  int
	}{
		{"first line", "hello world", 3, 1, 4},
		{"start", "hello", 0, 1, 1},
		{"after newline", "hello\nworld", 6, 2, 1},
		{"mid second line", "hello\nworld", 8, 2, 3},
		{"multibyte column", "abcafé", 7, 1, 7}, // é is 2 bytes; col counts runes
		{"at newline", "abcafé\nx", 7, 1, 7},    // offset on the '\n'
		{"line with accent", "x\ncafé end", 7, 2, 5},
		{"past end clamped", "abc", 100, 1, 4},
		{"negative clamped", "abc", -5, 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			line, col := LineCol(tc.src, tc.off)
			if line != tc.wantLine || col != tc.wantCol {
				t.Fatalf("LineCol(%q, %d) = %d:%d, want %d:%d", tc.src, tc.off, line, col, tc.wantLine, tc.wantCol)
			}
		})
	}
}

func TestLineMatchesNewlineCount(t *testing.T) {
	t.Parallel()
	// Cross-check the line number against a direct newline count for many offsets.
	src := "alpha\nbêta\ngamma\ndélta"
	for off := 0; off <= len(src); off++ {
		line, _ := LineCol(src, off)
		want := strings.Count(src[:off], "\n") + 1
		if line != want {
			t.Fatalf("LineCol(_, %d) line = %d, want %d", off, line, want)
		}
	}
}

func ExampleLineCol() {
	line, col := LineCol("ab\ncafé!", 8)
	fmt.Printf("%d:%d\n", line, col)
	// Output: 2:5
}
```

Byte offset 8 points at the `!` on line 2 (`café!`): `café` is 4 runes even though
it is 5 bytes, so the `!` is rune column 5, not the byte-based 6.

## Review

`LineCol` is correct when the line counts newline bytes and the column counts
runes within the current line — the cross-check test drives every offset and
compares the line against a direct `strings.Count`. The mistake it prevents is
reporting a *byte* column, which drifts right by one for every multi-byte rune
earlier on the line and makes an editor caret land on the wrong character. The
clamping keeps a bogus upstream offset from panicking the message formatter. Run
`go test -race`.

## Resources

- [`utf8.RuneCountInString`](https://pkg.go.dev/unicode/utf8#RuneCountInString)
- [`strings.LastIndexByte`](https://pkg.go.dev/strings#LastIndexByte)
- [`strings.Count`](https://pkg.go.dev/strings#Count)
- [`json.SyntaxError.Offset`](https://pkg.go.dev/encoding/json#SyntaxError)

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-pii-rune-mask.md](10-pii-rune-mask.md)
