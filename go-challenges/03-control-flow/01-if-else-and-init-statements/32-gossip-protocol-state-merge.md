# Exercise 32: Gossip Protocol: Merge Conflicting State from Replicas

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An eventually-consistent store lets any replica accept a write, then relies
on background gossip rounds to reconcile the inevitable conflicts: two
replicas both holding a value for the same key, with no coordinator ever in
the loop to say which one is "right." The reconciliation rule has to be
simple enough to run on every key of every gossip exchange, and it has to
get one subtle case exactly right — a delete has to survive being merged
against a peer's stale, pre-delete data, or the delete quietly undoes
itself the next time that peer gossips. This module is fully self-contained:
its own `go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
gossip/                       independent module: example.com/gossip-protocol-state-merge
  go.mod                    go 1.24
  gossip.go                 Entry, MergeEntry(local, remote), Replica (mutex-protected Set/Delete/Merge)
  cmd/
    demo/
      main.go               two replicas converge after one gossip round, then a delete survives a stale merge
  gossip_test.go            MergeEntry table; convergence; tombstone durability; concurrent Set/Delete/Merge -race
```

- Files: `gossip.go`, `cmd/demo/main.go`, `gossip_test.go`.
- Implement: an `Entry{Value string; Version int}` where an empty `Value` at a nonzero `Version` is a tombstone, a pure `MergeEntry(local, remote Entry) Entry` that keeps whichever entry has the higher version, and a `Replica` struct guarded by a `sync.Mutex` with `Set`, `Delete`, `Get`, `Snapshot`, and `Merge(remote map[string]Entry)`.
- Test: a table over `MergeEntry` covering remote older, remote newer, an equal-version tie, a newer remote tombstone beating an older value, and — critically — an older remote value never resurrecting a newer local tombstone; a two-replica convergence test; a concurrency test running concurrent `Set`/`Merge` on both replicas, under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/gossip/cmd/demo
cd ~/go-exercises/gossip
go mod init example.com/gossip-protocol-state-merge
go mod edit -go=1.24
```

### Why a delete is a tombstone, never a map deletion

`Delete` does not remove the key from the internal map — it overwrites the
entry with `Entry{Value: "", Version: cur.Version + 1}`, keeping the version
number alive. That is the one design decision this whole module hinges on:
if `Delete` instead called `delete(r.state, key)`, the next `Merge` against
a peer that still holds the old, pre-delete entry would see no local entry
at all for that key — the zero value `Entry{Value: "", Version: 0}` — and
`MergeEntry` would judge the peer's stale entry with `Version: 3` as newer
than local's phantom `Version: 0`, silently resurrecting data that was
deliberately deleted. Keeping the tombstone's version alive means a later,
lower-versioned entry from a stale peer can never win the comparison. The
caller-visible surface (`Get`) still reports a tombstone as absent, exactly
like a comma-ok map lookup reports a missing key — the distinction between
"deleted" and "never existed" only matters internally, to `MergeEntry`'s
version comparison, not to anyone reading the replica's contents.

Create `gossip.go`:

```go
// Package gossip implements a minimal eventual-consistency replica: each node
// holds its own view of a key-value map and periodically exchanges snapshots
// with peers. Conflicting updates for the same key are resolved by a version
// counter, and deletes are represented as tombstones so an older gossip round
// arriving late can never resurrect a key that was already deleted.
package gossip

import "sync"

// Entry is one versioned value. An empty Value with a nonzero Version is a
// tombstone: the key was deleted at that version, not "set to empty string."
type Entry struct {
	Value   string
	Version int
}

// MergeEntry is the pure decision behind one key's conflict resolution: given
// the local and remote entry for the same key, which one wins. It is the
// only place version-comparison logic lives, so every boundary — remote
// older, remote newer, and the tie — is a one-line table test with no
// mutex or network involved.
//
// A strictly higher remote version always wins, whether or not it carries a
// tombstone: a delete is a real update and must propagate exactly like a
// set. An equal version keeps the local entry — gossip is expected to
// re-exchange the same version repeatedly as rounds overlap, and treating a
// repeat as a fresh win would make merges non-deterministic depending on
// which replica happened to gossip last.
func MergeEntry(local, remote Entry) Entry {
	if remote.Version > local.Version {
		return remote
	}
	return local
}

// Replica holds one node's view of the key-value map, guarded by a mutex so
// Set, Delete, Snapshot, and Merge are all safe to call from the gossip
// goroutine and from application goroutines at the same time.
type Replica struct {
	mu    sync.Mutex
	state map[string]Entry
}

// NewReplica builds an empty Replica.
func NewReplica() *Replica {
	return &Replica{state: make(map[string]Entry)}
}

// Set installs value for key, incrementing the version past whatever this
// replica already had — including past a tombstone, so a set after a delete
// is a genuinely newer version and will win a subsequent merge.
func (r *Replica) Set(key, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.state[key]
	r.state[key] = Entry{Value: value, Version: cur.Version + 1}
}

// Delete marks key as deleted with a tombstone at a version past the
// current one, rather than removing it from the map outright — removing it
// would lose the version number, and a later merge with a peer's older,
// pre-delete entry would then look "newer than nothing" and resurrect it.
func (r *Replica) Delete(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.state[key]
	r.state[key] = Entry{Value: "", Version: cur.Version + 1}
}

// Get returns the live value for key. A tombstone (empty Value) is reported
// the same as a key that was never set: the caller-visible surface never
// distinguishes "deleted" from "absent," only the internal state does, so a
// later merge can still compare its version correctly.
func (r *Replica) Get(key string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.state[key]
	if !ok || e.Value == "" {
		return "", false
	}
	return e.Value, true
}

// Snapshot returns a copy of the full internal state, tombstones included,
// for gossiping to a peer.
func (r *Replica) Snapshot() map[string]Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Entry, len(r.state))
	for k, v := range r.state {
		out[k] = v
	}
	return out
}

// Merge folds a peer's snapshot into this replica's state, resolving every
// shared key with MergeEntry. The read of the current local entry and the
// write of the merge result happen inside the same lock acquisition per
// key, so a concurrent Set or Delete on this replica can never be silently
// overwritten by a merge that read a now-stale local value.
func (r *Replica) Merge(remote map[string]Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, re := range remote {
		r.state[k] = MergeEntry(r.state[k], re)
	}
}
```

### The runnable demo

Replica A sets one key, replica B sets a different key; a single gossip
round (snapshots exchanged in both directions) converges them both. Replica
A then deletes its key and gossips again, and the tombstone takes effect on
both sides.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	gossip "example.com/gossip-protocol-state-merge"
)

func show(label string, r *gossip.Replica, keys []string) {
	fmt.Println(label)
	for _, k := range keys {
		v, ok := r.Get(k)
		fmt.Printf("  %-20s value=%-10q present=%v\n", k, v, ok)
	}
}

func main() {
	a := gossip.NewReplica()
	b := gossip.NewReplica()
	keys := []string{"config:timeout", "config:retries"}

	a.Set("config:timeout", "30s")
	b.Set("config:retries", "3")

	show("before gossip, replica A:", a, keys)
	show("before gossip, replica B:", b, keys)

	// One gossip round: exchange snapshots in both directions.
	snapA, snapB := a.Snapshot(), b.Snapshot()
	a.Merge(snapB)
	b.Merge(snapA)

	fmt.Println("--- after one gossip round, both replicas converge ---")
	show("replica A:", a, keys)
	show("replica B:", b, keys)

	// A deletes a key; B still holds the stale, pre-delete value until the
	// next gossip round.
	a.Delete("config:timeout")

	snapA = a.Snapshot()
	b.Merge(snapA)

	fmt.Println("--- after A deletes config:timeout and gossips again ---")
	show("replica A:", a, keys)
	show("replica B:", b, keys)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before gossip, replica A:
  config:timeout       value="30s"      present=true
  config:retries       value=""         present=false
before gossip, replica B:
  config:timeout       value=""         present=false
  config:retries       value="3"        present=true
--- after one gossip round, both replicas converge ---
replica A:
  config:timeout       value="30s"      present=true
  config:retries       value="3"        present=true
replica B:
  config:timeout       value="30s"      present=true
  config:retries       value="3"        present=true
--- after A deletes config:timeout and gossips again ---
replica A:
  config:timeout       value=""         present=false
  config:retries       value="3"        present=true
replica B:
  config:timeout       value=""         present=false
  config:retries       value="3"        present=true
```

### Tests

The `MergeEntry` table covers every version-comparison outcome, including
the tombstone-durability case that is the whole point of this module. A
convergence test proves two replicas end up identical after one round. A
concurrency test runs `Set`, `Delete`, and `Merge` between two replicas from
many goroutines at once, under `-race`.

Create `gossip_test.go`:

```go
package gossip

import (
	"fmt"
	"sync"
	"testing"
)

func TestMergeEntry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		local  Entry
		remote Entry
		want   Entry
	}{
		{
			name:   "remote older loses",
			local:  Entry{Value: "local-v2", Version: 2},
			remote: Entry{Value: "remote-v1", Version: 1},
			want:   Entry{Value: "local-v2", Version: 2},
		},
		{
			name:   "remote newer wins",
			local:  Entry{Value: "local-v1", Version: 1},
			remote: Entry{Value: "remote-v2", Version: 2},
			want:   Entry{Value: "remote-v2", Version: 2},
		},
		{
			name:   "equal version keeps local",
			local:  Entry{Value: "local", Version: 3},
			remote: Entry{Value: "remote", Version: 3},
			want:   Entry{Value: "local", Version: 3},
		},
		{
			name:   "newer remote tombstone beats an older local value",
			local:  Entry{Value: "still-here", Version: 1},
			remote: Entry{Value: "", Version: 2},
			want:   Entry{Value: "", Version: 2},
		},
		{
			name:   "older remote value never resurrects a newer local tombstone",
			local:  Entry{Value: "", Version: 5},
			remote: Entry{Value: "stale", Version: 2},
			want:   Entry{Value: "", Version: 5},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := MergeEntry(tc.local, tc.remote); got != tc.want {
				t.Errorf("MergeEntry(%+v, %+v) = %+v, want %+v", tc.local, tc.remote, got, tc.want)
			}
		})
	}
}

func TestReplicasConvergeAfterOneGossipRound(t *testing.T) {
	t.Parallel()

	a := NewReplica()
	b := NewReplica()
	a.Set("k1", "from-a")
	b.Set("k2", "from-b")

	snapA, snapB := a.Snapshot(), b.Snapshot()
	a.Merge(snapB)
	b.Merge(snapA)

	for _, key := range []string{"k1", "k2"} {
		va, oka := a.Get(key)
		vb, okb := b.Get(key)
		if va != vb || oka != okb {
			t.Errorf("replicas disagree on %q after gossip: a=(%q,%v) b=(%q,%v)", key, va, oka, vb, okb)
		}
	}
}

func TestDeleteTombstoneSurvivesAStaleMerge(t *testing.T) {
	t.Parallel()

	a := NewReplica()
	b := NewReplica()

	a.Set("k", "v1")
	snapA := a.Snapshot()
	b.Merge(snapA) // b now has k=v1 at version 1

	a.Delete("k") // a's k is now a tombstone at version 2

	// b gossips its stale, pre-delete snapshot back to a.
	snapB := b.Snapshot()
	a.Merge(snapB)

	if _, ok := a.Get("k"); ok {
		t.Fatal("a's delete was resurrected by a stale merge from b")
	}
}

func TestConcurrentSetDeleteMerge(t *testing.T) {
	t.Parallel()

	a := NewReplica()
	b := NewReplica()
	const n = 50

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i%5)
			a.Set(key, fmt.Sprintf("val-%d", i))
			b.Merge(a.Snapshot())
			a.Merge(b.Snapshot())
		}(i)
	}
	wg.Wait()
}
```

Verify: `go test -count=1 -race ./...`

## Review

`TestDeleteTombstoneSurvivesAStaleMerge` is the test that justifies every
other design decision in this module: it deliberately re-injects stale,
pre-delete data back into a replica that already deleted the key, and
asserts the delete holds. Without a version-carrying tombstone, this exact
scenario — routine in any gossip system, since rounds overlap and peers
gossip on their own schedules — would quietly undo deletes at random,
depending on gossip timing. Carry this forward: whenever "delete" is modeled
in a system with more than one writer, ask what happens when a stale writer
re-asserts the old value after the delete, and make sure the delete carries
enough of its own history (a version, a timestamp, a vector clock) to win
that comparison.

## Resources

- [Amazon Dynamo paper, Section 4.4](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf) — vector clocks and conflict resolution in a production gossip-based store.
- [Epidemic Algorithms for Replicated Database Maintenance (Demers et al., 1987)](https://dl.acm.org/doi/10.1145/41840.41841) — the foundational gossip-protocol paper.
- [Cassandra: Tombstones](https://cassandra.apache.org/doc/latest/cassandra/architecture/storage-engine.html#tombstones-and-garbage-collection) — a production system's use of exactly this tombstone-over-deletion pattern.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-consistent-hashing-partition-routing.md](31-consistent-hashing-partition-routing.md) | Next: [33-bloom-filter-probabilistic-dedup.md](33-bloom-filter-probabilistic-dedup.md)
