# Exercise 8: Retry/Backoff — Attempt Sequences as a Context-Aware `iter.Seq2`

A retry loop is a schedule (how many attempts, how long to wait) wrapped around an
operation. Expressing the schedule as an `iter.Seq2[int, time.Duration]` decouples
the two: the iterator yields `(attemptNumber, backoffDelay)` with exponential
backoff and stops when the context is cancelled or the attempts are exhausted; the
consumer ranges, does the work, and breaks on success. Tests inject the clock, so
they run instantly with no real sleeping.

## What you'll build

```text
retry/                    independent module: example.com/retry
  go.mod                  module example.com/retry
  retry.go                Attempts, Retry
  cmd/
    demo/
      main.go             runnable demo: retry a flaky op with a no-op sleep
  retry_test.go           success-breaks, exhaustion, pre-cancel, mid-cancel tests
```

Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
Implement: `Attempts(ctx, maxAttempts, base) iter.Seq2[int, time.Duration]` with exponential delays and a `ctx.Err()` check before each yield, and `Retry` with an injected sleep.
Test: success on attempt 3 breaks after 3 yields; exhaustion yields exactly N with doubling delays; a pre-cancelled context yields zero; a mid-way cancel stops promptly.
Verify: `go test -count=1 -race ./...`

## The design

`Attempts` yields `(n+1, base<<n)` for `n` in `0..maxAttempts-1`: attempt numbers
are 1-based and the delay doubles each round via a left shift, so `base=100ms`
gives `100ms, 200ms, 400ms, ...`. Before every yield it checks `ctx.Err()`; if the
context is already done — cancelled or deadline exceeded — it returns without
yielding. That is the difference between a retry loop that respects cancellation
and one that keeps hammering a dead request: the check lives *inside* the iterator,
so a context cancelled mid-loop stops the very next attempt.

Crucially, `Attempts` does not sleep. It only produces the schedule. The consumer
decides what to do with each `(attempt, delay)` pair, and that is where the sleep
happens — which lets a test drive the schedule with zero wall-clock time. `Retry`
is that consumer: it ranges the attempts, runs `op(attempt)`, returns `nil` on
success, and otherwise calls an injected `sleep(delay)` before the next attempt.
Injecting `sleep` (rather than calling `time.Sleep` directly) is the whole reason
the tests are fast and deterministic — production passes `time.Sleep`, tests pass
a function that records the delay and returns immediately.

Because `Retry` returns from inside the `range` loop on success, that early exit
propagates as `yield` returning `false`, so `Attempts` stops — no wasted schedule
computation past the winning attempt.

Create `retry.go`:

```go
package retry

import (
	"context"
	"iter"
	"time"
)

// Attempts yields (attemptNumber, backoffDelay) with exponential backoff,
// stopping after maxAttempts or as soon as ctx is done. It does not sleep;
// the consumer owns the waiting.
func Attempts(ctx context.Context, maxAttempts int, base time.Duration) iter.Seq2[int, time.Duration] {
	return func(yield func(int, time.Duration) bool) {
		for n := range maxAttempts {
			if ctx.Err() != nil {
				return
			}
			if !yield(n+1, base<<n) {
				return
			}
		}
	}
}

// Retry runs op until it succeeds, the attempts are exhausted, or ctx is done.
// sleep is injected so tests can advance time without waiting.
func Retry(ctx context.Context, maxAttempts int, base time.Duration, sleep func(time.Duration), op func(attempt int) error) error {
	var err error
	for n, d := range Attempts(ctx, maxAttempts, base) {
		if err = op(n); err == nil {
			return nil
		}
		sleep(d)
	}
	if err == nil {
		return ctx.Err()
	}
	return err
}
```

## Demo

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

var errFlaky = errors.New("transient failure")

func main() {
	ctx := context.Background()
	last := 0

	err := retry.Retry(ctx, 5, 100*time.Millisecond, func(time.Duration) {}, func(n int) error {
		last = n
		if n < 3 {
			fmt.Printf("attempt %d: transient failure\n", n)
			return errFlaky
		}
		fmt.Printf("attempt %d: ok\n", n)
		return nil
	})

	fmt.Printf("done after %d attempts, err=%v\n", last, err)
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
attempt 3: ok
done after 3 attempts, err=<nil>
```

## Tests

Create `retry_test.go`:

```go
package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSuccessBreaksEarly(t *testing.T) {
	t.Parallel()

	var attempts []int
	err := Retry(context.Background(), 5, time.Millisecond, func(time.Duration) {}, func(n int) error {
		attempts = append(attempts, n)
		if n == 3 {
			return nil
		}
		return errors.New("flaky")
	})

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(attempts) != 3 {
		t.Fatalf("attempts = %v, want 3 yields", attempts)
	}
}

func TestExhaustionYieldsDoublingDelays(t *testing.T) {
	t.Parallel()

	var got []time.Duration
	for _, d := range collect(Attempts(context.Background(), 4, 100*time.Millisecond)) {
		got = append(got, d)
	}

	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d delays, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delay[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPreCancelledYieldsZero(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	count := 0
	for range Attempts(ctx, 5, time.Millisecond) {
		count++
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0 (pre-cancelled)", count)
	}
}

func TestMidCancelStopsPromptly(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var seen []int
	for n := range attemptNumbers(Attempts(ctx, 5, time.Millisecond)) {
		seen = append(seen, n)
		if n == 2 {
			cancel()
		}
	}

	if len(seen) != 2 {
		t.Fatalf("seen = %v, want [1 2] (stop after cancel)", seen)
	}
}

// collect drains a Seq2 into two parallel slices.
func collect(seq func(func(int, time.Duration) bool)) []time.Duration {
	var ds []time.Duration
	for _, d := range seq {
		ds = append(ds, d)
	}
	return ds
}

// attemptNumbers projects a Seq2 to just the attempt number.
func attemptNumbers(seq func(func(int, time.Duration) bool)) func(func(int) bool) {
	return func(yield func(int) bool) {
		for n := range seq {
			if !yield(n) {
				return
			}
		}
	}
}
```

## Review

The schedule is correct when the delays double from `base` and there are exactly
`maxAttempts` of them on exhaustion — the doubling test asserts the whole
sequence. Cancellation correctness is two tests: a pre-cancelled context yields
zero attempts because the `ctx.Err()` check fires before the first yield, and a
context cancelled after attempt two stops at two because the check runs before
every yield. Success short-circuits: `Retry` returning from inside the loop stops
`Attempts` at the winning attempt. The injected `sleep` is what makes all of this
run in microseconds — no test waits a real millisecond, which is the point of
decoupling the schedule from the wait.

## Resources

- [`iter` package documentation](https://pkg.go.dev/iter)
- [`context` package](https://pkg.go.dev/context)
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-layered-config-maps-seq.md](07-layered-config-maps-seq.md) | Next: [09-dedup-idempotency-combinator.md](09-dedup-idempotency-combinator.md)
