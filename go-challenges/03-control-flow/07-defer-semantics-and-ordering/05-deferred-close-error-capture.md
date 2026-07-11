# Exercise 5: Buffered Writer — Capturing the Deferred Close/Flush Error

The most expensive one-line bug in Go: `defer f.Close()` on a buffered writer
while never checking `Flush`. The bytes still in the `bufio.Writer` buffer never
reach disk, and because the error is discarded, the caller sees a nil return and
believes the write succeeded. This exercise builds the correct version — a
named-return deferred closure that flushes, syncs, and closes, assigning the first
non-nil error into the return — and a regression test proving the ignore-flush
variant silently loses data.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
atomicwrite/                 independent module: example.com/atomicwrite
  go.mod                     module example.com/atomicwrite
  atomicwrite.go             WriteAll (flush+sync+close, named-return capture), buggy variant
  cmd/
    demo/
      main.go                runnable demo: write a payload, read it back
  atomicwrite_test.go        forced flush/close error surfaces; roundtrip; data-loss regression
```

- Files: `atomicwrite.go`, `cmd/demo/main.go`, `atomicwrite_test.go`.
- Implement: `WriteAll(dst io.WriteCloser, data []byte) (err error)` that wraps `dst` in a `bufio.Writer`, writes, and in a deferred closure flushes, syncs (if `dst` supports it), and closes, assigning the first non-nil error into the named `err`; plus a `WriteAllBuggy` that closes without flushing.
- Test (fake `io.WriteCloser`): a forced `Flush` error and a forced `Close` error each surface as the returned error; a real-file roundtrip reads the payload back byte-for-byte; the buggy variant loses the buffered data.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/atomicwrite/cmd/demo
cd ~/go-exercises/atomicwrite
go mod init example.com/atomicwrite
```

### Why the buffer makes this a write path, not a read path

`bufio.Writer` exists to batch small writes into fewer, larger syscalls. The
consequence is that `bw.Write(data)` usually just copies into an in-memory buffer
and returns nil — the bytes have *not* hit the underlying file yet. They are
flushed to the file only when the buffer fills or when you call `bw.Flush()`
explicitly. So the error that reports a full disk, a broken pipe, or a failed
network write does not come back from `Write`; it comes back from `Flush` (or from
`Close` on the underlying file). If your cleanup is `defer f.Close()` and you never
`Flush`, three things go wrong at once: the buffered tail of the data is dropped,
the `Flush` error never happens because you never flushed, and the `Close` error
(if you even checked it) does not describe the lost data.

The correct cleanup is a deferred closure over a named `err` that runs the full
teardown in the right order — `Flush`, then `Sync` for durability, then `Close` —
and captures the *first* failure into `err`. Order matters: flush the buffer into
the file, sync the file's data to stable storage, then close the descriptor. And
`err` capture uses "first non-nil wins" so an earlier real failure is not
overwritten by a later, less informative one, and a successful primary write is
not masked if teardown is clean.

`Sync` is only defined on `*os.File`, not on the `io.WriteCloser` interface, so the
closure probes for it with a type assertion to an `interface{ Sync() error }` —
the real file implements it, a network socket or the test fake does not, and the
step is skipped when absent.

Create `atomicwrite.go`:

```go
package atomicwrite

import (
	"bufio"
	"fmt"
	"io"
	"os"
)

// syncer is the optional durability step: *os.File implements it, an in-memory
// or network writer does not.
type syncer interface {
	Sync() error
}

// WriteAll buffers data through a bufio.Writer into dst, then flushes, syncs,
// and closes dst. The deferred closure captures the FIRST non-nil error into the
// named return err, so a failure during teardown surfaces instead of being lost.
func WriteAll(dst io.WriteCloser, data []byte) (err error) {
	bw := bufio.NewWriter(dst)

	defer func() {
		// Flush pushes the buffered tail into dst; this is where a full-disk or
		// broken-pipe error actually appears.
		if fErr := bw.Flush(); fErr != nil && err == nil {
			err = fmt.Errorf("flush: %w", fErr)
		}
		// Sync forces the data to stable storage, if dst supports it.
		if s, ok := dst.(syncer); ok {
			if sErr := s.Sync(); sErr != nil && err == nil {
				err = fmt.Errorf("sync: %w", sErr)
			}
		}
		// Close releases the descriptor; its error still matters on a write path.
		if cErr := dst.Close(); cErr != nil && err == nil {
			err = fmt.Errorf("close: %w", cErr)
		}
	}()

	if _, err = bw.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// WriteAllBuggy is the anti-pattern kept for the regression test: it closes dst
// without flushing, so any bytes still buffered are silently dropped and no error
// is reported.
func WriteAllBuggy(dst io.WriteCloser, data []byte) error {
	bw := bufio.NewWriter(dst)
	defer dst.Close() // BUG: never flushes; buffered bytes are lost
	_, err := bw.Write(data)
	return err
}

// CreateAndWrite is the production convenience over WriteAll: create the file and
// write the payload atomically-enough that a teardown failure is reported.
func CreateAndWrite(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	return WriteAll(f, data)
}
```

### The runnable demo

The demo writes a payload to a temp file through `CreateAndWrite`, reads it back,
and prints the result, showing that the flush actually reached disk.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"example.com/atomicwrite"
)

func main() {
	dir, err := os.MkdirTemp("", "atomicwrite-demo")
	if err != nil {
		fmt.Println("mkdir:", err)
		return
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "report.txt")
	payload := []byte("orders=1042\nrevenue=88123.50\n")

	if err := atomicwrite.CreateAndWrite(path, payload); err != nil {
		fmt.Println("write:", err)
		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("read:", err)
		return
	}
	fmt.Printf("wrote %d bytes, read back %d bytes\n", len(payload), len(got))
	fmt.Print(string(got))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wrote 29 bytes, read back 29 bytes
orders=1042
revenue=88123.50
```

### Tests

The tests inject a fake `io.WriteCloser` whose `Write` (and therefore the
`bufio.Flush`) or whose `Close` returns a forced error, and assert `WriteAll`
surfaces it. `TestRoundTrip` writes to a real temp file and reads it back
byte-for-byte, exercising the `Sync` path. `TestBuggyVariantLosesData` proves the
ignore-flush version drops the buffered payload.

Create `atomicwrite_test.go`:

```go
package atomicwrite

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

var (
	errDisk  = errors.New("no space left on device")
	errClose = errors.New("close failed")
)

// failWriter fails its Write (which the buffered Flush calls) and/or its Close.
type failWriter struct {
	buf      bytes.Buffer
	writeErr error
	closeErr error
}

func (w *failWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.buf.Write(p)
}
func (w *failWriter) Close() error { return w.closeErr }

func TestFlushErrorSurfaces(t *testing.T) {
	t.Parallel()

	w := &failWriter{writeErr: errDisk}
	err := WriteAll(w, []byte("payload"))
	if !errors.Is(err, errDisk) {
		t.Fatalf("WriteAll() = %v, want to wrap errDisk", err)
	}
}

func TestCloseErrorSurfaces(t *testing.T) {
	t.Parallel()

	w := &failWriter{closeErr: errClose}
	err := WriteAll(w, []byte("payload"))
	if !errors.Is(err, errClose) {
		t.Fatalf("WriteAll() = %v, want to wrap errClose", err)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "out.bin")
	payload := []byte("the quick brown fox\n")

	if err := CreateAndWrite(path, payload); err != nil {
		t.Fatalf("CreateAndWrite() = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() = %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("read back %q, want %q", got, payload)
	}
}

func TestBuggyVariantLosesData(t *testing.T) {
	t.Parallel()

	w := &failWriter{}
	if err := WriteAllBuggy(w, []byte("this payload is buffered and never flushed")); err != nil {
		t.Fatalf("WriteAllBuggy() = %v", err)
	}
	// The buggy variant closed without flushing: nothing reached the writer.
	if w.buf.Len() != 0 {
		t.Fatalf("buggy variant wrote %d bytes; expected 0 (data lost in buffer)", w.buf.Len())
	}
}
```

## Review

The writer is correct when a teardown failure becomes a returned error instead of
silent data loss. `TestFlushErrorSurfaces` is the load-bearing case: a full disk
manifests as a `Flush` error, and the named-return closure is what carries it to
the caller. `TestBuggyVariantLosesData` makes the alternative concrete — 42 bytes
written, zero bytes delivered, nil error — which is precisely the outcome a
`defer f.Close()`-only pattern produces. Two rules to keep: on a write path, never
discard a `Flush`/`Close` error (capture it into a named return), and order the
teardown flush → sync → close, capturing the *first* non-nil failure so the most
informative error wins. The `Sync` type-assertion is the idiom for adding
durability only when the destination is a real file.

## Resources

- [`bufio.Writer`](https://pkg.go.dev/bufio#Writer) — `Write` buffers, `Flush` is where the error appears.
- [`os.File.Sync`](https://pkg.go.dev/os#File.Sync) — forcing buffered file data to stable storage.
- [`os.Create` / `os.File.Close`](https://pkg.go.dev/os#Create) — the underlying descriptor.
- [Go blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — capturing an error in a deferred closure over a named return.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-named-return-rollback.md](04-named-return-rollback.md) | Next: [06-request-latency-timer.md](06-request-latency-timer.md)
