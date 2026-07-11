# Exercise 2: Lockstep Zip and Merge

Some algorithms have to advance two sequences in a coordinated way: pair them up element by element, or interleave two sorted streams into one. A single push iterator owns its own loop and cannot do this. This exercise builds `Zip` and `MergeSorted` with `iter.Pull`, holding an independent cursor on each input so a coordinating loop can decide which one to step next.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
lockstep.go          FromSlice, Pair, Zip, MergeSorted (two iter.Pull cursors)
cmd/
  demo/
    main.go          zip uneven sequences, merge two sorted runs
lockstep_test.go     zip stops at shorter, merge drains both sides, stability
```

- Files: `lockstep.go`, `cmd/demo/main.go`, `lockstep_test.go`.
- Implement: `FromSlice[T]`, `Pair[A, B]`, `Zip[A, B](a iter.Seq[A], b iter.Seq[B]) iter.Seq[Pair[A, B]]`, and `MergeSorted(a, b iter.Seq[int]) iter.Seq[int]`.
- Test: `lockstep_test.go` checks zip stops at the shorter side, merge produces one sorted stream and drains the leftover side, and merge is stable on ties.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p lockstep/cmd/demo && cd lockstep
go mod init example.com/lockstep
```

### Why this needs two pull cursors, and the two stop calls

The reason push cannot express zip or merge is structural: a push iterator is a loop that knows how to walk *one* sequence to its end. To pair A with B you must hold a position in each and advance them independently — pull A, pull B, emit the pair, repeat. With `iter.Pull` each input becomes its own `(next, stop)` pair, so the coordinating loop reads `nextA()` and `nextB()` and is in full control of the stepping.

Both functions return push iterators themselves (`Zip` produces an `iter.Seq[Pair[A, B]]`, `MergeSorted` an `iter.Seq[int]`), so the *outputs* drop into a normal `for range`. The pull conversion is an internal implementation detail of the producer. Inside each producer, two `iter.Pull` calls open two pull cursors, and each gets its own `defer stop()` immediately, on its own line. Two cursors means two cleanups: if the consumer of the merged stream breaks early, both underlying producers must be unwound, so both `stop`s must be deferred. Putting each `defer` directly under its `iter.Pull` is the habit that makes "I opened two, I release two" impossible to get wrong.

`Zip` reads one value from each side per iteration and stops the instant *either* side is exhausted — it never pads the shorter sequence with zero values. `MergeSorted` keeps one buffered value from each side (`av`, `bv`) and their `ok` flags, emits the smaller, and re-pulls only the side it took; using `<=` for the comparison makes the merge stable, preferring the left side's value on a tie. When one side runs dry, two tail loops drain whatever remains on the other.

Create `lockstep.go`:

```go
package lockstep

import "iter"

// FromSlice returns a push iterator that yields each element of values in order.
func FromSlice[T any](values []T) iter.Seq[T] {
	return func(yield func(T) bool) {
		for _, v := range values {
			if !yield(v) {
				return
			}
		}
	}
}

// Pair is one zipped element: First from the left sequence, Second from the right.
type Pair[A, B any] struct {
	First  A
	Second B
}

// Zip pairs a and b element by element. It stops as soon as either sequence is
// exhausted and never pads the shorter side. Each input is pulled with its own
// cursor and released with its own deferred stop.
func Zip[A, B any](a iter.Seq[A], b iter.Seq[B]) iter.Seq[Pair[A, B]] {
	return func(yield func(Pair[A, B]) bool) {
		nextA, stopA := iter.Pull(a)
		defer stopA()
		nextB, stopB := iter.Pull(b)
		defer stopB()

		for {
			av, okA := nextA()
			bv, okB := nextB()
			if !okA || !okB {
				return
			}
			if !yield(Pair[A, B]{First: av, Second: bv}) {
				return
			}
		}
	}
}

// MergeSorted interleaves two ascending int sequences into one ascending
// sequence. Ties keep the left value first (stable). When one side is
// exhausted, the remainder of the other is drained.
func MergeSorted(a, b iter.Seq[int]) iter.Seq[int] {
	return func(yield func(int) bool) {
		nextA, stopA := iter.Pull(a)
		defer stopA()
		nextB, stopB := iter.Pull(b)
		defer stopB()

		av, okA := nextA()
		bv, okB := nextB()
		for okA && okB {
			if av <= bv {
				if !yield(av) {
					return
				}
				av, okA = nextA()
				continue
			}
			if !yield(bv) {
				return
			}
			bv, okB = nextB()
		}
		for okA {
			if !yield(av) {
				return
			}
			av, okA = nextA()
		}
		for okB {
			if !yield(bv) {
				return
			}
			bv, okB = nextB()
		}
	}
}
```

The merge keeps exactly one value of lookahead per side, which is what lets it compare before emitting. Note that `Zip` pulls from both sides *before* checking either `ok`: when the left side is longer, that means one extra value is pulled from the long side and discarded as the loop ends. That is harmless here because the discarded value is never needed, and the deferred `stop` cleans up the rest — but it is the kind of detail that matters once a sequence has side effects, and it is why the boundary behavior is pinned by a test rather than assumed.

### The runnable demo

The demo shows both functions on inputs that exercise their edges: a zip of a three-element and a two-element sequence (the pair count is the shorter length), and a merge of two sorted runs that interleave perfectly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lockstep"
)

func main() {
	for p := range lockstep.Zip(
		lockstep.FromSlice([]string{"x", "y", "z"}),
		lockstep.FromSlice([]int{10, 20}),
	) {
		fmt.Printf("%s=%d ", p.First, p.Second)
	}
	fmt.Println()

	for v := range lockstep.MergeSorted(
		lockstep.FromSlice([]int{1, 3, 5}),
		lockstep.FromSlice([]int{2, 4, 6}),
	) {
		fmt.Print(v, " ")
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
x=10 y=20 
1 2 3 4 5 6 
```

### Tests

`TestZipStopsAtShorter` zips a three-element and a two-element sequence and asserts exactly two pairs come out — the boundary rule that zip follows the shorter side. `TestZipEmpty` confirms an empty input yields nothing. `TestMergeSorted` merges `{1,3,5}` with `{2,4,6,8}` and checks the full sorted result, including the leftover `8` drained from the longer side. `TestMergeSortedEmptySide` runs the empty-left and empty-right cases so the tail-drain loops are covered, and `TestMergeSortedStable` pins the tie rule by feeding duplicate values.

Create `lockstep_test.go`:

```go
package lockstep

import (
	"iter"
	"reflect"
	"testing"
)

func collect[T any](seq iter.Seq[T]) []T {
	out := []T{}
	for v := range seq {
		out = append(out, v)
	}
	return out
}

func TestZipStopsAtShorter(t *testing.T) {
	t.Parallel()

	got := []string{}
	for p := range Zip(FromSlice([]int{1, 2, 3}), FromSlice([]string{"a", "b"})) {
		got = append(got, string(rune('0'+p.First))+p.Second)
	}
	want := []string{"1a", "2b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Zip = %v, want %v", got, want)
	}
}

func TestZipEmpty(t *testing.T) {
	t.Parallel()

	n := 0
	for range Zip(FromSlice([]int{}), FromSlice([]string{"a"})) {
		n++
	}
	if n != 0 {
		t.Fatalf("Zip over empty left yielded %d pairs, want 0", n)
	}
}

func TestMergeSorted(t *testing.T) {
	t.Parallel()

	got := collect(MergeSorted(FromSlice([]int{1, 3, 5}), FromSlice([]int{2, 4, 6, 8})))
	want := []int{1, 2, 3, 4, 5, 6, 8}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeSorted = %v, want %v", got, want)
	}
}

func TestMergeSortedEmptySide(t *testing.T) {
	t.Parallel()

	got := collect(MergeSorted(FromSlice([]int{}), FromSlice([]int{2, 4})))
	if want := []int{2, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("empty left: got %v, want %v", got, want)
	}
	got = collect(MergeSorted(FromSlice([]int{1, 3}), FromSlice([]int{})))
	if want := []int{1, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("empty right: got %v, want %v", got, want)
	}
}

func TestMergeSortedStable(t *testing.T) {
	t.Parallel()

	got := collect(MergeSorted(FromSlice([]int{1, 1, 2}), FromSlice([]int{1, 3})))
	if want := []int{1, 1, 1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
```

## Review

Both functions are correct when each pull cursor is matched by its own deferred `stop` and the boundary rules hold. For `Zip` the rule is "stop at the shorter side, never pad," verified by the unequal-length and empty-input tests. For `MergeSorted` the rules are "emit ascending, drain the leftover, prefer left on ties," verified by the four-versus-three merge, the two empty-side cases, and the duplicate-value case. Two `iter.Pull` calls mean two `defer stop()` lines; the merged output is itself a push iterator, so a consumer that breaks early triggers both deferred stops and unwinds both producers.

The traps are the boundary assumptions. Expecting zip to keep emitting after one side ends — padding with zero values — is the classic mistake; the test with a longer left side pins the opposite. Forgetting either tail-drain loop in the merge silently truncates the result whenever the inputs are uneven, which the empty-side test catches. And deferring only one `stop` when you opened two cursors leaks the other producer on early termination, exactly as in Exercise 1, just doubled.

## Resources

- [`iter.Pull`](https://pkg.go.dev/iter#Pull) — the conversion used for each cursor and the `stop` contract both producers honor.
- [`iter` package overview](https://pkg.go.dev/iter) — `Seq`, push vs pull, and composing iterators.
- [Go 1.23 release notes: iterators](https://go.dev/doc/go1.23) — the release that introduced range-over-func and the `iter` package.

---

Back to [01-bridge-push-to-pull.md](01-bridge-push-to-pull.md) | Next: [03-merge-join-with-pull2.md](03-merge-join-with-pull2.md)
