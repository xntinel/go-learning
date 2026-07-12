# 13. Lock-Free Hash Map

Building a correct, concurrent hash map without locks is one of the hardest
exercises in systems programming. The challenge is not the data structure alone:
it is keeping the probe-sequence invariant intact while multiple goroutines
simultaneously read, insert, delete, and resize the table. This lesson
implements a complete `lockfreemap` package using open addressing with Robin
Hood probing, immutable-entry copy-on-write semantics, and an incremental
migration protocol that lets one segment grow into the next without blocking
any concurrent operation.

```text
lockfreemap/
  go.mod
  map.go
  map_test.go
  cmd/demo/main.go
```

## Concepts

### Why "Lock-Free" Is Not Just "No Mutex"

A data structure is lock-free when at least one thread makes progress in a
finite number of steps regardless of what other threads do. A mutex satisfies
that definition only if the OS scheduler is fair; if the thread holding the
lock is descheduled forever, no other thread can proceed. The Go runtime's
goroutine scheduler is cooperative and preemptive, so a live-locked mutex
holder blocks all waiters. The formal guarantee of lock-free algorithms is
stronger: even under adversarial scheduling, the system as a whole does not
stall.

The primitive behind lock-free algorithms is compare-and-swap (CAS): atomically
compare a memory location with an expected value and replace it only if the
comparison succeeds. In Go, `atomic.Pointer[T].CompareAndSwap(old, new)` is
the generic CAS on pointers.

### The Immutable Entry Pattern

The fundamental barrier to lock-free shared data structures is partial reads:
if a goroutine reads a struct field-by-field and another goroutine updates it
concurrently, the reader may see a half-written value. The immutable entry
pattern eliminates this:

1. Every slot stores an `atomic.Pointer[entry[K, V]]`.
2. An entry struct is never modified after creation.
3. To update a slot, allocate a new entry and CAS the pointer.

Because the pointer swap is atomic, a reader that loads the pointer always
sees either the old complete entry or the new complete entry, never a partial
one. The Go memory model guarantees that the fields of a value written before
`Store` are visible to any goroutine that observes the stored pointer.

A subtle variant of this rule: if an insertion function carries an in-flight
entry pointer across iterations (e.g., for Robin Hood reinsertion) and then
writes to one of the entry's fields after the CAS has already published it to a
slot, that is a data race. The correct discipline is to track the in-flight
content as separate scalars and allocate a fresh `*entry` for every slot
attempt.

### Robin Hood Hashing

Standard linear probing distributes probe lengths unevenly: entries near their
ideal slot have short probes; entries far from their ideal slot have long
probes. Under concurrency, long probes create contention hot-spots.

Robin Hood hashing redistributes this variance. Each entry records its probe
distance `d` (0 = landed in the ideal slot). During insertion at probe
distance `d`, if the current slot holds an entry with probe `d' < d`, the
inserter steals that slot ("rob from the rich, give to the poor"), displaces
the existing entry, and continues the insertion with the displaced entry. The
invariant maintained:

    For every slot i: entry[i].probe == distance(entry[i].idealSlot, i)

This invariant enables an early exit in Get: if while probing at distance `d`
we encounter an entry whose probe is less than `d`, the target key cannot be
present anywhere further in the sequence (it would have been displaced here
first).

### The Eviction Ownership Problem

In a concurrent Robin Hood implementation, a successful steal changes a slot
atomically. The evicted live entry is immediately "in limbo": it is no longer
in any slot, but it also has not been reinserted. The critical error is to
return failure from the insertion function after a steal has already succeeded:
the outer retry loop would then reinsert only the original key, leaving the
evicted key permanently lost.

The fix is ownership transfer: when a steal evicts a live entry, the stealing
goroutine takes ownership of the evicted entry and is responsible for
reinserting it via a dedicated retry loop. The outer loop only handles failures
of the steal CAS itself (before ownership transfers), not failures that happen
after.

### The Slot State Machine

A slot transitions through states via CAS. The states are:

```text
nil (empty) --[CAS with live entry]--> *entry{kindLive}
                                            |
                          [CAS with tomb]   |   [CAS with relay during migration]
                                  v                     v
                           *entry{kindTomb}       *entry{kindRelay}
```

- `nil`: slot has never been used.
- `kindLive`: slot holds a live key-value pair.
- `kindTomb`: slot held a key that was deleted; tombstones preserve the
  probe-sequence invariant so that Gets do not short-circuit prematurely.
- `kindRelay`: slot has been migrated to the next segment; operations that
  encounter a relay follow the chain to the new segment.

Tombstones accumulate over time and inflate the load factor. They are purged
when the table grows: migration skips tombstones and copies only live entries,
effectively compacting the table. They may also be replaced by Robin Hood
stealing (a tombstone with a small probe distance is overwritten by a new live
entry with a higher probe distance).

### Incremental Migration and Deferred Promotion

When the load factor exceeds 70%, a new segment of double the capacity is
allocated and stored as `seg.next`. Migration is incremental: each subsequent
Put or Delete migrates a small batch of slots from the old segment before
performing its own operation.

A subtle livelock arises from premature promotion. The migration index
`migIdx` is atomically incremented per batch. When `migIdx >= cap`, all
batches have been CLAIMED but not necessarily FINISHED. If we promote `m.seg`
to `next` at that moment, new Puts start flowing into `next`. If the migration
workers are still inserting into `next`, they contend with the new Puts. If
`next` fills up (triggering its own grow), the migration workers trying to
insert into `next` encounter relay slots and spin forever.

The fix: defer promotion until a separate `migDone` counter (incremented per
fully migrated slot) reaches `cap`. This guarantees `next` only receives
entries from migration, keeping its load below 35% of its capacity — far from
the 70% threshold that would trigger a second grow.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/46-capstone-concurrency-deep-dive/13-lock-free-hash-map/13-lock-free-hash-map/cmd/demo
cd go-solutions/46-capstone-concurrency-deep-dive/13-lock-free-hash-map/13-lock-free-hash-map
```

This is a library. Verification is `go test -count=1 -race ./...`.

### Exercise 1: Slot State Machine and Immutable Entry

Create `map.go`:

```go
package lockfreemap

import (
	"hash/maphash"
	"runtime"
	"sync/atomic"
)

const (
	initCap      uint64 = 16 // must be a power of 2
	growAt              = 70 // grow when total*100/cap >= growAt
	migrateBatch uint64 = 32
)

// entryKind classifies the state of a slot.
type entryKind uint8

const (
	kindLive  entryKind = 1
	kindTomb  entryKind = 2
	kindRelay entryKind = 3
)

// entry is immutable once created. All fields are set before the struct is
// stored in a slot and never written after. This guarantee — one atomic
// pointer swap per entry, no field mutations — is what makes reads safe
// without locks: a Load always returns either the old complete entry or
// the new complete entry, never a partial write.
type entry[K comparable, V any] struct {
	kind  entryKind
	probe int32  // Robin Hood probe distance from the ideal slot (0 = ideal)
	hash  uint64 // pre-computed hash; avoids re-hashing during probing
	key   K
	val   V
}

// seg is a fixed-size open-addressed table.
// All mutable state is accessed through atomic operations.
type seg[K comparable, V any] struct {
	slots []atomic.Pointer[entry[K, V]]
	cap   uint64 // length of slots; always a power of 2

	live  atomic.Int64 // live (non-tomb) entry count
	total atomic.Int64 // live + tombstone count (used for load-factor check)

	// migIdx: next batch to claim. migDone: slots fully migrated.
	// Promotion happens only when migDone >= cap, not when migIdx >= cap.
	// This prevents next from receiving Put traffic before it is fully
	// populated, which would allow next to grow and develop relay slots
	// while migration workers are still trying to insert into it.
	migIdx  atomic.Uint64
	migDone atomic.Uint64
	next    atomic.Pointer[seg[K, V]]
}

func newSeg[K comparable, V any](cap uint64) *seg[K, V] {
	return &seg[K, V]{
		slots: make([]atomic.Pointer[entry[K, V]], cap),
		cap:   cap,
	}
}

// Hasher is a pure function: same key and seed always produce the same hash.
// The seed is fixed per Map instance, so probe sequences are stable within
// one run but unpredictable across restarts, preventing hash-flooding.
type Hasher[K comparable] func(key K, seed maphash.Seed) uint64

// StringHasher returns a Hasher for string keys backed by hash/maphash.
func StringHasher() Hasher[string] {
	return func(key string, seed maphash.Seed) uint64 {
		return maphash.String(seed, key)
	}
}

// Map is a lock-free concurrent hash map with Robin Hood open addressing.
//
// Correctness guarantees:
//   - Get is always safe without locks; a single atomic.Load gives a
//     consistent snapshot of any slot.
//   - Put and Delete are lock-free: at least one goroutine makes progress
//     at each CAS contention point.
//   - Linearizability: each operation takes effect at the instant its CAS
//     succeeds; no operation is ever partially visible to a concurrent reader.
type Map[K comparable, V any] struct {
	seed   maphash.Seed
	hasher Hasher[K]
	seg    atomic.Pointer[seg[K, V]]
}

// New returns an initialized Map with the given hasher.
func New[K comparable, V any](hasher Hasher[K]) *Map[K, V] {
	m := &Map[K, V]{
		seed:   maphash.MakeSeed(),
		hasher: hasher,
	}
	m.seg.Store(newSeg[K, V](initCap))
	return m
}

func (m *Map[K, V]) hashKey(key K) uint64 {
	h := m.hasher(key, m.seed)
	if h == 0 {
		h = 1 // reserve 0 as a convenient nil-entry sentinel if ever needed
	}
	return h
}

func idealSlot(h, cap uint64) uint64 {
	return h & (cap - 1) // cap is always a power of 2
}

func (m *Map[K, V]) zeroVal() V { var z V; return z }
```

### Exercise 2: Get With Robin Hood Short-Circuit

Add `Get` and its helper to `map.go`:

```go
// Get returns the value for key and whether the key is present.
// Get never blocks. Under a concurrent resize it probes the old segment
// first; encountering a relay slot follows the chain to the new segment.
func (m *Map[K, V]) Get(key K) (V, bool) {
	h := m.hashKey(key)
	s := m.seg.Load()
	for {
		v, ok, definitive := getInSeg(s, key, h)
		if definitive {
			return v, ok
		}
		next := s.next.Load()
		if next == nil {
			return m.zeroVal(), false
		}
		s = next
	}
}

// getInSeg probes segment s for key with pre-computed hash h.
// definitive=false means a relay slot was hit; the caller must consult
// the next segment.
func getInSeg[K comparable, V any](s *seg[K, V], key K, h uint64) (v V, ok, definitive bool) {
	cap := s.cap
	ideal := idealSlot(h, cap)
	for d := int32(0); uint64(d) < cap; d++ {
		idx := (ideal + uint64(d)) & (cap - 1)
		e := s.slots[idx].Load()

		if e == nil {
			// Empty slot: Robin Hood invariant guarantees the key is absent.
			return v, false, true
		}
		switch {
		case e.kind == kindRelay:
			return v, false, false // must follow chain
		case e.probe < d:
			// Robin Hood short-circuit: the key cannot be further along
			// because any entry displaced past this point would have had
			// probe distance >= d.
			return v, false, true
		case e.hash == h && e.key == key:
			if e.kind == kindLive {
				return e.val, true, true
			}
			return v, false, true // kindTomb: deleted
		}
	}
	return v, false, true
}
```

The `e.probe < d` short-circuit is the payoff of maintaining the displacement
invariant: any entry at a shorter probe distance proves the target was never
displaced this far.

### Exercise 3: Put With Robin Hood Insertion and Eviction Ownership

Add `Put`, `putInSeg`, and `reinsert` to `map.go`:

```go
// Put inserts or updates key with val.
// Returns the previous value and whether a previous value existed.
func (m *Map[K, V]) Put(key K, val V) (V, bool) {
	h := m.hashKey(key)
	for {
		s := m.seg.Load()
		if s.next.Load() != nil {
			m.helpMigrate(s)
			continue
		}
		if s.total.Load()*100 >= int64(s.cap)*growAt {
			m.startGrow(s)
			continue
		}
		prev, existed, ok, evicted := m.putInSeg(s, key, val, h)
		if ok {
			if evicted != nil {
				m.reinsert(s, evicted)
			}
			return prev, existed
		}
		runtime.Gosched()
	}
}

// putInSeg attempts a single Robin Hood insertion pass into s.
// Returns (prevVal, existed, success, evicted).
//
//   - success=false: a CAS raced; the caller should retry.
//   - evicted!=nil: a steal evicted a live entry that the caller must
//     reinsert. Ownership transfers to the caller on return.
//
// Counting invariants:
//   - nil slot claimed: live+1, total+1.
//   - Tombstone reclaimed (own key or Robin Hood steal): live+1, total unchanged.
//   - Live-for-live Robin Hood steal: live and total unchanged here; the
//     evicted entry's future reinsertion adds live+1, total+1, giving the
//     correct net +1 for the new key.
func (m *Map[K, V]) putInSeg(s *seg[K, V], key K, val V, h uint64) (prev V, existed bool, ok bool, evicted *entry[K, V]) {
	cap := s.cap
	ideal := idealSlot(h, cap)

	for d := int32(0); uint64(d) < cap; d++ {
		idx := (ideal + uint64(d)) & (cap - 1)
		newE := &entry[K, V]{kind: kindLive, probe: d, hash: h, key: key, val: val}
		e := s.slots[idx].Load()

		switch {
		case e == nil:
			if !s.slots[idx].CompareAndSwap(nil, newE) {
				return m.zeroVal(), false, false, nil
			}
			s.live.Add(1)
			s.total.Add(1)
			return m.zeroVal(), false, true, nil

		case e.kind == kindRelay:
			return m.zeroVal(), false, false, nil

		case e.kind == kindLive && e.hash == h && e.key == key:
			updated := &entry[K, V]{kind: kindLive, probe: e.probe, hash: h, key: key, val: val}
			if !s.slots[idx].CompareAndSwap(e, updated) {
				return m.zeroVal(), false, false, nil
			}
			return e.val, true, true, nil

		case e.kind == kindTomb && e.hash == h && e.key == key:
			if !s.slots[idx].CompareAndSwap(e, newE) {
				return m.zeroVal(), false, false, nil
			}
			s.live.Add(1) // total unchanged: slot was already counted
			return m.zeroVal(), false, true, nil

		case e.probe < d:
			// Robin Hood steal: our probe exceeds the current resident's.
			if !s.slots[idx].CompareAndSwap(e, newE) {
				return m.zeroVal(), false, false, nil
			}
			if e.kind == kindTomb {
				// Tombstone steal: discard tomb, live+1 (tomb→live).
				s.live.Add(1)
				return m.zeroVal(), false, true, nil
			}
			// Live steal: no live/total change here (live-for-live slot
			// swap). The caller's reinsert will add live+1, total+1
			// for the evicted entry's new slot, giving the correct +1.
			return m.zeroVal(), false, true, e
		}
	}
	return m.zeroVal(), false, false, nil
}

// reinsert inserts an evicted live entry back into the active segment.
// It follows migration chains and has its own CAS retry loop.
// Recursion depth is bounded by the maximum Robin Hood probe distance.
func (m *Map[K, V]) reinsert(s *seg[K, V], evicted *entry[K, V]) {
	for {
		target := s
		for nn := target.next.Load(); nn != nil; nn = target.next.Load() {
			target = nn
		}
		_, _, inserted, nextEvicted := m.putInSeg(target, evicted.key, evicted.val, evicted.hash)
		if inserted {
			if nextEvicted != nil {
				m.reinsert(target, nextEvicted)
			}
			return
		}
		if nn := s.next.Load(); nn != nil {
			s = nn
		} else {
			runtime.Gosched()
		}
	}
}
```

The key correctness rule: a goroutine that completes a steal CAS owns the
evicted entry and must not abandon it. Returning from `putInSeg` with
`evicted != nil` and `ok = true` transfers that ownership to the caller.

### Exercise 4: Incremental Migration

Add the migration helpers to `map.go`:

```go
// startGrow allocates a next segment of double capacity and begins migration.
// Concurrent callers race on the CAS; only one installs next, the rest help.
func (m *Map[K, V]) startGrow(s *seg[K, V]) {
	next := newSeg[K, V](s.cap * 2)
	if !s.next.CompareAndSwap(nil, next) {
		m.helpMigrate(s)
		return
	}
	m.helpMigrate(s)
}

// helpMigrate claims and migrates migrateBatch slots from s to s.next.
// Segment promotion is deferred until migDone >= cap to prevent premature
// promotion (see the Concepts section on deferred promotion).
func (m *Map[K, V]) helpMigrate(s *seg[K, V]) {
	next := s.next.Load()
	if next == nil {
		return
	}
	start := s.migIdx.Add(migrateBatch) - migrateBatch
	if start >= s.cap {
		if s.migDone.Load() >= s.cap {
			m.seg.CompareAndSwap(s, next)
		}
		return
	}
	end := start + migrateBatch
	if end > s.cap {
		end = s.cap
	}
	for i := start; i < end; i++ {
		m.migrateSlot(s, next, i)
		if s.migDone.Add(1) >= s.cap {
			m.seg.CompareAndSwap(s, next)
		}
	}
}

// migrateSlot atomically marks slot i as a relay and re-inserts any live
// entry into next. Tombstones are dropped, compacting the new table.
func (m *Map[K, V]) migrateSlot(old, next *seg[K, V], i uint64) {
	relay := &entry[K, V]{kind: kindRelay}
	var e *entry[K, V]
	for {
		e = old.slots[i].Load()
		if e != nil && e.kind == kindRelay {
			return
		}
		if old.slots[i].CompareAndSwap(e, relay) {
			break
		}
		runtime.Gosched()
	}
	if e == nil || e.kind != kindLive {
		return
	}
	for {
		_, _, inserted, evicted := m.putInSeg(next, e.key, e.val, e.hash)
		if inserted {
			if evicted != nil {
				m.reinsert(next, evicted)
			}
			return
		}
		runtime.Gosched()
	}
}
```

### Exercise 5: Delete and Utilities

Add `Delete`, `Len`, and `ForEach` to `map.go`:

```go
// Delete removes key from the map.
// Returns the previous value and whether the key was present.
// Delete places a tombstone rather than removing the slot to preserve the
// Robin Hood probe-sequence invariant.
func (m *Map[K, V]) Delete(key K) (V, bool) {
	h := m.hashKey(key)
	for {
		s := m.seg.Load()
		if s.next.Load() != nil {
			m.helpMigrate(s)
		}
		cap := s.cap
		ideal := idealSlot(h, cap)

		relay := false
		var found *entry[K, V]
		var foundIdx uint64

		for d := int32(0); uint64(d) < cap; d++ {
			idx := (ideal + uint64(d)) & (cap - 1)
			e := s.slots[idx].Load()
			if e == nil {
				break
			}
			if e.kind == kindRelay {
				relay = true
				break
			}
			if e.probe < d {
				break
			}
			if e.hash != h || e.key != key {
				continue
			}
			if e.kind == kindTomb {
				return m.zeroVal(), false
			}
			if e.kind == kindLive {
				found = e
				foundIdx = idx
				break
			}
		}

		if relay {
			if next := s.next.Load(); next != nil {
				m.seg.CompareAndSwap(s, next)
			}
			runtime.Gosched()
			continue
		}
		if found == nil {
			return m.zeroVal(), false
		}

		tomb := &entry[K, V]{kind: kindTomb, probe: found.probe, hash: h, key: key}
		if !s.slots[foundIdx].CompareAndSwap(found, tomb) {
			runtime.Gosched()
			continue
		}
		s.live.Add(-1)
		return found.val, true
	}
}

// Len returns the number of live key-value pairs in the map.
// It is a snapshot; concurrent operations may change the count immediately.
func (m *Map[K, V]) Len() int {
	s := m.seg.Load()
	n := s.live.Load()
	if next := s.next.Load(); next != nil {
		n += next.live.Load()
	}
	return int(n)
}

// ForEach calls fn for each live key-value pair in the map.
// fn returning false stops the iteration. ForEach may or may not see
// entries added or deleted concurrently; it guarantees that every entry
// live at the moment ForEach begins will be visited at most once.
func (m *Map[K, V]) ForEach(fn func(key K, val V) bool) {
	s := m.seg.Load()
	for i := uint64(0); i < s.cap; i++ {
		e := s.slots[i].Load()
		if e != nil && e.kind == kindLive {
			if !fn(e.key, e.val) {
				return
			}
		}
	}
}
```

### Exercise 6: Test Suite

Create `map_test.go`:

```go
package lockfreemap

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
)

func TestGetMissingKey(t *testing.T) {
	t.Parallel()
	m := New[string, int](StringHasher())
	if v, ok := m.Get("absent"); ok || v != 0 {
		t.Fatalf("Get(absent) = (%v, %v), want (0, false)", v, ok)
	}
}

func TestPutAndGet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key string
		val int
	}{
		{"hello", 1}, {"world", 2}, {"foo", 3}, {"bar", 4},
	}
	m := New[string, int](StringHasher())
	for _, tc := range cases {
		prev, existed := m.Put(tc.key, tc.val)
		if existed || prev != 0 {
			t.Errorf("Put(%q): prev=%v existed=%v, want (0, false)", tc.key, prev, existed)
		}
	}
	for _, tc := range cases {
		v, ok := m.Get(tc.key)
		if !ok || v != tc.val {
			t.Errorf("Get(%q) = (%v, %v), want (%v, true)", tc.key, v, ok, tc.val)
		}
	}
}

func TestPutUpdatesExistingKey(t *testing.T) {
	t.Parallel()
	m := New[string, int](StringHasher())
	m.Put("k", 1)
	prev, existed := m.Put("k", 2)
	if !existed || prev != 1 {
		t.Fatalf("Put(k,2): prev=%v existed=%v, want (1, true)", prev, existed)
	}
	if v, _ := m.Get("k"); v != 2 {
		t.Fatalf("Get(k) = %v after update, want 2", v)
	}
}

func TestDeleteRemovesKey(t *testing.T) {
	t.Parallel()
	m := New[string, int](StringHasher())
	m.Put("k", 42)
	prev, ok := m.Delete("k")
	if !ok || prev != 42 {
		t.Fatalf("Delete(k): prev=%v ok=%v, want (42, true)", prev, ok)
	}
	if _, ok := m.Get("k"); ok {
		t.Fatal("Get(k) returned true after Delete")
	}
}

func TestDeleteMissingKey(t *testing.T) {
	t.Parallel()
	m := New[string, int](StringHasher())
	prev, ok := m.Delete("ghost")
	if ok || prev != 0 {
		t.Fatalf("Delete(ghost) = (%v, %v), want (0, false)", prev, ok)
	}
}

func TestLen(t *testing.T) {
	t.Parallel()
	m := New[string, int](StringHasher())
	for i := range 10 {
		m.Put(fmt.Sprintf("k%d", i), i)
	}
	if n := m.Len(); n != 10 {
		t.Fatalf("Len() = %d, want 10", n)
	}
	m.Delete("k0")
	if n := m.Len(); n != 9 {
		t.Fatalf("Len() after Delete = %d, want 9", n)
	}
}

func TestGrowthUnderLoad(t *testing.T) {
	t.Parallel()
	const n = 10_000
	m := New[string, int](StringHasher())
	for i := range n {
		m.Put(fmt.Sprintf("key-%d", i), i)
	}
	for i := range n {
		k := fmt.Sprintf("key-%d", i)
		v, ok := m.Get(k)
		if !ok || v != i {
			t.Fatalf("Get(%q) = (%v, %v) after growth, want (%v, true)", k, v, ok, i)
		}
	}
}

func TestForEachVisitsAllLiveEntries(t *testing.T) {
	t.Parallel()
	const n = 200
	m := New[string, int](StringHasher())
	for i := range n {
		m.Put(fmt.Sprintf("k%d", i), i)
	}
	for i := 0; i < n; i += 2 {
		m.Delete(fmt.Sprintf("k%d", i))
	}
	seen := make(map[string]int)
	m.ForEach(func(key string, val int) bool {
		seen[key] = val
		return true
	})
	if len(seen) != n/2 {
		t.Fatalf("ForEach visited %d entries, want %d", len(seen), n/2)
	}
}

// TestConcurrentPutGet runs 8 writers on disjoint key spaces and verifies
// that every inserted key is found after all writers complete.
func TestConcurrentPutGet(t *testing.T) {
	t.Parallel()
	const (
		goroutines = 8
		perG       = 500
	)
	m := New[string, int](StringHasher())
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range perG {
				m.Put(fmt.Sprintf("g%d-k%d", g, i), g*perG+i)
			}
		}(g)
	}
	wg.Wait()
	for g := range goroutines {
		for i := range perG {
			key := fmt.Sprintf("g%d-k%d", g, i)
			v, ok := m.Get(key)
			if !ok {
				t.Errorf("Get(%q): not found after concurrent Put", key)
				continue
			}
			if want := g*perG + i; v != want {
				t.Errorf("Get(%q) = %d, want %d", key, v, want)
			}
		}
	}
}

// TestConcurrentMixedOps validates absence of data races via -race.
func TestConcurrentMixedOps(t *testing.T) {
	t.Parallel()
	const (
		goroutines = 16
		ops        = 2_000
		keyRange   = 100
	)
	m := New[string, int](StringHasher())
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(g), 0))
			for range ops {
				key := fmt.Sprintf("k%d", rng.IntN(keyRange))
				switch rng.IntN(3) {
				case 0:
					m.Put(key, rng.Int())
				case 1:
					m.Get(key)
				case 2:
					m.Delete(key)
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestRobinHoodProbeDistanceBound verifies the maximum probe distance stays
// within the expected Robin Hood bound after loading the map to 70%.
func TestRobinHoodProbeDistanceBound(t *testing.T) {
	t.Parallel()
	const n = 1_000
	m := New[string, int](StringHasher())
	for i := range n {
		m.Put(fmt.Sprintf("key-%d", i), i)
	}
	s := m.seg.Load()
	maxProbe := int32(0)
	for i := uint64(0); i < s.cap; i++ {
		e := s.slots[i].Load()
		if e != nil && e.kind == kindLive && e.probe > maxProbe {
			maxProbe = e.probe
		}
	}
	// Robin Hood keeps max probe well below O(log n); cap at 3*log2(n).
	bound := int32(33) // 3 * log2(1000) ≈ 30
	if maxProbe > bound {
		t.Errorf("max probe distance = %d, want <= %d", maxProbe, bound)
	}
}

// ExampleMap_Get demonstrates basic Put and Get.
func ExampleMap_Get() {
	m := New[string, int](StringHasher())
	m.Put("answer", 42)
	v, ok := m.Get("answer")
	fmt.Println(v, ok)
	// Output:
	// 42 true
}

// ExampleMap_Delete demonstrates that a deleted key is no longer found.
func ExampleMap_Delete() {
	m := New[string, string](StringHasher())
	m.Put("lang", "Go")
	m.Delete("lang")
	_, ok := m.Get("lang")
	fmt.Println(ok)
	// Output:
	// false
}

// Your turn: add TestConcurrentDeleteThenGet that spawns 4 goroutines each
// deleting a key immediately after inserting it and verifies that a concurrent
// Get never sees a value for a key whose Delete has already returned (zero, false).
```

### Exercise 7: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	lockfreemap "example.com/lockfreemap"
)

func main() {
	m := lockfreemap.New[string, int](lockfreemap.StringHasher())

	const (
		writers   = 4
		perWriter = 2_500
	)
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWriter {
				m.Put(fmt.Sprintf("w%d-k%d", w, i), w*perWriter+i)
			}
		}(w)
	}
	wg.Wait()

	fmt.Printf("after %d concurrent inserts: Len=%d\n", writers*perWriter, m.Len())

	var missed atomic.Int64
	var rg sync.WaitGroup
	for w := range writers {
		rg.Add(1)
		go func(w int) {
			defer rg.Done()
			for i := range perWriter {
				_, ok := m.Get(fmt.Sprintf("w%d-k%d", w, i))
				if !ok {
					missed.Add(1)
				}
			}
		}(w)
	}
	rg.Wait()

	if n := missed.Load(); n > 0 {
		fmt.Printf("FAIL: %d keys not found after concurrent inserts\n", n)
	} else {
		fmt.Println("all keys verified OK")
	}

	count := 0
	m.ForEach(func(_ string, _ int) bool {
		count++
		return count < 5
	})
	fmt.Printf("ForEach visited (up to 5): %d entries\n", count)
}
```

Run with:

```bash
go run ./cmd/demo
```

## Common Mistakes

**Wrong**: Mutating an entry struct after CAS-ing it into a slot.
```go
// Wrong: e is written after Store; a concurrent reader sees partial state.
e := &entry[K, V]{kind: kindLive}
s.slots[i].Store(e)
e.probe = d // data race: another goroutine may have already loaded e
```
**What happens**: The race detector flags it. Readers may observe a valid kind
but a zero probe or key.
**Fix**: Set all fields before the first Store or CAS. In an insertion loop that
carries an in-flight entry across iterations, track the content as separate
scalars (`curKey`, `curVal`, etc.) and allocate a fresh `*entry` at each probe
position.

**Wrong**: Returning failure from the insertion function after a steal CAS has
already succeeded, then letting the outer loop retry.
```go
// Wrong: steal succeeded (newE is in the slot), but then CAS for evicted entry fails.
// The outer loop retries with the original key. The evicted entry is lost.
if !s.slots[idx].CompareAndSwap(evicted, newE) { return ..., false }
// if CAS above succeeds, evicted is in limbo:
if !s.slots[j].CompareAndSwap(nil, evicted) { return ..., false } // BUG
```
**What happens**: The evicted entry disappears from the map with no error.
`Get` returns false for a key that was never deleted.
**Fix**: After a successful steal, transfer ownership of the evicted entry to
the caller as a separate return value. The caller retries the evicted
reinsertion independently from the outer Put loop.

**Wrong**: Promoting `m.seg` to `next` as soon as all migration batches are
CLAIMED rather than FINISHED.
```go
// Wrong: migIdx >= cap means all batches claimed, not all slots migrated.
if s.migIdx.Load() >= s.cap {
    m.seg.CompareAndSwap(s, next) // premature
}
```
**What happens**: New Puts start flowing into `next` while migration workers
are still inserting entries from `old` into `next`. If the new Puts push `next`
to 70% load, `next` grows (develops its own `next.next`). Migration workers
calling `putInSeg(next, ...)` encounter relay slots in `next` and spin forever —
a livelock.
**Fix**: Use a separate `migDone` counter (incremented after each slot is fully
migrated). Promote only when `migDone >= cap`.

**Wrong**: Using `break` inside a `switch` inside a `for` loop expecting it to
exit the loop.
```go
for d := ...; ... {
    switch e.kind {
    case kindRelay:
        break // exits switch only; the for loop continues
    }
}
```
**What happens**: The relay is silently ignored; the loop continues reading
additional slots without following the chain.
**Fix**: Extract the switch into a function that returns a boolean, or use a
labeled break: `break outerLabel`.

## Verification

From `~/go-exercises/lockfreemap`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass. The race detector (`-race`) is mandatory for concurrent
data structures; it has caught real bugs in this implementation pattern
(specifically the write-after-publish race on `entry.probe`).

Add a `BenchmarkConcurrentGet` that calls `m.Get` from `b.RunParallel` and
confirm throughput scales with `GOMAXPROCS`:

```bash
go test -bench=. -benchtime=5s -benchmem ./...
```

## Summary

- A slot's `atomic.Pointer[entry]` plus immutable entries eliminates all
  partial-read races: a single `Load` is a safe, consistent snapshot.
- Track in-flight entries as scalars across loop iterations; allocate a fresh
  `*entry` for each CAS attempt so the struct is never written after storage.
- Robin Hood probing bounds maximum probe distance to O(log n); the `e.probe < d`
  short-circuit in Get converts that bound into an O(1) early exit.
- CAS retry loops make operations lock-free: if a CAS fails, at least one
  other goroutine succeeded.
- After a successful steal, the stolen goroutine owns the evicted entry and
  must reinsert it via its own retry loop — not abandon it on CAS failure.
- Defer segment promotion until `migDone >= cap`; premature promotion allows
  the new segment to grow during migration, causing a livelock in migration
  workers.
- Tombstones preserve the Robin Hood probe-sequence invariant after Delete;
  they are compacted away when the segment migrates.
- Run every test under `go test -race`; the data race detector catches
  ordering bugs that correctness tests alone will not find.

## What's Next

Next: [Direct System Calls](../../47-capstone-systems-and-kernel/01-direct-syscalls/01-direct-syscalls.md).

## Resources

- Go Memory Model: https://go.dev/ref/mem — defines when stores by one goroutine become visible to another; the foundation for all lock-free reasoning in Go.
- `sync/atomic` package documentation: https://pkg.go.dev/sync/atomic — canonical reference for `atomic.Pointer`, `CompareAndSwap`, and the full set of atomic types.
- `hash/maphash` package documentation: https://pkg.go.dev/hash/maphash — `MakeSeed`, `String`, and the seeding contract that prevents hash-flooding.
- Pedro Celis, "Robin Hood Hashing" (University of Waterloo, 1986) — original thesis defining the displacement invariant and proving the O(log n) probe-distance bound.
- Cliff Click, "A Lock-Free Concurrent Hash Table" (JavaOne 2007) — the NonBlockingHashMap design; the slot state machine and incremental migration in this lesson are directly inspired by it.
