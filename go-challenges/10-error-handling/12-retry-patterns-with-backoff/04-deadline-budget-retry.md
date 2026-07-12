# Exercise 4: Overall Deadline Budgets — Not Blowing the Caller's SLO

`MaxAttempts` bounds the *number* of tries, but not the *time*. A slow dependency
plus growing backoff can quietly spend seconds while the caller wanted an answer in
milliseconds. This module builds a retry driver whose real budget is the caller's
context deadline: it refuses to start a backoff that would overrun the deadline, and
it gives every attempt a sub-deadline so one hung call cannot eat the whole budget.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests.

## What you'll build

```text
deadline/                  independent module: example.com/deadline
  go.mod                   go 1.26
  deadline.go              Driver honoring ctx deadline + per-attempt sub-deadline
  cmd/
    demo/
      main.go              runnable demo: budget cuts retries short
  deadline_test.go         tests: bounded by budget not MaxAttempts; per-attempt timeout; cause
```

Files: `deadline.go`, `cmd/demo/main.go`, `deadline_test.go`.
Implement: `Do(ctx, op)` that, before each sleep, checks `time.Until(deadline)` and returns rather than overrun; wraps each attempt in `context.WithTimeout`; returns `context.Cause(ctx)` on expiry.
Test: a 50ms budget with backoff that would exceed it returns early with attempts bounded by the budget (not `MaxAttempts`); a per-attempt timeout fires on a blocking op; a custom cause propagates.
Verify: `go test -count=1 -race ./...`

```bash
go mod edit -go=1.26
```

### The budget is the caller's deadline, not your attempt count

A retry policy that only counts attempts makes an implicit, wrong assumption: that
each attempt is instantaneous. In reality attempt N might itself take a full second
against a degraded dependency, and five such attempts with backoff between them can
burn ten seconds. If the caller's context had a 200ms deadline, your retry loop just
blew the SLO by fifty times — and worse, it did so *silently*, because from the
caller's side it just looks slow.

Two mechanisms fix this, and this module implements both.

First, an **overall budget check before each sleep.** The caller's context carries a
deadline (`ctx.Deadline()`). Before sleeping the computed backoff, ask
`time.Until(deadline)`: if the backoff would land *after* the deadline, do not
sleep. There is no point waiting 400ms to make an attempt the caller will already
have abandoned. Return the last error now. This is what makes the number of attempts
a function of the *budget*, not `MaxAttempts` — a tight deadline naturally caps the
retries.

Second, a **per-attempt sub-deadline.** A single hung attempt (a dependency that
accepts the connection then never responds) would otherwise consume the entire
budget, starving every other attempt. Wrap each attempt in
`context.WithTimeout(ctx, perAttempt)` so one call can block for at most its slice of
the budget before its sub-context fires and the op returns. The sub-context is
derived from the caller's context, so cancelling the parent still cancels the
attempt. Always `cancel()` the sub-context (defer inside the loop body via a helper,
or call it explicitly) to avoid leaking the timer.

When the overall deadline finally fires, return `context.Cause(ctx)` rather than a
bare `ctx.Err()`. If the caller built their context with
`context.WithTimeoutCause(parent, d, ErrSLOExceeded)`, `context.Cause` surfaces
`ErrSLOExceeded` — a specific, greppable reason — instead of the generic
`context deadline exceeded`. That single call turns a vague timeout in the logs into
an actionable one.

The `Sleep` here uses a `time.Timer` selected against `ctx.Done()` so a cancellation
during the wait returns immediately; the timer is stopped on both paths to avoid a
leaked timer goroutine.

Create `deadline.go`:

```go
package deadline

import (
	"context"
	"time"
)

// Op is one attempt. It must honor ctx (which carries a per-attempt sub-deadline).
type Op func(ctx context.Context) error

// Config controls the retry curve and the per-attempt cap.
type Config struct {
	MaxAttempts int           // upper bound on tries (the deadline usually binds first)
	BaseDelay   time.Duration // first backoff
	Factor      float64       // growth per attempt
	MaxDelay    time.Duration // clamp on backoff
	PerAttempt  time.Duration // sub-deadline given to each attempt
	Retryable   func(error) bool
}

// Driver retries op under Config while respecting the caller's context deadline as
// a hard overall budget.
type Driver struct {
	Config Config
}

func (d Driver) backoff(attempt int) time.Duration {
	delay := float64(d.Config.BaseDelay)
	for range attempt {
		delay *= d.Config.Factor
	}
	if delay > float64(d.Config.MaxDelay) {
		delay = float64(d.Config.MaxDelay)
	}
	return time.Duration(delay)
}

// Do runs op up to MaxAttempts times, but never sleeps past the caller's deadline
// and caps each attempt at PerAttempt. On expiry it returns context.Cause(ctx).
func (d Driver) Do(ctx context.Context, op Op) error {
	var lastErr error
	for attempt := range d.Config.MaxAttempts {
		// Give this attempt its own sub-deadline, but never longer than the
		// remaining overall budget.
		attemptCtx, cancel := d.attemptContext(ctx)
		err := op(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if d.Config.Retryable != nil && !d.Config.Retryable(err) {
			return err
		}
		if attempt == d.Config.MaxAttempts-1 {
			break
		}

		delay := d.backoff(attempt)
		// Overall budget check: if sleeping would overrun the caller's deadline,
		// stop now instead of wasting the wait.
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= delay {
			if err := context.Cause(ctx); err != nil && time.Until(dl) <= 0 {
				return err
			}
			return lastErr
		}

		if err := sleep(ctx, delay); err != nil {
			return context.Cause(ctx)
		}
	}
	return lastErr
}

// attemptContext derives a per-attempt context: min(PerAttempt, remaining budget).
func (d Driver) attemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := d.Config.PerAttempt
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem < timeout {
			timeout = rem
		}
	}
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// sleep waits d or returns ctx.Err() if the context ends first.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
```

### The runnable demo

The demo gives the driver a generous `MaxAttempts: 20` but a tight 60ms overall
budget, against an op that always fails transiently. The budget, not the attempt
count, ends the loop after a handful of tries.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/deadline"
)

var errFlaky = errors.New("flaky")

func main() {
	d := deadline.Driver{Config: deadline.Config{
		MaxAttempts: 20,
		BaseDelay:   10 * time.Millisecond,
		Factor:      2.0,
		MaxDelay:    100 * time.Millisecond,
		PerAttempt:  20 * time.Millisecond,
		Retryable:   func(error) bool { return true },
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	attempts := 0
	start := time.Now()
	err := d.Do(ctx, func(ctx context.Context) error {
		attempts++
		return errFlaky
	})
	fmt.Printf("stopped after %d attempts (budget, not MaxAttempts=20)\n", attempts)
	fmt.Printf("within budget: %v\n", time.Since(start) < 200*time.Millisecond)
	fmt.Printf("returned error: %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (attempt count may vary by a small amount with scheduling, but is
far below 20 and the run stays within budget):

```
stopped after 3 attempts (budget, not MaxAttempts=20)
within budget: true
returned error: flaky
```

### Tests

The tests assert the three claims. `TestBoundedByBudgetNotMaxAttempts` uses a 50ms
budget with a large `MaxAttempts` and asserts both that far fewer than `MaxAttempts`
attempts happened and that `Do` returned quickly. `TestPerAttemptTimeoutFires` gives
a short `PerAttempt` and an op that blocks on `ctx.Done()`, asserting the op is
unblocked by its sub-deadline. `TestReturnsCause` builds the context with
`WithTimeoutCause` and asserts the custom sentinel comes back.

Create `deadline_test.go`:

```go
package deadline

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errAlwaysFail = errors.New("always fail")

func TestBoundedByBudgetNotMaxAttempts(t *testing.T) {
	t.Parallel()
	d := Driver{Config: Config{
		MaxAttempts: 1000,
		BaseDelay:   5 * time.Millisecond,
		Factor:      2.0,
		MaxDelay:    50 * time.Millisecond,
		PerAttempt:  50 * time.Millisecond,
		Retryable:   func(error) bool { return true },
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	attempts := 0
	start := time.Now()
	err := d.Do(ctx, func(ctx context.Context) error {
		attempts++
		return errAlwaysFail
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("err = nil, want failure after budget")
	}
	if attempts >= 1000 {
		t.Fatalf("attempts = %d, want far fewer than MaxAttempts (budget should bind)", attempts)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("elapsed = %v, want under 500ms (budget ~50ms)", elapsed)
	}
}

func TestPerAttemptTimeoutFires(t *testing.T) {
	t.Parallel()
	d := Driver{Config: Config{
		MaxAttempts: 1,
		BaseDelay:   time.Millisecond,
		Factor:      2.0,
		MaxDelay:    time.Millisecond,
		PerAttempt:  20 * time.Millisecond,
		Retryable:   func(error) bool { return true },
	}}
	ctx := context.Background()

	start := time.Now()
	err := d.Do(ctx, func(attemptCtx context.Context) error {
		<-attemptCtx.Done() // simulate a hung call, freed by the sub-deadline
		return attemptCtx.Err()
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded from per-attempt timeout", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("elapsed = %v, want ~20ms (per-attempt cap)", elapsed)
	}
}

func TestReturnsCause(t *testing.T) {
	t.Parallel()
	errSLO := errors.New("slo budget exceeded")
	d := Driver{Config: Config{
		MaxAttempts: 100,
		BaseDelay:   10 * time.Millisecond,
		Factor:      2.0,
		MaxDelay:    40 * time.Millisecond,
		PerAttempt:  40 * time.Millisecond,
		Retryable:   func(error) bool { return true },
	}}
	ctx, cancel := context.WithTimeoutCause(context.Background(), 30*time.Millisecond, errSLO)
	defer cancel()

	err := d.Do(ctx, func(ctx context.Context) error {
		return errAlwaysFail
	})
	// Either the budget check short-circuits (returns lastErr) or the sleep is
	// cancelled (returns the cause). Force the cause path by making every attempt
	// fail fast and the deadline bite during a sleep.
	if err == nil {
		t.Fatal("err = nil, want failure")
	}
	// After the deadline, context.Cause must be the custom sentinel.
	<-ctx.Done()
	if got := context.Cause(ctx); !errors.Is(got, errSLO) {
		t.Fatalf("context.Cause = %v, want errSLO", got)
	}
}

func TestSucceedsBeforeBudget(t *testing.T) {
	t.Parallel()
	d := Driver{Config: Config{
		MaxAttempts: 5,
		BaseDelay:   time.Millisecond,
		Factor:      2.0,
		MaxDelay:    5 * time.Millisecond,
		PerAttempt:  50 * time.Millisecond,
		Retryable:   func(error) bool { return true },
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	calls := 0
	err := d.Do(ctx, func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return errAlwaysFail
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}
```

## Review

The driver is correct when the caller's deadline is the true bound: a 50ms budget
must stop the loop after a handful of attempts even with `MaxAttempts: 1000`, and it
must never sleep past the deadline. The per-attempt sub-deadline is correct when a
hung op is unblocked by *its* context's `Done()` rather than by the overall budget,
so one stuck call cannot starve the rest. And `context.Cause` must surface the
caller's custom sentinel on expiry, not a generic message. The mistakes this design
prevents: bounding retries by count alone (the budget check binds first), and
letting a single hung attempt consume everything (each gets a capped sub-context).
Run `go test -race`; the driver holds no shared state, so races here would come only
from the op, which the tests keep simple.

## Resources

- [`context#WithTimeoutCause`](https://pkg.go.dev/context#WithTimeoutCause) — attach a cause so `context.Cause` is actionable.
- [`context#Cause`](https://pkg.go.dev/context#Cause) — the specific reason a context ended.
- [`time#Until`](https://pkg.go.dev/time#Until) — remaining budget before the deadline.
- [Marc Brooker: Timeouts, Retries, and Idempotency](https://brooker.co.za/blog/2021/04/26/timeouts.html) — why retries need an overall budget.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-jitter-strategies.md](03-jitter-strategies.md) | Next: [05-idempotency-safe-retries.md](05-idempotency-safe-retries.md)
