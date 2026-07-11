# Exercise 4: Stream a CSV Export Endpoint: encoding/csv over a Builder vs Manual Joining

A reporting endpoint that exports rows as CSV is where "just join the fields with a
comma" quietly corrupts data the first time a field contains a comma, a quote, or a
newline. This exercise builds the export handler the correct way — `encoding/csv`
writing over a `strings.Builder`, then straight into an `http.ResponseWriter` — and
proves with a round-trip test that the naive manual join loses data the stdlib encoder
preserves.

This module is self-contained.

## What you'll build

```text
csvexport/                   independent module: example.com/csvexport
  go.mod
  csvexport.go               EncodeCSV (encoding/csv), NaiveJoin (buggy manual join), HTTP handler
  cmd/
    demo/
      main.go                encodes a row with an embedded comma and quote, prints both
  csvexport_test.go          round-trip test through csv.Reader; naive-join corruption; handler test
```

Files: `csvexport.go`, `cmd/demo/main.go`, `csvexport_test.go`.
Implement: `EncodeCSV(records [][]string) (string, error)` over `csv.Writer`, a deliberately buggy `NaiveJoin`, and `ExportHandler` writing CSV into the response.
Test: golden round-trip through `csv.Reader` for fields with commas/quotes/newlines; assert `NaiveJoin` differs (corrupts); check `csv.Writer.Error` after `Flush`.
Verify: `go test -count=1 -race ./...`

```bash
mkdir -p ~/go-exercises/csvexport/cmd/demo
cd ~/go-exercises/csvexport
go mod init example.com/csvexport
```

### Why the manual join is wrong

RFC 4180 says a CSV field that contains a comma, a double-quote, or a newline must be
wrapped in double-quotes, and any double-quote inside it must be doubled (`"` becomes
`""`). `NaiveJoin` — `strings.Join(record, ",")` per row — does none of that. Feed it
a city named `Portland, OR` and the comma is read back as a field boundary: one field
becomes two, and every subsequent column shifts. Feed it a note containing a newline
and the single record splits into two rows. The corruption is silent: the output is
still "valid-looking" CSV, just with the wrong shape, and it usually surfaces as a
downstream import that is subtly misaligned rather than an error you can catch.

`encoding/csv` handles all of this for you. `csv.NewWriter(w)` wraps any `io.Writer`;
`Write(record []string)` encodes one row with correct quoting; `Flush()` pushes the
buffered bytes to the underlying writer; and `Error()` reports any write error that
occurred during buffered writing (you must check it after `Flush`, because `Write`
itself buffers and may not surface the error immediately). Because it takes an
`io.Writer`, the same code serves two sinks: a `strings.Builder` when you want the CSV
as a string for a test, and an `http.ResponseWriter` when you want to stream it to a
client. That is the reuse the exercise is built around — one encoder, two
destinations.

The round-trip test is the honest proof of correctness: encode records that contain
every troublesome character, then decode the result with `csv.Reader` and assert you
get the original records back. If quoting were wrong, the decode would not
reconstruct the input. The naive join fails that same round-trip, which is what
motivates using the stdlib encoder rather than hand-rolling escaping.

Create `csvexport.go`:

```go
package csvexport

import (
	"encoding/csv"
	"net/http"
	"strings"
)

// EncodeCSV encodes records to RFC 4180 CSV using encoding/csv over a
// strings.Builder. Fields containing commas, quotes, or newlines are quoted and
// escaped correctly.
func EncodeCSV(records [][]string) (string, error) {
	var b strings.Builder
	w := csv.NewWriter(&b)
	for _, rec := range records {
		if err := w.Write(rec); err != nil {
			return "", err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", err
	}
	return b.String(), nil
}

// NaiveJoin is the WRONG way: it joins fields with a comma and rows with a
// newline, ignoring RFC 4180 quoting. It corrupts any field containing a comma,
// a double-quote, or a newline. Kept only to demonstrate the failure.
func NaiveJoin(records [][]string) string {
	var b strings.Builder
	for _, rec := range records {
		b.WriteString(strings.Join(rec, ","))
		b.WriteByte('\n')
	}
	return b.String()
}

// ExportHandler streams records as a CSV download straight into the
// ResponseWriter, without materializing the whole body as a separate string.
func ExportHandler(records [][]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="report.csv"`)
		cw := csv.NewWriter(w)
		for _, rec := range records {
			if err := cw.Write(rec); err != nil {
				http.Error(w, "encode error", http.StatusInternalServerError)
				return
			}
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			http.Error(w, "flush error", http.StatusInternalServerError)
		}
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/csvexport"
)

func main() {
	records := [][]string{
		{"city", "note"},
		{"Portland, OR", `she said "hi"`},
	}
	good, err := csvexport.EncodeCSV(records)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print("csv encoder:\n", good)
	fmt.Print("naive join:\n", csvexport.NaiveJoin(records))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
csv encoder:
city,note
"Portland, OR","she said ""hi"""
naive join:
city,note
Portland, OR,she said "hi"
```

### Tests

The first test round-trips troublesome records through `csv.Reader` and asserts they
survive. The second decodes the naive join and asserts it does *not* reconstruct the
input — proving the corruption. The third drives the HTTP handler through
`httptest.ResponseRecorder` and checks the header and body.

Create `csvexport_test.go`:

```go
package csvexport

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestEncodeCSVRoundTrips(t *testing.T) {
	t.Parallel()

	records := [][]string{
		{"id", "city", "note"},
		{"1", "Portland, OR", `she said "hi"`},
		{"2", "line1\nline2", "plain"},
	}

	encoded, err := EncodeCSV(records)
	if err != nil {
		t.Fatalf("EncodeCSV: %v", err)
	}

	got, err := csv.NewReader(strings.NewReader(encoded)).ReadAll()
	if err != nil {
		t.Fatalf("re-decode failed: %v", err)
	}
	if !reflect.DeepEqual(got, records) {
		t.Fatalf("round trip mismatch:\n got  %q\n want %q", got, records)
	}
}

func TestNaiveJoinCorrupts(t *testing.T) {
	t.Parallel()

	records := [][]string{{"Portland, OR", "ok"}}
	decoded, err := csv.NewReader(strings.NewReader(NaiveJoin(records))).ReadAll()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The comma inside the first field was read as a delimiter: one row of two
	// fields became one row of three.
	if len(decoded[0]) != 3 {
		t.Fatalf("expected naive join to corrupt into 3 fields, got %d: %q", len(decoded[0]), decoded[0])
	}
}

func TestExportHandler(t *testing.T) {
	t.Parallel()

	h := ExportHandler([][]string{{"a", "b"}, {"1,2", "3"}})
	req := httptest.NewRequest(http.MethodGet, "/export", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	const want = "a,b\n\"1,2\",3\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func ExampleEncodeCSV() {
	out, _ := EncodeCSV([][]string{{"Portland, OR", "ok"}})
	fmt.Print(out)
	// Output: "Portland, OR",ok
}
```

## Review

The encoder is correct when troublesome fields round-trip: encode with `csv.Writer`,
decode with `csv.Reader`, and get the original `[][]string` back. `TestNaiveJoinCorrupts`
is the counter-example that justifies the stdlib — the manual comma-join turns a
two-field row into three fields. Two operational details matter: check `csv.Writer.Error()`
after `Flush()` because `Write` buffers and defers write errors, and reuse the same
encoder over both a `strings.Builder` (tests) and an `http.ResponseWriter` (streaming)
rather than building a string and copying it into the response. Never hand-roll CSV
quoting; the stdlib already implements RFC 4180.

## Resources

- [encoding/csv.Writer](https://pkg.go.dev/encoding/csv#Writer) — `Write`, `Flush`, `Error`.
- [RFC 4180](https://www.rfc-editor.org/rfc/rfc4180) — the CSV format and its quoting rules.
- [encoding/csv.Reader](https://pkg.go.dev/encoding/csv#Reader) — `ReadAll`, used for the round-trip proof.

---

Prev: [03-sql-bulk-insert-placeholder-builder.md](03-sql-bulk-insert-placeholder-builder.md) | Back to [00-concepts.md](00-concepts.md) | Next: [05-prometheus-text-exposition-builder.md](05-prometheus-text-exposition-builder.md)
