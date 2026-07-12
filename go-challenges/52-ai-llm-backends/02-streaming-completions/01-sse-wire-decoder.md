# Exercise 1: A Spec-Compliant Server-Sent Events Decoder

Before you can consume any LLM stream you have to parse the wire it arrives on.
This exercise builds a dependency-free Server-Sent Events decoder that reads a
`text/event-stream` body from any `io.Reader` and yields structured events. It is
exactly what an SDK's internal `ssestream` package does; building it once makes
the format concrete and gives you a reusable reader for any OpenAI-compatible or
Anthropic endpoint.

This module is fully self-contained. It has its own `go mod init`, uses only the
standard library, and ships its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
ssedecode/                  independent module: example.com/ssedecode
  go.mod                    go 1.26
  ssedecode.go              type Event; type Decoder; NewDecoder; (*Decoder).All iter.Seq2[Event,error]; const Done
  cmd/
    demo/
      main.go               decode a canned Anthropic-style stream and print each event
  ssedecode_test.go         table-driven decoding: joins, comments, CRLF/CR, BOM, [DONE], split reads, huge lines
```

- Files: `ssedecode.go`, `cmd/demo/main.go`, `ssedecode_test.go`.
- Implement: an `Event{Name, Data, ID, Retry}`, a `Decoder` over an `io.Reader`, and an `All()` iterator (`iter.Seq2[Event, error]`) that joins multi-line `data`, skips `:` comments, handles CRLF/LF/CR and a leading BOM, raises the scanner buffer for large lines, and stops on the `[DONE]` sentinel.
- Test: feed crafted byte streams via `strings.NewReader` / `iotest.OneByteReader`; assert joins, comment skipping, named events, line-ending variants, BOM stripping, the terminator, event-split-across-reads, and a data line larger than the default 64 KiB scanner buffer.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The parsing model

SSE is a line protocol with a tiny state machine. You accumulate fields until a
blank line, then dispatch one event and reset. The subtleties are all in the
lexer and the field rules, so we split the work: a custom `bufio.SplitFunc`
turns the byte stream into lines regardless of whether they end in LF, CRLF, or a
lone CR, and the iterator applies the field rules on top of those lines.

Why a custom split function rather than `bufio.ScanLines`? Two reasons.
`bufio.ScanLines` only recognizes `\n` (stripping a trailing `\r`), but the SSE
spec allows a bare `\r` as a line terminator, which a strict provider or a proxy
rewrite can produce. And we need to raise the token size limit: `bufio.Scanner`
defaults to a 64 KiB maximum token, and a single `data:` line carrying a large
JSON payload — a base64 image, a long tool-call argument — blows past that with
`bufio.ErrTooLong`. `Scanner.Buffer(buf, max)` raises the ceiling; here we set it
to 1 MiB.

The split function returns `(0, nil, nil)` to mean "give me more input" — that is
the mechanism that lets a single event span multiple `Read` calls. The scanner
keeps buffering until the function can produce a whole line. That is why a stream
delivered one byte at a time decodes identically to one delivered in a single
read; the test exercises exactly that with `iotest.OneByteReader`.

### The field rules

Once we have lines, the rules are mechanical. A blank line dispatches. A line
starting with `:` is a comment and is skipped — this is the heartbeat channel. For
any other line we `strings.Cut` on the first colon: the part before is the field
name, the part after (with one leading space stripped) is the value; a line with
no colon is a field name with an empty value. `data` appends the value plus a
newline to the data buffer; `event` sets the event name; `id` sets the last event
id (ignored if it contains a NUL, per spec); `retry` sets the reconnection time
if the value is all ASCII digits. Every other field name is ignored.

On dispatch, if the data buffer is empty we reset the event name and continue
without emitting — this is how stray blank lines and comment-only keepalives do
not produce phantom events. Otherwise we strip the single trailing newline the
`data` rule added and emit the event. The `id` value persists across events,
matching the spec's "last event id" behavior. The spec instead treats `retry` as
a connection-level reconnection time, not a per-event value carried on each
dispatched event; persisting it onto every `Event` here is a deliberate
convenience of this decoder — it lets a caller read the most recent reconnection
hint off any event — rather than something the spec mandates.

The `[DONE]` sentinel is handled here, at the boundary: an event whose data is
exactly `[DONE]` is a terminator, so the iterator stops cleanly without yielding
it. A caller never has to special-case it, and never accidentally feeds it to a
JSON decoder.

Create `ssedecode.go`:

```go
package ssedecode

import (
	"bufio"
	"io"
	"iter"
	"strconv"
	"strings"
)

// Done is the sentinel data payload OpenAI-compatible endpoints send to mark the
// end of a stream. The decoder treats it as a terminator, not as an event.
const Done = "[DONE]"

// Event is one dispatched Server-Sent Event. Name is the event type ("message"
// is the SSE default but providers set their own), Data is the joined data
// payload, and ID/Retry carry the most recent id and reconnection time seen.
type Event struct {
	Name  string
	Data  string
	ID    string
	Retry int
}

// Decoder reads a text/event-stream body and yields structured events. It is
// safe to use with any io.Reader; the scanner buffers across Read calls.
type Decoder struct {
	sc *bufio.Scanner
}

// NewDecoder wraps r with a scanner whose token limit is raised to 1 MiB (large
// data lines exceed the default 64 KiB) and whose split honors LF, CRLF, and CR.
func NewDecoder(r io.Reader) *Decoder {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	sc.Split(scanEventLines)
	return &Decoder{sc: sc}
}

// scanEventLines splits on LF, CRLF, or a lone CR, returning each line without
// its terminator. Returning (0, nil, nil) asks the scanner for more input, which
// is what lets one event span multiple Read calls.
func scanEventLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\n':
			return i + 1, data[:i], nil
		case '\r':
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if atEOF {
				return i + 1, data[:i], nil
			}
			// A trailing CR at a buffer boundary might be part of a CRLF; wait
			// for the next byte before deciding.
			return 0, nil, nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// All returns an iterator over the events in the stream. Iteration ends on EOF,
// on the [DONE] terminator, or on a read error (yielded as a final error). A
// caller stops early by returning false from the range body; the scanner is not
// advanced further.
func (d *Decoder) All() iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		var (
			data   strings.Builder
			name   string
			lastID string
			retry  int
			first  = true
		)

		dispatch := func() bool {
			if data.Len() == 0 {
				name = ""
				return true
			}
			payload := strings.TrimSuffix(data.String(), "\n")
			data.Reset()
			ev := Event{Name: name, Data: payload, ID: lastID, Retry: retry}
			name = ""
			if payload == Done {
				return false
			}
			return yield(ev, nil)
		}

		for d.sc.Scan() {
			line := d.sc.Text()
			if first {
				line = strings.TrimPrefix(line, "\ufeff")
				first = false
			}
			switch {
			case line == "":
				if !dispatch() {
					return
				}
			case strings.HasPrefix(line, ":"):
				// Comment / heartbeat line: ignored per spec.
			default:
				field, value, found := strings.Cut(line, ":")
				if found {
					value = strings.TrimPrefix(value, " ")
				}
				switch field {
				case "data":
					data.WriteString(value)
					data.WriteByte('\n')
				case "event":
					name = value
				case "id":
					if !strings.ContainsRune(value, 0) {
						lastID = value
					}
				case "retry":
					if n, ok := parseRetry(value); ok {
						retry = n
					}
				default:
					// Unknown field: ignored per spec.
				}
			}
		}
		if err := d.sc.Err(); err != nil {
			yield(Event{}, err)
			return
		}
		// A stream that ends without a final blank line leaves an incomplete
		// event in the buffers; the spec discards it, so there is nothing to do.
	}
}

// parseRetry accepts only a run of ASCII digits, as the SSE spec requires for the
// retry field; anything else leaves the reconnection time unchanged.
func parseRetry(v string) (int, bool) {
	if v == "" {
		return 0, false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}
```

### The runnable demo

The demo feeds a canned Anthropic-style stream through the decoder and prints
each dispatched event. Note the `: ping` heartbeat is skipped and the trailing
`data: [DONE]` ends iteration without producing an event.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"strings"

	"example.com/ssedecode"
)

const stream = "event: message_start\n" +
	"data: {\"type\":\"message_start\"}\n" +
	"\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"text_delta\",\"text\":\"Hello\"}\n" +
	"\n" +
	": ping\n" +
	"\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"text_delta\",\"text\":\", world\"}\n" +
	"\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n" +
	"\n" +
	"data: [DONE]\n" +
	"\n"

func main() {
	dec := ssedecode.NewDecoder(strings.NewReader(stream))
	for ev, err := range dec.All() {
		if err != nil {
			log.Fatalf("decode: %v", err)
		}
		fmt.Printf("event=%s data=%s\n", ev.Name, ev.Data)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
event=message_start data={"type":"message_start"}
event=content_block_delta data={"type":"text_delta","text":"Hello"}
event=content_block_delta data={"type":"text_delta","text":", world"}
event=message_stop data={"type":"message_stop"}
```

### Tests

The tests are fully offline. Each case is a crafted raw byte string fed through
the decoder; the table asserts the exact sequence of events. `collect` drains the
iterator into a slice and surfaces any error. The interesting rows are the
multi-line join, the comment skip, the CRLF and lone-CR line endings, the leading
BOM, and the `[DONE]` terminator that must not appear as an event.
`TestSplitAcrossReads` runs the same input through `iotest.OneByteReader` so the
scanner has to reassemble events across single-byte reads, and `TestHugeDataLine`
proves the raised buffer handles a data line far larger than 64 KiB without
`bufio.ErrTooLong`.

Create `ssedecode_test.go`:

```go
package ssedecode

import (
	"fmt"
	"strings"
	"testing"
	"testing/iotest"
)

func collect(t *testing.T, r *strings.Reader) []Event {
	t.Helper()
	var got []Event
	for ev, err := range NewDecoder(r).All() {
		if err != nil {
			t.Fatalf("unexpected decode error: %v", err)
		}
		got = append(got, ev)
	}
	return got
}

func TestDecode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []Event
	}{
		{
			name: "single event",
			in:   "data: hello\n\n",
			want: []Event{{Data: "hello"}},
		},
		{
			name: "joined data lines",
			in:   "data: a\ndata: b\n\n",
			want: []Event{{Data: "a\nb"}},
		},
		{
			name: "comment skipped",
			in:   ": keepalive\ndata: x\n\n",
			want: []Event{{Data: "x"}},
		},
		{
			name: "named event",
			in:   "event: content_block_delta\ndata: {}\n\n",
			want: []Event{{Name: "content_block_delta", Data: "{}"}},
		},
		{
			name: "no space after colon",
			in:   "data:tight\n\n",
			want: []Event{{Data: "tight"}},
		},
		{
			name: "crlf line endings",
			in:   "data: x\r\n\r\n",
			want: []Event{{Data: "x"}},
		},
		{
			name: "lone cr line endings",
			in:   "data: x\rdata: y\r\r",
			want: []Event{{Data: "x\ny"}},
		},
		{
			name: "leading bom",
			in:   "\ufeffdata: x\n\n",
			want: []Event{{Data: "x"}},
		},
		{
			name: "id and retry persist",
			in:   "id: 42\nretry: 3000\ndata: one\n\ndata: two\n\n",
			want: []Event{{Data: "one", ID: "42", Retry: 3000}, {Data: "two", ID: "42", Retry: 3000}},
		},
		{
			name: "done terminates before yielding",
			in:   "data: hello\n\ndata: [DONE]\n\ndata: never\n\n",
			want: []Event{{Data: "hello"}},
		},
		{
			name: "blank lines produce no phantom events",
			in:   "\n\ndata: real\n\n\n\n",
			want: []Event{{Data: "real"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := collect(t, strings.NewReader(tt.in))
			if len(got) != len(tt.want) {
				t.Fatalf("got %d events %+v, want %d %+v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("event %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestSplitAcrossReads(t *testing.T) {
	t.Parallel()
	const in = "event: e\ndata: part-one\ndata: part-two\n\n"
	// OneByteReader delivers a single byte per Read, forcing the scanner to
	// reassemble the event across many reads.
	r := iotest.OneByteReader(strings.NewReader(in))
	var got []Event
	for ev, err := range NewDecoder(r).All() {
		if err != nil {
			t.Fatalf("decode error: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) != 1 || got[0].Name != "e" || got[0].Data != "part-one\npart-two" {
		t.Fatalf("got %+v, want one event e with joined data", got)
	}
}

func TestHugeDataLine(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 200_000) // far past the 64 KiB default scanner limit
	got := collect(t, strings.NewReader("data: "+big+"\n\n"))
	if len(got) != 1 || got[0].Data != big {
		t.Fatalf("got %d events; want one carrying the full %d-byte payload", len(got), len(big))
	}
}

func Example() {
	r := strings.NewReader("event: greeting\ndata: hi\ndata: there\n\n")
	for ev, err := range NewDecoder(r).All() {
		if err != nil {
			panic(err)
		}
		fmt.Printf("%s|%q\n", ev.Name, ev.Data)
	}
	// Output: greeting|"hi\nthere"
}
```

`iotest.OneByteReader` returns a plain `io.Reader` that yields one byte per
`Read`, which is the whole point of `TestSplitAcrossReads`: a decoder that assumes
each read delivers whole lines passes every other case and fails this one.

## Review

The decoder is correct when its output is a pure function of the byte stream and
the SSE rules: consecutive `data:` lines join with a single newline, a leading
space after the colon is dropped, comments and unknown fields vanish, `id` and
`retry` persist across events, and `[DONE]` ends iteration without appearing.
`TestDecode` pins each of those; if the join row fails you concatenated without a
newline, and if the CRLF row fails your split function only handles `\n`. The
`iotest.OneByteReader` test is the one that catches a fragile lexer — a decoder
that assumes each `Read` delivers whole lines passes every other test and fails
this one.

The mistakes to avoid are the ones the concepts flag. Do not reach for a bare
`bufio.Scanner` without raising `Buffer`; `TestHugeDataLine` fails with
`bufio.ErrTooLong` the moment a data line exceeds 64 KiB, which real payloads do.
Do not treat `[DONE]` as data to unmarshal; it is a terminator. And do not emit an
event for a blank-only or comment-only stretch — the empty-data guard in
`dispatch` is what keeps heartbeats from turning into phantom events. Run
`go test -race`; although the decoder is single-goroutine, the race detector
confirms the iterator holds no shared state across the range body.

## Resources

- [MDN — Using server-sent events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events/Using_server-sent_events) — the `text/event-stream` wire format, `data`/`event`/`id`/`retry`, comments, and dispatch.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — `Buffer`, `Split`, and the `SplitFunc` contract including the "request more data" return.
- [`iter` package](https://pkg.go.dev/iter) — `Seq2` and the range-over-func iterator contract.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-streaming-llm-client.md](02-streaming-llm-client.md)
