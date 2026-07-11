# Exercise 5: Reproduce and Fix a Goroutine Leak, Gated by goleak

Goroutine leaks are the quiet production killer: no crash, no error log, just a
slowly climbing `runtime.NumGoroutine()` and memory that never comes back until
the pod is OOM-killed. This exercise reproduces the canonical leak — a goroutine
parked forever on a send whose reader went away — then fixes it, and gates the fix
with `go.uber.org/goleak` so a regression fails the test suite.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
leakfix/                   independent module: example.com/leakfix
  go.mod                   requires go.uber.org/goleak
  subscribe.go             Subscribe(ctx, events) <-chan int (cancellation-safe)
  cmd/
    demo/
      main.go              runnable demo: subscribe, receive, cancel, confirm exit
  subscribe_test.go        TestMain goleak gate; leak-free and NumGoroutine tests
```

- Files: `subscribe.go`, `cmd/demo/main.go`, `subscribe_test.go`.
- Implement: `Subscribe` that spawns a goroutine forwarding events to an output channel, with the forwarding send guarded by `ctx.Done()` so the goroutine exits on cancel instead of parking forever.
- Test: `TestMain` calls `goleak.VerifyTestMain(m)`; a functional test exercises the subscriber and passes leak detection; a `NumGoroutine` test shows the count returns to baseline after cancel.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/leakfix/cmd/demo
cd ~/go-exercises/leakfix
go mod init example.com/leakfix
go get go.uber.org/goleak
```

### The leak, and why it happens

Here is the version that leaks. It is *not* compiled into this module (it is shown
in a plain fence so the gate does not assemble it, and so goleak does not fail on
it) — read it, understand the trap, then build the fix:

```
// LEAKY - do not ship. Illustrative only.
func Subscribe(events <-chan int) <-chan int {
	out := make(chan int)
	go func() {
		for e := range events {
			out <- e // parks forever if the caller stops reading out
		}
	}()
	return out
}
```

The forwarding goroutine sends each event on the unbuffered `out`. That send
blocks until someone receives. If the caller reads a few events and then walks
away — an early `return` on an error, a `break` out of a `range`, a request that
gets cancelled — nobody receives from `out` anymore. The goroutine is now parked
on `out <- e` with no way to make progress, and no way to be told to stop. It sits
in the scheduler for the life of the process. Do this once per request on a busy
endpoint and `NumGoroutine` climbs without bound.

The fix threads a `context.Context` into the goroutine and makes *every* blocking
operation a `select` that also watches `ctx.Done()`. There are two blocking points
— receiving from `events` and sending to `out` — and both must be guarded, because
a leak parked on the send is just as permanent as one parked on the receive. When
`ctx` is cancelled, whichever `select` the goroutine is sitting in wakes on
`ctx.Done()` and the goroutine returns, running `defer close(out)` on the way so
downstream receivers also unblock. The caller's contract becomes simple: cancel
the context when you are done, and the goroutine is guaranteed to exit.

Create `subscribe.go`:

```go
package leakfix

import "context"

// Subscribe forwards events onto a new output channel until events is closed or
// ctx is cancelled. Both the receive from events and the send to out are guarded
// by ctx.Done(), so the goroutine can never park forever: cancelling ctx makes
// it exit and close out.
func Subscribe(ctx context.Context, events <-chan int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-events:
				if !ok {
					return // upstream closed
				}
				select {
				case out <- e:
				case <-ctx.Done():
					return // caller stopped reading; do not park
				}
			}
		}
	}()
	return out
}
```

### The runnable demo

The demo subscribes, receives two events, cancels, and confirms the output
channel closes — the observable signature of the goroutine having exited.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/leakfix"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	events := make(chan int)
	out := leakfix.Subscribe(ctx, events)

	go func() {
		for i := 1; i <= 5; i++ {
			events <- i
		}
	}()

	fmt.Println("received:", <-out)
	fmt.Println("received:", <-out)

	cancel() // stop reading; the goroutine must exit, not park

	// Drain until out closes, proving the goroutine returned.
	for range out {
	}
	fmt.Println("subscriber exited cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
received: 1
received: 2
subscriber exited cleanly
```

### Tests

The gate is `goleak.VerifyTestMain(m)` in `TestMain`: after all tests run, goleak
checks that no unexpected goroutine is still alive and fails the run if one is.
It uses `VerifyTestMain` rather than a per-test `goleak.VerifyNone(t)` on purpose —
`VerifyNone` cannot be combined with `t.Parallel()` (it cannot attribute a leaked
goroutine to a specific parallel test), whereas `VerifyTestMain` checks the whole
suite once at the end and coexists with parallel tests. `TestNoLeakOnCancel`
exercises the subscriber and cancels; if `Subscribe` regressed to the leaky
version, the parked goroutine would survive and `VerifyTestMain` would fail.
`TestGoroutineReturnsToBaseline` makes the leak measurable directly: it records
`runtime.NumGoroutine()` before, runs a subscribe/cancel cycle, and polls until
the count returns to baseline — with the leaky version it never would.

Create `subscribe_test.go`:

```go
package leakfix

import (
	"context"
	"runtime"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestNoLeakOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan int)
	out := Subscribe(ctx, events)

	go func() {
		for i := range 10 {
			select {
			case events <- i:
			case <-ctx.Done():
				return
			}
		}
	}()

	if v := <-out; v != 0 {
		t.Fatalf("first event = %d, want 0", v)
	}
	cancel()
	for range out { // drain until closed
	}
}

func TestGoroutineReturnsToBaseline(t *testing.T) {
	base := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan int, 1)
	out := Subscribe(ctx, events)
	events <- 42
	if v := <-out; v != 42 {
		t.Fatalf("event = %d, want 42", v)
	}
	cancel()
	for range out {
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Fatalf("goroutine leaked: baseline=%d now=%d", base, n)
	}
}
```

`TestGoroutineReturnsToBaseline` does not call `t.Parallel()`: `NumGoroutine`
counts *all* goroutines in the process, so a baseline reading would be polluted by
other parallel tests running concurrently. Keeping it serial makes the baseline
meaningful. When goleak needs to ignore a known long-lived background goroutine
(a runtime or third-party one it cannot attribute), `goleak.IgnoreTopFunction(...)`
passed to `VerifyTestMain` suppresses just that one — not needed here, since this
module leaks nothing.

## Review

The subscriber is correct when it can never park permanently: both the receive
from `events` and the send to `out` are inside a `select` that watches
`ctx.Done()`, so cancellation always frees the goroutine. `goleak.VerifyTestMain`
is the ground-truth check — if a future edit drops the `ctx.Done()` guard on the
send, `TestNoLeakOnCancel` leaves a parked goroutine and the suite fails.
`TestGoroutineReturnsToBaseline` gives the same guarantee in a form you can watch:
the process's goroutine count comes back down. The traps this exercise targets are
the two halves of the same mistake — guarding the receive but not the send (a leak
parked on `out <- e`), and assuming that closing the caller's read side somehow
signals the sender (it does not; only the context does). Run `go test -count=1
-race ./...`.

## Resources

- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — `VerifyTestMain`, `VerifyNone`, `IgnoreTopFunction`.
- [`runtime.NumGoroutine`](https://pkg.go.dev/runtime#NumGoroutine) — the live goroutine count used to observe a leak.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical treatment of leak-free channel forwarding.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-context-worker-pool.md](04-context-worker-pool.md) | Next: [06-supervisor-restart-backoff.md](06-supervisor-restart-backoff.md)
