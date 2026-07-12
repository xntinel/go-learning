# Exercise 8: Parse multi-line log records, skipping malformed ones

Many log formats put one record across several lines: a header line plus indented
continuation lines (a stack trace, a SQL statement, a request body). A parser
assembles each record from its header and the continuation lines that follow. When
a header is malformed, the parser must skip it *and its continuation lines* and
resume at the next real record boundary — otherwise the orphaned continuation
lines get mis-attributed to the next record. That resume-at-the-next-boundary jump
is a labeled `continue`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
logscan/                   independent module: example.com/logscan
  go.mod                   go 1.24
  logscan.go               Record; Parse assembles multi-line records, skips bad ones
  cmd/
    demo/
      main.go              runnable demo: two records with a malformed one between
  logscan_test.go          valid records only; continuation of bad record discarded; edge cases
```

- Files: `logscan.go`, `cmd/demo/main.go`, `logscan_test.go`.
- Implement: `Parse(io.Reader) ([]Record, error)` using a `bufio.Scanner`; a header line begins a record and following indented lines are its body; a malformed (non-indented, non-header) line triggers `continue scan`, discarding it and its continuation lines up to the next header.
- Test: with valid records interleaved with a malformed header, only the valid records are emitted, the bad record's continuation lines are discarded (not merged into the next record), and `Scanner.Err()` is checked; include empty-input and trailing-partial-record cases.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The scanner, the one-line pushback, and the labeled continue

A `bufio.Scanner` reads forward one line at a time; it has no built-in
"unread." But assembling a multi-line record needs one line of lookahead: you read
continuation lines until you hit the *next* header, and at that point you have
already consumed the next record's header. The idiom is a single-line pushback: a
small `advance()` helper that returns a buffered line if one is pending, otherwise
calls `Scanner.Scan`. Setting `pending = true` "unreads" the current line so the
outer loop sees it next.

The control flow has three levels. The outer loop `scan:` starts a record. Within
it, an inner loop gathers continuation lines until the next header, then pushes
that header back. The malformed case is where the label pays off: when the outer
loop reads a line that is neither a valid header nor a continuation-of-a-record
(it is a non-indented line that does not parse as a header), it records an error
and enters a discard loop that eats every following continuation line. When that
discard loop reaches the next column-zero boundary, it pushes it back and does
`continue scan` — restarting the outer loop so that boundary is re-examined as a
fresh record start. A bare `continue` there would continue the *inner discard
loop*, not restart the outer record loop, so the pushed-back line would be re-read
as another line to discard: wrong, and an easy infinite-loop trap. The label is what makes "resume at the next record" a single
correct statement.

Records are distinguished from continuation lines structurally: a header line
starts at column zero and matches `[LEVEL] message`; a continuation line starts
with whitespace. A malformed header is a column-zero line that does not match the
header shape.

Create `logscan.go`:

```go
package logscan

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Record is one assembled log entry: a level, a header message, and the trimmed
// continuation lines that followed its header.
type Record struct {
	Level string
	Msg   string
	Body  []string
}

// ErrBadHeader is wrapped for every malformed record boundary that is skipped.
var ErrBadHeader = errors.New("malformed log header")

// Parse assembles multi-line records from r. A header line ("[LEVEL] message" at
// column zero) starts a record; following indented lines are its body. A
// column-zero line that is not a valid header is skipped along with its
// continuation lines (via a labeled continue), and recorded as an error. The
// returned error joins any scanner error with the malformed-header errors.
func Parse(r io.Reader) ([]Record, error) {
	sc := bufio.NewScanner(r)
	var (
		records []Record
		errs    []error
		line    string
		pending bool
		lineNo  int
	)

	// advance returns the next line: a pushed-back one if pending, else the next
	// scanned line. It reports false at end of input.
	advance := func() bool {
		if pending {
			pending = false
			return true
		}
		if sc.Scan() {
			line = sc.Text()
			lineNo++
			return true
		}
		return false
	}

scan:
	for advance() {
		if isContinuation(line) {
			// A continuation line with no active record is an orphan: malformed.
			errs = append(errs, fmt.Errorf("line %d: %w", lineNo, ErrBadHeader))
			continue
		}
		if !isHeader(line) {
			// A column-zero line that is not a valid header is malformed. Discard
			// it and its continuation lines, then resume at the next boundary.
			errs = append(errs, fmt.Errorf("line %d: %w", lineNo, ErrBadHeader))
			for advance() {
				if !isContinuation(line) {
					pending = true
					continue scan // restart the outer loop at the next record
				}
			}
			break scan // input ended before another boundary
		}

		level, msg := splitHeader(line)
		rec := Record{Level: level, Msg: msg}
		for advance() {
			if !isContinuation(line) {
				pending = true // pushed back: it starts the next record
				break
			}
			rec.Body = append(rec.Body, strings.TrimSpace(line))
		}
		records = append(records, rec)
	}

	if err := sc.Err(); err != nil {
		errs = append(errs, err)
	}
	return records, errors.Join(errs...)
}

// isContinuation reports whether line is an indented body line (starts with a
// space or tab), as opposed to a column-zero record boundary.
func isContinuation(line string) bool {
	return strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
}

// isHeader reports whether line is a column-zero header of the form "[LEVEL] ...".
func isHeader(line string) bool {
	return strings.HasPrefix(line, "[") && strings.Contains(line, "]")
}

// splitHeader extracts the level and trimmed message from a header line.
func splitHeader(line string) (level, msg string) {
	inner := strings.TrimPrefix(line, "[")
	parts := strings.SplitN(inner, "]", 2)
	level = parts[0]
	if len(parts) == 2 {
		msg = strings.TrimSpace(parts[1])
	}
	return level, msg
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/logscan"
)

func main() {
	input := strings.Join([]string{
		"[INFO] request started",
		"\tmethod=GET",
		"\tpath=/health",
		"garbage line with no header",
		"\tthis belongs to the bad record",
		"[ERROR] db timeout",
		"\tretry=3",
	}, "\n")

	records, err := logscan.Parse(strings.NewReader(input))
	for _, rec := range records {
		fmt.Printf("%s: %s body=%v\n", rec.Level, rec.Msg, rec.Body)
	}
	if err != nil {
		fmt.Println("parse warnings:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
INFO: request started body=[method=GET path=/health]
ERROR: db timeout body=[retry=3]
parse warnings: line 4: malformed log header
```

The `garbage line with no header` (line 4) and its continuation line are
discarded; the continuation is *not* attached to the following `[ERROR]` record.
The malformed header is surfaced through the returned error, which the demo prints
after the records as a parse warning.

### Tests

`TestSkipsMalformedRecord` feeds valid records interleaved with a malformed header
and asserts only the valid records are emitted, with the bad record's continuation
lines discarded rather than merged into the next record; it also checks the error
wraps `ErrBadHeader`. `TestEmptyInput` and `TestTrailingPartialRecord` cover the
edges.

Create `logscan_test.go`:

```go
package logscan

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestSkipsMalformedRecord(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		"[INFO] request started",
		"\tmethod=GET",
		"\tpath=/health",
		"garbage line with no header",
		"\tthis belongs to the bad record",
		"\tdiscard me too",
		"[ERROR] db timeout",
		"\tretry=3",
	}, "\n")

	records, err := Parse(strings.NewReader(input))

	if len(records) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(records), records)
	}
	if records[0].Level != "INFO" || !slices.Equal(records[0].Body, []string{"method=GET", "path=/health"}) {
		t.Fatalf("record 0 = %+v", records[0])
	}
	// The ERROR record must NOT have absorbed the bad record's continuation lines.
	if records[1].Level != "ERROR" || !slices.Equal(records[1].Body, []string{"retry=3"}) {
		t.Fatalf("record 1 = %+v (bad continuation lines leaked in?)", records[1])
	}
	if !errors.Is(err, ErrBadHeader) {
		t.Fatalf("err = %v, want it to wrap ErrBadHeader", err)
	}
}

func TestEmptyInput(t *testing.T) {
	t.Parallel()

	records, err := Parse(strings.NewReader(""))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(records) != 0 {
		t.Fatalf("got %d records, want 0", len(records))
	}
}

func TestTrailingPartialRecord(t *testing.T) {
	t.Parallel()

	// No trailing newline; last record is a header plus one continuation line.
	input := "[WARN] disk low\n\tfree=2%"
	records, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Level != "WARN" || !slices.Equal(records[0].Body, []string{"free=2%"}) {
		t.Fatalf("record = %+v", records[0])
	}
}

func TestLeadingGarbage(t *testing.T) {
	t.Parallel()

	// Opens with an orphan continuation line (indented, no active record), then a
	// malformed column-zero line, before any valid record.
	input := strings.Join([]string{
		"\torphan continuation with no header",
		"no header here",
		"\tmore orphan body",
		"[INFO] finally a record",
	}, "\n")
	records, err := Parse(strings.NewReader(input))
	if len(records) != 1 || records[0].Level != "INFO" {
		t.Fatalf("records = %+v, want one INFO record", records)
	}
	if len(records[0].Body) != 0 {
		t.Fatalf("record body = %v, want empty", records[0].Body)
	}
	if !errors.Is(err, ErrBadHeader) {
		t.Fatalf("err = %v, want ErrBadHeader", err)
	}
}
```

## Review

The parser is correct when every valid record is emitted with exactly its own
continuation lines, a malformed header and its continuation lines are discarded
rather than merged into the next record, and the scanner error is checked. The
label is load-bearing in the discard loop: `continue scan` restarts the outer
record loop at the pushed-back header, whereas a bare `continue` would continue
the discard loop and re-eat that header — a subtle infinite-loop or
merge-the-wrong-lines bug. The one-line pushback (`pending`) is the standard way
to give a forward-only `Scanner` a single line of lookahead. Always check
`Scanner.Err()` after the loop; a read error otherwise looks like a clean EOF.

## Resources

- [bufio.Scanner](https://pkg.go.dev/bufio#Scanner) — `Scan`, `Text`, and the `Err` you must check.
- [bufio.ScanLines](https://pkg.go.dev/bufio#ScanLines) — the default line-splitting behavior.
- [strings.SplitN](https://pkg.go.dev/strings#SplitN) — bounded splitting of the header line.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-config-validation-labeled-continue.md](07-config-validation-labeled-continue.md) | Next: [09-two-way-merge-join-labeled-break.md](09-two-way-merge-join-labeled-break.md)
