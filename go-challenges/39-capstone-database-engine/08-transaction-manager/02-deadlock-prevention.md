# Exercise 2: Deadlock Prevention with Wound-Wait and Wait-Die

The core manager *detects* deadlock after it forms, with a background graph search. The alternative is to *prevent* it from ever forming, using transaction timestamps. This exercise builds both classic timestamp schemes — wait-die and wound-wait — over a small exclusive lock table, in isolation, so the one thing that matters can be tested directly: the direction of the wait. Both schemes assign each transaction a start timestamp (smaller means older) and resolve every conflict by comparing the requester's age to the holder's, so that waits only ever point one way around the timestamp order. A one-directional wait graph cannot contain a cycle, and a graph with no cycle has no deadlock.

This module is fully self-contained. It defines its own `LockKey`, depends on nothing but the standard library, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
preventive.go         LockKey, Policy, Decision, resolveConflict, TSLockTable
cmd/
  demo/
    main.go           run the crossed deadlock under both schemes, report the victim
preventive_test.go    direction table test + crossed-deadlock progress under -race
```

- Files: `preventive.go`, `cmd/demo/main.go`, `preventive_test.go`.
- Implement: `Policy` (`WaitDie`, `WoundWait`), `Decision`, `resolveConflict`, and `TSLockTable` with `Acquire`, `Release`, and `ReleaseAll`; plus `ErrPreventAbort`.
- Test: a pure table test pins the conflict direction of each scheme, and the crossed-deadlock test asserts the older transaction always survives and the younger is always the one aborted, under `-race`.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p deadlock-prevention/cmd/demo && cd deadlock-prevention
go mod init example.com/deadlock-prevention
```

### Why direction is the whole game

The two schemes differ only in who yields when a requester meets a holder it conflicts with. Under wait-die an older requester waits and a younger requester dies (aborts and restarts). Under wound-wait an older requester wounds — preempts and aborts — the holder and takes the lock, while a younger requester waits. Each rule forces every wait edge to point the same way around the timestamp order: wait-die produces only old-waits-for-young edges, wound-wait only young-waits-for-old edges. Either way the graph is a strict order, and a strict order has no cycle.

Inverting either direction silently reintroduces deadlock, which is why `resolveConflict` is a pure function with no locking and a four-row table test pinned to it: the direction is the part worth proving, separate from any timing. A wounded or dying transaction restarts with its *original* timestamp, so it ages relative to newcomers and eventually becomes the oldest, at which point no scheme can abort it again — that is what rules out starvation.

Create `preventive.go`:

```go
// Package deadlock implements the two classic timestamp-based deadlock
// prevention schemes, wait-die and wound-wait, over a small exclusive lock
// table. The package is self-contained: it defines its own LockKey and depends
// on nothing but the standard library.
package deadlock

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

// ErrPreventAbort is returned when a timestamp-based prevention scheme aborts a
// transaction: it either died (wait-die) or was wounded by an older one (wound-wait).
var ErrPreventAbort = errors.New("transaction aborted by deadlock-prevention scheme")

// Policy selects a timestamp-based deadlock-prevention scheme.
type Policy int

const (
	WaitDie   Policy = iota // older waits, younger dies (non-preemptive)
	WoundWait               // older wounds the holder, younger waits (preemptive)
)

// Decision is the outcome of a single lock conflict under a prevention Policy.
type Decision int

const (
	DecideWait  Decision = iota // requester blocks until the holder releases
	DecideDie                   // requester aborts itself
	DecideWound                 // requester preempts (aborts) the holder and proceeds
)

// resolveConflict decides what a requester with timestamp reqTS must do when it
// conflicts with a holder with timestamp holdTS. A smaller timestamp means an
// older, higher-priority transaction.
func resolveConflict(p Policy, reqTS, holdTS uint64) Decision {
	requesterOlder := reqTS < holdTS
	switch p {
	case WaitDie:
		if requesterOlder {
			return DecideWait
		}
		return DecideDie
	case WoundWait:
		if requesterOlder {
			return DecideWound
		}
		return DecideWait
	default:
		return DecideDie
	}
}

// TSLockTable grants exclusive locks under a timestamp-based prevention policy.
// A transaction's timestamp doubles as its identifier; timestamps must be unique
// and assigned in start order (smaller == started earlier == older).
type TSLockTable struct {
	policy  Policy
	mu      sync.Mutex
	cond    *sync.Cond
	owner   map[LockKey]uint64          // current exclusive holder, 0 == free
	owned   map[uint64]map[LockKey]bool // keys held by each transaction
	aborted map[uint64]bool             // transactions that were wounded or died
}

// NewTSLockTable returns an empty lock table that uses policy p.
func NewTSLockTable(p Policy) *TSLockTable {
	t := &TSLockTable{
		policy:  p,
		owner:   make(map[LockKey]uint64),
		owned:   make(map[uint64]map[LockKey]bool),
		aborted: make(map[uint64]bool),
	}
	t.cond = sync.NewCond(&t.mu)
	return t
}

// Acquire takes an exclusive lock on key for the transaction identified by ts.
// It returns ErrPreventAbort if the prevention scheme aborts this transaction.
func (t *TSLockTable) Acquire(ts uint64, key LockKey) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for {
		if t.aborted[ts] {
			return fmt.Errorf("tx %d: %w", ts, ErrPreventAbort)
		}
		holder := t.owner[key]
		if holder == 0 || holder == ts {
			t.grantLocked(ts, key)
			return nil
		}
		switch resolveConflict(t.policy, ts, holder) {
		case DecideWait:
			t.cond.Wait()
		case DecideDie:
			return fmt.Errorf("tx %d: %w", ts, ErrPreventAbort)
		case DecideWound:
			t.woundLocked(holder)
			// The key is now free; loop and grab it.
		}
	}
}

func (t *TSLockTable) grantLocked(ts uint64, key LockKey) {
	t.owner[key] = ts
	if t.owned[ts] == nil {
		t.owned[ts] = make(map[LockKey]bool)
	}
	t.owned[ts][key] = true
}

// woundLocked aborts a holder: it loses every lock and is flagged so that its
// own next Acquire returns ErrPreventAbort.
func (t *TSLockTable) woundLocked(holder uint64) {
	for k := range t.owned[holder] {
		delete(t.owner, k)
	}
	delete(t.owned, holder)
	t.aborted[holder] = true
	t.cond.Broadcast()
}

// Release drops one lock held by ts.
func (t *TSLockTable) Release(ts uint64, key LockKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.owner[key] == ts {
		delete(t.owner, key)
		delete(t.owned[ts], key)
		t.cond.Broadcast()
	}
}

// ReleaseAll drops every lock held by ts and clears its aborted flag. A caller
// invokes it on commit, or after observing ErrPreventAbort, to roll back.
func (t *TSLockTable) ReleaseAll(ts uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.owned[ts] {
		delete(t.owner, k)
	}
	delete(t.owned, ts)
	delete(t.aborted, ts)
	t.cond.Broadcast()
}
```

The `for {}` in `Acquire` wraps `t.cond.Wait()` for the same reason every lock table in this lesson does: a spurious wakeup must re-check `t.aborted[ts]` and the current holder rather than assume progress. A wounded holder does not find out at the moment it is preempted; it discovers its fate the next time it calls `Acquire`, which sees the `aborted` flag and returns `ErrPreventAbort`, so the caller rolls back and restarts with the same — now relatively older — timestamp. The wound path loops rather than returning, because after `woundLocked` frees the key the requester must still grab it.

### The runnable demo

The demo sets up the classic crossed deadlock — transaction 1 (older) holds A and reaches for B, transaction 2 (younger) holds B and reaches for A — under each scheme, and reports which transaction the scheme aborts. The outcome is deterministic and identical for both schemes at the level of *who survives*: the older transaction always wins, because both rules are designed to favor age. What differs (the older one waits versus preempts) is internal; the observable victim is the same.

Create `cmd/demo/main.go`:

```go
// cmd/demo runs the classic crossed deadlock under both prevention schemes and
// reports which transaction the scheme aborts. The outcome is deterministic: in
// both wait-die and wound-wait the older transaction always survives and the
// younger is the one rolled back.
package main

import (
	"errors"
	"fmt"
	"sync"

	deadlock "example.com/deadlock-prevention"
)

func run(policy deadlock.Policy, name string) {
	tbl := deadlock.NewTSLockTable(policy)
	keyA := deadlock.LockKey{Table: "t", Row: "A"}
	keyB := deadlock.LockKey{Table: "t", Row: "B"}

	const oldTS, youngTS = 1, 2 // smaller timestamp == older
	_ = tbl.Acquire(oldTS, keyA)
	_ = tbl.Acquire(youngTS, keyB)

	var wg sync.WaitGroup
	var errOld, errYoung error
	wg.Add(2)
	go func() { defer wg.Done(); errOld = tbl.Acquire(oldTS, keyB); tbl.ReleaseAll(oldTS) }()
	go func() { defer wg.Done(); errYoung = tbl.Acquire(youngTS, keyA); tbl.ReleaseAll(youngTS) }()
	wg.Wait()

	fmt.Printf("%s: older(%d) aborted=%v  younger(%d) aborted=%v\n",
		name, oldTS, errors.Is(errOld, deadlock.ErrPreventAbort),
		youngTS, errors.Is(errYoung, deadlock.ErrPreventAbort))
}

func main() {
	run(deadlock.WaitDie, "wait-die  ")
	run(deadlock.WoundWait, "wound-wait")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wait-die  : older(1) aborted=false  younger(2) aborted=true
wound-wait: older(1) aborted=false  younger(2) aborted=true
```

### Tests

The direction test is a pure table test against `resolveConflict` — four rows pinning each scheme's decision for older-versus-younger, no concurrency. `TestWaitDieNeverWoundsOlder` pins the two invariants that are easy to invert: wait-die never wounds (it is non-preemptive) and wound-wait never makes the younger die. The progress tests run the real crossed deadlock under `-race` and assert that the older transaction is never aborted and the younger always is, with a one-second deadline that fails loudly if the scheme ever stalls.

Create `preventive_test.go`:

```go
package deadlock

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestResolveConflictDirection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		policy        Policy
		reqTS, holdTS uint64
		want          Decision
	}{
		{"waitdie older requester waits", WaitDie, 1, 2, DecideWait},
		{"waitdie younger requester dies", WaitDie, 2, 1, DecideDie},
		{"woundwait older requester wounds", WoundWait, 1, 2, DecideWound},
		{"woundwait younger requester waits", WoundWait, 2, 1, DecideWait},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveConflict(tc.policy, tc.reqTS, tc.holdTS); got != tc.want {
				t.Fatalf("resolveConflict(%d, %d, %d) = %v, want %v",
					tc.policy, tc.reqTS, tc.holdTS, got, tc.want)
			}
		})
	}
}

func TestWaitDieNeverWoundsOlder(t *testing.T) {
	t.Parallel()
	pairs := []struct{ older, younger uint64 }{
		{1, 2}, {3, 9}, {10, 11}, {100, 200},
	}
	for _, p := range pairs {
		if got := resolveConflict(WaitDie, p.older, p.younger); got == DecideWound {
			t.Fatalf("wait-die must never wound: older=%d younger=%d got DecideWound", p.older, p.younger)
		}
		if got := resolveConflict(WoundWait, p.younger, p.older); got == DecideDie {
			t.Fatalf("wound-wait must never make the younger die: younger=%d older=%d got DecideDie", p.younger, p.older)
		}
	}
}

// runCrossDeadlock sets up the classic two-transaction, two-key deadlock and
// returns each transaction's outcome. oldTS holds keyA then reaches for keyB;
// youngTS holds keyB then reaches for keyA.
func runCrossDeadlock(t *testing.T, policy Policy, oldTS, youngTS uint64) (errOld, errYoung error) {
	t.Helper()
	tbl := NewTSLockTable(policy)
	keyA := LockKey{Table: "t", Row: "A"}
	keyB := LockKey{Table: "t", Row: "B"}

	if err := tbl.Acquire(oldTS, keyA); err != nil {
		t.Fatalf("old initial acquire A: %v", err)
	}
	if err := tbl.Acquire(youngTS, keyB); err != nil {
		t.Fatalf("young initial acquire B: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		errOld = tbl.Acquire(oldTS, keyB)
		tbl.ReleaseAll(oldTS)
	}()
	go func() {
		defer wg.Done()
		errYoung = tbl.Acquire(youngTS, keyA)
		tbl.ReleaseAll(youngTS)
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("deadlock-prevention scheme failed to make progress")
	}
	return errOld, errYoung
}

func TestWaitDieMakesProgress(t *testing.T) {
	t.Parallel()
	errOld, errYoung := runCrossDeadlock(t, WaitDie, 1, 2)
	if errOld != nil {
		t.Fatalf("older tx must not be aborted under wait-die, got %v", errOld)
	}
	if !errors.Is(errYoung, ErrPreventAbort) {
		t.Fatalf("younger tx err = %v, want ErrPreventAbort", errYoung)
	}
}

func TestWoundWaitMakesProgress(t *testing.T) {
	t.Parallel()
	errOld, errYoung := runCrossDeadlock(t, WoundWait, 1, 2)
	if errOld != nil {
		t.Fatalf("older tx must not be aborted under wound-wait, got %v", errOld)
	}
	if !errors.Is(errYoung, ErrPreventAbort) {
		t.Fatalf("younger tx err = %v, want ErrPreventAbort", errYoung)
	}
}
```

## Review

The scheme is correct when every wait points one way and the older transaction always survives. Confirm the direction table holds for both policies, that wait-die never returns `DecideWound` and wound-wait never returns `DecideDie`, and that the crossed deadlock always resolves within the deadline with the younger transaction as the only victim. The progress tests must stay clean under `-race`, since `Acquire`, `woundLocked`, and `ReleaseAll` all touch the shared maps under the same mutex and broadcast on the same condition variable. The non-negotiable detail is the `for` loop around `cond.Wait`: a wounded transaction learns its fate only by re-checking the `aborted` flag on wakeup, so a one-shot `if` would let it proceed as though it still held its locks.

## Resources

- [CMU 15-445/645 Lecture 17: Timestamp Ordering Concurrency Control](https://15445.courses.cs.cmu.edu/fall2024/slides/17-timestampordering.pdf) — the wound-wait and wait-die prevention schemes and why the wait direction rules out cycles.
- [CMU 15-445/645 Lecture 16: Two-Phase Locking](https://15445.courses.cs.cmu.edu/fall2024/slides/16-twophaselocking.pdf) — deadlock detection versus prevention, and victim selection.
- [pkg.go.dev/sync — sync.Cond](https://pkg.go.dev/sync#Cond) — `Wait`, `Broadcast`, and the spurious-wakeup contract that forces the `for` loop.

---

Back to [01-transaction-manager-core.md](01-transaction-manager-core.md) | Next: [03-update-lock.md](03-update-lock.md)
