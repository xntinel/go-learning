# Exercise 6: Lazy Pull-Based Operators with range-over-func

The channel operators in the earlier exercises are push-based and concurrent: a goroutine pushes records downstream. Go 1.23 added range-over-func iterators (`iter.Seq[V]`), a pull-based, single-goroutine alternative with no channels and no goroutines. This exercise builds map, filter, and flat-map over `iter.Seq`, shows how early termination short-circuits an infinite generator, and explains when this lazy form beats the concurrent one.

This module is fully self-contained. It defines the generic `iter.Seq` operators, a couple of source and sink helpers, its own demo, and its own tests. Nothing here imports any other exercise.

## What you'll build

```text
seq.go                 FromSlice, Count, MapSeq, FilterSeq, FlatMapSeq, Collect, Take
cmd/
  demo/
    main.go            compose Filter -> Map over an infinite generator, take 5
seq_test.go            map/filter/flatmap results, lazy early-stop, composition
```

- Files: `seq.go`, `cmd/demo/main.go`, `seq_test.go`.
- Implement: `MapSeq`, `FilterSeq`, `FlatMapSeq` as `iter.Seq` transformers that propagate `yield`'s stop signal, plus `FromSlice`, `Count` (an infinite generator), `Collect`, and `Take`.
- Test: `seq_test.go` checks each operator's output, proves a chain over an infinite `Count` stops early without hanging, and proves composition is lazy and order-preserving.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/43-capstone-stream-processing-engine/02-operators-map-filter-flatmap/06-iter-seq-operators/cmd/demo && cd go-solutions/43-capstone-stream-processing-engine/02-operators-map-filter-flatmap/06-iter-seq-operators
go mod edit -go=1.26
```

### Why the yield boolean is load-bearing

An `iter.Seq[V]` is just a function `func(yield func(V) bool)`. A `for v := range seq` loop drives it: the runtime calls the sequence function, passing in a `yield` closure, and every `yield(v)` the sequence makes delivers one value to the loop body. When the loop body runs `break`, `return`, or otherwise stops early, the runtime makes the next `yield` call return `false`. That boolean is the entire backpressure and cancellation mechanism of a pull-based iterator, and an operator that ignores it is broken.

Consider `MapSeq`. It returns a new `iter.Seq[Out]` that ranges over its upstream `iter.Seq[In]`, applies `fn`, and yields the result. The body must be `if !yield(fn(v)) { return }`: when the downstream consumer stops, `yield(fn(v))` returns `false`, and returning immediately stops ranging over the upstream. Drop that check — write `for v := range seq { yield(fn(v)) }` — and the operator keeps pulling from its upstream forever even after the consumer has left. Over a finite slice that merely wastes work; over the infinite `Count` generator it hangs the program, because the generator never stops producing and the operator never stops asking. Propagating `false` up the whole chain is what lets a `break` five levels down halt an infinite source at the top.

This pull model is the inverse of the channel operators. There is no goroutine and no channel: nothing runs until the final `for v := range seq` loop pulls the first value, and each pull threads synchronously up through every operator to the source and back. That makes the operators lazy — `MapSeq(FilterSeq(Count(), even), square)` allocates nothing and computes nothing until ranged — and free of the concurrency overhead that channels impose. The trade-off is the mirror image of the channel operators' trade-off: pull-based iterators give zero-overhead, lazy, single-goroutine composition but no parallelism, so they win for in-memory transformation chains and lose where you actually need concurrent stages. `Take` makes the early-stop explicit: it wraps a sequence and yields at most the first `n` values, then returns `false`-free by simply ceasing to yield, which signals completion to its own consumer while having stopped pulling upstream after `n`.

Create `seq.go`:

```go
// Package stream provides lazy, pull-based stream operators built on Go's
// range-over-func iterators (iter.Seq). They use no channels and no
// goroutines: nothing runs until a range loop pulls, and early termination
// propagates up the chain via yield's boolean result.
package stream

import "iter"

// FromSlice returns a sequence that yields each element of s in order.
func FromSlice[V any](s []V) iter.Seq[V] {
	return func(yield func(V) bool) {
		for _, v := range s {
			if !yield(v) {
				return
			}
		}
	}
}

// Count returns an infinite sequence 1, 2, 3, ... It terminates only when
// the consumer stops, which is signalled by yield returning false.
func Count() iter.Seq[int] {
	return func(yield func(int) bool) {
		for n := 1; ; n++ {
			if !yield(n) {
				return
			}
		}
	}
}

// MapSeq lazily applies fn to every element of seq. The yield result is
// checked so a consumer that stops early halts the upstream pull.
func MapSeq[In, Out any](seq iter.Seq[In], fn func(In) Out) iter.Seq[Out] {
	return func(yield func(Out) bool) {
		for v := range seq {
			if !yield(fn(v)) {
				return
			}
		}
	}
}

// FilterSeq lazily yields only the elements of seq for which pred is true.
func FilterSeq[V any](seq iter.Seq[V], pred func(V) bool) iter.Seq[V] {
	return func(yield func(V) bool) {
		for v := range seq {
			if pred(v) {
				if !yield(v) {
					return
				}
			}
		}
	}
}

// FlatMapSeq lazily expands every element of seq into zero or more outputs.
func FlatMapSeq[In, Out any](seq iter.Seq[In], fn func(In) []Out) iter.Seq[Out] {
	return func(yield func(Out) bool) {
		for v := range seq {
			for _, o := range fn(v) {
				if !yield(o) {
					return
				}
			}
		}
	}
}

// Take returns a sequence yielding at most the first n elements of seq.
// It stops pulling upstream once n elements have been produced.
func Take[V any](seq iter.Seq[V], n int) iter.Seq[V] {
	return func(yield func(V) bool) {
		if n <= 0 {
			return
		}
		count := 0
		for v := range seq {
			if !yield(v) {
				return
			}
			count++
			if count >= n {
				return
			}
		}
	}
}

// Collect materialises a sequence into a slice. Use it only on finite
// sequences: collecting an infinite one never returns.
func Collect[V any](seq iter.Seq[V]) []V {
	var out []V
	for v := range seq {
		out = append(out, v)
	}
	return out
}
```

`Take` is the bridge between an infinite source and a finite result: `Collect(Take(Count(), 5))` returns `[1 2 3 4 5]` and then stops, because once `Take` has yielded five values it returns, its `for v := range seq` loop over `Count` ends, and `Count` is told to stop. Without `Take`, `Collect(Count())` would loop forever.

### The runnable demo

The demo composes `FilterSeq` and `MapSeq` over the infinite `Count` generator and takes the first five results, proving the chain is lazy: an infinite source flows through two operators and stops after five values because the consumer stops asking. It then shows `FlatMapSeq` expanding words into characters.

Create `cmd/demo/main.go`:

```go
// Command demo composes lazy iter.Seq operators over an infinite source and
// shows that early termination short-circuits the whole chain.
package main

import (
	"fmt"

	stream "example.com/iter-seq-operators"
)

func main() {
	// Square the even numbers, drawn from an INFINITE source, take 5.
	evens := stream.FilterSeq(stream.Count(), func(n int) bool { return n%2 == 0 })
	squares := stream.MapSeq(evens, func(n int) int { return n * n })
	fmt.Println("first 5 even squares:", stream.Collect(stream.Take(squares, 5)))

	// FlatMap: expand each word into its runes.
	words := stream.FromSlice([]string{"go", "fp"})
	chars := stream.FlatMapSeq(words, func(w string) []string {
		out := make([]string, 0, len(w))
		for _, r := range w {
			out = append(out, string(r))
		}
		return out
	})
	fmt.Println("characters:", stream.Collect(chars))

	// Early stop with a plain range/break also halts the infinite source.
	fmt.Print("count until 3: ")
	for n := range stream.Count() {
		fmt.Print(n, " ")
		if n == 3 {
			break
		}
	}
	fmt.Println()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first 5 even squares: [4 16 36 64 100]
characters: [g o f p]
count until 3: 1 2 3 
```

### Tests

`TestMapSeq` and `TestFilterSeq` check the obvious transforms over a finite slice. `TestFlatMapSeq` checks one-to-many expansion. `TestLazyEarlyStopOnInfinite` is the important one: it composes operators over the infinite `Count`, takes a few, and asserts the program returns at all — if any operator ignored `yield`'s result the test would hang and the `-timeout` would fail it. `TestCompositionOrder` proves chained operators preserve order and apply in the written sequence.

Create `seq_test.go`:

```go
package stream

import (
	"testing"
)

func eq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestMapSeq verifies lazy map over a finite source.
func TestMapSeq(t *testing.T) {
	t.Parallel()

	got := Collect(MapSeq(FromSlice([]int{1, 2, 3}), func(n int) int { return n * 10 }))
	if !eq(got, []int{10, 20, 30}) {
		t.Errorf("got %v, want [10 20 30]", got)
	}
}

// TestFilterSeq verifies lazy filter over a finite source.
func TestFilterSeq(t *testing.T) {
	t.Parallel()

	got := Collect(FilterSeq(FromSlice([]int{1, 2, 3, 4, 5, 6}), func(n int) bool { return n%2 == 0 }))
	if !eq(got, []int{2, 4, 6}) {
		t.Errorf("got %v, want [2 4 6]", got)
	}
}

// TestFlatMapSeq verifies one-to-many expansion.
func TestFlatMapSeq(t *testing.T) {
	t.Parallel()

	got := Collect(FlatMapSeq(FromSlice([]int{1, 2, 3}), func(n int) []int { return []int{n, -n} }))
	if !eq(got, []int{1, -1, 2, -2, 3, -3}) {
		t.Errorf("got %v, want [1 -1 2 -2 3 -3]", got)
	}
}

// TestLazyEarlyStopOnInfinite verifies a chain over an infinite generator
// terminates when the consumer takes a finite prefix. If any operator
// ignored yield's result this test would hang until the test timeout.
func TestLazyEarlyStopOnInfinite(t *testing.T) {
	t.Parallel()

	pulled := 0
	source := func(yield func(int) bool) {
		for n := 1; ; n++ {
			pulled++
			if !yield(n) {
				return
			}
		}
	}

	squares := MapSeq(FilterSeq(source, func(n int) bool { return n%2 == 0 }), func(n int) int { return n * n })
	got := Collect(Take(squares, 3))

	if !eq(got, []int{4, 16, 36}) {
		t.Fatalf("got %v, want [4 16 36]", got)
	}
	// The source must have stopped: it was pulled a bounded number of times,
	// not infinitely. Reaching 6 means it produced 1..6 and then stopped.
	if pulled != 6 {
		t.Errorf("source pulled %d times, want 6 (then stopped)", pulled)
	}
}

// TestCompositionOrder verifies chained operators preserve order and apply
// in the written sequence.
func TestCompositionOrder(t *testing.T) {
	t.Parallel()

	seq := FromSlice([]int{1, 2, 3, 4})
	seq2 := MapSeq(seq, func(n int) int { return n + 1 })      // 2 3 4 5
	seq3 := FilterSeq(seq2, func(n int) bool { return n > 3 }) // 4 5
	got := Collect(seq3)
	if !eq(got, []int{4, 5}) {
		t.Errorf("got %v, want [4 5]", got)
	}
}
```

## Review

These operators are correct when every yield call is guarded by `if !yield(...) { return }`, which is the one rule that makes early termination work. The defining test is `TestLazyEarlyStopOnInfinite`: a chain over an infinite generator must terminate, and it can only do so if the stop signal propagates from the consumer through `Take`, `MapSeq`, and `FilterSeq` to the source — the `pulled` counter pins the source to a bounded number of pulls. The conceptual mistake to avoid is reaching for these when you actually need concurrency: they run on a single goroutine and stages do not overlap, so a slow `fn` blocks the whole chain. They shine for lazy, allocation-light in-memory transforms; the channel operators remain the tool when stages must run concurrently. Run with `-race` for consistency, though a single-goroutine pull chain has no shared state to race.

## Resources

- [iter package](https://pkg.go.dev/iter) — `iter.Seq` and the range-over-func contract these operators implement.
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions) — how the `yield` boolean and early termination work.
- [Go spec: For statements with range clause](https://go.dev/ref/spec#For_range) — the language rule that lets `for v := range seq` drive a function iterator.

---

Prev: [05-keyby-partition.md](05-keyby-partition.md) | Next: [../03-windowing/00-concepts.md](../03-windowing/00-concepts.md)
