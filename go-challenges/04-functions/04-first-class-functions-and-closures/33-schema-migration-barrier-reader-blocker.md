# Exercise 33: Schema Migration Barrier Blocking Readers Until Phase Complete

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A live, multi-phase schema migration — add a nullable column, dual-write to
old and new, backfill, then read from the new column — has an intermediate
window where reading the new column would see incomplete data. A request
handler that starts mid-migration must not proceed until the migration has
reached a phase declared safe to read from. `NewBarrier` closes over a phase
counter and a broadcast channel; `waitUntilSafe` blocks any number of reader
goroutines until `advance` moves the phase far enough, then releases all of
them at once.

## What you'll build

```text
schema-migration-barrier/    independent module: example.com/schema-migration-barrier
  go.mod                      go 1.24
  migbarrier.go                NewBarrier returns advance/waitUntilSafe closures
  cmd/
    demo/
      main.go                   three readers block through two phases, then release
  migbarrier_test.go            table test: blocks then releases, already-safe, isolation, fan-out
```

- Files: `migbarrier.go`, `cmd/demo/main.go`, `migbarrier_test.go`.
- Implement: `NewBarrier(safePhase int) (advance func(phase int), waitUntilSafe func())`, closing over a mutex-guarded phase counter and a channel that is replaced and closed on every `advance`.
- Test: a reader blocks through an intermediate phase and only unblocks once the phase reaches `safePhase`; a reader started after the barrier is already safe returns immediately; two barriers never share phase state; a single `advance` releases fifty blocked readers at once under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/schema-migration-barrier/cmd/demo
cd ~/go-exercises/schema-migration-barrier
go mod init example.com/schema-migration-barrier
go mod edit -go=1.24
```

### A channel that gets replaced, not a condition variable

`NewBarrier` captures `phase`, a mutex, and `wake`, a `chan struct{}` used
purely as a broadcast signal — nothing is ever sent on it, it is only
closed. `waitUntilSafe` loops: under the lock it checks whether `phase` has
reached `safePhase`; if so it returns immediately, and if not it captures
the *current* `wake` channel and then, outside the lock, blocks receiving
from it. `advance` locks, updates `phase`, swaps in a brand-new `wake`
channel, unlocks, and closes the *old* one. Closing a channel wakes every
goroutine currently receiving from it — that is what lets one `advance`
release an arbitrary number of blocked readers without needing to track how
many there are, the same technique `context.Context.Done()` uses to cancel
every goroutine watching a context at once.

The loop in `waitUntilSafe` (rather than a single wait) matters because one
`advance` might still land below `safePhase` — a reader that wakes up must
re-check the condition and go back to waiting on the *new* current channel,
not assume the one wakeup meant it is now safe. This is the same
check-under-lock, wait-outside-lock discipline every condition-variable
pattern needs, expressed with a plain channel instead of `sync.Cond`.

Create `migbarrier.go`:

```go
// Package migbarrier gates readers behind a live schema migration's phase,
// so a reader started mid-migration blocks until the schema reaches a phase
// that is safe to read from, instead of racing an in-progress DDL change.
package migbarrier

import "sync"

// NewBarrier returns advance and waitUntilSafe closures sharing a private
// phase counter, a mutex, and a "wake" channel that is replaced and closed
// every time the phase changes.
//
//   - advance moves the migration to a new phase and wakes every reader
//     currently blocked in waitUntilSafe, by closing the current wake
//     channel and installing a fresh one.
//   - waitUntilSafe blocks the calling goroutine until the migration has
//     reached safePhase. It re-checks the phase in a loop after each wakeup,
//     since a single advance might still land below safePhase.
//
// Closing a channel wakes every goroutine blocked receiving on it, which is
// what lets one advance release an arbitrary number of waiting readers
// without advance needing to know how many there are.
func NewBarrier(safePhase int) (advance func(phase int), waitUntilSafe func()) {
	var mu sync.Mutex
	phase := 0
	wake := make(chan struct{})

	advance = func(p int) {
		mu.Lock()
		phase = p
		old := wake
		wake = make(chan struct{})
		mu.Unlock()
		close(old)
	}

	waitUntilSafe = func() {
		for {
			mu.Lock()
			if phase >= safePhase {
				mu.Unlock()
				return
			}
			ch := wake
			mu.Unlock()
			<-ch
		}
	}

	return advance, waitUntilSafe
}
```

### The runnable demo

Three readers block through phase 1 (dual-write, not yet safe) and are all
released together once phase 2 (backfilled) is reached.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/schema-migration-barrier"
)

func main() {
	// Phase 0: old column only. Phase 1: dual-write, old column still safe
	// to read. Phase 2: new column backfilled — safe for readers.
	advance, waitUntilSafe := migbarrier.NewBarrier(2)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var order []string

	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("reader-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			waitUntilSafe()
			mu.Lock()
			order = append(order, id)
			mu.Unlock()
		}()
	}

	fmt.Println("migration phase 0: readers blocked")
	advance(1)
	fmt.Println("migration phase 1 (dual-write): readers still blocked")
	advance(2)
	fmt.Println("migration phase 2 (backfilled): readers released")

	wg.Wait()
	fmt.Println("readers that proceeded:", len(order))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
migration phase 0: readers blocked
migration phase 1 (dual-write): readers still blocked
migration phase 2 (backfilled): readers released
readers that proceeded: 3
```

### Tests

Create `migbarrier_test.go`:

```go
package migbarrier

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitUntilSafeBlocksUntilPhaseReached(t *testing.T) {
	advance, waitUntilSafe := NewBarrier(2)

	unblocked := make(chan struct{})
	go func() {
		waitUntilSafe()
		close(unblocked)
	}()

	select {
	case <-unblocked:
		t.Fatal("waitUntilSafe returned at phase 0, want blocked")
	case <-time.After(50 * time.Millisecond):
		// still blocked, as expected
	}

	advance(1) // still below safePhase
	select {
	case <-unblocked:
		t.Fatal("waitUntilSafe returned at phase 1, want blocked until phase 2")
	case <-time.After(50 * time.Millisecond):
		// still blocked, as expected
	}

	advance(2) // now safe
	select {
	case <-unblocked:
		// released, as expected
	case <-time.After(2 * time.Second):
		t.Fatal("waitUntilSafe did not unblock after phase reached safePhase")
	}
}

func TestWaitUntilSafeReturnsImmediatelyIfAlreadySafe(t *testing.T) {
	advance, waitUntilSafe := NewBarrier(1)
	advance(1)

	done := make(chan struct{})
	go func() {
		waitUntilSafe()
		close(done)
	}()

	select {
	case <-done:
		// returned immediately, as expected
	case <-time.After(2 * time.Second):
		t.Fatal("waitUntilSafe blocked even though the barrier was already safe")
	}
}

func TestTwoBarriersDoNotShareState(t *testing.T) {
	advanceA, waitA := NewBarrier(1)
	_, waitB := NewBarrier(1)

	advanceA(1)

	doneA := make(chan struct{})
	go func() { waitA(); close(doneA) }()
	select {
	case <-doneA:
	case <-time.After(2 * time.Second):
		t.Fatal("barrier A did not release after its own advance")
	}

	doneB := make(chan struct{})
	go func() { waitB(); close(doneB) }()
	select {
	case <-doneB:
		t.Fatal("barrier B released, want blocked -- barriers must not share phase state")
	case <-time.After(50 * time.Millisecond):
		// still blocked, as expected
	}
}

func TestOneAdvanceReleasesManyBlockedReaders(t *testing.T) {
	advance, waitUntilSafe := NewBarrier(1)

	const readers = 50
	var released atomic.Int64
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			waitUntilSafe()
			released.Add(1)
		}()
	}

	advance(1)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("not all readers were released by a single advance")
	}

	if got := released.Load(); got != readers {
		t.Fatalf("released = %d, want %d", got, readers)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

`TestWaitUntilSafeBlocksUntilPhaseReached` proves the core contract with
bounded `select`/`time.After` checks — not testing business time logic, just
using a short timeout as the idiomatic way to assert "still blocked" and
"eventually unblocks" without a real hang. `TestWaitUntilSafeReturnsImmediatelyIfAlreadySafe`
covers the other edge: a reader arriving after the migration finished must
never wait at all. The isolation test repeats this lesson's structural
guarantee. The fan-out test is the one that would fail if `advance` woke
only one reader instead of every reader blocked on the same channel: fifty
goroutines must all observe the release from a single `advance` call, with
the shared `released` counter proving none were missed under `-race`.

## Resources

- [pkg.go.dev: close](https://pkg.go.dev/builtin#close) — closing a channel wakes every goroutine receiving from it, the mechanism `advance` uses to release all readers at once.
- [pkg.go.dev: context.Context.Done](https://pkg.go.dev/context#Context) — the standard library's own use of a closed channel as a broadcast-cancellation signal.
- [PlanetScale docs: Online schema change / Vitess gh-ost](https://vitess.io/docs/reference/vreplication/vreplication/) — phased, dual-write schema migrations this barrier models a safety gate for.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-idempotency-key-dedup-with-ttl-window.md](32-idempotency-key-dedup-with-ttl-window.md) | Next: [34-multi-destination-event-publisher-dlq.md](34-multi-destination-event-publisher-dlq.md)
