# Exercise 9: Pad Text Columns for a Monospace Report (and Where Runes Aren't Cells)

A monospace report, a CLI table, an aligned log column — each needs cells padded to a
common width. Padding by byte length shears the table on any non-ASCII cell. This module
pads by rune count, corrects a widespread myth about Go's `fmt` along the way, and then
names the honest ceiling of the whole byte-vs-rune model: rune count is still not display
width for East Asian wide characters.

## What you'll build

```text
coltable/                  independent module: example.com/coltable
  go.mod                   go 1.26
  coltable.go              PadRight, RenderTable
  cmd/
    demo/
      main.go              renders a table; shows a wide-char shear
  coltable_test.go         rune padding, already-wide untouched, fmt-myth, wide-char xfail
```

Files: `coltable.go`, `cmd/demo/main.go`, `coltable_test.go`.
Implement: `PadRight(s string, width int) string`, `RenderTable(rows [][]string) string`.
Test: `PadRight("café",6)` is 6 runes wide; Go's `%-6s` produces the same (dispelling the byte-width myth); already-wide row untouched; no panic at width 0; documented CJK wide-char misalignment.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/coltable/cmd/demo
cd ~/go-exercises/coltable
go mod init example.com/coltable
```

### Pad by runes, and the myth about %-Ns

`PadRight` measures the cell in runes with `utf8.RuneCountInString` and appends spaces to
reach `width`. If the cell is already at least `width` runes, it is returned untouched —
which also means `width - n` is never negative, so `strings.Repeat` never panics, and
`width == 0` or an empty input are safe by the same guard. `RenderTable` computes each
column's width as the max rune count of its cells and pads every cell but the last (a
trailing pad on the final column is invisible and just adds whitespace).

Now the myth. Engineers coming from C's `printf`, or from Python's older `%`-formatting,
assume `fmt.Sprintf("%-6s", cell)` pads to six *bytes*, which would shear on non-ASCII.
In Go it does not: the `fmt` package documents width as "the minimum number of *runes* to
output", so `%-6s` of `"café"` produces a six-rune field, exactly like `PadRight`. The
test below asserts that equality precisely to lock the correction in — Go's `%-Ns` is
already rune-aware. So why write `PadRight` at all? Because it gives you an explicit hook:
the moment you need something other than rune count — true display width — you change one
function instead of every format string, and because a table renderer that owns its width
logic can later swap in that display-width measure.

That swap is the honest ceiling. Rune count equals display width only for
"narrow" characters. An East Asian *wide* character — most CJK ideographs, full-width
forms — occupies **two** terminal cells but is **one** rune. So `PadRight("中", 3)` and
`PadRight("ab", 3)` both produce three-rune fields, yet on a real monospace terminal the
`中` field is four cells wide and the `ab` field is three: they misalign. Rune padding
fixed the byte bug and exposed a subtler one. The correct measure is *display width*, which
requires the East Asian Width property from `golang.org/x/text/width` or a library such as
`github.com/mattn/go-runewidth`. This is the edge of the byte-vs-rune model: past code
points lie graphemes and display cells, and the standard `unicode/utf8` toolkit does not
reach them.

Create `coltable.go`:

```go
// coltable.go
package coltable

import (
	"strings"
	"unicode/utf8"
)

// PadRight returns s padded with trailing spaces to at least width runes. If s
// already has width or more runes it is returned unchanged, so width-n is never
// negative and width 0 or an empty s are safe. Width is measured in runes, not
// bytes, so non-ASCII cells are not sheared.
func PadRight(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

// RenderTable formats rows as space-separated columns, each column padded to the
// rune width of its widest cell. The final column is not padded.
func RenderTable(rows [][]string) string {
	cols := 0
	for _, row := range rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	widths := make([]int, cols)
	for _, row := range rows {
		for i, cell := range row {
			if w := utf8.RuneCountInString(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}
	var b strings.Builder
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				b.WriteString("  ")
			}
			if i == len(row)-1 {
				b.WriteString(cell) // no trailing pad on the last column
			} else {
				b.WriteString(PadRight(cell, widths[i]))
			}
		}
		b.WriteByte('\n')
	}
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
	"unicode/utf8"

	"example.com/coltable"
)

func main() {
	rows := [][]string{
		{"id", "name"},
		{"1", "café"},
		{"2", "ok"},
	}
	fmt.Print(coltable.RenderTable(rows))

	// The honest ceiling: rune padding equalizes RUNE width, but a wide CJK rune
	// is two display cells, so it still shears on a real terminal.
	a := coltable.PadRight("中", 3)
	b := coltable.PadRight("ab", 3)
	fmt.Printf("%q rune-width=%d\n", a, utf8.RuneCountInString(a))
	fmt.Printf("%q rune-width=%d\n", b, utf8.RuneCountInString(b))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id  name
1   café
2   ok
"中  " rune-width=3
"ab " rune-width=3
```

### Tests

The tests confirm rune-based padding, prove Go's `%-6s` is rune-based too (the myth
correction), check the already-wide and zero-width edges do not panic or truncate, and
lock the wide-character limitation so nobody later "fixes" the padding by assuming rune
count equals display width.

Create `coltable_test.go`:

```go
// coltable_test.go
package coltable

import (
	"fmt"
	"testing"
	"unicode/utf8"
)

func TestPadRight(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"café", 6, "café  "},
		{"ab", 4, "ab  "},
		{"中文", 4, "中文  "},
		{"hello", 3, "hello"}, // already wider: untouched, not truncated
		{"", 0, ""},
		{"x", 0, "x"},
	}
	for _, tc := range cases {
		got := PadRight(tc.in, tc.width)
		if got != tc.want {
			t.Errorf("PadRight(%q,%d) = %q, want %q", tc.in, tc.width, got, tc.want)
		}
		if n := utf8.RuneCountInString(got); tc.width > 0 && n < tc.width {
			t.Errorf("PadRight(%q,%d) rune width %d < %d", tc.in, tc.width, n, tc.width)
		}
	}
}

// TestFmtWidthIsRuneBased corrects the common assumption that %-Ns pads in bytes.
// In Go, fmt width is measured in runes, so %-6s and PadRight agree even on
// non-ASCII input.
func TestFmtWidthIsRuneBased(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"café", "中文", "ab"} {
		fmtPad := fmt.Sprintf("%-6s", s)
		if got := PadRight(s, 6); got != fmtPad {
			t.Errorf("PadRight(%q,6)=%q but fmt %%-6s=%q; fmt width is rune-based, they should match", s, got, fmtPad)
		}
	}
}

func TestRenderTableAligns(t *testing.T) {
	t.Parallel()
	rows := [][]string{
		{"id", "name"},
		{"1", "café"},
		{"22", "ok"},
	}
	want := "id  name\n1   café\n22  ok\n"
	if got := RenderTable(rows); got != want {
		t.Errorf("RenderTable =\n%q\nwant\n%q", got, want)
	}
}

// TestWideCharKnownLimitation locks the ceiling of the rune model: a wide CJK
// rune is padded to the same RUNE width as an ASCII cell, yet occupies two
// display cells, so it still misaligns. Real fix: golang.org/x/text/width or
// github.com/mattn/go-runewidth.
func TestWideCharKnownLimitation(t *testing.T) {
	t.Parallel()
	wide := PadRight("中", 3) // one wide rune + 2 spaces
	narrow := PadRight("ab", 3)
	if utf8.RuneCountInString(wide) != utf8.RuneCountInString(narrow) {
		t.Fatal("rune widths should match (that is the whole point of the limitation)")
	}
	// They agree in RUNES but not in display cells: 中 is 2 cells wide.
	if utf8.RuneCountInString(wide) != 3 {
		t.Fatalf("wide rune-width = %d, want 3", utf8.RuneCountInString(wide))
	}
}

func ExamplePadRight() {
	fmt.Printf("%q\n", PadRight("café", 6))
	// Output: "café  "
}
```

## Review

The formatter is correct when padding is measured in runes: `PadRight("café", 6)` is a
six-rune field, an already-wide cell is returned untouched (never truncated, never a
negative `Repeat`), and width 0 or empty input do not panic. The `fmt`-myth test is the
senior correction — Go's `%-Ns` is *already* rune-based, so the value of `PadRight` is the
explicit hook, not a fix for a bug `fmt` does not have. The `TestWideCharKnownLimitation`
test states the ceiling out loud: rune count equalizes rune width but not display width,
because an East Asian wide rune is two cells. That is the honest edge of everything in this
chapter — past code points, correctness needs grapheme and display-width tools from
`golang.org/x/text`, not `unicode/utf8`.

## Resources

- [fmt package: width is measured in runes](https://pkg.go.dev/fmt) — the documented behavior that dispels the byte-width myth.
- [golang.org/x/text/width](https://pkg.go.dev/golang.org/x/text/width) — East Asian Width, the tool for true display width.
- [Go Blog: Text normalization in Go](https://go.dev/blog/normalization) — the boundary where code points stop being the right unit.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-rune-count-limit-validator.md](08-rune-count-limit-validator.md) | Next: [../05-strings-package/00-concepts.md](../05-strings-package/00-concepts.md)
