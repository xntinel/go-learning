# 17. Structured Logging with Rotation

Build an `io.Writer` that rotates log files when they exceed a byte limit, then use it with `log/slog`. The lesson focuses on synchronization, deterministic tests, and the boundary between structured logging and file management.

## Concepts

### slog Writes Structured Records

`log/slog` formats records through a handler. The handler writes bytes to an `io.Writer`, so rotation can be implemented below the logger without changing application logging calls.

### Rotation Needs Synchronization

Multiple goroutines may log at the same time. A rotating writer must protect file size checks, writes, closes, and renames with a mutex.

### Size Checks Are A Policy

This lesson rotates before a write when `current size + len(p)` would exceed the maximum. Other policies are possible; tests should pin the chosen one.

## Exercises

### Exercise 1: Implement A Rotating Writer

Create `writer.go`:

```go
package rotlog

import (
	"fmt"
	"os"
	"sync"
)

type Writer struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	file    *os.File
	size    int64
}

func New(path string, maxSize int64) (*Writer, error) {
	if path == "" {
		return nil, fmt.Errorf("new rotlog: %w", ErrEmptyPath)
	}
	if maxSize < 1 {
		return nil, fmt.Errorf("new rotlog: %w", ErrInvalidMaxSize)
	}
	w := &Writer{path: path, maxSize: maxSize}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, fmt.Errorf("write log: %w", ErrClosed)
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("write log: %w", err)
	}
	return n, nil
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return fmt.Errorf("close log: %w", ErrClosed)
	}
	err := w.file.Close()
	w.file = nil
	if err != nil {
		return fmt.Errorf("close log: %w", err)
	}
	return nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat log: %w", err)
	}
	w.file = f
	w.size = info.Size()
	return nil
}

func (w *Writer) rotate() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close before rotate: %w", err)
	}
	_ = os.Remove(w.path + ".1")
	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rename log: %w", err)
	}
	return w.open()
}
```

Create `errors.go`:

```go
package rotlog

import "errors"

var (
	ErrEmptyPath      = errors.New("path must not be empty")
	ErrInvalidMaxSize = errors.New("max size must be positive")
	ErrClosed         = errors.New("writer is closed")
)
```

### Exercise 2: Test Rotation

Create `writer_test.go`:

```go
package rotlog

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestWriterRotatesWhenLimitWouldBeExceeded(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "app.log")
	w, err := New(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
}

func TestNewValidationErrors(t *testing.T) {
	t.Parallel()

	if _, err := New("", 1); !errors.Is(err, ErrEmptyPath) {
		t.Fatalf("empty path err = %v", err)
	}
	if _, err := New(filepath.Join(t.TempDir(), "x.log"), 0); !errors.Is(err, ErrInvalidMaxSize) {
		t.Fatalf("max size err = %v", err)
	}
}

func TestWriteAfterClose(t *testing.T) {
	t.Parallel()

	w, err := New(filepath.Join(t.TempDir(), "app.log"), 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = w.Write([]byte("x"))
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

func ExampleWriter() {
	dir, _ := os.MkdirTemp("", "rotlog-example-")
	defer os.RemoveAll(dir)
	w, _ := New(filepath.Join(dir, "app.log"), 1024)
	defer w.Close()
	logger := slog.New(slog.NewTextHandler(w, nil))
	logger.Info("started", "service", "demo")
	fmt.Println("logged")
	// Output: logged
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"log"
	"log/slog"
	"os"
	"path/filepath"

	"example.com/rotlog"
)

func main() {
	dir, err := os.MkdirTemp("", "rotlog-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	w, err := rotlog.New(filepath.Join(dir, "app.log"), 128)
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()
	slog.New(slog.NewTextHandler(w, nil)).Info("demo", "path", dir)
}
```

## Common Mistakes

### Rotating Without A Mutex

Wrong: check size and rename files without synchronization.

Fix: guard write and rotation state with one mutex.

### Forgetting Existing File Size

Wrong: reopen an existing log and assume its size is zero.

Fix: call `Stat` after opening and initialize `size`.

### Ignoring Close Errors

Wrong: close the old log during rotation and ignore the error.

Fix: return a wrapped close error before renaming.

## Verification

Run this from `~/go-exercises/rotlog`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one more test that writes enough records through `slog` to force rotation.

## Summary

- `slog` can write structured logs to any `io.Writer`.
- Rotation must synchronize size checks, writes, closes, and renames.
- Rotation policy should be explicit and tested.
- Existing log size matters when opening an append-only writer.

## What's Next

Next: [Building a File Watcher](../18-building-a-file-watcher/18-building-a-file-watcher.md).

## Resources

- [log/slog package](https://pkg.go.dev/log/slog)
- [io.Writer](https://pkg.go.dev/io#Writer)
- [os.OpenFile](https://pkg.go.dev/os#OpenFile)
