# Exercise 7: Blocking Wait(ctx) with Deadline-Aware Backpressure

`Allow` is the right shape for an HTTP edge — deny fast, let the client
retry. A queue worker wants the opposite: when the bucket is empty, *wait*
for a token so the job is delayed instead of dropped, but never wait past
the job's deadline or a shutdown signal. This exercise adds
`Wait(ctx context.Context) error` to both limiters, and in doing so exposes
the sharpest architectural difference between them: blocking acquisition is
one `select` for the channel version and a careful sleep-outside-the-lock
loop for the mutex version.

## What you'll build

```text
waitlimiter/                    independent module: example.com/waitlimiter
  go.mod
  limiter/
    mutex.go                    MutexLimiter: Allow + Wait (timer vs ctx.Done,
                                never sleeping under the lock)
    channel.go                  ChannelLimiter: Allow + Wait (two-case select)
    wait_test.go                deadline, refill-success, pre-canceled ctx,
                                mid-block cancellation with context.Cause
  cmd/
    demo/
      main.go                   drain, wait through a refill, deadline expiry
```

- Files: `limiter/mutex.go`, `limiter/channel.go`, `limiter/wait_test.go`, `cmd/demo/main.go`.
- Implement: `Wait(ctx)` on both limiters returning `nil` on acquisition and `ctx.Err()` on cancellation or deadline; the mutex version computes the delay under the lock and sleeps outside it on a `time.NewTimer`.
- Test: `t.Context()`-derived timeouts prove prompt `context.DeadlineExceeded` on a drained limiter, success once refill lands, immediate return on a pre-canceled context, and clean release of ten goroutines blocked mid-`Wait`, with the cancel cause recovered via `context.Cause` and `errors.Is`.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p ~/go-exercises/waitlimiter/limiter ~/go-exercises/waitlimiter/cmd/demo
cd ~/go-exercises/waitlimiter
go mod init example.com/waitlimiter
```

### Channels block natively; mutexes must be taught to

For the channel limiter, `Wait` barely needs writing: the token store *is* a
channel, and receiving from it blocks until a token exists. Add
`ctx.Done()` as the second `select` case and cancellation falls out:

```
select {
case <-cl.tokens:
	return nil
case <-ctx.Done():
	return ctx.Err()
}
```

This is the strongest single argument for the channel design: the same
data structure serves non-blocking (`select`/`default`), blocking
(`<-tokens`), and cancellable-blocking (two-case `select`) acquisition with
no new state and no new goroutines.

The mutex version has no equivalent — you cannot `select` on a mutex — so
`Wait` must be assembled from parts, and the assembly has one iron rule:
**never block while holding the lock**. The loop is: lock; refill; if a
token exists, spend it and return; otherwise compute how long until the
deficit refills, *unlock*, then sleep on a `time.NewTimer` racing
`ctx.Done()`; on waking, loop and re-check. The re-check is not optional.
While you slept, another goroutine may have taken the token your timer was
counting toward — the sleep duration was a *prediction*, and predictions
about shared state expire the moment the lock is released. A version that
sleeps and then blindly decrements is a check-then-act race with a nap in
the middle.

Why is sleeping under the lock so bad? Every other caller — including plain
`Allow` calls that would have been denied in nanoseconds — queues behind
your timer. One waiting goroutine collapses the entire limiter to
single-file, and if the refill you are waiting for could only be observed
by code that needs the lock (as it is here, where refill happens inside
`Allow`/`Wait` themselves), you can deadlock outright. The pattern
generalizes far beyond limiters: compute-under-lock, block-outside-lock,
re-validate-after-waking is how any mutex-guarded resource supports waiting
(it is also exactly what `sync.Cond` packages up, and what
`golang.org/x/time/rate.Limiter.Wait` does internally with reservations).

Two smaller contracts both implementations share. First, a pre-canceled
context returns its error *before* consuming a token — check `ctx.Err()` at
entry; otherwise a canceled request path can still drain capacity. Second,
`Wait` returns `ctx.Err()` (`context.Canceled` or
`context.DeadlineExceeded`), and callers who need to know *why* a shutdown
canceled them use `context.Cause`, which surfaces the cause passed to
`context.WithCancelCause` — the tests pin exactly this with a sentinel
error and `errors.Is`. One honesty note: neither `Wait` here is FIFO-fair;
concurrently waiting goroutines race for each refilled token, and a very
unlucky waiter can starve. Fairness requires pre-booking tokens — that is
`rate.Limiter.Reserve`, in the next exercise.

Create `limiter/mutex.go`:

```go
package limiter

import (
	"context"
	"sync"
	"time"
)

// MutexLimiter is a continuous-refill token bucket supporting both
// non-blocking Allow and deadline-aware blocking Wait.
type MutexLimiter struct {
	mu         sync.Mutex
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second; 0 disables refill
	lastRefill time.Time
}

func NewMutexLimiter(maxTokens, refillRate float64) *MutexLimiter {
	return &MutexLimiter{
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

// refillLocked credits elapsed-time tokens. Callers must hold mu.
func (l *MutexLimiter) refillLocked() {
	now := time.Now()
	l.tokens += now.Sub(l.lastRefill).Seconds() * l.refillRate
	if l.tokens > l.maxTokens {
		l.tokens = l.maxTokens
	}
	l.lastRefill = now
}

// Allow spends one token if available, without blocking.
func (l *MutexLimiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.refillLocked()
	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	return false
}

// Wait blocks until a token is acquired or ctx is done. The delay until the
// next token is computed under the lock, but the sleep happens outside it:
// blocking while holding the mutex would serialize every other caller
// behind this goroutine's nap. After each wake the state is re-checked,
// because another goroutine may have taken the predicted token.
func (l *MutexLimiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err // a canceled request must not consume capacity
	}
	for {
		l.mu.Lock()
		l.refillLocked()
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		noRefill := l.refillRate <= 0
		var wait time.Duration
		if !noRefill {
			deficit := 1 - l.tokens
			wait = time.Duration(deficit / l.refillRate * float64(time.Second))
			if wait < time.Millisecond {
				wait = time.Millisecond // float truncation floor
			}
		}
		l.mu.Unlock()

		if noRefill {
			// No token will ever appear: only cancellation ends the wait.
			<-ctx.Done()
			return ctx.Err()
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop() // release the runtime timer on the early exit
			return ctx.Err()
		case <-timer.C:
			// Re-check under the lock on the next iteration.
		}
	}
}
```

Create `limiter/channel.go`:

```go
package limiter

import (
	"context"
	"sync"
	"time"
)

// ChannelLimiter is a ticker-refilled token bucket over a buffered channel,
// supporting non-blocking Allow and deadline-aware blocking Wait.
type ChannelLimiter struct {
	tokens chan struct{}
	stop   chan struct{}
	once   sync.Once
}

func NewChannelLimiter(maxTokens int, refillInterval time.Duration) *ChannelLimiter {
	cl := &ChannelLimiter{
		tokens: make(chan struct{}, maxTokens),
		stop:   make(chan struct{}),
	}
	for range maxTokens {
		cl.tokens <- struct{}{}
	}
	go func() {
		ticker := time.NewTicker(refillInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				select {
				case cl.tokens <- struct{}{}:
				default:
				}
			case <-cl.stop:
				return
			}
		}
	}()
	return cl
}

// Allow spends one token if available, without blocking.
func (cl *ChannelLimiter) Allow() bool {
	select {
	case <-cl.tokens:
		return true
	default:
		return false
	}
}

// Wait blocks until a token arrives or ctx is done. The token store is a
// channel, so cancellable blocking acquisition is a single select — the
// structural payoff of the channel design.
func (cl *ChannelLimiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-cl.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the refill goroutine. Idempotent.
func (cl *ChannelLimiter) Close() {
	cl.once.Do(func() { close(cl.stop) })
}
```

### The demo: queue-worker semantics in three lines each

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/waitlimiter/limiter"
)

func main() {
	// A worker waits through a refill instead of dropping the job.
	cl := limiter.NewChannelLimiter(1, 50*time.Millisecond)
	defer cl.Close()
	cl.Allow() // drain the burst

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	fmt.Println("channel Wait after drain:", cl.Wait(ctx))

	ml := limiter.NewMutexLimiter(1, 20) // one token every 50ms
	ml.Allow()
	fmt.Println("mutex Wait after drain:", ml.Wait(ctx))

	// No refill and a short deadline: backpressure honors the budget.
	dead := limiter.NewMutexLimiter(1, 0)
	dead.Allow()
	shortCtx, cancelShort := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancelShort()
	fmt.Println("mutex Wait with no refill:", dead.Wait(shortCtx))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
channel Wait after drain: <nil>
mutex Wait after drain: <nil>
mutex Wait with no refill: context deadline exceeded
```

### Tests

All contexts derive from `t.Context()` (Go 1.24), which is canceled
automatically when the test ends — so even a buggy `Wait` that never
returns is eventually unblocked rather than wedging the test binary. The
deadline tests assert both the error *and* promptness (an elapsed-time
upper bound loose enough to never flake, tight enough to catch a `Wait`
that ignores `ctx`). The mid-block cancellation test is the production
shutdown scenario: ten goroutines blocked in `Wait`, one
`context.WithCancelCause` cancel, all ten must return `context.Canceled`
with the sentinel cause recoverable through `context.Cause`.

Create `limiter/wait_test.go`:

```go
package limiter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var errShuttingDown = errors.New("worker pool shutting down")

func TestWaitDeadlineOnDrainedLimiter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wait func(ctx context.Context) error
	}{
		{"mutex no refill", func(ctx context.Context) error {
			l := NewMutexLimiter(1, 0)
			l.Allow()
			return l.Wait(ctx)
		}},
		{"channel slow refill", func(ctx context.Context) error {
			l := NewChannelLimiter(1, time.Hour)
			t.Cleanup(l.Close)
			l.Allow()
			return l.Wait(ctx)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()

			start := time.Now()
			err := tt.wait(ctx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Wait = %v, want context.DeadlineExceeded", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("Wait took %v to honor a 50ms deadline", elapsed)
			}
		})
	}
}

func TestWaitSucceedsOnceRefillLands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wait func(ctx context.Context) error
	}{
		{"mutex", func(ctx context.Context) error {
			l := NewMutexLimiter(1, 50) // one token every 20ms
			l.Allow()
			return l.Wait(ctx)
		}},
		{"channel", func(ctx context.Context) error {
			l := NewChannelLimiter(1, 20*time.Millisecond)
			t.Cleanup(l.Close)
			l.Allow()
			return l.Wait(ctx)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()

			if err := tt.wait(ctx); err != nil {
				t.Fatalf("Wait = %v, want nil (a ~20ms refill fits a 2s budget)", err)
			}
		})
	}
}

func TestWaitPreCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// Both limiters are FULL: Wait must still return the ctx error and,
	// critically, must not consume a token for a canceled request.
	ml := NewMutexLimiter(1, 0)
	if err := ml.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("mutex Wait = %v, want context.Canceled", err)
	}
	if !ml.Allow() {
		t.Fatal("canceled Wait consumed the mutex limiter's token")
	}

	cl := NewChannelLimiter(1, time.Hour)
	defer cl.Close()
	if err := cl.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("channel Wait = %v, want context.Canceled", err)
	}
	if !cl.Allow() {
		t.Fatal("canceled Wait consumed the channel limiter's token")
	}
}

func TestCancelReleasesBlockedWaiters(t *testing.T) {
	t.Parallel()

	l := NewChannelLimiter(1, time.Hour)
	defer l.Close()
	l.Allow() // drained: every Wait below blocks

	ctx, cancel := context.WithCancelCause(t.Context())

	errs := make(chan error, 10)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			errs <- l.Wait(ctx)
		})
	}

	time.Sleep(20 * time.Millisecond) // let the waiters block in select
	cancel(errShuttingDown)
	wg.Wait() // all ten goroutines must come back; a leak hangs here
	close(errs)

	for err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked Wait = %v, want context.Canceled", err)
		}
	}
	if cause := context.Cause(ctx); !errors.Is(cause, errShuttingDown) {
		t.Fatalf("context.Cause = %v, want errShuttingDown", cause)
	}
}
```

## Review

The design lesson to retain is the asymmetry: the channel limiter got
`Wait` for free because a channel is simultaneously the state and the wait
queue, while the mutex limiter needed a loop whose correctness depends on
three separate disciplines — check `ctx.Err()` before touching state, never
block under the lock, and re-validate after every wake. If any of those
feels optional, replay the failure: skip the entry check and canceled
requests drain capacity (the pre-canceled test fails on the follow-up
`Allow`); sleep under the lock and `TestWaitDeadlineOnDrainedLimiter`'s
promptness bound trips because concurrent callers wedge; skip the re-check
and two waiters woken by one token both decrement, driving `tokens`
negative.

Note also what `timer.Stop()` on the cancellation path buys: nothing
correctness-critical (an expired timer just fires into a buffered channel),
but each unstopped timer holds runtime resources until it fires, and a
server canceling thousands of `Wait`s per second should not accumulate
them. Verify with `go test -count=1 -race ./...`; `wg.Wait()` in the
release test doubles as the goroutine-leak check — it can only return if
all ten blocked waiters actually exited.

## Resources

- [context package](https://pkg.go.dev/context) — `Done`, `Err`, `Cause`, `WithCancelCause`, and the cancellation contract.
- [time package: NewTimer and Stop](https://pkg.go.dev/time#NewTimer) — timer lifecycle on early-exit paths.
- [testing package: T.Context](https://pkg.go.dev/testing#T.Context) — the per-test context canceled at test end (Go 1.24).
- [golang.org/x/time/rate: Limiter.Wait](https://pkg.go.dev/golang.org/x/time/rate#Limiter.Wait) — the library version of the same semantics, with fairness via reservations.

---

Prev: [06-per-client-limiter-registry.md](06-per-client-limiter-registry.md) | Back to [00-concepts.md](00-concepts.md) | Next: [08-x-time-rate-migration.md](08-x-time-rate-migration.md)
