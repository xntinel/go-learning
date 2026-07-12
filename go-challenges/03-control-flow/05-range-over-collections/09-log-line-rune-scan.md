# Exercise 9: UTF-8-safe Log Line Scan — range over a string

Tokenizing a log line into fields and reporting each field's byte position sounds
trivial until a user name contains an accented character and your redactor slices
mid-rune. This module ranges over a string the right way — `for i, r := range s`
gives the *byte* index `i` and the decoded `rune` `r`, not a byte-by-byte walk — and
uses that to split a line into fields with correct byte offsets for a downstream
highlighter or redactor.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
logscan/                    independent module: example.com/logscan
  go.mod                    go 1.24
  logscan.go                Field; Fields(line) []Field; Counts(s) (bytes, runes int)
  cmd/
    demo/
      main.go               runnable demo: tokenize a multibyte line, print offsets
  logscan_test.go           ASCII offsets, multibyte offsets+counts, byte-vs-rune contrast
```

- Files: `logscan.go`, `cmd/demo/main.go`, `logscan_test.go`.
- Implement: `Fields(line) []Field` splitting on whitespace with each field's starting byte offset via `for i, r := range line`, and `Counts(s) (bytes, runes int)`.
- Test: ASCII field boundaries at expected byte offsets; a multibyte line where byte offsets jump by rune width and rune count differs from `len(s)`; a byte-vs-rune iteration contrast.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Byte index, decoded rune

`for i, r := range s` decodes the string as UTF-8: on each iteration `i` is the
byte offset where the current rune begins, and `r` is the `rune` (a Unicode code
point, type `int32`). For an ASCII string the byte offset increments by one each
step, so it looks like a byte walk — but for a multibyte rune, `i` jumps by the
rune's byte width (2, 3, or 4). This is fundamentally different from ranging
`[]byte(s)`, which yields raw bytes one at a time and would split a multibyte rune
into its component bytes.

`len(s)` is the number of *bytes*, never runes. `utf8.RuneCountInString(s)` is the
rune count. Confusing the two is how redaction and truncation corrupt text: slicing
`s[:n]` where `n` is a "character count" you got from counting runes can land in the
middle of a multibyte rune and produce invalid UTF-8.

`Fields` uses the byte index correctly. It walks the line with `for i, r := range line`,
using `unicode.IsSpace(r)` to find field boundaries and recording the byte offset
`i` where each field starts. It slices `line[start:i]` to extract a field — and
because whitespace runes here are ASCII (their byte offset is a valid rune
boundary) and `start` is always the byte offset of a rune start, those slices never
cut a rune in half. The byte offset is exactly what a highlighter or redactor needs
to point back into the original bytes of the line.

Create `logscan.go`:

```go
package logscan

import (
	"unicode"
	"unicode/utf8"
)

// Field is a whitespace-delimited token and the byte offset where it begins in the
// original line.
type Field struct {
	Text      string
	ByteStart int
}

// Fields splits line on Unicode whitespace, recording each field's starting byte
// offset. It ranges the string so the offsets are byte positions and the slices
// never cut a multibyte rune.
func Fields(line string) []Field {
	var fields []Field
	start := -1
	for i, r := range line {
		switch {
		case unicode.IsSpace(r):
			if start >= 0 {
				fields = append(fields, Field{Text: line[start:i], ByteStart: start})
				start = -1
			}
		case start < 0:
			start = i
		}
	}
	if start >= 0 {
		fields = append(fields, Field{Text: line[start:], ByteStart: start})
	}
	return fields
}

// Counts returns the byte length and the rune count of s. They differ whenever s
// contains a multibyte rune.
func Counts(s string) (bytes, runes int) {
	return len(s), utf8.RuneCountInString(s)
}
```

### The runnable demo

The demo tokenizes a line containing a multibyte field (`señor`, where `ñ` is two
bytes) and prints each field with its byte offset, then the byte and rune counts so
you can see they differ.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/logscan"
)

func main() {
	line := "user señor id=7"
	for _, f := range logscan.Fields(line) {
		fmt.Printf("%s@%d\n", f.Text, f.ByteStart)
	}
	bytes, runes := logscan.Counts(line)
	fmt.Printf("bytes=%d runes=%d\n", bytes, runes)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
user@0
señor@5
id=7@12
bytes=16 runes=15
```

### Tests

The ASCII test asserts field boundaries at the expected byte offsets. The multibyte
test uses a line with a two-byte rune and asserts both the byte offsets (which jump
past the wide rune) and that `Counts` reports more bytes than runes. The contrast
test iterates the same string as runes and as bytes and asserts the two counts
differ, making the "range-string decodes UTF-8" behavior explicit.

Create `logscan_test.go`:

```go
package logscan

import "testing"

func TestFieldsASCII(t *testing.T) {
	t.Parallel()
	got := Fields("level=info msg=ok code=200")
	want := []Field{
		{Text: "level=info", ByteStart: 0},
		{Text: "msg=ok", ByteStart: 11},
		{Text: "code=200", ByteStart: 18},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d fields, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("field %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFieldsMultibyteOffsets(t *testing.T) {
	t.Parallel()
	line := "user señor id=7" // ñ is two bytes
	got := Fields(line)
	want := []Field{
		{Text: "user", ByteStart: 0},
		{Text: "señor", ByteStart: 5},
		{Text: "id=7", ByteStart: 12}, // offset jumped past the 2-byte ñ
	}
	if len(got) != len(want) {
		t.Fatalf("got %d fields, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("field %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	bytes, runes := Counts(line)
	if bytes != 16 || runes != 15 {
		t.Fatalf("Counts = (%d bytes, %d runes), want (16, 15)", bytes, runes)
	}
	if bytes <= runes {
		t.Fatal("multibyte line should have more bytes than runes")
	}
}

func TestRuneVsByteIteration(t *testing.T) {
	t.Parallel()
	s := "café" // é is two bytes: 5 bytes, 4 runes

	runeCount := 0
	lastIndex := -1
	for i := range s { // range over string: byte index of each rune
		runeCount++
		lastIndex = i
	}
	byteCount := 0
	for range len(s) { // counted loop over bytes
		byteCount++
	}

	if runeCount != 4 {
		t.Fatalf("rune iterations = %d, want 4", runeCount)
	}
	if byteCount != 5 {
		t.Fatalf("byte iterations = %d, want 5", byteCount)
	}
	if lastIndex != 3 { // last rune 'é' starts at byte 3, not 4
		t.Fatalf("last rune byte index = %d, want 3", lastIndex)
	}
}
```

## Review

The scan is correct when field byte offsets index back into the original line at
rune boundaries and `Counts` reports bytes and runes separately. The trap the whole
module targets is treating the range index as a character count: it is a byte
offset, so `len(s)` is bytes and only `utf8.RuneCountInString` is the character
count. Slicing at a byte offset from `for i, r := range s` is safe because those
offsets are rune boundaries; slicing at an arbitrary "character number" you computed
elsewhere is not. Run `go test`; the multibyte assertions fail loudly if someone
switches to a `[]byte` walk that splits the accented rune.

## Resources

- [Go blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings)
- [Go Specification: For statements (range over string)](https://go.dev/ref/spec#For_range)
- [unicode/utf8 (RuneCountInString)](https://pkg.go.dev/unicode/utf8#RuneCountInString)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-ndjson-stream-iterator.md](08-ndjson-stream-iterator.md) | Next: [10-batch-export-chunker.md](10-batch-export-chunker.md)
