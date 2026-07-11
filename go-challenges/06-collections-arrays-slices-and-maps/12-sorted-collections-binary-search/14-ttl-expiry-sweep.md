# Exercise 14: TTL Cache Expiry Sweep With slices.Delete

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Redis and memcached both run an active-expiration cycle: a background pass
that finds every key whose TTL has elapsed and evicts it, independent of
whether anything ever reads that key again. Done naively, that sweep costs
one removal operation per expired key, each one shifting the rest of the
structure. Done well, it is a single search for the boundary between
"expired" and "still alive" followed by one batch removal — the same
half-open-interval discipline this lesson has used for range queries,
applied here to a moving `now` instead of a fixed `[lo, hi)`.

The boundary search is an upper bound: "everything with `ExpiresAt <= now`"
is the same shape as "everything strictly less than `now+1`", which is
exactly what an upper-bound binary search answers in one call. The batch
removal is `slices.Delete(entries, 0, hi)`, reassigned back into the index —
and the detail worth internalizing here is not just the reassignment (this
lesson's other modules cover that), it is what `slices.Delete` does to the
elements it removes: the standard library documents that `Delete` zeroes
the slots between the new length and the old one, which is what lets the
garbage collector actually reclaim the evicted keys' string data instead of
holding it alive through a stale backing array.

The mistake this module contrasts is evicting one expired entry at a time
inside a `for range` over the same slice being mutated. Removing element
`i` shifts everything after it one slot left in the same backing array the
range is iterating — the very array the loop is reading from — so the
element that just slid into position `i` is silently skipped on the next
iteration. Two adjacent expired keys in a row, and the sweep misses every
other one. It is not part of this module's `ExpiryIndex` API; it lives only
in the tests, where it belongs, as the thing the tests prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
ttlindex/                module example.com/ttlindex
  go.mod                 go 1.24
  expiry.go              Entry, ExpiryIndex; NewExpiryIndex, Insert, Evict, Len;
                         two sentinel errors
  expiry_test.go         eviction table, sorted-insert property, empty index,
                         constructor/insert guards, aliasing, the buggy
                         range-removal contrast, ExampleExpiryIndex
```

- Files: `expiry.go`, `expiry_test.go`.
- Implement: `NewExpiryIndex(capacity int) (*ExpiryIndex, error)` rejecting a negative capacity with `ErrInvalidCapacity`; `(*ExpiryIndex).Insert(e Entry) error` rejecting an empty `Key` with `ErrEmptyKey` and placing `e` at its sorted position by `ExpiresAt`; `(*ExpiryIndex).Evict(now int64) []string` removing and returning the keys of every entry with `ExpiresAt <= now` via one upper-bound search and one `slices.Delete`; `(*ExpiryIndex).Len() int`.
- Test: the eviction table across boundaries (nothing expired, exactly at an expiry, several at once, everything, an already-empty index), a sorted-insert property proving out-of-order inserts still evict in ascending order, the two constructor/insert guards, `Evict`'s result never aliasing the index's storage, an `evictBuggy` contrast proving the in-place range-removal mistake leaves an already-expired key resident, and `ExampleExpiryIndex` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ttlindex
cd ~/go-exercises/ttlindex
go mod init example.com/ttlindex
go mod edit -go=1.24
```

### One upper-bound search, one batch delete, and why the range-removal mistake hides two errors at once

`Evict` finds the boundary between expired and live entries with the same
upper-bound trick this lesson uses for a strictly-greater bound: treat an
exact match (`ExpiresAt == now`) as "less than" the target, which folds it
into the expired side and leaves the boundary sitting on the first entry
that genuinely outlives `now`.

```go
hi, _ := slices.BinarySearchFunc(idx.entries, now, func(x Entry, t int64) int {
    if x.ExpiresAt == t {
        return -1
    }
    return cmp.Compare(x.ExpiresAt, t)
})
idx.entries = slices.Delete(idx.entries, 0, hi)
```

The tempting alternative — walk the slice once, delete as you find expired
entries — looks like a smaller, simpler version of the same idea and is
where the bug actually ships:

```go
for i, e := range entries {
    if e.ExpiresAt <= now {
        entries = append(entries[:i], entries[i+1:]...)
    }
}
```

`entries[:i]` and `entries[i+1:]` share the *same backing array* the
`for range` is reading from, so `append` here does not just shrink a
separate copy — it shifts every later element one slot left in the exact
array the loop keeps pulling values out of. Deleting index `i` moves what
used to be at `i+1` into slot `i`, but the range loop has already consumed
slot `i` for this iteration and moves on to `i+1` next, which now holds
what used to be two entries further along. The entry that slid into the
gap is never examined. Two consecutive expired keys and the second survives
the sweep, resident in the cache until some *later* sweep happens to catch
it — if `now` has moved on by then, or if the key gets refreshed in the
meantime, it may never be caught at all.

Create `expiry.go`:

```go
// Package ttlindex implements the active-expiration structure behind an
// in-memory TTL cache like Redis or memcached: keys held sorted by absolute
// expiry time, swept in one batch pass instead of one key at a time.
package ttlindex

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
)

// Sentinel errors returned by NewExpiryIndex and Insert. Callers should
// test for them with errors.Is rather than by comparing error strings.
var (
	// ErrInvalidCapacity means a negative capacity was given to NewExpiryIndex.
	ErrInvalidCapacity = errors.New("ttlindex: capacity must not be negative")
	// ErrEmptyKey means an Entry had an empty Key.
	ErrEmptyKey = errors.New("ttlindex: entry has empty key")
)

// Entry is one cached key and the absolute time (in the caller's clock
// unit, e.g. Unix seconds) at which it expires.
type Entry struct {
	Key       string
	ExpiresAt int64
}

// ExpiryIndex holds Entries sorted by ExpiresAt, ready to answer "which
// keys have expired as of now" in one batch sweep.
//
// An ExpiryIndex is not safe for concurrent use: Insert and Evict both
// mutate internal state. Callers typically pair one ExpiryIndex with a
// single background sweeper goroutine and guard the surrounding cache's Get
// and Set paths with their own synchronization.
type ExpiryIndex struct {
	entries []Entry
}

// NewExpiryIndex returns an empty ExpiryIndex pre-sized for capacity
// entries. It returns ErrInvalidCapacity if capacity is negative; capacity
// zero is valid and simply defers allocation to the first Insert.
func NewExpiryIndex(capacity int) (*ExpiryIndex, error) {
	if capacity < 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacity)
	}
	return &ExpiryIndex{entries: make([]Entry, 0, capacity)}, nil
}

// Insert adds e in its sorted position by ExpiresAt. It returns ErrEmptyKey
// if e.Key is empty. Insert does not deduplicate by Key; a caller updating
// an existing key's TTL is responsible for removing the stale Entry first.
func (idx *ExpiryIndex) Insert(e Entry) error {
	if e.Key == "" {
		return ErrEmptyKey
	}
	pos, _ := slices.BinarySearchFunc(idx.entries, e.ExpiresAt, func(x Entry, t int64) int {
		return cmp.Compare(x.ExpiresAt, t)
	})
	idx.entries = slices.Insert(idx.entries, pos, e)
	return nil
}

// Evict removes every Entry whose ExpiresAt is at or before now and returns
// their Keys, in ascending ExpiresAt order. It locates the boundary with a
// single upper-bound binary search -- the first index whose ExpiresAt is
// strictly greater than now -- then removes the whole expired prefix in one
// slices.Delete call, reassigned back into the index. slices.Delete zeroes
// the vacated tail, which is what lets the garbage collector reclaim the
// Key strings of the evicted entries instead of retaining them through a
// stale backing array.
//
// The returned slice is freshly allocated and never aliases the index's
// internal storage.
func (idx *ExpiryIndex) Evict(now int64) []string {
	hi, _ := slices.BinarySearchFunc(idx.entries, now, func(x Entry, t int64) int {
		if x.ExpiresAt == t {
			return -1 // equal counts as "less": push the boundary past ties
		}
		return cmp.Compare(x.ExpiresAt, t)
	})
	if hi == 0 {
		return nil
	}
	keys := make([]string, hi)
	for i := 0; i < hi; i++ {
		keys[i] = idx.entries[i].Key
	}
	idx.entries = slices.Delete(idx.entries, 0, hi)
	return keys
}

// Len reports the number of entries currently held in the index.
func (idx *ExpiryIndex) Len() int { return len(idx.entries) }
```

### Using it

`Insert` runs on every cache write, keeping entries ordered by `ExpiresAt`
regardless of what order writes arrive in. `Evict` runs on whatever cadence
a background sweeper chooses — every second, every time the cache crosses a
size threshold — and returns exactly the keys that just expired, which a
surrounding cache uses to remove the corresponding values from its own
key-value map. Because `Evict`'s clock is a plain `int64` parameter, tests
can drive `ExpiryIndex` through any sequence of sweep times deterministically,
with no `time.Sleep` and no dependence on wall-clock speed.

`ExpiryIndex` is not safe for concurrent use on its own, matching how these
structures are actually deployed: one sweeper goroutine owns the index, and
the surrounding cache's own locking protects the parts that other goroutines
touch. `ExampleExpiryIndex` is the runnable demonstration of this module:
`go test` executes it and compares its stdout against the `// Output:`
comment, so the usage shown here cannot drift away from the code.

### Tests

`TestEvict` is the table that matters most: four entries inserted out of
`ExpiresAt` order, then swept at every boundary that matters — nothing
expired yet, exactly at one expiry, several at once, everything remaining,
and a final sweep of an index that is already empty. `TestInsertKeepsSortedOrder`
turns the sortedness invariant into an executable property: entries go in
scrambled, and evicting at each distinct `ExpiresAt` value in ascending
order must always return exactly one key, which only holds if `Insert`
placed every entry correctly. `TestNewExpiryIndexRejectsNegativeCapacity`
and `TestInsertRejectsEmptyKey` cover the two sentinel errors, and
`TestEvictedSliceDoesNotAliasIndex` pins the aliasing contract on `Evict`'s
result.

`TestBuggyRangeRemovalLeavesExpiredKeysResident` is the heart of the module.
`evictBuggy` is unexported and unreachable from the package API; it removes
expired entries with the in-place, index-based `append` pattern and the test
shows it leaving the key `"b"` — expired at time 2, swept at time 5 — still
present in its result, then shows the real `ExpiryIndex.Evict` removing all
three expired entries in one pass over the same input.

Create `expiry_test.go`:

```go
package ttlindex

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func mustInsert(t *testing.T, idx *ExpiryIndex, key string, expiresAt int64) {
	t.Helper()
	if err := idx.Insert(Entry{Key: key, ExpiresAt: expiresAt}); err != nil {
		t.Fatalf("Insert(%q, %d): %v", key, expiresAt, err)
	}
}

// evictBuggy is the mistake this module exists to prevent: it removes
// expired entries one at a time with append(s[:i], s[i+1:]...) inside a
// for-range over the same slice. Removing entries[i] shifts everything
// after it left in the same backing array the range is iterating over, so
// the very next iteration silently skips the element that slid into
// position i+1. It is never exported and never reachable from the package
// API; it exists so the tests can pin which expired keys it leaves behind.
func evictBuggy(entries []Entry, now int64) []Entry {
	for i, e := range entries {
		if e.ExpiresAt <= now {
			entries = append(entries[:i], entries[i+1:]...)
		}
	}
	return entries
}

func TestNewExpiryIndexRejectsNegativeCapacity(t *testing.T) {
	t.Parallel()

	if _, err := NewExpiryIndex(-1); !errors.Is(err, ErrInvalidCapacity) {
		t.Fatalf("NewExpiryIndex(-1) error = %v, want ErrInvalidCapacity", err)
	}
}

func TestInsertRejectsEmptyKey(t *testing.T) {
	t.Parallel()

	idx, err := NewExpiryIndex(0)
	if err != nil {
		t.Fatalf("NewExpiryIndex: %v", err)
	}
	if err := idx.Insert(Entry{Key: "", ExpiresAt: 10}); !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("Insert with empty key error = %v, want ErrEmptyKey", err)
	}
}

// TestEvict is the table that matters most: entries inserted out of
// ExpiresAt order, then swept at boundaries that matter -- before anything
// expires, exactly at an expiry, past several at once, and past everything.
func TestEvict(t *testing.T) {
	t.Parallel()

	idx, err := NewExpiryIndex(4)
	if err != nil {
		t.Fatalf("NewExpiryIndex: %v", err)
	}
	mustInsert(t, idx, "d", 10)
	mustInsert(t, idx, "a", 1)
	mustInsert(t, idx, "c", 3)
	mustInsert(t, idx, "b", 2)

	tests := []struct {
		name    string
		now     int64
		want    []string
		wantLen int
	}{
		{name: "nothing expired yet", now: 0, want: nil, wantLen: 4},
		{name: "exactly at first expiry", now: 1, want: []string{"a"}, wantLen: 3},
		{name: "past two more at once", now: 3, want: []string{"b", "c"}, wantLen: 1},
		{name: "past everything remaining", now: 100, want: []string{"d"}, wantLen: 0},
		{name: "empty index, nothing to evict", now: 200, want: nil, wantLen: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := idx.Evict(tc.now)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Evict(%d) = %v, want %v", tc.now, got, tc.want)
			}
			if idx.Len() != tc.wantLen {
				t.Fatalf("Evict(%d): Len() = %d, want %d", tc.now, idx.Len(), tc.wantLen)
			}
		})
	}
}

// TestInsertKeepsSortedOrder feeds entries in scrambled ExpiresAt order,
// then evicts at each distinct ExpiresAt value in ascending order and
// checks exactly one entry comes out each time -- the property that only
// holds if Insert placed every entry at its correct sorted position.
func TestInsertKeepsSortedOrder(t *testing.T) {
	t.Parallel()

	idx, err := NewExpiryIndex(0)
	if err != nil {
		t.Fatalf("NewExpiryIndex: %v", err)
	}
	scrambled := []int64{40, 10, 30, 20, 50}
	for i, exp := range scrambled {
		mustInsert(t, idx, fmt.Sprintf("k%d", i), exp)
	}

	sorted := slices.Clone(scrambled)
	slices.Sort(sorted)
	for _, exp := range sorted {
		got := idx.Evict(exp)
		if len(got) != 1 {
			t.Fatalf("Evict(%d) = %v, want exactly one key", exp, got)
		}
	}
	if idx.Len() != 0 {
		t.Fatalf("Len() after evicting every entry = %d, want 0", idx.Len())
	}
}

func TestEvictOnEmptyIndex(t *testing.T) {
	t.Parallel()

	idx, err := NewExpiryIndex(0)
	if err != nil {
		t.Fatalf("NewExpiryIndex: %v", err)
	}
	if got := idx.Evict(1000); got != nil {
		t.Fatalf("Evict on empty index = %v, want nil", got)
	}
}

// TestBuggyRangeRemovalLeavesExpiredKeysResident is the heart of the
// module. Four entries expire at 1, 2, 3, and 10; sweeping at now=5 should
// evict the first three. evictBuggy's in-place, index-based removal skips
// "b" (ExpiresAt=2) because removing "a" shifts "b" into the very slot the
// range loop has already consumed, so "b" survives a sweep it should not
// have. The real ExpiryIndex evicts all three in one pass.
func TestBuggyRangeRemovalLeavesExpiredKeysResident(t *testing.T) {
	t.Parallel()

	build := func() []Entry {
		return []Entry{{Key: "a", ExpiresAt: 1}, {Key: "b", ExpiresAt: 2}, {Key: "c", ExpiresAt: 3}, {Key: "d", ExpiresAt: 10}}
	}

	buggy := evictBuggy(build(), 5)
	if !slices.ContainsFunc(buggy, func(e Entry) bool { return e.Key == "b" && e.ExpiresAt <= 5 }) {
		t.Fatalf("evictBuggy(now=5) = %v, want it to still contain the already-expired key %q", buggy, "b")
	}

	idx, err := NewExpiryIndex(4)
	if err != nil {
		t.Fatalf("NewExpiryIndex: %v", err)
	}
	for _, e := range build() {
		mustInsert(t, idx, e.Key, e.ExpiresAt)
	}
	evicted := idx.Evict(5)
	if !slices.Equal(evicted, []string{"a", "b", "c"}) {
		t.Fatalf("Evict(5) = %v, want [a b c]", evicted)
	}
	if idx.Len() != 1 {
		t.Fatalf("Len() after Evict(5) = %d, want 1", idx.Len())
	}
}

// TestEvictedSliceDoesNotAliasIndex pins the aliasing contract: Evict's
// result is a fresh []string, never a view into the index's storage.
func TestEvictedSliceDoesNotAliasIndex(t *testing.T) {
	t.Parallel()

	idx, err := NewExpiryIndex(2)
	if err != nil {
		t.Fatalf("NewExpiryIndex: %v", err)
	}
	mustInsert(t, idx, "only", 1)

	evicted := idx.Evict(10)
	evicted[0] = "mutated"

	mustInsert(t, idx, "only", 1)
	again := idx.Evict(10)
	if again[0] == "mutated" {
		t.Fatalf("mutating a previous Evict result corrupted a later one")
	}
}

// ExampleExpiryIndex is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleExpiryIndex() {
	idx, err := NewExpiryIndex(0)
	if err != nil {
		panic(err)
	}

	_ = idx.Insert(Entry{Key: "session-42", ExpiresAt: 100})
	_ = idx.Insert(Entry{Key: "session-7", ExpiresAt: 50})
	_ = idx.Insert(Entry{Key: "session-99", ExpiresAt: 150})

	fmt.Println("evicted at 60:", idx.Evict(60))
	fmt.Println("remaining:", idx.Len())
	fmt.Println("evicted at 200:", idx.Evict(200))
	fmt.Println("remaining:", idx.Len())

	// Output:
	// evicted at 60: [session-7]
	// remaining: 2
	// evicted at 200: [session-42 session-99]
	// remaining: 0
}
```

## Review

`Evict` is correct when every entry with `ExpiresAt <= now` comes out in one
call and nothing that qualifies is left behind. The mechanism is an
upper-bound binary search that treats an exact tie as belonging to the
expired side, followed by a single `slices.Delete(idx.entries, 0, hi)`
reassigned back into the index — the reassignment shrinks the slice, and
`slices.Delete`'s documented zeroing of the vacated tail is what actually
releases the evicted keys' string data to the garbage collector. The mistake
this design avoids is removing expired entries one at a time with
`append(s[:i], s[i+1:]...)` inside a `for range` over that same slice: the
removal shifts a later element into the very position the loop is about to
skip past, so a run of consecutive expired keys loses every other one to
the sweep. Around that core, `NewExpiryIndex` rejects a negative capacity
and `Insert` rejects an empty key, both checkable with `errors.Is`, and
`Evict`'s returned key slice is freshly allocated and never aliases the
index's internal storage. Run `go test -count=1 -race ./...` to confirm the
eviction table, the sorted-insert property, the constructor and insert
guards, the aliasing guarantee, and the buggy range-removal contrast.

## Resources

- [`slices.Delete`](https://pkg.go.dev/slices#Delete) — the batch removal used here, including its documented zeroing of the vacated tail.
- [`slices.BinarySearchFunc`](https://pkg.go.dev/slices#BinarySearchFunc) — the upper-bound search that locates the expired/live boundary.
- [Redis: how expiration is evaluated](https://redis.io/docs/latest/develop/reference/expire/) — the active-expiration cycle this module models.
- [Go Wiki: SliceTricks](https://go.dev/wiki/SliceTricks) — the `append(s[:i], s[i+1:]...)` idiom this module's antipattern misuses inside a range loop.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-sliding-window-rate-limiter.md](13-sliding-window-rate-limiter.md) | Next: [15-sorted-key-index-lazy-range-scan.md](15-sorted-key-index-lazy-range-scan.md)
