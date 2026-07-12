# Exercise 16: Async Queue Verifies Backpressure Guard

An async job queue that silently drops a job under load is worse than one
that rejects it outright — a caller that thinks its job is queued when it was
actually dropped has no way to notice until something downstream never runs.
This exercise builds a bounded queue whose `Enqueue` reports success or
backpressure through a named `ok bool`, and adds a deferred invariant check
that turns a future "reported success but did not actually append" bug into
a loud panic instead of a silent overflow.

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

## What you'll build

```text
jobqueue/                    independent module: example.com/jobqueue
  go.mod
  jobqueue.go                 Job; Queue; Enqueue (named ok, deferred invariant guard)
  cmd/demo/
    main.go                   runnable demo: fill a queue past capacity
  jobqueue_test.go             capacity table, concurrent enqueue race safety
```

- Files: `jobqueue.go`, `cmd/demo/main.go`, `jobqueue_test.go`.
- Implement: `(*Queue) Enqueue(j Job) (ok bool)` that rejects work once the queue is at capacity, and a deferred closure that panics if `ok` is true but the item was not actually appended.
- Test: a capacity table (under, at, over, zero capacity), plus a concurrent `Enqueue` test under `-race` asserting exactly `capacity` jobs are accepted.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/16-queue-backpressure-overflow-guard/cmd/demo
cd go-solutions/04-functions/02-named-return-values/16-queue-backpressure-overflow-guard
go mod edit -go=1.24
```

### A named return that polices its own promise

`Enqueue`'s named result `ok` is the caller-facing contract: true means the
job is in the queue, false means backpressure and the caller must retry, shed
load, or escalate. That contract is simple enough that a bug could
theoretically set `ok = true` without the append actually happening — a typo,
a refactor that reorders the append past the assignment, a future code path
that forgets to append at all. Because `ok` is named, a deferred closure can
read the exact value about to be returned and cross-check it against reality
(did the queue's length actually grow by one?) before the caller ever sees
it:

```go
before := len(q.items)
defer func() {
    if ok && len(q.items) != before+1 {
        panic("jobqueue: Enqueue reported success but item was not appended")
    }
}()
```

This is not defensive paranoia for its own sake — it is the difference
between a bug that silently drops jobs under load (discovered weeks later as
a customer complaint) and one that panics immediately in a test or in
staging, with a stack trace pointing at the exact function that broke the
promise.

Create `jobqueue.go`:

```go
package jobqueue

import "sync"

// Job is one unit of work accepted by the queue.
type Job struct {
	ID int
}

// Queue is a bounded async job queue. Capacity enforces backpressure: once
// full, Enqueue rejects new work instead of growing without bound.
type Queue struct {
	mu       sync.Mutex
	items    []Job
	Capacity int
}

// NewQueue returns a queue that rejects work once it holds capacity items.
func NewQueue(capacity int) *Queue {
	return &Queue{Capacity: capacity}
}

// Len reports how many jobs are currently queued.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Enqueue attempts to add j to the queue. ok reports whether the job was
// accepted; false means the queue is at capacity and the caller must apply
// backpressure (retry later, shed the job, and so on).
//
// The named result ok is not just documentation here: a deferred closure
// re-checks, after the append, that the queue actually grew by one whenever
// ok is true. That invariant check is a safety net against a future bug that
// reports success without truly appending the job — a silent overflow drop
// that would otherwise be invisible to the caller. Because ok is named, the
// defer can read the exact value the function is about to return and panic
// loudly rather than let the mismatch pass silently.
func (q *Queue) Enqueue(j Job) (ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) >= q.Capacity {
		return false // backpressure: queue is full, reject
	}

	before := len(q.items)
	defer func() {
		if ok && len(q.items) != before+1 {
			panic("jobqueue: Enqueue reported success but item was not appended (silent overflow drop)")
		}
	}()

	q.items = append(q.items, j)
	ok = true
	return
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/jobqueue"
)

func main() {
	q := jobqueue.NewQueue(3)

	for i := 1; i <= 5; i++ {
		ok := q.Enqueue(jobqueue.Job{ID: i})
		fmt.Printf("enqueue job %d: ok=%v queueLen=%d\n", i, ok, q.Len())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
enqueue job 1: ok=true queueLen=1
enqueue job 2: ok=true queueLen=2
enqueue job 3: ok=true queueLen=3
enqueue job 4: ok=false queueLen=3
enqueue job 5: ok=false queueLen=3
```

### Tests

The table covers under, at, and over capacity, plus the zero-capacity edge
case where every enqueue is rejected. The concurrent test hammers `Enqueue`
from 50 goroutines against a 10-slot queue under `-race` and asserts exactly
10 acceptances land — proof the lock genuinely serializes the
check-then-append, not just that it compiles under `-race`.

Create `jobqueue_test.go`:

```go
package jobqueue

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestEnqueueRespectsCapacity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		capacity int
		attempts int
		wantOK   int
	}{
		{name: "under capacity", capacity: 5, attempts: 3, wantOK: 3},
		{name: "exactly at capacity", capacity: 3, attempts: 3, wantOK: 3},
		{name: "over capacity rejects extras", capacity: 3, attempts: 5, wantOK: 3},
		{name: "zero capacity rejects everything", capacity: 0, attempts: 2, wantOK: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			q := NewQueue(tt.capacity)
			accepted := 0
			for i := 0; i < tt.attempts; i++ {
				if q.Enqueue(Job{ID: i}) {
					accepted++
				}
			}
			if accepted != tt.wantOK {
				t.Fatalf("accepted = %d, want %d", accepted, tt.wantOK)
			}
			if q.Len() != tt.wantOK {
				t.Fatalf("Len() = %d, want %d", q.Len(), tt.wantOK)
			}
		})
	}
}

func TestEnqueueConcurrentRespectsCapacity(t *testing.T) {
	t.Parallel()

	q := NewQueue(10)
	var wg sync.WaitGroup
	var accepted int64

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if q.Enqueue(Job{ID: id}) {
				atomic.AddInt64(&accepted, 1)
			}
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt64(&accepted); got != 10 {
		t.Fatalf("accepted = %d, want 10 (queue capacity)", got)
	}
	if q.Len() != 10 {
		t.Fatalf("Len() = %d, want 10", q.Len())
	}
}
```

## Review

`Enqueue` is correct when it accepts exactly up to capacity and rejects
everything past it, under both sequential and concurrent load. The named
result `ok` earns its place twice over: it is the caller-facing contract, and
it is the value a deferred invariant check can inspect to catch a future bug
that would otherwise report success while silently dropping the job. The
mistake to avoid is holding the lock across the append but computing `before`
or the invariant check outside it — the whole point is that the
check-then-act (capacity check, append, invariant verification) happens as
one atomic section, which is also why the concurrent test must run under
`-race`.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex)
- [Effective Go: Named result parameters](https://go.dev/doc/effective_go#named-results)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-resumable-scanner-position-cursor.md](15-resumable-scanner-position-cursor.md) | Next: [17-distributed-lock-ttl-extension.md](17-distributed-lock-ttl-extension.md)
