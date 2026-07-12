# Exercise 13: Top-N Leaderboard Selection from a Map of Scores

**Nivel: Intermedio** — validacion rapida (un test corto).

A game or a rate-limited API often keeps live scores in a `map[player]score`
because updates are by-key, but showing a leaderboard means turning that map
into an ordered top N — and doing it the same way every time two players tie.
This module ranges the map into a slice, breaks ties deterministically, and
truncates to N, so the leaderboard never depends on map iteration order.

## What you'll build

```text
leaderboard/                independent module: example.com/leaderboard-topn
  go.mod                     go 1.24
  leaderboard.go              type Entry; TopN(scores map[string]int, n int) []Entry
  leaderboard_test.go         table test: tie-break + n larger than map + n <= 0
```

- Files: `leaderboard.go`, `leaderboard_test.go`.
- Implement: `TopN(scores map[string]int, n int) []Entry` ranging the map into
  a slice, sorting by score descending with player name ascending as
  tiebreaker, then truncating to `n`.
- Test: one table covering a three-way tie, `n` larger than the map, and
  `n <= 0`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/13-leaderboard-top-n
cd go-solutions/03-control-flow/05-range-over-collections/13-leaderboard-top-n
go mod edit -go=1.24
```

### Why the tiebreaker matters more than the sort itself

Ranging `scores` into a slice is the easy half; picking the right comparator
is where a leaderboard actually gets tested. Sorting purely by score
descending is not a total order — when two players share a score, `sort.Slice`
is not guaranteed stable across runs relative to map-range order, so which one
lands at position `n` (and therefore whether it makes the cut at all) would
silently depend on the map's randomized iteration. Breaking ties by player name
ascending makes the comparator a total order: for any two distinct entries,
one comparison result is always the same regardless of what order they arrived
in. That is what makes `TopN(scores, 2)` return the *same* two players every
time it is called on an unchanged map, tie or no tie.

Create `leaderboard.go`:

```go
package leaderboard

import "sort"

// Entry is one player's score on the leaderboard.
type Entry struct {
	Player string
	Score  int
}

// TopN ranges scores into a slice, orders it by score descending (player name
// ascending breaks ties), and returns at most n entries. A non-positive n
// returns an empty, non-nil slice; an n larger than the map returns every
// entry. The map range itself never determines the result order — only the
// sort does.
func TopN(scores map[string]int, n int) []Entry {
	out := make([]Entry, 0, len(scores))
	if n <= 0 {
		return out
	}

	for player, score := range scores {
		out = append(out, Entry{Player: player, Score: score})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Player < out[j].Player
	})

	if n < len(out) {
		out = out[:n]
	}
	return out
}
```

### Test

The table drives one shared `scores` map through three cases: a top-2 request
that must resolve a three-way tie by name, an `n` larger than the map (every
entry, still sorted), and `n = 0` (empty, non-nil result).

Create `leaderboard_test.go`:

```go
package leaderboard

import (
	"reflect"
	"testing"
)

func TestTopN(t *testing.T) {
	t.Parallel()

	scores := map[string]int{
		"nova": 340,
		"rex":  500,
		"echo": 500,
		"vega": 120,
		"kilo": 500,
	}

	tests := []struct {
		name string
		n    int
		want []Entry
	}{
		{
			name: "top 2 breaks ties by player name ascending",
			n:    2,
			want: []Entry{
				{Player: "echo", Score: 500},
				{Player: "kilo", Score: 500},
			},
		},
		{
			name: "n larger than map returns every entry sorted",
			n:    10,
			want: []Entry{
				{Player: "echo", Score: 500},
				{Player: "kilo", Score: 500},
				{Player: "rex", Score: 500},
				{Player: "nova", Score: 340},
				{Player: "vega", Score: 120},
			},
		},
		{
			name: "non-positive n returns empty slice",
			n:    0,
			want: []Entry{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TopN(scores, tc.n)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("TopN(scores, %d) = %+v, want %+v", tc.n, got, tc.want)
			}
		})
	}
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

`TopN` is correct when the same input map produces the same output slice on
every call, tie or no tie — which only holds because the comparator is a total
order over `(Score, Player)`, not `Score` alone. The map range itself is
allowed to be as random as Go wants; it only ever seeds the pre-sort slice.
Truncating after the sort, not before, is what keeps a large `n` safe and a
non-positive `n` a clean empty result instead of a slice-bounds panic.

## Resources

- [Go Specification: For statements (range over map)](https://go.dev/ref/spec#For_range) — map iteration order is unspecified.
- [sort.Slice](https://pkg.go.dev/sort#Slice)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-header-canonicalization.md](12-header-canonicalization.md) | Next: [14-csv-row-validation.md](14-csv-row-validation.md)
