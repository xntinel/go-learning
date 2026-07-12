# Exercise 2: Deadline Budgets and a Circuit Breaker to Stop Retry Storms

Retries alone are dangerous. A per-attempt timeout that ignores the caller's SLA
lets one slow attempt eat the whole budget, and a retry loop against a
genuinely-down provider amplifies the outage. This exercise builds the two controls
that fix both: a budget-aware caller that sizes each attempt from the *remaining*
deadline, and a thread-safe circuit breaker that fails fast during an outage and
probes for recovery.

This module is fully self-contained: its own `go mod init`, the `Completer` seam,
an injected clock, and fakes for every path. Nothing here imports another exercise.

## What you'll build

```text
resilience/                  independent module: example.com/resilience
  go.mod                     go 1.26
  resilience.go              Completer, Request/Response, APIError, BudgetCaller, budgets
  breaker.go                 State, CircuitBreaker (closed/open/half-open), ErrCircuitOpen
  cmd/
    demo/
      main.go                runnable demo: the breaker state sequence over a cooldown
  resilience_test.go         per-attempt budget, budget exhaustion, breaker transitions
```

- Files: `resilience.go`, `breaker.go`, `cmd/demo/main.go`, `resilience_test.go`.
- Implement: a `BudgetCaller` that derives each attempt's timeout from `ctx.Deadline()` minus now via `context.WithTimeoutCause`, returning `ErrBudgetExhausted` when nothing is left and distinguishing a per-attempt timeout via `context.Cause`; a `CircuitBreaker` with an injected clock that opens after a consecutive-failure threshold, fails fast with `ErrCircuitOpen`, and half-opens after a cooldown to admit one trial.
- Test: per-attempt timeout equals the remaining budget; a call past the deadline returns `ErrBudgetExhausted` with zero downstream calls; the breaker opens after N failures and then rejects with zero calls; after the cooldown exactly one trial is admitted, a success closes it and a failure re-opens it; `-race` proves the mutex guards state.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/52-ai-llm-backends/08-llm-resilience-and-caching/02-timeout-budgets-and-circuit-breaker/cmd/demo
cd go-solutions/52-ai-llm-backends/08-llm-resilience-and-caching/02-timeout-budgets-and-circuit-breaker
go mod edit -go=1.26
```

### Sizing an attempt from the remaining budget

The caller sets one end-to-end deadline on the context: "answer within 3 seconds."
Each attempt must fit *inside* what is left of that budget, never a fresh copy of
it. `perAttemptTimeout` reads `ctx.Deadline()`, subtracts the current time, and
returns the smaller of that remainder and a per-attempt ceiling. If the remainder
is already zero or negative there is no point starting another attempt, so it
reports `ok == false` and `BudgetCaller` returns `ErrBudgetExhausted`. When the
context has no deadline at all, the per-attempt ceiling is used directly.

The attempt runs under a child context built with `context.WithTimeoutCause`. The
cause matters: when that child times out, `ctx.Err()` is the generic
`context.DeadlineExceeded`, but `context.Cause(child)` returns the sentinel
`ErrAttemptTimeout` we attached. That is how the caller tells "this one attempt was
slow" (retry it) from "the whole budget is gone" (give up). Without the cause, both
look identical.

### The circuit breaker state machine

A breaker has three states. In **closed** it passes requests through and counts
consecutive failures; a success resets the count. When the count reaches the
threshold it trips to **open**, where every request is rejected immediately with
`ErrCircuitOpen` and *zero* downstream work — this is the load-shedding that stops a
retry storm from pounding a down provider. After the cooldown elapses it moves to
**half-open** and admits exactly one trial request: if the trial succeeds the
breaker closes, if it fails it re-opens and the cooldown restarts. The "exactly
one" is enforced by a `halfOpenInFlight` flag, so a burst of concurrent callers in
half-open sends only one probe upstream and the rest get `ErrCircuitOpen`.

Two design choices make the breaker testable and safe. The clock is an injected
`func() time.Time`, so a test advances a fake clock past the cooldown instead of
sleeping. And every state read or transition is guarded by a `sync.Mutex`, because
the breaker is shared across all request goroutines; `go test -race` proves it. The
open-to-half-open transition is *lazy* — evaluated whenever the state is inspected —
so both `State()` and the request path observe the cooldown consistently.

Create `resilience.go`:

```go
package resilience

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Message is one turn in a conversation.
type Message struct {
	Role    string
	Content string
}

// Request is a provider-agnostic completion request.
type Request struct {
	Model       string
	System      string
	Messages    []Message
	Temperature float64
	MaxTokens   int
}

// Response is a provider-agnostic completion response.
type Response struct {
	Text  string
	Model string
}

// Completer is the seam every resilience wrapper composes over.
type Completer interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// APIError is a provider-agnostic transport error carrying the HTTP status.
type APIError struct {
	StatusCode int
	Err        error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error: status %d (%s)", e.StatusCode, http.StatusText(e.StatusCode))
}

func (e *APIError) Unwrap() error { return e.Err }

// ErrBudgetExhausted means the end-to-end deadline left no time for another attempt.
var ErrBudgetExhausted = errors.New("resilience: deadline budget exhausted")

// ErrAttemptTimeout is attached as the cause of a single attempt's timeout, so the
// caller can distinguish a slow attempt from an exhausted overall budget.
var ErrAttemptTimeout = errors.New("resilience: per-attempt timeout")

// perAttemptTimeout returns how long the next attempt may run: the smaller of the
// per-attempt ceiling and the time left before ctx's deadline. ok is false when the
// budget is already spent. A context with no deadline yields the ceiling.
func perAttemptTimeout(ctx context.Context, ceiling time.Duration, now time.Time) (time.Duration, bool) {
	dl, has := ctx.Deadline()
	if !has {
		return ceiling, true
	}
	rem := dl.Sub(now)
	if rem <= 0 {
		return 0, false
	}
	if ceiling > 0 && ceiling < rem {
		return ceiling, true
	}
	return rem, true
}

// BudgetCaller bounds a single attempt by the remaining end-to-end deadline.
type BudgetCaller struct {
	next    Completer
	ceiling time.Duration
	now     func() time.Time
}

var _ Completer = (*BudgetCaller)(nil)

// NewBudgetCaller wraps next, capping any single attempt at ceiling and never
// exceeding the caller's context deadline.
func NewBudgetCaller(next Completer, ceiling time.Duration) *BudgetCaller {
	return &BudgetCaller{next: next, ceiling: ceiling, now: time.Now}
}

// Complete runs one attempt inside a per-attempt context derived from the remaining
// budget. It returns ErrBudgetExhausted when the deadline has passed, and folds a
// per-attempt timeout into ErrAttemptTimeout via context.Cause.
func (b *BudgetCaller) Complete(ctx context.Context, req Request) (Response, error) {
	d, ok := perAttemptTimeout(ctx, b.ceiling, b.now())
	if !ok {
		return Response{}, ErrBudgetExhausted
	}
	actx, cancel := context.WithTimeoutCause(ctx, d, ErrAttemptTimeout)
	defer cancel()

	resp, err := b.next.Complete(actx, req)
	if err != nil && errors.Is(err, context.DeadlineExceeded) &&
		errors.Is(context.Cause(actx), ErrAttemptTimeout) {
		return Response{}, fmt.Errorf("attempt exceeded its slice of the budget: %w", ErrAttemptTimeout)
	}
	return resp, err
}
```

Create `breaker.go`:

```go
package resilience

import (
	"context"
	"errors"
	"sync"
	"time"
)

// State is a circuit breaker state.
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

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

// ErrCircuitOpen is returned, with zero downstream work, while the breaker is open.
var ErrCircuitOpen = errors.New("resilience: circuit breaker open")

// CircuitBreaker fails fast during an outage and probes for recovery. It is safe
// for concurrent use.
type CircuitBreaker struct {
	next      Completer
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu               sync.Mutex
	state            State
	failures         int
	openedAt         time.Time
	halfOpenInFlight bool
}

var _ Completer = (*CircuitBreaker)(nil)

// CBOption configures a CircuitBreaker.
type CBOption func(*CircuitBreaker)

// WithClock injects a clock so the half-open cooldown is testable without sleeping.
func WithClock(now func() time.Time) CBOption {
	return func(cb *CircuitBreaker) { cb.now = now }
}

// NewCircuitBreaker opens after threshold consecutive failures and stays open for
// cooldown before admitting a single half-open trial.
func NewCircuitBreaker(next Completer, threshold int, cooldown time.Duration, opts ...CBOption) *CircuitBreaker {
	cb := &CircuitBreaker{
		next:      next,
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
		state:     StateClosed,
	}
	for _, opt := range opts {
		opt(cb)
	}
	return cb
}

// State reports the current state, applying the lazy open-to-half-open transition.
func (cb *CircuitBreaker) State() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.maybeHalfOpen()
	return cb.state
}

// maybeHalfOpen moves an open breaker to half-open once the cooldown has elapsed.
// The caller must hold cb.mu.
func (cb *CircuitBreaker) maybeHalfOpen() {
	if cb.state == StateOpen && cb.now().Sub(cb.openedAt) >= cb.cooldown {
		cb.state = StateHalfOpen
		cb.halfOpenInFlight = false
	}
}

// beforeRequest decides whether a request may proceed. The caller must not hold
// cb.mu. It returns ErrCircuitOpen to reject.
func (cb *CircuitBreaker) beforeRequest() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.maybeHalfOpen()
	switch cb.state {
	case StateOpen:
		return ErrCircuitOpen
	case StateHalfOpen:
		if cb.halfOpenInFlight {
			return ErrCircuitOpen
		}
		cb.halfOpenInFlight = true
		return nil
	default:
		return nil
	}
}

// afterRequest records the outcome of an admitted request and transitions state.
func (cb *CircuitBreaker) afterRequest(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	failure := err != nil
	switch cb.state {
	case StateHalfOpen:
		cb.halfOpenInFlight = false
		if failure {
			cb.state = StateOpen
			cb.openedAt = cb.now()
		} else {
			cb.state = StateClosed
			cb.failures = 0
		}
	case StateClosed:
		if failure {
			cb.failures++
			if cb.failures >= cb.threshold {
				cb.state = StateOpen
				cb.openedAt = cb.now()
			}
		} else {
			cb.failures = 0
		}
	}
}

// Complete rejects immediately when open, otherwise runs the request and records
// its outcome.
func (cb *CircuitBreaker) Complete(ctx context.Context, req Request) (Response, error) {
	if err := cb.beforeRequest(); err != nil {
		return Response{}, err
	}
	resp, err := cb.next.Complete(ctx, req)
	cb.afterRequest(err)
	return resp, err
}
```

### The runnable demo

The demo drives the breaker with an injected clock so the whole
closed -> open -> half-open -> closed sequence is deterministic and instant.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/resilience"
)

// downstream is a controllable fake provider.
type downstream struct {
	err   error
	calls int
}

func (d *downstream) Complete(_ context.Context, _ resilience.Request) (resilience.Response, error) {
	d.calls++
	if d.err != nil {
		return resilience.Response{}, d.err
	}
	return resilience.Response{Text: "ok"}, nil
}

type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func main() {
	clk := &clock{t: time.Unix(0, 0).UTC()}
	fake := &downstream{err: &resilience.APIError{StatusCode: 500}}
	cb := resilience.NewCircuitBreaker(fake, 2, time.Second, resilience.WithClock(clk.Now))

	ctx := context.Background()
	req := resilience.Request{Model: "demo-model"}

	fmt.Println("state:", cb.State())
	_, _ = cb.Complete(ctx, req)
	_, _ = cb.Complete(ctx, req)
	fmt.Println("state:", cb.State(), "after 2 failures")

	clk.Advance(2 * time.Second)
	fmt.Println("state:", cb.State(), "after cooldown")

	fake.err = nil
	_, _ = cb.Complete(ctx, req)
	fmt.Println("state:", cb.State(), "after successful trial")
	fmt.Println("downstream calls:", fake.calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
state: closed
state: open after 2 failures
state: half-open after cooldown
state: closed after successful trial
downstream calls: 3
```

The downstream saw exactly three calls: two failures that tripped the breaker, then
one half-open trial. The rejected calls while open cost nothing.

### Tests

`TestPerAttemptTimeout` is a table over the budget math. `TestBudgetExhausted`
proves a past deadline returns `ErrBudgetExhausted` with zero downstream calls.
`TestAttemptTimeoutCause` proves a slow attempt surfaces `ErrAttemptTimeout` via the
context cause. The breaker tests drive the state machine: it opens after the
threshold, rejects with zero calls while open, admits exactly one half-open trial
(a success closes it, a failure re-opens it), and survives a concurrent barrage
under `-race`.

Create `resilience_test.go`:

```go
package resilience

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// countingCompleter records calls and returns a settable error.
type countingCompleter struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *countingCompleter) Complete(_ context.Context, _ Request) (Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return Response{}, f.err
	}
	return Response{Text: "ok"}, nil
}

func (f *countingCompleter) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *countingCompleter) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// blockingCompleter waits for its context, then returns the context error.
type blockingCompleter struct{}

func (blockingCompleter) Complete(ctx context.Context, _ Request) (Response, error) {
	<-ctx.Done()
	return Response{}, ctx.Err()
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestPerAttemptTimeout(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0).UTC()
	cases := []struct {
		name     string
		deadline time.Time
		ceiling  time.Duration
		wantOK   bool
		wantDur  time.Duration
	}{
		{"remaining below ceiling", now.Add(2 * time.Second), 5 * time.Second, true, 2 * time.Second},
		{"ceiling below remaining", now.Add(10 * time.Second), 3 * time.Second, true, 3 * time.Second},
		{"deadline passed", now.Add(-time.Second), 3 * time.Second, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithDeadline(context.Background(), tc.deadline)
			defer cancel()
			got, ok := perAttemptTimeout(ctx, tc.ceiling, now)
			if ok != tc.wantOK || got != tc.wantDur {
				t.Fatalf("perAttemptTimeout = %v,%v; want %v,%v", got, ok, tc.wantDur, tc.wantOK)
			}
		})
	}
}

func TestBudgetExhausted(t *testing.T) {
	t.Parallel()
	fake := &countingCompleter{}
	bc := NewBudgetCaller(fake, time.Second)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := bc.Complete(ctx, Request{})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if got := fake.Calls(); got != 0 {
		t.Fatalf("downstream calls = %d, want 0 (budget already spent)", got)
	}
}

func TestAttemptTimeoutCause(t *testing.T) {
	t.Parallel()
	bc := NewBudgetCaller(blockingCompleter{}, 10*time.Millisecond)

	_, err := bc.Complete(context.Background(), Request{})
	if !errors.Is(err, ErrAttemptTimeout) {
		t.Fatalf("err = %v, want ErrAttemptTimeout via context.Cause", err)
	}
}

func TestBreakerOpensAndRejects(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(0, 0).UTC()}
	fake := &countingCompleter{err: &APIError{StatusCode: 500}}
	cb := NewCircuitBreaker(fake, 3, time.Second, WithClock(clk.Now))

	ctx := context.Background()
	for range 3 {
		if _, err := cb.Complete(ctx, Request{}); err == nil {
			t.Fatal("expected downstream failure")
		}
	}
	if got := cb.State(); got != StateOpen {
		t.Fatalf("state = %v, want open after threshold failures", got)
	}

	before := fake.Calls()
	_, err := cb.Complete(ctx, Request{})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if got := fake.Calls(); got != before {
		t.Fatalf("downstream calls = %d, want unchanged %d (open rejects with zero work)", got, before)
	}
}

func TestBreakerHalfOpenSingleTrial(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(0, 0).UTC()}
	cb := NewCircuitBreaker(&countingCompleter{}, 1, time.Second, WithClock(clk.Now))

	// Force the breaker open, then advance past the cooldown.
	cb.mu.Lock()
	cb.state = StateOpen
	cb.openedAt = clk.Now()
	cb.mu.Unlock()
	clk.Advance(2 * time.Second)

	if got := cb.State(); got != StateHalfOpen {
		t.Fatalf("state = %v, want half-open after cooldown", got)
	}
	if err := cb.beforeRequest(); err != nil {
		t.Fatalf("first half-open trial should be admitted: %v", err)
	}
	if err := cb.beforeRequest(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("second concurrent trial should be rejected, got %v", err)
	}
	cb.afterRequest(nil) // success closes the breaker
	if got := cb.State(); got != StateClosed {
		t.Fatalf("state = %v, want closed after a successful trial", got)
	}
}

func TestBreakerHalfOpenFailureReopens(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(0, 0).UTC()}
	cb := NewCircuitBreaker(&countingCompleter{}, 1, time.Second, WithClock(clk.Now))

	cb.mu.Lock()
	cb.state = StateOpen
	cb.openedAt = clk.Now()
	cb.mu.Unlock()
	clk.Advance(2 * time.Second)

	if got := cb.State(); got != StateHalfOpen {
		t.Fatalf("state = %v, want half-open", got)
	}
	if err := cb.beforeRequest(); err != nil {
		t.Fatalf("trial should be admitted: %v", err)
	}
	cb.afterRequest(&APIError{StatusCode: 500}) // failed trial re-opens
	if got := cb.State(); got != StateOpen {
		t.Fatalf("state = %v, want open after a failed trial", got)
	}
}

func TestBreakerConcurrent(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(0, 0).UTC()}
	fake := &countingCompleter{err: &APIError{StatusCode: 500}}
	cb := NewCircuitBreaker(fake, 5, time.Second, WithClock(clk.Now))

	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cb.Complete(context.Background(), Request{})
			_ = cb.State()
		}()
	}
	wg.Wait()
}

func Example() {
	clk := &fakeClock{t: time.Unix(0, 0).UTC()}
	fake := &countingCompleter{err: &APIError{StatusCode: 500}}
	cb := NewCircuitBreaker(fake, 2, time.Second, WithClock(clk.Now))

	states := []string{cb.State().String()}
	_, _ = cb.Complete(context.Background(), Request{})
	_, _ = cb.Complete(context.Background(), Request{})
	states = append(states, cb.State().String())

	clk.Advance(2 * time.Second)
	states = append(states, cb.State().String())

	fake.setErr(nil)
	_, _ = cb.Complete(context.Background(), Request{})
	states = append(states, cb.State().String())

	fmt.Println(strings.Join(states, "->"))
	// Output: closed->open->half-open->closed
}
```

## Review

The budget caller is correct when a single attempt never outlives the caller's
deadline: `perAttemptTimeout` returns `min(ceiling, remaining)` and refuses once the
remaining is non-positive, which `TestPerAttemptTimeout` and `TestBudgetExhausted`
pin down. The `context.WithTimeoutCause` plumbing is what lets `TestAttemptTimeoutCause`
recover `ErrAttemptTimeout` through `context.Cause` even though `ctx.Err()` is the
generic `DeadlineExceeded`. The breaker is correct when open rejects with zero
downstream work (`TestBreakerOpensAndRejects` asserts the call count does not move),
when half-open admits exactly one trial (`TestBreakerHalfOpenSingleTrial` shows the
second `beforeRequest` is rejected), and when a failed trial re-opens
(`TestBreakerHalfOpenFailureReopens`).

The mistakes to avoid: giving each attempt a fresh copy of the full timeout so the
first attempt can eat the budget; putting the breaker *inside* a retry loop so one
request's own retries trip it; letting an open breaker still consume attempts;
transitioning breaker state without a mutex (run `TestBreakerConcurrent` under
`-race` to prove the guard). The clock is injected precisely so none of these tests
sleep — the cooldown is crossed by advancing a fake clock, and the whole suite runs
in microseconds.

## Resources

- [`context` package](https://pkg.go.dev/context) — `WithTimeoutCause`, `WithDeadline`, `Deadline`, and `Cause` for budget-aware timeouts.
- [Circuit Breaker (Martin Fowler)](https://martinfowler.com/bliki/CircuitBreaker.html) — the closed/open/half-open model this breaker implements.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — guarding shared breaker state read and written from many goroutines.
- [Google SRE Book — Handling Overload](https://sre.google/sre-book/handling-overload/) — why load-shedding beats retrying into a saturated dependency.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-retry-backoff-jitter.md](01-retry-backoff-jitter.md) | Next: [03-response-caching-and-singleflight.md](03-response-caching-and-singleflight.md)
