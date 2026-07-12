# Exercise 4: Hierarchical Locking with Intention Modes

Row-level locks give concurrency but make a table-wide operation expensive: to lock an entire table you would have to take a lock on every row. Multi-granularity locking solves this with intention modes. Locking a table in IX ("I will write some rows") lets two writers share the table lock as long as their row locks do not overlap, while locking the table in S ("I read the whole table") blocks all writers at once without touching a single row lock. The protocol requires that before locking a row you hold the matching intention lock on its table: IS or stronger to read a row, IX or stronger to write one. This exercise builds the two-level lock table that enforces all of it.

This module is fully self-contained. It defines its own `LockKey`, depends on nothing but the standard library, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
hierarchical.go       LockKey, IntentMode, Compatible, HierLockTable
cmd/
  demo/
    main.go           two IX writers share a table; an S read must wait
hierarchical_test.go  25-cell intention matrix + intention-protocol + row conflict
```

- Files: `hierarchical.go`, `cmd/demo/main.go`, `hierarchical_test.go`.
- Implement: `IntentMode` (`ModeIS`, `ModeIX`, `ModeS`, `ModeSIX`, `ModeX`), `Compatible`, the parent-intention check, and `HierLockTable` with `LockTable`, `LockRow`, and `Release`; plus `ErrMissingIntention`.
- Test: the matrix test drives all twenty-five `(held, req)` pairs against the published table; behavioral tests check the intention-protocol guard, a real row conflict between two IX writers, and that SIX blocks writers while still admitting readers.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/08-transaction-manager/04-hierarchical-locking/cmd/demo && cd go-solutions/39-capstone-database-engine/08-transaction-manager/04-hierarchical-locking
```

### The five-mode matrix and the parent rule

Two ideas combine here. The first is the five-mode compatibility matrix from the concepts file: IS conflicts only with X; IX conflicts with everything except IS and IX; S blocks any intent to write (IX, SIX, X); SIX is compatible only with IS; X is compatible with nothing. `Compatible(held, req)` encodes exactly that, and it is symmetric. The second is the parent rule that ties the two levels together: a transaction may take a row lock only if it already holds a compatible intention lock on the parent table — reading a row (IS or S on the row) needs IS or stronger on the table, writing a row (IX, SIX, or X on the row) needs IX or stronger. `intentParentOK` enforces that, and `LockRow` checks it *before* it consults the row's holders, so a request that violates the granularity protocol fails fast with `ErrMissingIntention` rather than blocking forever.

The payoff is the two-IX-writers case: both transactions take IX on the table (compatible, because IX-IX is allowed) and then take X on *different* rows (compatible, because the rows differ), so they run fully concurrently without ever inspecting each other's row locks. A table-wide S reader, by contrast, conflicts with either IX at the table level and is blocked by a single check, no row scan required. That is the entire reason intention locks exist.

Create `hierarchical.go`:

```go
// Package hierlock implements multi-granularity (table/row) locking with
// intention modes IS, IX, S, SIX, and X. It is self-contained: it defines its
// own LockKey and depends on nothing but the standard library.
package hierlock

import (
	"errors"
	"fmt"
	"sync"
)

// LockKey identifies a row by table name and row identifier.
type LockKey struct {
	Table string
	Row   string
}

// ErrMissingIntention is returned when a transaction tries to lock a row without
// first holding the required intention lock on its table.
var ErrMissingIntention = errors.New("row lock requires an intention lock on the table")

// IntentMode is a multi-granularity lock mode for hierarchical locking.
type IntentMode int

const (
	ModeIS  IntentMode = iota // intention shared: will read some child
	ModeIX                    // intention exclusive: will write some child
	ModeS                     // shared: read the whole node
	ModeSIX                   // shared + intention exclusive: read all, write some
	ModeX                     // exclusive: write the whole node
)

// Compatible reports whether req may be granted while another transaction
// holds held on the same node. The matrix is symmetric.
func Compatible(held, req IntentMode) bool {
	switch held {
	case ModeIS:
		return req != ModeX
	case ModeIX:
		return req == ModeIS || req == ModeIX
	case ModeS:
		return req == ModeIS || req == ModeS
	case ModeSIX:
		return req == ModeIS
	case ModeX:
		return false
	}
	return false
}

// intentParentOK reports whether a parent lock in mode parent permits taking
// child on a node beneath it. Reading a child needs IS or stronger; writing a
// child needs IX or stronger.
func intentParentOK(parent, child IntentMode) bool {
	switch child {
	case ModeIS, ModeS:
		return parent == ModeIS || parent == ModeIX || parent == ModeS ||
			parent == ModeSIX || parent == ModeX
	case ModeIX, ModeSIX, ModeX:
		return parent == ModeIX || parent == ModeSIX || parent == ModeX
	}
	return false
}

// HierLockTable manages locks at two granularities, table and row, and enforces
// the intention-lock protocol between them.
type HierLockTable struct {
	mu           sync.Mutex
	cond         *sync.Cond
	tableHolders map[string]map[uint64]IntentMode
	rowHolders   map[LockKey]map[uint64]IntentMode
}

// NewHierLockTable returns an empty hierarchical lock table.
func NewHierLockTable() *HierLockTable {
	h := &HierLockTable{
		tableHolders: make(map[string]map[uint64]IntentMode),
		rowHolders:   make(map[LockKey]map[uint64]IntentMode),
	}
	h.cond = sync.NewCond(&h.mu)
	return h
}

func (h *HierLockTable) compatibleWithAll(holders map[uint64]IntentMode, self uint64, req IntentMode) bool {
	for ts, held := range holders {
		if ts == self {
			continue
		}
		if !Compatible(held, req) {
			return false
		}
	}
	return true
}

// LockTable acquires a table-level lock for ts in the given mode.
func (h *HierLockTable) LockTable(ts uint64, table string, mode IntentMode) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for !h.compatibleWithAll(h.tableHolders[table], ts, mode) {
		h.cond.Wait()
	}
	if h.tableHolders[table] == nil {
		h.tableHolders[table] = make(map[uint64]IntentMode)
	}
	h.tableHolders[table][ts] = mode
	return nil
}

// LockRow acquires a row-level lock for ts, requiring a compatible intention
// lock on the parent table first.
func (h *HierLockTable) LockRow(ts uint64, table, row string, mode IntentMode) error {
	key := LockKey{Table: table, Row: row}
	h.mu.Lock()
	defer h.mu.Unlock()
	parent, ok := h.tableHolders[table][ts]
	if !ok || !intentParentOK(parent, mode) {
		return fmt.Errorf("tx %d row %s.%s: %w", ts, table, row, ErrMissingIntention)
	}
	for !h.compatibleWithAll(h.rowHolders[key], ts, mode) {
		h.cond.Wait()
	}
	if h.rowHolders[key] == nil {
		h.rowHolders[key] = make(map[uint64]IntentMode)
	}
	h.rowHolders[key][ts] = mode
	return nil
}

// Release drops every table and row lock held by ts.
func (h *HierLockTable) Release(ts uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, holders := range h.tableHolders {
		delete(holders, ts)
	}
	for _, holders := range h.rowHolders {
		delete(holders, ts)
	}
	h.cond.Broadcast()
}
```

`compatibleWithAll` skips the requester's own held mode, which is what lets a transaction re-lock a node or coexist with its own intention lock — without that skip, a transaction holding IX on a table could never take X on one of its rows, because its own table IX would appear to block it. `Release` drops every lock the transaction holds across both levels and broadcasts once, waking every blocked acquirer to recheck.

### The runnable demo

The demo shows the headline case: two transactions both take IX on the same table and lock different rows in X with no conflict, then it reports — without actually blocking — that a third transaction's table-wide S read is incompatible with the held IX and would have to wait. After both writers release, the table is free for the S read.

Create `cmd/demo/main.go`:

```go
// cmd/demo shows multi-granularity locking: two writers share the table in IX
// and lock different rows in X without conflict, while a third transaction's
// table-wide S read is blocked because IX and S are incompatible.
package main

import (
	"fmt"

	"example.com/hierarchical-locking"
)

func main() {
	h := hierlock.NewHierLockTable()

	// tx1 and tx2 both intend to write some rows: IX on the table is shared.
	_ = h.LockTable(1, "accounts", hierlock.ModeIX)
	_ = h.LockRow(1, "accounts", "r1", hierlock.ModeX)
	_ = h.LockTable(2, "accounts", hierlock.ModeIX)
	_ = h.LockRow(2, "accounts", "r2", hierlock.ModeX)
	fmt.Println("tx1 and tx2 share IX on accounts; X on r1 and r2 do not conflict")

	// tx3 wants a table-wide read (S). S is incompatible with IX, so it would
	// block; we report that without actually waiting.
	if hierlock.Compatible(hierlock.ModeIX, hierlock.ModeS) {
		fmt.Println("S read could proceed")
	} else {
		fmt.Println("tx3 S read must wait: S is incompatible with the held IX")
	}

	h.Release(1)
	h.Release(2)
	fmt.Println("tx1 and tx2 released; the table is now free for an S read")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
tx1 and tx2 share IX on accounts; X on r1 and r2 do not conflict
tx3 S read must wait: S is incompatible with the held IX
tx1 and tx2 released; the table is now free for an S read
```

### Tests

The matrix test drives all twenty-five `(held, req)` pairs against the published table, so any single flipped cell fails. `TestMissingIntentionRejected` proves the parent rule blocks a row write under only an IS table lock. `TestHierarchicalRowConflict` runs the real two-IX-writers scenario: both write different rows concurrently, then one reaches for a row the other holds in X and must block until release. `TestSIXBlocksWritersAllowsReaders` exercises the SIX row of the matrix directly — under a SIX table lock a reader's IS intent is granted at once while a writer's IX intent blocks until the SIX holder releases.

Create `hierarchical_test.go`:

```go
package hierlock

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestIntentCompatibilityMatrix(t *testing.T) {
	t.Parallel()
	modes := []IntentMode{ModeIS, ModeIX, ModeS, ModeSIX, ModeX}
	want := map[IntentMode]map[IntentMode]bool{
		ModeIS:  {ModeIS: true, ModeIX: true, ModeS: true, ModeSIX: true, ModeX: false},
		ModeIX:  {ModeIS: true, ModeIX: true, ModeS: false, ModeSIX: false, ModeX: false},
		ModeS:   {ModeIS: true, ModeIX: false, ModeS: true, ModeSIX: false, ModeX: false},
		ModeSIX: {ModeIS: true, ModeIX: false, ModeS: false, ModeSIX: false, ModeX: false},
		ModeX:   {ModeIS: false, ModeIX: false, ModeS: false, ModeSIX: false, ModeX: false},
	}
	for _, held := range modes {
		for _, req := range modes {
			t.Run(fmt.Sprintf("held=%d_req=%d", held, req), func(t *testing.T) {
				t.Parallel()
				if got := Compatible(held, req); got != want[held][req] {
					t.Fatalf("Compatible(%d, %d) = %v, want %v",
						held, req, got, want[held][req])
				}
			})
		}
	}
}

func TestMissingIntentionRejected(t *testing.T) {
	t.Parallel()
	h := NewHierLockTable()
	// Holding only IS on the table, but writing a row needs IX or SIX.
	if err := h.LockTable(1, "t", ModeIS); err != nil {
		t.Fatal(err)
	}
	if err := h.LockRow(1, "t", "r", ModeX); !errors.Is(err, ErrMissingIntention) {
		t.Fatalf("err = %v, want ErrMissingIntention", err)
	}
	h.Release(1)
}

func TestHierarchicalRowConflict(t *testing.T) {
	t.Parallel()
	h := NewHierLockTable()

	// Two writers share IX on the table (compatible) and write different rows.
	if err := h.LockTable(1, "accounts", ModeIX); err != nil {
		t.Fatal(err)
	}
	if err := h.LockRow(1, "accounts", "r1", ModeX); err != nil {
		t.Fatal(err)
	}
	if err := h.LockTable(2, "accounts", ModeIX); err != nil {
		t.Fatal(err)
	}
	if err := h.LockRow(2, "accounts", "r2", ModeX); err != nil {
		t.Fatal(err)
	}

	// tx2 now reaches for r1, held X by tx1: it must block.
	blocked := make(chan error, 1)
	go func() { blocked <- h.LockRow(2, "accounts", "r1", ModeX) }()
	time.Sleep(30 * time.Millisecond)
	select {
	case <-blocked:
		t.Fatal("tx2 acquired X on r1 while tx1 held it")
	default:
	}

	h.Release(1)
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("tx2 row r1: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tx2 did not acquire r1 after tx1 released")
	}
	h.Release(2)
}

func TestSIXBlocksWritersAllowsReaders(t *testing.T) {
	t.Parallel()
	h := NewHierLockTable()

	// tx1 scans the whole table and will write some rows: SIX.
	if err := h.LockTable(1, "accounts", ModeSIX); err != nil {
		t.Fatal(err)
	}

	// A pure reader's intent (IS) is compatible with SIX: it returns at once.
	isDone := make(chan error, 1)
	go func() { isDone <- h.LockTable(2, "accounts", ModeIS) }()
	select {
	case err := <-isDone:
		if err != nil {
			t.Fatalf("tx2 IS under SIX: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tx2 IS should be granted immediately under SIX")
	}

	// A would-be writer's intent (IX) conflicts with SIX: it must block.
	ixDone := make(chan error, 1)
	go func() { ixDone <- h.LockTable(3, "accounts", ModeIX) }()
	time.Sleep(30 * time.Millisecond)
	select {
	case <-ixDone:
		t.Fatal("tx3 IX was granted while tx1 held SIX")
	default:
	}

	h.Release(1) // drop SIX; only tx2's IS remains, which is compatible with IX
	select {
	case err := <-ixDone:
		if err != nil {
			t.Fatalf("tx3 IX after SIX released: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tx3 IX did not unblock after tx1 released SIX")
	}
	h.Release(2)
	h.Release(3)
}
```

## Review

The table is correct when the matrix is exact and the parent rule is enforced before any waiting. Confirm all twenty-five compatibility cells match the published table, that a row write under only an IS table lock fails fast with `ErrMissingIntention`, that two IX writers proceed concurrently on different rows but block on the same row, and that a SIX table lock admits an IS reader immediately while making an IX writer wait. The release path must broadcast so blocked acquirers recheck, and `compatibleWithAll` must skip the requester's own mode — otherwise a transaction's own table IX would forever block its row X. Keep the suite clean under `-race`: every map access is under the single table mutex, and every wait is a `for` loop.

## Resources

- [CMU 15-445/645 Lecture 16: Two-Phase Locking](https://15445.courses.cs.cmu.edu/fall2024/slides/16-twophaselocking.pdf) — the hierarchy/intention-lock section with the IS/IX/SIX compatibility matrix and the multi-granularity protocol.
- [Gray et al., "Granularity of Locks and Degrees of Consistency in a Shared Data Base" (1976)](https://dl.acm.org/doi/10.5555/1282480.1282513) — the original paper that introduces intention modes and SIX.
- [pkg.go.dev/sync — sync.Cond](https://pkg.go.dev/sync#Cond) — `Wait` and `Broadcast`, and why each acquire loop must recheck the predicate.

---

Back to [03-update-lock.md](03-update-lock.md) | Next: [Network Protocol](../09-network-protocol/00-concepts.md)
