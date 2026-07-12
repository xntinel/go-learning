# Exercise 9: A Delayed-Retry Scheduler — container/heap of *Task With heap.Fix

A backoff scheduler is a min-heap of tasks ordered by their next run time, and the
reason it stores `*Task` rather than `Task` is operational: pointers let each task
carry an `index` field that `Swap` maintains, which turns reprioritization into an
O(log n) `heap.Fix` instead of an O(n) scan. This module builds that priority
queue.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
retryheap/                    independent module: example.com/retryheap
  go.mod                      go 1.24
  scheduler.go                Task (index field); priorityQueue (heap.Interface over []*Task); Scheduler API
  scheduler_test.go           pop order; O(log n) UpdatePriority via heap.Fix; Remove; index maintained on Swap
  cmd/demo/main.go            runnable demo scheduling and reprioritizing tasks
```

Files: `scheduler.go`, `scheduler_test.go`, `cmd/demo/main.go`.
Implement: a `priorityQueue` implementing `heap.Interface` over `[]*Task`, with an
`index` field updated in `Swap`; a `Scheduler` with `Schedule`, `Pop`,
`Reschedule` (mutate `NextRunAt` then `heap.Fix`), and `Remove`.
Test: pop returns tasks in ascending `NextRunAt`; `Reschedule` moves a task to the
front; `Remove` preserves the heap invariant; each task's `index` equals its slice
position.
Verify: `go test -count=1 -race ./...`

### Why pointers and an index field

`container/heap` does not own storage; you implement `heap.Interface` (which embeds
`sort.Interface`'s `Len`/`Less`/`Swap` plus `Push(x any)` and `Pop() any`) over a
slice, and the package's functions (`heap.Init`, `heap.Push`, `heap.Pop`,
`heap.Fix`, `heap.Remove`) drive it by calling your methods. The classic use is a
min-heap where `Less(i, j)` compares priorities — here `NextRunAt`, so the earliest
task sits at the root and `Pop` returns the next one due.

The operational requirement in a scheduler is *reprioritization*: a task's backoff
changes, so its `NextRunAt` moves, and the heap must reorder. Doing that in O(log n)
needs to know *where* the task currently sits in the slice, and that is what the
`index` field is for. Store `[]*Task` and give `Task` an `index int` field that
`Swap` updates every time it exchanges two elements. Then a caller holding a
`*Task` handle can mutate `NextRunAt` in place and call `heap.Fix(pq, task.index)`,
which restores the invariant from that position in log time — no scan to find the
task. `heap.Remove(pq, task.index)` deletes it in log time the same way. A value
heap (`[]Task`) cannot do this: there is no stable handle for a caller to hold, and
`Swap` copying values around means the "index" a caller cached is immediately
wrong. Pointers plus the maintained index are what make the scheduler's live
handles work.

One correctness detail in `Push`/`Pop`: `Push` must set the new element's `index`
to the end position before it is sifted up, and `Pop` must return the last element
(the heap functions move the popped root to the end first) and set its `index` to
-1 to mark it detached. Getting these wrong corrupts the index bookkeeping.

Create `scheduler.go`:

```go
package retryheap

import (
	"container/heap"
	"time"
)

// Task is a scheduled unit of work. index is its position in the heap's backing
// slice, maintained by Swap so Reschedule/Remove are O(log n).
type Task struct {
	ID        string
	NextRunAt time.Time
	index     int
}

// priorityQueue is a min-heap of *Task ordered by NextRunAt.
type priorityQueue []*Task

func (pq priorityQueue) Len() int { return len(pq) }

func (pq priorityQueue) Less(i, j int) bool {
	return pq[i].NextRunAt.Before(pq[j].NextRunAt)
}

func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i // keep each task's cached position correct
	pq[j].index = j
}

func (pq *priorityQueue) Push(x any) {
	t := x.(*Task)
	t.index = len(*pq)
	*pq = append(*pq, t)
}

func (pq *priorityQueue) Pop() any {
	old := *pq
	n := len(old)
	t := old[n-1]
	old[n-1] = nil // drop the reference so the popped task can be collected
	t.index = -1   // mark detached
	*pq = old[:n-1]
	return t
}

// Scheduler is a delayed-retry queue. Callers hold the *Task returned by
// Schedule and can Reschedule or Remove it later.
type Scheduler struct {
	pq priorityQueue
}

func NewScheduler() *Scheduler {
	s := &Scheduler{}
	heap.Init(&s.pq)
	return s
}

// Schedule enqueues a task to run at runAt and returns its live handle.
func (s *Scheduler) Schedule(id string, runAt time.Time) *Task {
	t := &Task{ID: id, NextRunAt: runAt}
	heap.Push(&s.pq, t)
	return t
}

// Pop removes and returns the earliest-due task, or nil if empty.
func (s *Scheduler) Pop() *Task {
	if s.pq.Len() == 0 {
		return nil
	}
	return heap.Pop(&s.pq).(*Task)
}

// Reschedule mutates a live task's NextRunAt in place and restores the heap in
// O(log n) using its maintained index.
func (s *Scheduler) Reschedule(t *Task, runAt time.Time) {
	t.NextRunAt = runAt
	heap.Fix(&s.pq, t.index)
}

// Remove deletes a live task from the queue in O(log n).
func (s *Scheduler) Remove(t *Task) {
	if t.index >= 0 && t.index < s.pq.Len() {
		heap.Remove(&s.pq, t.index)
	}
}

func (s *Scheduler) Len() int { return s.pq.Len() }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/retryheap"
)

func main() {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := retryheap.NewScheduler()

	s.Schedule("a", base.Add(30*time.Second))
	b := s.Schedule("b", base.Add(60*time.Second))
	s.Schedule("c", base.Add(90*time.Second))

	// b's backoff shortens: it should now run first.
	s.Reschedule(b, base.Add(5*time.Second))

	for t := s.Pop(); t != nil; t = s.Pop() {
		fmt.Printf("%s at +%s\n", t.ID, t.NextRunAt.Sub(base))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
b at +5s
a at +30s
c at +1m30s
```

### Tests

`TestPopOrderByNextRun` pushes out-of-order times and asserts `Pop` yields ascending
`NextRunAt`. `TestUpdatePriorityReorders` mutates a task earlier and asserts it now
pops first. `TestRemoveByIndex` removes a middle task and validates the heap
invariant over the array (each parent ≤ its children). `TestIndexFieldMaintainedOnSwap`
asserts every task's `index` equals its slice position after operations.

Create `scheduler_test.go`:

```go
package retryheap

import (
	"testing"
	"time"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }

func TestPopOrderByNextRun(t *testing.T) {
	t.Parallel()

	s := NewScheduler()
	s.Schedule("c", at(90))
	s.Schedule("a", at(30))
	s.Schedule("b", at(60))

	var order []string
	for task := s.Pop(); task != nil; task = s.Pop() {
		order = append(order, task.ID)
	}
	want := []string{"a", "b", "c"}
	if len(order) != len(want) {
		t.Fatalf("popped %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("popped %v, want %v", order, want)
		}
	}
}

func TestUpdatePriorityReorders(t *testing.T) {
	t.Parallel()

	s := NewScheduler()
	s.Schedule("a", at(30))
	b := s.Schedule("b", at(60))
	s.Schedule("c", at(90))

	s.Reschedule(b, at(5)) // b now earliest

	if got := s.Pop(); got == nil || got.ID != "b" {
		t.Fatalf("first pop = %v, want b", got)
	}
}

func TestRemoveByIndex(t *testing.T) {
	t.Parallel()

	s := NewScheduler()
	s.Schedule("a", at(30))
	b := s.Schedule("b", at(60))
	s.Schedule("c", at(90))
	s.Schedule("d", at(120))

	s.Remove(b) // remove a middle task

	// Validate the min-heap invariant over the backing array.
	for i := range s.pq {
		l, r := 2*i+1, 2*i+2
		if l < len(s.pq) && s.pq[l].NextRunAt.Before(s.pq[i].NextRunAt) {
			t.Fatalf("heap invariant broken at %d (left child earlier)", i)
		}
		if r < len(s.pq) && s.pq[r].NextRunAt.Before(s.pq[i].NextRunAt) {
			t.Fatalf("heap invariant broken at %d (right child earlier)", i)
		}
	}
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3", s.Len())
	}
}

func TestIndexFieldMaintainedOnSwap(t *testing.T) {
	t.Parallel()

	s := NewScheduler()
	s.Schedule("a", at(50))
	s.Schedule("b", at(10))
	s.Schedule("c", at(80))
	s.Schedule("d", at(20))
	s.Schedule("e", at(40))

	for i := range s.pq {
		if s.pq[i].index != i {
			t.Fatalf("task %s index = %d, want %d", s.pq[i].ID, s.pq[i].index, i)
		}
	}
}
```

## Review

Storing `[]*Task` is not a micro-optimization here; it is what makes the scheduler's
core operation possible. Because `Swap` updates each task's `index`, a caller can
hold the `*Task` returned by `Schedule`, change its `NextRunAt`, and call
`heap.Fix(pq, t.index)` for an O(log n) reprioritization —
`TestUpdatePriorityReorders` proves the moved task pops first, and
`TestIndexFieldMaintainedOnSwap` proves the bookkeeping that makes it correct.
`Pop` nils the vacated slot and sets the detached task's `index` to -1 so a stale
handle cannot be mistaken for a live position. The value-heap alternative has no
stable handle and no maintainable index, forcing an O(n) find-then-remove and
handing callers copies that go stale the instant the heap reorders. Validate the
invariant directly, as `TestRemoveByIndex` does, rather than trusting that
`heap.Remove` "probably" kept order.

## Resources

- [`container/heap`](https://pkg.go.dev/container/heap) — `Interface`, `Init`, `Push`, `Pop`, `Fix`, `Remove`, and the priority-queue example with an `index` field.
- [`heap.Fix`](https://pkg.go.dev/container/heap#Fix) — re-establishes the ordering after the element at index i changes priority.
- [`sort.Interface`](https://pkg.go.dev/sort#Interface) — the `Len`/`Less`/`Swap` that `heap.Interface` embeds.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-deep-copy-api-boundary.md](10-deep-copy-api-boundary.md)
