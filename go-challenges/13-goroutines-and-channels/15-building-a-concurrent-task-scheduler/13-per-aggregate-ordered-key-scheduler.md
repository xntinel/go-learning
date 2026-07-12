# Exercise 13: Keyed Serial Execution: Per-Key Ordering, Cross-Key Parallelism

**Level: Advanced**

An event processor must apply events for the same aggregate — an account, an order
— in the order they were submitted, because a debit followed by a credit for
account X cannot be reordered without corrupting the balance. Yet distinct accounts
have no such dependency and must run concurrently for throughput, which is exactly
the guarantee a Kafka consumer derives from partition-by-key. This module builds a
scheduler that routes each task to a per-key serial lane: lanes run in parallel but
each lane processes its key strictly in order, lanes are spawned on demand and
retired when a key goes idle, and the retire-versus-`Submit` handoff is the race
that must be airtight.

This module is self-contained: its own module, a `keyed` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
keyed/                       independent module: example.com/keyed
  go.mod                     go 1.26
  keyed.go                   Scheduler: per-key serial lanes, on-demand spawn/retire, maxLanes bound
  cmd/demo/main.go           runnable demo: ordered ops per aggregate, then final stats
  keyed_test.go              per-key ordering, cross-key parallelism, lane reuse, maxLanes bound, shutdown, goleak
```

- Files: `keyed.go`, `cmd/demo/main.go`, `keyed_test.go`.
- Implement: `New(maxLanes int) *Scheduler`, `Submit(ctx, key, fn) (<-chan Result, error)`, `Shutdown(ctx) error`, `Stats() Stats`, with `Task func(ctx) (any, error)` and `Result{Value, Err}`.
- Test: per-key execution order equals submission order with an in-flight counter that never exceeds 1; K distinct keys run concurrently (would deadlock if serialized); a retired lane re-created by a later `Submit` still runs in order under stress; `maxLanes` bounds concurrent distinct keys; `Shutdown` drains and is idempotent; goleak confirms every lane goroutine is reclaimed.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go mod tidy
```

### One lane per key, owned by one goroutine, with an airtight retire

The correctness of a keyed scheduler rests on a single structural invariant: at any
instant, at most one goroutine is associated with a given key, and that goroutine
processes the key's tasks one at a time in FIFO order. Serialization is therefore
not enforced by a lock around the task body — it is a consequence of single
ownership. A lane is a per-key mailbox (a slice of pending jobs guarded by the
scheduler mutex) drained by exactly one goroutine. Because that goroutine pops and
runs jobs sequentially, a per-key in-flight count can never exceed one, and the
execution order is exactly the order jobs were appended.

The subtle part is the lifecycle. Keeping a goroutine alive forever for every key
ever seen is a leak in any system with unbounded key cardinality, so a lane must
retire when its key drains. That introduces the one genuine race in the design: a
lane deciding "my queue is empty, I will exit" concurrently with a `Submit`
appending a fresh job for the same key. If those two steps interleave wrong, the
task is enqueued into a lane that is about to vanish and is never run — a silent
lost event. The fix is to make the retire decision and every enqueue share the same
mutex, so they are strictly ordered:

1. `Submit` locks the mutex, and if the key already has a lane, appends the job and
   returns. The append and the lane's own empty-check are mutually exclusive.
2. When a lane finishes a job it re-locks the mutex and inspects its own queue. If
   it is empty it deletes itself from the map, unlocks, releases its admission slot,
   and returns. If it is non-empty it pops the next job and runs it.
3. Because both the append (step 1) and the empty-check (step 2) hold the mutex,
   exactly one wins. Either the append lands first and the lane sees a non-empty
   queue and keeps running, or the retire lands first and removes the lane so the
   next `Submit` finds no lane and re-creates one. There is no third outcome where a
   job is stranded.

Admission is the second concern. `maxLanes` bounds the number of *concurrently
active distinct keys*, which is a memory-and-fan-out budget, not a worker count. A
counting semaphore (`golang.org/x/sync/semaphore.Weighted`) holds one weight per
active key: a new key acquires a weight before its lane is created, and the lane
releases it on retire. `Weighted.Acquire` honors the caller's context, so a `Submit`
for a new key past the bound blocks only until a lane retires or the caller's
deadline fires — which is how load sheds at the edge instead of piling up. A key
that already has a live lane needs no new admission; only the *first* task for a key
pays the semaphore, and only its lane's retirement returns the weight.

Create `keyed.go`:

```go
package keyed

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
)

// ErrShuttingDown is returned by Submit after Shutdown has begun.
var ErrShuttingDown = errors.New("keyed: scheduler shutting down")

// Task is the unit of work. It must cooperate with ctx for cancellation.
type Task func(ctx context.Context) (any, error)

// Result carries a task's outcome to its per-submission result channel.
type Result struct {
	Value any
	Err   error
}

// Stats is a coherent snapshot of the scheduler.
type Stats struct {
	ActiveLanes int
	Completed   int64
}

type job struct {
	ctx  context.Context
	fn   Task
	done chan Result
}

// lane is a per-key FIFO mailbox. Its queue is guarded by Scheduler.mu; a single
// goroutine drains it, which is what makes execution for one key strictly serial.
type lane struct {
	queue []*job
}

// Scheduler routes each task to a per-key serial lane. Lanes run in parallel but
// each lane processes its key strictly in submission order. Lanes are spawned on
// demand and retired when their key goes idle; maxLanes bounds the number of
// concurrently active distinct keys.
type Scheduler struct {
	sem *semaphore.Weighted // admission budget: one weight per active distinct key

	mu     sync.Mutex
	closed bool
	lanes  map[string]*lane

	wg        sync.WaitGroup
	active    atomic.Int64
	completed atomic.Int64
}

// New returns a Scheduler that admits at most maxLanes concurrently active
// distinct keys.
func New(maxLanes int) *Scheduler {
	if maxLanes < 1 {
		maxLanes = 1
	}
	return &Scheduler{
		sem:   semaphore.NewWeighted(int64(maxLanes)),
		lanes: make(map[string]*lane),
	}
}

// Submit routes fn to key's serial lane, creating the lane on demand. Admission of
// a new key is bounded by maxLanes and honors ctx: if the budget is full, Submit
// blocks until a lane retires or ctx is done. It returns a capacity-1 result
// channel, or ErrShuttingDown once Shutdown has begun.
func (s *Scheduler) Submit(ctx context.Context, key string, fn Task) (<-chan Result, error) {
	done := make(chan Result, 1)
	j := &job{ctx: ctx, fn: fn, done: done}

	// Fast path: the key already has a live lane, so no new admission is needed.
	// Appending under mu is the exact point that races lane retirement; see runLane.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrShuttingDown
	}
	if ln, ok := s.lanes[key]; ok {
		ln.queue = append(ln.queue, j)
		s.mu.Unlock()
		return done, nil
	}
	s.mu.Unlock()

	// New key: admit against the maxLanes budget. Acquire honors ctx.
	if err := s.sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.sem.Release(1)
		return nil, ErrShuttingDown
	}
	// Double-check: another Submit for the same key may have created the lane while
	// we were blocked in Acquire. If so, hand off our weight and reuse the lane.
	if ln, ok := s.lanes[key]; ok {
		ln.queue = append(ln.queue, j)
		s.mu.Unlock()
		s.sem.Release(1)
		return done, nil
	}
	ln := &lane{queue: []*job{j}}
	s.lanes[key] = ln
	s.active.Add(1)
	s.wg.Add(1)
	go s.runLane(key)
	s.mu.Unlock()
	return done, nil
}

// runLane drains one key's mailbox strictly in order, then retires. The retire
// decision and every enqueue share s.mu, so a task appended by a concurrent Submit
// can never be stranded in a lane that has already decided to exit: either the
// append wins the lock and the loop sees a non-empty queue, or the retire wins and
// removes the lane so the next Submit re-creates it.
func (s *Scheduler) runLane(key string) {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		ln := s.lanes[key]
		if len(ln.queue) == 0 {
			delete(s.lanes, key)
			s.mu.Unlock()
			s.active.Add(-1)
			s.sem.Release(1)
			return
		}
		j := ln.queue[0]
		ln.queue = ln.queue[1:]
		s.mu.Unlock()

		s.runJob(j)
		s.completed.Add(1)
	}
}

func (s *Scheduler) runJob(j *job) {
	var res Result
	func() {
		defer func() {
			if r := recover(); r != nil {
				res = Result{Err: fmt.Errorf("keyed: task panicked: %v", r)}
			}
		}()
		v, err := j.fn(j.ctx)
		res = Result{Value: v, Err: err}
	}()
	j.done <- res // done is buffered 1, so an abandoned caller never wedges the lane
}

// Shutdown stops admitting new work and waits for every lane to drain, bounded by
// ctx. It is idempotent: a second call after a completed drain returns nil.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	drained := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats returns a coherent snapshot: the number of active lanes and the total
// completed task count.
func (s *Scheduler) Stats() Stats {
	return Stats{
		ActiveLanes: int(s.active.Load()),
		Completed:   s.completed.Load(),
	}
}
```

### The runnable demo

The demo submits an ordered sequence of operations for two aggregates and reads
each key's results back in submission order. Output is deterministic: keys and
results are iterated in a fixed slice order, never from a map.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/keyed"
)

// The demo submits an ordered sequence of operations for two aggregates and reads
// each key's results back in submission order. Output is deterministic: keys and
// results are read in a fixed order, never from a map iteration.
func main() {
	s := keyed.New(3)
	ctx := context.Background()

	keys := []string{"acct-A", "acct-B"}
	ops := map[string][]string{
		"acct-A": {"debit-0", "debit-1", "debit-2"},
		"acct-B": {"credit-0", "credit-1", "credit-2"},
	}

	results := make(map[string][]string)
	for _, k := range keys {
		var chans []<-chan keyed.Result
		for _, op := range ops[k] {
			op := op
			ch, err := s.Submit(ctx, k, func(context.Context) (any, error) {
				return op, nil
			})
			if err != nil {
				panic(err)
			}
			chans = append(chans, ch)
		}
		for _, ch := range chans {
			r := <-ch
			results[k] = append(results[k], r.Value.(string))
		}
	}

	for _, k := range keys {
		fmt.Printf("%s executed ops in order: %v\n", k, results[k])
	}

	if err := s.Shutdown(ctx); err != nil {
		panic(err)
	}
	st := s.Stats()
	fmt.Printf("completed=%d active_lanes_after_shutdown=%d\n", st.Completed, st.ActiveLanes)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acct-A executed ops in order: [debit-0 debit-1 debit-2]
acct-B executed ops in order: [credit-0 credit-1 credit-2]
completed=6 active_lanes_after_shutdown=0
```

### Tests

`TestPerKeyOrdering` submits 200 tasks for one key, recording each task's index and
tracking a per-key in-flight counter; it asserts the recorded order equals the
submission order and the counter never exceeds 1 (strict serialization).
`TestCrossKeyParallelism` submits K distinct keys whose tasks all block on a barrier
that only releases once all K arrive, so the run completes only if the lanes truly
run concurrently — it would deadlock if lanes were serialized.
`TestLaneLifecycleReuse` runs 300 rounds of submit-drain-resubmit on one key to
stress the retire/re-create handoff under `-race`. `TestMaxLanesBoundBlocks` pins
that a third distinct key past a `maxLanes=2` bound fails with the caller's context
deadline; `TestMaxLanesAdmitsAfterRetire` proves the bound is not a leak. 
`TestShutdownDrainsAndIdempotent` checks drain, idempotency, and post-shutdown
refusal. `TestMain` wraps the suite in `goleak.VerifyTestMain`.

Create `keyed_test.go`:

```go
package keyed

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// goleak asserts that no lane goroutine (or Shutdown waiter) outlives the tests.
	goleak.VerifyTestMain(m)
}

// TestPerKeyOrdering submits many tasks for one key and proves two things: the
// execution order equals the submission order, and no two tasks for the key ever
// run concurrently (a per-key in-flight counter never exceeds 1).
func TestPerKeyOrdering(t *testing.T) {
	t.Parallel()

	s := New(4)
	ctx := context.Background()

	const n = 200
	var (
		mu       sync.Mutex
		order    []int
		inFlight atomic.Int32
		maxSeen  atomic.Int32
	)

	var chans []<-chan Result
	for i := range n {
		ch, err := s.Submit(ctx, "acct-1", func(context.Context) (any, error) {
			if cur := inFlight.Add(1); cur > maxSeen.Load() {
				maxSeen.Store(cur)
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			inFlight.Add(-1)
			return i, nil
		})
		if err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
		chans = append(chans, ch)
	}
	for _, ch := range chans {
		<-ch
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if m := maxSeen.Load(); m > 1 {
		t.Fatalf("per-key in-flight peaked at %d, want <= 1 (lane not serial)", m)
	}
	if len(order) != n {
		t.Fatalf("recorded %d executions, want %d", len(order), n)
	}
	for i := range n {
		if order[i] != i {
			t.Fatalf("execution order[%d] = %d, want %d (out of submission order)", i, order[i], i)
		}
	}
}

// TestCrossKeyParallelism proves distinct keys run concurrently. Each of K tasks
// blocks on a barrier that only releases once all K have arrived; if lanes were
// serialized the run would deadlock because task 1 would wait forever for a task 2
// that never starts.
func TestCrossKeyParallelism(t *testing.T) {
	t.Parallel()

	const k = 8
	s := New(k)
	ctx := context.Background()

	var arrived atomic.Int32
	release := make(chan struct{})

	var chans []<-chan Result
	for i := range k {
		ch, err := s.Submit(ctx, fmt.Sprintf("key-%d", i), func(context.Context) (any, error) {
			if arrived.Add(1) == k {
				close(release) // the last arrival frees all K lanes
			}
			<-release
			return nil, nil
		})
		if err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
		chans = append(chans, ch)
	}

	deadline := time.After(5 * time.Second)
	for _, ch := range chans {
		select {
		case <-ch:
		case <-deadline:
			t.Fatal("timeout: lanes did not run concurrently across keys")
		}
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// TestLaneLifecycleReuse stresses the retire/re-create handoff: each round submits
// to the same key, drains it (letting the lane retire), then submits again. Under
// -race this surfaces any window where a retiring lane and a re-creating Submit
// touch shared state unsafely. Ordering must hold within every round.
func TestLaneLifecycleReuse(t *testing.T) {
	t.Parallel()

	s := New(4)
	ctx := context.Background()
	const rounds = 300

	for r := range rounds {
		var (
			mu    sync.Mutex
			order []int
		)
		var chans []<-chan Result
		for i := range 3 {
			ch, err := s.Submit(ctx, "reused", func(context.Context) (any, error) {
				mu.Lock()
				order = append(order, i)
				mu.Unlock()
				return i, nil
			})
			if err != nil {
				t.Fatalf("round %d submit %d: %v", r, i, err)
			}
			chans = append(chans, ch)
		}
		for _, ch := range chans {
			<-ch
		}
		if len(order) != 3 || order[0] != 0 || order[1] != 1 || order[2] != 2 {
			t.Fatalf("round %d order = %v, want [0 1 2]", r, order)
		}
	}

	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// TestMaxLanesBoundBlocks pins the admission bound: with maxLanes=2 and two lanes
// occupied by gated tasks, a Submit for a third distinct key must fail with the
// caller's context deadline rather than admit a third concurrent key.
func TestMaxLanesBoundBlocks(t *testing.T) {
	t.Parallel()

	s := New(2)
	gate := make(chan struct{})

	block := func(context.Context) (any, error) {
		<-gate
		return nil, nil
	}
	c1, err := s.Submit(context.Background(), "k1", block)
	if err != nil {
		t.Fatalf("Submit k1: %v", err)
	}
	c2, err := s.Submit(context.Background(), "k2", block)
	if err != nil {
		t.Fatalf("Submit k2: %v", err)
	}

	// Both lanes now hold the two admission slots. A third distinct key is refused
	// once the caller's short deadline elapses.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.Submit(ctx, "k3", block); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Submit k3 over-bound err = %v, want context.DeadlineExceeded", err)
	}

	close(gate)
	<-c1
	<-c2
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// TestMaxLanesAdmitsAfterRetire proves the bound is not a leak: once an occupying
// lane retires it frees its slot, so a previously-blocked new key is admitted.
func TestMaxLanesAdmitsAfterRetire(t *testing.T) {
	t.Parallel()

	s := New(1)
	gate := make(chan struct{})

	c1, err := s.Submit(context.Background(), "first", func(context.Context) (any, error) {
		<-gate
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Submit first: %v", err)
	}

	// Second distinct key blocks until the first lane retires.
	admitted := make(chan error, 1)
	go func() {
		_, err := s.Submit(context.Background(), "second", func(context.Context) (any, error) {
			return nil, nil
		})
		admitted <- err
	}()

	select {
	case err := <-admitted:
		t.Fatalf("second key admitted before slot freed: err=%v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(gate) // first lane drains and retires, freeing the single slot
	<-c1
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatalf("second key admission err = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second key never admitted after slot freed")
	}

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// TestShutdownDrainsAndIdempotent asserts Shutdown drains every lane, that a second
// Shutdown is a no-op, and that Submit after Shutdown is refused.
func TestShutdownDrainsAndIdempotent(t *testing.T) {
	t.Parallel()

	s := New(4)
	ctx := context.Background()

	const n = 40
	var chans []<-chan Result
	for i := range n {
		ch, err := s.Submit(ctx, fmt.Sprintf("k-%d", i%5), func(context.Context) (any, error) {
			return i, nil
		})
		if err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
		chans = append(chans, ch)
	}
	for _, ch := range chans {
		<-ch
	}

	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown not idempotent: %v", err)
	}

	if st := s.Stats(); st.Completed != n || st.ActiveLanes != 0 {
		t.Fatalf("after drain Stats = %+v, want Completed=%d ActiveLanes=0", st, n)
	}
	if _, err := s.Submit(ctx, "late", func(context.Context) (any, error) { return nil, nil }); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Submit after Shutdown err = %v, want ErrShuttingDown", err)
	}
}
```

## Review

Correct here means two invariants hold simultaneously: within a key, tasks execute
in submission order and never overlap; across keys, lanes make progress in parallel.
Both fall out of single ownership — one goroutine per active key draining a FIFO
mailbox — rather than from locking the task body, which is why the per-key in-flight
counter in `TestPerKeyOrdering` never exceeds 1 while the K-way barrier in
`TestCrossKeyParallelism` still releases. The one real hazard, a lane retiring in
the same instant a `Submit` enqueues for its key, is closed by making the retire's
empty-check and every enqueue share the scheduler mutex, so exactly one wins and no
job is ever stranded; `TestLaneLifecycleReuse` hammers that window 300 times under
`-race`, and goleak proves every lane goroutine and Shutdown waiter is reclaimed.
The `maxLanes` semaphore bounds concurrent distinct keys and honors the caller's
context, so admission sheds at the edge instead of piling up. The production bug this
prevents is the silent lost event: a naive keyed dispatcher that races lane teardown
against submission drops the task that arrives during retirement, and a dropped debit
is a corrupted balance no metric will explain. Run `go test -count=1 -race ./...`.

## Resources

- [`golang.org/x/sync/semaphore`](https://pkg.go.dev/golang.org/x/sync/semaphore) — context-aware weighted admission, used here to bound concurrently active keys.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — joining every lane goroutine so Shutdown can drain deterministically.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — asserting no lane goroutine survives Shutdown, the leak this design must avoid.
- [Go Memory Model](https://go.dev/ref/mem) — why the shared mutex over enqueue and retire establishes the happens-before that makes the handoff race-free.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-webhook-fair-share-tenant-dispatch.md](12-webhook-fair-share-tenant-dispatch.md) | Next: [../16-goroutine-debugging-under-load/00-concepts.md](../16-goroutine-debugging-under-load/00-concepts.md)
