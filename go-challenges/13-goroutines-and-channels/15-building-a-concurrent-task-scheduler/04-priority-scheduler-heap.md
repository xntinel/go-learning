# Exercise 4: Priority Dispatch — Run High-Priority Jobs First

When a backlog mixes latency-sensitive work (a password-reset email) with bulk
batch work (a nightly digest), a plain FIFO queue makes the interactive job wait
behind the batch. This module inserts a priority queue between `Submit` and the
worker pool: a single dispatcher goroutine owns a `container/heap` and always
hands the highest-priority ready task to the next idle worker.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
priority-scheduler/            module example.com/priority-scheduler
  go.mod                       go 1.25
  scheduler.go                 priorityQueue (heap.Interface); dispatcher owns the heap
  cmd/
    demo/
      main.go                  demo: three jobs dispatched by priority
  scheduler_test.go            ordering test, 1000-task non-increasing property, -race
```

Files: `scheduler.go`, `cmd/demo/main.go`, `scheduler_test.go`.
Implement: `priorityQueue` satisfying `heap.Interface` (`Len`/`Less`/`Swap`/`Push`/`Pop`); a single dispatcher goroutine that owns the heap; `Submit(priority, Task) <-chan Result`.
Test: pause the dispatcher by holding the worker busy, enqueue mixed priorities, release, and assert completion order is by priority then FIFO within a priority; a property test that pushes 1000 random-priority tasks and asserts the recorded order is non-increasing in priority.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/priority-scheduler/cmd/demo
cd ~/go-exercises/priority-scheduler
go mod init example.com/priority-scheduler
go mod edit -go=1.25
```

### Single ownership: one goroutine owns the heap

A `container/heap` is a plain slice with an ordering discipline; it is *not* safe
for concurrent use. Wrapping every `heap.Push`/`heap.Pop` in a mutex works but is
easy to get subtly wrong and serializes the hot path. The idiomatic Go answer is
single ownership: exactly one dispatcher goroutine ever touches the heap, so no
lock is needed and the data race is structurally impossible. Producers hand items
to the dispatcher over a channel; the dispatcher hands the chosen item to a worker
over another channel. Channels move ownership, not just data.

The dispatcher loop is the classic select-driven pattern. When the heap is
non-empty it selects over three cases: receive a newly submitted item and
`heap.Push` it, *or* send the current top (`pq[0]`) to a worker and `heap.Pop` it,
*or* quit. Because the same goroutine does both the push and the pop, the heap is
always consistent, and the top sent to a worker is always the highest-priority
ready item at the instant a worker becomes free. The unbuffered `dispatch` channel
is what makes "at the instant a worker becomes free" true: the dispatcher's send
blocks until a worker actually receives, so it never front-loads several items
into a buffer in the wrong order.

Ordering is "higher priority first, FIFO within a priority". The FIFO tiebreak
needs a monotonic sequence number; the dispatcher assigns it at push time, and
because `Submit` blocks on an unbuffered channel until the dispatcher accepts the
item, submission order and sequence order agree. `Less` compares priority
descending, then sequence ascending.

Create `scheduler.go`:

```go
package scheduler

import (
	"container/heap"
	"errors"
	"sync"
)

// ErrShuttingDown is delivered on the result channel when Submit races Stop.
var ErrShuttingDown = errors.New("scheduler shutting down")

type Task func() (any, error)

type Result struct {
	Value any
	Err   error
}

type item struct {
	fn       Task
	priority int
	seq      uint64
	done     chan Result
}

// priorityQueue orders items by priority descending, then by seq ascending
// (FIFO within a priority). It implements heap.Interface.
type priorityQueue []*item

func (pq priorityQueue) Len() int { return len(pq) }

func (pq priorityQueue) Less(i, j int) bool {
	if pq[i].priority != pq[j].priority {
		return pq[i].priority > pq[j].priority
	}
	return pq[i].seq < pq[j].seq
}

func (pq priorityQueue) Swap(i, j int) { pq[i], pq[j] = pq[j], pq[i] }

func (pq *priorityQueue) Push(x any) {
	*pq = append(*pq, x.(*item))
}

func (pq *priorityQueue) Pop() any {
	old := *pq
	n := len(old)
	it := old[n-1]
	old[n-1] = nil // avoid holding a reference
	*pq = old[:n-1]
	return it
}

// Scheduler dispatches tasks to a worker pool in priority order.
type Scheduler struct {
	submit   chan *item
	dispatch chan *item
	quit     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New starts the worker pool and the single heap-owning dispatcher.
func New(workers int) *Scheduler {
	if workers < 1 {
		workers = 1
	}
	s := &Scheduler{
		submit:   make(chan *item),
		dispatch: make(chan *item),
		quit:     make(chan struct{}),
	}
	s.wg.Add(1)
	go s.dispatcher()
	for range workers {
		s.wg.Add(1)
		go s.worker()
	}
	return s
}

func (s *Scheduler) worker() {
	defer s.wg.Done()
	for it := range s.dispatch {
		v, err := it.fn()
		it.done <- Result{Value: v, Err: err}
	}
}

// dispatcher is the sole owner of the heap.
func (s *Scheduler) dispatcher() {
	defer s.wg.Done()
	var pq priorityQueue
	var seq uint64
loop:
	for {
		if len(pq) == 0 {
			select {
			case it := <-s.submit:
				it.seq = seq
				seq++
				heap.Push(&pq, it)
			case <-s.quit:
				break loop
			}
			continue
		}
		select {
		case it := <-s.submit:
			it.seq = seq
			seq++
			heap.Push(&pq, it)
		case s.dispatch <- pq[0]:
			heap.Pop(&pq)
		case <-s.quit:
			break loop
		}
	}
	close(s.dispatch) // stop the workers once the dispatcher exits
}

// Submit enqueues fn at the given priority (higher runs first) and returns a
// capacity-1 result channel.
func (s *Scheduler) Submit(priority int, fn Task) <-chan Result {
	done := make(chan Result, 1)
	it := &item{fn: fn, priority: priority, done: done}
	select {
	case s.submit <- it:
	case <-s.quit:
		done <- Result{Err: ErrShuttingDown}
	}
	return done
}

// Stop signals the dispatcher and joins it and every worker.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.quit) })
	s.wg.Wait()
}
```

### The runnable demo

The demo holds the single worker busy with a blocker task while three real jobs of
mixed priority are submitted, so all three land in the heap before any is
dispatched. Releasing the gate then drains them in priority order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/priority-scheduler"
)

func main() {
	s := scheduler.New(1)
	defer s.Stop()

	var mu sync.Mutex
	var order []string
	rec := func(label string) scheduler.Task {
		return func() (any, error) {
			mu.Lock()
			order = append(order, label)
			mu.Unlock()
			return nil, nil
		}
	}

	gate := make(chan struct{})
	started := make(chan struct{})
	s.Submit(100, func() (any, error) {
		close(started)
		<-gate
		return nil, nil
	})
	<-started // the single worker is now busy

	var dones []<-chan scheduler.Result
	dones = append(dones, s.Submit(1, rec("email-digest")))
	dones = append(dones, s.Submit(9, rec("password-reset")))
	dones = append(dones, s.Submit(5, rec("thumbnail")))

	close(gate) // release the worker; the heap drains by priority
	for _, d := range dones {
		<-d
	}

	mu.Lock()
	fmt.Println(order)
	mu.Unlock()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[password-reset thumbnail email-digest]
```

### Tests

`TestPriorityOrder` proves both axes of the ordering: high before mid before low,
and FIFO within each priority (the `-a` label of a priority runs before its `-b`).
`TestPriorityNonIncreasing` is a property test: 1000 random-priority tasks, all
buffered in the heap behind a blocker, must dispatch in non-increasing priority.

Create `scheduler_test.go`:

```go
package scheduler

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
)

func TestPriorityOrder(t *testing.T) {
	t.Parallel()

	s := New(1)
	defer s.Stop()

	var mu sync.Mutex
	var order []string
	rec := func(label string) Task {
		return func() (any, error) {
			mu.Lock()
			order = append(order, label)
			mu.Unlock()
			return label, nil
		}
	}

	gate := make(chan struct{})
	started := make(chan struct{})
	s.Submit(1000, func() (any, error) {
		close(started)
		<-gate
		return nil, nil
	})
	<-started // hold the single worker busy

	tasks := []struct {
		label string
		prio  int
	}{
		{"low-a", 1},
		{"high-a", 9},
		{"mid-a", 5},
		{"high-b", 9},
		{"low-b", 1},
		{"mid-b", 5},
	}
	var dones []<-chan Result
	for _, tk := range tasks {
		dones = append(dones, s.Submit(tk.prio, rec(tk.label)))
	}

	close(gate)
	for _, d := range dones {
		<-d
	}

	mu.Lock()
	got := slices.Clone(order)
	mu.Unlock()

	want := []string{"high-a", "high-b", "mid-a", "mid-b", "low-a", "low-b"}
	if !slices.Equal(got, want) {
		t.Fatalf("dispatch order = %v, want %v", got, want)
	}
}

func TestPriorityNonIncreasing(t *testing.T) {
	t.Parallel()

	s := New(1)
	defer s.Stop()

	var mu sync.Mutex
	var prios []int

	gate := make(chan struct{})
	started := make(chan struct{})
	s.Submit(1<<30, func() (any, error) {
		close(started)
		<-gate
		return nil, nil
	})
	<-started

	const n = 1000
	var dones []<-chan Result
	for range n {
		p := rand.IntN(100)
		dones = append(dones, s.Submit(p, func() (any, error) {
			mu.Lock()
			prios = append(prios, p)
			mu.Unlock()
			return nil, nil
		}))
	}

	close(gate)
	for _, d := range dones {
		<-d
	}

	mu.Lock()
	got := slices.Clone(prios)
	mu.Unlock()

	if len(got) != n {
		t.Fatalf("recorded %d priorities, want %d", len(got), n)
	}
	for i := 1; i < len(got); i++ {
		if got[i] > got[i-1] {
			t.Fatalf("order not non-increasing at %d: %d > %d", i, got[i], got[i-1])
		}
	}
}

func Example() {
	s := New(2)
	defer s.Stop()

	r := <-s.Submit(5, func() (any, error) { return "ok", nil })
	fmt.Println(r.Value, r.Err)
	// Output: ok <nil>
}
```

## Review

Priority dispatch is correct when the dispatcher is the *only* goroutine that
touches the heap — that single-ownership discipline is what lets `container/heap`
run without a lock and keeps `-race` clean. The unbuffered `dispatch` channel is
load-bearing: it makes the dispatcher pick the top only when a worker is genuinely
free, so a higher-priority task submitted a moment later can still overtake a
lower one that has not been handed out yet. `Less` encodes both axes — priority
descending, sequence ascending — so ties break FIFO. The common failure is buffering
the dispatch channel, which lets several tasks queue in submission order and
defeats the priority ordering. Run `go test -race -count=1 ./...` and
`go vet ./...` to confirm the `heap.Interface` methods and the dispatcher are
sound.

## Resources

- [`container/heap`](https://pkg.go.dev/container/heap) — `heap.Interface`, `Init`, `Push`, `Pop`, `Fix`, `Remove`.
- [`container/heap` PriorityQueue example](https://pkg.go.dev/container/heap#example-package-PriorityQueue) — the canonical priority-queue implementation.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — single-owner goroutines communicating over channels.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-graceful-shutdown-drain-vs-cancel.md](03-graceful-shutdown-drain-vs-cancel.md) | Next: [05-delayed-and-scheduled-tasks.md](05-delayed-and-scheduled-tasks.md)
