# Exercise 1: Collection Iterators (All, Values, Backward, Sorted)

The first decision a collection's API makes is how callers walk it. This exercise builds a generic insertion-ordered `List[E]` whose iterator methods mirror the standard `slices` vocabulary exactly: `All` yields index-element pairs, `Values` yields elements alone, `Backward` yields the reverse, and `Sorted` returns an eager slice rather than a lazy sequence.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
collection.go        List[E], New, Append, Len, All, Values, Backward, Sorted
cmd/
  demo/
    main.go          build a List, range All, range Backward, print Sorted
collection_test.go   index pairs, reverse order, reusable passes, early break, Sorted
```

- Files: `collection.go`, `cmd/demo/main.go`, `collection_test.go`.
- Implement: `List[E any]` with methods `All() iter.Seq2[int, E]`, `Values() iter.Seq[E]`, `Backward() iter.Seq2[int, E]`, plus the package function `Sorted[E cmp.Ordered](l *List[E]) []E`.
- Test: `collection_test.go` checks `All` pairs each element with its index, `Backward` reverses, both `Values` passes match (reusable), `break` stops iteration, and `Sorted` returns an ordered slice.
- Verify: `go test -run TestList -race ./...`

Set up the module:

```bash
mkdir -p collection/cmd/demo && cd collection
go mod init example.com/collection
```

### Why these four methods, and why these exact shapes

A caller who has used `slices.All`, `slices.Values`, and `slices.Backward` already knows what your collection's methods should be called and what they should return. The entire value of the convention is that it lets that knowledge transfer without reading your code. So the shapes are not free choices; they are matches against the standard library.

`All` returns `iter.Seq2[int, E]` because the natural coordinate of an ordered list is its index, exactly as `slices.All([]E) iter.Seq2[int, E]`. The first component is the position, the second is the element, and a caller writes `for i, v := range list.All()`. `Values` returns `iter.Seq[E]` — the elements with no coordinate — for the common case where the index is noise: `for v := range list.Values()`. `Backward` returns `iter.Seq2[int, E]`, not `iter.Seq[E]`, because reverse iteration still wants to know where each element sits; `slices.Backward` makes the same call, yielding the original index counting down. Returning `Seq` from `Backward` would be a quiet deviation that breaks the symmetry a caller expects between forward and reverse walks.

`Sorted` is the odd one out, and deliberately so. It returns `[]E`, not an iterator, because sorting is inherently eager: you cannot order a sequence without first seeing all of it, so the work and the allocation happen no matter what, and a lazy wrapper would only hide that. It is also a package function rather than a method, `func Sorted[E cmp.Ordered](l *List[E]) []E`, because a method cannot add a constraint to the receiver's type parameter. `List[E any]` holds any element; sorting needs `E` to be `cmp.Ordered`; a method has no place to state that extra requirement, so it moves to a function — exactly the shape of `slices.Sorted[E cmp.Ordered](iter.Seq[E]) []E`, which this function delegates to.

Every iterator method follows the same skeleton: return a closure `func(yield func(...) bool)` that walks the backing slice and checks `yield`'s boolean before continuing. The `if !yield(...) { return }` is the early-termination contract: when the caller `break`s, the runtime makes the next `yield` return `false`, and the iterator must stop. Because each method returns a fresh closure over the slice the `List` still owns, these iterators are reusable — ranging `Values()` twice yields the same elements both times, which the tests assert.

Create `collection.go`:

```go
package collection

import (
	"cmp"
	"iter"
	"slices"
)

// List is an insertion-ordered collection of elements of type E.
type List[E any] struct {
	items []E
}

// New returns a List containing items in the given order.
func New[E any](items ...E) *List[E] {
	l := &List[E]{items: make([]E, len(items))}
	copy(l.items, items)
	return l
}

// Append adds v to the end of the list.
func (l *List[E]) Append(v E) {
	l.items = append(l.items, v)
}

// Len reports the number of elements.
func (l *List[E]) Len() int {
	return len(l.items)
}

// All iterates over index/element pairs in insertion order, mirroring
// slices.All. The sequence is reusable: each call returns a fresh iterator.
func (l *List[E]) All() iter.Seq2[int, E] {
	return func(yield func(int, E) bool) {
		for i, v := range l.items {
			if !yield(i, v) {
				return
			}
		}
	}
}

// Values iterates over elements in insertion order, mirroring slices.Values.
func (l *List[E]) Values() iter.Seq[E] {
	return func(yield func(E) bool) {
		for _, v := range l.items {
			if !yield(v) {
				return
			}
		}
	}
}

// Backward iterates over index/element pairs from last to first, mirroring
// slices.Backward. It is Seq2, not Seq: reverse iteration keeps the index.
func (l *List[E]) Backward() iter.Seq2[int, E] {
	return func(yield func(int, E) bool) {
		for i := len(l.items) - 1; i >= 0; i-- {
			if !yield(i, l.items[i]) {
				return
			}
		}
	}
}

// Sorted returns a sorted snapshot of l's elements as a new slice.
//
// It is a package function, not a method, because sorting requires E to satisfy
// cmp.Ordered and a method cannot add a constraint to the receiver's type
// parameter. It returns []E rather than iter.Seq because sorting is eager: the
// whole slice is materialized and ordered, so a lazy wrapper would hide work
// already done. This mirrors slices.Sorted.
func Sorted[E cmp.Ordered](l *List[E]) []E {
	return slices.Sorted(l.Values())
}
```

### The runnable demo

The demo builds a small list, walks it forward with `All` to show the index-element pairing, walks it backward with `Backward`, and prints the eager `Sorted` slice. It makes the difference between a lazy sequence and a materialized slice concrete in one screen.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/collection"
)

func main() {
	l := collection.New("charlie", "alice", "bob")
	l.Append("dave")

	fmt.Println("All (index, element):")
	for i, name := range l.All() {
		fmt.Printf("  %d: %s\n", i, name)
	}

	fmt.Println("Backward (index, element):")
	for i, name := range l.Backward() {
		fmt.Printf("  %d: %s\n", i, name)
	}

	fmt.Println("Sorted snapshot:", collection.Sorted(l))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
All (index, element):
  0: charlie
  1: alice
  2: bob
  3: dave
Backward (index, element):
  3: dave
  2: bob
  1: alice
  0: charlie
Sorted snapshot: [alice bob charlie dave]
```

### Tests

The tests pin every shape decision. `TestListAll` checks that `All` pairs each element with its insertion index. `TestListBackward` checks the reverse walk and its descending indices. `TestListValuesReusable` ranges `Values()` twice and asserts both passes produce the same elements, proving the iterator is reusable rather than single-use. `TestListBreak` breaks out of an `All` loop after the second element and asserts the iterator honored the early stop. `TestSorted` checks that the package function returns an ordered slice.

Create `collection_test.go`:

```go
package collection

import (
	"slices"
	"testing"
)

func TestListAll(t *testing.T) {
	t.Parallel()

	l := New("a", "b", "c")
	var idxs []int
	var vals []string
	for i, v := range l.All() {
		idxs = append(idxs, i)
		vals = append(vals, v)
	}
	if !slices.Equal(idxs, []int{0, 1, 2}) {
		t.Fatalf("indices = %v, want [0 1 2]", idxs)
	}
	if !slices.Equal(vals, []string{"a", "b", "c"}) {
		t.Fatalf("values = %v, want [a b c]", vals)
	}
}

func TestListBackward(t *testing.T) {
	t.Parallel()

	l := New(10, 20, 30)
	var idxs []int
	var vals []int
	for i, v := range l.Backward() {
		idxs = append(idxs, i)
		vals = append(vals, v)
	}
	if !slices.Equal(idxs, []int{2, 1, 0}) {
		t.Fatalf("indices = %v, want [2 1 0]", idxs)
	}
	if !slices.Equal(vals, []int{30, 20, 10}) {
		t.Fatalf("values = %v, want [30 20 10]", vals)
	}
}

func TestListValuesReusable(t *testing.T) {
	t.Parallel()

	l := New("x", "y", "z")
	first := slices.Collect(l.Values())
	second := slices.Collect(l.Values())
	if !slices.Equal(first, second) {
		t.Fatalf("reusable iterator differs: first %v, second %v", first, second)
	}
	if !slices.Equal(first, []string{"x", "y", "z"}) {
		t.Fatalf("values = %v, want [x y z]", first)
	}
}

func TestListBreak(t *testing.T) {
	t.Parallel()

	l := New(1, 2, 3, 4, 5)
	var seen []int
	for i, v := range l.All() {
		if i == 2 {
			break
		}
		seen = append(seen, v)
	}
	if !slices.Equal(seen, []int{1, 2}) {
		t.Fatalf("break did not stop iteration: saw %v, want [1 2]", seen)
	}
}

func TestSorted(t *testing.T) {
	t.Parallel()

	l := New(3, 1, 2)
	if got := Sorted(l); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("Sorted = %v, want [1 2 3]", got)
	}
	if l.Len() != 3 {
		t.Fatalf("Sorted mutated the list: Len = %d, want 3", l.Len())
	}
}
```

## Review

The API is sound when its shapes match the standard library a caller already knows. Confirm `All` and `Backward` both return `iter.Seq2[int, E]` so a caller writes `for i, v := range` over either and gets the index in both directions, while `Values` returns `iter.Seq[E]` for the index-free walk. The reusable-iterator test is the one that catches a subtle design slip: because each method returns a fresh closure over the slice the list owns, two passes over `Values()` must agree — an iterator that consumed a shared cursor would fail it. The break test confirms every method honors `yield`'s boolean: drop the `if !yield(...) { return }` and the loop keeps producing into a broken-out caller.

The most common mistakes for this feature are deviating from the standard shapes and misplacing the constraint. Returning `iter.Seq[E]` from `Backward` looks harmless but breaks the forward/reverse symmetry a `slices` user expects. Trying to write `Sorted` as a method fails to compile the moment you need `cmp.Ordered`, because the receiver's `E any` cannot be narrowed by a method — the constraint belongs on the package function, exactly as `slices.Sorted` is a function. Returning a lazy `iter.Seq` from `Sorted` after already sorting and allocating would compile but lie about the cost; an eager operation returns a slice.

## Resources

- [`iter` package: Naming Conventions](https://pkg.go.dev/iter#hdr-Naming_Conventions) — the standard names (`All`, `Values`, `Backward`) and the shapes they imply.
- [`slices.All`, `slices.Values`, `slices.Backward`](https://pkg.go.dev/slices#All) — the exact signatures this list mirrors.
- [`slices.Sorted`](https://pkg.go.dev/slices#Sorted) — the eager function that consumes an `iter.Seq` and returns an ordered slice.
- [Range Over Function Types](https://go.dev/blog/range-functions) — the Go blog post that introduced the iterator design and the `All` convention.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-ordered-map-iterators.md](02-ordered-map-iterators.md)
