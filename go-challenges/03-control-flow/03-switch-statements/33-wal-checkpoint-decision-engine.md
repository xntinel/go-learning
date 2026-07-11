# Exercise 33: Decide When to Checkpoint a Write-Ahead Log

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every embedded database that uses a write-ahead log — SQLite in WAL mode,
etcd's boltdb backend, Postgres's own WAL — eventually has to compact it:
fold the accumulated entries into the main store and truncate the log, or
it grows forever. Deciding *when* to checkpoint is a three-signal
judgment call, exactly like the load-shedding decision earlier in this
lesson: size alone misses a log that grows slowly but has sat dirty for
hours; elapsed time alone misses a burst that fills the log in seconds.
The interesting engineering problem here isn't the threshold check itself
— it's that a checkpoint has to briefly stop new writes while it resets
the log's state, and getting that "stop the world, briefly" step wrong is
how a real WAL implementation loses or duplicates a write. This module
builds a WAL whose thresholds are checked with a tagless switch and whose
checkpoint is safe under concurrent writers by construction. It is
self-contained: its own `go mod init`, code, demo, and test.

## What you'll build

```text
walcheckpoint/               independent module: example.com/wal-checkpoint-decision-engine
  go.mod                      go 1.24
  walcheckpoint.go              package walcheckpoint; WAL; New(maxSize, maxDirty, maxElapsed) *WAL; Append, ShouldCheckpoint, Checkpoint
  cmd/demo/main.go              runnable demo driving a WAL past each threshold with a fake clock
  walcheckpoint_test.go          per-threshold subtests, a checkpoint-resets-state test, and a concurrent-access subtest
```

- Implement: `ShouldCheckpoint() bool` — a tagless switch checking size, dirty-entry count, and elapsed time as three independent, sufficient triggers, all guarded by the same `sync.Mutex` that guards `Append` and `Checkpoint`.
- Test: one subtest per threshold in isolation, a test that `Checkpoint` actually resets the counters, and a concurrency subtest firing many `Append` calls alongside `Checkpoint` calls under the race detector.
- Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/walcheckpoint/cmd/demo
cd ~/go-exercises/walcheckpoint
go mod init example.com/wal-checkpoint-decision-engine
go mod edit -go=1.24
```

### "Stop writes momentarily" without a second flag to get wrong

The brief for this exercise calls out a real hazard: a checkpoint decision
requires stopping writes momentarily, and the naive way to implement that
is a second boolean — `checkpointing bool` — that `Append` checks before
recording an entry. That approach is exactly the kind of check-then-act
race the lesson before this one warns against: the check ("is a checkpoint
in progress?") and the act ("record this entry") would live in *different*
critical sections unless you're extremely careful, and a flag is easy to
forget to clear on an error path.

This implementation sidesteps the whole problem by not introducing a
second piece of state at all. `Append`, `ShouldCheckpoint`, and
`Checkpoint` all take the *same* `sync.Mutex`. Holding that one mutex for
the entire body of `Checkpoint` means every concurrent `Append` call
already blocks for the duration — not because it checked a flag and
decided to wait, but because the mutex itself won't let it proceed. There
is no window where a write could sneak in mid-checkpoint, because "mid-
checkpoint" and "the mutex is held" are the same interval by construction.
This is a case where reaching for less machinery (one mutex, zero flags)
produces a *more* correct implementation than reaching for more.

`ShouldCheckpoint`'s three thresholds are checked with a tagless switch
because they are independent, sufficient triggers — any one of them alone
is reason enough to checkpoint, which is exactly the shape the concepts
file describes for compound-predicate dispatch: `case a: ... case b: ...
case c: ... default: ...` rather than one large `||`-chained `if`, because
each case can carry its own comment explaining why that particular signal
matters.

Create `walcheckpoint.go`:

```go
// Package walcheckpoint decides when a write-ahead log needs to be
// checkpointed -- compacted into the main store and truncated -- based on
// three independent signals: how large the log has grown, how many dirty
// entries it holds, and how long it has been since the last checkpoint.
// Every embedded database (SQLite's WAL mode, etcd's boltdb backend,
// Postgres's own WAL) makes this same three-way call, because relying on
// only one signal misses the other two failure modes: size-only misses a
// log that grows slowly but has sat dirty for hours; time-only misses a
// burst that fills the log in seconds.
package walcheckpoint

import (
	"sync"
	"time"
)

// WAL tracks the accumulated state of a write-ahead log since its last
// checkpoint. All exported methods are safe for concurrent use: real WALs
// are appended to by many request-handling goroutines at once while a
// separate background goroutine periodically asks whether it's time to
// checkpoint.
type WAL struct {
	mu             sync.Mutex
	sizeBytes      int64
	dirtyEntries   int
	lastCheckpoint time.Time
	now            func() time.Time

	maxSizeBytes int64
	maxDirty     int
	maxElapsed   time.Duration
}

// New builds a WAL whose checkpoint thresholds are maxSizeBytes,
// maxDirtyEntries, and maxElapsed. now defaults to time.Now; tests
// substitute a fake clock so elapsed-time thresholds are exact and instant.
func New(maxSizeBytes int64, maxDirtyEntries int, maxElapsed time.Duration) *WAL {
	w := &WAL{
		maxSizeBytes: maxSizeBytes,
		maxDirty:     maxDirtyEntries,
		maxElapsed:   maxElapsed,
		now:          time.Now,
	}
	w.lastCheckpoint = w.now()
	return w
}

// SetClock overrides the WAL's time source, for tests and demos that need
// exact, non-sleeping control over elapsed-time thresholds.
func (w *WAL) SetClock(now func() time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = now
	w.lastCheckpoint = now()
}

// Append records a new entry of entryBytes. It blocks for the duration of
// any concurrent Checkpoint call, because Append and Checkpoint share the
// same mutex -- this is the "stop writes momentarily" requirement: rather
// than a separate flag that Append has to remember to check, holding the
// one mutex for the whole compaction means the type system (and the
// compiler-enforced Lock/Unlock discipline) guarantees no write can be
// recorded mid-checkpoint.
func (w *WAL) Append(entryBytes int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sizeBytes += entryBytes
	w.dirtyEntries++
}

// ShouldCheckpoint reports whether any threshold has been crossed since the
// last checkpoint. The three cases are independent triggers, any one of
// which is sufficient -- this is exactly the shape a tagless switch is for.
func (w *WAL) ShouldCheckpoint() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	switch {
	case w.sizeBytes >= w.maxSizeBytes:
		return true
	case w.dirtyEntries >= w.maxDirty:
		return true
	case w.now().Sub(w.lastCheckpoint) >= w.maxElapsed:
		return true
	default:
		return false
	}
}

// Checkpoint compacts the log: it resets the size and dirty-entry counters
// and records the checkpoint time. Holding w.mu for the entire call means
// every concurrent Append blocks until the checkpoint finishes, and every
// concurrent ShouldCheckpoint sees either the pre- or the post-checkpoint
// state, never a torn mix of the two.
func (w *WAL) Checkpoint() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.sizeBytes = 0
	w.dirtyEntries = 0
	w.lastCheckpoint = w.now()
}

// Stats reports the current size and dirty-entry counters, for tests and
// logging.
func (w *WAL) Stats() (sizeBytes int64, dirtyEntries int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sizeBytes, w.dirtyEntries
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	walcheckpoint "example.com/wal-checkpoint-decision-engine"
)

func main() {
	fakeNow := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	w := walcheckpoint.New(1024, 5, time.Minute)
	w.SetClock(func() time.Time { return fakeNow })

	for i := 1; i <= 4; i++ {
		w.Append(200)
		size, dirty := w.Stats()
		fmt.Printf("after append %d: size=%d dirty=%d shouldCheckpoint=%v\n", i, size, dirty, w.ShouldCheckpoint())
	}

	w.Append(200) // 5th append: 5 dirty entries, crosses maxDirtyEntries
	size, dirty := w.Stats()
	fmt.Printf("after append 5: size=%d dirty=%d shouldCheckpoint=%v\n", size, dirty, w.ShouldCheckpoint())

	w.Checkpoint()
	size, dirty = w.Stats()
	fmt.Printf("after checkpoint: size=%d dirty=%d shouldCheckpoint=%v\n", size, dirty, w.ShouldCheckpoint())

	fakeNow = fakeNow.Add(2 * time.Minute)
	fmt.Println("after 2 minutes idle, shouldCheckpoint:", w.ShouldCheckpoint())
}
```

Run `go run ./cmd/demo`, expected output:

```
after append 1: size=200 dirty=1 shouldCheckpoint=false
after append 2: size=400 dirty=2 shouldCheckpoint=false
after append 3: size=600 dirty=3 shouldCheckpoint=false
after append 4: size=800 dirty=4 shouldCheckpoint=false
after append 5: size=1000 dirty=5 shouldCheckpoint=true
after checkpoint: size=0 dirty=0 shouldCheckpoint=false
after 2 minutes idle, shouldCheckpoint: true
```

### Tests

`TestShouldCheckpointThresholds` runs one subtest per trigger in
isolation — size alone, dirty-entry count alone, and elapsed time alone,
with the elapsed-time subtest driven by a fake clock rather than a real
sleep so it's exact and instant. `TestCheckpointResetsCounters` confirms
`Checkpoint` actually zeroes both counters and that `ShouldCheckpoint`
immediately reflects the reset. `TestConcurrentAppendAndCheckpoint` fires
many goroutines appending while another goroutine checkpoints
concurrently; because the exact interleaving isn't deterministic, it only
asserts the invariants that must hold regardless — counters never go
negative, and the dirty count never exceeds what a fully serialized
execution could have produced — and relies on `go test -race` to catch any
synchronization bug.

Create `walcheckpoint_test.go`:

```go
package walcheckpoint

import (
	"sync"
	"testing"
	"time"
)

func TestShouldCheckpointThresholds(t *testing.T) {
	t.Parallel()

	t.Run("below every threshold does not checkpoint", func(t *testing.T) {
		fakeNow := time.Now()
		w := New(1000, 10, time.Hour)
		w.SetClock(func() time.Time { return fakeNow })
		w.Append(100)
		if w.ShouldCheckpoint() {
			t.Fatal("ShouldCheckpoint() = true, want false")
		}
	})

	t.Run("size threshold alone triggers", func(t *testing.T) {
		fakeNow := time.Now()
		w := New(500, 100, time.Hour)
		w.SetClock(func() time.Time { return fakeNow })
		w.Append(500)
		if !w.ShouldCheckpoint() {
			t.Fatal("ShouldCheckpoint() = false, want true (size threshold crossed)")
		}
	})

	t.Run("dirty entry count alone triggers", func(t *testing.T) {
		fakeNow := time.Now()
		w := New(1_000_000, 3, time.Hour)
		w.SetClock(func() time.Time { return fakeNow })
		w.Append(1)
		w.Append(1)
		w.Append(1)
		if !w.ShouldCheckpoint() {
			t.Fatal("ShouldCheckpoint() = false, want true (dirty entry threshold crossed)")
		}
	})

	t.Run("elapsed time alone triggers", func(t *testing.T) {
		fakeNow := time.Now()
		w := New(1_000_000, 1_000_000, time.Minute)
		w.SetClock(func() time.Time { return fakeNow })
		w.Append(1)
		if w.ShouldCheckpoint() {
			t.Fatal("ShouldCheckpoint() = true before the elapsed threshold, want false")
		}
		fakeNow = fakeNow.Add(time.Minute)
		if !w.ShouldCheckpoint() {
			t.Fatal("ShouldCheckpoint() = false after the elapsed threshold, want true")
		}
	})
}

func TestCheckpointResetsCounters(t *testing.T) {
	t.Parallel()

	fakeNow := time.Now()
	w := New(100, 2, time.Hour)
	w.SetClock(func() time.Time { return fakeNow })

	w.Append(60)
	w.Append(60)
	if !w.ShouldCheckpoint() {
		t.Fatal("ShouldCheckpoint() = false before Checkpoint, want true")
	}

	w.Checkpoint()

	size, dirty := w.Stats()
	if size != 0 || dirty != 0 {
		t.Fatalf("after Checkpoint: size=%d dirty=%d, want 0, 0", size, dirty)
	}
	if w.ShouldCheckpoint() {
		t.Fatal("ShouldCheckpoint() = true immediately after Checkpoint, want false")
	}
}

// TestConcurrentAppendAndCheckpoint drives many goroutines appending
// entries while another goroutine repeatedly checkpoints, all at once. The
// mutex shared by Append, ShouldCheckpoint, and Checkpoint means every
// individual operation is race-free regardless of interleaving; the
// invariant this test actually checks is that the counters never go
// negative and never exceed what a fully-serialized execution could have
// produced, which is the property that matters under the race detector.
func TestConcurrentAppendAndCheckpoint(t *testing.T) {
	t.Parallel()

	w := New(10_000, 10_000, time.Hour)

	const appenders = 20
	const appendsEach = 25

	var wg sync.WaitGroup
	for i := 0; i < appenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < appendsEach; j++ {
				w.Append(1)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			if w.ShouldCheckpoint() {
				w.Checkpoint()
			}
		}
	}()

	wg.Wait()

	size, dirty := w.Stats()
	if size < 0 || dirty < 0 {
		t.Fatalf("after concurrent access: size=%d dirty=%d, want both >= 0", size, dirty)
	}
	if dirty > appenders*appendsEach {
		t.Fatalf("dirty = %d, want <= %d (total possible appends)", dirty, appenders*appendsEach)
	}
}
```

Verify with:

```bash
go test -race -count=1 ./...
```

## Review

The engine is correct when any one of the three thresholds independently
triggers a checkpoint recommendation, when `Checkpoint` fully resets the
size and dirty-entry counters so `ShouldCheckpoint` immediately reflects a
clean log, and when concurrent `Append` calls can never observe a torn or
negative counter regardless of how they interleave with a concurrent
`Checkpoint`. Carry this forward: when an operation genuinely needs to
briefly exclude all other operations on the same piece of state (a
checkpoint, a snapshot, a compaction), reach first for holding the existing
mutex for the operation's whole duration before reaching for a second flag
— an extra piece of state to keep in sync is an extra way for the
check-then-act discipline to be violated.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless (expressionless) switch form.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding Append, ShouldCheckpoint, and Checkpoint with one shared lock.
- [SQLite: Write-Ahead Logging](https://www.sqlite.org/wal.html) — checkpointing thresholds in a real, widely-deployed WAL implementation.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-vector-clock-ordering-classifier.md](32-vector-clock-ordering-classifier.md) | Next: [34-dns-service-discovery-with-staleness.md](34-dns-service-discovery-with-staleness.md)
