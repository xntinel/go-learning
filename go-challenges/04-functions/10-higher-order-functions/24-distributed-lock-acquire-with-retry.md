# Exercise 24: Exclusive Lock Acquisition with Retry and Backoff Decorator

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Acquiring an exclusive lock — a leader election slot, a distributed
mutex, a per-resource advisory lock — routinely fails not because
anything is broken, but because someone else holds it right now. `WithRetry`
composes a single-shot `Acquirer` with a backoff schedule and a
cancellation-aware wait into one `Acquirer` that keeps trying until it
succeeds, hits a real error, or the caller's context gives up.

## What you'll build

```text
lock/                        independent module: example.com/lock
  go.mod                     go 1.24
  lock.go                    type Acquirer, Backoff, Sleeper; func WithRetry, RealSleeper
  lock_test.go               first-try success, retries then success, hard error, cancellation, concurrency
  cmd/demo/
    main.go                  retries against a fake lock that frees up after two attempts
```

- Files: `lock.go`, `lock_test.go`, `cmd/demo/main.go`.
- Implement: `Acquirer func(ctx context.Context) (bool, error)`, `Backoff func(attempt int) time.Duration`, `Sleeper func(ctx context.Context, d time.Duration) error`, and `WithRetry(acquire Acquirer, backoff Backoff, sleep Sleeper) Acquirer`.
- Test: success on the first attempt makes zero retries; failing several times before succeeding retries with the exact requested backoff schedule; a hard error from `acquire` is returned immediately and never retried; a canceled context during the wait stops the loop with that context's error; concurrent callers competing for the same lock never both hold it at once.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/10-higher-order-functions/24-distributed-lock-acquire-with-retry/cmd/demo
cd go-solutions/04-functions/10-higher-order-functions/24-distributed-lock-acquire-with-retry
go mod edit -go=1.24
```

### Three outcomes, three exits — and only one of them retries

`Acquirer` has a three-valued outcome baked into two return values: `(true,
nil)` means acquired, `(false, nil)` means someone else holds it right
now — a normal, expected outcome — and `(false, err)` means something is
actually broken (the lock backend is unreachable, credentials are
invalid). `WithRetry`'s loop treats these three cases completely
differently. A hard error returns immediately without ever calling
`sleep` — retrying a broken connection with the same broken connection
rarely helps and often makes an outage worse by adding retry storm
traffic. A clean "not acquired" calls `sleep(ctx, backoff(attempt))` and,
only if that wait completes without error, loops around to try again.

`Sleeper` is the same seam `FullJitter` used for randomness in Exercise 9:
`WithRetry` never calls `time.Sleep` itself, so a test can replace waiting
with an instant, recorded call instead of a real delay, making the entire
retry-then-succeed test run in microseconds while still asserting the
exact backoff durations requested. `Sleeper` also folds in cancellation:
it returns `ctx.Err()` the moment `ctx` is done, which is what lets a
caller's timeout or explicit cancellation interrupt a retry loop that
would otherwise keep trying forever against a lock nobody is going to
release.

Create `lock.go`:

```go
package lock

import (
	"context"
	"time"
)

// Acquirer attempts to acquire an exclusive lock exactly once. A false,
// nil result means "someone else holds it right now, try again later," a
// non-nil error means a hard failure that must not be retried.
type Acquirer func(ctx context.Context) (bool, error)

// Backoff maps a zero-based retry attempt to the delay before the next one.
type Backoff func(attempt int) time.Duration

// Sleeper waits for d or returns early with ctx's error if ctx is done
// first. It is a seam: production sleeps in real time, tests fake it so
// retry loops run instantly and deterministically.
type Sleeper func(ctx context.Context, d time.Duration) error

// WithRetry decorates acquire so that a failed-to-acquire attempt (false,
// nil) is retried after backoff(attempt), instead of giving up
// immediately. It stops in exactly three cases: acquire succeeds (true,
// nil); acquire returns a hard error (which is never retried); or sleep
// returns an error, which happens when ctx is canceled or its deadline
// expires while waiting.
func WithRetry(acquire Acquirer, backoff Backoff, sleep Sleeper) Acquirer {
	return func(ctx context.Context) (bool, error) {
		for attempt := 0; ; attempt++ {
			ok, err := acquire(ctx)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return false, err
			}
		}
	}
}

// RealSleeper waits for d using a real timer, honoring ctx cancellation.
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
```

### The runnable demo

The demo simulates a lock another process holds for the first two
attempts and releases before the third. A fake `Sleeper` logs the
requested delay but never really waits, so the whole retry loop runs
instantly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/lock"
)

func main() {
	// Simulates a lock another process holds for the first two attempts,
	// then releases — no real network or storage involved.
	heldUntilAttempt := 2
	attempt := 0
	acquire := func(ctx context.Context) (bool, error) {
		attempt++
		if attempt <= heldUntilAttempt {
			return false, nil
		}
		return true, nil
	}

	backoff := func(n int) time.Duration { return time.Duration(n+1) * 10 * time.Millisecond }

	// A fake Sleeper that logs the requested delay but never really
	// waits, so the demo runs instantly and deterministically.
	sleep := func(ctx context.Context, d time.Duration) error {
		fmt.Printf("waiting %s before retrying\n", d)
		return ctx.Err()
	}

	acquireWithRetry := lock.WithRetry(acquire, backoff, sleep)

	ok, err := acquireWithRetry(context.Background())
	fmt.Printf("acquired=%v err=%v after %d attempt(s)\n", ok, err, attempt)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
waiting 10ms before retrying
waiting 20ms before retrying
acquired=true err=<nil> after 3 attempt(s)
```

The first two attempts find the lock held and back off for 10ms and 20ms;
the third attempt runs after the simulated holder has released it and
succeeds.

### Tests

`TestWithRetrySucceedsOnFirstAttempt` proves the cheap path: no retry,
no sleep call at all. `TestWithRetrySucceedsAfterRetries` fails three
times before succeeding and asserts both the final success and the exact
three backoff durations requested, in order. `TestWithRetryHardErrorIsNeverRetried`
is the asymmetry the whole exercise hinges on: a non-nil error from
`acquire` must return immediately, with `sleep` never called.
`TestWithRetryStopsOnContextCancellation` cancels the context from inside
the fake `Sleeper` (simulating the deadline elapsing mid-wait) and asserts
the loop stops with `context.Canceled` rather than looping forever.
`TestWithRetryConcurrentAcquireIsMutuallyExclusive` runs ten goroutines
racing for the same in-memory `fakeLock` under `-race`, each holding it
briefly before releasing, and asserts a shared counter never observes more
than one concurrent holder — the actual mutual-exclusion property a real
distributed lock exists to provide.

Create `lock_test.go`:

```go
package lock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSleeper records every requested duration and returns immediately
// (or ctx.Err() if ctx is already done), so retry loops in tests run with
// no real wall-clock delay.
func fakeSleeper(durations *[]time.Duration) Sleeper {
	return func(ctx context.Context, d time.Duration) error {
		*durations = append(*durations, d)
		return ctx.Err()
	}
}

func TestWithRetrySucceedsOnFirstAttempt(t *testing.T) {
	t.Parallel()

	calls := 0
	acquire := func(ctx context.Context) (bool, error) {
		calls++
		return true, nil
	}
	var slept []time.Duration
	acquireWithRetry := WithRetry(acquire, func(int) time.Duration { return 0 }, fakeSleeper(&slept))

	ok, err := acquireWithRetry(context.Background())
	if err != nil || !ok {
		t.Fatalf("acquireWithRetry() = (%v, %v), want (true, nil)", ok, err)
	}
	if calls != 1 {
		t.Fatalf("acquire called %d times, want 1", calls)
	}
	if len(slept) != 0 {
		t.Fatalf("slept %v times, want 0 (no retry needed)", slept)
	}
}

func TestWithRetrySucceedsAfterRetries(t *testing.T) {
	t.Parallel()

	calls := 0
	acquire := func(ctx context.Context) (bool, error) {
		calls++
		return calls > 3, nil // fails on attempts 1-3, succeeds on 4
	}
	backoff := func(attempt int) time.Duration { return time.Duration(attempt+1) * time.Millisecond }
	var slept []time.Duration
	acquireWithRetry := WithRetry(acquire, backoff, fakeSleeper(&slept))

	ok, err := acquireWithRetry(context.Background())
	if err != nil || !ok {
		t.Fatalf("acquireWithRetry() = (%v, %v), want (true, nil)", ok, err)
	}
	if calls != 4 {
		t.Fatalf("acquire called %d times, want 4", calls)
	}
	want := []time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}
	if len(slept) != len(want) {
		t.Fatalf("slept %v, want %v", slept, want)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Fatalf("slept %v, want %v", slept, want)
		}
	}
}

func TestWithRetryHardErrorIsNeverRetried(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("lock store unreachable")
	calls := 0
	acquire := func(ctx context.Context) (bool, error) {
		calls++
		return false, errBoom
	}
	var slept []time.Duration
	acquireWithRetry := WithRetry(acquire, func(int) time.Duration { return 0 }, fakeSleeper(&slept))

	ok, err := acquireWithRetry(context.Background())
	if ok {
		t.Fatal("acquireWithRetry() reported success despite a hard error")
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	if calls != 1 {
		t.Fatalf("acquire called %d times, want 1 (a hard error must not be retried)", calls)
	}
	if len(slept) != 0 {
		t.Fatalf("slept %v times, want 0", slept)
	}
}

func TestWithRetryStopsOnContextCancellation(t *testing.T) {
	t.Parallel()

	acquire := func(ctx context.Context) (bool, error) {
		return false, nil // never succeeds
	}
	ctx, cancel := context.WithCancel(context.Background())
	var slept []time.Duration
	sleep := func(sctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		cancel() // simulate the deadline elapsing during the wait
		return sctx.Err()
	}
	acquireWithRetry := WithRetry(acquire, func(int) time.Duration { return time.Millisecond }, sleep)

	ok, err := acquireWithRetry(ctx)
	if ok {
		t.Fatal("acquireWithRetry() reported success, want cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(slept) != 1 {
		t.Fatalf("slept %d times, want exactly 1", len(slept))
	}
}

// fakeLock is a single-holder in-memory lock used to prove mutual
// exclusion under concurrent acquisition attempts.
type fakeLock struct {
	mu   sync.Mutex
	held bool
}

func (l *fakeLock) tryAcquire(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		return false, nil
	}
	l.held = true
	return true, nil
}

func (l *fakeLock) release() {
	l.mu.Lock()
	l.held = false
	l.mu.Unlock()
}

func TestWithRetryConcurrentAcquireIsMutuallyExclusive(t *testing.T) {
	t.Parallel()

	l := &fakeLock{}
	backoff := func(int) time.Duration { return 0 }
	sleep := func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	acquireWithRetry := WithRetry(l.tryAcquire, backoff, sleep)

	var mu sync.Mutex
	current := 0
	maxConcurrent := 0

	const workers = 10
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()

			ok, err := acquireWithRetry(ctx)
			if err != nil || !ok {
				return
			}

			mu.Lock()
			current++
			if current > maxConcurrent {
				maxConcurrent = current
			}
			mu.Unlock()

			time.Sleep(time.Millisecond) // hold briefly to widen the window for a real race to surface

			mu.Lock()
			current--
			mu.Unlock()
			l.release()
		}()
	}
	wg.Wait()

	if maxConcurrent > 1 {
		t.Fatalf("maxConcurrent = %d, want at most 1 (mutual exclusion violated)", maxConcurrent)
	}
}
```

## Review

`WithRetry` is correct when it treats "not acquired" and "hard error" as
genuinely different outcomes — only the former loops back to `sleep` and
tries again, and `TestWithRetryHardErrorIsNeverRetried` is what pins that
asymmetry down instead of leaving it as a comment. Injecting `Sleeper`
does double duty: it makes the retry-then-succeed test assert an exact
schedule of durations instead of guessing about timing, and it is the
single place cancellation plugs into the loop, since `WithRetry` itself
never touches `ctx.Done()` directly. `fakeLock.tryAcquire`'s check-then-act
— check `held`, then set it — happens entirely inside one `mu.Lock()`
critical section, which is what the concurrency test actually verifies:
splitting that check from that assignment, even briefly, is exactly the
kind of bug that would let two goroutines both believe they acquired the
lock. Run `go test -race`, since this exercise's entire point is
concurrent callers competing for one resource.

## Resources

- [context package](https://pkg.go.dev/context) — `WithTimeout`, `WithCancel`, `Err`, the cancellation propagation this exercise depends on.
- [sync package](https://pkg.go.dev/sync) — `Mutex`, `WaitGroup`, the primitives behind `fakeLock` and the concurrency test.
- [etcd: Distributed Locks](https://etcd.io/docs/v3.5/dev-guide/api_concurrency_reference_v3/) — a real distributed lock service with the same acquire/retry/release shape this exercise models in memory.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-lazy-iterator-collect-transform.md](23-lazy-iterator-collect-transform.md) | Next: [25-feature-flag-variant-selector.md](25-feature-flag-variant-selector.md)
