# Exercise 9: Surviving A Double Signal Without Panic Or Double-Close

An impatient operator hits Ctrl+C twice; a fatal-error teardown races an incoming
SIGTERM. Either way, a service's shutdown can be invoked more than once, and it
must survive that without panicking on a closed channel or running teardown twice.
This module hardens the shutdown path with `sync.Once` so a repeated invocation is
a memoized no-op — the original lesson's "your turn" `TestDoubleCancel` promoted to
a full module.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
idempotentshutdown/        module example.com/idempotentshutdown
  go.mod                   go 1.26
  app.go                   App; New, Run, Shutdown (sync.Once guarded), TeardownCount
  cmd/
    demo/
      main.go              signal wiring + a double-triggered shutdown
  app_test.go              double-cancel, concurrent double-shutdown, no double-close panic
```

Files: `app.go`, `cmd/demo/main.go`, `app_test.go`.
Implement: `App` with `Run(ctx) error`, `Shutdown() error` guarded by `sync.Once`, and `TeardownCount() int64`.
Test: a double `cancel()` yields one teardown and no panic; two concurrent `Shutdown` calls run teardown once and both see the same result; a recover-guarded harness proves no "close of closed channel" panic.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/11-graceful-shutdown-with-context/09-idempotent-double-signal-shutdown/cmd/demo
cd go-solutions/14-select-and-context/11-graceful-shutdown-with-context/09-idempotent-double-signal-shutdown
```

## Why sync.Once is the right guard

There are two things a shutdown does that are unsafe to repeat. `context.CancelFunc`
is *not* one of them — calling `cancel()` twice is defined to be a safe no-op, so a
double signal that cancels the root context twice is harmless on its own. The
danger is the *teardown work* that runs in response: closing a channel a second
time panics with "close of closed channel," and re-running the drain (closing a
pool twice, shutting a server twice) is at best wasted work and at worst a crash —
and it happens at exactly the worst moment, when an operator is trying to force a
wedged process to stop.

`sync.Once` is the precise tool. `Shutdown` wraps the whole teardown in
`a.once.Do(func(){ ... })`: the first caller runs it, records the result, and
closes the `stopped` channel once; every subsequent caller — a second signal, a
concurrent invocation from another goroutine — finds the `Once` already fired and
returns immediately. Storing the teardown's error in a field and returning it after
`once.Do` memoizes the result, so all callers observe the same outcome. The
`teardownCount` atomic exists only to prove in a test that the body ran exactly
once no matter how many callers raced in.

`Run` is the thin blocker: it waits on `ctx.Done()` and then calls `Shutdown`. Because
`Shutdown` is idempotent, `Run` returning and an explicit `Shutdown()` from a signal
handler can both fire without conflict. The restored default signal handler still
force-kills on the truly-second *OS* signal; `sync.Once` guards the program's own
drain logic against re-entry, which is a separate concern from the OS escape hatch.

Create `app.go`:

```go
package idempotentshutdown

import (
	"context"
	"sync"
	"sync/atomic"
)

// App runs until its context is cancelled, then tears down exactly once. Shutdown
// is idempotent: a second signal or a concurrent call is a memoized no-op.
type App struct {
	once      sync.Once
	result    error
	teardowns atomic.Int64
	stopped   chan struct{}
	drain     func() error
}

// New returns an App whose drain function performs the real teardown (close
// pools, shut servers). drain runs at most once.
func New(drain func() error) *App {
	return &App{stopped: make(chan struct{}), drain: drain}
}

// Shutdown runs the teardown exactly once and memoizes its result. A repeated
// call returns the same result without re-running teardown or double-closing the
// stopped channel.
func (a *App) Shutdown() error {
	a.once.Do(func() {
		a.teardowns.Add(1)
		a.result = a.drain()
		close(a.stopped) // safe: the Once guarantees this runs once
	})
	return a.result
}

// Run blocks until ctx is cancelled, then shuts down. Safe to pair with an
// explicit Shutdown from a signal handler because Shutdown is idempotent.
func (a *App) Run(ctx context.Context) error {
	<-ctx.Done()
	return a.Shutdown()
}

// TeardownCount reports how many times the teardown body actually ran; it must
// be at most 1 however many times Shutdown is called.
func (a *App) TeardownCount() int64 { return a.teardowns.Load() }

// Stopped is closed once, when teardown completes.
func (a *App) Stopped() <-chan struct{} { return a.stopped }
```

## The runnable demo

The demo wires signal handling (the real double-Ctrl+C path) and then double-triggers
shutdown from two goroutines to show teardown runs exactly once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"os/signal"
	"sync"
	"syscall"

	"example.com/idempotentshutdown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	_ = ctx // in production, app.Run(ctx) blocks here until a signal

	app := idempotentshutdown.New(func() error {
		fmt.Println("draining resources")
		return nil
	})

	// Simulate an impatient operator double-triggering shutdown concurrently.
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = app.Shutdown()
		}()
	}
	wg.Wait()

	fmt.Println("teardowns run:", app.TeardownCount())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
draining resources
teardowns run: 1
```

## Tests

`TestDoubleCancel` is the promoted "your turn": it cancels the root context twice,
asserts `Run` returns exactly one result with no panic, and that teardown ran once.
`TestConcurrentDoubleShutdown` calls `Shutdown` from two goroutines and asserts the
body ran once and both callers observed the same memoized error.
`TestNoDoubleClosePanic` calls `Shutdown` twice under a recover guard and asserts no
"close of closed channel" panic and that `Stopped` is closed.

Create `app_test.go`:

```go
package idempotentshutdown

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestDoubleCancel(t *testing.T) {
	t.Parallel()

	app := New(func() error { return nil })
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	cancel()
	cancel() // double cancel is a safe no-op

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s")
	}
	if got := app.TeardownCount(); got != 1 {
		t.Fatalf("TeardownCount = %d, want 1", got)
	}
}

func TestConcurrentDoubleShutdown(t *testing.T) {
	t.Parallel()

	errClose := errors.New("pool closed")
	app := New(func() error {
		time.Sleep(10 * time.Millisecond) // widen the race window
		return errClose
	})

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = app.Shutdown()
		}()
	}
	wg.Wait()

	if got := app.TeardownCount(); got != 1 {
		t.Fatalf("TeardownCount = %d, want 1 despite concurrent callers", got)
	}
	for i, err := range results {
		if !errors.Is(err, errClose) {
			t.Fatalf("caller %d result = %v, want errClose (memoized)", i, err)
		}
	}
}

func TestNoDoubleClosePanic(t *testing.T) {
	t.Parallel()

	app := New(func() error { return nil })

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Shutdown panicked: %v", r)
			}
		}()
		_ = app.Shutdown()
		_ = app.Shutdown() // would double-close without the sync.Once guard
	}()

	select {
	case <-app.Stopped():
	default:
		t.Fatal("Stopped channel was not closed")
	}
}

func ExampleApp_Shutdown() {
	app := New(func() error { return nil })
	_ = app.Shutdown()
	_ = app.Shutdown() // no-op
	println(app.TeardownCount() == 1)
	// Output:
}
```

## Review

Idempotency is correct when the teardown body runs at most once regardless of how
many callers race in, and every caller sees the same result. `TestDoubleCancel`
covers the common operational case (a second signal). `TestConcurrentDoubleShutdown`
covers the concurrent case with a widened race window and `-race` on, proving the
`sync.Once` serializes the body and memoizes the error. `TestNoDoubleClosePanic`
proves the specific failure the guard prevents — closing the `stopped` channel
twice. The mistakes to avoid: closing a channel or re-running a drain on a second
invocation (the classic "close of closed channel" panic), and conflating the
program's `sync.Once` guard with the OS-level second-signal escape hatch — they are
separate mechanisms, and you want both. Run `go test -race` to exercise the
concurrent double-entry.

## Resources

- [sync.Once](https://pkg.go.dev/sync#Once) — run an action exactly once, the guard for idempotent teardown.
- [context.CancelFunc](https://pkg.go.dev/context#CancelFunc) — safe to call multiple times, so a double cancel is harmless.
- [Go Spec: close](https://go.dev/ref/spec#Close) — closing an already-closed channel panics, which the guard prevents.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-kubernetes-prestop-readiness-drain.md](08-kubernetes-prestop-readiness-drain.md) | Next: [10-drain-queue-then-close-pool.md](10-drain-queue-then-close-pool.md)
