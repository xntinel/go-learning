# Exercise 9: A Background Refresher Started Once and Closed Exactly Once

A long-lived client — a config loader, a credentials cache, an API client with a
token-refresh loop — is usually a shared singleton with a background goroutine
that periodically refreshes state. Two things go wrong under concurrent callers:
the refresh loop gets started twice, or the stop channel gets closed twice
(a panic). `sync.Once` on both ends makes start and close idempotent and
race-safe. This exercise builds that component.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
refresher/                 independent module: example.com/refresher
  go.mod
  refresher.go             type Refresher; New, Start, Close; Once-guarded, AfterFunc cleanup
  cmd/
    demo/
      main.go              runnable demo: start, refresh a few times, close cleanly
  refresher_test.go        concurrent-start-runs-once, concurrent-close-no-panic, AfterFunc tests
```

- Files: `refresher.go`, `cmd/demo/main.go`, `refresher_test.go`.
- Implement: a `Refresher` whose `Start` is idempotent (`sync.Once` guards launching the loop) and whose `Close` is idempotent and concurrency-safe (`sync.Once` guards the channel close), waiting on a `WaitGroup` for the loop to exit; a `context.AfterFunc` wires ctx cancellation to `Close`.
- Test: many concurrent `Start` calls run exactly one loop; many concurrent `Close` calls never double-close and the loop exits; cancelling the context triggers `Close` via `AfterFunc`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

This module uses `sync.WaitGroup.Go`, which requires Go 1.25+, so the setup pins
the module's Go version accordingly.

```bash
mkdir -p go-solutions/13-goroutines-and-channels/11-goroutine-lifecycle-management/09-once-guarded-closer/cmd/demo
cd go-solutions/13-goroutines-and-channels/11-goroutine-lifecycle-management/09-once-guarded-closer
go mod edit -go=1.25
```

### Two Onces, one WaitGroup, and an AfterFunc

The component has four coordination primitives, each with a precise job:

- `startOnce sync.Once` guards launching the refresh loop. However many
  goroutines race to call `Start`, `startOnce.Do` runs its body exactly once, so
  exactly one loop goroutine ever exists. A `starts` atomic counter (incremented
  inside the `Do` body) lets a test *prove* it ran once.
- `closeOnce sync.Once` guards `close(stop)`. Closing a channel twice panics, and
  a shared singleton will have many goroutines calling `Close`; wrapping the close
  in `closeOnce.Do` makes the second and hundredth `Close` no-ops instead of
  panics. This is the idiomatic single-owner-closes-once pattern, made safe under
  concurrency.
- `wg sync.WaitGroup` tracks the loop goroutine. `Start` does `wg.Go(loop)`
  (Go 1.25, which fuses `Add`/`go`/`Done`); `Close` calls `wg.Wait()` after
  signalling, so `Close` returns only once the loop has actually exited — the
  signal-then-wait discipline again. If `Start` was never called, `wg` has a zero
  count and `Wait` returns immediately, so `Close` before `Start` is safe.
- `context.AfterFunc(ctx, r.Close)` registers `Close` to run when `ctx` is
  cancelled, in its own goroutine. This wires the component's teardown to a
  lifecycle context without a bespoke watcher goroutine: cancel the context and
  the refresher stops. `AfterFunc` returns a `stop func() bool` to deregister the
  callback, which `Close` calls (via `closeOnce`) so a manual `Close` cancels the
  pending `AfterFunc`. Because both `AfterFunc` and a manual call route through the
  same `closeOnce`, they cannot double-close no matter which fires first.

The loop itself is a `select` over the `stop` channel and a ticker; on `stop` it
returns (its `wg` slot completes). The `refresh` callback is where a real client
would fetch a new token or reload config.

Create `refresher.go`:

```go
package refresher

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Refresher runs a background loop that periodically calls refresh. It is a
// shared, long-lived component: Start and Close are both idempotent and safe
// to call concurrently from many goroutines.
type Refresher struct {
	interval    time.Duration
	refresh     func()
	stop        chan struct{}
	stopCleanup func() bool // deregisters the AfterFunc

	startOnce sync.Once
	closeOnce sync.Once
	wg        sync.WaitGroup
	starts    atomic.Int64
}

// New builds a Refresher bound to ctx: cancelling ctx calls Close via
// context.AfterFunc. refresh is invoked once per interval by the loop.
func New(ctx context.Context, interval time.Duration, refresh func()) *Refresher {
	r := &Refresher{
		interval: interval,
		refresh:  refresh,
		stop:     make(chan struct{}),
	}
	r.stopCleanup = context.AfterFunc(ctx, func() { r.Close() })
	return r
}

// Start launches the refresh loop. Calling it many times, concurrently, still
// starts exactly one loop.
func (r *Refresher) Start() {
	r.startOnce.Do(func() {
		r.starts.Add(1)
		r.wg.Go(r.loop)
	})
}

func (r *Refresher) loop() {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			if r.refresh != nil {
				r.refresh()
			}
		}
	}
}

// Close stops the refresh loop and waits for it to exit. It is idempotent and
// safe to call concurrently: the channel is closed exactly once, and a Close
// before Start is a no-op wait over a zero WaitGroup.
func (r *Refresher) Close() {
	r.closeOnce.Do(func() {
		r.stopCleanup() // deregister the AfterFunc callback
		close(r.stop)
	})
	r.wg.Wait()
}

// Starts reports how many times the loop was actually launched (0 or 1).
func (r *Refresher) Starts() int64 {
	return r.starts.Load()
}
```

### The runnable demo

The demo starts the refresher, lets it refresh a few times over a short window,
closes it, and reports that it launched exactly one loop and shut down cleanly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/refresher"
)

func main() {
	var refreshes atomic.Int64
	r := refresher.New(context.Background(), 10*time.Millisecond, func() {
		refreshes.Add(1)
	})

	r.Start()
	r.Start() // idempotent: still one loop
	time.Sleep(55 * time.Millisecond)
	r.Close()
	r.Close() // idempotent: no panic

	fmt.Println("loops launched:", r.Starts())
	fmt.Println("refreshed at least once:", refreshes.Load() > 0)
	fmt.Println("closed cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
loops launched: 1
refreshed at least once: true
closed cleanly
```

### Tests

This module is a race-detector magnet by design, so every test runs under `-race`.
`TestConcurrentStartRunsOnce` fires many `Start` calls from separate goroutines
and asserts `Starts() == 1` — the `startOnce` guarantee. `TestConcurrentCloseNoPanic`
starts the loop and then calls `Close` from many goroutines at once; if the close
were not `Once`-guarded, one of them would panic on a double `close(stop)`. The
test asserts no panic and that the loop exited (a second `Close` returns promptly,
proving `wg.Wait` already drained). `TestAfterFuncClosesOnCancel` proves the
context wiring: cancelling the context triggers `Close` through `AfterFunc`, which
the test observes by polling `Starts` state and confirming a subsequent manual
`Close` is a clean no-op.

Create `refresher_test.go`:

```go
package refresher

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestConcurrentStartRunsOnce(t *testing.T) {
	t.Parallel()

	r := New(context.Background(), time.Millisecond, func() {})
	defer r.Close()

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(r.Start)
	}
	wg.Wait()

	if n := r.Starts(); n != 1 {
		t.Fatalf("Starts() = %d, want 1", n)
	}
}

func TestConcurrentCloseNoPanic(t *testing.T) {
	t.Parallel()

	r := New(context.Background(), time.Millisecond, func() {})
	r.Start()

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(r.Close)
	}
	wg.Wait()

	// The loop has exited; a further Close is a clean no-op.
	r.Close()
}

func TestAfterFuncClosesOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	r := New(ctx, time.Millisecond, func() {})
	r.Start()

	cancel() // AfterFunc fires Close in its own goroutine

	// Wait until the loop has stopped: Close blocks on wg.Wait, so once a manual
	// Close returns, the AfterFunc-driven close has taken effect too.
	done := make(chan struct{})
	go func() { r.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not complete after context cancel")
	}
	if n := r.Starts(); n != 1 {
		t.Fatalf("Starts() = %d, want 1", n)
	}
}
```

## Review

The refresher is correct when both ends of its lifecycle are idempotent under
concurrency. `startOnce` guarantees exactly one loop no matter how many goroutines
call `Start`, which `TestConcurrentStartRunsOnce` pins with `Starts() == 1`.
`closeOnce` guarantees the stop channel is closed exactly once, which
`TestConcurrentCloseNoPanic` pins by hammering `Close` from fifty goroutines with
no panic. Routing both the manual `Close` and the `context.AfterFunc` callback
through the same `closeOnce` means they cannot race into a double-close regardless
of ordering. The traps this exercise inoculates against are the two classic
singleton bugs: a second `Start` that orphans the first loop (a leak) and a second
`Close` that panics on a double `close`. Both vanish once each side is guarded by a
`Once`. Run `go test -count=1 -race ./...`; `-race` is the tool that would expose
any unguarded shared state here.

## Resources

- [`sync.Once`](https://pkg.go.dev/sync#Once) — run-exactly-once for idempotent start and close.
- [`context.AfterFunc`](https://pkg.go.dev/context#AfterFunc) — running cleanup when a context is cancelled.
- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) — the Go 1.25 launch-and-track helper.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the close-as-broadcast signalling pattern.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-cancel-cause-diagnostics.md](08-cancel-cause-diagnostics.md) | Next: [10-outbox-relay-drain-on-stop.md](10-outbox-relay-drain-on-stop.md)
