# Exercise 7: An NDJSON/Log Line Reader Robust to Split Records

Line-delimited ingestion — NDJSON event streams, structured log tailing — is
everywhere in a backend, and every record can straddle an arbitrary `Read`
boundary. `bufio.Scanner` is the right tool because it reframes a chunked byte
stream into logical tokens independent of read size. This exercise proves that
robustness against `iotest.OneByteReader` and `iotest.HalfReader`, and pins the
two failure modes that bite in production: an over-long line and a missing
trailing newline.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
linescan/                   independent module: example.com/linescan
  go.mod                    module example.com/linescan
  scan.go                   ReadLines: bufio.Scanner with a tunable max token size
  cmd/
    demo/
      main.go               parses a multi-line NDJSON blob into records
  scan_test.go              identical records across plain/OneByte/Half; ErrTooLong; trailing-no-newline
```

Files: `scan.go`, `cmd/demo/main.go`, `scan_test.go`.
Implement: `ReadLines(r io.Reader, maxLine int) ([]string, error)` that scans lines with `bufio.Scanner`, tunes the buffer with `Scanner.Buffer`, and returns `Scanner.Err()`.
Test: identical records across `{plain, OneByteReader, HalfReader}`; an over-long line yields `bufio.ErrTooLong`; a trailing line without a newline is still emitted.
Verify: `go test -count=1 -race ./...`

### Why the Scanner is boundary-agnostic, and where it still fails

`bufio.Scanner` buffers internally and applies a split function (`ScanLines` by
default) that only emits a token once a full line is present. It accumulates across
as many `Read` calls as it takes, so a record split across ten one-byte reads
emerges whole. That is the property the three-wrapper test proves: the same
multi-line payload through plain, `OneByteReader`, and `HalfReader` must yield
byte-identical records. A hand-rolled `Read`-and-split-on-newline loop almost always
gets this wrong because it forgets to carry a partial line across reads.

Two things still fail, and both are real ingestion incidents. First, the token size
is **bounded**. The default cap is `bufio.MaxScanTokenSize` (64 KB); a single line
longer than the buffer makes `Scan` return false and `Scanner.Err()` return
`bufio.ErrTooLong`. A log line with a giant embedded stack trace or a malformed
NDJSON record with no newlines can trip this, and if you never call `Err()` you
silently lose the rest of the stream and think ingestion completed. You tune the
cap with `Scanner.Buffer(buf, max)`; this exercise sets a small `max` to exercise
the failure deterministically.

Second, `ScanLines` emits a final line that has no trailing newline. That is the
common case for the last record of a file or a stream cut mid-flush, and the test
pins it so a "drop the last line" bug cannot hide.

The non-negotiable discipline: **always call `Scanner.Err()` after the loop**. The
loop condition `for sc.Scan()` returns false both at clean EOF and on error;
`Err()` is the only thing that tells them apart. And if a token must outlive the
next `Scan`, copy it — `Scanner.Bytes()` is reused in place, so this code returns
`Scanner.Text()`, which allocates a fresh string.

Create `scan.go`:

```go
package linescan

import (
	"bufio"
	"io"
)

// ReadLines scans r into lines. If maxLine > 0 it caps the per-line buffer at
// maxLine bytes, so a longer line yields bufio.ErrTooLong via the returned error.
// The final line is emitted even without a trailing newline.
func ReadLines(r io.Reader, maxLine int) ([]string, error) {
	sc := bufio.NewScanner(r)
	if maxLine > 0 {
		sc.Buffer(make([]byte, 0, 64), maxLine)
	}
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text()) // Text copies; Bytes would be reused
	}
	return lines, sc.Err()
}
```

### The runnable demo

The demo feeds a small NDJSON blob and prints each record, showing the last line
(no trailing newline) is included.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"strings"

	"example.com/linescan"
)

func main() {
	blob := `{"lvl":"info","msg":"start"}` + "\n" +
		`{"lvl":"warn","msg":"slow"}` + "\n" +
		`{"lvl":"error","msg":"boom"}` // no trailing newline

	lines, err := linescan.ReadLines(strings.NewReader(blob), 0)
	if err != nil {
		log.Fatal(err)
	}
	for i, l := range lines {
		fmt.Printf("record %d: %s\n", i, l)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
record 0: {"lvl":"info","msg":"start"}
record 1: {"lvl":"warn","msg":"slow"}
record 2: {"lvl":"error","msg":"boom"}
```

### Tests

`TestRecordsSurviveChunking` drives the same multi-line payload through plain,
`OneByteReader`, and `HalfReader`, asserting identical records and a nil error
every time. `TestOversizeLineIsTooLong` sets a small `maxLine`, feeds a line beyond
it, and asserts `Scan` stops with `bufio.ErrTooLong`. `TestTrailingNoNewline` pins
that a final line without a newline is still emitted.

Create `scan_test.go`:

```go
package linescan

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

const payload = "line one\nline two\nline three\nline four"

func TestRecordsSurviveChunking(t *testing.T) {
	t.Parallel()
	want := []string{"line one", "line two", "line three", "line four"}

	wrappers := map[string]func(io.Reader) io.Reader{
		"plain":   func(r io.Reader) io.Reader { return r },
		"oneByte": iotest.OneByteReader,
		"half":    iotest.HalfReader,
	}
	for name, wrap := range wrappers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := ReadLines(wrap(strings.NewReader(payload)), 0)
			if err != nil {
				t.Fatalf("ReadLines: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("got %d records, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("record %d = %q, want %q", i, got[i], want[i])
				}
			}
		})
	}
}

func TestOversizeLineIsTooLong(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 100) + "\n"
	_, err := ReadLines(strings.NewReader(long), 8) // cap far below the line length
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("err = %v, want bufio.ErrTooLong", err)
	}
}

func TestTrailingNoNewline(t *testing.T) {
	t.Parallel()
	got, err := ReadLines(strings.NewReader("only line, no newline"), 0)
	if err != nil {
		t.Fatalf("ReadLines: %v", err)
	}
	if len(got) != 1 || got[0] != "only line, no newline" {
		t.Fatalf("got %v, want one line", got)
	}
}
```

## Review

The reader is correct when records are independent of read size, which
`bufio.Scanner` gives you for free and the three-wrapper test proves. The two
production traps it must not fall into are silent: an over-long line that trips
`bufio.ErrTooLong` and is lost if you skip `Scanner.Err()`, and a final line with
no newline that a naive splitter drops. Both are pinned here. Remember `Text()`
copies while `Bytes()` is reused in place — returning `Bytes()` from this function
would hand callers slices that mutate under them. Run `go test -race` to confirm.

## Resources

- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — `Scan`, `Text`, `Bytes`, `Buffer`, `Err`.
- [`bufio.ErrTooLong`](https://pkg.go.dev/bufio#pkg-variables) — the over-long-token sentinel.
- [`testing/iotest`](https://pkg.go.dev/testing/iotest) — `OneByteReader` and `HalfReader` for chunk-boundary tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-bounded-body-reader-oom-guard.md](06-bounded-body-reader-oom-guard.md) | Next: [08-composite-multireader-preamble.md](08-composite-multireader-preamble.md)
