# Exercise 6: Startup Readiness Poller — Wait for DB/Cache Before Serving

A service that binds its HTTP port before Postgres is reachable will serve 500s for
the first few seconds of every deploy. The fix is a boot gate: poll the dependency
until it answers, bounded by a startup deadline, and only then start serving. This
is a `select` over a ticker and `ctx.Done()` — the error-returning, cancellable
generalization of the `PollUntil` from Exercise 1.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
readiness/                  independent module: example.com/readiness
  go.mod                    go 1.26
  readiness.go              WaitReady(ctx, interval, probe) error
  cmd/
    demo/
      main.go               probe that fails twice then succeeds; a deadline miss
  readiness_test.go         first-try, K-failures-then-ok, deadline, prompt cancel
```

- Files: `readiness.go`, `cmd/demo/main.go`, `readiness_test.go`.
- Implement: `WaitReady(ctx context.Context, interval time.Duration, probe func(context.Context) error) error` that returns nil on the first successful probe and `context.Cause(ctx)` when the context is done.
- Test: an immediately-passing probe returns before the first tick; a probe that fails K times then succeeds returns nil after at least K ticks; an always-failing probe under a deadline returns `context.DeadlineExceeded` promptly (near the deadline, not a full interval past it) with a bounded probe count.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/readiness/cmd/demo
cd ~/go-exercises/readiness
go mod init example.com/readiness
go mod edit -go=1.26
```

### Probe once, then tick, and let cancellation win

The shape is a poll loop, but three details make it production-grade rather than a
`time.Sleep` spin. First, `WaitReady` probes *once immediately* before creating any
ticker: a dependency that is already up should not cost a full `interval` of
artificial delay on every boot. Second, the loop is a `select` over `ticker.C` and
`ctx.Done()`, so a cancelled or expired context wakes the loop *at once* rather than
after the current interval elapses — the difference between a shutdown that responds
in microseconds and one that dawdles for the poll interval. Third, it returns
`context.Cause(ctx)`, not a bare sentinel: for a `context.WithTimeout` the cause is
`context.DeadlineExceeded`, for a `context.WithCancelCause` it is whatever cause the
canceller supplied, so the caller learns *why* readiness was abandoned.

The probe itself takes the context so it can honor the deadline too — a probe that
dials Postgres should pass `ctx` to the dial so a slow connect does not blow past
the startup budget. `defer t.Stop()` releases the runtime timer; a `WaitReady`
called once per dependency at boot may seem harmless to leak, but the habit of
`defer t.Stop()` after every `NewTicker` is what keeps long-lived processes from
accumulating timers.

Note the asymmetry with `PollUntil` from Exercise 1: that one returned a bool
(true = predicate met, false = cancelled). `WaitReady` returns an `error`, because a
boot gate that gives up needs to tell the caller the deadline was missed — a service
that cannot reach its database should fail its readiness check loudly, not return a
quiet false.

Create `readiness.go`:

```go
package readiness

import (
	"context"
	"time"
)

// WaitReady polls probe until it returns nil (dependency ready) or ctx is done. It
// probes once immediately, then every interval. On success it returns nil; if ctx
// is cancelled or its deadline passes first, it returns context.Cause(ctx) so the
// caller learns why readiness was abandoned.
func WaitReady(ctx context.Context, interval time.Duration, probe func(context.Context) error) error {
	if err := probe(ctx); err == nil {
		return nil
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-t.C:
			if err := probe(ctx); err == nil {
				return nil
			}
		}
	}
}
```

### The runnable demo

The demo runs two probes. The first fails twice then succeeds, so `WaitReady`
returns nil. The second always fails under a short deadline, so `WaitReady` returns
`context.DeadlineExceeded`.

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
	// A dependency that comes up on the third probe.
	attempts := 0
	probe := func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("connection refused")
		}
		return nil
	}
	err := readiness.WaitReady(context.Background(), 5*time.Millisecond, probe)
	fmt.Printf("ready after %d probes: err=%v\n", attempts, err)

	// A dependency that never comes up, under a startup deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	down := func(context.Context) error { return errors.New("connection refused") }
	err = readiness.WaitReady(ctx, 5*time.Millisecond, down)
	fmt.Println("deadline case:", errors.Is(err, context.DeadlineExceeded))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ready after 3 probes: err=<nil>
deadline case: true
```

### Tests

`TestReadyOnFirstProbe` asserts an already-up dependency returns nil after exactly
one probe, before any ticker fires. `TestReadyAfterKFailures` asserts a probe that
fails K times then succeeds returns nil, with at least K+1 probe calls.
`TestDeadlineReturnsCause` asserts an always-failing probe under a deadline returns
`context.DeadlineExceeded` (via `errors.Is`), promptly (elapsed within a small
window of the deadline, not a whole extra interval), and with a bounded probe count
that proves the ticker stopped rather than spun.

Create `readiness_test.go`:

```go
package readiness

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errDown = errors.New("dependency down")

func TestReadyOnFirstProbe(t *testing.T) {
	t.Parallel()

	calls := 0
	err := WaitReady(context.Background(), time.Hour, func(context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("WaitReady = %v, want nil", err)
	}
	if calls != 1 {
		t.Fatalf("probe called %d times, want 1 (immediate success, no tick)", calls)
	}
}

func TestReadyAfterKFailures(t *testing.T) {
	t.Parallel()

	const k = 3
	calls := 0
	err := WaitReady(context.Background(), 2*time.Millisecond, func(context.Context) error {
		calls++
		if calls <= k {
			return errDown
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WaitReady = %v, want nil", err)
	}
	if calls < k+1 {
		t.Fatalf("probe called %d times, want at least %d", calls, k+1)
	}
}

func TestDeadlineReturnsCause(t *testing.T) {
	t.Parallel()

	const deadline = 60 * time.Millisecond
	const interval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	calls := 0
	start := time.Now()
	err := WaitReady(ctx, interval, func(context.Context) error {
		calls++
		return errDown
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitReady = %v, want context.DeadlineExceeded", err)
	}
	// Prompt: returns near the deadline, not a whole interval past it.
	if elapsed >= deadline+interval {
		t.Fatalf("WaitReady returned after %v, want promptly near %v", elapsed, deadline)
	}
	// Bounded: the ticker did not spin. At most deadline/interval + slack probes.
	if maxCalls := int(deadline/interval) + 3; calls > maxCalls {
		t.Fatalf("probe called %d times, want <= %d (ticker must not busy-loop)", calls, maxCalls)
	}
}
```

## Review

`WaitReady` is correct when readiness is decided solely by the probe and the
deadline: nil the instant the probe succeeds, `context.Cause` the instant the
context is done, and nothing in between advances the outcome. The immediate first
probe is what makes an already-up dependency free; dropping it forces a needless
`interval` of latency onto every boot. Returning `context.Cause(ctx)` rather than a
hand-rolled `errors.New("timeout")` is what lets the caller distinguish a deadline
miss (`context.DeadlineExceeded`) from an operator cancel — a `context.WithTimeout`
sets the cause to `DeadlineExceeded` automatically, which the test asserts with
`errors.Is`. The mistake to avoid is faking the wait with `time.Sleep(interval)` in
a `for`: it cannot be cancelled promptly, so a boot that should abort on SIGTERM
instead sleeps out its current interval, and the `select`-over-`ctx.Done()` form is
what fixes it.

## Resources

- [`context`](https://pkg.go.dev/context) — `WithTimeout`, `Cause`, `Done`, and `DeadlineExceeded`.
- [`context.Cause`](https://pkg.go.dev/context#Cause) — why a cancelled/expired context ended, surfaced to the caller.
- [`time.Ticker`](https://pkg.go.dev/time#Ticker) — `NewTicker`, `Ticker.Stop` for the bounded poll.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-ticker-batch-flusher.md](07-ticker-batch-flusher.md)
