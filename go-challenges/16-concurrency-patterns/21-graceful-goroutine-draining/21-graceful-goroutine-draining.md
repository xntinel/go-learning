# 21. Graceful Goroutine Draining

A hard kill (`kill -9`, `os.Exit`) discards everything in flight: half-written files,
uncommitted transactions, unacknowledged messages. Graceful draining instead moves through
three ordered phases: stop accepting new work, wait for in-flight work to finish, then exit.
This lesson builds a reusable `Pool` that implements those phases with a configurable drain
deadline.

```text
drain/
  go.mod
  internal/drain/drain.go
  internal/drain/drain_test.go
  cmd/demo/main.go
```

## Concepts

### Stop-Accepting vs Drain-in-Flight vs Force-Exit

These are three distinct phases with different goals.

Stop-accepting: the pool closes its intake; new `Submit` calls return an error immediately.
This is cheap — close a channel or flip a flag. In-flight work is not interrupted.

Drain-in-flight: the pool waits for all goroutines that are currently executing a job to
finish. `sync.WaitGroup.Wait()` does this. The wait can take as long as the longest running
job, so a timeout is mandatory in production.

Force-exit: if the drain deadline passes before all goroutines finish, the pool returns an
error and the caller decides what to do. The pool must not block the caller past the deadline.
Context cancellation delivers this: `select { case <-done: return nil; case <-ctx.Done(): return ctx.Err() }`.

### WaitGroup as the Join Barrier

`sync.WaitGroup` is the canonical join barrier for goroutines. Each goroutine calls
`wg.Add(1)` before starting (not inside the goroutine — the goroutine might not run before
the parent calls `Wait`) and `wg.Done()` on exit via `defer`. `wg.Wait()` blocks until the
counter reaches zero.

The mistake is calling `wg.Add(1)` inside the goroutine: if the goroutine hasn't started by
the time `Wait` is called, the counter is still zero and `Wait` returns early.

### Closing a Channel to Broadcast Shutdown

Closing a channel is a broadcast: every goroutine receiving on the closed channel
immediately unblocks and receives the zero value. This is the right primitive for
signalling N goroutines to stop accepting new jobs, because a send on a channel would only
wake one goroutine at a time.

Worker loops use `for job := range p.jobs { ... }`. When the jobs channel is closed, the
range loop exits naturally after draining any buffered items — which is exactly the drain
phase. No extra done channel is needed for the workers themselves.

### Idempotent Shutdown

`Shutdown` may be called multiple times by accident (deferred calls, signal handlers). A
double close of a channel panics. The solution is a `sync.Once` that executes the close
exactly once regardless of how many callers race to shut down.

### Context Timeout for the Drain Deadline

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
err := pool.Shutdown(ctx)
```

The pool wraps `wg.Wait()` in a goroutine and selects on its completion channel and
`ctx.Done()`. If the deadline fires first, `Shutdown` returns `ctx.Err()` (which is
`context.DeadlineExceeded`). The caller can log the unfinished goroutines, try again with
a longer timeout, or just exit.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/drain/internal/drain ~/go-exercises/drain/cmd/demo
cd ~/go-exercises/drain
go mod init example.com/drain
```

### Exercise 1: The Pool

Create `internal/drain/drain.go`:

```go
package drain

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// ErrPoolClosed is returned by Submit when the pool has been shut down.
var ErrPoolClosed = errors.New("drain: pool is closed")

// Pool is a fixed-size worker pool that supports graceful draining.
// The zero value is not usable; use New.
type Pool struct {
	jobs   chan func()
	wg     sync.WaitGroup
	once   sync.Once
	closed atomic.Bool
}

// New creates a Pool with numWorkers goroutines and a job queue of size queueSize.
// Both must be positive.
func New(numWorkers, queueSize int) *Pool {
	p := &Pool{
		jobs: make(chan func(), queueSize),
	}
	for i := 0; i < numWorkers; i++ {
		p.wg.Add(1)
		go p.run()
	}
	return p
}

func (p *Pool) run() {
	defer p.wg.Done()
	for job := range p.jobs {
		job()
	}
}

// Submit enqueues job for execution. It returns ErrPoolClosed if the pool has
// been shut down, or blocks until there is space in the queue.
func (p *Pool) Submit(job func()) error {
	if p.closed.Load() {
		return ErrPoolClosed
	}
	p.jobs <- job
	return nil
}

// Shutdown stops the pool from accepting new jobs and waits for all in-flight
// jobs to complete. If ctx expires before draining is complete, Shutdown
// returns ctx.Err(). Shutdown is safe to call multiple times.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.once.Do(func() {
		p.closed.Store(true)
		close(p.jobs)
	})

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

Setting `closed` before closing the channel avoids a send-on-closed-channel panic. The
`sync.Once` ensures the close happens exactly once regardless of concurrent `Shutdown` calls.

### Exercise 2: Table-Driven Tests

Create `internal/drain/drain_test.go`:

```go
package drain_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"example.com/drain/internal/drain"
)

func TestPoolDrainsCleanly(t *testing.T) {
	t.Parallel()

	p := drain.New(2, 10)
	var count atomic.Int64

	for i := 0; i < 5; i++ {
		if err := p.Submit(func() {
			time.Sleep(10 * time.Millisecond)
			count.Add(1)
		}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := count.Load(); got != 5 {
		t.Errorf("completed = %d, want 5", got)
	}
}

func TestPoolShutdownTimeoutReturnsError(t *testing.T) {
	t.Parallel()

	p := drain.New(1, 5)
	// Submit a job that takes longer than the drain timeout.
	if err := p.Submit(func() {
		time.Sleep(500 * time.Millisecond)
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := p.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown error = %v, want DeadlineExceeded", err)
	}
}

func TestSubmitAfterShutdownReturnsError(t *testing.T) {
	t.Parallel()

	p := drain.New(1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if err := p.Submit(func() {}); !errors.Is(err, drain.ErrPoolClosed) {
		t.Errorf("Submit after Shutdown = %v, want ErrPoolClosed", err)
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	t.Parallel()

	p := drain.New(2, 4)
	ctx := context.Background()

	// Call Shutdown three times; none should panic or hang.
	for i := 0; i < 3; i++ {
		if err := p.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown call %d: %v", i+1, err)
		}
	}
}

func ExampleNew() {
	p := drain.New(2, 10)

	done := make(chan struct{})
	if err := p.Submit(func() {
		close(done)
	}); err != nil {
		panic(err)
	}

	ctx := context.Background()
	if err := p.Shutdown(ctx); err != nil {
		panic(err)
	}
	<-done
	// Output:
}
```

### Exercise 3: Demo Binary

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/drain/internal/drain"
)

func main() {
	p := drain.New(4, 20)
	var completed atomic.Int64

	for i := 0; i < 12; i++ {
		id := i
		if err := p.Submit(func() {
			time.Sleep(30 * time.Millisecond)
			completed.Add(1)
			fmt.Printf("job %d done\n", id)
		}); err != nil {
			fmt.Printf("submit error: %v\n", err)
		}
	}

	fmt.Println("shutting down pool")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := p.Shutdown(ctx); err != nil {
		fmt.Printf("drain error: %v\n", err)
	} else {
		fmt.Printf("drained cleanly: %d jobs completed\n", completed.Load())
	}
}
```

## Common Mistakes

### Calling wg.Add(1) Inside the Goroutine

Wrong: `go func() { wg.Add(1); defer wg.Done(); ... }()`

What happens: the goroutine may not be scheduled before the parent calls `wg.Wait()`. The
counter is still zero; `Wait` returns immediately and the parent exits while goroutines are
still running.

Fix: call `wg.Add(1)` in the parent before launching the goroutine. The goroutine's `defer
wg.Done()` matches it.

### Double-Closing the Jobs Channel

Wrong: calling `close(p.jobs)` directly in `Shutdown` and allowing multiple callers.

What happens: the second close panics with "close of closed channel".

Fix: wrap the close in `sync.Once`. The first caller closes the channel; subsequent callers
are no-ops.

### Blocking Submit Indefinitely After Shutdown

Wrong: `p.jobs <- job` with no closed-channel check.

What happens: after `Shutdown` closes the channel, a blocked send panics. If the channel is
full and not yet closed, `Submit` blocks forever with no way to unblock.

Fix: set a `closed` atomic flag to true before closing the channel. `Submit` checks the flag
first and returns `ErrPoolClosed` immediately.

### Ignoring the Drain Timeout Error

Wrong: `pool.Shutdown(ctx)` without checking the return value.

What happens: if draining times out, the caller assumes all work completed. Goroutines may
still be running after the function returns, accessing resources that the caller has already
cleaned up.

Fix: always check `if err := pool.Shutdown(ctx); err != nil` and handle
`context.DeadlineExceeded` explicitly — typically by logging the in-flight count and exiting.

## Verification

From `~/go-exercises/drain`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. `TestPoolDrainsCleanly` verifies that all submitted jobs run to
completion. `TestPoolShutdownTimeoutReturnsError` verifies the deadline path. The race
detector validates the atomic counter and channel operations.

## Summary

- Graceful draining has three ordered phases: stop-accepting, drain-in-flight, force-exit.
- Close the jobs channel to broadcast shutdown to all workers; a range loop drains buffered jobs automatically.
- Call `wg.Add(1)` in the parent before launching each goroutine, not inside it.
- Wrap `wg.Wait()` in a goroutine and select on its completion and `ctx.Done()` to honour the drain deadline.
- Use `sync.Once` to make `Shutdown` safe to call multiple times.

## What's Next

Next: [Channel-Based State Machine](../22-channel-based-state-machine/22-channel-based-state-machine.md).

## Resources

- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [context package](https://pkg.go.dev/context)
- [sync.Once](https://pkg.go.dev/sync#Once)
- [Go Blog: Pipelines and Cancellation](https://go.dev/blog/pipelines)
- [The Twelve-Factor App: Disposability](https://12factor.net/disposability)
