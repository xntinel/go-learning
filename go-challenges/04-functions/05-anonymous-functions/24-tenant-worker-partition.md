# Exercise 24: Multi-Tenant System with Per-Tenant Goroutine Worker Partitioning

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A multi-tenant system needs work from different tenants to run concurrently without
ever letting one tenant's jobs touch another tenant's state. This module builds a
dispatcher that gives every tenant its own dedicated worker goroutine — launched
lazily, from a function literal, the first time that tenant submits a job — so
cross-tenant races are impossible by construction: no two tenants ever share a
queue, a goroutine, or any other mutable state.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
tenant/                        module example.com/tenant
  go.mod
  tenant.go                     Dispatcher, NewDispatcher, Submit, CloseAndWait
  tenant_test.go                each job runs once, per-tenant order preserved, drains all workers
  cmd/demo/main.go              three tenants, 50 jobs each, concurrently
```

- Files: `tenant.go`, `tenant_test.go`, `cmd/demo/main.go`.
- Implement: `Dispatcher.Submit(tenant, fn)` lazily creating a tenant's own buffered queue and worker goroutine literal on first use, guarded by a mutex over the queue map only (never over per-tenant state); `CloseAndWait` closing every queue and joining every worker.
- Test: every tenant's jobs run exactly once across many tenants concurrently; a single tenant's jobs run strictly in submission order (proving one dedicated worker per tenant); `CloseAndWait` drains every already-queued job before returning. Under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/24-tenant-worker-partition/cmd/demo
cd go-solutions/04-functions/05-anonymous-functions/24-tenant-worker-partition
go mod edit -go=1.24
```

### One worker per tenant, not one worker per job

`Submit` looks up the tenant's queue under a mutex; if it does not exist yet, it
creates a buffered channel and launches exactly one goroutine literal to drain it,
passing the channel in as an argument — `go func(tenantQueue chan func()) { ... }(q)`
— so the goroutine only ever reaches into the one channel it was handed. That
goroutine then runs every job for that tenant, one at a time, for as long as the
`Dispatcher` lives. Two different tenants' jobs can run at the exact same instant,
on two different goroutines, without any risk of racing on each other's state,
because they never share anything: not a queue, not a counter, not a lock beyond the
one brief mutex hold used only to look up or create a queue in the map. The mutex
protects `Dispatcher.queues`, the map itself — never a tenant's actual job
processing, which is why lookups are cheap and jobs never contend with each other
across tenants.

Because each tenant has exactly one worker goroutine draining its queue in FIFO
order, jobs submitted for the same tenant are guaranteed to run in the order they
were submitted — a property the per-tenant order test checks directly.

Create `tenant.go`:

```go
package tenant

import "sync"

// Dispatcher runs jobs for many tenants concurrently, but every tenant's
// jobs are processed serially by that tenant's own dedicated worker
// goroutine. Different tenants never share a worker, a queue, or any other
// mutable state, so two tenants' jobs can run at the same instant without
// racing on anything — the partitioning itself is what makes the fan-out
// safe, not a lock shared across tenants.
type Dispatcher struct {
	mu     sync.Mutex
	queues map[string]chan func()
	wg     sync.WaitGroup
}

// NewDispatcher returns an empty Dispatcher.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{queues: make(map[string]chan func())}
}

// Submit enqueues fn to run on tenant's dedicated worker, creating that
// worker's queue and goroutine literal lazily on the tenant's first job.
// The goroutine literal captures only tenantQueue, passed as an argument,
// so it never reaches into another tenant's state.
func (d *Dispatcher) Submit(tenant string, fn func()) {
	d.mu.Lock()
	q, ok := d.queues[tenant]
	if !ok {
		q = make(chan func(), 32)
		d.queues[tenant] = q
		d.wg.Add(1)
		go func(tenantQueue chan func()) {
			defer d.wg.Done()
			for job := range tenantQueue {
				job()
			}
		}(q)
	}
	d.mu.Unlock()
	q <- fn
}

// CloseAndWait closes every tenant's queue and waits for every worker to
// drain its remaining jobs and exit.
func (d *Dispatcher) CloseAndWait() {
	d.mu.Lock()
	for _, q := range d.queues {
		close(q)
	}
	d.mu.Unlock()
	d.wg.Wait()
}
```

### The runnable demo

The demo submits fifty jobs for each of three tenants concurrently, each job
incrementing that tenant's own counter, then prints the sorted final counts — which
are always the same regardless of scheduling.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"example.com/tenant"
)

func main() {
	d := tenant.NewDispatcher()

	tenants := []string{"acme", "globex", "initech"}
	counts := make(map[string]*atomic.Int64, len(tenants))
	for _, tn := range tenants {
		counts[tn] = &atomic.Int64{}
	}

	var wg sync.WaitGroup
	for _, tn := range tenants {
		counter := counts[tn]
		for i := 0; i < 50; i++ {
			wg.Add(1)
			d.Submit(tn, func() {
				defer wg.Done()
				counter.Add(1)
			})
		}
	}
	wg.Wait()
	d.CloseAndWait()

	sort.Strings(tenants)
	for _, tn := range tenants {
		fmt.Printf("%s: %d jobs\n", tn, counts[tn].Load())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acme: 50 jobs
globex: 50 jobs
initech: 50 jobs
```

### Tests

`TestEachTenantJobRunsExactlyOnce` submits two hundred jobs per tenant across three
tenants concurrently and checks every tenant's own atomic counter lands exactly on
the expected count — proving no cross-tenant interference. `TestTenantJobsRunInSubmissionOrder`
submits a hundred jobs to a single tenant and checks they land in a shared slice in
exact submission order, which only holds because one dedicated goroutine drains that
tenant's queue serially. `TestCloseAndWaitDrainsAllTenantWorkers` checks
`CloseAndWait` does not return until every already-queued job across every tenant
has actually run.

Create `tenant_test.go`:

```go
package tenant

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestEachTenantJobRunsExactlyOnce(t *testing.T) {
	t.Parallel()
	d := NewDispatcher()

	tenants := []string{"a", "b", "c"}
	const jobsPerTenant = 200
	counters := make(map[string]*atomic.Int64, len(tenants))
	for _, tn := range tenants {
		counters[tn] = &atomic.Int64{}
	}

	var wg sync.WaitGroup
	for _, tn := range tenants {
		counter := counters[tn]
		for i := 0; i < jobsPerTenant; i++ {
			wg.Add(1)
			d.Submit(tn, func() {
				defer wg.Done()
				counter.Add(1)
			})
		}
	}
	wg.Wait()
	d.CloseAndWait()

	for _, tn := range tenants {
		if got := counters[tn].Load(); got != jobsPerTenant {
			t.Fatalf("tenant %s ran %d jobs, want %d", tn, got, jobsPerTenant)
		}
	}
}

func TestTenantJobsRunInSubmissionOrder(t *testing.T) {
	t.Parallel()
	d := NewDispatcher()

	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		i := i
		wg.Add(1)
		d.Submit("single-tenant", func() {
			defer wg.Done()
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
		})
	}
	wg.Wait()
	d.CloseAndWait()

	if len(order) != 100 {
		t.Fatalf("len(order) = %d, want 100", len(order))
	}
	for i, v := range order {
		if v != i {
			t.Fatalf("order[%d] = %d, want %d (a single tenant's worker must run jobs serially, in order)", i, v, i)
		}
	}
}

func TestCloseAndWaitDrainsAllTenantWorkers(t *testing.T) {
	t.Parallel()
	d := NewDispatcher()

	var completed atomic.Int64
	for _, tn := range []string{"x", "y"} {
		for i := 0; i < 20; i++ {
			d.Submit(tn, func() { completed.Add(1) })
		}
	}
	d.CloseAndWait()

	if got := completed.Load(); got != 40 {
		t.Fatalf("completed = %d, want 40 (CloseAndWait must drain every queued job)", got)
	}
}
```

## Review

The dispatcher is correct when every tenant's job count lands exactly right under
`-race`, which is the strongest evidence that no cross-tenant state exists at all —
if two tenants ever shared a counter or a queue, the race detector would flag it
under concurrent submission. The submission-order test is the one that verifies the
"one dedicated worker per tenant" design specifically, as opposed to a pool shared
across tenants: only a single serial worker per tenant guarantees FIFO order for that
tenant's own jobs while still allowing full concurrency across tenants. The mutex in
`Submit` is scoped as tightly as possible — around the map lookup and the one-time
goroutine launch only — so it is never a source of contention between tenants once
each has its queue.

## Resources

- [Go Language Specification: Function literals](https://go.dev/ref/spec#Function_literals)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-deadline-afterfunc-enforcement.md](23-deadline-afterfunc-enforcement.md) | Next: [25-deferred-cleanup-guard.md](25-deferred-cleanup-guard.md)
