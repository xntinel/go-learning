# Exercise 2: History with Persistence

Pressing Up to recall the last expression is the feature that turns a toy REPL into one you actually want to use. This exercise builds the history store behind that key: a bounded list of past entries plus a navigation cursor, with consecutive-duplicate suppression and load/save to a file so recall survives a restart. Like the line buffer, it is pure data-structure work with no terminal dependency, which makes its trickiest part — the navigation cursor that must reset on every add — straightforward to test in isolation.

The store is deliberately separate from the editor. The editor knows how to hold and mutate one line; the history knows how to remember many and walk through them. The REPL later glues them together: an Up key asks history for the previous entry and hands it to the editor's `SetContent`. Keeping the two apart means each can be tested without the other.

## What you'll build

```text
history.go         History: bounded entries + navigation cursor, Add/Prev/Next/Search, Load/Save
cmd/
  demo/
    main.go        add entries, navigate, persist to a temp file and reload
history_test.go    dedup, max-size eviction, navigation, search, round-trip persistence
```

- Files: `history.go`, `cmd/demo/main.go`, `history_test.go`.
- Implement: `History`, `NewHistory`, `Add`, `Prev`, `Next`, `SearchBack`, `Len`, `Entry`, `Load`, `Save`.
- Test: `history_test.go` checks dedup, eviction at the size bound, Up/Down navigation and its clamps, substring search, and a Save-then-Load round trip through `t.TempDir()`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p history/cmd/demo && cd history
go mod init example.com/history
```

### The navigation cursor

`History` holds the entries slice, a `maxSize` bound, and a single integer `pos` that is the navigation cursor. The invariant that makes navigation feel right is that `pos == len(entries)` means "nothing selected, sitting just past the newest entry." `Prev` (Up) decrements `pos` and returns that entry; `Next` (Down) increments it, and when it walks back past the last entry it returns `("", false)` so the REPL can blank the line. Both clamp at their ends.

`Add` is where the cursor discipline lives. It trims the line, drops it if empty, suppresses it if it is identical to the most recent entry (so three Enters on the same expression store one copy), evicts the oldest entry when the list is at its bound, appends, and — critically — resets `pos` to `len(entries)` as its last act. That reset is what guarantees the next Up starts from the line just submitted rather than from some stale mid-list position left over from earlier navigation.

`Load` and `Save` give the store a life beyond one process: `Save` writes every entry one per line, `Load` reads them back, treating a missing file as an empty (not failed) history and trimming an over-long file down to the size bound. `SearchBack` powers an incremental reverse search (Ctrl+R) by scanning backward from the current position for the first entry containing a substring.

Create `history.go`:

```go
package history

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

const defaultHistoryMax = 10_000

// History is a bounded list of past input entries with a navigation cursor.
type History struct {
	entries []string
	maxSize int
	pos     int // pos == len(entries) means "no history item is selected"
}

// NewHistory returns an empty History. maxSize <= 0 uses defaultHistoryMax.
func NewHistory(maxSize int) *History {
	if maxSize <= 0 {
		maxSize = defaultHistoryMax
	}
	return &History{maxSize: maxSize, pos: 0}
}

// Add appends line to the history, suppressing consecutive duplicates.
// It resets the navigation cursor to the "after last entry" position.
func (h *History) Add(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if len(h.entries) > 0 && h.entries[len(h.entries)-1] == line {
		h.resetPos()
		return
	}
	if len(h.entries) >= h.maxSize {
		h.entries = h.entries[1:]
	}
	h.entries = append(h.entries, line)
	h.resetPos()
}

func (h *History) resetPos() { h.pos = len(h.entries) }

// Prev moves to the previous (older) entry. Returns ("", false) at the start.
func (h *History) Prev() (string, bool) {
	if len(h.entries) == 0 || h.pos == 0 {
		return "", false
	}
	h.pos--
	return h.entries[h.pos], true
}

// Next moves to the next (newer) entry. Returns ("", false) past the last entry.
func (h *History) Next() (string, bool) {
	if h.pos >= len(h.entries) {
		return "", false
	}
	h.pos++
	if h.pos == len(h.entries) {
		return "", false
	}
	return h.entries[h.pos], true
}

// SearchBack finds the most recent entry that contains substr, starting from
// the entry before the current navigation position. Used by Ctrl+R.
func (h *History) SearchBack(substr string) (string, bool) {
	start := h.pos - 1
	if start < 0 {
		start = len(h.entries) - 1
	}
	for i := start; i >= 0; i-- {
		if strings.Contains(h.entries[i], substr) {
			h.pos = i
			return h.entries[i], true
		}
	}
	return "", false
}

// Len returns the number of stored entries.
func (h *History) Len() int { return len(h.entries) }

// Entry returns the entry at index i (0 = oldest).
func (h *History) Entry(i int) string { return h.entries[i] }

// Load reads entries from path (one per line). A missing file is not an error.
func (h *History) Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("history load %q: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			h.entries = append(h.entries, line)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("history scan %q: %w", path, err)
	}
	if len(h.entries) > h.maxSize {
		h.entries = h.entries[len(h.entries)-h.maxSize:]
	}
	h.resetPos()
	return nil
}

// Save writes all entries to path (one per line), creating or truncating the
// file.
func (h *History) Save(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("history save %q: %w", path, err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	for _, entry := range h.entries {
		fmt.Fprintln(w, entry)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("history flush %q: %w", path, err)
	}
	return nil
}
```

The two-stage clamp in `Next` is worth a second look: it first refuses to move past `len(entries)`, then, after incrementing, returns the empty signal when it lands exactly on `len(entries)`. That second check is what lets Down walk off the end of the list and clear the editor, rather than getting stuck on the newest entry. `Load` resets the cursor after reading so a freshly loaded history behaves exactly like one built up with `Add`.

### The runnable demo

The demo adds three lines — including a consecutive duplicate that must be suppressed — walks the cursor up and down, then saves to a temporary file and reloads into a fresh `History` to prove persistence round-trips. Everything is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"example.com/history"
)

func main() {
	h := history.NewHistory(10)
	h.Add("let x = 1")
	h.Add("let y = 2")
	h.Add("let y = 2") // consecutive duplicate: suppressed
	fmt.Println("entries:", h.Len())

	p, _ := h.Prev()
	fmt.Println("Up ->  ", p)
	p, _ = h.Prev()
	fmt.Println("Up ->  ", p)
	n, _ := h.Next()
	fmt.Println("Down ->", n)

	dir, err := os.MkdirTemp("", "monkey-hist")
	if err != nil {
		fmt.Println("tempdir:", err)
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "history")

	if err := h.Save(path); err != nil {
		fmt.Println("save:", err)
		return
	}
	reloaded := history.NewHistory(10)
	if err := reloaded.Load(path); err != nil {
		fmt.Println("load:", err)
		return
	}
	fmt.Println("reloaded entries:", reloaded.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
entries: 2
Up ->   let y = 2
Up ->   let x = 1
Down -> let y = 2
reloaded entries: 2
```

### Tests

The tests pin every behavior the REPL depends on: that an add stores, that a consecutive duplicate does not, that the oldest entry is evicted when the bound is hit, that Up/Down navigation returns the right entries and clamps at both ends, that reverse search finds a substring, and that Save followed by Load reproduces the entries exactly. The persistence tests use `t.TempDir()` so they clean up after themselves.

Create `history_test.go`:

```go
package history

import (
	"path/filepath"
	"testing"
)

func TestAdd(t *testing.T) {
	t.Parallel()
	h := NewHistory(10)
	h.Add("let x = 1")
	h.Add("let y = 2")
	if h.Len() != 2 {
		t.Fatalf("Len = %d, want 2", h.Len())
	}
}

func TestDeduplicatesConsecutive(t *testing.T) {
	t.Parallel()
	h := NewHistory(10)
	h.Add("x + 1")
	h.Add("x + 1")
	if h.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (consecutive duplicate suppressed)", h.Len())
	}
}

func TestEmptyNotStored(t *testing.T) {
	t.Parallel()
	h := NewHistory(10)
	h.Add("   ")
	if h.Len() != 0 {
		t.Fatalf("Len = %d, want 0 (blank input ignored)", h.Len())
	}
}

func TestMaxSize(t *testing.T) {
	t.Parallel()
	h := NewHistory(3)
	h.Add("a")
	h.Add("b")
	h.Add("c")
	h.Add("d")
	if h.Len() != 3 {
		t.Fatalf("Len = %d, want 3", h.Len())
	}
	if h.Entry(0) != "b" {
		t.Fatalf("oldest entry = %q, want %q", h.Entry(0), "b")
	}
}

func TestNavigation(t *testing.T) {
	t.Parallel()
	h := NewHistory(10)
	h.Add("first")
	h.Add("second")
	h.Add("third")

	s, ok := h.Prev()
	if !ok || s != "third" {
		t.Fatalf("Prev() = %q %v, want %q true", s, ok, "third")
	}
	s, ok = h.Prev()
	if !ok || s != "second" {
		t.Fatalf("Prev() = %q %v, want %q true", s, ok, "second")
	}
	s, ok = h.Next()
	if !ok || s != "third" {
		t.Fatalf("Next() = %q %v, want %q true", s, ok, "third")
	}
	if _, ok = h.Next(); ok {
		t.Fatal("Next() past the last entry should return false")
	}
}

func TestPrevAtStart(t *testing.T) {
	t.Parallel()
	h := NewHistory(10)
	h.Add("only")
	h.Prev()
	if _, ok := h.Prev(); ok {
		t.Fatal("Prev() before the first entry should return false")
	}
}

func TestSearchBack(t *testing.T) {
	t.Parallel()
	h := NewHistory(10)
	h.Add("let x = 1")
	h.Add("puts x")
	h.Add("let y = 2")

	s, ok := h.SearchBack("let")
	if !ok || s != "let y = 2" {
		t.Fatalf("SearchBack(%q) = %q %v, want %q true", "let", s, ok, "let y = 2")
	}
}

func TestPersistence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "history")

	h1 := NewHistory(10)
	h1.Add("puts 1")
	h1.Add("puts 2")
	if err := h1.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	h2 := NewHistory(10)
	if err := h2.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if h2.Len() != 2 {
		t.Fatalf("after Load Len = %d, want 2", h2.Len())
	}
	if h2.Entry(0) != "puts 1" || h2.Entry(1) != "puts 2" {
		t.Fatalf("entries = [%q, %q]", h2.Entry(0), h2.Entry(1))
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()
	h := NewHistory(10)
	if err := h.Load(filepath.Join(t.TempDir(), "no-such-file")); err != nil {
		t.Fatalf("Load of missing file should not error: %v", err)
	}
}
```

## Review

History is correct when the navigation cursor is honest at both ends and resets on every add. After three adds, two Ups return the second-newest and oldest entries, a Down returns to the newest, and a further Down signals the empty line — proof the two-stage clamp in `Next` works. A consecutive duplicate must not grow the list, a blank line must be ignored, and exceeding the size bound must evict the oldest entry so `Entry(0)` advances. The persistence round trip must reproduce the entries in order, and loading a missing file must be a no-op rather than an error, because a first run has no file yet.

Common mistakes for this module. Forgetting to reset `pos` in `Add` leaves the cursor mid-list, so the first Up after submitting a line skips the entry just added. Returning a real error from `Load` when the file is absent breaks every first run. And evicting from the wrong end — dropping the newest instead of the oldest when the bound is hit — throws away the entries the user most wants to recall.

## Resources

- [pkg.go.dev/bufio#Scanner](https://pkg.go.dev/bufio#Scanner) — line-oriented reading used by `Load`.
- [pkg.go.dev/os#IsNotExist](https://pkg.go.dev/os#IsNotExist) — distinguishing a missing history file from a real I/O error.
- [GNU Readline: history](https://tiswww.case.edu/php/chet/readline/rltop.html) — the canonical line-editing library whose history and reverse-search behavior this models.

---

Back to [01-line-editing-buffer.md](01-line-editing-buffer.md) | Next: [03-tab-completion.md](03-tab-completion.md)
