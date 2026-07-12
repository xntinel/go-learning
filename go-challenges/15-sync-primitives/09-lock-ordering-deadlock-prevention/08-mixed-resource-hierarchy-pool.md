# Exercise 8: Acquisition Hierarchy Across Lock Types — Pool Slot Before Row Lock

The nastiest production deadlocks involve no two mutexes at all: a bounded
connection pool and a per-record mutex can form a cycle between them, and no
mutex-ordering analysis will ever see it. This exercise builds a batch-update
worker system that wedges with the wrong acquisition order across *resource
types*, then fixes it with a documented hierarchy: pool slot before any record
lock, always.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
batchpool/                 independent module: example.com/batchpool
  go.mod
  batchpool.go             type Pool (buffered-channel semaphore) with
                           Acquire(ctx)/Release; type Record (per-row mutex);
                           UpdateDirect (slot -> record) and UpdateComputed
                           (read, release, slot, re-lock with version check)
  cmd/
    demo/
      main.go              runnable demo: workers mixing both paths, counts
  batchpool_test.go        the reproduced wedge (broken path B vs path A on a
                           1-slot pool), the fixed paths under a 20-worker
                           stress run with a watchdog, update-count invariant
```

- Files: `batchpool.go`, `cmd/demo/main.go`, `batchpool_test.go`.
- Implement: a semaphore `Pool` whose `Acquire` selects against `ctx.Done()`; `Record` with a mutex, a version counter, and an update counter; two update paths that both honor the hierarchy — the second by releasing the record lock before blocking on the pool and revalidating the version after reacquiring.
- Test: a deterministic reproduction of the cross-type wedge using the broken ordering; a 20-worker, 500-iteration stress mix of both fixed paths under `-race` with a watchdog; exact update-count accounting.
- Verify: `go test -count=1 -race ./...`

### A deadlock with only one mutex in it

The system: workers update records through a bounded pool of K database
connections. Two code paths grow naturally in such a codebase:

- Path A, the bulk updater: acquire a pool slot (it will need the connection
  for the whole operation), lock the record, write, unlock, release.
- Path B, the read-modify-write: lock the record to read its current state,
  compute, and *then* — still holding the record lock, "so nothing changes
  under me" — acquire a pool slot to write the result back.

Each path is locally reasonable. Together they wedge: suppose all K slots are
held by A-style workers, each blocked on a record lock; each of those record
locks is held by a B-style worker; each B-style worker is blocked waiting for
a pool slot. Wait-for edges: A-workers wait on record mutexes, B-workers wait
on the pool. That is a cycle — through a *channel*, not a second mutex. The
Coffman conditions do not mention mutexes; any resource with mutual exclusion
and blocking acquisition participates, and a buffered-channel semaphore is
exactly that. This is why a lock hierarchy documented as "lock A before lock
B" is incomplete: the hierarchy must rank *every blocking resource* — pools,
semaphores, bounded queues, channels — or it has holes.

The fix here is the documented rule *pool slot before any record lock*, plus
restructuring path B, which cannot naively comply (it wants the record's state
before deciding to use a connection). The restructure is the standard one:
read what you need under the record lock, *release it*, block on the pool with
nothing held, then reacquire the record lock to commit. Releasing means the
record can change while you wait for a connection, so the record carries a
version counter and the commit revalidates: if the version moved, drop the
slot and recompute. That is optimistic concurrency — the same
compare-and-retry shape used against databases with row versions — and it is
the honest price of never blocking on one resource while holding another.
Note what path B must *not* do instead: hold the record lock with a "brief"
pool wait and a comment promising it is fine. Under full load, K "brief"
waits are forever.

`Pool.Acquire` selects against `ctx.Done()`, for the same reason as exercise
4: a bounded resource under a wedge (or just under overload) should convert
waiters into deadline errors, not goroutine pileups.

Create `batchpool.go`:

```go
// Package batchpool runs record updates through a bounded connection
// pool. Acquisition hierarchy, which spans BOTH blocking resource types
// in this package: a pool slot is always acquired before any record
// lock, and no goroutine ever blocks on the pool while holding a record
// lock. Violating this wedges the system with all slots held by workers
// waiting on records whose holders are waiting on slots.
package batchpool

import (
	"context"
	"sync"
)

// Pool is a bounded connection pool: a buffered-channel semaphore.
type Pool struct {
	slots chan struct{}
}

// NewPool returns a pool with size connection slots (minimum 1).
func NewPool(size int) *Pool {
	if size < 1 {
		size = 1
	}
	return &Pool{slots: make(chan struct{}, size)}
}

// Acquire takes a slot, blocking until one frees or ctx is done.
func (p *Pool) Acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case p.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release returns a slot to the pool.
func (p *Pool) Release() {
	<-p.slots
}

// Record is a row guarded by its own mutex. version supports the
// optimistic revalidation UpdateComputed needs after releasing and
// reacquiring the lock.
type Record struct {
	ID      int
	mu      sync.Mutex
	version int
	updates int
}

// NewRecord returns a record with the given id.
func NewRecord(id int) *Record {
	return &Record{ID: id}
}

// Updates returns how many committed updates the record has received.
func (r *Record) Updates() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updates
}

// UpdateDirect is path A: it needs the connection for the whole
// operation, so it follows the hierarchy directly — slot first, then
// the record lock.
func UpdateDirect(ctx context.Context, p *Pool, r *Record) error {
	if err := p.Acquire(ctx); err != nil {
		return err
	}
	defer p.Release()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.version++
	r.updates++
	return nil
}

// UpdateComputed is path B: it must read the record before using a
// connection. The broken shape holds the record lock across
// p.Acquire; this version releases the record lock before blocking on
// the pool, then revalidates the version after reacquiring — if the
// record changed while we waited for a connection, recompute.
func UpdateComputed(ctx context.Context, p *Pool, r *Record) error {
	for {
		r.mu.Lock()
		ver := r.version // the read the computation depends on
		r.mu.Unlock()

		// Hierarchy: nothing held while blocking on the pool.
		if err := p.Acquire(ctx); err != nil {
			return err
		}

		r.mu.Lock()
		if r.version != ver {
			// Record moved while we waited for a connection: our
			// computation is stale. Drop everything and retry.
			r.mu.Unlock()
			p.Release()
			continue
		}
		r.version++
		r.updates++
		r.mu.Unlock()
		p.Release()
		return nil
	}
}
```

### The runnable demo

Four workers push five updates each through a two-slot pool, alternating
paths. The counts land exactly because both paths commit exactly one update
per call, no matter how many optimistic retries path B needed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"

	"example.com/batchpool"
)

func main() {
	pool := batchpool.NewPool(2)
	records := []*batchpool.Record{batchpool.NewRecord(1), batchpool.NewRecord(2)}

	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := records[w%len(records)]
			for i := range 5 {
				var err error
				if i%2 == 0 {
					err = batchpool.UpdateDirect(context.Background(), pool, rec)
				} else {
					err = batchpool.UpdateComputed(context.Background(), pool, rec)
				}
				if err != nil {
					fmt.Println("update failed:", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	for _, r := range records {
		fmt.Printf("record %d updates: %d\n", r.ID, r.Updates())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
record 1 updates: 10
record 2 updates: 10
```

### Tests

`TestBrokenOrderingWedges` reproduces the outage in miniature, fully
sequenced with channels so it wedges on every run, not just unlucky ones: a
one-slot pool, an A-style goroutine that takes the slot and then wants the
record, and a broken B-style goroutine that takes the record and then blocks
sending into the full pool channel. A watchdog `select` asserts the pair is
still stuck 300 ms later — this test *passes* by observing the wedge, and the
two goroutines are deliberately abandoned (they hold only test-local
resources). `TestStressBothPathsComplete` is the fix's proof: 20 workers, a
two-slot pool, 500 iterations each, both paths mixed, wrapped in a watchdog
because a reintroduced ordering bug would hang rather than fail. The final
accounting is exact: every call that returned nil committed exactly one
update, so the counts must sum to 20 x 500.

Create `batchpool_test.go`:

```go
package batchpool

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestBrokenOrderingWedges demonstrates the cross-resource-type deadlock:
// path A holds the only pool slot and wants the record lock; a broken
// path B holds the record lock and waits for a pool slot. The cycle runs
// through a mutex AND a channel; neither alone is cyclic.
func TestBrokenOrderingWedges(t *testing.T) {
	t.Parallel()

	pool := NewPool(1)
	rec := NewRecord(1)

	slotHeld := make(chan struct{})
	recHeld := make(chan struct{})
	done := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() { // path A: slot -> record (follows the hierarchy)
		defer wg.Done()
		if err := pool.Acquire(context.Background()); err != nil {
			t.Errorf("acquire: %v", err)
			return
		}
		close(slotHeld)
		<-recHeld
		rec.mu.Lock() // blocks: B holds the record
		rec.mu.Unlock()
		pool.Release()
	}()

	go func() { // BROKEN path B: record -> slot (violates the hierarchy)
		defer wg.Done()
		<-slotHeld
		rec.mu.Lock()
		close(recHeld)
		pool.slots <- struct{}{} // blocks: A holds the only slot
		<-pool.slots
		rec.mu.Unlock()
	}()

	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
		t.Fatal("expected a wedge, but both goroutines completed; the broken ordering did not deadlock")
	case <-time.After(300 * time.Millisecond):
		// Wedged, as predicted. The two goroutines are abandoned; they
		// hold only this test's pool and record.
	}
}

// TestFixedPathsNoWedge runs the exact scenario that wedged above, but
// with the hierarchy-respecting UpdateComputed as path B.
func TestFixedPathsNoWedge(t *testing.T) {
	t.Parallel()

	pool := NewPool(1)
	rec := NewRecord(1)

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := UpdateDirect(context.Background(), pool, rec); err != nil {
			t.Errorf("UpdateDirect: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := UpdateComputed(context.Background(), pool, rec); err != nil {
			t.Errorf("UpdateComputed: %v", err)
		}
	}()
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("fixed paths wedged: hierarchy violation reintroduced")
	}
	if got := rec.Updates(); got != 2 {
		t.Fatalf("updates = %d, want 2", got)
	}
}

func TestStressBothPathsComplete(t *testing.T) {
	t.Parallel()

	pool := NewPool(2)
	records := []*Record{NewRecord(1), NewRecord(2), NewRecord(3), NewRecord(4)}

	const workers = 20
	const iterations = 500

	done := make(chan struct{})
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range iterations {
				rec := records[(w+i)%len(records)]
				var err error
				if (w+i)%2 == 0 {
					err = UpdateDirect(t.Context(), pool, rec)
				} else {
					err = UpdateComputed(t.Context(), pool, rec)
				}
				if err != nil {
					t.Errorf("worker %d iteration %d: %v", w, i, err)
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("stress run wedged: acquisition hierarchy violated somewhere")
	}

	total := 0
	for _, r := range records {
		total += r.Updates()
	}
	if want := workers * iterations; total != want {
		t.Fatalf("total updates = %d, want %d (every nil return must commit exactly once)", total, want)
	}
}

func TestAcquireHonorsContext(t *testing.T) {
	t.Parallel()

	pool := NewPool(1)
	if err := pool.Acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer pool.Release()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := pool.Acquire(ctx); err != context.DeadlineExceeded {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func ExampleUpdateDirect() {
	pool := NewPool(2)
	rec := NewRecord(7)
	if err := UpdateDirect(context.Background(), pool, rec); err != nil {
		fmt.Println("err:", err)
		return
	}
	fmt.Println(rec.Updates())
	// Output: 1
}
```

## Review

The transferable rule: a lock hierarchy is only as complete as its resource
list. Rank the pool alongside the mutexes ("slot before record"), and audit
every path for the forbidden shape — *blocking on resource X while holding
resource Y that ranks after X*. In this package the audit is short: both
update paths block on the pool only with nothing held, and both lock records
only after the slot is secured (path A) or without needing a slot at that
moment (path B's read). The version-check retry is not decoration; deleting it
keeps the system deadlock-free but silently commits stale computations, which
is the kind of corruption that passes every liveness test.

Watch for the disguises this bug wears in review: a channel send inside a
critical section, a `wg.Wait()` under a lock, an errgroup limit hit while
holding a row lock, a callback invoked under a lock that internally acquires
from a pool. All are the same edge in the wait-for graph. Confirm with
`go test -count=1 -race ./...` — and note how `TestBrokenOrderingWedges`
inverts the usual polarity: it passes by *observing* the wedge, which is only
safe because the wedge is sequenced deterministically with channels rather
than left to timing.

## Resources

- [Effective Go — channels as semaphores](https://go.dev/doc/effective_go#channels) — the buffered-channel pool this package builds on.
- [sync package — Mutex](https://pkg.go.dev/sync#Mutex) — the record lock; the hierarchy spans both primitives.
- [golang.org/x/sync/semaphore](https://pkg.go.dev/golang.org/x/sync/semaphore) — the weighted production alternative to a hand-rolled slot channel, same hierarchy rules apply.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-deadlock-watchdog-harness.md](09-deadlock-watchdog-harness.md)
