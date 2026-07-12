# Exercise 4: A Unit Of Work With Optimistic Concurrency

A repository tells you where one entity lives; a Unit of Work tells you when a set of changes becomes durable together. This exercise builds an in-memory store whose only mutation path is a `UnitOfWork` that stages inserts, updates, and deletes and applies them atomically, guarding every update with an optimistic-concurrency version check so that, when two transactions race to modify the same row, exactly one commits and the other is told it lost.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
uow.go                   Account entity with a Version token, Store with a
                         single-lock Commit path, UnitOfWork that stages
                         Load/Update/Add/Remove and validates versions before
                         applying them all-or-nothing, ErrConflict sentinel
cmd/
  demo/
    main.go              transfer across two accounts in one unit of work, then
                         show two stale commits where only the first wins
uow_test.go              atomic multi-entity commit, stale-version conflict,
                         all-or-nothing abort, and a -race test proving exactly
                         one of N concurrent commits succeeds
```

- Files: `uow.go`, `cmd/demo/main.go`, `uow_test.go`.
- Implement: the `Account` entity with a `Version` field, the `Store` with `Seed`/`Get`/`Begin`, and the `UnitOfWork` with `Load`, `Update`, `Add`, `Remove`, and a single-lock `Commit`.
- Test: a transfer commits atomically, a stale update returns `ErrConflict`, a partially-conflicting commit applies nothing, and N concurrent commits on the same base version produce exactly one winner.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/05-repository-pattern/04-unit-of-work-optimistic-concurrency/cmd/demo && cd go-solutions/24-design-patterns-in-go/05-repository-pattern/04-unit-of-work-optimistic-concurrency
```

### Why a unit of work and what optimistic concurrency buys you

A bare repository exposes one-entity operations: get this, save that. Real work rarely touches one entity. Moving money debits one account and credits another, and both writes must land or neither must — a credit without its matching debit is money invented from nothing. The Unit of Work pattern is the boundary that makes "both or neither" expressible: a caller `Load`s the entities it will touch, mutates its own copies, stages them with `Update`/`Add`/`Remove`, and calls `Commit` once. The store applies the whole batch under a single lock, so no other transaction observes a half-applied state. That single-lock apply is the atomicity guarantee in miniature; a database gives you the same property with a real transaction, and this in-memory model deliberately mirrors its semantics.

Atomicity answers "do these writes land together"; it says nothing about "what if someone else changed the row while I was thinking". That is the concurrency question, and there are two classic answers. Pessimistic concurrency locks the row when you read it, so nobody else can touch it until you are done — correct, but it serializes readers and invites deadlocks. Optimistic concurrency does the opposite: it assumes conflicts are rare, lets everyone read freely, and detects a collision at commit time using a version token. Every entity carries a `Version`. When you `Load` an account you also record the version you saw. At commit the store compares the version you based your change on against the version currently stored; if they differ, someone committed in between, your change is built on a stale read, and the commit is rejected with `ErrConflict` rather than silently clobbering their work. A successful write increments the version, so the next stale committer is caught the same way.

The decisive design choice is that validation and application happen together, under one acquisition of the store lock, inside `Commit`. If you validated versions, released the lock, and then applied, another transaction could slip in between the check and the write — the lost-update bug optimistic concurrency exists to prevent. Holding the lock across both halves makes the compare-and-set indivisible. The second choice is that `Commit` is all-or-nothing across the whole batch: it validates every staged update and removal first, and only if all preconditions hold does it apply any of them. A batch that touches three accounts where one has a stale version applies zero changes, not two — partial application would reintroduce exactly the inconsistency the unit of work is supposed to forbid.

Note the snapshot discipline carried over from the earlier exercises: `Store.Get` returns the `Account` by value, so a caller mutating its copy cannot reach into stored state, and the version it carries is the base the eventual commit checks against. The store never hands out a pointer into its map; the only way to change stored state is to stage a copy and commit it.

Create `uow.go`:

```go
package uow

import (
	"context"
	"errors"
	"sync"
)

// Domain sentinels. A caller distinguishes a lost optimistic race (ErrConflict)
// from a missing or duplicate entity, and from reusing a spent unit of work.
var (
	ErrNotFound  = errors.New("uow: not found")
	ErrConflict  = errors.New("uow: version conflict")
	ErrDuplicate = errors.New("uow: duplicate id")
	ErrCommitted = errors.New("uow: unit of work already committed")
)

// Account is the versioned entity. Version is the optimistic-concurrency token:
// every successful write increments it, so a commit built on an older Version is
// detected as stale.
type Account struct {
	ID      string
	Owner   string
	Balance int64
	Version int64
}

// Store is the in-memory backing store. It hands out snapshots and only mutates
// state through a committed UnitOfWork.
type Store struct {
	mu   sync.Mutex
	data map[string]Account
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{data: make(map[string]Account)}
}

// Seed inserts an account directly, bypassing the unit of work. It exists to
// establish initial state in tests and demos; a zero Version is forced to 1.
func (s *Store) Seed(a Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.Version == 0 {
		a.Version = 1
	}
	s.data[a.ID] = a
}

// Get returns a snapshot copy of the account, including its current Version.
func (s *Store) Get(ctx context.Context, id string) (Account, error) {
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.data[id]
	if !ok {
		return Account{}, ErrNotFound
	}
	return a, nil
}

// Begin opens a new unit of work bound to this store.
func (s *Store) Begin() *UnitOfWork {
	return &UnitOfWork{
		store:   s,
		bases:   make(map[string]int64),
		updates: make(map[string]Account),
		creates: make(map[string]Account),
		removes: make(map[string]struct{}),
	}
}

// UnitOfWork batches changes and commits them atomically against the store. It
// records the base Version of every entity it Loads so Commit can detect a
// concurrent modification (optimistic concurrency).
type UnitOfWork struct {
	store     *Store
	bases     map[string]int64    // id -> Version observed at Load/stage time
	updates   map[string]Account  // staged updates
	creates   map[string]Account  // staged inserts
	removes   map[string]struct{} // staged deletions (id; base Version in bases)
	committed bool
}

// Load reads an account through the unit of work, recording the Version it saw
// as the base the eventual commit will check against.
func (u *UnitOfWork) Load(ctx context.Context, id string) (Account, error) {
	a, err := u.store.Get(ctx, id)
	if err != nil {
		return Account{}, err
	}
	u.bases[id] = a.Version
	return a, nil
}

// Update stages a modified account. The account should come from Load so its
// Version reflects the base the commit validates against.
func (u *UnitOfWork) Update(a Account) {
	u.updates[a.ID] = a
	if _, ok := u.bases[a.ID]; !ok {
		u.bases[a.ID] = a.Version
	}
}

// Add stages a brand-new account. Commit fails with ErrDuplicate if the id
// already exists at apply time.
func (u *UnitOfWork) Add(a Account) {
	u.creates[a.ID] = a
}

// Remove stages a deletion, version-checked like an update.
func (u *UnitOfWork) Remove(a Account) {
	u.removes[a.ID] = struct{}{}
	if _, ok := u.bases[a.ID]; !ok {
		u.bases[a.ID] = a.Version
	}
}

// Commit validates every optimistic precondition and applies the whole batch
// under a single hold of the store lock, so the compare-and-set is indivisible
// and the batch lands all-or-nothing.
func (u *UnitOfWork) Commit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if u.committed {
		return ErrCommitted
	}

	u.store.mu.Lock()
	defer u.store.mu.Unlock()

	// Validate first: nothing is mutated until every precondition holds.
	for id := range u.updates {
		cur, ok := u.store.data[id]
		if !ok {
			return ErrNotFound
		}
		if cur.Version != u.bases[id] {
			return ErrConflict
		}
	}
	for id := range u.removes {
		cur, ok := u.store.data[id]
		if !ok {
			return ErrNotFound
		}
		if cur.Version != u.bases[id] {
			return ErrConflict
		}
	}
	for id := range u.creates {
		if _, ok := u.store.data[id]; ok {
			return ErrDuplicate
		}
	}

	// All preconditions hold: apply the batch.
	for id, a := range u.updates {
		a.Version = u.bases[id] + 1
		u.store.data[id] = a
	}
	for id, a := range u.creates {
		a.Version = 1
		u.store.data[id] = a
	}
	for id := range u.removes {
		delete(u.store.data, id)
	}
	u.committed = true
	return nil
}
```

The whole concurrency story lives in `Commit`. Because the validate loops and the apply loops share one `u.store.mu.Lock()`, no other goroutine can commit between the version check and the write: the lock makes "if the stored version still equals my base, write the new version" a single atomic step. Two units of work that both loaded version 1 will serialize on this lock; the first to enter validates (stored is 1, matches its base), applies (stored becomes 2), and returns; the second enters, validates (stored is now 2, its base is 1), and returns `ErrConflict` having written nothing.

### The runnable demo

The demo does two things. First it transfers thirty units from one account to another inside a single unit of work, proving the debit and credit land together. Then it opens two units of work that both load the same account at the same version and tries to commit both; the first wins and the second is rejected as a conflict, and the final balance reflects only the winning change.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/unit-of-work"
)

func commitResult(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, uow.ErrConflict):
		return "conflict"
	default:
		return err.Error()
	}
}

func main() {
	ctx := context.Background()
	store := uow.NewStore()
	store.Seed(uow.Account{ID: "a", Owner: "Alice", Balance: 100})
	store.Seed(uow.Account{ID: "b", Owner: "Bob", Balance: 0})

	// One unit of work moves 30 from a to b: both writes land or neither does.
	tx := store.Begin()
	a, _ := tx.Load(ctx, "a")
	b, _ := tx.Load(ctx, "b")
	a.Balance -= 30
	b.Balance += 30
	tx.Update(a)
	tx.Update(b)
	if err := tx.Commit(ctx); err != nil {
		fmt.Println("transfer failed:", err)
	}

	a, _ = store.Get(ctx, "a")
	b, _ = store.Get(ctx, "b")
	fmt.Printf("after transfer: a=%d (v%d) b=%d (v%d)\n", a.Balance, a.Version, b.Balance, b.Version)

	// Two units of work load the same base Version; only the first commit wins.
	x := store.Begin()
	y := store.Begin()
	ax, _ := x.Load(ctx, "a")
	ay, _ := y.Load(ctx, "a")

	ax.Balance += 5
	x.Update(ax)
	fmt.Println("first commit:", commitResult(x.Commit(ctx)))

	ay.Balance += 9
	y.Update(ay)
	fmt.Println("second commit:", commitResult(y.Commit(ctx)))

	a, _ = store.Get(ctx, "a")
	fmt.Printf("final: a=%d (v%d)\n", a.Balance, a.Version)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after transfer: a=70 (v2) b=30 (v2)
first commit: ok
second commit: conflict
final: a=75 (v3)
```

The transfer leaves both accounts at version 2. The two stale committers both based their change on version 2; the first pushes the account to version 3 and adds 5, the second is rejected as a conflict and its `+9` is discarded, so the final balance is 75, not 84.

### Tests

`TestTransferIsAtomic` exercises the happy path: a multi-entity commit lands both writes and bumps both versions. `TestStaleUpdateConflicts` pins the core rule by committing one change and then a second based on the now-stale version. `TestCommitAbortsAllOnConflict` proves the all-or-nothing property: a batch where one of two entities has a stale version applies neither. `TestConcurrentCommitsExactlyOneWins` is the `-race` test that matters most; it lets N goroutines all load the same base version, releases them to commit simultaneously, and asserts exactly one succeeds while the other `N-1` see `ErrConflict`.

Create `uow_test.go`:

```go
package uow

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestTransferIsAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewStore()
	s.Seed(Account{ID: "a", Owner: "Alice", Balance: 100})
	s.Seed(Account{ID: "b", Owner: "Bob", Balance: 0})

	tx := s.Begin()
	a, _ := tx.Load(ctx, "a")
	b, _ := tx.Load(ctx, "b")
	a.Balance -= 40
	b.Balance += 40
	tx.Update(a)
	tx.Update(b)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	a, _ = s.Get(ctx, "a")
	b, _ = s.Get(ctx, "b")
	if a.Balance != 60 || a.Version != 2 {
		t.Errorf("a = %d (v%d), want 60 (v2)", a.Balance, a.Version)
	}
	if b.Balance != 40 || b.Version != 2 {
		t.Errorf("b = %d (v%d), want 40 (v2)", b.Balance, b.Version)
	}
}

func TestStaleUpdateConflicts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewStore()
	s.Seed(Account{ID: "a", Owner: "Alice", Balance: 100})

	x := s.Begin()
	y := s.Begin()
	ax, _ := x.Load(ctx, "a")
	ay, _ := y.Load(ctx, "a")

	ax.Balance += 1
	x.Update(ax)
	if err := x.Commit(ctx); err != nil {
		t.Fatalf("first Commit: %v", err)
	}

	ay.Balance += 100
	y.Update(ay)
	if err := y.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale Commit = %v, want ErrConflict", err)
	}

	a, _ := s.Get(ctx, "a")
	if a.Balance != 101 || a.Version != 2 {
		t.Errorf("a = %d (v%d), want 101 (v2)", a.Balance, a.Version)
	}
}

func TestCommitAbortsAllOnConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewStore()
	s.Seed(Account{ID: "a", Owner: "Alice", Balance: 100})
	s.Seed(Account{ID: "b", Owner: "Bob", Balance: 100})

	// A concurrent committer bumps b's version out from under tx.
	tx := s.Begin()
	a, _ := tx.Load(ctx, "a")
	b, _ := tx.Load(ctx, "b")

	other := s.Begin()
	ob, _ := other.Load(ctx, "b")
	ob.Balance += 1
	other.Update(ob)
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("other Commit: %v", err)
	}

	// tx touches both a (fresh) and b (now stale); the whole batch must abort.
	a.Balance += 10
	b.Balance += 10
	tx.Update(a)
	tx.Update(b)
	if err := tx.Commit(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("batch Commit = %v, want ErrConflict", err)
	}

	a, _ = s.Get(ctx, "a")
	if a.Balance != 100 || a.Version != 1 {
		t.Errorf("a applied despite abort: %d (v%d), want 100 (v1)", a.Balance, a.Version)
	}
}

func TestConcurrentCommitsExactlyOneWins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewStore()
	s.Seed(Account{ID: "a", Owner: "Alice", Balance: 0})

	const n = 16
	loaded := make(chan struct{}, n)
	release := make(chan struct{})
	results := make([]error, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			tx := s.Begin()
			a, err := tx.Load(ctx, "a")
			if err != nil {
				results[idx] = err
				loaded <- struct{}{}
				<-release
				return
			}
			a.Balance += int64(idx + 1)
			tx.Update(a)
			// Signal we have loaded the base Version, then wait so every
			// goroutine commits against the same version 1.
			loaded <- struct{}{}
			<-release
			results[idx] = tx.Commit(ctx)
		}(i)
	}

	for i := 0; i < n; i++ {
		<-loaded
	}
	close(release)
	wg.Wait()

	var wins, conflicts int
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected Commit error: %v", err)
		}
	}
	if wins != 1 {
		t.Errorf("wins = %d, want exactly 1", wins)
	}
	if conflicts != n-1 {
		t.Errorf("conflicts = %d, want %d", conflicts, n-1)
	}
	final, _ := s.Get(ctx, "a")
	if final.Version != 2 {
		t.Errorf("final version = %d, want 2", final.Version)
	}
}
```

The `loaded`/`release` handshake is what makes the race test deterministic in its claim: every goroutine has read base version 1 before any of them is allowed to commit, so there is no interleaving in which a late loader sees the already-incremented version and slips through. Exactly one commit can satisfy "stored version equals my base of 1"; the rest collide. Run under `-race` it also proves the store's single mutex actually serializes the concurrent commits.

## Review

The implementation is correct when the version check and the write are indivisible and the batch is all-or-nothing. The single decisive property is that `Commit` validates and applies under one lock acquisition: if you ever release the lock between checking versions and writing, two committers can both pass validation and the second silently overwrites the first — the lost update the whole pattern exists to prevent. Confirm that a successful commit increments `Version`, that a stale commit returns `ErrConflict` and changes nothing, and that a batch containing one stale entity applies none of its changes, not the ones that happened to be fresh.

Common mistakes for this feature. The first is validating versions, unlocking, then applying in a second lock — the textbook compare-and-set race; keep both halves inside one `Lock`/`Unlock`. The second is applying changes as you validate them, so a conflict discovered on the third entity leaves the first two already written; validate the entire batch before mutating anything. The third is forgetting to bump `Version` on write, which makes every future stale committer look fresh and defeats the whole mechanism. The fourth is handing out a pointer from `Get` instead of a value, which lets a caller mutate stored state without committing and erases the base the conflict check depends on. Running `go test -race ./...` is what proves the locking is real: the concurrent test deliberately drives N goroutines through `Commit` at once, and a missing or mis-scoped lock surfaces as either a data race or a win count above one.

## Resources

- [Martin Fowler: Unit of Work](https://martinfowler.com/eaaCatalog/unitOfWork.html) — the catalog entry defining the pattern as an object that tracks changes and writes them out as one transaction.
- [Martin Fowler: Optimistic Offline Lock](https://martinfowler.com/eaaCatalog/optimisticOfflineLock.html) — the version-token strategy this exercise models, and when to prefer it over pessimistic locking.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the mutual-exclusion primitive whose single critical section makes the commit's compare-and-set atomic.
- [Go Blog: Introducing the Go Race Detector](https://go.dev/blog/race-detector) — what `-race` instruments and why the concurrent commit test must run under it.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-decorator-cache-and-logging.md](03-decorator-cache-and-logging.md) | Next: [05-cursor-pagination-and-cache.md](05-cursor-pagination-and-cache.md)
