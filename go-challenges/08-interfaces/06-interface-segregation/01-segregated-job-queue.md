# Exercise 1: A Job Queue With Segregated Producer, Consumer, and Inspector Ports

An in-memory job queue is the smallest realistic place to see the Interface
Segregation Principle pay off: the same concrete `*Queue` is used by three
unrelated consumers — one that only submits, one that only takes, one that only
inspects — and each depends on a one-method interface that names exactly its own
call. This module is fully self-contained: its own module, its own demo, its own
tests.

## What you'll build

```text
jobqueue/                      independent module: example.com/jobqueue
  go.mod                       go 1.24
  queue.go                     Producer, Consumer, Inspector (one method each); *Queue satisfies all three
  cmd/
    demo/
      main.go                  binds one *Queue to three interface vars and drives each
  queue_test.go                submit/take, empty + nil-work rejection, stats, all-three-interfaces
```

Files: `queue.go`, `cmd/demo/main.go`, `queue_test.go`.
Implement: a `*Queue` under a `sync.Mutex` with `Submit`, `Take`, `Stats`, plus three one-method interfaces `Producer`, `Consumer`, `Inspector`.
Test: submit/take round-trip, `ErrEmpty` on empty take, nil-work rejection, stats accounting, and that one `*Queue` binds to all three interface variables independently.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/06-interface-segregation/01-segregated-job-queue/cmd/demo
cd go-solutions/08-interfaces/06-interface-segregation/01-segregated-job-queue
go mod edit -go=1.24
```

### Why three interfaces for one type

A naive job queue would export one `JobQueue` interface with `Submit`, `Take`,
and `Stats` and hand it to everyone. That is a fat interface in miniature: the
ingestion path that only submits now depends on `Take` and `Stats`, the worker
that only takes now depends on `Submit`, and a test double for either must stub
all three. The segregated design declares three interfaces, each holding exactly
one method, and lets the single `*Queue` satisfy all three structurally. The
producer side depends on `Producer`; the worker side depends on `Consumer`; a
metrics endpoint depends on `Inspector`. None of them can reach a method it does
not name, and none of them recompiles when an unrelated method changes.

The interfaces are declared here in one file only because this is a teaching
module; in a real service each would live in the package of the consumer that
declares it. What matters is that they are *unrelated* — `Producer` and
`Consumer` do not embed a common base, do not share a type, and were not designed
together. They independently happen to be satisfied by the same concrete type.

`Submit` rejects a job whose `Work` is nil, because a queued job with no work is
a latent nil-call panic at execution time; failing fast at submission turns a
runtime crash into a caller-visible error. `Take` returns a sentinel `ErrEmpty`
so a worker loop can distinguish "nothing to do right now" (poll again) from a
real fault, matched with `errors.Is` rather than string comparison.

Create `queue.go`:

```go
package jobs

import (
	"errors"
	"sync"
)

// ErrEmpty is returned by Take when the queue holds no jobs.
var ErrEmpty = errors.New("queue is empty")

// ErrNilWork is returned by Submit when a job carries no work function.
var ErrNilWork = errors.New("job work is nil")

// Job is a unit of deferred work.
type Job struct {
	ID   string
	Work func() error
}

// Stats is a point-in-time snapshot of queue counters.
type Stats struct {
	Pending int
	Total   int
}

// Producer is the write side: submit a job. Declared by producers only.
type Producer interface {
	Submit(j Job) error
}

// Consumer is the read side: take the next job. Declared by workers only.
type Consumer interface {
	Take() (Job, error)
}

// Inspector is the metrics side: read counters. Declared by observers only.
type Inspector interface {
	Stats() Stats
}

// Queue is a concurrency-safe FIFO job queue. One concrete type that
// satisfies Producer, Consumer, and Inspector at once.
type Queue struct {
	mu    sync.Mutex
	jobs  []Job
	total int
}

// NewQueue returns an empty *Queue.
func NewQueue() *Queue {
	return &Queue{}
}

// Submit appends a job. It rejects a job with no Work.
func (q *Queue) Submit(j Job) error {
	if j.Work == nil {
		return ErrNilWork
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, j)
	q.total++
	return nil
}

// Take removes and returns the oldest job, or ErrEmpty if none.
func (q *Queue) Take() (Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.jobs) == 0 {
		return Job{}, ErrEmpty
	}
	j := q.jobs[0]
	q.jobs = q.jobs[1:]
	return j, nil
}

// Stats reports pending (still queued) and total (ever submitted) counts.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Stats{Pending: len(q.jobs), Total: q.total}
}
```

### The runnable demo

The demo binds one `*Queue` to three separate interface variables to make the
segregation visible: `prod` can only submit, `cons` can only take, `insp` can
only inspect. Nothing in `main` can call `Take` through `prod`, because
`Producer` has no such method.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/jobqueue"
)

func main() {
	q := jobs.NewQueue()

	var prod jobs.Producer = q
	var cons jobs.Consumer = q
	var insp jobs.Inspector = q

	_ = prod.Submit(jobs.Job{ID: "email-42", Work: func() error { return nil }})
	_ = prod.Submit(jobs.Job{ID: "email-43", Work: func() error { return nil }})

	j, _ := cons.Take()
	fmt.Printf("took: %s\n", j.ID)

	s := insp.Stats()
	fmt.Printf("pending=%d total=%d\n", s.Pending, s.Total)
}
```

Note the import path is the module path `example.com/jobqueue`, but the package
name is `jobs`, so the demo refers to `jobs.NewQueue`.

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
took: email-42
pending=1 total=2
```

### Tests

The tests preserve the original suite and add
`TestProducerCanBeUsedWithoutConsumer`, which pins the "consumer defines the
interface" contract: a `Producer` variable can submit forever without the code
ever naming `Consumer`. `TestQueueSatisfiesAllThreeInterfaces` is the core ISP
assertion — one `*Queue` assigned to three interface variables, each exercised
independently.

Create `queue_test.go`:

```go
package jobs

import (
	"errors"
	"testing"
)

func TestQueueSubmitAndTake(t *testing.T) {
	t.Parallel()

	q := NewQueue()
	if err := q.Submit(Job{ID: "j1", Work: func() error { return nil }}); err != nil {
		t.Fatal(err)
	}

	j, err := q.Take()
	if err != nil {
		t.Fatal(err)
	}
	if j.ID != "j1" {
		t.Fatalf("ID = %q, want j1", j.ID)
	}
}

func TestQueueTakeRejectsEmpty(t *testing.T) {
	t.Parallel()

	q := NewQueue()
	if _, err := q.Take(); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
}

func TestQueueSubmitRejectsNilWork(t *testing.T) {
	t.Parallel()

	q := NewQueue()
	if err := q.Submit(Job{}); !errors.Is(err, ErrNilWork) {
		t.Fatalf("err = %v, want ErrNilWork", err)
	}
}

func TestQueueStatsReportPendingAndTotal(t *testing.T) {
	t.Parallel()

	q := NewQueue()
	_ = q.Submit(Job{ID: "j1", Work: func() error { return nil }})
	_ = q.Submit(Job{ID: "j2", Work: func() error { return nil }})
	_, _ = q.Take()

	s := q.Stats()
	if s.Pending != 1 {
		t.Fatalf("Pending = %d, want 1", s.Pending)
	}
	if s.Total != 2 {
		t.Fatalf("Total = %d, want 2", s.Total)
	}
}

func TestQueueSatisfiesAllThreeInterfaces(t *testing.T) {
	t.Parallel()

	q := NewQueue()
	var p Producer = q
	var c Consumer = q
	var i Inspector = q

	if err := p.Submit(Job{ID: "j1", Work: func() error { return nil }}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Take(); err != nil {
		t.Fatal(err)
	}
	if s := i.Stats(); s.Total != 1 {
		t.Fatalf("Total = %d, want 1", s.Total)
	}
}

func TestProducerCanBeUsedWithoutConsumer(t *testing.T) {
	t.Parallel()

	// A Producer-only consumer of the queue never names Consumer.
	var p Producer = NewQueue()
	for i := range 3 {
		if err := p.Submit(Job{ID: "batch", Work: func() error { return nil }}); err != nil {
			t.Fatalf("Submit #%d: %v", i, err)
		}
	}
	// p has no Take method; the write path cannot drain the queue.
}
```

Compile-time interface satisfaction is also worth pinning explicitly so a dropped
method fails at the type, not at a distant call site:

Append to `queue.go`:

```go
var (
	_ Producer  = (*Queue)(nil)
	_ Consumer  = (*Queue)(nil)
	_ Inspector = (*Queue)(nil)
)
```

## Review

The queue is correct when each interface variable can reach exactly its one
method and no more, and when the same `*Queue` satisfies all three without ever
declaring so. The most common mistake this design guards against is bundling the
three methods into one interface "because the queue has all three": that would
force the worker to depend on `Submit` and the producer to depend on `Take`,
inflating both blast radii for no reason. Keeping the interfaces one method each
means a future `FileQueue` that can only produce satisfies `Producer` alone. The
sentinel errors matter too — a worker loop distinguishes `ErrEmpty` (poll again)
from a real fault by `errors.Is`, never by string matching. Run `go test -race`
to confirm the mutex actually guards the slice under concurrent `Submit`/`Take`.

## Resources

- [Go Specification: Interface types](https://go.dev/ref/spec#Interface_types)
- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces)
- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces)
- [Interface Segregation Principle](https://en.wikipedia.org/wiki/Interface_segregation_principle)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-split-fat-repository.md](02-split-fat-repository.md)
