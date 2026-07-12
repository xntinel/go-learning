# Exercise 10: Frequency Counting and Top-N: aggregate with a map, rank with a sorted slice

Counting events and reporting the Top-N is the canonical map-to-sorted-slice
pattern: the map is the O(1) accumulator, the slice is the ordered view. This
exercise counts HTTP status codes into `map[int]int`, then ranks them with
`slices.SortFunc`, breaking count ties deterministically by code with `cmp.Or` so
the report is reproducible in tests and dashboards.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
statusrank/                independent module: example.com/statusrank
  go.mod
  rank.go                  Count; Frequencies (map[int]int), TopN (sorted slice, cmp.Or tie-break)
  cmd/
    demo/
      main.go              runnable demo: count a status stream, print the Top-3
  rank_test.go             frequencies, Top-N, deterministic ties, N>distinct, empty input
```

- Files: `rank.go`, `cmd/demo/main.go`, `rank_test.go`.
- Implement: `Frequencies(codes) map[int]int` and `TopN(freq, n) []Count`, ranked by count descending with ties broken by code ascending.
- Test: counting a fixed stream yields expected frequencies, `TopN` returns the N highest, ties are broken deterministically (run twice, identical), N larger than the distinct count returns all, and empty input returns empty.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/10-frequency-top-n-aggregation/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/10-frequency-top-n-aggregation
```

### Why the map accumulates and the slice ranks

The map is the natural accumulator: `freq[code]++` is O(1), and the zero-value
read makes it setup-free — an unseen code reads as `0`, increments to `1`, and is
stored. But a map cannot be *ranked*, because its iteration order is randomized;
any Top-N you tried to read straight from the map would differ run to run. So the
pattern is two-phase: accumulate into the map, then materialize a slice of
`Count{Code, N}` pairs and sort it.

The sort is where determinism is won or lost. Ranking by count alone is not
enough: two codes with the same count could appear in either order, so a golden
test or a dashboard would flap. The fix is a total order — sort by count
descending, and break ties by code ascending — expressed cleanly with `cmp.Or`,
which returns the first non-zero comparison:

```go
slices.SortFunc(pairs, func(a, b Count) int {
	return cmp.Or(
		cmp.Compare(b.N, a.N),       // count descending
		cmp.Compare(a.Code, b.Code), // tie-break: code ascending
	)
})
```

Note the argument order in `cmp.Compare(b.N, a.N)` — swapping `a` and `b` is how
you get descending order without negating. `TopN` then clamps `n` to the slice
length (so asking for more than exist returns all) and slices off the top.

Create `rank.go`:

```go
package statusrank

import (
	"cmp"
	"slices"
)

// Count is one ranked entry: a status code and how often it occurred.
type Count struct {
	Code int
	N    int
}

// Frequencies counts each code in the stream. An unseen code starts at zero, so
// no per-key initialization is needed.
func Frequencies(codes []int) map[int]int {
	freq := make(map[int]int)
	for _, c := range codes {
		freq[c]++
	}
	return freq
}

// TopN returns the n most frequent codes, ranked by count descending and, on a
// tie, by code ascending, so the order is deterministic. If n exceeds the number
// of distinct codes, all are returned.
func TopN(freq map[int]int, n int) []Count {
	pairs := make([]Count, 0, len(freq))
	for code, c := range freq {
		pairs = append(pairs, Count{Code: code, N: c})
	}
	slices.SortFunc(pairs, func(a, b Count) int {
		return cmp.Or(
			cmp.Compare(b.N, a.N),       // count descending
			cmp.Compare(a.Code, b.Code), // tie-break: code ascending
		)
	})
	if n > len(pairs) {
		n = len(pairs)
	}
	return pairs[:n]
}
```

### The runnable demo

The demo counts a stream of status codes and prints the Top-3, whose ordering is
stable across runs because of the total-order sort.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/statusrank"
)

func main() {
	codes := []int{200, 200, 200, 404, 404, 500, 200, 404, 301}
	freq := statusrank.Frequencies(codes)

	fmt.Println("top 3 status codes:")
	for _, c := range statusrank.TopN(freq, 3) {
		fmt.Printf("  %d: %d\n", c.Code, c.N)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
top 3 status codes:
  200: 4
  404: 3
  301: 1
```

`200` (4) and `404` (3) rank by count. The third slot is a tie between `301` and
`500`, both seen once; the tie-break by code ascending puts `301` ahead of `500`,
so the Top-3 is stable every run.

### Tests

`TestFrequencies` pins the counts for a fixed stream. `TestTopN` checks the ranked
order. `TestTieBreakDeterministic` builds the frequencies and asserts two `TopN`
calls return identical slices, then checks the exact tie order. `TestNLargerThanDistinct`
proves asking for more than exist returns all. `TestEmpty` proves an empty stream
ranks to an empty slice.

Create `rank_test.go`:

```go
package statusrank

import (
	"fmt"
	"slices"
	"testing"
)

func TestFrequencies(t *testing.T) {
	t.Parallel()

	freq := Frequencies([]int{200, 200, 404, 200, 500})
	want := map[int]int{200: 3, 404: 1, 500: 1}
	if len(freq) != len(want) {
		t.Fatalf("len = %d, want %d", len(freq), len(want))
	}
	for code, n := range want {
		if freq[code] != n {
			t.Fatalf("freq[%d] = %d, want %d", code, freq[code], n)
		}
	}
}

func TestTopN(t *testing.T) {
	t.Parallel()

	freq := map[int]int{200: 4, 404: 3, 500: 2, 301: 1}
	got := TopN(freq, 2)
	want := []Count{{200, 4}, {404, 3}}
	if !slices.Equal(got, want) {
		t.Fatalf("TopN(2) = %v, want %v", got, want)
	}
}

func TestTieBreakDeterministic(t *testing.T) {
	t.Parallel()

	freq := map[int]int{500: 1, 301: 1, 404: 1, 200: 1}
	first := TopN(freq, 4)
	second := TopN(freq, 4)
	if !slices.Equal(first, second) {
		t.Fatalf("TopN not deterministic: %v vs %v", first, second)
	}
	want := []Count{{200, 1}, {301, 1}, {404, 1}, {500, 1}} // all tied: code ascending
	if !slices.Equal(first, want) {
		t.Fatalf("tie order = %v, want %v", first, want)
	}
}

func TestNLargerThanDistinct(t *testing.T) {
	t.Parallel()

	freq := map[int]int{200: 2, 404: 1}
	got := TopN(freq, 10)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (all distinct codes)", len(got))
	}
}

func TestEmpty(t *testing.T) {
	t.Parallel()

	if got := TopN(Frequencies(nil), 3); len(got) != 0 {
		t.Fatalf("TopN of empty = %v, want empty", got)
	}
}

func ExampleTopN() {
	freq := Frequencies([]int{200, 200, 404, 500, 404})
	fmt.Println(TopN(freq, 2))
	// Output: [{200 2} {404 2}]
}
```

## Review

The pattern is correct when the map does the counting and the slice does the
ranking: `Frequencies` accumulates in O(1) with zero-value increments, and `TopN`
materializes a slice and imposes a total order. The determinism hinges entirely on
the tie-break — `TestTieBreakDeterministic` would flap if `TopN` sorted by count
alone, because the map's iteration order would leak into equal-count runs. `cmp.Or`
composes the two comparisons cleanly, and swapping the arguments in
`cmp.Compare(b.N, a.N)` gives descending count without negation. Clamping `n` to the
slice length is what makes `TopN(freq, 10)` on four codes return four rather than
panic. Never rank straight from the map; the accumulator and the ordered view are
two different structures.

## Resources

- [slices.SortFunc](https://pkg.go.dev/slices#SortFunc) — sorting the materialized pairs.
- [cmp.Or](https://pkg.go.dev/cmp#Or) and [cmp.Compare](https://pkg.go.dev/cmp#Compare) — composing the count-then-code total order.
- [Go blog: Go maps in action](https://go.dev/blog/maps) — the map-accumulate, slice-rank idiom.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-map-value-addressability-pointers.md](09-map-value-addressability-pointers.md) | Next: [11-comma-ok-vs-zero-value-flags.md](11-comma-ok-vs-zero-value-flags.md)
