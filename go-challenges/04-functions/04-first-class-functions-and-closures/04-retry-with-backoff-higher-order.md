# Exercise 4: Retry Wrapper as a Higher-Order Function with Injected Sleep

Retry-with-backoff is a higher-order function: it takes the operation to retry as
a first-class argument and wraps it in attempt-counting, backoff, cancellation,
and permanent-error handling. This module builds `Retry(ctx, policy, op)` where
`op` is a `func(context.Context) error`, and the policy injects the sleeper and
jitter as function parameters so the test runs with zero real delay.

This module is fully self-contained.

## What you'll build

```text
retry/                     independent module: example.com/retry
  go.mod                   go 1.26
  retry.go                 Policy, Permanent, Retry (higher-order)
  cmd/
    demo/
      main.go              retries a flaky op that succeeds on the 3rd try
  retry_test.go            success-after-K, permanent stops immediately, ctx cancel
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `Retry(ctx, Policy, op func(context.Context) error) error` with `MaxAttempts`, `BaseDelay`, an injected `Sleep func(time.Duration)`, and a `Jitter func() float64`; plus a `Permanent(err)` sentinel wrapper that is never retried.
- Test: an op that fails K times then succeeds is called exactly K+1 times with a no-op sleeper; a `Permanent`-wrapped error returns immediately after one call; a cancelled context returns a wrapped `ctx.Err()` without further attempts.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/retry/cmd/demo
cd ~/go-exercises/retry
go mod init example.com/retry
```

### The operation is a parameter, and so is time

`Retry` is higher-order twice over: `op` is the function it retries, and `Sleep`
is the function it uses to wait between attempts. Taking `op` as
`func(context.Context) error` means the same retry logic wraps any cancellable
operation — an HTTP call, a DB query, a publish. Taking `Sleep` as a parameter
means the test injects a no-op `func(time.Duration){}` and the whole retry runs
instantly with zero wall-clock delay, while production injects `time.Sleep`.

Two error behaviors are non-negotiable in real retry code. First, **not every
error should be retried.** A 400 or a validation failure is permanent; retrying
it wastes attempts and delays the inevitable failure. `Permanent(err)` wraps an
error in a sentinel type; `Retry` checks for it with `errors.As` and returns the
underlying error immediately, without another attempt. Second, **cancellation
must stop the loop.** `Retry` checks `ctx.Err()` at the top of every attempt, so a
request cancelled mid-retry returns a wrapped `ctx.Err()` instead of spinning
through its remaining attempts.

Backoff is exponential: attempt `n` waits `BaseDelay * 2^(n-1)`, scaled by
`Jitter()` (production returns a random factor to de-synchronize clients; the
test returns a constant so the arithmetic is predictable). The captured
`attempt` counter is the closure-style private state of the loop, though here it
lives on the stack of the single `Retry` call rather than in a returned closure.

Create `retry.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// Policy configures Retry. Sleep and Jitter are injected so tests run without
// real delays or real randomness.
type Policy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	Sleep       func(time.Duration)
	Jitter      func() float64
}

type permanent struct{ err error }

func (p *permanent) Error() string { return p.err.Error() }
func (p *permanent) Unwrap() error { return p.err }

// Permanent marks err as non-retryable. Retry returns the underlying error
// immediately without another attempt.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanent{err: err}
}

// Retry calls op until it returns nil, returns a Permanent error, exhausts
// MaxAttempts, or ctx is cancelled. Between attempts it sleeps
// BaseDelay*2^(attempt-1) scaled by Jitter, using the injected Sleep.
func Retry(ctx context.Context, p Policy, op func(context.Context) error) error {
	var last error
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("retry: %w", err)
		}

		err := op(ctx)
		if err == nil {
			return nil
		}

		var perm *permanent
		if errors.As(err, &perm) {
			return perm.err
		}
		last = err

		if attempt == p.MaxAttempts {
			break
		}
		if p.Sleep != nil {
			p.Sleep(backoff(p, attempt))
		}
	}
	return fmt.Errorf("retry: exhausted after %d attempts: %w", p.MaxAttempts, last)
}

func backoff(p Policy, attempt int) time.Duration {
	d := float64(p.BaseDelay) * math.Pow(2, float64(attempt-1))
	if p.Jitter != nil {
		d *= p.Jitter()
	}
	return time.Duration(d)
}
```

### The runnable demo

The demo retries an operation that fails twice with a transient error and
succeeds on the third attempt, using a no-op sleeper so it returns immediately.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/retry"
)

func main() {
	attempts := 0
	op := func(ctx context.Context) error {
		attempts++
		if attempts < 3 {
			return fmt.Errorf("attempt %d: connection refused", attempts)
		}
		return nil
	}

	p := retry.Policy{
		MaxAttempts: 5,
		BaseDelay:   10 * time.Millisecond,
		Sleep:       func(time.Duration) {}, // no real delay
		Jitter:      func() float64 { return 1 },
	}

	err := retry.Retry(context.Background(), p, op)
	fmt.Printf("attempts=%d err=%v\n", attempts, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempts=3 err=<nil>
```

### Tests

Create `retry_test.go`:

```go
package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errFlaky = errors.New("flaky")

func noopPolicy(maxAttempts int) Policy {
	return Policy{
		MaxAttempts: maxAttempts,
		BaseDelay:   time.Millisecond,
		Sleep:       func(time.Duration) {},
		Jitter:      func() float64 { return 1 },
	}
}

func TestRetrySucceedsAfterKFailures(t *testing.T) {
	t.Parallel()
	const failures = 2
	calls := 0
	op := func(ctx context.Context) error {
		calls++
		if calls <= failures {
			return errFlaky
		}
		return nil
	}

	if err := Retry(context.Background(), noopPolicy(5), op); err != nil {
		t.Fatalf("Retry err = %v, want nil", err)
	}
	if calls != failures+1 {
		t.Fatalf("op called %d times, want %d", calls, failures+1)
	}
}

func TestRetryStopsOnPermanent(t *testing.T) {
	t.Parallel()
	errBadRequest := errors.New("bad request")
	calls := 0
	op := func(ctx context.Context) error {
		calls++
		return Permanent(errBadRequest)
	}

	err := Retry(context.Background(), noopPolicy(5), op)
	if !errors.Is(err, errBadRequest) {
		t.Fatalf("err = %v, want to wrap errBadRequest", err)
	}
	if calls != 1 {
		t.Fatalf("op called %d times, want 1 (permanent must not retry)", calls)
	}
}

func TestRetryExhaustsAndWrapsLast(t *testing.T) {
	t.Parallel()
	calls := 0
	op := func(ctx context.Context) error {
		calls++
		return errFlaky
	}

	err := Retry(context.Background(), noopPolicy(3), op)
	if !errors.Is(err, errFlaky) {
		t.Fatalf("err = %v, want to wrap errFlaky", err)
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want 3", calls)
	}
}

func TestRetryHonorsContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	calls := 0
	op := func(ctx context.Context) error {
		calls++
		return errFlaky
	}

	err := Retry(ctx, noopPolicy(5), op)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Fatalf("op called %d times, want 0 (cancelled before first attempt)", calls)
	}
}
```

## Review

The retry is correct when it calls `op` exactly `K+1` times for `K` transient
failures, stops after one call on a `Permanent` error, wraps the last error on
exhaustion so `errors.Is` still matches the underlying cause, and returns a
wrapped `ctx.Err()` the instant the context is cancelled. Because `Sleep` is
injected, every test runs with a no-op sleeper and finishes instantly — no
`time.Sleep`, no flakiness. The two defects to avoid are retrying a permanent
error (waste and delay) and ignoring cancellation (a dead request that keeps
spinning); both are asserted directly with a captured call counter. Run
`go test -race`.

## Resources

- [pkg.go.dev: errors.As](https://pkg.go.dev/errors#As) — matching the `Permanent` sentinel type.
- [pkg.go.dev: context.Context](https://pkg.go.dev/context#Context) — `Err()` and cancellation.
- [pkg.go.dev: math/rand/v2 Float64](https://pkg.go.dev/math/rand/v2#Float64) — the production jitter source.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-rate-limiter-token-bucket-closure.md](03-rate-limiter-token-bucket-closure.md) | Next: [05-memoize-config-loader.md](05-memoize-config-loader.md)
