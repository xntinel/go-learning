# Exercise 1: Hand-Rolled CSV Export Writer on bytes.Buffer

An export endpoint streams rows to a client as CSV. This exercise builds that
writer on a `bytes.Buffer`: it takes `[]string` records, escapes each field per
RFC 4180, buffers a whole row, and flushes the line to an `io.Writer`. The buffer
is the mutable, append-friendly type that touches the wire; the fields arrive as
immutable strings. Building the escape rules by hand is how you learn what
`encoding/csv` does for you — and the final test proves the two agree.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
csvw/                       independent module: example.com/csvw
  go.mod                    go 1.25
  csvw.go                   Writer; NewWriter, WriteRecord, writeField, needsQuoting, closeRecord, Flush, WriteAll
  cmd/
    demo/
      main.go               writes a few records to stdout
  csvw_test.go              RFC-4180 escape cases + encoding/csv round-trip
```

- Files: `csvw.go`, `cmd/demo/main.go`, `csvw_test.go`.
- Implement: a `Writer` over an `io.Writer` that buffers each row in a `bytes.Buffer`, quotes fields containing comma/quote/CR/LF, doubles internal quotes, and flushes whole lines; plus a `WriteAll` convenience.
- Test: the escape cases (simple, comma, quote-doubling, embedded newline, empty field, multiple records) and a round-trip that parses the output back with `encoding/csv`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why a bytes.Buffer, and where the string/[]byte line falls

The records come in as `[]string` — immutable, read-only handles, exactly right
for input you will not modify. The *output* is the opposite job: you are building a
byte stream one field at a time, doubling quotes, injecting commas and newlines.
That is mutation, so it lives on `[]byte`, and `bytes.Buffer` is the idiomatic
append-friendly wrapper. You buffer an entire row and write it to the sink in one
`Write`, so a partially-escaped row can never reach the client if an error
interrupts mid-field.

RFC 4180 gives the escape rules. A field must be quoted if it contains a comma
(the field separator), a double-quote (the quote character), or a carriage return
or line feed (which would otherwise be read as a record boundary). Inside a quoted
field, a literal double-quote is escaped by *doubling* it: `he said "hi"` becomes
`"he said ""hi"""`. `needsQuoting` scans the field's bytes for any of the four
trigger characters; `writeField` emits the field verbatim when no quoting is
needed, and otherwise wraps it in quotes and doubles any interior quote. Scanning
by byte is correct here because all four trigger characters are ASCII, and in
UTF-8 an ASCII byte never appears as a continuation byte of a multibyte rune — so
a byte-level scan cannot false-match inside a `é` or an emoji.

`closeRecord` appends the record-terminating newline, writes the buffer to the
sink, and `Reset`s the buffer so the next row reuses the same backing array.
`Flush` closes any open record. `WriteAll` is the common-case convenience: write a
slice of records and flush.

The one production caveat, stated up front: ship `encoding/csv`, not this. The
hand-rolled writer exists to make the escape rules concrete; the last test proves
it is byte-compatible with the standard reader, which is the honest way to close
the loop.

Create `csvw.go`:

```go
package csvw

import (
	"bytes"
	"io"
)

// Writer streams []string records to an io.Writer as RFC 4180 CSV. It buffers
// each row in a bytes.Buffer and flushes the whole line at once.
type Writer struct {
	w          io.Writer
	buf        bytes.Buffer
	needsComma bool
	recordOpen bool
}

// NewWriter returns a Writer that emits to w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// WriteRecord escapes and buffers one row. The previous row, if any, is flushed
// to the sink first.
func (w *Writer) WriteRecord(fields []string) error {
	if w.recordOpen {
		if err := w.closeRecord(); err != nil {
			return err
		}
	}
	w.recordOpen = true
	w.needsComma = false
	for _, f := range fields {
		if err := w.writeField(f); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) writeField(field string) error {
	if w.needsComma {
		if err := w.buf.WriteByte(','); err != nil {
			return err
		}
	}
	w.needsComma = true

	if !needsQuoting(field) {
		w.buf.WriteString(field)
		return nil
	}
	w.buf.WriteByte('"')
	for i := range len(field) {
		c := field[i]
		if c == '"' {
			w.buf.WriteString(`""`)
			continue
		}
		w.buf.WriteByte(c)
	}
	w.buf.WriteByte('"')
	return nil
}

// needsQuoting reports whether field contains a character that forces quoting:
// the separator, the quote, or a line break.
func needsQuoting(field string) bool {
	for i := range len(field) {
		c := field[i]
		if c == ',' || c == '"' || c == '\n' || c == '\r' {
			return true
		}
	}
	return false
}

func (w *Writer) closeRecord() error {
	w.buf.WriteByte('\n')
	if _, err := w.w.Write(w.buf.Bytes()); err != nil {
		return err
	}
	w.buf.Reset()
	w.recordOpen = false
	return nil
}

// Flush writes any buffered record to the sink.
func (w *Writer) Flush() error {
	if w.recordOpen {
		if err := w.closeRecord(); err != nil {
			return err
		}
	}
	return nil
}

// WriteAll writes every record and flushes.
func WriteAll(w io.Writer, records [][]string) error {
	bw := NewWriter(w)
	for _, r := range records {
		if err := bw.WriteRecord(r); err != nil {
			return err
		}
	}
	return bw.Flush()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/csvw"
)

func main() {
	records := [][]string{
		{"id", "name", "note"},
		{"1", "Alice", "hello, world"},
		{"2", "Bob", `he said "hi"`},
		{"3", "Carol", "line1\nline2"},
	}
	if err := csvw.WriteAll(os.Stdout, records); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id,name,note
1,Alice,"hello, world"
2,Bob,"he said ""hi"""
3,Carol,"line1
line2"
```

### Tests

The table covers the RFC-4180 cases: a plain record, a field quoted for its comma,
quote-doubling, an embedded newline, an empty field, and multiple records.
`TestWriteRoundTripsThroughEncodingCSV` is the load-bearing one: it writes with
`WriteAll`, parses the bytes back with `encoding/csv.NewReader`, and asserts the
parsed records deep-equal the input — proving the hand-rolled escaping is
compatible with the standard reader, not merely self-consistent.

Create `csvw_test.go`:

```go
package csvw

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"reflect"
	"testing"
)

func TestWriteEscaping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		records [][]string
		want    string
	}{
		{"simple", [][]string{{"a", "b", "c"}}, "a,b,c\n"},
		{"comma", [][]string{{"a", "b,c", "d"}}, "a,\"b,c\",d\n"},
		{"quote doubling", [][]string{{`he said "hi"`, "b"}}, `"he said ""hi""",b` + "\n"},
		{"embedded newline", [][]string{{"line1\nline2"}}, "\"line1\nline2\"\n"},
		{"empty field", [][]string{{"a", "", "b"}}, "a,,b\n"},
		{"carriage return", [][]string{{"a\rb"}}, "\"a\rb\"\n"},
		{"multiple records", [][]string{{"h1", "h2"}, {"v1", "v2"}, {"v3", "v4"}}, "h1,h2\nv1,v2\nv3,v4\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := WriteAll(&buf, tc.records); err != nil {
				t.Fatalf("WriteAll: %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriteRoundTripsThroughEncodingCSV(t *testing.T) {
	t.Parallel()
	in := [][]string{
		{"id", "name", "note"},
		{"1", "Alice", "hello, world"},
		{"2", "Bob", `he said "hi"`},
		{"3", "Carol", "line1\nline2"},
		{"4", "", "trailing,comma,"},
	}
	var buf bytes.Buffer
	if err := WriteAll(&buf, in); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	r := csv.NewReader(&buf)
	got, err := r.ReadAll()
	if err != nil {
		t.Fatalf("csv.ReadAll: %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round-trip mismatch:\n got %#v\nwant %#v", got, in)
	}
}

func ExampleWriteAll() {
	var buf bytes.Buffer
	_ = WriteAll(&buf, [][]string{{"a", "b,c"}})
	fmt.Print(buf.String())
	// Output: a,"b,c"
}
```

## Review

The writer is correct when its output parses back to the input it was given —
which is exactly what the round-trip test asserts, and why it is the test that
matters. The escape rules reduce to four trigger characters and the doubling of
interior quotes; if a field is quoted when it need not be, or a quote is left
undoubled, `encoding/csv` will either reject the stream or return a different
shape, and the `reflect.DeepEqual` fails. Scanning by byte is safe precisely
because the triggers are ASCII and UTF-8 never reuses an ASCII byte inside a
multibyte rune.

Two mistakes to avoid. First, do not forget `Flush`: the last record sits in the
buffer until a record is closed, and `WriteAll` flushes for you, but a manual loop
must call it or lose the final line. Second, do not ship this — `encoding/csv`
handles the corner cases (BOMs, `\r\n` line endings, configurable delimiters) that
a hand-rolled writer will miss. This exercise teaches the rules; production uses
the standard library.

## Resources

- [RFC 4180 — Common Format and MIME Type for CSV Files](https://www.rfc-editor.org/rfc/rfc4180)
- [`encoding/csv`](https://pkg.go.dev/encoding/csv) — the standard reader/writer this one is checked against.
- [`bytes.Buffer`](https://pkg.go.dev/bytes#Buffer) — `WriteByte`, `WriteString`, `Bytes`, `Reset`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-zero-copy-conversion-guard.md](02-zero-copy-conversion-guard.md)
