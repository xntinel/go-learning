# 10. Retry with Exponential Backoff and Jitter

Transient failures — network blips, brief leader elections, temporary overloads — are normal in distributed systems. The hard part is not retrying; it is retrying correctly. Naive strategies (immediate retries, fixed delays, or synchronized retries) can overwhelm a recovering service worse than the original failure. This lesson builds a generic retry library using exponential backoff and jitter, implements correct error classification (retryable vs. permanent), and wires it to a resilient HTTP client. The interesting design question is how to make the library composable and testable without leaking timer internals.

```text
retry/
  go.mod
  retry.go
  retry_test.go
  cmd/demo/main.go
```

## Concepts

### Exponential Backoff

After the k-th failure (zero-indexed), wait `base * multiplier^k` before the next attempt, capped at a maximum delay. The cap prevents unbounded waits when the service is down for a long time; the exponential growth gives the service progressively more recovery time between burst retries.

The formula in Go:

```go
delay := time.Duration(float64(cfg.Base) * math.Pow(cfg.Multiplier, float64(attempt)))
if delay > cfg.MaxDelay {
	delay = cfg.MaxDelay
}
```

Typical values: base 100 ms, multiplier 2.0, cap 30 s. After five attempts the delays are 100 ms, 200 ms, 400 ms, 800 ms, 1.6 s — far enough apart to survive short outages without blocking for minutes.

### Jitter and the Thundering Herd

Without jitter, every client that started at the same time (after a deployment, a timeout storm, or a mass reconnect) will sleep for exactly the same interval and retry at the same instant. When the service recovers, all of them hit it simultaneously — a thundering herd that can re-trigger the very failure they recovered from.

Full jitter picks the actual sleep duration uniformly at random in `[0, delay)`:

```go
actual := rand.N(delay) // math/rand/v2 generic N accepts time.Duration
```

The AWS architecture blog (2015, updated 2022) simulated all strategies and found full jitter reduces both total client work and contention better than equal jitter or decorrelated jitter, at the cost of occasionally very short waits — which is fine because the next attempt pays a longer cap anyway.

Equal jitter (`delay/2 + rand.N(delay/2)`) guarantees a minimum wait half the computed delay, at the cost of somewhat higher collision probability. Both are correct; full jitter is the default.

### Error Classification

Not every error warrants a retry. Retrying a 401 Unauthorized, a 403 Forbidden, or a 404 Not Found is pointless: those statuses reflect a property of the request that will not change between attempts. Only transient errors deserve retries:

- Network-level errors (refused connections, timeouts, resets)
- HTTP 429 Too Many Requests (rate-limited; may include a Retry-After header)
- HTTP 502, 503, 504 (gateway or upstream errors)

The library uses a sentinel wrapper type `PermanentError` so callers can classify any error as non-retryable:

```go
return Permanent(fmt.Errorf("invalid credentials: %w", ErrUnauthorized))
```

`Retry` checks for `*PermanentError` via `errors.As` and stops immediately if the wrapper is found. Callers test with `errors.Is(err, ErrUnauthorized)` — the wrapping is transparent.

### Context Deadlines

A retry loop must check the context between every attempt. If the caller's deadline expires during a backoff sleep, sleeping longer is wrong — the result is already stale. Use a `select` with `ctx.Done()` and a timer channel so the goroutine wakes up on whichever fires first:

```go
select {
case <-time.After(delay):
case <-ctx.Done():
	return RetryResult{Attempts: attempt + 1, Err: fmt.Errorf("retry: %w", ctx.Err())}
}
```

As of Go 1.23, the garbage collector can recover an unstopped `time.After` timer once it is no longer referenced, so the older advice to prefer `time.NewTimer` for short-lived loops no longer applies.

### Idempotency

Retrying a non-idempotent operation (an HTTP POST that creates a resource, a charge) can cause duplicate side effects. The library does not solve idempotency — it cannot — but callers should only pass operations that are safe to repeat, or use server-side idempotency keys before enabling retries.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/retry/cmd/demo
cd ~/go-exercises/retry
go mod init example.com/retry
```

This is a library. You verify it with `go test`, not `go run`.

### Exercise 1: The Core Types

Create `retry.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// ErrAllAttemptsFailed is returned (wrapped) when every attempt fails and
// the last error was not a PermanentError.
var ErrAllAttemptsFailed = errors.New("all attempts failed")

// PermanentError wraps an error that should never be retried.
// Use Permanent to construct one; errors.Is / errors.As unwrap through it.
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as non-retryable. Retry stops immediately when it
// encounters a PermanentError, without consuming more attempts.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// Config controls the retry behaviour.
type Config struct {
	// MaxAttempts is the total number of calls, including the first one.
	// Zero or negative means one attempt (no retries).
	MaxAttempts int

	// Base is the delay before the second attempt.
	Base time.Duration

	// MaxDelay caps the computed exponential delay.
	MaxDelay time.Duration

	// Multiplier is the base of the exponential. 2.0 is a standard choice.
	Multiplier float64

	// Jitter, if set, randomises the computed delay to spread retries.
	// Use FullJitter or EqualJitter; nil disables jitter.
	Jitter func(computed time.Duration) time.Duration
}

// DefaultConfig returns sensible defaults for most RPC retry scenarios.
func DefaultConfig() Config {
	return Config{
		MaxAttempts: 5,
		Base:        100 * time.Millisecond,
		MaxDelay:    30 * time.Second,
		Multiplier:  2.0,
		Jitter:      FullJitter,
	}
}

// FullJitter returns a uniformly random duration in [0, d).
// It is the strategy recommended by the AWS architecture blog.
func FullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return rand.N(d)
}

// EqualJitter returns d/2 + uniform random in [0, d/2).
// It guarantees at least half the computed delay.
func EqualJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + rand.N(half+1)
}

// Result is returned by Do and carries the number of attempts made and the
// final error (nil on success).
type Result struct {
	// Attempts is the number of times the operation was called.
	Attempts int
	// Err is nil on success, non-nil on failure.
	Err error
}

// Do calls op repeatedly until it returns nil, returns a PermanentError,
// cfg.MaxAttempts is exhausted, or ctx is cancelled/expired.
//
// The returned Result always has Attempts >= 1.
func Do(ctx context.Context, cfg Config, op func(ctx context.Context) error) Result {
	max := cfg.MaxAttempts
	if max <= 0 {
		max = 1
	}

	var lastErr error
	for attempt := 0; attempt < max; attempt++ {
		if err := ctx.Err(); err != nil {
			return Result{Attempts: attempt + 1, Err: fmt.Errorf("retry: %w", err)}
		}

		err := op(ctx)
		if err == nil {
			return Result{Attempts: attempt + 1}
		}

		var perm *PermanentError
		if errors.As(err, &perm) {
			return Result{Attempts: attempt + 1, Err: err}
		}

		lastErr = err

		// No sleep after the final attempt.
		if attempt == max-1 {
			break
		}

		delay := time.Duration(float64(cfg.Base) * math.Pow(cfg.Multiplier, float64(attempt)))
		if delay > cfg.MaxDelay {
			delay = cfg.MaxDelay
		}
		if cfg.Jitter != nil {
			delay = cfg.Jitter(delay)
		}

		select {
		case <-ctx.Done():
			return Result{
				Attempts: attempt + 1,
				Err:      fmt.Errorf("retry: %w", ctx.Err()),
			}
		case <-time.After(delay):
		}
	}

	return Result{
		Attempts: max,
		Err:      fmt.Errorf("%w after %d attempt(s): %w", ErrAllAttemptsFailed, max, lastErr),
	}
}
```

`Do` is the whole library surface. Every branch is covered by a test below: success on first attempt, success after transient failures, permanent stop, context cancellation, and exhausted attempts.

### Exercise 2: Test the Library

Create `retry_test.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

var errTransient = errors.New("transient")
var errBusiness = errors.New("business rule violated")

func TestDoSucceedsFirstAttempt(t *testing.T) {
	t.Parallel()

	r := Do(context.Background(), DefaultConfig(), func(_ context.Context) error {
		return nil
	})
	if r.Err != nil {
		t.Fatalf("Err = %v, want nil", r.Err)
	}
	if r.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", r.Attempts)
	}
}

func TestDoRetriesTransientErrors(t *testing.T) {
	t.Parallel()

	calls := 0
	cfg := Config{
		MaxAttempts: 4,
		Base:        time.Millisecond,
		MaxDelay:    10 * time.Millisecond,
		Multiplier:  2.0,
	}
	r := Do(context.Background(), cfg, func(_ context.Context) error {
		calls++
		if calls < 3 {
			return errTransient
		}
		return nil
	})
	if r.Err != nil {
		t.Fatalf("Err = %v, want nil", r.Err)
	}
	if r.Attempts != 3 {
		t.Fatalf("Attempts = %d, want 3", r.Attempts)
	}
}

func TestDoStopsOnPermanentError(t *testing.T) {
	t.Parallel()

	calls := 0
	cfg := Config{MaxAttempts: 10, Base: time.Millisecond, MaxDelay: time.Second, Multiplier: 2.0}
	r := Do(context.Background(), cfg, func(_ context.Context) error {
		calls++
		return Permanent(fmt.Errorf("not allowed: %w", errBusiness))
	})
	if calls != 1 {
		t.Fatalf("called %d times, want 1 (permanent error must stop immediately)", calls)
	}
	if !errors.Is(r.Err, errBusiness) {
		t.Fatalf("Err = %v, want to unwrap to errBusiness", r.Err)
	}
}

func TestDoExhaustsMaxAttempts(t *testing.T) {
	t.Parallel()

	cfg := Config{MaxAttempts: 3, Base: time.Millisecond, MaxDelay: 5 * time.Millisecond, Multiplier: 2.0}
	r := Do(context.Background(), cfg, func(_ context.Context) error {
		return errTransient
	})
	if r.Attempts != 3 {
		t.Fatalf("Attempts = %d, want 3", r.Attempts)
	}
	if !errors.Is(r.Err, ErrAllAttemptsFailed) {
		t.Fatalf("Err = %v, want to wrap ErrAllAttemptsFailed", r.Err)
	}
	if !errors.Is(r.Err, errTransient) {
		t.Fatalf("Err = %v, want to unwrap to errTransient", r.Err)
	}
}

func TestDoRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before first call

	cfg := Config{MaxAttempts: 5, Base: time.Millisecond, MaxDelay: time.Second, Multiplier: 2.0}
	r := Do(ctx, cfg, func(_ context.Context) error {
		return errTransient
	})
	if !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", r.Err)
	}
}

func TestDoRespectsContextDeadlineDuringBackoff(t *testing.T) {
	t.Parallel()

	// The operation always fails; the deadline expires during the first backoff.
	cfg := Config{
		MaxAttempts: 10,
		Base:        200 * time.Millisecond,
		MaxDelay:    time.Second,
		Multiplier:  2.0,
		Jitter:      nil, // no jitter so the delay is deterministic
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	r := Do(ctx, cfg, func(_ context.Context) error {
		return errTransient
	})
	elapsed := time.Since(start)

	if !errors.Is(r.Err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want context.DeadlineExceeded", r.Err)
	}
	// Should have stopped well before the 10 * 200 ms budget.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("took %v, deadline should have cut it off sooner", elapsed)
	}
}

func TestFullJitterRange(t *testing.T) {
	t.Parallel()

	const base = 100 * time.Millisecond
	for range 200 {
		got := FullJitter(base)
		if got < 0 || got >= base {
			t.Fatalf("FullJitter(%v) = %v, want in [0, %v)", base, got, base)
		}
	}
}

func TestEqualJitterAtLeastHalf(t *testing.T) {
	t.Parallel()

	const base = 100 * time.Millisecond
	for range 200 {
		got := EqualJitter(base)
		if got < base/2 {
			t.Fatalf("EqualJitter(%v) = %v, want >= %v", base, got, base/2)
		}
		if got > base {
			t.Fatalf("EqualJitter(%v) = %v, want <= %v", base, got, base)
		}
	}
}

func ExampleDo() {
	calls := 0
	cfg := Config{
		MaxAttempts: 3,
		Base:        time.Millisecond,
		MaxDelay:    10 * time.Millisecond,
		Multiplier:  2.0,
	}
	r := Do(context.Background(), cfg, func(_ context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("not ready")
		}
		return nil
	})
	fmt.Printf("attempts=%d err=%v\n", r.Attempts, r.Err)
	// Output:
	// attempts=3 err=<nil>
}

func ExamplePermanent() {
	sentinel := errors.New("auth failed")
	cfg := Config{MaxAttempts: 5, Base: time.Millisecond, MaxDelay: time.Second, Multiplier: 2.0}
	r := Do(context.Background(), cfg, func(_ context.Context) error {
		return Permanent(fmt.Errorf("unauthorized: %w", sentinel))
	})
	fmt.Printf("attempts=%d unwraps=%v\n", r.Attempts, errors.Is(r.Err, sentinel))
	// Output:
	// attempts=1 unwraps=true
}
```

Your turn: add `TestPermanentNilReturnsNil` that calls `Permanent(nil)` and asserts the return value is `nil`.

### Exercise 3: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	"example.com/retry"
)

func main() {
	// Spin up a test server that fails the first three requests, then succeeds.
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n <= 3 {
			log.Printf("server: request %d -> 503", n)
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		log.Printf("server: request %d -> 200", n)
		fmt.Fprintln(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	cfg := retry.Config{
		MaxAttempts: 6,
		Base:        20 * time.Millisecond,
		MaxDelay:    500 * time.Millisecond,
		Multiplier:  2.0,
		Jitter:      retry.FullJitter,
	}

	// Demo 1: transient failures then success.
	fmt.Println("--- demo 1: transient failures then success ---")
	r := retry.Do(context.Background(), cfg, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			return retry.Permanent(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if resp.StatusCode >= 400 {
			return retry.Permanent(fmt.Errorf("HTTP %d (permanent)", resp.StatusCode))
		}
		return nil
	})
	if r.Err != nil {
		log.Fatalf("unexpected error: %v", r.Err)
	}
	fmt.Printf("succeeded after %d attempt(s)\n\n", r.Attempts)

	// Demo 2: context deadline cuts the retry loop short.
	fmt.Println("--- demo 2: context deadline ---")
	count.Store(0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r2 := retry.Do(ctx, cfg, func(ctx context.Context) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return nil
	})
	fmt.Printf("stopped after %d attempt(s): deadline=%v\n\n", r2.Attempts, errors.Is(r2.Err, context.DeadlineExceeded))

	// Demo 3: permanent error stops immediately.
	fmt.Println("--- demo 3: permanent error ---")
	sentinel := errors.New("bad credentials")
	r3 := retry.Do(context.Background(), cfg, func(_ context.Context) error {
		return retry.Permanent(fmt.Errorf("401: %w", sentinel))
	})
	fmt.Printf("stopped after %d attempt(s), unwraps sentinel=%v\n", r3.Attempts, errors.Is(r3.Err, sentinel))
}
```

Run with:

```bash
go run ./cmd/demo
```

## Common Mistakes

**Retrying non-idempotent operations without idempotency keys**

Wrong: retry a POST that creates a charge. Each retry creates another charge.

Fix: use a server-side idempotency key (`Idempotency-Key` header) so the server deduplicates. Only then is it safe to retry.

**No jitter — synchronized retries**

Wrong: all 500 clients restart with the same base delay after a shared timeout. They all retry at t=0.1 s, t=0.2 s, etc. The herd overwhelms the recovering server.

Fix: use `FullJitter` or `EqualJitter`. The `Jitter` field on `Config` is `nil` by default only for the zero value; `DefaultConfig()` sets `FullJitter`.

**Retrying permanent errors**

Wrong: retry a 404 or a 401. These reflect a property of the request (missing resource, wrong credentials) that will not change between attempts. Each retry wastes time and may trigger rate-limiting.

Fix: classify the error as permanent. In the HTTP client case, return `retry.Permanent(...)` for any 4xx that is not 429.

**Not checking context before the first attempt**

Wrong:

```go
func Do(...) Result {
	for attempt := 0; attempt < max; attempt++ {
		op(ctx) // ctx might already be done
```

Fix: check `ctx.Err()` at the top of every iteration, before calling `op`. The library does this in Exercise 1.

**Sleeping inside the operation instead of between attempts**

Wrong: the operation calls `time.Sleep` when it gets a 429. This blocks the goroutine, ignores the context, and double-counts the sleep (both inside `op` and in the retry loop).

Fix: return an error from the operation; let the retry loop handle all sleeping so context cancellation is always respected.

## Verification

From `~/go-exercises/retry`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The test output should include the two `Example` functions being verified automatically by `go test`.

## Summary

- Exponential backoff: `delay = base * multiplier^attempt`, capped at a maximum.
- Full jitter (`rand.N(delay)`) is the recommended default; equal jitter (`delay/2 + rand.N(delay/2)`) guarantees a minimum wait.
- Classify errors as retryable (network errors, 429, 502/503/504) or permanent (most 4xx). Wrap permanent errors with `Permanent(err)`.
- Check `ctx.Err()` before every attempt and use a `select` during backoff sleep so context cancellation is always respected.
- Never retry non-idempotent operations without a server-side idempotency key.
- The `*_test.go` is the verification; `cmd/demo` shows the public API in a realistic scenario.

## What's Next

Next: [11. Timeout Budgets](../11-timeout-budgets/11-timeout-budgets.md).

## Resources

- [Exponential Backoff and Jitter — AWS Architecture Blog](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — primary source for jitter strategy analysis and formulas.
- [pkg.go.dev/math/rand/v2](https://pkg.go.dev/math/rand/v2) — generic `N` function; accepts `time.Duration` directly.
- [pkg.go.dev/time](https://pkg.go.dev/time) — `time.After` goroutine recovery note (Go 1.23+), `time.Duration` arithmetic.
- [pkg.go.dev/context](https://pkg.go.dev/context) — `ctx.Err()`, `context.DeadlineExceeded`, `context.Canceled`.
- [Go Code Review Comments — Error Wrapping](https://go.dev/wiki/CodeReviewComments#error-wrapping) — wrapping with `%w` and the `errors.Is` contract.
