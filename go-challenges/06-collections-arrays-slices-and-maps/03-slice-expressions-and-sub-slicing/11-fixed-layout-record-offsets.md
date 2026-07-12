# Exercise 11: Parse a Fixed-Width Settlement Record by Named Byte Offsets

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Not every feed is delimited. A banking partner's settlement file still ships
one fixed-width line per transaction: the account ID lives in bytes 0 through
11, the transaction type in bytes 12 and 13, and so on, with no comma or tab
in sight. The parser has to know the layout up front and slice each field out
by its documented offset. Get the length guard wrong and a truncated line —
one dropped byte from an upstream transfer glitch — panics the whole batch
instead of failing that one record with a reportable error, and an operator
staring at a stack trace has no way to tell which of the day's ten thousand
lines was the culprit.

This exercise builds `settle`, a command-line tool that reads a settlement
batch from stdin, parses each line at its documented byte offsets, and writes
one CSV row per record to stdout. It stops at the first record that does not
fit the fixed layout and reports exactly which line failed, so an operator
can go straight to the bad row in the source file instead of grepping a
panic's stack trace for a line number that was never printed.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
settle/                        module example.com/settle
  go.mod                       go 1.24
  settlement.go                 package main — field offset/length consts, RecordLen,
                                ErrShortRecord, Record with CSV(), Parse(line) (Record, error)
  settlement_test.go            package main — valid line, short line, trim table, CSV
                                rendering, run() end to end, stop-at-first-bad-line
  main.go                       package main — -header flag, stdin/stdout, exit codes
```

- Files: `settlement.go`, `settlement_test.go`, `main.go`.
- Implement: named offset/length constants for each field, `RecordLen` as their
  sum, `Parse(line string) (Record, error)` guarding `len(line) < RecordLen`
  before slicing any field and trimming trailing space padding off each one,
  and `(Record).CSV() string` rendering the fields as one comma-separated
  line.
- Tool: `settle` reads fixed-width lines from stdin and writes CSV to stdout,
  one row per record; `-header` prints a column-name row first. It stops
  processing at the first line shorter than the fixed layout. Exit 0 on
  success, exit 2 for a short record or an unrecognized flag (usage errors),
  exit 1 is reserved for a runtime failure reading stdin.
- Test: a well-formed line decomposes into the expected `Record`; a short line
  returns `ErrShortRecord`; trailing space padding is trimmed without
  touching field content; `CSV()` renders the expected row; `run` end to end
  over a `strings.Reader` and a `bytes.Buffer` for multi-line input, the
  `-header` flag, a tolerated trailing blank line, a short line, and an
  unknown flag; and that a short line partway through a batch stops
  processing without emitting anything for the line after it.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/03-slice-expressions-and-sub-slicing/11-fixed-layout-record-offsets
cd go-solutions/06-collections-arrays-slices-and-maps/03-slice-expressions-and-sub-slicing/11-fixed-layout-record-offsets
go mod edit -go=1.24
```

### Why the length guard has to run before any field is sliced

A fixed-width record has no natural way to signal "field three is missing" —
every byte in the line has a fixed meaning by position, so the only failure
mode a truncated line can produce is running out of bytes partway through.
Slicing `line[offSettleDate : offSettleDate+lenSettleDate]` on a line that is
three bytes short of `RecordLen` panics with a slice-bounds-out-of-range, and
it panics on whichever field the truncation happens to land in — the error a
caller sees depends on exactly how corrupt the input is, which makes it
useless for triage. `Parse` checks `len(line) < RecordLen` once, before
touching any field, so a short line always fails the same documented way
(`ErrShortRecord`, wrapped with the actual and expected lengths) regardless of
where the truncation occurred, and every subsequent slice expression is
guaranteed to be in bounds.

The offsets themselves are named constants rather than numbers scattered
through the parser, and each one is defined as the previous field's offset
plus its length (`offTxnType = offAccountID + lenAccountID`), so the layout
reads top to bottom exactly like the partner's format spec and shifting one
field's width automatically shifts every field after it. Trimming happens
*after* slicing, not before: the fixed-width format pads unused field width
with trailing spaces, and that padding is only meaningful once a field has
been isolated at its own offset — trimming the whole line first would not
misalign anything here, but slicing first and trimming second is what keeps
each field's trim independent of every other field's padding.

Create `settlement.go`:

```go
// Package main implements settle, a command-line tool that parses a banking
// partner's fixed-width settlement file: one transaction per line, every
// field at a documented byte offset instead of delimited by a separator.
package main

import (
	"errors"
	"fmt"
	"strings"
)

// Field layout, documented once as named offset/length constants rather than
// scattered magic numbers through the parser. Each field's start is the
// previous field's start plus its length, so the layout reads top to bottom
// exactly like the partner's format spec.
const (
	offAccountID = 0
	lenAccountID = 12

	offTxnType = offAccountID + lenAccountID
	lenTxnType = 2

	offAmount = offTxnType + lenTxnType
	lenAmount = 12

	offCurrency = offAmount + lenAmount
	lenCurrency = 3

	offSettleDate = offCurrency + lenCurrency
	lenSettleDate = 8

	// RecordLen is the total width of one fixed-layout record, in bytes.
	RecordLen = offSettleDate + lenSettleDate
)

// ErrShortRecord means a line is narrower than the documented fixed layout,
// so slicing any field past its end would run off the end of the string.
var ErrShortRecord = errors.New("settlement: record shorter than fixed layout")

// Record is one parsed settlement line. Every field is trimmed of the
// trailing space padding the fixed-width format uses to fill unused width.
type Record struct {
	AccountID   string
	TxnType     string
	AmountCents string
	Currency    string
	SettleDate  string
}

// CSV renders r as one comma-separated line, in field order, with no
// trailing newline.
func (r Record) CSV() string {
	return strings.Join([]string{r.AccountID, r.TxnType, r.AmountCents, r.Currency, r.SettleDate}, ",")
}

// Parse extracts a Record from one fixed-layout line by named sub-slices at
// the documented byte offsets above. It guards line length before slicing
// any field: a short line -- truncated by an upstream transfer error, or a
// stray blank line in the batch -- would otherwise panic with a slice-out-
// of-range instead of failing the single bad record with a reportable error.
func Parse(line string) (Record, error) {
	if len(line) < RecordLen {
		return Record{}, fmt.Errorf("%w: got %d bytes, want %d", ErrShortRecord, len(line), RecordLen)
	}

	return Record{
		AccountID:   trimField(line, offAccountID, lenAccountID),
		TxnType:     trimField(line, offTxnType, lenTxnType),
		AmountCents: trimField(line, offAmount, lenAmount),
		Currency:    trimField(line, offCurrency, lenCurrency),
		SettleDate:  trimField(line, offSettleDate, lenSettleDate),
	}, nil
}

// trimField slices the [off, off+n) window and trims its trailing space
// padding. It is called only after Parse's length guard has already
// confirmed the line is wide enough for every field, so the slice
// expression here can never run past the end of line.
func trimField(line string, off, n int) string {
	return strings.TrimRight(line[off:off+n], " ")
}
```

### The tool

`settle` has one job: turn a batch of fixed-width lines into CSV, stopping
the instant it finds one it cannot parse. `run` takes the argument slice, an
`io.Reader` for stdin, and an `io.Writer` for stdout, so it never touches
`os.Stdin`, `os.Stdout`, or `os.Exit` directly and can be driven end to end
from a test with a `strings.Reader` and a `bytes.Buffer`. It streams the
input line by line with `bufio.Scanner` rather than reading the whole batch
into memory first, so a multi-gigabyte settlement file costs no more memory
than one line at a time.

Every failure `run` can produce from bad input or a bad flag — an unknown
flag, a settlement line shorter than the fixed layout — wraps the `errUsage`
sentinel, and `main` maps that to exit code 2. `run` deliberately stops at
the first bad line instead of skipping it and continuing: a truncated record
partway through a batch is evidence the upstream transfer itself is suspect,
and silently dropping the rest of the file would understate the day's
settlement total without anyone noticing. Exit code 1 is reserved for a
genuine runtime failure — an I/O error reading stdin — which cannot happen
with the piped or redirected input this tool expects, but the mapping stays
in place for completeness.

Create `main.go`:

```go
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure the caller can fix by changing the input or the
// command line: a bad flag, or a settlement line shorter than the fixed
// layout. main maps it to exit code 2; any other error maps to exit code 1.
var errUsage = errors.New("usage")

// run reads fixed-width settlement lines from stdin, parses each with
// Parse, and writes one CSV line per record to stdout. It never touches
// os.Stdin, os.Stdout, or os.Exit directly, so it can be exercised in a
// test with a strings.Reader and a bytes.Buffer.
//
// run stops at the first line that fails to parse and returns an error
// wrapping errUsage with the failing line number, rather than skipping bad
// records: a truncated line partway through a settlement batch means the
// upstream transfer itself is suspect, and silently dropping the rest of
// the file would understate the day's settlement total.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("settle", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	header := fs.Bool("header", false, "print a CSV header row before the parsed records")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	if *header {
		fmt.Fprintln(stdout, "account_id,txn_type,amount_cents,currency,settle_date")
	}

	scanner := bufio.NewScanner(stdin)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if line == "" {
			continue // tolerate a trailing blank line
		}
		rec, err := Parse(line)
		if err != nil {
			return fmt.Errorf("%w: line %d: %v", errUsage, lineNo, err)
		}
		fmt.Fprintln(stdout, rec.CSV())
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: settle [-header] < settlement-file")
		fmt.Fprintln(os.Stderr, "parses fixed-width settlement records from stdin and prints CSV to stdout.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "settle:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'ACCT0001    CR150000      USD20250115\nACCT0002    DR225075      EUR20250116\n' | go run . -header
printf 'ACCT0001    CR150000      USD20250115\nACCT0002    DR225075      EUR2025011\n' | go run .
```

Expected output:

```text
account_id,txn_type,amount_cents,currency,settle_date
ACCT0001,CR,150000,USD,20250115
ACCT0002,DR,225075,EUR,20250116
ACCT0001,CR,150000,USD,20250115
settle: usage: line 2: settlement: record shorter than fixed layout: got 36 bytes, want 37
```

The first command's two-line batch decodes cleanly with the `-header` row
prepended. The second command's second line is missing its last byte — 36
instead of the required 37 — and `settle` prints the CSV row for the first,
good line, then reports the exact line number and byte counts for the one
that failed, and exits 2 without emitting anything for a hypothetical third
line.

### Tests

`TestParseValidLine` pins the field-by-field decomposition against a line
with realistic space padding on two of its fields, and
`TestParseTrimsTrailingSpacePadding` isolates that trim behavior so a
regression that stops trimming, or trims too much, fails independently of
content correctness. `TestParseRejectsShortLine` confirms `ErrShortRecord`
fires — instead of a panic — on a line missing the tail of its last field,
and `TestRecordCSV` pins the exact rendered row.

`TestRunEndToEnd` is the table that drives the tool itself: two valid lines
with no flags, the `-header` flag prepending the column row, a tolerated
trailing blank line, a short line rejected as a usage error, and an unknown
flag rejected the same way. `TestRunStopsAtFirstBadLine` is the sharpest one:
it feeds a good line, then a short line, then a second good line, and asserts
stdout carries only the first record — proving `run` stops at the failure
instead of skipping the bad record and continuing, which would silently
understate the batch.

Create `settlement_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// validLine is one well-formed record: AccountID and AmountCents both carry
// trailing space padding to their field width, exercising the trim.
const validLine = "ACCT0001    " + "CR" + "150000      " + "USD" + "20250115"

// secondLine is a second well-formed record, used to exercise multi-line
// input through run().
const secondLine = "ACCT0002    " + "DR" + "225075      " + "EUR" + "20250116"

func TestParseValidLine(t *testing.T) {
	t.Parallel()

	got, err := Parse(validLine)
	if err != nil {
		t.Fatalf("Parse() unexpected error: %v", err)
	}

	want := Record{
		AccountID:   "ACCT0001",
		TxnType:     "CR",
		AmountCents: "150000",
		Currency:    "USD",
		SettleDate:  "20250115",
	}
	if got != want {
		t.Fatalf("Parse() = %+v, want %+v", got, want)
	}
}

func TestParseRejectsShortLine(t *testing.T) {
	t.Parallel()

	short := validLine[:RecordLen-3] // missing the tail of SettleDate

	_, err := Parse(short)
	if !errors.Is(err, ErrShortRecord) {
		t.Fatalf("Parse(short) err = %v, want ErrShortRecord", err)
	}
}

func TestParseTrimsTrailingSpacePadding(t *testing.T) {
	t.Parallel()

	got, err := Parse(validLine)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		got  string
	}{
		{"AccountID", got.AccountID},
		{"AmountCents", got.AmountCents},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if len(tc.got) == 0 {
				t.Fatalf("%s trimmed to empty string", tc.name)
			}
			if tc.got[len(tc.got)-1] == ' ' {
				t.Fatalf("%s = %q still has trailing space padding", tc.name, tc.got)
			}
		})
	}
}

func TestRecordCSV(t *testing.T) {
	t.Parallel()

	got, err := Parse(validLine)
	if err != nil {
		t.Fatal(err)
	}
	want := "ACCT0001,CR,150000,USD,20250115"
	if got.CSV() != want {
		t.Fatalf("CSV() = %q, want %q", got.CSV(), want)
	}
}

func TestRunEndToEnd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		stdin      string
		wantStdout string
		wantErr    bool
		usage      bool
	}{
		{
			name:       "two valid lines, no header",
			args:       nil,
			stdin:      validLine + "\n" + secondLine + "\n",
			wantStdout: "ACCT0001,CR,150000,USD,20250115\nACCT0002,DR,225075,EUR,20250116\n",
		},
		{
			name:       "header flag prepends the column row",
			args:       []string{"-header"},
			stdin:      validLine + "\n",
			wantStdout: "account_id,txn_type,amount_cents,currency,settle_date\nACCT0001,CR,150000,USD,20250115\n",
		},
		{
			name:       "trailing blank line is tolerated",
			args:       nil,
			stdin:      validLine + "\n\n",
			wantStdout: "ACCT0001,CR,150000,USD,20250115\n",
		},
		{
			name:    "short line is a usage error",
			args:    nil,
			stdin:   validLine + "\n" + validLine[:RecordLen-3] + "\n",
			wantErr: true,
			usage:   true,
		},
		{
			name:    "unknown flag is a usage error",
			args:    []string{"-bogus"},
			stdin:   validLine + "\n",
			wantErr: true,
			usage:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &stdout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run(): want error, got nil")
				}
				if tc.usage && !errors.Is(err, errUsage) {
					t.Fatalf("run() error = %v, want it to wrap errUsage", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(): %v", err)
			}
			if stdout.String() != tc.wantStdout {
				t.Fatalf("run() stdout = %q, want %q", stdout.String(), tc.wantStdout)
			}
		})
	}
}

// TestRunStopsAtFirstBadLine pins that a short line partway through a batch
// stops processing rather than silently skipping the bad record: any CSV
// rows already written for earlier good lines stay on stdout, but nothing
// from the line after the failure is emitted.
func TestRunStopsAtFirstBadLine(t *testing.T) {
	t.Parallel()

	stdin := validLine + "\n" + validLine[:RecordLen-3] + "\n" + secondLine + "\n"
	var stdout bytes.Buffer
	err := run(nil, strings.NewReader(stdin), &stdout)
	if !errors.Is(err, errUsage) {
		t.Fatalf("run() error = %v, want errUsage", err)
	}
	want := "ACCT0001,CR,150000,USD,20250115\n"
	if stdout.String() != want {
		t.Fatalf("run() stdout = %q, want %q (only the line before the failure)", stdout.String(), want)
	}
}
```

## Review

The parser is correct when a well-formed line decomposes into exactly the
expected fields, a short line fails with `ErrShortRecord` rather than a panic
regardless of which field the truncation lands in, and every field's trailing
padding is trimmed without touching the field's actual content. The wrong
turn here is checking `len(line) != RecordLen` instead of `<`: a partner that
appends an optional trailing field, or a trailing newline the caller forgot
to strip, would then be rejected as malformed even though every documented
field is present and correctly positioned — the guard only needs to
guarantee every slice expression stays in bounds, which `<` does without
being needlessly strict about extra trailing bytes. The tool wraps that
parser in a stop-at-first-failure batch reader: exit 2 for a short record or
a bad flag, exit 1 reserved for an I/O failure reading stdin, and `run`
kept free of `os.Args`/`os.Exit` so the full pipeline is testable over a
`strings.Reader` and a `bytes.Buffer`. Run `go test -count=1 -race ./...`.

## Resources

- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions)
- [`strings.TrimRight`](https://pkg.go.dev/strings#TrimRight)
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the line-at-a-time reader `run` streams stdin through.
- [`flag.FlagSet`](https://pkg.go.dev/flag#FlagSet) — `NewFlagSet` with `ContinueOnError` is what lets `run` return a parse error instead of exiting the process.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-cursor-pagination-bounds-clamp.md](10-cursor-pagination-bounds-clamp.md) | Next: [12-cursor-advance-buf-reslice.md](12-cursor-advance-buf-reslice.md)
