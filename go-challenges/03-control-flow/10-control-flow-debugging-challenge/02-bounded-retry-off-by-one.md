# Exercise 2: The Retry That Ran One Too Many (or One Too Few)

A bounded retry wrapper around a flaky downstream call shipped with a loop-boundary
bug: it performs one call too many and, on a cancelled context, a misplaced
`continue` spins past the backoff instead of stopping. You will read a test that
counts calls, locate the off-by-one, and fix the loop so exactly `maxAttempts`
calls happen and the final error is returned wrapped.

## What you'll build

```text
retry/                     module example.com/retry
  go.mod
  retry.go                 Do(ctx, maxAttempts, backoff, fn); ErrExhausted sentinel
  cmd/demo/
    main.go                runnable demo: a downstream that succeeds on attempt 3
  retry_test.go            exact-count table tests, cancellation test, wrap assertion
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `Do(ctx, maxAttempts, backoff, fn)` that calls `fn` at most `maxAttempts` times, sleeps `backoff` between attempts (never after the last), honors `ctx` cancellation, and returns `errors.Join(ErrExhausted, lastErr)` when all attempts fail.
- Test: exact attempt counts for success-on-first, success-on-Nth, and all-fail; a `context.WithTimeout` cancellation test; an assertion that the returned error wraps the final downstream error.
- Verify: `go test -count=1 -race ./...`.

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/02-bounded-retry-off-by-one/cmd/demo
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/02-bounded-retry-off-by-one
```

### The artifact and the planted bug

A bounded retry has one job that a test can pin exactly: call the downstream *at
most* `maxAttempts` times. The version that shipped got the boundary wrong twice.
Read it before fixing it:

```go
func Do(ctx context.Context, maxAttempts int, backoff time.Duration, fn func(ctx context.Context) error) error {
	var lastErr error
	for attempt := 0; attempt <= maxAttempts; attempt++ { // BUG: <= runs maxAttempts+1 times
		lastErr = fn(ctx)
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			continue // BUG: skips the backoff and keeps looping after cancellation
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return errors.Join(ErrExhausted, lastErr)
}
```

Two boundary defects. First, `attempt <= maxAttempts` runs the body
`maxAttempts + 1` times: a caller asking for 3 attempts gets 4 calls against a
downstream that may be rate-limiting them. Second, the `continue` on a cancelled
context skips the backoff *and* re-enters the loop, so instead of returning
promptly the retry burns through its remaining budget calling a downstream it
already knows is unreachable. A test that only checks "did it eventually error"
sees green; a test that counts calls sees `4` where it wanted `3`.

The failing count test reads:

```text
--- FAIL: TestExactAttemptCount/all_fail (0.00s)
    retry_test.go:41: calls = 4, want 3
```

The fix uses `for attempt := range maxAttempts` (indices `0..maxAttempts-1`,
exactly `maxAttempts` iterations), checks cancellation at the *top* of each
iteration, and breaks before sleeping after the final attempt so the loop never
waits on a backoff it will not use.

Create `retry.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrExhausted reports that every attempt failed. The returned error joins it
// with the final downstream error, so callers can match either with errors.Is.
var ErrExhausted = errors.New("retry: attempts exhausted")

// Do calls fn up to maxAttempts times, sleeping backoff between attempts. It
// returns nil on the first success. If every attempt fails it returns
// errors.Join(ErrExhausted, lastErr). It honors ctx cancellation: a cancelled
// context aborts before the next attempt and returns ctx.Err().
func Do(ctx context.Context, maxAttempts int, backoff time.Duration, fn func(ctx context.Context) error) error {
	if maxAttempts < 1 {
		return fmt.Errorf("retry: maxAttempts must be >= 1, got %d", maxAttempts)
	}
	var lastErr error
	for attempt := range maxAttempts {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = fn(ctx)
		if lastErr == nil {
			return nil
		}
		if attempt == maxAttempts-1 {
			break // no backoff after the final attempt
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return errors.Join(ErrExhausted, lastErr)
}
```

The loop now runs exactly `maxAttempts` times. The `ctx.Err()` check at the top
turns a context cancelled *between* attempts into an immediate return, and the
`select` turns a cancellation that arrives *during* a backoff into an immediate
return as well — no attempt is wasted on a dead context.

### The runnable demo

The demo wraps a downstream that fails twice and succeeds on the third attempt, so
you can watch the exact call count against the wall clock.

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

func main() {
	calls := 0
	err := retry.Do(context.Background(), 3, 10*time.Millisecond, func(ctx context.Context) error {
		calls++
		fmt.Printf("attempt %d\n", calls)
		if calls < 3 {
			return errors.New("503 service unavailable")
		}
		return nil
	})
	fmt.Printf("done after %d attempts, err=%v\n", calls, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
attempt 1
attempt 2
attempt 3
done after 3 attempts, err=<nil>
```

### Tests

`TestExactAttemptCount` is the core: a call-counting fake `fn` and a table that
pins the count for success-on-first, success-on-third, and all-fail.
`TestHonorsCancellation` gives the loop a `WithTimeout` shorter than one backoff
and asserts it stops early with `context.DeadlineExceeded` rather than draining
all 100 attempts. `TestWrapsFinalError` asserts the returned error matches both
`ErrExhausted` and the final downstream sentinel via `errors.Is`.

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

var errDownstream = errors.New("downstream unavailable")

func TestExactAttemptCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		maxAttempts int
		failUntil   int // number of leading calls that fail before success
		wantCalls   int
		wantErr     error
	}{
		{"success on first", 3, 0, 1, nil},
		{"success on third", 3, 2, 3, nil},
		{"all fail", 3, 3, 3, ErrExhausted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			calls := 0
			fn := func(ctx context.Context) error {
				calls++
				if calls <= tc.failUntil {
					return errDownstream
				}
				return nil
			}
			err := Do(t.Context(), tc.maxAttempts, time.Millisecond, fn)
			if calls != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tc.wantCalls)
			}
			if tc.wantErr == nil && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	calls := 0
	fn := func(ctx context.Context) error {
		calls++
		return errDownstream
	}
	// backoff (100ms) far exceeds the timeout (20ms), so cancellation fires
	// during the first backoff, well before the 100-attempt budget.
	err := Do(ctx, 100, 100*time.Millisecond, fn)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if calls >= 100 {
		t.Fatalf("calls = %d, expected to stop early on cancellation", calls)
	}
}

func TestWrapsFinalError(t *testing.T) {
	t.Parallel()

	fn := func(ctx context.Context) error { return errDownstream }
	err := Do(t.Context(), 2, time.Millisecond, fn)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want to wrap ErrExhausted", err)
	}
	if !errors.Is(err, errDownstream) {
		t.Fatalf("err = %v, want to wrap errDownstream", err)
	}
}

func ExampleDo() {
	attempts := 0
	err := Do(context.Background(), 3, time.Millisecond, func(ctx context.Context) error {
		attempts++
		if attempts < 2 {
			return errors.New("flaky")
		}
		return nil
	})
	fmt.Println(attempts, err)
	// Output: 2 <nil>
}
```

## Review

The retry is correct when the call count equals `maxAttempts` on total failure,
equals `N` when the Nth attempt succeeds, and equals `1` on immediate success —
and when a cancelled context returns `ctx.Err()` without wasting the remaining
budget. The count assertions are what make this trustworthy: `for i := range n`
runs `n` times, and testing only success-versus-failure would let a
`maxAttempts + 1` off-by-one ship. The cancellation test proves the loop honors
`ctx` between attempts, and the wrap assertion proves the final downstream error
survives to the caller through `errors.Join` — so upstream code can match
`errors.Is(err, errDownstream)` and decide whether the failure was retryable at a
higher layer.

## Resources

- [context package](https://pkg.go.dev/context) — `WithTimeout`, `Done`, `Err`, and `DeadlineExceeded`.
- [errors.Join](https://pkg.go.dev/errors#Join) — combining `ErrExhausted` with the last downstream error.
- [testing.T.Context](https://pkg.go.dev/testing#T.Context) — the per-test context (Go 1.24), cancelled at test cleanup.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-connection-state-machine-closed-twice.md](01-connection-state-machine-closed-twice.md) | Next: [03-defer-close-in-loop-fd-leak.md](03-defer-close-in-loop-fd-leak.md)
