# Exercise 8: A Ranked Leaderboard with Multi-Key Ordering and Insertion-Point Insert

A game or API leaderboard is sorted by score descending, ties broken by name
ascending, and it is updated incrementally: each score submission upserts one
player. Keeping a *non-trivially* ordered slice sorted on every write means a
compound comparator, a binary search for the insertion point, and an
`slices.Insert`. This exercise builds that, plus `Rank(name)` returning the
1-based position.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
leaderboard/                 independent module: example.com/leaderboard
  go.mod
  board.go                   type Entry, Board; Upsert, Rank, Top, Len; compound comparator
  cmd/
    demo/
      main.go                submit scores, show the ranked board
  board_test.go              ordering, tie-break, upsert-moves-rank, rank lookup, invariant
```

Files: `board.go`, `cmd/demo/main.go`, `board_test.go`.
Implement: `Board` with `Upsert(Entry)`, `Rank(name) (int, bool)`, `Top(n) []Entry`, `Len()`, ordered by score descending then name ascending.
Test: out-of-order inserts land in the right order, ties break by name, upsert moves a player's rank, `Rank` returns 1-based positions and a sentinel for absent players, and the invariant holds after every op.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/12-sorted-collections-binary-search/08-leaderboard-ranked-insert/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/12-sorted-collections-binary-search/08-leaderboard-ranked-insert
```

### A compound comparator with cmp.Or, and upsert as delete-then-insert

The order is two keys: score descending, then name ascending. `cmp.Or` composes
them — it returns its first non-zero argument, so it reads as a priority list of
tie-breakers:

```go
func cmpEntry(a, b Entry) int {
	return cmp.Or(
		cmp.Compare(b.Score, a.Score), // score DESC: note b,a are swapped
		cmp.Compare(a.Name, b.Name),   // name ASC
	)
}
```

The swap `cmp.Compare(b.Score, a.Score)` is how you get *descending*: a higher
score compares "less" so it sorts earlier. The second term only decides ties,
because `cmp.Or` has already returned by then if the scores differ. This
comparator is a strict total order — no two entries share both a score and a name,
since `Upsert` keeps names unique — which is the precondition a binary search over
the slice needs.

`Upsert(e)` is delete-then-insert. First it removes any existing entry for the
same player with `slices.IndexFunc` + `slices.Delete`, because a score change
moves the player and the old position would otherwise linger as a duplicate.
Then it finds the insertion point with `slices.BinarySearchFunc(entries, e,
cmpEntry)` and splices `e` in with `slices.Insert`. The binary search runs against
the *already-sorted* slice with the player removed, so the returned position is
exactly where `e` belongs under the compound order — no full re-sort per update,
just an O(log n) search and an O(n) shift.

`Rank(name)` finds the player's current index with `slices.IndexFunc` and returns
`index + 1` (ranks are 1-based) or `(0, false)` when the player is absent. The
lookup is by name, which is not the sort key, so it is a linear scan — acceptable
for a name query on a leaderboard whose hot path is the ranked read, not the
name-to-rank lookup.

Create `board.go`:

```go
package leaderboard

import (
	"cmp"
	"slices"
)

// Entry is a player's current score on the board.
type Entry struct {
	Name  string
	Score int
}

// Board keeps entries sorted by score descending, then name ascending.
type Board struct {
	entries []Entry
}

// New returns an empty board.
func New() *Board {
	return &Board{}
}

// cmpEntry orders by score DESC (swap the args), then name ASC.
func cmpEntry(a, b Entry) int {
	return cmp.Or(
		cmp.Compare(b.Score, a.Score),
		cmp.Compare(a.Name, b.Name),
	)
}

// Upsert inserts or updates a player's entry, keeping the board sorted. An
// existing entry for the same name is removed first, then the new one is
// inserted at its binary-searched position.
func (b *Board) Upsert(e Entry) {
	if i := slices.IndexFunc(b.entries, func(x Entry) bool { return x.Name == e.Name }); i >= 0 {
		b.entries = slices.Delete(b.entries, i, i+1)
	}
	pos, _ := slices.BinarySearchFunc(b.entries, e, cmpEntry)
	b.entries = slices.Insert(b.entries, pos, e)
}

// Rank returns the 1-based position of name, or (0, false) if absent.
func (b *Board) Rank(name string) (int, bool) {
	i := slices.IndexFunc(b.entries, func(x Entry) bool { return x.Name == name })
	if i < 0 {
		return 0, false
	}
	return i + 1, true
}

// Top returns a copy of the highest-ranked n entries (fewer if the board is
// smaller).
func (b *Board) Top(n int) []Entry {
	if n > len(b.entries) {
		n = len(b.entries)
	}
	return slices.Clone(b.entries[:n])
}

// Len reports the number of entries.
func (b *Board) Len() int { return len(b.entries) }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/leaderboard"
)

func main() {
	b := leaderboard.New()
	for _, e := range []leaderboard.Entry{
		{Name: "alice", Score: 100},
		{Name: "bob", Score: 100},
		{Name: "carol", Score: 200},
		{Name: "dave", Score: 50},
	} {
		b.Upsert(e)
	}

	fmt.Println("initial board:")
	for _, e := range b.Top(b.Len()) {
		rank, _ := b.Rank(e.Name)
		fmt.Printf("  #%d %-6s %d\n", rank, e.Name, e.Score)
	}

	b.Upsert(leaderboard.Entry{Name: "bob", Score: 250}) // bob surges
	rank, _ := b.Rank("bob")
	fmt.Printf("after bob scores 250: bob is #%d\n", rank)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial board:
  #1 carol  200
  #2 alice  100
  #3 bob    100
  #4 dave   50
after bob scores 250: bob is #1
```

### Tests

`TestOrdering` inserts out of order and asserts the final ranked order.
`TestTieBreak` gives two players the same score and asserts name-ascending order.
`TestUpsertMovesRank` updates a score and asserts the rank changes.
`TestRankAbsent` pins the sentinel. Every test asserts the invariant with
`slices.IsSortedFunc` so a mis-ordered insert is caught even if the specific
assertion misses it.

Create `board_test.go`:

```go
package leaderboard

import (
	"fmt"
	"slices"
	"testing"
)

func assertSorted(t *testing.T, b *Board) {
	t.Helper()
	if !slices.IsSortedFunc(b.entries, cmpEntry) {
		t.Fatalf("board not sorted: %v", b.entries)
	}
}

func names(b *Board) []string {
	out := make([]string, b.Len())
	for i, e := range b.entries {
		out[i] = e.Name
	}
	return out
}

func TestOrdering(t *testing.T) {
	t.Parallel()

	b := New()
	for _, e := range []Entry{
		{"alice", 100}, {"bob", 100}, {"carol", 200}, {"dave", 50},
	} {
		b.Upsert(e)
		assertSorted(t, b)
	}
	want := []string{"carol", "alice", "bob", "dave"}
	if !slices.Equal(names(b), want) {
		t.Fatalf("order = %v, want %v", names(b), want)
	}
}

func TestTieBreak(t *testing.T) {
	t.Parallel()

	b := New()
	for _, e := range []Entry{{"zoe", 100}, {"amy", 100}, {"mike", 100}} {
		b.Upsert(e)
	}
	want := []string{"amy", "mike", "zoe"} // equal scores: name ascending
	if !slices.Equal(names(b), want) {
		t.Fatalf("tie-break order = %v, want %v", names(b), want)
	}
}

func TestUpsertMovesRank(t *testing.T) {
	t.Parallel()

	b := New()
	for _, e := range []Entry{{"a", 100}, {"b", 90}, {"c", 80}} {
		b.Upsert(e)
	}
	if r, _ := b.Rank("c"); r != 3 {
		t.Fatalf("Rank(c) before = %d, want 3", r)
	}

	b.Upsert(Entry{"c", 300}) // c jumps to the top
	assertSorted(t, b)
	if r, _ := b.Rank("c"); r != 1 {
		t.Fatalf("Rank(c) after = %d, want 1", r)
	}
	if b.Len() != 3 {
		t.Fatalf("Len = %d, want 3 (upsert must not duplicate)", b.Len())
	}
}

func TestRankAbsent(t *testing.T) {
	t.Parallel()

	b := New()
	b.Upsert(Entry{"solo", 10})
	if _, ok := b.Rank("ghost"); ok {
		t.Fatal("Rank of absent player should return ok=false")
	}
	if r, ok := b.Rank("solo"); !ok || r != 1 {
		t.Fatalf("Rank(solo) = %d,%v; want 1,true", r, ok)
	}
}

func Example() {
	b := New()
	b.Upsert(Entry{"alice", 100})
	b.Upsert(Entry{"bob", 200})
	top := b.Top(1)
	fmt.Println(top[0].Name)
	// Output: bob
}
```

## Review

The board is correct when it stays sorted under any submission order, ties break
by name, and an upsert replaces rather than duplicates. The invariant assertion
after every op is the safety net: because `Upsert` deletes then binary-searches
the insertion point, any error in the compound comparator (a forgotten arg swap
for descending, or a missing tie-break term) would break the order and
`slices.IsSortedFunc` would fire. The one thing to keep straight is the descending
key: `cmp.Compare(b.Score, a.Score)` — swapping the arguments is how you reverse a
single key inside `cmp.Or`. Run `go test -race`.

## Resources

- [`cmp.Or`](https://pkg.go.dev/cmp#Or) — compose tie-breakers by returning the first non-zero comparison.
- [`slices.BinarySearchFunc`](https://pkg.go.dev/slices#BinarySearchFunc) — insertion-point search under a custom comparator.
- [`slices.Insert` / `slices.Delete`](https://pkg.go.dev/slices#Insert) — the shift-based splice and remove behind `Upsert`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-insertion-strategy-benchmark.md](09-insertion-strategy-benchmark.md)
