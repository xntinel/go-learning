# Exercise 8: A Retry/Backoff Wrapper That Never Retries After Cancellation

A retry loop is where good intentions become self-inflicted outages. This
exercise builds the everyday resilient DB-call wrapper the right way: it retries
only transient errors, waits between attempts in a `select` that a cancellation
can win, and stops the instant the context fires — never sleeping through its own
backoff and never hammering a database the caller already abandoned.

## What you'll build

```text
dbretry/                     independent module: example.com/dbretry
  go.mod                     go 1.25
  dbretry.go                 Do(ctx, attempts, base, fn): retry transient, backoff via select, stop on cancel
  cmd/
    demo/
      main.go                recover on attempt 3; abort promptly on mid-backoff cancel
  dbretry_test.go            recover, non-retryable, cancel-during-backoff, give-up, -race
```

Files: `dbretry.go`, `cmd/demo/main.go`, `dbretry_test.go`.
Implement: `Do(ctx, attempts, base, fn)` that retries only errors marked transient (`errors.Is(err, ErrTransient)`), backs off exponentially with jitter inside a `select` on `ctx.Done()` versus a timer, checks `ctx.Err()` before each attempt, and joins the last error with `ctx.Err()` when it gives up on cancellation.
Test: a transient failure recovers on attempt 3 (exactly 3 calls); a non-retryable error returns after 1 call; a context cancelled during the first backoff returns promptly with a context error and no second attempt; exhausting attempts returns the last error wrapped.
Verify: `go test -count=1 -race ./...`

### The two invariants: retry only transient, and wake on cancel

The first invariant is *what* to retry. Retrying a `sql.ErrNoRows` is pointless —
the row will not materialize because you asked twice — and retrying a validation
error just repeats a guaranteed rejection. Only genuinely transient failures
(a dropped connection, a serialization conflict, a deadlock victim) can succeed on
a second try. We mark those with a sentinel and classify with `errors.Is`:

```
var ErrTransient = errors.New("dbretry: transient")
func Transient(err error) error { return fmt.Errorf("%w: %w", ErrTransient, err) }
func isRetryable(err error) bool { return errors.Is(err, ErrTransient) }
```

A caller wraps the driver's transient errors with `Transient(...)`; everything
else — domain errors, `sql.ErrNoRows`, validation — is returned unwrapped and
`Do` gives up on it immediately after one call.

The second invariant is *how* to wait. The wrong way is `time.Sleep(backoff)`: a
request cancelled a microsecond into the sleep still burns the entire delay before
noticing. The right way is a `select` that races the backoff timer against
`ctx.Done()`:

```
timer := time.NewTimer(backoff)
select {
case <-ctx.Done():
    timer.Stop()
    return errors.Join(last, ctx.Err())
case <-timer.C:
}
```

Cancellation wins immediately, and the returned error is the *last* attempt's
failure joined with `ctx.Err()`, so the caller sees both why the driver failed and
that the whole thing was cancelled. `Do` also pre-checks `ctx.Err()` at the top of
each attempt, so a context that fires between attempts stops before the next call
instead of hammering the database one more time. The backoff itself is exponential
(`base << attempt`) with additive jitter (`rand.N(base)` from `math/rand/v2`) so a
thundering herd of retriers does not resynchronize on the same schedule.

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/10-context-aware-database-queries/08-context-aware-retry-backoff/cmd/demo
cd go-solutions/14-select-and-context/10-context-aware-database-queries/08-context-aware-retry-backoff
go mod edit -go=1.25
```

Create `dbretry.go`:

```go
package dbretry

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// ErrTransient marks an error as worth retrying. Callers wrap driver/connection
// failures with Transient; everything else is returned after a single attempt.
var ErrTransient = errors.New("dbretry: transient")

// ErrNotFound is a non-retryable domain sentinel (retrying will not find a row).
var ErrNotFound = errors.New("dbretry: not found")

// Transient wraps err so Do will retry it.
func Transient(err error) error { return fmt.Errorf("%w: %w", ErrTransient, err) }

func isRetryable(err error) bool { return errors.Is(err, ErrTransient) }

// Do calls fn up to attempts times, retrying only transient errors. It backs off
// exponentially with jitter between attempts, waiting in a select so a cancelled
// context aborts immediately instead of sleeping through the delay.
func Do(ctx context.Context, attempts int, base time.Duration, fn func(context.Context) error) error {
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(last, err)
		}

		last = fn(ctx)
		if last == nil {
			return nil
		}
		if !isRetryable(last) {
			return last // non-retryable: do not waste another attempt
		}
		if attempt == attempts-1 {
			break // out of attempts
		}

		backoff := base<<attempt + rand.N(base)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(last, ctx.Err())
		case <-timer.C:
		}
	}
	return fmt.Errorf("dbretry: gave up after %d attempts: %w", attempts, last)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/dbretry"
)

func main() {
	calls := 0
	err := dbretry.Do(context.Background(), 5, 5*time.Millisecond, func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return dbretry.Transient(errors.New("connection reset"))
		}
		return nil
	})
	fmt.Printf("recovered: calls=%d err=%v\n", calls, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(3 * time.Millisecond); cancel() }()
	calls = 0
	start := time.Now()
	err = dbretry.Do(ctx, 5, 50*time.Millisecond, func(ctx context.Context) error {
		calls++
		return dbretry.Transient(errors.New("timeout"))
	})
	fmt.Printf("cancelled: calls=%d canceled=%v fast=%v\n",
		calls, errors.Is(err, context.Canceled), time.Since(start) < 40*time.Millisecond)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
recovered: calls=3 err=<nil>
cancelled: calls=1 canceled=true fast=true
```

### Tests

`TestStopsOnCancelDuringBackoff` is the invariant that keeps a retrier from
becoming a database DoS: after one transient failure the wrapper enters its
backoff, the context is cancelled during it, and the wrapper must return promptly
without a second attempt.

Create `dbretry_test.go`:

```go
package dbretry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRecoversFromTransient(t *testing.T) {
	t.Parallel()
	calls := 0
	err := Do(context.Background(), 5, time.Millisecond, func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return Transient(errors.New("connection reset"))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do: err = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("fn called %d times, want 3", calls)
	}
}

func TestNonRetryableReturnsImmediately(t *testing.T) {
	t.Parallel()
	calls := 0
	err := Do(context.Background(), 5, time.Millisecond, func(ctx context.Context) error {
		calls++
		return ErrNotFound
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Do: err = %v, want ErrNotFound", err)
	}
	if calls != 1 {
		t.Fatalf("fn called %d times, want exactly 1 (non-retryable)", calls)
	}
}

func TestStopsOnCancelDuringBackoff(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(3 * time.Millisecond); cancel() }()

	calls := 0
	start := time.Now()
	err := Do(ctx, 5, 50*time.Millisecond, func(ctx context.Context) error {
		calls++
		return Transient(errors.New("timeout"))
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do: err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("fn called %d times, want exactly 1 (must not retry after cancel)", calls)
	}
	if elapsed >= 50*time.Millisecond {
		t.Fatalf("Do slept through its backoff: took %v, want prompt cancel", elapsed)
	}
}

func TestGivesUpAfterAttempts(t *testing.T) {
	t.Parallel()
	calls := 0
	err := Do(context.Background(), 3, time.Millisecond, func(ctx context.Context) error {
		calls++
		return Transient(errors.New("still failing"))
	})
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("Do: err = %v, want wrapped ErrTransient", err)
	}
	if calls != 3 {
		t.Fatalf("fn called %d times, want exactly 3 (attempts)", calls)
	}
}

func TestSucceedsFirstTry(t *testing.T) {
	t.Parallel()
	calls := 0
	err := Do(context.Background(), 3, time.Millisecond, func(ctx context.Context) error {
		calls++
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("Do = %v with %d calls, want nil with 1", err, calls)
	}
}
```

## Review

The wrapper is correct when it respects both invariants at once: it retries only
transient errors and it never continues past a cancellation.
`TestNonRetryableReturnsImmediately` proves a non-transient error costs exactly one
call — no wasted retries on a `sql.ErrNoRows`-shaped failure.
`TestStopsOnCancelDuringBackoff` proves the two hardest properties together: the
call count stays at 1 (no attempt after cancel) and the elapsed time is well under
the 50ms backoff (the `select` woke on `ctx.Done()` rather than the timer). The
give-up path returns the last error wrapped so the caller can still inspect the
cause. The failure modes this prevents are exactly the Common Mistakes: retrying
after cancellation and sleeping through backoff with `time.Sleep`. Run `-race`:
the cancel fires from a separate goroutine while `Do` is parked in its `select`.

## Resources

- [context: Context.Done and Err](https://pkg.go.dev/context#Context) — the cancellation signal the backoff races.
- [time: NewTimer and Timer.Stop](https://pkg.go.dev/time#NewTimer) — a stoppable backoff timer for the `select`.
- [math/rand/v2: N](https://pkg.go.dev/math/rand/v2#N) — jitter to de-synchronize retriers.
- [errors: Join](https://pkg.go.dev/errors#Join) — combining the last attempt's error with `ctx.Err()`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-healthcheck-ping-and-pool.md](09-healthcheck-ping-and-pool.md)
