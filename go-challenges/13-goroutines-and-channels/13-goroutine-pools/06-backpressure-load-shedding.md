# Exercise 6: Reject Work Under Load Instead Of Queueing Unboundedly

A blocking `Submit` under sustained overload makes the caller wait inside the
pool; an unbounded queue makes the backlog — and latency, and memory — grow
without limit. The production answer is to shed load: accept work only while there
is room, and reject the rest fast so the caller gets a 503-style signal and can
back off. This exercise builds an admission-controlled `TrySubmit` that does a
non-blocking send on a bounded queue and returns `ErrQueueFull` when saturated,
plus `Pending`/`Capacity` for observability.

This module is fully self-contained.

## What you'll build

```text
shedpool/                  independent module: example.com/shedpool
  go.mod                   go 1.25
  pool.go                  type Pool; New, TrySubmit, Pending, Capacity, Close;
                           ErrQueueFull, ErrClosed sentinels
  cmd/
    demo/
      main.go              runnable demo: saturate the queue, watch rejections
  pool_test.go             reject-when-full, accept-again, closed, pending-bound tests, -race
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: a bounded pool with `TrySubmit(job) (bool, error)` that never blocks — it returns `ErrQueueFull` when the queue is saturated and `ErrClosed` after `Close` — plus `Pending` and `Capacity`.
- Test: a saturated queue rejects further `TrySubmit` immediately, accepts again once workers drain it, rejects with `ErrClosed` after `Close`, and `Pending` never exceeds `Capacity`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/shedpool/cmd/demo
cd ~/go-exercises/shedpool
go mod init example.com/shedpool
```

### Non-blocking send is the load-shedding primitive

The whole mechanism is a `select` with a `default` case:

```go
select {
case p.jobs <- job:
	// accepted: there was room in the bounded queue
default:
	// queue full: shed this job instead of blocking
}
```

A plain `p.jobs <- job` blocks when the buffered channel is full, which under
overload means the caller stalls inside the pool — backpressure expressed as
latency. Adding `default` makes the send *non-blocking*: if the buffer has room
the job is enqueued and `TrySubmit` returns `(true, nil)`; if it is full the
`default` runs immediately and `TrySubmit` returns `(false, ErrQueueFull)`. The
caller learns *right now* that the service is saturated and can translate that
into an HTTP 503, drop the request, or route it elsewhere — instead of piling onto
an ever-growing backlog. That is the difference between a service that degrades
gracefully and one that falls over: bounded queue plus fast rejection.

`Pending` (the current queue length, `len(p.jobs)`) and `Capacity` (the buffer
size, `cap(p.jobs)`) exist so an operator can see how close to saturation the pool
is running. The invariant `Pending <= Capacity` always holds because the channel
is bounded; watching `Pending` approach `Capacity` is the early warning that
rejections are imminent. A closed pool returns `ErrClosed`, distinct from
`ErrQueueFull`, so the caller can tell "try again later" from "this pool is gone."

Create `pool.go`:

```go
package shedpool

import (
	"errors"
	"sync"
)

// ErrQueueFull is returned by TrySubmit when the bounded queue is saturated: the
// caller should shed the work (e.g. respond 503) rather than wait.
var ErrQueueFull = errors.New("job queue full")

// ErrClosed is returned by TrySubmit after the pool is closed.
var ErrClosed = errors.New("pool closed")

// Job is a unit of work.
type Job func() error

// Pool is a bounded worker pool that sheds load: TrySubmit never blocks.
type Pool struct {
	jobs   chan Job
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// New starts workers goroutines draining a queue of the given capacity.
func New(workers, queue int) *Pool {
	p := &Pool{jobs: make(chan Job, queue)}
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

// TrySubmit enqueues job without blocking. It returns (true, nil) on admission,
// (false, ErrQueueFull) when the queue is saturated, and (false, ErrClosed) when
// the pool is closed.
func (p *Pool) TrySubmit(job Job) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false, ErrClosed
	}
	select {
	case p.jobs <- job:
		return true, nil
	default:
		return false, ErrQueueFull
	}
}

// Pending reports the number of jobs waiting in the queue.
func (p *Pool) Pending() int { return len(p.jobs) }

// Capacity reports the queue size, the maximum Pending can reach.
func (p *Pool) Capacity() int { return cap(p.jobs) }

// Close stops accepting work and drains the queue.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.jobs)
	p.mu.Unlock()
	p.wg.Wait()
}
```

### The runnable demo

The demo blocks the single worker, fills the queue, then keeps submitting and
counts how many are shed. With a queue of 3 and the worker busy, 3 are accepted
and the rest rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/shedpool"
)

func main() {
	p := shedpool.New(1, 3)
	defer p.Close()

	release := make(chan struct{})
	started := make(chan struct{})
	p.TrySubmit(func() error { close(started); <-release; return nil })
	<-started // the one worker is now busy; queue is empty

	accepted, rejected := 0, 0
	for range 10 {
		ok, err := p.TrySubmit(func() error { return nil })
		switch {
		case ok:
			accepted++
		case errors.Is(err, shedpool.ErrQueueFull):
			rejected++
		}
	}
	fmt.Printf("capacity=%d accepted=%d rejected=%d\n", p.Capacity(), accepted, rejected)
	close(release)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
capacity=3 accepted=3 rejected=7
```

### Tests

`TestRejectsWhenFullThenAcceptsAgain` blocks both workers, fills the queue,
asserts a burst is accepted exactly up to `Capacity` and the rest rejected with
`ErrQueueFull` while `Pending` never exceeds `Capacity`, then releases the workers,
waits for the queue to drain, and asserts `TrySubmit` succeeds again.
`TestClosedRejects` asserts `TrySubmit` returns `ErrClosed` after `Close`.

Create `pool_test.go`:

```go
package shedpool

import (
	"errors"
	"testing"
	"time"
)

func TestRejectsWhenFullThenAcceptsAgain(t *testing.T) {
	t.Parallel()

	const workers, queue = 2, 3
	p := New(workers, queue)
	defer p.Close()

	release := make(chan struct{})
	started := make(chan struct{}, workers)
	for range workers {
		ok, err := p.TrySubmit(func() error {
			started <- struct{}{}
			<-release
			return nil
		})
		if !ok {
			t.Fatalf("initial TrySubmit failed: %v", err)
		}
	}
	for range workers {
		<-started // both workers busy; queue empty
	}

	accepted, rejected := 0, 0
	for range 20 {
		ok, err := p.TrySubmit(func() error { return nil })
		if ok {
			accepted++
		} else {
			if !errors.Is(err, ErrQueueFull) {
				t.Fatalf("reject err = %v, want ErrQueueFull", err)
			}
			rejected++
		}
		if p.Pending() > p.Capacity() {
			t.Fatalf("Pending %d exceeds Capacity %d", p.Pending(), p.Capacity())
		}
	}
	if accepted != queue {
		t.Fatalf("accepted = %d, want %d (Capacity)", accepted, queue)
	}
	if rejected != 20-queue {
		t.Fatalf("rejected = %d, want %d", rejected, 20-queue)
	}

	close(release) // workers finish and drain the queued jobs

	deadline := time.After(time.Second)
	for p.Pending() > 0 {
		select {
		case <-deadline:
			t.Fatal("queue never drained")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	// Give a worker a moment to become idle, then admission should succeed.
	for {
		ok, _ := p.TrySubmit(func() error { return nil })
		if ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("TrySubmit never succeeded after drain")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestClosedRejects(t *testing.T) {
	t.Parallel()

	p := New(2, 2)
	p.Close()
	ok, err := p.TrySubmit(func() error { return nil })
	if ok {
		t.Fatal("TrySubmit admitted after Close")
	}
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}
```

## Review

The pool is correct when admission is bounded and non-blocking. The `default` case
makes `TrySubmit` return immediately in every state — never stalling the caller —
and the bounded channel guarantees `Pending <= Capacity`, so a burst against a
saturated pool is accepted only up to `Capacity` and the rest are shed with
`ErrQueueFull`. `TestRejectsWhenFullThenAcceptsAgain` proves both the rejection
under load and the recovery once workers drain the queue; `TestClosedRejects`
proves the distinct `ErrClosed` signal.

The mistakes to avoid: a blocking `p.jobs <- job` with no `default` (backpressure
becomes latency and the caller stalls); an unbounded queue (the backlog grows to
OOM instead of shedding); and collapsing `ErrQueueFull` and `ErrClosed` into one
error (the caller cannot tell "retry later" from "gone"). Run `-race` to confirm
the `closed` flag and the channel operations are serialized under concurrent
`TrySubmit` and `Close`.

## Resources

- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — buffered channels and the non-blocking `select`/`default` idiom.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — bounded queues and backpressure.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — distinguishing `ErrQueueFull` from `ErrClosed`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-context-cancelable-pool.md](05-context-cancelable-pool.md) | Next: [07-panic-safe-workers.md](07-panic-safe-workers.md)
