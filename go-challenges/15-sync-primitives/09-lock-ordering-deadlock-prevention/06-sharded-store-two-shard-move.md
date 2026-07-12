# Exercise 6: Sharded Session Store — Cross-Shard Rename Without Deadlock

Striped locking is the standard fix for a hot global mutex: split the map into
N shards, each with its own lock, and most operations touch exactly one. The
trap is the multi-key operation — a rename whose old and new keys hash to
different shards needs two shard locks, and the two-lock ordering problem from
exercise 1 silently returns. This exercise builds the store and gets the
rename right.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
sessionstore/              independent module: example.com/sessionstore
  go.mod
  store.go                 type Store (N shards, each sync.RWMutex + map);
                           New, Get, Set, Delete, Len, Rename (ascending
                           shard-index order, same-shard fast path);
                           ErrNotFound, ErrTargetExists
  cmd/
    demo/
      main.go              runnable demo: set, get, cross-key rename, error path
  store_test.go            same-shard and cross-shard rename tests (colliding
                           keys found at setup), opposite-direction rename storm
                           with a watchdog, count invariant under -race
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: shard selection via `maphash.String` with a per-store `maphash.MakeSeed`; `Get` under `RLock`; `Set`/`Delete` under `Lock`; `Rename(oldKey, newKey)` that locks the two shards in ascending shard-index order, with a single-lock fast path when both keys land on one shard.
- Test: unit coverage for all operations including keys that collide onto one shard (found by searching at test setup); the hard test — opposite-direction cross-shard renames looping concurrently under `-race` with a watchdog, asserting no session is lost or duplicated.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/09-lock-ordering-deadlock-prevention/06-sharded-store-two-shard-move/cmd/demo
cd go-solutions/15-sync-primitives/09-lock-ordering-deadlock-prevention/06-sharded-store-two-shard-move
```

### Striped locks, and the operation that spans two stripes

A single `sync.RWMutex` around one big map serializes every session lookup in
the process; under load, the lock — not the map — becomes the bottleneck.
Sharding fixes it: hash the key, take `hash % N` as the shard index, and only
that shard's lock is touched. Reads on different shards proceed fully in
parallel; even writes only contend one-Nth as often. The hash is
`hash/maphash`: seeded (`maphash.MakeSeed`) so shard distribution cannot be
engineered by an attacker who controls session IDs — with an unseeded or
guessable hash, a hostile client could craft keys that all land on one shard
and turn your striping back into a global lock (hash-flooding).

`Get`, `Set`, and `Delete` are unremarkable: compute the index, take one lock.
`Rename` is where the chapter's lesson lives. Moving a session from `oldKey`
to `newKey` must be atomic — an observer must never see both keys present, nor
neither — so both shards must be locked *simultaneously*. Two goroutines
renaming in "opposite directions" (one moving a key from shard 2 to shard 5,
another from shard 5 to shard 2) are exactly the two opposite-order transfer
goroutines from exercise 1 in a new costume. Same bug, same fix: the shard
*index* is a stable, total ordering key, so lock the lower index first, always.

The self-deadlock guard also generalizes, and it is sneakier here: the guard
is *not* `oldKey == newKey`. Two different keys can hash to the same shard,
and the two-lock path would then lock one `RWMutex` twice and wedge. The guard
must compare the identity of the *locks* — `shardIndex(oldKey) ==
shardIndex(newKey)` — and take a single-lock fast path. Guarding on key
equality alone is the classic review miss in sharded code: it passes every
test until two keys happen to collide, and with N shards and enough traffic,
they always eventually do. (Renaming a key to itself is handled before any of
this: it is a read-only existence check.)

Both checks — old key present, new key absent — happen under the locks, and
the delete and insert happen in the same critical section. `ErrTargetExists`
protects against silently clobbering another live session, which in a real
auth system is an account-takeover class bug, not an inconvenience.

Create `store.go`:

```go
// Package sessionstore is an in-memory session store sharded N ways to
// reduce lock contention. Single-key operations lock one shard;
// Rename, the one multi-shard operation, acquires shard locks in
// ascending shard-index order so it can never deadlock against a
// concurrent Rename crossing the same shards the other way.
package sessionstore

import (
	"errors"
	"fmt"
	"hash/maphash"
	"sync"
)

var (
	// ErrNotFound reports that the session key is absent.
	ErrNotFound = errors.New("sessionstore: session not found")

	// ErrTargetExists reports that Rename would overwrite a live session.
	ErrTargetExists = errors.New("sessionstore: target key already exists")
)

type shard struct {
	mu sync.RWMutex
	m  map[string]string
}

// Store is a sharded session store. The zero value is not usable; call New.
type Store struct {
	seed   maphash.Seed
	shards []shard
}

// New returns a Store with n shards (minimum 1). The maphash seed is
// per-Store and random, so shard placement cannot be engineered by
// clients who control key strings.
func New(n int) *Store {
	if n < 1 {
		n = 1
	}
	s := &Store{seed: maphash.MakeSeed(), shards: make([]shard, n)}
	for i := range s.shards {
		s.shards[i].m = make(map[string]string)
	}
	return s
}

// shardIndex maps a key to its shard. This index is also the global
// lock-ordering key for multi-shard operations.
func (s *Store) shardIndex(key string) int {
	return int(maphash.String(s.seed, key) % uint64(len(s.shards)))
}

// Get returns the session value for key.
func (s *Store) Get(key string) (string, bool) {
	sh := &s.shards[s.shardIndex(key)]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	v, ok := sh.m[key]
	return v, ok
}

// Set stores value under key, overwriting any existing session.
func (s *Store) Set(key, value string) {
	sh := &s.shards[s.shardIndex(key)]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.m[key] = value
}

// Delete removes key, reporting whether it was present.
func (s *Store) Delete(key string) bool {
	sh := &s.shards[s.shardIndex(key)]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	_, ok := sh.m[key]
	delete(sh.m, key)
	return ok
}

// Len reports the total number of sessions, counted shard by shard.
// Like Snapshot in a ledger, it is not a point-in-time count under
// concurrent writes; it is exact at quiescence.
func (s *Store) Len() int {
	n := 0
	for i := range s.shards {
		s.shards[i].mu.RLock()
		n += len(s.shards[i].m)
		s.shards[i].mu.RUnlock()
	}
	return n
}

// Rename atomically moves the session at oldKey to newKey: at no instant
// are both keys visible, and the session is never absent. It fails with
// ErrNotFound if oldKey is missing and ErrTargetExists if newKey is
// already a live session.
//
// Locking: the guard compares shard INDEXES, not keys — two distinct
// keys on the same shard must not lock that shard twice. Cross-shard
// moves lock the lower-indexed shard first, the package's total order.
func (s *Store) Rename(oldKey, newKey string) error {
	if oldKey == newKey {
		if _, ok := s.Get(oldKey); !ok {
			return fmt.Errorf("%w: %q", ErrNotFound, oldKey)
		}
		return nil
	}

	oi, ni := s.shardIndex(oldKey), s.shardIndex(newKey)

	if oi == ni {
		// Same-shard fast path: one lock, or we self-deadlock.
		sh := &s.shards[oi]
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return moveLocked(sh.m, sh.m, oldKey, newKey)
	}

	lo, hi := min(oi, ni), max(oi, ni)
	s.shards[lo].mu.Lock()
	defer s.shards[lo].mu.Unlock()
	s.shards[hi].mu.Lock()
	defer s.shards[hi].mu.Unlock()

	return moveLocked(s.shards[oi].m, s.shards[ni].m, oldKey, newKey)
}

// moveLocked moves oldKey to newKey between two (possibly identical)
// maps whose shard locks are held by the caller.
func moveLocked(from, to map[string]string, oldKey, newKey string) error {
	v, ok := from[oldKey]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, oldKey)
	}
	if _, exists := to[newKey]; exists {
		return fmt.Errorf("%w: %q", ErrTargetExists, newKey)
	}
	delete(from, oldKey)
	to[newKey] = v
	return nil
}
```

### The runnable demo

Shard placement depends on the random seed, so the demo prints nothing
seed-dependent — whether this particular rename crossed shards varies by run,
and the correctness contract is identical either way. That is the point of
pushing the ordering into the package: callers cannot tell and must not care.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sessionstore"
)

func main() {
	store := sessionstore.New(8)

	store.Set("sess-100", "alice")
	store.Set("sess-200", "bob")
	store.Set("sess-300", "carol")

	if user, ok := store.Get("sess-100"); ok {
		fmt.Println("sess-100 belongs to:", user)
	}

	// Session-ID rotation after privilege elevation: same session data,
	// new unguessable key. May or may not cross shards; the store
	// handles both identically.
	if err := store.Rename("sess-100", "sess-100-rotated"); err != nil {
		fmt.Println("rename err:", err)
		return
	}
	if user, ok := store.Get("sess-100-rotated"); ok {
		fmt.Println("sess-100-rotated belongs to:", user)
	}
	if _, ok := store.Get("sess-100"); !ok {
		fmt.Println("old key is gone")
	}
	fmt.Println("sessions stored:", store.Len())

	if err := store.Rename("sess-999", "sess-999-new"); err != nil {
		fmt.Println("rename missing:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sess-100 belongs to: alice
sess-100-rotated belongs to: alice
old key is gone
sessions stored: 3
rename missing: sessionstore: session not found: "sess-999"
```

### Tests

The seed is random per store, so the tests cannot hard-code which keys
collide; instead `findKeys` searches generated key names at setup until it has
a pair on the same shard (exercising the fast path) and a pair on different
shards (exercising the two-lock path) — the same-package test reaches
`shardIndex` directly. The hard test, `TestOppositeDirectionRenameStorm`, is
exercise 1's both-directions test rebuilt for shards: one session bounces
between a cross-shard key pair while two goroutines rename it in opposite
directions, each retrying past `ErrNotFound` (losing the race is expected;
losing the *session* is not). A watchdog goroutine fails the test with a
diagnostic if the storm wedges — remember that a deadlocked test otherwise
just hangs until the ten-minute kill. The invariant at the end: exactly one of
the two keys holds the session, the value survived, and `Len` is exactly 1.

Create `store_test.go`:

```go
package sessionstore

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// findKeys searches generated key names until it finds a pair hashing to
// the same shard and a pair hashing to different shards.
func findKeys(t *testing.T, s *Store) (sameA, sameB, diffA, diffB string) {
	t.Helper()
	byShard := map[int]string{}
	for i := 0; ; i++ {
		key := fmt.Sprintf("sess-%d", i)
		idx := s.shardIndex(key)
		if prev, ok := byShard[idx]; ok && sameA == "" && prev != key {
			sameA, sameB = prev, key
		}
		for otherIdx, prev := range byShard {
			if otherIdx != idx && diffA == "" {
				diffA, diffB = prev, key
			}
		}
		byShard[idx] = key
		if sameA != "" && diffA != "" {
			return sameA, sameB, diffA, diffB
		}
		if i > 10000 {
			t.Fatal("could not find colliding/non-colliding keys")
		}
	}
}

func TestSetGetDelete(t *testing.T) {
	t.Parallel()

	s := New(4)
	s.Set("k", "v")
	if got, ok := s.Get("k"); !ok || got != "v" {
		t.Fatalf("Get = %q,%v, want v,true", got, ok)
	}
	if !s.Delete("k") {
		t.Fatal("Delete reported absent for a present key")
	}
	if _, ok := s.Get("k"); ok {
		t.Fatal("key still present after Delete")
	}
	if s.Delete("k") {
		t.Fatal("Delete reported present for an absent key")
	}
}

func TestRenamePaths(t *testing.T) {
	t.Parallel()

	s := New(4)
	sameA, sameB, diffA, diffB := findKeys(t, s)

	t.Run("same shard", func(t *testing.T) {
		s.Set(sameA, "alice")
		if err := s.Rename(sameA, sameB); err != nil {
			t.Fatalf("same-shard rename: %v", err)
		}
		if v, ok := s.Get(sameB); !ok || v != "alice" {
			t.Fatalf("Get(%q) = %q,%v, want alice,true", sameB, v, ok)
		}
		if _, ok := s.Get(sameA); ok {
			t.Fatal("old key survived rename")
		}
	})

	t.Run("cross shard", func(t *testing.T) {
		s.Set(diffA, "bob")
		if err := s.Rename(diffA, diffB); err != nil {
			t.Fatalf("cross-shard rename: %v", err)
		}
		if v, ok := s.Get(diffB); !ok || v != "bob" {
			t.Fatalf("Get(%q) = %q,%v, want bob,true", diffB, v, ok)
		}
	})
}

func TestRenameErrors(t *testing.T) {
	t.Parallel()

	s := New(4)
	s.Set("a", "alice")
	s.Set("b", "bob")

	if err := s.Rename("missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename of missing key: err = %v, want ErrNotFound", err)
	}
	if err := s.Rename("a", "b"); !errors.Is(err, ErrTargetExists) {
		t.Errorf("rename onto live key: err = %v, want ErrTargetExists", err)
	}
	if err := s.Rename("a", "a"); err != nil {
		t.Errorf("self-rename of present key: err = %v, want nil", err)
	}
	if err := s.Rename("missing", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("self-rename of missing key: err = %v, want ErrNotFound", err)
	}
	if v, _ := s.Get("a"); v != "alice" {
		t.Errorf("session data damaged by failed renames: %q", v)
	}
}

func TestOppositeDirectionRenameStorm(t *testing.T) {
	t.Parallel()

	s := New(4)
	_, _, keyA, keyB := findKeys(t, s) // guaranteed cross-shard pair
	s.Set(keyA, "alice")

	const iterations = 2000
	var wg sync.WaitGroup
	renameLoop := func(from, to string) {
		defer wg.Done()
		for range iterations {
			err := s.Rename(from, to)
			if err != nil && !errors.Is(err, ErrNotFound) {
				t.Errorf("Rename(%q,%q): %v", from, to, err)
				return
			}
		}
	}
	wg.Add(2)
	go renameLoop(keyA, keyB) // locks shards (i, j) ...
	go renameLoop(keyB, keyA) // ... and (j, i): deadlock bait

	// Watchdog: a deadlock here would hang the job, not fail it.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("rename storm wedged: opposite-direction cross-shard renames deadlocked")
	}

	// The session must have survived in exactly one place.
	vA, okA := s.Get(keyA)
	vB, okB := s.Get(keyB)
	if okA == okB {
		t.Fatalf("session lost or duplicated: %q present=%v, %q present=%v", keyA, okA, keyB, okB)
	}
	if v := vA + vB; v != "alice" {
		t.Fatalf("session value corrupted: %q", v)
	}
	if got := s.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
}

func TestConcurrentSetGetAcrossShards(t *testing.T) {
	t.Parallel()

	s := New(8)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				key := fmt.Sprintf("sess-%d-%d", i, j)
				s.Set(key, "u")
				if _, ok := s.Get(key); !ok {
					t.Errorf("lost own write for %q", key)
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := s.Len(); got != 16*200 {
		t.Fatalf("Len = %d, want %d", got, 16*200)
	}
}

func ExampleStore_Rename() {
	s := New(4)
	s.Set("old", "alice")
	if err := s.Rename("old", "new"); err != nil {
		fmt.Println("err:", err)
		return
	}
	v, ok := s.Get("new")
	fmt.Println(v, ok)
	// Output: alice true
}
```

## Review

Re-read the guard in `Rename` until the distinction sticks: the fast-path
condition is `shardIndex(oldKey) == shardIndex(newKey)`, never `oldKey ==
newKey`. Key equality is about *semantics* (a self-rename); index equality is
about *lock identity* (would the two-lock path lock one mutex twice). Confusing
them produces a store that passes every fixed-key test and self-deadlocks in
production the first time two session IDs collide onto a shard — and because
the seed randomizes placement, no hard-coded test key pair can pin this; the
test must search for colliding keys the way `findKeys` does.

The other lesson is about `Len` and any future multi-shard read: this module
deliberately made `Len` lock-by-lock (approximate under load, deadlock-free),
matching the `Snapshot` contract from exercise 2. If a consumer ever needs an
exact count, the hold-all version must lock shards in ascending index order —
by then the phrase should sound familiar. Confirm with
`go test -count=1 -race ./...`; to see the watchdog earn its keep, flip the
`lo, hi` acquisition in `Rename` to lock `hi` first in one of the two paths
and watch the storm test fail with the wedge diagnostic instead of hanging
your terminal.

## Resources

- [hash/maphash package](https://pkg.go.dev/hash/maphash) — `MakeSeed` and `String`; why the seed randomizes placement per process.
- [sync package — RWMutex](https://pkg.go.dev/sync#RWMutex) — reader/writer semantics used per shard.
- [Go maps in action — concurrency](https://go.dev/blog/maps) — why every map access here is under a shard lock.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-rwmutex-upgrade-deadlock.md](07-rwmutex-upgrade-deadlock.md)
