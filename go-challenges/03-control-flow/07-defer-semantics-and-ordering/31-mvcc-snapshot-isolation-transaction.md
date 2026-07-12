# Exercise 31: MVCC Transactions — Deferred Abort/Commit Controls Write Visibility

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Snapshot isolation — the model PostgreSQL, MySQL/InnoDB, and most modern
OLTP engines use — gives every transaction a consistent, unchanging view
of the database from the instant it began, no matter what other
transactions commit while it is still running. The mechanism underneath
is multi-version concurrency control: writes never overwrite data in
place, they append a new version tagged with a commit timestamp, and a
reader only ever sees versions committed strictly before its own
snapshot was taken. This module builds a minimal in-memory MVCC engine
and, critically, the deferred commit-or-abort decision that controls
whether a transaction's writes ever become visible to anyone. The module
is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
mvcc/                         independent module: example.com/mvcc-snapshot-isolation-transaction
  go.mod                       go 1.24
  mvcc.go                       DB (Begin), Txn (Read, Write, Finish)
  cmd/
    demo/
      main.go                  runnable demo: concurrent snapshots see consistent, isolated data
  mvcc_test.go                  concurrent-readers-vs-writers snapshot isolation under -race; abort and idempotency cases
```

- Files: `mvcc.go`, `cmd/demo/main.go`, `mvcc_test.go`.
- Implement: `DB` (`Begin`) and `Txn` (`Read`, `Write`, `Finish(err error) error`).
- Test: a concurrency case proving readers with an earlier snapshot never see a later writer's commit, plus abort and idempotent-`Finish` cases.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why Finish's branch on err is the whole isolation guarantee

`Finish` is designed to be used exactly the way this lesson's rollback
exercises use a deferred closure over a named return:

```go
tx := db.Begin()
defer func() { err = tx.Finish(err) }()
```

Everything a transaction writes goes into `tx.writes`, a map private to
that `Txn` value — no other transaction, and no reader, can see it there
under any circumstance. The only two things that can happen to that
private log are exactly the two branches `Finish` implements. If the
calling function's named `err` is nil, `Finish` commits: it takes the
*next* commit timestamp from the database's clock and appends one new
`version` per buffered key, stamped with that timestamp. Because a
reader's `Read` only accepts versions whose `commitTS` does not exceed
its own snapshot, and a snapshot is fixed at `Begin` time, no transaction
that had already taken its snapshot before this commit can ever see
these new versions — they were not just invisible by convention, they
were appended to the version list *after* that reader's snapshot number
was already frozen. If `err` is non-nil, `Finish` aborts: `tx.writes` is
discarded outright and the database's clock does not move at all. An
aborted transaction consumes no timestamp and leaves absolutely no trace
in `db.versions` — not a version nobody points to, not a gap in the
sequence, nothing a future reader could ever observe.

Create `mvcc.go`:

```go
package mvcc

import "sync"

// version is one committed value for a key, tagged with the commit
// timestamp that made it visible.
type version struct {
	commitTS int64
	value    string
}

// DB is an in-memory multi-version store. Each key holds a list of
// versions ordered by commitTS; a reader sees the latest version whose
// commitTS is <= its own snapshot timestamp -- never a version committed
// after the reader's transaction began.
type DB struct {
	mu       sync.Mutex
	versions map[string][]version
	clock    int64 // logical commit-timestamp counter
}

// NewDB returns an empty database.
func NewDB() *DB {
	return &DB{versions: map[string][]version{}}
}

// Txn is one MVCC transaction: a fixed read snapshot plus a private,
// uncommitted write log.
type Txn struct {
	db       *DB
	snapshot int64
	writes   map[string]string
	done     bool
}

// Begin takes a snapshot of the database's current commit timestamp. Every
// Read during this transaction is pinned to that snapshot, regardless of
// what other transactions commit afterward.
func (db *DB) Begin() *Txn {
	db.mu.Lock()
	defer db.mu.Unlock()
	return &Txn{db: db, snapshot: db.clock, writes: map[string]string{}}
}

// Read returns the value visible at this transaction's snapshot: either a
// pending write from this same transaction (read-your-own-writes), or the
// latest committed version whose commitTS does not exceed the snapshot.
func (tx *Txn) Read(key string) (string, bool) {
	if v, ok := tx.writes[key]; ok {
		return v, true
	}
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	versions := tx.db.versions[key]
	for i := len(versions) - 1; i >= 0; i-- {
		if versions[i].commitTS <= tx.snapshot {
			return versions[i].value, true
		}
	}
	return "", false
}

// Write buffers key=value in this transaction's private log. It is not
// visible to any other transaction -- including ones with a later
// snapshot -- until Finish(nil) publishes it.
func (tx *Txn) Write(key, value string) {
	tx.writes[key] = value
}

// Finish is meant to be deferred right after Begin, with a named error
// return from the caller's function:
//
//	tx := db.Begin()
//	defer func() { err = tx.Finish(err) }()
//
// If err is nil, Finish commits: it takes the next commit timestamp and
// publishes every buffered write as a new version at that timestamp, so
// only transactions that Begin *after* this point can ever see them --
// any transaction already holding an earlier snapshot keeps reading the
// prior versions, which is exactly snapshot isolation. If err is non-nil
// (or becomes non-nil because a panic was recovered upstream), Finish
// aborts: the private write log is simply discarded and the database's
// commit clock does not advance, so an aborted transaction leaves no
// trace for any reader, past or future.
func (tx *Txn) Finish(err error) error {
	if tx.done {
		return err
	}
	tx.done = true

	if err != nil {
		tx.writes = nil // abort: discard the private log, nothing published
		return err
	}

	if len(tx.writes) == 0 {
		return nil // read-only commit: nothing to publish, no timestamp spent
	}

	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	tx.db.clock++
	commitTS := tx.db.clock
	for k, v := range tx.writes {
		tx.db.versions[k] = append(tx.db.versions[k], version{commitTS: commitTS, value: v})
	}
	return nil
}
```

### The runnable demo

`txA` takes its snapshot before `txB` commits a new balance; it must
still read the old value. `txC` begins after `txB`'s commit and sees the
new one. `txD` writes a value and then aborts — that write must never be
visible, not even to a transaction that begins after the abort.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/mvcc-snapshot-isolation-transaction"
)

func main() {
	db := mvcc.NewDB()

	seed := db.Begin()
	seed.Write("balance", "100")
	_ = seed.Finish(nil)

	txA := db.Begin() // snapshot taken before txB commits

	txB := db.Begin()
	txB.Write("balance", "150")
	_ = txB.Finish(nil)

	valA, _ := txA.Read("balance")
	fmt.Printf("txA (snapshot before txB's commit) reads balance=%s\n", valA)
	_ = txA.Finish(nil)

	txC := db.Begin() // snapshot taken after txB committed
	valC, _ := txC.Read("balance")
	fmt.Printf("txC (snapshot after txB's commit) reads balance=%s\n", valC)

	txD := db.Begin()
	txD.Write("balance", "999")
	err := txD.Finish(errors.New("insufficient funds"))
	fmt.Println("txD finished with:", err)

	txE := db.Begin()
	valE, _ := txE.Read("balance")
	fmt.Printf("txE reads balance=%s (txD's aborted write never became visible)\n", valE)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
txA (snapshot before txB's commit) reads balance=100
txC (snapshot after txB's commit) reads balance=150
txD finished with: insufficient funds
txE reads balance=150 (txD's aborted write never became visible)
```

### Tests

`TestSnapshotIsolationUnderConcurrentWriters` takes eight readers'
snapshots before launching 20 concurrent writer goroutines, each
committing its own value for the same key, and asserts every reader
still sees the original pre-write value no matter how the writers'
commits interleave under `-race`. `TestAbortDiscardsWritesAndDoesNotAdvanceClock`
and `TestFinishIsIdempotent` cover the abort path and the guard against
double-`Finish`.

Create `mvcc_test.go`:

```go
package mvcc

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestSnapshotIsolationUnderConcurrentWriters gives readers below a
// snapshot taken before any of the concurrent writers commit, then races
// 20 writers against that fixed snapshot. Every reader must still see the
// pre-write value: its snapshot timestamp is strictly less than every
// writer's commit timestamp, no matter how the goroutines interleave.
func TestSnapshotIsolationUnderConcurrentWriters(t *testing.T) {
	db := NewDB()
	seed := db.Begin()
	seed.Write("k", "v0")
	if err := seed.Finish(nil); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	const readers = 8
	txns := make([]*Txn, readers)
	for i := range txns {
		txns[i] = db.Begin() // snapshot taken before any writer below commits
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx := db.Begin()
			tx.Write("k", fmt.Sprintf("v%d", i+1))
			if err := tx.Finish(nil); err != nil {
				t.Errorf("writer %d commit: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	for i, tx := range txns {
		got, ok := tx.Read("k")
		if !ok || got != "v0" {
			t.Errorf("reader %d snapshot read = %q, want %q (its snapshot predates every concurrent writer)", i, got, "v0")
		}
	}
}

func TestAbortDiscardsWritesAndDoesNotAdvanceClock(t *testing.T) {
	db := NewDB()
	seed := db.Begin()
	seed.Write("k", "v0")
	_ = seed.Finish(nil)

	tx := db.Begin()
	tx.Write("k", "should-never-be-visible")
	err := tx.Finish(errors.New("boom"))
	if err == nil {
		t.Fatal("want non-nil error")
	}

	after := db.Begin()
	got, _ := after.Read("k")
	if got != "v0" {
		t.Fatalf("read after aborted write = %q, want %q", got, "v0")
	}
}

func TestFinishIsIdempotent(t *testing.T) {
	db := NewDB()
	tx := db.Begin()
	tx.Write("k", "v1")

	if err := tx.Finish(nil); err != nil {
		t.Fatalf("first Finish: %v", err)
	}
	// A second Finish on an already-finished transaction is a no-op: it
	// must not re-publish the write log (which was already cleared) or
	// spend another commit timestamp.
	if err := tx.Finish(nil); err != nil {
		t.Fatalf("second Finish should be a no-op returning nil, got %v", err)
	}

	check := db.Begin()
	got, ok := check.Read("k")
	if !ok || got != "v1" {
		t.Fatalf("read = %q, %v, want v1, true", got, ok)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

`Finish` is correct when the visibility of a transaction's writes is
governed entirely by the commit-or-abort decision, and by nothing else —
in particular, never by execution order alone. Assigning a commit
timestamp only in the commit branch, and only after all buffered writes
exist, is what guarantees a reader's fixed snapshot number is a reliable
boundary: everything at or before it is visible, everything after is
not, with no partial or racing updates in between, because the whole
publish step happens under the database's mutex in one pass. The mistake
this design avoids is publishing writes eagerly, at `Write` time, and
only rolling them back on abort — under real concurrency, that ordering
briefly exposes uncommitted, possibly-about-to-be-aborted data to other
transactions, which is precisely the "dirty read" snapshot isolation
exists to make impossible.

## Resources

- [PostgreSQL: 13.2. Transaction Isolation](https://www.postgresql.org/docs/current/transaction-iso.html) — the snapshot-isolation semantics this exercise's `Read`/`Finish` pair implements a minimal model of.
- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — the named-return-plus-deferred-closure idiom `Finish` is designed to be driven by.
- [Jepsen: A Framework for Analyzing Database Isolation Guarantees](https://jepsen.io/analyses) — how real MVCC engines' isolation claims get tested and, sometimes, broken.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-feature-flag-rollout-rule-evaluation.md](30-feature-flag-rollout-rule-evaluation.md) | Next: [32-gossip-protocol-peer-broadcast.md](32-gossip-protocol-peer-broadcast.md)
