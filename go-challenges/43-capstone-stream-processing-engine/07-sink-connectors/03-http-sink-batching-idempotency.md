# Exercise 3: HTTP Sink with Batching and Idempotency Keys

An HTTP sink POSTs batches of records to an endpoint, and it has to solve three problems at once: amortize the per-request cost by batching, bound latency for low-volume streams with a timer, and survive the fact that a timed-out POST may already have been processed. This exercise builds all three — dual-trigger batching, retry with backoff, and an idempotency key that lets the receiver dedupe retries.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
sink.go                Record, Sink, Metrics, sentinel errors, capDuration
http_sink.go           HTTPSink: dual-trigger batching, idempotency key, retry
cmd/
  demo/
    main.go            POST seven records in batches of three to a test server
http_sink_test.go      size batching, key uniqueness, retry-key reuse, exhaustion
```

- Files: `sink.go`, `http_sink.go`, `cmd/demo/main.go`, `http_sink_test.go`.
- Implement: `HTTPSink` with `Open`, `Write`, `Flush`, `Close`, the background `flushLoop`, and the `drainBatch` and `send` helpers, plus the shared `Record` and `Metrics` types.
- Test: `http_sink_test.go` proves size-triggered batching, unique keys across batches, the *same* key across retries of one batch, and `ErrMaxRetries` when the server stays down.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The shared types

The HTTP sink shares the `Record` and `Metrics` shapes and the `capDuration` backoff helper. The metrics of interest are `BatchesFlushed` (one per successful POST) and `Retries` (one per retried attempt), which together tell you the request rate and how flaky the endpoint is.

Create `sink.go`:

```go
// Package sink provides output connectors for a stream processing pipeline.
// HTTPSink POSTs batches of records and deduplicates retries with an
// Idempotency-Key header.
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
	ErrEmptyURL    = errors.New("sink: URL must not be empty")
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

### Dual-trigger batching, drain-then-send, and the idempotency key

`HTTPSink` accumulates records in a `batch` slice guarded by a mutex. Two independent triggers flush it. The size trigger lives inside `Write`: after appending, if the batch has reached `BatchSize`, `Write` drains and sends it synchronously before returning. The timer trigger lives in a background goroutine started by `Open` — `flushLoop` ticks every `FlushInterval` and calls `Flush`, which sends a partial batch so a low-volume stream does not strand records indefinitely. Together they bound memory (size) and latency (interval).

The concurrency-critical move is `drainBatch`. The mutex serializes access to the shared `batch`, but the HTTP round-trip must not happen under that lock — holding it would block every concurrent `Write` for the full network latency. So `Write` and `Flush` both follow the same shape: take the lock, swap the batch out into a local variable and bump the sequence counter (`drainBatch`), release the lock, and only then call `send` with the local copy. The shared state is locked for microseconds; the network call runs lock-free.

`send` is where idempotency comes in. It marshals the batch once, then loops: build a request, set the `Idempotency-Key` header to the batch's sequence number, and POST. On a 2xx it records metrics and returns. On a network error or non-2xx it retries with cancellable exponential backoff up to `MaxRetries`, then gives up with `ErrMaxRetries`. The key detail is that the sequence number is captured once, by `drainBatch`, for the whole logical batch — so every retry of that batch carries the *same* `Idempotency-Key`. The receiver can therefore recognize a retry as a duplicate of a batch it already applied and ignore it. A fresh key per attempt would defeat this entirely, which is why the key comes from the per-batch sequence counter and never from random data.

`Close` is careful about shutdown order. It cancels the flush loop's context and waits for the goroutine to exit (`<-done`) *before* draining the final batch, so the timer goroutine cannot race the final drain. It then sends whatever remains with a fresh `context.Background()` rather than the cancelled context, so the last batch is not abandoned by the very cancellation that stopped the loop.

Create `http_sink.go`:

```go
package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// HTTPSinkConfig holds configuration for HTTPSink.
type HTTPSinkConfig struct {
	// URL is the HTTP endpoint to POST batches to. Required.
	URL string
	// BatchSize triggers a synchronous flush when the batch reaches this many
	// records. Defaults to 100.
	BatchSize int
	// FlushInterval triggers a timer-based flush when this duration elapses.
	// Defaults to 5s.
	FlushInterval time.Duration
	// MaxRetries is the maximum POST retries per batch. Defaults to 3.
	MaxRetries int
	// RetryBackoff is the initial retry backoff; doubles each attempt, capped
	// at 30s. Defaults to 200ms.
	RetryBackoff time.Duration
	// Client is the HTTP client to use. Nil uses http.DefaultClient.
	Client *http.Client
}

// HTTPSink POSTs batches of records as a JSON array to a configured URL.
// Batches flush when the batch reaches BatchSize or FlushInterval elapses.
// Each request includes an Idempotency-Key header for deduplication.
type HTTPSink struct {
	cfg      HTTPSinkConfig
	mu       sync.Mutex
	batch    []Record
	batchSeq int64
	cancel   context.CancelFunc
	done     chan struct{}
	open     bool
	metrics  Metrics
}

// NewHTTPSink constructs an HTTPSink, applying defaults for unset fields.
func NewHTTPSink(cfg HTTPSinkConfig) (*HTTPSink, error) {
	if cfg.URL == "" {
		return nil, ErrEmptyURL
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 200 * time.Millisecond
	}
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	return &HTTPSink{cfg: cfg}, nil
}

// Metrics returns the live activity counters.
func (s *HTTPSink) Metrics() *Metrics { return &s.metrics }

// Open starts the background flush goroutine.
func (s *HTTPSink) Open(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open {
		return ErrAlreadyOpen
	}
	flushCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	s.open = true
	go s.flushLoop(flushCtx)
	return nil
}

// flushLoop sends partial batches every FlushInterval. It exits when the
// context passed to Open is cancelled (via Close).
func (s *HTTPSink) flushLoop(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(s.cfg.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = s.Flush(ctx) // best-effort; errors counted in metrics
		case <-ctx.Done():
			return
		}
	}
}

// Write appends records to the current batch. If the batch reaches BatchSize,
// it is sent synchronously before Write returns.
func (s *HTTPSink) Write(ctx context.Context, records []Record) error {
	s.mu.Lock()
	if !s.open {
		s.mu.Unlock()
		return ErrNotOpen
	}
	s.batch = append(s.batch, records...)
	if len(s.batch) < s.cfg.BatchSize {
		s.mu.Unlock()
		return nil
	}
	// Batch full: drain under lock, send without lock.
	batch, seq := s.drainBatch()
	s.mu.Unlock()
	return s.send(ctx, batch, seq)
}

// Flush sends any pending records immediately, regardless of batch size.
func (s *HTTPSink) Flush(ctx context.Context) error {
	s.mu.Lock()
	if !s.open {
		s.mu.Unlock()
		return ErrNotOpen
	}
	if len(s.batch) == 0 {
		s.mu.Unlock()
		return nil
	}
	batch, seq := s.drainBatch()
	s.mu.Unlock()
	return s.send(ctx, batch, seq)
}

// drainBatch extracts the current batch and advances the sequence counter.
// Callers must hold s.mu.
func (s *HTTPSink) drainBatch() ([]Record, int64) {
	batch := s.batch
	s.batch = nil
	seq := s.batchSeq
	s.batchSeq++
	return batch, seq
}

// send POSTs one batch as a JSON array. It retries with exponential backoff on
// network errors and non-2xx responses. The Idempotency-Key header carries the
// batch sequence number so the receiver can deduplicate retries of the same batch.
func (s *HTTPSink) send(ctx context.Context, batch []Record, seq int64) error {
	body, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("http sink: marshal: %w", err)
	}
	backoff := s.cfg.RetryBackoff
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.URL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("http sink: new request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("%d", seq))

		resp, doErr := s.cfg.Client.Do(req)
		if doErr == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				s.metrics.RecordsWritten.Add(int64(len(batch)))
				s.metrics.BytesWritten.Add(int64(len(body)))
				s.metrics.BatchesFlushed.Add(1)
				return nil
			}
			doErr = fmt.Errorf("status %d", resp.StatusCode)
		}
		if attempt >= s.cfg.MaxRetries {
			s.metrics.FlushErrors.Add(1)
			return fmt.Errorf("http sink: %w after %d attempts: %v", ErrMaxRetries, attempt, doErr)
		}
		s.metrics.Retries.Add(1)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = capDuration(backoff*2, 30*time.Second)
	}
}

// Close stops the background flush goroutine, drains any remaining batch, and
// marks the sink closed.
func (s *HTTPSink) Close() error {
	s.mu.Lock()
	if !s.open {
		s.mu.Unlock()
		return nil
	}
	cancel := s.cancel
	done := s.done
	s.mu.Unlock()

	// Stop the ticker goroutine first so it cannot race with our final drain.
	cancel()
	<-done

	// Drain whatever remains with a fresh context (not the cancelled one).
	s.mu.Lock()
	s.open = false
	batch, seq := s.drainBatch()
	s.mu.Unlock()

	if len(batch) > 0 {
		if err := s.send(context.Background(), batch, seq); err != nil {
			return fmt.Errorf("http sink close: %w", err)
		}
	}
	return nil
}
```

### The runnable demo

The demo spins up an `httptest` server that prints each batch it receives along with its idempotency key, then writes seven records with a batch size of three. The output shows two full batches of three and a final batch of one drained by `Close`, with sequence keys 0, 1, 2.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"example.com/http-sink-batching-idempotency"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

func run() error {
	received := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []sink.Record
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received += len(batch)
		fmt.Printf("server: batch of %d (Idempotency-Key %s)\n",
			len(batch), r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hs, err := sink.NewHTTPSink(sink.HTTPSinkConfig{
		URL:           srv.URL,
		BatchSize:     3,
		FlushInterval: time.Hour,
	})
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := hs.Open(ctx); err != nil {
		return err
	}
	for i := 0; i < 7; i++ {
		if err := hs.Write(ctx, []sink.Record{{
			Key: []byte(fmt.Sprintf("k%d", i)),
		}}); err != nil {
			return err
		}
	}
	if err := hs.Close(); err != nil {
		return err
	}
	m := hs.Metrics()
	fmt.Printf("total records received: %d\n", received)
	fmt.Printf("batches=%d retries=%d\n", m.BatchesFlushed.Load(), m.Retries.Load())
	return nil
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
server: batch of 3 (Idempotency-Key 0)
server: batch of 3 (Idempotency-Key 1)
server: batch of 1 (Idempotency-Key 2)
total records received: 7
batches=3 retries=0
```

### Tests

`TestHTTPSinkBatchesOnSize` writes two records into a batch of two (size trigger fires) plus a third that needs an explicit `Flush`, and asserts the server saw exactly two batches of the right sizes. `TestHTTPSinkSetsIdempotencyKey` proves consecutive batches get *different* keys. `TestHTTPSinkRetryReusesIdempotencyKey` is the important one: a server that fails the first attempt and succeeds the second records the keys it saw, and the test asserts every retry of the one batch carried the *same* key — the property the whole dedup scheme depends on. `TestHTTPSinkRetriesOn5xx` and `TestHTTPSinkExhaustsRetries` cover the transient-then-success and permanent-failure paths.

Create `http_sink_test.go`:

```go
package sink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestHTTPSinkBatchesOnSize(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		received [][]Record
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []Record
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, batch)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hs, err := NewHTTPSink(HTTPSinkConfig{
		URL:           srv.URL,
		BatchSize:     2,
		FlushInterval: time.Hour, // disable timer; rely on size trigger
		MaxRetries:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := hs.Open(ctx); err != nil {
		t.Fatal(err)
	}

	if err := hs.Write(ctx, []Record{{Key: []byte("k1")}, {Key: []byte("k2")}}); err != nil {
		t.Fatal(err)
	}
	if err := hs.Write(ctx, []Record{{Key: []byte("k3")}}); err != nil {
		t.Fatal(err)
	}
	if err := hs.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := hs.Close(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("got %d batches, want 2", len(received))
	}
	if got := len(received[0]); got != 2 {
		t.Fatalf("batch[0]: got %d records, want 2", got)
	}
	if got := len(received[1]); got != 1 {
		t.Fatalf("batch[1]: got %d records, want 1", got)
	}
}

func TestHTTPSinkSetsIdempotencyKey(t *testing.T) {
	t.Parallel()

	var (
		mu   sync.Mutex
		keys []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hs, _ := NewHTTPSink(HTTPSinkConfig{
		URL: srv.URL, BatchSize: 1, FlushInterval: time.Hour, MaxRetries: 1,
	})
	ctx := context.Background()
	_ = hs.Open(ctx)
	_ = hs.Write(ctx, []Record{{Key: []byte("a")}})
	_ = hs.Write(ctx, []Record{{Key: []byte("b")}})
	_ = hs.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 {
		t.Fatalf("got %d requests, want 2", len(keys))
	}
	if keys[0] == keys[1] {
		t.Fatalf("idempotency keys must differ across batches: both %q", keys[0])
	}
}

// TestHTTPSinkRetryReusesIdempotencyKey verifies that all retries of one
// logical batch carry the SAME Idempotency-Key, so the receiver can recognise
// the retry as a duplicate. A flaky server fails once, then succeeds.
func TestHTTPSinkRetryReusesIdempotencyKey(t *testing.T) {
	t.Parallel()

	var (
		mu   sync.Mutex
		keys []string
	)
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		attempts++
		first := attempts == 1
		mu.Unlock()
		if first {
			http.Error(w, "transient", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hs, _ := NewHTTPSink(HTTPSinkConfig{
		URL: srv.URL, BatchSize: 1, FlushInterval: time.Hour,
		MaxRetries: 3, RetryBackoff: time.Millisecond,
	})
	ctx := context.Background()
	_ = hs.Open(ctx)
	if err := hs.Write(ctx, []Record{{Key: []byte("k")}}); err != nil {
		t.Fatalf("Write: %v (want success after retry)", err)
	}
	_ = hs.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(keys) < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", len(keys))
	}
	for i, k := range keys {
		if k != keys[0] {
			t.Fatalf("retry %d used key %q, want same as first %q", i, k, keys[0])
		}
	}
}

func TestHTTPSinkRejectsEmptyURL(t *testing.T) {
	t.Parallel()

	_, err := NewHTTPSink(HTTPSinkConfig{})
	if !errors.Is(err, ErrEmptyURL) {
		t.Fatalf("err = %v, want ErrEmptyURL", err)
	}
}

func TestHTTPSinkRetriesOn5xx(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 2 {
			http.Error(w, "transient", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hs, _ := NewHTTPSink(HTTPSinkConfig{
		URL:           srv.URL,
		BatchSize:     1,
		FlushInterval: time.Hour,
		MaxRetries:    3,
		RetryBackoff:  time.Millisecond,
	})
	ctx := context.Background()
	_ = hs.Open(ctx)
	if err := hs.Write(ctx, []Record{{Key: []byte("k")}}); err != nil {
		t.Fatalf("Write: %v (want success after retry)", err)
	}
	_ = hs.Close()

	mu.Lock()
	defer mu.Unlock()
	if attempts < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", attempts)
	}
	if got := hs.metrics.Retries.Load(); got == 0 {
		t.Fatal("Retries counter should be non-zero")
	}
}

func TestHTTPSinkExhaustsRetries(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	hs, _ := NewHTTPSink(HTTPSinkConfig{
		URL: srv.URL, BatchSize: 1, FlushInterval: time.Hour,
		MaxRetries: 2, RetryBackoff: time.Millisecond,
	})
	ctx := context.Background()
	_ = hs.Open(ctx)
	err := hs.Write(ctx, []Record{{Key: []byte("k")}})
	if !errors.Is(err, ErrMaxRetries) {
		t.Fatalf("err = %v, want ErrMaxRetries", err)
	}
	_ = hs.Close()
}
```

## Review

The sink is correct when batching bounds both memory and latency and retries never break idempotency. Confirm the batch is drained under the lock and sent without it, so concurrent `Write` calls do not serialize behind the network. Confirm the `Idempotency-Key` is captured per logical batch by `drainBatch` and reused across every retry — the retry-key-reuse test is the one that catches a regression here. Confirm `Close` stops the timer goroutine and joins it before the final drain, and sends the last batch with a fresh context. The classic mistakes are holding the mutex across `Client.Do` (throughput collapses), generating a new key per attempt (the server double-processes retries), and returning from `Close` without joining the flush goroutine (a leaked goroutine that races the next sink). The suite passing under `go test -race ./...` establishes these properties.

## Resources

- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — hermetic HTTP testing without an external server.
- [`http.NewRequestWithContext`](https://pkg.go.dev/net/http#NewRequestWithContext) — context-aware requests so retries respect cancellation.
- [Stripe idempotency keys](https://stripe.com/docs/api/idempotent_requests) — the canonical HTTP idempotency-key pattern this exercise mirrors.
- [IETF draft: The Idempotency-Key HTTP Header Field](https://datatracker.ietf.org/doc/draft-ietf-httpapi-idempotency-key-header/) — the in-progress standardization of the header.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-tcp-sink-reconnection.md](02-tcp-sink-reconnection.md) | Next: [04-idempotent-upsert-sink.md](04-idempotent-upsert-sink.md)
