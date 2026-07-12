# Exercise 6: Supervise a Panicking Worker: Recover, Restart, Backoff, Cap

A background consumer that panics on one malformed message must not take the whole
process down with it — but it also must not be restarted in a tight loop that pins
a CPU and floods the logs. The production answer is a supervisor: recover the
panic, restart with exponential backoff, and give up after a bounded budget. This
exercise builds that supervisor.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
supervisor/                independent module: example.com/supervisor
  go.mod
  supervisor.go            Supervise(ctx, cfg, work) error; backoffFor; ErrGaveUp, ErrPanic
  cmd/
    demo/
      main.go              runnable demo: worker panics twice then succeeds
  supervisor_test.go       recovers+succeeds, gives-up-after-budget, ctx-cancel, backoff-grows
```

- Files: `supervisor.go`, `cmd/demo/main.go`, `supervisor_test.go`.
- Implement: `Supervise` that runs `work` in a goroutine, recovers panics into an error, restarts with capped exponential backoff up to `MaxRestarts`, and returns `ErrGaveUp` wrapping the last failure when the budget is spent or the context is cancelled between restarts.
- Test: a worker that panics k times then succeeds is restarted and eventually reports success; an always-panicking worker stops after the budget with a wrapped error identifying the panic; a context cancel stops the supervisor between restarts without leaking the worker; `backoffFor` grows then caps.
- Verify: `go test -count=1 -race ./...`

### Recovering a panic without losing the goroutine

A panic unwinds only its own goroutine, and if it reaches the top unrecovered it
crashes the process. So the recover boundary must be *inside* the goroutine that
runs the worker, at its top, via `defer`. `runOnce` does exactly that: it launches
`work` in a goroutine whose deferred function recovers any panic and turns it into
an error wrapping the `ErrPanic` sentinel. The result — normal error or recovered
panic — flows back on a buffered channel (buffer 1, so the goroutine can always
send its single result and exit even if the supervisor has moved on; an unbuffered
channel would leak the worker goroutine on a timeout path).

There is a subtlety in `runOnce` worth staring at: the goroutine's body is
`done <- work(ctx)`. If `work` returns normally, that send delivers the error (or
nil) and the deferred recover finds nothing. If `work` panics, the expression
`work(ctx)` never produces a value, the send never happens, and the deferred
recover fires and sends the panic error instead. Exactly one value reaches `done`
on either path.

`Supervise` is the restart loop around `runOnce`. On success it returns `nil`. On
failure it records the error, checks the restart budget (`attempt > MaxRestarts`
means give up), and otherwise sleeps a backoff interval — but that sleep is itself
a `select` against `ctx.Done()`, so a shutdown during the backoff window stops the
supervisor promptly instead of waiting out the delay. Backoff is computed by the
pure helper `backoffFor(attempt, base, max)`: `base << attempt`, capped at `max`
(and guarded against the shift overflowing into a non-positive duration). Keeping
backoff a pure function makes it unit-testable without any timing.

Create `supervisor.go`:

```go
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors describing why supervision ended.
var (
	// ErrPanic wraps a value recovered from a worker panic.
	ErrPanic = errors.New("worker panicked")
	// ErrGaveUp is returned when the restart budget is exhausted.
	ErrGaveUp = errors.New("supervisor gave up")
)

// Config tunes the restart policy.
type Config struct {
	MaxRestarts int           // restarts allowed after the first attempt
	BaseBackoff time.Duration // delay before the first restart
	MaxBackoff  time.Duration // cap on the backoff delay
}

// backoffFor returns base << attempt, capped at max. attempt 0 yields base.
func backoffFor(attempt int, base, max time.Duration) time.Duration {
	if attempt < 0 {
		return base
	}
	d := base << uint(attempt)
	if d <= 0 || d > max { // <= 0 catches shift overflow
		return max
	}
	return d
}

// runOnce runs work in a goroutine and returns its error, converting a panic
// into an error wrapping ErrPanic.
func runOnce(ctx context.Context, work func(context.Context) error) (err error) {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("%w: %v", ErrPanic, r)
			}
		}()
		done <- work(ctx)
	}()
	return <-done
}

// Supervise runs work, restarting it with capped exponential backoff when it
// fails or panics, up to cfg.MaxRestarts. It returns nil once work succeeds, or
// ErrGaveUp (wrapping the last failure) when the budget is spent or ctx is
// cancelled between restarts.
func Supervise(ctx context.Context, cfg Config, work func(context.Context) error) error {
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %w", ErrGaveUp, errors.Join(err, lastErr))
		}
		if attempt > cfg.MaxRestarts {
			return fmt.Errorf("%w after %d restarts: %w", ErrGaveUp, cfg.MaxRestarts, lastErr)
		}

		if err := runOnce(ctx, work); err == nil {
			return nil
		} else {
			lastErr = err
		}

		timer := time.NewTimer(backoffFor(attempt, cfg.BaseBackoff, cfg.MaxBackoff))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w: %w", ErrGaveUp, errors.Join(ctx.Err(), lastErr))
		case <-timer.C:
		}
	}
}
```

Note `errors.Join(ctx.Err(), lastErr)`: the give-up error carries *both* why the
supervisor stopped waiting (the context error) and the last worker failure, so
`errors.Is` matches `context.Canceled`, `ErrGaveUp`, and `ErrPanic` all at once —
an operator reading the logs sees the full story.

### The runnable demo

The demo supervises a worker that panics on its first two attempts and succeeds on
the third, with a tiny base backoff so it finishes fast. The worker prints each
attempt so the restart sequence is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/supervisor"
)

func main() {
	attempt := 0
	work := func(ctx context.Context) error {
		attempt++
		if attempt < 3 {
			fmt.Printf("attempt %d: panicking\n", attempt)
			panic("bad message")
		}
		fmt.Printf("attempt %d: ok\n", attempt)
		return nil
	}

	cfg := supervisor.Config{
		MaxRestarts: 5,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  50 * time.Millisecond,
	}
	if err := supervisor.Supervise(context.Background(), cfg, work); err != nil {
		fmt.Println("supervisor:", err)
		return
	}
	fmt.Println("supervisor: worker completed")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempt 1: panicking
attempt 2: panicking
attempt 3: ok
supervisor: worker completed
```

### Tests

`TestRecoversThenSucceeds` proves the core value: a worker that panics twice then
succeeds is restarted and `Supervise` returns `nil`, with the attempt count
confirming it really ran three times. `TestGivesUpAfterBudget` proves the cap: a
worker that always panics stops after `MaxRestarts`, and the returned error
`errors.Is` both `ErrGaveUp` and `ErrPanic`, so the failure is both categorized
and traceable to the panic. `TestContextCancelStops` proves shutdown wins over
restart: with a worker that always panics and a moderate backoff, cancelling the
context makes `Supervise` return promptly with an error that `errors.Is`
`context.Canceled` — and because `runOnce` recovers and its goroutine exits every
attempt, nothing leaks. `TestBackoffGrows` unit-tests the pure schedule:
doubling then capping.

Create `supervisor_test.go`:

```go
package supervisor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecoversThenSucceeds(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int64
	work := func(ctx context.Context) error {
		if attempts.Add(1) < 3 {
			panic("boom")
		}
		return nil
	}
	cfg := Config{MaxRestarts: 5, BaseBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond}

	if err := Supervise(context.Background(), cfg, work); err != nil {
		t.Fatalf("Supervise: %v", err)
	}
	if n := attempts.Load(); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
}

func TestGivesUpAfterBudget(t *testing.T) {
	t.Parallel()

	work := func(ctx context.Context) error { panic("always") }
	cfg := Config{MaxRestarts: 2, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}

	err := Supervise(context.Background(), cfg, work)
	if !errors.Is(err, ErrGaveUp) {
		t.Fatalf("err = %v, want ErrGaveUp", err)
	}
	if !errors.Is(err, ErrPanic) {
		t.Fatalf("err = %v, want it to wrap ErrPanic", err)
	}
}

func TestContextCancelStops(t *testing.T) {
	t.Parallel()

	work := func(ctx context.Context) error { panic("always") }
	cfg := Config{MaxRestarts: 100, BaseBackoff: 50 * time.Millisecond, MaxBackoff: time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := Supervise(ctx, cfg, work)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Supervise took %v, want prompt stop on cancel", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
}

func TestBackoffGrows(t *testing.T) {
	t.Parallel()

	base := 10 * time.Millisecond
	max := 40 * time.Millisecond
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 10 * time.Millisecond},
		{1, 20 * time.Millisecond},
		{2, 40 * time.Millisecond},
		{3, 40 * time.Millisecond},  // capped
		{99, 40 * time.Millisecond}, // shift overflow -> cap
	}
	for _, tc := range cases {
		if got := backoffFor(tc.attempt, base, max); got != tc.want {
			t.Errorf("backoffFor(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}
```

## Review

The supervisor is correct when it satisfies three properties. A transient failure
is survived: a worker that panics then recovers is restarted and eventually
succeeds, and `Supervise` returns `nil`. A permanent failure is bounded: an
always-panicking worker stops after `MaxRestarts` with an error that names both
the give-up and the panic, so it neither crashes the process nor spins forever.
And shutdown is honored between restarts: a cancelled context stops the supervisor
promptly, with no leaked worker goroutine because every attempt's goroutine
recovers and exits. The traps this exercise inoculates against are the two
extremes — no recover at all (one bad message kills the service) and recover with
no backoff (a startup-panicking worker becomes a hot loop). The backoff schedule
being a pure, unit-tested function keeps its correctness independent of any timing
flake. Run `go test -count=1 -race ./...`.

## Resources

- [Go spec: Handling panics](https://go.dev/ref/spec#Handling_panics) — `recover` semantics and where it must run.
- [`time.NewTimer`](https://pkg.go.dev/time#NewTimer) — the cancellable backoff delay and why `Stop` is called.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining the context and worker errors so both match `errors.Is`.
- [`context.Context`](https://pkg.go.dev/context) — cancellation that pre-empts the backoff window.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-goroutine-leak-detection.md](05-goroutine-leak-detection.md) | Next: [07-deadline-bounded-stop.md](07-deadline-bounded-stop.md)
