# Exercise 10: Draining A Worker Queue Before Closing The Database Pool

This is the tail of reverse-dependency order: the last thing a service does is
close its shared resources — the database pool, the broker connection. Do it too
early and in-flight workers write to a closed resource mid-operation. This module
builds a job consumer that pulls from an in-memory queue and writes through a fake
pool, and shuts down in the correct order: stop accepting new jobs, drain the
queue within a budget, then close the pool *last*.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
jobdrain/                  module example.com/jobdrain
  go.mod                   go 1.26
  jobdrain.go              Pool (io.Closer), Service{Enqueue,Start,Shutdown}
  cmd/
    demo/
      main.go              enqueue jobs, cancel, drain, close pool last
  jobdrain_test.go         drains-then-closes, wedged-job-bounded, no-write-after-close
```

Files: `jobdrain.go`, `cmd/demo/main.go`, `jobdrain_test.go`.
Implement: a `Pool` with `Write`/`Close` that flags any write after close, and a `Service` whose `Shutdown(budget)` drains the queue (bounded), then closes the pool last.
Test: normal jobs all drain and the pool closes after the last write with no write-after-close; a wedged job does not block pool close past the budget and is reported.
Verify: `go test -count=1 -race ./...`

## Why the pool closes last, and how the drain stays bounded

The dependency graph puts the pool at the bottom: consumers depend on it, so it
must be released after them. The classic corruption bug is closing it first —
`defer pool.Close()` at the top of `main` looks tidy but runs while consumers are
still draining, so an in-flight job calls `Write` on a closed pool and either
errors, panics, or silently drops data. The `Pool` here is a guarded fake that
records exactly this: a `Write` after `Close` sets a `wroteAfterClose` flag, so a
test can assert the ordering was respected rather than hoping.

`Shutdown` encodes the correct order in three moves. First it stops accepting new
jobs (an atomic `draining` flag makes `Enqueue` reject) so the queue can only
shrink. Second it drains: the consumers, on `ctx.Done()`, switch to a
non-blocking drain loop that pulls buffered jobs until the queue is empty, and
`Shutdown` waits for them with `wg.Wait()` — but raced against `time.After(budget)`,
so a wedged job cannot hang the process past the grace period. Third, and only
third, it closes the pool. Closing last means every job that finished within the
budget wrote to a live pool; a job that blew the budget is abandoned, reported via
a timeout error, and the process still exits rather than hanging into SIGKILL.

The bounded `wg.Wait()` is the same idiom the concepts stress: a `sync.WaitGroup`
tells you when the workers finished, but `Wait()` alone blocks forever on a wedged
worker, so you close a channel after it and `select` that against a timer. The pool
closing after that select — clean or timed-out — is what keeps a stuck job from
holding the pool open past the budget while still guaranteeing the pool is closed.

Create `jobdrain.go`:

```go
package jobdrain

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrDrainTimeout reports that the queue did not fully drain within the budget,
// so the pool was closed with jobs still in flight.
var ErrDrainTimeout = errors.New("job drain exceeded budget")

// ErrQueueClosed is returned by Enqueue once shutdown has begun.
var ErrQueueClosed = errors.New("queue is draining; not accepting new jobs")

// Pool is a stand-in for a *sql.DB or broker connection: an io.Closer that
// records writes and flags any write that arrives after Close, so a test can
// prove the pool was closed last.
type Pool struct {
	mu              sync.Mutex
	closed          bool
	writes          []string
	wroteAfterClose bool
}

// Write records a job write. A write after Close sets wroteAfterClose and errors.
func (p *Pool) Write(job string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		p.wroteAfterClose = true
		return errors.New("write on closed pool")
	}
	p.writes = append(p.writes, job)
	return nil
}

// Close marks the pool closed. Satisfies io.Closer.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

// Writes returns the jobs written, in order.
func (p *Pool) Writes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.writes...)
}

// WroteAfterClose reports whether any write arrived after Close.
func (p *Pool) WroteAfterClose() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wroteAfterClose
}

// Closed reports whether Close has been called.
func (p *Pool) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Service consumes jobs from a queue and writes them through a pool. On shutdown
// it drains the queue within a budget, then closes the pool last.
type Service struct {
	queue    chan string
	pool     *Pool
	workers  int
	process  func(ctx context.Context, pool *Pool, job string) error
	wg       sync.WaitGroup
	draining atomic.Bool
}

// NewService builds a Service with a buffered queue and a job processor.
func NewService(pool *Pool, workers, queueSize int, process func(ctx context.Context, pool *Pool, job string) error) *Service {
	return &Service{
		queue:   make(chan string, queueSize),
		pool:    pool,
		workers: workers,
		process: process,
	}
}

// Enqueue submits a job. It rejects new jobs once shutdown has begun.
func (s *Service) Enqueue(job string) error {
	if s.draining.Load() {
		return ErrQueueClosed
	}
	select {
	case s.queue <- job:
		return nil
	default:
		return errors.New("queue full")
	}
}

// Start launches the consumer goroutines. Each runs until ctx is cancelled, then
// drains any buffered jobs before exiting.
func (s *Service) Start(ctx context.Context) {
	for range s.workers {
		s.wg.Add(1)
		go s.consume(ctx)
	}
}

func (s *Service) consume(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			// Drain buffered jobs, then exit.
			for {
				select {
				case job := <-s.queue:
					_ = s.process(ctx, s.pool, job)
				default:
					return
				}
			}
		case job := <-s.queue:
			_ = s.process(ctx, s.pool, job)
		}
	}
}

// Shutdown stops accepting new jobs, waits up to budget for the queue to drain,
// then closes the pool LAST. A wedged job cannot hold the pool open past budget.
func (s *Service) Shutdown(budget time.Duration) error {
	s.draining.Store(true)

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	var err error
	select {
	case <-done:
	case <-time.After(budget):
		err = ErrDrainTimeout
	}

	// Close the pool last, after the drain completed or timed out.
	if cerr := s.pool.Close(); cerr != nil {
		err = errors.Join(err, cerr)
	}
	return err
}
```

## The runnable demo

The demo enqueues jobs, cancels, drains them, and closes the pool last, then
prints how many jobs were written and that no write landed after close.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/jobdrain"
)

func main() {
	pool := &jobdrain.Pool{}
	svc := jobdrain.NewService(pool, 3, 100, func(ctx context.Context, p *jobdrain.Pool, job string) error {
		return p.Write(job)
	})

	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)

	for i := range 10 {
		_ = svc.Enqueue(fmt.Sprintf("job-%d", i))
	}

	cancel() // signal consumers to drain and stop
	if err := svc.Shutdown(2 * time.Second); err != nil {
		fmt.Println("shutdown error:", err)
		return
	}

	fmt.Println("jobs written:", len(pool.Writes()))
	fmt.Println("wrote after close:", pool.WroteAfterClose())
	fmt.Println("pool closed:", pool.Closed())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
jobs written: 10
wrote after close: false
pool closed: true
```

## Tests

`TestDrainsThenClosesPool` enqueues jobs, cancels, and asserts every job was
written, the pool is closed, and no write landed after close — proving the pool
closed last. `TestWedgedJobDoesNotBlockClose` uses a job that blocks past the
budget: `Shutdown` returns `ErrDrainTimeout` within ~budget rather than hanging,
and the pool is still closed. A `t.Cleanup` releases the wedged job so its
goroutine exits.

Create `jobdrain_test.go`:

```go
package jobdrain

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDrainsThenClosesPool(t *testing.T) {
	t.Parallel()

	pool := &Pool{}
	svc := NewService(pool, 3, 100, func(ctx context.Context, p *Pool, job string) error {
		return p.Write(job)
	})

	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)

	const n = 20
	for i := range n {
		if err := svc.Enqueue(fmt.Sprintf("job-%d", i)); err != nil {
			t.Fatalf("Enqueue(%d): %v", i, err)
		}
	}

	cancel()
	if err := svc.Shutdown(2 * time.Second); err != nil {
		t.Fatalf("Shutdown: %v; want nil", err)
	}

	if got := len(pool.Writes()); got != n {
		t.Fatalf("writes = %d, want %d (jobs lost on drain)", got, n)
	}
	if pool.WroteAfterClose() {
		t.Fatal("a job wrote after Close; pool was not closed last")
	}
	if !pool.Closed() {
		t.Fatal("pool was not closed")
	}
	// After shutdown, Enqueue must reject new jobs.
	if err := svc.Enqueue("late"); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("Enqueue after shutdown = %v, want ErrQueueClosed", err)
	}
}

func TestWedgedJobDoesNotBlockClose(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // let the wedged goroutine exit

	pool := &Pool{}
	svc := NewService(pool, 2, 100, func(ctx context.Context, p *Pool, job string) error {
		if job == "wedged" {
			<-release // ignores ctx; blocks past the drain budget
		}
		return p.Write(job)
	})

	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)

	_ = svc.Enqueue("wedged")
	_ = svc.Enqueue("ok-1")
	_ = svc.Enqueue("ok-2")
	time.Sleep(20 * time.Millisecond) // let a worker pick up "wedged"

	cancel()
	start := time.Now()
	err := svc.Shutdown(80 * time.Millisecond)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("Shutdown = %v, want ErrDrainTimeout", err)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("Shutdown blocked %v; a wedged job held the pool open", elapsed)
	}
	if !pool.Closed() {
		t.Fatal("pool was not closed after a wedged drain")
	}
}

func ExampleService_Shutdown() {
	pool := &Pool{}
	svc := NewService(pool, 1, 10, func(ctx context.Context, p *Pool, job string) error {
		return p.Write(job)
	})
	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)
	_ = svc.Enqueue("a")
	cancel()
	_ = svc.Shutdown(time.Second)
	fmt.Println(pool.Closed())
	// Output: true
}
```

## Review

The ordering is correct when every job that drained within the budget wrote to a
live pool and the pool closed last. `TestDrainsThenClosesPool` proves it directly:
all N jobs written, no write-after-close, pool closed — plus `Enqueue` rejecting
new work once draining. `TestWedgedJobDoesNotBlockClose` proves the bound: a job
that ignores its context does not hold the pool open past the budget; `Shutdown`
returns `ErrDrainTimeout` fast and closes the pool anyway, trading an abandoned job
for a process that exits instead of getting SIGKILL'd. The mistakes to avoid:
closing the pool before draining (in-flight writes hit a closed resource — the
`wroteAfterClose` flag exists to catch exactly that), and `wg.Wait()` with no timer
(one wedged job hangs shutdown past the grace period). Run `go test -race`; the
pool is written by every consumer goroutine and read by the assertions, so its
mutex is load-bearing.

## Resources

- [io.Closer](https://pkg.go.dev/io#Closer) — the `Close() error` contract the pool and a real `*sql.DB` share.
- [database/sql DB.Close](https://pkg.go.dev/database/sql#DB.Close) — the real resource this pool stands in for, closed last in shutdown.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — draining workers, raced against a timer so a wedged job cannot hang exit.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-idempotent-double-signal-shutdown.md](09-idempotent-double-signal-shutdown.md) | Next: [../12-multi-stage-pipeline-cancellation/00-concepts.md](../12-multi-stage-pipeline-cancellation/00-concepts.md)
