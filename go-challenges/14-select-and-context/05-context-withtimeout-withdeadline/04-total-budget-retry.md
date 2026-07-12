# Exercise 4: Retry With Backoff Bounded by a Total Deadline

A retry loop that ignores the caller's deadline is a latency amplifier: it sleeps
out its full exponential backoff and overruns the budget it was supposed to
protect. This exercise builds a retrier that treats the context deadline as the hard
total budget — it refuses to start an attempt or a backoff sleep that cannot finish
in time, sleeps with a context-aware timer so a mid-backoff cancel returns
immediately, and distinguishes "ran out of retries" from "ran out of time."

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
budget-retry/                        independent module: example.com/budgetretry
  go.mod                             go 1.26
  retry.go                           Retrier, RetryWithBudget(ctx, op); backoff, budget guard
  cmd/
    demo/
      main.go                        runnable demo: succeeds-after-N vs tight-deadline
  retry_test.go                      ample budget, tight deadline, mid-backoff cancel, -race
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `Retrier{Base, Max, MaxAttempts}` and `RetryWithBudget(ctx, op) (int, error)` with capped exponential backoff, a `time.Until(deadline)` guard before each sleep, and a `ctx`-aware timer.
- Test: with ample budget it succeeds and reports the attempt count; with a tight deadline it returns before exhausting retries with `DeadlineExceeded`, near the deadline (not the sum of backoffs); a mid-backoff cancel returns promptly; no attempt starts after the deadline.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/05-context-withtimeout-withdeadline/04-total-budget-retry/cmd/demo
cd go-solutions/14-select-and-context/05-context-withtimeout-withdeadline/04-total-budget-retry
```

### The two things a budget-aware retry must do differently

A naive retry loop calls the operation, and on failure sleeps a growing backoff and
tries again until it runs out of attempts. Two changes turn that into a loop that
honors a total deadline.

First, the backoff sleep is bounded by the remaining budget, and it is *cancellable*.
Before sleeping, the loop reads `ctx.Deadline()` and, if a deadline is set, compares
`time.Until(deadline)` to the backoff it is about to sleep. If the sleep would land
past the deadline, there is no point sleeping — the next attempt could not finish in
time — so it stops now and reports that it ran out of time. The sleep itself is a
`select` over a `time.NewTimer` and `ctx.Done()`, so if the context is cancelled
mid-backoff the loop returns at the cancel instant rather than waiting out the timer.
A plain `time.Sleep` cannot be interrupted; a timer-plus-select can.

Second, the two exit reasons must be *distinguishable*. Running out of attempts is an
operational fact about the dependency — it failed every time we asked — so the error
wraps the operation's last error. Running out of time is a budget fact — the caller's
deadline arrived — so the error wraps `context.DeadlineExceeded`. A caller uses
`errors.Is` to tell them apart: a retries-exhausted error may warrant an alert about
a persistently failing dependency, while a budget-exhausted error is the expected
shape of load-shedding and warrants no such alert. Collapsing them into one error
throws away the signal.

The backoff is capped exponential: `Base`, then `2*Base`, `4*Base`, …, clamped at
`Max`. Capping matters because uncapped doubling reaches minutes quickly and the
budget guard would just reject every attempt; a sensible `Max` keeps the loop useful.

Create `retry.go`:

```go
package budgetretry

import (
	"context"
	"fmt"
	"time"
)

// Retrier retries an operation with capped exponential backoff, bounded by the
// caller's context deadline as the hard total budget.
type Retrier struct {
	Base        time.Duration // first backoff
	Max         time.Duration // backoff cap
	MaxAttempts int           // hard attempt ceiling
}

// backoff returns the capped exponential delay before the given 1-based attempt's
// retry. Overflow from the shift is treated as "past the cap".
func (r Retrier) backoff(attempt int) time.Duration {
	d := r.Base << (attempt - 1)
	if d <= 0 || d > r.Max {
		return r.Max
	}
	return d
}

// RetryWithBudget runs op, retrying failures with capped exponential backoff. It
// never sleeps or starts an attempt that cannot finish before ctx's deadline. It
// returns the number of attempts made and an error that wraps context.
// DeadlineExceeded when time ran out, or op's last error when retries ran out.
func (r Retrier) RetryWithBudget(ctx context.Context, op func() error) (int, error) {
	var lastErr error
	for attempt := 1; attempt <= r.MaxAttempts; attempt++ {
		// Do not even start an attempt after the deadline has passed.
		if err := ctx.Err(); err != nil {
			return attempt - 1, fmt.Errorf("budget exhausted before attempt %d: %w", attempt, err)
		}

		lastErr = op()
		if lastErr == nil {
			return attempt, nil
		}
		if attempt == r.MaxAttempts {
			break
		}

		wait := r.backoff(attempt)

		// Refuse a backoff that would blow the total budget.
		if deadline, ok := ctx.Deadline(); ok {
			if time.Until(deadline) <= wait {
				return attempt, fmt.Errorf("retry budget exhausted after %d attempts: %w", attempt, context.DeadlineExceeded)
			}
		}

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return attempt, fmt.Errorf("cancelled during backoff after %d attempts: %w", attempt, ctx.Err())
		}
	}
	return r.MaxAttempts, fmt.Errorf("exhausted %d attempts: %w", r.MaxAttempts, lastErr)
}
```

### The runnable demo

The demo runs the same flaky operation (fails twice, then succeeds) under a generous
budget where it succeeds, then under a tight deadline where it runs out of time
first.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/budgetretry"
)

func flaky(failFirst int) func() error {
	n := 0
	return func() error {
		n++
		if n <= failFirst {
			return errors.New("transient failure")
		}
		return nil
	}
}

func main() {
	r := budgetretry.Retrier{Base: 10 * time.Millisecond, Max: 100 * time.Millisecond, MaxAttempts: 10}

	ctx1, cancel1 := context.WithTimeout(context.Background(), time.Second)
	defer cancel1()
	attempts, err := r.RetryWithBudget(ctx1, flaky(2))
	fmt.Printf("ample: attempts=%d err=%v\n", attempts, err)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel2()
	start := time.Now()
	attempts, err = r.RetryWithBudget(ctx2, flaky(100))
	fmt.Printf("tight: timeout=%v attempts<10=%v elapsed<200ms=%v\n",
		errors.Is(err, context.DeadlineExceeded), attempts < 10, time.Since(start) < 200*time.Millisecond)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ample: attempts=3 err=<nil>
tight: timeout=true attempts<10=true elapsed<200ms=true
```

### Tests

`TestSucceedsWithinBudget` runs an op that fails twice then succeeds under a 1s
budget and asserts three attempts, no error. `TestTightDeadlineRunsOutOfTime` gives a
25ms budget to an always-failing op and asserts the error `errors.Is`
`context.DeadlineExceeded`, the loop stopped well before its 10 attempts, and the
elapsed time is near the deadline (not the summed backoffs). `TestExhaustsRetries`
gives ample budget but few attempts and asserts the error wraps the operation error
and is *not* a `DeadlineExceeded`. `TestCancelMidBackoffReturnsPromptly` cancels the
context during a long backoff and asserts the return is faster than the backoff and
`errors.Is` `context.Canceled`. `TestNoAttemptAfterDeadline` records each op call
time and asserts none happened after the deadline.

Create `retry_test.go`:

```go
package budgetretry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

// flaky returns an op that fails failFirst times, then succeeds.
func flaky(failFirst int) func() error {
	var mu sync.Mutex
	n := 0
	return func() error {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n <= failFirst {
			return errBoom
		}
		return nil
	}
}

func TestSucceedsWithinBudget(t *testing.T) {
	t.Parallel()
	r := Retrier{Base: 10 * time.Millisecond, Max: 100 * time.Millisecond, MaxAttempts: 10}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	attempts, err := r.RetryWithBudget(ctx, flaky(2))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestTightDeadlineRunsOutOfTime(t *testing.T) {
	t.Parallel()
	r := Retrier{Base: 10 * time.Millisecond, Max: 100 * time.Millisecond, MaxAttempts: 10}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	start := time.Now()
	attempts, err := r.RetryWithBudget(ctx, func() error { return errBoom })
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if errors.Is(err, errBoom) {
		t.Fatalf("err = %v, must be attributed to the budget, not the op error", err)
	}
	if attempts >= r.MaxAttempts {
		t.Fatalf("attempts = %d, want < %d (stopped by budget)", attempts, r.MaxAttempts)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("elapsed = %v, want near the 25ms deadline, not the summed backoffs", elapsed)
	}
}

func TestExhaustsRetries(t *testing.T) {
	t.Parallel()
	r := Retrier{Base: time.Millisecond, Max: 5 * time.Millisecond, MaxAttempts: 3}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	attempts, err := r.RetryWithBudget(ctx, func() error { return errBoom })
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want wrapped errBoom (retries exhausted)", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, must NOT be a deadline error", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestCancelMidBackoffReturnsPromptly(t *testing.T) {
	t.Parallel()
	r := Retrier{Base: 500 * time.Millisecond, Max: time.Second, MaxAttempts: 5}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	start := time.Now()
	_, err := r.RetryWithBudget(ctx, func() error { return errBoom })
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("returned in %v, want prompt (< the 500ms backoff)", elapsed)
	}
}

func TestNoAttemptAfterDeadline(t *testing.T) {
	t.Parallel()
	r := Retrier{Base: 10 * time.Millisecond, Max: 40 * time.Millisecond, MaxAttempts: 20}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()

	var mu sync.Mutex
	var calls []time.Time
	_, _ = r.RetryWithBudget(ctx, func() error {
		mu.Lock()
		calls = append(calls, time.Now())
		mu.Unlock()
		return errBoom
	})

	mu.Lock()
	defer mu.Unlock()
	for i, at := range calls {
		if at.After(deadline) {
			t.Fatalf("attempt %d started at %v, after deadline %v", i+1, at, deadline)
		}
	}
	if len(calls) == 0 {
		t.Fatal("no attempts were made")
	}
}
```

## Review

The retrier is correct when elapsed time tracks the budget, not the backoff schedule.
The guard `time.Until(deadline) <= wait` is what stops the loop from sleeping into
overrun, which `TestTightDeadlineRunsOutOfTime` proves by asserting the call returns
near 25ms with a `DeadlineExceeded` error while the summed backoffs would be far
longer. The context-aware timer is what makes a mid-backoff cancel return at the
cancel instant, which `TestCancelMidBackoffReturnsPromptly` proves by cancelling
during a 500ms backoff and asserting a sub-200ms return. And the two exit reasons
stay distinct: `TestExhaustsRetries` sees the operation error and no deadline,
`TestTightDeadlineRunsOutOfTime` sees the deadline and no operation error.

The mistakes to avoid: using `time.Sleep` for the backoff (uninterruptible, so a
cancel waits out the whole sleep); dropping the pre-sleep budget check (the loop
sleeps past the caller's deadline); and reporting one exit reason as the other. The
`ctx.Err()` check at the top of each iteration is the belt-and-suspenders that
guarantees no attempt starts after the deadline, which `TestNoAttemptAfterDeadline`
verifies by timestamping every op call. Run `go test -race`; the flaky closure and
the call-recording slice are mutex-guarded for the detector.

## Resources

- [time.Until](https://pkg.go.dev/time#Until) — the remaining-budget computation the guard is built on.
- [time.NewTimer](https://pkg.go.dev/time#NewTimer) — the cancellable timer that makes the backoff sleep interruptible.
- [context.Context.Deadline](https://pkg.go.dev/context#Context) — reading the effective deadline to bound the backoff.
- [Google SRE Book: Handling Overload](https://sre.google/sre-book/handling-overload/) — why retries must respect budgets to avoid amplifying load.

---

Back to [03-db-query-deadline.md](03-db-query-deadline.md) | Next: [05-budget-guard-skip-work.md](05-budget-guard-skip-work.md)
