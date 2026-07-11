# 14. HTTP Client: Retry, Circuit Breaker, and Tracing

Build a reusable HTTP client package that composes retry, circuit breaker, and tracing behavior with `http.RoundTripper` decorators. Use only the standard library.

## Concepts

The official `net/http` documentation says `http.Client` and `http.Transport` are safe for concurrent use and should be reused. A client delegates one request attempt to its `Transport`, whose `RoundTrip` method returns a response or an error. This makes `http.RoundTripper` a natural boundary for small middleware-like decorators.

The official `net/http/httptest` documentation provides `httptest.NewServer`, which starts a real HTTP server on a loopback port. Use it for end-to-end client tests instead of mocking `http.Client` itself.

The stack in this exercise is `TracingTransport -> RetryTransport -> CircuitBreakerTransport -> http.DefaultTransport`. Tracing creates one top-level trace and passes it through the request context. Retry records each attempt. The circuit breaker sits inside retry so repeated failed attempts can open the circuit, and an already open circuit returns a wrapped sentinel error that callers can check with `errors.Is`.

## Exercises

Create this module layout:

```text
retrybreaker/
  go.mod
  client.go
  client_example_test.go
  client_test.go
  cmd/demo/main.go
```

Create `go.mod`:

```go
module example.com/retrybreaker

go 1.26
```

Create `client.go`:

```go
package retrybreaker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrCircuitOpen   = errors.New("circuit open")
	ErrRequestFailed = errors.New("request failed")
)

type Config struct {
	MaxRetries       int
	BaseDelay        time.Duration
	FailureThreshold int
	OpenTimeout      time.Duration
	AllowUnsafeRetry bool
	TraceLogSize     int
	Base             http.RoundTripper
}

type Option func(*Config)

func WithBaseTransport(base http.RoundTripper) Option {
	return func(cfg *Config) { cfg.Base = base }
}

func WithRetry(max int, baseDelay time.Duration) Option {
	return func(cfg *Config) {
		cfg.MaxRetries = max
		cfg.BaseDelay = baseDelay
	}
}

func WithCircuitBreaker(threshold int, openTimeout time.Duration) Option {
	return func(cfg *Config) {
		cfg.FailureThreshold = threshold
		cfg.OpenTimeout = openTimeout
	}
}

func defaultConfig() Config {
	return Config{
		MaxRetries:       3,
		BaseDelay:        100 * time.Millisecond,
		FailureThreshold: 5,
		OpenTimeout:      30 * time.Second,
		TraceLogSize:     100,
		Base:             http.DefaultTransport,
	}
}

func NewResilientClient(opts ...Option) (*http.Client, *TraceLog) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.Base == nil {
		cfg.Base = http.DefaultTransport
	}
	if cfg.TraceLogSize <= 0 {
		cfg.TraceLogSize = 100
	}

	log := NewTraceLog(cfg.TraceLogSize)
	breaker := &CircuitBreakerTransport{
		Base:      cfg.Base,
		Threshold: cfg.FailureThreshold,
		Timeout:   cfg.OpenTimeout,
		Breakers:  make(map[string]*CircuitBreaker),
	}
	retry := &RetryTransport{
		Base:             breaker,
		MaxRetries:       cfg.MaxRetries,
		BaseDelay:        cfg.BaseDelay,
		AllowUnsafeRetry: cfg.AllowUnsafeRetry,
	}
	tracing := &TracingTransport{Base: retry, Log: log}
	return &http.Client{Transport: tracing}, log
}

func GetOK(ctx context.Context, client *http.Client, url string) error {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("GET %s returned %d: %w", url, resp.StatusCode, ErrRequestFailed)
	}
	return nil
}

type RetryTransport struct {
	Base             http.RoundTripper
	MaxRetries       int
	BaseDelay        time.Duration
	AllowUnsafeRetry bool
}

func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	maxRetries := t.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	baseDelay := t.BaseDelay
	if baseDelay <= 0 {
		baseDelay = 100 * time.Millisecond
	}
	retryableMethod := t.AllowUnsafeRetry || isIdempotent(req.Method)

	var lastErr error
	var lastResp *http.Response
	for attempt := 1; attempt <= maxRetries+1; attempt++ {
		if attempt > 1 {
			if err := sleep(req.Context(), retryDelay(lastResp, baseDelay, attempt-2)); err != nil {
				return nil, fmt.Errorf("retry wait canceled: %w", err)
			}
		}

		attemptReq := req.Clone(req.Context())
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("clone request body: %w", err)
			}
			attemptReq.Body = body
		}

		start := time.Now()
		resp, err := base.RoundTrip(attemptReq)
		recordAttempt(req.Context(), attempt, statusCode(resp), time.Since(start), err)
		if err == nil && !shouldRetryStatus(resp.StatusCode) {
			return resp, nil
		}
		if !retryableMethod || errors.Is(err, ErrCircuitOpen) || attempt == maxRetries+1 {
			if err != nil {
				return nil, fmt.Errorf("attempt %d failed: %w", attempt, err)
			}
			return resp, nil
		}
		if resp != nil && resp.Body != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		lastResp = resp
		lastErr = err
	}
	return nil, fmt.Errorf("exhausted retries: %w: %v", ErrRequestFailed, lastErr)
}

func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func shouldRetryStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
}

func retryDelay(resp *http.Response, base time.Duration, attempt int) time.Duration {
	if resp != nil {
		if delay := retryAfter(resp.Header.Get("Retry-After")); delay > 0 {
			return delay
		}
	}
	delay := base << attempt
	return delay + time.Duration(rand.Int64N(int64(delay/2)+1))
}

func retryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	return time.Until(when)
}

func sleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func statusCode(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

type CircuitBreakerTransport struct {
	Base      http.RoundTripper
	Threshold int
	Timeout   time.Duration
	Mu        sync.Mutex
	Breakers  map[string]*CircuitBreaker
}

func (t *CircuitBreakerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	breaker := t.breaker(req.URL.Host)
	if !breaker.allow() {
		return nil, fmt.Errorf("%s: %w", req.URL.Host, ErrCircuitOpen)
	}
	resp, err := base.RoundTrip(req)
	if err != nil || shouldRetryStatus(statusCode(resp)) {
		breaker.fail()
		return resp, err
	}
	breaker.succeed()
	return resp, nil
}

func (t *CircuitBreakerTransport) breaker(host string) *CircuitBreaker {
	t.Mu.Lock()
	defer t.Mu.Unlock()
	if t.Breakers == nil {
		t.Breakers = make(map[string]*CircuitBreaker)
	}
	breaker := t.Breakers[host]
	if breaker == nil {
		threshold := t.Threshold
		if threshold <= 0 {
			threshold = 5
		}
		timeout := t.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		breaker = &CircuitBreaker{threshold: threshold, timeout: timeout, state: StateClosed}
		t.Breakers[host] = breaker
	}
	return breaker
}

type CircuitState string

const (
	StateClosed   CircuitState = "closed"
	StateOpen     CircuitState = "open"
	StateHalfOpen CircuitState = "half-open"
)

type CircuitBreaker struct {
	mu          sync.Mutex
	state       CircuitState
	failures    int
	threshold   int
	timeout     time.Duration
	openedAt    time.Time
	probeActive bool
}

func (b *CircuitBreaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if time.Since(b.openedAt) < b.timeout {
			return false
		}
		b.state = StateHalfOpen
		b.probeActive = true
		return true
	case StateHalfOpen:
		if b.probeActive {
			return false
		}
		b.probeActive = true
		return true
	default:
		return true
	}
}

func (b *CircuitBreaker) fail() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probeActive = false
	b.failures++
	if b.state == StateHalfOpen || b.failures >= b.threshold {
		b.state = StateOpen
		b.openedAt = time.Now()
	}
}

func (b *CircuitBreaker) succeed() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = StateClosed
	b.failures = 0
	b.probeActive = false
}

type TracingTransport struct {
	Base http.RoundTripper
	Log  *TraceLog
}

func (t *TracingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	log := t.Log
	if log == nil {
		log = NewTraceLog(100)
	}
	trace := &Trace{
		ID:     nextTraceID(),
		Method: req.Method,
		URL:    req.URL.String(),
		Start:  time.Now(),
	}
	ctx := context.WithValue(req.Context(), traceKey{}, trace)
	attemptReq := req.Clone(ctx)
	attemptReq.Header.Set("X-Request-ID", trace.ID)
	resp, err := base.RoundTrip(attemptReq)
	trace.End = time.Now()
	trace.Status = statusCode(resp)
	if err != nil {
		trace.Error = err.Error()
	} else {
		trace.Error = ""
	}
	log.Add(*trace)
	return resp, err
}

type traceKey struct{}

type Attempt struct {
	Number  int
	Status  int
	Latency time.Duration
	Error   string
}

type Trace struct {
	ID       string
	Method   string
	URL      string
	Start    time.Time
	End      time.Time
	Status   int
	Error    string
	Attempts []Attempt
}

type TraceLog struct {
	mu     sync.Mutex
	limit  int
	traces []Trace
}

func NewTraceLog(limit int) *TraceLog {
	if limit <= 0 {
		limit = 100
	}
	return &TraceLog{limit: limit}
}

func (l *TraceLog) Add(trace Trace) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.traces) == l.limit {
		copy(l.traces, l.traces[1:])
		l.traces[len(l.traces)-1] = trace
		return
	}
	l.traces = append(l.traces, trace)
}

func (l *TraceLog) Recent() []Trace {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Trace, len(l.traces))
	copy(out, l.traces)
	return out
}

func recordAttempt(ctx context.Context, number int, status int, latency time.Duration, err error) {
	trace, ok := ctx.Value(traceKey{}).(*Trace)
	if !ok {
		return
	}
	attempt := Attempt{Number: number, Status: status, Latency: latency}
	if err != nil {
		attempt.Error = err.Error()
	}
	trace.Attempts = append(trace.Attempts, attempt)
}

var traceSeq atomic.Uint64

func nextTraceID() string {
	return fmt.Sprintf("req-%06d", traceSeq.Add(1))
}
```

Create `client_example_test.go`:

```go
package retrybreaker_test

import (
	"fmt"
	"net/http"

	"example.com/retrybreaker"
)

func ExampleTraceLog_Recent() {
	log := retrybreaker.NewTraceLog(2)
	log.Add(retrybreaker.Trace{ID: "req-000001", Method: http.MethodGet, Status: http.StatusOK})
	log.Add(retrybreaker.Trace{ID: "req-000002", Method: http.MethodGet, Status: http.StatusServiceUnavailable})

	for _, trace := range log.Recent() {
		fmt.Println(trace.ID, trace.Method, trace.Status)
	}

	// Output:
	// req-000001 GET 200
	// req-000002 GET 503
}
```

Create `client_test.go`:

```go
package retrybreaker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryTransport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		statuses   []int
		wantCalls  int
		wantStatus int
	}{
		{name: "get retries server errors", method: http.MethodGet, statuses: []int{500, 502, 200}, wantCalls: 3, wantStatus: 200},
		{name: "client error is not retried", method: http.MethodGet, statuses: []int{404}, wantCalls: 1, wantStatus: 404},
		{name: "post is not retried", method: http.MethodPost, statuses: []int{500}, wantCalls: 1, wantStatus: 500},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := int(calls.Add(1))
				status := tt.statuses[min(call-1, len(tt.statuses)-1)]
				w.WriteHeader(status)
				_, _ = fmt.Fprintln(w, status)
			}))
			defer server.Close()

			client, log := NewResilientClient(
				WithBaseTransport(server.Client().Transport),
				WithRetry(3, time.Nanosecond),
				WithCircuitBreaker(10, time.Second),
			)
			req, err := http.NewRequestWithContext(context.Background(), tt.method, server.URL, nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext: %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if got := int(calls.Load()); got != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", got, tt.wantCalls)
			}
			traces := log.Recent()
			if len(traces) != 1 || len(traces[0].Attempts) != tt.wantCalls {
				t.Fatalf("trace attempts = %+v, want %d attempts", traces, tt.wantCalls)
			}
		})
	}
}

func TestCircuitBreakerOpensAndRecovers(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, _ := NewResilientClient(
		WithBaseTransport(server.Client().Transport),
		WithRetry(0, time.Nanosecond),
		WithCircuitBreaker(2, 10*time.Millisecond),
	)

	for range 2 {
		resp, err := client.Get(server.URL)
		if err != nil {
			t.Fatalf("initial failing GET: %v", err)
		}
		resp.Body.Close()
	}

	_, err := client.Get(server.URL)
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("open circuit error = %v, want ErrCircuitOpen", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls while open = %d, want 2", got)
	}

	time.Sleep(15 * time.Millisecond)
	healthy.Store(true)
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("half-open probe: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probe status = %d, want 200", resp.StatusCode)
	}
}

func TestGetOKWrapsSentinel(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client, _ := NewResilientClient(
		WithBaseTransport(server.Client().Transport),
		WithRetry(0, time.Nanosecond),
		WithCircuitBreaker(10, time.Second),
	)
	err := GetOK(context.Background(), client, server.URL)
	if !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("GetOK error = %v, want ErrRequestFailed", err)
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	"example.com/retrybreaker"
)

func main() {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Request-ID") == "" {
			http.Error(w, "missing request id", http.StatusBadRequest)
			return
		}
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintln(w, "ok")
	}))
	defer server.Close()

	client, log := retrybreaker.NewResilientClient(
		retrybreaker.WithBaseTransport(server.Client().Transport),
		retrybreaker.WithRetry(3, time.Millisecond),
		retrybreaker.WithCircuitBreaker(5, time.Second),
	)

	if err := retrybreaker.GetOK(context.Background(), client, server.URL); err != nil {
		fmt.Println("request failed:", err)
		return
	}
	for _, trace := range log.Recent() {
		fmt.Printf("trace %s attempts=%d status=%d\n", trace.ID, len(trace.Attempts), trace.Status)
	}
}
```

## Verification

Run these commands from the module root:

```bash
gofmt -l .
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Confirm these outcomes:

- `gofmt -l .` prints no files.
- `go vet ./...` passes.
- `go build ./...` passes.
- `go test -count=1 -race ./...` passes.
- Tests use `httptest.NewServer` for real HTTP behavior.
- Tests are table-driven where cases vary and call `t.Parallel` safely.
- Sentinel errors are wrapped with `%w` and checked with `errors.Is`.
- The stack is implemented with composable `http.RoundTripper` values.

## Summary

Use `http.RoundTripper` decorators to keep retry, circuit breaker, and tracing concerns small, reusable, and testable. Verify them against `httptest.NewServer` so the code exercises the same `net/http` client path used in production.
