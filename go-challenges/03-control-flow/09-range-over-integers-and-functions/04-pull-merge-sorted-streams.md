# Exercise 4: Merge-Join — `iter.Pull` to Interleave Two Sorted Streams On Demand

Merging two already-sorted streams (a merge-join, or a merge of two sorted log
segments) needs to look at the front of both streams at once and advance only the
smaller side. The push model of range-over-func cannot express that peek-both-
sides look-ahead. `iter.Pull` is the tool: it converts a push iterator into a
`next`/`stop` pair you drive by hand, and getting the `defer stop()` placement
right is the whole game.

## What you'll build

```text
mergejoin/                independent module: example.com/mergejoin
  go.mod                  module example.com/mergejoin
  mergejoin.go            MergeSorted using iter.Pull on both sides
  cmd/
    demo/
      main.go             runnable demo: merge two sorted slices
  mergejoin_test.go       merge correctness + stop-count assertions, -race
```

Files: `mergejoin.go`, `cmd/demo/main.go`, `mergejoin_test.go`.
Implement: `MergeSorted(a, b iter.Seq[int], cmp func(int, int) int) iter.Seq[int]` that pulls each side independently.
Test: interleaving of equal and unequal lengths and an empty side; both `stop`s fire exactly once even on early consumer break.
Verify: `go test -count=1 -race ./...`

## The design

`MergeSorted` returns an `iter.Seq[int]`, but inside its body it does not range —
it pulls. `next, stop := iter.Pull(a)` runs `a` on a goroutine and hands back a
`next()` that returns the next `(value, ok)` and a `stop()` that tears the
sequence down. With both sides pulled, the merge is the textbook two-pointer walk:
peek `va` and `vb`, yield the smaller (using `cmp`), and advance only that side.
When one side runs out, drain the other.

The resource discipline is the reason `iter.Pull` demands care. Each pull spins a
goroutine that lives until either the sequence is exhausted or `stop()` is called.
If the consumer of `MergeSorted` breaks early, the merge body returns with one or
both sides not yet exhausted — and without a `defer stop()` on each side, those
goroutines leak forever. So the very first thing after each `iter.Pull` is
`defer stop()`. `stop()` is safe to call more than once and safe to call after
exhaustion, so the defers are always correct: on natural exhaustion they are
no-ops, on early break they do the actual teardown. `stop()` also synchronizes —
it waits for the pulled goroutine to finish — which is what makes reading a
per-source counter after the merge race-free.

Create `mergejoin.go`:

```go
package mergejoin

import "iter"

// MergeSorted interleaves two already-sorted sequences into one sorted sequence,
// pulling each side independently so it can compare the fronts. Both pulled
// sequences are stopped on every exit path, including an early consumer break.
func MergeSorted(a, b iter.Seq[int], cmp func(int, int) int) iter.Seq[int] {
	return func(yield func(int) bool) {
		nextA, stopA := iter.Pull(a)
		defer stopA()
		nextB, stopB := iter.Pull(b)
		defer stopB()

		va, oka := nextA()
		vb, okb := nextB()
		for oka && okb {
			if cmp(va, vb) <= 0 {
				if !yield(va) {
					return
				}
				va, oka = nextA()
			} else {
				if !yield(vb) {
					return
				}
				vb, okb = nextB()
			}
		}
		for oka {
			if !yield(va) {
				return
			}
			va, oka = nextA()
		}
		for okb {
			if !yield(vb) {
				return
			}
			vb, okb = nextB()
		}
	}
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"cmp"
	"fmt"
	"slices"

	"example.com/mergejoin"
)

func main() {
	a := slices.Values([]int{1, 3, 5, 7})
	b := slices.Values([]int{2, 4, 6})

	merged := mergejoin.MergeSorted(a, b, cmp.Compare[int])
	fmt.Println(slices.Collect(merged))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[1 2 3 4 5 6 7]
```

## Tests

The stop-count tests wrap each source in a `counting` iterator that increments a
counter in a `defer` when its run ends. Because `iter.Pull`'s `stop()` waits for
the pulled goroutine to finish, reading the counters after the merge completes is
safe under `-race`.

Create `mergejoin_test.go`:

```go
package mergejoin

import (
	"cmp"
	"iter"
	"reflect"
	"slices"
	"testing"
)

func counting(seq iter.Seq[int], stops *int) iter.Seq[int] {
	return func(yield func(int) bool) {
		defer func() { *stops++ }()
		for v := range seq {
			if !yield(v) {
				return
			}
		}
	}
}

func TestMergeInterleaves(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b []int
		want []int
	}{
		{"equal length", []int{1, 3, 5}, []int{2, 4, 6}, []int{1, 2, 3, 4, 5, 6}},
		{"unequal length", []int{1, 2, 3}, []int{4, 5}, []int{1, 2, 3, 4, 5}},
		{"a empty", nil, []int{1, 2}, []int{1, 2}},
		{"b empty", []int{1, 2}, nil, []int{1, 2}},
		{"both empty", nil, nil, nil},
		{"duplicates", []int{1, 1, 2}, []int{1, 3}, []int{1, 1, 1, 2, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := slices.Collect(MergeSorted(slices.Values(tc.a), slices.Values(tc.b), cmp.Compare[int]))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("merge = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBothSidesStopOnFullDrain(t *testing.T) {
	t.Parallel()

	var sa, sb int
	a := counting(slices.Values([]int{1, 3, 5}), &sa)
	b := counting(slices.Values([]int{2, 4, 6}), &sb)

	for range MergeSorted(a, b, cmp.Compare[int]) {
	}

	if sa != 1 || sb != 1 {
		t.Fatalf("stops = (%d,%d), want (1,1)", sa, sb)
	}
}

func TestBothSidesStopOnEarlyBreak(t *testing.T) {
	t.Parallel()

	var sa, sb int
	a := counting(slices.Values([]int{1, 3, 5}), &sa)
	b := counting(slices.Values([]int{2, 4, 6}), &sb)

	for v := range MergeSorted(a, b, cmp.Compare[int]) {
		if v >= 2 {
			break
		}
	}

	if sa != 1 || sb != 1 {
		t.Fatalf("stops = (%d,%d), want (1,1) even on early break", sa, sb)
	}
}
```

## Review

The merge is correct when it always advances the side whose front is smaller (ties
go to `a` via `cmp(va, vb) <= 0`, which keeps the output stable) and drains the
remaining side once one is exhausted. The resource proof is the pair of
stop-counter tests: on both full drain and early break, each side's teardown runs
exactly once, which is only true because `defer stopA()`/`defer stopB()` sit
immediately after their `iter.Pull` calls. Delete either `defer` and the
early-break test fails and the `-race` build reports a leaked goroutine. This is
the canonical reason to reach for `iter.Pull`: the push model cannot peek two
streams, and Pull's cost is a goroutine you are responsible for stopping.

## Resources

- [`iter.Pull` documentation](https://pkg.go.dev/iter#Pull)
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions)
- [`cmp.Compare`](https://pkg.go.dev/cmp#Compare)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-paginated-list-iterator.md](03-paginated-list-iterator.md) | Next: [05-log-lines-seq-pipeline.md](05-log-lines-seq-pipeline.md)
