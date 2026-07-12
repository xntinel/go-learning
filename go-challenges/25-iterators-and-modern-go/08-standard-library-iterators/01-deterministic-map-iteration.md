# Exercise 1: Deterministic Map Iteration

A map is the right data structure for a scoreboard, and the wrong one for
showing it: Go randomizes map iteration order on purpose. This exercise builds a
small `mapiter` package that turns a `map[string]int` of scores into ordered,
reproducible output using `maps.Keys`, `maps.Values`, and `slices.Sorted`, and
composes a custom `Filter` iterator with `maps.Keys` to answer a domain
question without writing a single traversal loop by hand.

This module is fully self-contained. It begins with its own `go mod init`,
defines every function it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
mapiter.go           SortedKeys, Filter, HighScorers, FormatScores, TotalScore
cmd/
  demo/
    main.go          score a roster, print high scorers and a sorted table
mapiter_test.go      determinism, filtering, totals, invalid-input rejection
```

- Files: `mapiter.go`, `cmd/demo/main.go`, `mapiter_test.go`.
- Implement: `SortedKeys`, the `Filter` iterator adapter, `HighScorers`, `FormatScores`, `TotalScore`, and the `ErrInvalidMinimum` sentinel.
- Test: `mapiter_test.go` asserts sorted output is stable across runs, filtering and totals are correct, and an out-of-range minimum is rejected.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/08-standard-library-iterators/01-deterministic-map-iteration/cmd/demo && cd go-solutions/25-iterators-and-modern-go/08-standard-library-iterators/01-deterministic-map-iteration
```

### Why sorting is the consumer that makes a map readable

The shape of every function here is the same: a map producer (`maps.Keys` or
`maps.Values`) feeds a consumer that imposes order (`slices.Sorted`). That
pairing is the entire idiom for deterministic map output, and it is worth seeing
why no simpler version is correct. `maps.Keys(m)` returns an `iter.Seq[K]` whose
yield order follows the runtime's randomized map walk, so collecting it directly
would give a different slice on different runs. `slices.Sorted` collects that
sequence and sorts it ascending in one call, so `slices.Sorted(maps.Keys(m))` is
the canonical "give me the keys in a stable order" expression. `SortedKeys` is
just that one line behind a name, generic over any ordered key type.

`HighScorers` is where a custom iterator earns its place. The question — which
names scored at least some minimum, listed alphabetically — decomposes into a
predicate and an order. The order is `slices.Sorted`, already solved. The
predicate is the only genuinely domain-specific part, so it goes into a `Filter`
adapter: `Filter(maps.Keys(scores), keep)` wraps the key producer and forwards
only the names whose score clears the bar. `Filter` is a textbook
`iter.Seq[V]`-to-`iter.Seq[V]` adapter, and the detail that makes it correct is
the `!yield(v)` check: when the downstream consumer stops early, `yield` returns
`false`, and the adapter must `return` rather than keep pushing. Here the
consumer is `slices.Sorted`, which drains the whole sequence, so early
termination never fires; writing the check anyway is what makes `Filter` safe to
reuse in front of a consumer that does break early.

`FormatScores` shows the read-the-map-in-sorted-key-order pattern in full: it
sorts the keys once, then walks that ordered slice and looks each value back up,
producing `"name=score"` pairs in a fixed sequence. `TotalScore` uses
`maps.Values` to sum every value; order is irrelevant to a sum, so no sort is
needed, which is itself the lesson — impose order only when the output depends
on it.

Create `mapiter.go`:

```go
package mapiter

import (
	"cmp"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
)

// ErrInvalidMinimum is returned when a score threshold is outside [0, 100].
var ErrInvalidMinimum = errors.New("minimum must be between 0 and 100")

// SortedKeys returns the keys of m in ascending order. It is the canonical
// idiom for deterministic map iteration: a map producer feeding a sorting
// consumer.
func SortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	return slices.Sorted(maps.Keys(m))
}

// Filter returns an iterator that yields only the elements of seq for which
// keep reports true. It is an adapter: an iter.Seq[V] in, an iter.Seq[V] out.
func Filter[V any](seq iter.Seq[V], keep func(V) bool) iter.Seq[V] {
	return func(yield func(V) bool) {
		for v := range seq {
			if keep(v) && !yield(v) {
				return
			}
		}
	}
}

// HighScorers returns, in ascending name order, the names whose score is at
// least minimum. It composes a custom Filter over maps.Keys with slices.Sorted.
func HighScorers(scores map[string]int, minimum int) ([]string, error) {
	if minimum < 0 || minimum > 100 {
		return nil, fmt.Errorf("minimum %d: %w", minimum, ErrInvalidMinimum)
	}
	names := Filter(maps.Keys(scores), func(name string) bool {
		return scores[name] >= minimum
	})
	return slices.Sorted(names), nil
}

// FormatScores renders scores as "name=score" pairs in ascending name order.
func FormatScores(scores map[string]int) []string {
	pairs := make([]string, 0, len(scores))
	for _, key := range SortedKeys(scores) {
		pairs = append(pairs, fmt.Sprintf("%s=%d", key, scores[key]))
	}
	return pairs
}

// TotalScore sums every value via maps.Values. Order does not affect a sum, so
// no sorting is needed.
func TotalScore(scores map[string]int) int {
	total := 0
	for v := range maps.Values(scores) {
		total += v
	}
	return total
}
```

### The runnable demo

The demo scores a small roster and prints the three deterministic views: the
high scorers, the full sorted table, and the total. Run it twice and the output
is byte-for-byte identical, which is the whole point of pushing the map through
a sorting consumer.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/deterministic-map-iteration"
)

func main() {
	scores := map[string]int{"Alice": 95, "Bob": 80, "Charlie": 92, "Dave": 70}

	high, err := mapiter.HighScorers(scores, 90)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("high scorers (>=90):", high)
	fmt.Println("table:", mapiter.FormatScores(scores))
	fmt.Println("total:", mapiter.TotalScore(scores))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
high scorers (>=90): [Alice Charlie]
table: [Alice=95 Bob=80 Charlie=92 Dave=70]
total: 337
```

### Tests

The tests pin determinism and correctness. `TestSortedKeysIsStable` builds the
sorted keys many times and asserts every run matches, which is the property a
raw map walk would fail. `TestHighScorers` checks the filter-and-sort
composition, including the boundary where a perfect-score threshold admits only
exact matches. `TestFormatScores` and `TestTotalScore` check the table and the
sum, and `TestHighScorersRejectsInvalidMinimum` proves the sentinel error fires
for out-of-range thresholds.

Create `mapiter_test.go`:

```go
package mapiter

import (
	"errors"
	"reflect"
	"testing"
)

func TestSortedKeysIsStable(t *testing.T) {
	t.Parallel()

	scores := map[string]int{"Dave": 70, "Alice": 95, "Charlie": 92, "Bob": 80}
	want := []string{"Alice", "Bob", "Charlie", "Dave"}
	for i := 0; i < 100; i++ {
		if got := SortedKeys(scores); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: SortedKeys = %v, want %v", i, got, want)
		}
	}
}

func TestHighScorers(t *testing.T) {
	t.Parallel()

	scores := map[string]int{"Alice": 95, "Bob": 80, "Charlie": 92, "Dave": 70}

	got, err := HighScorers(scores, 90)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Alice", "Charlie"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("HighScorers(90) = %v, want %v", got, want)
	}

	perfect, err := HighScorers(map[string]int{"Alice": 100, "Bob": 99}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Alice"}; !reflect.DeepEqual(perfect, want) {
		t.Fatalf("HighScorers(100) = %v, want %v", perfect, want)
	}
}

func TestFormatScores(t *testing.T) {
	t.Parallel()

	scores := map[string]int{"Alice": 95, "Bob": 80, "Charlie": 92, "Dave": 70}
	want := []string{"Alice=95", "Bob=80", "Charlie=92", "Dave=70"}
	if got := FormatScores(scores); !reflect.DeepEqual(got, want) {
		t.Fatalf("FormatScores = %v, want %v", got, want)
	}
}

func TestTotalScore(t *testing.T) {
	t.Parallel()

	scores := map[string]int{"Alice": 95, "Bob": 80, "Charlie": 92, "Dave": 70}
	if got, want := TotalScore(scores), 337; got != want {
		t.Fatalf("TotalScore = %d, want %d", got, want)
	}
}

func TestHighScorersRejectsInvalidMinimum(t *testing.T) {
	t.Parallel()

	for _, minimum := range []int{-1, 101} {
		if _, err := HighScorers(map[string]int{"Alice": 95}, minimum); !errors.Is(err, ErrInvalidMinimum) {
			t.Fatalf("minimum %d error = %v, want ErrInvalidMinimum", minimum, err)
		}
	}
}
```

## Review

The package is correct when every map-derived output is funneled through a
sorting consumer. `SortedKeys` is `slices.Sorted(maps.Keys(m))` and nothing
more; `FormatScores` reuses it so the table order and the key order can never
drift apart; `TotalScore` deliberately skips sorting because a sum does not
depend on order. Confirm `TestSortedKeysIsStable` passes its hundred runs — that
is the assertion a raw `maps.Keys` walk would flake. The `Filter` adapter is the
one piece of custom iterator code, and its `!yield(v)` guard is what lets it sit
in front of a consumer that breaks early without leaking values past the stop.

The mistakes this exercise is built to prevent are three. Asserting on raw map
order is the headline one: a test that compares `maps.Keys(m)` directly to a
fixed slice passes intermittently and wastes an afternoon. Forgetting to sort
before formatting produces a scoreboard whose rows shuffle between runs. And
reaching for a manual `sort.Slice` over a hand-collected key slice restates in
five lines what `slices.Sorted(maps.Keys(m))` says in one, with more room to get
the comparison wrong.

## Resources

- [`maps` package](https://pkg.go.dev/maps) — `Keys`, `Values`, and `All`, the map producers used here.
- [`slices` package](https://pkg.go.dev/slices) — `Sorted` and `SortedFunc`, the consumers that impose a deterministic order.
- [`iter` package](https://pkg.go.dev/iter) — the `Seq` and `Seq2` types that `Filter` and every standard iterator share.
- [Go Blog: Range Over Function Types](https://go.dev/blog/range-functions) — how a range loop calls an iterator and what the `yield` return value means.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-slice-iterator-pipelines.md](02-slice-iterator-pipelines.md)
