# Exercise 4: Worker Pool with Backpressure

The fan-out in the earlier exercises uses one bounded channel per partition; this module makes that pattern explicit as a reusable worker pool and proves its two load-shaping properties: a bounded queue applies backpressure (a full queue blocks `Submit`), and the number of concurrently running handlers never exceeds the worker count. Both are tested deterministically — no sleeps standing in for synchronisation.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
pool.go                Handler, Pool, NewPool, Workers, Submit (round-robin,
                       blocking), Close
cmd/
  demo/
    main.go            4000 items across 4 workers, exactly even split
pool_test.go           even distribution, per-worker FIFO, bounded concurrency
                       (peak == workers), backpressure blocks when full
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: `Pool` with one bounded queue per worker, a `Submit` that routes round-robin and blocks when the target queue is full, and a `Close` that drains and stops.
- Test: `pool_test.go` asserts an exactly even split, per-worker FIFO order, a peak concurrency of exactly `workers` (via a barrier), and that `Submit` blocks once a worker's queue is full.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p pool/cmd/demo && cd pool
go mod init example.com/pool
go mod edit -go=1.26
```

### Backpressure for free, and a peak-concurrency invariant

Backpressure is the property that a fast producer is throttled to the rate of the slowest consumer rather than allowed to enqueue unbounded work. In Go it falls out of a bounded buffered channel: a send on a full channel blocks. The pool gives each worker its own queue of capacity `queueSize`, and `Submit` routes an item to worker `item mod workers` with a blocking send. When that worker falls behind and its queue fills, `Submit` blocks — the producer is forced to wait, memory stays bounded, and the slowdown propagates upstream exactly when it should. Sizing the queue is the only knob: too small wastes throughput on constant blocking, too large lets a burst buffer stale work.

The other property worth proving is the peak concurrency bound: at most `workers` handlers run at once, because each worker is a single goroutine that calls one handler at a time. This is an invariant, not a timing accident — it holds for every interleaving — which is what makes it testable without sleeps. The bounded-concurrency test installs a barrier in the handler: each handler records the live count, updates a peak, then blocks until all `workers` handlers have arrived. Because the barrier cannot release until exactly `workers` handlers are simultaneously live, the observed peak is exactly `workers` — never less (the barrier guarantees the floor) and never more (the one-handler-per-worker invariant guarantees the ceiling). Routing by `item mod workers` also gives an exactly even split for a contiguous range of items, and each worker's queue is FIFO, so per-worker order is the submission order — both deterministic and asserted directly.

Create `pool.go`:

```go
// Package pool implements a fan-out worker pool with one bounded queue per
// worker. Submit routes items round-robin across workers and blocks when the
// chosen worker's queue is full; that blocking is backpressure, which bounds
// the number of in-flight items and propagates load back to the producer.
package pool

import (
	"context"
	"sync"
)

// Handler processes one item on the given worker index. It is called by at
// most one goroutine per worker, so handlers on the same worker never overlap.
type Handler func(item, worker int)

// Pool is a fixed set of workers, each draining its own bounded queue.
type Pool struct {
	workers int
	queues  []chan int
	wg      sync.WaitGroup
}

// NewPool starts workers goroutines, each with a queue of capacity queueSize,
// and returns the running pool. Both arguments are clamped to at least 1. The
// pool stops when ctx is cancelled or Close is called.
func NewPool(ctx context.Context, workers, queueSize int, h Handler) *Pool {
	if workers < 1 {
		workers = 1
	}
	if queueSize < 1 {
		queueSize = 1
	}
	p := &Pool{workers: workers, queues: make([]chan int, workers)}
	for i := 0; i < workers; i++ {
		p.queues[i] = make(chan int, queueSize)
		w := i
		q := p.queues[i]
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for {
				select {
				case item, ok := <-q:
					if !ok {
						return
					}
					h(item, w)
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	return p
}

// Workers reports the number of worker goroutines.
func (p *Pool) Workers() int { return p.workers }

// Submit routes item to worker (item mod Workers) and blocks until that
// worker's bounded queue accepts it. It returns ctx.Err() if ctx is cancelled
// while blocked. Routing by item index gives an exactly even split when items
// are a contiguous range.
func (p *Pool) Submit(ctx context.Context, item int) error {
	w := item % p.workers
	select {
	case p.queues[w] <- item:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops accepting work and waits for every worker to drain its queue and
// exit. It must not be called concurrently with Submit.
func (p *Pool) Close() {
	for _, q := range p.queues {
		close(q)
	}
	p.wg.Wait()
}
```

`Submit` selects over the target queue send and `ctx.Done()`, so a cancelled context unblocks a producer that is parked on a full queue instead of deadlocking it. `Close` closes every queue and waits for the workers to drain — it must not race with `Submit`, since closing a channel that a `Submit` is sending on would panic; the contract is that the producer stops calling `Submit` before calling `Close`.

### The runnable demo

The demo submits four thousand items across four workers. Round-robin routing of the contiguous range `0..3999` gives each worker exactly one thousand items, so the per-worker counts are deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"example.com/pool"
)

func main() {
	const workers = 4
	var counts [workers]atomic.Int64

	p := pool.NewPool(context.Background(), workers, 8, func(item, w int) {
		counts[w].Add(1)
	})

	// Submit a contiguous range; round-robin routing splits it evenly and the
	// bounded per-worker queues apply backpressure if a worker falls behind.
	const total = 4000
	for i := 0; i < total; i++ {
		if err := p.Submit(context.Background(), i); err != nil {
			panic(err)
		}
	}
	p.Close()

	fmt.Printf("submitted %d items across %d workers\n", total, workers)
	for w := 0; w < workers; w++ {
		fmt.Printf("  worker %d processed %d items\n", w, counts[w].Load())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
submitted 4000 items across 4 workers
  worker 0 processed 1000 items
  worker 1 processed 1000 items
  worker 2 processed 1000 items
  worker 3 processed 1000 items
```

### Tests

`TestEvenDistribution` and `TestPerWorkerFIFO` pin the deterministic routing and ordering. `TestBoundedConcurrency` uses the barrier described above to assert the peak is exactly `workers`. `TestBackpressureBlocksWhenFull` proves backpressure structurally: with one worker and a one-slot queue, exactly two items fit (one parked in the handler, one in the queue), so a third `Submit` must block — the test runs it in a goroutine and confirms it has not returned, then releases the worker so the blocked `Submit` completes. The pre-state is deterministic (the two earlier `Submit`s return synchronously), so the confirmation is reliable.

Create `pool_test.go`:

```go
package pool

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEvenDistribution proves round-robin routing splits a contiguous range of
// items exactly evenly across workers. The counts are deterministic.
func TestEvenDistribution(t *testing.T) {
	t.Parallel()
	const workers = 4
	const perWorker = 250
	var counts [workers]atomic.Int64

	p := NewPool(context.Background(), workers, 8, func(item, w int) {
		counts[w].Add(1)
	})
	for i := 0; i < workers*perWorker; i++ {
		if err := p.Submit(context.Background(), i); err != nil {
			t.Fatal(err)
		}
	}
	p.Close()

	for w := 0; w < workers; w++ {
		if got := counts[w].Load(); got != perWorker {
			t.Fatalf("worker %d processed %d items, want %d", w, got, perWorker)
		}
	}
}

// TestPerWorkerFIFO proves each worker sees its items in submission order,
// because a bounded channel is FIFO and one goroutine drains it.
func TestPerWorkerFIFO(t *testing.T) {
	t.Parallel()
	const workers = 3
	const perWorker = 100

	var mu sync.Mutex
	seen := make([][]int, workers)

	p := NewPool(context.Background(), workers, 4, func(item, w int) {
		mu.Lock()
		seen[w] = append(seen[w], item)
		mu.Unlock()
	})
	for i := 0; i < workers*perWorker; i++ {
		if err := p.Submit(context.Background(), i); err != nil {
			t.Fatal(err)
		}
	}
	p.Close()

	for w := 0; w < workers; w++ {
		if len(seen[w]) != perWorker {
			t.Fatalf("worker %d saw %d items, want %d", w, len(seen[w]), perWorker)
		}
		for i, item := range seen[w] {
			want := w + i*workers
			if item != want {
				t.Fatalf("worker %d position %d: item %d, want %d (not FIFO)", w, i, item, want)
			}
		}
	}
}

// TestBoundedConcurrency proves no more than Workers handlers run at once. A
// barrier forces exactly Workers handlers to be live simultaneously, so the
// peak is exactly Workers: never less (the barrier blocks until all arrive),
// never more (the invariant of one handler per worker).
func TestBoundedConcurrency(t *testing.T) {
	t.Parallel()
	const workers = 4

	var live atomic.Int32
	var peak atomic.Int32
	var arrived sync.WaitGroup
	arrived.Add(workers)
	release := make(chan struct{})

	p := NewPool(context.Background(), workers, 2, func(item, w int) {
		n := live.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		arrived.Done()
		<-release // hold the handler so all workers overlap
		live.Add(-1)
	})

	// Submit one item per worker so each worker has exactly one live handler.
	for w := 0; w < workers; w++ {
		if err := p.Submit(context.Background(), w); err != nil {
			t.Fatal(err)
		}
	}
	arrived.Wait() // every worker is now inside its handler
	if got := peak.Load(); got != workers {
		t.Fatalf("peak concurrency = %d, want %d", got, workers)
	}
	close(release)
	p.Close()
	if got := peak.Load(); got > workers {
		t.Fatalf("peak concurrency = %d exceeded workers %d", got, workers)
	}
}

// TestBackpressureBlocksWhenFull proves Submit blocks once a worker's queue is
// full and the worker is busy. With one worker and a queue of one, exactly two
// items fit (one in the handler, one in the queue); the third Submit blocks.
func TestBackpressureBlocksWhenFull(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	p := NewPool(context.Background(), 1, 1, func(item, w int) {
		<-release // the only worker is parked inside the handler
	})

	ctx := context.Background()
	// Item 0 is taken by the worker and parks in the handler; item 1 fills the
	// one-slot queue. Both of these Submits return.
	if err := p.Submit(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := p.Submit(ctx, 1); err != nil {
		t.Fatal(err)
	}

	// Item 2 has nowhere to go: the queue is full and the worker is busy.
	done := make(chan error, 1)
	go func() { done <- p.Submit(ctx, 2) }()
	select {
	case <-done:
		t.Fatal("Submit returned while the queue was full; backpressure not applied")
	case <-time.After(50 * time.Millisecond):
		// Still blocked: backpressure is working.
	}

	close(release) // let the worker drain so the blocked Submit can proceed
	if err := <-done; err != nil {
		t.Fatalf("blocked Submit eventually failed: %v", err)
	}
	p.Close()
}

func ExamplePool_Submit() {
	var total atomic.Int64
	p := NewPool(context.Background(), 2, 4, func(item, w int) {
		total.Add(int64(item))
	})
	for i := 1; i <= 5; i++ {
		_ = p.Submit(context.Background(), i)
	}
	p.Close()
	fmt.Println(total.Load())
	// Output:
	// 15
}
```

## Review

The pool is correct when the split is exactly even, per-worker order is FIFO, and the peak concurrency equals the worker count. The deterministic backpressure test is the subtle one: it relies on the fact that with one worker and a one-slot queue, the first `Submit` parks an item in the handler and the second fills the queue, both returning, so the third is guaranteed to block — there is no timing race in reaching that state. The most common real bug is closing the queues from `Close` while a producer is still calling `Submit`: a send on a closed channel panics, which is why the contract requires the producer to stop first. The second is sizing queues by guesswork; the queue capacity is the latency-versus-throughput knob and should be chosen against measured load. Run `go test -race -count=20` to shake out any handler that touches shared state without synchronisation.

## Resources

- [Reactive Streams](https://www.reactive-streams.org/) — the standard treatment of backpressure as bounded queues mediating fast producers and slow consumers.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — bounded channels and the fan-out pattern the pool generalises.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — the barrier that lets `Close` wait for every worker to drain.
- [sync/atomic](https://pkg.go.dev/sync/atomic) — the atomic counters the tests use to observe distribution and peak concurrency.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-keyed-state-operator.md](03-keyed-state-operator.md)
