# Exercise 1: Build a Swiss-Table-Style Open-Addressing Set

The built-in map exposes no probe counter, so the only way to build intuition for
how a Swiss Table behaves under load is to build one. This exercise implements a
generic open-addressing set that mirrors the Go 1.24 design — groups of 8 slots, a
per-slot control byte, H1/H2 hash splitting, linear probing, tombstones, and
grow-and-rehash — and exposes a `ProbeStats()` method so probe length becomes a
number you can measure.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
swissset/                  independent module: example.com/swissset
  go.mod                   go 1.24 (maphash.Comparable, math/bits)
  swissset.go              SwissSet[K comparable]: Insert, Contains, Delete, Len,
                           All, Groups, ProbeStats; groups of 8, control bytes,
                           H1/H2 split, linear probing, tombstones, grow
  cmd/
    demo/
      main.go              runnable demo: build, query, delete, probe stats
  swissset_test.go         differential test vs map[K]struct{}, tombstone reuse,
                           grow consistency, Example with fixed // Output
```

- Files: `swissset.go`, `cmd/demo/main.go`, `swissset_test.go`.
- Implement: `SwissSet[K comparable]` with `New`, `Insert`, `Contains`, `Delete`, `Len`, `All`, `Groups`, and `ProbeStats`, hashing keys with `hash/maphash.Comparable` and matching control bytes with a bitmask scanned via `math/bits.TrailingZeros8`.
- Test: a differential/property test running identical random op sequences against `SwissSet` and a built-in `map[K]struct{}`; tombstone-reuse and grow-consistency table cases; an `Example` printing `slices.Sorted`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/swissset/cmd/demo
cd ~/go-exercises/swissset
go mod init example.com/swissset
go mod edit -go=1.24
```

### The control byte is the whole design

Every slot in the table has a one-byte control value that encodes both its state and
a fingerprint of its key. A full slot stores H2 — the low 7 bits of the hash, a value
in `0..127`, so its top bit is always clear. An empty slot is `0b1000_0000` (`0x80`)
and a tombstone is `0b1111_1110` (`0xFE`); both have the top bit set. That single top
bit is what lets one masked comparison classify all 8 slots at once: `ctrl & 0x80`
distinguishes occupied from free, and an equality test against H2 finds candidate
matches. The zero value of a byte is `0`, which is a *valid* H2, so a freshly
allocated group must have every control byte set to `ctrlEmpty` before use — forget
that and empty slots masquerade as full slots holding fingerprint 0.

The real runtime compares all 8 bytes with a single SIMD instruction. We emulate that
with a small loop that builds an 8-bit mask (`matchByte` sets bit `i` when
`ctrl[i] == h2`), then walk the set bits with `bits.TrailingZeros8`, clearing the
lowest set bit each iteration with `m &= m - 1`. That is the same candidate-filter
the hardware does; only the mechanism differs.

### H1 selects the group, H2 filters within it

`maphash.Comparable(seed, key)` returns a 64-bit hash. We split it: H2 is the low 7
bits (`hash & 0x7f`), stored in the control byte; H1 is the rest (`hash >> 7`), and
`H1 % numGroups` picks the home group. The search scans the home group, and if the
key is neither found nor proven absent (no empty slot in the group), it advances to
the next group and wraps around — linear probing. Probing stops the instant it sees
an empty slot, because an absent key would have been inserted at the first empty (or
reusable tombstone) along this exact path, so if we reach an empty without a match,
the key is not in the set.

### Why deletion writes a tombstone, not empty

If `Delete` marked a slot empty, any key that had probed *past* that slot during its
own insertion would become unreachable — a search would stop early at the new hole
and wrongly report the key absent. Writing a tombstone (`ctrlDeleted`) keeps the
probe chain intact: searches skip over it, and inserts may reclaim it. `Insert`
therefore remembers the first empty-or-deleted slot it sees along the probe path and,
once it confirms the key is absent (hits an empty), places the key there, decrementing
the tombstone count if it reused a deleted slot. This is why churn accumulates
tombstones and why `ProbeStats` on a deleted-heavy set shows longer chains until a
grow rehashes them away.

### Growth keeps the load factor bounded

The table grows before it gets too full: when `count + tombstones` reaches 7/8 of
capacity, `grow` doubles the number of groups, allocates fresh control bytes, and
re-inserts every live key (dropping all tombstones). Because the load factor never
exceeds 7/8, at least one empty slot always exists, so the probe loop always
terminates. Presizing the built-in map is the production analogue of skipping these
repeated rehashes.

Create `swissset.go`:

```go
package swissset

import (
	"hash/maphash"
	"iter"
	"math/bits"
)

const (
	groupSize   = 8
	ctrlEmpty   = 0x80 // 0b1000_0000: never used
	ctrlDeleted = 0xFE // 0b1111_1110: tombstone
	// A full slot stores H2 in 0..127, so its top bit is clear.
)

type group[K comparable] struct {
	ctrl [groupSize]byte
	keys [groupSize]K
}

// SwissSet is a generic open-addressing hash set modelled on the Go 1.24 map:
// groups of 8 slots, a per-slot control byte holding a 7-bit fingerprint plus
// empty/deleted/full state, H1/H2 hash splitting, linear probing over groups,
// tombstones on delete, and grow-and-rehash at 7/8 load. It is not
// concurrency-safe, exactly like the built-in map.
type SwissSet[K comparable] struct {
	seed       maphash.Seed
	groups     []group[K]
	numGroups  int
	count      int
	tombstones int
}

// New returns an empty set with a fresh random hash seed.
func New[K comparable]() *SwissSet[K] {
	s := &SwissSet[K]{seed: maphash.MakeSeed(), numGroups: 1}
	s.groups = newGroups[K](s.numGroups)
	return s
}

func newGroups[K comparable](n int) []group[K] {
	gs := make([]group[K], n)
	for i := range gs {
		for j := range gs[i].ctrl {
			gs[i].ctrl[j] = ctrlEmpty
		}
	}
	return gs
}

func (s *SwissSet[K]) hash(key K) uint64 { return maphash.Comparable(s.seed, key) }

// maxLoad is 7/8 of capacity; capacity is numGroups*groupSize.
func (s *SwissSet[K]) maxLoad() int { return (s.numGroups * groupSize * 7) / 8 }

// matchByte returns a bitmask of slots whose control byte equals b.
func matchByte(ctrl [groupSize]byte, b byte) uint8 {
	var m uint8
	for i := range groupSize {
		if ctrl[i] == b {
			m |= 1 << uint8(i)
		}
	}
	return m
}

// matchEmpty returns a bitmask of empty (never-used) slots.
func matchEmpty(ctrl [groupSize]byte) uint8 {
	var m uint8
	for i := range groupSize {
		if ctrl[i] == ctrlEmpty {
			m |= 1 << uint8(i)
		}
	}
	return m
}

// matchFree returns a bitmask of slots that can take a new key: empty or deleted
// (both have the control byte's top bit set).
func matchFree(ctrl [groupSize]byte) uint8 {
	var m uint8
	for i := range groupSize {
		if ctrl[i]&0x80 != 0 {
			m |= 1 << uint8(i)
		}
	}
	return m
}

// Len reports the number of live entries (tombstones excluded).
func (s *SwissSet[K]) Len() int { return s.count }

// Groups reports the number of 8-slot groups currently allocated.
func (s *SwissSet[K]) Groups() int { return s.numGroups }

// Contains reports whether key is in the set.
func (s *SwissSet[K]) Contains(key K) bool {
	h := s.hash(key)
	h2 := byte(h & 0x7f)
	start := int((h >> 7) % uint64(s.numGroups))
	for probe := range s.numGroups {
		g := (start + probe) % s.numGroups
		grp := &s.groups[g]
		m := matchByte(grp.ctrl, h2)
		for m != 0 {
			i := bits.TrailingZeros8(m)
			if grp.keys[i] == key {
				return true
			}
			m &= m - 1
		}
		if matchEmpty(grp.ctrl) != 0 {
			return false
		}
	}
	return false
}

// Insert adds key to the set. Re-inserting a present key is a no-op.
func (s *SwissSet[K]) Insert(key K) {
	if s.count+s.tombstones >= s.maxLoad() {
		s.grow()
	}
	s.insert(key)
}

func (s *SwissSet[K]) insert(key K) {
	h := s.hash(key)
	h2 := byte(h & 0x7f)
	start := int((h >> 7) % uint64(s.numGroups))
	insertGrp, insertSlot := -1, -1
	insertTomb := false
	for probe := range s.numGroups {
		g := (start + probe) % s.numGroups
		grp := &s.groups[g]
		m := matchByte(grp.ctrl, h2)
		for m != 0 {
			i := bits.TrailingZeros8(m)
			if grp.keys[i] == key {
				return // already present
			}
			m &= m - 1
		}
		if insertSlot == -1 {
			if free := matchFree(grp.ctrl); free != 0 {
				i := bits.TrailingZeros8(free)
				insertGrp, insertSlot = g, i
				insertTomb = grp.ctrl[i] == ctrlDeleted
			}
		}
		if matchEmpty(grp.ctrl) != 0 {
			if insertTomb {
				s.tombstones--
			}
			s.groups[insertGrp].ctrl[insertSlot] = h2
			s.groups[insertGrp].keys[insertSlot] = key
			s.count++
			return
		}
	}
}

// Delete removes key if present, leaving a tombstone so probe chains stay intact.
func (s *SwissSet[K]) Delete(key K) {
	h := s.hash(key)
	h2 := byte(h & 0x7f)
	start := int((h >> 7) % uint64(s.numGroups))
	for probe := range s.numGroups {
		g := (start + probe) % s.numGroups
		grp := &s.groups[g]
		m := matchByte(grp.ctrl, h2)
		for m != 0 {
			i := bits.TrailingZeros8(m)
			if grp.keys[i] == key {
				grp.ctrl[i] = ctrlDeleted
				var zero K
				grp.keys[i] = zero
				s.count--
				s.tombstones++
				return
			}
			m &= m - 1
		}
		if matchEmpty(grp.ctrl) != 0 {
			return
		}
	}
}

func (s *SwissSet[K]) grow() {
	old := s.groups
	s.numGroups *= 2
	s.groups = newGroups[K](s.numGroups)
	s.count = 0
	s.tombstones = 0
	for g := range old {
		for i := range groupSize {
			if old[g].ctrl[i]&0x80 == 0 {
				s.insert(old[g].keys[i])
			}
		}
	}
}

// All yields every live key in an unspecified order.
func (s *SwissSet[K]) All() iter.Seq[K] {
	return func(yield func(K) bool) {
		for g := range s.groups {
			grp := &s.groups[g]
			for i := range groupSize {
				if grp.ctrl[i]&0x80 == 0 {
					if !yield(grp.keys[i]) {
						return
					}
				}
			}
		}
	}
}

// ProbeStat reports probe-length statistics over the live entries.
type ProbeStat struct {
	Avg float64 // mean groups visited to reach a live key (home group counts as 1)
	Max int     // worst-case groups visited
}

// ProbeStats computes, for every live key, how many groups a lookup visits from
// its home group to where it actually resides. This is the behavior the built-in
// map hides; here it is directly measurable.
func (s *SwissSet[K]) ProbeStats() ProbeStat {
	if s.count == 0 {
		return ProbeStat{}
	}
	total, max := 0, 0
	for g := range s.groups {
		grp := &s.groups[g]
		for i := range groupSize {
			if grp.ctrl[i]&0x80 != 0 {
				continue // empty or deleted
			}
			h := s.hash(grp.keys[i])
			home := int((h >> 7) % uint64(s.numGroups))
			dist := (g - home + s.numGroups) % s.numGroups
			pl := dist + 1
			total += pl
			if pl > max {
				max = pl
			}
		}
	}
	return ProbeStat{Avg: float64(total) / float64(s.count), Max: max}
}
```

### The runnable demo

The demo builds a set of 1000 integers, queries it, deletes half, and reports probe
stats. Every printed value is deterministic across runs: `Len`, membership, and group
count depend only on the sequence of operations, not on the random hash seed. Probe
lengths themselves vary with the seed, so the demo prints the invariant that every
live key is reachable within the table (probe length is at least 1 and at most the
group count) rather than a seed-dependent number.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/swissset"
)

func main() {
	s := swissset.New[int]()
	for i := range 1000 {
		s.Insert(i)
	}
	fmt.Println("len:", s.Len())
	fmt.Println("contains 500:", s.Contains(500))
	fmt.Println("contains 5000:", s.Contains(5000))

	for i := range 500 {
		s.Delete(i)
	}
	fmt.Println("len after deleting 0..499:", s.Len())
	fmt.Println("contains 499:", s.Contains(499))
	fmt.Println("contains 500:", s.Contains(500))

	st := s.ProbeStats()
	fmt.Println("every live key reachable:", st.Max >= 1 && st.Max <= s.Groups())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
len: 1000
contains 500: true
contains 5000: false
len after deleting 0..499: 500
contains 499: false
contains 500: true
every live key reachable: true
```

### Tests

The strongest test for a data structure is a differential test: run the same random
sequence of operations against your implementation and a known-correct reference (a
built-in `map[K]struct{}`), and assert they agree after every operation. A fixed
`math/rand/v2` PCG source makes the sequence deterministic and reproducible, and a
small key space forces collisions, tombstones, and grows to actually happen. The
table cases pin specific behaviors: tombstone reuse (delete then re-insert must not
leak a tombstone), and grow consistency (enough inserts to trigger several grows,
after which every key is still present). The `Example` prints `slices.Sorted` of the
set's keys — the idiom for deterministic output over an unordered container.

Create `swissset_test.go`:

```go
package swissset

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
)

func TestDifferentialAgainstMap(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 1024))
	set := New[int]()
	ref := make(map[int]struct{})

	const ops = 20000
	const keySpace = 300 // small: forces collisions, deletes, and grows

	for range ops {
		key := rng.IntN(keySpace)
		switch rng.IntN(3) {
		case 0:
			set.Insert(key)
			ref[key] = struct{}{}
		case 1:
			set.Delete(key)
			delete(ref, key)
		case 2:
			// read-only probe
		}
		if got, want := set.Contains(key), containsRef(ref, key); got != want {
			t.Fatalf("Contains(%d) = %v, map says %v", key, got, want)
		}
		if got, want := set.Len(), len(ref); got != want {
			t.Fatalf("Len() = %d, map len = %d", got, want)
		}
	}

	// Full membership sweep at the end.
	for k := range keySpace {
		if got, want := set.Contains(k), containsRef(ref, k); got != want {
			t.Errorf("final Contains(%d) = %v, want %v", k, got, want)
		}
	}
	got := slices.Sorted(set.All())
	want := slices.Sorted(maps.Keys(ref))
	if !slices.Equal(got, want) {
		t.Errorf("All() = %v, want %v", got, want)
	}
}

func containsRef(m map[int]struct{}, k int) bool {
	_, ok := m[k]
	return ok
}

func TestTombstoneReuse(t *testing.T) {
	t.Parallel()
	s := New[int]()
	s.Insert(7)
	if s.count != 1 || s.tombstones != 0 {
		t.Fatalf("after Insert: count=%d tombstones=%d, want 1,0", s.count, s.tombstones)
	}
	s.Delete(7)
	if s.count != 0 || s.tombstones != 1 {
		t.Fatalf("after Delete: count=%d tombstones=%d, want 0,1", s.count, s.tombstones)
	}
	s.Insert(7) // must reclaim the tombstone, not leak it
	if s.count != 1 || s.tombstones != 0 {
		t.Fatalf("after re-Insert: count=%d tombstones=%d, want 1,0", s.count, s.tombstones)
	}
	if !s.Contains(7) {
		t.Fatal("Contains(7) false after re-insert")
	}
}

func TestGrowKeepsEverything(t *testing.T) {
	t.Parallel()
	s := New[int]()
	const n = 5000
	for i := range n {
		s.Insert(i)
	}
	if s.Groups() <= 1 {
		t.Fatalf("Groups() = %d, expected several grows", s.Groups())
	}
	if s.Len() != n {
		t.Fatalf("Len() = %d, want %d", s.Len(), n)
	}
	for i := range n {
		if !s.Contains(i) {
			t.Fatalf("Contains(%d) false after grow", i)
		}
	}
	if s.Contains(n) {
		t.Fatalf("Contains(%d) true for never-inserted key", n)
	}
}

func TestDuplicateInsertIsNoop(t *testing.T) {
	t.Parallel()
	s := New[string]()
	s.Insert("a")
	s.Insert("a")
	s.Insert("a")
	if s.Len() != 1 {
		t.Fatalf("Len() = %d after inserting the same key thrice, want 1", s.Len())
	}
}

func Example() {
	s := New[int]()
	for _, v := range []int{5, 3, 9, 1, 3, 7} {
		s.Insert(v)
	}
	fmt.Println(slices.Sorted(s.All()))
	// Output: [1 3 5 7 9]
}
```

## Review

The set is correct when it agrees with a built-in map on membership and length after
every operation — that is what `TestDifferentialAgainstMap` proves, and a small key
space is what makes it exercise tombstones and grows rather than just a stream of
distinct inserts. The subtle bugs live in three places. First, control-byte
initialization: a fresh group must be filled with `ctrlEmpty`, because a zero byte is
a legitimate H2 and an uninitialized group would look full of fingerprint-0 keys.
Second, the probe-stop condition: a search may stop only at an *empty* slot, never at
a tombstone, or a key reached by probing past a later-deleted slot goes missing —
`TestTombstoneReuse` and the differential test together catch this. Third, growth
must preserve every live key and reset tombstones; `TestGrowKeepsEverything` forces
several grows and re-checks all keys.

The common mistakes are conceptual, not just mechanical. Do not treat the top bit of
the control byte as part of the fingerprint — H2 is only 7 bits precisely so the top
bit can carry empty/deleted/full state. Do not "optimize" delete to write empty; the
tombstone is load-bearing. And do not read anything into `ProbeStats` numbers across
runs: they move with the random seed, which is why the demo asserts only the
seed-independent invariant that every key is reachable. Run `go test -race` to confirm
the differential loop stays consistent; the set is single-threaded by design, exactly
like the built-in map.

## Resources

- [Faster Go maps with Swiss Tables (The Go Blog)](https://go.dev/blog/swisstable) — the group/control-byte/H1-H2 design this exercise mirrors.
- [`hash/maphash` package](https://pkg.go.dev/hash/maphash) — `Comparable[T]`, `MakeSeed`, and `Seed`, all added in Go 1.24.
- [`math/bits` package](https://pkg.go.dev/math/bits) — `TrailingZeros8`, used to walk the candidate bitmask.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-map-memory-footprint.md](02-map-memory-footprint.md)
