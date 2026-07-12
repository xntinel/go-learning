# Exercise 8: Graceful Shutdown: Signal Done, Drain In-Flight, Wait

This is the standard SIGTERM path of a worker service: stop accepting new work, let in-flight work
finish, and bound how long you wait so one stuck worker cannot hang the deployment forever. It is
the done-channel pattern assembled with a `WaitGroup` and a timeout into the three-step shutdown
every production service needs, with a `sync.Once` guard so a double `Shutdown` is safe.

## What you'll build

```text
gracefulrunner/                    independent module: example.com/gracefulrunner
  go.mod
  runner.go                        type Runner; Start(job); Shutdown(timeout) error; ErrShutdownTimeout
  cmd/
    demo/
      main.go                      runnable demo: start workers, shut down, report the drain
  runner_test.go                   drains-in-flight, times-out, double-shutdown-safe; -race
```

Files: `runner.go`, `cmd/demo/main.go`, `runner_test.go`.
Implement: a `Runner` with `Start(job func(done <-chan struct{}))` tracked by a `sync.WaitGroup`, and `Shutdown(timeout time.Duration) error` that closes `done` once (via `sync.Once`), waits for the WaitGroup, and returns `ErrShutdownTimeout` if the drain exceeds the budget.
Test: workers that respect `done` drain to a nil error; a worker that ignores `done` past the budget yields `ErrShutdownTimeout`; calling `Shutdown` twice does not panic.
Verify: `go test -count=1 -race ./...`

### The three ordered steps

`Shutdown` is the whole lesson, in order:

1. Close `done` to signal every worker to stop — once. The `sync.Once` guard matters because
   shutdown is often triggered from more than one place (a signal handler and a `defer`), and a second
   `close(done)` panics with "close of closed channel". `once.Do(func(){ close(r.done) })` makes the
   close idempotent.
2. Wait for in-flight work to drain. Each `Start` did `wg.Add(1)` and its goroutine does
   `defer wg.Done()`, so `wg.Wait()` returns when every worker has exited.
3. Bound the wait. `wg.Wait()` alone has no timeout, so one worker that ignores `done` would hang the
   process on SIGTERM. Run `wg.Wait()` in a goroutine that closes a `drained` channel, then `select`
   between `drained` and `time.After(timeout)`. If the budget expires first, return
   `ErrShutdownTimeout` — the service gives up on the stuck worker rather than hanging forever.

The worker receives `done <-chan struct{}` — receive-only — so it can observe shutdown but cannot
close the channel it does not own. The `Runner` owns `done` and is its sole closer.

Create `runner.go`:

```go
package gracefulrunner

import (
	"errors"
	"sync"
	"time"
)

// ErrShutdownTimeout is returned by Shutdown when in-flight work does not drain
// within the allotted budget.
var ErrShutdownTimeout = errors.New("shutdown: drain exceeded budget")

// Runner supervises background workers and shuts them down gracefully.
type Runner struct {
	done chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// New returns a ready Runner.
func New() *Runner {
	return &Runner{done: make(chan struct{})}
}

// Start launches job as a supervised worker. job must return promptly once its
// done channel is closed.
func (r *Runner) Start(job func(done <-chan struct{})) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		job(r.done)
	}()
}

// Shutdown signals every worker to stop, then waits up to timeout for them to
// drain. It returns nil if all workers exited in time, or ErrShutdownTimeout if
// the budget expired first. Shutdown is safe to call more than once.
func (r *Runner) Shutdown(timeout time.Duration) error {
	r.once.Do(func() { close(r.done) })

	drained := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		return nil
	case <-time.After(timeout):
		return ErrShutdownTimeout
	}
}
```

### The runnable demo

Three workers each respect `done` and record that they drained; the demo shuts them down with a
generous budget and reports the result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync/atomic"
	"time"

	"example.com/gracefulrunner"
)

func main() {
	r := gracefulrunner.New()
	var drained atomic.Int64

	for range 3 {
		r.Start(func(done <-chan struct{}) {
			<-done // do in-flight work until signalled
			drained.Add(1)
		})
	}

	err := r.Shutdown(time.Second)
	fmt.Printf("drained %d workers, err: %v\n", drained.Load(), err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
drained 3 workers, err: <nil>
```

### Tests

`TestShutdownDrainsInFlight` starts workers that block on `done` and increment a counter on exit; a
generous budget yields a nil error and a full count. `TestShutdownTimesOut` starts a worker that
deliberately ignores `done` (it blocks on a separate channel released only by `t.Cleanup`), so a tiny
budget forces `ErrShutdownTimeout`; the cleanup releases the worker afterward so nothing leaks past the
test. `TestDoubleShutdownSafe` calls `Shutdown` twice and asserts neither panics — the `sync.Once`
guard makes the second close a no-op.

Create `runner_test.go`:

```go
package gracefulrunner

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestShutdownDrainsInFlight(t *testing.T) {
	t.Parallel()

	r := New()
	var drained atomic.Int64
	for range 5 {
		r.Start(func(done <-chan struct{}) {
			<-done
			drained.Add(1)
		})
	}

	if err := r.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown returned %v, want nil", err)
	}
	if n := drained.Load(); n != 5 {
		t.Fatalf("drained %d workers, want 5", n)
	}
}

func TestShutdownTimesOut(t *testing.T) {
	t.Parallel()

	r := New()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // let the stuck worker exit after the test

	r.Start(func(done <-chan struct{}) {
		<-release // ignores done: only leaves when released
	})

	err := r.Shutdown(20 * time.Millisecond)
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Shutdown returned %v, want ErrShutdownTimeout", err)
	}
}

func TestDoubleShutdownSafe(t *testing.T) {
	t.Parallel()

	r := New()
	r.Start(func(done <-chan struct{}) { <-done })

	if err := r.Shutdown(time.Second); err != nil {
		t.Fatalf("first Shutdown = %v, want nil", err)
	}
	if err := r.Shutdown(time.Second); err != nil {
		t.Fatalf("second Shutdown = %v, want nil (no panic on double close)", err)
	}
}

func ExampleRunner_Shutdown() {
	r := New()
	r.Start(func(done <-chan struct{}) { <-done })
	fmt.Println(r.Shutdown(time.Second))
	// Output: <nil>
}
```

## Review

The runner is correct when a cooperative worker set drains to nil within budget, a stuck worker forces
`ErrShutdownTimeout` instead of hanging, and a repeated `Shutdown` never panics. The timeout is the
non-negotiable part: `wg.Wait()` with no bound is the classic mistake that turns one misbehaving worker
into a hung SIGTERM, and the `select` against `time.After` is what bounds it. The `sync.Once` guard is
what makes `Shutdown` idempotent — real services trigger it from multiple paths. Run `go test -race`
across `Start` and `Shutdown` to confirm the WaitGroup and the once-guarded close are race-free. Note
that a timed-out worker is still running after `Shutdown` returns; a real service logs it and proceeds,
because the alternative — waiting forever — is worse.

## Resources

- [pkg.go.dev: sync.Once](https://pkg.go.dev/sync#Once)
- [pkg.go.dev: sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [pkg.go.dev: time.After](https://pkg.go.dev/time#After)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-broadcast-tee-subscribers.md](07-broadcast-tee-subscribers.md) | Next: [09-ticker-poller-stop-on-done.md](09-ticker-poller-stop-on-done.md)
