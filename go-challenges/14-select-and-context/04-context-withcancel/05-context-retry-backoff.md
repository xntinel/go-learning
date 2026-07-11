# Exercise 5: Context-Respecting Retry With Exponential Backoff

Every production HTTP or RPC client retries transient failures with exponential
backoff and jitter. The detail that separates a correct retry loop from a
dangerous one is the sleep *between* attempts: a bare `time.Sleep` ignores
cancellation, so a client whose request was already aborted still waits out the
full backoff before giving up. The fix is to sleep on a `select` between a timer
and `ctx.Done()`, so a cancel unwinds the wait immediately.

This module is fully self-contained: its own `go mod init`, package, demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
retry/                     independent module: example.com/retry
  go.mod                   module example.com/retry
  retry.go                 Policy; Retry(ctx, op, policy); backoff; cancellable sleep
  cmd/
    demo/
      main.go              op fails twice then succeeds; prints each attempt
  retry_test.go            succeeds-after-retries, aborts-on-cancel, exhausts-attempts
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `Retry(ctx, op, policy)` that retries `op` with exponential backoff and jitter, sleeping on a `select` of a `*time.Timer` versus `ctx.Done()`, and stops calling `op` the moment `ctx` is cancelled.
- Test: `op` fails twice then succeeds in exactly 3 attempts; a cancel during the backoff returns a context error promptly without re-invoking `op`; an always-failing `op` returns its last error after `MaxAttempts` tries.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p retry/cmd/demo
cd retry
go mod init example.com/retry
```

### Why the sleep is a select, not time.Sleep

The retry loop has two jobs: call `op`, and if it fails and attempts remain, wait
a backoff interval and try again. The trap is in the wait. Write it as
`time.Sleep(backoff)` and the loop becomes uncancellable *during the sleep*: a
context cancelled one nanosecond after the sleep starts is not noticed until the
full interval elapses. With exponential backoff the later intervals are seconds
long, so a client whose caller has already given up keeps a goroutine parked,
holding the attempt's state, for seconds. Under load that is a pile of zombie
retries.

The correct sleep is a `select`:

```go
timer := time.NewTimer(d)
defer timer.Stop()
select {
case <-ctx.Done():
	return context.Cause(ctx)
case <-timer.C:
	return nil
}
```

Now the wait ends on whichever comes first — the timer or the cancel. `defer
timer.Stop()` releases the timer whether we woke from `timer.C` or from
`ctx.Done()`; stopping a timer that has already fired is harmless, and stopping
one that has not prevents it from leaking until it would have fired. Returning
`context.Cause(ctx)` rather than `ctx.Err()` means that if the caller cancelled
with a diagnosable cause (a `WithCancelCause`), the retry surfaces *that* reason;
when there is no custom cause, `context.Cause` falls back to `ctx.Err()`
automatically, so the coarse `context.Canceled`/`context.DeadlineExceeded` still
flows through.

The loop also checks `ctx.Err()` at the top of each iteration, before calling
`op`. This closes the gap where a context is cancelled while `op` itself is
running: the next iteration sees the cancellation and returns without a redundant
call. The backoff itself is `Base << attempt`, capped at `Max`, with jitter added
so a thundering herd of clients does not retry in lockstep — the standard
"exponential backoff with full jitter" shape.

Create `retry.go`:

```go
package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// ErrInvalidPolicy is returned when a Policy has fewer than one attempt.
var ErrInvalidPolicy = errors.New("retry: MaxAttempts must be >= 1")

// Policy configures the retry schedule.
type Policy struct {
	MaxAttempts int           // total attempts, including the first
	Base        time.Duration // backoff for the first retry
	Max         time.Duration // cap on any single backoff interval
}

// Retry calls op until it returns nil, ctx is cancelled, or the policy's attempt
// budget is exhausted. Between attempts it sleeps a jittered exponential backoff
// on a select against ctx.Done(), so a cancel aborts the wait immediately and no
// further op call is made. It returns nil on success, the context's cause if
// cancelled, or the last op error wrapped with the attempt count.
func Retry(ctx context.Context, op func(context.Context) error, p Policy) error {
	if p.MaxAttempts < 1 {
		return ErrInvalidPolicy
	}

	var lastErr error
	for attempt := 0; attempt < p.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}

		lastErr = op(ctx)
		if lastErr == nil {
			return nil
		}
		if attempt == p.MaxAttempts-1 {
			break
		}
		if err := sleep(ctx, backoff(p, attempt)); err != nil {
			return err
		}
	}
	return fmt.Errorf("retry: exhausted after %d attempts: %w", p.MaxAttempts, lastErr)
}

// backoff returns the jittered wait before the retry following the given
// zero-based attempt: Base<<attempt, capped at Max, then full jitter in
// [d/2, d].
func backoff(p Policy, attempt int) time.Duration {
	d := p.Base << attempt
	if d <= 0 || d > p.Max {
		d = p.Max
	}
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// sleep waits d, or returns the context's cause if ctx is cancelled first. The
// timer is always stopped so it never leaks.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}
```

### The runnable demo

The demo retries an operation that fails on its first two attempts and succeeds on
the third, printing each attempt. The sequential loop makes the output order
fixed.

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
	errTemporary := errors.New("temporary failure")

	attempt := 0
	op := func(ctx context.Context) error {
		attempt++
		if attempt < 3 {
			fmt.Printf("attempt %d: failed\n", attempt)
			return errTemporary
		}
		fmt.Printf("attempt %d: succeeded\n", attempt)
		return nil
	}

	err := retry.Retry(context.Background(), op, retry.Policy{
		MaxAttempts: 5,
		Base:        time.Millisecond,
		Max:         10 * time.Millisecond,
	})
	fmt.Println("result:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempt 1: failed
attempt 2: failed
attempt 3: succeeded
result: <nil>
```

### Tests

`TestSucceedsAfterRetries` fails twice then succeeds and asserts exactly three
`op` calls. `TestAbortsOnCancel` uses a large `Base` so the loop parks in the
backoff sleep, cancels during it, and asserts a prompt context error with no
second `op` call. `TestExhaustsAttempts` fails every time and asserts the returned
error wraps the op's sentinel after `MaxAttempts` calls. The attempt counter is an
`atomic.Int64` because `op` runs on the goroutine driving `Retry`.

Create `retry_test.go`:

```go
package retry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestSucceedsAfterRetries(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	var calls atomic.Int64
	op := func(ctx context.Context) error {
		if calls.Add(1) < 3 {
			return errBoom
		}
		return nil
	}

	err := Retry(context.Background(), op, Policy{
		MaxAttempts: 5,
		Base:        time.Millisecond,
		Max:         5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Retry err = %v, want nil", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("op called %d times, want 3", got)
	}
}

func TestAbortsOnCancel(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	var calls atomic.Int64
	called := make(chan struct{}, 1)
	op := func(ctx context.Context) error {
		calls.Add(1)
		called <- struct{}{}
		return errBoom
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- Retry(ctx, op, Policy{
			MaxAttempts: 5,
			Base:        time.Hour, // the loop will park in sleep()
			Max:         time.Hour,
		})
	}()

	<-called // first attempt done; loop is now in the backoff sleep
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Retry err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Retry did not abort within 1s of cancel")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("op called %d times after cancel, want 1", got)
	}
}

func TestExhaustsAttempts(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	var calls atomic.Int64
	op := func(ctx context.Context) error {
		calls.Add(1)
		return errBoom
	}

	err := Retry(context.Background(), op, Policy{
		MaxAttempts: 3,
		Base:        time.Millisecond,
		Max:         5 * time.Millisecond,
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("Retry err = %v, want wrapped errBoom", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("op called %d times, want 3", got)
	}
}

func TestInvalidPolicy(t *testing.T) {
	t.Parallel()

	err := Retry(context.Background(), func(context.Context) error { return nil },
		Policy{MaxAttempts: 0})
	if !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("Retry err = %v, want ErrInvalidPolicy", err)
	}
}
```

## Review

The retry is correct when success stops immediately, exhaustion returns the last
`op` error wrapped with `%w` (so `errors.Is` finds the underlying sentinel), and a
cancel during the backoff returns promptly *without* another `op` call. The last
property is the one that catches the `time.Sleep` bug: `TestAbortsOnCancel` parks
the loop in the sleep with a one-hour `Base`, cancels, and requires the return
within a second and `op` called exactly once. Surface `context.Cause(ctx)` rather
than `ctx.Err()` from the sleep so a custom cancellation cause is not flattened.
Always `Stop()` the timer in the sleep helper — a fired timer's stop is a no-op,
an unfired timer's stop prevents a leak. Run `go test -race` to confirm the
`Retry` goroutine and the cancel are race-free.

## Resources

- [context package](https://pkg.go.dev/context) — `Context.Done`, `Context.Err`, `context.Cause` in the sleep.
- [time.NewTimer](https://pkg.go.dev/time#NewTimer) and [Timer.Stop](https://pkg.go.dev/time#Timer.Stop) — the cancellable, leak-free wait.
- [AWS Architecture Blog: Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — why full jitter beats lockstep retries.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-worker-pool-shutdown.md](06-worker-pool-shutdown.md)
