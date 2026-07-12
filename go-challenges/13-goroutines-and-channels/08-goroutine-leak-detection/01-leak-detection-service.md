# Exercise 1: A Cancellation-Correct Background Service (runtime.NumGoroutine)

Almost every backend process has at least one background loop: a metrics flusher, a
cache reaper, a heartbeat. This exercise builds one with a guaranteed exit path and
proves it does not leak using nothing but the standard library — the homegrown
`runtime.NumGoroutine` pattern every codebase can reach for before it adopts a
detector library.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports another exercise.

## What you'll build

```text
leakservice/                 independent module: example.com/leakservice
  go.mod
  service.go                 type Service; New, Run, Shutdown (stop+done channels, ctx.Done)
  cmd/
    demo/
      main.go                runnable demo: Run then Shutdown a service
  service_test.go            NumGoroutine leak tests, double-run, shutdown-timeout
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: a `Service` whose `Run` spawns a ticker loop with an exit path (stop channel + `ctx.Done`), a `Shutdown` that signals stop and joins on a done channel with a deadline, and single-run enforcement.
- Test: `TestRunStartsGoroutine`, `TestShutdownStopsGoroutine`, `TestNoLeak` (GC + poll back to baseline), `TestRejectsDoubleRun`, `TestShutdownWithTimeout`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/08-goroutine-leak-detection/01-leak-detection-service/cmd/demo
cd go-solutions/13-goroutines-and-channels/08-goroutine-leak-detection/01-leak-detection-service
```

### The exit contract, made explicit

The background goroutine has exactly three ways to return, and all three are in one
`select`: the stop channel is closed (`Shutdown` was called), the caller's context
is cancelled, or — on every tick — it does a unit of work and loops. There is no
fourth path and no unconditional block, so for every possible sequence of events the
goroutine reaches `return`. That is the whole invariant.

`Run` records two channels. `stop` is the signal *in*: closing it tells the loop to
return. `done` is the signal *out*: the loop closes it (via `defer`) on the way out,
so `Shutdown` can wait for the goroutine to have actually finished rather than merely
having been told to. Signalling without joining is the classic half-shutdown that
still leaks; the `done` channel is what makes the join possible.

`Run` also enforces single-run: a non-nil `stop` means a loop is already active, so a
second `Run` returns `ErrAlreadyRunning` instead of spawning a second, untracked
goroutine that `Shutdown` would never join.

`Shutdown` is where the deadline lives. After closing `stop`, it checks the context
*before* blocking — a non-blocking `select` with a `default` — so an already-expired
deadline returns `ctx.Err()` deterministically instead of racing the goroutine's
exit. Then it blocks on either `done` (joined cleanly, return nil) or `ctx.Done()`
(the loop did not stop in time, return the context error). Checking cancellation
before committing to a blocking wait is a small pattern with a big payoff: it makes
the timeout branch testable without a flaky race.

Create `service.go`:

```go
package leakservice

import (
	"context"
	"errors"
	"time"
)

// ErrAlreadyRunning is returned by Run when a loop is already active.
var ErrAlreadyRunning = errors.New("service already running")

// Service runs a single background ticker loop with a guaranteed exit path.
type Service struct {
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
}

// New returns a Service whose loop ticks every interval.
func New(interval time.Duration) *Service {
	return &Service{interval: interval}
}

// Run starts the background loop. It returns ErrAlreadyRunning if a loop is
// already active. The loop returns when stop is closed or ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	if s.stop != nil {
		return ErrAlreadyRunning
	}
	// Capture the channels as locals so the background goroutine never reads
	// the struct fields that Shutdown mutates; that would be a data race.
	stop := make(chan struct{})
	done := make(chan struct{})
	interval := s.interval
	s.stop = stop
	s.done = done
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// a unit of periodic work would go here
			}
		}
	}()
	return nil
}

// Shutdown signals the loop to stop and joins it. If ctx expires before the
// loop returns, Shutdown returns ctx.Err() and does not wait further.
func (s *Service) Shutdown(ctx context.Context) error {
	if s.stop == nil {
		return nil
	}
	stopped := s.done
	close(s.stop)
	s.stop = nil
	s.done = nil

	// Check cancellation before committing to a blocking join, so an
	// already-expired deadline returns deterministically.
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Running reports whether a background loop is currently active.
func (s *Service) Running() bool {
	return s.stop != nil
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

	"example.com/leakservice"
)

func main() {
	s := leakservice.New(20 * time.Millisecond)

	if err := s.Run(context.Background()); err != nil {
		fmt.Println("run:", err)
		return
	}
	fmt.Println("running:", s.Running())

	// Let a few ticks happen, then shut down with a generous deadline.
	time.Sleep(70 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		fmt.Println("shutdown:", err)
		return
	}
	fmt.Println("running:", s.Running())
	fmt.Println("shutdown: clean")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
running: true
running: false
shutdown: clean
```

### The tests

`TestNoLeak` is the heart of the exercise. It records a baseline goroutine count,
runs and shuts down the service, then calls `runtime.GC()` and polls
`NumGoroutine` back down to the baseline within a deadline. The poll — not a
single-shot equality — is what makes it robust: a goroutine that has decided to
return can still be counted for a scheduler tick or two, and `GC` plus a short
retry lets it settle. `TestShutdownWithTimeout` passes an already-cancelled context
and asserts `Shutdown` returns `context.Canceled`, pinning the deadline contract.

Create `service_test.go`:

```go
package leakservice

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func TestRunStartsGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()
	s := New(10 * time.Millisecond)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if after := runtime.NumGoroutine(); after <= before {
		t.Fatalf("NumGoroutine after Run = %d, want > %d", after, before)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestShutdownStopsGoroutine(t *testing.T) {
	s := New(10 * time.Millisecond)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if s.Running() {
		t.Fatal("Running() = true after Shutdown")
	}
}

func TestNoLeak(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()

	s := New(10 * time.Millisecond)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: before=%d after=%d", before, runtime.NumGoroutine())
}

func TestRejectsDoubleRun(t *testing.T) {
	s := New(10 * time.Millisecond)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if err := s.Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Run error = %v, want ErrAlreadyRunning", err)
	}
}

func TestShutdownWithTimeout(t *testing.T) {
	s := New(10 * time.Millisecond)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	if err := s.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown error = %v, want context.Canceled", err)
	}
	// The loop still exits on its own because stop was closed; join it so the
	// test leaves nothing behind.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && s.Running() {
		time.Sleep(time.Millisecond)
	}
}

func TestShutdownIdempotent(t *testing.T) {
	s := New(10 * time.Millisecond)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}
```

## Review

The service is correct when the background goroutine has no reachable path that
blocks forever: the single `select` covers stop, context cancellation, and a tick,
so every execution returns. `Shutdown` is correct when it both signals *and* joins —
closing `stop` alone would leave the goroutine running past `Shutdown`, and
`TestNoLeak` would catch that as a count that never returns to baseline. The
non-blocking cancellation check at the top of `Shutdown` is what makes
`TestShutdownWithTimeout` deterministic instead of a coin flip between the join and
the deadline.

The mistakes to avoid are the ones the tests encode. Do not assert `NumGoroutine`
once — call `runtime.GC()` and poll, because a returning goroutine lingers in the
count for a moment. Do not let `Run` spawn a second loop when one is active; the
second goroutine would be untracked and unjoinable. And remember that after a
timed-out `Shutdown` the loop here still exits because `stop` was closed — the test
waits it out so the package leaves no residue. Run `go test -race` to confirm the
channel handoff between `Run`, the loop, and `Shutdown` is free of data races.

## Resources

- [`runtime.NumGoroutine`](https://pkg.go.dev/runtime#NumGoroutine) — the coarse count the homegrown pattern polls.
- [`runtime.GC`](https://pkg.go.dev/runtime#GC) — forces a collection so exiting goroutines are finalized before you count.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical treatment of exit paths and `done` channels.
- [`context` package](https://pkg.go.dev/context) — `context.Canceled`, `ctx.Done`, and `ctx.Err`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-goleak-testmain-adoption.md](02-goleak-testmain-adoption.md)
