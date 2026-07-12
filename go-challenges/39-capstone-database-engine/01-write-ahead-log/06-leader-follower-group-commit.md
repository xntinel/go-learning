# Exercise 6: Leader/Follower Group Commit with a Measured Amortization

The WAL core batches appends on a fixed timer; this exercise builds the other classic group-commit design, the timer-free leader/follower coalescer, in isolation. Here the flush of one batch defines the window for the next: commits that arrive while a flush is in flight pile into the following batch and are flushed together. Building it as a standalone component, with the flush function injected, lets a test pause a flush mid-call and assert the amortization deterministically — `N` commits cost a countable number of flushes, not `N` of them.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
groupcommit.go        GroupCommitter, NewGroupCommitter, Commit, Flushes, Close, FlushFunc
cmd/
  demo/
    main.go           run sequential commits and report flushes == commits
groupcommit_test.go   parked-leader coalescing (N commits -> 2 flushes) + closed behavior
```

- Files: `groupcommit.go`, `cmd/demo/main.go`, `groupcommit_test.go`.
- Implement: `FlushFunc`, `GroupCommitter`, `NewGroupCommitter(flush FlushFunc) *GroupCommitter`, `(*GroupCommitter).Commit`, `(*GroupCommitter).Flushes`, `(*GroupCommitter).Close`, and `ErrCommitterClosed`.
- Test: `groupcommit_test.go` parks the first flush so the remaining commits coalesce, asserts exactly two flushes serve twenty commits, and that `Commit` after `Close` returns `ErrCommitterClosed`.
- Verify: `go test -run 'TestGroupCommitter|ExampleGroupCommitter' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/01-write-ahead-log/06-leader-follower-group-commit/cmd/demo && cd go-solutions/39-capstone-database-engine/01-write-ahead-log/06-leader-follower-group-commit
```

### Why a second design, and why in isolation

The timer design in the WAL core is simple and bounds latency at one tick, but it has a structural flaw: when the queue is already full it still waits out the remainder of the tick before flushing, spending latency it did not need to. The leader/follower design removes the timer entirely. The first commit to arrive becomes the leader and triggers a flush; every commit that arrives while that flush is in progress cannot be served yet, so it accumulates, and when the flush returns the whole accumulated set is flushed together as the next batch. The window is therefore self-tuning and equal to exactly how long the previous flush took: a slow device produces large batches (more coalescing, higher throughput), a fast device produces small ones (less added latency), with no tunable to misconfigure.

Building it as a standalone `GroupCommitter` rather than wiring it into the WAL is a deliberate testability choice. The flush function is injected (`FlushFunc`), so a test can substitute a flush it controls — one it can park mid-call — and then *count* flushes against commits. That turns the amortization claim from a vague "it batches" into a hard assertion: park the leader, fire nineteen more commits, release, and prove the total was two flushes. You cannot make that assertion against a real fsync because you cannot pause one on command.

Create `groupcommit.go`:

```go
package groupcommit

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// ErrCommitterClosed is returned by Commit after the GroupCommitter is closed.
var ErrCommitterClosed = errors.New("groupcommit: committer closed")

// FlushFunc persists everything written so far and reports whether the durable
// write succeeded. A GroupCommitter calls it exactly once per coalesced batch;
// in a real WAL it is the fsync.
type FlushFunc func() error

// GroupCommitter coalesces concurrent Commit calls into batches and invokes the
// flush function once per batch, amortizing its cost across every caller in that
// batch. It is a self-contained, leader/follower model of the WAL's group-commit
// path: while one batch is being flushed, commits that arrive pile into the next
// batch and are flushed together when the current flush returns.
type GroupCommitter struct {
	flush FlushFunc

	mu      sync.Mutex
	cond    *sync.Cond
	pending []chan error
	closed  bool
	flushes uint64

	wg sync.WaitGroup
}

// NewGroupCommitter starts a committer whose flusher goroutine calls flush once
// per coalesced batch. Call Close to stop it.
func NewGroupCommitter(flush FlushFunc) *GroupCommitter {
	g := &GroupCommitter{flush: flush}
	g.cond = sync.NewCond(&g.mu)
	g.wg.Add(1)
	go g.loop()
	return g
}

// Commit enqueues the caller and blocks until the batch containing it has been
// flushed. It returns the flush error, or ErrCommitterClosed if the committer is
// already closed.
func (g *GroupCommitter) Commit() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return fmt.Errorf("groupcommit: commit: %w", ErrCommitterClosed)
	}
	ch := make(chan error, 1)
	g.pending = append(g.pending, ch)
	g.cond.Signal()
	g.mu.Unlock()
	return <-ch
}

// Flushes reports how many times the flush function has been called. Comparing
// it against the number of commits measures the fsync amortization.
func (g *GroupCommitter) Flushes() uint64 {
	return atomic.LoadUint64(&g.flushes)
}

// Close stops the flusher after draining any queued commits. Subsequent Commit
// calls return ErrCommitterClosed.
func (g *GroupCommitter) Close() error {
	g.mu.Lock()
	g.closed = true
	g.cond.Signal()
	g.mu.Unlock()
	g.wg.Wait()
	return nil
}

func (g *GroupCommitter) loop() {
	defer g.wg.Done()
	for {
		g.mu.Lock()
		for len(g.pending) == 0 && !g.closed {
			g.cond.Wait()
		}
		if len(g.pending) == 0 && g.closed {
			g.mu.Unlock()
			return
		}
		batch := g.pending
		g.pending = nil
		g.mu.Unlock()

		err := g.flush()
		atomic.AddUint64(&g.flushes, 1)
		for _, ch := range batch {
			ch <- err
		}
	}
}
```

The coalescing emerges from one structural fact: `g.flush()` is called *outside* the lock, after the loop has snapshotted `g.pending` into `batch` and reset it to nil. While that flush runs, any `Commit` is free to take the lock, append its channel to a now-empty `g.pending`, signal, and block on its result channel. Those commits cannot be drained until the flusher finishes its current flush and loops back to grab the lock again — at which point they are all sitting in `g.pending` as one batch and get flushed with a single call. There is no timer and no batch-size parameter; the size of each batch is simply however many commits arrived during the previous flush. The leader is whichever commit the flusher happens to pick up first; the followers are everyone who arrived during its flush.

The `sync.Cond` is the right primitive here because the flusher needs to sleep when there is nothing to do and wake the instant work arrives, and a condition variable does exactly that without a polling interval. The `for` loop around `cond.Wait()` is mandatory, not stylistic: `Wait` can return without a matching `Signal` (spurious or coalesced wakeups), so the predicate `len(g.pending) == 0 && !g.closed` must be rechecked on every wakeup. `Close` sets `closed`, signals once to wake a parked flusher, and waits for the goroutine to drain whatever is already queued before returning; commits that arrive after `closed` is set are rejected up front with `ErrCommitterClosed`. `Flushes` uses an atomic so a test can read the counter without racing the flusher's increment — the whole point is to read it concurrently and assert on it.

### The runnable demo

Coalescing only happens under concurrency: when commits overlap a flush. A sequential driver is the opposite case and makes the baseline visible — every commit blocks until its own flush returns, so each is its own batch and flushes equal commits exactly. The demo runs that deterministic baseline; the coalescing claim is proven in the test, which can pause a flush mid-call.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/group-commit"
)

func main() {
	g := groupcommit.NewGroupCommitter(func() error { return nil })
	defer g.Close()

	const n = 5
	for i := 0; i < n; i++ {
		if err := g.Commit(); err != nil {
			fmt.Println("commit error:", err)
			return
		}
	}
	// With no overlap, each commit is its own batch: flushes == commits.
	fmt.Printf("sequential commits=%d flushes=%d\n", n, g.Flushes())
	fmt.Println("under concurrency, commits that overlap a flush coalesce into one")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sequential commits=5 flushes=5
under concurrency, commits that overlap a flush coalesce into one
```

### Tests

The coalescing test is the centerpiece and earns its complexity. It installs a flush that parks the very first call (via `sync.Once`) on a release channel, so the leader is frozen mid-flush. With the leader parked, it fires the remaining nineteen commits; they cannot be drained, so they all land in `g.pending` together. `waitPending` spins until all nineteen are queued, then the test releases the leader, and the result must be exactly two flushes: one for the parked leader, one for the coalesced batch of nineteen. The closed test and the example pin the lifecycle and the single-commit case.

Create `groupcommit_test.go`:

```go
package groupcommit

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
)

func TestGroupCommitterCoalesces(t *testing.T) {
	t.Parallel()

	var once sync.Once
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	flush := func() error {
		// Park only the first flush so later commits accumulate into one batch.
		once.Do(func() {
			entered <- struct{}{}
			<-release
		})
		return nil
	}

	g := NewGroupCommitter(flush)
	defer g.Close()

	const n = 20
	done := make(chan error, n)

	// The first commit triggers the first flush, which then parks.
	go func() { done <- g.Commit() }()
	<-entered

	// Enqueue the rest while the first flush is parked: they cannot be drained
	// until the parked flush returns, so they all land in the next batch.
	for i := 0; i < n-1; i++ {
		go func() { done <- g.Commit() }()
	}
	waitPending(g, n-1)

	close(release)

	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}

	// Exactly two flushes for n commits: the parked leader plus one coalesced
	// batch of the remaining n-1.
	if got := g.Flushes(); got != 2 {
		t.Fatalf("flushes = %d, want 2 for %d commits", got, n)
	}
}

func TestGroupCommitterClosed(t *testing.T) {
	t.Parallel()

	g := NewGroupCommitter(func() error { return nil })
	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := g.Commit(); !errors.Is(err, ErrCommitterClosed) {
		t.Fatalf("err = %v, want ErrCommitterClosed", err)
	}
}

func ExampleGroupCommitter() {
	g := NewGroupCommitter(func() error { return nil })
	defer g.Close()

	_ = g.Commit()
	fmt.Println(g.Flushes())
	// Output:
	// 1
}

// waitPending blocks until at least target commits are queued in g.
func waitPending(g *GroupCommitter, target int) {
	for {
		g.mu.Lock()
		n := len(g.pending)
		g.mu.Unlock()
		if n >= target {
			return
		}
		runtime.Gosched()
	}
}
```

## Review

The coalescer is correct when it batches and fails as one unit. With the leader parked, twenty commits must resolve in exactly two flushes — the parked leader plus one coalesced batch of nineteen — while sequential commits flush one-for-one because nothing overlaps a flush. Confirm that `Commit` after `Close` returns an error satisfying `errors.Is(err, ErrCommitterClosed)`, that queued commits are drained before `Close` returns, and that `Flushes()` read concurrently with the flusher stays clean under `go test -race ./...`.

Common mistakes for this feature. Calling `flush()` while holding the lock serializes everything and destroys the coalescing — the flush must run with the lock released so followers can queue. Using a plain `if` instead of a `for` around `cond.Wait()` lets a spurious wakeup proceed with an empty batch and flush nothing (or panic), so the predicate must be rechecked on every wake. Sending the flush result to only the leader's channel, not every channel in the batch, half-acknowledges the followers — a durability lie, because a failed fsync must fail every waiter in the batch. And reading `flushes` with a non-atomic load races the flusher's increment under `-race`.

## Resources

- [PostgreSQL: WAL parameters (commit_delay, commit_siblings)](https://www.postgresql.org/docs/current/runtime-config-wal.html) — the tunables behind Postgres's group-commit delay.
- [PostgreSQL: Write-Ahead Logging (WAL)](https://www.postgresql.org/docs/current/wal-intro.html) — "a single `fsync` ... may commit multiple concurrent transactions," the amortization this exercise measures.
- [`sync` package](https://pkg.go.dev/sync) — `sync.Cond`, the condition variable that wakes the flusher the instant a commit arrives.

---

Back to [05-checkpoint-payloads-and-segment-gc.md](05-checkpoint-payloads-and-segment-gc.md) | Next: [07-torn-write-detection.md](07-torn-write-detection.md)
