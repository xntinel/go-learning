# Exercise 8: Dependency Readiness Poll with an Attempt Budget

Waiting for a dependency to come up — the classic "wait-for-postgres" at container
startup, a health probe before a service starts serving — is a condition-only loop
whose termination must be *provable*. It runs until the dependency reports healthy,
but a dependency that never comes up must not hang the loop forever, so the loop
pairs its predicate with a bounded attempt budget and a cancelable wait on a
ticker. This module builds `WaitReady` and tests every exit deterministically with
an injected ticker, no real sleeping.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
readiness/                   module example.com/readiness
  go.mod
  readiness.go               Poller.WaitReady(ctx, check) error; ErrNotReady
  readiness_test.go          healthy on k-th poll, never-ready budget, ctx cancel mid-wait
  cmd/demo/
    main.go                  polls a dependency that becomes healthy on the 3rd check
```

- Files: `readiness.go`, `readiness_test.go`, `cmd/demo/main.go`.
- Implement: `(*Poller).WaitReady(ctx, check func(ctx) error) error` — a bounded loop over `MaxAttempts`, returning `nil` on the first healthy check, `ErrNotReady` after the budget, and `ctx.Err()` on cancel; the inter-poll wait is a `select` on an injected ticker channel and `ctx.Done()`.
- Test: a fake check that becomes healthy on the `k`-th call (assert `nil` after exactly `k` polls), a check that never becomes healthy (assert `ErrNotReady`), and a check plus a pre-cancelled context (assert `ctx.Err()`), all with an injected ticker so nothing sleeps.
- Verify: `go test -count=1 -race ./...`

### A condition loop needs a provable stop and a cancel

A naive readiness wait is `for check() != nil { time.Sleep(interval) }`. It has two
fatal flaws: it never stops if the dependency stays down, and it cannot be
cancelled, so a shutdown during startup hangs. The production shape fixes both. The
loop is bounded by `MaxAttempts`, so after the budget it returns `ErrNotReady`
rather than looping forever — that is the *provable* termination the concepts file
demands. And the wait between polls is a `select` over the ticker channel and
`ctx.Done()`, so a cancelled context ends the wait immediately with `ctx.Err()`.

The ordering inside the loop is deliberate: *check first, then wait*. Each iteration
runs `check(ctx)`; a `nil` result means healthy and returns immediately. Only if the
check failed and there is still budget left does the loop wait for the next tick.
Checking before waiting means a dependency that is already up returns on the first
attempt with no delay — the common case at steady state.

The ticker is injected as a small factory (`NewTicker func(d) (<-chan time.Time,
func())`), defaulting to `time.NewTicker` in production. Injecting it is what makes
the test deterministic: the test supplies a channel it controls, so it can advance
the poll without sleeping and assert the exact number of checks. Taking the ticker
factory at construction — not a mutable setter — keeps the poller free of the
hidden shared state a `SetTicker` would introduce.

Create `readiness.go`:

```go
package readiness

import (
	"context"
	"errors"
	"time"
)

// ErrNotReady means the dependency did not become healthy within the attempt
// budget.
var ErrNotReady = errors.New("dependency not ready within attempt budget")

// Poller waits for a dependency to become healthy, polling on an interval with a
// bounded number of attempts.
type Poller struct {
	Interval    time.Duration
	MaxAttempts int
	// NewTicker, when non-nil, replaces time.NewTicker so tests can inject a
	// controllable channel. It returns the tick channel and a stop function.
	NewTicker func(time.Duration) (<-chan time.Time, func())
}

func (p *Poller) newTicker() (<-chan time.Time, func()) {
	if p.NewTicker != nil {
		return p.NewTicker(p.Interval)
	}
	t := time.NewTicker(p.Interval)
	return t.C, t.Stop
}

// WaitReady polls check until it returns nil (healthy: returns nil), the attempt
// budget is spent (returns ErrNotReady), or ctx is cancelled (returns ctx.Err()).
// It checks before each wait, so an already-healthy dependency returns at once.
func (p *Poller) WaitReady(ctx context.Context, check func(context.Context) error) error {
	attempts := p.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	tick, stop := p.newTicker()
	defer stop()

	for attempt := range attempts {
		if err := check(ctx); err == nil {
			return nil
		}
		if attempt == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick:
		}
	}
	return ErrNotReady
}
```

### The runnable demo

The demo polls a dependency that reports unhealthy twice, then healthy on the third
check, using a short real interval.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/readiness"
)

func main() {
	p := &readiness.Poller{Interval: 20 * time.Millisecond, MaxAttempts: 5}

	checks := 0
	check := func(context.Context) error {
		checks++
		if checks < 3 {
			fmt.Printf("check %d: not ready\n", checks)
			return errors.New("connection refused")
		}
		fmt.Printf("check %d: ready\n", checks)
		return nil
	}

	if err := p.WaitReady(context.Background(), check); err != nil {
		fmt.Printf("gave up: %v\n", err)
		return
	}
	fmt.Printf("dependency ready after %d checks\n", checks)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
check 1: not ready
check 2: not ready
check 3: ready
dependency ready after 3 checks
```

### Tests

The injected ticker is what makes the suite deterministic. For the "healthy on the
k-th poll" and "never ready" cases the test supplies a tick channel pre-filled with
enough ticks that the `select` never blocks, so the outcome is decided purely by
the check-call counter. For the cancellation case the tick channel is left empty
and the context is pre-cancelled, so the `select` takes the `ctx.Done()` branch.

Create `readiness_test.go`:

```go
package readiness

import (
	"context"
	"errors"
	"testing"
	"time"
)

// filledTicker returns a NewTicker factory whose channel already holds n ticks,
// so WaitReady never blocks waiting for one.
func filledTicker(n int) func(time.Duration) (<-chan time.Time, func()) {
	return func(time.Duration) (<-chan time.Time, func()) {
		ch := make(chan time.Time, n)
		for range n {
			ch <- time.Unix(0, 0)
		}
		return ch, func() {}
	}
}

func TestReadyOnKthPoll(t *testing.T) {
	t.Parallel()

	p := &Poller{Interval: time.Second, MaxAttempts: 10, NewTicker: filledTicker(10)}

	calls := 0
	err := p.WaitReady(context.Background(), func(context.Context) error {
		calls++
		if calls < 4 {
			return errors.New("not up")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WaitReady() = %v, want nil", err)
	}
	if calls != 4 {
		t.Fatalf("check called %d times, want exactly 4", calls)
	}
}

func TestReadyOnFirstPoll(t *testing.T) {
	t.Parallel()

	p := &Poller{Interval: time.Second, MaxAttempts: 5, NewTicker: filledTicker(5)}

	calls := 0
	err := p.WaitReady(context.Background(), func(context.Context) error {
		calls++
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("WaitReady() = %v after %d calls, want nil after 1", err, calls)
	}
}

func TestNeverReadyExhaustsBudget(t *testing.T) {
	t.Parallel()

	p := &Poller{Interval: time.Second, MaxAttempts: 5, NewTicker: filledTicker(5)}

	calls := 0
	err := p.WaitReady(context.Background(), func(context.Context) error {
		calls++
		return errors.New("still down")
	})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
	if calls != 5 {
		t.Fatalf("check called %d times, want 5 (the budget)", calls)
	}
}

func TestCancelledMidWait(t *testing.T) {
	t.Parallel()

	// Empty ticker: the select can only proceed via ctx.Done().
	empty := func(time.Duration) (<-chan time.Time, func()) {
		return make(chan time.Time), func() {}
	}
	p := &Poller{Interval: time.Second, MaxAttempts: 10, NewTicker: empty}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.WaitReady(ctx, func(context.Context) error {
		return errors.New("down")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
```

## Review

The poll is correct when its termination is provable: it is bounded by
`MaxAttempts` (returns `ErrNotReady` when the budget is spent, never loops
forever), it returns `nil` on the first healthy check, and it returns `ctx.Err()`
the instant the context is cancelled during a wait. The check runs *before* each
wait, so an already-up dependency returns immediately. The wait is a `select` over
the ticker and `ctx.Done()` — not a bare `time.Sleep`, which could neither be
cancelled nor advanced in a test. The ticker is injected at construction so tests
drive it deterministically; there is no mutable setter to race on.
`TestNeverReadyExhaustsBudget` proves the loop stops at exactly the budget, and
`TestCancelledMidWait` proves the cancel path. Run `go test -count=1 -race ./...`.

## Resources

- [context package](https://pkg.go.dev/context) — `Context.Done` and `Context.Err`, the cancel exit.
- [time.NewTicker](https://pkg.go.dev/time#NewTicker) — the production ticker and its `Stop`.
- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — the cancelable wait.
- [Docker: Control startup order (wait-for-it patterns)](https://docs.docker.com/compose/how-tos/startup-order/) — the real "wait-for-dependency" problem this solves.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-graceful-shutdown-drain.md](07-graceful-shutdown-drain.md) | Next: [09-rangefunc-paginator.md](09-rangefunc-paginator.md)
