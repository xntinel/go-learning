# Exercise 7: Retrying Client That Spends One Deadline Budget

A resilient client retries idempotent calls on transient failures, but the naive
loop resets the timeout each attempt — turning one logical request into a
multiple of its SLA. This exercise builds `DoWithRetry`, which treats the inbound
context deadline as a single shared budget: it never resets the deadline, aborts
the moment the remaining budget is smaller than the next backoff, and returns
`context.Cause(ctx)` when the budget is exhausted mid-flight.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
budgetretry/               independent module: example.com/budgetretry
  go.mod                   go 1.26
  retry.go                 RetryPolicy; DoWithRetry(ctx, client, req, policy)
  cmd/
    demo/
      main.go              succeeds after N failures within budget
  retry_test.go            eventual-success test, budget-exhausted test
```

Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
Implement: `DoWithRetry(ctx, client, req, policy)` retrying idempotent GETs on 5xx/transport errors with exponential backoff + injectable jitter, spending one shared `ctx` deadline; clones the request per attempt via `Request.Clone` and rewinds the body via `GetBody`.
Test: server fails N times then succeeds — generous budget yields eventual `200`; tight budget returns before exhausting retries, `errors.Is(err, context.DeadlineExceeded)`, elapsed within budget.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/budgetretry/cmd/demo
cd ~/go-exercises/budgetretry
go mod init example.com/budgetretry
```

## The design

The core discipline is that the deadline lives on `ctx` and is never touched. Each
attempt runs against a clone of the request bound to that same `ctx`
(`req.Clone(ctx)`), so the transport enforces the one shared deadline across every
attempt's dial/handshake/response wait. Between attempts, the loop computes the
exponential backoff for the next attempt and then checks the *remaining* budget:
if `time.Until(deadline)` is not greater than the backoff, there is no point
sleeping — the deadline would fire mid-sleep — so it aborts immediately and
returns a `DeadlineExceeded`-wrapped error. This is what keeps a logical request
inside its SLA instead of letting three "up to two seconds" attempts run six.

Backoff is `base * 2^attempt` capped at `MaxDelay`, then passed through an
injectable `Jitter` function. Real jitter uses a random source; tests inject an
identity function so timing is deterministic and the budget arithmetic is
exactly assertable. The actual sleep is a `select` on `ctx.Done()` versus a
`time.NewTimer`, so a cancellation during the backoff returns `context.Cause(ctx)`
promptly (and stops the timer to avoid leaking it).

Request cloning and body rewind matter for correctness even though the tests use
GET: `Request.Clone(ctx)` deep-copies headers but shares the body reader, so for a
request with a body you must reset it per attempt via `GetBody` (which
`http.NewRequest*` populates for in-memory bodies). Retrying without rewinding
sends an empty or partial body on the second attempt. The helper handles it
generically.

Retry triggers are transport errors and `5xx` responses; a `4xx` is the client's
own fault and is returned immediately (not retried). On a `5xx` the response body
is drained and closed before the next attempt so the connection can be reused.

Create `retry.go`:

```go
package budgetretry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// RetryPolicy configures DoWithRetry. Jitter is injectable so tests can make
// backoff deterministic; in production it randomizes to avoid thundering herds.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      func(time.Duration) time.Duration
}

func (p RetryPolicy) backoff(attempt int) time.Duration {
	d := p.BaseDelay << attempt // base * 2^attempt
	if d > p.MaxDelay || d <= 0 {
		d = p.MaxDelay
	}
	if p.Jitter != nil {
		d = p.Jitter(d)
	}
	return d
}

// DoWithRetry retries an idempotent request on transport errors and 5xx
// responses, spending ctx's deadline as a single shared budget across all
// attempts. It never resets the deadline, and aborts as soon as the remaining
// budget is not larger than the next backoff, returning a DeadlineExceeded-
// wrapped error. On a mid-backoff cancellation it returns context.Cause(ctx).
func DoWithRetry(ctx context.Context, client *http.Client, req *http.Request, policy RetryPolicy) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < policy.MaxAttempts; attempt++ {
		r := req.Clone(ctx)
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			r.Body = body
		}

		resp, err := client.Do(r)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			lastErr = err
		case resp.StatusCode < 500:
			return resp, nil
		default:
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("upstream status %d", resp.StatusCode)
		}

		if attempt == policy.MaxAttempts-1 {
			break
		}

		delay := policy.backoff(attempt)
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			return nil, fmt.Errorf("retry budget exhausted after %d attempts: %w", attempt+1, context.DeadlineExceeded)
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, context.Cause(ctx)
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("all %d attempts failed: %w", policy.MaxAttempts, lastErr)
}
```

## The runnable demo

The demo points `DoWithRetry` at a server that fails twice then succeeds, with a
generous budget, and prints the final status and the attempt count.

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

	"example.com/budgetretry"
)

func main() {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)

	policy := budgetretry.RetryPolicy{
		MaxAttempts: 5,
		BaseDelay:   5 * time.Millisecond,
		MaxDelay:    50 * time.Millisecond,
		Jitter:      func(d time.Duration) time.Duration { return d },
	}
	resp, err := budgetretry.DoWithRetry(ctx, http.DefaultClient, req, policy)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	resp.Body.Close()
	fmt.Printf("status=%d attempts=%d\n", resp.StatusCode, calls.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=200 attempts=3
```

## Tests

`TestEventualSuccess` gives a generous budget and asserts the call reaches `200`
after exactly the expected number of attempts (an atomic counter on the server).
`TestBudgetExhausted` gives a tight budget against an always-failing server and
asserts three things at once: the call returns *before* exhausting `MaxAttempts`,
the error is `DeadlineExceeded` (via `errors.Is`), and the total elapsed time
stays within the budget — the proof that the budget is shared, not reset per
attempt. Both inject an identity `Jitter` for deterministic timing.

Create `retry_test.go`:

```go
package budgetretry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func noJitter(d time.Duration) time.Duration { return d }

func TestEventualSuccess(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}

	policy := RetryPolicy{MaxAttempts: 5, BaseDelay: 5 * time.Millisecond, MaxDelay: 50 * time.Millisecond, Jitter: noJitter}
	resp, err := DoWithRetry(ctx, http.DefaultClient, req, policy)
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestBudgetExhausted(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	budget := 60 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}

	// Backoff grows 20 -> 40ms; by attempt 1 the 40ms backoff no longer fits
	// the remaining budget, so the loop aborts well before MaxAttempts.
	policy := RetryPolicy{MaxAttempts: 10, BaseDelay: 20 * time.Millisecond, MaxDelay: time.Second, Jitter: noJitter}

	start := time.Now()
	_, err = DoWithRetry(ctx, http.DefaultClient, req, policy)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if got := calls.Load(); got >= 10 {
		t.Fatalf("attempts = %d, want fewer than MaxAttempts (budget must cut it short)", got)
	}
	if elapsed > budget+50*time.Millisecond {
		t.Fatalf("elapsed = %v, want within the %v budget (deadline must be shared, not reset)", elapsed, budget)
	}
}
```

## Review

The client is correct when the deadline is spent, never reset: every attempt
clones the request against the same `ctx`, and the loop refuses to start a backoff
it cannot afford. The budget-exhausted test is the load-bearing one — it asserts
not just the error but that the elapsed wall time stayed within the budget, which
is the only way to prove the deadline was shared rather than reset per attempt.
The mistakes this guards against are resetting the timeout each attempt (SLA
blowout), forgetting `GetBody` (empty body on retry of a request with a payload),
and leaking the backoff timer on cancellation (fixed with `timer.Stop()`).
Inject a deterministic `Jitter` in tests; keep the randomized one in production
to avoid synchronized retry storms. Run with `-race`.

## Resources

- [`http.Request.Clone`](https://pkg.go.dev/net/http#Request.Clone) — per-attempt request copies bound to the shared context.
- [`http.Request.GetBody`](https://pkg.go.dev/net/http#Request) — rewinding an in-memory body for a retry.
- [`context.Cause`](https://pkg.go.dev/context#Cause) — the error returned when the budget is exhausted mid-flight.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-detached-background-work.md](06-detached-background-work.md) | Next: [08-outbound-httptrace-timing.md](08-outbound-httptrace-timing.md)
