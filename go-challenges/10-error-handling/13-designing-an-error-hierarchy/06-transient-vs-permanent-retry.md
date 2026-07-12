# Exercise 6: Classifying Transient vs Permanent Errors for a Backoff Worker

Retryability is not a domain category — "user not found" is permanent, "user store
timed out" is transient, and both are user errors. This exercise models
retryability as its own axis (an `ErrTransient` base and a `Retryable` interface)
and builds a `DoWithRetry` that retries only transient failures with backoff,
returning immediately on permanent ones.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
transient-retry/                   module example.com/transient-retry
  go.mod
  retry.go                         ErrTransient axis; Retryable iface; Transient(); IsRetryable(); DoWithRetry()
  cmd/demo/main.go                 transient-then-success, permanent-no-retry, both-categories
  retry_test.go                    counts calls; no retry on permanent; retried yet still Is domain
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: an `ErrTransient` base, a `Retryable` interface, a `Transient(cause)` that joins the axis with multiple `%w`, an `IsRetryable(err)` that checks the axis (never a domain category), and a `DoWithRetry(ctx, attempts, base, fn)` with exponential backoff.
- Test: a function that fails transiently twice then succeeds runs exactly 3 times and returns nil; a permanent `ErrUserInvalid` runs exactly once; an error joined as both `ErrUser` and `ErrTransient` is retried yet still `errors.Is` `ErrUser`.
- Verify: `go test -count=1 -race ./...`

### Retryability is orthogonal to the domain category

The mistake this exercise inoculates against is inferring "should I retry?" from the
domain category — "retry all user errors" or "retry everything that isn't a
validation error". That reasoning retries permanent failures forever and, if the
operation is not idempotent, corrupts data. Retryability is a *separate axis*, and
there are two idiomatic ways to express it.

The first is a `Retryable() bool` method on a typed error — here `timeoutError`, a
stand-in for a network timeout, returns `Retryable() true`. A caller extracts the
interface with `errors.As` and asks. The second is an `ErrTransient` sentinel joined
onto any error with a *second* `%w`: `fmt.Errorf("%w: %w", cause, ErrTransient)`
produces an error whose `Unwrap` returns `[]error{cause, ErrTransient}`, so the
value is simultaneously `errors.Is(cause)` and `errors.Is(ErrTransient)`. This is
the multiple-`%w` composition: an error can be "a user error" and "a transient
error" at once, and the worker and the domain each ask their own orthogonal
question. `IsRetryable` checks the axis both ways — the `Retryable` interface first,
then `ErrTransient` — and never looks at a domain category.

`DoWithRetry` is the worker. It calls `fn` up to `attempts` times. On success it
returns nil; on a non-retryable error it returns *immediately* (the whole point);
on a retryable error it waits `base << i` (exponential backoff) and tries again,
aborting early if the context is cancelled. The backoff `select` watches
`ctx.Done()` so a shutdown or deadline cancels the wait instead of blocking through
it. On exhaustion it returns the last error, still carrying its identity so the
caller can classify it.

Create `retry.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Domain categories. ErrUser is a permanent domain failure; ErrTransient is the
// orthogonal "worth retrying" axis. An error can carry both via multiple %w.
var (
	ErrUser        = errors.New("user error")
	ErrUserInvalid = fmt.Errorf("user: invalid: %w", ErrUser)
	ErrTransient   = errors.New("transient error")
)

// Retryable is the interface a typed error implements to declare retryability
// without joining ErrTransient. A caller checks it with errors.As.
type Retryable interface {
	Retryable() bool
}

// timeoutError is a typed transient error (a stand-in for a network timeout).
type timeoutError struct{ op string }

func (e *timeoutError) Error() string   { return e.op + ": i/o timeout" }
func (e *timeoutError) Retryable() bool { return true }

// NewTimeout builds a retryable typed error.
func NewTimeout(op string) error { return &timeoutError{op: op} }

// Transient wraps cause as retryable by joining ErrTransient alongside it, so the
// result is both errors.Is(cause) and errors.Is(ErrTransient).
func Transient(cause error) error {
	return fmt.Errorf("%w: %w", cause, ErrTransient)
}

// IsRetryable reports whether err should be retried. It asks the explicit
// transient axis (a Retryable method or the ErrTransient base), never a domain
// category.
func IsRetryable(err error) bool {
	var r Retryable
	if errors.As(err, &r) {
		return r.Retryable()
	}
	return errors.Is(err, ErrTransient)
}

// DoWithRetry calls fn up to attempts times, retrying only retryable errors with
// an exponential backoff. A permanent error returns immediately; a cancelled
// context aborts. It returns the last error on exhaustion.
func DoWithRetry(ctx context.Context, attempts int, base time.Duration, fn func() error) error {
	var err error
	for i := range attempts {
		err = fn()
		if err == nil {
			return nil
		}
		if !IsRetryable(err) {
			return err
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(base << i):
		}
	}
	return err
}
```

### The runnable demo

The demo runs the three defining scenarios: a call that is transient twice then
succeeds (three calls, nil error), a permanent error (one call, no retry), and an
error that is both a domain category and transient (retryable, yet still matches the
domain base).

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/transient-retry"
)

func main() {
	ctx := context.Background()

	// Transient twice, then success.
	calls := 0
	err := retry.DoWithRetry(ctx, 5, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return retry.NewTimeout("query")
		}
		return nil
	})
	fmt.Printf("transient-then-ok: calls=%d err=%v\n", calls, err)

	// Permanent: no retry.
	calls = 0
	err = retry.DoWithRetry(ctx, 5, time.Millisecond, func() error {
		calls++
		return retry.ErrUserInvalid
	})
	fmt.Printf("permanent: calls=%d Is ErrUserInvalid=%v\n", calls, errors.Is(err, retry.ErrUserInvalid))

	// Both a domain category and transient.
	both := retry.Transient(retry.ErrUser)
	fmt.Printf("both: retryable=%v Is ErrUser=%v\n", retry.IsRetryable(both), errors.Is(both, retry.ErrUser))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
transient-then-ok: calls=3 err=<nil>
permanent: calls=1 Is ErrUserInvalid=true
both: retryable=true Is ErrUser=true
```

### Tests

The tests count calls, which is the observable that proves the classification: a
transient-twice-then-success `fn` must be called exactly 3 times, a permanent error
exactly once (no retry), and a both-categories error must be retried to exhaustion
*and* still `errors.Is` its domain base. A tiny backoff base keeps the tests fast.

Create `retry_test.go`:

```go
package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetriesTransientThenSucceeds(t *testing.T) {
	t.Parallel()
	calls := 0
	err := DoWithRetry(context.Background(), 5, time.Microsecond, func() error {
		calls++
		if calls < 3 {
			return NewTimeout("query")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v; want nil", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d; want 3", calls)
	}
}

func TestPermanentErrorIsNotRetried(t *testing.T) {
	t.Parallel()
	calls := 0
	err := DoWithRetry(context.Background(), 5, time.Microsecond, func() error {
		calls++
		return ErrUserInvalid
	})
	if calls != 1 {
		t.Fatalf("calls = %d; want 1 (permanent must not retry)", calls)
	}
	if !errors.Is(err, ErrUserInvalid) {
		t.Fatalf("err = %v; want errors.Is ErrUserInvalid", err)
	}
}

func TestBothCategoriesIsRetriedAndStillMatchesDomain(t *testing.T) {
	t.Parallel()
	calls := 0
	joined := Transient(ErrUser)
	err := DoWithRetry(context.Background(), 3, time.Microsecond, func() error {
		calls++
		return joined
	})
	if calls != 3 {
		t.Fatalf("calls = %d; want 3 (transient axis must retry)", calls)
	}
	if !errors.Is(err, ErrUser) {
		t.Fatal("exhausted error no longer Is ErrUser")
	}
	if !errors.Is(err, ErrTransient) {
		t.Fatal("exhausted error no longer Is ErrTransient")
	}
}

func TestCancelledContextAborts(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := DoWithRetry(ctx, 5, time.Second, func() error {
		calls++
		return NewTimeout("query")
	})
	if calls != 1 {
		t.Fatalf("calls = %d; want 1 (cancel aborts before second try)", calls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
}
```

## Review

Retry logic is correct when the call count matches the classification, not the
category: transient errors retry up to the limit, permanent errors return after
exactly one call, and a cancelled context aborts the backoff instead of sleeping
through it. The design rule underneath is that `IsRetryable` consults only the
transient axis — the `Retryable` method or `ErrTransient` — and never a domain
category, which is what keeps a permanent "invalid" from being retried and a
transient "timeout" from being given up on. Multiple `%w` is the composition that
makes this ergonomic: one error is both a domain failure and a transient one, and
each consumer asks its own question. Keep the backoff `select` watching
`ctx.Done()`; a retry loop that ignores cancellation is how a shutdown turns into a
hang.

## Resources

- [`errors.As`](https://pkg.go.dev/errors#As) — extracting the `Retryable` interface from a chain.
- [`fmt.Errorf` and multiple `%w`](https://pkg.go.dev/fmt#Errorf) — joining a domain error with the transient axis.
- [Go 1.20 release notes: errors](https://go.dev/doc/go1.20#errors) — multiple-`%w` wrapping and `Unwrap() []error`.

---

Back to [05-aggregate-validation-errors.md](05-aggregate-validation-errors.md) | Next: [07-context-error-distinction.md](07-context-error-distinction.md)
