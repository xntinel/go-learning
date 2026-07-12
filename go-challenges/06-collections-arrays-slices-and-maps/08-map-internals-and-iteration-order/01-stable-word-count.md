# Exercise 1: Deterministic Term-Frequency Counter with Stable Top-N Output

A `/report` or `/leaderboard` handler that counts terms and emits the top results is
the canonical place a map's randomized iteration order leaks into user-visible output.
This module builds the counter the right way: aggregate in a `map[string]int`, then
emit through a *sorted* slice so the same input always produces byte-identical output.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
wordcount/                 independent module: example.com/wordcount
  go.mod                   go 1.26
  wordcount.go             Count, Sorted, TopN, tokenize; type Pair
  cmd/
    demo/
      main.go              counts a paragraph, prints the top 3 terms
  wordcount_test.go        table tests + determinism + TopN truncation + Example
```

- Files: `wordcount.go`, `cmd/demo/main.go`, `wordcount_test.go`.
- Implement: `Count(text) map[string]int`, `Sorted(text) []Pair` (descending count,
  alphabetical tie-break), `TopN(text, n) []Pair`, and an internal `tokenize`.
- Test: preserve the five table tests; add `TestSortedIsDeterministic` (two calls,
  identical) and `TestTopNTruncates`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/01-stable-word-count/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/01-stable-word-count
```

### Why the sorted slice is the contract, not the map

`Count` returns a `map[string]int` — the natural accumulator, O(1) per increment. But
a map is where determinism goes to die: `for word, n := range counts` visits terms in
a randomized order that changes every call. If a handler ranged that map straight into
the response body, two identical requests would return two different byte streams, and
any golden-file test would flap on the second run.

`Sorted` is the fix and the public contract. It copies the map into a `[]Pair`, then
sorts by descending count with an alphabetical tie-break, so ties (two terms with the
same count) resolve to one fixed order. `sort.Slice` takes a `less` function; the
comparison returns `pairs[i].Count > pairs[j].Count` for the primary key and falls
back to `pairs[i].Word < pairs[j].Word` when counts are equal. That tie-break is what
makes the output a *pure function* of the input — without it, two equal-count terms
could swap places between runs and break the determinism test.

`TopN` layers on top: sort, then slice to the first `n`. It clamps `n` to the length so
`TopN(text, 100)` on a 3-term text returns 3 pairs rather than panicking on a
slice-out-of-range. This is exactly the shape of a leaderboard endpoint.

`tokenize` splits on non-alphanumeric runes and lowercases, using a `strings.Builder`
to accumulate each token's runes. Note why `strings.Fields` would be wrong here:
`Fields` splits on whitespace only, so `"hello,world"` would come back as one token.
Splitting on `unicode.IsLetter`/`IsDigit` boundaries treats punctuation as a separator.

Create `wordcount.go`:

```go
package wordcount

import (
	"sort"
	"strings"
	"unicode"
)

// Pair is a term and its frequency.
type Pair struct {
	Word  string
	Count int
}

// Count tallies each token's frequency. The returned map has no defined
// iteration order; use Sorted or TopN for deterministic output.
func Count(text string) map[string]int {
	out := make(map[string]int)
	for _, word := range tokenize(text) {
		out[word]++
	}
	return out
}

// Sorted returns every term ordered by descending count, breaking ties
// alphabetically. The output is a pure function of the input.
func Sorted(text string) []Pair {
	counts := Count(text)
	pairs := make([]Pair, 0, len(counts))
	for word, count := range counts {
		pairs = append(pairs, Pair{Word: word, Count: count})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Count != pairs[j].Count {
			return pairs[i].Count > pairs[j].Count
		}
		return pairs[i].Word < pairs[j].Word
	})
	return pairs
}

// TopN returns the n highest-frequency terms in Sorted order. If n exceeds the
// number of distinct terms, every term is returned; a negative n returns none.
func TopN(text string, n int) []Pair {
	pairs := Sorted(text)
	if n < 0 {
		n = 0
	}
	if n > len(pairs) {
		n = len(pairs)
	}
	return pairs[:n]
}

// tokenize splits text into lowercase alphanumeric tokens, treating every other
// rune as a separator. strings.Fields would be wrong: it splits on whitespace
// only, so "hello,world" would be a single token.
func tokenize(text string) []string {
	var out []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			out = append(out, current.String())
			current.Reset()
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/wordcount"
)

func main() {
	const text = "the cat sat on the mat the cat ran"
	for _, p := range wordcount.TopN(text, 3) {
		fmt.Printf("%-5s %d\n", p.Word, p.Count)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
the   3
cat   2
mat   1
```

(`mat` wins the third slot over `on`, `ran`, and `sat` by the alphabetical tie-break
among the count-1 terms.)

### Tests

The five original table tests are preserved verbatim. Two are added: 
`TestSortedIsDeterministic` calls `Sorted` twice and asserts the results are identical
with `reflect.DeepEqual` — this is the test that would catch any accidental reliance on
map order. `TestTopNTruncates` pins the clamp behavior. Running under `-race` surfaces
any hidden shared state.

Create `wordcount_test.go`:

```go
package wordcount

import (
	"fmt"
	"reflect"
	"testing"
)

func TestCountReturnsAccurateCounts(t *testing.T) {
	t.Parallel()

	counts := Count("the quick brown fox jumps over the lazy dog")
	want := map[string]int{
		"the":   2,
		"quick": 1,
		"brown": 1,
		"fox":   1,
		"jumps": 1,
		"over":  1,
		"lazy":  1,
		"dog":   1,
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("Count() = %v, want %v", counts, want)
	}
}

func TestCountIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	counts := Count("The THE the")
	if counts["the"] != 3 {
		t.Fatalf("Count(the) = %d, want 3", counts["the"])
	}
}

func TestCountIgnoresPunctuation(t *testing.T) {
	t.Parallel()

	counts := Count("hello, world! hello.")
	if counts["hello"] != 2 {
		t.Fatalf("Count(hello) = %d, want 2", counts["hello"])
	}
	if counts["world"] != 1 {
		t.Fatalf("Count(world) = %d, want 1", counts["world"])
	}
}

func TestSortedOrdersByDescendingCount(t *testing.T) {
	t.Parallel()

	pairs := Sorted("a b c a b a")
	if len(pairs) != 3 {
		t.Fatalf("len = %d, want 3", len(pairs))
	}
	if pairs[0].Word != "a" || pairs[0].Count != 3 {
		t.Fatalf("pairs[0] = %+v, want {a 3}", pairs[0])
	}
	if pairs[1].Word != "b" || pairs[1].Count != 2 {
		t.Fatalf("pairs[1] = %+v, want {b 2}", pairs[1])
	}
	if pairs[2].Word != "c" || pairs[2].Count != 1 {
		t.Fatalf("pairs[2] = %+v, want {c 1}", pairs[2])
	}
}

func TestSortedBreaksTiesAlphabetically(t *testing.T) {
	t.Parallel()

	pairs := Sorted("a b c")
	want := []Pair{{"a", 1}, {"b", 1}, {"c", 1}}
	if !reflect.DeepEqual(pairs, want) {
		t.Fatalf("Sorted() = %v, want %v", pairs, want)
	}
}

func TestSortedIsDeterministic(t *testing.T) {
	t.Parallel()

	const text = "alpha beta beta gamma gamma gamma delta delta"
	first := Sorted(text)
	second := Sorted(text)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Sorted not deterministic:\n first  = %v\n second = %v", first, second)
	}
}

func TestTopNTruncates(t *testing.T) {
	t.Parallel()

	const text = "a b c a b a"
	tests := []struct {
		n    int
		want []Pair
	}{
		{n: 0, want: []Pair{}},
		{n: -3, want: []Pair{}},
		{n: 2, want: []Pair{{"a", 3}, {"b", 2}}},
		{n: 99, want: []Pair{{"a", 3}, {"b", 2}, {"c", 1}}},
	}
	for _, tc := range tests {
		got := TopN(text, tc.n)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("TopN(n=%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}

func ExampleTopN() {
	for _, p := range TopN("go go go rust rust c", 2) {
		fmt.Println(p.Word, p.Count)
	}
	// Output:
	// go 3
	// rust 2
}
```

Note `TestTopNTruncates` compares against `[]Pair{}` (length zero) for the `n<=0`
cases; `TopN` returns `pairs[:0]`, a non-nil empty slice, and `reflect.DeepEqual`
treats a non-nil empty slice as equal to `[]Pair{}` but not to `nil`, so the test
pins that `TopN` never returns `nil` here.

## Review

The counter is correct when its output is a pure function of the input text. `Count`
is order-free by design; the whole point is that `Sorted` and `TopN` — never a raw
range over `Count`'s map — are what a handler emits. The determinism test is the
guardrail: if someone later "optimizes" by ranging the map into the response,
`TestSortedIsDeterministic` still passes (it calls `Sorted`) but a golden test on the
handler would flap, which is exactly why the guardrail lives at the emit boundary. The
alphabetical tie-break is load-bearing: drop it and equal-count terms can swap between
runs. Run `go test -race` to confirm nothing shares mutable state across the parallel
subtests.

## Resources

- [Go Specification: Map types](https://go.dev/ref/spec#Map_types) — including that range order over a map is unspecified.
- [`sort.Slice`](https://pkg.go.dev/sort#Slice) — the `less`-function sort used here.
- [`strings.Builder`](https://pkg.go.dev/strings#Builder) — efficient token accumulation.
- [`unicode` package](https://pkg.go.dev/unicode) — `IsLetter`, `IsDigit`, `ToLower`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-metrics-label-aggregator.md](02-metrics-label-aggregator.md)
