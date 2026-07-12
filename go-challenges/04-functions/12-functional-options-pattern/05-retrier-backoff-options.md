# Exercise 5: Exponential-Backoff Retrier with Injected Clock and Randomness

A retrier that sleeps with capped exponential backoff and random jitter is exactly
the kind of code that is miserable to test: its behavior depends on wall-clock
sleeps and a random number generator. This module makes it fully testable by
injecting both — `WithSleep` replaces the real sleep with one that records
durations, and `WithRand` seeds jitter deterministically. This is the single
highest-value use of the options pattern.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
retrier/                         independent module: example.com/retrier
  go.mod                         go 1.26
  retrier.go                     Retrier, Option, New, Do, WithMaxAttempts, WithBaseDelay,
                                 WithMaxDelay, WithMultiplier, WithJitter, WithSleep, WithRand
  cmd/
    demo/
      main.go                    retries a flaky operation that succeeds on attempt 3
  retrier_test.go                asserts the recorded backoff sequence, jitter reproducibility, ctx cancel
```

- Files: `retrier.go`, `cmd/demo/main.go`, `retrier_test.go`.
- Implement: `Do(ctx, fn)` retrying `fn` with capped exponential backoff, optional jitter, and context cancellation between attempts, using an injectable sleep and RNG.
- Test: inject a recording sleep and assert the delays match the expected capped-exponential sequence; seed the RNG and assert jitter is reproducible and never exceeds `maxDelay`; cancel the context mid-retry and assert a prompt joined error.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/12-functional-options-pattern/05-retrier-backoff-options/cmd/demo
cd go-solutions/04-functions/12-functional-options-pattern/05-retrier-backoff-options
```

### Injecting time and randomness

In production the retrier sleeps real durations and jitters with a randomly seeded
`math/rand/v2` generator. Both are hostile to tests: a real sleep makes the test
slow and a random generator makes it non-reproducible. The fix is to make each of
them a defaulted, injectable collaborator:

- `WithSleep(func(context.Context, time.Duration) error)` replaces the sleep. The
  default sleeps on a `time.Timer` and honors `ctx.Done`. A test injects a
  function that appends the requested duration to a slice and returns immediately,
  so the "backoff" is recorded rather than waited.
- `WithRand(*rand.Rand)` replaces the RNG. The default is auto-seeded; a test
  passes `rand.New(rand.NewPCG(seed1, seed2))` with fixed seeds so the jitter is
  reproducible.

Neither collaborator is a global. Production passes nothing and gets real time and
real randomness; the test passes options and gets a deterministic, instant run.
The retrier's logic — the backoff math, the cap, the cancellation — is identical
in both cases, so the test exercises the real code path.

### The backoff math

The delay before attempt `n` (attempts are 1-based, and attempt 1 has no
preceding sleep) is `baseDelay * multiplier^(n-2)`, capped at `maxDelay`. The cap
is applied progressively so that once the delay reaches `maxDelay` it stays there.
With `baseDelay = 100ms`, `multiplier = 2`, and `maxDelay = 1s`, the sleeps before
attempts 2..6 are `100ms, 200ms, 400ms, 800ms, 1s` (the last capped down from
1.6s). When jitter is enabled, the actual sleep is a uniform random draw in
`[0, delay)`, which can never exceed `maxDelay`. Full jitter like this is the
AWS-recommended approach because it spreads retries out and avoids a thundering
herd of clients retrying in lockstep.

Create `retrier.go`:

```go
package retrier

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// SleepFunc pauses for d or returns early if ctx is cancelled.
type SleepFunc func(ctx context.Context, d time.Duration) error

// Retrier retries an operation with capped exponential backoff.
type Retrier struct {
	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration
	multiplier  float64
	jitter      bool
	sleep       SleepFunc
	rng         *rand.Rand
}

// Option configures a Retrier and may reject invalid input.
type Option func(*Retrier) error

func defaultSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// New builds a Retrier, seeding defaults and applying opts.
func New(opts ...Option) (*Retrier, error) {
	r := &Retrier{
		maxAttempts: 3,
		baseDelay:   100 * time.Millisecond,
		maxDelay:    30 * time.Second,
		multiplier:  2,
		jitter:      true,
		sleep:       defaultSleep,
		rng:         rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
	}

	for _, opt := range opts {
		if err := opt(r); err != nil {
			return nil, err
		}
	}

	if r.maxDelay < r.baseDelay {
		return nil, fmt.Errorf("maxDelay (%s) must be >= baseDelay (%s)", r.maxDelay, r.baseDelay)
	}
	return r, nil
}

// delayBeforeAttempt returns the base (un-jittered) delay before a 1-based
// attempt number. Attempt 1 has no preceding delay.
func (r *Retrier) delayBeforeAttempt(attempt int) time.Duration {
	d := float64(r.baseDelay)
	for i := 2; i < attempt; i++ {
		d *= r.multiplier
		if d > float64(r.maxDelay) {
			d = float64(r.maxDelay)
		}
	}
	return time.Duration(d)
}

// Do runs fn, retrying up to maxAttempts with backoff between attempts. It
// returns nil on the first success, or the joined errors of every failure.
func (r *Retrier) Do(ctx context.Context, fn func() error) error {
	if fn == nil {
		return fmt.Errorf("fn is nil")
	}

	var errs []error
	for attempt := 1; attempt <= r.maxAttempts; attempt++ {
		if attempt > 1 {
			d := r.delayBeforeAttempt(attempt)
			if r.jitter {
				d = time.Duration(r.rng.Float64() * float64(d))
			}
			if err := r.sleep(ctx, d); err != nil {
				return errors.Join(append(errs, err)...)
			}
		}

		err := fn()
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// WithMaxAttempts sets the maximum number of attempts (>= 1).
func WithMaxAttempts(n int) Option {
	return func(r *Retrier) error {
		if n < 1 {
			return fmt.Errorf("maxAttempts must be >= 1, got %d", n)
		}
		r.maxAttempts = n
		return nil
	}
}

// WithBaseDelay sets the first backoff delay (> 0).
func WithBaseDelay(d time.Duration) Option {
	return func(r *Retrier) error {
		if d <= 0 {
			return fmt.Errorf("baseDelay must be positive, got %s", d)
		}
		r.baseDelay = d
		return nil
	}
}

// WithMaxDelay caps the backoff delay (> 0).
func WithMaxDelay(d time.Duration) Option {
	return func(r *Retrier) error {
		if d <= 0 {
			return fmt.Errorf("maxDelay must be positive, got %s", d)
		}
		r.maxDelay = d
		return nil
	}
}

// WithMultiplier sets the exponential growth factor (>= 1).
func WithMultiplier(m float64) Option {
	return func(r *Retrier) error {
		if m < 1 {
			return fmt.Errorf("multiplier must be >= 1, got %g", m)
		}
		r.multiplier = m
		return nil
	}
}

// WithJitter enables or disables full jitter.
func WithJitter(enabled bool) Option {
	return func(r *Retrier) error {
		r.jitter = enabled
		return nil
	}
}

// WithSleep injects the sleep function (for tests). Nil is rejected.
func WithSleep(fn SleepFunc) Option {
	return func(r *Retrier) error {
		if fn == nil {
			return fmt.Errorf("sleep function is nil")
		}
		r.sleep = fn
		return nil
	}
}

// WithRand injects the RNG used for jitter (for tests). Nil is rejected.
func WithRand(rng *rand.Rand) Option {
	return func(r *Retrier) error {
		if rng == nil {
			return fmt.Errorf("rand source is nil")
		}
		r.rng = rng
		return nil
	}
}
```

### The runnable demo

The demo retries an operation that fails twice and succeeds on the third attempt,
using tiny real delays and jitter off so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/retrier"
)

func main() {
	r, err := retrier.New(
		retrier.WithMaxAttempts(5),
		retrier.WithBaseDelay(time.Millisecond),
		retrier.WithJitter(false),
	)
	if err != nil {
		panic(err)
	}

	attempt := 0
	err = r.Do(context.Background(), func() error {
		attempt++
		if attempt < 3 {
			fmt.Printf("attempt %d failed\n", attempt)
			return fmt.Errorf("temporary failure")
		}
		fmt.Printf("succeeded on attempt %d\n", attempt)
		return nil
	})
	if err != nil {
		panic(err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempt 1 failed
attempt 2 failed
succeeded on attempt 3
```

### Tests

`TestBackoffSequence` injects a recording sleep and, with jitter off, asserts the
recorded delays are exactly the capped-exponential sequence. `TestJitterReproducible`
seeds two retriers identically and asserts they produce the same jittered delays,
each bounded by `maxDelay`. `TestContextCancelStopsRetry` injects a sleep that
reports cancellation and asserts `Do` returns promptly with a joined error that
`errors.Is` finds `context.Canceled`.

Create `retrier_test.go`:

```go
package retrier

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"
	"time"
)

var errFlaky = errors.New("flaky")

func TestBackoffSequence(t *testing.T) {
	t.Parallel()

	var delays []time.Duration
	record := func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}

	r, err := New(
		WithMaxAttempts(6),
		WithBaseDelay(100*time.Millisecond),
		WithMaxDelay(time.Second),
		WithMultiplier(2),
		WithJitter(false),
		WithSleep(record),
	)
	if err != nil {
		t.Fatal(err)
	}

	got := r.Do(context.Background(), func() error { return errFlaky })
	if !errors.Is(got, errFlaky) {
		t.Fatalf("Do error = %v, want errors.Is errFlaky", got)
	}

	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		time.Second, // capped from 1.6s
	}
	if len(delays) != len(want) {
		t.Fatalf("recorded %d delays, want %d: %v", len(delays), len(want), delays)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("delay[%d] = %s, want %s", i, delays[i], want[i])
		}
	}
}

func TestJitterReproducible(t *testing.T) {
	t.Parallel()

	run := func() []time.Duration {
		var delays []time.Duration
		record := func(ctx context.Context, d time.Duration) error {
			delays = append(delays, d)
			return nil
		}
		r, err := New(
			WithMaxAttempts(5),
			WithBaseDelay(100*time.Millisecond),
			WithMaxDelay(time.Second),
			WithJitter(true),
			WithRand(rand.New(rand.NewPCG(42, 99))),
			WithSleep(record),
		)
		if err != nil {
			t.Fatal(err)
		}
		r.Do(context.Background(), func() error { return errFlaky })
		return delays
	}

	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatalf("different lengths: %v vs %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("jitter not reproducible at %d: %s vs %s", i, a[i], b[i])
		}
		if a[i] > time.Second {
			t.Fatalf("jittered delay %s exceeds maxDelay", a[i])
		}
	}
}

func TestContextCancelStopsRetry(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancelSleep := func(c context.Context, d time.Duration) error {
		cancel()
		return context.Canceled
	}

	r, err := New(
		WithMaxAttempts(10),
		WithJitter(false),
		WithSleep(cancelSleep),
	)
	if err != nil {
		t.Fatal(err)
	}

	got := r.Do(ctx, func() error { return errFlaky })
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("Do error = %v, want errors.Is context.Canceled", got)
	}
	if !errors.Is(got, errFlaky) {
		t.Fatalf("Do error = %v, want joined errFlaky too", got)
	}
}
```

## Review

The retrier is correct when its backoff is a pure function of the attempt number,
the multiplier, and the cap — `TestBackoffSequence` pins that exactly by recording
the delays the injected sleep is asked for. The value of `WithSleep` and
`WithRand` is that they make a time- and randomness-dependent component
deterministic without a global clock or a global RNG: the same code that sleeps
real seconds in production records fake durations in the test. `errors.Join`
preserves every attempt's failure plus any cancellation error, so a caller can
`errors.Is` the specific cause out of the aggregate — which
`TestContextCancelStopsRetry` relies on to find `context.Canceled` inside the
joined result.

## Resources

- [math/rand/v2 package](https://pkg.go.dev/math/rand/v2)
- [errors.Join](https://pkg.go.dev/errors#Join)
- [AWS: Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/)
- [context package](https://pkg.go.dev/context)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-http-server-timeouts-options.md](04-http-server-timeouts-options.md) | Next: [06-clock-injection-cache-options.md](06-clock-injection-cache-options.md)
