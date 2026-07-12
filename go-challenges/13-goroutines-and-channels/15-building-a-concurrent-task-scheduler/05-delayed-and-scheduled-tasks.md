# Exercise 5: Delayed Execution — Run a Job After a Delay or at a Wall-Clock Time

Retry-after, reminder emails, and TTL cleanups all need deferred work:
`ScheduleAfter(d, fn)` and `ScheduleAt(t, fn)`. This module builds a timer
goroutine that owns a min-heap of scheduled entries and a single re-armable
`time.Timer`, feeding due tasks out for execution — and it gets the classic pitfall
right: re-arming the timer when a nearer task is inserted.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
delayed-scheduler/             module example.com/delayed-scheduler
  go.mod                       go 1.25
  scheduler.go                 min-heap by run-at; one re-armable time.Timer; Cancel via heap.Remove
  cmd/
    demo/
      main.go                  demo: reminder + cleanup fire in time order; one cancelled
  scheduler_test.go            fires-after-delay, nearer-re-arms, cancel-removes, -race
```

Files: `scheduler.go`, `cmd/demo/main.go`, `scheduler_test.go`.
Implement: `ScheduleAfter(d, fn)` / `ScheduleAt(t, fn)` returning a cancellation id; a timer goroutine owning a min-heap and one re-armable `time.Timer`; `Cancel(id)` via `heap.Remove`.
Test: schedule 50 ms out and assert it does not run before ~50 ms; schedule a far task then a nearer one and assert the nearer runs first (timer re-arm); assert `Cancel` on a pending task removes it and it never runs.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### One goroutine owns the heap and the timer

Deferred scheduling is a min-heap keyed by run-at, drained by a single
`time.Timer` armed for the earliest entry. As in the priority scheduler, a single
goroutine owns the heap outright, so there is no lock and no race; producers reach
it over channels (`add`, `cancel`). The loop recomputes the earliest deadline on
*every* iteration and re-arms the timer to it. That recompute is exactly what
handles the classic bug: when a nearer task is inserted while the timer is already
armed for a later one, the next loop iteration resets the timer to the nearer
deadline, so the nearer task is not delayed behind the previously-earliest one.
Forget the re-arm and the nearer task fires late.

The re-arm is simple here because Go 1.23 changed `time.Timer` channels to be
unbuffered and made `Stop`/`Reset` guarantee no stale value is delivered — the old
`if !t.Stop() { <-t.C }` drain idiom is gone. So the loop just calls
`timer.Reset(time.Until(earliest))` when the heap is non-empty and `timer.Stop()`
when it is empty. `Cancel` is synchronous: it sends the id to the loop, which
`heap.Remove`s the entry at its tracked heap index and replies whether it was
found. Tracking the index requires `Swap` and `Push`/`Pop` to keep each entry's
`index` field current, which is the standard `container/heap` bookkeeping for
`Remove`/`Fix`.

Create `scheduler.go`:

```go
package scheduler

import (
	"container/heap"
	"sync"
	"sync/atomic"
	"time"
)

// entry is one scheduled task. index is its position in the heap, maintained by
// the heap methods so Cancel can heap.Remove it.
type entry struct {
	runAt time.Time
	fn    func()
	id    uint64
	index int
}

type entryHeap []*entry

func (h entryHeap) Len() int           { return len(h) }
func (h entryHeap) Less(i, j int) bool { return h[i].runAt.Before(h[j].runAt) }
func (h entryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *entryHeap) Push(x any) {
	e := x.(*entry)
	e.index = len(*h)
	*h = append(*h, e)
}

func (h *entryHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	*h = old[:n-1]
	return e
}

type cancelReq struct {
	id    uint64
	reply chan bool
}

// Scheduler runs functions after a delay or at a wall-clock time.
type Scheduler struct {
	add      chan *entry
	cancel   chan cancelReq
	quit     chan struct{}
	stopOnce sync.Once
	loopWG   sync.WaitGroup
	taskWG   sync.WaitGroup
	nextID   atomic.Uint64
}

// New starts the timer goroutine.
func New() *Scheduler {
	s := &Scheduler{
		add:    make(chan *entry),
		cancel: make(chan cancelReq),
		quit:   make(chan struct{}),
	}
	s.loopWG.Add(1)
	go s.run()
	return s
}

func (s *Scheduler) run() {
	defer s.loopWG.Done()

	var h entryHeap
	byID := make(map[uint64]*entry)

	timer := time.NewTimer(time.Hour)
	timer.Stop()

	for {
		var timerC <-chan time.Time
		if h.Len() > 0 {
			d := time.Until(h[0].runAt)
			if d < 0 {
				d = 0
			}
			timer.Reset(d)
			timerC = timer.C
		} else {
			timer.Stop()
		}

		select {
		case e := <-s.add:
			heap.Push(&h, e)
			byID[e.id] = e
		case req := <-s.cancel:
			e, ok := byID[req.id]
			if ok {
				heap.Remove(&h, e.index)
				delete(byID, req.id)
			}
			req.reply <- ok
		case now := <-timerC:
			for h.Len() > 0 && !h[0].runAt.After(now) {
				e := heap.Pop(&h).(*entry)
				delete(byID, e.id)
				s.taskWG.Add(1)
				go func(fn func()) {
					defer s.taskWG.Done()
					fn()
				}(e.fn)
			}
		case <-s.quit:
			timer.Stop()
			return
		}
	}
}

// ScheduleAfter runs fn after d has elapsed and returns a cancellation id.
func (s *Scheduler) ScheduleAfter(d time.Duration, fn func()) uint64 {
	return s.ScheduleAt(time.Now().Add(d), fn)
}

// ScheduleAt runs fn at wall-clock time t and returns a cancellation id.
func (s *Scheduler) ScheduleAt(t time.Time, fn func()) uint64 {
	id := s.nextID.Add(1)
	e := &entry{runAt: t, fn: fn, id: id}
	select {
	case s.add <- e:
	case <-s.quit:
	}
	return id
}

// Cancel removes a scheduled-but-not-yet-run task. It returns false if the id is
// unknown (already run, already cancelled, or never scheduled).
func (s *Scheduler) Cancel(id uint64) bool {
	reply := make(chan bool, 1)
	select {
	case s.cancel <- cancelReq{id: id, reply: reply}:
		return <-reply
	case <-s.quit:
		return false
	}
}

// Stop halts the timer goroutine and waits for already-fired tasks to finish.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.quit) })
	s.loopWG.Wait()
	s.taskWG.Wait()
}
```

### The runnable demo

The demo schedules a reminder (20 ms), a cleanup (60 ms), and a third task it then
cancels, and prints the two that fire, in time order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/delayed-scheduler"
)

func main() {
	s := scheduler.New()
	defer s.Stop()

	done := make(chan string, 3)
	s.ScheduleAfter(60*time.Millisecond, func() { done <- "cleanup" })
	s.ScheduleAfter(20*time.Millisecond, func() { done <- "reminder" })

	cancelled := s.ScheduleAfter(40*time.Millisecond, func() { done <- "should-not-run" })
	s.Cancel(cancelled)

	fmt.Println(<-done)
	fmt.Println(<-done)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
reminder
cleanup
```

### Tests

`TestScheduleAfterFires` asserts a 50 ms task does not fire noticeably early (a
loose lower bound to stay CI-stable). `TestNearerTaskReArmsTimer` schedules a far
task, then a nearer one, and asserts the nearer fires first — the proof the timer
re-armed. `TestCancelRemovesTask` cancels a pending task and asserts it never runs
and that a second cancel of the same id returns false.

Create `scheduler_test.go`:

```go
package scheduler

import (
	"fmt"
	"testing"
	"time"
)

func TestScheduleAfterFires(t *testing.T) {
	t.Parallel()

	s := New()
	defer s.Stop()

	fired := make(chan time.Time, 1)
	start := time.Now()
	s.ScheduleAfter(50*time.Millisecond, func() { fired <- time.Now() })

	select {
	case at := <-fired:
		if elapsed := at.Sub(start); elapsed < 40*time.Millisecond {
			t.Fatalf("fired too early: %v (want >= ~50ms)", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("task never fired")
	}
}

func TestNearerTaskReArmsTimer(t *testing.T) {
	t.Parallel()

	s := New()
	defer s.Stop()

	order := make(chan string, 2)
	s.ScheduleAfter(500*time.Millisecond, func() { order <- "far" })
	s.ScheduleAfter(50*time.Millisecond, func() { order <- "near" })

	if first := <-order; first != "near" {
		t.Fatalf("first fired = %q, want near (timer did not re-arm)", first)
	}
	if second := <-order; second != "far" {
		t.Fatalf("second fired = %q, want far", second)
	}
}

func TestCancelRemovesTask(t *testing.T) {
	t.Parallel()

	s := New()
	defer s.Stop()

	ran := make(chan struct{}, 1)
	id := s.ScheduleAfter(50*time.Millisecond, func() { ran <- struct{}{} })

	if !s.Cancel(id) {
		t.Fatal("Cancel returned false for a pending task")
	}
	select {
	case <-ran:
		t.Fatal("cancelled task ran anyway")
	case <-time.After(150 * time.Millisecond):
		// good: it never ran
	}

	if s.Cancel(id) {
		t.Fatal("Cancel of an already-removed id returned true")
	}
}

func Example() {
	s := New()
	defer s.Stop()

	done := make(chan struct{})
	s.ScheduleAfter(time.Millisecond, func() { close(done) })
	<-done
	fmt.Println("task ran")
	// Output: task ran
}
```

## Review

Delayed scheduling is correct when the loop re-arms the timer to the current
earliest run-at on every iteration; that is what makes a nearer task inserted
after a far one fire on time rather than behind it. Owning the heap and the timer
in one goroutine keeps `container/heap` lock-free and `-race` clean, and the
`index` bookkeeping in `Swap`/`Push`/`Pop` is what lets `Cancel` `heap.Remove` an
arbitrary entry. On Go 1.23+ the `Timer.Reset` discipline is simply "reset when
non-empty, stop when empty" — no channel-drain idiom. Keep timing assertions loose
(a lower bound, not an exact match) so the tests do not flake on a loaded CI box.
Run `go test -race -count=1 ./...`.

## Resources

- [`time.Timer` and `Timer.Reset`](https://pkg.go.dev/time#Timer.Reset) — the re-arm contract, including the Go 1.23 channel change.
- [`container/heap` — Remove](https://pkg.go.dev/container/heap#Remove) — removing an arbitrary entry by index.
- [Go 1.23 release notes: timer changes](https://go.dev/doc/go1.23#timer-changes) — why the drain idiom is no longer needed.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-priority-scheduler-heap.md](04-priority-scheduler-heap.md) | Next: [06-per-task-timeout-and-cancellation.md](06-per-task-timeout-and-cancellation.md)
