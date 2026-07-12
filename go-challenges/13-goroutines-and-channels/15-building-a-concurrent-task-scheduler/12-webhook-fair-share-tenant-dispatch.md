# Exercise 12: Fair-Share Dispatch: Per-Tenant Isolation Under a Noisy Neighbour

**Level: Advanced**

A webhook fan-out service is shared by many tenants over one fixed worker pool. A
single FIFO queue is the trap: one tenant that dumps 10k events monopolizes every
worker and starves another tenant's latency-sensitive deliveries behind a wall of
backlog. This module builds a dispatcher that keeps one queue per tenant and
services them round-robin, so throughput is shared and no tenant starves, while
FIFO order is preserved within a tenant. The subtle correctness is reclaiming an
idle tenant's queue without racing a concurrent `Submit` that re-populates it.

This module is self-contained: its own module, a `fairshare` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
fairshare/                   independent module: example.com/fairshare
  go.mod                     go 1.26
  fairshare.go               New, Submit, Shutdown, Stats over a single-owner dispatcher
  cmd/demo/main.go           runnable demo: round-robin interleaving of a heavy and a light tenant
  fairshare_test.go          fairness/no-starvation, FIFO-within-tenant, reclamation churn, bounded shutdown
```

- Files: `fairshare.go`, `cmd/demo/main.go`, `fairshare_test.go`.
- Implement: `New(workers int) *Dispatcher`, `Submit(ctx, tenant, fn) (<-chan Result, error)`, `Shutdown(ctx) error`, `Stats() Stats`, with `Task func(ctx) (any, error)` and `Result{Value, Err}`.
- Test: light-tenant tasks all complete before the heavy tenant's 100th; FIFO within a tenant; a drained lane retires and re-creates correctly under churn; graceful drain bounded by ctx; idempotent shutdown.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get go.uber.org/goleak
go mod tidy
```

### One owner for the queues, round-robin for fairness

The design rests on a single rule: exactly one goroutine — the dispatcher — owns
every per-tenant queue, the ring of active tenants, and the round-robin cursor.
Producers calling `Submit` never touch those maps; they hand a task in over a
channel. Workers never touch them either; they receive a ready job over another
channel. Because one goroutine reads and writes the shared structure, the data
race is structurally impossible rather than merely guarded by a mutex, and the
whole fairness policy lives in one readable `select` loop.

Fairness is round-robin over the tenants that currently have queued work. The
dispatcher keeps a `ring []string` that holds *exactly* the tenants with a
non-empty queue, plus a `cursor` into it. To pick the next job it takes the head
of `queues[ring[cursor]]` (FIFO within the tenant) and advances the cursor to the
next tenant. With one gated worker and two tenants, this produces a strict
interleave: heavy, light, heavy, light, ... so the light tenant's ten tasks all
clear while the heavy tenant is barely a fraction into its thousand.

The delicate part is reclamation. When a tenant's queue drains you want its lane
to disappear so `ActiveTenants` reflects reality and the ring stays small. The
protocol:

1. The dispatcher pops the last job from a tenant's queue.
2. In the same step it deletes the tenant from `queues` and removes it from the
   ring. Removing `ring[cursor]` shifts the next tenant into the cursor's slot, so
   round-robin simply continues without a skipped turn.
3. A later `Submit` for that same tenant is just another message on the submit
   channel; the dispatcher re-creates the lane and appends it to the ring's tail.

Steps 2 and 3 are the retire-vs-submit race that sinks lock-based designs: a
mutex-guarded map where one goroutine deletes a key while another re-inserts it is
a minefield of lost updates and "assignment to entry in nil map" panics. Here both
happen inside the dispatcher goroutine, one after the other, so there is nothing
to race. Because the ring holds only non-empty lanes, picking a job is always
valid — no scanning for a non-empty queue, no cursor pointing at a retired tenant.

Two channel-capacity choices keep workers from ever wedging. Each result channel
is buffered with capacity one, so a worker delivering a result never blocks on a
caller that walked away. The worker-to-dispatcher completion channel is buffered
to the worker count; since at most `workers` jobs are ever in flight, a worker can
always report completion even after the dispatcher has stopped reading it during a
hard shutdown — which is what lets every goroutine exit cleanly under `goleak`.

Create `fairshare.go`:

```go
// Package fairshare dispatches webhook deliveries across many tenants over one
// fixed worker pool without letting a noisy neighbour starve the others.
//
// A single FIFO queue lets a tenant that dumps 10k events monopolize every
// worker. This dispatcher keeps one queue per tenant and services them
// round-robin, so throughput is shared and no tenant starves, while FIFO order
// is preserved within a tenant. A single dispatcher goroutine owns all per-tenant
// queues and the round-robin cursor: producers hand work in over a channel and
// never touch the shared maps, so retiring an idle tenant's lane can never race a
// concurrent Submit that re-populates it.
package fairshare

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrShuttingDown is returned by Submit once Shutdown has begun.
var ErrShuttingDown = errors.New("fairshare: dispatcher shutting down")

// Task is the unit of work. It must cooperate with cancellation: on shutdown the
// dispatcher cancels the context it is given.
type Task func(ctx context.Context) (any, error)

// Result carries a task's outcome on its capacity-1 result channel.
type Result struct {
	Value any
	Err   error
}

// Stats is a coherent snapshot of dispatcher state.
type Stats struct {
	ActiveTenants int              // tenants with at least one queued (not-yet-dispatched) task
	PerTenant     map[string]int64 // cumulative tasks submitted per tenant
}

type submission struct {
	tenant string
	fn     Task
	res    chan Result
}

type job struct {
	fn  Task
	res chan Result
}

// Dispatcher fans webhook tasks out over a fixed worker pool with per-tenant
// round-robin fairness. The dispatcher goroutine is the sole owner of the
// per-tenant queues and the round-robin cursor.
type Dispatcher struct {
	dispatch chan job
	submitCh chan submission
	doneCh   chan struct{}
	statsReq chan chan Stats

	stopping chan struct{}
	hardStop chan struct{}
	drained  chan struct{}
	stopOnce sync.Once
	hardOnce sync.Once

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelCauseFunc

	// finalStats is written by the dispatcher before it closes drained; a reader
	// that observes drained closed is guaranteed to see the completed write.
	finalStats Stats
}

// New starts a Dispatcher with the given number of workers (at least 1).
func New(workers int) *Dispatcher {
	if workers < 1 {
		workers = 1
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	d := &Dispatcher{
		dispatch: make(chan job),
		submitCh: make(chan submission),
		doneCh:   make(chan struct{}, workers), // in-flight <= workers, so sends never block
		statsReq: make(chan chan Stats),
		stopping: make(chan struct{}),
		hardStop: make(chan struct{}),
		drained:  make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}
	for range workers {
		d.wg.Go(d.worker)
	}
	go d.run()
	return d
}

func (d *Dispatcher) worker() {
	for j := range d.dispatch {
		res := runTask(d.ctx, j.fn)
		j.res <- res           // res is buffered cap 1: never blocks on an absent caller
		d.doneCh <- struct{}{} // doneCh is buffered cap workers: never blocks the worker
	}
}

// runTask converts a panic into an error result so one poison payload cannot kill
// the pool. recover works only in a deferred function on the panicking goroutine.
func runTask(ctx context.Context, fn Task) (r Result) {
	defer func() {
		if p := recover(); p != nil {
			r = Result{Err: fmt.Errorf("fairshare: task panicked: %v", p)}
		}
	}()
	v, err := fn(ctx)
	return Result{Value: v, Err: err}
}

// run is the single owner of every per-tenant queue and the round-robin cursor.
// Because it is the only goroutine that touches these maps, retiring a drained
// tenant cannot race a Submit that re-creates it: both are serialized here.
func (d *Dispatcher) run() {
	queues := map[string][]job{}
	submitted := map[string]int64{}
	var ring []string // exactly the tenants with a non-empty queue
	cursor := 0
	pending := 0
	inflight := 0
	draining := false
	stopC := d.stopping

	snapshot := func() Stats {
		per := make(map[string]int64, len(submitted))
		for k, v := range submitted {
			per[k] = v
		}
		return Stats{ActiveTenants: len(queues), PerTenant: per}
	}
	finish := func() {
		d.finalStats = snapshot()
		close(d.dispatch)
		close(d.drained)
	}

	for {
		if draining && pending == 0 && inflight == 0 {
			finish()
			return
		}

		// A nil channel disables its select case. New submissions are refused once
		// draining; dispatch is offered only when some tenant has a queued task.
		var subC chan submission
		if !draining {
			subC = d.submitCh
		}
		var dispC chan job
		var next job
		if len(ring) > 0 {
			next = queues[ring[cursor]][0] // peek head of the current tenant (FIFO)
			dispC = d.dispatch
		}

		select {
		case sub := <-subC:
			q, ok := queues[sub.tenant]
			if !ok {
				ring = append(ring, sub.tenant) // new lane joins the round-robin at the tail
			}
			queues[sub.tenant] = append(q, job{fn: sub.fn, res: sub.res})
			submitted[sub.tenant]++
			pending++

		case dispC <- next:
			t := ring[cursor]
			q := queues[t][1:]
			pending--
			inflight++
			if len(q) == 0 {
				// Retire the drained lane. Removing ring[cursor] shifts the next
				// tenant into cursor's slot, so round-robin simply continues.
				delete(queues, t)
				ring = append(ring[:cursor], ring[cursor+1:]...)
				if len(ring) == 0 {
					cursor = 0
				} else {
					cursor %= len(ring)
				}
			} else {
				queues[t] = q
				cursor = (cursor + 1) % len(ring)
			}

		case <-d.doneCh:
			inflight--

		case <-stopC:
			draining = true
			stopC = nil // a closed channel is always ready; nil it to stop spinning

		case <-d.hardStop:
			finish() // abandon: workers finish their current task and exit
			return

		case reply := <-d.statsReq:
			reply <- snapshot()
		}
	}
}

// Submit enqueues fn for tenant and returns a capacity-1 result channel. It
// respects ctx for admission and returns ErrShuttingDown once Shutdown has begun.
func (d *Dispatcher) Submit(ctx context.Context, tenant string, fn Task) (<-chan Result, error) {
	select {
	case <-d.stopping:
		return nil, ErrShuttingDown
	default:
	}
	res := make(chan Result, 1)
	select {
	case d.submitCh <- submission{tenant: tenant, fn: fn, res: res}:
		return res, nil
	case <-d.stopping:
		return nil, ErrShuttingDown
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Stats returns a coherent snapshot. While the dispatcher runs it answers over a
// request channel; after shutdown it returns the final snapshot the dispatcher
// stored before exiting.
func (d *Dispatcher) Stats() Stats {
	reply := make(chan Stats, 1)
	select {
	case d.statsReq <- reply:
		return <-reply
	case <-d.drained:
		return d.finalStats
	}
}

// Shutdown stops accepting new work and drains what is pending and in-flight,
// bounded by ctx. If ctx expires first it cancels running tasks and returns
// ctx.Err(). It is idempotent.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	d.stopOnce.Do(func() { close(d.stopping) })
	select {
	case <-d.drained:
		d.cancel(ErrShuttingDown)
		d.wg.Wait()
		return nil
	case <-ctx.Done():
		d.hardOnce.Do(func() { close(d.hardStop) })
		d.cancel(ErrShuttingDown) // cooperative tasks observe cancellation and abort
		<-d.drained
		d.wg.Wait()
		return ctx.Err()
	}
}
```

### The runnable demo

The demo makes round-robin fairness visible with one worker. A primer task parks
the sole worker while both tenant lanes fill completely, so no heavy task escapes
into flight ahead of the light lane; then the gate opens and completion order is
exactly the dispatch order. The heavy tenant `reports` submits six tasks and the
light tenant `alerts` submits three, and they interleave until `alerts` drains.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"example.com/fairshare"
)

func main() {
	d := fairshare.New(1) // one worker makes dispatch order fully observable

	// A primer task occupies the single worker while we enqueue everything, so no
	// real task escapes into flight before both tenant lanes are fully queued.
	gate := make(chan struct{})
	running := make(chan struct{})
	ctx := context.Background()
	if _, err := d.Submit(ctx, "warmup", func(context.Context) (any, error) {
		close(running)
		<-gate
		return nil, nil
	}); err != nil {
		panic(err)
	}
	<-running // the worker is now parked inside the primer

	// Heavy tenant "reports" (6) and light tenant "alerts" (3), submitted in full
	// before any of them can run.
	order := make(chan string, 9)
	mkTask := func(label string) fairshare.Task {
		return func(context.Context) (any, error) { order <- label; return label, nil }
	}
	for i := range 6 {
		if _, err := d.Submit(ctx, "reports", mkTask(fmt.Sprintf("reports-%d", i))); err != nil {
			panic(err)
		}
	}
	for i := range 3 {
		if _, err := d.Submit(ctx, "alerts", mkTask(fmt.Sprintf("alerts-%d", i))); err != nil {
			panic(err)
		}
	}

	fmt.Println("active tenants while queued:", d.Stats().ActiveTenants)

	close(gate) // release the worker; round-robin dispatch begins

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Shutdown(shutCtx); err != nil {
		panic(err)
	}

	fmt.Println("completion order:")
	for range 9 {
		fmt.Println(" ", <-order)
	}

	st := d.Stats()
	fmt.Println("active tenants after drain:", st.ActiveTenants)
	keys := make([]string, 0, len(st.PerTenant))
	for k := range st.PerTenant {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Print("per-tenant submitted:")
	for _, k := range keys {
		fmt.Printf(" %s=%d", k, st.PerTenant[k])
	}
	fmt.Println()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
active tenants while queued: 2
completion order:
  reports-0
  alerts-0
  reports-1
  alerts-1
  reports-2
  alerts-2
  reports-3
  reports-4
  reports-5
active tenants after drain: 0
per-tenant submitted: alerts=3 reports=6 warmup=1
```

### Tests

`TestFairnessNoStarvation` is the headline property: one worker, a heavy tenant
with 1000 tasks and a light tenant with 10, a primer to fully populate both lanes
before dispatch begins, and an assertion that the last light completion lands
strictly before the heavy tenant's 100th — a deterministic consequence of
round-robin. `TestFIFOWithinTenant` submits 200 tasks to one tenant on one worker
and asserts they execute in submission order. `TestReclamationChurn` runs six
concurrent producers over 40 rounds each, repeatedly creating and draining lanes,
proving under `-race` that single ownership makes retire-vs-submit safe and that
`ActiveTenants` returns to zero. `TestReclaimThenReuse` retires and re-creates one
lane 25 times. `TestShutdownDrainsAndIsIdempotent` shows graceful drain, post-
shutdown `ErrShuttingDown`, and a no-op second shutdown.
`TestShutdownBoundedByContext` shows a deadline-bounded shutdown returning
`context.DeadlineExceeded` while cancelling a cooperative task.
`TestPanicBecomesError` shows a poison task fails its caller without killing the
pool. `TestMain` wraps everything in `goleak.VerifyTestMain` to prove no dispatcher
or worker goroutine leaks after `Shutdown`.

Create `fairshare_test.go`:

```go
package fairshare

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func mustSubmit(t *testing.T, d *Dispatcher, tenant string, fn Task) <-chan Result {
	t.Helper()
	res, err := d.Submit(context.Background(), tenant, fn)
	if err != nil {
		t.Fatalf("Submit(%s): %v", tenant, err)
	}
	return res
}

// TestFairnessNoStarvation pins down the core property: with a single worker and
// two tenants, a heavy tenant submitting 1000 tasks cannot starve a light tenant's
// 10 tasks. Round-robin dispatch interleaves the lanes, so every light task
// completes long before the heavy tenant's 100th.
func TestFairnessNoStarvation(t *testing.T) {
	d := New(1) // one worker: completion order == dispatch order

	// A primer parks the sole worker while both lanes fill, so no heavy task
	// escapes into flight ahead of the light lane.
	gate := make(chan struct{})
	running := make(chan struct{})
	mustSubmit(t, d, "primer", func(context.Context) (any, error) {
		close(running)
		<-gate
		return nil, nil
	})
	<-running

	var mu sync.Mutex
	var order []string
	record := func(tenant string) Task {
		return func(context.Context) (any, error) {
			mu.Lock()
			order = append(order, tenant)
			mu.Unlock()
			return nil, nil
		}
	}

	const heavy, light = 1000, 10
	for range heavy {
		mustSubmit(t, d, "heavy", record("heavy"))
	}
	for range light {
		mustSubmit(t, d, "light", record("light"))
	}

	close(gate)
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	lightSeen, heavySeen := 0, 0
	lastLightAt, hundredthHeavyAt := -1, -1
	for i, tenant := range order {
		switch tenant {
		case "light":
			lightSeen++
			lastLightAt = i
		case "heavy":
			heavySeen++
			if heavySeen == 100 {
				hundredthHeavyAt = i
			}
		}
	}
	if lightSeen != light {
		t.Fatalf("light completions = %d, want %d", lightSeen, light)
	}
	if hundredthHeavyAt < 0 {
		t.Fatalf("heavy tenant never reached 100 completions")
	}
	if lastLightAt >= hundredthHeavyAt {
		t.Fatalf("light lane starved: last light at %d, 100th heavy at %d", lastLightAt, hundredthHeavyAt)
	}
}

// TestFIFOWithinTenant asserts a single tenant's tasks execute in submission order.
func TestFIFOWithinTenant(t *testing.T) {
	d := New(1)

	const n = 200
	seq := make(chan int, n)
	for i := range n {
		mustSubmit(t, d, "acme", func(context.Context) (any, error) {
			seq <- i
			return i, nil
		})
	}
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	got := make([]int, 0, n)
	for range n {
		got = append(got, <-seq)
	}
	want := make([]int, n)
	for i := range want {
		want[i] = i
	}
	if !slices.Equal(got, want) {
		t.Fatalf("execution order not FIFO: got %v", got)
	}
}

// TestReclamationChurn stresses the retire-vs-submit boundary: many rounds and
// concurrent producers repeatedly create and drain tenant lanes. Under -race this
// proves single ownership makes the churn safe, and that ActiveTenants returns to
// zero once every lane drains.
func TestReclamationChurn(t *testing.T) {
	d := New(4)

	const producers, rounds, burst = 6, 40, 15
	var wg sync.WaitGroup
	for p := range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tenant := fmt.Sprintf("tenant-%d", p)
			for r := range rounds {
				results := make([]<-chan Result, 0, burst)
				for k := range burst {
					want := r*burst + k
					res := mustSubmit(t, d, tenant, func(context.Context) (any, error) {
						return want, nil
					})
					results = append(results, res)
				}
				for _, res := range results {
					if got := <-res; got.Err != nil {
						t.Errorf("%s: unexpected error %v", tenant, got.Err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	// Every lane has drained and retired; no new work is pending.
	if got := d.Stats().ActiveTenants; got != 0 {
		t.Fatalf("ActiveTenants after full drain = %d, want 0", got)
	}
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	st := d.Stats()
	for p := range producers {
		tenant := fmt.Sprintf("tenant-%d", p)
		if got, want := st.PerTenant[tenant], int64(rounds*burst); got != want {
			t.Fatalf("PerTenant[%s] = %d, want %d", tenant, got, want)
		}
	}
}

// TestReclaimThenReuse asserts a retired lane can be re-created and runs correctly.
func TestReclaimThenReuse(t *testing.T) {
	d := New(2)
	defer func() {
		if err := d.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()

	for round := range 25 {
		res := mustSubmit(t, d, "recurring", func(context.Context) (any, error) {
			return round, nil
		})
		got := <-res
		if got.Err != nil || got.Value.(int) != round {
			t.Fatalf("round %d: got %+v", round, got)
		}
		if a := d.Stats().ActiveTenants; a != 0 {
			t.Fatalf("round %d: ActiveTenants = %d, want 0 after drain", round, a)
		}
	}
}

// TestShutdownDrainsAndIsIdempotent asserts pending work completes on graceful
// shutdown, that Submit is refused afterward, and that a second Shutdown is a no-op.
func TestShutdownDrainsAndIsIdempotent(t *testing.T) {
	d := New(2)

	const n = 50
	var done int
	var mu sync.Mutex
	for range n {
		mustSubmit(t, d, "webhooks", func(context.Context) (any, error) {
			mu.Lock()
			done++
			mu.Unlock()
			return nil, nil
		})
	}

	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	mu.Lock()
	if done != n {
		t.Fatalf("drained %d tasks, want %d", done, n)
	}
	mu.Unlock()

	if _, err := d.Submit(context.Background(), "webhooks", func(context.Context) (any, error) {
		return nil, nil
	}); err != ErrShuttingDown {
		t.Fatalf("Submit after Shutdown = %v, want ErrShuttingDown", err)
	}

	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown = %v, want nil", err)
	}
}

// TestShutdownBoundedByContext asserts a shutdown whose deadline expires while a
// cooperative task is still running returns ctx.Err() and cancels the task, with
// no leaked goroutine.
func TestShutdownBoundedByContext(t *testing.T) {
	d := New(2)

	running := make(chan struct{})
	mustSubmit(t, d, "slow", func(ctx context.Context) (any, error) {
		close(running)
		<-ctx.Done() // cooperative: aborts when the dispatcher cancels
		return nil, ctx.Err()
	})
	<-running

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := d.Shutdown(ctx); err != context.DeadlineExceeded {
		t.Fatalf("Shutdown = %v, want DeadlineExceeded", err)
	}
}

// TestPanicBecomesError asserts a panicking task fails its caller without killing
// the pool: a later task on the same worker still runs.
func TestPanicBecomesError(t *testing.T) {
	d := New(1)
	defer func() {
		if err := d.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()

	bad := mustSubmit(t, d, "poison", func(context.Context) (any, error) {
		panic("boom")
	})
	if got := <-bad; got.Err == nil {
		t.Fatalf("panicking task returned nil error")
	}

	good := mustSubmit(t, d, "poison", func(context.Context) (any, error) {
		return "ok", nil
	})
	if got := <-good; got.Err != nil || got.Value.(string) != "ok" {
		t.Fatalf("pool did not survive panic: %+v", got)
	}
}
```

## Review

Correct here means two things at once: no tenant starves, and the lane bookkeeping
never corrupts under concurrent create/retire. Both fall out of one decision — a
single dispatcher goroutine owns the per-tenant queues, the ring of active
tenants, and the cursor, so producers and workers only ever exchange messages with
it. Fairness is the round-robin pick over a ring that holds exactly the non-empty
lanes; `TestFairnessNoStarvation` proves it by asserting all ten light-tenant
completions land before the heavy tenant's 100th, a deterministic ordering that a
single FIFO queue would violate outright. Reclamation is a delete-and-remove done
in the same goroutine step as the pop, so `TestReclamationChurn` can hammer the
retire-vs-submit boundary with six concurrent producers under `-race` and find
nothing to race — the "assignment to entry in nil map" panic and the lost-update
drift that a mutex-guarded map invites are simply unreachable. Buffered result and
completion channels keep workers from wedging on absent callers or a departed
dispatcher, and `goleak` confirms every goroutine exits after `Shutdown`. The
production bug this prevents is the one operators page on: a shared webhook pool
where one tenant's bulk backfill silently blackholes everyone else's real-time
deliveries.

## Resources

- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) -- the single-owner-plus-channels model this dispatcher is built on.
- [`context`](https://pkg.go.dev/context) -- `WithCancelCause` for cancelling in-flight tasks on a bounded shutdown.
- [Share Memory By Communicating](https://go.dev/blog/codelab-share) -- why one owning goroutine beats locking a shared map.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) -- asserting no dispatcher or worker goroutine leaks after shutdown.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-periodic-compaction-skip-if-running.md](11-periodic-compaction-skip-if-running.md) | Next: [13-per-aggregate-ordered-key-scheduler.md](13-per-aggregate-ordered-key-scheduler.md)
