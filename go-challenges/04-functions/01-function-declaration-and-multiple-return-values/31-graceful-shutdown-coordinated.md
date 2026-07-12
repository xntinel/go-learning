# Exercise 31: OS Signal Handler With Multi-Stage Shutdown

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Not every shutdown signal deserves the same grace period. An orchestrator's
`SIGTERM` before a rolling deploy should let in-flight requests drain for
thirty seconds; an operator's Ctrl-C wants the process gone in two. This
exercise builds `Coordinator.WaitForSignal(ctx, sigCh) (signal os.Signal,
timeout time.Duration, error)`, mapping each signal to its own grace
period and distinguishing "a signal arrived" from "shutdown was canceled
before one did".

This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
shutdown/                  independent module: example.com/graceful-shutdown-coordinated
  go.mod                   go 1.24
  shutdown.go              package shutdown; ErrSignalChannelClosed; Coordinator; NewCoordinator; WaitForSignal(ctx,sigCh) (signal,timeout,error)
  cmd/
    demo/
      main.go              SIGTERM (long grace), Ctrl-C (short grace), context canceled before any signal
  shutdown_test.go          configured grace; unconfigured falls back to default; context canceled; channel closed; concurrent delivery via unbuffered channel
```

- Files: `shutdown.go`, `cmd/demo/main.go`, `shutdown_test.go`.
- Implement: `(*Coordinator).WaitForSignal(ctx context.Context, sigCh <-chan os.Signal) (signal os.Signal, timeout time.Duration, err error)`, returning the received signal and its configured (or default) grace period, or a nil signal and a wrapped `ctx.Err()`/`ErrSignalChannelClosed` when the context finishes or the channel closes first.
- Test: a signal with a configured grace period returns that exact duration; an unconfigured signal falls back to the coordinator's default; an already-canceled context returns a wrapped `context.Canceled` with a nil signal; a closed channel returns `ErrSignalChannelClosed`; a goroutine blocked in `WaitForSignal` on an *unbuffered* channel only returns once another goroutine actually sends a signal, proving the call really blocks rather than requiring a pre-buffered value.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/01-function-declaration-and-multiple-return-values/31-graceful-shutdown-coordinated/cmd/demo
cd go-solutions/04-functions/01-function-declaration-and-multiple-return-values/31-graceful-shutdown-coordinated
go mod edit -go=1.24
```

### Why the grace period has to travel with the signal

A shutdown handler that only reports "a signal arrived" forces every caller
back into a global constant: one grace period for every reason the process
might be stopping. That is wrong in both directions. Treat every signal
like `SIGTERM` (long grace) and an operator's impatient Ctrl-C now takes
thirty seconds to actually exit. Treat every signal like Ctrl-C (short
grace) and a rolling deploy's `SIGTERM` cuts off in-flight requests that
had time to finish cleanly. `WaitForSignal` returns the timeout *alongside*
the signal specifically so the caller's shutdown sequence — stop accepting
new connections, wait up to `timeout` for in-flight ones to finish, then
force-close — uses the grace period that matches what actually happened,
not a single guess for every case.

The context-cancellation branch matters just as much: a process can be
told to shut down through a path that never touches the signal channel at
all (a supervisor calling a `Shutdown()` RPC, a parent context canceled by
a test). `WaitForSignal` treats that as a distinct, equally valid trigger —
a nil signal with a wrapped `ctx.Err()` — rather than blocking forever
waiting for an OS signal that will never come.

Create `shutdown.go`:

```go
package shutdown

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrSignalChannelClosed is returned when the signal channel is closed
// before any signal arrives -- a programming error upstream (whoever owns
// the channel closed it instead of only ever sending to it).
var ErrSignalChannelClosed = errors.New("shutdown: signal channel closed")

// Coordinator maps each shutdown-triggering signal to how long the
// shutdown sequence should wait for in-flight work to drain before forcing
// a close. Different signals warrant different grace periods: an operator
// SIGINT from a terminal usually wants to stop fast, while an orchestrator
// SIGTERM before a rolling deploy should drain generously.
type Coordinator struct {
	gracePeriods map[os.Signal]time.Duration
	defaultGrace time.Duration
}

// NewCoordinator builds a Coordinator. Any signal not present in
// gracePeriods falls back to defaultGrace.
func NewCoordinator(gracePeriods map[os.Signal]time.Duration, defaultGrace time.Duration) *Coordinator {
	return &Coordinator{gracePeriods: gracePeriods, defaultGrace: defaultGrace}
}

// WaitForSignal blocks until either a signal arrives on sigCh or ctx is
// done, whichever happens first. On a signal, it reports the signal
// received and the grace period configured for it (or the coordinator's
// default, if that signal has no specific entry). On context cancellation
// -- e.g. a parent shutdown sequence already in progress elsewhere -- it
// reports a nil signal, a zero timeout, and the context's error.
func (c *Coordinator) WaitForSignal(ctx context.Context, sigCh <-chan os.Signal) (signal os.Signal, timeout time.Duration, err error) {
	select {
	case sig, ok := <-sigCh:
		if !ok {
			return nil, 0, fmt.Errorf("shutdown: %w", ErrSignalChannelClosed)
		}
		if grace, configured := c.gracePeriods[sig]; configured {
			return sig, grace, nil
		}
		return sig, c.defaultGrace, nil
	case <-ctx.Done():
		return nil, 0, fmt.Errorf("shutdown: %w", ctx.Err())
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
	"os"
	"syscall"
	"time"

	shutdown "example.com/graceful-shutdown-coordinated"
)

func main() {
	coord := shutdown.NewCoordinator(map[os.Signal]time.Duration{
		syscall.SIGTERM: 30 * time.Second, // orchestrator drain before a rolling deploy
		os.Interrupt:    2 * time.Second,  // operator hit Ctrl-C, stop fast
	}, 10*time.Second)

	// SIGTERM arrives: a long, generous drain window.
	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGTERM
	sig, timeout, err := coord.WaitForSignal(context.Background(), sigCh)
	fmt.Printf("received %-8v grace=%-4v err=%v\n", sig, timeout, err)

	// Ctrl-C arrives: a short, urgent drain window.
	sigCh = make(chan os.Signal, 1)
	sigCh <- os.Interrupt
	sig, timeout, err = coord.WaitForSignal(context.Background(), sigCh)
	fmt.Printf("received %-8v grace=%-4v err=%v\n", sig, timeout, err)

	// A shutdown sequence started by an unrelated caller cancels our wait
	// before any signal shows up.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sig, timeout, err = coord.WaitForSignal(ctx, make(chan os.Signal))
	fmt.Printf("received %-8v grace=%-4v err=%v\n", sig, timeout, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
received terminated grace=30s  err=<nil>
received interrupt grace=2s   err=<nil>
received <nil>    grace=0s   err=shutdown: context canceled
```

### Tests

Create `shutdown_test.go`:

```go
package shutdown

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

func testCoordinator() *Coordinator {
	return NewCoordinator(map[os.Signal]time.Duration{
		syscall.SIGTERM: 30 * time.Second,
		os.Interrupt:    2 * time.Second,
	}, 10*time.Second)
}

func TestWaitForSignalConfiguredGrace(t *testing.T) {
	t.Parallel()
	coord := testCoordinator()
	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGTERM

	sig, timeout, err := coord.WaitForSignal(context.Background(), sigCh)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig != syscall.SIGTERM {
		t.Fatalf("signal = %v, want SIGTERM", sig)
	}
	if timeout != 30*time.Second {
		t.Fatalf("timeout = %v, want 30s", timeout)
	}
}

func TestWaitForSignalUnconfiguredFallsBackToDefault(t *testing.T) {
	t.Parallel()
	coord := testCoordinator()
	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGHUP // not in the gracePeriods map

	sig, timeout, err := coord.WaitForSignal(context.Background(), sigCh)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig != syscall.SIGHUP {
		t.Fatalf("signal = %v, want SIGHUP", sig)
	}
	if timeout != 10*time.Second {
		t.Fatalf("timeout = %v, want the 10s default", timeout)
	}
}

func TestWaitForSignalContextCanceledBeforeSignal(t *testing.T) {
	t.Parallel()
	coord := testCoordinator()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sig, timeout, err := coord.WaitForSignal(ctx, make(chan os.Signal))
	if err == nil {
		t.Fatal("want a non-nil error when the context is already done")
	}
	if sig != nil {
		t.Fatalf("signal = %v, want nil", sig)
	}
	if timeout != 0 {
		t.Fatalf("timeout = %v, want 0", timeout)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
}

func TestWaitForSignalChannelClosed(t *testing.T) {
	t.Parallel()
	coord := testCoordinator()
	sigCh := make(chan os.Signal)
	close(sigCh)

	_, _, err := coord.WaitForSignal(context.Background(), sigCh)
	if !errors.Is(err, ErrSignalChannelClosed) {
		t.Fatalf("err = %v, want ErrSignalChannelClosed", err)
	}
}

// TestWaitForSignalConcurrentDelivery proves WaitForSignal correctly
// blocks until another goroutine actually delivers a signal, rather than
// returning early or requiring the signal to already be buffered.
func TestWaitForSignalConcurrentDelivery(t *testing.T) {
	t.Parallel()
	coord := testCoordinator()
	sigCh := make(chan os.Signal) // unbuffered: forces a real rendezvous

	type result struct {
		sig     os.Signal
		timeout time.Duration
		err     error
	}
	done := make(chan result, 1)
	go func() {
		sig, timeout, err := coord.WaitForSignal(context.Background(), sigCh)
		done <- result{sig, timeout, err}
	}()

	sigCh <- os.Interrupt // blocks until WaitForSignal's receive is ready

	res := <-done
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if res.sig != os.Interrupt {
		t.Fatalf("signal = %v, want os.Interrupt", res.sig)
	}
	if res.timeout != 2*time.Second {
		t.Fatalf("timeout = %v, want 2s", res.timeout)
	}
}
```

## Review

`WaitForSignal` is correct when the returned `timeout` always corresponds
to the returned `signal` — never a stale default from a previous call,
never a mismatch between which signal fired and which grace period is
reported. `TestWaitForSignalConfiguredGrace` and
`TestWaitForSignalUnconfiguredFallsBackToDefault` together prove the
lookup-with-fallback logic; `TestWaitForSignalConcurrentDelivery` is the
load-bearing concurrency test — using an *unbuffered* channel forces a real
goroutine handoff, so the test cannot pass by accident with a buggy
implementation that returns before a signal genuinely arrives.

The mistake to avoid is reading the grace period from the map with a plain
index expression (`c.gracePeriods[sig]`) instead of the two-value
comma-ok form. A signal legitimately configured with a `0`-second grace
period (some deployments want SIGKILL-adjacent behavior for a specific
signal) is indistinguishable from "not configured" under a plain index
read, silently substituting the wrong default exactly when the caller
explicitly asked for zero grace.

## Resources

- [os/signal: Notify](https://pkg.go.dev/os/signal#Notify) — how a real program feeds OS signals into the `chan os.Signal` this exercise's `sigCh` parameter models.
- [context.Context](https://pkg.go.dev/context#Context) — the `Done()`/`Err()` pattern for a second, non-signal shutdown trigger.
- [Kubernetes: Pod termination](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination) — the SIGTERM-then-grace-period-then-SIGKILL sequence this coordinator's per-signal timeouts are built to support.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-s3-object-metadata-lookup.md](30-s3-object-metadata-lookup.md) | Next: [32-grpc-metadata-parse-extract.md](32-grpc-metadata-parse-extract.md)
