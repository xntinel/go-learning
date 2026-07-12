# Exercise 2: Slice Iterator Pipelines

A slice does not need to be copied and re-copied at every transformation step.
This exercise builds a `pipeline` package that turns a slice into an iterator
with `slices.Values`, threads it through custom `Filter` and `Map` adapters, and
materializes the result exactly once with `slices.Collect` — a lazy pipeline that
allocates only at its end. It also uses `slices.Backward`, `slices.Chunk`, and
`slices.SortedFunc` for the reverse, batch, and multi-key-sort cases.

This module is fully self-contained. It begins with its own `go mod init`,
defines every function it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
pipeline.go          Map, Filter adapters; SquaresOfEvens, Reversed, Batches, PeopleByAge
cmd/
  demo/
    main.go          run each pipeline and print the materialized result
pipeline_test.go     pipeline output, reverse, chunk cloning, multi-key sort
```

- Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
- Implement: the `Map` and `Filter` iterator adapters, `SquaresOfEvens`, `Reversed`, `Batches`, `PeopleByAge`, and the `Person` type.
- Test: `pipeline_test.go` checks the fused pipeline output, reversal, batch cloning, and a stable two-key sort.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/08-standard-library-iterators/02-slice-iterator-pipelines/cmd/demo && cd go-solutions/25-iterators-and-modern-go/08-standard-library-iterators/02-slice-iterator-pipelines
```

### Why a lazy pipeline allocates only once

`slices.Values(nums)` does not copy `nums`; it returns an `iter.Seq[int]` that,
when ranged, yields each element in turn. Stacking adapters on top of it does not
build an intermediate slice between stages. `Filter` wraps the values producer
and forwards only the evens; `Map` wraps that and squares each forwarded value;
`slices.Collect` wraps that and is the only stage that allocates, growing one
result slice as it drains the sequence. The three stages fuse into a single
pass: `Collect` pulls one element, which pulls it through `Map`, which pulls it
through `Filter`, which pulls it from `Values`; then the next element. No
element-2 work happens until element-1 has flowed all the way to the result.
That is the payoff of laziness — `SquaresOfEvens` reads as three independent
transformations but executes as one loop with one allocation.

`Map` and `Filter` are both `iter.Seq` adapters, and both carry the `!yield(...)`
early-stop guard for the same reason as in the previous exercise: a downstream
consumer that breaks must be able to halt the producer. `slices.Collect` drains
fully, so the guard is dormant here, but writing it keeps the adapters reusable
in front of a consumer that stops early.

`Reversed` uses `slices.Backward`, whose iterator is an `iter.Seq2[int, E]` —
it yields the index alongside the element, from the last position to the first.
The index is discarded with `_` here, but it is there when you need it.

`Batches` regroups a slice into chunks of at most `size` using `slices.Chunk`.
The detail that matters, and the one this exercise tests, is that each chunk
`slices.Chunk` yields is a window into the input's backing array, not a fresh
copy. Storing those windows in a longer-lived `[][]E` and then mutating or
reusing the input would corrupt the stored batches. `Batches` therefore calls
`slices.Clone` on each chunk before retaining it, which is the correct discipline
whenever a chunk outlives the loop iteration that produced it.

`PeopleByAge` sorts with `slices.SortedFunc`, which collects the
`slices.Values(people)` sequence and orders it by a comparison function. The
comparison sorts by age, then breaks ties by name, using `cmp.Compare` for each
field — the standard way to express a multi-key order without writing
three-way-branch comparison logic by hand.

Create `pipeline.go`:

```go
package pipeline

import (
	"cmp"
	"iter"
	"slices"
)

// Person is a record sorted by PeopleByAge.
type Person struct {
	Name string
	Age  int
}

// Map returns an iterator that applies f to each element of seq, lazily.
func Map[A, B any](seq iter.Seq[A], f func(A) B) iter.Seq[B] {
	return func(yield func(B) bool) {
		for v := range seq {
			if !yield(f(v)) {
				return
			}
		}
	}
}

// Filter returns an iterator that yields only the elements of seq for which
// keep reports true.
func Filter[V any](seq iter.Seq[V], keep func(V) bool) iter.Seq[V] {
	return func(yield func(V) bool) {
		for v := range seq {
			if keep(v) && !yield(v) {
				return
			}
		}
	}
}

// SquaresOfEvens builds a lazy Values -> Filter -> Map pipeline over nums and
// materializes it once with slices.Collect.
func SquaresOfEvens(nums []int) []int {
	evens := Filter(slices.Values(nums), func(n int) bool { return n%2 == 0 })
	squares := Map(evens, func(n int) int { return n * n })
	return slices.Collect(squares)
}

// Reversed returns the elements of s in reverse order using slices.Backward.
func Reversed[E any](s []E) []E {
	out := make([]E, 0, len(s))
	for _, v := range slices.Backward(s) {
		out = append(out, v)
	}
	return out
}

// Batches splits s into chunks of at most size. Each chunk is cloned because
// slices.Chunk yields windows into the input's backing array.
func Batches[E any](s []E, size int) [][]E {
	var out [][]E
	for chunk := range slices.Chunk(s, size) {
		out = append(out, slices.Clone(chunk))
	}
	return out
}

// PeopleByAge sorts people by age, breaking ties by name, via slices.SortedFunc.
func PeopleByAge(people []Person) []Person {
	return slices.SortedFunc(slices.Values(people), func(a, b Person) int {
		if n := cmp.Compare(a.Age, b.Age); n != 0 {
			return n
		}
		return cmp.Compare(a.Name, b.Name)
	})
}
```

### The runnable demo

The demo runs each pipeline once and prints the materialized result, so the
lazy machinery upstream is invisible and only the final slices show.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/slice-iterator-pipelines"
)

func main() {
	fmt.Println("squares of evens:", pipeline.SquaresOfEvens([]int{1, 2, 3, 4, 5, 6}))
	fmt.Println("reversed:", pipeline.Reversed([]int{1, 2, 3}))
	fmt.Println("batches:", pipeline.Batches([]string{"a", "b", "c", "d", "e"}, 2))

	people := []pipeline.Person{{Name: "Charlie", Age: 35}, {Name: "Bob", Age: 25}, {Name: "Alice", Age: 25}}
	fmt.Println("by age:", pipeline.PeopleByAge(people))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
squares of evens: [4 16 36]
reversed: [3 2 1]
batches: [[a b] [c d] [e]]
by age: [{Alice 25} {Bob 25} {Charlie 35}]
```

### Tests

The tests pin each transformation. `TestSquaresOfEvens` checks the fused
pipeline against a known result and includes an empty input. `TestReversed`
checks `slices.Backward`. `TestBatches` checks both the grouping and, crucially,
that mutating the source after batching does not disturb the stored batches —
the property `slices.Clone` provides. `TestPeopleByAge` checks the two-key sort,
including a tie on age resolved by name.

Create `pipeline_test.go`:

```go
package pipeline

import (
	"reflect"
	"testing"
)

func TestSquaresOfEvens(t *testing.T) {
	t.Parallel()

	if got, want := SquaresOfEvens([]int{1, 2, 3, 4, 5, 6}), []int{4, 16, 36}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SquaresOfEvens = %v, want %v", got, want)
	}
	if got := SquaresOfEvens(nil); len(got) != 0 {
		t.Fatalf("SquaresOfEvens(nil) = %v, want empty", got)
	}
}

func TestReversed(t *testing.T) {
	t.Parallel()

	if got, want := Reversed([]int{1, 2, 3}), []int{3, 2, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Reversed = %v, want %v", got, want)
	}
}

func TestBatches(t *testing.T) {
	t.Parallel()

	src := []string{"a", "b", "c", "d", "e"}
	got := Batches(src, 2)
	want := [][]string{{"a", "b"}, {"c", "d"}, {"e"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Batches = %v, want %v", got, want)
	}

	// Mutating the source must not disturb already-collected batches.
	src[0] = "X"
	if got[0][0] != "a" {
		t.Fatalf("batch aliased source: got[0][0] = %q, want %q", got[0][0], "a")
	}
}

func TestPeopleByAge(t *testing.T) {
	t.Parallel()

	people := []Person{{Name: "Charlie", Age: 35}, {Name: "Bob", Age: 25}, {Name: "Alice", Age: 25}}
	got := PeopleByAge(people)
	want := []Person{{Name: "Alice", Age: 25}, {Name: "Bob", Age: 25}, {Name: "Charlie", Age: 35}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PeopleByAge = %v, want %v", got, want)
	}
}
```

## Review

The pipeline is sound when the only allocation is at the consumer. `Filter` and
`Map` return iterators and copy nothing; `slices.Collect` is where the result
slice is born, so `SquaresOfEvens` runs as one fused pass over the input. Confirm
the empty-input case yields an empty result rather than panicking, since a nil
slice produces a sequence that yields nothing and `Collect` returns an empty
slice. The `slices.SortedFunc` comparison must be a proper three-way function —
negative, zero, positive — and chaining `cmp.Compare` per field, returning early
on the first non-zero, is the idiom that gets the multi-key order right.

The mistake this exercise targets most directly is aliasing the windows that
`slices.Chunk` yields. A chunk is a view into the input's backing array; storing
it and later mutating or reusing the input silently rewrites the stored batch.
`TestBatches` proves the fix by mutating `src[0]` after collecting and asserting
the first batch is untouched, which only holds because `Batches` clones each
chunk. The second mistake is assuming an adapter does work eagerly: nothing in a
`Values -> Filter -> Map` chain runs until a consumer drains it, so a missing
`slices.Collect` (or range loop) means the transformations never execute at all.

## Resources

- [`slices` package](https://pkg.go.dev/slices) — `Values`, `Collect`, `Backward`, `Chunk`, and `SortedFunc`, every standard function this exercise uses.
- [`cmp` package](https://pkg.go.dev/cmp) — `Compare`, the three-way comparison used to build a multi-key sort order.
- [Go 1.23 Release Notes](https://go.dev/doc/go1.23) — the release that introduced range-over-function iterators and the first `slices`/`maps` iterator functions.
- [Go Blog: Range Over Function Types](https://go.dev/blog/range-functions) — how producers, adapters, and consumers compose into a single-pass pipeline.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-deterministic-map-iteration.md](01-deterministic-map-iteration.md) | Next: [03-text-iterators.md](03-text-iterators.md)
