# Exercise 34: Snapshot Isolation: Determine Row Visibility for a Transaction Start Time

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A database running snapshot isolation — the isolation level behind
PostgreSQL's default `REPEATABLE READ` and every "MVCC" store — never blocks
a reader on a writer: instead, every row keeps every version anyone has ever
written to it, and each transaction's reads are answered against the single
version that was true at the exact instant its own snapshot began. Getting
that visibility check right is the entire mechanism: get it wrong in either
direction and a transaction either sees a value that was never actually
committed yet, or fails to see its own consistent point-in-time view of the
world. This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
mvcc/                          independent module: example.com/snapshot-isolation-mvcc-visibility
  go.mod                    go 1.24
  mvcc.go                   Version, Visible(v, snapshotTS), Table (mutex-protected Write/Commit/Abort/Read)
  cmd/
    demo/
      main.go               five reads across three versions of one key: before, during, and after each commit
  mvcc_test.go              Visible table incl. exact-boundary and aborted cases; Table read sequence; concurrent readers -race
```

- Files: `mvcc.go`, `cmd/demo/main.go`, `mvcc_test.go`.
- Implement: a `Version{Value string; CommitTS int64; Aborted bool}` where `CommitTS == 0` means "not yet committed," a pure `Visible(v Version, snapshotTS int64) bool` checking aborted, then uncommitted, then committed-at-or-after-the-snapshot in that order, and a `Table` guarded by a `sync.Mutex` with `Write`, `Commit`, `Abort`, and `Read(key string, snapshotTS int64) (string, bool)` that walks a key's versions newest-to-oldest for the first one `Visible` accepts.
- Test: a table over `Visible` covering committed-before (visible), committed-exactly-at (invisible boundary), committed-after (invisible), still-uncommitted (invisible), and aborted-despite-a-set-timestamp (invisible); a sequential `Table` walk through commit, an in-flight write, and an abort; a concurrency test with readers holding a fixed snapshot while writes and commits happen concurrently, under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why "committed exactly at the snapshot" is invisible, not visible

`Visible` rejects a version the instant `v.CommitTS >= snapshotTS`, not
`v.CommitTS > snapshotTS`. That inclusive rejection at the boundary is not
an arbitrary tie-break — it is what makes "snapshot" mean something
precise: a transaction's snapshot timestamp is the instant *before* which
its view of the world is frozen, so a commit that lands at exactly that
instant happened concurrently with (or logically after) the reader's own
transaction starting, and letting it through would mean two transactions
that started at the "same" timestamp could observe different data depending
on commit ordering that neither could see. `Visible` also checks `Aborted`
before it ever looks at `CommitTS`, which matters because an aborted
transaction's write must never become visible no matter what timestamp
ended up recorded on it — the two checks are independent facts about a
version, and the order they run in is what guarantees a stray or leftover
`CommitTS` on an aborted version can never accidentally leak through a
weaker check that only looked at the timestamp.

Create `mvcc.go`:

```go
// Package mvcc implements multi-version concurrency control visibility: each
// transaction reads rows as they stood at the instant its snapshot began,
// regardless of what concurrent transactions commit, abort, or are still
// writing after that instant.
package mvcc

import "sync"

// Version is one write to a key. CommitTS is 0 until the writing transaction
// commits — a zero CommitTS means "not yet committed," never "committed at
// time zero," so no valid timestamp can ever be confused with "still
// active."
type Version struct {
	Value    string
	CommitTS int64
	Aborted  bool
}

// Visible is the pure guard behind every row read: given one version and
// the timestamp at which a transaction's snapshot began, decide whether that
// transaction may see it. The three guards run in a fixed order and the
// first one that applies decides the outcome, mirroring exactly how a real
// MVCC engine's cascading visibility check works: an aborted write is never
// visible to anyone, no matter its timestamp; an uncommitted write is
// invisible to everyone but its own transaction (which this package does
// not model, since only committed reads are in scope here); and a write
// committed at or after the reader's snapshot start is invisible because,
// from the reader's point of view, it happened concurrently with or after
// its own transaction began.
func Visible(v Version, snapshotTS int64) bool {
	if v.Aborted {
		return false
	}
	if v.CommitTS == 0 {
		return false
	}
	if v.CommitTS >= snapshotTS {
		return false
	}
	return true
}

// Table stores every version ever written per key, guarded by a mutex.
// Versions are never deleted — a read walks backward from the newest
// version to find the first one Visible to its snapshot, so old versions
// must stay available for as long as any transaction's snapshot might still
// need them.
type Table struct {
	mu   sync.Mutex
	data map[string][]*Version
}

// NewTable builds an empty Table.
func NewTable() *Table {
	return &Table{data: make(map[string][]*Version)}
}

// Write appends a new, uncommitted version of value for key and returns a
// pointer to it, so the caller can later Commit or Abort that exact version.
func (t *Table) Write(key, value string) *Version {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := &Version{Value: value}
	t.data[key] = append(t.data[key], v)
	return v
}

// Commit marks v as committed at commitTS, making it visible to any
// transaction whose snapshot starts after commitTS.
func (t *Table) Commit(v *Version, commitTS int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v.CommitTS = commitTS
}

// Abort marks v as aborted: it is never visible to any transaction,
// regardless of timestamp.
func (t *Table) Abort(v *Version) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v.Aborted = true
}

// Read returns the value of key as it was visible at snapshotTS: the newest
// version whose Visible check passes, walking from the most recent version
// backward. Older versions further back are what let a long-running
// transaction still see a consistent snapshot even while newer versions pile
// up ahead of it.
func (t *Table) Read(key string, snapshotTS int64) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	versions := t.data[key]
	for i := len(versions) - 1; i >= 0; i-- {
		if Visible(*versions[i], snapshotTS) {
			return versions[i].Value, true
		}
	}
	return "", false
}
```

### The runnable demo

One key gets three versions in sequence: "v1" committed at ts=10, "v2"
written but left uncommitted for a while before committing at ts=25, and
"v3" written and then aborted. Five reads at different snapshot timestamps
show exactly which version each one sees.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	mvcc "example.com/snapshot-isolation-mvcc-visibility"
)

func main() {
	table := mvcc.NewTable()

	// T1 writes "v1" and commits at ts=10.
	v1 := table.Write("balance", "v1")
	table.Commit(v1, 10)

	// A snapshot taken at ts=5 (before the commit) never sees v1.
	val, ok := table.Read("balance", 5)
	fmt.Printf("snapshot ts=5:  value=%q found=%v (before v1's commit)\n", val, ok)

	// A snapshot taken at ts=15 (after the commit) sees v1.
	val, ok = table.Read("balance", 15)
	fmt.Printf("snapshot ts=15: value=%q found=%v (after v1's commit)\n", val, ok)

	// T2 writes "v2" but has not committed yet.
	v2 := table.Write("balance", "v2")

	// A snapshot at ts=20 still sees v1 — v2 is uncommitted, invisible to
	// everyone until it commits.
	val, ok = table.Read("balance", 20)
	fmt.Printf("snapshot ts=20: value=%q found=%v (v2 still uncommitted)\n", val, ok)

	table.Commit(v2, 25)

	// A snapshot at ts=30 (after v2's commit) sees v2.
	val, ok = table.Read("balance", 30)
	fmt.Printf("snapshot ts=30: value=%q found=%v (after v2's commit)\n", val, ok)

	// T3 writes "v3" and aborts. No snapshot, past or future, ever sees it.
	v3 := table.Write("balance", "v3")
	table.Abort(v3)

	val, ok = table.Read("balance", 100)
	fmt.Printf("snapshot ts=100: value=%q found=%v (v3 aborted, falls back to v2)\n", val, ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
snapshot ts=5:  value="" found=false (before v1's commit)
snapshot ts=15: value="v1" found=true (after v1's commit)
snapshot ts=20: value="v1" found=true (v2 still uncommitted)
snapshot ts=30: value="v2" found=true (after v2's commit)
snapshot ts=100: value="v2" found=true (v3 aborted, falls back to v2)
```

### Tests

The `Visible` table covers all five outcomes directly, including the exact-
boundary and aborted-with-a-timestamp cases. A sequential `Table` test walks
the same commit/uncommitted/abort sequence as the demo. A concurrency test
holds a fixed snapshot in many reader goroutines while a hundred later
versions are written and committed concurrently, asserting every reader
keeps seeing the pre-snapshot value throughout, under `-race`.

Create `mvcc_test.go`:

```go
package mvcc

import (
	"sync"
	"testing"
)

func TestVisible(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		v          Version
		snapshotTS int64
		want       bool
	}{
		{
			name:       "committed strictly before the snapshot is visible",
			v:          Version{Value: "v", CommitTS: 5},
			snapshotTS: 10,
			want:       true,
		},
		{
			name:       "committed exactly at the snapshot boundary is invisible",
			v:          Version{Value: "v", CommitTS: 10},
			snapshotTS: 10,
			want:       false,
		},
		{
			name:       "committed after the snapshot is invisible",
			v:          Version{Value: "v", CommitTS: 20},
			snapshotTS: 10,
			want:       false,
		},
		{
			name:       "uncommitted (CommitTS zero) is invisible",
			v:          Version{Value: "v", CommitTS: 0},
			snapshotTS: 1000,
			want:       false,
		},
		{
			name:       "aborted is invisible even with a committed timestamp",
			v:          Version{Value: "v", CommitTS: 5, Aborted: true},
			snapshotTS: 10,
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Visible(tc.v, tc.snapshotTS); got != tc.want {
				t.Errorf("Visible(%+v, %d) = %v, want %v", tc.v, tc.snapshotTS, got, tc.want)
			}
		})
	}
}

func TestTableReadSequence(t *testing.T) {
	t.Parallel()

	table := NewTable()

	v1 := table.Write("k", "v1")
	table.Commit(v1, 10)

	if _, ok := table.Read("k", 5); ok {
		t.Fatal("Read at ts=5 should not see v1 (committed at ts=10)")
	}
	if val, ok := table.Read("k", 15); !ok || val != "v1" {
		t.Fatalf("Read at ts=15 = (%q, %v), want (v1, true)", val, ok)
	}

	v2 := table.Write("k", "v2")
	if val, ok := table.Read("k", 20); !ok || val != "v1" {
		t.Fatalf("Read at ts=20 while v2 is uncommitted = (%q, %v), want (v1, true)", val, ok)
	}

	table.Commit(v2, 25)
	if val, ok := table.Read("k", 30); !ok || val != "v2" {
		t.Fatalf("Read at ts=30 after v2 commits = (%q, %v), want (v2, true)", val, ok)
	}

	v3 := table.Write("k", "v3")
	table.Abort(v3)
	if val, ok := table.Read("k", 1000); !ok || val != "v2" {
		t.Fatalf("Read after v3 aborts = (%q, %v), want (v2, true), v3 must never be visible", val, ok)
	}
}

func TestReadMissingKey(t *testing.T) {
	t.Parallel()

	table := NewTable()
	if _, ok := table.Read("nope", 100); ok {
		t.Fatal("Read on a never-written key should report not found")
	}
}

func TestConcurrentReadersDuringWrites(t *testing.T) {
	t.Parallel()

	table := NewTable()
	v0 := table.Write("k", "initial")
	table.Commit(v0, 1)

	const readers = 32
	const snapshotTS = 2
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Every reader holds a fixed snapshot at ts=2: it must always see
	// "initial", no matter how many later versions are written and
	// committed concurrently, since none of those later commits can ever
	// have a timestamp less than the snapshot's own timestamp of 2.
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					val, ok := table.Read("k", snapshotTS)
					if !ok || val != "initial" {
						t.Errorf("Read(k, %d) = (%q, %v), want (initial, true)", snapshotTS, val, ok)
						return
					}
				}
			}
		}()
	}

	for i := 0; i < 100; i++ {
		v := table.Write("k", "later")
		table.Commit(v, int64(3+i))
	}
	close(stop)
	wg.Wait()
}
```

Verify: `go test -count=1 -race ./...`

## Review

`TestConcurrentReadersDuringWrites` is the test that actually validates
"snapshot isolation" as a real property rather than just a correct-looking
function: it proves that a reader's view stays frozen at its own snapshot
timestamp for the reader's entire lifetime, no matter how much concurrent
writing and committing happens around it. Carry this forward: whenever a
type's whole reason to exist is to give one caller a stable view while
other callers keep mutating shared state, the test that matters most holds
that view open across real concurrent mutation and asserts it never moved —
a single-threaded before/after test can never catch a stable-view guarantee
breaking under genuine concurrency.

## Resources

- [PostgreSQL: 13.2.1. Read Committed Isolation Level](https://www.postgresql.org/docs/current/transaction-iso.html#XACT-READ-COMMITTED) and the following Repeatable Read section — the production isolation levels this module's `Visible` implements the core mechanism of.
- [A Critique of ANSI SQL Isolation Levels (Berenson et al., 1995)](https://www.microsoft.com/en-us/research/publication/a-critique-of-ansi-sql-isolation-levels/) — the paper that formalized snapshot isolation as distinct from the ANSI levels.
- [CockroachDB: How CockroachDB does distributed atomic transactions](https://www.cockroachlabs.com/blog/how-cockroachdb-distributes-atomic-transactions/) — a production MVCC system built on the same visibility-by-timestamp principle.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-bloom-filter-probabilistic-dedup.md](33-bloom-filter-probabilistic-dedup.md) | Next: [../02-for-loops/00-concepts.md](../02-for-loops/00-concepts.md)
