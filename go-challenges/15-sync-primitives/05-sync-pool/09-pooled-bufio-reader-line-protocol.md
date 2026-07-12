# Exercise 9: Pooling bufio.Readers Across Connections in a Line-Protocol Ingest Server

A TCP log collector accepts thousands of short-lived connections per second,
and each one needs a buffered reader — a 4 KiB allocation that lives for
milliseconds. This module pools the `bufio.Reader` across connections, and in
doing so hits the sharpest edge of the reset contract: `Reset(conn)` is not a
style nicety here, it is the boundary that keeps one tenant's buffered bytes
from being parsed as another tenant's log records.

This module is fully self-contained: its own `go mod init`, its own demo, its
own tests. Nothing here imports another exercise.

## What you'll build

```text
lineingest/                   independent module: example.com/lineingest
  go.mod                      go 1.26
  ingest/
    ingest.go                 Record; ErrMalformedLine; HandleConn (pooled reader); parse
    ingest_test.go            parse table, cross-connection bleed, concurrency, Example
    bench_test.go             BenchmarkPooledReader vs BenchmarkPerConnReader
  cmd/
    demo/
      main.go                 three connections through one pooled reader
```

Files: `ingest/ingest.go`, `ingest/ingest_test.go`, `ingest/bench_test.go`, `cmd/demo/main.go`.
Implement: `HandleConn(conn io.Reader) ([]Record, error)` that borrows a pooled `*bufio.Reader`, rebinds it with `Reset(conn)`, and parses newline-delimited `LEVEL message` records via `ReadString('\n')`, returning `ErrMalformedLine` (wrapped with the line number) on garbage.
Test: table-driven parsing including a final line without a trailing newline; a sequential bleed test proving a reader abandoned mid-stream leaks nothing into the next connection; concurrent distinct-payload connections under `-race`; a pooled-vs-fresh-reader benchmark.
Verify: `go test -count=1 -race ./...` then `go test -bench=. -benchmem -run=^$ ./ingest`

### Reset(conn) is a data-integrity boundary, not bookkeeping

A `bufio.Reader` reads ahead: `ReadString('\n')` may pull 4 KiB from the
source into its internal buffer and hand you only the first line. That
read-ahead is the entire performance point — and the entire hazard. Suppose
connection A's handler returns early (a malformed line aborts the parse) while
the reader still buffers A's unread bytes. The reader goes back to the pool
holding those bytes. If the next `Get` wires it to connection B *without*
`Reset`, the very first `ReadString` serves A's leftovers as if B had sent
them: tenant A's log lines ingested under tenant B's connection. That is
cross-tenant data bleed — a correctness bug that doubles as a security
incident, and it is invisible in tests that only ever consume streams to EOF.

`bufio.Reader.Reset(r)` is the documented cure: it "discards any buffered
data, resets all state, and switches the buffered reader to read from r." Like
`gzip.Writer.Reset(w)` (and unlike `bytes.Buffer.Reset`, which only clears),
it both wipes *and rebinds* — and the rebinding is exactly what makes a
`bufio.Reader` poolable across connections at all, since the reader you get
from the pool is still wired to a connection that no longer exists. The
discipline in this module is Reset-on-Get: `HandleConn` calls `Reset(conn)`
immediately after `Get`, so no matter how dirty the previous user left the
reader — mid-stream abort, error path, anything — the current connection
starts from a provably empty buffer. The pool's `New` returns
`bufio.NewReaderSize(nil, 4096)`; binding to `nil` is safe precisely because
every `Get` is followed by a `Reset` before the first read.

### The protocol and its two boundary cases

The wire format is the classic newline-delimited text protocol: one record per
line, `LEVEL message`, split on the first space with `strings.Cut`. Two
boundaries carry most of the test weight. First, the final line often arrives
without a trailing newline (the sender closed the socket after the last byte):
`ReadString('\n')` returns that partial line *together with* `io.EOF`, so the
loop must process the returned data before acting on the error — checking the
error first silently drops the last record of every connection. Second, a
malformed line returns a wrapped `ErrMalformedLine` carrying the line number,
and returns the records parsed so far; callers assert the failure class with
`errors.Is` rather than string matching, and the early return is exactly the
path that leaves the reader dirty for the bleed test to catch.

Create `ingest/ingest.go`:

```go
// Package ingest parses a newline-delimited "LEVEL message" log protocol from
// short-lived collector connections, recycling buffered readers across
// connections through a sync.Pool.
package ingest

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// ErrMalformedLine reports a line that does not match "LEVEL message".
var ErrMalformedLine = errors.New("ingest: malformed line")

// Record is one parsed log record.
type Record struct {
	Level string
	Msg   string
}

// readerPool recycles buffered readers across connections. New binds to nil,
// which is safe because HandleConn always Resets to the live connection
// before the first read.
var readerPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 4096) },
}

// HandleConn parses all records from one collector connection through a
// pooled reader. Reset(conn) discards whatever the previous connection left
// buffered and rebinds the reader to this connection — the line that makes
// pooling safe across tenants.
func HandleConn(conn io.Reader) ([]Record, error) {
	br := readerPool.Get().(*bufio.Reader)
	br.Reset(conn)
	defer readerPool.Put(br)
	return parse(br)
}

// parse consumes br to EOF. ReadString returns any partial final line along
// with io.EOF, so data is handled before the error — otherwise the last
// record of a connection that omits the trailing newline is silently lost.
func parse(br *bufio.Reader) ([]Record, error) {
	var records []Record
	for lineNo := 1; ; lineNo++ {
		line, err := br.ReadString('\n')
		if trimmed := strings.TrimRight(line, "\r\n"); trimmed != "" {
			level, msg, ok := strings.Cut(trimmed, " ")
			if !ok || level == "" || msg == "" {
				return records, fmt.Errorf("line %d: %w", lineNo, ErrMalformedLine)
			}
			records = append(records, Record{Level: level, Msg: msg})
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return records, nil
			}
			return records, fmt.Errorf("line %d: %w", lineNo, err)
		}
	}
}
```

### The runnable demo

The demo pushes three "connections" through the pooled reader. Connection 2
aborts mid-stream on a malformed line, deliberately leaving unread bytes in
the reader's buffer; connection 3 then proves none of them bleed through.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/lineingest/ingest"
)

func main() {
	// Connection 1: three records, final line without a trailing newline.
	conn1 := strings.NewReader("INFO checkout started\nWARN retry scheduled\nINFO checkout done")
	recs, err := ingest.HandleConn(conn1)
	fmt.Printf("conn1: records=%d err=%v\n", len(recs), err)
	for _, r := range recs {
		fmt.Printf("  level=%s msg=%q\n", r.Level, r.Msg)
	}

	// Connection 2: a malformed line aborts the parse mid-stream, leaving
	// "INFO never parsed" buffered inside the pooled reader.
	conn2 := strings.NewReader("INFO ok\ngarbage-without-space\nINFO never parsed\n")
	recs, err = ingest.HandleConn(conn2)
	fmt.Printf("conn2: records=%d err=%v\n", len(recs), err)

	// Connection 3: Reset(conn) guarantees none of conn2's leftover bytes are
	// served as this tenant's records.
	conn3 := strings.NewReader("ERROR payment declined\n")
	recs, err = ingest.HandleConn(conn3)
	fmt.Printf("conn3: records=%d err=%v first=%q\n", len(recs), err, recs[0].Msg)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
conn1: records=3 err=<nil>
  level=INFO msg="checkout started"
  level=WARN msg="retry scheduled"
  level=INFO msg="checkout done"
conn2: records=1 err=line 2: ingest: malformed line
conn3: records=1 err=<nil> first="payment declined"
```

### Tests

The bleed test is the heart of the module: it aborts a connection mid-stream
(so the pooled reader goes back dirty, with buffered bytes from tenant A) and
then asserts the next connection sees only its own records. The concurrent
test gives every goroutine a distinct payload so any reader shared between two
live connections shows up as a parse mismatch under `-race`.

Create `ingest/ingest_test.go`:

```go
package ingest

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestParseTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    []Record
		wantErr error
	}{
		{
			name:  "well-formed with trailing newline",
			input: "INFO started\nWARN slow disk\n",
			want:  []Record{{"INFO", "started"}, {"WARN", "slow disk"}},
		},
		{
			name:  "final line without newline is not lost",
			input: "INFO started\nERROR socket closed",
			want:  []Record{{"INFO", "started"}, {"ERROR", "socket closed"}},
		},
		{
			name:  "message keeps its own spaces",
			input: "WARN retry in 5s due to timeout\n",
			want:  []Record{{"WARN", "retry in 5s due to timeout"}},
		},
		{
			name:  "blank lines are skipped",
			input: "INFO a\n\nINFO b\n",
			want:  []Record{{"INFO", "a"}, {"INFO", "b"}},
		},
		{
			name:  "empty connection",
			input: "",
			want:  nil,
		},
		{
			name:    "malformed line returns partial records and sentinel",
			input:   "INFO ok\nnospacehere\n",
			want:    []Record{{"INFO", "ok"}},
			wantErr: ErrMalformedLine,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := HandleConn(strings.NewReader(tt.input))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want errors.Is(err, %v)", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("HandleConn: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d records, want %d: %v", len(got), len(tt.want), got)
			}
			for i, r := range got {
				if r != tt.want[i] {
					t.Fatalf("record %d = %+v, want %+v", i, r, tt.want[i])
				}
			}
		})
	}
}

func TestNoCrossConnectionBleed(t *testing.T) {
	// Not parallel: the bleed scenario needs sequential reuse of the pooled
	// reader on one goroutine so the dirty reader from conn A is the one that
	// serves conn B.
	dirty := "INFO ok\ngarbage\nSECRET tenant-a-leftover\n"
	if _, err := HandleConn(strings.NewReader(dirty)); !errors.Is(err, ErrMalformedLine) {
		t.Fatalf("setup: err = %v, want ErrMalformedLine", err)
	}

	// The reader went back to the pool with "SECRET tenant-a-leftover\n"
	// buffered. Reset(conn) must discard it before serving tenant B.
	recs, err := HandleConn(strings.NewReader("INFO tenant-b-line\n"))
	if err != nil {
		t.Fatalf("HandleConn: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(recs), recs)
	}
	if recs[0].Level == "SECRET" || strings.Contains(recs[0].Msg, "tenant-a") {
		t.Fatalf("cross-connection bleed: tenant B received %+v", recs[0])
	}
	if recs[0] != (Record{"INFO", "tenant-b-line"}) {
		t.Fatalf("record = %+v, want tenant B's own line", recs[0])
	}
}

func TestConcurrentConnections(t *testing.T) {
	t.Parallel()

	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			payload := fmt.Sprintf("INFO conn-%d line one\nWARN conn-%d line two\n", i, i)
			recs, err := HandleConn(strings.NewReader(payload))
			if err != nil {
				t.Errorf("conn %d: %v", i, err)
				return
			}
			want := fmt.Sprintf("conn-%d line one", i)
			if len(recs) != 2 || recs[0].Msg != want {
				t.Errorf("conn %d: got %+v, want first msg %q", i, recs, want)
			}
		}()
	}
	wg.Wait()
}

func ExampleHandleConn() {
	recs, err := HandleConn(strings.NewReader("INFO cache warmed\nERROR upstream 503\n"))
	if err != nil {
		panic(err)
	}
	for _, r := range recs {
		fmt.Printf("%s: %s\n", r.Level, r.Msg)
	}
	// Output:
	// INFO: cache warmed
	// ERROR: upstream 503
}
```

### The benchmark: pooled reader vs a fresh reader per connection

The per-connection variant allocates a 4 KiB buffer (plus the reader struct)
for every connection; the pooled variant amortizes it away. On a collector
taking thousands of connections per second, that difference is exactly the
kind of steady GC pressure a pool is for.

Create `ingest/bench_test.go`:

```go
package ingest

import (
	"bufio"
	"strings"
	"testing"
)

var benchPayload = strings.Repeat("INFO a realistic log line for the benchmark\n", 50)

func BenchmarkPooledReader(b *testing.B) {
	r := strings.NewReader(benchPayload)
	b.ReportAllocs()
	for range b.N {
		r.Reset(benchPayload)
		if _, err := HandleConn(r); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPerConnReader(b *testing.B) {
	r := strings.NewReader(benchPayload)
	b.ReportAllocs()
	for range b.N {
		r.Reset(benchPayload)
		br := bufio.NewReaderSize(r, 4096) // fresh 4 KiB buffer per connection
		if _, err := parse(br); err != nil {
			b.Fatal(err)
		}
	}
}
```

Run the benchmarks:

```bash
go test -bench=. -benchmem -run=^$ ./ingest
```

Both variants still allocate the records they return; the per-connection
variant pays an extra reader-plus-buffer allocation on top, visible as one
more alloc/op and roughly 4 KiB more B/op.

## Review

The parser is correct when the table passes — especially the
final-line-without-newline case, where handling `ReadString`'s returned data
before its `io.EOF` is what saves the last record of every connection. The
pooling is correct when `TestNoCrossConnectionBleed` passes: the malformed
line aborts conn A mid-stream, the reader returns to the pool with A's bytes
still buffered, and only the `Reset(conn)` at the top of `HandleConn` stands
between those bytes and tenant B's record stream. Delete that line and this
test fails with tenant A's `SECRET` record attributed to B — run the
experiment once and the reset contract stops feeling optional. Verify with
`go test -count=1 -race ./...`.

## Resources

- [`bufio.Reader.Reset`](https://pkg.go.dev/bufio#Reader.Reset) — discards buffered data, resets state, and rebinds to a new source.
- [`bufio.NewReaderSize`](https://pkg.go.dev/bufio#NewReaderSize) — sizing the read-ahead buffer the pool recycles.
- [`bufio.Reader.ReadString`](https://pkg.go.dev/bufio#Reader.ReadString) — returns the data read before the error, the final-line contract.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — asserting the wrapped sentinel instead of matching error strings.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-capacity-capped-pool.md](08-capacity-capped-pool.md) | Next: [10-pooled-hmac-webhook-verifier.md](10-pooled-hmac-webhook-verifier.md)
