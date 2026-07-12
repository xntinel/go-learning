# Exercise 1: The Line Editing Buffer

Every interactive line editor rests on one data structure: an in-memory buffer of what the user has typed so far, plus a cursor marking where the next edit lands. This exercise builds that buffer in complete isolation from any terminal. Because it has no I/O, it is the easiest piece to get exactly right and to test exhaustively, and getting it right first means the raw-terminal loop later can treat editing as a solved problem and worry only about bytes and escape sequences.

The one design decision that colors everything is that the buffer stores `[]rune`, not `[]byte`. Monkey identifiers may contain multi-byte UTF-8 characters, and every operation here — insert, delete-before, delete-word, jump to start — is defined on characters. With a rune slice the cursor is a character index and all arithmetic is safe; a byte slice would let the cursor split a multi-byte rune and corrupt the line.

## What you'll build

```text
editor.go          LineEditor: rune buffer + cursor, all edit/move operations
cmd/
  demo/
    main.go        type and edit a line, printing the buffer after each step
editor_test.go     insert, delete, move, word-delete, reset, set-content
```

- Files: `editor.go`, `cmd/demo/main.go`, `editor_test.go`.
- Implement: `LineEditor` with `Insert`, `DeleteBack`, `DeleteForward`, `MoveLeft`, `MoveRight`, `MoveHome`, `MoveEnd`, `DeleteWordBack`, `DeleteToEnd`, `String`, `Cursor`, `PrefixAtCursor`, `Reset`, `SetContent`.
- Test: `editor_test.go` drives the buffer with method calls and asserts on `String()` and `Cursor()`; no terminal is involved.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/40-capstone-language-interpreter/07-repl-line-editing/01-line-editing-buffer/cmd/demo && cd go-solutions/40-capstone-language-interpreter/07-repl-line-editing/01-line-editing-buffer
```

### The buffer model

`LineEditor` holds exactly two fields: `buf []rune` and `cursor int`. The cursor is the index *between* runes where the next inserted character will go, so it ranges from `0` (before the first rune) to `len(buf)` (after the last). Insert places a rune at the cursor and advances it; the two backspace-family operations remove the rune just before the cursor, and the delete-family operations remove the rune just at the cursor. Keeping the cursor as a half-open position is what makes Home (`cursor = 0`) and End (`cursor = len(buf)`) one-liners and what makes "insert in the middle" identical to "insert at the end".

Each movement and deletion returns a `bool` reporting whether it actually did anything: `MoveLeft` at column zero returns false, `DeleteBack` on an empty buffer returns false. The caller uses that to decide whether a redraw is needed and, in tests, to assert the boundary behavior directly. The word and line deletions (`DeleteWordBack`, `DeleteToEnd`) and the absolute jumps (`MoveHome`, `MoveEnd`) round out the Ctrl-key shortcuts the REPL will bind later.

Create `editor.go`:

```go
package editor

import "unicode"

// LineEditor is an in-memory line buffer with a cursor.
// All positions are rune indices, not byte offsets.
type LineEditor struct {
	buf    []rune
	cursor int
}

// Insert inserts r at the cursor position and advances the cursor.
func (e *LineEditor) Insert(r rune) {
	e.buf = append(e.buf, 0)
	copy(e.buf[e.cursor+1:], e.buf[e.cursor:])
	e.buf[e.cursor] = r
	e.cursor++
}

// DeleteBack removes the rune immediately before the cursor (Backspace).
// Returns false if the cursor is already at the start.
func (e *LineEditor) DeleteBack() bool {
	if e.cursor == 0 {
		return false
	}
	e.buf = append(e.buf[:e.cursor-1], e.buf[e.cursor:]...)
	e.cursor--
	return true
}

// DeleteForward removes the rune at the cursor (Delete key).
// Returns false if the cursor is at the end of the buffer.
func (e *LineEditor) DeleteForward() bool {
	if e.cursor >= len(e.buf) {
		return false
	}
	e.buf = append(e.buf[:e.cursor], e.buf[e.cursor+1:]...)
	return true
}

// MoveLeft moves the cursor one position left. Returns false at the start.
func (e *LineEditor) MoveLeft() bool {
	if e.cursor == 0 {
		return false
	}
	e.cursor--
	return true
}

// MoveRight moves the cursor one position right. Returns false at the end.
func (e *LineEditor) MoveRight() bool {
	if e.cursor >= len(e.buf) {
		return false
	}
	e.cursor++
	return true
}

// MoveHome jumps the cursor to position 0.
func (e *LineEditor) MoveHome() { e.cursor = 0 }

// MoveEnd jumps the cursor past the last rune.
func (e *LineEditor) MoveEnd() { e.cursor = len(e.buf) }

// DeleteWordBack removes from the cursor back to the start of the previous word
// (Ctrl+W). Trailing whitespace is consumed first, then non-whitespace.
// Returns false if the cursor is already at the start.
func (e *LineEditor) DeleteWordBack() bool {
	if e.cursor == 0 {
		return false
	}
	end := e.cursor
	for e.cursor > 0 && unicode.IsSpace(e.buf[e.cursor-1]) {
		e.cursor--
	}
	for e.cursor > 0 && !unicode.IsSpace(e.buf[e.cursor-1]) {
		e.cursor--
	}
	e.buf = append(e.buf[:e.cursor], e.buf[end:]...)
	return true
}

// DeleteToEnd removes everything from the cursor to the end of the buffer
// (Ctrl+K).
func (e *LineEditor) DeleteToEnd() {
	e.buf = e.buf[:e.cursor]
}

// String returns the buffer content as a UTF-8 string.
func (e *LineEditor) String() string { return string(e.buf) }

// Cursor returns the current cursor position (rune index).
func (e *LineEditor) Cursor() int { return e.cursor }

// PrefixAtCursor returns the buffer content from the start up to the cursor.
// Used by tab completion to extract the word being typed.
func (e *LineEditor) PrefixAtCursor() string { return string(e.buf[:e.cursor]) }

// Reset clears the buffer and moves the cursor to position 0.
func (e *LineEditor) Reset() { e.buf = e.buf[:0]; e.cursor = 0 }

// SetContent replaces the buffer with s and moves the cursor to the end.
// Used when restoring a history entry into the editor.
func (e *LineEditor) SetContent(s string) {
	e.buf = []rune(s)
	e.cursor = len(e.buf)
}
```

`Insert` is the only non-obvious method: it grows the slice by one, shifts the tail one position right with `copy`, then drops the new rune into the gap. `DeleteWordBack` runs two scans — first over trailing whitespace, then over the non-whitespace run before it — so deleting after `hello world` removes exactly `world` and leaves the trailing space, matching the Ctrl+W behavior of a real shell. `SetContent` is what history recall uses to drop a recalled line into the editor with the cursor parked at the end, and `PrefixAtCursor` is what tab completion uses to read the text the cursor sits behind.

### The runnable demo

The demo types a misspelled word, fixes it by moving into the middle and inserting, appends more text, then exercises the word-delete and line-delete shortcuts, printing the buffer and cursor after each step. Every operation is deterministic, so the output is fixed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/line-editor"
)

func main() {
	var e editor.LineEditor

	for _, r := range "hllo" {
		e.Insert(r)
	}
	// Move back three positions and insert the missing 'e': "hllo" -> "hello".
	e.MoveLeft()
	e.MoveLeft()
	e.MoveLeft()
	e.Insert('e')
	fmt.Printf("after insert: %q (cursor=%d)\n", e.String(), e.Cursor())

	e.MoveEnd()
	for _, r := range " world" {
		e.Insert(r)
	}
	fmt.Printf("typed:        %q\n", e.String())

	e.DeleteWordBack()
	fmt.Printf("after Ctrl+W: %q\n", e.String())

	e.MoveHome()
	e.DeleteToEnd()
	fmt.Printf("after Ctrl+K: %q\n", e.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after insert: "hello" (cursor=2)
typed:        "hello world"
after Ctrl+W: "hello "
after Ctrl+K: ""
```

### Tests

The tests cover insertion at the end and in the middle, both deletions and their boundary cases, left/right movement and its clamps, the Home/End jumps, word-back and to-end deletion, `Reset`, `SetContent`, and `PrefixAtCursor`. Because the buffer is pure logic, each test is a few method calls and a string comparison.

Create `editor_test.go`:

```go
package editor

import "testing"

func TestInsert(t *testing.T) {
	t.Parallel()
	var e LineEditor
	for _, r := range "hello" {
		e.Insert(r)
	}
	if got := e.String(); got != "hello" {
		t.Fatalf("String() = %q, want %q", got, "hello")
	}
	if e.Cursor() != 5 {
		t.Fatalf("Cursor = %d, want 5", e.Cursor())
	}
}

func TestInsertAtMiddle(t *testing.T) {
	t.Parallel()
	var e LineEditor
	for _, r := range "hllo" {
		e.Insert(r)
	}
	e.MoveLeft()
	e.MoveLeft()
	e.MoveLeft()
	e.Insert('e')
	if got := e.String(); got != "hello" {
		t.Fatalf("String() = %q, want %q", got, "hello")
	}
}

func TestDeleteBack(t *testing.T) {
	t.Parallel()
	var e LineEditor
	for _, r := range "hello" {
		e.Insert(r)
	}
	if !e.DeleteBack() {
		t.Fatal("DeleteBack returned false")
	}
	if got := e.String(); got != "hell" {
		t.Fatalf("String() = %q, want %q", got, "hell")
	}
}

func TestDeleteBackAtStart(t *testing.T) {
	t.Parallel()
	var e LineEditor
	if e.DeleteBack() {
		t.Fatal("DeleteBack on empty buffer should return false")
	}
}

func TestDeleteForward(t *testing.T) {
	t.Parallel()
	var e LineEditor
	for _, r := range "hello" {
		e.Insert(r)
	}
	e.MoveHome()
	e.DeleteForward()
	if got := e.String(); got != "ello" {
		t.Fatalf("String() = %q, want %q", got, "ello")
	}
}

func TestMoveLeftRight(t *testing.T) {
	t.Parallel()
	var e LineEditor
	for _, r := range "hi" {
		e.Insert(r)
	}
	if !e.MoveLeft() {
		t.Fatal("MoveLeft should return true")
	}
	if e.Cursor() != 1 {
		t.Fatalf("Cursor = %d, want 1", e.Cursor())
	}
	if !e.MoveRight() {
		t.Fatal("MoveRight should return true")
	}
	if e.Cursor() != 2 {
		t.Fatalf("Cursor = %d, want 2", e.Cursor())
	}
	if e.MoveRight() {
		t.Fatal("MoveRight at end should return false")
	}
}

func TestHomeEnd(t *testing.T) {
	t.Parallel()
	var e LineEditor
	for _, r := range "hello" {
		e.Insert(r)
	}
	e.MoveHome()
	if e.Cursor() != 0 {
		t.Fatalf("Cursor after MoveHome = %d, want 0", e.Cursor())
	}
	e.MoveEnd()
	if e.Cursor() != 5 {
		t.Fatalf("Cursor after MoveEnd = %d, want 5", e.Cursor())
	}
}

func TestDeleteToEnd(t *testing.T) {
	t.Parallel()
	var e LineEditor
	for _, r := range "hello world" {
		e.Insert(r)
	}
	e.MoveHome()
	for range "hello" {
		e.MoveRight()
	}
	e.DeleteToEnd()
	if got := e.String(); got != "hello" {
		t.Fatalf("String() = %q, want %q", got, "hello")
	}
}

func TestDeleteWordBack(t *testing.T) {
	t.Parallel()
	var e LineEditor
	for _, r := range "hello world" {
		e.Insert(r)
	}
	if !e.DeleteWordBack() {
		t.Fatal("DeleteWordBack returned false")
	}
	if got := e.String(); got != "hello " {
		t.Fatalf("String() = %q, want %q", got, "hello ")
	}
}

func TestReset(t *testing.T) {
	t.Parallel()
	var e LineEditor
	for _, r := range "hello" {
		e.Insert(r)
	}
	e.Reset()
	if e.String() != "" || e.Cursor() != 0 {
		t.Fatalf("after Reset: String=%q Cursor=%d", e.String(), e.Cursor())
	}
}

func TestSetContent(t *testing.T) {
	t.Parallel()
	var e LineEditor
	e.SetContent("hello")
	if e.String() != "hello" || e.Cursor() != 5 {
		t.Fatalf("SetContent: String=%q Cursor=%d", e.String(), e.Cursor())
	}
}

func TestPrefixAtCursor(t *testing.T) {
	t.Parallel()
	var e LineEditor
	for _, r := range "hello" {
		e.Insert(r)
	}
	e.MoveHome()
	e.MoveRight()
	e.MoveRight()
	if got := e.PrefixAtCursor(); got != "he" {
		t.Fatalf("PrefixAtCursor = %q, want %q", got, "he")
	}
}

func TestUnicode(t *testing.T) {
	t.Parallel()
	var e LineEditor
	for _, r := range "café" {
		e.Insert(r)
	}
	if e.Cursor() != 4 {
		t.Fatalf("Cursor = %d, want 4 (runes, not bytes)", e.Cursor())
	}
	if !e.DeleteBack() {
		t.Fatal("DeleteBack returned false")
	}
	if got := e.String(); got != "caf" {
		t.Fatalf("String() = %q, want %q", got, "caf")
	}
}
```

## Review

The buffer is correct when its cursor arithmetic is in characters, not bytes, and when every boundary returns honestly. `Insert` followed by edits anywhere in the line must reconstruct the intended string; `DeleteBack` on an empty buffer and `MoveRight` at the end must return false rather than panic or wrap. The Unicode test is the one that matters most: inserting `café` leaves a cursor of 4, and a single `DeleteBack` removes the whole `é` — proof that the slice is `[]rune` and the cursor a rune index. `DeleteWordBack` after `hello world` leaves `hello ` with the trailing space intact, matching shell Ctrl+W, and `SetContent` parks the cursor at the end so a recalled history line is ready to extend.

Common mistakes for this module. Storing the buffer as `[]byte` while treating the cursor as a character position splits multi-byte runes and corrupts the line; keep it `[]rune` and convert to `string` only in `String`. Forgetting the bounds check in `MoveRight`/`DeleteForward` walks the cursor past `len(buf)` and panics on the next access. And shifting the tail the wrong direction in `Insert` (copying forward instead of making room first) overwrites the character you meant to keep.

## Resources

- [pkg.go.dev/unicode#IsSpace](https://pkg.go.dev/unicode#IsSpace) — the whitespace test that drives `DeleteWordBack`.
- [go.dev/ref/spec#Rune_literals](https://go.dev/ref/spec#Rune_literals) — the `rune` type and how Go ranges over UTF-8 by code point.
- [Thorsten Ball, Writing An Interpreter In Go](https://interpreterbook.com/) — Chapter 1 introduces the Monkey REPL this buffer powers.

---

Next: [02-history-persistence.md](02-history-persistence.md)
