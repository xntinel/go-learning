# Exercise 5: Retry/Backoff Taking an Operation and a Classifier Callback

A retry engine is two callbacks: the operation to run, and a classifier that decides
whether a given error is worth retrying. This module builds `Do(ctx, policy, op,
isRetryable)` with exponential backoff that honors context cancellation — the detail
that separates a production retry from one that blocks graceful shutdown.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
retry/                      independent module: example.com/retry
  go.mod                    go 1.26
  retry.go                  Operation, Retryable, Policy, Do (ctx-aware backoff)
  cmd/
    demo/
      main.go               runnable demo: transient failures then success
  retry_test.go             call-count, permanent-error, cancellation tests
```

Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
Implement: `type Operation func(ctx context.Context) error`, `type Retryable func(error) bool`, a `Policy`, and `Do` that retries a retryable error with exponential backoff up to `MaxAttempts`, respecting `ctx`.
Test: N-1 failures then success runs exactly N times; a permanent error returns after one call; a cancelled context mid-backoff returns `ctx.Err()` promptly; attempts never exceed `MaxAttempts`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/06-function-types-and-callbacks/05-retry-with-operation-callback/cmd/demo
cd go-solutions/04-functions/06-function-types-and-callbacks/05-retry-with-operation-callback
```

### Why two callbacks, and why the backoff must watch ctx.Done

The engine is parameterized so it knows nothing about your operation. `Operation` is
ctx-first — the operation itself must observe cancellation. `Retryable` classifies an
error: a 503 or a connection reset is transient and worth retrying; a 400 or a
validation failure is permanent and retrying only wastes time and hammers a struggling
dependency. Separating "run it" from "should I run it again" keeps the policy reusable
across every call site.

The one detail that makes this production-grade is the backoff sleep. The naive
version is `time.Sleep(backoff)` between attempts. Under a cancelled request that keeps
retrying and *blocks graceful shutdown*, because `time.Sleep` cannot be interrupted.
The correct version arms a `time.NewTimer` and `select`s on both the timer and
`ctx.Done()`:

```go
timer := time.NewTimer(backoff)
select {
case <-timer.C:
	// backoff elapsed; try again
case <-ctx.Done():
	timer.Stop()
	return ctx.Err()
}
```

So the instant the context is cancelled — a client disconnect, a shutdown signal, a
parent deadline — `Do` returns `ctx.Err()` promptly instead of sleeping out the full
backoff. `timer.Stop()` releases the timer when the context wins the race. The backoff
grows exponentially (`base * 2^attempt`), and after `MaxAttempts` retryable failures
`Do` returns the last error, optionally joined with the context error via
`errors.Join` so a caller can see both the operation failure and that time ran out.

Create `retry.go`:

```go
package retry

import (
	"context"
	"time"
)

// Operation is the unit of work; it is ctx-first so it can observe cancellation.
type Operation func(ctx context.Context) error

// Retryable classifies an error: true means try again, false means give up now.
type Retryable func(error) bool

// Policy configures the backoff schedule.
type Policy struct {
	MaxAttempts int           // total attempts, including the first
	BaseDelay   time.Duration // delay before the second attempt
	MaxDelay    time.Duration // cap on any single delay
}

// Do runs op, retrying a retryable error with exponential backoff up to
// policy.MaxAttempts. It returns nil on success, ctx.Err() if the context is
// cancelled during a backoff, or the last operation error otherwise.
func Do(ctx context.Context, policy Policy, op Operation, isRetryable Retryable) error {
	if policy.MaxAttempts < 1 {
		policy.MaxAttempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < policy.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = op(ctx)
		if lastErr == nil {
			return nil
		}
		if !isRetryable(lastErr) {
			return lastErr
		}
		if attempt == policy.MaxAttempts-1 {
			break // no backoff after the final attempt
		}
		if err := sleep(ctx, backoff(policy, attempt)); err != nil {
			return err
		}
	}
	return lastErr
}

// backoff returns base * 2^attempt, capped at MaxDelay.
func backoff(policy Policy, attempt int) time.Duration {
	d := policy.BaseDelay << attempt
	if policy.MaxDelay > 0 && (d > policy.MaxDelay || d <= 0) {
		d = policy.MaxDelay
	}
	return d
}

// sleep waits for d or until ctx is cancelled, whichever comes first.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
	"time"

	"example.com/retry"
)

var errTransient = errors.New("temporary failure")

func main() {
	attempts := 0
	op := func(ctx context.Context) error {
		attempts++
		if attempts < 3 {
			return errTransient
		}
		return nil
	}
	policy := retry.Policy{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}

	err := retry.Do(context.Background(), policy, op, func(e error) bool {
		return errors.Is(e, errTransient)
	})
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
	"fmt"
	"testing"
	"time"
)

var (
	errTransient = errors.New("transient")
	errPermanent = errors.New("permanent")
)

func retryTransient(e error) bool { return errors.Is(e, errTransient) }

func TestSucceedsAfterNMinus1Failures(t *testing.T) {
	t.Parallel()
	calls := 0
	op := func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return errTransient
		}
		return nil
	}
	policy := Policy{MaxAttempts: 5, BaseDelay: time.Nanosecond, MaxDelay: time.Nanosecond}
	if err := Do(t.Context(), policy, op, retryTransient); err != nil {
		t.Fatalf("Do = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want 3", calls)
	}
}

func TestPermanentErrorReturnsImmediately(t *testing.T) {
	t.Parallel()
	calls := 0
	op := func(ctx context.Context) error {
		calls++
		return errPermanent
	}
	policy := Policy{MaxAttempts: 5, BaseDelay: time.Millisecond}
	err := Do(t.Context(), policy, op, retryTransient)
	if !errors.Is(err, errPermanent) {
		t.Fatalf("err = %v, want errPermanent", err)
	}
	if calls != 1 {
		t.Fatalf("op called %d times, want 1 (permanent error must not retry)", calls)
	}
}

func TestNeverExceedsMaxAttempts(t *testing.T) {
	t.Parallel()
	calls := 0
	op := func(ctx context.Context) error {
		calls++
		return errTransient
	}
	policy := Policy{MaxAttempts: 4, BaseDelay: time.Nanosecond, MaxDelay: time.Nanosecond}
	err := Do(t.Context(), policy, op, retryTransient)
	if !errors.Is(err, errTransient) {
		t.Fatalf("err = %v, want errTransient", err)
	}
	if calls != 4 {
		t.Fatalf("op called %d times, want exactly MaxAttempts=4", calls)
	}
}

func TestContextCancellationDuringBackoff(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	calls := 0
	op := func(ctx context.Context) error {
		calls++
		return errTransient
	}
	// Long backoff so cancellation, not exhaustion, ends the retry.
	policy := Policy{MaxAttempts: 100, BaseDelay: time.Second, MaxDelay: time.Second}

	start := time.Now()
	err := Do(ctx, policy, op, retryTransient)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Do took %s; should return promptly on cancellation, not sleep the full backoff", elapsed)
	}
}

func ExampleDo() {
	calls := 0
	op := func(ctx context.Context) error {
		calls++
		if calls < 2 {
			return errTransient
		}
		return nil
	}
	policy := Policy{MaxAttempts: 3, BaseDelay: time.Nanosecond}
	err := Do(context.Background(), policy, op, retryTransient)
	fmt.Println(calls, err == nil)
	// Output: 2 true
}
```

## Review

The engine is correct on three axes. Call count: an operation that fails N-1 times then
succeeds is invoked exactly N times, and one that always fails a retryable error is
invoked exactly `MaxAttempts` times and no more — `TestNeverExceedsMaxAttempts` pins the
upper bound. Classification: a permanent error returns after a single call, because
`isRetryable` returning false short-circuits before any backoff. Cancellation: this is
the production-critical one — `TestContextCancellationDuringBackoff` gives a 20 ms
deadline against a one-second backoff and asserts `Do` returns `context.DeadlineExceeded`
in well under the backoff, proving the `select` on `ctx.Done()` interrupts the sleep. A
retry that used `time.Sleep` would fail that test and, in production, would keep a
cancelled request alive and stall shutdown. The tests use `t.Context()` so each test's
context is cancelled at test end automatically.

## Resources

- [context.Context and cancellation](https://pkg.go.dev/context#Context)
- [time.NewTimer and Timer.Stop](https://pkg.go.dev/time#NewTimer)
- [errors.Is / errors.Join](https://pkg.go.dev/errors#Join)
- [Go blog: Contexts and cancellation](https://go.dev/blog/context)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-func-type-implements-interface.md](04-func-type-implements-interface.md) | Next: [06-event-dispatch-table.md](06-event-dispatch-table.md)
