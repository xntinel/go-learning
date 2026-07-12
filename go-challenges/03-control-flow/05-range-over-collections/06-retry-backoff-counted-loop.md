# Exercise 6: Retry with Capped Exponential Backoff using for range n

Retrying a flaky outbound call is one of the most common backend chores, and the
loop that drives it is the Go 1.22 integer range: `for attempt := range maxAttempts`.
The attempt index is not decoration — it is the exponent in `base << attempt`, the
capped exponential backoff. This module builds a context-aware retry wrapper with
jitter, injectable sleep for fast deterministic tests, and correct error wrapping.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
retry/                      independent module: example.com/retry
  go.mod                    go 1.24
  retry.go                  Retrier{MaxAttempts,Base,Max}; Do(ctx, op) error; capped backoff+jitter
  cmd/
    demo/
      main.go               runnable demo: op fails twice then succeeds
  retry_test.go             success-after-K, exhaustion wraps last err, context cancel, backoff durations
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `Retrier.Do(ctx, op)` looping `for attempt := range r.MaxAttempts`, backing off `base << attempt` capped at `Max` with jitter, honoring `ctx` between sleeps.
- Test: succeeds on attempt K+1; exhaustion returns the last error wrapped; context cancel aborts mid-backoff; backoff durations follow the capped exponential (via an injected sleep and deterministic jitter).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the attempt index drives the backoff

`for attempt := range r.MaxAttempts` runs `MaxAttempts` iterations with `attempt`
going `0, 1, 2, ...`. That index is exactly the exponent you need: the delay before
the next try is `Base << attempt` — `Base` on the first retry, `2*Base` on the
second, `4*Base` on the third — capped at `Max` so it never grows without bound.
Writing this as an old-style `for i := 0; i < n; i++` would work too, but the
integer range reads as "try up to n times" and keeps the exponent and the count in
one place.

Three details make this production-grade rather than a toy. First, jitter: real
systems add randomness to the delay so that a fleet of clients retrying after a
shared outage does not synchronize into a thundering herd. We use "full jitter" in
`[d/2, d]`. Because raw randomness would make tests non-deterministic, the random
source is an injectable field; the default is `rand.Float64`, and tests inject a
fixed value. Second, context: between the failed call and the sleep, and inside the
sleep, we watch `ctx`. If the caller's deadline fires mid-backoff, `Do` returns
`ctx.Err()` immediately rather than sleeping out the full delay. Third, the sleep
itself is injectable so tests run in microseconds; the default is a context-aware
timer.

On the last attempt there is no point sleeping — there will be no further try — so
the loop breaks before the final backoff and returns the last error wrapped with
`%w`, so callers can `errors.Is` it against whatever the operation returned.

Note `cap` is a builtin, so the field and locals are named `Max`/`maxDelay` to
avoid shadowing it. The shift `Base << attempt` can overflow for large attempt
counts, so `capExp` clamps to `Max` on overflow or when the shift exceeds `Max`.

Create `retry.go`:

```go
package retry

import (
	"context"
	"fmt"
	"math/rand"
	"time"
)

// Retrier retries an operation with capped exponential backoff and jitter.
type Retrier struct {
	MaxAttempts int
	Base        time.Duration
	Max         time.Duration

	// rand returns a value in [0,1) for jitter; nil uses math/rand.
	rand func() float64
	// sleep waits d honoring ctx; nil uses a context-aware timer.
	sleep func(ctx context.Context, d time.Duration) error
}

// Do runs op until it returns nil or attempts are exhausted, sleeping a capped
// exponential backoff (with jitter) between tries and honoring ctx.
func (r Retrier) Do(ctx context.Context, op func(ctx context.Context) error) error {
	randFn := r.rand
	if randFn == nil {
		randFn = rand.Float64
	}
	sleepFn := r.sleep
	if sleepFn == nil {
		sleepFn = sleepCtx
	}

	var err error
	for attempt := range r.MaxAttempts {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err = op(ctx); err == nil {
			return nil
		}
		if attempt == r.MaxAttempts-1 {
			break // last attempt: no backoff after it
		}
		d := jittered(capExp(r.Base, r.Max, attempt), randFn())
		if serr := sleepFn(ctx, d); serr != nil {
			return serr
		}
	}
	return fmt.Errorf("retry: exhausted after %d attempts: %w", r.MaxAttempts, err)
}

// capExp returns base<<attempt clamped to maxDelay, guarding against overflow.
func capExp(base, maxDelay time.Duration, attempt int) time.Duration {
	if attempt >= 62 {
		return maxDelay
	}
	d := base << attempt
	if d <= 0 || d > maxDelay {
		return maxDelay
	}
	return d
}

// jittered applies full jitter, returning a delay in [d/2, d]. jit is in [0,1).
func jittered(d time.Duration, jit float64) time.Duration {
	return time.Duration(float64(d) * (0.5 + 0.5*jit))
}

// sleepCtx waits d or returns early with ctx.Err() if ctx is done first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

### The runnable demo

The demo retries an operation that fails its first two calls and succeeds on the
third, printing the attempt count. Backoff uses a tiny base so the demo finishes
quickly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/retry"
)

func main() {
	r := retry.Retrier{MaxAttempts: 5, Base: time.Millisecond, Max: 10 * time.Millisecond}

	calls := 0
	err := r.Do(context.Background(), func(context.Context) error {
		calls++
		if calls < 3 {
			return fmt.Errorf("transient failure %d", calls)
		}
		return nil
	})

	fmt.Printf("err=%v calls=%d\n", err, calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
err=<nil> calls=3
```

### Tests

Tests inject a no-op sleep (and, where durations matter, a fixed jitter) so they run
instantly and deterministically. The success test proves `Do` stops on the first
success. The exhaustion test proves the returned error wraps the operation's last
error via `errors.Is`. The cancel test proves a context cancellation surfaced by the
sleep aborts the loop. The durations test records what the sleep was asked to wait
and asserts the capped exponential sequence.

Create `retry_test.go`:

```go
package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// noSleep is an injected sleep that never waits.
func noSleep(context.Context, time.Duration) error { return nil }

// fullJitter makes jittered(d, .) == d, so recorded delays are exact.
func fullJitter() float64 { return 1.0 }

func TestSucceedsAfterKFailures(t *testing.T) {
	t.Parallel()
	r := Retrier{MaxAttempts: 5, Base: time.Second, Max: time.Minute, rand: fullJitter, sleep: noSleep}

	calls := 0
	err := r.Do(context.Background(), func(context.Context) error {
		calls++
		if calls <= 2 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want 3", calls)
	}
}

var errBoom = errors.New("boom")

func TestExhaustionWrapsLastError(t *testing.T) {
	t.Parallel()
	r := Retrier{MaxAttempts: 3, Base: time.Second, Max: time.Minute, rand: fullJitter, sleep: noSleep}

	calls := 0
	err := r.Do(context.Background(), func(context.Context) error {
		calls++
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("Do = %v, want wrapped errBoom", err)
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want 3", calls)
	}
}

func TestContextCancelAborts(t *testing.T) {
	t.Parallel()
	// This sleep simulates the context being cancelled during the backoff.
	cancelDuringSleep := func(context.Context, time.Duration) error { return context.Canceled }
	r := Retrier{MaxAttempts: 5, Base: time.Second, Max: time.Minute, rand: fullJitter, sleep: cancelDuringSleep}

	calls := 0
	err := r.Do(context.Background(), func(context.Context) error {
		calls++
		return errBoom
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("op called %d times, want 1 (aborted in first backoff)", calls)
	}
}

func TestBackoffDurationsAreCappedExponential(t *testing.T) {
	t.Parallel()
	var got []time.Duration
	record := func(_ context.Context, d time.Duration) error {
		got = append(got, d)
		return nil
	}
	r := Retrier{MaxAttempts: 5, Base: time.Second, Max: 4 * time.Second, rand: fullJitter, sleep: record}

	_ = r.Do(context.Background(), func(context.Context) error { return errBoom })

	// 5 attempts => 4 backoffs between them: 1s, 2s, 4s, then capped at 4s.
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("recorded %d delays, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delay[%d] = %v, want %v (all: %v)", i, got[i], want[i], got)
		}
	}
}
```

## Review

The retry is correct when it stops on the first success, wraps the last error on
exhaustion so `errors.Is` still finds the cause, and never sleeps past a cancelled
context. The `for attempt := range r.MaxAttempts` loop keeps the attempt count and
the backoff exponent in one expression; the capped shift `capExp` guards overflow;
jitter is injected so the delay is deterministic in tests but randomized in
production. The subtle bug to avoid is sleeping after the final attempt — there is
no try after it, so the loop breaks before that backoff. Run `go test -race` to
confirm the injected functions and the loop compose cleanly.

## Resources

- [Go Specification: For statements (range over integer)](https://go.dev/ref/spec#For_range)
- [package context](https://pkg.go.dev/context)
- [errors.Is](https://pkg.go.dev/errors#Is)
- [AWS Architecture Blog: Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-ttl-cache-sweep.md](05-ttl-cache-sweep.md) | Next: [07-record-batch-inplace-update.md](07-record-batch-inplace-update.md)
