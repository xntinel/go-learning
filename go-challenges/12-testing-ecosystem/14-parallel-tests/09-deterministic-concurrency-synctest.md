# Exercise 9: Deterministic Tests for a Retry/Backoff Worker with testing/synctest

A retry with exponential backoff is timer-driven concurrent code, and testing it
honestly is miserable: real `time.Sleep` makes the test slow, and "sleep a bit
longer than the backoff and hope" makes it flaky. Go 1.25's `testing/synctest`
runs the code in a bubble with a virtual clock, so a fifteen-second backoff is
asserted in microseconds, deterministically. This module builds the retry caller
and tests its exact virtual timing.

This module is fully self-contained: its own module, demo, and tests. It requires
Go 1.25+ for `testing/synctest`.

## What you'll build

```text
retrybackoff/               independent module: example.com/retrybackoff
  go.mod                    go 1.25+ (synctest)
  retry.go                  Do(ctx, base, maxAttempts, op); ErrExhausted
  cmd/
    demo/
      main.go               runnable demo: retry a flaky op, print attempts
  retry_test.go             synctest bubbles: exact virtual elapsed time, Wait
```

Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
Implement: `Do(ctx, base, maxAttempts, op)` retrying `op` with exponential
backoff (`base * 2^(attempt-1)`) between attempts, cancellable via `ctx`.
Test: `synctest.Test` bubbles asserting the exact virtual elapsed time equals the
backoff sum, and using `synctest.Wait` to pin the worker mid-backoff.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/14-parallel-tests/09-deterministic-concurrency-synctest/cmd/demo
cd go-solutions/12-testing-ecosystem/14-parallel-tests/09-deterministic-concurrency-synctest
go mod edit -go=1.25
```

### Why synctest instead of an injected clock

Backoff logic is a pure function of time: attempt, fail, wait `base`, attempt,
fail, wait `2*base`, and so on. To test that the caller waited the *right* amount
you must observe elapsed time. The old approaches both hurt: sleeping real
durations makes a suite that exercises a 15-second backoff take 15 real seconds,
and injecting a `Clock` interface litters production code with an abstraction that
exists only for tests. `synctest.Test(t, f)` gives a third way — it runs `f` in a
bubble where the `time` package is virtualized. `time.After(8*time.Second)` does
not wait eight seconds; the bubble sees every goroutine durably blocked and jumps
the clock forward to fire the timer. The code under test calls `time.After`
directly, exactly as in production, and the test still reads `time.Since(start)`
and gets the exact virtual elapsed time.

The key guarantee: the clock advances only when *every* goroutine in the bubble is
durably blocked (blocked on an in-bubble channel, `time.After`, a `WaitGroup`,
...). So there is no scheduler slack: the elapsed time is exactly the sum of the
backoffs, and you can assert it with `==`, not a tolerance.

### synctest.Wait for pinning a background goroutine

When the retry runs in its own goroutine, you sometimes want to assert its state
*mid-flight* — "after the first failed attempt, before the first backoff elapses".
`synctest.Wait()` blocks the caller until every *other* bubble goroutine is durably
blocked. After the worker has run its first `op()` and parked on `time.After`,
`synctest.Wait()` returns, and you can read the call count knowing exactly one
attempt has happened and the next is waiting on the (virtual) clock. This removes
the race between "advance the clock" and "the goroutine reacted" that plagues
real-time concurrency tests.

Create `retry.go`:

```go
package retrybackoff

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrExhausted wraps the final failure after all attempts are used.
var ErrExhausted = errors.New("retry: attempts exhausted")

// Do calls op up to maxAttempts times. Between attempt i and i+1 it waits
// base*2^(i-1) (exponential backoff). It returns the attempt count and nil on
// the first success; if op never succeeds it returns maxAttempts and an error
// wrapping ErrExhausted. A cancelled ctx aborts the wait and returns ctx.Err().
func Do(ctx context.Context, base time.Duration, maxAttempts int, op func() error) (int, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		lastErr = op()
		if lastErr == nil {
			return attempt, nil
		}
		if attempt == maxAttempts {
			break
		}
		delay := base << (attempt - 1) // base * 2^(attempt-1)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return attempt, ctx.Err()
		}
	}
	return maxAttempts, fmt.Errorf("%w: last error: %v", ErrExhausted, lastErr)
}
```

### The runnable demo

The demo runs outside a bubble with a tiny real backoff so it completes instantly
against the wall clock: a flaky op that fails twice then succeeds.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/retrybackoff"
)

func main() {
	calls := 0
	n, err := retrybackoff.Do(context.Background(), time.Millisecond, 5, func() error {
		calls++
		if calls < 3 {
			return errors.New("temporary failure")
		}
		return nil
	})
	fmt.Printf("attempts: %d, err: %v\n", n, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempts: 3, err: <nil>
```

### Tests

`TestBackoffTimingSuccess` runs inside `synctest.Test`: an op that fails twice then
succeeds, with `base = 100ms`. The two backoffs are 100ms and 200ms, so the exact
virtual elapsed time is 300ms — asserted with `==`. `TestBackoffTimingExhausted`
uses all four attempts (backoffs 100+200+400ms = 700ms) and asserts the error
wraps `ErrExhausted`. `TestWaitPinsFirstAttempt` runs `Do` in a goroutine and uses
`synctest.Wait()` to confirm exactly one attempt has happened while the worker is
parked on the first backoff. Each outer test may be `t.Parallel()`, but the bubble
function must not call it.

Create `retry_test.go`:

```go
package retrybackoff

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

var errFlaky = errors.New("flaky")

func TestBackoffTimingSuccess(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls := 0
		n, err := Do(context.Background(), 100*time.Millisecond, 5, func() error {
			calls++
			if calls < 3 {
				return errFlaky
			}
			return nil
		})
		if err != nil || n != 3 {
			t.Fatalf("Do = %d,%v; want 3,nil", n, err)
		}
		// Backoffs before attempts 2 and 3: 100ms + 200ms = 300ms, exactly.
		if elapsed := time.Since(start); elapsed != 300*time.Millisecond {
			t.Fatalf("virtual elapsed = %s, want 300ms", elapsed)
		}
	})
}

func TestBackoffTimingExhausted(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		n, err := Do(context.Background(), 100*time.Millisecond, 4, func() error {
			return errFlaky
		})
		if n != 4 {
			t.Fatalf("attempts = %d, want 4", n)
		}
		if !errors.Is(err, ErrExhausted) {
			t.Fatalf("err = %v, want errors.Is ErrExhausted", err)
		}
		// Backoffs before attempts 2,3,4: 100+200+400 = 700ms.
		if elapsed := time.Since(start); elapsed != 700*time.Millisecond {
			t.Fatalf("virtual elapsed = %s, want 700ms", elapsed)
		}
	})
}

func TestWaitPinsFirstAttempt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		calls := 0
		done := make(chan struct{})

		go func() {
			_, _ = Do(context.Background(), time.Second, 5, func() error {
				mu.Lock()
				calls++
				mu.Unlock()
				return errFlaky
			})
			close(done)
		}()

		// Wait returns once the worker is durably blocked on its first backoff.
		synctest.Wait()
		mu.Lock()
		got := calls
		mu.Unlock()
		if got != 1 {
			t.Fatalf("after first backoff, calls = %d, want 1", got)
		}

		<-done // let virtual time carry the retries to exhaustion
	})
}

func ExampleDo() {
	// First attempt succeeds: no backoff, deterministic without a bubble.
	n, err := Do(context.Background(), time.Second, 3, func() error { return nil })
	fmt.Println(n, err)
	// Output: 1 <nil>
}
```

## Review

The retry is correct when the elapsed time is exactly the sum of the backoffs and
the exhausted case wraps `ErrExhausted`. Under `synctest.Test`, both are asserted
with exact equality because virtual time has no scheduler slack: `TestBackoffTimingSuccess`
sees precisely 300ms, `TestBackoffTimingExhausted` precisely 700ms, and neither
test waits a real millisecond. `TestWaitPinsFirstAttempt` shows the other half of
the tool — `synctest.Wait()` lets you read the worker's state at a defined point
without a race, because it returns only when the worker is durably blocked.

The rules the bubble enforces: do not call `t.Parallel`/`t.Run` on the bubble's
`T` (mark the *outer* test parallel instead), and do not do real I/O inside the
bubble — a goroutine blocked on a socket or a real syscall never becomes durably
blocked, so the clock cannot advance and the test deadlocks. Prefer
`synctest.Test` over the deprecated `synctest.Run`; `Test` wires the bubble to the
test's `T` so cleanups and failures work.

## Resources

- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — `Test`, `Wait`, the bubble, and the virtual clock.
- [Testing concurrent code with testing/synctest](https://go.dev/blog/synctest) — the Go blog's walkthrough of virtual time and durable blocking.
- [`time.After`](https://pkg.go.dev/time#After) — the timer the backoff waits on, virtualized inside a bubble.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-flaky-detection-shuffle-count-race.md](08-flaky-detection-shuffle-count-race.md) | Next: [../15-testable-examples/00-concepts.md](../15-testable-examples/00-concepts.md)
