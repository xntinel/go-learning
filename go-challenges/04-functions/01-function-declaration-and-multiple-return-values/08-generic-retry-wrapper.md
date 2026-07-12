# Exercise 8: A Generic Retry Wrapper Around A Fallible Function

Retry is where functions-as-values and multiple returns meet: you hand a retryer a
`func() (T, error)` and it threads that tuple through bounded backoff, returning
the first success or the last error, and giving up promptly when the context is
cancelled. This exercise builds `Retry[T any]` — generic so the wrapped value's
concrete type survives — with a permanent-error short-circuit.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
retry/                     independent module: example.com/retry
  go.mod                   go 1.25
  retry.go                 Retry[T any](ctx, attempts, fn) (T, error); ErrPermanent short-circuit
  cmd/
    demo/
      main.go              retries a flaky op that succeeds on the 3rd try
  retry_test.go            success within budget; N attempts then last error; ctx cancel; permanent; -race
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `Retry[T any](ctx context.Context, attempts int, fn func() (T, error)) (T, error)` that returns the first success; on exhaustion returns the last wrapped error; aborts on `ctx.Done()` returning `ctx.Err()`; and short-circuits when the error wraps `ErrPermanent`.
- Test: a fn that succeeds on attempt 3 returns the value and nil; a fn that always fails returns the zero `T` and the last error after exactly N attempts; a cancelled context aborts early with `ctx.Err()`; a permanent error stops after one attempt.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Threading a tuple through backoff

`fn func() (T, error)` is a first-class value: a "fallible computation" the caller
hands to `Retry`. Because `Retry` is generic over `T`, retrying a
`func() (User, error)` returns a `User`, not an `any` the caller must re-assert —
the multiple-return tuple stays typed all the way through. That is the whole reason
this is `Retry[T]` and not `Retry` returning `(any, error)`.

The control flow encodes the retry-vs-give-up contract:

- Call `fn`. If `err == nil`, return `(value, nil)` immediately — first success
  wins.
- If the error wraps `ErrPermanent`, return it now. Retrying a permanent failure
  (bad credentials, malformed request) just burns time; `errors.Is(err,
  ErrPermanent)` classifies it as non-retryable.
- Otherwise, if attempts remain, wait a backoff interval — but the wait must
  `select` on `ctx.Done()` so a cancelled request aborts *during* the sleep and
  returns `ctx.Err()`, not after exhausting the budget.
- After the last attempt, return the zero `T` and the last error, wrapped with how
  many attempts were spent.

The subtle correctness point is cancellation. A retry loop that sleeps with
`time.Sleep` between attempts ignores the context: a client that has already hung
up keeps the goroutine alive, sleeping and retrying against a dead request. Using
`time.NewTimer` inside a `select` with `ctx.Done()` is what makes the abort prompt.

Create `retry.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrPermanent marks an error as non-retryable. Wrap it (fmt.Errorf("...: %w",
// ErrPermanent)) to tell Retry to give up immediately instead of retrying.
var ErrPermanent = errors.New("permanent")

// baseDelay is the first backoff interval; it doubles each attempt.
const baseDelay = time.Millisecond

// Retry calls fn up to attempts times, returning the first success. It gives up
// early if fn returns an error wrapping ErrPermanent, and aborts with ctx.Err()
// if ctx is cancelled during a backoff wait. On exhaustion it returns the zero T
// and the last error.
func Retry[T any](ctx context.Context, attempts int, fn func() (T, error)) (T, error) {
	var zero T
	if attempts < 1 {
		return zero, fmt.Errorf("retry: attempts must be >= 1, got %d", attempts)
	}

	var lastErr error
	delay := baseDelay
	for i := range attempts {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		lastErr = err
		if errors.Is(err, ErrPermanent) {
			return zero, fmt.Errorf("retry: permanent failure on attempt %d: %w", i+1, err)
		}
		if i == attempts-1 {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
		delay *= 2
	}
	return zero, fmt.Errorf("retry: giving up after %d attempts: %w", attempts, lastErr)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/retry"
)

func main() {
	calls := 0
	fetch := func() (string, error) {
		calls++
		if calls < 3 {
			return "", errors.New("temporary network blip")
		}
		return "payload", nil
	}

	v, err := retry.Retry(context.Background(), 5, fetch)
	fmt.Printf("value=%q err=%v attempts=%d\n", v, err, calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
value="payload" err=<nil> attempts=3
```

### Tests

Tests use tiny virtual-free durations (the `baseDelay` is a millisecond) and
`t.Context()` for the happy paths. The cancellation test cancels its own context
and asserts the abort returns `context.Canceled`.

Create `retry_test.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestRetrySucceedsWithinBudget(t *testing.T) {
	t.Parallel()
	calls := 0
	fn := func() (int, error) {
		calls++
		if calls < 3 {
			return 0, errors.New("transient")
		}
		return 42, nil
	}

	v, err := Retry(t.Context(), 5, fn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 42 {
		t.Fatalf("value = %d, want 42", v)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (succeeded on the third)", calls)
	}
}

func TestRetryExhaustsAttempts(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("always fails")
	calls := 0
	fn := func() (int, error) {
		calls++
		return 0, sentinel
	}

	v, err := Retry(t.Context(), 4, fn)
	if v != 0 {
		t.Fatalf("value = %d, want zero on failure", v)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the last error", err)
	}
	if calls != 4 {
		t.Fatalf("calls = %d, want exactly 4 attempts", calls)
	}
}

func TestRetryAbortsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	calls := 0
	fn := func() (int, error) {
		calls++
		return 0, errors.New("transient")
	}

	_, err := Retry(ctx, 100, fn)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls >= 100 {
		t.Fatalf("calls = %d, want an early abort well under the budget", calls)
	}
}

func TestRetryStopsOnPermanent(t *testing.T) {
	t.Parallel()
	calls := 0
	fn := func() (int, error) {
		calls++
		return 0, fmt.Errorf("bad credentials: %w", ErrPermanent)
	}

	_, err := Retry(t.Context(), 10, fn)
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want it to wrap ErrPermanent", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (permanent errors are not retried)", calls)
	}
}

func ExampleRetry() {
	calls := 0
	fn := func() (string, error) {
		calls++
		if calls < 2 {
			return "", errors.New("blip")
		}
		return "ok", nil
	}
	v, err := Retry(context.Background(), 3, fn)
	fmt.Println(v, err)
	// Output: ok <nil>
}
```

## Review

`Retry` is correct when the first success short-circuits, an always-failing `fn`
runs exactly N times before returning the last wrapped error, a cancelled context
aborts before the budget is spent, and a permanent error stops after one call.
`TestRetryExhaustsAttempts` pins the attempt count and the `%w` wrap of the last
error; `TestRetryAbortsOnCancel` pins the prompt abort. The generic `T` is what
lets the wrapped value come back typed — an `int` here, a `User` in real code —
instead of an `any`.

The mistakes are cancellation and classification. A backoff that uses `time.Sleep`
instead of a `select` on `ctx.Done()` keeps retrying a request the caller already
abandoned — always thread the context through the wait. And retrying a permanent
failure wastes the whole budget on an error that will never succeed;
`errors.Is(err, ErrPermanent)` is how the wrapper tells retryable from
non-retryable. Run `go test -race`: each subtest owns its own `calls` counter and
`fn`, so there is no shared state to race.

## Resources

- [context.Context](https://pkg.go.dev/context#Context) — `Done` and `Err` for prompt cancellation.
- [time.NewTimer](https://pkg.go.dev/time#NewTimer) — a stoppable timer to `select` against `ctx.Done()`.
- [Go Spec: Type parameters](https://go.dev/ref/spec#Type_parameter_declarations) — the generics that keep `fn`'s `(T, error)` tuple typed.
- [errors.Is](https://pkg.go.dev/errors#Is) — classifying an error as permanent via a wrapped sentinel.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-variadic-sql-in-clause-builder.md](07-variadic-sql-in-clause-builder.md) | Next: [09-parse-hostport-chained-returns.md](09-parse-hostport-chained-returns.md)
