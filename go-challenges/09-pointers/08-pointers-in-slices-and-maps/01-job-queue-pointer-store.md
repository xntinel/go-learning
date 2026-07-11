# Exercise 1: The Job Queue — map[string]*Job for O(1) Mutable Lookup

An in-memory job queue is the canonical pointer-store: a `map[string]*Job` for
O(1) lookup, a `[]string` for insertion order, and lifecycle methods that mutate
the one canonical `*Job` in place while a `Snapshot()` hands callers independent
`[]Job` copies. This module builds that queue and proves every pointer property
with a test.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
ptrcoll/                       independent module: example.com/ptrcoll
  go.mod                       go 1.24
  internal/jobs/jobs.go        Job, Queue{byID map[string]*Job, order []string}; Add/Get/Start/Complete/Snapshot
  internal/jobs/jobs_test.go   pointer-identity, in-place mutation, snapshot-copy, insertion-order tests
  cmd/demo/main.go             runnable demo: add, start, complete, snapshot
```

Files: `internal/jobs/jobs.go`, `internal/jobs/jobs_test.go`, `cmd/demo/main.go`.
Implement: a `Queue` storing `*Job` in a `map[string]*Job` plus a `[]string`
order slice; `Add`, `Get`, `Start`, `Complete` mutate in place; `Snapshot`
returns `[]Job` copies. Guarded by a `sync.RWMutex`.
Test: pointer identity from `Get`, in-place mutation visible to the original
holder, snapshot independence, insertion order.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ptrcoll/internal/jobs ~/go-exercises/ptrcoll/cmd/demo
cd ~/go-exercises/ptrcoll
go mod init example.com/ptrcoll
```

### Why pointers, and why a snapshot of values

The queue must do two things that pull in opposite directions. It must let
lifecycle transitions (`Start`, `Complete`) mutate a job in place so that every
holder of the job — the caller that added it, a worker that captured the handle —
sees the new `Status`; and it must expose the queue's contents to a reader without
letting that reader corrupt internal state. Pointers solve the first: `byID` is a
`map[string]*Job`, so `Start` looks the job up in O(1), reaches through the
pointer, and sets `Status = StatusRunning` on the one canonical object. Value
copies solve the second: `Snapshot` dereferences each `*Job` into a `[]Job`, so
the returned slice shares no memory with the queue. A caller that mutates
`snap[0].Status` mutates its own copy and the queue is untouched. The `[]string`
order slice records insertion order independently of Go's unordered map iteration,
so `Snapshot` is deterministic.

A `sync.RWMutex` guards all of it: readers (`Get`, `Snapshot`) take `RLock` and
run concurrently; writers (`Add`, `Start`, `Complete`) take the exclusive `Lock`.
The `-race` run is what proves the locking is real.

Create `internal/jobs/jobs.go`:

```go
package jobs

import (
	"errors"
	"sync"
	"time"
)

// ErrNotFound is returned when a job ID is not present in the queue.
var ErrNotFound = errors.New("job not found")

type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// Job is one unit of work. It is stored by pointer so lifecycle transitions
// mutate the one canonical instance in place.
type Job struct {
	ID        string
	Payload   string
	Status    Status
	CreatedAt time.Time
	StartedAt time.Time
	EndedAt   time.Time
}

// New builds a pending job stamped with the current time.
func New(id, payload string) *Job {
	return &Job{
		ID:        id,
		Payload:   payload,
		Status:    StatusPending,
		CreatedAt: time.Now(),
	}
}

// Queue is a concurrency-safe in-memory job store. byID gives O(1) lookup and
// mutation through the pointer; order records insertion order for Snapshot.
type Queue struct {
	mu    sync.RWMutex
	byID  map[string]*Job
	order []string
}

func NewQueue() *Queue {
	return &Queue{byID: make(map[string]*Job), order: nil}
}

// Add stores a job and appends its ID to the order slice.
func (q *Queue) Add(j *Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.byID[j.ID] = j
	q.order = append(q.order, j.ID)
}

// Get returns the stored pointer, so the caller shares the queue's instance.
func (q *Queue) Get(id string) (*Job, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	j, ok := q.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	return j, nil
}

// Start transitions a job to running, mutating it in place through the pointer.
func (q *Queue) Start(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.byID[id]
	if !ok {
		return ErrNotFound
	}
	j.Status = StatusRunning
	j.StartedAt = time.Now()
	return nil
}

// Complete transitions a job to the terminal completed state.
func (q *Queue) Complete(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.byID[id]
	if !ok {
		return ErrNotFound
	}
	j.Status = StatusCompleted
	j.EndedAt = time.Now()
	return nil
}

// Snapshot returns independent value copies in insertion order. A caller
// mutating the result cannot touch the queue's internal state.
func (q *Queue) Snapshot() []Job {
	q.mu.RLock()
	defer q.mu.RUnlock()
	out := make([]Job, 0, len(q.order))
	for _, id := range q.order {
		if j, ok := q.byID[id]; ok {
			out = append(out, *j)
		}
	}
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ptrcoll/internal/jobs"
)

func main() {
	q := jobs.NewQueue()
	q.Add(jobs.New("j1", "resize-image"))
	q.Add(jobs.New("j2", "send-email"))

	_ = q.Start("j1")
	_ = q.Complete("j1")

	for _, j := range q.Snapshot() {
		fmt.Printf("%s %s\n", j.ID, j.Status)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
j1 completed
j2 pending
```

### Tests

`TestAddAndGet` asserts `Get` returns the *same pointer* that was added — proof
the map stores the pointer, not a copy. `TestStartMutatesJobInPlace` is the core
test: it holds the original `*Job`, calls `Start` through the queue, and observes
the mutation on its own handle. `TestSnapshotReturnsCopies` mutates a snapshot
element and asserts the queue is unaffected. `TestQueuePreservesInsertionOrder`
pins the ordering contract.

Create `internal/jobs/jobs_test.go`:

```go
package jobs

import (
	"errors"
	"fmt"
	"testing"
)

func TestAddAndGet(t *testing.T) {
	t.Parallel()

	q := NewQueue()
	j := New("j1", "hello")
	q.Add(j)

	got, err := q.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if got != j {
		t.Fatal("Get should return the same pointer that was added")
	}
}

func TestGetReturnsNotFound(t *testing.T) {
	t.Parallel()

	q := NewQueue()
	if _, err := q.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStartMutatesJobInPlace(t *testing.T) {
	t.Parallel()

	q := NewQueue()
	j := New("j1", "hello")
	q.Add(j)
	if err := q.Start("j1"); err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusRunning {
		t.Fatalf("j.Status = %q, want running", j.Status)
	}
	if j.StartedAt.IsZero() {
		t.Fatal("j.StartedAt is zero")
	}
}

func TestCompleteTransitionsToCompleted(t *testing.T) {
	t.Parallel()

	q := NewQueue()
	j := New("j1", "hello")
	q.Add(j)
	_ = q.Start("j1")
	if err := q.Complete("j1"); err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusCompleted {
		t.Fatalf("j.Status = %q, want completed", j.Status)
	}
	if j.EndedAt.IsZero() {
		t.Fatal("j.EndedAt is zero")
	}
}

func TestSnapshotReturnsCopies(t *testing.T) {
	t.Parallel()

	q := NewQueue()
	q.Add(New("j1", "hello"))
	q.Add(New("j2", "world"))

	snap := q.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("len = %d, want 2", len(snap))
	}
	if snap[0].ID != "j1" || snap[1].ID != "j2" {
		t.Fatalf("order = %+v", snap)
	}
	snap[0].Status = StatusFailed
	got, _ := q.Get("j1")
	if got.Status == StatusFailed {
		t.Fatal("mutating snapshot should not affect queue")
	}
}

func TestQueuePreservesInsertionOrder(t *testing.T) {
	t.Parallel()

	q := NewQueue()
	q.Add(New("j1", "a"))
	q.Add(New("j2", "b"))
	q.Add(New("j3", "c"))

	snap := q.Snapshot()
	want := []string{"j1", "j2", "j3"}
	if len(snap) != len(want) {
		t.Fatalf("len = %d, want %d", len(snap), len(want))
	}
	for i, id := range want {
		if snap[i].ID != id {
			t.Fatalf("snap[%d].ID = %q, want %q", i, snap[i].ID, id)
		}
	}
}

func ExampleQueue_Snapshot() {
	q := NewQueue()
	q.Add(New("j1", "resize"))
	q.Add(New("j2", "email"))
	_ = q.Start("j1")

	for _, j := range q.Snapshot() {
		fmt.Println(j.ID, j.Status)
	}
	// Output:
	// j1 running
	// j2 pending
}
```

## Review

The queue is correct when three properties hold together. Identity: `Get` returns
the stored pointer, so a caller and the queue share one `Job` — `TestAddAndGet`
proves it with pointer equality. In-place mutation: `Start` and `Complete` reach
through that shared pointer, so the change is visible on the original handle
without any re-fetch — `TestStartMutatesJobInPlace` is the executable proof.
Isolation: `Snapshot` dereferences into values, so the returned `[]Job` is a
severed copy — `TestSnapshotReturnsCopies` mutates it and confirms the queue is
untouched. The trap this exercise closes off is returning `[]*Job` from a read
method; that would let a reader mutate internal jobs through the pointers and break
the lifecycle invariants, which is why `Snapshot` returns values. Run
`go test -race` to confirm the `RWMutex` actually serializes the writers.

## Resources

- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — when a method needs a pointer receiver to mutate.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read/write lock guarding the queue.
- [Go Blog: The race detector](https://go.dev/blog/race-detector) — what `-race` proves about the locking.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-shallow-clone-snapshot-leak.md](02-shallow-clone-snapshot-leak.md)
