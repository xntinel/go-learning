# Exercise 8: Cost-Weighted Admission — Cap Total In-Flight Work by Resource Weight

A fixed-count worker pool over-commits when tasks are heterogeneous: eight
"workers" each running a 2 GB job blows the memory envelope. This module admits
work by *cost* instead, using `golang.org/x/sync/semaphore.Weighted`, so a few
heavy jobs and many light jobs share a fixed resource budget without
over-committing.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
weighted-scheduler/            module example.com/weighted-scheduler
  go.mod                       go 1.25; require golang.org/x/sync
  scheduler.go                 semaphore.Weighted admission by cost; TrySubmit, Submit
  cmd/
    demo/
      main.go                  demo: weight-8 blocks a second weight-8 but not a weight-2
  scheduler_test.go            over-capacity, cancelled Acquire, paired Release, -race
```

Files: `scheduler.go`, `cmd/demo/main.go`, `scheduler_test.go`.
Implement: `New(capacity)`; `TrySubmit(weight, Task)` returning `ErrOverCapacity`; `Submit(ctx, weight, Task)` whose `Acquire` honors context cancellation; `Release` always paired via `defer`.
Test: capacity 10, one weight-8 task blocking, assert a second weight-8 `TrySubmit` fails while a weight-2 admits; assert a blocking `Submit` with a cancelled context returns `ctx.Err()` and does not deadlock; assert capacity is fully restored after all tasks (a later weight-10 admits).
Verify: `go test -count=1 -race ./...`

Set up the module (this one has an external dependency):

```bash
go mod edit -go=1.25
go get golang.org/x/sync/semaphore
```

### Bound by cost, not by count

A fixed worker pool bounds concurrency by goroutine count — the wrong unit when
tasks differ wildly in resource cost. `semaphore.Weighted` bounds it by a
*capacity budget*: each task declares a weight, `Acquire(ctx, w)` admits it only if
`w` units are free (blocking, up to the caller's context, otherwise), and
`TryAcquire(w)` is the non-blocking form. With capacity 10, a weight-8 task and a
weight-2 task fit together; a second weight-8 must wait. That is the whole model:
heterogeneous work sharing one envelope.

Two disciplines make it correct. First, every `Acquire`/`TryAcquire` of weight `w`
must be paired with exactly `Release(w)` on *every* path, including panics — so the
run goroutine `defer`s the release. Releasing a different weight than acquired, or
skipping it on an error path, permanently shrinks effective capacity until the
scheduler stalls. Second, a weight larger than the total capacity can never be
admitted; `Acquire` would block forever, so `Submit`/`TrySubmit` reject it up front
with `ErrTooLarge` rather than hang. `Acquire` honoring context cancellation is
what lets a blocked admission abort at the caller's deadline instead of
deadlocking when the budget is exhausted.

Create `scheduler.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/sync/semaphore"
)

var (
	// ErrOverCapacity is returned by TrySubmit when the budget cannot fit the weight now.
	ErrOverCapacity = errors.New("over capacity")
	// ErrTooLarge is returned when a task's weight exceeds total capacity.
	ErrTooLarge = errors.New("weight exceeds capacity")
	// ErrShuttingDown is returned after Stop.
	ErrShuttingDown = errors.New("scheduler shutting down")
)

type Task func() (any, error)

type Result struct {
	Value any
	Err   error
}

// Scheduler admits tasks against a total capacity budget by cost weight.
type Scheduler struct {
	sem      *semaphore.Weighted
	capacity int64
	wg       sync.WaitGroup

	mu     sync.Mutex
	closed bool
}

// New builds a Scheduler with the given total capacity budget.
func New(capacity int64) *Scheduler {
	if capacity < 1 {
		capacity = 1
	}
	return &Scheduler{
		sem:      semaphore.NewWeighted(capacity),
		capacity: capacity,
	}
}

func (s *Scheduler) run(weight int64, fn Task, done chan Result) {
	defer s.wg.Done()
	defer s.sem.Release(weight) // always paired with the Acquire, even on panic
	v, err := fn()
	done <- Result{Value: v, Err: err}
}

func (s *Scheduler) reject(weight int64) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrShuttingDown
	}
	if weight > s.capacity {
		return ErrTooLarge
	}
	return nil
}

// TrySubmit admits fn without blocking. It returns ErrOverCapacity if the weight
// cannot fit the remaining budget right now.
func (s *Scheduler) TrySubmit(weight int64, fn Task) (<-chan Result, error) {
	if err := s.reject(weight); err != nil {
		return nil, err
	}
	if !s.sem.TryAcquire(weight) {
		return nil, ErrOverCapacity
	}
	done := make(chan Result, 1)
	s.wg.Add(1)
	go s.run(weight, fn, done)
	return done, nil
}

// Submit blocks (up to ctx) until weight units are available, then admits fn.
func (s *Scheduler) Submit(ctx context.Context, weight int64, fn Task) (<-chan Result, error) {
	if err := s.reject(weight); err != nil {
		return nil, err
	}
	if err := s.sem.Acquire(ctx, weight); err != nil {
		return nil, err // e.g. context.Canceled / context.DeadlineExceeded
	}
	done := make(chan Result, 1)
	s.wg.Add(1)
	go s.run(weight, fn, done)
	return done, nil
}

// Stop rejects new submits and waits for admitted tasks to finish.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.wg.Wait()
}
```

### The runnable demo

The demo sets capacity 10, holds a weight-8 task busy on a gate, shows a second
weight-8 shed while a weight-2 admits, then releases and drains.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/weighted-scheduler"
)

func main() {
	s := scheduler.New(10)
	defer s.Stop()

	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	heavy := func() (any, error) {
		started <- struct{}{}
		<-gate
		return "heavy done", nil
	}

	d8, _ := s.TrySubmit(8, heavy)
	<-started
	fmt.Println("weight-8 admitted")

	if _, err := s.TrySubmit(8, heavy); err != nil {
		fmt.Println("second weight-8:", err)
	}

	d2, _ := s.TrySubmit(2, func() (any, error) { return "light done", nil })
	fmt.Println("weight-2:", (<-d2).Value)

	close(gate)
	fmt.Println("weight-8:", (<-d8).Value)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
weight-8 admitted
second weight-8: over capacity
weight-2: light done
weight-8: heavy done
```

### Tests

`TestWeightedAdmission` proves the budget math: a weight-8 task leaves room for a
weight-2 but not a second weight-8, and after release a blocking weight-10
`Submit` (which waits for the paired `Release`) succeeds — proof capacity was fully
restored. `TestAcquireRespectsContext` proves a blocked `Submit` aborts on a
cancelled context instead of deadlocking.

Create `scheduler_test.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestWeightedAdmission(t *testing.T) {
	t.Parallel()

	s := New(10)
	defer s.Stop()

	gate := make(chan struct{})
	started := make(chan struct{}, 2)
	block := func() (any, error) {
		started <- struct{}{}
		<-gate
		return "done", nil
	}

	d8, err := s.TrySubmit(8, block)
	if err != nil {
		t.Fatalf("weight-8 TrySubmit: %v", err)
	}
	<-started // it has acquired 8 and is running

	if _, err := s.TrySubmit(8, block); !errors.Is(err, ErrOverCapacity) {
		t.Fatalf("second weight-8: err = %v, want ErrOverCapacity", err)
	}

	d2, err := s.TrySubmit(2, block)
	if err != nil {
		t.Fatalf("weight-2 TrySubmit: %v", err)
	}
	<-started

	// Reject a weight larger than total capacity up front.
	if _, err := s.TrySubmit(11, block); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("weight-11: err = %v, want ErrTooLarge", err)
	}

	// Release everything.
	close(gate)
	if r := <-d8; r.Value != "done" {
		t.Fatalf("weight-8 result = %v, want done", r.Value)
	}
	if r := <-d2; r.Value != "done" {
		t.Fatalf("weight-2 result = %v, want done", r.Value)
	}

	// Full capacity must be admittable again (Release was paired).
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	dFull, err := s.Submit(ctx, 10, func() (any, error) { return "full", nil })
	if err != nil {
		t.Fatalf("full-capacity Submit after release: %v", err)
	}
	if r := <-dFull; r.Err != nil || r.Value != "full" {
		t.Fatalf("full-capacity result = (%v, %v), want (full, nil)", r.Value, r.Err)
	}
}

func TestAcquireRespectsContext(t *testing.T) {
	t.Parallel()

	s := New(4)
	defer s.Stop()

	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	d, err := s.TrySubmit(4, func() (any, error) {
		started <- struct{}{}
		<-gate
		return nil, nil
	})
	if err != nil {
		t.Fatalf("initial TrySubmit: %v", err)
	}
	<-started // all capacity is held

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the (blocking) Acquire

	if _, err := s.Submit(ctx, 4, func() (any, error) { return nil, nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Submit with cancelled ctx: err = %v, want context.Canceled", err)
	}

	close(gate)
	<-d
}

func Example() {
	s := New(4)
	defer s.Stop()

	d, err := s.TrySubmit(2, func() (any, error) { return "ok", nil })
	fmt.Println(err)
	fmt.Println((<-d).Value)
	// Output:
	// <nil>
	// ok
}
```

## Review

Weighted admission is correct when the budget arithmetic holds and every `Acquire`
is paired with an exactly-matching `Release`. The paired release lives in a
`defer` on the run goroutine, so it survives a panicking task; the final
blocking weight-10 `Submit` in the test only succeeds if both prior releases
happened, which is the proof capacity was fully restored. `Acquire` honoring the
context is what turns "the budget is full" into a prompt `context.Canceled` rather
than a deadlock. Rejecting an over-large weight up front avoids an `Acquire` that
could never complete. The failure to watch for is a mismatched `Release` — release
a different weight than acquired and the effective capacity shrinks permanently.
Run `go test -race -count=1 ./...` and `go vet ./...`.

## Resources

- [`golang.org/x/sync/semaphore`](https://pkg.go.dev/golang.org/x/sync/semaphore) — `NewWeighted`, `Acquire`, `TryAcquire`, `Release`.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — bounding concurrency and cancelling blocked work.
- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) — the cancellation `Acquire` observes.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-panic-recovery-and-retry-backoff.md](07-panic-recovery-and-retry-backoff.md) | Next: [09-scheduler-metrics-observability.md](09-scheduler-metrics-observability.md)
