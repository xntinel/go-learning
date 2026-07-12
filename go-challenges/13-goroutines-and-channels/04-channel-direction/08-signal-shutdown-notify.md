# Exercise 8: Graceful Shutdown Driven by a Send-Only Signal Channel

The stdlib itself proves the ownership rule: `signal.Notify(c chan<- os.Signal,
...)` takes a *send-only* channel because the signal package is the producer — it
feeds your channel. You supply a bidirectional channel, hand the send-only end to
`Notify`, and hand the receive-only end to a waiter that blocks until a signal
arrives and runs cleanup.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
shutdown/                    independent module: example.com/shutdown
  go.mod                     go 1.26
  shutdown.go                Install() (<-chan os.Signal, func());
                             WaitForShutdown(sigs <-chan os.Signal, cleanup func()) os.Signal
  cmd/
    demo/
      main.go                runnable demo: self-send a signal, wait, clean up
  shutdown_test.go           waiter returns on signal, cleanup runs once
```

Files: `shutdown.go`, `cmd/demo/main.go`, `shutdown_test.go`.
Implement: `Install()` wiring a buffered channel through `signal.Notify` (send-only) and returning the receive-only end plus a stop function; `WaitForShutdown(sigs <-chan os.Signal, cleanup func()) os.Signal` that blocks, runs cleanup once, and returns the signal.
Test: the waiter returns the fed signal and runs cleanup exactly once — using an injected fake `<-chan os.Signal`, never a real OS signal.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/04-channel-direction/08-signal-shutdown-notify/cmd/demo
cd go-solutions/13-goroutines-and-channels/04-channel-direction/08-signal-shutdown-notify
```

### Why direction and buffering are separate concerns here

`Install` creates a bidirectional `chan os.Signal` with a buffer of one. It passes
that channel to `signal.Notify(c chan<- os.Signal, ...)`, where it is narrowed to
send-only — the signal package will feed it. `Install` then returns the same
channel narrowed to `<-chan os.Signal` for the waiter, plus a stop function that
calls `signal.Stop(c chan<- os.Signal)` (also send-only) to unregister.

Two independent facts are at play, and conflating them is a classic bug.
*Direction* is about who may send, receive, and close: `Notify` and `Stop` want
send-only because the runtime is the sender. *Buffering* is about not dropping the
signal: `signal.Notify` sends non-blocking, so if the waiter is not parked on the
channel at the instant the signal arrives, an *unbuffered* channel drops it. The
fix is `make(chan os.Signal, 1)` — a buffer of one is enough for single-signal
notification. Direction does not save you from the drop; buffering does. Both are
required, for different reasons.

`WaitForShutdown` takes `sigs <-chan os.Signal` — receive-only, because the waiter
only drains. It blocks on a single receive, runs `cleanup` exactly once, and
returns the received signal so the caller can log which signal triggered the
shutdown. Making the waiter take an injected `<-chan os.Signal` (rather than
calling `signal.Notify` itself) is the key to a deterministic test: the test feeds
a fake channel a synthetic `syscall.SIGTERM` and never touches a real OS signal,
which would be flaky and could tear down the test runner.

Create `shutdown.go`:

```go
package shutdown

import (
	"os"
	"os/signal"
)

// Install registers a buffered channel for the given signals and returns the
// receive-only end plus a stop function. The buffer of one is required because
// signal.Notify sends non-blocking; direction (chan<- to Notify/Stop) and
// buffering are separate concerns.
func Install(signals ...os.Signal) (<-chan os.Signal, func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, signals...)
	stop := func() { signal.Stop(c) }
	return c, stop
}

// WaitForShutdown blocks until a signal arrives on sigs, runs cleanup exactly
// once, and returns the signal that triggered shutdown. sigs is receive-only:
// the waiter only drains it.
func WaitForShutdown(sigs <-chan os.Signal, cleanup func()) os.Signal {
	sig := <-sigs
	cleanup()
	return sig
}
```

### The runnable demo

The demo wires `Install`, then self-sends `SIGTERM` to its own process so the
demo is deterministic and does not wait for a human to press Ctrl-C. The waiter
returns the signal and the cleanup prints.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"syscall"

	"example.com/shutdown"
)

func main() {
	sigs, stop := shutdown.Install(syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Self-send SIGTERM so the demo terminates deterministically.
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)

	sig := shutdown.WaitForShutdown(sigs, func() {
		fmt.Println("cleanup: draining connections")
	})
	fmt.Printf("shut down on signal: %s\n", sig)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cleanup: draining connections
shut down on signal: terminated
```

`SIGTERM`'s string form is `terminated`. On a system where the signal name
differs, that final line reflects the local signal string.

### Tests

The unit tests never deliver a real OS signal. They feed `WaitForShutdown` a fake
`<-chan os.Signal` with a synthetic `syscall.SIGTERM` and assert the return value
and that cleanup ran exactly once. This keeps the default test path deterministic
and portable.

Create `shutdown_test.go`:

```go
package shutdown

import (
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestWaitForShutdownReturnsOnSignal(t *testing.T) {
	t.Parallel()

	sigs := make(chan os.Signal, 1)
	sigs <- syscall.SIGTERM

	got := WaitForShutdown(sigs, func() {})
	if got != syscall.SIGTERM {
		t.Fatalf("WaitForShutdown returned %v, want SIGTERM", got)
	}
}

func TestWaitForShutdownRunsCleanupOnce(t *testing.T) {
	t.Parallel()

	sigs := make(chan os.Signal, 1)
	sigs <- syscall.SIGINT

	var calls atomic.Int64
	WaitForShutdown(sigs, func() { calls.Add(1) })
	if got := calls.Load(); got != 1 {
		t.Fatalf("cleanup ran %d times, want 1", got)
	}
}

func TestWaitForShutdownBlocksUntilSignal(t *testing.T) {
	t.Parallel()

	sigs := make(chan os.Signal)
	done := make(chan os.Signal, 1)
	go func() {
		done <- WaitForShutdown(sigs, func() {})
	}()

	select {
	case <-done:
		t.Fatal("WaitForShutdown returned before any signal")
	case <-time.After(50 * time.Millisecond):
	}

	sigs <- syscall.SIGTERM
	select {
	case got := <-done:
		if got != syscall.SIGTERM {
			t.Fatalf("got %v, want SIGTERM", got)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForShutdown did not return after signal")
	}
}
```

## Review

The waiter is correct when it blocks until a signal, runs cleanup exactly once,
and returns the triggering signal. Testing it against an injected `<-chan
os.Signal` is the whole discipline: a unit test that raises a real signal is
flaky and can kill the runner, so the deterministic path feeds a fake channel and
reserves real signals for the demo (which self-sends and is timeout-bounded by
its own termination). The two separate requirements — `chan<-` for
`Notify`/`Stop` and a buffer of one so the non-blocking send is not dropped — are
both present in `Install`; dropping the buffer is the silent bug this design
avoids. Run `go test -race` to confirm the handoff in the blocking test is clean.

## Resources

- [`os/signal`](https://pkg.go.dev/os/signal) — `Notify` and `Stop` take `chan<- os.Signal`; `NotifyContext` for the context variant.
- [`os/signal.Notify`](https://pkg.go.dev/os/signal#Notify) — the note that the channel must be buffered because delivery is non-blocking.
- [`syscall.Signal`](https://pkg.go.dev/syscall#Signal) — `SIGTERM`/`SIGINT` values used as `os.Signal` in the tests.

---

Prev: [07-broker-pubsub-directional.md](07-broker-pubsub-directional.md) | Back to [00-concepts.md](00-concepts.md) | Next: [09-rate-limiter-ticker-refill.md](09-rate-limiter-ticker-refill.md)
