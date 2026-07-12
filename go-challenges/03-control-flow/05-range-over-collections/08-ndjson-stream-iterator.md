# Exercise 8: Streaming NDJSON Decoder Exposed as an Iterator

Ingest pipelines love newline-delimited JSON: one event per line, streamed from a
file, an HTTP body, or a message payload. This module exposes that stream from an
`io.Reader` as an `iter.Seq2[Event, error]`, so callers write
`for ev, err := range Decode(r)` and stop on the first error with `break`. The
iterator uses `bufio.Scanner` internally, yields decode errors instead of
panicking, and honors early termination.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
ndjson/                     independent module: example.com/ndjson
  go.mod                    go 1.24
  ndjson.go                 Event; Decode(r io.Reader) iter.Seq2[Event, error]
  cmd/
    demo/
      main.go               runnable demo: decode a multi-line NDJSON string
  ndjson_test.go            golden decode, malformed-line-midstream, early break, empty input
```

- Files: `ndjson.go`, `cmd/demo/main.go`, `ndjson_test.go`.
- Implement: `Decode(r io.Reader) iter.Seq2[Event, error]` yielding one `Event` per line, a zero `Event` plus the error on a bad line or scan error, honoring `break`.
- Test: a golden NDJSON string decodes to the expected slice; a malformed line mid-stream is observed after the good events; early break stops iteration; empty input yields zero iterations.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### An iterator over lines that yields errors instead of panicking

`Decode` returns a `func(yield func(Event, error) bool)`. Inside, a
`bufio.Scanner` splits the reader into lines. For each non-blank line it calls
`json.Unmarshal` into an `Event`; on success it yields `(event, nil)`, on failure it
yields `(zeroEvent, err)` and stops — a malformed record ends the stream rather than
silently skipping, which is the safe default for ingest (you want to know a producer
sent garbage, not lose it). After the loop, if the scanner itself failed (an I/O
error on the underlying reader, or a line longer than the buffer), it yields that
error too.

The two rules that make this a correct iterator are the same as always. Every
`yield` return is checked: `if !yield(ev, nil) { return }` so a caller's `break`
actually stops scanning, and any cleanup (here there is none beyond letting the
scanner be garbage-collected, but a file-backed version would `defer f.Close()`
inside `Decode`) runs. And errors travel as the second range value, never as a
panic, so the caller handles them with the `if err != nil` it already writes.

Blank lines are skipped so a trailing newline does not produce a spurious empty
event. Because errors terminate the stream, the caller sees every good event that
preceded the bad line, then the error, then the loop ends.

Create `ndjson.go`:

```go
package ndjson

import (
	"bufio"
	"encoding/json"
	"io"
	"iter"
	"strings"
)

// Event is one decoded NDJSON record.
type Event struct {
	Type string `json:"type"`
	ID   int    `json:"id"`
}

// Decode returns a push iterator over the newline-delimited JSON in r. Each line
// yields (event, nil); a malformed line or scan error yields (zero, err) and ends
// the stream. Blank lines are skipped. The caller may stop early with break.
func Decode(r io.Reader) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				yield(Event{}, err)
				return
			}
			if !yield(ev, nil) {
				return // caller did break
			}
		}
		if err := sc.Err(); err != nil {
			yield(Event{}, err)
		}
	}
}
```

### The runnable demo

The demo decodes a three-line NDJSON string and prints each event.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/ndjson"
)

func main() {
	const stream = `{"type":"login","id":1}
{"type":"click","id":2}
{"type":"logout","id":3}
`
	for ev, err := range ndjson.Decode(strings.NewReader(stream)) {
		if err != nil {
			fmt.Println("error:", err)
			break
		}
		fmt.Printf("%s#%d\n", ev.Type, ev.ID)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
login#1
click#2
logout#3
```

### Tests

The golden test decodes a clean stream and asserts the exact event slice. The
malformed test puts a bad line between two good ones and asserts the two good events
arrive, then the error, and iteration ends. The early-break test breaks after the
first event and asserts only one was collected. The empty-input test asserts zero
iterations.

Create `ndjson_test.go`:

```go
package ndjson

import (
	"strings"
	"testing"
)

func TestDecodeGolden(t *testing.T) {
	t.Parallel()
	const stream = `{"type":"login","id":1}
{"type":"click","id":2}

{"type":"logout","id":3}
`
	var got []Event
	for ev, err := range Decode(strings.NewReader(stream)) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got = append(got, ev)
	}
	want := []Event{
		{Type: "login", ID: 1},
		{Type: "click", ID: 2},
		{Type: "logout", ID: 3},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDecodeStopsOnMalformedLine(t *testing.T) {
	t.Parallel()
	const stream = `{"type":"a","id":1}
{"type":"b","id":2}
{not json}
{"type":"c","id":3}
`
	var got []Event
	var sawErr error
	for ev, err := range Decode(strings.NewReader(stream)) {
		if err != nil {
			sawErr = err
			break
		}
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("events before bad line = %d, want 2: %+v", len(got), got)
	}
	if sawErr == nil {
		t.Fatal("expected a decode error on the malformed line")
	}
}

func TestDecodeEarlyBreak(t *testing.T) {
	t.Parallel()
	const stream = `{"type":"a","id":1}
{"type":"b","id":2}
{"type":"c","id":3}
`
	count := 0
	for _, err := range Decode(strings.NewReader(stream)) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		count++
		break
	}
	if count != 1 {
		t.Fatalf("collected %d events after break, want 1", count)
	}
}

func TestDecodeEmptyInput(t *testing.T) {
	t.Parallel()
	count := 0
	for range Decode(strings.NewReader("")) {
		count++
	}
	if count != 0 {
		t.Fatalf("empty input produced %d iterations, want 0", count)
	}
}
```

## Review

The decoder is correct when a clean stream yields every event in order, a malformed
line surfaces as the second range value after the events that preceded it, and a
caller's `break` stops the scan. The dangerous shortcut is to `panic` or
`log.Fatal` on a bad line inside the iterator — that takes down the caller's control
flow; yielding the error keeps the caller in charge. The other classic omission is
ignoring `yield`'s return so `break` does not stop scanning; the early-break test
pins that only one event is consumed. A file-backed `Decode` would `defer f.Close()`
inside the returned function so the file is closed whether the caller drains the
stream or breaks out of it.

## Resources

- [package iter (Seq2)](https://pkg.go.dev/iter#Seq2)
- [bufio.Scanner](https://pkg.go.dev/bufio#Scanner)
- [encoding/json Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal)
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-record-batch-inplace-update.md](07-record-batch-inplace-update.md) | Next: [09-log-line-rune-scan.md](09-log-line-rune-scan.md)
