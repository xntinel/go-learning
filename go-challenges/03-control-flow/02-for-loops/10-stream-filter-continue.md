# Exercise 10: Early-Continue Guard Clauses in a Log/CSV Ingestion Loop

Ingesting a file line by line — a CSV import, a log tail, an inventory feed — is a
`for scanner.Scan()` loop whose body is mostly *rejection*: blank lines, comment
lines, and malformed rows all have to be skipped before the one interesting case,
a valid parsed record, is reached. The clean way to write that body is a stack of
early `continue` guard clauses, each of which handles one reject-and-move-on case
so the happy path stays at the loop's top indentation level. This module builds
`ParseRecords` and tests every branch — valid rows, each skip reason, and a
scanner error — deterministically from an in-memory `strings.Reader`.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
ingest/                     module example.com/ingest
  go.mod
  ingest.go                 ParseRecords(r) (Result, error); Record; Result{Records, Skipped, Rejected}
  ingest_test.go            valid/blank/comment/malformed mix, all-valid, all-invalid, oversized-line error
  cmd/demo/
    main.go                 parses an inline feed and prints records plus skip/reject counts
```

- Files: `ingest.go`, `ingest_test.go`, `cmd/demo/main.go`.
- Implement: `ParseRecords(r io.Reader) (Result, error)` — a `for scanner.Scan()` loop that uses early `continue` guards to skip blank and comment lines (counted in `Skipped`) and malformed rows (counted in `Rejected`), appending each valid `Record` in order; a scanner failure (an oversized line) is surfaced via `bufio.Scanner.Err` wrapped with `%w`.
- Test: a `strings.Reader` mixing valid rows, blank lines, comment lines, and malformed rows (assert the exact records in order and exact `Skipped`/`Rejected` counts); an all-valid and an all-invalid edge case; and a `>64KB` line that trips `bufio.ErrTooLong` surfaced through `Err`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ingest/cmd/demo
cd ~/go-exercises/ingest
go mod init example.com/ingest
```

### continue is a flattening tool, not a jump

The tempting way to write an ingestion body is a nested `if`: "if the line is not
blank, then if it is not a comment, then if it splits into the right number of
fields, then if the amount parses, append it." Four levels of indentation, and the
one line that matters — the `append` — is buried at the bottom under a pile of
closing braces. Every reader has to hold four conditions in their head to know
when a record is kept.

Inverting each condition into an early `continue` guard fixes this. Each guard
states one reason to reject the line and immediately moves to the next iteration;
after the last guard, the code is unconditionally on the happy path at the loop's
top indentation level. The body reads top to bottom as "here are the four reasons
we skip a line, and here is what we do with the ones that survive." This is the
same shape as early-return guard clauses in a function, applied to a loop.

The ordering of the guards encodes intent and is not arbitrary. Blank and comment
lines are *expected* input a well-formed feed contains — they are `Skipped`, not
errors. A row with the wrong field count or an unparseable amount is *malformed*
data — it is `Rejected`, a distinct counter an operator watches to catch a broken
upstream. Separating "skipped" from "rejected" turns the loop into an
observability surface: a feed that suddenly rejects half its rows is a signal, and
the counts make it visible.

The one thing the loop must never do is swallow a *scanner* failure. `Scan`
returns `false` both at clean end-of-input and on an error such as a line longer
than the scanner's buffer (`bufio.ErrTooLong`). The two are told apart only by
checking `scanner.Err()` after the loop: `nil` means a clean finish, non-nil means
the input was truncated and the parsed records are incomplete. Returning that
error wrapped with `%w` lets a caller distinguish "the feed had some bad rows"
(reflected in `Rejected`) from "we could not read the whole feed" (a hard error).

Create `ingest.go`:

```go
package ingest

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Record is one parsed inventory adjustment: a SKU and a signed quantity delta.
type Record struct {
	SKU   string
	Delta int
}

// Result is the outcome of parsing a feed: the valid records in input order, the
// number of blank/comment lines skipped, and the number of malformed rows
// rejected.
type Result struct {
	Records  []Record
	Skipped  int
	Rejected int
}

// ParseRecords reads a line-oriented "sku,delta" feed. Blank and comment lines
// are skipped, malformed rows are rejected, and valid rows are collected in
// order. A scanner failure (for example a line longer than the buffer) is
// returned wrapped, with the partial Result gathered so far.
func ParseRecords(r io.Reader) (Result, error) {
	var res Result
	sc := bufio.NewScanner(r)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())

		// Guard 1: blank line — expected filler, skip it.
		if line == "" {
			res.Skipped++
			continue
		}
		// Guard 2: comment line — expected metadata, skip it.
		if strings.HasPrefix(line, "#") {
			res.Skipped++
			continue
		}
		// Guard 3: wrong shape — malformed, reject it.
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			res.Rejected++
			continue
		}
		// Guard 4: unparseable amount or empty key — malformed, reject it.
		sku := strings.TrimSpace(fields[0])
		delta, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if sku == "" || err != nil {
			res.Rejected++
			continue
		}

		// Happy path: every guard passed, so this row is valid.
		res.Records = append(res.Records, Record{SKU: sku, Delta: delta})
	}

	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("ingest: scan feed: %w", err)
	}
	return res, nil
}
```

### The runnable demo

The demo parses a small inline feed containing every case — valid rows, a blank
line, a comment, a short row, and a row with a non-numeric amount — and prints the
records plus the two counters.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/ingest"
)

func main() {
	feed := strings.Join([]string{
		"# inventory deltas for 2026-07-02",
		"WIDGET-1,5",
		"",
		"WIDGET-2,-3",
		"WIDGET-3",       // short row: rejected
		"WIDGET-4,seven", // bad amount: rejected
		"WIDGET-5,12",
	}, "\n")

	res, err := ingest.ParseRecords(strings.NewReader(feed))
	if err != nil {
		fmt.Printf("read error: %v\n", err)
		return
	}

	for _, r := range res.Records {
		fmt.Printf("record: %s %+d\n", r.SKU, r.Delta)
	}
	fmt.Printf("skipped=%d rejected=%d\n", res.Skipped, res.Rejected)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
record: WIDGET-1 +5
record: WIDGET-2 -3
record: WIDGET-5 +12
skipped=2 rejected=2
```

### Tests

The suite drives `ParseRecords` from a `strings.Reader`, so nothing touches the
filesystem and every case is deterministic. The mixed-input table asserts both the
exact records in order and the exact `Skipped`/`Rejected` counts, which is what
pins the guard ordering — a bug that counted a comment as rejected would move a
number and fail. The oversized-line case builds a single token longer than
`bufio.MaxScanTokenSize`; `Scan` stops early and `Err` returns `bufio.ErrTooLong`,
which `ParseRecords` wraps and the test asserts with `errors.Is`.

Create `ingest_test.go`:

```go
package ingest

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseRecords(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		wantRecords []Record
		wantSkip    int
		wantReject  int
	}{
		{
			name: "mixed feed",
			input: "# header\n" +
				"A,1\n" +
				"\n" +
				"B,-2\n" +
				"C\n" + // short: reject
				"D,x\n" + // bad int: reject
				"   \n" + // whitespace-only: skip
				"E,3\n",
			wantRecords: []Record{{"A", 1}, {"B", -2}, {"E", 3}},
			wantSkip:    3, // header, blank, whitespace-only
			wantReject:  2, // short row, bad int
		},
		{
			name:        "all valid",
			input:       "A,1\nB,2\nC,3\n",
			wantRecords: []Record{{"A", 1}, {"B", 2}, {"C", 3}},
			wantSkip:    0,
			wantReject:  0,
		},
		{
			name:        "all invalid",
			input:       "A\nB,,\n,5\nX,nope\n",
			wantRecords: nil,
			wantSkip:    0,
			wantReject:  4, // short, three fields, empty sku, bad int
		},
		{
			name:        "only skips",
			input:       "# a\n\n# b\n",
			wantRecords: nil,
			wantSkip:    3,
			wantReject:  0,
		},
		{
			name:        "empty input",
			input:       "",
			wantRecords: nil,
			wantSkip:    0,
			wantReject:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res, err := ParseRecords(strings.NewReader(tc.input))
			if err != nil {
				t.Fatalf("ParseRecords() error = %v, want nil", err)
			}
			if len(res.Records) != len(tc.wantRecords) {
				t.Fatalf("got %d records, want %d: %+v", len(res.Records), len(tc.wantRecords), res.Records)
			}
			for i, want := range tc.wantRecords {
				if res.Records[i] != want {
					t.Errorf("record[%d] = %+v, want %+v", i, res.Records[i], want)
				}
			}
			if res.Skipped != tc.wantSkip {
				t.Errorf("Skipped = %d, want %d", res.Skipped, tc.wantSkip)
			}
			if res.Rejected != tc.wantReject {
				t.Errorf("Rejected = %d, want %d", res.Rejected, tc.wantReject)
			}
		})
	}
}

func TestParseRecordsScannerError(t *testing.T) {
	t.Parallel()

	// A single token longer than the scanner's buffer trips bufio.ErrTooLong,
	// which Scan surfaces via Err after stopping early.
	huge := strings.Repeat("a", bufio.MaxScanTokenSize+1)
	res, err := ParseRecords(strings.NewReader(huge))
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("err = %v, want bufio.ErrTooLong", err)
	}
	if len(res.Records) != 0 {
		t.Fatalf("got %d records on a truncated read, want 0", len(res.Records))
	}
}

func ExampleParseRecords() {
	res, _ := ParseRecords(strings.NewReader("# feed\nSKU-1,4\n\nSKU-1,bad\n"))
	fmt.Println(len(res.Records), res.Skipped, res.Rejected)
	// Output: 1 2 1
}
```

## Review

`ParseRecords` is correct when the happy path is unconditional. Each `continue`
guard rejects exactly one shape of bad line and moves on, so after the last guard
the `append` runs with no surrounding `if` — that is the flattening the concepts
file describes, and it is what keeps a body with four skip reasons readable. The
two counters must stay distinct: blank and comment lines are *expected* and counted
as `Skipped`, malformed rows are *data errors* and counted as `Rejected`, and the
tests pin both numbers so a mis-ordered guard fails. The scanner error is the trap:
`Scan` returning `false` is ambiguous, so `Err` is checked after the loop and
`bufio.ErrTooLong` is wrapped with `%w` and returned — a caller must be able to
tell a truncated read from a clean one with some bad rows. Run
`go test -count=1 -race ./...`.

## Resources

- [bufio.Scanner](https://pkg.go.dev/bufio#Scanner) — `Scan`, `Text`, and the `Err` that distinguishes a clean finish from `ErrTooLong`.
- [strings.NewReader](https://pkg.go.dev/strings#NewReader) — the in-memory `io.Reader` the tests feed the parser.
- [strconv.Atoi](https://pkg.go.dev/strconv#Atoi) — the amount parse whose error becomes a rejected row.
- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — `continue` and the loop forms.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-rangefunc-paginator.md](09-rangefunc-paginator.md) | Next: [11-paginated-drain-safety-cap.md](11-paginated-drain-safety-cap.md)
