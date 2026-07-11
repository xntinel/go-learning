# Exercise 7: Retry With Backoff Driven By An Injected Clock And Sleeper

The canonical reason to inject time is a retry loop. A backoff that calls
`time.Sleep` directly cannot be tested without actually waiting seconds, so its
schedule goes unverified and the suite is slow. Here you build a `Retrier` whose
waits go through an injected `Sleeper` interface instead of `time.Sleep`, so a fake
records the backoff schedule without sleeping and the whole loop is asserted in
microseconds.

This module is fully self-contained, with its own `go mod init`, code, demo, and
tests.

## What you'll build

```text
retry/                      independent module: example.com/retry
  go.mod                    module example.com/retry
  retry.go                  Sleeper interface; realSleeper; Retrier; Do with exponential backoff
  cmd/
    demo/
      main.go               retries a flaky operation with the real sleeper
  retry_test.go             fakeSleeper records the schedule; success/early-stop/max-attempts/cancel tests
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `Sleeper` (`Sleep(ctx, d) error`); a `realSleeper` backed by `time.NewTimer` that respects context cancellation; a `Retrier` with base delay, factor, max attempts, and an injected `Sleeper`; `Do(ctx, op)` implementing exponential backoff with a max-attempts cap and context cancellation.
- Test: a `fakeSleeper` that records requested durations without sleeping; assert the backoff schedule (100ms, 200ms, 400ms), that success stops early, that exhausting attempts returns the last error, and that a cancelled context aborts mid-retry with zero real elapsed time.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/retry/cmd/demo
cd ~/go-exercises/retry
go mod init example.com/retry
```

### Why the wait is a dependency

A retry's waits are the one part that a test must not actually perform. The trick
is to make the wait an injected collaborator. `Sleeper` has a single method,
`Sleep(ctx, d) error`, which returns nil when the delay elapses and the context's
error when it is cancelled first. In production the `realSleeper` implements this
with a `time.NewTimer` and a `select` on the timer and `ctx.Done()`, so a
cancellation aborts the wait immediately rather than blocking for the full delay.
In tests the `fakeSleeper` implements the same method by appending the requested
duration to a slice and returning at once — it records the schedule the retrier
asked for without any real time passing. The `Sleeper` interface is the injected
clock: it is where "wait this long" attaches to either real time or a recording
fake.

`Do` runs the operation, and on failure computes the next delay as
`base * factor^n`, capped by a maximum number of attempts. The loop structure
matters: the operation runs first with no delay, and a sleep only precedes each
*retry*. So with base 100ms, factor 2, and four attempts, the operation runs at t=0,
then the retrier sleeps 100ms, 200ms, 400ms before the second, third, and fourth
attempts — three sleeps for four attempts. If any attempt succeeds, `Do` returns
immediately and the later sleeps never happen; if all attempts fail, `Do` returns
the last error unchanged so the caller sees the real failure, not a generic
"exhausted" message. And crucially, the sleep is the cancellation point: if the
context is cancelled while waiting, `Sleep` returns `ctx.Err()`, `Do` propagates it,
and the retry aborts without a further attempt.

Create `retry.go`:

```go
package retry

import (
	"context"
	"time"
)

// Sleeper is the injected wait: Sleep blocks for d unless ctx is cancelled
// first, in which case it returns ctx.Err(). It is the time seam a test fakes.
type Sleeper interface {
	Sleep(ctx context.Context, d time.Duration) error
}

// realSleeper waits on a real timer and aborts early on context cancellation.
type realSleeper struct{}

func (realSleeper) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RealSleeper is the production Sleeper.
func RealSleeper() Sleeper { return realSleeper{} }

// Retrier retries an operation with exponential backoff.
type Retrier struct {
	base    time.Duration
	factor  int
	max     int
	sleeper Sleeper
}

// NewRetrier builds a Retrier. The sleeper is injected so tests can record the
// backoff schedule without sleeping.
func NewRetrier(base time.Duration, factor, maxAttempts int, sleeper Sleeper) *Retrier {
	return &Retrier{base: base, factor: factor, max: maxAttempts, sleeper: sleeper}
}

// Do runs op, retrying with exponential backoff up to max attempts. The first
// attempt runs immediately; each retry is preceded by a sleep of base *
// factor^(n-1). A cancelled context aborts the wait and Do returns ctx.Err().
// When all attempts fail, Do returns the last error from op.
func (r *Retrier) Do(ctx context.Context, op func() error) error {
	delay := r.base
	var err error
	for attempt := 0; attempt < r.max; attempt++ {
		if attempt > 0 {
			if serr := r.sleeper.Sleep(ctx, delay); serr != nil {
				return serr
			}
			delay *= time.Duration(r.factor)
		}
		if err = op(); err == nil {
			return nil
		}
	}
	return err
}
```

### The runnable demo

The demo uses the real sleeper (with a short base delay) to retry an operation that
fails twice and then succeeds, printing the attempt count.

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
	r := retry.NewRetrier(10*time.Millisecond, 2, 5, retry.RealSleeper())

	attempts := 0
	err := r.Do(context.Background(), func() error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary failure")
		}
		return nil
	})

	if err != nil {
		fmt.Println("failed:", err)
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

### Tests

The `fakeSleeper` is the whole reason these tests are fast and deterministic: it
records each requested delay and returns immediately, and it honors a cancelled
context so the cancellation path can be exercised without real time. The four tests
pin the four contracts: the exact backoff schedule, early stop on success,
last-error on exhaustion, and abort on cancellation.

Create `retry_test.go`:

```go
package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSleeper records requested durations without sleeping. It returns the
// context error if the context is already cancelled, mirroring realSleeper's
// cancellation contract.
type fakeSleeper struct {
	delays []time.Duration
}

func (f *fakeSleeper) Sleep(ctx context.Context, d time.Duration) error {
	f.delays = append(f.delays, d)
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

var errBoom = errors.New("boom")

func TestBackoffSchedule(t *testing.T) {
	t.Parallel()

	fs := &fakeSleeper{}
	r := NewRetrier(100*time.Millisecond, 2, 4, fs)

	err := r.Do(context.Background(), func() error { return errBoom })
	if !errors.Is(err, errBoom) {
		t.Fatalf("Do error = %v, want %v", err, errBoom)
	}

	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	if len(fs.delays) != len(want) {
		t.Fatalf("recorded %d delays, want %d: %v", len(fs.delays), len(want), fs.delays)
	}
	for i, d := range want {
		if fs.delays[i] != d {
			t.Fatalf("delay[%d] = %v, want %v", i, fs.delays[i], d)
		}
	}
}

func TestSuccessStopsEarly(t *testing.T) {
	t.Parallel()

	fs := &fakeSleeper{}
	r := NewRetrier(100*time.Millisecond, 2, 5, fs)

	calls := 0
	err := r.Do(context.Background(), func() error {
		calls++
		if calls < 3 {
			return errBoom
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do: unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want 3", calls)
	}
	// Two retries means two sleeps: 100ms, 200ms.
	if len(fs.delays) != 2 {
		t.Fatalf("recorded %d delays, want 2: %v", len(fs.delays), fs.delays)
	}
}

func TestMaxAttemptsReturnsLastError(t *testing.T) {
	t.Parallel()

	fs := &fakeSleeper{}
	r := NewRetrier(1*time.Millisecond, 2, 3, fs)

	calls := 0
	last := errors.New("attempt 3 failed")
	err := r.Do(context.Background(), func() error {
		calls++
		if calls == 3 {
			return last
		}
		return errBoom
	})
	if !errors.Is(err, last) {
		t.Fatalf("Do error = %v, want last error %v", err, last)
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want 3 (max)", calls)
	}
}

func TestContextCancelAborts(t *testing.T) {
	t.Parallel()

	fs := &fakeSleeper{}
	r := NewRetrier(100*time.Millisecond, 2, 10, fs)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	calls := 0
	err := r.Do(ctx, func() error {
		calls++
		return errBoom
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do error = %v, want context.Canceled", err)
	}
	// First attempt ran (t=0, no sleep), then the first sleep observed the
	// cancelled context and aborted: exactly one op call and one recorded delay.
	if calls != 1 {
		t.Fatalf("op called %d times, want 1", calls)
	}
	if len(fs.delays) != 1 {
		t.Fatalf("recorded %d delays, want 1", len(fs.delays))
	}
}
```

## Review

The retrier is correct when its waits go through the injected `Sleeper` and never
through `time.Sleep`. `TestBackoffSchedule` proves the schedule is exactly
100ms/200ms/400ms; `TestSuccessStopsEarly` proves a success short-circuits the
remaining sleeps; `TestMaxAttemptsReturnsLastError` proves the caller receives the
real final error, asserted with `errors.Is`; `TestContextCancelAborts` proves a
cancelled context stops the loop at the first wait with zero real elapsed time. The
mistake to avoid is sleeping directly in `Do` — the tests would then take
seconds and the cancellation path could not be checked deterministically. Note the
`realSleeper` selects on `ctx.Done()` so production cancellation is prompt too. Run
`go test -race` to confirm the loop is race-free.

## Resources

- [time.NewTimer](https://pkg.go.dev/time#NewTimer) — the cancellable wait the real sleeper is built on.
- [context: cancellation](https://pkg.go.dev/context) — `ctx.Err`, `context.Canceled`, and `context.DeadlineExceeded`.
- [Google API design guide: retry with backoff](https://cloud.google.com/storage/docs/retry-strategy) — why exponential backoff, and the shape of a real schedule.

---

Back to [06-composition-root-wiring-main.md](06-composition-root-wiring-main.md) | Next: [08-synctest-time-dependent-timeout.md](08-synctest-time-dependent-timeout.md)
