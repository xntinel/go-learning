# Exercise 1: Retry/Timeout/Backoff as Composable Operation Decorators

Every resilient call to a downstream dependency is a plain operation wrapped in
policy. Here you build the policy as decorators over a single function type,
`Operation`, so a call site composes exactly the resilience it needs and nothing
it does not.

## What you'll build

```text
retrypipe/                       independent module: example.com/retrypipe
  go.mod                         go 1.25
  internal/retry/
    retry.go                     type Operation; WithRetry, WithTimeout, WithBackoff, ErrPermanent
    retry_test.go                attempt-count, timeout-fires, ctx-cancel, backoff, joined-error tests
  cmd/demo/
    main.go                      composes WithTimeout(WithRetry(op)) against a flaky dependency
```

- Files: `internal/retry/retry.go`, `internal/retry/retry_test.go`, `cmd/demo/main.go`.
- Implement: `Operation func(ctx context.Context) error`; `WithRetry(op, attempts, isRetryable)`, `WithTimeout(op, d)`, and the `WithBackoff(base)` factory.
- Test: exact attempt counts (1, 2, non-retryable stops at 1), timeout fires measured by elapsed time, context cancellation, exponential backoff factory, and the joined final error after all attempts fail.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/retrypipe/internal/retry ~/go-exercises/retrypipe/cmd/demo
cd ~/go-exercises/retrypipe
go mod init example.com/retrypipe
go mod edit -go=1.25
```

### The one type everything composes over

`Operation func(ctx context.Context) error` is the whole abstraction. A retry, a
timeout, and a backoff are each a function that takes an `Operation` and returns
an `Operation` — a `T -> T` decorator. Because the output is another `Operation`,
they compose by nesting: `WithTimeout(WithRetry(op, 3, pred), d)` is a single
`Operation` that retries up to three times, the whole loop bounded by one deadline
`d`. That is a per-operation deadline. Had you written `WithRetry(WithTimeout(op,
d), 3, pred)` instead, each attempt would get its own fresh `d` — a per-attempt
deadline, a different product. State which one you built; this exercise builds the
per-operation form at the demo call site.

`WithRetry` takes the operation, the attempt count, and an `isRetryable`
predicate. The predicate is how retryability stays a domain decision: the policy
never assumes an error is worth retrying, the caller says so. When the predicate
returns false, the loop returns that error immediately without burning the rest of
the budget — auth failures and validation errors must not be retried.

The wait between attempts is a `select` on `ctx.Done()`, never a bare
`time.After`. A cancelled context ends the loop at once instead of blocking for the
full backoff, which is what makes cancellation and shutdown actually prompt.

When every attempt fails, the loop returns one error that `errors.Join`s all the
attempt failures, wrapped with `%w`. Callers' `errors.Is`/`errors.As` and your
metrics can then classify the failure by any of its causes, including the last.

Create `internal/retry/retry.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Operation is a cancellable unit of work. Every policy in this package is a
// decorator with signature Operation -> Operation.
type Operation func(ctx context.Context) error

// ErrPermanent classifies an error that must never be retried. A caller's
// isRetryable predicate typically returns !errors.Is(err, ErrPermanent).
var ErrPermanent = errors.New("permanent error")

// WithRetry runs op up to attempts times, stopping early when op succeeds or
// when isRetryable reports the error is not worth retrying. Between attempts it
// waits backoff(i), abandoning the wait if ctx is cancelled. After all attempts
// fail it returns the joined attempt errors wrapped with %w.
func WithRetry(op Operation, attempts int, isRetryable func(error) bool) Operation {
	if attempts < 1 {
		attempts = 1
	}
	return func(ctx context.Context) error {
		var errs []error
		for i := range attempts {
			err := op(ctx)
			if err == nil {
				return nil
			}
			errs = append(errs, err)
			if isRetryable != nil && !isRetryable(err) {
				return err
			}
			if i == attempts-1 {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(i)):
			}
		}
		return fmt.Errorf("retry: %d attempts failed: %w", attempts, errors.Join(errs...))
	}
}

// WithTimeout runs op under a context that is cancelled after d.
func WithTimeout(op Operation, d time.Duration) Operation {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return op(ctx)
	}
}

// WithBackoff is a factory: it returns an exponential backoff function whose
// delay for attempt i is base * 2^i. It is the pluggable delay policy a caller
// can hand to a retry loop.
func WithBackoff(base time.Duration) func(int) time.Duration {
	return func(attempt int) time.Duration {
		if attempt < 0 {
			attempt = 0
		}
		return base * time.Duration(1<<attempt)
	}
}

// backoff is the default inter-attempt delay used by WithRetry.
func backoff(attempt int) time.Duration {
	return 100 * time.Millisecond * time.Duration(1<<attempt)
}
```

### The runnable demo

The demo composes the two decorators against a dependency that refuses the first
two connection attempts and accepts the third — the everyday shape of a cold
downstream. The whole retry loop runs under a single two-second deadline.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/retrypipe/internal/retry"
)

func main() {
	attempt := 0
	op := func(ctx context.Context) error {
		attempt++
		fmt.Printf("attempt %d: dialing payments-api\n", attempt)
		if attempt < 3 {
			return errors.New("connection refused")
		}
		return nil
	}

	policy := retry.WithTimeout(
		retry.WithRetry(op, 3, func(error) bool { return true }),
		2*time.Second,
	)

	if err := policy(context.Background()); err != nil {
		fmt.Println("failed:", err)
		return
	}
	fmt.Printf("result: ok after %d attempts\n", attempt)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempt 1: dialing payments-api
attempt 2: dialing payments-api
attempt 3: dialing payments-api
result: ok after 3 attempts
```

### Tests

The tests use an atomic call counter so concurrency under `-race` stays honest,
and they assert exact attempt counts rather than "an error came back". The timeout
test proves the deadline actually fired by bounding elapsed time well under the
inner operation's own 100ms sleep. `TestRetryReturnsJoinedErrorAfterAllAttemptsFail`
is the case the original lesson left as a follow-up: every attempt fails with a
distinct error, and `errors.Is` must find the last cause in the joined result.

Create `internal/retry/retry_test.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestWithRetrySucceedsOnFirstAttempt(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	op := WithRetry(func(ctx context.Context) error {
		calls.Add(1)
		return nil
	}, 3, nil)
	if err := op(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestWithRetrySucceedsOnSecondAttempt(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	op := WithRetry(func(ctx context.Context) error {
		if calls.Add(1) < 2 {
			return errors.New("transient")
		}
		return nil
	}, 3, func(error) bool { return true })
	if err := op(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestWithRetryStopsOnNonRetryable(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	op := WithRetry(func(ctx context.Context) error {
		calls.Add(1)
		return ErrPermanent
	}, 3, func(err error) bool { return !errors.Is(err, ErrPermanent) })
	err := op(context.Background())
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 (no retries on permanent)", got)
	}
}

func TestWithTimeoutCancelsLongOperation(t *testing.T) {
	t.Parallel()

	op := WithTimeout(func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
			return nil
		}
	}, 10*time.Millisecond)

	start := time.Now()
	err := op(context.Background())
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("elapsed = %s, want < 50ms (timeout must actually fire)", elapsed)
	}
}

func TestRetryRespectsContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls atomic.Int32
	op := WithRetry(func(ctx context.Context) error {
		calls.Add(1)
		return errors.New("boom")
	}, 5, func(error) bool { return true })
	err := op(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 (cancel between attempts)", got)
	}
}

func TestBackoffGrowsExponentially(t *testing.T) {
	t.Parallel()

	bf := WithBackoff(50 * time.Millisecond)
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 50 * time.Millisecond},
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := bf(tc.attempt); got != tc.want {
			t.Fatalf("backoff(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
}

func TestRetryReturnsJoinedErrorAfterAllAttemptsFail(t *testing.T) {
	t.Parallel()

	errFirst := errors.New("attempt-1: dns")
	errSecond := errors.New("attempt-2: refused")
	errThird := errors.New("attempt-3: reset")
	seq := []error{errFirst, errSecond, errThird}

	var calls atomic.Int32
	op := WithRetry(func(ctx context.Context) error {
		return seq[calls.Add(1)-1]
	}, 3, func(error) bool { return true })

	err := op(context.Background())
	if err == nil {
		t.Fatal("want an error after all attempts fail")
	}
	if !errors.Is(err, errThird) {
		t.Fatalf("errors.Is did not find the last cause in %v", err)
	}
	if !errors.Is(err, errFirst) {
		t.Fatalf("joined error dropped the first cause: %v", err)
	}
}

func Example() {
	bf := WithBackoff(50 * time.Millisecond)
	fmt.Println(bf(0), bf(1), bf(2))
	// Output: 50ms 100ms 200ms
}
```

## Review

The policies are correct when each one does exactly its job and nothing else.
`WithRetry` calls the operation the exact number of times the counter asserts:
once on first success, twice when the second attempt succeeds, once when the
predicate rejects the error. `WithTimeout` is correct only if the elapsed-time
bound holds — an assertion that merely checks for a non-nil error would pass even
if the deadline never fired. The joined-error test proves the final error carries
every cause, so `errors.Is` and metrics can classify it.

The traps are the ones the concepts named: do not hardcode the count or the delay
inside the wrapper; do not retry on any non-nil error; and never sleep between
attempts without a `select` on `ctx.Done()`, or a cancelled request stalls for the
full backoff. Be explicit about composition order — the demo builds a
per-operation deadline; a per-attempt deadline would need the opposite nesting.
Run `go test -race` so the atomic counter proves the operation is called
concurrently-safely.

## Resources

- [context package](https://pkg.go.dev/context) — `WithTimeout`, `Context.Done`, `Context.Err`.
- [errors package](https://pkg.go.dev/errors) — `Join`, `Is`, `As`, and `%w` wrapping semantics.
- [Go spec: Function types](https://go.dev/ref/spec#Function_types) — the `Operation` decorator shape.
- [time package](https://pkg.go.dev/time) — `time.After`, `time.Duration` arithmetic.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-http-middleware-chain.md](02-http-middleware-chain.md)
