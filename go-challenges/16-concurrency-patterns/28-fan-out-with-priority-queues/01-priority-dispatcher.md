# Exercise 1: Priority Dispatcher

A fan-out dispatcher that serves work in urgency order instead of arrival order. One goroutine owns a `container/heap`, producers submit items to it over a channel, and it extracts the most urgent item and hands it to a pool of workers. This is the goroutine-confinement design: the heap is never locked because exactly one goroutine ever touches it.

This module is fully self-contained. It defines its own `Item` type, heap, and dispatcher, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
dispatcher.go          Item, itemHeap (heap.Interface), Dispatcher,
                       NewDispatcher, Submit, Results, Start, Stop, loop, worker
cmd/
  demo/
    main.go            submit a mixed-priority batch, then drain in priority order
dispatcher_test.go     heap order, single-worker dispatch order, FIFO within a
                       tier, all-processed-under-load, graceful stop
```

- Files: `dispatcher.go`, `cmd/demo/main.go`, `dispatcher_test.go`.
- Implement: `Item`, `itemHeap` (the five `heap.Interface` methods), `Dispatcher`, `NewDispatcher(bufSize int)`, `(*Dispatcher).Submit`, `(*Dispatcher).Results`, `(*Dispatcher).Start`, and `(*Dispatcher).Stop`.
- Test: `dispatcher_test.go` asserts the heap extracts by priority then sequence, that a single worker draining a pre-filled queue sees strict priority order, that equal-priority items keep first-in-first-out order, that every item is processed under concurrent load with the race detector on, and that `Stop` returns and is idempotent.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p priority-dispatcher/cmd/demo && cd priority-dispatcher
go mod init example.com/priority-dispatcher
```

## How the pieces fit

The dispatcher has three channels and one rule. Producers call `Submit`, which stamps the item with a monotonic sequence number and sends it on the buffered `incoming` channel. A single `loop` goroutine owns the heap: it reads from `incoming`, pushes into the heap, extracts the minimum, and sends it on the `work` channel. Each `worker` goroutine reads from `work` and forwards the item to the buffered `results` channel that the caller drains. The rule is that nothing but `loop` ever touches the heap, so the heap needs no lock.

The sequence number, not a wall-clock timestamp, drives the first-in-first-out tiebreak. `Submit` assigns it with an atomic counter, so the *n*th call to `Submit` gets sequence *n* regardless of how producers race. The heap's `Less` compares priority first and sequence second, so equal-priority items come out in submission order. A timestamp would work too but can collide at nanosecond resolution; a counter cannot. The item also records `EnqueuedAt` for observability, but the ordering contract is the sequence.

The `work` channel is deliberately unbuffered. That means `loop` only extracts an item when a worker is actually ready to take it, and between extractions it re-drains everything waiting on `incoming`. This is the drain-before-dispatch invariant: the heap always has the most complete view of the backlog before it chooses what to serve next. If `work` were buffered, `loop` could dump several items into the buffer before the most urgent one had even arrived, weakening the ordering guarantee for no real gain.

### The single most important honesty in this design

A priority queue promises the order in which items are *extracted*, not the order in which N concurrent workers *finish* them. With four workers, a priority-2 job handed to a fast worker can finish before a priority-1 job handed to a slow one. So the test that asserts a specific order does it with one worker draining a queue that was fully populated *before the worker started* — that removes the race between "producer still submitting" and "worker already consuming," and turns extraction order into the observable result order. The load test, which does use four workers, asserts only that every item is eventually processed and that the run is race-clean. It never asserts a completion order across workers, because there is no deterministic one to assert.

### The requeue case

The dispatch step selects over three things at once: send the popped item to a worker, receive a new item from `incoming`, or observe context cancellation. The middle case matters. If every worker is busy, the send to `work` blocks; if a new item arrives during that block, the loop pushes *both* the popped item and the new one back into the heap and loops, so the heap re-decides which is now most urgent and nothing is dropped. The sequence number is assigned once in `Submit` and never touched on requeue, so a requeued item keeps its place in the first-in-first-out order rather than appearing newer than work that genuinely arrived later.

Create `dispatcher.go`:

```go
package dispatch

import (
	"container/heap"
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Item is a unit of work. A lower Priority value is more urgent: Priority 0 is
// served before Priority 1. Among items of equal priority, the one submitted
// first (smaller seq) is served first.
type Item struct {
	Value      any
	Priority   int
	EnqueuedAt time.Time

	seq   uint64 // submission order; drives the FIFO tiebreak
	index int    // position in the heap, kept in sync by Swap
}

// itemHeap is a min-heap of *Item implementing container/heap.Interface.
type itemHeap []*Item

func (h itemHeap) Len() int { return len(h) }

func (h itemHeap) Less(i, j int) bool {
	if h[i].Priority != h[j].Priority {
		return h[i].Priority < h[j].Priority
	}
	return h[i].seq < h[j].seq
}

func (h itemHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *itemHeap) Push(x any) {
	it := x.(*Item)
	it.index = len(*h)
	*h = append(*h, it)
}

func (h *itemHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	old[n-1] = nil // let the popped item be garbage-collected
	it.index = -1
	*h = old[:n-1]
	return it
}

// Dispatcher fans items out to workers in priority order. The heap is owned by
// the single loop goroutine and is never locked.
type Dispatcher struct {
	incoming chan *Item
	work     chan *Item
	results  chan *Item
	nextSeq  atomic.Uint64

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// NewDispatcher creates a Dispatcher. bufSize buffers the incoming and results
// channels; the work channel is intentionally unbuffered so the loop only
// extracts when a worker is ready.
func NewDispatcher(bufSize int) *Dispatcher {
	return &Dispatcher{
		incoming: make(chan *Item, bufSize),
		work:     make(chan *Item),
		results:  make(chan *Item, bufSize),
	}
}

// Submit stamps the item with a submission sequence and enqueues it. It is safe
// for concurrent callers.
func (d *Dispatcher) Submit(it *Item) {
	if it.EnqueuedAt.IsZero() {
		it.EnqueuedAt = time.Now()
	}
	it.seq = d.nextSeq.Add(1)
	d.incoming <- it
}

// Results is the channel on which processed items are delivered.
func (d *Dispatcher) Results() <-chan *Item { return d.results }

// Start launches the dispatcher loop and the given number of workers.
func (d *Dispatcher) Start(ctx context.Context, workers int) {
	ctx, d.cancel = context.WithCancel(ctx)
	d.wg.Add(1)
	go d.loop(ctx)
	for i := 0; i < workers; i++ {
		d.wg.Add(1)
		go d.worker(ctx)
	}
}

// Stop cancels the dispatcher and waits for the loop and all workers to exit.
// It is idempotent and safe to call concurrently.
func (d *Dispatcher) Stop() {
	d.stopOnce.Do(func() {
		if d.cancel != nil {
			d.cancel()
		}
		d.wg.Wait()
	})
}

func (d *Dispatcher) loop(ctx context.Context) {
	defer d.wg.Done()
	pq := &itemHeap{}
	heap.Init(pq)
	for {
		if pq.Len() == 0 {
			// Nothing to do: block instead of spinning.
			select {
			case it := <-d.incoming:
				heap.Push(pq, it)
			case <-ctx.Done():
				return
			}
			continue
		}

		// Drain everything already queued so the heap has the most complete
		// view before it chooses the next item to serve.
		draining := true
		for draining {
			select {
			case it := <-d.incoming:
				heap.Push(pq, it)
			default:
				draining = false
			}
		}

		next := heap.Pop(pq).(*Item)
		select {
		case d.work <- next:
		case it := <-d.incoming:
			// No worker was ready and new work arrived. Keep both and let
			// the heap re-decide; seq is never reassigned on requeue.
			heap.Push(pq, next)
			heap.Push(pq, it)
		case <-ctx.Done():
			return
		}
	}
}

func (d *Dispatcher) worker(ctx context.Context) {
	defer d.wg.Done()
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

### The runnable demo

The demo submits a mixed-priority batch *before* starting a single worker, so the loop drains all of it into the heap and then extracts in strict priority order. With one worker, extraction order is the order results appear, which is why this output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	dispatch "example.com/priority-dispatcher"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := dispatch.NewDispatcher(16)

	items := []*dispatch.Item{
		{Value: "debug trace", Priority: 20},
		{Value: "critical alert", Priority: 1},
		{Value: "warning", Priority: 5},
		{Value: "error report", Priority: 2},
		{Value: "info log", Priority: 10},
	}
	for _, it := range items {
		d.Submit(it)
	}
	d.Start(ctx, 1)

	for range items {
		it := <-d.Results()
		fmt.Printf("processed: %v (priority %d)\n", it.Value, it.Priority)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed: critical alert (priority 1)
processed: error report (priority 2)
processed: warning (priority 5)
processed: info log (priority 10)
processed: debug trace (priority 20)
```

### Tests

The tests pin each guarantee separately. The heap test extracts directly to confirm priority-then-sequence order and exercises the `Index` bookkeeping. The single-worker dispatch test pre-fills the queue and asserts strict priority order; the FIFO test does the same for equal priorities. The load test runs four workers against two hundred concurrently submitted items and asserts only that every one is processed — never a completion order — and is the test that earns the `-race` flag. The stop test confirms `Stop` returns promptly and a second call is a no-op.

Create `dispatcher_test.go`:

```go
package dispatch

import (
	"container/heap"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func collect(t *testing.T, d *Dispatcher, n int) []string {
	t.Helper()
	got := make([]string, 0, n)
	deadline := time.After(3 * time.Second)
	for len(got) < n {
		select {
		case it := <-d.Results():
			got = append(got, it.Value.(string))
		case <-deadline:
			t.Fatalf("timeout: got %d of %d (%v)", len(got), n, got)
		}
	}
	return got
}

func TestHeapOrdersByPriorityThenSeq(t *testing.T) {
	t.Parallel()

	h := &itemHeap{}
	heap.Init(h)
	heap.Push(h, &Item{Value: "a", Priority: 2, seq: 1})
	heap.Push(h, &Item{Value: "b", Priority: 1, seq: 2})
	heap.Push(h, &Item{Value: "c", Priority: 1, seq: 3})
	heap.Push(h, &Item{Value: "d", Priority: 0, seq: 4})

	var got []string
	for h.Len() > 0 {
		got = append(got, heap.Pop(h).(*Item).Value.(string))
	}
	want := []string{"d", "b", "c", "a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("heap order: got %v want %v", got, want)
		}
	}
}

func TestDispatchOrderSingleWorker(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := NewDispatcher(16)
	d.Submit(&Item{Value: "low", Priority: 3})
	d.Submit(&Item{Value: "high", Priority: 1})
	d.Submit(&Item{Value: "mid", Priority: 2})
	d.Start(ctx, 1)

	got := collect(t, d, 3)
	want := []string{"high", "mid", "low"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestFIFOWithinPriority(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := NewDispatcher(16)
	d.Submit(&Item{Value: "first", Priority: 1})
	d.Submit(&Item{Value: "second", Priority: 1})
	d.Submit(&Item{Value: "third", Priority: 1})
	d.Start(ctx, 1)

	got := collect(t, d, 3)
	want := []string{"first", "second", "third"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("FIFO position %d: got %q want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestConcurrentProducersAllProcessed(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := NewDispatcher(256)
	d.Start(ctx, 4)

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Submit(&Item{Value: i, Priority: i % 5})
		}()
	}
	wg.Wait()

	got := 0
	deadline := time.After(5 * time.Second)
	for got < n {
		select {
		case <-d.Results():
			got++
		case <-deadline:
			t.Fatalf("timeout: only %d of %d processed", got, n)
		}
	}
}

func TestGracefulStop(t *testing.T) {
	t.Parallel()

	d := NewDispatcher(16)
	d.Start(context.Background(), 2)
	d.Submit(&Item{Value: "x", Priority: 1})
	time.Sleep(20 * time.Millisecond)

	done := make(chan struct{})
	go func() { d.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}
	d.Stop() // idempotent: must not panic or block
}

func ExampleDispatcher() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := NewDispatcher(8)
	d.Submit(&Item{Value: "p3", Priority: 3})
	d.Submit(&Item{Value: "p1", Priority: 1})
	d.Submit(&Item{Value: "p2", Priority: 2})
	d.Start(ctx, 1)

	for i := 0; i < 3; i++ {
		fmt.Println((<-d.Results()).Value)
	}
	// Output:
	// p1
	// p2
	// p3
}
```

## Review

The dispatcher is correct when extraction follows priority-then-sequence and nothing is lost. The most common way to get it subtly wrong is to test it wrong: fanning out to several workers and asserting the results arrive in priority order is asserting timing, and it flakes the first time a fast worker laps a slow one. The fix is structural, not a longer sleep — assert dispatch order with a single worker draining a pre-filled queue, and assert only aggregate properties (all processed, race-clean) under multi-worker load. That single distinction is the difference between a test suite that means something and one that passes by luck.

The heap bookkeeping is the other place to look. `Swap` must update each element's `index` or `heap.Fix`/`heap.Remove` will edit the wrong element; `Pop` must trim the last slice element rather than search for the minimum, because the package has already sifted the minimum to the end. The drain-before-dispatch loop must block on `incoming` when the heap is empty rather than spinning, and the dispatch `select` must keep the popped item on a full `work` channel by pushing it back alongside any newly arrived item — re-stamping its sequence there would silently break first-in-first-out within its tier. Run the suite with `-race`; the load test is what will surface any accidental sharing of the heap.

## Resources

- [container/heap — Go standard library](https://pkg.go.dev/container/heap) — the five-method interface and the `Init`/`Push`/`Pop`/`Fix`/`Remove` operations this dispatcher is built on.
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — fan-out, fan-in, and context cancellation, the shape the dispatcher's loop and workers follow.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — buffered versus unbuffered semantics, which is why `work` is unbuffered and `incoming` is not.

---

Next: [02-preemptive-job-dispatcher.md](02-preemptive-job-dispatcher.md)
</content>
