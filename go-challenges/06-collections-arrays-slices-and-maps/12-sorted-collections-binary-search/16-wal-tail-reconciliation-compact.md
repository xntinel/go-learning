# Exercise 16: WAL Tail Reconciliation via slices.CompactFunc

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A Raft or etcd-style leader failover leaves two write-ahead logs that mostly
agree but diverge at the very end: the old leader appended an entry, crashed
before every follower acknowledged it, and the newly promoted follower's own
log carries a copy of the same entry it received just before the crash.
Bringing the cluster back into a single consistent log means merging both
tails into one (Term, Index)-ordered sequence with every duplicate collapsed
to a single copy -- not two, because a state machine that applies the same
mutation twice is a state machine that just corrupted its own data.

The two building blocks for this are `slices.SortFunc`, which produces one
ordered sequence out of the two logs concatenated, and `slices.CompactFunc`,
which removes adjacent duplicates from an already-sorted slice in place. Both
share a trap common to every `slices` function that can shrink or reallocate:
they return the new slice header rather than mutating the one you passed in,
and a caller who forgets to reassign it keeps looking at stale data through
the old length -- exactly the same discipline `slices.Delete` and `append`
already demand, applied here to a function this lesson's other modules never
reach for.

This module builds `Reconcile`, a pure function over two logs that validates
each input is sorted by (Term, Index) before it does anything else, because a
merge over an unsorted log does not fail loudly -- it produces a wrong
ordering that looks plausible until a downstream reader trips over it.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
walreconcile/            module example.com/walreconcile
  go.mod                 go 1.24
  walreconcile.go         LogEntry, Reconcile; sentinel error ErrNotSorted
  walreconcile_test.go    merge table, sortedness rejection, aliasing,
                          concurrency, the discarded-CompactFunc contrast,
                          ExampleReconcile
```

- Files: `walreconcile.go`, `walreconcile_test.go`.
- Implement: `type LogEntry struct { Term, Index int64; Data string }`; `func Reconcile(a, b []LogEntry) ([]LogEntry, error)`, which returns an error wrapping `ErrNotSorted` if either input fails `slices.IsSortedFunc` against the (Term, Index) comparator, otherwise concatenates both inputs into a freshly allocated slice, sorts it with `slices.SortFunc`, deduplicates it with `slices.CompactFunc`, and returns the reassigned result.
- Test: disjoint logs that interleave, an overlapping tail replayed identically, a duplicate inside a single input, empty `a`, empty `b`, both empty, a higher term outranking a later index of a lower term, both sortedness-violation cases mapped to `ErrNotSorted` with `errors.Is`, the result never aliasing either input, safety under concurrent calls, the discarded-`CompactFunc` contrast, and `ExampleReconcile` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### CompactFunc returns a new header; drop it and the shrink never happened

`slices.CompactFunc(s, eq)` walks an already-sorted slice and, for every
maximal run of adjacent elements `eq` reports equal, keeps only the first
element of the run. It does its work in place on `s`'s backing array -- no
extra allocation -- but the elements past the new, shorter length are not
left as leftover duplicates. The standard library zeroes them, because a
slice that still references removed elements past its reported length would
keep them alive for the garbage collector for no reason. `CompactFunc`
returns the slice header with the new, shorter length; the backing array
underneath is shared, but the length that says how much of it is valid data
changes, and that change only reaches the caller through the return value.

```go
slices.SortFunc(merged, compareEntry)
slices.CompactFunc(merged, sameEntry) // return value discarded
return merged                         // still the OLD length
```

Call it as a bare statement and `merged` keeps its pre-compact length. The
elements that used to hold the duplicate are not still duplicates -- they are
now the zero value, `LogEntry{Term: 0, Index: 0, Data: ""}`, because
`CompactFunc` zeroed them before returning. A caller that ignores the return
value does not get a log with a repeated entry; it gets a log with a bogus
entry claiming to be term 0, index 0, spliced onto the end -- an entry a Raft
state machine would try to apply as if it were the very first write the
cluster ever made. The fix is the same one-line discipline every
slice-shrinking function in this package demands: `merged =
slices.CompactFunc(merged, sameEntry)`.

The other half of `Reconcile` is validating its own precondition instead of
trusting it. Both `a` and `b` must already be sorted by (Term, Index) --
`slices.SortFunc` over their concatenation only produces a correct merge if
each half was already in order going in, the same way merging two sorted runs
in mergesort depends on each run being sorted. `slices.IsSortedFunc` checks
that assumption with the exact comparator the sort itself uses, so a caller
that passes an unsorted log gets `ErrNotSorted` instead of a merge that looks
plausible and is wrong.

Create `walreconcile.go`:

```go
// Package walreconcile merges the write-ahead logs of two Raft-style replicas
// that overlap at the tail after a leader failover into one deduplicated,
// (Term, Index)-ordered sequence.
//
// A follower promoted to leader and the log the old leader left behind
// typically share a prefix and diverge (or merely repeat) at the tail: the
// old leader may have replicated an entry to itself and crashed before the
// follower's ack landed, so both copies of the same (Term, Index) entry show
// up when the two logs are combined. Reconcile assumes each input is already
// sorted by (Term, Index) -- the invariant every Raft log append upholds --
// and rejects an input that violates it rather than silently returning a
// wrong merge.
package walreconcile

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
)

// ErrNotSorted is returned by Reconcile when an input log is not sorted
// ascending by (Term, Index). Reconcile's merge depends on that invariant;
// callers should uphold it on every append and check for this sentinel with
// errors.Is rather than assuming the merge silently degrades to a correct
// answer over unsorted input -- it does not.
var ErrNotSorted = errors.New("walreconcile: log is not sorted by (term, index)")

// LogEntry is one write-ahead-log record. Two entries with the same Term and
// Index are considered the same entry regardless of Data, matching the Raft
// invariant that any two log entries sharing (Term, Index) carry identical
// data -- Reconcile relies on exactly that invariant when it deduplicates.
type LogEntry struct {
	Term  int64
	Index int64
	Data  string
}

// compareEntry orders entries ascending by Term, then by Index, and is the
// single comparator every sortedness check and sort in this package uses.
func compareEntry(x, y LogEntry) int {
	return cmp.Or(cmp.Compare(x.Term, y.Term), cmp.Compare(x.Index, y.Index))
}

// sameEntry reports whether x and y are the same log entry: equal Term and
// Index. It ignores Data, per the Raft invariant documented on LogEntry.
func sameEntry(x, y LogEntry) bool {
	return x.Term == y.Term && x.Index == y.Index
}

// Reconcile merges a and b, each assumed sorted ascending by (Term, Index),
// into a single slice sorted the same way with every duplicate (Term, Index)
// pair collapsed to one entry. It is the tail-reconciliation step a Raft or
// etcd-style leader failover performs: a is typically the new leader's log
// and b the promoted follower's log, or vice versa; the order of the
// arguments does not affect the result.
//
// Reconcile validates both inputs with slices.IsSortedFunc before merging
// and returns an error wrapping ErrNotSorted, naming the offending argument,
// if either violates the invariant. A nil or empty input is valid and
// trivially sorted.
//
// Reconcile is a pure function over its arguments: it holds no state, reads
// a and b only, and is safe to call concurrently from multiple goroutines,
// including concurrently on overlapping inputs. The returned slice is a
// freshly allocated backing array; it never aliases a or b, so the caller
// may retain, sort, or mutate it without affecting either input.
func Reconcile(a, b []LogEntry) ([]LogEntry, error) {
	if !slices.IsSortedFunc(a, compareEntry) {
		return nil, fmt.Errorf("%w: first argument", ErrNotSorted)
	}
	if !slices.IsSortedFunc(b, compareEntry) {
		return nil, fmt.Errorf("%w: second argument", ErrNotSorted)
	}

	merged := make([]LogEntry, 0, len(a)+len(b))
	merged = append(merged, a...)
	merged = append(merged, b...)
	slices.SortFunc(merged, compareEntry)

	// CompactFunc may shrink the slice in place and always returns the new
	// header; the result must be reassigned or the shrink is lost and the
	// stale duplicates it removed remain reachable through the old length.
	merged = slices.CompactFunc(merged, sameEntry)
	return merged, nil
}
```

### Using it

`Reconcile` has no configuration to validate at construction time -- it is a
pure function of its two arguments, so there is no `New` and no state to
share -- and that statelessness is exactly what makes it safe to call from
every replica-sync goroutine at once without a mutex, which
`TestReconcileSafeForConcurrentUse` holds it to. Import the package, pass it
the two logs in either order, and check the returned error with
`errors.Is(err, walreconcile.ErrNotSorted)` before touching the result.

The result never shares a backing array with either input --
`TestReconcileDoesNotAliasInputs` pins that -- so a caller may sort it,
truncate it, or hand it straight to a log-apply loop without any risk of
corrupting `a` or `b` in the process.

`ExampleReconcile` is the runnable demonstration of this module: `go test`
executes it and compares its stdout against the `// Output:` comment below.

```go
func ExampleReconcile() {
	newLeader := []LogEntry{
		{Term: 2, Index: 8, Data: "set x=1"},
		{Term: 2, Index: 9, Data: "set y=2"},
	}
	promotedFollower := []LogEntry{
		{Term: 2, Index: 9, Data: "set y=2"}, // replayed before the crash
		{Term: 2, Index: 10, Data: "set z=3"},
	}

	merged, err := Reconcile(newLeader, promotedFollower)
	if err != nil {
		panic(err)
	}
	for _, e := range merged {
		fmt.Printf("term=%d index=%d data=%s\n", e.Term, e.Index, e.Data)
	}

	// Output:
	// term=2 index=8 data=set x=1
	// term=2 index=9 data=set y=2
	// term=2 index=10 data=set z=3
}
```

### Tests

`TestReconcile` is the merge table: disjoint logs whose entries interleave, an
overlapping tail entry replayed byte-for-byte, a duplicate that exists inside
a single input rather than across the two, both inputs empty, one input
empty, and a case where a higher term outranks a later index belonging to a
lower term -- the comparator must never fall back to comparing Index when Term
already decides the order. `TestReconcileRejectsUnsortedInput` checks both
argument positions map their sortedness violation to `ErrNotSorted`.

`TestDiscardingCompactResultLeavesZeroValueTail` is the heart of the module.
`reconcileDiscardingCompactResult` is unexported and unreachable from the
package API; it exists so the test can state the defect precisely -- the
buggy result keeps the pre-compact length, and the slot the duplicate used to
occupy is now the zero-value `LogEntry{}`, not a second copy of the real
entry -- and then show `Reconcile` over the same input producing exactly the
deduplicated entries with no zero value anywhere. `TestReconcileSafeForConcurrentUse`
runs the same merge from twenty goroutines to back the concurrency contract
in the doc comment.

Create `walreconcile_test.go`:

```go
package walreconcile

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

func TestReconcile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a    []LogEntry
		b    []LogEntry
		want []LogEntry
	}{
		{
			name: "disjoint logs interleave",
			a:    []LogEntry{{1, 1, "a1"}, {1, 3, "a3"}},
			b:    []LogEntry{{1, 2, "b2"}, {1, 4, "b4"}},
			want: []LogEntry{{1, 1, "a1"}, {1, 2, "b2"}, {1, 3, "a3"}, {1, 4, "b4"}},
		},
		{
			name: "overlapping tail replayed identically",
			a:    []LogEntry{{1, 1, "x"}, {1, 2, "y"}, {2, 3, "z"}},
			b:    []LogEntry{{2, 3, "z"}, {2, 4, "w"}},
			want: []LogEntry{{1, 1, "x"}, {1, 2, "y"}, {2, 3, "z"}, {2, 4, "w"}},
		},
		{
			name: "duplicate within a single input",
			a:    []LogEntry{{1, 1, "x"}, {1, 1, "x"}},
			b:    nil,
			want: []LogEntry{{1, 1, "x"}},
		},
		{
			name: "empty a",
			a:    nil,
			b:    []LogEntry{{1, 1, "x"}},
			want: []LogEntry{{1, 1, "x"}},
		},
		{
			name: "empty b",
			a:    []LogEntry{{1, 1, "x"}},
			b:    nil,
			want: []LogEntry{{1, 1, "x"}},
		},
		{
			name: "both empty",
			a:    nil,
			b:    nil,
			want: []LogEntry{},
		},
		{
			name: "higher term outranks a later index of a lower term",
			a:    []LogEntry{{1, 5, "old"}},
			b:    []LogEntry{{2, 1, "new"}},
			want: []LogEntry{{1, 5, "old"}, {2, 1, "new"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Reconcile(tc.a, tc.b)
			if err != nil {
				t.Fatalf("Reconcile: unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Reconcile() = %+v, want %+v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("Reconcile()[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestReconcileRejectsUnsortedInput(t *testing.T) {
	t.Parallel()

	unsorted := []LogEntry{{1, 2, "later"}, {1, 1, "earlier"}}
	sorted := []LogEntry{{1, 1, "x"}}

	if _, err := Reconcile(unsorted, sorted); !errors.Is(err, ErrNotSorted) {
		t.Fatalf("Reconcile(unsorted, sorted) error = %v, want ErrNotSorted", err)
	}
	if _, err := Reconcile(sorted, unsorted); !errors.Is(err, ErrNotSorted) {
		t.Fatalf("Reconcile(sorted, unsorted) error = %v, want ErrNotSorted", err)
	}
}

func TestReconcileDoesNotAliasInputs(t *testing.T) {
	t.Parallel()

	a := []LogEntry{{1, 1, "a"}}
	b := []LogEntry{{1, 2, "b"}}

	got, err := Reconcile(a, b)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got[0].Data = "mutated"
	if a[0].Data != "a" {
		t.Fatalf("mutating the result changed a: %q", a[0].Data)
	}
	if b[0].Data != "b" {
		t.Fatalf("mutating the result changed b: %q", b[0].Data)
	}
}

// reconcileDiscardingCompactResult is the bug this module contrasts, kept
// unexported and unreachable from the package API. slices.CompactFunc
// compacts in place and zeroes the now-unused tail of the backing array for
// GC safety, then returns a shorter slice header over the same array. Called
// as a bare statement, that header is thrown away: the caller keeps looking
// at the pre-compact length, so the tail it walks past the true end is not
// the removed duplicate but a zero-value LogEntry{0, 0, ""} -- an entry the
// state machine would apply as term 0, index 0, colliding with (or
// preceding) the log's real starting point.
func reconcileDiscardingCompactResult(a, b []LogEntry) []LogEntry {
	merged := make([]LogEntry, 0, len(a)+len(b))
	merged = append(merged, a...)
	merged = append(merged, b...)
	slices.SortFunc(merged, compareEntry)
	_ = slices.CompactFunc(merged, sameEntry) // BUG: return value discarded
	return merged
}

// TestDiscardingCompactResultLeavesZeroValueTail is the heart of the
// module: it pins the exact defect of dropping CompactFunc's return value
// -- a trailing zero-value LogEntry the state machine would misapply as
// term 0, index 0 -- and shows the same input through Reconcile carrying
// only the real, deduplicated entries.
func TestDiscardingCompactResultLeavesZeroValueTail(t *testing.T) {
	t.Parallel()

	a := []LogEntry{{1, 1, "x"}, {1, 2, "y"}}
	b := []LogEntry{{1, 2, "y"}} // replayed tail entry

	buggy := reconcileDiscardingCompactResult(a, b)
	if len(buggy) != 3 {
		t.Fatalf("len(buggy) = %d, want 3 (compact ran but its shrink was discarded)", len(buggy))
	}
	if buggy[2] != (LogEntry{}) {
		t.Fatalf("buggy[2] = %+v, want the zero-value LogEntry compact left behind", buggy[2])
	}

	good, err := Reconcile(a, b)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(good) != 2 {
		t.Fatalf("len(good) = %d, want 2", len(good))
	}
	for _, e := range good {
		if e == (LogEntry{}) {
			t.Fatalf("Reconcile result carries a zero-value entry: %+v", good)
		}
	}
}

func TestReconcileSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	a := []LogEntry{{1, 1, "x"}, {1, 3, "z"}}
	b := []LogEntry{{1, 2, "y"}, {1, 3, "z"}}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Reconcile(a, b)
			if err != nil {
				t.Errorf("Reconcile: %v", err)
				return
			}
			if len(got) != 3 {
				t.Errorf("Reconcile() len = %d, want 3", len(got))
			}
		}()
	}
	wg.Wait()
}

// ExampleReconcile is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleReconcile() {
	newLeader := []LogEntry{
		{Term: 2, Index: 8, Data: "set x=1"},
		{Term: 2, Index: 9, Data: "set y=2"},
	}
	promotedFollower := []LogEntry{
		{Term: 2, Index: 9, Data: "set y=2"}, // replayed before the crash
		{Term: 2, Index: 10, Data: "set z=3"},
	}

	merged, err := Reconcile(newLeader, promotedFollower)
	if err != nil {
		panic(err)
	}
	for _, e := range merged {
		fmt.Printf("term=%d index=%d data=%s\n", e.Term, e.Index, e.Data)
	}

	// Output:
	// term=2 index=8 data=set x=1
	// term=2 index=9 data=set y=2
	// term=2 index=10 data=set z=3
}
```

## Review

`Reconcile` is correct when it rejects any input that is not sorted by
(Term, Index) before touching it, and when its result carries every distinct
entry from both logs exactly once, in order, with no zero-value entries
anywhere in it. The trap this module isolates is `slices.CompactFunc`'s
return value: the function zeroes the elements it removes and reports the
new, shorter length only through what it returns, so a caller who calls it as
a bare statement keeps the old length and finds a bogus term-0/index-0 entry
spliced onto the tail instead of the duplicate they meant to remove -- a
subtler failure than a plain double-apply, and one `TestDiscardingCompactResultLeavesZeroValueTail`
pins exactly. `ErrNotSorted`, checkable with `errors.Is`, is what keeps an
out-of-order log from silently producing a wrong merge instead of a loud
rejection. `Reconcile` holds no state, so it is safe for any number of
goroutines to call at once, and its result never aliases either input.
`ExampleReconcile` is the executable documentation: `go test` verifies its
output. Run `go test -count=1 -race ./...`.

## Resources

- [`slices.CompactFunc`](https://pkg.go.dev/slices#CompactFunc) — the dedup primitive this module builds around, including the in-place zeroing behavior.
- [`slices.SortFunc`](https://pkg.go.dev/slices#SortFunc) — the merge step, sorting the concatenation of both logs by a custom comparator.
- [`slices.IsSortedFunc`](https://pkg.go.dev/slices#IsSortedFunc) — the precondition check this module runs before trusting either input.
- [Raft consensus paper, section 5.3](https://raft.github.io/raft.pdf) — log matching and the tail-overlap scenario this module models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-sorted-key-index-lazy-range-scan.md](15-sorted-key-index-lazy-range-scan.md) | Next: [17-paginated-log-exponential-seek.md](17-paginated-log-exponential-seek.md)
