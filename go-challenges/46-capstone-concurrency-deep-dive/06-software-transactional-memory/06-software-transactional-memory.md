# 6. Software Transactional Memory

Software Transactional Memory (STM) lets goroutines modify shared state inside *transactions* that either commit atomically or abort and restart, instead of acquiring locks in the correct order. The hard parts are the commit protocol (lock ordering, read-set validation, write-skew prevention), the `Retry` primitive (block until a read TVar changes), and the `OrElse` composition combinator (try first; if it retries, try second). This lesson builds a complete, race-detector-clean STM library from scratch using Go generics and `sync/atomic`.

```text
stm/
  go.mod
  stm.go
  stm_test.go
  cmd/demo/main.go
```

## Concepts

### Optimistic Concurrency Control

Pessimistic locking holds mutexes during reads — safe but serializes concurrent readers and forces an acquisition order that can deadlock if two callers lock the same pair in different order. Optimistic concurrency control (OCC) lets every transaction execute on a *private snapshot*: reads pull committed values into a local read set; writes buffer their updates locally. At commit time the library validates that nothing in the read set was modified by a concurrent commit. If validation passes, it applies the buffered writes atomically. If it fails, the transaction restarts from scratch.

The trade-off: OCC wastes CPU on transactions that conflict. The benefit is composability — combining two atomic operations means composing two functions inside one `Atomically` call; there are no lock-order rules and no forgotten `defer mu.Unlock()`.

### Transactional Variables and the Read/Write Contract

A `TVar[T]` wraps a committed value plus a monotonically increasing `version uint64` (an `atomic.Uint64`). Each committed write increments the version. A `Tx` carries two untyped maps keyed on `*core` (the interior of a TVar):

- `reads map[*core]uint64` — the version at which each TVar was read.
- `writes map[*core]any` — the buffered value to apply on commit.

When `TVar.Read` is called it first checks `writes` (so a transaction sees its own writes). On a miss it takes the TVar's mutex to get a consistent `(value, version)` snapshot, then records the version in `reads`. If the same TVar is read twice in one transaction and the version changed between the two reads, a concurrent commit happened — the transaction panics with an internal `abortSignal` and restarts.

### Commit Protocol and Lock Ordering

Commit has five steps:

1. Collect the write set; sort its `*core` pointers by memory address (`slices.SortFunc` + `unsafe.Pointer`).
2. Acquire each mutex in address order. Any two transactions that write overlapping sets always acquire in the same order — no deadlock.
3. Validate: for every `*core` in the read set, check `c.version.Load() == tx.reads[c]`. TVars in the write set are already locked; TVars only in the read set are checked via an atomic load (safe because `atomic.Uint64` operations are sequentially consistent).
4. Apply writes: store the new value and call `c.version.Add(1)`. Signal waiting `Retry` callers by calling `signal()` on each registered `*waiter`.
5. Release all locks.

If step 3 fails, release all locks without writing, sleep with exponential back-off (capped at 32 ms), and restart.

### The Retry Primitive

`tx.Retry()` says "the transaction's precondition is not met; suspend until something changes." Implementation:

1. `tx.Retry()` panics with an internal `retrySignal`.
2. `Atomically`'s `recover` catches the signal.
3. `waitForChange` creates a `*waiter` (a `chan struct{}` wrapped in a `sync.Once` so it is closed exactly once). Under each read TVar's mutex it checks whether the version already changed; if not, it appends the waiter to the TVar's waiter list. Then it blocks on the channel.
4. When any subsequent commit writes to one of those TVars, it calls `w.signal()` which closes the channel, waking all blocked waiters.
5. `Atomically` restarts the transaction.

The `sync.Once` ensures that even if multiple TVars close the same waiter channel simultaneously, the channel is closed exactly once and never double-closed.

### Write Skew: The Anomaly STM Must Prevent

Write skew: two concurrent transactions each read an overlapping set and write to disjoint subsets. Both see consistent individual snapshots but together violate a global invariant.

Classic example: two accounts with `a + b >= 0`. T1 reads `(a=50, b=50)` and writes `a = -10`. T2 reads `(a=50, b=50)` and writes `b = -10`. Each transaction's individual write passes a naive "only lock what you write" check. Together they leave `a + b = -20`.

STM prevents this because **both variables are in the read set of each transaction**. T1's read set records `{b: ver_b}`. At T1's commit time, while T1 holds the lock on `a`, it validates `b.version == ver_b`. If T2 already committed to `b`, validation fails and T1 retries with the updated values.

### OrElse: Composable Choice

`OrElse(tx, first, second)` tries `first`; if it calls `Retry`, tries `second`; if both call `Retry`, blocks until any TVar read by either branch changes:

```go
_ = stm.Atomically(func(tx *stm.Tx) error {
	return stm.OrElse(tx,
		func(tx *stm.Tx) error { return dequeue(tx, q1, &item) },
		func(tx *stm.Tx) error { return dequeue(tx, q2, &item) },
	)
})
```

`OrElse` snapshots `tx.reads` and `tx.writes` before running `first`. If `first` retries it saves first's read set, restores the snapshot, runs `second`. If `second` also retries it merges both read sets back into `tx.reads` and re-panics with `retrySignal`, causing `Atomically` to block on the union.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/stm/cmd/demo
cd ~/go-exercises/stm
go mod init example.com/stm
```

This is a library; there is no `main`. Verification is `go test`.

### Exercise 1: TVar and the Transaction Context

Create `stm.go`:

```go
package stm

import (
	"cmp"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// waiter is a single-use notification token registered with one or more TVars.
// sync.Once ensures the channel is closed exactly once regardless of how many
// TVars signal it concurrently.
type waiter struct {
	once sync.Once
	ch   chan struct{}
}

func newWaiter() *waiter { return &waiter{ch: make(chan struct{})} }

func (w *waiter) signal() { w.once.Do(func() { close(w.ch) }) }
func (w *waiter) wait()   { <-w.ch }

// core is the lock-protected interior shared by all *TVar[T] instances.
// Storing *core in untyped maps lets Tx work across different type parameters.
type core struct {
	mu      sync.Mutex    // guards value and waiters during commit
	version atomic.Uint64 // bumped on every committed write
	value   any           // most recently committed value
	waiters []*waiter     // goroutines blocked in Retry
}

// TVar is a transactional variable of type T.
// Do not copy a TVar after first use; pass by pointer.
type TVar[T any] struct {
	c core
}

// NewTVar creates a TVar with the given initial value.
func NewTVar[T any](v T) *TVar[T] {
	t := &TVar[T]{}
	t.c.value = v
	return t
}

// Read returns the value of v as seen by tx.
//
// If v was written within tx, the buffered value is returned.
// Otherwise the committed (value, version) pair is read under c.mu,
// and the version is recorded in tx.reads.  If the same TVar is read
// twice and the version changed between the two reads, a concurrent
// commit occurred; the transaction panics with abortSignal to restart.
func (v *TVar[T]) Read(tx *Tx) T {
	if w, ok := tx.writes[&v.c]; ok {
		return w.(T)
	}
	v.c.mu.Lock()
	val := v.c.value.(T)
	ver := v.c.version.Load()
	v.c.mu.Unlock()

	if prev, seen := tx.reads[&v.c]; seen && prev != ver {
		// Concurrent commit between two reads of this variable.
		panic(abortSignal{})
	}
	tx.reads[&v.c] = ver
	return val
}

// Write buffers value for v within tx.
// The write is not visible to other goroutines until tx commits.
func (v *TVar[T]) Write(tx *Tx, value T) {
	tx.writes[&v.c] = value
}

// Tx is the transaction context.  Do not copy; always pass by pointer.
// A Tx must not be used concurrently from multiple goroutines.
type Tx struct {
	reads  map[*core]uint64 // version recorded at each read
	writes map[*core]any    // buffered values to apply on commit
}

// Retry aborts the current transaction and suspends the goroutine until
// at least one TVar in the read set has been written by another committed
// transaction, then re-executes from scratch.
//
// Retry must be called from within the function passed to Atomically.
// Calling Retry outside Atomically panics the goroutine.
func (tx *Tx) Retry() { panic(retrySignal{}) }

// Internal sentinel types; never exported.
type retrySignal struct{}
type abortSignal struct{}

// sortedCores returns the write-set cores sorted by address for
// deadlock-free lock acquisition: any two concurrent transactions that
// write overlapping sets always acquire their locks in the same order.
func sortedCores(writes map[*core]any) []*core {
	cs := make([]*core, 0, len(writes))
	for c := range writes {
		cs = append(cs, c)
	}
	slices.SortFunc(cs, func(a, b *core) int {
		return cmp.Compare(
			uintptr(unsafe.Pointer(a)),
			uintptr(unsafe.Pointer(b)),
		)
	})
	return cs
}
```

`core` is the untyped, lockable interior; `TVar[T]` is the thin typed wrapper. The two maps in `Tx` key on `*core` so they can store entries for `*TVar[int]`, `*TVar[string]`, etc., without a generic constraint on the map itself.

### Exercise 2: The Atomically Commit Loop

Append to `stm.go`:

```go
// Atomically executes fn as an atomic transaction.
//
// fn receives a *Tx and may call TVar.Read and TVar.Write through it.
// If fn returns a non-nil error, Atomically returns that error immediately
// without committing.  If read-set validation fails (a concurrent commit
// changed a read variable), fn is re-executed from scratch.  If fn calls
// tx.Retry(), Atomically blocks until at least one read TVar changes and
// then re-executes.  The backoff between retries is capped at 32 ms to
// prevent livelock under sustained contention.
func Atomically(fn func(*Tx) error) error {
	backoff := time.Millisecond
	for {
		tx := &Tx{
			reads:  make(map[*core]uint64),
			writes: make(map[*core]any),
		}
		var userErr error
		var doRetry bool
		func() {
			defer func() {
				r := recover()
				switch r.(type) {
				case retrySignal:
					doRetry = true
				case abortSignal:
					// validation failed mid-read; loop
				case nil:
					// fn returned normally
				default:
					panic(r) // re-panic unexpected panics
				}
			}()
			userErr = fn(tx)
		}()
		if userErr != nil {
			return userErr
		}
		if doRetry {
			waitForChange(tx.reads)
			backoff = time.Millisecond
			continue
		}
		if tryCommit(tx) {
			return nil
		}
		// Read-set validation failed at commit time.
		time.Sleep(backoff)
		if backoff < 32*time.Millisecond {
			backoff *= 2
		}
	}
}

// tryCommit attempts to atomically apply tx's writes.
// Returns true on success, false if read-set validation fails.
func tryCommit(tx *Tx) bool {
	cores := sortedCores(tx.writes)
	// Acquire write-set locks in address order.
	for _, c := range cores {
		c.mu.Lock()
	}
	defer func() {
		for _, c := range cores {
			c.mu.Unlock()
		}
	}()

	// Validate all read-set versions while holding write locks.
	// TVars in the write set are locked; others are checked atomically.
	for c, ver := range tx.reads {
		if c.version.Load() != ver {
			return false
		}
	}

	// Apply writes, bump versions, and wake Retry waiters.
	for _, c := range cores {
		c.value = tx.writes[c]
		c.version.Add(1)
		waiters := c.waiters
		c.waiters = nil
		for _, w := range waiters {
			w.signal()
		}
	}
	return true
}

// waitForChange blocks until at least one TVar in reads has been written.
// For each TVar it registers a shared *waiter under the TVar's mutex,
// checking first whether the version already advanced (early return).
func waitForChange(reads map[*core]uint64) {
	if len(reads) == 0 {
		time.Sleep(time.Millisecond)
		return
	}
	w := newWaiter()
	for c, ver := range reads {
		c.mu.Lock()
		if c.version.Load() != ver {
			// Already changed while we were registering.
			c.mu.Unlock()
			return
		}
		c.waiters = append(c.waiters, w)
		c.mu.Unlock()
	}
	w.wait()
}

// OrElse tries first within tx; if first calls Retry, tries second.
// If both call Retry, OrElse merges their read sets and re-panics with
// retrySignal so Atomically blocks on the union of reads.
//
// OrElse must be called from within a function passed to Atomically.
func OrElse(tx *Tx, first, second func(*Tx) error) error {
	savedReads := cloneReads(tx.reads)
	savedWrites := cloneWrites(tx.writes)

	firstReads, firstRetried := runBranch(tx, first)
	if !firstRetried {
		return nil
	}

	// first retried; restore snapshot and run second.
	tx.reads = savedReads
	tx.writes = savedWrites

	_, secondRetried := runBranch(tx, second)
	if !secondRetried {
		return nil
	}

	// Both retried: merge read sets and re-panic so Atomically
	// blocks on the union of both branches' read sets.
	for c, ver := range firstReads {
		tx.reads[c] = ver
	}
	panic(retrySignal{})
}

// runBranch executes fn(tx) catching only retrySignal.
// Other panics (abortSignal, unexpected) are re-panicked.
// Returns (reads at retry time, retried).
func runBranch(tx *Tx, fn func(*Tx) error) (map[*core]uint64, bool) {
	var snapshotReads map[*core]uint64
	var retried bool
	var panicVal any
	func() {
		defer func() {
			r := recover()
			switch r.(type) {
			case retrySignal:
				snapshotReads = cloneReads(tx.reads)
				retried = true
			case nil:
				// normal return
			default:
				panicVal = r // abortSignal or unexpected; re-panic below
			}
		}()
		_ = fn(tx)
	}()
	if panicVal != nil {
		panic(panicVal)
	}
	return snapshotReads, retried
}

func cloneReads(r map[*core]uint64) map[*core]uint64 {
	c := make(map[*core]uint64, len(r))
	for k, v := range r {
		c[k] = v
	}
	return c
}

func cloneWrites(w map[*core]any) map[*core]any {
	c := make(map[*core]any, len(w))
	for k, v := range w {
		c[k] = v
	}
	return c
}
```

The deferred unlock in `tryCommit` runs before the return, so all cores are always unlocked even if the validation loop returns early. The `runBranch` helper re-panics anything other than `retrySignal` so that `abortSignal` and unexpected panics propagate correctly to `Atomically`.

### Exercise 3: Tests, Example, and Demo

Create `stm_test.go`:

```go
package stm

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ExampleNewTVar is auto-verified by go test.
func ExampleNewTVar() {
	v := NewTVar(7)
	var got int
	_ = Atomically(func(tx *Tx) error {
		got = v.Read(tx)
		return nil
	})
	fmt.Println(got)
	// Output:
	// 7
}

// TestReadAfterWrite checks that a write is visible within the same transaction.
func TestReadAfterWrite(t *testing.T) {
	t.Parallel()

	v := NewTVar(0)
	err := Atomically(func(tx *Tx) error {
		v.Write(tx, 42)
		if got := v.Read(tx); got != 42 {
			t.Errorf("read after write = %d, want 42", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestCommitMakesValueVisible checks that a committed write is seen by a
// subsequent independent transaction.
func TestCommitMakesValueVisible(t *testing.T) {
	t.Parallel()

	v := NewTVar(10)
	if err := Atomically(func(tx *Tx) error {
		v.Write(tx, 20)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var got int
	if err := Atomically(func(tx *Tx) error {
		got = v.Read(tx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got != 20 {
		t.Fatalf("got %d, want 20", got)
	}
}

// TestUserErrorPropagated verifies that fn's returned error surfaces from
// Atomically and can be checked with errors.Is.
func TestUserErrorPropagated(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("insufficient funds")
	v := NewTVar(0)
	err := Atomically(func(tx *Tx) error {
		if v.Read(tx) == 0 {
			return fmt.Errorf("withdraw: %w", sentinel)
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapping %v", err, sentinel)
	}
}

// TestBankTransferPreservesTotal runs many concurrent transfers across
// multiple accounts and verifies the total balance is unchanged.
func TestBankTransferPreservesTotal(t *testing.T) {
	t.Parallel()

	const (
		accounts   = 20
		goroutines = 8
		transfers  = 300
	)
	vars := make([]*TVar[int], accounts)
	for i := range vars {
		vars[i] = NewTVar(100)
	}

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for k := range transfers {
				from := (seed + k) % accounts
				to := (seed + k + 1) % accounts
				amount := (k % 10) + 1
				_ = Atomically(func(tx *Tx) error {
					a := vars[from].Read(tx)
					b := vars[to].Read(tx)
					if a < amount {
						return nil // insufficient funds; skip
					}
					vars[from].Write(tx, a-amount)
					vars[to].Write(tx, b+amount)
					return nil
				})
			}
		}(g * 31)
	}
	wg.Wait()

	total := 0
	_ = Atomically(func(tx *Tx) error {
		total = 0
		for _, v := range vars {
			total += v.Read(tx)
		}
		return nil
	})
	want := accounts * 100
	if total != want {
		t.Fatalf("total = %d, want %d (balance not preserved)", total, want)
	}
}

// TestRetryBlocksUntilChange verifies that a transaction calling Retry
// suspends until a producer commits a matching value.
func TestRetryBlocksUntilChange(t *testing.T) {
	t.Parallel()

	v := NewTVar(0)
	done := make(chan int, 1)

	go func() {
		var result int
		_ = Atomically(func(tx *Tx) error {
			val := v.Read(tx)
			if val == 0 {
				tx.Retry()
			}
			result = val
			return nil
		})
		done <- result
	}()

	// Give the consumer goroutine time to block in Retry before we produce.
	time.Sleep(20 * time.Millisecond)
	_ = Atomically(func(tx *Tx) error {
		v.Write(tx, 42)
		return nil
	})

	select {
	case got := <-done:
		if got != 42 {
			t.Fatalf("Retry woke with %d, want 42", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Retry did not unblock within 2s")
	}
}

// TestOrElseFallback checks that OrElse falls back to the second branch
// when the first calls Retry.
func TestOrElseFallback(t *testing.T) {
	t.Parallel()

	empty := NewTVar(0)
	full := NewTVar(99)
	var got int

	err := Atomically(func(tx *Tx) error {
		return OrElse(tx,
			func(tx *Tx) error {
				if empty.Read(tx) == 0 {
					tx.Retry()
				}
				got = empty.Read(tx)
				return nil
			},
			func(tx *Tx) error {
				got = full.Read(tx)
				return nil
			},
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != 99 {
		t.Fatalf("OrElse got %d, want 99", got)
	}
}

// TestWriteSkewPrevented verifies that overlapping read sets are validated
// at commit time, preventing write skew.  Two sets of goroutines each read
// both accounts and withdraw from one; if write skew were allowed the sum
// could go negative.
func TestWriteSkewPrevented(t *testing.T) {
	t.Parallel()

	// Invariant: a + b >= 0.
	a := NewTVar(50)
	b := NewTVar(50)

	const (
		workers = 16
		iters   = 100
	)
	var wg sync.WaitGroup
	var aborts atomic.Int64

	for i := range workers {
		wg.Add(1)
		go func(withdrawA bool) {
			defer wg.Done()
			for range iters {
				var tries int
				_ = Atomically(func(tx *Tx) error {
					tries++
					av := a.Read(tx)
					bv := b.Read(tx)
					if av+bv < 10 {
						return nil // invariant would be violated; skip
					}
					if withdrawA {
						a.Write(tx, av-5)
					} else {
						b.Write(tx, bv-5)
					}
					return nil
				})
				aborts.Add(int64(tries - 1))
			}
		}(i%2 == 0)
	}
	wg.Wait()

	var total int
	_ = Atomically(func(tx *Tx) error {
		total = a.Read(tx) + b.Read(tx)
		return nil
	})
	if total < 0 {
		t.Fatalf("write skew allowed: a+b = %d (must be >= 0), aborts = %d",
			total, aborts.Load())
	}
	t.Logf("final a+b = %d, aborts = %d", total, aborts.Load())
}
```

Your turn: add `TestOrElseBothRetryBlocks` that starts a consumer goroutine calling `OrElse` with two branches that both call `Retry` on an empty TVar, then verifies that the consumer unblocks after a producer writes to that TVar.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/stm"
)

func main() {
	// Demonstrate a producer-consumer using Retry.
	// The consumer blocks until the producer commits a non-zero value.
	queue := stm.NewTVar(0)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var item int
		_ = stm.Atomically(func(tx *stm.Tx) error {
			v := queue.Read(tx)
			if v == 0 {
				tx.Retry() // block until queue is non-empty
			}
			item = v
			queue.Write(tx, 0) // consume: reset to empty
			return nil
		})
		fmt.Printf("consumed: %d\n", item)
	}()

	_ = stm.Atomically(func(tx *stm.Tx) error {
		queue.Write(tx, 42)
		return nil
	})
	wg.Wait()
	fmt.Println("done")
}
```

Run the demo with `go run ./cmd/demo`.

## Common Mistakes

### Calling Retry Outside Atomically

Wrong: `tx.Retry()` called from a goroutine not running inside `Atomically`. The `retrySignal` panic propagates without a `recover` and crashes the program.

Fix: `Retry` is only valid inside the function passed to `Atomically`. If you need to wait for a condition, restructure the code so the wait is inside the transaction.

### Side Effects Inside Transaction Functions

Wrong: printing, writing to a file, or incrementing a counter outside a TVar inside the transaction function. Transactions re-execute on conflict; the side effect runs multiple times.

Fix: keep transaction functions pure (only TVar reads and writes). Perform side effects after `Atomically` returns using the values read or written during the committed transaction.

### Skipping the Read on the Written Variable (Missed Write Skew)

Wrong: a transaction reads `a` and `b` to check an invariant but only writes to `a`. Because `b` is only checked, not written, a naive implementation might not include `b` in the commit validation.

Fix: this implementation records ALL reads in `tx.reads`, not just the written ones. Both `a` and `b` are validated at commit time. Any other design that only validates the write set is vulnerable to write skew.

### Deadlock from Unsorted Lock Acquisition

Wrong: acquiring write-set mutexes in an arbitrary or insertion order. Transaction T1 locks `a` then `b`; transaction T2 locks `b` then `a`; both block waiting for the other.

Fix: `sortedCores` sorts all write-set `*core` pointers by memory address. Any two transactions competing for the same pair always acquire in the same order. This is why `unsafe.Pointer` is used: the address is the canonical tie-breaker.

### Waiter Channel Accumulation

Wrong: in a TVar that is rarely written, `c.waiters` grows without bound as transactions repeatedly call `Retry` and register waiters that are never signaled.

Fix (lesson): when a TVar is committed, `c.waiters = nil` clears the list. Waiters from transactions that have already woken (the `sync.Once` fired) are naturally cleaned up on the next write. For a production system, periodically sweep dead waiters or use a generation counter paired with a shared condition variable.

## Verification

From `~/go-exercises/stm`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./cmd/demo
go test -count=1 -race ./...
```

All four must pass. The race detector is mandatory: STM's correctness relies on precise memory ordering and the race detector validates that atomic operations are used where needed.

## Summary

- Optimistic concurrency control reads speculatively and validates at commit time; wasted work on conflicts is the cost of lock-free composition.
- `TVar[T]` wraps a value with a `version atomic.Uint64`; `Tx` carries a read set (versions at read time) and a write set (buffered values).
- The commit protocol sorts write-set pointers by address (deadlock-free), validates the read set atomically, then applies writes and signals waiters.
- `Retry` panics with a sentinel, suspending the transaction until a read TVar changes; `sync.Once` ensures the waiter channel is closed exactly once even when multiple TVars commit concurrently.
- Write-skew safety comes from validating the full read set at commit time, not just the written variables.
- `OrElse` composes two `Retry`-capable branches by snapshotting and restoring `tx.reads/writes`, merging read sets if both retry.
- Transaction functions must be free of side effects; they may re-execute an arbitrary number of times before committing.

## What's Next

Next: [Concurrent B-Tree](../07-concurrent-btree/07-concurrent-btree.md).

## Resources

- Tim Harris, Simon Marlow, Simon Peyton Jones, Maurice Herlihy, "Composable Memory Transactions", PPoPP 2005 — foundational paper defining Retry and OrElse: https://www.microsoft.com/en-us/research/publication/composable-memory-transactions/
- `sync/atomic` package, `atomic.Uint64` type and methods: https://pkg.go.dev/sync/atomic#Uint64
- lukechampine/stm — reference Go STM implementation (non-generic): https://pkg.go.dev/github.com/lukechampine/stm
- `slices.SortFunc` and `cmp.Compare` (Go 1.21): https://pkg.go.dev/slices#SortFunc and https://pkg.go.dev/cmp#Compare
- Go Memory Model, "sync/atomic" happens-before rules: https://go.dev/ref/mem#atomic
