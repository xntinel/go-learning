# Exercise 23: Lazy Iterator with Map/Filter Without Intermediate Allocation

**Nivel: Intermedio** — validacion rapida (un test corto).

Chaining `Map` then `Filter` then taking the first three results, if each
stage builds its own slice, means allocating and fully processing a slice
of results you only needed three of. Go 1.23's `iter.Seq` makes `Map` and
`Filter` lazy: each stage is a function that pulls one value at a time
from the stage before it, so nothing downstream of `Take`'s cutoff is ever
computed.

## What you'll build

```text
lazyseq/                     independent module: example.com/lazyseq
  go.mod                     go 1.24
  lazyseq.go                 func Of, Map, Filter, Take, Collect (iter.Seq[T])
  lazyseq_test.go            chained transform, early Take, proof of laziness via call counts
```

- Files: `lazyseq.go`, `lazyseq_test.go`.
- Implement: `Of[T any](values ...T) iter.Seq[T]`, `Map[T, R any](seq iter.Seq[T], fn func(T) R) iter.Seq[R]`, `Filter[T any](seq iter.Seq[T], keep func(T) bool) iter.Seq[T]`, `Take[T any](seq iter.Seq[T], n int) iter.Seq[T]`, and `Collect[T any](seq iter.Seq[T]) []T`.
- Test: a chained `Map` + `Filter` + `Collect` produces the expected values; `Take` cuts a sequence off at `n`, including `n <= 0`; the counted-calls tests prove `Map`'s function and `Filter`'s predicate never run on values past what `Take` actually consumed.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/10-higher-order-functions/23-lazy-iterator-collect-transform
cd go-solutions/04-functions/10-higher-order-functions/23-lazy-iterator-collect-transform
go mod edit -go=1.24
```

### Each stage is a closure over the stage before it, not a slice

An `iter.Seq[T]` is `func(yield func(T) bool)` — a function that, when
called, produces values by calling `yield` with each one and stops early
if `yield` returns `false`. `Map` and `Filter` do not consume their input
`seq` and build a `[]T` in one pass; instead each one returns a *new*
`iter.Seq` that, when finally driven by something like `Collect`, pulls
one value from the upstream `seq`, transforms or checks it, and forwards
it — all inside a single `range seq` loop that itself only advances one
step per `yield` call. Nothing is materialized until `Collect` (or any
other terminal consumer) actually asks for values.

The propagation of `yield`'s `false` return is what makes the laziness
observable, not just theoretical. `Take(seq, n)` calls the downstream
`yield` for each of the first `n` values and then simply `return`s without
consuming any more of `seq` — cutting the whole pipeline off at the
source. Both `Map` and `Filter` check `!yield(...)` and `return` early too:
if a consumer downstream (like `Take`) ever stops asking for values, every
stage in the chain stops pulling from the one before it, rather than
running to completion and discarding the extra results. `Collect` is
where allocation finally happens exactly once, in the one place a caller
actually asked to materialize a slice — every intermediate `Map` or
`Filter` stage allocates nothing.

Create `lazyseq.go`:

```go
package lazyseq

import "iter"

// Of returns a Seq over a fixed list of values, the simplest possible
// source for building a pipeline on top of.
func Of[T any](values ...T) iter.Seq[T] {
	return func(yield func(T) bool) {
		for _, v := range values {
			if !yield(v) {
				return
			}
		}
	}
}

// Map lazily transforms each value seq produces using fn. No intermediate
// slice is ever allocated: fn runs once per value, exactly when the
// downstream consumer asks for the next one.
func Map[T, R any](seq iter.Seq[T], fn func(T) R) iter.Seq[R] {
	return func(yield func(R) bool) {
		for v := range seq {
			if !yield(fn(v)) {
				return
			}
		}
	}
}

// Filter lazily keeps only the values from seq for which keep reports
// true, again without materializing anything in between.
func Filter[T any](seq iter.Seq[T], keep func(T) bool) iter.Seq[T] {
	return func(yield func(T) bool) {
		for v := range seq {
			if keep(v) {
				if !yield(v) {
					return
				}
			}
		}
	}
}

// Take stops seq after its first n values, signaling upstream stages to
// stop producing more — the mechanism that makes laziness observable:
// without it, every stage would have to run to completion.
func Take[T any](seq iter.Seq[T], n int) iter.Seq[T] {
	return func(yield func(T) bool) {
		if n <= 0 {
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
	}
}

// Collect is the one point in a pipeline where allocation happens: it
// materializes seq into a slice, deferred until the caller actually wants
// one instead of happening at every intermediate stage.
func Collect[T any](seq iter.Seq[T]) []T {
	var out []T
	for v := range seq {
		out = append(out, v)
	}
	return out
}
```

### Tests

`TestMapFilterCollect` is the basic chained-pipeline case: double every
number, keep only multiples of four, collect. `TestTakeStopsEarly` and
`TestTakeZeroOrNegativeYieldsNothing` cover `Take`'s cutoff and its two
zero-or-negative edges. The two laziness tests are the ones that actually
matter for this exercise's claim: `TestMapIsLazyNotEagerlyAppliedToWholeSource`
counts how many times `Map`'s function ran across a ten-element source cut
off at three by `Take`, and requires that count to be exactly three, not
ten — proving `Map` never touched the other seven. `TestFilterIsLazyNotEagerlyAppliedToWholeSource`
does the same for `Filter`'s predicate. `TestCollectOnEmptySeqReturnsEmptySlice`
guards the empty-source edge.

Create `lazyseq_test.go`:

```go
package lazyseq

import "testing"

func TestMapFilterCollect(t *testing.T) {
	t.Parallel()

	nums := Of(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	doubled := Map(nums, func(n int) int { return n * 2 })
	even := Filter(doubled, func(n int) bool { return n%4 == 0 })

	got := Collect(even)
	want := []int{4, 8, 12, 16, 20}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestTakeStopsEarly(t *testing.T) {
	t.Parallel()

	nums := Of(1, 2, 3, 4, 5)
	got := Collect(Take(nums, 3))
	want := []int{1, 2, 3}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestTakeZeroOrNegativeYieldsNothing(t *testing.T) {
	t.Parallel()

	nums := Of(1, 2, 3)
	if got := Collect(Take(nums, 0)); len(got) != 0 {
		t.Fatalf("Take(seq, 0) = %v, want empty", got)
	}
	if got := Collect(Take(nums, -1)); len(got) != 0 {
		t.Fatalf("Take(seq, -1) = %v, want empty", got)
	}
}

func TestMapIsLazyNotEagerlyAppliedToWholeSource(t *testing.T) {
	t.Parallel()

	var mapped int
	source := Of(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	counted := Map(source, func(n int) int {
		mapped++
		return n
	})

	got := Collect(Take(counted, 3))

	if len(got) != 3 {
		t.Fatalf("got %v, want length 3", got)
	}
	if mapped != 3 {
		t.Fatalf("Map's fn ran %d times, want exactly 3 — laziness means it must not touch the remaining 7 source values", mapped)
	}
}

func TestFilterIsLazyNotEagerlyAppliedToWholeSource(t *testing.T) {
	t.Parallel()

	var checked int
	source := Of(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	keepEven := Filter(source, func(n int) bool {
		checked++
		return n%2 == 0
	})

	got := Collect(Take(keepEven, 2)) // first two even numbers: 2, 4

	want := []int{2, 4}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if checked != 4 {
		t.Fatalf("Filter's predicate ran %d times, want exactly 4 (1,2,3,4 checked to find two evens)", checked)
	}
}

func TestCollectOnEmptySeqReturnsEmptySlice(t *testing.T) {
	t.Parallel()

	got := Collect(Of[int]())
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}
```

## Review

`Map` and `Filter` are correct when they never allocate a slice and never
consume more of their upstream `seq` than a downstream consumer actually
asked for — the call-counting tests are what turn "this is lazy" from a
claim about the implementation into an assertion a test enforces. The
`!yield(...)` check and early `return` in every stage is the propagation
mechanism: without it, `Take` cutting a consumer off would have no effect
on how much work `Map` or `Filter` upstream still did. `Collect` is
deliberately the only function in this file that allocates, which is the
whole point of building a pipeline out of `iter.Seq` values instead of
plain slice-returning helpers — the cost of materializing is paid exactly
once, at the point the caller actually needs a `[]T`.

## Resources

- [iter package](https://pkg.go.dev/iter) — `Seq`, the range-over-func iterator type this exercise is built on.
- [Go 1.23 Release Notes: Range over function types](https://go.dev/doc/go1.23#language) — the language feature that made user-defined lazy iterators possible.
- [Go Blog: Range over Function Types](https://go.dev/blog/range-functions) — the design rationale behind `iter.Seq` and range-over-func.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-reduce-fold-with-early-stop.md](22-reduce-fold-with-early-stop.md) | Next: [24-distributed-lock-acquire-with-retry.md](24-distributed-lock-acquire-with-retry.md)
