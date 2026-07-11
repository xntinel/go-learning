# Exercise 4: Classify Retryable vs Terminal Errors In A Backoff Loop

A retry loop is only as good as its classifier. Retry a transient blip and you
survive it; retry a permission denial or a cancelled context and you hammer a
dependency that will never recover. This exercise builds a backoff wrapper whose
`isRetryable(err)` uses `errors.Is` to retry a transient set and stop *fast* on
terminal sentinels and on context cancellation.

This module is fully self-contained: its own `go mod init`, demo, and tests. The
backoff sleep is injectable so tests run instantly with no real waiting.

## What you'll build

```text
retry/                        independent module: example.com/retry
  go.mod                      go 1.26
  retry.go                    ErrRetryable/ErrPermission/ErrInvalidID; isRetryable; Do with backoff
  cmd/
    demo/
      main.go                 retries a flaky op to success, then stops on a terminal error
  retry_test.go               eventual-success, terminal-stops-once, cancel-stops-fast
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `isRetryable(err)` returning `false` for `nil`, `context.Canceled`, `context.DeadlineExceeded`, `ErrPermission`, `ErrInvalidID` and `true` only for `ErrRetryable`; a `Do` loop that stops immediately on non-retryable errors.
- Test: N transient failures then success (assert attempt count), a terminal error (assert exactly one attempt), a mid-loop cancellation (assert prompt stop wrapping `context.Canceled`).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/retry/cmd/demo
cd ~/go-exercises/retry
go mod init example.com/retry
```

### Fail fast on terminal errors

The whole value of the classifier is negative: it decides what *not* to retry. A
transient error (`ErrRetryable` — a dropped connection, a 503) is worth another
attempt after a backoff. Everything else is terminal and retrying it is actively
harmful:

- `ErrPermission` / `ErrInvalidID` will fail identically on every attempt; the
  loop just wastes time and log volume on a request that can never succeed.
- `context.Canceled` means the caller already gave up — continuing to retry
  hammers a dependency on behalf of a request nobody is waiting for.
- `context.DeadlineExceeded` means the time budget is spent; another attempt
  cannot fit.

So `isRetryable` returns `true` for exactly one thing and `false` for everything
else, including `nil`. Ordering matters: it checks the terminal and context
sentinels *before* the retryable one, and it uses `errors.Is` so classification
survives wrapping — an `ErrPermission` buried under `fmt.Errorf("acl: %w", ...)`
is still recognized as terminal.

The loop itself watches the context at the top of each iteration and during
backoff, so a cancellation that arrives mid-sleep returns promptly rather than
sleeping out the full delay. The backoff `sleep` is an injected function: in
production it is a real timer, but a test passes a no-op (instant) or a
cancelling one, which is how the tests run in microseconds with no flakiness.

Create `retry.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrRetryable  = errors.New("temporary failure")
	ErrPermission = errors.New("permission denied")
	ErrInvalidID  = errors.New("invalid id")
)

// isRetryable reports whether err is worth another attempt. It returns fast
// (false) on terminal sentinels and on context cancellation/deadline so the loop
// never hammers a dead or forbidden dependency. Only ErrRetryable is retryable.
func isRetryable(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.Is(err, ErrPermission), errors.Is(err, ErrInvalidID):
		return false
	case errors.Is(err, ErrRetryable):
		return true
	default:
		return false
	}
}

// Config controls the retry loop. Sleep is injectable; a nil Sleep uses a real
// context-aware timer.
type Config struct {
	MaxAttempts int
	BaseDelay   time.Duration
	Sleep       func(ctx context.Context, d time.Duration) error
}

// sleepCtx waits d or returns early with ctx.Err() if the context is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Do runs op with bounded retries and exponential backoff. It returns the number
// of attempts made and the final error (nil on success). It stops immediately on
// any non-retryable error and on context cancellation.
func Do(ctx context.Context, cfg Config, op func(ctx context.Context) error) (int, error) {
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}

	var err error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if cerr := ctx.Err(); cerr != nil {
			return attempt - 1, fmt.Errorf("retry aborted before attempt %d: %w", attempt, cerr)
		}

		err = op(ctx)
		if err == nil {
			return attempt, nil
		}
		if !isRetryable(err) {
			return attempt, fmt.Errorf("retry stopped after attempt %d: %w", attempt, err)
		}
		if attempt < cfg.MaxAttempts {
			delay := cfg.BaseDelay * time.Duration(1<<(attempt-1))
			if serr := sleep(ctx, delay); serr != nil {
				return attempt, fmt.Errorf("retry aborted during backoff: %w", serr)
			}
		}
	}
	return cfg.MaxAttempts, fmt.Errorf("retry exhausted after %d attempts: %w", cfg.MaxAttempts, err)
}
```

### The runnable demo

The demo uses a real (tiny) backoff. A flaky operation fails twice with
`ErrRetryable` then succeeds; a second operation fails with `ErrPermission` and
stops on the first attempt.

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
	calls := 0
	flaky := func(context.Context) error {
		calls++
		if calls < 3 {
			return fmt.Errorf("dial: %w", retry.ErrRetryable)
		}
		return nil
	}
	attempts, err := retry.Do(context.Background(),
		retry.Config{MaxAttempts: 5, BaseDelay: time.Millisecond},
		flaky)
	fmt.Printf("succeeded after %d attempts (err=%v)\n", attempts, err)

	denied := func(context.Context) error { return fmt.Errorf("acl: %w", retry.ErrPermission) }
	attempts, err = retry.Do(context.Background(),
		retry.Config{MaxAttempts: 5, BaseDelay: time.Millisecond},
		denied)
	fmt.Printf("stopped after %d attempt(s): %v\n", attempts, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
succeeded after 3 attempts (err=<nil>)
stopped after 1 attempt(s): retry stopped after attempt 1: acl: permission denied
```

### Tests

`TestRetriesUntilSuccess` uses a no-op sleep and asserts the exact attempt count.
`TestTerminalStopsImmediately` proves a wrapped `ErrPermission` stops the loop on
the first attempt. `TestStopsOnCancellation` cancels the context inside the
backoff sleep and asserts the loop returns promptly wrapping `context.Canceled`
without looping again.

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

func noSleep(context.Context, time.Duration) error { return nil }

func TestRetriesUntilSuccess(t *testing.T) {
	t.Parallel()

	calls := 0
	op := func(context.Context) error {
		calls++
		if calls <= 2 {
			return fmt.Errorf("attempt %d: %w", calls, ErrRetryable)
		}
		return nil
	}
	attempts, err := Do(context.Background(),
		Config{MaxAttempts: 5, BaseDelay: time.Millisecond, Sleep: noSleep}, op)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestTerminalStopsImmediately(t *testing.T) {
	t.Parallel()

	calls := 0
	op := func(context.Context) error { calls++; return fmt.Errorf("acl: %w", ErrPermission) }
	attempts, err := Do(context.Background(),
		Config{MaxAttempts: 5, BaseDelay: time.Millisecond, Sleep: noSleep}, op)
	if !errors.Is(err, ErrPermission) {
		t.Fatalf("want ErrPermission, got %v", err)
	}
	if attempts != 1 || calls != 1 {
		t.Fatalf("attempts=%d calls=%d, want 1,1 (terminal must not retry)", attempts, calls)
	}
}

func TestDeadlineExceededIsTerminal(t *testing.T) {
	t.Parallel()

	calls := 0
	op := func(context.Context) error { calls++; return context.DeadlineExceeded }
	_, err := Do(context.Background(),
		Config{MaxAttempts: 5, BaseDelay: time.Millisecond, Sleep: noSleep}, op)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (deadline must not retry)", calls)
	}
}

func TestStopsOnCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	sleep := func(c context.Context, _ time.Duration) error {
		cancel() // caller gives up mid-backoff
		return c.Err()
	}
	got, err := Do(ctx,
		Config{MaxAttempts: 10, BaseDelay: time.Second, Sleep: sleep},
		func(context.Context) error { attempts++; return ErrRetryable })

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if attempts > 2 {
		t.Fatalf("op ran %d times after cancellation; loop should stop fast", attempts)
	}
	if got > 2 {
		t.Fatalf("reported %d attempts; loop should stop fast", got)
	}
}

func Example_isRetryable() {
	fmt.Println(isRetryable(ErrRetryable))
	fmt.Println(isRetryable(fmt.Errorf("acl: %w", ErrPermission)))
	fmt.Println(isRetryable(context.Canceled))
	// Output:
	// true
	// false
	// false
}
```

## Review

The classifier is correct when it retries `ErrRetryable` and nothing else, and
when `errors.Is` recognizes a terminal sentinel even under wrapping — that is
what `TestTerminalStopsImmediately` (wrapped `ErrPermission`, one attempt) and
`Example_isRetryable` pin down. The cancellation test proves the loop honors a
context that flips mid-backoff instead of sleeping out a full delay against a
request nobody awaits. The classic bug is a loop that retries on *any* non-nil
error: it turns a permission denial or a cancelled request into a burst of
useless load against a struggling dependency. Retry the transient set; fail fast
on everything else.

## Resources

- [`context` variables](https://pkg.go.dev/context#pkg-variables) — `context.Canceled` and `context.DeadlineExceeded`.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — the wrap-aware comparison the classifier depends on.
- [`time.NewTimer`](https://pkg.go.dev/time#NewTimer) — the context-aware backoff timer.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-translate-driver-sentinels.md](03-translate-driver-sentinels.md) | Next: [05-custom-is-method.md](05-custom-is-method.md)
