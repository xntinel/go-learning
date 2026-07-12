# Exercise 14: Sentinel-Fill A Shard Assignment Table With slices.Repeat

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A consistent-hashing cluster -- Cassandra's token ring, DynamoDB's partition
map, any hand-rolled sharded store -- needs a table mapping each slot in the
hash space to the shard that owns it. Building that table starts with the same
line every time: allocate one slot per partition, mark every slot
"unassigned" until the rebalancer places a real shard there. In Go, the
obvious way to allocate that table is `make([]ShardID, slots)`. It is also
wrong, for a reason specific to this domain: shard id 0 is a completely
ordinary, legal shard, and `make`'s zero-fill hands every slot the value `0`
before anything assigns it. A freshly allocated table is bit-for-bit
indistinguishable from a table where every slot has already been assigned to
shard 0.

This is a different bug from the classic `make(len)` versus `make(len, cap)`
mapper mistake taught elsewhere in this curriculum -- that one is about length
versus capacity; this one is about the zero value colliding with a legitimate
domain value. It survives review because `make([]ShardID, slots)` reads as
"allocate a table of the right size", which it does; the bug is only visible
once you ask "how do I tell an unassigned slot apart from one assigned to
shard 0", and by then the zero value has already made that question
unanswerable. `slices.Repeat` fixes it by construction: it never returns a
slice of zero values, it returns a slice where every slot has been explicitly
set to whatever sentinel you name.

This module builds `shardtable`, a fixed-size slot table backed by
`slices.Repeat`, with an explicit `Unassigned` sentinel distinct from any real
shard id. The zero-fill bug is never reachable through the package's own API;
it lives only in the test file, where it is shown corrupting a real
assignment on purpose.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
shardtable/                module example.com/shardtable
  go.mod                    go 1.24
  shardtable.go             ShardID, Unassigned, Table; NewTable, Assign, ShardAt, Unassigned, Len
  shardtable_test.go        table tests, aliasing, the zero-fill collision contrast,
                            ExampleTable
```

- Files: `shardtable.go`, `shardtable_test.go`.
- Implement: `const Unassigned ShardID = -1`; `NewTable(slots int) *Table` backed by `slices.Repeat([]ShardID{Unassigned}, slots)`, clamping a negative `slots` to zero; `(*Table).Assign(slot int, shard ShardID) error` rejecting an out-of-range slot with `ErrSlotOutOfRange`; `(*Table).ShardAt(slot int) (ShardID, error)`; `(*Table).Unassigned() []int` reporting every slot still at the sentinel; `(*Table).Len() int`.
- Test: table construction across ordinary, zero, and negative slot counts; assignment and lookup, including assigning the legal shard id `0`; out-of-range rejection for both `Assign` and `ShardAt`; `Assign(slot, Unassigned)` freeing a slot; `Unassigned` never aliasing the table; the naive `make([]ShardID, slots)` zero-fill contrast; `ExampleTable`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the zero value cannot be the sentinel

`ShardID` is a plain `int`-based type, and its zero value is `0`. That is
also a valid shard id -- shard numbering starts at 0 in every consistent-hashing
scheme this module's domain covers. The naive table initialization is one
line:

```go
slots := make([]ShardID, n) // every slot starts at ShardID(0)
```

Every slot in that table reads as "assigned to shard 0" from the moment it is
allocated, before a single real assignment has happened. There is no way,
looking only at this representation, to tell a slot that was deliberately
assigned to shard 0 apart from one nobody has touched yet -- both are the
value `0`. A rebalancer that asks "which slots still need placing" by scanning
for the zero value will either skip every slot that was legitimately assigned
to shard 0, or -- if it instead treats every zero-valued slot as unassigned
and eligible for reassignment -- will silently steal shard 0's slots away from
it on the next rebalance pass.

`slices.Repeat(x, count)` sidesteps the zero value entirely: it returns a
slice of `count` copies of `x`, and never a slice of the type's zero value
unless `x` itself happens to be the zero value. Here `x` is
`[]ShardID{Unassigned}`, and `Unassigned` is `-1` specifically because no
real shard id in this domain is ever negative. Every slot the table starts
with is explicitly the sentinel, set by the same operation that allocates the
table, not left to whatever the zero value happens to be for this particular
type.

Create `shardtable.go`:

```go
// Package shardtable maintains a fixed-size partition assignment table for a
// consistent-hashing cluster: one ShardID per slot, initialized so that
// "never assigned" and "assigned to shard 0" can never be confused, because
// shard id 0 is a perfectly legal shard.
package shardtable

import (
	"errors"
	"fmt"
	"slices"
)

// ShardID identifies a shard. Zero is a valid, ordinary shard id; it is not
// reserved for any special meaning.
type ShardID int

// Unassigned is the sentinel value a slot holds until Assign is called on
// it. It is negative specifically so it can never collide with a real
// ShardID, all of which this package treats as non-negative.
const Unassigned ShardID = -1

// ErrSlotOutOfRange is returned by Assign and ShardAt when slot is outside
// [0, Len()).
var ErrSlotOutOfRange = errors.New("shardtable: slot out of range")

// Table is a fixed-size slot-to-shard assignment table.
//
// Table is not safe for concurrent use; a caller that assigns slots from
// more than one goroutine must synchronize externally.
type Table struct {
	slots []ShardID
}

// NewTable returns a Table with the given number of slots, every one
// initialized to Unassigned. A negative slots is clamped to zero rather
// than rejected, since a zero-slot table is a well-defined, usable value:
// Len reports zero and every method that indexes a slot returns
// ErrSlotOutOfRange.
func NewTable(slots int) *Table {
	if slots < 0 {
		slots = 0
	}
	// slices.Repeat fills every slot explicitly; unlike make([]ShardID,
	// slots), it never leaves a slot holding the type's zero value by
	// default, which for ShardID would be indistinguishable from a real
	// assignment to shard 0. See the package tests for what that
	// collision looks like in practice.
	return &Table{slots: slices.Repeat([]ShardID{Unassigned}, slots)}
}

// Len reports the number of slots in the table.
func (t *Table) Len() int {
	return len(t.slots)
}

// Assign sets slot to shard, overwriting whatever it held before,
// including a previous assignment. Passing Unassigned explicitly frees the
// slot. Assign returns ErrSlotOutOfRange if slot is outside [0, Len()).
func (t *Table) Assign(slot int, shard ShardID) error {
	if slot < 0 || slot >= len(t.slots) {
		return fmt.Errorf("%w: slot=%d, table has %d slots", ErrSlotOutOfRange, slot, len(t.slots))
	}
	t.slots[slot] = shard
	return nil
}

// ShardAt reports the shard currently assigned to slot, or Unassigned if
// none has been. It returns ErrSlotOutOfRange if slot is outside
// [0, Len()).
func (t *Table) ShardAt(slot int) (ShardID, error) {
	if slot < 0 || slot >= len(t.slots) {
		return 0, fmt.Errorf("%w: slot=%d, table has %d slots", ErrSlotOutOfRange, slot, len(t.slots))
	}
	return t.slots[slot], nil
}

// Unassigned returns the indices of every slot still holding the sentinel,
// in ascending order.
//
// The returned slice is freshly allocated; it never aliases the Table's
// internal storage, so the caller may retain or mutate it without affecting
// a later call to Assign.
func (t *Table) Unassigned() []int {
	idx := make([]int, 0, len(t.slots))
	for i, s := range t.slots {
		if s == Unassigned {
			idx = append(idx, i)
		}
	}
	return idx
}
```

### Using it

Build one `Table` per partition map with `NewTable(n)`, then call `Assign`
each time the rebalancer places a shard on a slot -- including with
`Unassigned` itself, which is a legitimate way to free a slot rather than a
rejected input. `Unassigned()` is the query a rebalancer runs to find slots
that still need placing; because the table never starts with a real shard id
sitting in a slot's zero value, that query is exact from the moment the table
is constructed, with no warm-up assignment pass required to make it
trustworthy. `Table` is documented as unsafe for concurrent use, so a
rebalancer that assigns from more than one goroutine needs its own
synchronization around it, the same way most partition-map implementations
already serialize rebalancing through a single controller.

`ExampleTable` in the `_test.go` file is the runnable demonstration of this
module: `go test` executes it and compares its stdout against the
`// Output:` comment, so it cannot drift from the code it documents.

```go
func ExampleTable() {
	tbl := NewTable(4)
	fmt.Println("fresh table, unassigned slots:", tbl.Unassigned())

	_ = tbl.Assign(1, ShardID(0)) // shard 0 is a legal, ordinary assignment
	_ = tbl.Assign(3, ShardID(9))
	fmt.Println("after two assignments, unassigned slots:", tbl.Unassigned())

	shard, _ := tbl.ShardAt(1)
	fmt.Println("slot 1 holds shard", shard)

	if err := tbl.Assign(4, ShardID(2)); err != nil {
		fmt.Println("rejected:", err)
	}

	// Output:
	// fresh table, unassigned slots: [0 1 2 3]
	// after two assignments, unassigned slots: [0 2]
	// slot 1 holds shard 0
	// rejected: shardtable: slot out of range: slot=4, table has 4 slots
}
```

### Tests

`TestNewTable` covers ordinary construction plus the zero and negative
edge cases the component bar requires, confirming a clamped-to-zero table is
a usable, empty value rather than a nil pointer or a panic.
`TestAssignAndShardAt` deliberately assigns shard `0` alongside an ordinary
shard id, to keep that legal case in the table wherever the sentinel is
discussed. `TestAssignRejectsOutOfRangeSlot` and
`TestAssignUnassignedFreesASlot` cover the two directions `Assign` can be
called with a meaning beyond "place a shard": an invalid slot, and a
deliberate un-assignment. `TestUnassignedDoesNotAliasTable` pins the
aliasing contract stated on `Unassigned`.

`TestNaiveZeroFillCollidesWithShardZero` is the module's core test.
`newTableNaive` is unexported and unreachable from the package API; it is
the `make([]ShardID, slots)` line from the prose above. The test captures
the naive table's string form before and after assigning shard 0 into slot
2, and asserts the two are byte-identical -- proving the assignment produced
no observable change, because the slot already read as shard 0 by default.
It then shows the real `Table` telling that same slot's real assignment
apart from the four slots nobody ever touched.

Create `shardtable_test.go`:

```go
package shardtable

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestNewTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		slots int
		want  int
	}{
		{"ordinary size", 5, 5},
		{"zero slots", 0, 0},
		{"negative slots clamps to zero", -3, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tbl := NewTable(tc.slots)
			if tbl.Len() != tc.want {
				t.Fatalf("Len() = %d, want %d", tbl.Len(), tc.want)
			}
			if got := tbl.Unassigned(); len(got) != tc.want {
				t.Fatalf("Unassigned() = %v, want %d entries", got, tc.want)
			}
		})
	}
}

func TestAssignAndShardAt(t *testing.T) {
	t.Parallel()

	tbl := NewTable(4)

	if err := tbl.Assign(0, ShardID(7)); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := tbl.Assign(1, 0); err != nil { // shard id 0 is a legal, ordinary assignment
		t.Fatalf("Assign(shard 0): %v", err)
	}

	tests := []struct {
		slot int
		want ShardID
	}{
		{0, ShardID(7)},
		{1, ShardID(0)},
		{2, Unassigned},
		{3, Unassigned},
	}
	for _, tc := range tests {
		got, err := tbl.ShardAt(tc.slot)
		if err != nil {
			t.Fatalf("ShardAt(%d): %v", tc.slot, err)
		}
		if got != tc.want {
			t.Errorf("ShardAt(%d) = %v, want %v", tc.slot, got, tc.want)
		}
	}
}

func TestAssignRejectsOutOfRangeSlot(t *testing.T) {
	t.Parallel()

	tbl := NewTable(3)
	for _, slot := range []int{-1, 3, 100} {
		if err := tbl.Assign(slot, ShardID(1)); !errors.Is(err, ErrSlotOutOfRange) {
			t.Errorf("Assign(%d, ...) error = %v, want ErrSlotOutOfRange", slot, err)
		}
	}
	if _, err := tbl.ShardAt(3); !errors.Is(err, ErrSlotOutOfRange) {
		t.Errorf("ShardAt(3) error = %v, want ErrSlotOutOfRange", err)
	}
}

// TestAssignUnassignedFreesASlot proves passing Unassigned to Assign is a
// legitimate way to free a slot, not a rejected input.
func TestAssignUnassignedFreesASlot(t *testing.T) {
	t.Parallel()

	tbl := NewTable(2)
	if err := tbl.Assign(0, ShardID(5)); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := tbl.Assign(0, Unassigned); err != nil {
		t.Fatalf("Assign(Unassigned): %v", err)
	}
	if !slices.Equal(tbl.Unassigned(), []int{0, 1}) {
		t.Fatalf("Unassigned() = %v, want [0 1]", tbl.Unassigned())
	}
}

// TestUnassignedDoesNotAliasTable pins the aliasing contract: mutating the
// returned index slice must not affect a later Assign or Unassigned call.
func TestUnassignedDoesNotAliasTable(t *testing.T) {
	t.Parallel()

	tbl := NewTable(3)
	idx := tbl.Unassigned()
	idx[0] = 999

	if err := tbl.Assign(0, ShardID(1)); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if !slices.Equal(tbl.Unassigned(), []int{1, 2}) {
		t.Fatalf("Unassigned() = %v, want [1 2]", tbl.Unassigned())
	}
}

// newTableNaive is the initialization every hand-rolled shard table starts
// out as. It is not part of the package API. It looks reasonable because
// make allocates exactly the right number of slots -- it is wrong because
// every slot's zero value, ShardID(0), is also a perfectly legal shard id,
// so a freshly made table is bit-for-bit indistinguishable from a table
// where every single slot has already been assigned to shard 0.
func newTableNaive(slots int) []ShardID {
	return make([]ShardID, slots)
}

// TestNaiveZeroFillCollidesWithShardZero is the heart of the module. It
// shows a freshly made naive table reading identically to one this test
// then "assigns" shard 0 into: there is no way, looking only at the naive
// representation, to tell "assigned to shard 0" apart from "never
// assigned". The real Table keeps them apart because Unassigned() reports
// exactly the slots holding the sentinel, never a real shard id.
func TestNaiveZeroFillCollidesWithShardZero(t *testing.T) {
	t.Parallel()

	naive := newTableNaive(5)
	beforeAssignment := fmt.Sprint(naive)

	naive[2] = 0 // a real, deliberate assignment of shard 0 to slot 2
	afterAssignment := fmt.Sprint(naive)

	if beforeAssignment != afterAssignment {
		t.Fatalf("naive table changed after assigning shard 0: %s -> %s; want identical, that is the bug", beforeAssignment, afterAssignment)
	}
	for i, s := range naive {
		if s != 0 {
			t.Fatalf("naive[%d] = %v, want 0 (every slot, assigned or not, reads as shard 0)", i, s)
		}
	}

	// Contrast: the real Table tells slot 2's real assignment apart from
	// the four slots nobody ever touched.
	tbl := NewTable(5)
	if err := tbl.Assign(2, 0); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	want := []int{0, 1, 3, 4}
	if !slices.Equal(tbl.Unassigned(), want) {
		t.Fatalf("Unassigned() = %v, want %v", tbl.Unassigned(), want)
	}
}

// ExampleTable is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleTable() {
	tbl := NewTable(4)
	fmt.Println("fresh table, unassigned slots:", tbl.Unassigned())

	_ = tbl.Assign(1, ShardID(0)) // shard 0 is a legal, ordinary assignment
	_ = tbl.Assign(3, ShardID(9))
	fmt.Println("after two assignments, unassigned slots:", tbl.Unassigned())

	shard, _ := tbl.ShardAt(1)
	fmt.Println("slot 1 holds shard", shard)

	if err := tbl.Assign(4, ShardID(2)); err != nil {
		fmt.Println("rejected:", err)
	}

	// Output:
	// fresh table, unassigned slots: [0 1 2 3]
	// after two assignments, unassigned slots: [0 2]
	// slot 1 holds shard 0
	// rejected: shardtable: slot out of range: slot=4, table has 4 slots
}
```

## Review

`Table` is correct when `Unassigned()` reports exactly the slots nobody has
called `Assign` on, and never a slot that legitimately holds shard `0` --
the two states `make([]ShardID, slots)` cannot tell apart, since the type's
zero value and a real shard id are the same value. `NewTable` closes that
gap by using `slices.Repeat` to fill every slot with the explicit `-1`
sentinel `Unassigned`, an operation that never returns the zero value unless
asked to, unlike `make`. `TestNaiveZeroFillCollidesWithShardZero` proves the
naive allocation's failure directly: assigning shard 0 into a naively
allocated table produces no observable change at all, because the slot
already read as shard 0 from the moment it was allocated. Around that core,
`Assign` and `ShardAt` reject an out-of-range slot with `ErrSlotOutOfRange`,
checkable with `errors.Is`, `Assign` accepts `Unassigned` itself as a
legitimate way to free a slot, and `Unassigned()` always returns a fresh
slice that never aliases the table's storage. `Table` is documented as
unsafe for concurrent use, matching how a real rebalancer already serializes
assignment through one controller. Run `go test -count=1 -race ./...` to
confirm the construction, assignment, aliasing, and zero-fill-collision
tests.

## Resources

- [`slices.Repeat`](https://pkg.go.dev/slices#Repeat) — the "every slot explicitly populated, never nil" fill this module builds on.
- [The Go Programming Language Specification: The zero value](https://go.dev/ref/spec#The_zero_value) — why `make`'s zero-fill is not always a safe default.
- [Apache Cassandra: Data distribution and replication](https://cassandra.apache.org/doc/latest/cassandra/architecture/dynamo.html) — the token-ring partition assignment this module's domain is modeled on.
- [`slices.Equal`](https://pkg.go.dev/slices#Equal) — used throughout the tests to compare `Unassigned()`'s result against an expected index list.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-dns-negative-cache-nil-vs-empty.md](13-dns-negative-cache-nil-vs-empty.md) | Next: [15-wal-replay-backward-iteration.md](15-wal-replay-backward-iteration.md)
