# 1. Reading and Writing Files In A Notes Store

Build a small notes-store package that demonstrates the four ways to move bytes between a program and the filesystem: the all-at-once helpers in `os`, an `*os.File` opened with explicit flags, line-by-line reading with `bufio.Scanner`, and metadata access with `os.Stat`. The lesson focuses on the rule that the helpers cover the 90% case, the rule that `*os.File` gives the flags and the control the helpers hide, and the rule that `bufio.Scanner` plus `os.Stat` are the standard answer for "read it line by line and tell me about the file".

```text
notesstore/
  go.mod
  notes.go
  notes_test.go
  cmd/demo/main.go
```

The package exposes a `Store` that owns a directory and a tiny set of operations: write a note (whole-file), append a line (`O_APPEND`), read all notes, read line-by-line, and report the metadata of the file. The tests pin the contract of every method; the demo shows the same operations from the command line.

## Concepts

### The All-At-Once Helpers Cover Most Reads And Writes

`os.ReadFile(name)` reads a file fully into a `[]byte`; `os.WriteFile(name, data, perm)` writes a `[]byte` to a file with a Unix permission mode. Both are documented as "convenience functions" (`pkg.go.dev/os`): they open, do the work, and close the file for you. They are the right tool for small configuration files, JSON blobs, and the body of a test fixture; they are the wrong tool for a multi-gigabyte log because the whole file lands in memory.

### `*os.File` Is The Lower-Level Handle

When you need flags (`O_APPEND`, `O_CREATE`, `O_EXCL`, `O_TRUNC`), concurrent access to the same handle, or streaming reads/writes, you reach for `os.Open` (read-only), `os.Create` (write/truncate), or `os.OpenFile(name, flag, perm)` (everything). The file is an `io.Reader` and `io.Writer`; the `WriteString` method avoids the `[]byte` allocation that `Write` requires; `Stat` returns an `os.FileInfo` for the file as the handle sees it.

### `bufio.Scanner` Reads Text By Tokens

`bufio.NewScanner(f)` returns a `*bufio.Scanner` that, by default, splits on lines (`ScanLines`). Each call to `Scan` advances one token; `Text` returns the token as a `string`. Always check `scanner.Err()` after the loop terminates: `Scan` returns `false` both at `io.EOF` and on an actual read error, and the only way to tell them apart is the `Err` method.

### `os.Stat` Returns Metadata Without Reading

`os.Stat(name)` returns an `os.FileInfo` for a path without opening the file. `Name`, `Size`, `ModTime`, `IsDir`, and `Mode` answer the common metadata questions; `errors.Is(err, fs.ErrNotExist)` (aliased from `os.ErrNotExist`) is the standard way to test for "the file does not exist".

### Failure Modes

- `os.ReadFile` on a missing file returns the error `*os.PathError` wrapping `fs.ErrNotExist`; string-matching the message breaks the moment the message changes. Compare with `errors.Is`.
- A `bufio.Writer` that is not `Flush`ed before the program exits will lose data because the buffered bytes never reach the underlying `*os.File`.
- `os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)` writes start at end-of-file for `O_APPEND` even when the file already exists; a re-creation of the file is implicit if it did not exist.

## Exercises

### Exercise 1: The Store Type And The All-At-Once Helpers

Create `notes.go`:

```go
package notes

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const defaultPerm os.FileMode = 0o644

// Store keeps notes under a single directory. The directory must exist
// when a Store is constructed; the Store does not create it.
type Store struct {
	dir string
}

// New returns a Store rooted at dir. The directory is created if it
// does not exist.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("notes: dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("notes: mkdir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Path returns the on-disk path for name.
func (s *Store) Path(name string) string {
	return filepath.Join(s.dir, name)
}

// WriteAll replaces the contents of name with data.
func (s *Store) WriteAll(name string, data []byte) error {
	if name == "" {
		return errors.New("notes: name is required")
	}
	if err := os.WriteFile(s.Path(name), data, defaultPerm); err != nil {
		return fmt.Errorf("notes: write %s: %w", name, err)
	}
	return nil
}

// ReadAll returns the full contents of name.
func (s *Store) ReadAll(name string) ([]byte, error) {
	data, err := os.ReadFile(s.Path(name))
	if err != nil {
		return nil, fmt.Errorf("notes: read %s: %w", name, err)
	}
	return data, nil
}
```

`os.WriteFile` and `os.ReadFile` are the right tool for small files. Wrapping the path with `filepath.Join` keeps the construction in one place.

### Exercise 2: Append, Open With Flags, And Stat

Append to `notes.go`:

```go
// AppendLine opens name with O_APPEND|O_CREATE|O_WRONLY and writes
// line plus a newline. The file is created if it does not exist;
// existing content is preserved.
func (s *Store) AppendLine(name, line string) error {
	if name == "" {
		return errors.New("notes: name is required")
	}
	f, err := os.OpenFile(s.Path(name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, defaultPerm)
	if err != nil {
		return fmt.Errorf("notes: open %s: %w", name, err)
	}
	defer f.Close()

	if _, err := fmt.Fprintln(f, line); err != nil {
		return fmt.Errorf("notes: append %s: %w", name, err)
	}
	return nil
}

// Info returns metadata for name. errors.Is(err, fs.ErrNotExist)
// is true when the file does not exist.
func (s *Store) Info(name string) (os.FileInfo, error) {
	info, err := os.Stat(s.Path(name))
	if err != nil {
		return nil, fmt.Errorf("notes: stat %s: %w", name, err)
	}
	return info, nil
}
```

`OpenFile` with `O_APPEND|O_CREATE|O_WRONLY` is the canonical idiom for an append-only log line; the file is created if missing and writes start at end-of-file.

### Exercise 3: Read A File Line By Line With `bufio.Scanner`

Append to `notes.go`:

```go
// ReadLines returns every line of name, with trailing newline
// characters stripped by bufio.ScanLines.
func (s *Store) ReadLines(name string) ([]string, error) {
	f, err := os.Open(s.Path(name))
	if err != nil {
		return nil, fmt.Errorf("notes: open %s: %w", name, err)
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("notes: scan %s: %w", name, err)
	}
	return lines, nil
}
```

`bufio.Scanner` strips the trailing newline on every token (`ScanLines` definition). `sc.Err()` is the only way to distinguish a clean EOF from a read error; `Scan` returns `false` in both cases.

### Exercise 4: Test The Store

Create `notes_test.go`:

```go
package notes

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreWriteAndReadAll(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	want := []byte("Buy groceries\nFinish homework\n")
	if err := s.WriteAll("notes.txt", want); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}

	got, err := s.ReadAll("notes.txt")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("ReadAll = %q, want %q", got, want)
	}
}

func TestStoreAppendLineCreatesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := s.AppendLine("log.txt", "first"); err != nil {
		t.Fatalf("AppendLine #1: %v", err)
	}
	if err := s.AppendLine("log.txt", "second"); err != nil {
		t.Fatalf("AppendLine #2: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "log.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "first\nsecond\n"
	if string(got) != want {
		t.Fatalf("log content = %q, want %q", got, want)
	}
}

func TestStoreAppendLinePreservesExistingContent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "log.txt"), []byte("preexisting\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLine("log.txt", "appended"); err != nil {
		t.Fatalf("AppendLine: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := "preexisting\nappended\n"
	if string(got) != want {
		t.Fatalf("log content = %q, want %q", got, want)
	}
}

func TestStoreReadLines(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := s.WriteAll("notes.txt", []byte("alpha\nbeta\ngamma\n")); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReadLines("notes.txt")
	if err != nil {
		t.Fatalf("ReadLines: %v", err)
	}
	want := []string{"alpha", "beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestStoreInfoMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = s.Info("does-not-exist.txt")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestStoreInfoReportsSize(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := []byte("hello world")
	if err := s.WriteAll("hello.txt", body); err != nil {
		t.Fatal(err)
	}

	info, err := s.Info("hello.txt")
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Size() != int64(len(body)) {
		t.Fatalf("Size = %d, want %d", info.Size(), len(body))
	}
	if info.IsDir() {
		t.Fatal("hello.txt reported as directory")
	}
}

func TestStoreNewCreatesDir(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	target := filepath.Join(parent, "nested", "data")

	s, err := New(target)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(s.dir); err != nil {
		t.Fatalf("dir not created: %v", err)
	}
}

func TestStoreNewRejectsEmptyDir(t *testing.T) {
	t.Parallel()

	if _, err := New(""); err == nil {
		t.Fatal("New(\"\") returned nil error")
	}
}

func ExampleStore_WriteAll() {
	dir, err := os.MkdirTemp("", "notes-demo-*")
	if err != nil {
		fmt.Println("tempdir:", err)
		return
	}
	defer os.RemoveAll(dir)

	s, _ := New(dir)
	_ = s.WriteAll("notes.txt", []byte("Hello, Go!\n"))
	data, _ := s.ReadAll("notes.txt")
	fmt.Print(string(data))
	// Output: Hello, Go!
}
```

The tests pin each contract: write/read round-trip, append behaviour (creation plus preservation), line-by-line reading, missing-file detection through `fs.ErrNotExist`, and metadata reporting. `t.TempDir()` gives each test a private directory that the test framework removes at the end.

### Exercise 5: The Demo Binary

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"

	notes "example.com/notesstore"
)

func main() {
	dir, err := os.MkdirTemp("", "notes-demo-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s, err := notes.New(dir)
	if err != nil {
		log.Fatal(err)
	}

	if err := s.WriteAll("notes.txt", []byte("Buy groceries\nFinish homework\n")); err != nil {
		log.Fatal(err)
	}
	fmt.Println("File created.")

	data, err := s.ReadAll("notes.txt")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Full contents:\n%s\n", data)

	for _, line := range []string{"Read a book", "Go for a walk"} {
		if err := s.AppendLine("notes.txt", line); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Println("Two lines appended.")

	lines, err := s.ReadLines("notes.txt")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Line-by-line:")
	for i, ln := range lines {
		fmt.Printf("  %d: %s\n", i+1, ln)
	}

	info, err := s.Info("notes.txt")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nFile: %s, Size: %d bytes\n", info.Name(), info.Size())
}
```

Run with:

```bash
cd ~/go-exercises/notesstore
go run ./cmd/demo
```

The demo exercises every public method through the `Store` API. It writes, reads whole, appends, reads line-by-line, and reports metadata.

## Common Mistakes

### Comparing Error Strings Instead Of Using `errors.Is`

Wrong:

```go
info, err := os.Stat(name)
if err != nil && !strings.Contains(err.Error(), "no such file") {
	return err
}
```

What happens: the message format is an implementation detail; the test breaks the first time the runtime changes the wording.

Fix: use `errors.Is(err, fs.ErrNotExist)`. The `*os.PathError` returned by `Stat` wraps `fs.ErrNotExist` and `errors.Is` walks the chain. `TestStoreInfoMissing` pins this contract.

### Forgetting `Flush` On A `bufio.Writer`

Wrong: writing through a `bufio.Writer` and then closing the file. Buffered bytes never reach disk.

What happens: the file is truncated or missing the last write; `go test` may catch it, the user sees it.

Fix: call `bw.Flush()` before `bw.Close()` (which calls `Flush` automatically). The lesson never uses `bufio.Writer` because the demo is line-based, but the rule still applies when you add buffered writing.

### Using `package main` For A Library Lesson

Wrong: putting the file operations in `package main` with a `func main()` that prints results. The test then has to compare stdout against an "Expected output" block.

What happens: the verification does not fail on its own; the lesson can pass while the code is broken.

Fix: a library lesson is a real `package notes` with a real `*_test.go`. The demo lives in `cmd/demo/main.go` as a separate `package main` that uses the exported API only.

### Re-Implementing What `os.Stat` Already Returns

Wrong: opening a file to discover its size. `f.Stat()` does it without reading.

Fix: `os.Stat` returns the metadata; opening the file is wasted work. The lesson's `Info` method is the canonical wrapper.

## Verification

From `~/go-exercises/notesstore`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The tests are the verification; the demo is illustrative.

Your turn: add `TestStoreWriteAllRejectsEmptyName` that calls `s.WriteAll("", []byte("x"))` and asserts the returned error message starts with `"notes: "`. The test pins the empty-name rejection contract.

## Summary

- `os.ReadFile` and `os.WriteFile` are the convenience helpers for small files; they open, do the work, and close.
- `os.OpenFile(name, flag, perm)` is the lower-level handle for flags like `O_APPEND|O_CREATE|O_WRONLY`.
- `bufio.Scanner` reads text token by token; check `sc.Err()` after the loop to distinguish `io.EOF` from a read error.
- `os.Stat` returns an `os.FileInfo` without reading the file; use `errors.Is(err, fs.ErrNotExist)` for the missing-file check.
- The lesson's `Store` keeps path construction in one place and gives every operation a typed error with `%w` so callers can match with `errors.Is`.

## What's Next

Next: [io.Reader and io.Writer Composition](../02-io-reader-writer-composition/02-io-reader-writer-composition.md).

## Resources

- [os package](https://pkg.go.dev/os) — `ReadFile`, `WriteFile`, `OpenFile`, `Stat`.
- [bufio package](https://pkg.go.dev/bufio) — `Scanner`, `ScanLines`, `ScanWords`.
- [io/fs.ErrNotExist](https://pkg.go.dev/io/fs#pkg-variables) — the canonical "file not found" sentinel.
