# Exercise 1: Bounded-Memory Stream Ingest with UnmarshalDecode

An ingest endpoint that decodes a whole JSON array into `[]Event` before summing
it turns any large body into an out-of-memory lever. This exercise builds a
constant-memory ingester on `json.UnmarshalDecode` and a shared
`jsontext.Decoder`: it reads one element at a time into a single reused struct,
enforces an element budget, and reports malformed input with an exact byte offset
and JSON pointer.

Everything here is gated behind `GOEXPERIMENT=jsonv2`; the files carry the
`//go:build goexperiment.jsonv2` constraint.

## What you'll build

```text
streamingest/                 independent module: example.com/streamingest
  go.mod                      go 1.26
  ingest.go                   Event, Summary, ErrTooManyElements; SumArray, SumStream
  cmd/
    demo/
      main.go                 runnable demo: array sum, concatenated-stream sum, malformed report
  ingest_test.go              streaming, budget, malformed-offset, and EOF tests
```

Files: `ingest.go`, `cmd/demo/main.go`, `ingest_test.go`.
Implement: `SumArray(r, limit)` streaming a JSON array one element at a time, and `SumStream(r)` folding a whitespace-separated sequence of top-level values until `io.EOF`.
Test: a generating `io.Reader` proving the array need never be materialized, an element-budget rejection via a wrapped sentinel, a malformed element asserted to unwrap to `*json.SemanticError` with a nonzero `ByteOffset` and the pointer `/1/amount`, and a concatenated-stream fold to EOF.
Verify: `go test -count=1 -race ./...`

Set up the module. `encoding/json/v2` requires the experiment, so the language
version is pinned and the experiment must be set in the environment:

```bash
mkdir -p go-solutions/48-modern-go-language-and-stdlib/12-encoding-json-v2/01-streaming-decode-large-arrays/cmd/demo
cd go-solutions/48-modern-go-language-and-stdlib/12-encoding-json-v2/01-streaming-decode-large-arrays
go mod edit -go=1.26
export GOEXPERIMENT=jsonv2
```

### Why UnmarshalDecode is the streaming primitive

`json.Unmarshal(data, &slice)` and `json.UnmarshalRead(body, &slice)` both build
the entire Go value before returning: to sum a million-element array they
materialize a million `Event`s (and, transiently, the whole input). The memory
ceiling is the payload size, which an attacker controls.

`json.UnmarshalDecode(dec, &v)` decodes *exactly one* JSON value from the shared
`jsontext.Decoder` `dec` and returns. The decoder holds only its small read buffer
and a position; the Go side holds one `Event`. So the loop below has a memory
ceiling set by *one element*, not by the body. That is the whole design: read the
opening `[`, then decode one element per iteration into a single reused struct,
folding each into the running `Summary`, until the decoder reports the closing `]`.

The framing is done at the token layer so we never guess. `dec.ReadToken()`
consumes the opening `[` (and we reject any top-level value that is not an array).
`dec.PeekKind()` looks at the next token without consuming it: while it is not
`]`, another element remains. `dec.InputOffset()` gives the absolute byte position,
which we fold into the budget-exceeded error so the log line points at the exact
place the stream grew too large.

Because `SumArray` and `SumStream` decode against one long-lived decoder, a
`*json.SemanticError` from a bad element carries a `JSONPointer` relative to the
*whole* document (for the second element's `amount` field, `/1/amount`), not
relative to the element in isolation. That is a direct benefit of sharing the
decoder rather than re-parsing each element from a fresh `[]byte`.

Create `ingest.go`:

```go
//go:build goexperiment.jsonv2

package streamingest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
)

// ErrTooManyElements is returned when a stream exceeds the caller's budget.
var ErrTooManyElements = errors.New("streamingest: element budget exceeded")

// Event is one metering record. It holds only integers, so decoding into a
// reused value allocates nothing per element.
type Event struct {
	ID     int64 `json:"id"`
	Amount int64 `json:"amount"`
}

// Summary is the running aggregate over a stream of events.
type Summary struct {
	Count int
	Total int64
}

// SumArray decodes a single top-level JSON array of Events one element at a
// time, never holding more than one Event in memory. limit caps the number of
// elements; exceeding it returns an error wrapping ErrTooManyElements. A
// malformed element returns the decoder's *json.SemanticError unchanged, so the
// caller can read its ByteOffset and JSONPointer.
func SumArray(r io.Reader, limit int) (Summary, error) {
	dec := jsontext.NewDecoder(r)

	tok, err := dec.ReadToken() // consume the opening '['
	if err != nil {
		return Summary{}, err
	}
	if tok.Kind() != jsontext.KindBeginArray {
		return Summary{}, fmt.Errorf("streamingest: want JSON array, got %v", tok.Kind())
	}

	var sum Summary
	var ev Event // reused across every element
	for dec.PeekKind() != jsontext.KindEndArray {
		if sum.Count == limit {
			return sum, fmt.Errorf("at byte %d: %w", dec.InputOffset(), ErrTooManyElements)
		}
		ev = Event{}
		if err := json.UnmarshalDecode(dec, &ev); err != nil {
			return sum, err
		}
		sum.Count++
		sum.Total += ev.Amount
	}

	if _, err := dec.ReadToken(); err != nil { // consume the closing ']'
		return sum, err
	}
	return sum, nil
}

// SumStream folds a whitespace-separated sequence of top-level JSON objects
// (not wrapped in an array), decoding one value per call until io.EOF.
func SumStream(r io.Reader) (Summary, error) {
	dec := jsontext.NewDecoder(r)
	var sum Summary
	var ev Event
	for {
		ev = Event{}
		err := json.UnmarshalDecode(dec, &ev)
		if errors.Is(err, io.EOF) {
			return sum, nil
		}
		if err != nil {
			return sum, err
		}
		sum.Count++
		sum.Total += ev.Amount
	}
}
```

### The runnable demo

The demo exercises all three behaviors against small in-memory readers: a sum over
a JSON array, a sum over a concatenated stream of top-level objects, and a
malformed array whose second element has a string where an integer belongs. The
malformed branch pulls the `JSONPointer` off the `*json.SemanticError` to show the
exact failing location.

Create `cmd/demo/main.go`:

```go
//go:build goexperiment.jsonv2

package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"example.com/streamingest"
)

func main() {
	arr := `[{"id":1,"amount":100},{"id":2,"amount":250},{"id":3,"amount":50}]`
	s, err := streamingest.SumArray(strings.NewReader(arr), 1000)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("array: count=%d total=%d\n", s.Count, s.Total)

	stream := `{"id":10,"amount":5} {"id":11,"amount":7}`
	s2, err := streamingest.SumStream(strings.NewReader(stream))
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("stream: count=%d total=%d\n", s2.Count, s2.Total)

	bad := `[{"id":1,"amount":100},{"id":2,"amount":"oops"}]`
	_, err = streamingest.SumArray(strings.NewReader(bad), 1000)
	var semErr *json.SemanticError
	if errors.As(err, &semErr) {
		fmt.Printf("malformed: decode failed at %s\n", semErr.JSONPointer)
	}
}
```

Run it:

```bash
GOEXPERIMENT=jsonv2 go run ./cmd/demo
```

Expected output:

```
array: count=3 total=400
stream: count=2 total=12
malformed: decode failed at /1/amount
```

### Tests

The tests prove the two properties that matter. `TestSumArrayLargeStreaming`
feeds fifty thousand elements from an `io.Pipe` whose writer goroutine generates
the array on the fly — the full document never exists in memory, so a correct
result is evidence the loop is incremental rather than buffered.
`TestMalformedElement` asserts the returned error unwraps to `*json.SemanticError`
with a nonzero `ByteOffset` and the pointer `/1/amount`, confirming the shared
decoder reports document-relative locations. `TestElementBudget` asserts the
budget error unwraps to the sentinel with `errors.Is`, and `TestSumStreamEOF`
confirms the concatenated fold terminates cleanly at `io.EOF`.

Create `ingest_test.go`:

```go
//go:build goexperiment.jsonv2

package streamingest

import (
	"bufio"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// genArray streams "[{...},{...},...]" from a writer goroutine so the full array
// is never held in memory by the reader under test.
func genArray(n int) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		bw := bufio.NewWriter(pw)
		bw.WriteByte('[')
		for i := range n {
			if i > 0 {
				bw.WriteByte(',')
			}
			fmt.Fprintf(bw, `{"id":%d,"amount":1}`, i)
		}
		bw.WriteByte(']')
		bw.Flush()
		pw.Close()
	}()
	return pr
}

func TestSumArray(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  Summary
	}{
		{"empty", `[]`, Summary{Count: 0, Total: 0}},
		{"one", `[{"id":1,"amount":42}]`, Summary{Count: 1, Total: 42}},
		{"many", `[{"id":1,"amount":10},{"id":2,"amount":20},{"id":3,"amount":30}]`, Summary{Count: 3, Total: 60}},
		{"whitespace", "[\n  {\"id\":1,\"amount\":5}\n]", Summary{Count: 1, Total: 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := SumArray(strings.NewReader(tt.input), 1000)
			if err != nil {
				t.Fatalf("SumArray: %v", err)
			}
			if got != tt.want {
				t.Fatalf("SumArray = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSumArrayLargeStreaming(t *testing.T) {
	t.Parallel()
	const n = 50000
	got, err := SumArray(genArray(n), n+1)
	if err != nil {
		t.Fatalf("SumArray: %v", err)
	}
	// Each element contributes amount=1, so Total==Count==n. The io.Pipe source
	// never materializes the whole array, so a correct fold proves the loop is
	// incremental rather than buffering the document.
	if got.Count != n || got.Total != int64(n) {
		t.Fatalf("SumArray = %+v, want Count=Total=%d", got, n)
	}
}

func TestElementBudget(t *testing.T) {
	t.Parallel()
	_, err := SumArray(genArray(10), 5)
	if !errors.Is(err, ErrTooManyElements) {
		t.Fatalf("err = %v, want ErrTooManyElements", err)
	}
}

func TestMalformedElement(t *testing.T) {
	t.Parallel()
	const input = `[{"id":1,"amount":100},{"id":2,"amount":"oops"}]`
	_, err := SumArray(strings.NewReader(input), 1000)
	var semErr *json.SemanticError
	if !errors.As(err, &semErr) {
		t.Fatalf("err = %v (%T), want *json.SemanticError", err, err)
	}
	if semErr.ByteOffset == 0 {
		t.Errorf("SemanticError.ByteOffset = 0, want nonzero")
	}
	if got := string(semErr.JSONPointer); got != "/1/amount" {
		t.Errorf("SemanticError.JSONPointer = %q, want %q", got, "/1/amount")
	}
}

func TestSumStreamEOF(t *testing.T) {
	t.Parallel()
	const input = `{"id":10,"amount":5}
{"id":11,"amount":7}
{"id":12,"amount":9}`
	got, err := SumStream(strings.NewReader(input))
	if err != nil {
		t.Fatalf("SumStream: %v", err)
	}
	want := Summary{Count: 3, Total: 21}
	if got != want {
		t.Fatalf("SumStream = %+v, want %+v", got, want)
	}
}

func ExampleSumArray() {
	const input = `[{"id":1,"amount":100},{"id":2,"amount":250}]`
	s, _ := SumArray(strings.NewReader(input), 100)
	fmt.Printf("count=%d total=%d\n", s.Count, s.Total)
	// Output: count=2 total=350
}
```

## Review

The ingester is correct when its memory ceiling is one element, not the payload:
`SumArray` reads the framing tokens with `ReadToken`/`PeekKind`, decodes each
element with `UnmarshalDecode` into a single reused `Event`, and folds it into the
`Summary` — nothing accumulates a slice of elements. The large-stream test is the
proof: fifty thousand elements arriving through an `io.Pipe`, summed correctly,
with the document never fully in memory.

The mistakes to avoid are the ones that quietly reintroduce the memory ceiling or
lose the location. Do not call `Unmarshal`/`UnmarshalRead` into a `[]Event` and
range over it "because it is simpler" — that buffers the whole body, which is the
attack you are defending against. Do not build a fresh decoder per element from a
sliced `[]byte`; sharing one decoder is what makes `JSONPointer` document-relative
(`/1/amount` rather than a bare `/amount`). Return the `*json.SemanticError`
unchanged from the decode path so callers can `errors.As` it and read the offset;
wrapping it with `fmt.Errorf("...: %w", err)` also preserves it, but discarding it
for a generic message throws away the coordinate. Confirm the sentinel path with
`errors.Is(err, ErrTooManyElements)` and run `go test -race` to check the pipe
goroutine in the large-stream test.

## Resources

- [`encoding/json/v2`](https://pkg.go.dev/encoding/json/v2) — `UnmarshalDecode`, `SemanticError`, and the option set.
- [`encoding/json/jsontext`](https://pkg.go.dev/encoding/json/jsontext) — `Decoder`, `ReadToken`, `PeekKind`, `InputOffset`, and `Kind`.
- [A new experimental Go API for JSON](https://go.dev/blog/jsonv2-exp) — the design rationale, including streaming and the layer split.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-jsontext-token-rewriting.md](02-jsontext-token-rewriting.md)
