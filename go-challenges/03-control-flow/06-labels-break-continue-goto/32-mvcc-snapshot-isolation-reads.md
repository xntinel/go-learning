# Exercise 32: Multi-version concurrency control (MVCC) snapshot isolation

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every database that offers snapshot isolation is solving the same problem: a
long-running read transaction must see a single, consistent view of the
data for its entire duration, even while other transactions are actively
committing writes to the same rows in parallel. Multi-version concurrency
control gets there without locking readers out at all — every write appends
a new, timestamped version instead of overwriting anything, and a read
simply asks for the newest version that existed at or before its own
snapshot timestamp. This module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
mvcc/                       independent module: example.com/mvcc
  go.mod                     go 1.24
  mvcc.go                      Version, Store; BeginRead, Write, Delete, ReadAt
  cmd/
    demo/
      main.go                runnable demo: three snapshots across a write, a delete, and a rewrite
  mvcc_test.go                 table-style tests: missing key, isolation across a later write, delete visibility, resurrection after delete, concurrent readers/writers under -race
```

- Files: `mvcc.go`, `cmd/demo/main.go`, `mvcc_test.go`.
- Implement: `Store.Write`/`Delete` appending a new timestamped `Version` per key, `Store.BeginRead` capturing a snapshot timestamp, and `Store.ReadAt(key, ts)` walking a key's version chain newest-first to find the newest version at or before `ts`.
- Test: a key that was never written, a snapshot that predates any write, a write committed after a snapshot staying invisible to it, a delete's visibility boundary, a write after a delete resurrecting the key for later snapshots, and many concurrent readers holding one fixed snapshot against many concurrent writers, asserting every read returns the identical answer.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/mvcc/cmd/demo
cd ~/go-exercises/mvcc
go mod init example.com/mvcc
go mod edit -go=1.24
```

### Why break needs the label here but continue does not — in the same loop

`ReadAt` walks a key's version chain from newest to oldest, and the moment
it finds the version that answers the question — either a value or a
tombstone at or before the snapshot timestamp — every strictly older
version is irrelevant and the search must stop immediately. That "stop
immediately" decision is made inside a `switch`, nested in the `for`, and
that placement is exactly where this chapter's central asymmetry surfaces:
`break` is aware of an enclosing `switch` and, left bare, leaves only the
`switch` — but `continue` is not, and a bare `continue` inside a `switch`
already reaches the enclosing `for` directly.

That means the "too new, skip it" branch (`v.Ts > ts`) can use a plain,
unlabeled `continue` — it already does the right thing, moving to the next
(older) version. But the two branches that have actually found the answer —
a tombstone, or a real value — cannot use a bare `break`: it would leave
only the `switch`, and the `for` would carry on to strictly older versions,
silently overwriting the correct, already-found answer with a stale one the
instant the loop's next iteration ran. `break search`, naming the `for`,
is what actually stops the scan the moment the answer is known. Try
deleting the label from either `break search` in `ReadAt` and running the
tests: `TestDeleteIsVisibleOnlyAtOrAfterItsSnapshot` fails immediately,
because the loop would continue past the tombstone to an older, real
value and wrongly report the key as still present.

Create `mvcc.go`:

```go
package mvcc

import "sync"

// Version is one committed revision of a key: Ts is the commit timestamp
// (a monotonically increasing counter, standing in for a real MVCC engine's
// transaction or commit sequence number), and Deleted marks a tombstone --
// a commit that removed the key rather than setting a new value.
type Version struct {
	Ts      int64
	Value   string
	Deleted bool
}

// Store is a minimal MVCC key-value store: every write appends a new
// Version to that key's chain rather than overwriting anything, so a
// reader holding an older snapshot timestamp can keep reading the value as
// it existed at that instant, even while writers commit newer versions of
// the same key concurrently.
type Store struct {
	mu   sync.RWMutex
	data map[string][]Version // versions appended in increasing Ts order per key
	next int64                // monotonically increasing commit timestamp
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{data: make(map[string][]Version)}
}

// BeginRead returns a snapshot timestamp: every ReadAt call using it sees
// exactly the versions committed at or before this instant, and NONE
// committed after -- even if a writer commits a newer version to the same
// key a moment later. This is the entire snapshot-isolation guarantee,
// expressed as "compare against a number captured once."
func (s *Store) BeginRead() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.next
}

// Write commits a new version of key, stamped with the next timestamp, and
// returns that timestamp.
func (s *Store) Write(key, value string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	ts := s.next
	s.data[key] = append(s.data[key], Version{Ts: ts, Value: value})
	return ts
}

// Delete commits a tombstone version of key: reads at or after this
// timestamp see the key as absent, but reads at an earlier snapshot are
// unaffected.
func (s *Store) Delete(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	ts := s.next
	s.data[key] = append(s.data[key], Version{Ts: ts, Deleted: true})
	return ts
}

// ReadAt returns the value of key as of snapshot ts: the newest version
// whose Ts is <= ts. It walks the version chain newest-first and stops the
// INSTANT that answer is known -- every strictly older version is
// irrelevant once the right one is found, and every newer version was
// committed after this snapshot began and must never be visible to it.
func (s *Store) ReadAt(key string, ts int64) (value string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	versions := s.data[key]
search:
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		switch {
		case v.Ts > ts:
			// Committed after this snapshot began: invisible to it. A bare
			// continue here already reaches the enclosing for -- unlike
			// break, continue is not switch-aware -- so no label is needed
			// on this branch; it just moves to the next (older) version.
			continue
		case v.Deleted:
			// The newest version visible to this snapshot is a tombstone:
			// the key does not exist as of ts. break search is REQUIRED
			// here: a bare break would only leave the switch (break IS
			// switch-aware, unlike continue), and the loop would carry on
			// to strictly OLDER versions that are no longer relevant --
			// silently overwriting this correct "not found" answer with a
			// stale one.
			break search
		default:
			value, ok = v.Value, true
			break search
		}
	}
	return value, ok
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mvcc"
)

func main() {
	s := mvcc.NewStore()

	s.Write("counter", "1")
	snapA := s.BeginRead() // snapshot taken with "counter"=1 committed

	s.Write("counter", "2")
	snapB := s.BeginRead() // snapshot taken with "counter"=2 committed

	s.Delete("counter")
	snapC := s.BeginRead() // snapshot taken after the key was deleted

	s.Write("counter", "3") // committed after every snapshot above was taken

	snapshots := []struct {
		name string
		ts   int64
	}{
		{"snapA", snapA},
		{"snapB", snapB},
		{"snapC", snapC},
	}
	for _, snap := range snapshots {
		v, ok := s.ReadAt("counter", snap.ts)
		fmt.Printf("%s (ts=%d): value=%q ok=%v\n", snap.name, snap.ts, v, ok)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
snapA (ts=1): value="1" ok=true
snapB (ts=2): value="2" ok=true
snapC (ts=3): value="" ok=false
```

Even though `"counter"` is written a fourth time (`"3"`) after every
snapshot was captured, none of the three snapshots ever see it — each one
is frozen at the instant `BeginRead` was called, exactly as MVCC promises.

### Tests

`TestReadAtMissingKey` and `TestReadAtBeforeAnyWrite` cover the boundaries.
`TestSnapshotIsolationHidesLaterWrites` and
`TestDeleteIsVisibleOnlyAtOrAfterItsSnapshot` are the core isolation cases.
`TestWriteAfterDeleteResurrectsForLaterSnapshots` proves a key can come back
for snapshots taken after a rewrite while staying deleted for snapshots in
between. `TestSnapshotStaysConsistentUnderConcurrentWrites` is the
concurrency case: it captures one snapshot, then runs 50 writer goroutines
committing new versions of the same key alongside 50 reader goroutines
reading at that fixed snapshot, and asserts every single read returned the
identical answer — a deterministic invariant that never flakes regardless
of how the goroutines are actually scheduled.

Create `mvcc_test.go`:

```go
package mvcc

import (
	"fmt"
	"sync"
	"testing"
)

func TestReadAtMissingKey(t *testing.T) {
	t.Parallel()

	s := NewStore()
	if _, ok := s.ReadAt("nope", 100); ok {
		t.Fatal("ReadAt on a key that was never written should report ok=false")
	}
}

func TestReadAtBeforeAnyWrite(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Write("k", "v1")
	if _, ok := s.ReadAt("k", 0); ok {
		t.Fatal("ReadAt at ts=0, before any commit, should see nothing")
	}
}

func TestSnapshotIsolationHidesLaterWrites(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Write("k", "v1")
	snap := s.BeginRead()
	s.Write("k", "v2") // committed after the snapshot began

	v, ok := s.ReadAt("k", snap)
	if !ok || v != "v1" {
		t.Fatalf("ReadAt(snap) = (%q, %v), want (%q, true)", v, ok, "v1")
	}

	// A fresh snapshot taken now sees the later write.
	fresh := s.BeginRead()
	v, ok = s.ReadAt("k", fresh)
	if !ok || v != "v2" {
		t.Fatalf("ReadAt(fresh) = (%q, %v), want (%q, true)", v, ok, "v2")
	}
}

func TestDeleteIsVisibleOnlyAtOrAfterItsSnapshot(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Write("k", "v1")
	beforeDelete := s.BeginRead()
	s.Delete("k")
	afterDelete := s.BeginRead()

	if v, ok := s.ReadAt("k", beforeDelete); !ok || v != "v1" {
		t.Fatalf("ReadAt(beforeDelete) = (%q, %v), want (%q, true)", v, ok, "v1")
	}
	if _, ok := s.ReadAt("k", afterDelete); ok {
		t.Fatal("ReadAt(afterDelete) should see the key as deleted")
	}
}

func TestWriteAfterDeleteResurrectsForLaterSnapshots(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Write("k", "v1")
	s.Delete("k")
	afterDelete := s.BeginRead()
	s.Write("k", "v2")
	afterRewrite := s.BeginRead()

	if _, ok := s.ReadAt("k", afterDelete); ok {
		t.Fatal("ReadAt(afterDelete) should still see the key as deleted")
	}
	if v, ok := s.ReadAt("k", afterRewrite); !ok || v != "v2" {
		t.Fatalf("ReadAt(afterRewrite) = (%q, %v), want (%q, true)", v, ok, "v2")
	}
}

// TestSnapshotStaysConsistentUnderConcurrentWrites is the concurrency
// case: a snapshot is captured once, and then many goroutines write NEW
// versions of the same key while many other goroutines repeatedly read at
// that fixed, already-captured snapshot. Every one of those reads must
// return the exact same answer -- if even one goroutine observes a write
// committed after the snapshot was taken, isolation is broken. This is a
// deterministic invariant (the answer set must have exactly one member),
// not a timing-dependent assertion, so it never flakes regardless of how
// the goroutines are scheduled.
func TestSnapshotStaysConsistentUnderConcurrentWrites(t *testing.T) {
	s := NewStore()
	s.Write("k", "v0")
	snap := s.BeginRead()

	const writers = 50
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Write("k", fmt.Sprintf("v%d", i+1))
		}(i)
	}

	const readers = 50
	var mu sync.Mutex
	answers := make(map[string]bool)
	var rwg sync.WaitGroup
	for range readers {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			v, ok := s.ReadAt("k", snap)
			mu.Lock()
			answers[fmt.Sprintf("%s/%v", v, ok)] = true
			mu.Unlock()
		}()
	}

	rwg.Wait()
	wg.Wait()

	if len(answers) != 1 {
		t.Fatalf("snapshot reads returned %d distinct answers, want exactly 1 (snapshot isolation violated): %v", len(answers), answers)
	}
	if !answers["v0/true"] {
		t.Fatalf("snapshot reads = %v, want the single answer to be %q", answers, "v0/true")
	}
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

`ReadAt` is correct when it returns exactly the newest version at or before
its snapshot timestamp, no matter what committed afterward — the
concurrency test is the one to study, since 50 writers race against 50
readers on the same key and every reader must still land on the single
answer the snapshot was entitled to. The bug this exercise guards against
is subtle precisely because it would not show up in a single-threaded,
single-version test: removing the label from `break search` still passes
`TestSnapshotIsolationHidesLaterWrites` if the version chain happens to be
short, but fails the moment a tombstone sits behind an older real value,
because the loop keeps walking past the correct answer. Locking with
`sync.RWMutex` — `RLock` for reads, `Lock` for writes — is what makes the
concurrent test race-free: many `ReadAt` calls can run in parallel, but a
`Write` or `Delete` excludes all of them for the instant it takes to append.

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — `break` is aware of an enclosing `switch` or `select`; `continue` is not.
- [PostgreSQL: Multiversion Concurrency Control](https://www.postgresql.org/docs/current/mvcc.html) — how a real database implements the snapshot-isolation guarantee modeled here.
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — allowing concurrent readers while excluding them during a write.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-distributed-trace-span-propagation.md](31-distributed-trace-span-propagation.md) | Next: [33-gossip-protocol-eventual-consistency.md](33-gossip-protocol-eventual-consistency.md)
