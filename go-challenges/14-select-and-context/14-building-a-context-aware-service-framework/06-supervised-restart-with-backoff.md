# Exercise 6: Supervising a Flaky Service with Capped Backoff and Jitter

Some dependencies fail and recover on their own — a broker that restarts, an
upstream that sheds load. A supervisor restarts such a component when its run
loop exits with an error, but a naive retry loop is a self-inflicted outage. This
module builds the correct one: exponential backoff with a cap, full jitter to
avoid a synchronized reconnect storm, a max-retries limit, a stable-uptime reset
window, and every wait selecting on `ctx.Done()` so a shutdown mid-backoff exits
instantly.

## What you'll build

```text
supervisor/                   independent module: example.com/supervisor
  go.mod                      go 1.26
  supervisor.go               Runnable, BackoffConfig, backoffCap, jitter, Supervisor.Run
  supervisor_test.go          restart count, growing cap, cancel-mid-backoff, max-retries fatal
  cmd/
    demo/
      main.go                 supervises a connector that fails twice then serves
```

Files: `supervisor.go`, `cmd/demo/main.go`, `supervisor_test.go`.
Implement: `Supervisor.Run` that calls `Runnable.Run`, and on a non-nil error waits `jitter(backoffCap(attempt))` — a `select` on a timer and `ctx.Done()` — then retries; resets the attempt counter after a stable-uptime window; returns a wrapped fatal error once retries are exhausted.
Test: a `Runnable` that fails N times then succeeds is restarted exactly N times; `backoffCap` is monotonic and capped; cancelling `ctx` during a backoff returns promptly; exceeding max-retries surfaces a wrapped fatal error.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/14-building-a-context-aware-service-framework/06-supervised-restart-with-backoff/cmd/demo
cd go-solutions/14-select-and-context/14-building-a-context-aware-service-framework/06-supervised-restart-with-backoff
```

### The supervision loop, piece by piece

The supervised unit is a `Runnable` whose `Run(ctx)` *blocks* until it either
finishes cleanly (returns `nil`), fails (returns a non-nil error), or is
cancelled. The supervisor loops: it times the run, and when `Run` returns an
error it decides whether to retry.

Three policies shape that decision. **Capped exponential backoff**: the base
delay doubles each attempt (`base << (attempt-1)`) but never exceeds `Max`, so a
persistently-down dependency is retried on a bounded schedule, not hammered and
not delayed for hours. **Full jitter**: the actual sleep is a uniform random
duration in `(0, cap]`, which decorrelates a fleet of replicas that all lost the
same dependency at the same instant — without it they retry in lockstep and knock
the dependency straight back down. **Stable-uptime reset**: if a run lasted at
least `ResetAfter` before failing, the attempt counter resets, so a component
that was healthy for an hour does not inherit backoff from an unrelated failure a
day ago.

Two correctness details. The backoff wait is a `select` on both a `time.Timer`
and `ctx.Done()`, so a shutdown arriving mid-backoff returns immediately rather
than blocking until the timer fires — otherwise the supervisor goroutine leaks
and shutdown stalls. And a cancellation always wins: if `ctx` is done, the
supervisor returns `context.Cause(ctx)` regardless of what `Run` returned, so a
`Run` that returns its own `ctx.Err()` on shutdown is treated as a clean stop,
not a crash to retry.

Backoff and jitter are separated into pure functions so the exponential schedule
can be unit-tested deterministically while the randomized sleep is tested for its
*bound*. The sleep itself is injectable so tests can drive the loop without real
delays.

Create `supervisor.go`:

```go
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
)

// ErrRetriesExhausted classifies a supervised run that gave up after exceeding
// its retry budget.
var ErrRetriesExhausted = errors.New("supervisor: retries exhausted")

// Runnable is a long-running unit the supervisor manages. Run blocks until it
// finishes cleanly (nil), fails (non-nil error), or ctx is cancelled.
type Runnable interface {
	Name() string
	Run(ctx context.Context) error
}

// BackoffConfig tunes the restart policy.
type BackoffConfig struct {
	Base       time.Duration // first-retry delay ceiling; doubles each attempt
	Max        time.Duration // cap on the delay ceiling
	MaxRetries int           // retries allowed before giving up (0 = none)
	ResetAfter time.Duration // uptime after which the attempt counter resets (0 = never)
}

// backoffCap returns the capped exponential delay ceiling for a 1-based attempt.
// The actual sleep is a jittered fraction of this value.
func backoffCap(attempt int, base, maxDelay time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if base <= 0 {
		return maxDelay
	}
	// Guard the shift against overflow: beyond ~62 doublings, use maxDelay.
	if attempt-1 >= 62 {
		return maxDelay
	}
	d := base << (attempt - 1)
	if d <= 0 || d > maxDelay {
		return maxDelay
	}
	return d
}

// jitter returns a uniform random duration in (0, ceiling] (full jitter). A
// non-positive ceiling yields 0.
func jitter(rng *rand.Rand, ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(rng.Int64N(int64(ceiling)) + 1)
}

// Supervisor restarts a Runnable per its BackoffConfig.
type Supervisor struct {
	R   Runnable
	Cfg BackoffConfig
	Rng *rand.Rand
	Log *slog.Logger

	// sleep is the backoff wait, injectable for tests; nil means the real
	// timer-and-ctx select. onSleep, if set, observes each chosen delay.
	sleep   func(ctx context.Context, d time.Duration) error
	onSleep func(time.Duration)
}

func (s *Supervisor) sleepFor(ctx context.Context, d time.Duration) error {
	if s.sleep != nil {
		return s.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}

// Run supervises the Runnable until it stops cleanly, ctx is cancelled, or the
// retry budget is exhausted (returning a wrapped ErrRetriesExhausted).
func (s *Supervisor) Run(ctx context.Context) error {
	attempts := 0
	for {
		start := time.Now()
		err := s.R.Run(ctx)

		if ctx.Err() != nil {
			return context.Cause(ctx) // shutdown wins over any Run error
		}
		if err == nil {
			return nil // clean stop
		}

		if s.Cfg.ResetAfter > 0 && time.Since(start) >= s.Cfg.ResetAfter {
			attempts = 0
		}
		attempts++
		if attempts > s.Cfg.MaxRetries {
			return fmt.Errorf("%w: %s after %d retries: %w",
				ErrRetriesExhausted, s.R.Name(), s.Cfg.MaxRetries, err)
		}

		delay := jitter(s.Rng, backoffCap(attempts, s.Cfg.Base, s.Cfg.Max))
		if s.onSleep != nil {
			s.onSleep(delay)
		}
		s.Log.Warn("supervised run failed; backing off",
			"name", s.R.Name(), "attempt", attempts, "delay", delay, "err", err)
		if err := s.sleepFor(ctx, delay); err != nil {
			return err // cancelled mid-backoff
		}
	}
}
```

### The runnable demo

The demo supervises a database connector that fails its first two dial attempts,
then connects and serves until the root context is cancelled. Backoff base is a
few milliseconds so the demo runs quickly; the random number generator is seeded
deterministically so the run is reproducible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"example.com/supervisor"
)

type connector struct {
	mu        sync.Mutex
	failsLeft int
}

func (c *connector) Name() string { return "db-connector" }

func (c *connector) Run(ctx context.Context) error {
	c.mu.Lock()
	fail := c.failsLeft > 0
	if fail {
		c.failsLeft--
	}
	c.mu.Unlock()

	if fail {
		fmt.Println("connector: dial failed")
		return errors.New("dial tcp: connection refused")
	}
	fmt.Println("connector: connected, serving")
	<-ctx.Done()
	fmt.Println("connector: context cancelled, exiting")
	return ctx.Err()
}

func main() {
	sup := &supervisor.Supervisor{
		R:   &connector{failsLeft: 2},
		Cfg: supervisor.BackoffConfig{Base: 5 * time.Millisecond, Max: time.Second, MaxRetries: 5, ResetAfter: time.Second},
		Rng: rand.New(rand.NewPCG(1, 2)),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(120 * time.Millisecond)
		cancel()
	}()

	err := sup.Run(ctx)
	fmt.Println("supervisor exited:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
connector: dial failed
connector: dial failed
connector: connected, serving
connector: context cancelled, exiting
supervisor exited: context canceled
```

### Tests

The tests are the same package, so they set the unexported `sleep` and `onSleep`
hooks directly. `TestRestartsThenSucceeds` injects a `Runnable` that fails three
times then returns `nil`, with an instant recording sleep; it asserts `Run` was
called four times, backoff ran three times, and each recorded delay fell within
`(0, backoffCap]`. `TestBackoffCapMonotonic` unit-tests the pure schedule.
`TestCancelDuringBackoff` uses the *real* timer sleep with a one-second base, then
cancels mid-backoff and asserts `Run` returns promptly with `context.Canceled`.
`TestMaxRetriesFatal` asserts an always-failing runnable surfaces a wrapped
`ErrRetriesExhausted`.

Create `supervisor_test.go`:

```go
package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testRng() *rand.Rand { return rand.New(rand.NewPCG(1, 2)) }

type scriptedRunnable struct {
	mu    sync.Mutex
	calls int
	fails int // number of leading calls that return an error
	err   error
}

func (r *scriptedRunnable) Name() string { return "scripted" }

func (r *scriptedRunnable) Run(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls <= r.fails {
		return r.err
	}
	return nil // succeed once past the scripted failures
}

func TestRestartsThenSucceeds(t *testing.T) {
	t.Parallel()

	r := &scriptedRunnable{fails: 3, err: errors.New("dial refused")}
	var delays []time.Duration

	sup := &Supervisor{
		R:       r,
		Cfg:     BackoffConfig{Base: 10 * time.Millisecond, Max: time.Second, MaxRetries: 10},
		Rng:     testRng(),
		Log:     testLogger(),
		sleep:   func(context.Context, time.Duration) error { return nil }, // instant
		onSleep: nil,
	}
	sup.onSleep = func(d time.Duration) { delays = append(delays, d) }

	if err := sup.Run(context.Background()); err != nil {
		t.Fatalf("Run: err = %v, want nil after eventual success", err)
	}
	if r.calls != 4 {
		t.Fatalf("Run called %d times, want 4 (3 failures + 1 success)", r.calls)
	}
	if len(delays) != 3 {
		t.Fatalf("backoff waited %d times, want 3", len(delays))
	}
	for i, d := range delays {
		ceiling := backoffCap(i+1, sup.Cfg.Base, sup.Cfg.Max)
		if d <= 0 || d > ceiling {
			t.Fatalf("delay[%d] = %v, want in (0, %v]", i, d, ceiling)
		}
	}
}

func TestBackoffCapMonotonic(t *testing.T) {
	t.Parallel()

	base := 100 * time.Millisecond
	max := 2 * time.Second
	var prev time.Duration
	for attempt := 1; attempt <= 10; attempt++ {
		got := backoffCap(attempt, base, max)
		if got < prev {
			t.Fatalf("backoffCap(%d) = %v < previous %v; must be non-decreasing", attempt, got, prev)
		}
		if got > max {
			t.Fatalf("backoffCap(%d) = %v exceeds max %v", attempt, got, max)
		}
		prev = got
	}
	if backoffCap(1, base, max) != base {
		t.Fatalf("backoffCap(1) = %v, want base %v", backoffCap(1, base, max), base)
	}
}

func TestCancelDuringBackoff(t *testing.T) {
	t.Parallel()

	r := &scriptedRunnable{fails: 100, err: errors.New("always down")}
	sup := &Supervisor{
		R:   r,
		Cfg: BackoffConfig{Base: time.Second, Max: time.Minute, MaxRetries: 100},
		Rng: testRng(),
		Log: testLogger(),
		// real timer-based sleep, so cancellation must interrupt it
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	time.Sleep(20 * time.Millisecond) // let it enter the first backoff
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return promptly after cancel during backoff")
	}
}

func TestMaxRetriesFatal(t *testing.T) {
	t.Parallel()

	r := &scriptedRunnable{fails: 100, err: errors.New("permanent failure")}
	sup := &Supervisor{
		R:     r,
		Cfg:   BackoffConfig{Base: time.Millisecond, Max: time.Second, MaxRetries: 2},
		Rng:   testRng(),
		Log:   testLogger(),
		sleep: func(context.Context, time.Duration) error { return nil },
	}

	err := sup.Run(context.Background())
	if !errors.Is(err, ErrRetriesExhausted) {
		t.Fatalf("Run: err = %v, want wrapping ErrRetriesExhausted", err)
	}
	if r.calls != 3 {
		t.Fatalf("Run called %d times, want 3 (initial + 2 retries)", r.calls)
	}
}
```

## Review

The supervisor is correct when the restart count matches the failure script, the
backoff schedule is monotonic and capped, cancellation is honored mid-backoff,
and an exhausted budget surfaces a wrapped `ErrRetriesExhausted`. The two traps
this module is built to avoid: a backoff sleep that ignores `ctx.Done()` (the
`TestCancelDuringBackoff` test fails if `sleepFor` selects only on the timer), and
retries with no cap or no jitter (the cap is proven monotonic-and-bounded, and
`jitter` returns a value strictly inside `(0, cap]` so the fleet decorrelates).
Note that `Run` checks `ctx.Err()` *before* inspecting the run error, so a
`Runnable` that returns its own `ctx.Err()` on shutdown is treated as a clean stop
rather than a failure to retry — getting that order wrong makes shutdown look
like a crash and triggers a pointless final backoff. Run `go test -race`; the
scripted runnable's mutex guards its call counter under the supervisor goroutine.

## Resources

- [math/rand/v2](https://pkg.go.dev/math/rand/v2) — `rand.New`, `rand.NewPCG`, and `Rand.Int64N` for seeded, testable jitter.
- [AWS Architecture Blog: Exponential Backoff And Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — why full jitter beats plain exponential backoff.
- [context.Cause](https://pkg.go.dev/context#Cause) — retrieving the cancellation reason a shutdown carries.
- [time.NewTimer](https://pkg.go.dev/time#NewTimer) — the select-able backoff timer.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-readiness-and-health-aggregation.md](05-readiness-and-health-aggregation.md) | Next: [07-bounded-startup-with-deadline-cause.md](07-bounded-startup-with-deadline-cause.md)
