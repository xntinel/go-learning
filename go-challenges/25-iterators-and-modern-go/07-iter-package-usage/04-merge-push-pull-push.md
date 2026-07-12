# Exercise 4: Merge — Push to Pull and Back

The defining compositional trick of the `iter` package is the round trip: take push iterators, convert them to pull form to do work that needs lookahead, then wrap the result back into a single push iterator. This exercise builds `Merge`, which combines two already-sorted push sequences into one sorted push sequence — the textbook case that needs to peek both heads, which only pull form allows.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
merge.go             Merge(a, b iter.Seq[int]) iter.Seq[int] via iter.Pull on each input
cmd/
  demo/
    main.go          merge two sorted sequences, range over the merged result
merge_test.go        interleave, uneven lengths, empties, early-break stops both inputs
```

- Files: `merge.go`, `cmd/demo/main.go`, `merge_test.go`.
- Implement: `Merge(a, b iter.Seq[int]) iter.Seq[int]` returning the sorted union (duplicates preserved) of two sorted inputs.
- Test: interleave two sequences, merge uneven lengths, handle an empty input, and prove an early `break` stops draining both inputs.
- Verify: `go test -run 'TestMerge|TestMergeEarly' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/07-iter-package-usage/04-merge-push-pull-push/cmd/demo && cd go-solutions/25-iterators-and-modern-go/07-iter-package-usage/04-merge-push-pull-push
```

### The push-pull-push sandwich

A merge cannot be written against push iterators directly. Each input wants to own its own loop and push values at you; merging requires holding one element from each side at the same time and emitting the smaller, which is random lookahead the push model forbids. The fix is to pull. `Merge` returns an `iter.Seq[int]` — a closure over `yield` — and inside that closure it calls `iter.Pull` on both inputs, turning each push `Seq` into a `next`/`stop` pair it can advance independently.

The body is the classic two-finger merge. Prime both heads with one `next()` each. While both sides have a value, compare them, `yield` the smaller (`<=` keeps the merge stable and preserves duplicates), and advance only the side that was emitted with another `next()`. When one side runs dry, drain the other. Every `yield` is guarded: if it returns `false`, the consumer broke out of its range, so the closure returns at once — and because both `stop` functions were deferred immediately after their `iter.Pull`, returning fires both, releasing both source goroutines and running their cleanup.

That last property is what makes the result a well-behaved push iterator and not a leak waiting to happen. The output is lazy (no pulling happens until someone ranges over the returned `Seq`), composable (the merged `Seq` can itself be an input to another `Merge`), and correctly terminating on early `break`. The push → pull → push shape — return a `Seq`, pull inside it, defer the stops, yield the results — is the template every iterator combinator follows.

Create `merge.go`:

```go
// Create `merge.go`
package merge

import "iter"

// Merge returns a push iterator over the sorted union of two already-sorted
// push iterators. Equal values from both inputs are both emitted (duplicates
// are preserved), with a's value first to keep the merge stable.
func Merge(a, b iter.Seq[int]) iter.Seq[int] {
	return func(yield func(int) bool) {
		nextA, stopA := iter.Pull(a)
		defer stopA()
		nextB, stopB := iter.Pull(b)
		defer stopB()

		va, okA := nextA()
		vb, okB := nextB()

		for okA && okB {
			if va <= vb {
				if !yield(va) {
					return
				}
				va, okA = nextA()
			} else {
				if !yield(vb) {
					return
				}
				vb, okB = nextB()
			}
		}
		for okA {
			if !yield(va) {
				return
			}
			va, okA = nextA()
		}
		for okB {
			if !yield(vb) {
				return
			}
			vb, okB = nextB()
		}
	}
}
```

### The runnable demo

The demo merges two sorted sequences, ranges over the merged result to show the interleaving, then merges three sequences by nesting `Merge` calls — proof that the output is itself a valid input.

Create `cmd/demo/main.go`:

```go
// Create `cmd/demo/main.go`
package main

import (
	"fmt"
	"slices"

	"example.com/merge-push-pull-push"
)

func main() {
	a := slices.Values([]int{1, 4, 7, 10})
	b := slices.Values([]int{2, 3, 8, 9})

	var out []int
	for v := range merge.Merge(a, b) {
		out = append(out, v)
	}
	fmt.Println("merged:", out)

	// The merged Seq is itself a valid input to another Merge.
	c := slices.Values([]int{0, 5, 6})
	ab := merge.Merge(slices.Values([]int{1, 4, 7, 10}), slices.Values([]int{2, 3, 8, 9}))
	fmt.Println("merged three:", slices.Collect(merge.Merge(ab, c)))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
merged: [1 2 3 4 7 8 9 10]
merged three: [0 1 2 3 4 5 6 7 8 9 10]
```

### Tests

`TestMerge` covers interleaving, uneven lengths, an empty input on each side, and duplicate values across inputs. `TestMergeEarlyBreak` is the one that proves the stops fire: it merges two finite sorted inputs, breaks after a few values, and asserts that neither source was drained past the break — observed through a counter each input increments as it produces.

Create `merge_test.go`:

```go
// Create `merge_test.go`
package merge

import (
	"iter"
	"slices"
	"testing"
)

func TestMerge(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b []int
		want []int
	}{
		{"interleave", []int{1, 4, 7}, []int{2, 3, 8}, []int{1, 2, 3, 4, 7, 8}},
		{"uneven", []int{1, 2, 3, 4, 5}, []int{10}, []int{1, 2, 3, 4, 5, 10}},
		{"empty a", nil, []int{1, 2}, []int{1, 2}},
		{"empty b", []int{1, 2}, nil, []int{1, 2}},
		{"both empty", nil, nil, nil},
		{"duplicates", []int{1, 3, 3}, []int{2, 3}, []int{1, 2, 3, 3, 3}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := slices.Collect(Merge(slices.Values(tc.a), slices.Values(tc.b)))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Merge(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// counted wraps values in a push iterator that records how many it produced.
func counted(values []int, produced *int) iter.Seq[int] {
	return func(yield func(int) bool) {
		for _, v := range values {
			*produced++
			if !yield(v) {
				return
			}
		}
	}
}

func TestMergeEarlyBreak(t *testing.T) {
	t.Parallel()

	var producedA, producedB int
	a := counted([]int{1, 3, 5, 7, 9}, &producedA)
	b := counted([]int{2, 4, 6, 8, 10}, &producedB)

	var got []int
	for v := range Merge(a, b) {
		got = append(got, v)
		if len(got) == 3 {
			break
		}
	}
	if !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("early break collected %v, want [1 2 3]", got)
	}
	// After breaking at the third value, neither source was drained: the
	// deferred stops halted both pulls well short of their five elements.
	if producedA >= 5 || producedB >= 5 {
		t.Fatalf("inputs over-consumed after early break: a=%d b=%d", producedA, producedB)
	}
}
```

## Review

`Merge` is correct when it returns a `Seq` that pulls both inputs, defers both stops, and guards every `yield`. The table covers the algorithmic edges — interleaving, one side exhausting first, empty inputs, duplicates spanning both sources — and `<=` rather than `<` is what makes the duplicate case emit every equal value in a stable order. The early-break test is the one that proves the combinator is leak-free: when the consumer breaks at the third value, both `stop` functions run via `defer`, the underlying pull goroutines halt, and the per-input counters confirm neither source was driven to completion. Drop either `defer stop()` and the goroutines would linger; forget to check a `yield` result and a consumer's `break` would trigger the runtime's continued-iteration panic.

The mental model to keep is the sandwich: a combinator that needs lookahead returns a push `Seq`, converts its inputs to pull form inside, defers the stops so every exit path cleans up, and yields. Merge, zip, dedup, and sliding windows are all variations on those four moves.

## Resources

- [`iter.Pull`](https://pkg.go.dev/iter#Pull) — the adapter that gives `Merge` the on-demand `next`/`stop` it needs to peek both heads.
- [`slices.Values`](https://pkg.go.dev/slices#Values) — turns the test and demo slices into the push iterators `Merge` consumes.
- [Range Over Function Types](https://go.dev/blog/range-functions) — the design rationale for push and pull iterators and how they convert.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-zip-generator-with-stop.md](05-zip-generator-with-stop.md)
