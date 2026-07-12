# Exercise 3: HTTP Source

Some origins speak a protocol with an explicit backpressure signal. This `HTTPSource` accepts POST requests, emits each body as a `Record`, and — instead of blocking or silently dropping when its buffer is full — returns `429 Too Many Requests` so the client can back off and retry.

Every module in this lesson is fully self-contained: it begins with its own `go mod init`, bundles the shared `Record`, `Metrics`, and `Source` definitions it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
http-source/
  go.mod
  source.go             Record, Metrics, Source, ErrSourceClosed, ErrBufferFull
  http_source.go        HTTPSource: http.Server, non-blocking send, 429 on full
  http_source_test.go   202 accept, 429 when full, 405 non-POST, Example
  cmd/demo/main.go      POST one body and print the emitted record
```

- Files: `source.go`, `http_source.go`, `http_source_test.go`, `cmd/demo/main.go`.
- Implement: `HTTPSource` with `Open`/`Close`/`Metrics`, `Addr()`, and a handler that returns 202, 429, or 405.
- Test: a POST body arrives as a record with 202; a full buffer yields 429; a GET yields 405; an `Example` documents the handler via `httptest`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/43-capstone-stream-processing-engine/01-source-connectors/03-http-source/cmd/demo && cd go-solutions/43-capstone-stream-processing-engine/01-source-connectors/03-http-source
```

### Two sentinels this time

`source.go` adds `ErrBufferFull` alongside `ErrSourceClosed`. The HTTP source is the one place in this lesson where a full buffer is a first-class, reportable condition rather than a reason to block, so it deserves a named error whose text the handler returns in the 429 body.

Create `source.go`:

```go
package httpsource

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
var ErrSourceClosed = errors.New("httpsource: source not open")

// ErrBufferFull is returned (wrapped) when a non-blocking send fails.
var ErrBufferFull = errors.New("httpsource: output buffer full")

// Source is the common interface for all data origins.
type Source interface {
	Open(ctx context.Context) (<-chan Record, <-chan error)
	Close() error
	Metrics() Metrics
}
```

### The non-blocking send and graceful server shutdown

`Open` binds a listener exactly like the TCP source (`:0` for an OS-chosen port, surfaced by `Addr()`), then serves an `http.Server` whose handler is `hs.handle`. Two coordination goroutines bracket the server: one calls `server.Serve(ln)` under the `WaitGroup` and treats `http.ErrServerClosed` as the normal stop signal rather than an error; another waits on `inner.Done()` and then calls `server.Shutdown` with a timeout, so cancelling the context drains in-flight requests and unblocks `Serve`. The familiar closer goroutine waits and closes the channels once.

`handle` is where the backpressure decision lives. It rejects non-POST methods with 405, reads the body under a 1 MiB `io.LimitReader` cap (so a hostile client cannot exhaust memory), then performs a *non-blocking* send: `select { case hs.records <- rec: ...202; default: ...429 }`. This is the third backpressure strategy from the concepts file — reject — and it is the right one precisely because HTTP clients understand 429 and can retry. A blocking send here would stall the server's request goroutine and tie up a connection; a silent drop would lie to the client with a 202 it did not earn. The 429 tells the truth, and the client owns the retry.

Create `http_source.go`:

```go
package httpsource

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// HTTPSource listens for HTTP POST requests and emits each request body as a Record.
// Returns 202 Accepted on success and 429 Too Many Requests when the buffer is full.
type HTTPSource struct {
	addr       string
	bufferSize int

	server  *http.Server
	ln      net.Listener
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	records chan Record
	errs    chan error

	emitted atomic.Int64
	bytes   atomic.Int64
	errCnt  atomic.Int64
	backlog atomic.Int64
}

// NewHTTPSource creates an HTTPSource that listens on addr (e.g. "127.0.0.1:8080").
func NewHTTPSource(addr string, bufferSize int) *HTTPSource {
	return &HTTPSource{addr: addr, bufferSize: bufferSize}
}

// Addr returns the actual listen address.
func (hs *HTTPSource) Addr() string {
	if hs.ln == nil {
		return hs.addr
	}
	return hs.ln.Addr().String()
}

func (hs *HTTPSource) Open(ctx context.Context) (<-chan Record, <-chan error) {
	inner, cancel := context.WithCancel(ctx)
	hs.cancel = cancel
	hs.records = make(chan Record, hs.bufferSize)
	hs.errs = make(chan error, 16)

	ln, err := net.Listen("tcp", hs.addr)
	if err != nil {
		cancel()
		close(hs.records)
		close(hs.errs)
		return hs.records, hs.errs
	}
	hs.ln = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/", hs.handle)
	hs.server = &http.Server{Handler: mux}

	hs.wg.Add(1)
	go func() {
		defer hs.wg.Done()
		if err := hs.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			hs.errCnt.Add(1)
			select {
			case hs.errs <- fmt.Errorf("httpsource: serve: %w", err):
			default:
			}
		}
	}()

	go func() {
		<-inner.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		hs.server.Shutdown(shutCtx) //nolint:errcheck
	}()

	go func() {
		hs.wg.Wait()
		close(hs.records)
		close(hs.errs)
	}()

	return hs.records, hs.errs
}

func (hs *HTTPSource) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB max
	if err != nil {
		hs.errCnt.Add(1)
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	hs.bytes.Add(int64(len(body)))

	rec := Record{
		Value:     body,
		Timestamp: time.Now().UTC(),
		Source:    "http",
		Metadata:  map[string]string{"remote": r.RemoteAddr},
	}

	select {
	case hs.records <- rec:
		hs.emitted.Add(1)
		hs.backlog.Store(int64(len(hs.records)))
		w.WriteHeader(http.StatusAccepted)
	default:
		hs.errCnt.Add(1)
		hs.backlog.Store(int64(len(hs.records)))
		http.Error(w, ErrBufferFull.Error(), http.StatusTooManyRequests)
	}
}

func (hs *HTTPSource) Close() error {
	if hs.cancel == nil {
		return ErrSourceClosed
	}
	hs.cancel()
	hs.wg.Wait()
	return nil
}

func (hs *HTTPSource) Metrics() Metrics {
	return Metrics{
		RecordsEmitted: hs.emitted.Load(),
		BytesRead:      hs.bytes.Load(),
		ErrorsTotal:    hs.errCnt.Load(),
		BacklogSize:    hs.backlog.Load(),
	}
}

var _ Source = (*HTTPSource)(nil)
```

### The runnable demo

The demo opens the source, POSTs one body, prints the status code (202) and the record that arrives.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	hsrc "example.com/http-source"
)

func main() {
	hs := hsrc.NewHTTPSource("127.0.0.1:0", 16)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	recs, _ := hs.Open(ctx)

	resp, _ := http.Post(
		"http://"+hs.Addr()+"/",
		"text/plain",
		strings.NewReader("line-from-http"),
	)
	resp.Body.Close()
	fmt.Printf("HTTPSource POST status: %d\n", resp.StatusCode)

	r := <-recs
	fmt.Printf("HTTPSource emitted: %s\n", r.Value)
	hs.Close()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
HTTPSource POST status: 202
HTTPSource emitted: line-from-http
```

### Tests

`TestHTTPSourceAccepts` POSTs a body and asserts a 202 and the matching record. `TestHTTPSourceReturns429WhenFull` opens a source with a buffer of 1, deliberately never drains it, sends one POST to fill the buffer, and asserts the second POST gets 429 — the backpressure contract, proven end to end. `TestHTTPSourceRejectsNonPOST` asserts a GET gets 405. `ExampleNewHTTPSource` drives the handler directly with `httptest.NewRecorder`, so the documented output is verified by `go test` without binding a real port.

Create `http_source_test.go`:

```go
package httpsource

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestHTTPSourceAccepts(t *testing.T) {
	t.Parallel()

	hs := NewHTTPSource("127.0.0.1:0", 16)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	recs, _ := hs.Open(ctx)

	url := "http://" + hs.Addr() + "/"
	resp, err := http.Post(url, "text/plain", strings.NewReader("event-payload"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	got := drain(recs, 1, time.Second)
	if len(got) != 1 {
		t.Fatal("no record received")
	}
	if string(got[0].Value) != "event-payload" {
		t.Errorf("value = %q, want %q", got[0].Value, "event-payload")
	}
	hs.Close()
}

func TestHTTPSourceReturns429WhenFull(t *testing.T) {
	t.Parallel()

	hs := NewHTTPSource("127.0.0.1:0", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	recs, _ := hs.Open(ctx)
	_ = recs

	url := "http://" + hs.Addr() + "/"

	resp1, err := http.Post(url, "text/plain", strings.NewReader("first"))
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()

	resp2, err := http.Post(url, "text/plain", strings.NewReader("second"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp2.StatusCode)
	}

	hs.Close()
}

func TestHTTPSourceRejectsNonPOST(t *testing.T) {
	t.Parallel()

	hs := NewHTTPSource("127.0.0.1:0", 8)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = hs.Open(ctx)

	resp, err := http.Get("http://" + hs.Addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	hs.Close()
}

func ExampleNewHTTPSource() {
	hs := NewHTTPSource("127.0.0.1:0", 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	recs, _ := hs.Open(ctx)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
	w := httptest.NewRecorder()
	hs.handle(w, req)
	fmt.Println(w.Code)

	r := <-recs
	fmt.Println(string(r.Value))
	hs.Close()
	// Output:
	// 202
	// hello
}
```

## Review

The source is correct when a full buffer produces a 429 rather than a stall or a silent drop, and when cancelling the context cleanly shuts the server down. Confirm the send is non-blocking (`default` arm), the body read is capped by `io.LimitReader`, and `Serve` treats `http.ErrServerClosed` as normal. The common mistakes are using a blocking send in the handler (which stalls request goroutines and exhausts connections), reading the body without a size limit (a memory-exhaustion vector), and forgetting that `Serve` returns `ErrServerClosed` on a clean shutdown and reporting it as a spurious error. The 429 test under `-race` confirms both the backpressure behaviour and the safety of the shared counters.

## Resources

- [`net/http.Server`](https://pkg.go.dev/net/http#Server) — `Serve`, `Shutdown`, and `ErrServerClosed`, the exact lifecycle this source uses.
- [HTTP 429 Too Many Requests (MDN)](https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/429) — the status code's semantics and the `Retry-After` convention behind this backpressure design.
- [`io.LimitReader`](https://pkg.go.dev/io#LimitReader) — bounding request-body reads to defend against memory exhaustion.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-tcp-source.md](02-tcp-source.md) | Next: [04-multi-source.md](04-multi-source.md)
