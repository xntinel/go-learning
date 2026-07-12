# Exercise 1: The Retry Client — Exponential Backoff, Jitter, and a Bounded Budget

This is the foundational artifact the rest of the lesson builds on: a service
client that retries a transient operation with exponential backoff and jitter,
stops immediately on a permanent error, honors context cancellation, and bounds
its attempts. It reads the two rules that matter most — retry only transient
errors, and a retry is never free — into a single small `Do` loop.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
errretry/                  independent module: example.com/errretry
  go.mod                   go 1.26
  errretry.go              ErrTransient/ErrPermanent, Policy, DefaultPolicy,
                           IsRetryable, Policy.Backoff, Client, Do
  cmd/
    demo/
      main.go              runnable demo: retries then succeeds
  errretry_test.go         table tests: success, retry, exhaust, permanent,
                           cancellation, backoff growth, clamp, jitter bounds
```

Files: `errretry.go`, `cmd/demo/main.go`, `errretry_test.go`.
Implement: `Policy` with `Backoff(attempt)` (exponential growth + `MaxDelay` clamp + symmetric jitter) and `Client.Do(ctx, op)` that classifies, sleeps between attempts, honors `ctx.Done()`, and returns the last error after `MaxAttempts`.
Test: success-on-first, retry-then-succeed, last-error-after-max, no-retry-on-permanent, context-cancelled, exact `Backoff` values, clamp-to-max, and jitter-within-bounds.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/12-retry-patterns-with-backoff/01-backoff-retry-client/cmd/demo
cd go-solutions/10-error-handling/12-retry-patterns-with-backoff/01-backoff-retry-client
go mod edit -go=1.26
```

### The design: a Policy that computes delays, a Client that drives the loop

The type is split into two pieces on purpose. `Policy` is *pure configuration plus
a pure-ish function*: given an attempt number, `Backoff` returns a delay. It has no
loop, no clock, no side effects except reading the RNG for jitter, which makes it
trivial to unit-test the delay curve in isolation. `Client` owns the *loop*: call
the operation, classify the result, and either return or sleep-and-retry.

`Backoff(attempt)` computes `BaseDelay · Factor^attempt`, clamps it to `MaxDelay`,
then applies *symmetric* jitter. Symmetric jitter means the final delay is drawn
uniformly from `[d·(1−J), d·(1+J)]` for a jitter fraction `J` — centered on the
computed delay, spreading `±J·d` around it. The construction is
`d − spread + rand·2·spread` where `spread = d·J` and `rand ∈ [0,1)`: at `rand=0`
you get `d−spread`, at `rand→1` you approach `d+spread`. Clamping happens *before*
jitter so the center is bounded; the jittered value can exceed `MaxDelay` by up to
`J·MaxDelay`, which is intentional and harmless (later exercises explore
jitter strategies that clamp differently).

`Do` runs the classic loop. On each attempt it calls `op(ctx)`. `nil` means
success — return immediately. A non-`nil` error is stashed as `lastErr` and
classified: if it is *not* retryable (`IsRetryable` returns false), return it now,
without retrying — this is the guardrail that stops a permanent error or a
programmer bug from being amplified. If it is retryable and this was the last
allowed attempt, break out and return `lastErr`. Otherwise compute the backoff and
sleep — but sleep inside a `select` that also watches `ctx.Done()`, so a cancelled
or timed-out context aborts the wait and returns `ctx.Err()` instead of blocking
for the full delay. That `select` is the difference between a retry loop that
respects deadlines and one that ignores them.

`IsRetryable` here is deliberately minimal: it matches the `ErrTransient` sentinel
via `errors.Is`, so any error wrapped with `%w` around `ErrTransient` is retryable
and everything else (including `ErrPermanent` and `context.Canceled`) is not.
Exercise 2 replaces this with a production classifier that understands timeouts and
HTTP status codes; keeping it a single `errors.Is` here keeps the focus on the loop.

Create `errretry.go`:

```go
package errretry

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

// Sentinels the classifier matches. A real operation wraps one of these with %w.
var (
	ErrTransient = errors.New("transient error")
	ErrPermanent = errors.New("permanent error")
)

// Op is a retryable unit of work. It must honor ctx cancellation.
type Op func(ctx context.Context) error

// Policy configures the backoff curve and the attempt budget.
type Policy struct {
	MaxAttempts int           // total tries, including the first
	BaseDelay   time.Duration // delay before the first retry
	MaxDelay    time.Duration // clamp on the computed (pre-jitter) delay
	Factor      float64       // growth factor per attempt (>= 1)
	Jitter      float64       // symmetric jitter fraction in [0,1)
}

// DefaultPolicy is a reasonable starting point for a service client.
func DefaultPolicy() Policy {
	return Policy{
		MaxAttempts: 5,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    2 * time.Second,
		Factor:      2.0,
		Jitter:      0.2,
	}
}

// IsRetryable reports whether err is a transient failure worth retrying.
func IsRetryable(err error) bool {
	return errors.Is(err, ErrTransient)
}

// Backoff returns the delay before the given retry attempt: BaseDelay*Factor^attempt,
// clamped to MaxDelay, then spread by symmetric jitter of +/- Jitter.
func (p Policy) Backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := float64(p.BaseDelay)
	for range attempt {
		d *= p.Factor
	}
	if d > float64(p.MaxDelay) {
		d = float64(p.MaxDelay)
	}
	if p.Jitter > 0 {
		spread := d * p.Jitter
		d = d - spread + rand.Float64()*2*spread
	}
	return time.Duration(d)
}

// Client retries an Op according to its Policy.
type Client struct {
	Policy Policy
}

// New returns a Client with the given policy.
func New(p Policy) *Client {
	return &Client{Policy: p}
}

// Do runs op up to MaxAttempts times, sleeping Backoff(attempt) between tries.
// It returns nil on success, the error immediately on a permanent failure, the
// context error if ctx is cancelled during a wait, or the last error after the
// budget is exhausted.
func (c *Client) Do(ctx context.Context, op Op) error {
	var lastErr error
	for attempt := range c.Policy.MaxAttempts {
		err := op(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if !IsRetryable(err) {
			return err
		}
		if attempt == c.Policy.MaxAttempts-1 {
			break
		}
		delay := c.Policy.Backoff(attempt)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return lastErr
}
```

### The runnable demo

The demo drives a flaky operation that fails transiently twice and then succeeds,
using a small base delay so the run is quick, and prints which attempt won.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/errretry"
)

func main() {
	c := errretry.New(errretry.Policy{
		MaxAttempts: 5,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    100 * time.Millisecond,
		Factor:      2.0,
		Jitter:      0,
	})

	attempt := 0
	err := c.Do(context.Background(), func(ctx context.Context) error {
		attempt++
		if attempt < 3 {
			fmt.Printf("attempt %d: transient failure\n", attempt)
			return fmt.Errorf("dial upstream: %w", errretry.ErrTransient)
		}
		fmt.Printf("attempt %d: success\n", attempt)
		return nil
	})
	if err != nil {
		fmt.Println("give up:", err)
		return
	}
	fmt.Println("done")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempt 1: transient failure
attempt 2: transient failure
attempt 3: success
done
```

### Tests

The tests cover the whole contract. `Do` behavior is asserted through call counts
and the returned error; `Backoff` is asserted with `Jitter: 0` for exact values and
with `Jitter: 0.2` sampled 100 times for the bounds. All use tiny delays so the
suite is fast even though a couple of them do sleep for real.

Create `errretry_test.go`:

```go
package errretry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func fastPolicy(maxAttempts int) Policy {
	return Policy{
		MaxAttempts: maxAttempts,
		BaseDelay:   time.Millisecond,
		MaxDelay:    5 * time.Millisecond,
		Factor:      2.0,
		Jitter:      0,
	}
}

func TestDoSucceedsOnFirstAttempt(t *testing.T) {
	t.Parallel()
	c := New(DefaultPolicy())
	calls := 0
	err := c.Do(context.Background(), func(ctx context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestDoRetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	c := New(fastPolicy(3))
	calls := 0
	err := c.Do(context.Background(), func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return fmt.Errorf("blip: %w", ErrTransient)
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

func TestDoReturnsLastErrorAfterMax(t *testing.T) {
	t.Parallel()
	c := New(fastPolicy(3))
	calls := 0
	err := c.Do(context.Background(), func(ctx context.Context) error {
		calls++
		return fmt.Errorf("try %d: %w", calls, ErrTransient)
	})
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("err = %v, want wrapped ErrTransient", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestDoDoesNotRetryOnPermanent(t *testing.T) {
	t.Parallel()
	c := New(fastPolicy(5))
	calls := 0
	err := c.Do(context.Background(), func(ctx context.Context) error {
		calls++
		return fmt.Errorf("bad request: %w", ErrPermanent)
	})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on permanent)", calls)
	}
}

func TestDoRespectsContextCancellation(t *testing.T) {
	t.Parallel()
	c := New(Policy{
		MaxAttempts: 5,
		BaseDelay:   time.Hour, // long, so the select must exit via ctx.Done
		MaxDelay:    time.Hour,
		Factor:      2.0,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.Do(ctx, func(ctx context.Context) error {
		return ErrTransient
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestBackoffGrowsExponentially(t *testing.T) {
	t.Parallel()
	p := Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: 10 * time.Second, Factor: 2.0, Jitter: 0}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 100 * time.Millisecond},
		{1, 200 * time.Millisecond},
		{2, 400 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := p.Backoff(tc.attempt); got != tc.want {
			t.Errorf("Backoff(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestBackoffClampsToMax(t *testing.T) {
	t.Parallel()
	p := Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: 250 * time.Millisecond, Factor: 2.0, Jitter: 0}
	if got := p.Backoff(5); got != 250*time.Millisecond {
		t.Fatalf("Backoff(5) = %v, want 250ms (clamped)", got)
	}
}

func TestBackoffJitterStaysWithinBounds(t *testing.T) {
	t.Parallel()
	p := Policy{BaseDelay: 100 * time.Millisecond, MaxDelay: 10 * time.Second, Factor: 2.0, Jitter: 0.2}
	center := 400 * time.Millisecond // Backoff(2) with no jitter
	lo := time.Duration(float64(center) * 0.8)
	hi := time.Duration(float64(center) * 1.2)
	for i := range 100 {
		d := p.Backoff(2)
		if d < lo || d > hi {
			t.Fatalf("sample %d: Backoff(2)=%v outside [%v,%v]", i, d, lo, hi)
		}
	}
}

func Example() {
	p := DefaultPolicy()
	p.Jitter = 0 // deterministic delay for the printed output
	fmt.Println(p.Backoff(0))
	fmt.Println(IsRetryable(fmt.Errorf("wrap: %w", ErrTransient)))
	fmt.Println(IsRetryable(ErrPermanent))
	// Output:
	// 100ms
	// true
	// false
}
```

The `Example` zeroes `Jitter` before printing `Backoff(0)`: `DefaultPolicy` uses
`Jitter: 0.2`, which would make the delay vary run to run and break the
`// Output:` match. Zeroing it pins the printed delay at exactly `BaseDelay`.

## Review

The client is correct when `Do` returns `nil` exactly on success, the error
unchanged on a permanent failure (call count frozen at 1), `ctx.Err()` when the
context is cancelled during a wait, and the wrapped last error after the budget is
spent — and when `Backoff` produces `base·factor^n` clamped to `MaxDelay`, jittered
within `±J`. The three structural mistakes to avoid: retrying on every error
(classify with `IsRetryable` first), sleeping without watching `ctx.Done()` (the
`select` is mandatory), and returning a generic error after exhaustion instead of
the wrapped `lastErr` (which would break `errors.Is` upstream). Run
`go test -race` to confirm there is no data race in the jitter RNG — the top-level
`math/rand/v2` functions are safe for concurrent use, which is why the `Backoff`
sampling test can run in parallel with everything else.

## Resources

- [AWS Architecture Blog: Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — the measurements that justify jitter.
- [`math/rand/v2`](https://pkg.go.dev/math/rand/v2) — the concurrency-safe top-level `Float64` used for jitter.
- [`context`](https://pkg.go.dev/context) — cancellation the `Do` loop honors.
- [`time#After`](https://pkg.go.dev/time#After) — the timer the retry wait selects on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-error-classification-for-retries.md](02-error-classification-for-retries.md)
