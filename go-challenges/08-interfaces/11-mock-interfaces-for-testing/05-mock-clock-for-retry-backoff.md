# Exercise 5: Mock the Clock — Deterministic Tests for Retry With Backoff

Retry-with-backoff is the textbook flaky test: a real `time.Sleep` makes the
suite wait real seconds and still races at the boundaries. Hoist time behind a
`Clock` interface and a fake clock makes the exponential-backoff schedule
deterministic and instant — you assert attempt counts, the exact backoff
durations requested, and the total simulated elapsed time with zero wall-clock
waiting.

Fully self-contained: its own module, package, demo, and test.

## What you'll build

```text
retryclock/                  independent module: example.com/retryclock
  go.mod                     go 1.26
  retry.go                   Clock interface; Retry with exponential backoff
  cmd/
    demo/
      main.go                runnable demo with an instant clock
  retry_test.go             fakeClock recording waits; attempt/backoff/budget/cancel tests
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: a `Retry(ctx, clk, cfg, op)` that calls `op`, and on failure waits `clk.After(backoff)` with doubling backoff (capped), honoring a clock-measured budget and `ctx` cancellation.
- Test: a fake clock whose `After` records the duration and advances `Now` instantly; assert attempts, the recorded backoff sequence, total simulated elapsed, budget abort via `ErrBudgetExceeded`, and mid-backoff cancellation.
- Verify: `go test -count=1 -race ./...`

### The Clock seam

Code that calls `time.Now()` and `time.Sleep()` has smuggled in a dependency on
the wall clock that no test can control. Hoist it behind a two-method interface:

```go
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}
```

`After` returns a channel that fires after the duration — exactly the shape of
`time.After`, so the production `Clock` is a one-line wrapper around the real
package and the retry loop's `select` reads identically whether time is real or
fake. In a test, a fake `After` records the requested duration and fires
immediately with the clock advanced, so a three-retry backoff schedule runs in
microseconds and the test can assert on the *sequence of durations the code asked
to wait*, which is the real backoff contract.

### The retry loop

`Retry` runs `op`, and on failure sleeps `clk.After(backoff)` before the next
attempt, doubling the backoff each time up to a cap. Two guards bound it. A
*budget*, measured on the injected clock (`clk.Now().Sub(start)`), aborts before a
sleep that would overrun it — driven entirely by the fake clock, so the test
controls it without wall time. And the sleep is a `select` over `clk.After` and
`ctx.Done()`, so a caller can cancel mid-backoff; that branch returns the wrapped
`ctx.Err()`. Sentinels (`ErrExhausted`, `ErrBudgetExceeded`) are wrapped with `%w`
so callers classify the outcome with `errors.Is` and can still unwrap the
underlying operation error.

Create `retry.go`:

```go
package retryclock

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Sentinels callers classify with errors.Is.
var (
	ErrExhausted      = errors.New("retry attempts exhausted")
	ErrBudgetExceeded = errors.New("retry budget exceeded")
)

// Clock is the injected time seam. Production uses a real-time implementation;
// tests use a fake that advances instantly.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Config controls the backoff schedule.
type Config struct {
	Base        time.Duration // first backoff
	Max         time.Duration // cap per backoff
	MaxAttempts int           // total attempts including the first
	Budget      time.Duration // 0 means unlimited; measured on the Clock
}

// Operation is the retried unit of work.
type Operation func(ctx context.Context) error

// Retry runs op with exponential backoff, honoring a clock-measured budget and
// context cancellation.
func Retry(ctx context.Context, clk Clock, cfg Config, op Operation) error {
	start := clk.Now()
	backoff := cfg.Base
	var lastErr error

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		lastErr = op(ctx)
		if lastErr == nil {
			return nil
		}
		if attempt == cfg.MaxAttempts {
			break
		}
		if cfg.Budget > 0 && clk.Now().Add(backoff).Sub(start) > cfg.Budget {
			return fmt.Errorf("after attempt %d: %w: %w", attempt, ErrBudgetExceeded, lastErr)
		}
		select {
		case <-clk.After(backoff):
		case <-ctx.Done():
			return fmt.Errorf("after attempt %d: %w", attempt, ctx.Err())
		}
		backoff = min(backoff*2, cfg.Max)
	}
	return fmt.Errorf("after %d attempts: %w: %w", cfg.MaxAttempts, ErrExhausted, lastErr)
}
```

### The runnable demo

The demo defines a tiny instant clock in `main` (advancing its own `Now` on each
`After`) and an operation that fails twice, then succeeds — so `go run ./cmd/demo`
prints a deterministic result with no wall-clock delay.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/retryclock"
)

// instantClock advances virtual time on each After and never really sleeps.
type instantClock struct {
	now time.Time
}

func (c *instantClock) Now() time.Time { return c.now }

func (c *instantClock) After(d time.Duration) <-chan time.Time {
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

func main() {
	clk := &instantClock{now: time.Unix(0, 0)}
	attempts := 0
	op := func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary failure")
		}
		return nil
	}

	cfg := retryclock.Config{Base: 100 * time.Millisecond, Max: time.Second, MaxAttempts: 5}
	err := retryclock.Retry(context.Background(), clk, cfg, op)

	fmt.Printf("err: %v\n", err)
	fmt.Printf("attempts: %d\n", attempts)
	fmt.Printf("simulated elapsed: %v\n", clk.Now().Sub(time.Unix(0, 0)))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
err: <nil>
attempts: 3
simulated elapsed: 300ms
```

### Tests

The fake clock records every `After` duration and advances `Now` instantly.
`TestRetrySucceedsAfterFailures` asserts the attempt count, the exact recorded
backoff sequence `[100ms 200ms]`, and the total simulated elapsed. `TestRetry
BudgetAborts` proves a clock-measured budget stops retrying via `ErrBudgetExceeded`.
`TestRetryExhausts` proves a persistently failing op ends in `ErrExhausted` with
the cause still unwrappable. `TestRetryCancelDuringBackoff` uses a blocking clock
and cancels the context mid-backoff to exercise the `ctx.Done()` branch — no wall
sleep, the cancellation fires the select.

Create `retry_test.go`:

```go
package retryclock

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// fakeClock advances virtual time on each After and records requested waits.
type fakeClock struct {
	now   time.Time
	waits []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(0, 0)} }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

// blockingClock never fires After; used to force the cancellation branch.
type blockingClock struct{}

func (blockingClock) Now() time.Time { return time.Unix(0, 0) }

func (blockingClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

var errTemporary = errors.New("temporary failure")

func TestRetrySucceedsAfterFailures(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	attempts := 0
	op := func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errTemporary
		}
		return nil
	}

	cfg := Config{Base: 100 * time.Millisecond, Max: time.Second, MaxAttempts: 5}
	if err := Retry(context.Background(), clk, cfg, op); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	wantWaits := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}
	if !slices.Equal(clk.waits, wantWaits) {
		t.Fatalf("waits = %v, want %v", clk.waits, wantWaits)
	}
	if got := clk.Now().Sub(time.Unix(0, 0)); got != 300*time.Millisecond {
		t.Fatalf("elapsed = %v, want 300ms", got)
	}
}

func TestRetryBudgetAborts(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	attempts := 0
	op := func(context.Context) error { attempts++; return errTemporary }

	cfg := Config{Base: time.Second, Max: 8 * time.Second, MaxAttempts: 10, Budget: 3 * time.Second}
	err := Retry(context.Background(), clk, cfg, op)

	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if !errors.Is(err, errTemporary) {
		t.Fatalf("cause not unwrappable: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRetryExhausts(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	op := func(context.Context) error { return errTemporary }

	cfg := Config{Base: time.Millisecond, Max: time.Second, MaxAttempts: 3}
	err := Retry(context.Background(), clk, cfg, op)

	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
	if !errors.Is(err, errTemporary) {
		t.Fatalf("cause not unwrappable: %v", err)
	}
}

func TestRetryCancelDuringBackoff(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{}, 1)
	op := func(context.Context) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		return errTemporary
	}

	done := make(chan error, 1)
	go func() {
		cfg := Config{Base: time.Hour, Max: time.Hour, MaxAttempts: 5}
		done <- Retry(ctx, blockingClock{}, cfg, op)
	}()

	<-entered // op ran once; loop is now blocked in the select on After
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
```

## Review

The fake clock turns a time-dependent unit into a pure function of the schedule:
`Retry` asks for `100ms` then `200ms`, the fake records exactly that and advances
`Now` by the same, so the test asserts the backoff sequence and the `300ms` total
without ever waiting. The budget is measured on the same injected clock, which is
why `TestRetryBudgetAborts` is deterministic down to the attempt count, and the
`%w`-wrapped sentinels let one assertion check both the classification
(`ErrBudgetExceeded`) and the underlying cause (`errTemporary`).

Two things make this honest. Never call `time.Now`/`time.Sleep` inside the unit —
the whole point is that the clock is injected, so the demo and both clock doubles
control it. And exercise the cancellation branch for real: the blocking clock plus
a mid-flight `cancel()` drives the `ctx.Done()` path deterministically, so it is
tested behavior rather than dead code. In production you supply a real-time
`Clock`; in tests you supply time itself.

## Resources

- [`time` package](https://pkg.go.dev/time) — `time.After`, `time.Now`, and `Duration`, the shapes the `Clock` mirrors.
- [`context` cancellation](https://pkg.go.dev/context) — `WithCancel`/`Done` for aborting an in-flight backoff.
- [`errors` wrapping](https://pkg.go.dev/errors) — `%w` with `errors.Is` to classify and unwrap the retry outcome.

---

Back to [04-mock-http-client-roundtripper.md](04-mock-http-client-roundtripper.md) | Next: [06-gomock-generated-payment-gateway.md](06-gomock-generated-payment-gateway.md)
