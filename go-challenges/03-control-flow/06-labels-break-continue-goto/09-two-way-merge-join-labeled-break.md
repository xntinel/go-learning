# Exercise 9: Merge two sorted streams and stop early at a result limit

A two-way merge join walks two sorted inputs — sorted index segments, or sorted
key ranges from two shards — emitting their ordered union. When a query has a
`LIMIT`, the merge should stop the moment it has produced enough rows, not merge
everything and slice afterward. The early exit is decided inside a comparison
branch of a `switch`, so leaving the merge loop from there requires a labeled
`break`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
mergejoin/                 independent module: example.com/mergejoin
  go.mod                   go 1.24
  mergejoin.go             Merge[T cmp.Ordered]: ordered union with an optional limit
  cmd/
    demo/
      main.go              runnable demo: merge two sorted shard key ranges, limit 5
  mergejoin_test.go        full merge; prefix under limit; disjoint/empty; sorted property
```

- Files: `mergejoin.go`, `cmd/demo/main.go`, `mergejoin_test.go`.
- Implement: `Merge[T cmp.Ordered](a, b []T, limit int) []T` that produces the sorted union of two sorted inputs, stopping once `limit` elements have been emitted (a non-positive limit means no limit), using `cmp.Compare` for ordering and a labeled `break` from inside a comparison branch.
- Test: the full merge of two sorted inputs is the correct ordered union; with `limit = k` the output has exactly `k` elements and is a prefix of the full merge; disjoint and one-empty inputs; a property check that output is sorted and `len <= limit`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/mergejoin/cmd/demo
cd ~/go-exercises/mergejoin
go mod init example.com/mergejoin
go mod edit -go=1.24
```

### Why the early break needs a label

The merge loop advances two cursors, `i` over `a` and `j` over `b`, always
emitting the smaller of the two heads so the output stays sorted. The decision of
*which* input to take from is a `switch`: take from `a` when `b` is exhausted or
`a`'s head is not greater than `b`'s; otherwise take from `b`. The limit check
sits *inside* each branch, right after the emit — and that is the crux. A `break`
inside a `switch` case targets the `switch`, not the enclosing `for`. So a bare
`break` after appending the k-th element would leave the `switch`, the `for` would
loop again, and the merge would keep emitting past the limit. The labeled `break
merge` names the `for` and actually stops the merge. This is the same gotcha as
the `for`-`select` event loop, in a CPU-bound shape instead of an I/O one.

Using `cmp.Compare` rather than a hand-written `<` keeps the join generic over any
`cmp.Ordered` type and gives a three-way result (`-1`, `0`, `+1`) that documents
the tie-handling: on equal heads the code takes from `a` first, so equal keys from
both inputs are both emitted (a merge, not a dedup) with `a`'s copy ordered first.
The output is always sorted and never longer than the limit.

Create `mergejoin.go`:

```go
package mergejoin

import "cmp"

// Merge returns the sorted union of two sorted slices, emitting every element of
// both (duplicates across inputs are kept: this is a merge, not a dedup). If
// limit > 0 it stops once limit elements have been produced. Inputs are assumed
// sorted in ascending order.
func Merge[T cmp.Ordered](a, b []T, limit int) []T {
	out := make([]T, 0, len(a)+len(b))
	i, j := 0, 0

merge:
	for i < len(a) || j < len(b) {
		switch {
		case j >= len(b) || (i < len(a) && cmp.Compare(a[i], b[j]) <= 0):
			out = append(out, a[i])
			i++
			if limit > 0 && len(out) == limit {
				break merge // leave the for, not just the switch
			}
		default:
			out = append(out, b[j])
			j++
			if limit > 0 && len(out) == limit {
				break merge // leave the for, not just the switch
			}
		}
	}

	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mergejoin"
)

func main() {
	// Sorted key ranges from two shards.
	shardA := []int{1, 4, 6, 9, 11}
	shardB := []int{2, 3, 6, 10}

	full := mergejoin.Merge(shardA, shardB, 0)
	fmt.Println("full merge:", full)

	limited := mergejoin.Merge(shardA, shardB, 5)
	fmt.Println("limit 5:   ", limited)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
full merge: [1 2 3 4 6 6 9 10 11]
limit 5:    [1 2 3 4 6]
```

The key `6` appears in both shards and is emitted twice in the full merge. Under
`limit 5` the output is exactly the first five elements of the full merge.

### Tests

`TestFullMerge` checks the ordered union of two sorted inputs. `TestLimitPrefix`
asserts that for every `k` the limited output is exactly the first `k` elements of
the full merge — proving the early break stops at the right place and does not
overshoot. `TestEdges` covers disjoint inputs, an empty input, and both empty.
`TestSortedProperty` is a property check over generated inputs: the output is
always sorted and never longer than the limit.

Create `mergejoin_test.go`:

```go
package mergejoin

import (
	"fmt"
	"slices"
	"testing"
)

func TestFullMerge(t *testing.T) {
	t.Parallel()

	a := []int{1, 4, 6, 9}
	b := []int{2, 3, 6, 10}
	got := Merge(a, b, 0)
	want := []int{1, 2, 3, 4, 6, 6, 9, 10}
	if !slices.Equal(got, want) {
		t.Fatalf("Merge = %v, want %v", got, want)
	}
}

func TestLimitPrefix(t *testing.T) {
	t.Parallel()

	a := []int{1, 4, 6, 9}
	b := []int{2, 3, 6, 10}
	full := Merge(a, b, 0)

	for k := 0; k <= len(full); k++ {
		got := Merge(a, b, k)
		want := full
		if k > 0 {
			want = full[:k]
		}
		if !slices.Equal(got, want) {
			t.Fatalf("Merge limit %d = %v, want prefix %v", k, got, want)
		}
	}
}

func TestEdges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b []int
		want []int
	}{
		{name: "disjoint", a: []int{1, 3, 5}, b: []int{2, 4, 6}, want: []int{1, 2, 3, 4, 5, 6}},
		{name: "a empty", a: nil, b: []int{2, 4}, want: []int{2, 4}},
		{name: "b empty", a: []int{1, 3}, b: nil, want: []int{1, 3}},
		{name: "both empty", a: nil, b: nil, want: []int{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Merge(tc.a, tc.b, 0)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Merge = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSortedProperty(t *testing.T) {
	t.Parallel()

	// Deterministic generated inputs: two arithmetic progressions.
	a := make([]int, 0, 50)
	b := make([]int, 0, 50)
	for i := range 50 {
		a = append(a, i*2)
		b = append(b, i*3)
	}
	if !slices.IsSorted(a) || !slices.IsSorted(b) {
		t.Fatal("inputs must be sorted")
	}

	for _, limit := range []int{0, 1, 7, 33, 200} {
		got := Merge(a, b, limit)
		if !slices.IsSorted(got) {
			t.Fatalf("limit %d: output not sorted: %v", limit, got)
		}
		if limit > 0 && len(got) > limit {
			t.Fatalf("limit %d: output length %d exceeds limit", limit, len(got))
		}
	}
}

func ExampleMerge() {
	a := []string{"apple", "cherry"}
	b := []string{"banana", "date"}
	fmt.Println(Merge(a, b, 3))
	// Output: [apple banana cherry]
}
```

## Review

The merge is correct when the full output is the sorted union of both inputs
(duplicates across inputs kept) and the limited output is exactly its length-`k`
prefix. The bug this exercise targets is a bare `break` after the k-th emit: it
leaves the `switch`, the `for` loops again, and the merge overshoots the limit —
`TestLimitPrefix` fails against that version because the output is longer than
`k`. Using `cmp.Compare` keeps the join generic and makes the equal-heads
tie-handling explicit (take from `a` first, both copies emitted). The property
test pins the two invariants that matter regardless of input: sorted output, and
length bounded by the limit. This closes the lesson: the same labeled-break that
stops a `for`-`select` on shutdown stops a CPU-bound merge at its limit.

## Resources

- [cmp.Compare](https://pkg.go.dev/cmp#Compare) — the three-way ordering used to pick the smaller head.
- [cmp.Ordered](https://pkg.go.dev/cmp#Ordered) — the constraint that makes `Merge` generic.
- [slices.IsSorted](https://pkg.go.dev/slices#IsSorted) — the sorted-output property check.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-logscan-multiline-records.md](08-logscan-multiline-records.md) | Next: [10-shard-fanout-labeled-break-continue.md](10-shard-fanout-labeled-break-continue.md)
