# Exercise 1: Streaming Pipeline — Composable `iter.Seq` Combinators with Cooperative Stop

This is the foundation module. You build a small `pipe` package of stream
combinators — `Range`, `Map`, `Filter`, `Take`, `Collect`, `Reduce` — typed as
the standard `iter.Seq[T]` aliases so they interoperate with `slices.Collect` and
`slices.Values`, and you prove the load-bearing property: when the consumer stops,
the producer stops. Every later module in this lesson reuses this stop contract.

## What you'll build

```text
pipe/                     independent module: example.com/pipe
  go.mod                  module example.com/pipe
  pipe.go                 Range, Map, Filter, Take, Collect, Reduce as iter.Seq combinators
  cmd/
    demo/
      main.go             runnable demo: Range -> Map -> Filter -> Take -> Collect
  pipe_test.go            equality tests + the cooperative-stop counter tests, -race
```

Files: `pipe.go`, `cmd/demo/main.go`, `pipe_test.go`.
Implement: `Range(n) iter.Seq[int]`, generic `Map`, `Filter`, `Take`, `Collect`, `Reduce`, each respecting `yield`'s `bool`.
Test: equality of `Range`/`Map`/`Filter`/`Take`/`Reduce`/compose, plus two counter tests proving early termination stops the source.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/09-range-over-integers-and-functions/01-streaming-pipeline-combinators/cmd/demo
cd go-solutions/03-control-flow/09-range-over-integers-and-functions/01-streaming-pipeline-combinators
```

## The design

Each combinator is a function that returns an `iter.Seq[T]`. Because `iter.Seq[V]`
is just `func(yield func(V) bool)`, a combinator is a closure that either drives a
loop (`Range`) or wraps a source iterator (`Map`, `Filter`, `Take`). The wrapping
is the whole trick: `Map` runs the source with a yield that applies `f` and
forwards the source's stop signal; `Filter` forwards a value only when the
predicate holds and returns `true` (ask for more) when it drops one; `Take` counts
down and returns `false` to stop the source once it has passed `n` values through.

The critical detail is that every wrapper must propagate the downstream `false`.
When the outer `yield` returns `false`, `Map`'s inner yield returns that `false`
to the source, the source's `if !yield(...) { return }` fires, and the whole chain
unwinds. That is what makes `Take(3, Filter(even, Map(square, Range(10))))` stop
`Range` after just a handful of integers instead of counting to ten and beyond.

Typing the signatures as `iter.Seq[T]` (rather than the bare `func(yield ...)`)
buys interoperation: `Range(5)` can be passed straight to `slices.Collect`, and a
slice can be turned into a source with `slices.Values`. `Collect` here is a thin
wrapper over `slices.Collect` to make that explicit.

Create `pipe.go`:

```go
package pipe

import (
	"iter"
	"slices"
)

// Range yields 0,1,...,n-1 and stops early if the consumer does.
func Range(n int) iter.Seq[int] {
	return func(yield func(int) bool) {
		for i := range n {
			if !yield(i) {
				return
			}
		}
	}
}

// Map applies f to every value of src, preserving the stop signal.
func Map[T, U any](f func(T) U, src iter.Seq[T]) iter.Seq[U] {
	return func(yield func(U) bool) {
		src(func(v T) bool {
			return yield(f(v))
		})
	}
}

// Filter forwards only values that satisfy pred. Dropping a value returns true
// so the source keeps producing; a forwarded value propagates the stop signal.
func Filter[T any](pred func(T) bool, src iter.Seq[T]) iter.Seq[T] {
	return func(yield func(T) bool) {
		src(func(v T) bool {
			if !pred(v) {
				return true
			}
			return yield(v)
		})
	}
}

// Take yields at most n values, then stops the source by returning false.
func Take[T any](n int, src iter.Seq[T]) iter.Seq[T] {
	return func(yield func(T) bool) {
		remaining := n
		src(func(v T) bool {
			if remaining <= 0 {
				return false
			}
			remaining--
			return yield(v)
		})
	}
}

// Collect drains src into a slice, reusing the stdlib helper.
func Collect[T any](src iter.Seq[T]) []T {
	return slices.Collect(src)
}

// Reduce folds src into a single accumulator.
func Reduce[T, U any](initial U, f func(acc U, v T) U, src iter.Seq[T]) U {
	acc := initial
	src(func(v T) bool {
		acc = f(acc, v)
		return true
	})
	return acc
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pipe"
)

func main() {
	square := func(x int) int { return x * x }
	even := func(x int) bool { return x%2 == 0 }

	firstThreeEvenSquares := pipe.Take(3, pipe.Filter(even, pipe.Map(square, pipe.Range(10))))
	fmt.Println(pipe.Collect(firstThreeEvenSquares))

	sum := func(acc, v int) int { return acc + v }
	fmt.Println(pipe.Reduce(0, sum, pipe.Range(5)))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[0 4 16]
10
```

## Tests

`TestTakeStopsEarlyOnConsumerBreak` and `TestFilterRespectsEarlyStop` are the
load-bearing tests: each instruments a producer counter and asserts the source
produced only a small constant after the consumer broke, proving cooperative
termination rather than run-to-completion.

Create `pipe_test.go`:

```go
package pipe

import (
	"reflect"
	"testing"
)

func TestRangeProducesZeroToN(t *testing.T) {
	t.Parallel()

	got := Collect(Range(5))
	want := []int{0, 1, 2, 3, 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Collect(Range(5)) = %v, want %v", got, want)
	}
}

func TestMapAppliesFunction(t *testing.T) {
	t.Parallel()

	square := func(x int) int { return x * x }
	got := Collect(Map(square, Range(5)))
	want := []int{0, 1, 4, 9, 16}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Map(square, Range(5)) = %v, want %v", got, want)
	}
}

func TestFilterRetainsMatches(t *testing.T) {
	t.Parallel()

	even := func(x int) bool { return x%2 == 0 }
	got := Collect(Filter(even, Range(10)))
	want := []int{0, 2, 4, 6, 8}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Filter(even, Range(10)) = %v, want %v", got, want)
	}
}

func TestTakeStopsAfterN(t *testing.T) {
	t.Parallel()

	got := Collect(Take(3, Range(100)))
	want := []int{0, 1, 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Take(3, Range(100)) = %v, want %v", got, want)
	}
}

func TestPipelineComposes(t *testing.T) {
	t.Parallel()

	square := func(x int) int { return x * x }
	even := func(x int) bool { return x%2 == 0 }

	pipeline := Take(3, Filter(even, Map(square, Range(10))))
	got := Collect(pipeline)
	want := []int{0, 4, 16}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("composed = %v, want %v", got, want)
	}
}

func TestReduceFoldsValues(t *testing.T) {
	t.Parallel()

	sum := func(acc, v int) int { return acc + v }
	got := Reduce(0, sum, Range(5))
	if got != 10 {
		t.Fatalf("Reduce(0, sum, Range(5)) = %d, want 10", got)
	}
}

func TestTakeStopsEarlyOnConsumerBreak(t *testing.T) {
	t.Parallel()

	var produced int
	src := func(yield func(int) bool) {
		for i := range 1000 {
			produced++
			if !yield(i) {
				return
			}
		}
	}

	limited := Take(5, src)
	count := 0
	for v := range limited {
		if v >= 3 {
			break
		}
		count++
	}

	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}
	if produced > 5 {
		t.Fatalf("produced = %d, want <= 5 (cooperative stop)", produced)
	}
}

func TestFilterRespectsEarlyStop(t *testing.T) {
	t.Parallel()

	var produced int
	src := func(yield func(int) bool) {
		for i := range 1_000_000 {
			produced++
			if !yield(i) {
				return
			}
		}
	}

	even := func(x int) bool { return x%2 == 0 }
	for range Filter(even, src) {
		break
	}

	if produced > 2 {
		t.Fatalf("produced = %d, want <= 2 (source must stop on early break)", produced)
	}
}
```

## Review

The combinators are correct when every wrapper propagates `yield`'s `bool`
unchanged: `Map` and `Take` forward the downstream stop to the source, and
`Filter` returns `true` only for values it *drops* so the source keeps producing,
never masking a real stop. The proof is in the two counter tests — a source that
would produce a million values must produce a small constant once the consumer
breaks. If either counter climbs, some wrapper is swallowing the `false`. Typing
everything as `iter.Seq[T]` is what lets `Collect` delegate to `slices.Collect`
and lets any of these sources feed the stdlib `slices`/`maps` helpers unchanged.
Run `go test -race` to confirm no combinator introduced shared mutable state.

## Resources

- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions)
- [`iter` package documentation](https://pkg.go.dev/iter)
- [`slices` package (Values, Collect)](https://pkg.go.dev/slices)
- [Go 1.23 release notes](https://go.dev/doc/go1.23)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-repository-seq-scan.md](02-repository-seq-scan.md)
