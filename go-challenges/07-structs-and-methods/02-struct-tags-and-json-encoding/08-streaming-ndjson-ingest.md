# Exercise 8: Stream a Large NDJSON Payload Without Loading It All Into Memory

An ingest endpoint that receives a million records must not read the whole body
into memory before processing it — that is an out-of-memory waiting to happen and
a denial-of-service vector. The fix is to stream: `json.Decoder.Decode` in a loop
pulls one record at a time, and `json.Encoder.Encode` writes results incrementally,
so memory stays bounded by a single record regardless of stream length. This module
builds that loop for newline-delimited JSON and for a top-level JSON array.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
ndjson/                        independent module: example.com/ndjson
  go.mod                       go 1.24
  ingest/
    ingest.go                  ProcessNDJSON (Decode loop + Encode), CountArray (Token/More)
  cmd/
    demo/
      main.go                  process a 3-line NDJSON stream, count an array
  ingest/ingest_test.go        order+EOF, array count, mid-stream error, valid NDJSON out
```

Files: `ingest/ingest.go`, `cmd/demo/main.go`, `ingest/ingest_test.go`.
Implement: `ProcessNDJSON(io.Reader, io.Writer) (int, error)` looping `Decoder.Decode` to `io.EOF` and emitting with `Encoder.Encode`; `CountArray(io.Reader) (int, error)` using `Decoder.Token` + `Decoder.More`.
Test: records decode in order and the loop ends on `io.EOF`; a top-level array is counted; a malformed middle record surfaces an error tied to its position; the encoder emits valid one-object-per-line NDJSON.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/02-struct-tags-and-json-encoding/08-streaming-ndjson-ingest/ingest go-solutions/07-structs-and-methods/02-struct-tags-and-json-encoding/08-streaming-ndjson-ingest/cmd/demo
cd go-solutions/07-structs-and-methods/02-struct-tags-and-json-encoding/08-streaming-ndjson-ingest
go mod edit -go=1.24
```

## Decode in a loop, encode incrementally

`json.Unmarshal` takes a `[]byte` — it needs the entire input in memory first. A
`json.Decoder` wraps an `io.Reader` and reads incrementally, and that difference is
the whole lesson. For newline-delimited JSON (NDJSON: one JSON value per line, the
format used by log pipelines, bulk APIs, and BigQuery/Elasticsearch loaders), you
call `dec.Decode(&rec)` in a loop. Each call reads exactly one value and advances;
when the stream is exhausted, `Decode` returns `io.EOF`, which is the loop's normal
termination signal — check it with `errors.Is(err, io.EOF)` and break. Any other
error is a real decode failure, and because you know how many records you have
already processed, you can report *which* record failed, which is exactly the
diagnostic an ops engineer needs when line 40,000 of a feed is bad.

The output side mirrors this. `json.Encoder.Encode` writes one value followed by a
newline, so encoding each result as you compute it produces valid NDJSON and never
buffers more than one record. Reading and writing are both streaming, so the whole
pipeline runs in constant memory.

A top-level JSON **array** (`[ {...}, {...} ]`) is the other common shape and needs
token-level control, because the array is one big value that `Decode` would
otherwise buffer whole. `dec.Token()` reads the opening `[` delimiter;
`dec.More()` reports whether another element remains before the closing `]`; you
`Decode` each element inside the loop; and a final `dec.Token()` consumes the `]`.
This streams the array's elements one at a time even though syntactically they are
wrapped in a single array value.

Create `ingest/ingest.go`:

```go
// ingest/ingest.go
package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Record is one input line; Result is one output line.
type Record struct {
	ID    string `json:"id"`
	Value int    `json:"value"`
}

type Result struct {
	ID      string `json:"id"`
	Doubled int    `json:"doubled"`
}

// ProcessNDJSON streams newline-delimited JSON from r, transforms each record,
// and writes NDJSON results to w. Memory stays bounded by one record. It returns
// the number of records processed and an error tagged with the failing record's
// position.
func ProcessNDJSON(r io.Reader, w io.Writer) (int, error) {
	dec := json.NewDecoder(r)
	enc := json.NewEncoder(w)
	n := 0
	for {
		var rec Record
		err := dec.Decode(&rec)
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("record %d: %w", n+1, err)
		}
		if err := enc.Encode(Result{ID: rec.ID, Doubled: rec.Value * 2}); err != nil {
			return n, fmt.Errorf("encode record %d: %w", n+1, err)
		}
		n++
	}
}

// CountArray streams a top-level JSON array and counts its elements using
// Token and More, without buffering the whole array.
func CountArray(r io.Reader) (int, error) {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return 0, fmt.Errorf("read opening token: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return 0, fmt.Errorf("expected a JSON array, got %v", tok)
	}
	n := 0
	for dec.More() {
		var rec Record
		if err := dec.Decode(&rec); err != nil {
			return n, fmt.Errorf("element %d: %w", n+1, err)
		}
		n++
	}
	if _, err := dec.Token(); err != nil {
		return n, fmt.Errorf("read closing token: %w", err)
	}
	return n, nil
}
```

## The runnable demo

The demo streams a three-line NDJSON input to a buffer and prints the count and the
emitted NDJSON, then counts a top-level array.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"bytes"
	"fmt"
	"strings"

	"example.com/ndjson/ingest"
)

func main() {
	in := strings.NewReader(`{"id":"a","value":1}
{"id":"b","value":2}
{"id":"c","value":3}`)

	var out bytes.Buffer
	n, err := ingest.ProcessNDJSON(in, &out)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("processed %d records\n", n)
	fmt.Print(out.String())

	count, _ := ingest.CountArray(strings.NewReader(`[{"id":"x","value":1},{"id":"y","value":2}]`))
	fmt.Printf("array count: %d\n", count)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed 3 records
{"id":"a","doubled":2}
{"id":"b","doubled":4}
{"id":"c","doubled":6}
array count: 2
```

## Tests

`TestProcessesInOrderToEOF` streams three records and asserts three lines out in
order, each a valid JSON object. `TestValidNDJSONOutput` asserts every output line
parses on its own. `TestMidStreamErrorIsPositioned` puts a bad record in the middle
and asserts the error names its position and that the records before it were
processed. `TestCountArray` counts a top-level array with `Token`/`More`.

Create `ingest/ingest_test.go`:

```go
// ingest/ingest_test.go
package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestProcessesInOrderToEOF(t *testing.T) {
	t.Parallel()
	in := strings.NewReader(`{"id":"a","value":1}
{"id":"b","value":2}
{"id":"c","value":3}`)
	var out bytes.Buffer
	n, err := ProcessNDJSON(in, &out)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("processed %d, want 3", n)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	want := []string{`{"id":"a","doubled":2}`, `{"id":"b","doubled":4}`, `{"id":"c","doubled":6}`}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %q", len(lines), len(want), out.String())
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestValidNDJSONOutput(t *testing.T) {
	t.Parallel()
	in := strings.NewReader(`{"id":"a","value":10}
{"id":"b","value":20}`)
	var out bytes.Buffer
	if _, err := ProcessNDJSON(in, &out); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		var r Result
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("output line is not valid JSON: %q: %v", line, err)
		}
	}
}

func TestMidStreamErrorIsPositioned(t *testing.T) {
	t.Parallel()
	in := strings.NewReader(`{"id":"a","value":1}
{"id":"b","value":"oops"}
{"id":"c","value":3}`)
	var out bytes.Buffer
	n, err := ProcessNDJSON(in, &out)
	if err == nil {
		t.Fatal("expected an error on the malformed middle record")
	}
	if !strings.Contains(err.Error(), "record 2") {
		t.Fatalf("error should name record 2, got %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 record processed before the failure, got %d", n)
	}
}

func TestCountArray(t *testing.T) {
	t.Parallel()
	got, err := CountArray(strings.NewReader(`[{"id":"x","value":1},{"id":"y","value":2},{"id":"z","value":3}]`))
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("counted %d, want 3", got)
	}
}

func ExampleProcessNDJSON() {
	var out bytes.Buffer
	n, _ := ProcessNDJSON(strings.NewReader(`{"id":"a","value":21}`), &out)
	fmt.Printf("%d %s", n, out.String())
	// Output: 1 {"id":"a","doubled":42}
}
```

## Review

The pipeline is correct when every record is transformed in order, the loop ends
cleanly on `io.EOF`, a bad record surfaces an error naming its position (with the
prior records already emitted), and the output parses as one JSON object per line.
The point is memory: `Decode` in a loop and `Encode` per result hold one record at
a time, so the same code processes three records or three billion in constant
space, whereas a single `json.Unmarshal` of the body would scale memory with input
size — the difference between a robust ingest path and an OOM under load. Reach for
`Token`/`More` only when the payload is a single top-level array you must stream
element by element.

## Resources

- [`json.Decoder.Decode`](https://pkg.go.dev/encoding/json#Decoder.Decode) — streaming one value at a time and the `io.EOF` contract.
- [`json.Decoder.Token`](https://pkg.go.dev/encoding/json#Decoder.Token) and [`More`](https://pkg.go.dev/encoding/json#Decoder.More) — token-level streaming of an array.
- [`json.Encoder.Encode`](https://pkg.go.dev/encoding/json#Encoder.Encode) — incremental output, one newline-terminated value per call.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-rawmessage-polymorphic-envelope.md](07-rawmessage-polymorphic-envelope.md) | Next: [09-number-precision-usenumber.md](09-number-precision-usenumber.md)
