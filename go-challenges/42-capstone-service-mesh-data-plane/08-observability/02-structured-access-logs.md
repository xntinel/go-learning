# Exercise 2: Structured Access Logs

Where a metric drops dimensions to stay cheap, an access log keeps them: one machine-parseable record per request carrying the path, client, upstream, status, latency, and the trace id that joins this line to its distributed trace. This exercise builds a concurrency-safe JSON Lines access logger from scratch — a fixed-schema record type and a mutex-guarded writer that emits one valid JSON object per line, no matter how many request goroutines call it at once.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
accesslog.go         Entry (fixed JSON schema), Logger (mutex-guarded ndjson writer)
cmd/
  demo/
    main.go          log a deterministic batch of proxied requests as ndjson
accesslog_test.go    field/type assertions, ndjson line count, duration math, concurrent well-formedness
```

- Files: `accesslog.go`, `cmd/demo/main.go`, `accesslog_test.go`.
- Implement: `Entry` with stable JSON tags, `Logger` with `Log(Entry) error` and a `LogRequest(...)` convenience builder.
- Test: parse the emitted JSON back, assert fields and types, count ndjson lines, and confirm 200 concurrent goroutines never interleave a line (under `-race`).
- Verify: `go test -race -count=1 ./...`

### Why JSON Lines, a fixed struct, and one mutex

Three design choices make this logger useful rather than just convenient. The first is the format: one JSON object per line, the format called JSON Lines or newline-delimited JSON (ndjson). A log aggregator ingests it by splitting on `\n` and parsing each line independently — no streaming JSON parser, no array brackets to balance across a file that is still being appended to — and the same property lets `grep` and `jq` work a line at a time. The second is the schema: a fixed Go struct with explicit `json` tags rather than a `map[string]any`. `encoding/json` marshals struct fields in declaration order, so the field order on the wire is stable and the types are fixed at compile time; a map would give you neither, and a downstream schema that drifts field order or flips a number to a string breaks every consumer. The third is concurrency: the proxy calls the logger from one goroutine per in-flight request, and if two `Encode` calls were allowed to interleave their bytes on the same underlying writer, the result would be one corrupt line that no JSON parser accepts. A single mutex held across the encode-and-write makes each line atomic. That lock looks like a bottleneck but is not: it is held for the microseconds of a buffer write, which is negligible beside the network and upstream-call work the proxy is already doing per request.

The schema deliberately carries the dimensions a metric cannot afford as labels. The full path, the client IP, and above all the trace id are high-cardinality: as metric labels they would explode the time-series count, but as fields on a once-per-request log line they cost exactly one record each, which is the price a log is designed to pay. The trace id is the field that earns the logger its place next to the metrics and the tracer: it is the join key that turns an alert on a latency metric into a query for the exact slow requests, and each of those into the trace that shows where the time went.

### Duration as a number, not a string

Latency is stored as `duration_ms`, a `float64` of milliseconds with microsecond resolution, computed as `dur.Microseconds() / 1000.0`. Keeping it numeric (rather than formatting `time.Duration`'s `"4.2ms"` string) lets the log backend do range queries and aggregations — "all requests over 100 ms" — without re-parsing. Microsecond resolution is enough for a proxy hop and avoids the noise of nanosecond floats. The convenience builder `LogRequest` does this conversion so call sites pass a `time.Duration` and never hand-roll the arithmetic.

Create `accesslog.go`:

```go
package accesslog

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Entry is one structured access-log record for a single proxied request.
// Field order in the struct is the field order in the emitted JSON object,
// because encoding/json marshals struct fields in declaration order.
type Entry struct {
	Method     string  `json:"method"`
	Path       string  `json:"path"`
	Upstream   string  `json:"upstream"`
	ClientIP   string  `json:"client_ip"`
	Status     int     `json:"status"`
	Bytes      int64   `json:"bytes"`
	DurationMS float64 `json:"duration_ms"`
	TraceID    string  `json:"trace_id"`
}

// Logger writes Entry values as newline-delimited JSON (ndjson) to an
// io.Writer. A single mutex serializes writes so the logger is safe for
// concurrent use from every request goroutine in the proxy.
type Logger struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewLogger returns a Logger that writes ndjson to w.
func NewLogger(w io.Writer) *Logger {
	return &Logger{enc: json.NewEncoder(w)}
}

// Log writes e as one JSON object followed by a newline. json.Encoder.Encode
// already appends the newline, producing valid ndjson.
func (l *Logger) Log(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enc.Encode(&e)
}

// LogRequest is a convenience builder: it converts a request's parts plus a
// measured latency into an Entry and logs it. The duration is recorded in
// milliseconds with microsecond resolution.
func (l *Logger) LogRequest(method, path, upstream, clientIP string, status int, bytes int64, dur time.Duration, traceID string) error {
	return l.Log(Entry{
		Method:     method,
		Path:       path,
		Upstream:   upstream,
		ClientIP:   clientIP,
		Status:     status,
		Bytes:      bytes,
		DurationMS: float64(dur.Microseconds()) / 1000.0,
		TraceID:    traceID,
	})
}
```

`json.NewEncoder(w).Encode` is the quiet workhorse here: it marshals the value and writes a trailing `\n`, so a sequence of `Encode` calls is already valid ndjson with no manual newline handling. The mutex wraps that single call, which is why a line is emitted atomically even under heavy concurrency.

### The runnable demo

The demo logs a fixed batch of three proxied requests — two sharing a trace id (the same logical request fanning out to two upstreams) and one belonging to a different trace that failed with a 503. Fixed inputs make the ndjson output exact and reproducible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"
	"time"

	"example.com/access-log"
)

func main() {
	lg := accesslog.NewLogger(os.Stdout)

	// A fixed, deterministic batch of proxied requests. Each carries the trace
	// id that the proxy extracted from the inbound traceparent header, so a log
	// aggregator can join an access line to its distributed trace.
	lg.LogRequest("GET", "/api/users", "svc-a", "10.0.0.7", 200, 1432, 4200*time.Microsecond, "4bf92f3577b34da6a3ce929d0e0e4736")
	lg.LogRequest("POST", "/api/orders", "svc-b", "10.0.0.7", 201, 96, 11500*time.Microsecond, "4bf92f3577b34da6a3ce929d0e0e4736")
	lg.LogRequest("GET", "/api/inventory", "svc-c", "10.0.0.9", 503, 0, 250*time.Microsecond, "00f067aa0ba902b7d3ce929d0e0e4736")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"method":"GET","path":"/api/users","upstream":"svc-a","client_ip":"10.0.0.7","status":200,"bytes":1432,"duration_ms":4.2,"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"}
{"method":"POST","path":"/api/orders","upstream":"svc-b","client_ip":"10.0.0.7","status":201,"bytes":96,"duration_ms":11.5,"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"}
{"method":"GET","path":"/api/inventory","upstream":"svc-c","client_ip":"10.0.0.9","status":503,"bytes":0,"duration_ms":0.25,"trace_id":"00f067aa0ba902b7d3ce929d0e0e4736"}
```

### Tests

The tests pin the schema and the concurrency guarantee. `TestLogEmitsExpectedFields` round-trips one entry through `json.Unmarshal` and asserts the status, trace id, and numeric duration survive. `TestLogIsNDJSON` confirms three `Log` calls produce three newline-separated lines. `TestLogRequestComputesDuration` checks the microsecond-to-millisecond math. The important one is `TestLoggerConcurrentWritesAreWellFormed`: 200 goroutines logging 10 entries each, then every emitted line is parsed independently — if the mutex were missing, the race detector and the JSON parser would both fail on an interleaved line.

Create `accesslog_test.go`:

```go
package accesslog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLogEmitsExpectedFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	lg := NewLogger(&buf)

	err := lg.Log(Entry{
		Method:     "GET",
		Path:       "/api/users",
		Upstream:   "svc-a",
		ClientIP:   "10.0.0.7",
		Status:     200,
		Bytes:      1432,
		DurationMS: 4.2,
		TraceID:    "4bf92f3577b34da6a3ce929d0e0e4736",
	})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}

	var got Entry
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if got.Status != 200 {
		t.Errorf("status = %d, want 200", got.Status)
	}
	if got.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace_id = %q, want propagated id", got.TraceID)
	}
	if got.DurationMS != 4.2 {
		t.Errorf("duration_ms = %g, want 4.2", got.DurationMS)
	}
}

func TestLogIsNDJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	lg := NewLogger(&buf)
	for i := 0; i < 3; i++ {
		if err := lg.Log(Entry{Method: "GET", Path: "/", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Count(strings.TrimRight(buf.String(), "\n"), "\n") + 1
	if lines != 3 {
		t.Fatalf("got %d lines, want 3 (one JSON object per line)", lines)
	}
}

func TestLogRequestComputesDuration(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	lg := NewLogger(&buf)
	if err := lg.LogRequest("POST", "/login", "svc-auth", "10.0.0.9", 201, 64, 3500*time.Microsecond, "abc"); err != nil {
		t.Fatal(err)
	}
	var got Entry
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.DurationMS != 3.5 {
		t.Errorf("duration_ms = %g, want 3.5 (3500us)", got.DurationMS)
	}
}

func TestLoggerConcurrentWritesAreWellFormed(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	lg := NewLogger(&buf)

	const goroutines = 200
	const each = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				_ = lg.Log(Entry{Method: "GET", Path: "/x", Status: 200, TraceID: "t"})
			}
		}()
	}
	wg.Wait()

	// Every line must be independently parseable: the mutex prevents two
	// concurrent Encode calls from interleaving bytes on the same line.
	sc := bufio.NewScanner(&buf)
	n := 0
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %d not valid JSON: %v\n%s", n, err, sc.Text())
		}
		n++
	}
	if n != goroutines*each {
		t.Fatalf("got %d lines, want %d", n, goroutines*each)
	}
}
```

## Review

The logger is correct when every line is independently valid JSON and the schema does not drift. The schema stability comes from the fixed struct: `encoding/json` emits fields in declaration order, so as long as the struct's field order and tags are unchanged, every consumer sees the same shape — a `map[string]any` would forfeit that. The concurrency correctness comes from holding the mutex across `enc.Encode`, not around some narrower region; the failure mode it prevents is two goroutines interleaving bytes into one unparseable line, which `TestLoggerConcurrentWritesAreWellFormed` catches by parsing every line back under `-race`. Watch two mistakes. The first is computing the duration as a formatted string (`dur.String()`), which forces the log backend to re-parse `"4.2ms"` for every range query; keep it a number. The second is forgetting that `json.Encoder.Encode` already appends a newline — adding your own produces blank lines that break a strict ndjson reader. The trace id field is what ties this back to the other two pillars: it must be the same identifier the tracer propagates, so a log query and a trace lookup land on the same request.

## Resources

- [JSON Lines (ndjson) format](https://jsonlines.org/) — the one-object-per-line convention this logger emits and the rationale for using it for streaming records.
- [`encoding/json`](https://pkg.go.dev/encoding/json) — `json.Encoder`, struct field tags, and the declaration-order marshaling that gives the schema its stability.
- [Go blog: Structured Logging with slog](https://go.dev/blog/slog) — the standard library's production structured-logging package, the natural next step once a hand-rolled access logger needs levels, grouping, and handlers.

---

Back to [01-metrics-pipeline.md](01-metrics-pipeline.md) | Next: [03-trace-context-propagation.md](03-trace-context-propagation.md)
