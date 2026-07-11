# Exercise 24: Streaming NDJSON Parser with Per-Line Recovery and Error Breadcrumbs

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Newline-delimited JSON is the workhorse format for log shipping and event
streams: one record per line, parsed as the stream is read rather than all
at once. A malformed record deep in a multi-gigabyte stream must not abort
the whole parse — but the operator debugging a data-quality incident needs
more than "some line failed": they need the exact line number, the byte
offset into the stream, and the raw text, so the bad record can be found and
fixed at the source. This module builds `ParseStream`, which decodes each
line under its own recover boundary and leaves an exact breadcrumb — line,
offset, raw text, and the original error — for every record that failed,
whether from a JSON syntax error or from a decoder that panics on a missing
field. It is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
streamjson/                   independent module: example.com/streamjson
  go.mod                      go 1.24
  streamjson.go                Event, BadRecord, Result, DefaultDecode, ParseStream, decodeOne
  cmd/
    demo/
      main.go                 runnable demo: 5 lines, 3 malformed in different ways
  streamjson_test.go            breadcrumb table (line/offset/raw), all-valid, empty
```

Files: `streamjson.go`, `cmd/demo/main.go`, `streamjson_test.go`.
Implement: `ParseStream(r io.Reader, decode func([]byte) (Event, error)) Result` that scans line by line, tracks the byte offset of each line's start, and isolates `decode`'s panic per line via `decodeOne`; `DefaultDecode`, a deliberately unsafe decoder that demonstrates the exact bug this package defends against (a bare type assertion on a possibly-missing JSON field).
Test: a five-line stream mixing one JSON-syntax error (an ordinary decode error, not a panic) with two missing-field panics and two valid records; assert exactly 2 events decoded correctly, exactly 3 `BadRecord`s with the correct line numbers, correctly increasing byte offsets matching the stream's actual layout, and the raw offending line preserved verbatim; an all-valid stream and an empty stream.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/streaming-json-recovery/cmd/demo
cd ~/go-exercises/streaming-json-recovery
go mod init example.com/streamjson
go mod edit -go=1.24
```

### Why DefaultDecode panics instead of returning an error, and how the offset is tracked without seeking

`DefaultDecode` is written deliberately unsafely: it unmarshals a line into
`map[string]any` and then does `fields["type"].(string)` with no comma-ok
check. This is not a contrived toy bug — it is the single most common way a
"just JSON-decode it" handler ships broken, because `encoding/json` itself
never panics on malformed input (`json.Unmarshal` always returns a clean
`error`), but the type assertion that follows a successful-but-incomplete
unmarshal absolutely does: a missing `"type"` field means `fields["type"]`
is a `nil` interface, and `nil.(string)` panics with "interface conversion:
interface {} is nil, not string" instead of surfacing as a decode error.
`ParseStream` has to defend against both failure modes at once — an
ordinary JSON syntax error *and* a panicking post-processing step — which is
why `decodeOne`'s recover boundary sits around the entire caller-supplied
`decode` call, not just around `json.Unmarshal`.

The byte offset is computed incrementally as the stream is read, not by
seeking: `ParseStream` keeps a running `offset` that starts at zero and, after
each line, advances by `len(raw) + 1` — the line's byte length plus the
newline `bufio.Scanner` stripped off and does not report. Recording
`startOffset` *before* advancing (not after) is what makes `BadRecord.Offset`
point at where the bad line actually begins in the stream, which is exactly
the position a human would need to seek to in the raw file to find it. The
scanner's returned byte slice is copied (`append([]byte(nil), raw...)`)
before being handed to `decode` or stored in a `BadRecord`, because
`bufio.Scanner` reuses its internal buffer on the next `Scan()` call — storing
the slice directly would let a later line silently overwrite an earlier
line's supposedly-preserved breadcrumb.

Create `streamjson.go`:

```go
package streamjson

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// Event is one decoded record from the stream.
type Event struct {
	Type    string
	Payload string
}

// BadRecord is a breadcrumb for one line that failed to decode, whether
// through an ordinary error or a panic.
type BadRecord struct {
	Line   int
	Offset int64
	Raw    string
	Err    error
}

// Result is everything ParseStream produced from one stream.
type Result struct {
	Events []Event
	Bad    []BadRecord
}

// DefaultDecode is a deliberately fragile decoder: it parses one line of
// JSON into a generic map and then reaches into required fields with a bare
// type assertion, no comma-ok. That is a realistic bug — the exact bug this
// package defends callers against — because a missing "type" or "payload"
// field turns fields["type"] into a nil interface, and asserting .(string)
// on it panics with "interface conversion: interface {} is nil, not
// string" instead of returning an error.
func DefaultDecode(raw []byte) (Event, error) {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Event{}, fmt.Errorf("invalid json: %w", err)
	}
	return Event{
		Type:    fields["type"].(string),
		Payload: fields["payload"].(string),
	}, nil
}

// ParseStream reads newline-delimited JSON records from r, decoding each
// with decode. A malformed record's panic — bad JSON handled as an error,
// but a missing or wrong-typed required field panicking instead — is
// isolated per line: recovery captures the 1-based line number and the
// byte offset where that line started in the stream, records a BadRecord
// breadcrumb carrying the raw line and the original error, and parsing
// continues from the next line. Events already collected are never
// touched by a later line's failure, so one poison record cannot corrupt
// or truncate the events collected from the rest of the stream.
func ParseStream(r io.Reader, decode func(raw []byte) (Event, error)) Result {
	scanner := bufio.NewScanner(r)
	var result Result
	var offset int64
	line := 0

	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		lineCopy := append([]byte(nil), raw...) // Scanner reuses its buffer between calls
		startOffset := offset
		offset += int64(len(raw)) + 1 // +1 for the newline the scanner stripped

		ev, err := decodeOne(lineCopy, decode)
		if err != nil {
			result.Bad = append(result.Bad, BadRecord{
				Line:   line,
				Offset: startOffset,
				Raw:    string(lineCopy),
				Err:    err,
			})
			continue
		}
		result.Events = append(result.Events, ev)
	}
	return result
}

// decodeOne is the recover boundary: exactly one line's worth of untrusted
// decode logic.
func decodeOne(raw []byte, decode func([]byte) (Event, error)) (ev Event, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = fmt.Errorf("decode panicked: %w", e)
				return
			}
			err = fmt.Errorf("decode panicked: %v", r)
		}
	}()
	return decode(raw)
}
```

### The runnable demo

Five lines: two valid, one with a missing `payload` field (panics), one that
is not JSON at all (an ordinary decode error), and one with a missing
`type` field (panics).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/streamjson"
)

func main() {
	input := strings.Join([]string{
		`{"type":"login","payload":"user1"}`,
		`{"type":"login"}`,
		`not json at all`,
		`{"type":"logout","payload":"user2"}`,
		`{"payload":"x"}`,
	}, "\n") + "\n"

	result := streamjson.ParseStream(strings.NewReader(input), streamjson.DefaultDecode)

	fmt.Printf("events: %d\n", len(result.Events))
	for _, e := range result.Events {
		fmt.Printf("  %s: %s\n", e.Type, e.Payload)
	}
	fmt.Printf("bad records: %d\n", len(result.Bad))
	for _, b := range result.Bad {
		fmt.Printf("  line %d offset %d: %v\n", b.Line, b.Offset, b.Err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
events: 2
  login: user1
  logout: user2
bad records: 3
  line 2 offset 35: decode panicked: interface conversion: interface {} is nil, not string
  line 3 offset 52: invalid json: invalid character 'o' in literal null (expecting 'u')
  line 5 offset 104: decode panicked: interface conversion: interface {} is nil, not string
```

### Tests

`TestParseStreamIsolatesBadRecords` drives the same five-line mix as the
demo, asserting the two valid events decoded correctly, exactly three bad
records at the right line numbers, that the JSON-syntax failure (line 3) is
distinguishable from the two panics (lines 2 and 5) by message content, that
the raw offending text is preserved verbatim, and that every recorded offset
matches the stream's actual byte layout computed independently in the test.
`TestParseStreamAllValid` and `TestParseStreamEmpty` cover the boundary
cases.

Create `streamjson_test.go`:

```go
package streamjson

import (
	"strings"
	"testing"
)

func TestParseStreamIsolatesBadRecords(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"login","payload":"user1"}`,
		`{"type":"login"}`,
		`not json at all`,
		`{"type":"logout","payload":"user2"}`,
		`{"payload":"x"}`,
	}, "\n") + "\n"

	result := ParseStream(strings.NewReader(input), DefaultDecode)

	if len(result.Events) != 2 {
		t.Fatalf("len(Events) = %d, want 2", len(result.Events))
	}
	if result.Events[0] != (Event{Type: "login", Payload: "user1"}) {
		t.Fatalf("Events[0] = %+v, want login/user1", result.Events[0])
	}
	if result.Events[1] != (Event{Type: "logout", Payload: "user2"}) {
		t.Fatalf("Events[1] = %+v, want logout/user2", result.Events[1])
	}

	if len(result.Bad) != 3 {
		t.Fatalf("len(Bad) = %d, want 3", len(result.Bad))
	}
	wantLines := []int{2, 3, 5}
	for i, want := range wantLines {
		if result.Bad[i].Line != want {
			t.Fatalf("Bad[%d].Line = %d, want %d", i, result.Bad[i].Line, want)
		}
	}
	if !strings.Contains(result.Bad[0].Err.Error(), "decode panicked") {
		t.Fatalf("Bad[0].Err = %v, want a panic breadcrumb (missing payload)", result.Bad[0].Err)
	}
	if strings.Contains(result.Bad[1].Err.Error(), "decode panicked") {
		t.Fatalf("Bad[1].Err = %v, want an ordinary json error, not a panic", result.Bad[1].Err)
	}
	if !strings.Contains(result.Bad[2].Err.Error(), "decode panicked") {
		t.Fatalf("Bad[2].Err = %v, want a panic breadcrumb (missing type)", result.Bad[2].Err)
	}

	if result.Bad[0].Raw != `{"type":"login"}` {
		t.Fatalf("Bad[0].Raw = %q, want the raw offending line", result.Bad[0].Raw)
	}

	// Offsets must strictly increase and match the byte position where
	// each bad line started in the stream.
	lines := strings.Split(strings.TrimSuffix(input, "\n"), "\n")
	var expectedOffset int64
	offsetByLine := make(map[int]int64, len(lines))
	for i, l := range lines {
		offsetByLine[i+1] = expectedOffset
		expectedOffset += int64(len(l)) + 1
	}
	for _, bad := range result.Bad {
		if bad.Offset != offsetByLine[bad.Line] {
			t.Fatalf("Bad line %d Offset = %d, want %d", bad.Line, bad.Offset, offsetByLine[bad.Line])
		}
	}
}

func TestParseStreamAllValid(t *testing.T) {
	input := `{"type":"a","payload":"1"}` + "\n" + `{"type":"b","payload":"2"}` + "\n"
	result := ParseStream(strings.NewReader(input), DefaultDecode)
	if len(result.Events) != 2 || len(result.Bad) != 0 {
		t.Fatalf("result = %+v, want 2 clean events and no bad records", result)
	}
}

func TestParseStreamEmpty(t *testing.T) {
	result := ParseStream(strings.NewReader(""), DefaultDecode)
	if len(result.Events) != 0 || len(result.Bad) != 0 {
		t.Fatalf("result = %+v, want both empty", result)
	}
}
```

## Review

`ParseStream` is correct when a malformed line — whichever way it fails —
never stops later, valid lines from being decoded, and when every
`BadRecord` carries enough context (line, offset, raw text) that an
operator could locate and fix the offending record without re-running the
parse with extra instrumentation. The recover boundary in `decodeOne` has to
wrap the entire caller-supplied `decode` call, not just the JSON unmarshal
step, because the realistic failure this module defends against —
`DefaultDecode`'s bare type assertions — happens *after* `json.Unmarshal`
already succeeded. Copying the scanner's byte slice before storing or
decoding it is easy to skip and easy to get wrong silently: `bufio.Scanner`
reusing its buffer means an uncopied `BadRecord.Raw` would end up holding
whatever the *last* line scanned happened to be, not the line it was
actually recorded for.

## Resources

- [bufio.Scanner](https://pkg.go.dev/bufio#Scanner) — line-oriented streaming reads, and why its returned buffer must be copied before it is retained.
- [encoding/json: Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal) — decoding into `map[string]any`, and why a missing field yields `nil`, not a decode error.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the per-line recover boundary this streaming parser relies on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-worker-pool-supervisor.md](23-worker-pool-supervisor.md) | Next: [25-mutex-critical-section-panic.md](25-mutex-critical-section-panic.md)
