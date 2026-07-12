# Exercise 6: A Retryable Error Type Consumed by a Retry Loop

Whether an operation should be retried is a property of the *error*, not a guess
by the retry loop. This module builds a `TransientError` implementing a
`Retryable() bool` contract and carrying a `RetryAfter` hint, plus a small retry
helper that uses `errors.As` to decide whether to retry and honors the hint. The
error type is the contract between the failing layer and the backoff loop.

This module is fully self-contained: its own module, code, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
retryclass/                independent module: example.com/retryclass
  go.mod                   go 1.24
  retryclass.go            TransientError (Retryable, RetryAfter); Retrier.Do
  cmd/
    demo/
      main.go              runs a flaky op that succeeds on the 3rd attempt
  retryclass_test.go       retries-then-succeeds, permanent-immediate, ctx, hint
```

Files: `retryclass.go`, `cmd/demo/main.go`, `retryclass_test.go`.
Implement: `*TransientError` with `Retryable() bool`, `Unwrap()`, and a `RetryAfter` field; a `Retrier` with an injectable sleep and a `Do(ctx, op)` that retries only classified-transient errors.
Test: an op failing N times then succeeding is retried to success; a permanent error returns immediately; the loop honors `ctx` cancellation via `t.Context()`; the `RetryAfter` hint is read and passed to the (fake) sleep.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The error is the retry contract

A retry loop that inspects error *strings* to decide retryability
(`strings.Contains(err.Error(), "timeout")`) is fragile and couples the loop to
every message it might see. The durable design is to let the failing layer
*classify* its own errors: a layer that knows a failure is transient (a connection
reset, a 503, a lock timeout) returns a `*TransientError`, and the retry loop asks
the error, via `errors.As`, whether it is retryable and how long to wait.

`TransientError` carries three things: the wrapped cause (`Unwrap`), a
`Retryable() bool` method (always true for this type — its very existence is the
signal), and a `RetryAfter time.Duration` hint that a server can set from a
`Retry-After` header or a backend's advice. The loop reads all three. A permanent
failure is simply *not* a `*TransientError`, so `errors.As` misses and the loop
returns immediately — no retry budget wasted on an error that will never succeed.

### Injecting the sleep to keep tests deterministic

The loop must wait `RetryAfter` between attempts, but a test must not burn real
wall-clock seconds. So `Retrier` holds a `sleep func(context.Context,
time.Duration) error` field. The production default blocks on a `time.Timer` while
watching `ctx.Done()`; a test injects a fake that records the requested durations
and returns instantly. This keeps the retry logic honest (it really does compute
and pass the delay) while the test asserts the *value* passed, not a real elapsed
time — the same discipline a full backoff library uses. (Backoff schedules and
jitter live in Chapter 12; this module is only about the error-as-contract.)

The sleep also carries the cancellation semantics: it returns `ctx.Err()` if the
context is done, so `Do` propagates cancellation promptly instead of sleeping
through it.

Create `retryclass.go`:

```go
// Package retryclass shows a TransientError type acting as the contract between a
// failing layer and a retry loop: the error decides whether a retry is warranted
// and how long to wait.
package retryclass

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TransientError marks a temporary failure worth retrying. Its existence in the
// chain is the retry signal; RetryAfter is the suggested wait before the next
// attempt.
type TransientError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *TransientError) Error() string {
	return fmt.Sprintf("transient (retry after %s): %v", e.RetryAfter, e.Err)
}

func (e *TransientError) Unwrap() error { return e.Err }

// Retryable reports that this error class is worth retrying.
func (e *TransientError) Retryable() bool { return true }

// NewTransient wraps a cause as a retryable error with a wait hint.
func NewTransient(cause error, retryAfter time.Duration) *TransientError {
	return &TransientError{Err: cause, RetryAfter: retryAfter}
}

// Retrier runs an operation with bounded retries. sleep is injectable so tests
// are deterministic and do not burn wall-clock time.
type Retrier struct {
	MaxAttempts int
	sleep       func(ctx context.Context, d time.Duration) error
}

// NewRetrier builds a Retrier with the production time-based sleep.
func NewRetrier(maxAttempts int) *Retrier {
	return &Retrier{MaxAttempts: maxAttempts, sleep: sleepCtx}
}

// sleepCtx blocks for d or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Do runs op up to MaxAttempts times. It retries ONLY when op returns an error
// that classifies as a *TransientError AND that error's Retryable() reports true,
// waiting the error's RetryAfter between attempts. A non-transient (or explicitly
// non-retryable) error is returned immediately.
func (r *Retrier) Do(ctx context.Context, op func() error) error {
	var err error
	for attempt := 0; attempt < r.MaxAttempts; attempt++ {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}

		err = op()
		if err == nil {
			return nil
		}

		var te *TransientError
		if !errors.As(err, &te) || !te.Retryable() {
			return err // permanent (or explicitly non-retryable): do not retry
		}
		if attempt == r.MaxAttempts-1 {
			break // out of attempts; return the last transient error
		}
		if serr := r.sleep(ctx, te.RetryAfter); serr != nil {
			return serr // context cancelled during the wait
		}
	}
	return err
}
```

### The runnable demo

The demo runs an operation that fails twice with a transient error and succeeds on
the third attempt, printing each attempt so you can watch the retry.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/retryclass"
)

func main() {
	attempts := 0
	op := func() error {
		attempts++
		fmt.Printf("attempt %d\n", attempts)
		if attempts < 3 {
			return retryclass.NewTransient(errors.New("connection reset"), 1*time.Millisecond)
		}
		return nil
	}

	r := retryclass.NewRetrier(5)
	err := r.Do(context.Background(), op)
	fmt.Printf("result: err=%v after %d attempts\n", err, attempts)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempt 1
attempt 2
attempt 3
result: err=<nil> after 3 attempts
```

### Tests

The tests inject a fake sleep so no real time passes. `TestRetriesThenSucceeds`
fails twice then succeeds and asserts the attempt count and the recorded waits.
`TestPermanentReturnsImmediately` asserts a non-transient error is returned after a
single attempt. `TestHonorsContextCancellation` uses `t.Context()` with a cancelled
child and asserts the loop returns the context error. `TestReadsRetryAfterHint`
asserts the exact `RetryAfter` value reached the sleep.

Create `retryclass_test.go`:

```go
package retryclass

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newFakeRetrier returns a Retrier whose sleep records durations and never waits.
func newFakeRetrier(maxAttempts int, recorded *[]time.Duration) *Retrier {
	return &Retrier{
		MaxAttempts: maxAttempts,
		sleep: func(ctx context.Context, d time.Duration) error {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			*recorded = append(*recorded, d)
			return nil
		},
	}
}

func TestRetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	var waits []time.Duration
	r := newFakeRetrier(5, &waits)

	attempts := 0
	op := func() error {
		attempts++
		if attempts < 3 {
			return NewTransient(errors.New("reset"), 250*time.Millisecond)
		}
		return nil
	}

	if err := r.Do(t.Context(), op); err != nil {
		t.Fatalf("Do = %v; want nil", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d; want 3", attempts)
	}
	if len(waits) != 2 {
		t.Errorf("waits = %d; want 2 (between the 3 attempts)", len(waits))
	}
}

func TestPermanentReturnsImmediately(t *testing.T) {
	t.Parallel()
	var waits []time.Duration
	r := newFakeRetrier(5, &waits)

	permanent := errors.New("400 bad request")
	attempts := 0
	op := func() error {
		attempts++
		return permanent
	}

	err := r.Do(t.Context(), op)
	if !errors.Is(err, permanent) {
		t.Errorf("Do = %v; want the permanent error", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d; want 1 (no retry on permanent)", attempts)
	}
	if len(waits) != 0 {
		t.Errorf("waits = %d; want 0", len(waits))
	}
}

func TestHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	var waits []time.Duration
	r := newFakeRetrier(5, &waits)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // already cancelled

	op := func() error { return NewTransient(errors.New("reset"), time.Second) }

	err := r.Do(ctx, op)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Do = %v; want context.Canceled", err)
	}
}

func TestReadsRetryAfterHint(t *testing.T) {
	t.Parallel()
	var waits []time.Duration
	r := newFakeRetrier(3, &waits)

	attempts := 0
	op := func() error {
		attempts++
		if attempts < 2 {
			return NewTransient(errors.New("reset"), 750*time.Millisecond)
		}
		return nil
	}

	if err := r.Do(t.Context(), op); err != nil {
		t.Fatalf("Do = %v; want nil", err)
	}
	if len(waits) != 1 || waits[0] != 750*time.Millisecond {
		t.Errorf("waits = %v; want [750ms]", waits)
	}
}
```

## Review

The type is the contract: `Do` never inspects a message, it asks the error via
`errors.As` whether it is a `*TransientError`, consults its `Retryable()` verdict,
and reads `RetryAfter` from it, so the failing layer alone decides retryability. `TestPermanentReturnsImmediately`
proves a non-transient error short-circuits the loop — the retry budget is spent
only on errors that might succeed. The injected sleep keeps the tests
deterministic: `TestReadsRetryAfterHint` asserts the exact hint value reached the
waiter without any real delay, and `TestHonorsContextCancellation` proves a
cancelled context aborts promptly. This module intentionally stops at
classification; backoff schedules, jitter, and budgets are Chapter 12's job. Run
`go test -race` to confirm.

## Resources

- [errors: As](https://pkg.go.dev/errors#As) — extracting the typed error to read its retry hint.
- [context package](https://pkg.go.dev/context) — cancellation propagated through the sleep.
- [AWS Architecture Blog: Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — why retry decisions belong to the error class and the caller, not a string match.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-aggregated-validation-errors.md](05-aggregated-validation-errors.md) | Next: [07-ratelimit-quota-error.md](07-ratelimit-quota-error.md)
