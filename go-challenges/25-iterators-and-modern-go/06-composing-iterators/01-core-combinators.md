# Exercise 1: Core Combinators

A combinator library is a handful of small, sharp functions that each take an `iter.Seq` and return an `iter.Seq`, so they nest into pipelines without any glue. This exercise builds that core: the sources `Range` and `FromSlice`, the intermediate combinators `Filter`, `Map`, `Take`, `Skip`, and `Chain`, and the terminal `Reduce`. The whole point is that they stay lazy — a five-element `Take` over a thousand-element `Range` pulls only five values — and the tests prove it by counting upstream pulls.

This module is fully self-contained. It begins with its own `go mod init`, defines every combinator it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
itertools.go         Range, FromSlice, Filter, Map, Take, Skip, Chain, Reduce
cmd/
  demo/
    main.go          build a Filter -> Map -> Take pipeline and reduce the result
itertools_test.go    pipeline output, Skip/Chain/Reduce, exact-pull laziness, validation
example_test.go      a runnable ExampleFilter with verified output
```

- Files: `itertools.go`, `cmd/demo/main.go`, `itertools_test.go`, `example_test.go`.
- Implement: `Range`, `FromSlice`, `Filter`, `Map`, `Take`, `Skip`, `Chain`, `Reduce`, with the sentinel errors `ErrInvalidRange` and `ErrNegativeLimit`.
- Test: a `Filter -> Map -> Take` pipeline yields the right values, `Take` pulls *exactly* `n` from upstream, `Take(seq, 0)` yields nothing, and the validating constructors return their sentinel errors.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p itertools/cmd/demo && cd itertools
go mod init example.com/itertools
```

### Sources, intermediates, and terminals

The library has three kinds of function and it is worth naming them before reading the code. A *source* manufactures a sequence from non-sequence input: `Range(start, end)` produces the integers in a half-closed span, `FromSlice(values)` replays a slice. Both return an `iter.Seq[V]` (and `Range` also returns an `error`, because an inverted range is a caller mistake worth rejecting up front rather than silently yielding nothing). An *intermediate* combinator takes a sequence and returns a transformed sequence: `Filter` drops values, `Map` rewrites them, `Take` and `Skip` slice the stream by position, `Chain` concatenates several streams. A *terminal* drives the pipeline and returns a plain value: `Reduce` folds the sequence into a single accumulator.

Every intermediate shares the same skeleton — `return func(yield func(V) bool) { for v := range seq { ... if !yield(...) { return } } }` — and the discipline that makes it lazy is the `if !yield(...) { return }` check. When a downstream consumer stops (it breaks, or a later `Take` has had enough), its `yield` returns `false`; the `return` ends this combinator's loop, which makes *its* upstream's `yield` return `false`, and the stop signal climbs the whole pipeline back to the source. Miss the check in one combinator and that link keeps draining its upstream after everyone downstream has left.

`Take` is the combinator where laziness is easiest to get subtly wrong, so it is written to pull *exactly* `n` values. A `for v := range seq` pulls the next value before the loop body runs, so checking a counter at the top of the body would pull one value too many — `n+1` to yield `n`. Instead `Take` yields first, then increments, then returns the moment the count reaches `n`, with an `n == 0` guard so it never enters the loop at all. That is why the laziness test can assert the source was pulled exactly `n` times, not `n+1`.

Create `itertools.go`:

```go
package itertools

import (
	"errors"
	"fmt"
	"iter"
)

// ErrInvalidRange is returned by Range when start > end.
var ErrInvalidRange = errors.New("start must be less than or equal to end")

// ErrNegativeLimit is returned by Take and Skip when the count is negative.
var ErrNegativeLimit = errors.New("limit must not be negative")

// Range yields the integers start, start+1, ..., end-1 (a half-open span).
// It returns ErrInvalidRange if start > end.
func Range(start, end int) (iter.Seq[int], error) {
	if start > end {
		return nil, fmt.Errorf("range %d..%d: %w", start, end, ErrInvalidRange)
	}
	return func(yield func(int) bool) {
		for n := start; n < end; n++ {
			if !yield(n) {
				return
			}
		}
	}, nil
}

// FromSlice yields each element of values in order.
func FromSlice[V any](values []V) iter.Seq[V] {
	return func(yield func(V) bool) {
		for _, v := range values {
			if !yield(v) {
				return
			}
		}
	}
}

// Filter yields only the values of seq for which keep returns true.
func Filter[V any](seq iter.Seq[V], keep func(V) bool) iter.Seq[V] {
	return func(yield func(V) bool) {
		for v := range seq {
			if keep(v) && !yield(v) {
				return
			}
		}
	}
}

// Map yields transform(v) for each v in seq, possibly changing the element type.
func Map[A, B any](seq iter.Seq[A], transform func(A) B) iter.Seq[B] {
	return func(yield func(B) bool) {
		for a := range seq {
			if !yield(transform(a)) {
				return
			}
		}
	}
}

// Take yields at most the first n values of seq and pulls exactly n from
// upstream (zero when n == 0). It returns ErrNegativeLimit if n < 0.
func Take[V any](seq iter.Seq[V], n int) (iter.Seq[V], error) {
	if n < 0 {
		return nil, fmt.Errorf("take %d: %w", n, ErrNegativeLimit)
	}
	return func(yield func(V) bool) {
		if n == 0 {
			return
		}
		count := 0
		for v := range seq {
			if !yield(v) {
				return
			}
			count++
			if count == n {
				return
			}
		}
	}, nil
}

// Skip discards the first n values of seq and yields the rest. It returns
// ErrNegativeLimit if n < 0.
func Skip[V any](seq iter.Seq[V], n int) (iter.Seq[V], error) {
	if n < 0 {
		return nil, fmt.Errorf("skip %d: %w", n, ErrNegativeLimit)
	}
	return func(yield func(V) bool) {
		count := 0
		for v := range seq {
			if count < n {
				count++
				continue
			}
			if !yield(v) {
				return
			}
		}
	}, nil
}

// Chain yields all values of the first sequence, then the second, and so on.
func Chain[V any](seqs ...iter.Seq[V]) iter.Seq[V] {
	return func(yield func(V) bool) {
		for _, seq := range seqs {
			for v := range seq {
				if !yield(v) {
					return
				}
			}
		}
	}
}

// Reduce folds seq into a single value, starting from initial and applying
// combine to each element in order. It is terminal: it consumes one pass.
func Reduce[V, R any](seq iter.Seq[V], initial R, combine func(R, V) R) R {
	result := initial
	for v := range seq {
		result = combine(result, v)
	}
	return result
}
```

Read the intermediates as the same loop with a different body. `Filter`'s `keep(v) && !yield(v)` short-circuits: only a kept value is yielded, and only a rejected `yield` returns. `Map` changes the type parameter from `A` to `B`, which is why it is the combinator that lets a pipeline of `int` become a pipeline of `string`. `Skip` counts down before it starts yielding; `Chain` runs an outer loop over the sequences and an inner loop over each one's values, with the stop check on the inner `yield` so a downstream break ends the whole concatenation, not just the current sequence. `Reduce` has no `yield` at all because it is terminal — it ranges to the end (or until a `break` upstream) and returns the accumulator.

### The runnable demo

The demo builds the canonical pipeline — keep the even numbers, square them, take the first five — over a `Range` of a hundred, then folds the result with `Reduce`. Because the pipeline is lazy, `Take(squares, 5)` stops the whole chain after the fifth even square, so `Range` is never driven anywhere near 100.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"

	"example.com/itertools"
)

func main() {
	source, err := itertools.Range(1, 100)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	evens := itertools.Filter(source, func(n int) bool { return n%2 == 0 })
	squares := itertools.Map(evens, func(n int) int { return n * n })
	firstFive, err := itertools.Take(squares, 5)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	result := slices.Collect(firstFive)
	fmt.Println("first five even squares:", result)

	sum := itertools.Reduce(itertools.FromSlice(result), 0, func(acc, n int) int { return acc + n })
	fmt.Println("sum:", sum)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first five even squares: [4 16 36 64 100]
sum: 220
```

### Tests

The tests pin four properties. `TestPipeline` checks that a `Filter -> Map -> Take` chain yields exactly the right values. `TestSkipChainReduce` exercises the remaining combinators together. `TestTakeIsLazy` is the sharpest test: it wraps a counting source and asserts `Take` pulled *exactly* `n` values, which is the property the careful `Take` structure exists to provide. `TestTakeZero` proves `Take(seq, 0)` yields nothing and pulls nothing. `TestValidationErrors` confirms each validating constructor returns its sentinel error.

Create `itertools_test.go`:

```go
package itertools

import (
	"errors"
	"slices"
	"testing"
)

func TestPipeline(t *testing.T) {
	t.Parallel()

	source, err := Range(1, 1000)
	if err != nil {
		t.Fatal(err)
	}
	evens := Filter(source, func(n int) bool { return n%2 == 0 })
	squares := Map(evens, func(n int) int { return n * n })
	pipeline, err := Take(squares, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := slices.Collect(pipeline), []int{4, 16, 36, 64, 100}; !slices.Equal(got, want) {
		t.Fatalf("pipeline = %v, want %v", got, want)
	}
}

func TestSkipChainReduce(t *testing.T) {
	t.Parallel()

	skipped, err := Skip(FromSlice([]int{1, 2, 3, 4}), 2)
	if err != nil {
		t.Fatal(err)
	}
	combined := Chain(skipped, FromSlice([]int{10, 20}))
	if got, want := slices.Collect(combined), []int{3, 4, 10, 20}; !slices.Equal(got, want) {
		t.Fatalf("Chain = %v, want %v", got, want)
	}

	sum := Reduce(FromSlice([]int{1, 2, 3, 4}), 0, func(total, n int) int { return total + n })
	if sum != 10 {
		t.Fatalf("sum = %d, want 10", sum)
	}
}

func TestTakeIsLazy(t *testing.T) {
	t.Parallel()

	pulled := 0
	source := func(yield func(int) bool) {
		for i := 1; i <= 1_000_000; i++ {
			pulled++
			if !yield(i) {
				return
			}
		}
	}

	taken, err := Take(source, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := slices.Collect(taken), []int{1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("Take = %v, want %v", got, want)
	}
	if pulled != 3 {
		t.Fatalf("source pulled %d values, want exactly 3", pulled)
	}
}

func TestTakeZero(t *testing.T) {
	t.Parallel()

	pulled := 0
	source := func(yield func(int) bool) {
		for i := 1; i <= 10; i++ {
			pulled++
			if !yield(i) {
				return
			}
		}
	}

	taken, err := Take(source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Collect(taken); len(got) != 0 {
		t.Fatalf("Take(seq, 0) = %v, want no values", got)
	}
	if pulled != 0 {
		t.Fatalf("Take(seq, 0) pulled %d values, want 0", pulled)
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		call func() error
		want error
	}{
		{name: "range", call: func() error { _, err := Range(5, 1); return err }, want: ErrInvalidRange},
		{name: "take", call: func() error { _, err := Take(FromSlice([]int{1}), -1); return err }, want: ErrNegativeLimit},
		{name: "skip", call: func() error { _, err := Skip(FromSlice([]int{1}), -1); return err }, want: ErrNegativeLimit},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.call(); !errors.Is(err, tc.want) {
				t.Fatalf("%s error = %v, want %v", tc.name, err, tc.want)
			}
		})
	}
}
```

Create `example_test.go`:

```go
package itertools

import (
	"fmt"
	"slices"
)

func ExampleFilter() {
	even := Filter(FromSlice([]int{1, 2, 3, 4, 5, 6}), func(n int) bool { return n%2 == 0 })
	fmt.Println(slices.Collect(even))
	// Output: [2 4 6]
}
```

## Review

The library is sound when every intermediate is a closure that does no work until ranged over and stops the instant its `yield` returns `false`. The clearest evidence is `TestTakeIsLazy`: a million-element source driven through `Take(_, 3)` pulls exactly three values, which is only true if every link — source, and `Take` itself — returns the moment downstream is satisfied. Confirm `Take(seq, 0)` pulls zero (the `n == 0` guard returns before the loop), and that `Range(5, 1)`, `Take(_, -1)`, and `Skip(_, -1)` each return their sentinel error rather than silently yielding nothing.

Common mistakes for this feature. The first is the bare `yield(v)` with no `if !... { return }`, which keeps draining upstream after a consumer breaks and trips the runtime's "continued iteration after ... returned false" panic. The second is checking `Take`'s counter at the top of the loop body, which pulls `n+1` values because `for v := range seq` pulls before the body runs; yield-then-count-then-return pulls exactly `n`. The third is treating `Reduce` as if it returned a re-rangeable sequence — it is terminal and consumes one pass, so collect into a slice first if you need the values again.

## Resources

- [`iter` package](https://pkg.go.dev/iter) — the definitions of `Seq` and `Seq2` that every combinator here produces and consumes.
- [Go Blog: Range Over Function Types](https://go.dev/blog/range-functions) — how `for range` over a `func(yield func(V) bool)` works and why `yield` returns a `bool`.
- [`slices.Collect`](https://pkg.go.dev/slices#Collect) — the terminal that drives a pipeline into a slice, used throughout the demo and tests.
- [Go Blog: An Introduction To Generics](https://go.dev/blog/intro-generics) — the type-parameter mechanics behind `Map[A, B]` changing a pipeline's element type.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-zip-and-flatten.md](02-zip-and-flatten.md)
