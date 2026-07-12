# 28. Fan-Out with Priority Queues

Fan-out dispatches work from one source to many workers. Adding a priority queue in front of that dispatch changes the guarantee: instead of serving work in arrival order, the system serves it in urgency order. This matters whenever a backlog can accumulate and some items are worth more than others — payment processing vs. analytics, critical alerts vs. debug traces, SLA-bound requests vs. background jobs.

## Concepts

### Why FIFO channels are not enough

A Go channel is a FIFO queue. Every item waits behind the item that arrived before it, regardless of urgency. Under load, a burst of low-priority work fills the channel buffer and delays high-priority items for the entire drain time. A priority queue breaks that guarantee by reordering the buffer on every insertion.

### The heap contract

Go's `container/heap` package requires five methods: `Len`, `Less`, `Swap`, `Push`, and `Pop`. The `Less` function is the only place priority logic lives. For a min-heap (smallest value = highest urgency), `Less(i, j)` returns `true` when item `i` should be processed before item `j`. A secondary sort on `EnqueuedAt` gives FIFO behavior among items of equal priority, which prevents starvation within a tier.

The heap operates on an index-tracked slice so `Fix` and `Remove` can work in O(log n). Every `Swap` must keep the `Index` field in sync; forgetting this corrupts the heap invariant silently.

### Dispatcher design: one goroutine owns the heap

The heap is not goroutine-safe. The cleanest design is to confine it to a single goroutine — the dispatcher loop — rather than protecting it with a mutex. Producers write to an `incoming` channel; the loop reads from that channel, inserts into the heap, and writes the highest-priority item to a `work` channel. Workers read from `work` and push results to `results`.

This avoids lock contention on the hot path and makes the ordering logic easy to reason about.

### The drain-before-dispatch pattern

When the incoming channel has many items already buffered, the dispatcher should drain all of them into the heap before popping the next work item. This gives the heap the most complete view of the workload and produces the best ordering. The pattern is:

1. If the heap is empty, block on `incoming` (do not spin).
2. Non-blocking drain: consume everything currently in `incoming`.
3. Pop the minimum item and send it to `work`.
4. If `work` blocks and a new item arrives on `incoming`, push the popped item back and enqueue the new item.

Step 4 is important: pushing back preserves the item rather than dropping it, and the EnqueuedAt timestamp must not be overwritten on requeue or the item could be re-prioritized incorrectly against newer items.

### Starvation and bounded priority

Pure priority queues starve low-priority items during sustained high-priority load. Mitigations include aging (gradually increasing priority as wait time grows), rate limits per priority tier, or time-slice interleaving. For most workloads, defining only two or three priority tiers (critical / normal / background) and never accepting more critical work than can be drained quickly is simpler and more predictable than algorithmic anti-starvation.

### Graceful shutdown

A dispatcher owns goroutines. Cancelling the context stops the loop and all workers, but items that are in-flight or still in the heap are discarded. If that is unacceptable, drain the heap to a persistent store before returning from `Stop`. The simplest correct behavior — good enough for tests and many production uses — is context-based cancellation with a `done` channel that the caller can wait on.

## Exercises

### Setup

### Exercise 1: Priority queue and dispatcher

Build the `internal/pqueue` package. The heap implementation and the dispatcher live here.

```go
package pqueue

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

// Item is a work item. Lower Priority value = higher urgency.
type Item struct {
	Value      any
	Priority   int
	Index      int
	EnqueuedAt time.Time
}

type itemHeap []*Item

func (h itemHeap) Len() int { return len(h) }
func (h itemHeap) Less(i, j int) bool {
	if h[i].Priority != h[j].Priority {
		return h[i].Priority < h[j].Priority
	}
	return h[i].EnqueuedAt.Before(h[j].EnqueuedAt)
}
func (h itemHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].Index = i
	h[j].Index = j
}
func (h *itemHeap) Push(x any) {
	n := len(*h)
	it := x.(*Item)
	it.Index = n
	*h = append(*h, it)
}
func (h *itemHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	it.Index = -1
	*h = old[:n-1]
	return it
}

// Dispatcher fans out Items to workers in priority order.
type Dispatcher struct {
	incoming chan *Item
	work     chan *Item
	results  chan *Item
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
}

// NewDispatcher creates a Dispatcher with the given channel buffer size.
func NewDispatcher(bufSize int) *Dispatcher {
	return &Dispatcher{
		incoming: make(chan *Item, bufSize),
		work:     make(chan *Item, bufSize),
		results:  make(chan *Item, bufSize),
		done:     make(chan struct{}),
	}
}

// Submit enqueues an item for priority-ordered dispatch.
// It sets EnqueuedAt to the current time if not already set.
func (d *Dispatcher) Submit(item *Item) {
	if item.EnqueuedAt.IsZero() {
		item.EnqueuedAt = time.Now()
	}
	d.incoming <- item
}

// Results returns the channel on which processed items are delivered.
func (d *Dispatcher) Results() <-chan *Item {
	return d.results
}

// Start launches the dispatcher goroutine and n worker goroutines.
func (d *Dispatcher) Start(ctx context.Context, n int) {
	ctx, d.cancel = context.WithCancel(ctx)
	go d.loop(ctx)
	for i := 0; i < n; i++ {
		go d.worker(ctx)
	}
}

// Stop cancels the dispatcher and waits for the dispatcher loop to finish.
func (d *Dispatcher) Stop() {
	d.once.Do(func() {
		if d.cancel != nil {
			d.cancel()
		}
		<-d.done
	})
}

func (d *Dispatcher) loop(ctx context.Context) {
	defer close(d.done)
	pq := &itemHeap{}
	heap.Init(pq)
	for {
		if pq.Len() == 0 {
			select {
			case it := <-d.incoming:
				heap.Push(pq, it)
			case <-ctx.Done():
				return
			}
			continue
		}
		// Drain any pending incoming items before dispatching.
		for {
			select {
			case it := <-d.incoming:
				heap.Push(pq, it)
			default:
				goto dispatch
			}
		}
	dispatch:
		next := heap.Pop(pq).(*Item)
		select {
		case d.work <- next:
		case it := <-d.incoming:
			heap.Push(pq, next)
			heap.Push(pq, it)
		case <-ctx.Done():
			return
		}
	}
}

func (d *Dispatcher) worker(ctx context.Context) {
	for {
		select {
		case it := <-d.work:
			select {
			case d.results <- it:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
```

### Exercise 2: Tests

Write tests that confirm priority ordering, FIFO tiebreaking within a tier, concurrent safety, and graceful shutdown.

```go
package pqueue_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"example.com/pqueue/internal/pqueue"
)

func TestPriorityOrder(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := pqueue.NewDispatcher(16)
	d.Start(ctx, 1)

	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	d.Submit(&pqueue.Item{Value: "low", Priority: 3, EnqueuedAt: t0})
	d.Submit(&pqueue.Item{Value: "high", Priority: 1, EnqueuedAt: t0.Add(time.Nanosecond)})
	d.Submit(&pqueue.Item{Value: "mid", Priority: 2, EnqueuedAt: t0.Add(2 * time.Nanosecond)})

	got := make([]string, 0, 3)
	deadline := time.After(500 * time.Millisecond)
	for len(got) < 3 {
		select {
		case it := <-d.Results():
			got = append(got, it.Value.(string))
		case <-deadline:
			t.Fatalf("timeout waiting for results, got %v", got)
		}
	}

	want := []string{"high", "mid", "low"}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("position %d: got %q want %q", i, got[i], v)
		}
	}
}

func TestFIFOSamePriority(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := pqueue.NewDispatcher(16)
	d.Start(ctx, 1)

	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	d.Submit(&pqueue.Item{Value: "first", Priority: 1, EnqueuedAt: t0})
	d.Submit(&pqueue.Item{Value: "second", Priority: 1, EnqueuedAt: t0.Add(time.Millisecond)})
	d.Submit(&pqueue.Item{Value: "third", Priority: 1, EnqueuedAt: t0.Add(2 * time.Millisecond)})

	got := make([]string, 0, 3)
	deadline := time.After(500 * time.Millisecond)
	for len(got) < 3 {
		select {
		case it := <-d.Results():
			got = append(got, it.Value.(string))
		case <-deadline:
			t.Fatalf("timeout, got %v", got)
		}
	}

	want := []string{"first", "second", "third"}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("FIFO position %d: got %q want %q", i, got[i], v)
		}
	}
}

func TestConcurrentProducers(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := pqueue.NewDispatcher(256)
	d.Start(ctx, 4)

	const n = 50
	for i := 0; i < n; i++ {
		go func(i int) {
			d.Submit(&pqueue.Item{Value: i, Priority: i % 5})
		}(i)
	}

	got := 0
	deadline := time.After(2 * time.Second)
	for got < n {
		select {
		case <-d.Results():
			got++
		case <-deadline:
			t.Fatalf("timeout: only got %d of %d", got, n)
		}
	}
}

func TestGracefulStop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := pqueue.NewDispatcher(16)
	d.Start(ctx, 2)
	d.Submit(&pqueue.Item{Value: "x", Priority: 1})
	time.Sleep(10 * time.Millisecond)
	d.Stop()
}

func ExampleDispatcher() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := pqueue.NewDispatcher(8)

	// Submit all items before Start so they all land in the buffer.
	// The dispatcher loop drains them atomically into the heap,
	// then dispatches in priority order.
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	d.Submit(&pqueue.Item{Value: "priority 3", Priority: 3, EnqueuedAt: t0})
	d.Submit(&pqueue.Item{Value: "priority 1", Priority: 1, EnqueuedAt: t0.Add(time.Nanosecond)})
	d.Submit(&pqueue.Item{Value: "priority 2", Priority: 2, EnqueuedAt: t0.Add(2 * time.Nanosecond)})

	d.Start(ctx, 1)

	for i := 0; i < 3; i++ {
		it := <-d.Results()
		fmt.Println(it.Value)
	}
	// Output:
	// priority 1
	// priority 2
	// priority 3
}
```

### Exercise 3: CLI demo

Build a small program that shows the dispatcher routing items of different priorities.

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/pqueue/internal/pqueue"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := pqueue.NewDispatcher(32)
	d.Start(ctx, 3)

	items := []*pqueue.Item{
		{Value: "critical alert", Priority: 1},
		{Value: "info log", Priority: 10},
		{Value: "warning", Priority: 5},
		{Value: "debug trace", Priority: 20},
		{Value: "error report", Priority: 2},
	}
	for _, it := range items {
		d.Submit(it)
	}

	time.Sleep(50 * time.Millisecond)

	got := 0
	for got < len(items) {
		select {
		case it := <-d.Results():
			fmt.Printf("processed: %v\n", it.Value)
			got++
		case <-time.After(100 * time.Millisecond):
			fmt.Printf("timeout after %d items\n", got)
			return
		}
	}
}
```

## Common Mistakes

### Mistake 1: Forgetting to update Index in Swap

Wrong:
```go
func (h itemHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}
```
What happens: `heap.Fix` and `heap.Remove` use `Index` to locate an item. Stale index fields corrupt the heap silently; items may be returned in the wrong order or cause a nil-pointer panic.

Fix:
```go
func (h itemHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].Index = i
	h[j].Index = j
}
```

### Mistake 2: Overwriting EnqueuedAt on requeue

Wrong:
```go
func (d *Dispatcher) Submit(item *Item) {
	item.EnqueuedAt = time.Now()
	d.incoming <- item
}
```
What happens: when the dispatcher pushes a popped item back to the heap because the work channel was full, the re-push gives the item a fresh timestamp. This makes it appear newer than items that arrived later, breaking FIFO ordering within a priority tier.

Fix:
```go
func (d *Dispatcher) Submit(item *Item) {
	if item.EnqueuedAt.IsZero() {
		item.EnqueuedAt = time.Now()
	}
	d.incoming <- item
}
```

### Mistake 3: Sharing the heap across goroutines without synchronization

Wrong:
```go
go func() { heap.Push(&shared, item1) }()
go func() { heap.Push(&shared, item2) }()
```
What happens: `heap.Push` reads and writes slice internals and calls `Swap`. Concurrent access causes data races that corrupt the heap structure; the race detector will flag this and the program may panic or produce incorrect ordering.

Fix: confine the heap to one goroutine (the loop pattern used here) or protect it with a mutex.

### Mistake 4: Spinning when the queue is empty

Wrong:
```go
for {
	if pq.Len() > 0 {
		next := heap.Pop(pq).(*Item)
		d.work <- next
	}
}
```
What happens: the goroutine consumes a full CPU core even when there is nothing to do.

Fix:
```go
if pq.Len() == 0 {
	select {
	case it := <-d.incoming:
		heap.Push(pq, it)
	case <-ctx.Done():
		return
	}
	continue
}
```

## Verification

```bash
gofmt -l ./...
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

- `container/heap` requires five methods; `Less` encodes priority, and `Swap` must keep `Index` in sync.
- Confine the heap to one goroutine (the dispatcher loop) to avoid lock contention and data races.
- Drain the incoming channel before each dispatch to maximize ordering quality.
- Set `EnqueuedAt` once at submit time and never overwrite it, so FIFO tiebreaking survives requeue.
- Use context cancellation with a `done` channel so `Stop` blocks cleanly without a sleep.
- Prevent starvation by capping priority tier counts or adding aging via `heap.Fix`.

## What's Next

Next: [HTTP Server with net/http](../../17-http-programming/01-http-server-with-net-http/01-http-server-with-net-http.md).

## Resources

- [container/heap — Go standard library](https://pkg.go.dev/container/heap)
- [The Go Memory Model](https://go.dev/ref/mem)
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines)
- [Scheduling in Go — Ardan Labs](https://www.ardanlabs.com/blog/2018/08/scheduling-in-go-part1.html)
