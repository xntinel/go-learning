# 4. Streaming JSON

Streaming JSON is for data that arrives as a sequence: NDJSON logs, sockets, pipes, and large arrays. The hard part is not calling `Decode`; it is defining the stream boundary, stopping cleanly at `io.EOF`, and rejecting malformed input without reading the whole payload into memory.

## Concepts

### Values, Streams, and Boundaries

`json.Marshal` and `json.Unmarshal` work on complete byte slices. `json.NewEncoder` and `json.NewDecoder` work on `io.Writer` and `io.Reader`, so the code can process one JSON value at a time. `Encoder.Encode` writes one JSON value followed by a newline, which makes it a natural fit for NDJSON.

### EOF Is Not a Decode Failure

A decoder loop normally ends with `io.EOF`. Treating EOF as a bad record turns a successful stream into a failure. Treating every other error as recoverable is also wrong: a malformed object may leave the decoder in a position where the next read is meaningless.

### Token Walking Is Structural

`Decoder.Token` exposes delimiters, object keys, strings, numbers, booleans, and nulls. `Decoder.More` only answers whether the current array or object has another element, so it must be called after the decoder has consumed the opening delimiter of that container.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/streamjson/cmd/demo
cd ~/go-exercises/streamjson
go mod init example.com/streamjson
go mod edit -go=1.26
```

This is a library package. The demo is only a consumer of the exported API; tests are the verification.

### Exercise 1: Encode and Decode NDJSON

Create `streamjson.go`:

```go
package streamjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	ErrEmptyLevel   = errors.New("level must not be empty")
	ErrEmptyMessage = errors.New("message must not be empty")
	ErrBadResults   = errors.New("results payload is malformed")
)

type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
}

func (e LogEntry) Time() time.Time  { return e.Timestamp }
func (e LogEntry) Severity() string { return e.Level }
func (e LogEntry) Text() string     { return e.Message }

func EncodeLogs(w io.Writer, entries []LogEntry) error {
	enc := json.NewEncoder(w)
	for _, entry := range entries {
		if entry.Level == "" {
			return fmt.Errorf("encode log: %w", ErrEmptyLevel)
		}
		if entry.Message == "" {
			return fmt.Errorf("encode log: %w", ErrEmptyMessage)
		}
		if err := enc.Encode(entry); err != nil {
			return fmt.Errorf("encode log: %w", err)
		}
	}
	return nil
}

func DecodeLogs(r io.Reader) ([]LogEntry, error) {
	dec := json.NewDecoder(r)
	var entries []LogEntry
	for {
		var entry LogEntry
		err := dec.Decode(&entry)
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode log: %w", err)
		}
		if entry.Level == "" {
			return nil, fmt.Errorf("decode log: %w", ErrEmptyLevel)
		}
		if entry.Message == "" {
			return nil, fmt.Errorf("decode log: %w", ErrEmptyMessage)
		}
		entries = append(entries, entry)
	}
}

type Item struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func (i Item) Identifier() int { return i.ID }
func (i Item) Label() string   { return i.Name }

func DecodeResults(r io.Reader) ([]Item, error) {
	dec := json.NewDecoder(r)

	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		if err != nil {
			return nil, fmt.Errorf("decode results: %w", err)
		}
		return nil, fmt.Errorf("decode results: %w", ErrBadResults)
	}
	key, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("decode results: %w", err)
	}
	if key != "results" {
		return nil, fmt.Errorf("decode results: %w", ErrBadResults)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		if err != nil {
			return nil, fmt.Errorf("decode results: %w", err)
		}
		return nil, fmt.Errorf("decode results: %w", ErrBadResults)
	}

	var items []Item
	for dec.More() {
		var item Item
		if err := dec.Decode(&item); err != nil {
			return nil, fmt.Errorf("decode results item: %w", err)
		}
		items = append(items, item)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
		if err != nil {
			return nil, fmt.Errorf("decode results: %w", err)
		}
		return nil, fmt.Errorf("decode results: %w", ErrBadResults)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		if err != nil {
			return nil, fmt.Errorf("decode results: %w", err)
		}
		return nil, fmt.Errorf("decode results: %w", ErrBadResults)
	}
	return items, nil
}
```

### Exercise 2: Walk a Nested Array Without Loading It Whole

The `DecodeResults` function in `streamjson.go` walks the `results` array with `Token`, `More`, and `Decode`. Read it carefully before writing the tests below.

### Exercise 3: Test the Stream Contract

Create `streamjson_test.go`:

```go
package streamjson

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEncodeDecodeLogsRoundTrip(t *testing.T) {
	t.Parallel()

	entries := []LogEntry{{Timestamp: time.Unix(10, 0).UTC(), Level: "info", Message: "started"}}
	var buf bytes.Buffer
	if err := EncodeLogs(&buf, entries); err != nil {
		t.Fatalf("EncodeLogs() error = %v", err)
	}
	got, err := DecodeLogs(&buf)
	if err != nil {
		t.Fatalf("DecodeLogs() error = %v", err)
	}
	if len(got) != 1 || got[0].Level != "info" || got[0].Message != "started" {
		t.Fatalf("decoded logs = %+v", got)
	}
}

func TestEncodeLogsRejectsInvalidEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   LogEntry
		want error
	}{
		{name: "empty level", in: LogEntry{Message: "x"}, want: ErrEmptyLevel},
		{name: "empty message", in: LogEntry{Level: "info"}, want: ErrEmptyMessage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := EncodeLogs(ioDiscard{}, []LogEntry{tc.in})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDecodeResults(t *testing.T) {
	t.Parallel()

	items, err := DecodeResults(strings.NewReader(`{"results":[{"id":1,"name":"alpha"},{"id":2,"name":"beta"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[1].Name != "beta" {
		t.Fatalf("items = %+v", items)
	}
}

func TestDecodeResultsRejectsMalformedShape(t *testing.T) {
	t.Parallel()

	_, err := DecodeResults(strings.NewReader(`[]`))
	if !errors.Is(err, ErrBadResults) {
		t.Fatalf("err = %v, want ErrBadResults", err)
	}
}

func ExampleDecodeResults() {
	items, _ := DecodeResults(strings.NewReader(`{"results":[{"id":1,"name":"alpha"},{"id":2,"name":"beta"}]}`))
	for _, item := range items {
		fmt.Printf("%d:%s\n", item.Identifier(), item.Label())
	}
	// Output:
	// 1:alpha
	// 2:beta
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
```

Your turn: add a table row proving `DecodeLogs` returns `ErrEmptyMessage` when the NDJSON object has no `message` field.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"time"

	"example.com/streamjson"
)

func main() {
	entries := []streamjson.LogEntry{
		{Timestamp: time.Unix(10, 0).UTC(), Level: "info", Message: "started"},
		{Timestamp: time.Unix(11, 0).UTC(), Level: "warn", Message: "slow request"},
	}
	var buf bytes.Buffer
	if err := streamjson.EncodeLogs(&buf, entries); err != nil {
		log.Fatal(err)
	}
	decoded, err := streamjson.DecodeLogs(&buf)
	if err != nil {
		log.Fatal(err)
	}
	items, err := streamjson.DecodeResults(strings.NewReader(`{"results":[{"id":1,"name":"alpha"},{"id":2,"name":"beta"}]}`))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("logs=%d first=%s items=%d\n", len(decoded), decoded[0].Severity(), len(items))
}
```

## Common Mistakes

- Wrong: breaking the decoder loop on any error and returning partial data. What happens: malformed input looks like a successful short stream. Fix: only `io.EOF` ends the stream; all other errors are returned.
- Wrong: calling `More` before entering an array or object. What happens: it answers for the wrong parser state. Fix: consume the opening delimiter with `Token` first.
- Wrong: using `json.Unmarshal` on the whole NDJSON file. What happens: it fails because NDJSON is multiple JSON values, not one array. Fix: use a `Decoder` loop.

## Verification

From `~/go-exercises/streamjson`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All commands must pass. Add at least one test of your own before considering the lesson complete.

## Summary

- `Encoder.Encode` writes one JSON value and a newline, which fits NDJSON.
- `Decoder.Decode` can read consecutive JSON values from one stream.
- `io.EOF` is the successful end of a decoder loop.
- `Token` and `More` let you process nested arrays without decoding the entire document.

## What's Next

Next: [Handling Unknown JSON Fields](../05-handling-unknown-json-fields/05-handling-unknown-json-fields.md).

## Resources

- [encoding/json package documentation](https://pkg.go.dev/encoding/json)
- [json.Decoder.Token documentation](https://pkg.go.dev/encoding/json#Decoder.Token)
- [json.Decoder.More documentation](https://pkg.go.dev/encoding/json#Decoder.More)
- [Go blog: JSON and Go](https://go.dev/blog/json)
