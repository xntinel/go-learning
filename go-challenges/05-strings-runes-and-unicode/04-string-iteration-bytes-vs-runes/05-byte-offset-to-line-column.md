# Exercise 5: Map a Parser Byte Offset to Line:Column for User-Facing Errors

Scanners, tokenizers, and decoders report errors as a byte offset — `json.SyntaxError`
carries an `Offset int64`, and most hand-written lexers track a byte position. Users,
though, count characters, and a column computed in bytes points to the wrong place on
any line that contains non-ASCII. This module converts a byte offset into a
human-facing 1-based line and a rune-based column.

## What you'll build

```text
srcpos/                    independent module: example.com/srcpos
  go.mod                   go 1.26
  srcpos.go                OffsetToPosition
  cmd/
    demo/
      main.go              shows rune column < byte column after non-ASCII
  srcpos_test.go           ASCII, newline, non-ASCII, EOF; cross-check vs range
```

Files: `srcpos.go`, `cmd/demo/main.go`, `srcpos_test.go`.
Implement: `OffsetToPosition(src string, byteOffset int) (line, col int)`.
Test: `(1,1)` at offset 0; correct line/col across a newline; rune column smaller than byte column after `café`; EOF offset; cross-check column vs range iteration from line start.
Verify: `go test -count=1 -race ./...`

### Two counts, in the right units

A position has two coordinates and they use different units. The line number is
naturally a count of newlines before the offset, plus one, and newlines are ASCII, so
counting them in bytes is exact — `strings.Count(prefix, "\n") + 1`. The column,
however, is what a user reads off their editor's status bar: the number of *characters*
since the start of the line, not bytes. Computing it in bytes is the bug — after any
multi-byte rune on the line, a byte column overshoots the character the user sees.

So the column is a rune count over just the current line's prefix. Find where the line
starts with `strings.LastIndexByte(prefix, '\n')`, which returns the byte index of the
last newline or `-1` when there is none; adding one turns both into the byte index of
the first character on the line (`-1 + 1 == 0` handles the first line). Then
`utf8.RuneCountInString` over the slice from the line start to the offset gives the
character column, and `+1` makes it 1-based. Slicing `prefix` — the bytes strictly
before the offset — means the offset itself is reported as the column of the character
*at* that offset, which is what a caret under an error wants.

The offset is clamped to `[0, len(src)]` so an EOF offset (`len(src)`, common when a
parser hits an unexpected end) and any out-of-range value are handled without a panic.

Create `srcpos.go`:

```go
// srcpos.go
package srcpos

import (
	"strings"
	"unicode/utf8"
)

// OffsetToPosition converts a byte offset into src (such as a json.SyntaxError
// Offset or a lexer position) into a 1-based line and a 1-based, rune-counted
// column. The column is measured in code points, not bytes, so it points at the
// character a user sees even after multi-byte runes earlier on the line.
func OffsetToPosition(src string, byteOffset int) (line, col int) {
	if byteOffset < 0 {
		byteOffset = 0
	}
	if byteOffset > len(src) {
		byteOffset = len(src)
	}
	prefix := src[:byteOffset]
	line = strings.Count(prefix, "\n") + 1
	lineStart := strings.LastIndexByte(prefix, '\n') + 1 // -1 -> 0 on the first line
	col = utf8.RuneCountInString(prefix[lineStart:]) + 1
	return line, col
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/srcpos"
)

func main() {
	// The '=' sits at byte offset 5, but at rune column 5 (c a f é =): after the
	// two-byte é the rune column trails the byte column.
	const src = "café=1\nx=2"
	line, col := srcpos.OffsetToPosition(src, 5)
	fmt.Printf("offset 5 -> line %d, rune col %d (byte col would be %d)\n", line, col, 5+1)

	line, col = srcpos.OffsetToPosition(src, 8) // 'x' on line 2 (é is 2 bytes)
	fmt.Printf("offset 8 -> line %d, col %d\n", line, col)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
offset 5 -> line 1, rune col 5 (byte col would be 6)
offset 8 -> line 2, col 1
```

### Tests

The tests walk the coordinate through ASCII, a newline, and a line with a multi-byte
rune, and the decisive one asserts the rune column is strictly smaller than the byte
column after `café` — the exact discrepancy a byte-based column would get wrong. A
cross-check recomputes the column by counting range iterations from the line start,
proving the two ways of counting agree.

Create `srcpos_test.go`:

```go
// srcpos_test.go
package srcpos

import (
	"fmt"
	"strings"
	"testing"
)

func TestOffsetToPosition(t *testing.T) {
	t.Parallel()
	const src = "ab\ncd"
	cases := []struct {
		offset   int
		wantLine int
		wantCol  int
		wantWhat string
	}{
		{0, 1, 1, "start"},
		{1, 1, 2, "second char, line 1"},
		{2, 1, 3, "the newline itself, end of line 1"},
		{3, 2, 1, "first char of line 2"},
		{5, 2, 3, "EOF (len src)"},
	}
	for _, tc := range cases {
		line, col := OffsetToPosition(src, tc.offset)
		if line != tc.wantLine || col != tc.wantCol {
			t.Errorf("offset %d (%s): got (%d,%d), want (%d,%d)",
				tc.offset, tc.wantWhat, line, col, tc.wantLine, tc.wantCol)
		}
	}
}

func TestColumnIsRuneBasedNotByteBased(t *testing.T) {
	t.Parallel()
	const src = "café=1" // '=' at byte offset 5
	const eqByteOffset = 5
	line, col := OffsetToPosition(src, eqByteOffset)
	if line != 1 {
		t.Fatalf("line = %d, want 1", line)
	}
	// Rune column is 5 (c a f é =); a byte-based column would be offset+1 = 6.
	if col != 5 {
		t.Errorf("rune col = %d, want 5", col)
	}
	if byteCol := eqByteOffset + 1; col >= byteCol {
		t.Errorf("rune col %d should be < byte col %d after a multi-byte rune", col, byteCol)
	}
}

func TestColumnMatchesRangeCount(t *testing.T) {
	t.Parallel()
	inputs := []struct {
		src    string
		offset int
	}{
		{"café=1\nx=2", 5},
		{"αβγ\nδε", 9}, // second line, after some 2-byte Greek letters
		{"plain ascii here", 6},
	}
	for _, in := range inputs {
		_, col := OffsetToPosition(in.src, in.offset)
		// Recompute the column independently: runes between the line start and
		// the offset, counted by ranging.
		lineStart := strings.LastIndexByte(in.src[:in.offset], '\n') + 1
		want := 1
		for range in.src[lineStart:in.offset] {
			want++
		}
		if col != want {
			t.Errorf("OffsetToPosition(%q,%d) col = %d, range count = %d", in.src, in.offset, col, want)
		}
	}
}

func ExampleOffsetToPosition() {
	line, col := OffsetToPosition("café=1", 5)
	fmt.Println(line, col)
	// Output: 1 5
}
```

## Review

The mapping is correct when the line is a newline count (exact in bytes, since `\n`
is ASCII) and the column is a rune count over the current line's prefix, `+1` for
1-based. `TestColumnIsRuneBasedNotByteBased` is the proof that matters: after `café`
the rune column (5) is strictly less than the byte column (6), so a caret placed with
a byte column would point one position too far right — and further right the more
non-ASCII precedes it. Clamping the offset to `[0, len(src)]` makes the common EOF
offset and any stray value safe. The mistake this prevents is the natural one of
reporting `offset - lineStartByte + 1` as the column, which is bytes, not characters.

## Resources

- [encoding/json: SyntaxError.Offset](https://pkg.go.dev/encoding/json#SyntaxError) — a real byte offset you convert for users.
- [strings: Count, LastIndexByte](https://pkg.go.dev/strings) — newline counting and line-start location.
- [unicode/utf8: RuneCountInString](https://pkg.go.dev/unicode/utf8) — the rune-based column count.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-utf8-scrubber-replace-invalid.md](04-utf8-scrubber-replace-invalid.md) | Next: [06-streaming-rune-decoder.md](06-streaming-rune-decoder.md)
