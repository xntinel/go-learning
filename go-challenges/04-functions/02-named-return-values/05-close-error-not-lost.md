# Exercise 5: Capture a Deferred Close/Flush Error Without Clobbering

`defer f.Close()` is fine for a file you only read. For a *writer* it is a
data-loss bug: a buffered or compressed writer reports a full disk or a short write
only when it flushes at close time, and a bare deferred `Close` throws that error
on the floor. This exercise builds an atomic config writer that promotes the close
error into the named return — but only when no earlier error has already occurred.

This module is self-contained: its own `go mod init`, its own demo, its own tests.

## What you'll build

```text
cfgwrite/                   independent module: example.com/cfgwrite
  go.mod
  cfgwrite.go               WriteFlushCloser; WriteThrough (promote close); WriteConfig (gzip+file)
  cmd/demo/
    main.go                 runnable demo: write a gzipped config, read it back
  cfgwrite_test.go          fake writer: clobber-guard, close-error surfaced, close called once
```

- Files: `cfgwrite.go`, `cmd/demo/main.go`, `cfgwrite_test.go`.
- Implement: `WriteThrough(w WriteFlushCloser, data) (err error)` that writes, flushes, and closes, promoting the close error into the named return only when no earlier error occurred; `WriteConfig(path, data) (err error)` wiring a real gzip-over-file.
- Test: a fake writer whose Write/Flush/Close return sentinels; happy path surfaces a close error; a write failure is preserved and not overwritten by the close error; Close is invoked exactly once.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/05-close-error-not-lost/cmd/demo
cd go-solutions/04-functions/02-named-return-values/05-close-error-not-lost
```

### Promote the close error, but never clobber

The core is `WriteThrough`, which owns a writer that must be flushed and closed:

```go
func WriteThrough(w WriteFlushCloser, data []byte) (err error) {
	defer func() {
		if cerr := w.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close: %w", cerr)
		}
	}()
	if _, err = w.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err = w.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return nil
}
```

Two properties fall out of the `if cerr := w.Close(); cerr != nil && err == nil`
guard. First, `Close` is *always* called, on every exit path, because it is in the
defer — no leak even when the write fails. Second, the close error is promoted into
the named `err` only when `err` is still nil, so a genuine earlier failure (a full
disk on `Write`, a short `Flush`) is never masked by a subsequent close error. On
the happy path, where `Write` and `Flush` both succeed and `err` is nil, a
non-nil `Close` becomes the returned error — which is exactly where a buffered
writer surfaces a final flush failure.

Notice the assignments use `=`, not `:=`: `w.Write` and `w.Flush` assign to the
named `err` directly. Using `:=` would declare fresh locals and leave the named
`err` untouched — the shadowing failure mode this lesson keeps returning to. The
close error, by contrast, is captured in a distinct local `cerr` precisely so it
cannot overwrite `err` unless the guard says it may.

`WriteConfig` is the realistic wrapper: it creates a file, wraps it in a
`gzip.Writer` (which satisfies `WriteFlushCloser`), and runs it through
`WriteThrough`. The file's own `Close` is promoted with the same guard, so a
failed file close is not lost either.

Create `cfgwrite.go`:

```go
package cfgwrite

import (
	"compress/gzip"
	"fmt"
	"os"
)

// WriteFlushCloser is a writer that buffers or compresses and therefore must be
// both flushed and closed. gzip.Writer and bufio.Writer-over-a-Closer satisfy it.
type WriteFlushCloser interface {
	Write(p []byte) (int, error)
	Flush() error
	Close() error
}

// WriteThrough writes data to w, then flushes and closes it. Close always runs,
// and its error is promoted into the named err only when no earlier error already
// occurred, so a genuine write/flush failure is never masked by a close error.
func WriteThrough(w WriteFlushCloser, data []byte) (err error) {
	defer func() {
		if cerr := w.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close: %w", cerr)
		}
	}()
	if _, err = w.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err = w.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return nil
}

// WriteConfig writes data to path through a gzip writer. Both the gzip writer and
// the underlying file have their close error promoted, so a full-disk failure that
// only surfaces at close time reaches the caller.
func WriteConfig(path string, data []byte) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", path, cerr)
		}
	}()
	return WriteThrough(gzip.NewWriter(f), data)
}
```

### The runnable demo

The demo writes a gzipped config to a temp file, then reads it back and
decompresses it, proving the round trip.

Create `cmd/demo/main.go`:

```go
package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"example.com/cfgwrite"
)

func main() {
	path := filepath.Join(os.TempDir(), "cfgwrite-demo.json.gz")
	defer os.Remove(path)

	data := []byte(`{"log_level":"info","workers":8}`)
	if err := cfgwrite.WriteConfig(path, data); err != nil {
		fmt.Println("write error:", err)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		fmt.Println("open error:", err)
		return
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		fmt.Println("gzip error:", err)
		return
	}
	defer gz.Close()

	got, _ := io.ReadAll(gz)
	fmt.Printf("wrote %d bytes, read back: %s\n", len(data), got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wrote 32 bytes, read back: {"log_level":"info","workers":8}
```

### Tests

The fake writer records how many times `Close` was called and can be told to fail
`Write`, `Flush`, or `Close`. The tests assert the two properties that make the
promotion correct — a close error surfaces on the happy path, and it never
overwrites an earlier write error — plus that `Close` runs exactly once regardless.

Create `cfgwrite_test.go`:

```go
package cfgwrite

import (
	"errors"
	"fmt"
	"testing"
)

type fakeWriter struct {
	writeErr error
	flushErr error
	closeErr error
	closes   int
}

func (w *fakeWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return len(p), nil
}

func (w *fakeWriter) Flush() error { return w.flushErr }

func (w *fakeWriter) Close() error {
	w.closes++
	return w.closeErr
}

func TestWriteThroughHappyPath(t *testing.T) {
	t.Parallel()

	w := &fakeWriter{}
	if err := WriteThrough(w, []byte("data")); err != nil {
		t.Fatalf("WriteThrough: unexpected error: %v", err)
	}
	if w.closes != 1 {
		t.Fatalf("Close called %d times, want 1", w.closes)
	}
}

func TestWriteThroughSurfacesCloseError(t *testing.T) {
	t.Parallel()

	closeErr := errors.New("disk full at flush")
	w := &fakeWriter{closeErr: closeErr}
	err := WriteThrough(w, []byte("data"))
	if !errors.Is(err, closeErr) {
		t.Fatalf("WriteThrough err = %v, want the close error surfaced", err)
	}
	if w.closes != 1 {
		t.Fatalf("Close called %d times, want 1", w.closes)
	}
}

func TestWriteThroughDoesNotClobberWriteError(t *testing.T) {
	t.Parallel()

	writeErr := errors.New("short write")
	closeErr := errors.New("close also failed")
	w := &fakeWriter{writeErr: writeErr, closeErr: closeErr}
	err := WriteThrough(w, []byte("data"))
	if !errors.Is(err, writeErr) {
		t.Fatalf("WriteThrough err = %v, want the original write error preserved", err)
	}
	if errors.Is(err, closeErr) {
		t.Fatal("close error clobbered the earlier write error")
	}
	if w.closes != 1 {
		t.Fatalf("Close called %d times, want 1 even after a write failure", w.closes)
	}
}

func TestWriteConfigRoundTrip(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/cfg.json.gz"
	data := []byte(`{"k":"v"}`)
	if err := WriteConfig(path, data); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
}

func ExampleWriteThrough() {
	w := &fakeWriter{closeErr: errors.New("disk full")}
	err := WriteThrough(w, []byte("payload"))
	fmt.Println(err)
	// Output: close: disk full
}
```

## Review

The writer is correct when `Close` runs exactly once on every path and its error is
promoted only when it is the *first* failure. The two mistakes this exercise
inoculates against are the common ones. A bare `defer w.Close()` silently drops the
close error, so a full-disk failure that a buffered writer only reports at flush
time vanishes — the `TestWriteThroughSurfacesCloseError` case would then fail.
Unconditionally assigning `err = w.Close()` in the defer does the opposite: it
overwrites a genuine earlier error with the cleanup error, which
`TestWriteThroughDoesNotClobberWriteError` catches. If you need both errors, replace
the guard with `errors.Join(err, cerr)` and assert each with `errors.Is`. Run
`go test -race`.

## Resources

- [`compress/gzip.Writer`](https://pkg.go.dev/compress/gzip#Writer)
- [`errors.Join`](https://pkg.go.dev/errors#Join)
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover)
- [`os.File.Close`](https://pkg.go.dev/os#File.Close)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-panic-recovery-to-error.md](04-panic-recovery-to-error.md) | Next: [06-latency-and-outcome-metrics.md](06-latency-and-outcome-metrics.md)
