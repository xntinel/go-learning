# Exercise 6: Retry Loop: Classify Retryable vs Terminal Errors via errors.As

A retry loop that retries *everything* is a denial-of-service against your own
dependencies: it hammers a service that returned a permanent 400, and it keeps
going after the caller has already cancelled. A correct loop classifies each
failure — retry the transient ones, stop on terminal ones, and abort instantly when
the context is done. This module builds that loop, using `errors.As` to detect a
`Retryable` capability and `errors.Is` to detect cancellation.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
retry/                      independent module: example.com/retry
  go.mod                    go 1.24
  retry.go                  Retryable interface, *TransientError, ErrValidation, Do
  cmd/
    demo/
      main.go               runnable demo: a flaky op that succeeds on the 3rd try
  retry_test.go             transient-then-success, terminal, cancelled context
```

Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
Implement: `Do(ctx, maxAttempts, backoff, op)` that retries while the error is `Retryable` (extracted via `errors.As`), stops on terminal errors, and aborts immediately on `context.Canceled`/`DeadlineExceeded` (via `errors.Is`).
Test: an op that fails K times then succeeds retries and succeeds; a terminal error stops after one attempt; a cancelled context aborts immediately; count attempts via a closure and keep backoff injectable.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Classify, don't blindly retry

The loop makes a decision after every failure, and the decision is driven by the
error's shape, not by a fragile string match. Two questions, two tools:

Is this failure *retryable*? That is a *capability* question — does the error carry
a `Retryable() bool` that says "try again"? `errors.As(err, &r)` extracts the first
error in the chain that implements the `Retryable` interface; if none does, the
error is terminal and the loop stops immediately. Modeling retryability as an
interface (rather than a sentinel) lets many concrete error types opt in, and lets
the *value* decide — a `TransientError` says yes, a validation error simply does not
implement the interface and so is terminal.

Has the caller *given up*? That is an *identity* question against the context
sentinels. `errors.Is(err, context.Canceled)` and
`errors.Is(err, context.DeadlineExceeded)` — checked before the retryable check —
abort the loop at once: there is no point retrying an operation the caller cancelled
or whose deadline passed. The loop also checks `ctx.Err()` at the top of each
iteration so a context cancelled *between* attempts stops the very next pass without
calling `op` again.

Backoff is injected as a `func(attempt int)` so tests run instantly (a no-op) while
production passes a real sleep. This is the one place a `time.Sleep` would otherwise
make the test slow and flaky; keeping it a parameter is the standard way to keep
retry logic unit-testable.

Create `retry.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
)

// Retryable is the capability a transient error advertises. errors.As extracts it.
type Retryable interface {
	Retryable() bool
}

// TransientError wraps a cause that is worth retrying (a timeout, a 503, ...).
type TransientError struct {
	Err error
}

func (e *TransientError) Error() string   { return "transient: " + e.Err.Error() }
func (e *TransientError) Unwrap() error   { return e.Err }
func (e *TransientError) Retryable() bool { return true }

// ErrValidation is a terminal error: retrying a malformed request never helps.
var ErrValidation = errors.New("validation failed")

// Do calls op until it succeeds, the error is terminal, the attempts run out, or
// the context is done. backoff is invoked between attempts (inject a no-op in tests).
func Do(ctx context.Context, maxAttempts int, backoff func(attempt int), op func() error) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("retry aborted before attempt %d: %w", attempt, err)
		}

		err := op()
		if err == nil {
			return nil
		}
		lastErr = err

		// Caller gave up: abort immediately, do not retry.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		// Terminal unless the error advertises retryability.
		var r Retryable
		if !errors.As(err, &r) || !r.Retryable() {
			return err
		}

		if attempt < maxAttempts {
			backoff(attempt)
		}
	}
	return fmt.Errorf("giving up after %d attempts: %w", maxAttempts, lastErr)
}
```

### The runnable demo

The demo runs a flaky operation that fails twice with a `TransientError` and
succeeds on the third attempt, printing the attempt count and the outcome. Backoff
is a no-op so the demo is instant.

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
	attempts := 0
	op := func() error {
		attempts++
		if attempts < 3 {
			return &retry.TransientError{Err: errors.New("503 service unavailable")}
		}
		return nil
	}

	err := retry.Do(context.Background(), 5, func(int) {}, op)
	fmt.Printf("attempts: %d\n", attempts)
	fmt.Printf("result:   %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempts: 3
result:   <nil>
```

### Tests

Each case counts attempts with a closure counter and injects a no-op backoff. The
transient-then-success case proves the loop retries and eventually succeeds; the
terminal case proves a non-retryable error stops after exactly one attempt; the
cancelled-context case builds a cancellable context off `t.Context()`, cancels it,
and asserts the loop aborts before calling `op` at all, with
`errors.Is(err, context.Canceled)` true. The exhaustion case proves a persistently
transient error gives up after `maxAttempts` and wraps the last error.

Create `retry_test.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestDo(t *testing.T) {
	t.Parallel()

	t.Run("transient then success", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		op := func() error {
			attempts++
			if attempts < 3 {
				return &TransientError{Err: errors.New("timeout")}
			}
			return nil
		}
		err := Do(t.Context(), 5, func(int) {}, op)
		if err != nil {
			t.Fatalf("Do = %v, want nil", err)
		}
		if attempts != 3 {
			t.Errorf("attempts = %d, want 3", attempts)
		}
	})

	t.Run("terminal error stops after one attempt", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		op := func() error {
			attempts++
			return ErrValidation
		}
		err := Do(t.Context(), 5, func(int) {}, op)
		if !errors.Is(err, ErrValidation) {
			t.Errorf("err = %v, want ErrValidation", err)
		}
		if attempts != 1 {
			t.Errorf("attempts = %d, want 1", attempts)
		}
	})

	t.Run("cancelled context aborts immediately", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		attempts := 0
		op := func() error {
			attempts++
			return nil
		}
		err := Do(ctx, 5, func(int) {}, op)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if attempts != 0 {
			t.Errorf("attempts = %d, want 0 (op must not run)", attempts)
		}
	})

	t.Run("exhausts attempts then gives up", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		op := func() error {
			attempts++
			return &TransientError{Err: errors.New("still down")}
		}
		err := Do(t.Context(), 3, func(int) {}, op)
		if err == nil {
			t.Fatal("Do = nil, want give-up error")
		}
		if attempts != 3 {
			t.Errorf("attempts = %d, want 3", attempts)
		}
		var r Retryable
		if !errors.As(err, &r) {
			t.Errorf("give-up error should still wrap the retryable cause")
		}
	})
}

func ExampleDo() {
	attempts := 0
	err := Do(context.Background(), 3, func(int) {}, func() error {
		attempts++
		if attempts < 2 {
			return &TransientError{Err: errors.New("flaky")}
		}
		return nil
	})
	fmt.Println(attempts, err)
	// Output: 2 <nil>
}
```

## Review

`Do` is correct when its three exits are each provable: a transient error retries and
eventually succeeds (attempts climb, error is nil), a terminal error stops after one
attempt (`errors.As` finds no `Retryable`, so the loop returns immediately), and a
done context aborts before `op` runs (the top-of-loop `ctx.Err()` check fires,
attempts stay 0). The classification must use `errors.As` for the capability and
`errors.Is` for the context sentinels — a string match on the error message would
break the moment a wrapper changed the text. Keeping backoff injectable is what makes
these assertions instant and deterministic; a real `time.Sleep` inside the loop would
make the test slow and its timing flaky.

## Resources

- [errors.As](https://pkg.go.dev/errors#As) — extracting a typed capability from the chain.
- [context package](https://pkg.go.dev/context) — `Canceled`, `DeadlineExceeded`, and `Context.Err`.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `As` for behavior/capability matching.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-custom-is-semantic-equality.md](07-custom-is-semantic-equality.md)
