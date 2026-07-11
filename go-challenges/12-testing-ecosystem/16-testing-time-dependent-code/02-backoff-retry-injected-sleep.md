# Exercise 2: Exponential backoff retry with an injected sleeper

A retry helper with capped exponential backoff is a piece of code every backend
owns, and a piece that is miserable to test honestly: the whole point of backoff
is that it *waits longer each time*, so a faithful test would take seconds. The
fix is to inject the sleep as a function. Production sleeps on a real timer that
also watches the context; the test injects a recorder that returns instantly and
captures the exact schedule, so you assert the backoff sequence with zero waiting.

## What you'll build

```text
backoffretry/                  independent module: example.com/backoffretry
  go.mod
  retry.go                     Policy, Sleeper, RealSleeper, Retry(ctx, op, policy, sleep)
  cmd/
    demo/
      main.go                  retry a flaky op with a real sleeper; print the outcome
  retry_test.go                schedule assertion, success-after-N, exhaustion, ctx cancel
```

Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
Implement: `Retry` doing capped exponential backoff, sleeping through an injected `Sleeper func(context.Context, time.Duration) error`; a `RealSleeper` built on `time.NewTimer` + `ctx.Done`.
Test: inject a recording sleeper — assert the recorded delay schedule equals the expected capped-exponential sequence, that `Retry` returns after k failures, returns the last error when attempts exhaust, and returns `context.Canceled` when the injected sleeper is cancelled.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/backoffretry/cmd/demo
cd ~/go-exercises/backoffretry
go mod init example.com/backoffretry
```

### The injected Sleeper is the whole seam

The one time-dependent operation in a retry loop is the wait between attempts.
Instead of calling `time.Sleep` inline, `Retry` takes a `Sleeper`: a function
`func(context.Context, time.Duration) error`. It returns an error so the sleep can
be *cancelled* — the real implementation selects on `ctx.Done()` versus a timer,
returning `ctx.Err()` if the context is cancelled mid-wait. This is not just for
testing: a retry that ignores cancellation during its backoff will keep a
cancelled request alive for the full delay, which is a real production bug (a
client hangs up, but your service sleeps 8 more seconds before noticing). Making
the sleep context-aware fixes that *and* gives the test its seam for free.

`RealSleeper` uses `time.NewTimer` rather than `time.After` so it can `Stop` the
timer on the cancellation path; with Go 1.23+ semantics an un-stopped `time.After`
timer is GC-reclaimable, but stopping explicitly is still the tidy habit when you
hold the `*Timer`.

### The backoff schedule, and why the test asserts it exactly

The delay before retry number `n` (1-indexed among the *gaps*) is
`min(Base * 2^(n-1), Max)` — classic capped exponential. With `Base = 100ms` and
`Max = 1s`, the gap sequence is `100ms, 200ms, 400ms, 800ms, 1s, 1s, ...`. A test
that only checked "it retried a few times" would let a bug in the cap or the
exponent slip through. Because the sleeper is injected, the test records every
requested duration and asserts the *whole* slice equals the expected sequence —
the cap at `Max` and the doubling are both pinned.

`Retry` calls the operation up to `MaxAttempts` times. It sleeps *between*
attempts, not after the last one (sleeping after the final failure just wastes
time), so `N` attempts produce `N-1` sleeps. On success it returns `nil`; on
exhaustion it returns the last operation error; if a sleep is cancelled it returns
that cancellation error immediately.

Create `retry.go`:

```go
package backoffretry

import (
	"context"
	"time"
)

// Sleeper waits for d or until ctx is cancelled, returning ctx.Err() if
// cancelled first. Injecting it makes the backoff schedule testable.
type Sleeper func(ctx context.Context, d time.Duration) error

// RealSleeper waits on a real timer while watching ctx.
func RealSleeper(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Policy configures capped exponential backoff.
type Policy struct {
	Base        time.Duration
	Max         time.Duration
	MaxAttempts int
}

// backoff returns the delay before the nth retry gap (n starts at 1):
// min(Base * 2^(n-1), Max).
func (p Policy) backoff(n int) time.Duration {
	d := p.Base
	for i := 1; i < n; i++ {
		d *= 2
		if d >= p.Max {
			return p.Max
		}
	}
	if d > p.Max {
		return p.Max
	}
	return d
}

// Retry runs op up to p.MaxAttempts times, sleeping between attempts through
// sleep. It returns nil on the first success, the last error if all attempts
// fail, or the sleeper's error if a backoff sleep is cancelled.
func Retry(ctx context.Context, op func() error, p Policy, sleep Sleeper) error {
	var err error
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		if err = op(); err == nil {
			return nil
		}
		if attempt == p.MaxAttempts {
			break
		}
		if serr := sleep(ctx, p.backoff(attempt)); serr != nil {
			return serr
		}
	}
	return err
}
```

### The runnable demo

The demo retries an operation that fails twice then succeeds, using
`RealSleeper` with a small base so the run is quick. It prints each attempt and
the final outcome.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/backoffretry"
)

func main() {
	attempts := 0
	op := func() error {
		attempts++
		fmt.Printf("attempt %d\n", attempts)
		if attempts < 3 {
			return errors.New("temporary failure")
		}
		return nil
	}

	p := backoffretry.Policy{Base: 10 * time.Millisecond, Max: 100 * time.Millisecond, MaxAttempts: 5}
	err := backoffretry.Retry(context.Background(), op, p, backoffretry.RealSleeper)
	if err != nil {
		fmt.Printf("failed: %v\n", err)
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
attempt 1
attempt 2
attempt 3
succeeded after 3 attempts
```

### Tests

The tests inject a fake sleeper that records each requested duration and returns
immediately, so the whole suite runs in microseconds. `TestBackoffSchedule`
forces the op to always fail and asserts the recorded delays equal the exact
capped-exponential sequence. `TestSucceedsAfterFailures` proves `Retry` stops at
the first success. `TestExhaustsAndReturnsLastError` proves the last op error
propagates via `errors.Is`. `TestSleeperCancellation` scripts the sleeper to
return `context.Canceled` and asserts `Retry` surfaces it.

Create `retry_test.go`:

```go
package backoffretry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

var errFlaky = errors.New("flaky")

// recorder is an injectable Sleeper that records durations and never waits.
type recorder struct {
	delays []time.Duration
}

func (r *recorder) sleep(_ context.Context, d time.Duration) error {
	r.delays = append(r.delays, d)
	return nil
}

func TestBackoffSchedule(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	p := Policy{Base: 100 * time.Millisecond, Max: time.Second, MaxAttempts: 6}
	err := Retry(context.Background(), func() error { return errFlaky }, p, rec.sleep)
	if !errors.Is(err, errFlaky) {
		t.Fatalf("Retry err = %v, want errFlaky", err)
	}
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		time.Second, // capped
	}
	if len(rec.delays) != len(want) {
		t.Fatalf("recorded %d delays, want %d: %v", len(rec.delays), len(want), rec.delays)
	}
	for i, d := range want {
		if rec.delays[i] != d {
			t.Fatalf("delay[%d] = %v, want %v (schedule %v)", i, rec.delays[i], d, rec.delays)
		}
	}
}

func TestSucceedsAfterFailures(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	calls := 0
	op := func() error {
		calls++
		if calls < 3 {
			return errFlaky
		}
		return nil
	}
	p := Policy{Base: time.Second, Max: 30 * time.Second, MaxAttempts: 5}
	if err := Retry(context.Background(), op, p, rec.sleep); err != nil {
		t.Fatalf("Retry err = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want 3", calls)
	}
	if len(rec.delays) != 2 {
		t.Fatalf("slept %d times, want 2", len(rec.delays))
	}
}

func TestExhaustsAndReturnsLastError(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	p := Policy{Base: time.Second, Max: time.Minute, MaxAttempts: 3}
	err := Retry(context.Background(), func() error { return errFlaky }, p, rec.sleep)
	if !errors.Is(err, errFlaky) {
		t.Fatalf("Retry err = %v, want errFlaky", err)
	}
	if len(rec.delays) != 2 {
		t.Fatalf("slept %d times for 3 attempts, want 2", len(rec.delays))
	}
}

func TestSleeperCancellation(t *testing.T) {
	t.Parallel()
	// A sleeper that reports cancellation on its first call, as a real
	// context-aware sleeper would after the caller cancels.
	cancelSleep := func(_ context.Context, _ time.Duration) error {
		return context.Canceled
	}
	p := Policy{Base: time.Second, Max: time.Minute, MaxAttempts: 5}
	err := Retry(context.Background(), func() error { return errFlaky }, p, cancelSleep)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Retry err = %v, want context.Canceled", err)
	}
}

func TestRealSleeperRespectsCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RealSleeper(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("RealSleeper err = %v, want context.Canceled", err)
	}
}

func ExamplePolicy_backoff() {
	p := Policy{Base: 100 * time.Millisecond, Max: time.Second, MaxAttempts: 6}
	for n := 1; n <= 6; n++ {
		fmt.Print(p.backoff(n), " ")
	}
	fmt.Println()
	// Output: 100ms 200ms 400ms 800ms 1s 1s
}
```

## Review

The retry is correct when the injected sleeper sees exactly the capped-exponential
schedule and `Retry` stops on the first success, propagates the last error on
exhaustion, and short-circuits when a sleep is cancelled. The demo cannot prove
any of that — it just prints — which is precisely why the schedule assertion lives
in a test with a recording sleeper. The trap to avoid is sleeping *after* the last
attempt (wasted delay and an off-by-one in the recorded slice); `Retry` breaks
before the final sleep, so `N` attempts record `N-1` delays. The second trap is a
backoff sleep that ignores cancellation; `RealSleeper` selects on `ctx.Done()` so
a cancelled request stops waiting immediately, which `TestRealSleeperRespectsCancel`
pins.

## Resources

- [`time.NewTimer`](https://pkg.go.dev/time#NewTimer) and [`Timer.Stop`](https://pkg.go.dev/time#Timer.Stop) — the timer the real sleeper stops on cancel.
- [`context` package](https://pkg.go.dev/context) — `Context`, `WithCancel`, cancellation semantics.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching wrapped sentinel errors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-injectable-clock-scheduler.md](01-injectable-clock-scheduler.md) | Next: [03-token-bucket-rate-limiter.md](03-token-bucket-rate-limiter.md)
