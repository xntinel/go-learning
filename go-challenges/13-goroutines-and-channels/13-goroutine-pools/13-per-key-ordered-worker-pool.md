# Exercise 13: A Bounded Pool That Serializes Jobs Per Key

**Level: Advanced**

A webhook fan-out service (equivalently a per-partition Kafka consumer) must
deliver events for the same tenant in submission order while still processing
different tenants concurrently, all under one global concurrency ceiling that
protects the delivery endpoint. A plain worker pool interleaves same-key jobs and
violates that ordering; giving each key its own goroutine plus a mutex restores
order but blows the global ceiling and leaks a goroutine per idle key. This
exercise builds the correct design: a FIFO queue per key, only each key's head
job dispatched into a shared bounded pool, the next head requeued on completion,
and the key's state reclaimed the instant its queue empties.

This module is self-contained: its own module, a `keyedpool` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
keyedpool/                   independent module: example.com/keyedpool
  go.mod                     go 1.26
  keyedpool.go               type Pool; New, Submit, Close with per-key FIFO under a global ceiling
  cmd/demo/main.go           runnable demo: three tenants, ordered per-key output, ceiling respected
  keyedpool_test.go          per-key FIFO, global-ceiling peak, distinct-key overlap, key reclamation, close/drain
```

- Files: `keyedpool.go`, `cmd/demo/main.go`, `keyedpool_test.go`.
- Implement: `New(workers int) *Pool`, `(*Pool) Submit(key string, job func()) bool`, `(*Pool) Close()`.
- Test: same-key jobs run in submission order; global concurrency never exceeds `workers`; distinct keys overlap in time; idle keys leave no goroutine or map entry; `Submit` returns false after `Close`, and `Close` blocks until fully drained.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/13-goroutine-pools/13-per-key-ordered-worker-pool/cmd/demo
cd go-solutions/13-goroutines-and-channels/13-goroutine-pools/13-per-key-ordered-worker-pool
go get go.uber.org/goleak
```

### One head per key, one shared pool

The whole design turns on a single invariant: for any key, at most one of its
jobs is *dispatched* at a time, where dispatched means "queued for a worker or
currently executing." Everything else follows from maintaining that invariant.

Keep a FIFO slice per key in a `map[string][]func()`. The job at index 0 is the
head — the one that is dispatched. The rest wait their turn. Submission and
completion are the two events that change the picture:

1. On `Submit(key, job)`, append the job to `queues[key]`. If it is now the only
   entry (`len == 1`), then no head was in flight for this key, so this job
   becomes the head and is pushed onto the shared ready list. If the slice was
   already non-empty, a head is already running, so the job just waits; it is not
   dispatched yet.
2. When a worker finishes a head, it pops index 0. If the queue still has jobs,
   the new index-0 becomes the next head and is dispatched. If the queue is now
   empty, the key is deleted from the map — its state is reclaimed, leaving no
   trace behind.

Because a key's next head is dispatched only after its previous head completes,
the key is serialized, and because both dispatch and pop preserve slice order,
that serialization is FIFO. Different keys are independent: their heads sit in the
shared ready list together and any free worker can run either, so distinct keys
run concurrently.

The global ceiling is not a channel buffer — it is the fixed number of worker
goroutines. Exactly `workers` goroutines exist; each runs one job at a time, so at
most `workers` jobs execute simultaneously, no matter how many keys are live. That
is the property a plain per-key-goroutine design cannot offer: a thousand keys
would mean a thousand goroutines all hammering the endpoint at once.

Workers block on a `sync.Cond` rather than a channel because the ready list both
grows (a worker dispatches the next head) and drains (a worker takes one) from
inside the same worker goroutines; a bounded channel shared as producer and
consumer by the same pool deadlocks when it fills. A condition variable over a
slice sidesteps that: the ready list is bounded only by the number of live keys,
and appends never block. A `pending` counter (submitted minus completed) tells an
idle worker when a closed pool is truly finished so it can retire.

Create `keyedpool.go`:

```go
// Package keyedpool implements a bounded worker pool that serializes jobs
// sharing a key while running distinct keys concurrently under one global
// ceiling. Jobs with the same key execute one at a time in submission order;
// jobs with different keys may run in parallel, but never more than `workers`
// jobs run at once. Idle keys are reclaimed so no per-key goroutine or map
// entry outlives its queue.
package keyedpool

import "sync"

// task is a dispatched head: the front job of some key's FIFO queue, ready for
// any free worker to run.
type task struct {
	key string
	fn  func()
}

// Pool is a shared bounded pool with per-key FIFO ordering.
//
// The invariant: for any key, at most one of its jobs is dispatched (queued in
// `ready` or executing) at a time. The next head is dispatched only after the
// current one finishes, which is what serializes a key in submission order. The
// global ceiling is the fixed count of worker goroutines draining `ready`.
type Pool struct {
	mu      sync.Mutex
	cond    *sync.Cond
	ready   []task              // dispatched heads waiting for a free worker
	queues  map[string][]func() // per-key FIFO; index 0 is the dispatched head
	pending int                 // submitted-but-not-completed jobs, all keys
	closed  bool
	wg      sync.WaitGroup // the fixed worker goroutines
}

// New starts a shared pool with a global ceiling of `workers` in-flight jobs.
func New(workers int) *Pool {
	if workers < 1 {
		workers = 1
	}
	p := &Pool{queues: make(map[string][]func())}
	p.cond = sync.NewCond(&p.mu)
	for range workers {
		p.wg.Go(p.worker)
	}
	return p
}

// Submit enqueues job under key. Jobs sharing a key run one at a time in FIFO
// submission order; distinct keys run concurrently up to the global ceiling.
// Returns false after Close.
func (p *Pool) Submit(key string, job func()) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	q := append(p.queues[key], job)
	p.queues[key] = q
	p.pending++
	// If this job is the only one for its key, no head is in flight, so it
	// becomes the head and is dispatched immediately. Otherwise it waits its
	// turn behind the jobs already queued for this key.
	if len(q) == 1 {
		p.ready = append(p.ready, task{key: key, fn: job})
		p.cond.Signal()
	}
	return true
}

// worker is one of the fixed goroutines. It pulls a dispatched head, runs it,
// then advances that key: pop the finished head and dispatch the next, or
// reclaim the key when its queue empties.
func (p *Pool) worker() {
	for {
		p.mu.Lock()
		for len(p.ready) == 0 && !(p.closed && p.pending == 0) {
			p.cond.Wait()
		}
		if len(p.ready) == 0 {
			// closed and nothing left anywhere: this worker retires.
			p.mu.Unlock()
			return
		}
		t := p.ready[0]
		p.ready = p.ready[1:]
		p.mu.Unlock()

		t.fn()

		p.mu.Lock()
		p.pending--
		q := p.queues[t.key][1:] // drop the head we just ran
		if len(q) == 0 {
			delete(p.queues, t.key) // reclaim: no state survives an empty key
		} else {
			p.queues[t.key] = q
			p.ready = append(p.ready, task{key: t.key, fn: q[0]})
		}
		// Wake blocked workers: a new head may be runnable, or pending may have
		// reached zero and idle workers must observe that and retire.
		p.cond.Broadcast()
		p.mu.Unlock()
	}
}

// Close drains every key's queue and blocks until all jobs finish.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.cond.Broadcast() // release workers idling on an empty pool
	p.mu.Unlock()
	p.wg.Wait()
}
```

### The runnable demo

The demo submits three events for each of three tenants under a two-worker
ceiling. Each job records its label into a per-key slice; because a key is
serialized, its recorded sequence is deterministic. Cross-key completion order is
not, so the demo prints the keys sorted, giving stable output. It also reports
that the observed peak concurrency stayed within the ceiling and that a post-close
submit is rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"example.com/keyedpool"
)

func main() {
	const workers = 2
	p := keyedpool.New(workers)

	// Per-key recorded execution order. A key's jobs are serialized, so appends
	// under one key are ordered; the mutex only guards the shared map against
	// concurrent writes from different keys' jobs.
	var mu sync.Mutex
	order := map[string][]string{}

	// Track the peak number of jobs running at once across all keys.
	var active, peak atomic.Int64

	tenants := []string{"tenant-a", "tenant-b", "tenant-c"}
	for _, tenant := range tenants {
		for i := range 3 {
			label := fmt.Sprintf("%c%d", tenant[len(tenant)-1], i)
			p.Submit(tenant, func() {
				n := active.Add(1)
				for {
					old := peak.Load()
					if n <= old || peak.CompareAndSwap(old, n) {
						break
					}
				}
				mu.Lock()
				order[tenant] = append(order[tenant], label)
				mu.Unlock()
				active.Add(-1)
			})
		}
	}
	p.Close()

	keys := make([]string, 0, len(order))
	for k := range order {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s: %v\n", k, order[k])
	}
	fmt.Printf("peak-within-ceiling(%d): %v\n", workers, peak.Load() <= workers)
	fmt.Printf("submit-after-close: %v\n", p.Submit("tenant-a", func() {}))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
tenant-a: [a0 a1 a2]
tenant-b: [b0 b1 b2]
tenant-c: [c0 c1 c2]
peak-within-ceiling(2): true
submit-after-close: false
```

### Tests

`TestPerKeyFIFOOrder` submits 200 jobs under one key, each appending its index;
because the key is serialized no lock is needed, and the recorded slice must be
`0,1,...,199`. `TestGlobalConcurrencyCeiling` fans 200 jobs across 25 keys and
records the peak concurrent count via a CAS loop; the design bounds it by
`workers` regardless of timing, so the assertion is exact and sleep-free.
`TestDistinctKeysOverlap` places two jobs on different keys at a two-party
barrier: each announces arrival and waits for the other, so a serialized pool
would deadlock and hit the generous timeout, while a correct pool lets both meet.
`TestNoKeyStateLeak` drains 500 transient keys and asserts the queue map is empty
afterward, and the `TestMain` `goleak.VerifyTestMain` confirms no worker or
per-key goroutine leaked. `TestSubmitAfterCloseAndDrain` shows `Close` blocks
until every queued job ran and `Submit` returns false once closed.

Create `keyedpool_test.go`:

```go
package keyedpool

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestPerKeyFIFOOrder pins the core contract: for a single key, jobs execute in
// submission order. Each job appends its index to a slice; because the key is
// serialized, no lock is needed to observe a well-defined sequence, and it must
// equal 0,1,2,...,n-1.
func TestPerKeyFIFOOrder(t *testing.T) {
	t.Parallel()

	p := New(4) // more workers than needed; ordering must not depend on that
	const n = 200
	got := make([]int, 0, n)
	var done sync.WaitGroup
	for i := range n {
		done.Add(1)
		p.Submit("tenant", func() {
			got = append(got, i) // safe: same-key jobs never overlap
			done.Done()
		})
	}
	done.Wait()
	p.Close()

	if len(got) != n {
		t.Fatalf("ran %d jobs, want %d", len(got), n)
	}
	for i := range n {
		if got[i] != i {
			t.Fatalf("execution order broken at position %d: got %d", i, got[i])
		}
	}
}

// TestGlobalConcurrencyCeiling submits many jobs across many keys and records
// the peak number executing at once. The design guarantees it never exceeds the
// worker count regardless of timing, so the assertion is exact and needs no
// sleep.
func TestGlobalConcurrencyCeiling(t *testing.T) {
	t.Parallel()

	const workers = 4
	p := New(workers)
	var active, peak atomic.Int64
	var done sync.WaitGroup

	const keys, perKey = 25, 8
	for k := range keys {
		key := fmt.Sprintf("k%d", k)
		for range perKey {
			done.Add(1)
			p.Submit(key, func() {
				n := active.Add(1)
				for {
					old := peak.Load()
					if n <= old || peak.CompareAndSwap(old, n) {
						break
					}
				}
				active.Add(-1)
				done.Done()
			})
		}
	}
	done.Wait()
	p.Close()

	if got := peak.Load(); got > workers {
		t.Fatalf("observed peak concurrency %d exceeds ceiling %d", got, workers)
	}
}

// TestDistinctKeysOverlap proves distinct keys really run concurrently, not
// serialized. Two jobs on different keys meet at a two-party barrier: each
// announces arrival and waits for the other. If the pool serialized them, the
// first would block forever waiting for a job that cannot start until it
// returns, and the generous timeout would fire.
func TestDistinctKeysOverlap(t *testing.T) {
	t.Parallel()

	p := New(2)
	defer p.Close()

	const timeout = 2 * time.Second
	aArrived := make(chan struct{})
	bArrived := make(chan struct{})
	met := make(chan bool, 2)

	rendezvous := func(mine, other chan struct{}) func() {
		return func() {
			close(mine)
			select {
			case <-other:
				met <- true
			case <-time.After(timeout):
				met <- false
			}
		}
	}
	p.Submit("a", rendezvous(aArrived, bArrived))
	p.Submit("b", rendezvous(bArrived, aArrived))

	for range 2 {
		if !<-met {
			t.Fatal("distinct-key jobs did not overlap; they were serialized")
		}
	}
}

// TestNoKeyStateLeak drains a large burst across many transient keys, then
// asserts every key's map entry was reclaimed. Combined with the TestMain
// goleak check, this confirms idle keys leave behind no goroutine or structure.
func TestNoKeyStateLeak(t *testing.T) {
	t.Parallel()

	p := New(3)
	var done sync.WaitGroup
	const keys = 500
	for k := range keys {
		done.Add(1)
		p.Submit(fmt.Sprintf("ephemeral-%d", k), func() { done.Done() })
	}
	done.Wait()
	p.Close()

	p.mu.Lock()
	remaining := len(p.queues)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d key entries survived drain, want 0 (keys not reclaimed)", remaining)
	}
}

// TestSubmitAfterCloseAndDrain proves two contracts: Close blocks until every
// queued job has run (the counter must reach the full count), and Submit
// returns false once the pool is closed.
func TestSubmitAfterCloseAndDrain(t *testing.T) {
	t.Parallel()

	p := New(2)
	var ran atomic.Int64
	const n = 50
	for range n {
		// One key, so these are strictly serialized and still all must run.
		p.Submit("k", func() { ran.Add(1) })
	}
	p.Close() // must block until all n have executed

	if got := ran.Load(); got != n {
		t.Fatalf("Close returned with %d/%d jobs run; it did not drain", got, n)
	}
	if p.Submit("k", func() {}) {
		t.Fatal("Submit returned true after Close, want false")
	}
}
```

## Review

Correct here means three properties hold at once: same-key jobs run in submission
order, distinct keys run concurrently, and no more than `workers` jobs run at any
instant. All three come from the single invariant that each key has at most one
dispatched head, advanced only on completion: FIFO within a key because dispatch
and pop preserve order, concurrency across keys because independent heads share
the ready list, and the ceiling because a fixed set of worker goroutines is the
only thing that runs jobs. `TestPerKeyFIFOOrder` proves the ordering, the CAS peak
in `TestGlobalConcurrencyCeiling` proves the ceiling is never breached, the
two-party barrier in `TestDistinctKeysOverlap` proves keys truly overlap rather
than merely interleaving, and reclaiming a key on an empty queue plus
`goleak.VerifyTestMain` proves the design does not leak a goroutine or a map entry
per key. That last point is the production bug this pattern prevents: the tempting
"one goroutine and one mutex per key" design serializes correctly but accumulates
an idle goroutine and lock for every key ever seen and ignores the global ceiling,
so a high-cardinality key space (millions of tenants) both leaks unboundedly and
stampedes the very endpoint the pool was meant to protect.

## Resources

- [`sync.Cond`](https://pkg.go.dev/sync#Cond) -- the condition variable that lets workers wait for a ready head without a self-deadlocking shared channel.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) -- `Go` launches each worker and its `Wait` gives `Close` its drain-until-finished contract.
- [go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- verifies that draining and reclaiming keys leaves no goroutine behind.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- background on structuring bounded concurrent stages in Go.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-outbox-relay-partial-ack.md](12-outbox-relay-partial-ack.md) | Next: [14-dynamic-resize-worker-pool.md](14-dynamic-resize-worker-pool.md)
