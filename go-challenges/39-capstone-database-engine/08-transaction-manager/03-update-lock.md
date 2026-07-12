# Exercise 3: Lock Upgrade and the Update Lock

A transaction that reads a row and then decides to write it must upgrade its shared lock to exclusive. The hazard is symmetric: if two transactions both hold a shared lock on the same row and both try to upgrade to exclusive, each waits for the other to drop its shared lock, and neither ever will. That is the upgrade deadlock, and it is invisible to a wait-for graph built only from blocked lock *acquisitions*, because both transactions already hold their shared locks — there is no acquisition edge to find. This exercise builds the standard cure, the update lock, in isolation.

This module is fully self-contained. It defines its own `LockKey`, depends on nothing but the standard library, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
upgrade.go            LockKey, ULockMode, uCompatible, UpdateLockTable
cmd/
  demo/
    main.go           walk one S/U/X upgrade through the protocol
upgrade_test.go       S/U/X matrix + upgrade-deadlock-avoidance under -race
```

- Files: `upgrade.go`, `cmd/demo/main.go`, `upgrade_test.go`.
- Implement: `ULockMode` (`ModeS`, `ModeU`, `ModeX`), `uCompatible`, and `UpdateLockTable` with `AcquireShared`, `AcquireUpdate`, `UpgradeToExclusive`, and `Release`; plus `ErrUpgradeNoLock`.
- Test: a table test pins the S/U/X compatibility matrix; the concurrency test reproduces the upgrade scenario and asserts the update lock serializes two would-be upgraders and forces the upgrade to wait for an unrelated shared reader.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

### Why a third mode fixes the symmetric upgrade

The update lock (U) sits between shared and exclusive. A transaction that intends to upgrade takes U instead of S. U is compatible with S — other pure readers proceed normally — but not with another U, so at most one would-be upgrader can exist at any moment. That single asymmetry is the entire fix: two transactions can never both sit holding a lock while waiting to upgrade, because the second one blocks at `AcquireUpdate` before it holds anything that the first one needs. When the U holder is ready it waits out the remaining shared holders and becomes X.

The asymmetry lives in one line of the compatibility function, `uCompatible(ModeU, ModeU) == false`. Because the update lock excludes itself, `AcquireUpdate` serializes the upgraders *before* they take any conflicting lock, rather than after — which is exactly where plain shared-to-exclusive upgrade gets it wrong. The full matrix: S is compatible with S and U; U is compatible with S only; X is compatible with nothing.

Create `upgrade.go`:

```go
// Package updatelock implements the S/U/X update-lock protocol that resolves the
// upgrade deadlock. It is self-contained: it defines its own LockKey and depends
// on nothing but the standard library.
package updatelock

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

// ErrUpgradeNoLock is returned when a transaction tries to upgrade a lock it
// does not currently hold in shared or update mode.
var ErrUpgradeNoLock = errors.New("upgrade requires a held shared or update lock")

// ULockMode is a lock mode in the update-lock protocol that resolves the
// upgrade deadlock: S (shared), U (update), and X (exclusive).
type ULockMode int

const (
	ModeS ULockMode = iota // shared: many readers
	ModeU                  // update: a single designated upgrader, compatible with S
	ModeX                  // exclusive: sole holder
)

// uCompatible reports whether a requested mode may be granted while another
// transaction holds held. The update lock is the key asymmetry: U is compatible
// with S but not with another U, so at most one transaction can be mid-upgrade.
func uCompatible(held, req ULockMode) bool {
	switch held {
	case ModeS:
		return req == ModeS || req == ModeU
	case ModeU:
		return req == ModeS
	case ModeX:
		return false
	}
	return false
}

// UpdateLockTable implements the S/U/X protocol on a per-key basis. A reader
// that intends to write takes U instead of S; because U excludes U, two would-be
// upgraders can never both sit holding a lock while waiting to upgrade, which is
// the upgrade deadlock.
type UpdateLockTable struct {
	mu        sync.Mutex
	cond      *sync.Cond
	shared    map[LockKey]map[uint64]bool
	updater   map[LockKey]uint64
	exclusive map[LockKey]uint64
}

// NewUpdateLockTable returns an empty update-lock table.
func NewUpdateLockTable() *UpdateLockTable {
	t := &UpdateLockTable{
		shared:    make(map[LockKey]map[uint64]bool),
		updater:   make(map[LockKey]uint64),
		exclusive: make(map[LockKey]uint64),
	}
	t.cond = sync.NewCond(&t.mu)
	return t
}

// AcquireShared takes a shared lock; it blocks only while another transaction
// holds the key exclusively.
func (t *UpdateLockTable) AcquireShared(ts uint64, key LockKey) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for t.exclusive[key] != 0 && t.exclusive[key] != ts {
		t.cond.Wait()
	}
	if t.shared[key] == nil {
		t.shared[key] = make(map[uint64]bool)
	}
	t.shared[key][ts] = true
	return nil
}

// AcquireUpdate takes the update lock; it blocks while another transaction holds
// U or X. Shared holders do not block it.
func (t *UpdateLockTable) AcquireUpdate(ts uint64, key LockKey) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for (t.updater[key] != 0 && t.updater[key] != ts) ||
		(t.exclusive[key] != 0 && t.exclusive[key] != ts) {
		t.cond.Wait()
	}
	t.updater[key] = ts
	return nil
}

// UpgradeToExclusive promotes ts to an exclusive lock. It blocks until every
// other shared holder has released, then takes X. Only the update-lock holder
// reaches this point alone, so the upgrade cannot deadlock against a peer.
func (t *UpdateLockTable) UpgradeToExclusive(ts uint64, key LockKey) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.updater[key] != ts && !t.shared[key][ts] {
		return fmt.Errorf("tx %d on %v: %w", ts, key, ErrUpgradeNoLock)
	}
	for t.otherSharedHolders(key, ts) || (t.exclusive[key] != 0 && t.exclusive[key] != ts) {
		t.cond.Wait()
	}
	delete(t.shared[key], ts)
	if t.updater[key] == ts {
		t.updater[key] = 0
	}
	t.exclusive[key] = ts
	t.cond.Broadcast()
	return nil
}

func (t *UpdateLockTable) otherSharedHolders(key LockKey, self uint64) bool {
	for h := range t.shared[key] {
		if h != self {
			return true
		}
	}
	return false
}

// Release drops every mode ts holds on key.
func (t *UpdateLockTable) Release(ts uint64, key LockKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.shared[key] != nil {
		delete(t.shared[key], ts)
	}
	if t.updater[key] == ts {
		t.updater[key] = 0
	}
	if t.exclusive[key] == ts {
		t.exclusive[key] = 0
	}
	t.cond.Broadcast()
}
```

`UpgradeToExclusive` rejects a caller that holds neither U nor S on the key with `ErrUpgradeNoLock`, then blocks while any *other* shared holder remains or another transaction holds X. Because `AcquireUpdate` already serialized the upgraders, the transaction that reaches the wait loop is the only upgrader, so the only thing it can wait for is unrelated shared readers draining — never a peer upgrader, which is what would deadlock.

### The runnable demo

The demo walks one upgrade through the protocol on a single key: transaction 1 takes U (announcing intent to write), transaction 3 takes a plain S (a pure reader, compatible with U), and then transaction 1 upgrades to X — which it can only do once the shared reader releases. The sequence is deterministic because each step waits for the previous one.

Create `cmd/demo/main.go`:

```go
// cmd/demo walks one upgrade through the S/U/X protocol: a reader takes the
// update lock, coexists with a plain shared reader, then upgrades to exclusive
// once that reader releases. The update lock is what keeps a second would-be
// upgrader from ever sitting on a lock at the same time.
package main

import (
	"fmt"

	"example.com/update-lock"
)

func main() {
	tbl := updatelock.NewUpdateLockTable()
	key := updatelock.LockKey{Table: "accounts", Row: "acc-1"}

	// tx1 intends to write: it takes U, not S.
	_ = tbl.AcquireUpdate(1, key)
	fmt.Println("tx1 holds U")

	// tx3 is a pure reader: S is compatible with tx1's U.
	_ = tbl.AcquireShared(3, key)
	fmt.Println("tx3 holds S (compatible with U)")

	// tx1 upgrades; it must wait for tx3's S, so release it first.
	tbl.Release(3, key)
	_ = tbl.UpgradeToExclusive(1, key)
	fmt.Println("tx3 released; tx1 upgraded U -> X")

	tbl.Release(1, key)
	fmt.Println("tx1 released X")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
tx1 holds U
tx3 holds S (compatible with U)
tx3 released; tx1 upgraded U -> X
tx1 released X
```

### Tests

One table test pins all nine `(held, req)` cells of the S/U/X matrix. `TestUpgradeWithoutLockRejected` checks the guard. `TestUpdateLockPreventsUpgradeDeadlock` is the centerpiece: transaction 1 takes U, transaction 2's `AcquireUpdate` must block (proving U excludes U), an unrelated transaction 3 takes S and coexists, and transaction 1's upgrade must wait for transaction 3's S to drop before it becomes X — at no point do two transactions deadlock. `TestTwoUpdatersSerialize` runs two full upgrade cycles concurrently and asserts, with an atomic counter checked under `-race`, that the two never hold X at the same instant.

Create `upgrade_test.go`:

```go
package updatelock

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpdateLockCompatibilityMatrix(t *testing.T) {
	t.Parallel()
	modes := []ULockMode{ModeS, ModeU, ModeX}
	want := map[ULockMode]map[ULockMode]bool{
		ModeS: {ModeS: true, ModeU: true, ModeX: false},
		ModeU: {ModeS: true, ModeU: false, ModeX: false},
		ModeX: {ModeS: false, ModeU: false, ModeX: false},
	}
	for _, held := range modes {
		for _, req := range modes {
			t.Run(fmt.Sprintf("held=%d_req=%d", held, req), func(t *testing.T) {
				t.Parallel()
				if got := uCompatible(held, req); got != want[held][req] {
					t.Fatalf("uCompatible(%d, %d) = %v, want %v", held, req, got, want[held][req])
				}
			})
		}
	}
}

func TestUpgradeWithoutLockRejected(t *testing.T) {
	t.Parallel()
	tbl := NewUpdateLockTable()
	key := LockKey{Table: "t", Row: "r"}
	if err := tbl.UpgradeToExclusive(1, key); !errors.Is(err, ErrUpgradeNoLock) {
		t.Fatalf("err = %v, want ErrUpgradeNoLock", err)
	}
}

func TestUpdateLockPreventsUpgradeDeadlock(t *testing.T) {
	t.Parallel()
	tbl := NewUpdateLockTable()
	key := LockKey{Table: "t", Row: "r"}

	// tx1 becomes the designated upgrader by taking the update lock.
	if err := tbl.AcquireUpdate(1, key); err != nil {
		t.Fatal(err)
	}

	// tx2 also wants to upgrade; it must block on the update lock, not deadlock.
	got2 := make(chan error, 1)
	go func() { got2 <- tbl.AcquireUpdate(2, key) }()

	// tx3 holds a plain shared lock, compatible with tx1's update lock.
	if err := tbl.AcquireShared(3, key); err != nil {
		t.Fatal(err)
	}

	time.Sleep(30 * time.Millisecond)
	select {
	case <-got2:
		t.Fatal("tx2 acquired the update lock while tx1 held it")
	default:
	}

	// tx1 upgrades to exclusive: it must wait for tx3's shared lock to drop.
	upDone := make(chan error, 1)
	go func() { upDone <- tbl.UpgradeToExclusive(1, key) }()
	time.Sleep(30 * time.Millisecond)
	select {
	case <-upDone:
		t.Fatal("tx1 upgraded while tx3 still held a shared lock")
	default:
	}

	tbl.Release(3, key)
	select {
	case err := <-upDone:
		if err != nil {
			t.Fatalf("tx1 upgrade: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tx1 did not upgrade after tx3 released")
	}

	// tx2 was still blocked the whole time; only now can it take U.
	select {
	case <-got2:
		t.Fatal("tx2 acquired the update lock while tx1 held X")
	default:
	}

	tbl.Release(1, key)
	select {
	case err := <-got2:
		if err != nil {
			t.Fatalf("tx2 update acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tx2 did not acquire the update lock after tx1 released")
	}
	tbl.Release(2, key)
}

func TestTwoUpdatersSerialize(t *testing.T) {
	t.Parallel()
	tbl := NewUpdateLockTable()
	key := LockKey{Table: "t", Row: "r"}

	var concurrentX atomic.Int32
	var maxX atomic.Int32

	upgrade := func(ts uint64) {
		if err := tbl.AcquireUpdate(ts, key); err != nil {
			t.Errorf("tx %d AcquireUpdate: %v", ts, err)
			return
		}
		if err := tbl.UpgradeToExclusive(ts, key); err != nil {
			t.Errorf("tx %d UpgradeToExclusive: %v", ts, err)
			return
		}
		n := concurrentX.Add(1)
		for {
			m := maxX.Load()
			if n <= m || maxX.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond) // hold X briefly to expose any overlap
		concurrentX.Add(-1)
		tbl.Release(ts, key)
	}

	done := make(chan struct{}, 2)
	go func() { upgrade(1); done <- struct{}{} }()
	go func() { upgrade(2); done <- struct{}{} }()

	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("two updaters deadlocked instead of serializing")
		}
	}
	if maxX.Load() > 1 {
		t.Fatalf("two transactions held X simultaneously (max=%d)", maxX.Load())
	}
}
```

## Review

The protocol is correct when U excludes U and the upgrade waits only for unrelated readers. Confirm the matrix has S compatible with S and U, U compatible with S alone, and X compatible with nothing; that a second `AcquireUpdate` blocks while the first U is held; that `UpgradeToExclusive` waits out a concurrent shared reader before taking X; and that two full upgrade cycles run concurrently without deadlocking and never hold X simultaneously. The whole point is to read `maxX` concurrently with the upgraders and assert on it under `-race`, so the counter must be atomic and the wait loops must be `for`, not `if` — a spurious wakeup that skipped the recheck would let an upgrade complete while a reader still held S.

## Resources

- [CMU 15-445/645 Lecture 16: Two-Phase Locking](https://15445.courses.cs.cmu.edu/fall2024/slides/16-twophaselocking.pdf) — lock modes, upgrades, and the deadlock that update locks resolve.
- [Microsoft SQL Server: Lock modes (update locks)](https://learn.microsoft.com/en-us/sql/relational-databases/sql-server-transaction-locking-and-row-versioning-guide) — the production rationale for a distinct U mode between S and X.
- [pkg.go.dev/sync — sync.Cond](https://pkg.go.dev/sync#Cond) — `Wait`, `Broadcast`, and the spurious-wakeup contract behind the `for` loops.

---

Back to [02-deadlock-prevention.md](02-deadlock-prevention.md) | Next: [04-hierarchical-locking.md](04-hierarchical-locking.md)
