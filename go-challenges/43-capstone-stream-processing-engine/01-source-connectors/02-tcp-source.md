# Exercise 2: TCP Source

A network source must accept many clients at once without losing a byte. This `TCPSource` listens on an address, spawns a goroutine per connection, and funnels every newline-delimited message from every connection into one shared, buffered record channel.

Every module in this lesson is fully self-contained: it begins with its own `go mod init`, bundles the shared `Record`, `Metrics`, and `Source` definitions it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
tcp-source/
  go.mod
  source.go             Record, Metrics, Source, ErrSourceClosed
  tcp_source.go         TCPSource: listener, accept loop, per-conn goroutines
  tcp_source_test.go    single client, 10 concurrent clients, close-before-open
  cmd/demo/main.go      dial the listener and print one emitted line
```

- Files: `source.go`, `tcp_source.go`, `tcp_source_test.go`, `cmd/demo/main.go`.
- Implement: `TCPSource` with `Open`/`Close`/`Metrics` and an `Addr()` accessor for the bound port.
- Test: a single client's lines arrive in order; ten concurrent clients lose no records; `Close` before `Open` returns the sentinel.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/43-capstone-stream-processing-engine/01-source-connectors/02-tcp-source/cmd/demo && cd go-solutions/43-capstone-stream-processing-engine/01-source-connectors/02-tcp-source
```

### The shared vocabulary

`source.go` carries the same `Record`, `Metrics`, and `Source` definitions every module bundles, plus `ErrSourceClosed`. Keeping it in each module is what makes the module independently buildable and gateable.

Create `source.go`:

```go
package tcpsource

import (
	"context"
	"errors"
	"time"
)

// Record is the atomic unit flowing through the pipeline.
type Record struct {
	Key       []byte
	Value     []byte
	Timestamp time.Time
	Source    string
	Metadata  map[string]string
}

// Metrics is a point-in-time snapshot of a source's counters.
type Metrics struct {
	RecordsEmitted int64
	BytesRead      int64
	ErrorsTotal    int64
	BacklogSize    int64
}

// ErrSourceClosed is returned by Close when the source was never opened.
var ErrSourceClosed = errors.New("tcpsource: source not open")

// Source is the common interface for all data origins.
type Source interface {
	Open(ctx context.Context) (<-chan Record, <-chan error)
	Close() error
	Metrics() Metrics
}
```

### One goroutine per connection, one channel for all

`Open` binds the listener with `net.Listen("tcp", addr)`. Passing `127.0.0.1:0` lets the OS choose a free port, which `Addr()` then reports — this is what makes the tests hermetic, with no hard-coded port to collide. If the bind fails, `Open` cancels, closes both channels, and returns them already-closed, so a caller's `range` terminates immediately rather than hanging.

The `accept` goroutine is the heart of the design and it solves one specific problem: `listener.Accept()` blocks, so cancelling the context cannot unblock it directly. The fix is a small helper goroutine — `go func() { <-ctx.Done(); ts.listener.Close() }()` — that closes the listener when the context is cancelled, which makes the blocked `Accept` return an error. The accept loop then distinguishes a real error from this expected shutdown by re-checking `ctx.Done()` after `Accept` fails: if the context is done, it returns cleanly; otherwise it reports the error. This is also the answer to the "race on the listener in Close" question — `Close` only cancels the context and never touches the listener itself, so the listener has exactly one closer.

Each accepted connection runs in its own goroutine tracked by a local `connWG`, and the accept goroutine `defer connWG.Wait()`s so it does not return until every connection reader has drained. `readConn` runs a `bufio.Scanner` over the socket, emitting each line with a context-guarded blocking send — the same durable-delivery choice as the file source — and records the remote address as the record's `Source`.

Create `tcp_source.go`:

```go
package tcpsource

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// TCPSource listens on a TCP address and emits each newline-delimited message as a Record.
// Multiple concurrent connections are supported; each runs in its own goroutine.
type TCPSource struct {
	addr       string
	bufferSize int

	listener net.Listener
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	records  chan Record
	errs     chan error

	emitted atomic.Int64
	bytes   atomic.Int64
	errCnt  atomic.Int64
}

// NewTCPSource creates a TCPSource that listens on addr (e.g. "127.0.0.1:9000").
func NewTCPSource(addr string, bufferSize int) *TCPSource {
	return &TCPSource{addr: addr, bufferSize: bufferSize}
}

// Addr returns the actual listen address (useful when addr was ":0").
func (ts *TCPSource) Addr() string {
	if ts.listener == nil {
		return ts.addr
	}
	return ts.listener.Addr().String()
}

func (ts *TCPSource) Open(ctx context.Context) (<-chan Record, <-chan error) {
	inner, cancel := context.WithCancel(ctx)
	ts.cancel = cancel
	ts.records = make(chan Record, ts.bufferSize)
	ts.errs = make(chan error, 16)

	ln, err := net.Listen("tcp", ts.addr)
	if err != nil {
		cancel()
		close(ts.records)
		close(ts.errs)
		return ts.records, ts.errs
	}
	ts.listener = ln

	ts.wg.Add(1)
	go ts.accept(inner)

	go func() {
		ts.wg.Wait()
		close(ts.records)
		close(ts.errs)
	}()

	return ts.records, ts.errs
}

func (ts *TCPSource) accept(ctx context.Context) {
	defer ts.wg.Done()

	var connWG sync.WaitGroup
	defer connWG.Wait()

	go func() {
		<-ctx.Done()
		ts.listener.Close()
	}()

	for {
		conn, err := ts.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			ts.errCnt.Add(1)
			select {
			case ts.errs <- fmt.Errorf("tcpsource: accept: %w", err):
			default:
			}
			return
		}
		connWG.Add(1)
		go func(c net.Conn) {
			defer connWG.Done()
			ts.readConn(ctx, c)
		}(conn)
	}
}

func (ts *TCPSource) readConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Bytes()
		ts.bytes.Add(int64(len(line)))

		r := Record{
			Value:     append([]byte(nil), line...),
			Timestamp: time.Now().UTC(),
			Source:    conn.RemoteAddr().String(),
		}
		select {
		case ts.records <- r:
			ts.emitted.Add(1)
		case <-ctx.Done():
			return
		}
	}
	if err := scanner.Err(); err != nil {
		ts.errCnt.Add(1)
		select {
		case ts.errs <- fmt.Errorf("tcpsource: read %s: %w", conn.RemoteAddr(), err):
		default:
		}
	}
}

func (ts *TCPSource) Close() error {
	if ts.cancel == nil {
		return ErrSourceClosed
	}
	ts.cancel()
	ts.wg.Wait()
	return nil
}

func (ts *TCPSource) Metrics() Metrics {
	return Metrics{
		RecordsEmitted: ts.emitted.Load(),
		BytesRead:      ts.bytes.Load(),
		ErrorsTotal:    ts.errCnt.Load(),
	}
}

var _ Source = (*TCPSource)(nil)
```

### The runnable demo

The demo opens the source, waits briefly for the listener to bind, dials it, writes one line, and prints the record that arrives.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net"
	"time"

	tcp "example.com/tcp-source"
)

func main() {
	ts := tcp.NewTCPSource("127.0.0.1:0", 16)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	recs, _ := ts.Open(ctx)

	time.Sleep(20 * time.Millisecond)
	conn, _ := net.Dial("tcp", ts.Addr())
	fmt.Fprintln(conn, "line-from-tcp")
	conn.Close()

	r := <-recs
	fmt.Printf("TCPSource emitted: %s\n", r.Value)
	ts.Close()
	fmt.Printf("records=%d\n", ts.Metrics().RecordsEmitted)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
TCPSource emitted: line-from-tcp
records=1
```

### Tests

`TestTCPSourceEmitsLines` dials once, sends three lines, and asserts they arrive in order. `TestTCPSourceConcurrentConnections` is the load test: ten clients each send five messages concurrently, and the test asserts all fifty records arrive — the property that the per-connection goroutine model and the shared buffered channel lose nothing under concurrency, which `-race` then confirms is also data-race free. `TestTCPSourceCloseBeforeOpen` asserts the sentinel.

Create `tcp_source_test.go`:

```go
package tcpsource

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

func drain(ch <-chan Record, max int, timeout time.Duration) []Record {
	var out []Record
	deadline := time.After(timeout)
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, r)
			if len(out) >= max {
				return out
			}
		case <-deadline:
			return out
		}
	}
}

func TestTCPSourceEmitsLines(t *testing.T) {
	t.Parallel()

	ts := NewTCPSource("127.0.0.1:0", 64)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	recs, errs := ts.Open(ctx)
	go func() {
		for e := range errs {
			t.Logf("tcpsource error: %v", e)
		}
	}()

	conn, err := net.Dial("tcp", ts.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	lines := []string{"alpha", "beta", "gamma"}
	for _, l := range lines {
		fmt.Fprintln(conn, l)
	}
	conn.Close()

	got := drain(recs, len(lines), 2*time.Second)
	if len(got) != len(lines) {
		t.Fatalf("got %d records, want %d", len(got), len(lines))
	}
	for i, r := range got {
		if string(r.Value) != lines[i] {
			t.Errorf("record[%d] = %q, want %q", i, r.Value, lines[i])
		}
	}

	ts.Close()
}

func TestTCPSourceConcurrentConnections(t *testing.T) {
	t.Parallel()

	ts := NewTCPSource("127.0.0.1:0", 256)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	recs, _ := ts.Open(ctx)

	const conns = 10
	const perConn = 5
	for i := 0; i < conns; i++ {
		go func(id int) {
			conn, err := net.Dial("tcp", ts.Addr())
			if err != nil {
				return
			}
			defer conn.Close()
			for j := 0; j < perConn; j++ {
				fmt.Fprintf(conn, "conn%d-msg%d\n", id, j)
			}
		}(i)
	}

	got := drain(recs, conns*perConn, 4*time.Second)
	if len(got) != conns*perConn {
		t.Errorf("got %d records, want %d", len(got), conns*perConn)
	}
	ts.Close()
}

func TestTCPSourceCloseBeforeOpen(t *testing.T) {
	t.Parallel()
	ts := NewTCPSource("127.0.0.1:0", 8)
	if err := ts.Close(); err != ErrSourceClosed {
		t.Errorf("Close = %v, want %v", err, ErrSourceClosed)
	}
}
```

## Review

The source is correct when shutdown is race-free and no message is lost under concurrent clients. The two subtle points are both about `Accept`: it can only be unblocked by closing the listener, so a dedicated goroutine closes the listener on `ctx.Done()`, and the accept loop must re-check `ctx.Done()` after an `Accept` error to tell a real failure apart from an intentional shutdown. The common mistakes are calling `listener.Close()` directly from `Close` (creating two closers of shared state), forgetting `defer connWG.Wait()` so `Close` returns while connection readers are still writing to a channel about to be closed, and using a non-blocking send that drops messages under load. The fifty-record concurrent test under `-race` is the proof.

## Resources

- [`net.Listener` and `net.Listen`](https://pkg.go.dev/net#Listen) — binding a TCP port, the `:0` ephemeral-port trick, and the `Accept` contract.
- [Go blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the goroutine-per-source and context-cancellation patterns this listener uses.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — newline-delimited framing over a streaming connection.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-file-source.md](01-file-source.md) | Next: [03-http-source.md](03-http-source.md)
