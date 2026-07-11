# Exercise 3: Stream DB Rows Over a Channel and Detect End-of-Stream

A repository that streams rows from a paginated cursor is a channel problem: the
producer pushes records until the query is exhausted (or errors), and the consumer
must tell "here is another row" apart from "the stream ended". The comma-ok receive
form `rec, ok := <-ch` is exactly that distinction, and closing the channel is how
the producer signals end-of-stream — mirroring `rows.Next()` returning false and
`rows.Err()` reporting a mid-stream failure in `database/sql`.

This module is self-contained: its own module, a `cursor` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
cursor/                      independent module: example.com/cursor
  go.mod                     go 1.26
  cursor.go                  type Record, Row; Rows() <-chan Row; ErrStreamAborted
  cmd/demo/main.go           runnable demo: drain a stream, print each row
  cursor_test.go             delivery, comma-ok end detection, empty, mid-stream error
```

- Files: `cursor.go`, `cmd/demo/main.go`, `cursor_test.go`.
- Implement: `Rows(source []Record, failAfter int) <-chan Row` that streams each record, optionally emits a terminal error row, and closes the channel when finished.
- Test: all rows delivered; the consumer sees `ok == false` exactly once after the last value; an empty source closes immediately; a mid-stream error terminates with `ErrStreamAborted`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cursor/cmd/demo
cd ~/go-exercises/cursor
go mod init example.com/cursor
```

### Modeling a cursor: values, then maybe an error, then close

A real cursor delivers a stream of rows and can fail partway through — a dropped
connection, a query timeout. `database/sql` models this by having `Next()` return
`false` both at normal end and on error, and then `Err()` tells you which. Over a
channel the clean equivalent is a `Row` envelope that carries either a `Record` or
an `Err`. The producer goroutine:

1. owns the channel (it is the sole sender), and `defer close(out)` guarantees the
   channel is closed on *every* return path — normal exhaustion or early error;
2. emits one `Row{Record: r}` per source record;
3. if a failure is injected at index `failAfter`, emits a single
   `Row{Err: ErrStreamAborted}` and returns (the deferred close still runs).

The consumer distinguishes three states. `rec, ok := <-ch` with `ok == true` and
`Err == nil` is a real row. A `Row` with a non-nil `Err` is a mid-stream failure —
the consumer records it and stops. And `ok == false` means the channel is closed
and drained: end of stream, no more rows ever. That is the state a `for range`
loop terminates on automatically; the explicit comma-ok form is what you use when
you need to react to the close itself.

The error is a package-level sentinel wrapped nowhere here but returned directly,
so callers assert it with `errors.Is`. Using a sentinel rather than a bare string
lets callers branch on the specific failure without string matching.

Create `cursor.go`:

```go
package cursor

import "errors"

// ErrStreamAborted is delivered as a terminal Row.Err when the underlying cursor
// fails partway through streaming.
var ErrStreamAborted = errors.New("cursor: stream aborted")

// Record is one row of data from the source.
type Record struct {
	ID   int
	Name string
}

// Row is the envelope streamed over the channel: exactly one of Record or Err is
// meaningful. A Row with a non-nil Err is the last value before the channel
// closes.
type Row struct {
	Record Record
	Err    error
}

// Rows streams source over a channel and closes it when finished. If failAfter is
// in range [0, len(source)), the stream emits that many records and then a single
// error Row before closing, simulating a cursor that failed mid-scan. A negative
// failAfter (use -1) streams the whole source with no error.
func Rows(source []Record, failAfter int) <-chan Row {
	out := make(chan Row)
	go func() {
		defer close(out)
		for i, r := range source {
			if failAfter >= 0 && i == failAfter {
				out <- Row{Err: ErrStreamAborted}
				return
			}
			out <- Row{Record: r}
		}
	}()
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cursor"
)

func main() {
	src := []cursor.Record{
		{ID: 1, Name: "alice"},
		{ID: 2, Name: "bob"},
		{ID: 3, Name: "carol"},
	}

	// Drain with the comma-ok form so we can report the close explicitly.
	ch := cursor.Rows(src, -1)
	for {
		row, ok := <-ch
		if !ok {
			fmt.Println("end of stream")
			break
		}
		if row.Err != nil {
			fmt.Println("error:", row.Err)
			continue
		}
		fmt.Printf("row: %d %s\n", row.Record.ID, row.Record.Name)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
row: 1 alice
row: 2 bob
row: 3 carol
end of stream
```

### Tests

`TestStreamDeliversAllRows` ranges the channel and collects every record, proving
the whole source arrives. `TestConsumerSeesCloseViaCommaOk` is the point of the
exercise: after the last value the very next receive must report `ok == false`
exactly once, and stay closed. `TestEmptySourceClosesImmediately` proves a
zero-length source produces a channel that a `for range` exits with zero
iterations — the empty stream is not a special case in the consumer. And
`TestStreamStopsAtError` injects a failure after two rows and asserts the consumer
sees two records, then an `ErrStreamAborted` row, then close.

Create `cursor_test.go`:

```go
package cursor

import (
	"errors"
	"fmt"
	"testing"
)

func sampleSource(n int) []Record {
	src := make([]Record, n)
	for i := range src {
		src[i] = Record{ID: i + 1, Name: fmt.Sprintf("r%d", i+1)}
	}
	return src
}

func TestStreamDeliversAllRows(t *testing.T) {
	t.Parallel()

	src := sampleSource(4)
	var got []Record
	for row := range Rows(src, -1) {
		if row.Err != nil {
			t.Fatalf("unexpected error: %v", row.Err)
		}
		got = append(got, row.Record)
	}
	if len(got) != len(src) {
		t.Fatalf("got %d rows, want %d", len(got), len(src))
	}
	for i, r := range got {
		if r != src[i] {
			t.Fatalf("row %d = %+v, want %+v", i, r, src[i])
		}
	}
}

func TestConsumerSeesCloseViaCommaOk(t *testing.T) {
	t.Parallel()

	ch := Rows(sampleSource(2), -1)

	if _, ok := <-ch; !ok {
		t.Fatal("first receive: ok=false, want a value")
	}
	if _, ok := <-ch; !ok {
		t.Fatal("second receive: ok=false, want a value")
	}
	if _, ok := <-ch; ok {
		t.Fatal("third receive: ok=true, want closed")
	}
	// A closed channel stays closed.
	if _, ok := <-ch; ok {
		t.Fatal("fourth receive: ok=true, want closed")
	}
}

func TestEmptySourceClosesImmediately(t *testing.T) {
	t.Parallel()

	count := 0
	for range Rows(nil, -1) {
		count++
	}
	if count != 0 {
		t.Fatalf("ranged %d times over an empty stream, want 0", count)
	}
}

func TestStreamStopsAtError(t *testing.T) {
	t.Parallel()

	var records []Record
	var streamErr error
	for row := range Rows(sampleSource(5), 2) {
		if row.Err != nil {
			streamErr = row.Err
			break
		}
		records = append(records, row.Record)
	}

	if len(records) != 2 {
		t.Fatalf("got %d records before error, want 2", len(records))
	}
	if !errors.Is(streamErr, ErrStreamAborted) {
		t.Fatalf("stream error = %v, want ErrStreamAborted", streamErr)
	}
}

func ExampleRows() {
	for row := range Rows([]Record{{ID: 7, Name: "z"}}, -1) {
		fmt.Println(row.Record.ID, row.Record.Name)
	}
	// Output: 7 z
}
```

## Review

The stream is correct when the channel is closed on every exit path and the
consumer can always tell a value from the end. The `defer close(out)` is what
guarantees the first property — remove it and the error path leaks the channel and
hangs any ranging consumer, which the default test timeout would surface as a
failure. The comma-ok test proves the second: `ok` flips to `false` exactly once,
after the last value, and never flips back. The realistic mistake this guards
against is treating a mid-stream error like end-of-stream and silently dropping the
fact that the scan failed — here the error is a distinct `Row` carrying a sentinel
the consumer asserts with `errors.Is`, so a partial read is never mistaken for a
complete one.

## Resources

- [Go Language Spec: Receive operator](https://go.dev/ref/spec#Receive_operator) — the comma-ok form and what receiving from a closed channel returns.
- [`database/sql` Rows](https://pkg.go.dev/database/sql#Rows) — the real cursor whose `Next`/`Err` shape this exercise mirrors.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — producers that own and close their output channel.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-request-reply-command-channel.md](02-request-reply-command-channel.md) | Next: [04-synchronous-handoff-backpressure.md](04-synchronous-handoff-backpressure.md)
