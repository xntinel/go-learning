# Exercise 7: A Poller Without Ticker/Timer Leaks

A health-check poller runs a check on a cadence, with a per-attempt timeout. Written
naively it leaks in two ways: `time.After(d)` inside the `select` loop allocates a new
`Timer` every iteration that lives until it fires, and a forgotten `(*Ticker).Stop()`
keeps the ticker's runtime timer alive after the loop ends. This exercise builds a
poller that reuses a single `Ticker` and a single `Timer`, stops both on shutdown, and
proves it leaks nothing under `go.uber.org/goleak`.

This module is self-contained: its own `go mod init`, all code inline, its own demo
and tests. It imports `go.uber.org/goleak`.

## What you'll build

```text
poller/                      independent module: example.com/poller
  go.mod
  poller.go                  type Poller; New, Run (reused Ticker + Timer, ctx-aware)
  cmd/
    demo/
      main.go                runnable demo: poll a healthy check three times
  poller_test.go             stops on cancel (goleak), per-attempt timeout, alloc benchmark
```

- Files: `poller.go`, `cmd/demo/main.go`, `poller_test.go`.
- Implement: `Poller.Run(ctx, onResult)` running a check every `interval` with a `timeout` per attempt, reusing one `Ticker` and one `Timer`, and exiting on `ctx.Done()` after stopping both.
- Test: the poller stops on cancel with no leaked goroutine; a slow check yields `ErrTimeout`; a benchmark contrasts `time.After` allocation with the reused timer.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/poller/cmd/demo
cd ~/go-exercises/poller
go mod init example.com/poller
go get go.uber.org/goleak@v1.3.0
```

### The two timer leaks, and why reuse fixes them

The tempting way to add a per-attempt timeout is inside the loop:

```go
for {
	select {
	case <-ctx.Done():
		return
	case <-ticker.C:
		select {
		case err := <-doCheck():
			report(err)
		case <-time.After(p.timeout): // BUG: a fresh Timer every tick
			report(ErrTimeout)
		}
	}
}
```

`time.After(d)` returns a channel backed by a `Timer` that the runtime keeps alive
until it fires — there is no handle to `Stop` it. On a fast cadence you allocate one
per tick, and each lives for the full `timeout` before the runtime can reclaim it. A
poller ticking every 10ms with a 5s timeout can accumulate hundreds of live timers. It
is a slow leak of runtime timer state and heap, exactly the kind that only shows up as
gradual RSS growth.

The fix is to allocate the timing primitives **once** and reuse them. `Run` creates a
single `time.NewTicker(interval)` and a single `time.NewTimer` that it `Reset`s at the
start of each attempt and `Stop`s when the attempt finishes early. Both are `Stop`ped
by `defer` when `Run` returns, so the loop leaves no runtime timer behind. (Since Go
1.23 a `Timer`/`Ticker` channel is unbuffered and `Stop`/`Reset` no longer require the
old `if !t.Stop() { <-t.C }` drain dance — a stale value is never delivered after
`Stop`.)

The per-attempt check runs in its own goroutine so it can be raced against the timer,
and that goroutine is made leak-safe the same way as Exercise 4: the result channel is
buffered (size 1) so the check can always send even after the attempt gave up, and the
attempt's derived context is cancelled on return so a ctx-aware check exits promptly.

Create `poller.go`:

```go
package poller

import (
	"context"
	"errors"
	"time"
)

// ErrTimeout is reported when a check does not finish within the poller timeout.
var ErrTimeout = errors.New("poller: check timed out")

// CheckFunc performs one health check. It must honor ctx so it can be abandoned
// on timeout or shutdown without leaking.
type CheckFunc func(ctx context.Context) error

// Poller runs a check on a cadence with a per-attempt timeout, reusing a single
// Ticker and a single Timer for the whole run.
type Poller struct {
	interval time.Duration
	timeout  time.Duration
	check    CheckFunc
}

// New returns a Poller that runs check every interval with a per-attempt timeout.
func New(interval, timeout time.Duration, check CheckFunc) *Poller {
	return &Poller{interval: interval, timeout: timeout, check: check}
}

// Run polls until ctx is cancelled, calling onResult with each attempt's outcome.
// It reuses one Ticker and one Timer and stops both on return, so it leaks no
// runtime timer and no goroutine.
func (p *Poller) Run(ctx context.Context, onResult func(error)) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	timer := time.NewTimer(p.timeout)
	timer.Stop() // idle until the first attempt resets it
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			onResult(p.runOnce(ctx, timer))
		}
	}
}

// runOnce races the check against the reused timer. The check runs in a
// goroutine with a buffered result channel and a cancellable context, so it
// never leaks even when the attempt times out.
func (p *Poller) runOnce(ctx context.Context, timer *time.Timer) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	resCh := make(chan error, 1) // buffered: the check can always send and exit
	go func() { resCh <- p.check(cctx) }()

	timer.Reset(p.timeout)
	select {
	case err := <-resCh:
		timer.Stop()
		return err
	case <-timer.C:
		return ErrTimeout
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/poller"
)

func main() {
	healthy := func(ctx context.Context) error { return nil }
	p := poller.New(10*time.Millisecond, 100*time.Millisecond, healthy)

	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan error, 16)
	go p.Run(ctx, func(err error) {
		select {
		case results <- err:
		default:
		}
	})

	healthyCount := 0
	for range 3 {
		if <-results == nil {
			healthyCount++
		}
	}
	cancel()

	fmt.Println("healthy polls:", healthyCount)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
healthy polls: 3
```

### The tests

`TestMain` installs `goleak.VerifyTestMain`, so any leaked ticker-driven goroutine or
check goroutine fails the package. `TestStopsOnCancel` runs the poller, lets several
ticks happen, cancels, and asserts `Run` returns — with goleak confirming the ticker,
timer, and check goroutines are all gone. `TestPerAttemptTimeout` uses a ctx-aware slow
check and asserts the first result is `ErrTimeout`, proving the reused timer fires and
the check goroutine is abandoned safely. `BenchmarkTimeAfterVsReused` documents the
allocation difference; run it with `-benchmem` to see `time.After` allocate a timer per
iteration while the reused timer does not.

Create `poller_test.go`:

```go
package poller

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestStopsOnCancel(t *testing.T) {
	p := New(5*time.Millisecond, 50*time.Millisecond, func(context.Context) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	var polls atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx, func(error) { polls.Add(1) })
	}()

	time.Sleep(30 * time.Millisecond) // let several ticks happen
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if polls.Load() == 0 {
		t.Fatal("expected at least one poll before cancel")
	}
}

func TestPerAttemptTimeout(t *testing.T) {
	slow := func(ctx context.Context) error {
		select {
		case <-time.After(500 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p := New(5*time.Millisecond, 20*time.Millisecond, slow)

	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx, func(err error) {
			select {
			case results <- err:
			default:
			}
		})
	}()

	got := <-results
	cancel()
	<-done

	if !errors.Is(got, ErrTimeout) {
		t.Fatalf("first result = %v, want ErrTimeout", got)
	}
}

func BenchmarkTimeAfterVsReused(b *testing.B) {
	b.Run("time.After", func(b *testing.B) {
		for range b.N {
			t := time.After(time.Hour) // allocates a Timer that outlives the loop
			_ = t
		}
	})
	b.Run("reused", func(b *testing.B) {
		timer := time.NewTimer(time.Hour)
		timer.Stop()
		for range b.N {
			timer.Reset(time.Hour)
			timer.Stop()
		}
	})
}
```

## Review

The poller is correct when it allocates its timing primitives once and stops them on
exit: one `Ticker`, one `Timer`, both `defer`-stopped, and no `time.After` inside the
loop. `TestStopsOnCancel` under `VerifyTestMain` proves the loop and its check
goroutines all exit on cancel; `TestPerAttemptTimeout` proves the reused timer fires
and the slow check is abandoned without leaking, thanks to the buffered result channel
and the cancelled attempt context.

The mistakes to avoid: never put `time.After(d)` in a hot `select` loop — its timer is
un-stoppable and lives until it fires; reuse a `Timer` with `Reset`/`Stop` instead.
Never forget `(*Ticker).Stop()`; the ticker keeps a runtime timer alive after the loop.
And make the per-attempt check ctx-aware with a buffered result channel, or a timed-out
check goroutine leaks. Run under `-race`; the check goroutine and the loop share only
the buffered channel and the context.

## Resources

- [`time.NewTicker`](https://pkg.go.dev/time#NewTicker) and [`(*Ticker).Stop`](https://pkg.go.dev/time#Ticker.Stop) — the reused cadence source and its required stop.
- [`time.NewTimer`](https://pkg.go.dev/time#Timer) — `Reset`/`Stop` for a reused per-attempt timeout.
- [`time.After`](https://pkg.go.dev/time#After) — the doc's own note that the underlying Timer is not recovered until it fires.
- [Go 1.23 release notes: timers](https://go.dev/doc/go1.23#timers) — why `Stop`/`Reset` no longer need channel draining.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-broker-subscriber-unsubscribe.md](06-broker-subscriber-unsubscribe.md) | Next: [08-errgroup-bounded-fanout.md](08-errgroup-bounded-fanout.md)
