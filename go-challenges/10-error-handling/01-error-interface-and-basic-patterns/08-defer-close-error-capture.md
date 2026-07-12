# Exercise 8: Capturing Deferred Close and Flush Errors on a Write Path

`defer f.Close()` on a file you wrote to can silently lose data. A buffered writer
holds the last bytes in memory; `Flush` and `Close` are where they reach the disk,
and either can fail on a full volume or a broken pipe. If you discard that error
you return success while the report is truncated. This exercise builds a
`WriteReport` that captures both the flush error and the deferred close error into
a named return so a late failure is never dropped.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
writereport/                 independent module: example.com/writereport
  go.mod                     go 1.26
  report.go                  Row; WriteReport(path,rows); writeRows(io.WriteCloser,rows)
  cmd/
    demo/
      main.go                runnable demo: write a small report to a temp file
  report_test.go             success + close-fail + flush-fail + both-via-Join
```

- Files: `report.go`, `cmd/demo/main.go`, `report_test.go`.
- Implement: `WriteReport(path string, rows []Row) (err error)` writing through a `bufio.Writer` to an `os.File`, with the flush error and the deferred close error captured into the named return, joined with `errors.Join`.
- Test: the success path writes a readable file and returns nil; a failing writer/closer seam surfaces the close/flush failure; a mid-write failure and a close failure are both reported via `errors.Join`.
- Verify: `go test -count=1 -race ./...`

### Why the close error must be captured, and how the named return does it

A `bufio.Writer` exists to batch small writes into larger ones, which means the
bytes you "wrote" may still be sitting in its buffer. `Flush` pushes the buffer to
the underlying file; on a writable file `Close` may flush too. Both do real I/O
and both can fail — and the failure is exactly the last chunk of your data. So the
error from `Flush` and the error from `Close` are not noise to swallow; on a write
path they are the difference between a complete report and a truncated one that was
reported as a success.

The mechanism is a named return value plus a deferred closure. Declaring
`func writeRows(...) (err error)` gives the deferred function a variable to write
into *after* the function body has computed its result. The deferred closure calls
`Close`, and if that returns an error it folds it into `err` with `errors.Join`.
`errors.Join` is what lets both failures survive: if the body already failed at
`Flush` and `Close` then also fails, the returned error carries *both* (and
`errors.Is` matches either), rather than the close error masking the flush error or
vice versa. If the body succeeded and only `Close` fails, `errors.Join(nil, cerr)`
yields just the close error. The pattern generalizes to any `io.WriteCloser` —
database transactions (`Commit`/`Rollback`), gzip writers, network connections.

The seam that makes this testable is the `io.WriteCloser` parameter on `writeRows`.
`WriteReport` supplies a real `*os.File`; a test supplies a fake whose `Write` or
`Close` fails on command. That is how you exercise the failure branches without a
full disk. Contrast this with a *read-only* handle, where `defer f.Close()` with
the error ignored is legitimately safe — closing a file you only read cannot lose
data. The rule is not "always check Close"; it is "check Close on anything you
wrote to", and make the `_ = f.Close()` on a read handle a deliberate, commented
choice.

Create `report.go`:

```go
package writereport

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
)

// Row is one line of the report.
type Row struct {
	Name  string
	Value int
}

// WriteReport writes rows to a new file at path. The real *os.File is passed to
// writeRows, which owns the flush/close error capture.
func WriteReport(path string, rows []Row) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create report %q: %w", path, err)
	}
	return writeRows(f, rows)
}

// writeRows buffers each row into wc and captures both the flush error and the
// deferred close error into the named return, joining them so neither masks the
// other. The io.WriteCloser seam lets tests inject failing writers and closers.
func writeRows(wc io.WriteCloser, rows []Row) (err error) {
	bw := bufio.NewWriter(wc)
	defer func() {
		if cerr := wc.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()

	for _, r := range rows {
		if _, werr := fmt.Fprintf(bw, "%s,%d\n", r.Name, r.Value); werr != nil {
			return fmt.Errorf("write row: %w", werr)
		}
	}
	if ferr := bw.Flush(); ferr != nil {
		return fmt.Errorf("flush report: %w", ferr)
	}
	return nil
}
```

### The runnable demo

The demo writes a small report to a temp file and prints its contents, so a
successful write path is visible end to end.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"example.com/writereport"
)

func main() {
	path := filepath.Join(os.TempDir(), "report.csv")
	rows := []writereport.Row{{Name: "alice", Value: 3}, {Name: "bob", Value: 7}}

	if err := writereport.WriteReport(path, rows); err != nil {
		fmt.Println("write failed:", err)
		return
	}

	data, _ := os.ReadFile(path)
	fmt.Print(string(data))
	_ = os.Remove(path)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alice,3
bob,7
```

### Tests

The success test writes through a real `*os.File` in a temp dir and reads the file
back. The failure tests inject a fake `io.WriteCloser` whose `Write` and/or `Close`
returns a sentinel: one where only `Close` fails (assert the close error surfaces),
one where the flush fails (assert the write error surfaces), and one where both a
mid-write write and the close fail (assert `errors.Join` reports both).

Create `report_test.go`:

```go
package writereport

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeFile is an in-memory io.WriteCloser whose Write and Close can be made to
// fail, so the flush/close error paths are exercised without a full disk.
type fakeFile struct {
	buf      bytes.Buffer
	writeErr error
	closeErr error
}

func (f *fakeFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.buf.Write(p)
}

func (f *fakeFile) Close() error { return f.closeErr }

func TestWriteReportSuccess(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "report.csv")
	rows := []Row{{Name: "alice", Value: 3}, {Name: "bob", Value: 7}}

	if err := WriteReport(path, rows); err != nil {
		t.Fatalf("WriteReport = %v, want nil", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "alice,3\nbob,7\n"; got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestCloseErrorSurfaces(t *testing.T) {
	t.Parallel()

	errClose := errors.New("close failed")
	f := &fakeFile{closeErr: errClose}

	err := writeRows(f, []Row{{Name: "x", Value: 1}})
	if !errors.Is(err, errClose) {
		t.Fatalf("err = %v, want it to surface the close error", err)
	}
}

func TestFlushErrorSurfaces(t *testing.T) {
	t.Parallel()

	errWrite := errors.New("device full")
	f := &fakeFile{writeErr: errWrite}

	err := writeRows(f, []Row{{Name: "x", Value: 1}})
	if !errors.Is(err, errWrite) {
		t.Fatalf("err = %v, want it to surface the write/flush error", err)
	}
}

func TestFlushAndCloseBothReported(t *testing.T) {
	t.Parallel()

	errWrite := errors.New("device full")
	errClose := errors.New("close failed")
	f := &fakeFile{writeErr: errWrite, closeErr: errClose}

	err := writeRows(f, []Row{{Name: "x", Value: 1}})
	if !errors.Is(err, errWrite) {
		t.Fatalf("err = %v, want it to include the write error", err)
	}
	if !errors.Is(err, errClose) {
		t.Fatalf("err = %v, want it to include the close error", err)
	}
}
```

## Review

The write path is correct when a successful run produces the exact file bytes and
a nil error, and when every failure of `Flush` or `Close` reaches the caller.
`TestFlushAndCloseBothReported` is the one that proves the `errors.Join` design: a
buffer-flush failure and a close failure both appear in the returned error, so
neither hides the other and a caller can `errors.Is` for either. The `io.WriteCloser`
seam is what makes those disk-failure branches testable in memory.

The mistake this exercise exists to prevent is `defer f.Close()` with the error
discarded on a file you wrote to — the last buffered bytes vanish silently and the
function returns success. Capture the close error into a named return. The
converse mistake is capturing `Close` on a read-only handle where it cannot lose
data and only adds noise; there, a commented `_ = f.Close()` is the right call.

## Resources

- [pkg.go.dev: errors.Join](https://pkg.go.dev/errors#Join) — combining multiple errors so neither masks the other.
- [pkg.go.dev: bufio.Writer.Flush](https://pkg.go.dev/bufio#Writer.Flush) — where buffered bytes actually reach the underlying writer.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — how deferred functions can modify named return values.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-domain-error-string-conventions.md](07-domain-error-string-conventions.md) | Next: [09-error-as-api-contract.md](09-error-as-api-contract.md)
