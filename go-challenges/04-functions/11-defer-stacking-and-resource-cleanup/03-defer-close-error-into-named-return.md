# Exercise 3: Surfacing Close/Flush Errors Through a Deferred Named Return

`defer w.Close()` on a writable sink silently throws away the most important
error there is: the one from the final flush. This module builds a buffered
report writer where a write can succeed in memory but the flush to the sink fails
(disk full, a short write to a network endpoint), and shows the `defer func() { err = errors.Join(err, w.Close()) }()`
pattern that surfaces that error to the caller.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
report/                     independent module: example.com/report
  go.mod
  report/report.go          WriteReport(sink, rows) (err error): bufio.Writer, named-return capture
  cmd/demo/main.go          write two rows to a temp file, read them back
  report/report_test.go     flush-fails, success, write+close both fail cases
```

- Files: `report/report.go`, `cmd/demo/main.go`, `report/report_test.go`.
- Implement: `WriteReport(sink io.WriteCloser, rows []string) (err error)` that wraps `sink` in a `bufio.Writer`, writes each row, and in a deferred closure over the named `err` joins the `Flush` error and the `Close` error so neither is dropped.
- Test: inject a `failWriter` whose `Write`/`Close` return sentinels; assert the returned error wraps the flush sentinel; assert the success path returns nil and all bytes reach the sink; assert that when both a flush error and a close error occur, `errors.Join` surfaces both.
- Verify: `go test -count=1 -race ./...`

### Why the buffered writer makes this trap so easy to fall into

A `bufio.Writer` accumulates bytes in memory and writes them through to the
underlying sink only when the buffer fills or you call `Flush`. That is exactly
what makes the naive `defer sink.Close()` dangerous. Every `WriteString` for a
small report returns `nil` — the bytes went into the 4 KiB buffer, nothing was
sent to the disk or the socket yet, so nothing has failed *yet*. The write to the
real sink happens at `Flush`. If the disk is full or the network write is short,
`Flush` returns the error — and if you deferred a bare `sink.Close()` and never
checked `Flush`, that error vanishes and your function returns `nil`. You told the
caller the report was written; it was not.

The fix has two parts, both riding on a named return `err`:

- Call `Flush` and capture its error. The buffered bytes must reach the sink, and
  a failure to flush is a failure to write the report.
- Capture `Close`'s error too. Closing a file can fail (a delayed write error
  surfaced at close on some filesystems), and a network `Close` can report a
  final send failure.

`errors.Join(err, bw.Flush(), sink.Close())` does all of it in one line. `Join`
ignores nil arguments, so on the happy path it collapses to whatever `err`
already was (nil). Crucially, Go evaluates function arguments left to right, so
`bw.Flush()` runs before `sink.Close()` — you must flush the buffer to the sink
*before* you close the sink, and the argument order encodes that.

Create `report/report.go`:

```go
package report

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// WriteReport writes each row as a line to sink through a buffered writer. The
// deferred closure runs Flush then Close and joins their errors into the named
// return, so a final flush that fails (disk full, short network write) is never
// silently dropped.
func WriteReport(sink io.WriteCloser, rows []string) (err error) {
	bw := bufio.NewWriter(sink)

	defer func() {
		// Argument order matters: Flush drains the buffer into sink, then Close
		// closes sink. Join drops nils, so the happy path returns err unchanged.
		err = errors.Join(err, bw.Flush(), sink.Close())
	}()

	for i, row := range rows {
		if _, werr := bw.WriteString(row); werr != nil {
			return fmt.Errorf("write row %d: %w", i, werr)
		}
		if werr := bw.WriteByte('\n'); werr != nil {
			return fmt.Errorf("write newline %d: %w", i, werr)
		}
	}
	return nil
}
```

### The runnable demo

The demo writes to a real temp file, then reads it back to prove the bytes landed
and the error was nil.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"

	"example.com/report/report"
)

func main() {
	f, err := os.CreateTemp("", "report-*.csv")
	if err != nil {
		log.Fatal(err)
	}
	name := f.Name()
	defer os.Remove(name)

	if err := report.WriteReport(f, []string{"alice,100", "bob,200"}); err != nil {
		log.Fatal(err)
	}

	data, err := os.ReadFile(name)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(string(data))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alice,100
bob,200
```

### Tests

The `failWriter` is an `io.WriteCloser` whose `Write` and `Close` can be armed
with sentinel errors. Because the rows are small, `WriteString` only buffers and
never touches `Write` — the write error surfaces exactly at `Flush`, which is the
point of the exercise. The buggy `defer sink.Close()` alternative is shown in
prose only, in a plain fence, so the gate never assembles it.

The mistake this test guards against:

```
// WRONG: the Flush error is discarded; a full disk looks like success.
func WriteReportBuggy(sink io.WriteCloser, rows []string) error {
	bw := bufio.NewWriter(sink)
	defer sink.Close()
	for _, row := range rows {
		bw.WriteString(row + "\n")
	}
	return bw.Flush() // returned, but Close's error is lost, and a panic skips this
}
```

Create `report/report_test.go`:

```go
package report

import (
	"bytes"
	"errors"
	"testing"
)

// failWriter is an in-memory sink whose Write and Close can be made to fail.
type failWriter struct {
	buf      bytes.Buffer
	writeErr error
	closeErr error
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.buf.Write(p)
}

func (f *failWriter) Close() error { return f.closeErr }

var (
	errDiskFull  = errors.New("disk full")
	errCloseSink = errors.New("close failed")
)

func TestWriteReportSurfacesFlushError(t *testing.T) {
	t.Parallel()

	sink := &failWriter{writeErr: errDiskFull}
	err := WriteReport(sink, []string{"a", "b"})
	if !errors.Is(err, errDiskFull) {
		t.Fatalf("err = %v, want errors.Is %v", err, errDiskFull)
	}
}

func TestWriteReportSuccess(t *testing.T) {
	t.Parallel()

	sink := &failWriter{}
	err := WriteReport(sink, []string{"alice,100", "bob,200"})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	got := sink.buf.String()
	want := "alice,100\nbob,200\n"
	if got != want {
		t.Fatalf("sink = %q, want %q", got, want)
	}
}

func TestWriteReportJoinsFlushAndClose(t *testing.T) {
	t.Parallel()

	sink := &failWriter{writeErr: errDiskFull, closeErr: errCloseSink}
	err := WriteReport(sink, []string{"a"})
	if !errors.Is(err, errDiskFull) {
		t.Errorf("err = %v, want errors.Is %v (flush)", err, errDiskFull)
	}
	if !errors.Is(err, errCloseSink) {
		t.Errorf("err = %v, want errors.Is %v (close)", err, errCloseSink)
	}
}

func TestWriteReportCloseErrorOnly(t *testing.T) {
	t.Parallel()

	sink := &failWriter{closeErr: errCloseSink}
	err := WriteReport(sink, []string{"ok"})
	if !errors.Is(err, errCloseSink) {
		t.Fatalf("err = %v, want errors.Is %v", err, errCloseSink)
	}
	// The bytes still made it to the sink before Close failed.
	if got := sink.buf.String(); got != "ok\n" {
		t.Fatalf("sink = %q, want %q", got, "ok\n")
	}
}
```

## Review

The helper is correct when three things hold: the success path returns nil and
every byte reaches the sink; a flush failure (a real write to the sink that fails)
propagates as an error the caller can match with `errors.Is`; and when both the
flush and the close fail, `errors.Join` surfaces both so neither is lost. The
mistake to internalize is that `defer sink.Close()` is not merely incomplete —
it is actively wrong for a writable sink, because it discards the one error most
likely to matter. Note also that a bare `defer sink.Close()` combined with a
`return bw.Flush()` still loses the close error, and a panic between the writes
and the manual `Flush` skips the flush entirely; the deferred `errors.Join` form
handles the panic path too, because a `defer` runs while a panic unwinds. Run
`go test -race`.

## Resources

- [bufio.Writer: Flush](https://pkg.go.dev/bufio#Writer.Flush)
- [errors.Join](https://pkg.go.dev/errors#Join)
- [io.WriteCloser](https://pkg.go.dev/io#WriteCloser)
- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-tx-manager-defer-rollback.md](02-tx-manager-defer-rollback.md) | Next: [04-defer-loop-leak-batch-importer.md](04-defer-loop-leak-batch-importer.md)
