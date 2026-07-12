# Exercise 19: Top-N Leaderboard That Never Reorders the Caller's Slice

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A game leaderboard's "top 10" query and a search engine's "top 10 results"
query are the same operation: given a slice of scored records, return the
highest-scoring `n` without disturbing anything else. The natural way to
write it reaches for `sort.Sort` or `slices.SortFunc`, and both of those sort
their argument *in place* -- that is the whole reason they are efficient,
and it is documented plainly, and it is also the exact shape of the mistake
this exercise is about. `Top(records, n, less)` that hands `records` straight
to `slices.SortFunc` has silently reordered the caller's own slice as a side
effect of what looks like a read-only query.

Every earlier exercise in this lesson taught cloning at a boundary where a
slice enters or leaves a function -- a repository read, a scanner token, a
buffer handed to an async sink. This one is different in an important way:
the caller does not hand `Top` a view into someone else's internal state, it
hands over its *own* slice, fully expecting to keep using it afterward. The
bug does not corrupt data belonging to another part of the system; it
corrupts the caller's own variable, invisibly, and only becomes observable
the moment that caller reuses the slice right after the call -- rendering a
page in original database order, say, only to find it has been silently
resorted by score. That delay between cause and symptom is what makes this
mistake easy to ship: the function's own return value is completely correct
every time.

This module builds `topn`, a package whose `Top` function clones before it
sorts, so the query and the caller's own slice never interfere with each
other.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
topn/                     module example.com/topn
  go.mod                  go 1.24
  topn.go                  Record; Top(records []Record, n int, less func(a, b Record) bool) []Record
  topn_test.go             ranking table, ties, aliasing, the topNAliased contrast,
                          allocation property, concurrency, ExampleTop
```

- Files: `topn.go`, `topn_test.go`.
- Implement: `Top(records []Record, n int, less func(a, b Record) bool) []Record` cloning `records` with `slices.Clone`, sorting the clone with `slices.SortFunc` driven by `less`, and returning at most `n` of it -- clamping a non-positive `n` to an empty result and an oversized `n` to `len(records)`.
- Test: the ranking table across ordinary, clamped-to-all, zero, negative, and single-result `n`; nil and empty `records`; the returned slice never aliasing the input; ties resolved by score-order property rather than an assumed stable order; the `topNAliased` contrast proving the caller's own slice gets resorted; the allocation property that requesting one result never costs more than requesting all of them; `Top` is safe for concurrent use; and `ExampleTop` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### `slices.Clone` before `slices.SortFunc`, not after

`slices.SortFunc(s, cmp)` sorts `s` in place and returns nothing -- there is
no new slice to accidentally use instead of the old one, because there is no
new slice at all. Every element of `s` is where the sort put it, in the same
backing array the caller still holds a reference to. A `Top` written the
obvious way looks entirely reasonable:

```go
func topNAliased(records []Record, n int, less func(a, b Record) bool) []Record {
    slices.SortFunc(records, cmpFromLess(less))   // sorts the caller's own slice
    return records[:n]
}
```

This returns the correct top-`n` result every single time -- the bug is
invisible from the return value. It shows up only in what happened to
`records` itself: the caller's variable now points at the same backing
array, resorted, no longer in whatever order it was in before the call. If
that caller was about to render the same slice in a different order, or
pass it to a second call with a different comparator, both of those now
observe a mutation they never asked for. The fix is the clone-before-mutate
discipline this lesson has used at every other boundary, applied here to the
caller's *own* argument rather than to internal state being handed out:

```go
ranked := slices.Clone(records)   // an independent backing array
slices.SortFunc(ranked, cmpFromLease(less))
return ranked[:n]
```

`slices.Clone` makes no promise about the returned slice's capacity, only
its length and contents, so `Top` never asserts anything about `cap` on the
result -- only that its order and length are correct, and that `records`
itself is untouched.

Create `topn.go`:

```go
// Package topn answers "top N" queries -- the kind behind a game
// leaderboard or a search-results ranker -- without disturbing the order of
// the caller's own slice, since both sort.Sort and slices.SortFunc sort in
// place.
package topn

import "slices"

// Record is one ranked entry: a name and the score it is ranked by.
type Record struct {
	Name  string
	Score int
}

// Top returns the highest-ranked n records from records, ordered by less: a
// standard slices.SortFunc comparator where less(a, b) reports whether a
// should sort before b, so a "highest score first" leaderboard passes
// func(a, b Record) bool { return a.Score > b.Score }.
//
// Top has no configuration and therefore no constructor; it fully validates
// its own arguments instead. n is clamped: a non-positive n returns an
// empty, non-nil slice, and an n greater than len(records) is reduced to
// len(records). A nil records is treated as empty.
//
// The returned slice is an independent clone; sorting it, appending to it,
// or mutating a Record inside it never affects records or its order. Top
// holds no state and is safe to call concurrently, so long as concurrent
// callers do not mutate the same records slice while another call is
// reading it -- the same rule that applies to any function reading a slice
// it does not own.
func Top(records []Record, n int, less func(a, b Record) bool) []Record {
	if n <= 0 || len(records) == 0 {
		return []Record{}
	}

	ranked := slices.Clone(records)
	slices.SortFunc(ranked, func(a, b Record) int {
		switch {
		case less(a, b):
			return -1
		case less(b, a):
			return 1
		default:
			return 0
		}
	})

	if n > len(ranked) {
		n = len(ranked)
	}
	return ranked[:n]
}
```

### Using it

`Top` has no configuration to validate, so unlike most of the components in
this lesson it has no `New` constructor -- the doc comment says so
explicitly, so a reader does not go looking for one. Call it directly with a
`less` matching whatever ranking the caller needs; the same package serves a
"highest score first" leaderboard and a "lowest latency first" ranker with
two different comparators and no change to `Top` itself. Because `Top` holds
no state, it is safe to call from many goroutines at once, and
`TestTopIsSafeForConcurrentUse` holds it to that.

The one contract worth internalizing is the one this module exists to teach:
the returned slice is a clone, and `records` -- the caller's own argument --
is guaranteed untouched, both in its contents and in its order, no matter
what `n` or `less` was passed. That is what lets a caller pass its live
in-memory leaderboard slice straight into `Top` without a defensive copy of
its own.

The module's runnable demonstration is `ExampleTop`: `go test` runs it and
compares its stdout against the `// Output:` comment, so the usage shown
below cannot drift away from the code.

```go
func ExampleTop() {
	records := []Record{
		{Name: "alice", Score: 40},
		{Name: "bob", Score: 90},
		{Name: "carol", Score: 70},
	}

	top := Top(records, 2, func(a, b Record) bool { return a.Score > b.Score })
	for _, r := range top {
		fmt.Printf("%s: %d\n", r.Name, r.Score)
	}
	fmt.Println("caller's slice still starts with:", records[0].Name)

	// Output:
	// bob: 90
	// carol: 70
	// caller's slice still starts with: alice
}
```

`records[0]` is printed last, after `Top` has already run, and it still
reads `"alice"` -- the position it held before the call -- because `Top`
never touched the caller's backing array.

### Tests

`TestTop` is the ranking table: an ordinary top-3, an `n` larger than the
input clamped to all of it, `n` zero and negative both returning empty, and
`n` one returning the single leader. `TestTopOnEmptyOrNilRecords` checks both
edge inputs return a non-nil empty slice rather than `nil`.
`TestTopReturnedSliceDoesNotAliasInput` mutates a `Record` in the result and
confirms the caller's `records` is untouched. `TestTopHandlesTiesAndDuplicateScores`
does not assume a stable tie-break order -- `slices.SortFunc` makes no such
promise -- and instead checks the property that must hold regardless: every
returned record's score is at least as high as every record left out.

`TestTopDoesNotReorderCallerSliceButTopNAliasedDoes` is the heart of the
module. `topNAliased` is unexported and unreachable from the package API; it
performs the identical sort but on the caller's own slice. The test captures
the caller's slice order before either call, then shows `Top` leaves it
exactly as it was while `topNAliased` has resorted it in place --
`aliasedRecords[0]` ends up the highest scorer, proof the caller's own
variable was mutated. `TestTopAllocatesOnceRegardlessOfN` checks a property,
not a count: asking for the single top record never costs more allocations
than asking for all of them, confirming the clone happens once up front
rather than growing with `n`.

Create `topn_test.go`:

```go
package topn

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

func byScoreDesc(a, b Record) bool { return a.Score > b.Score }

func sampleRecords() []Record {
	return []Record{
		{Name: "alice", Score: 40},
		{Name: "bob", Score: 90},
		{Name: "carol", Score: 70},
		{Name: "dave", Score: 20},
		{Name: "erin", Score: 85},
	}
}

func TestTop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		want []string // names, in expected order
	}{
		{name: "top three by score descending", n: 3, want: []string{"bob", "erin", "carol"}},
		{name: "n larger than the input clamps to all records", n: 100, want: []string{"bob", "erin", "carol", "alice", "dave"}},
		{name: "n zero returns empty", n: 0, want: []string{}},
		{name: "n negative returns empty", n: -5, want: []string{}},
		{name: "n one returns the single leader", n: 1, want: []string{"bob"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Top(sampleRecords(), tc.n, byScoreDesc)
			if len(got) != len(tc.want) {
				t.Fatalf("len(Top) = %d, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, name := range tc.want {
				if got[i].Name != name {
					t.Errorf("Top[%d].Name = %q, want %q", i, got[i].Name, name)
				}
			}
			if got == nil {
				t.Error("Top returned nil, want a non-nil (possibly empty) slice")
			}
		})
	}
}

func TestTopOnEmptyOrNilRecords(t *testing.T) {
	t.Parallel()

	if got := Top(nil, 5, byScoreDesc); len(got) != 0 || got == nil {
		t.Fatalf("Top(nil, ...) = %#v, want a non-nil empty slice", got)
	}
	if got := Top([]Record{}, 5, byScoreDesc); len(got) != 0 || got == nil {
		t.Fatalf("Top([]Record{}, ...) = %#v, want a non-nil empty slice", got)
	}
}

// TestTopReturnedSliceDoesNotAliasInput confirms the clone: mutating a
// Record in the result must never reach the caller's own records.
func TestTopReturnedSliceDoesNotAliasInput(t *testing.T) {
	t.Parallel()

	records := sampleRecords()
	top := Top(records, 2, byScoreDesc)
	top[0].Name = "mutated"

	for _, r := range records {
		if r.Name == "mutated" {
			t.Fatalf("mutating the Top result changed the caller's records: %+v", records)
		}
	}
}

// topNAliased is the version of this query a first draft tends to ship: it
// sorts the caller's own slice in place instead of a clone, because
// slices.SortFunc's in-place contract is easy to forget when all you are
// thinking about is "get me the top n." It is unexported and unreachable
// from the package API; it exists only so the tests can pin the exact
// side effect it has on the caller's slice.
func topNAliased(records []Record, n int, less func(a, b Record) bool) []Record {
	slices.SortFunc(records, func(a, b Record) int {
		switch {
		case less(a, b):
			return -1
		case less(b, a):
			return 1
		default:
			return 0
		}
	})
	if n < 0 {
		n = 0
	}
	if n > len(records) {
		n = len(records)
	}
	return records[:n]
}

// TestTopDoesNotReorderCallersSliceButTopNAliasedDoes is the heart of this
// module. It captures the caller's slice order before the call, runs both
// functions, and shows Top leaves the caller's slice exactly as it was
// while topNAliased has silently resorted it -- the bug only shows up if
// the caller reuses that slice right after the call returns, which is
// exactly what this test does.
func TestTopDoesNotReorderCallerSliceButTopNAliasedDoes(t *testing.T) {
	t.Parallel()

	original := sampleRecords()
	before := slices.Clone(original)

	callerRecords := slices.Clone(original)
	_ = Top(callerRecords, 3, byScoreDesc)
	if !slices.Equal(callerRecords, before) {
		t.Fatalf("Top reordered the caller's slice: got %+v, want unchanged %+v", callerRecords, before)
	}

	aliasedRecords := slices.Clone(original)
	_ = topNAliased(aliasedRecords, 3, byScoreDesc)
	if slices.Equal(aliasedRecords, before) {
		t.Fatalf("topNAliased left the caller's slice unchanged; expected it to have been sorted in place")
	}
	if aliasedRecords[0].Name != "bob" {
		t.Fatalf("topNAliased caller slice[0] = %+v, want the highest scorer first, proving it was sorted in place", aliasedRecords[0])
	}
}

// TestTopHandlesTiesAndDuplicateScores does not assume any particular order
// among tied records -- slices.SortFunc makes no stability promise -- and
// instead checks the property that must hold regardless of how ties break:
// every returned record's score is at least as high as every record left
// out.
func TestTopHandlesTiesAndDuplicateScores(t *testing.T) {
	t.Parallel()

	records := []Record{
		{Name: "a", Score: 50},
		{Name: "b", Score: 50},
		{Name: "c", Score: 50},
		{Name: "d", Score: 10},
	}

	top := Top(records, 2, byScoreDesc)
	if len(top) != 2 {
		t.Fatalf("len(Top) = %d, want 2", len(top))
	}

	returned := make(map[string]bool, len(top))
	minReturnedScore := top[0].Score
	for _, r := range top {
		returned[r.Name] = true
		if r.Score < minReturnedScore {
			minReturnedScore = r.Score
		}
	}
	for _, r := range records {
		if !returned[r.Name] && r.Score > minReturnedScore {
			t.Fatalf("record %+v was left out despite outscoring a returned record (min returned score %d)", r, minReturnedScore)
		}
	}
}

// TestTopAllocatesOnceRegardlessOfN checks a property, not a count: Top
// clones the full input exactly once no matter how small n is, so its
// allocation footprint does not grow as n shrinks toward 1. The exact
// number of allocations slices.Clone and slices.SortFunc perform is a
// runtime detail and is not asserted here, only that requesting the single
// top record allocates no more than requesting all of them.
//
// This test deliberately does not call t.Parallel: testing.AllocsPerRun
// panics when run from a parallel test, because a concurrent goroutine
// allocating in the background would corrupt its measurement.
func TestTopAllocatesOnceRegardlessOfN(t *testing.T) {
	records := sampleRecords()

	allocsForOne := testing.AllocsPerRun(100, func() {
		_ = Top(records, 1, byScoreDesc)
	})
	allocsForAll := testing.AllocsPerRun(100, func() {
		_ = Top(records, len(records), byScoreDesc)
	})
	if allocsForOne > allocsForAll {
		t.Fatalf("allocations: Top(n=1) = %v, Top(n=all) = %v; want n=1 to allocate no more", allocsForOne, allocsForAll)
	}
}

func TestTopIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	records := sampleRecords()

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			got := Top(records, n%len(records)+1, byScoreDesc)
			if len(got) != n%len(records)+1 {
				t.Errorf("goroutine %d: len(Top) = %d, want %d", n, len(got), n%len(records)+1)
			}
		}(i)
	}
	wg.Wait()
}

// ExampleTop is the runnable demonstration of this module: go test executes
// it and compares its stdout against the Output comment below.
func ExampleTop() {
	records := []Record{
		{Name: "alice", Score: 40},
		{Name: "bob", Score: 90},
		{Name: "carol", Score: 70},
	}

	top := Top(records, 2, func(a, b Record) bool { return a.Score > b.Score })
	for _, r := range top {
		fmt.Printf("%s: %d\n", r.Name, r.Score)
	}
	fmt.Println("caller's slice still starts with:", records[0].Name)

	// Output:
	// bob: 90
	// carol: 70
	// caller's slice still starts with: alice
}
```

## Review

`Top` is correct when it returns the right top-`n` ranking *and* leaves
`records` bit-for-bit and order-for-order as the caller left it --
`TestTopDoesNotReorderCallerSliceButTopNAliasedDoes` is the test that would
catch a regression to the in-place version, since the return value alone
cannot distinguish the two implementations. The mechanism is `slices.Clone`
before `slices.SortFunc`, not after: sorting has to happen on a slice the
caller never sees, because `slices.SortFunc` gives no way to sort a copy
without making the copy yourself first. Around that core, a non-positive `n`
or empty input returns an empty non-nil slice, an oversized `n` clamps to
`len(records)`, ties are resolved by whatever order `slices.SortFunc`
produces without the package asserting a specific one, and the returned
slice never aliases the caller's. `Top` holds no state and is therefore safe
to call from multiple goroutines at once. `ExampleTop` is the executable
documentation: `go test` verifies its output. Run
`go test -count=1 -race ./...`.

## Resources

- [`slices.SortFunc`](https://pkg.go.dev/slices#SortFunc) — documents the in-place sort this module clones ahead of.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the copy that isolates the sort from the caller's own slice.
- [`slices.Equal`](https://pkg.go.dev/slices#Equal) — used in the tests to confirm the caller's slice order is exactly preserved.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — the allocation probe, and its restriction against parallel tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-wal-compaction-scanner-token-in-map.md](18-wal-compaction-scanner-token-in-map.md) | Next: [../07-slice-internals/00-concepts.md](../07-slice-internals/00-concepts.md)
