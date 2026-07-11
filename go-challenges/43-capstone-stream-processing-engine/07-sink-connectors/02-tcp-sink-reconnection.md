# Exercise 2: TCP Sink with Reconnection

A TCP sink streams records as newline-delimited JSON over a long-lived connection, and the whole engineering problem is what happens when that connection breaks. This exercise builds a sink that detects a write failure, reconnects with cancellable exponential backoff, and resumes — without ever blocking the pipeline past a context cancellation.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
sink.go                Record, Sink, Metrics, sentinel errors, capDuration
tcp_sink.go            TCPSink: dial, buffered write, writeWithRetry, reconnect
cmd/
  demo/
    main.go            stream three records to a local TCP server, count lines
tcp_sink_test.go       happy path, empty-addr guard, retry exhaustion, cancellation
```

- Files: `sink.go`, `tcp_sink.go`, `cmd/demo/main.go`, `tcp_sink_test.go`.
- Implement: `TCPSink` with `Open`, `Write`, `Flush`, `Close`, and the internal `dial` and `writeWithRetry` helpers, plus the shared `Record` and `Metrics` types.
- Test: `tcp_sink_test.go` streams records to a real `net.Listen` server, rejects an empty address, drives the retry loop to exhaustion when the destination is gone, and proves the backoff wait is cancellable.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p tcp-sink-reconnection/cmd/demo && cd tcp-sink-reconnection
go mod init example.com/tcp-sink-reconnection
go mod edit -go=1.26
```

### The shared types

`TCPSink` reuses the same `Record` and `Metrics` shapes as the other connectors, plus a small `capDuration` helper that the exponential backoff uses to cap its growth. The `Retries` metric is the one to watch here: it counts reconnection attempts, so a sink whose `Retries` climbs is telling you the destination is flapping.

Create `sink.go`:

```go
// Package sink provides output connectors for a stream processing pipeline.
// TCPSink writes newline-delimited JSON over a TCP connection and reconnects
// with exponential backoff on failure.
package sink

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// Record is the unit of data flowing through the pipeline.
type Record struct {
	Key       []byte
	Value     []byte
	Timestamp time.Time
	Metadata  map[string]string
}

// Sentinel errors. Wrap with fmt.Errorf("%w", ...) for context; check with errors.Is.
var (
	ErrNotOpen     = errors.New("sink: not open")
	ErrAlreadyOpen = errors.New("sink: already open")
	ErrEmptyAddr   = errors.New("sink: address must not be empty")
	ErrMaxRetries  = errors.New("sink: max retries exhausted")
)

// Sink is the write side of the stream pipeline.
// Open must be called before Write. Close must be called exactly once.
type Sink interface {
	Open(ctx context.Context) error
	Write(ctx context.Context, records []Record) error
	Flush(ctx context.Context) error
	Close() error
}

// Metrics tracks per-sink activity counters. All fields are atomic and safe
// to read from any goroutine at any time.
type Metrics struct {
	RecordsWritten atomic.Int64
	BytesWritten   atomic.Int64
	BatchesFlushed atomic.Int64
	FlushErrors    atomic.Int64
	Retries        atomic.Int64
}

func capDuration(d, max time.Duration) time.Duration {
	if d > max {
		return max
	}
	return d
}
```

### Detecting failure and reconnecting without hanging

`Open` dials the configured address once and wraps the connection in a `bufio.Writer`. `Write` serializes each record to a JSON line and hands it to `writeWithRetry`, which is where the reconnection logic lives.

`writeWithRetry` is a loop. Each iteration first tries the buffered write; on success it returns immediately. On failure — or when there is no live connection — it checks whether it has exhausted `MaxRetries` and, if so, returns `ErrMaxRetries`. Otherwise it closes the broken connection, waits out the current backoff, redials, doubles the backoff (capped at 30s), and loops. The redial error is deliberately ignored: if the dial fails, the next iteration's write will fail too and drive another retry, so there is no need to handle the dial error separately.

The wait is the part that must be right. It is a `select` over `time.After(backoff)` and `ctx.Done()`, so a pipeline shutdown that cancels the context aborts the wait immediately and returns `ctx.Err()` instead of sleeping out the backoff. The reconnection test exploits the inverse: with a one-hour backoff, *only* a context cancellation can end the wait, which is how the test proves the cancellation path works without waiting an hour.

One nuance worth understanding: because writes go through a `bufio.Writer`, a small write lands in the buffer and returns success even if the underlying connection is dead — the error does not surface until the buffer flushes to the socket. That is why the retry-exhaustion test writes a one-megabyte record with a tiny buffer: it forces the buffered writer to flush real bytes to the broken socket inside `Write`, surfacing the connection error that drives the retry loop. In production the periodic `Flush` plays the same role of forcing buffered bytes out where failures become visible.

Create `tcp_sink.go`:

```go
package sink

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

// TCPSinkConfig holds configuration for TCPSink.
type TCPSinkConfig struct {
	// Addr is the TCP address to connect to (host:port). Required.
	Addr string
	// DialTimeout is the per-attempt dial timeout. Defaults to 5s.
	DialTimeout time.Duration
	// MaxRetries is the maximum reconnection attempts per write. Defaults to 5.
	MaxRetries int
	// RetryBackoff is the initial backoff between reconnection attempts.
	// It doubles on each failure and is capped at 30s. Defaults to 200ms.
	RetryBackoff time.Duration
	// BufSize is the bufio.Writer buffer size in bytes. Defaults to 64 KiB.
	BufSize int
}

// TCPSink writes records as newline-delimited JSON over a TCP connection.
// On a write failure it closes the broken connection and reconnects with
// exponential backoff before retrying.
type TCPSink struct {
	cfg     TCPSinkConfig
	mu      sync.Mutex
	conn    net.Conn
	bw      *bufio.Writer
	open    bool
	metrics Metrics
}

// NewTCPSink constructs a TCPSink with sensible defaults applied.
func NewTCPSink(cfg TCPSinkConfig) (*TCPSink, error) {
	if cfg.Addr == "" {
		return nil, ErrEmptyAddr
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 5
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 200 * time.Millisecond
	}
	if cfg.BufSize <= 0 {
		cfg.BufSize = 64 * 1024
	}
	return &TCPSink{cfg: cfg}, nil
}

// Metrics returns the live activity counters.
func (s *TCPSink) Metrics() *Metrics { return &s.metrics }

// Open dials the configured address.
func (s *TCPSink) Open(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open {
		return ErrAlreadyOpen
	}
	if err := s.dial(); err != nil {
		return fmt.Errorf("tcp sink open: %w", err)
	}
	s.open = true
	return nil
}

// dial creates a new TCP connection. Callers must hold s.mu.
func (s *TCPSink) dial() error {
	conn, err := net.DialTimeout("tcp", s.cfg.Addr, s.cfg.DialTimeout)
	if err != nil {
		return err
	}
	s.conn = conn
	s.bw = bufio.NewWriterSize(conn, s.cfg.BufSize)
	return nil
}

// Write serializes each record as JSON and sends it over the TCP connection.
// On a write error it reconnects with exponential backoff and retries.
func (s *TCPSink) Write(ctx context.Context, records []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		return ErrNotOpen
	}
	for _, r := range records {
		b, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("tcp sink write: marshal: %w", err)
		}
		line := append(b, '\n')
		if err := s.writeWithRetry(ctx, line); err != nil {
			return err
		}
		s.metrics.RecordsWritten.Add(1)
		s.metrics.BytesWritten.Add(int64(len(line)))
	}
	return nil
}

// writeWithRetry writes one line. On failure it closes the connection, waits
// for backoff, redials, and tries again up to MaxRetries times.
// Callers must hold s.mu.
func (s *TCPSink) writeWithRetry(ctx context.Context, line []byte) error {
	backoff := s.cfg.RetryBackoff
	for attempt := 0; ; attempt++ {
		if s.bw != nil {
			if _, err := s.bw.Write(line); err == nil {
				return nil
			}
		}
		if attempt >= s.cfg.MaxRetries {
			s.metrics.FlushErrors.Add(1)
			return fmt.Errorf("tcp sink: %w after %d attempts", ErrMaxRetries, attempt)
		}
		s.metrics.Retries.Add(1)
		if s.conn != nil {
			s.conn.Close()
			s.conn = nil
			s.bw = nil
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		_ = s.dial() // ignore error; next iteration retries
		backoff = capDuration(backoff*2, 30*time.Second)
	}
}

// Flush flushes the bufio.Writer buffer to the underlying TCP connection.
func (s *TCPSink) Flush(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		return ErrNotOpen
	}
	if s.bw == nil {
		return nil
	}
	if err := s.bw.Flush(); err != nil {
		s.metrics.FlushErrors.Add(1)
		return fmt.Errorf("tcp sink flush: %w", err)
	}
	s.metrics.BatchesFlushed.Add(1)
	return nil
}

// Close flushes the buffer (best-effort) and closes the TCP connection.
func (s *TCPSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		return nil
	}
	s.open = false
	if s.bw != nil {
		s.bw.Flush() // best-effort; ignore error on close
	}
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}
```

### The runnable demo

The demo starts a local TCP server on an ephemeral port, streams three records to it, flushes, and reports how many lines the server received. It uses a real listener so the connection path is genuine, not mocked.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"example.com/tcp-sink-reconnection"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

func run() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()

	lines := make(chan int, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			lines <- -1
			return
		}
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		var buf bytes.Buffer
		io.Copy(&buf, conn)
		conn.Close()
		lines <- bytes.Count(buf.Bytes(), []byte("\n"))
	}()

	ts, err := sink.NewTCPSink(sink.TCPSinkConfig{Addr: ln.Addr().String()})
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := ts.Open(ctx); err != nil {
		return err
	}
	for i := 0; i < 3; i++ {
		if err := ts.Write(ctx, []sink.Record{{
			Key:   []byte(fmt.Sprintf("k%d", i)),
			Value: []byte(fmt.Sprintf("v%d", i)),
		}}); err != nil {
			return err
		}
	}
	if err := ts.Flush(ctx); err != nil {
		return err
	}
	if err := ts.Close(); err != nil {
		return err
	}

	got := <-lines
	m := ts.Metrics()
	fmt.Printf("server received %d lines\n", got)
	fmt.Printf("records=%d retries=%d\n", m.RecordsWritten.Load(), m.Retries.Load())
	return nil
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
server received 3 lines
records=3 retries=0
```

`retries=0` because the local server never drops the connection; on a flapping upstream this counter would climb with each reconnection.

### Tests

`TestTCPSinkWritesRecords` is the happy path: a real listener accepts the connection, the sink streams two records, and the test asserts both lines arrive. `TestTCPSinkRejectsEmptyAddr` guards the constructor. `TestTCPSinkReconnectExhaustsRetries` tears the destination down after `Open` and writes a large payload so the broken socket surfaces an error, then asserts the retry loop ends in `ErrMaxRetries` with a non-zero `Retries` counter rather than blocking forever. `TestTCPSinkRetryHonoursContextCancellation` sets an hour-long backoff so the only way the write can return is via the cancelled context, proving the wait is cancellable.

Create `tcp_sink_test.go`:

```go
package sink

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestTCPSinkWritesRecords(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	type result struct {
		data []byte
		err  error
	}
	serverDone := make(chan result, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverDone <- result{err: err}
			return
		}
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		var buf bytes.Buffer
		io.Copy(&buf, conn)
		conn.Close()
		serverDone <- result{data: buf.Bytes()}
	}()

	ts, err := NewTCPSink(TCPSinkConfig{Addr: ln.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := ts.Open(ctx); err != nil {
		t.Fatal(err)
	}
	records := []Record{
		{Key: []byte("k1"), Value: []byte("v1")},
		{Key: []byte("k2"), Value: []byte("v2")},
	}
	if err := ts.Write(ctx, records); err != nil {
		t.Fatal(err)
	}
	if err := ts.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	ts.Close()

	res := <-serverDone
	if res.err != nil {
		t.Fatal(res.err)
	}
	lines := bytes.Split(bytes.TrimRight(res.data, "\n"), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), res.data)
	}
}

func TestTCPSinkRejectsEmptyAddr(t *testing.T) {
	t.Parallel()

	_, err := NewTCPSink(TCPSinkConfig{})
	if !errors.Is(err, ErrEmptyAddr) {
		t.Fatalf("err = %v, want ErrEmptyAddr", err)
	}
}

// TestTCPSinkReconnectExhaustsRetries verifies that when the destination is
// gone and every redial fails, writeWithRetry counts retries and finally
// returns ErrMaxRetries rather than blocking forever. A large payload with a
// tiny bufio buffer forces the buffered writer to flush to the broken socket
// inside Write, surfacing the connection error that drives the retry loop.
func TestTCPSinkReconnectExhaustsRetries(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- conn
	}()

	ts, err := NewTCPSink(TCPSinkConfig{
		Addr:         addr,
		MaxRetries:   3,
		RetryBackoff: time.Millisecond,
		BufSize:      16,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := ts.Open(ctx); err != nil {
		t.Fatal(err)
	}

	// Tear the destination down: close the server side and stop listening so
	// every redial attempt is refused.
	if c := <-accepted; c != nil {
		c.Close()
	}
	ln.Close()

	big := make([]byte, 1<<20) // 1 MiB forces real socket writes through bufio
	err = ts.Write(ctx, []Record{{Key: []byte("k"), Value: big}})
	if !errors.Is(err, ErrMaxRetries) {
		t.Fatalf("err = %v, want ErrMaxRetries", err)
	}
	if got := ts.metrics.Retries.Load(); got == 0 {
		t.Fatal("Retries counter should be non-zero after reconnection attempts")
	}
	ts.Close()
}

// TestTCPSinkRetryHonoursContextCancellation verifies the backoff wait is
// cancellable: a cancelled context aborts the retry loop promptly instead of
// sleeping through shutdown.
func TestTCPSinkRetryHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- conn
	}()

	ts, err := NewTCPSink(TCPSinkConfig{
		Addr:         addr,
		MaxRetries:   100,
		RetryBackoff: time.Hour, // long, so only cancellation can end the wait
		BufSize:      16,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := <-accepted; c != nil {
		c.Close()
	}
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	big := make([]byte, 1<<20)
	err = ts.Write(ctx, []Record{{Key: []byte("k"), Value: big}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	ts.Close()
}
```

## Review

The sink is correct when a broken connection turns into a bounded, cancellable retry rather than a hang or a panic. Confirm `writeWithRetry` closes the dead connection before redialing (a leaked connection on every failure exhausts file descriptors), caps the exponential backoff so it cannot grow without bound, and selects on `ctx.Done()` in the wait so shutdown is prompt. The classic mistakes are using `time.Sleep` instead of the `select` (uncancellable, hangs the pipeline on shutdown), ignoring the buffered-writer subtlety and concluding that a successful small write means a live connection, and forgetting to bound the retries so a permanently dead destination loops forever. The suite passing repeatedly under `go test -race ./...` — including the exhaustion and cancellation paths — establishes these properties.

## Resources

- [`net.DialTimeout`](https://pkg.go.dev/net#DialTimeout) — bounded TCP dialing so a redial cannot block indefinitely.
- [`bufio.Writer`](https://pkg.go.dev/bufio#Writer) — buffered writes and the flush-surfaces-errors behavior the retry loop depends on.
- [`context.Context` cancellation](https://pkg.go.dev/context) — the mechanism that makes the backoff wait abortable on shutdown.
- [Exponential Backoff And Jitter (AWS Builders' Library)](https://aws.amazon.com/builders-library/timeouts-retries-and-backoff-with-jitter/) — why backoff must grow and be capped.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-file-sink-two-phase-commit.md](01-file-sink-two-phase-commit.md) | Next: [03-http-sink-batching-idempotency.md](03-http-sink-batching-idempotency.md)
