# Exercise 8: Retry Stage — Bounded Exponential Backoff On Transient Errors, Cancellable

A stage that calls a flaky downstream will occasionally get a 503 or a connection
reset. Those are *transient*: retrying after a short wait usually succeeds. Other
errors — a 400, a validation rejection — are *permanent*: retrying is pointless and
just burns time, so the stage should fail fast and cancel the pipeline. This module
builds a retry wrapper that distinguishes the two, applies bounded exponential
backoff with jitter between transient attempts, aborts the backoff sleep on
`ctx.Done()` instead of `time.Sleep`, and on a permanent failure sets the pipeline
`Cause`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
retry/                       module example.com/retry
  go.mod
  retry.go                   Do(ctx, op, policy); ErrPermanent; type Policy
  cmd/
    demo/
      main.go                a call that fails twice then succeeds; a permanent failure
  retry_test.go              succeeds on attempt k, permanent fails fast, cancel aborts backoff, exhaustion
```

Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
Implement: `Do(ctx, op, policy) error` — retry `op` on transient errors up to
`MaxAttempts` with exponential backoff and jitter, fail fast on a permanent error,
and abort the wait on `ctx.Done()`.
Test: an op that succeeds on attempt k is retried exactly k-1 times, a permanent
error fails without retry, a cancel during backoff returns within a small window,
exhaustion returns the last error, and jitter stays within `[base, base*factor)`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/12-multi-stage-pipeline-cancellation/08-retry-with-backoff-stage/cmd/demo
cd go-solutions/14-select-and-context/12-multi-stage-pipeline-cancellation/08-retry-with-backoff-stage
```

### Classify first, then wait cancellably

The retry loop is small but every line is a decision:

1. Call `op(ctx)`. If it returns `nil`, done.
2. Classify the error. If it wraps `ErrPermanent` (checked with `errors.Is`), return
   it immediately — no retry, no backoff. A permanent error retried is latency
   spent to reach the same failure.
3. If it is the last allowed attempt, return the error (exhaustion).
4. Otherwise compute the backoff for this attempt and wait — but *cancellably*.

The backoff is exponential: attempt `i` (0-based) waits roughly `base * factor^i`,
capped at `maxDelay`. Jitter spreads retries so a fleet of clients that all failed
at once do not synchronize their retries into a thundering herd; here the jitter is
"full jitter" bounded into `[d, d*jitterFactor)` using `math/rand/v2`. `rand/v2` is
the modern, per-call-safe RNG — no seeding, no global mutex contention.

The wait must not be `time.Sleep(d)`: a sleeping goroutine ignores a cancel and
delays shutdown by up to `maxDelay`. Instead the stage arms a `time.NewTimer(d)`
and selects over `timer.C` and `ctx.Done()`; if the context fires first it stops
the timer and returns `ctx.Err()`. That makes the retry stage as responsive to
cancellation as every other stage in the pipeline.

`Do` itself does not cancel the pipeline; it returns an error. The *caller* decides
what a terminal retry failure means — typically feeding it into a
`context.CancelCauseFunc` so the reason propagates as `Cause`. The demo shows that
wiring so the retry stage composes with the cause-carrying cancellation from
Exercise 1.

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

// ErrPermanent marks an error that must not be retried. Wrap a permanent failure
// with fmt.Errorf("%w: ...", ErrPermanent, ...) so Do fails fast on it.
var ErrPermanent = errors.New("retry: permanent error")

// Policy configures the retry loop.
type Policy struct {
	MaxAttempts  int           // total attempts, including the first
	Base         time.Duration // backoff for the first retry
	Factor       float64       // multiplier per attempt (e.g. 2.0)
	MaxDelay     time.Duration // cap on any single backoff
	JitterFactor float64       // backoff is in [d, d*JitterFactor); 1.0 disables jitter
}

// backoff returns the (pre-jitter) delay before the retry after attempt i (0-based).
func (p Policy) backoff(i int) time.Duration {
	d := float64(p.Base)
	for range i {
		d *= p.Factor
		if d >= float64(p.MaxDelay) {
			return p.MaxDelay
		}
	}
	if d >= float64(p.MaxDelay) {
		return p.MaxDelay
	}
	return time.Duration(d)
}

// jitter spreads d into [d, d*JitterFactor). A JitterFactor <= 1 returns d unchanged.
func (p Policy) jitter(d time.Duration) time.Duration {
	if p.JitterFactor <= 1 {
		return d
	}
	extra := float64(d) * (p.JitterFactor - 1)
	return d + time.Duration(rand.Float64()*extra)
}

// Do calls op until it succeeds, returns a permanent error, exhausts MaxAttempts,
// or ctx is cancelled. Transient errors trigger a cancellable exponential backoff.
func Do(ctx context.Context, op func(ctx context.Context) error, p Policy) error {
	var last error
	for attempt := range p.MaxAttempts {
		last = op(ctx)
		if last == nil {
			return nil
		}
		if errors.Is(last, ErrPermanent) {
			return last
		}
		if attempt == p.MaxAttempts-1 {
			break // exhausted; return last below
		}
		if err := wait(ctx, p.jitter(p.backoff(attempt))); err != nil {
			return err
		}
	}
	return fmt.Errorf("retry: exhausted %d attempts: %w", p.MaxAttempts, last)
}

// wait sleeps for d but returns ctx.Err() immediately if ctx is cancelled first.
func wait(ctx context.Context, d time.Duration) error {
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

The demo runs two operations: one that fails twice with a transient error then
succeeds (retried, ultimately OK), and one that returns a permanent error (fails
fast, no retry). The second is wired into a `CancelCauseFunc` to show the reason
propagating as `Cause`.

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
	policy := retry.Policy{
		MaxAttempts:  5,
		Base:         2 * time.Millisecond,
		Factor:       2,
		MaxDelay:     50 * time.Millisecond,
		JitterFactor: 1.5,
	}

	// Transient: fails twice, then succeeds.
	attempts := 0
	transient := func(ctx context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("503 service unavailable")
		}
		return nil
	}
	err := retry.Do(context.Background(), transient, policy)
	fmt.Printf("transient: attempts=%d err=%v\n", attempts, err)

	// Permanent: fails fast, reason propagates as Cause.
	ctx, cancel := context.WithCancelCause(context.Background())
	permanent := func(ctx context.Context) error {
		return fmt.Errorf("%w: 400 bad request", retry.ErrPermanent)
	}
	if err := retry.Do(ctx, permanent, policy); err != nil {
		cancel(err)
	}
	cause := context.Cause(ctx)
	fmt.Printf("permanent: is_ErrPermanent=%v\n", errors.Is(cause, retry.ErrPermanent))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
transient: attempts=3 err=<nil>
permanent: is_ErrPermanent=true
```

### Tests

`TestSucceedsOnAttemptK` asserts an op that succeeds on attempt k is called exactly
k times (k-1 retries). `TestPermanentFailsFast` asserts a permanent error is
returned after a single call. `TestCancelAbortsBackoff` cancels during a long
backoff and asserts `Do` returns within a small window rather than sleeping the
full delay. `TestExhaustionReturnsLastError` asserts the wrapped last error after
all attempts fail. `TestJitterBounds` asserts the jittered delay stays in range.

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

func fastPolicy(maxAttempts int) Policy {
	return Policy{
		MaxAttempts:  maxAttempts,
		Base:         time.Millisecond,
		Factor:       2,
		MaxDelay:     10 * time.Millisecond,
		JitterFactor: 1,
	}
}

func TestSucceedsOnAttemptK(t *testing.T) {
	t.Parallel()

	const k = 3
	calls := 0
	op := func(ctx context.Context) error {
		calls++
		if calls < k {
			return errors.New("transient")
		}
		return nil
	}
	if err := Do(context.Background(), op, fastPolicy(5)); err != nil {
		t.Fatalf("Do err = %v, want nil", err)
	}
	if calls != k {
		t.Fatalf("op called %d times, want %d (k-1 retries)", calls, k)
	}
}

func TestPermanentFailsFast(t *testing.T) {
	t.Parallel()

	calls := 0
	op := func(ctx context.Context) error {
		calls++
		return fmt.Errorf("%w: bad input", ErrPermanent)
	}
	err := Do(context.Background(), op, fastPolicy(5))
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("Do err = %v, want ErrPermanent", err)
	}
	if calls != 1 {
		t.Fatalf("op called %d times, want 1 (no retry on permanent)", calls)
	}
}

func TestCancelAbortsBackoff(t *testing.T) {
	t.Parallel()

	// A long backoff so the cancel must interrupt the wait, not the op.
	policy := Policy{
		MaxAttempts:  5,
		Base:         time.Second,
		Factor:       2,
		MaxDelay:     time.Second,
		JitterFactor: 1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	op := func(ctx context.Context) error { return errors.New("transient") }

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := Do(ctx, op, policy)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do err = %v, want context.Canceled", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Do took %v; backoff was not aborted by cancel", elapsed)
	}
}

func TestExhaustionReturnsLastError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("still failing")
	op := func(ctx context.Context) error { return sentinel }

	err := Do(context.Background(), op, fastPolicy(3))
	if !errors.Is(err, sentinel) {
		t.Fatalf("Do err = %v, want wrapped sentinel", err)
	}
}

func TestJitterBounds(t *testing.T) {
	t.Parallel()

	p := Policy{Base: 10 * time.Millisecond, Factor: 2, MaxDelay: time.Second, JitterFactor: 1.5}
	base := p.backoff(0) // 10ms
	for range 1000 {
		j := p.jitter(base)
		if j < base || j >= time.Duration(float64(base)*1.5) {
			t.Fatalf("jitter %v out of [%v, %v)", j, base, time.Duration(float64(base)*1.5))
		}
	}
}
```

## Review

The retry stage is correct when a transient failure is retried up to the bound, a
permanent error fails after exactly one call, backoff grows exponentially but is
capped and jittered within range, and a cancel during a wait returns promptly
rather than sleeping the full delay. The two mistakes that matter: using
`time.Sleep` for backoff (which `TestCancelAbortsBackoff` catches by timing out),
and retrying a permanent error (which `TestPermanentFailsFast` catches by call
count). `Do` returns the error rather than cancelling the pipeline itself, so it
composes with any cancellation strategy — the demo feeds it into `CancelCause` to
surface the reason as `Cause`.

## Resources

- [`math/rand/v2`](https://pkg.go.dev/math/rand/v2) — the modern RNG used for jitter, no seeding or global lock.
- [AWS Architecture Blog: Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — why full jitter beats fixed backoff.
- [`context.Cause`](https://pkg.go.dev/context#Cause) — surfacing a terminal retry failure as a typed pipeline reason.

---

Back to [07-rate-limited-egress-stage.md](07-rate-limited-egress-stage.md) | Next: [09-graceful-drain-on-shutdown.md](09-graceful-drain-on-shutdown.md)
