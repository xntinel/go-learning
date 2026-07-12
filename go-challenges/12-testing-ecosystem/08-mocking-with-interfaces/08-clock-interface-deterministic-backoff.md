# Exercise 8: Injecting a Clock — Deterministic Retry/Backoff Testing

Time is a collaborator, and testing retry/backoff with real `time.Sleep` makes the
suite slow and flaky. The seam is a small `Clock` interface: production injects a
clock backed by the `time` package, and the test injects a fake clock that records
the durations it was asked to sleep and advances virtual time instantly. A backoff
sequence of 100ms, 200ms, 400ms is then asserted in microseconds.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. No external dependencies, no real sleeping in the tests.

## What you'll build

```text
backoff/                     independent module: example.com/backoff
  go.mod                     go 1.26
  backoff.go                 Clock port; SystemClock; Retrier; Do; ErrExhausted/ErrBudgetExceeded
  cmd/
    demo/
      main.go                runnable demo over SystemClock
  backoff_test.go            fake clock recording durations; sequence/exhaust/budget/cancel tests
```

- Files: `backoff.go`, `cmd/demo/main.go`, `backoff_test.go`.
- Implement: a `Retrier` with a `Clock` (`Now`, context-aware `Sleep`); `Do` retries with exponential backoff, honors a total-elapsed budget via `Now`, and aborts when `Sleep` returns a context error.
- Test: a fake clock that records each sleep duration and advances virtual `Now`; assert the backoff sequence, the attempt count, `ErrExhausted`, `ErrBudgetExceeded`, and a context-cancelled abort — all with no real sleep.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/08-mocking-with-interfaces/08-clock-interface-deterministic-backoff/cmd/demo
cd go-solutions/12-testing-ecosystem/08-mocking-with-interfaces/08-clock-interface-deterministic-backoff
```

### The Clock seam and the retrier

The `Clock` interface is deliberately tiny: `Now() time.Time` and `Sleep(ctx,
d) error`. Making `Sleep` context-aware — returning `ctx.Err()` if the context is
done before the duration elapses — is what lets a retry abort mid-backoff instead
of finishing a pointless wait after the caller has already given up. Production
wires `SystemClock`, whose `Sleep` selects on a real `time.NewTimer` and
`ctx.Done()`; the test wires a fake clock whose `Sleep` records the duration,
advances a virtual `Now`, and returns instantly (or returns the context error if
the context is already cancelled).

`Retrier.Do` runs the operation, and on failure computes an exponential backoff
(`base << attempt` — 100ms, 200ms, 400ms, ...). Two guards make it production-grade
rather than a naive loop. First, it enforces a *total elapsed budget*: before each
sleep it consults `Now` and, if the elapsed time plus the next backoff would exceed
`maxElapsed`, it stops with `ErrBudgetExceeded` rather than waiting past the
caller's tolerance — this is the real use of `Now`, and the fake clock's advancing
`Now` is what makes it testable. Second, it respects the context through `Sleep`.
When it runs out of attempts it returns `ErrExhausted` wrapping the last error.

Create `backoff.go`:

```go
package backoff

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrExhausted is returned when all attempts fail.
	ErrExhausted = errors.New("retries exhausted")
	// ErrBudgetExceeded is returned when the next backoff would exceed maxElapsed.
	ErrBudgetExceeded = errors.New("retry time budget exceeded")
)

// Clock is the time seam: production uses SystemClock, tests use a fake.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the production Clock backed by the time package.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Retrier retries an operation with exponential backoff under an injected Clock.
type Retrier struct {
	clock      Clock
	base       time.Duration
	maxTries   int
	maxElapsed time.Duration // 0 disables the budget check
}

// New injects the clock and policy. maxTries is clamped to at least 1.
func New(clock Clock, base time.Duration, maxTries int, maxElapsed time.Duration) *Retrier {
	if maxTries < 1 {
		maxTries = 1
	}
	return &Retrier{clock: clock, base: base, maxTries: maxTries, maxElapsed: maxElapsed}
}

// Do runs op, retrying on error with exponential backoff. It stops early if the
// elapsed-time budget would be exceeded or the context is cancelled.
func (r *Retrier) Do(ctx context.Context, op func() error) error {
	start := r.clock.Now()
	for attempt := 0; attempt < r.maxTries; attempt++ {
		err := op()
		if err == nil {
			return nil
		}
		if attempt == r.maxTries-1 {
			return fmt.Errorf("%w after %d attempts: %w", ErrExhausted, r.maxTries, err)
		}

		backoff := r.base << attempt
		if r.maxElapsed > 0 && r.clock.Now().Sub(start)+backoff > r.maxElapsed {
			return fmt.Errorf("%w: %w", ErrBudgetExceeded, err)
		}
		if serr := r.clock.Sleep(ctx, backoff); serr != nil {
			return serr
		}
	}
	return ErrExhausted
}
```

### The runnable demo

The demo uses `SystemClock` with a tiny 1ms base so it finishes instantly while
still exercising the real `time`-backed path: the operation fails twice, then
succeeds on the third try.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/backoff"
)

func main() {
	r := backoff.New(backoff.SystemClock{}, 1*time.Millisecond, 5, 0)

	attempts := 0
	err := r.Do(context.Background(), func() error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary failure")
		}
		return nil
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("succeeded after %d attempts\n", attempts)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
succeeded after 3 attempts
```

### The fake clock and the tests

`fakeClock` is the whole point: `Sleep` appends the requested duration to a slice,
advances a virtual `now` by that duration, and returns immediately — unless the
context is already cancelled, in which case it returns the context error, modeling
"the caller gave up during the wait." Because a test might one day drive the retrier
concurrently and the gate runs `-race`, the recorder is mutex-guarded.

`TestBackoffSequence` fails the operation three times then succeeds; it asserts the
recorded sequence is exactly `[100ms, 200ms, 400ms]`, that the operation ran four
times (N failures + 1 success), and that virtual `Now` advanced by 700ms — all
instantly. `TestExhausted` fails every time and asserts `ErrExhausted` after the
configured attempts. `TestBudgetExceeded` sets a small `maxElapsed` so the
`Now`-based budget guard trips before the second sleep, returning
`ErrBudgetExceeded`. `TestContextCancelledAbortsBackoff` cancels the context before
`Do`, so the first `Sleep` sees the cancelled context and aborts, proving the
context path with the operation having run exactly once.

Create `backoff_test.go`:

```go
package backoff

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock records sleeps and advances virtual time with no real waiting.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.slept = append(f.slept, d)
	f.now = f.now.Add(d)
	f.mu.Unlock()
	return nil
}

func (f *fakeClock) Slept() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Duration, len(f.slept))
	copy(out, f.slept)
	return out
}

func TestBackoffSequence(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	start := clk.Now()
	r := New(clk, 100*time.Millisecond, 5, 0)

	attempts := 0
	err := r.Do(context.Background(), func() error {
		attempts++
		if attempts <= 3 { // fail three times, then succeed
			return errors.New("temporary")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if attempts != 4 {
		t.Fatalf("attempts = %d, want 4", attempts)
	}

	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	got := clk.Slept()
	if len(got) != len(want) {
		t.Fatalf("slept = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("slept[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if elapsed := clk.Now().Sub(start); elapsed != 700*time.Millisecond {
		t.Fatalf("virtual elapsed = %v, want 700ms", elapsed)
	}
}

func TestExhausted(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	r := New(clk, 100*time.Millisecond, 3, 0)

	attempts := 0
	err := r.Do(context.Background(), func() error {
		attempts++
		return errors.New("always fails")
	})
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if got := clk.Slept(); len(got) != 2 {
		t.Fatalf("slept %d times, want 2 (between 3 attempts)", len(got))
	}
}

func TestBudgetExceeded(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	// base 100ms, budget 250ms: after the first 100ms sleep, the next backoff of
	// 200ms would push elapsed to 300ms > 250ms, so Do stops.
	r := New(clk, 100*time.Millisecond, 5, 250*time.Millisecond)

	err := r.Do(context.Background(), func() error {
		return errors.New("always fails")
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if got := clk.Slept(); len(got) != 1 {
		t.Fatalf("slept %d times, want 1 before budget trips", len(got))
	}
}

func TestContextCancelledAbortsBackoff(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	r := New(clk, 100*time.Millisecond, 5, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // caller gives up before the first backoff

	attempts := 0
	err := r.Do(ctx, func() error {
		attempts++
		return errors.New("fails")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 before abort", attempts)
	}
}
```

## Review

The retrier is correct when backoff is `base << attempt`, when the total-elapsed
budget stops it before a wait that would exceed `maxElapsed`, and when a cancelled
context aborts through `Sleep` rather than completing the wait. The injected `Clock`
is what makes every one of those assertions instant and deterministic: the fake
records the exact sleep sequence, advances virtual `Now` so the budget guard can be
exercised without real time, and returns the context error to prove the abort path.
The mistake this design exists to prevent is `time.Sleep` in a unit test — slow,
flaky, and untestable at the boundary. Note the fake clock is mutex-guarded and the
suite runs under `-race`. (Go 1.25's `testing/synctest` is an alternative that
virtualizes the real `time` package under unmodified code; the injected `Clock`
still matters for older toolchains and for controlling time in production.)

## Resources

- [time.NewTimer](https://pkg.go.dev/time#NewTimer) — the timer `SystemClock.Sleep` selects on.
- [context](https://pkg.go.dev/context) — `WithCancel` and the `ctx.Err()` the fake `Sleep` returns.
- [errors: wrapping with %w](https://pkg.go.dev/errors) — the double-wrapped `ErrExhausted`/`ErrBudgetExceeded` the tests match with `errors.Is`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-consumer-defined-interface-segregation.md](09-consumer-defined-interface-segregation.md)
