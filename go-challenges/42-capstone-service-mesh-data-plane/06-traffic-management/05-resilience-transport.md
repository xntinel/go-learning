# Exercise 5: Resilience Transport

The four policies built so far — circuit breaker, retry, budget, route timeout — are only safe when they compose in the right order. This exercise assembles them into a single `http.RoundTripper` that any `http.Client` can wrap, and proves the composition: circuit check first, then a timeout context that governs the whole call, then the retry loop, with the budget and backoff inside it.

This module is fully self-contained: it bundles its own copies of the circuit breaker, retry policy, budget, and route timeout so it gates alone. It begins with its own `go mod init` and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
traffic.go             CircuitBreaker, RetryPolicy, RetryBudget, RouteTimeout, ResilienceTransport
cmd/
  demo/
    main.go            httptest upstream that fails twice then succeeds; one Get drives all policies
traffic_test.go        retry, no-retry, circuit reject/trip, budget block, ctx cancel, timeout abort
```

- Files: `traffic.go`, `cmd/demo/main.go`, `traffic_test.go`.
- Implement: `ResilienceTransport` with `RoundTrip(*http.Request) (*http.Response, error)` composing the four policies, on top of the bundled `CircuitBreaker`, `RetryPolicy` (now with a `Budget` field), `RetryBudget`, and `RouteTimeout`.
- Test: `traffic_test.go` drives retry-until-success, non-retriable pass-through, circuit rejection and tripping, budget blocking, context cancellation during backoff, and a real `httptest` upstream that is slower than the route timeout.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why the order is circuit, then timeout, then retry

Each policy is correct alone; the danger is in how they nest. `RoundTrip` applies them in exactly one order, and every other order has a failure mode.

The circuit breaker is checked first, before anything else happens. If the circuit is open the call must be rejected without creating a context, opening a connection, or entering the retry loop — the whole point of the breaker is to spend zero upstream resources while the upstream is known-bad. Putting the circuit check inside the retry loop, or after the timeout context is built, wastes exactly the work the breaker exists to avoid.

The timeout context is built second, around the *entire* retry loop. This is the subtle one. If instead each attempt got its own `context.WithTimeout`, three retries of a 2s timeout could burn 6s of wall clock plus three backoff waits, so a caller with a 3s deadline gets an answer long after giving up. One context around the loop means the timeout is a true end-to-end budget: the attempts and the backoff waits all draw from it, and when it expires the loop's `ctx.Done()` check ends the call. The route timeout's `EffectiveTimeout` decides the duration, folding in any propagated caller deadline.

The retry loop is third, innermost. Before each attempt it re-checks `ctx.Done()` so an expired deadline stops it immediately. After each response it records the outcome in the circuit breaker — including retriable failures, because the breaker and the retry policy are independent observers and the breaker must be able to trip even while retries are still in flight. A retriable response with attempts remaining is closed (its body returned to the pool), checked against the budget, and followed by a context-aware backoff. A non-retriable response, a network error, or the last attempt ends the loop. Network errors are returned without recording them in the breaker, because a transport-level error may mean the upstream was never reached, and counting unreachable-but-maybe-fine attempts against the breaker would trip it on the proxy's own connectivity blips.

Create `traffic.go`:

```go
// Package traffic provides resilient HTTP transport middleware for a
// service-mesh data plane: a three-state circuit breaker, exponential backoff
// with jitter, retry budgets, and per-route timeout with deadline propagation.
// All policies compose as an http.RoundTripper that wraps any upstream transport.
package traffic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Sentinel errors. Callers check identity with errors.Is.
var (
	ErrCircuitOpen     = errors.New("circuit breaker open")
	ErrBudgetExhausted = errors.New("retry budget exhausted")
)

// ---- Circuit breaker --------------------------------------------------------

// State is the circuit breaker state.
type State int

const (
	StateClosed   State = iota // normal; requests pass through
	StateOpen                  // failing; requests rejected immediately
	StateHalfOpen              // recovery probe; one request is allowed
)

// String returns the lowercase name of the state.
func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// CircuitBreakerConfig holds the tuning knobs for a CircuitBreaker.
type CircuitBreakerConfig struct {
	// Threshold is the number of consecutive failures before the circuit opens.
	// Default: 5.
	Threshold int
	// Cooldown is how long the circuit stays open before entering half-open.
	// Default: 30s.
	Cooldown time.Duration
}

// CircuitBreaker implements the closed -> open -> half-open state machine.
// It is safe for concurrent use.
type CircuitBreaker struct {
	mu           sync.Mutex
	state        State
	failures     int
	cfg          CircuitBreakerConfig
	openedAt     time.Time
	halfOpenSent bool
	// StateChange is called on every state transition. Nil is safe.
	StateChange func(from, to State)
}

// NewCircuitBreaker creates a CircuitBreaker. Zero fields are replaced with
// defaults (Threshold=5, Cooldown=30s).
func NewCircuitBreaker(cfg CircuitBreakerConfig) *CircuitBreaker {
	if cfg.Threshold <= 0 {
		cfg.Threshold = 5
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 30 * time.Second
	}
	return &CircuitBreaker{cfg: cfg}
}

// Allow returns nil if the request may be forwarded, or a wrapped ErrCircuitOpen.
func (cb *CircuitBreaker) Allow() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		return nil

	case StateOpen:
		if time.Since(cb.openedAt) >= cb.cfg.Cooldown {
			cb.transitionLocked(StateHalfOpen)
			cb.halfOpenSent = true
			return nil
		}
		return fmt.Errorf("%w", ErrCircuitOpen)

	case StateHalfOpen:
		if !cb.halfOpenSent {
			cb.halfOpenSent = true
			return nil
		}
		return fmt.Errorf("%w", ErrCircuitOpen)

	default:
		return fmt.Errorf("%w", ErrCircuitOpen)
	}
}

// Record records the outcome of a forwarded request.
func (cb *CircuitBreaker) Record(success bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		if success {
			cb.failures = 0
			return
		}
		cb.failures++
		if cb.failures >= cb.cfg.Threshold {
			cb.transitionLocked(StateOpen)
		}

	case StateHalfOpen:
		cb.halfOpenSent = false
		if success {
			cb.failures = 0
			cb.transitionLocked(StateClosed)
		} else {
			cb.transitionLocked(StateOpen)
		}
	}
}

// CurrentState returns the current state without side effects.
func (cb *CircuitBreaker) CurrentState() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

func (cb *CircuitBreaker) transitionLocked(to State) {
	from := cb.state
	cb.state = to
	if to == StateOpen {
		cb.openedAt = time.Now()
	}
	if cb.StateChange != nil {
		cb.StateChange(from, to)
	}
}

// ---- Retry budget -----------------------------------------------------------

// RetryBudget limits the fraction of total traffic that may be retries,
// measured over a sliding time window. It is safe for concurrent use.
type RetryBudget struct {
	mu       sync.Mutex
	window   time.Duration
	maxRatio float64
	entries  []budgetEntry
}

type budgetEntry struct {
	at      time.Time
	isRetry bool
}

// NewRetryBudget creates a RetryBudget.
func NewRetryBudget(maxRatio float64, window time.Duration) *RetryBudget {
	return &RetryBudget{maxRatio: maxRatio, window: window}
}

// Record registers a request. Pass isRetry=false for originals, true for retries.
func (b *RetryBudget) Record(isRetry bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()
	b.entries = append(b.entries, budgetEntry{at: time.Now(), isRetry: isRetry})
}

// Allow returns nil if a retry is within budget, or a wrapped ErrBudgetExhausted.
func (b *RetryBudget) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()

	var originals, retries int
	for _, e := range b.entries {
		if e.isRetry {
			retries++
		} else {
			originals++
		}
	}
	if originals == 0 {
		return nil
	}
	ratio := float64(retries) / float64(originals)
	if ratio >= b.maxRatio {
		return fmt.Errorf("%w: ratio %.2f >= limit %.2f", ErrBudgetExhausted, ratio, b.maxRatio)
	}
	return nil
}

func (b *RetryBudget) pruneLocked() {
	cutoff := time.Now().Add(-b.window)
	i := 0
	for i < len(b.entries) && b.entries[i].at.Before(cutoff) {
		i++
	}
	b.entries = b.entries[i:]
}

// ---- Retry policy -----------------------------------------------------------

// RetryPolicy configures retry logic for forwarded requests.
type RetryPolicy struct {
	MaxRetries       int
	RetriableCodes   map[int]struct{}
	RetriableMethods map[string]struct{}
	BackoffBase      time.Duration
	BackoffMax       time.Duration
	JitterFactor     float64
	// Budget optionally limits the fraction of traffic from retries. Nil disables it.
	Budget *RetryBudget
}

// DefaultRetryPolicy returns sensible production defaults.
func DefaultRetryPolicy() *RetryPolicy {
	return &RetryPolicy{
		MaxRetries: 3,
		RetriableCodes: map[int]struct{}{
			http.StatusBadGateway:         {},
			http.StatusServiceUnavailable: {},
			http.StatusGatewayTimeout:     {},
		},
		RetriableMethods: map[string]struct{}{
			http.MethodGet:     {},
			http.MethodHead:    {},
			http.MethodOptions: {},
		},
		BackoffBase:  100 * time.Millisecond,
		BackoffMax:   10 * time.Second,
		JitterFactor: 0.5,
	}
}

// IsRetriable reports whether the method/code combination should trigger a retry.
func (rp *RetryPolicy) IsRetriable(method string, code int) bool {
	if _, ok := rp.RetriableMethods[method]; !ok {
		return false
	}
	_, ok := rp.RetriableCodes[code]
	return ok
}

// Backoff returns the wait before attempt n (0-indexed) with full jitter.
func (rp *RetryPolicy) Backoff(n int) time.Duration {
	d := time.Duration(float64(rp.BackoffBase) * math.Pow(2, float64(n)))
	if d > rp.BackoffMax {
		d = rp.BackoffMax
	}
	if rp.JitterFactor > 0 {
		d += time.Duration(float64(d) * rp.JitterFactor * rand.Float64())
	}
	return d
}

// Sleep waits for Backoff(attempt) or returns early when ctx is cancelled.
func (rp *RetryPolicy) Sleep(ctx context.Context, attempt int) error {
	t := time.NewTimer(rp.Backoff(attempt))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---- Route timeout ----------------------------------------------------------

// RouteTimeout configures per-route request timeouts and deadline propagation.
type RouteTimeout struct {
	RequestTimeout time.Duration
	DeadlineHeader string
}

// EffectiveTimeout returns the tighter of the configured timeout and any
// propagated caller deadline; (0, false) when neither applies.
func (rt *RouteTimeout) EffectiveTimeout(r *http.Request) (time.Duration, bool) {
	configured := rt.RequestTimeout

	var remaining time.Duration
	hasDeadline := false
	if rt.DeadlineHeader != "" {
		if raw := r.Header.Get(rt.DeadlineHeader); raw != "" {
			if nsec, err := strconv.ParseInt(raw, 10, 64); err == nil {
				rem := time.Until(time.Unix(0, nsec))
				if rem > 0 {
					remaining = rem
					hasDeadline = true
				}
			}
		}
	}

	switch {
	case configured > 0 && hasDeadline:
		if remaining < configured {
			return remaining, true
		}
		return configured, true
	case configured > 0:
		return configured, true
	case hasDeadline:
		return remaining, true
	default:
		return 0, false
	}
}

// ---- ResilienceTransport ----------------------------------------------------

// ResilienceTransport is an http.RoundTripper that wraps an upstream transport
// with circuit breaking, per-route timeout, and retry policy.
//
// Composition order: circuit check -> timeout context -> retry loop.
type ResilienceTransport struct {
	CB      *CircuitBreaker
	Retry   *RetryPolicy
	Timeout *RouteTimeout
	Inner   http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (rt *ResilienceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Step 1: circuit breaker gate — reject immediately if the circuit is open.
	if rt.CB != nil {
		if err := rt.CB.Allow(); err != nil {
			return nil, err
		}
	}

	// Step 2: per-route timeout wraps the entire retry loop, not each attempt.
	ctx := r.Context()
	if rt.Timeout != nil {
		if d, ok := rt.Timeout.EffectiveTimeout(r); ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
	}

	inner := rt.Inner
	if inner == nil {
		inner = http.DefaultTransport
	}

	maxAttempts := 1
	if rt.Retry != nil {
		maxAttempts = 1 + rt.Retry.MaxRetries
	}

	// Record the original request in the budget before the first attempt.
	if rt.Retry != nil && rt.Retry.Budget != nil {
		rt.Retry.Budget.Record(false)
	}

	// Step 3: retry loop.
	var resp *http.Response
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Abort if the context deadline has already passed.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		var err error
		resp, err = inner.RoundTrip(r.Clone(ctx))
		if err != nil {
			// Network-level errors are not retried; the circuit does not
			// record them (the upstream may not have been reached at all).
			return nil, err
		}

		// Record the outcome so the circuit breaker can track health.
		if rt.CB != nil {
			rt.CB.Record(resp.StatusCode < 500)
		}

		// Not retriable: return immediately.
		if rt.Retry == nil || !rt.Retry.IsRetriable(r.Method, resp.StatusCode) {
			return resp, nil
		}

		// Retriable response: return it if no attempts remain.
		if attempt == maxAttempts-1 {
			break
		}

		// More attempts available; discard the retriable response body.
		resp.Body.Close()

		// Budget check before this retry.
		if rt.Retry.Budget != nil {
			if budgetErr := rt.Retry.Budget.Allow(); budgetErr != nil {
				return nil, budgetErr
			}
			rt.Retry.Budget.Record(true)
		}

		// Backoff wait (honours context cancellation).
		if sleepErr := rt.Retry.Sleep(ctx, attempt); sleepErr != nil {
			return nil, sleepErr
		}
	}

	// All retries exhausted; return the last retriable response to the caller.
	return resp, nil
}
```

Set `ResilienceTransport` as the `Transport` field of an `http.Client` and every request made through that client gets the full circuit-breaker, retry, budget, and timeout treatment automatically — application code calls `client.Get` and never sees the machinery.

### The runnable demo

The demo stands up a real upstream with `net/http/httptest` that returns 503 for its first two calls and 200 afterward, then makes a single `client.Get`. The transport retries the two 503s and succeeds on the third call, all transparently. The circuit threshold is 5, so two retriable failures do not trip it, and the circuit ends closed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	"example.com/resilience-transport"
)

func main() {
	// Upstream that fails the first two calls, then succeeds.
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "OK")
	}))
	defer upstream.Close()

	cb := traffic.NewCircuitBreaker(traffic.CircuitBreakerConfig{
		Threshold: 5,
		Cooldown:  2 * time.Second,
	})

	rp := traffic.DefaultRetryPolicy()
	rp.BackoffBase = 50 * time.Millisecond
	rp.BackoffMax = 200 * time.Millisecond
	rp.JitterFactor = 0.2

	client := &http.Client{
		Transport: &traffic.ResilienceTransport{
			CB:    cb,
			Retry: rp,
			Timeout: &traffic.RouteTimeout{
				RequestTimeout: 2 * time.Second,
				DeadlineHeader: "X-Request-Deadline",
			},
			Inner: upstream.Client().Transport,
		},
	}

	fmt.Printf("circuit state before call: %s\n", cb.CurrentState())

	resp, err := client.Get(upstream.URL + "/hello")
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("status: %d, body: %s", resp.StatusCode, body)
	fmt.Printf("upstream calls: %d (2 retriable + 1 success)\n", calls.Load())
	fmt.Printf("circuit state after call: %s\n", cb.CurrentState())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (backoff durations vary due to jitter; the printed lines do not):

```
circuit state before call: closed
status: 200, body: OK
upstream calls: 3 (2 retriable + 1 success)
circuit state after call: closed
```

### Tests

Most tests use a deterministic `mockTransport` that replays a fixed sequence of responses, so retry counts and circuit transitions are exact. `TestResilienceTransportRetriesOnRetriableCode` checks two 503s then a 200 produce three upstream calls and a final 200. `TestResilienceTransportCircuitTripsAfterFailures` drives three failing single-attempt calls and asserts the fourth is rejected by the open circuit without reaching the upstream. `TestResilienceTransportBudgetPreventsExcessiveRetries` pre-seeds the budget over its limit and asserts the retry is blocked. The last test, `TestResilienceTransportTimeoutAbortsLongUpstream`, uses a real `httptest` server that sleeps past the route timeout and asserts `RoundTrip` returns promptly with an error — this is the end-to-end proof that the timeout context governs the whole call.

Create `traffic_test.go`:

```go
package traffic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockTransport returns a pre-programmed sequence of responses or errors.
type mockTransport struct {
	mu    sync.Mutex
	queue []mockResult
	pos   int
}

type mockResult struct {
	code int
	err  error
}

func (m *mockTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pos >= len(m.queue) {
		return codeResp(http.StatusOK), nil
	}
	r := m.queue[m.pos]
	m.pos++
	if r.err != nil {
		return nil, r.err
	}
	return codeResp(r.code), nil
}

func codeResp(code int) *http.Response {
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}
}

func getReq() *http.Request {
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://upstream/", nil)
	return r
}

func TestResilienceTransportSucceedsOnFirstAttempt(t *testing.T) {
	t.Parallel()

	tr := &ResilienceTransport{
		Retry: DefaultRetryPolicy(),
		Inner: &mockTransport{queue: []mockResult{{code: http.StatusOK}}},
	}
	resp, err := tr.RoundTrip(getReq())
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("RoundTrip() = %v, %v; want 200, nil", resp, err)
	}
}

func TestResilienceTransportRetriesOnRetriableCode(t *testing.T) {
	t.Parallel()

	inner := &mockTransport{queue: []mockResult{
		{code: http.StatusServiceUnavailable},
		{code: http.StatusServiceUnavailable},
		{code: http.StatusOK},
	}}
	rp := DefaultRetryPolicy()
	rp.BackoffBase = 0 // no wait in tests

	tr := &ResilienceTransport{Retry: rp, Inner: inner}
	resp, err := tr.RoundTrip(getReq())
	if err != nil {
		t.Fatalf("RoundTrip error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if inner.pos != 3 {
		t.Fatalf("calls = %d, want 3 (2 retriable + 1 success)", inner.pos)
	}
}

func TestResilienceTransportDoesNotRetryNonRetriableCode(t *testing.T) {
	t.Parallel()

	inner := &mockTransport{queue: []mockResult{
		{code: http.StatusInternalServerError}, // 500 is not in RetriableCodes
		{code: http.StatusOK},
	}}
	tr := &ResilienceTransport{Retry: DefaultRetryPolicy(), Inner: inner}
	resp, _ := tr.RoundTrip(getReq())
	if inner.pos != 1 {
		t.Fatalf("calls = %d, want 1 (500 is not retriable)", inner.pos)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestResilienceTransportCircuitBreakerRejects(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 1, Cooldown: time.Hour})
	cb.Record(false) // trip the circuit

	tr := &ResilienceTransport{
		CB:    cb,
		Retry: DefaultRetryPolicy(),
		Inner: &mockTransport{queue: []mockResult{{code: http.StatusOK}}},
	}
	_, err := tr.RoundTrip(getReq())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
}

func TestResilienceTransportCircuitTripsAfterFailures(t *testing.T) {
	t.Parallel()

	cb := NewCircuitBreaker(CircuitBreakerConfig{Threshold: 3, Cooldown: time.Hour})
	rp := DefaultRetryPolicy()
	rp.MaxRetries = 0 // no retries: one attempt per call
	rp.BackoffBase = 0

	inner := &mockTransport{queue: []mockResult{
		{code: http.StatusServiceUnavailable},
		{code: http.StatusServiceUnavailable},
		{code: http.StatusServiceUnavailable},
		{code: http.StatusOK},
	}}
	tr := &ResilienceTransport{CB: cb, Retry: rp, Inner: inner}

	for i := 0; i < 3; i++ {
		tr.RoundTrip(getReq()) //nolint:errcheck
	}
	if cb.CurrentState() != StateOpen {
		t.Fatalf("state = %s, want open after 3 failures", cb.CurrentState())
	}

	_, err := tr.RoundTrip(getReq())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if inner.pos != 3 {
		t.Fatalf("upstream calls = %d, want 3 (fourth rejected by circuit)", inner.pos)
	}
}

func TestResilienceTransportBudgetPreventsExcessiveRetries(t *testing.T) {
	t.Parallel()

	// Pre-seed so the ratio is already above the 0.20 limit; RoundTrip records
	// one more original, leaving 6 originals + 2 retries = 0.33, still over.
	budget := NewRetryBudget(0.20, time.Minute)
	for i := 0; i < 5; i++ {
		budget.Record(false)
	}
	budget.Record(true)
	budget.Record(true)

	rp := DefaultRetryPolicy()
	rp.Budget = budget
	rp.BackoffBase = 0

	inner := &mockTransport{queue: []mockResult{
		{code: http.StatusServiceUnavailable},
		{code: http.StatusOK},
	}}
	tr := &ResilienceTransport{Retry: rp, Inner: inner}

	_, err := tr.RoundTrip(getReq())
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if inner.pos != 1 {
		t.Fatalf("upstream calls = %d, want 1 (budget blocked retry)", inner.pos)
	}
}

func TestResilienceTransportContextCancellationAbortsRetry(t *testing.T) {
	t.Parallel()

	rp := DefaultRetryPolicy()
	rp.BackoffBase = 10 * time.Second // long backoff so cancellation fires first
	rp.BackoffMax = 10 * time.Second
	rp.JitterFactor = 0

	inner := &mockTransport{queue: []mockResult{
		{code: http.StatusServiceUnavailable},
		{code: http.StatusServiceUnavailable},
	}}
	tr := &ResilienceTransport{Retry: rp, Inner: inner}

	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream/", nil)

	done := make(chan error, 1)
	go func() {
		_, err := tr.RoundTrip(r)
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	if err := <-done; err == nil {
		t.Fatal("expected error after context cancellation")
	}
}

func TestResilienceTransportTimeoutAbortsLongUpstream(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond) // slower than the route timeout
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	tr := &ResilienceTransport{
		Retry:   DefaultRetryPolicy(),
		Timeout: &RouteTimeout{RequestTimeout: 50 * time.Millisecond},
		Inner:   upstream.Client().Transport,
	}
	req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/slow", nil)

	start := time.Now()
	_, err := tr.RoundTrip(req)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from the route timeout, got nil")
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("RoundTrip took %v, want it to abort well before the 200ms upstream", elapsed)
	}
}
```

## Review

The transport is correct when the three policies nest in the mandatory order and each still does its own job. The order itself is the first thing to verify: the circuit check happens before any context or connection (so an open circuit costs nothing), the timeout context wraps the whole loop (so retries plus backoff share one budget), and the loop re-checks `ctx.Done()` before every attempt (so an expired deadline stops it). The second thing is that every response outcome reaches `cb.Record`, including retriable failures — `TestResilienceTransportCircuitTripsAfterFailures` would not trip the breaker if retriable responses were skipped. The third is hygiene inside the loop: a retriable response's body is closed before the next attempt so connections return to the pool, and a network error returns without recording the circuit because the upstream may never have been reached. Running the whole suite under `go test -race` exercises the concurrent `mockTransport`, the cancellation goroutine, and the real `httptest` timeout path together.

## Resources

- [net/http: RoundTripper](https://pkg.go.dev/net/http#RoundTripper) — the single-method interface that lets this transport drop into any `http.Client`.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — the in-process server used by the demo and the timeout test.
- [Envoy: retry semantics](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/router_filter#x-envoy-retry-on) — the production retry model that informed these defaults.
- [Google SRE Book: Addressing Cascading Failures](https://sre.google/sre-book/addressing-cascading-failures/) — why circuit breaking, budgets, and deadlines must work together to stop an outage from spreading.

---

Back to [04-route-timeout.md](04-route-timeout.md) | Next: [Rate Limiting](../07-rate-limiting/00-concepts.md)
