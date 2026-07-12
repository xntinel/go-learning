# Exercise 9: Drain In-Flight Work On SIGTERM Within A Deadline

When an orchestrator sends `SIGTERM`, a server should stop taking new work, let
in-flight work finish, and exit — but not hang forever, because the orchestrator
will `SIGKILL` it after its own grace period anyway. The right contract is
drain-with-deadline: wait for in-flight jobs, but give up after a grace period and
return `context.DeadlineExceeded` so the shutdown is bounded. This exercise builds
that `Shutdown(ctx)`.

This module is fully self-contained.

## What you'll build

```text
drainpool/                 independent module: example.com/drainpool
  go.mod                   go 1.25
  pool.go                  type Pool; New, Submit, Shutdown(ctx) error
  cmd/
    demo/
      main.go              runnable demo: drain succeeds, then a too-slow drain times out
  pool_test.go             drain-ok, deadline-exceeded, reject, idempotent tests, -race
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: a pool with `Shutdown(ctx) error` that stops accepting work, waits for in-flight jobs, returns `nil` if they finish before `ctx` is done and `context.DeadlineExceeded` otherwise, is idempotent, and rejects `Submit` after being called.
- Test: an ample-deadline `Shutdown` returns `nil` after all jobs finish; a job longer than the grace period makes `Shutdown` return `DeadlineExceeded` while still refusing `Submit`; a second `Shutdown` does not panic.
- Verify: `go test -count=1 -race ./...`

### Race the drain against the deadline

`Shutdown` has to do two things that can each take arbitrarily long: wait for the
`WaitGroup` (until every worker finishes) and respect the caller's deadline
(`ctx.Done()`). You cannot `select` on `wg.Wait()` directly — it is a blocking call,
not a channel. The idiom is to run `wg.Wait()` in a goroutine that closes a
`done` channel when it returns, then `select` between `done` and `ctx.Done()`:

```go
done := make(chan struct{})
go func() { p.wg.Wait(); close(done) }()
select {
case <-done:
	return nil                 // drained in time
case <-ctx.Done():
	return ctx.Err()           // grace period exceeded
}
```

Whichever fires first wins. If the workers finish before the deadline, `done`
closes and `Shutdown` returns `nil`. If the deadline fires first, `ctx.Done()`
closes and `Shutdown` returns `ctx.Err()` — `context.DeadlineExceeded` for a
`WithTimeout` context — so the caller (and the orchestrator) is not left hanging
past the grace period. Note that returning on the deadline does *not* stop the
in-flight jobs; they keep running (this pool drains, it does not cancel — combine
with Exercise 5's cancellation if you need both). The waiter goroutine is not
leaked: it stays blocked on `wg.Wait()` only until the jobs actually finish, then
closes `done` and exits.

Before the race, `Shutdown` closes the job channel under the mutex so no new work
is accepted and the workers' `range` loops will end once the queue drains. The
`closed` flag makes this idempotent: a second `Shutdown` sees the channel already
closed and skips re-closing it (a double close would panic), but still runs the
drain-race, so calling `Shutdown` twice is safe and both calls observe the same
outcome.

Create `pool.go`:

```go
package drainpool

import (
	"context"
	"sync"
)

// Job is a unit of work.
type Job func() error

// Pool runs a fixed set of workers and supports a bounded graceful shutdown.
type Pool struct {
	jobs   chan Job
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// New starts workers goroutines.
func New(workers int) *Pool {
	p := &Pool{jobs: make(chan Job, workers*2)}
	for range workers {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for job := range p.jobs {
		_ = job()
	}
}

// Submit enqueues job, returning false once Shutdown has been called.
func (p *Pool) Submit(job Job) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.jobs <- job
	return true
}

// Shutdown stops accepting work and waits for in-flight jobs to finish, but no
// longer than ctx allows: it returns nil if the pool drains before ctx is done,
// or ctx.Err() (context.DeadlineExceeded) otherwise. It is idempotent.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.jobs)
	}
	p.mu.Unlock()

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

### The runnable demo

The demo runs a quick drain that succeeds, then a second pool with a job longer
than the grace period so the drain times out, printing each outcome.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/drainpool"
)

func main() {
	// Fast jobs: drain succeeds within the grace period.
	fast := drainpool.New(4)
	for range 8 {
		fast.Submit(func() error { time.Sleep(5 * time.Millisecond); return nil })
	}
	ctx1, cancel1 := context.WithTimeout(context.Background(), time.Second)
	defer cancel1()
	fmt.Printf("fast drain: %v\n", fast.Shutdown(ctx1))

	// A slow job: drain exceeds the grace period.
	slow := drainpool.New(1)
	slow.Submit(func() error { time.Sleep(200 * time.Millisecond); return nil })
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	err := slow.Shutdown(ctx2)
	fmt.Printf("slow drain timed out: %v\n", errors.Is(err, context.DeadlineExceeded))
	_ = slow.Shutdown(context.Background()) // let the slow job finish
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast drain: <nil>
slow drain timed out: true
```

### Tests

`TestDrainWithinDeadline` submits fast jobs and asserts `Shutdown` with an ample
deadline returns `nil` after all finish. `TestDeadlineExceeded` submits a job
longer than the grace period, asserts `Shutdown` returns `context.DeadlineExceeded`
and that `Submit` is refused afterward, then drains fully to avoid leaking the job.
`TestIdempotentShutdown` calls `Shutdown` twice and asserts both return `nil`
without panic.

Create `pool_test.go`:

```go
package drainpool

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDrainWithinDeadline(t *testing.T) {
	t.Parallel()

	p := New(4)
	var done atomic.Int64
	for range 10 {
		p.Submit(func() error {
			time.Sleep(5 * time.Millisecond)
			done.Add(1)
			return nil
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	if got := done.Load(); got != 10 {
		t.Fatalf("done = %d, want 10", got)
	}
}

func TestDeadlineExceeded(t *testing.T) {
	t.Parallel()

	p := New(1)
	p.Submit(func() error {
		time.Sleep(200 * time.Millisecond)
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := p.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want context.DeadlineExceeded", err)
	}
	if p.Submit(func() error { return nil }) {
		t.Fatal("Submit should be refused after Shutdown")
	}
	// Drain fully so the slow job does not outlive the test.
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown = %v, want nil", err)
	}
}

func TestIdempotentShutdown(t *testing.T) {
	t.Parallel()

	p := New(2)
	p.Submit(func() error { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown = %v, want nil", err)
	}
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown = %v, want nil", err)
	}
}
```

## Review

`Shutdown` is correct when it is bounded and idempotent. The drain-versus-deadline
`select` returns `nil` when the pool finishes first (`TestDrainWithinDeadline`) and
`context.DeadlineExceeded` when the deadline wins (`TestDeadlineExceeded`), so a
`SIGTERM` handler never hangs past its grace period. Closing the channel under the
`closed` flag stops new `Submit`s and makes a repeat call safe
(`TestIdempotentShutdown`).

The mistakes to avoid: trying to `select` on `wg.Wait()` directly (it is not a
channel — wrap it in a goroutine that closes a `done` channel); closing the job
channel twice across two `Shutdown` calls (guard with the `closed` flag);
forgetting that a timed-out drain leaves the jobs *running* (this pool does not
cancel them — the test drains fully afterward so the slow job does not outlive the
test); and returning before closing the channel (then workers never see the range
end and the drain never completes). Run `-race` to confirm the `closed` flag and
the channel close are serialized against `Submit`.

## Resources

- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) — the bounded grace period and `DeadlineExceeded`.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the drain the `done`-channel goroutine waits on.
- [`net/http.Server.Shutdown`](https://pkg.go.dev/net/http#Server.Shutdown) — the standard library's own drain-with-context contract, the model for this API.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-rate-limited-outbound.md](08-rate-limited-outbound.md) | Next: [10-pool-metrics-hook.md](10-pool-metrics-hook.md)
