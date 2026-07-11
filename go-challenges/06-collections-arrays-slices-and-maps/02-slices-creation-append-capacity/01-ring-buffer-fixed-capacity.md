# Exercise 1: Fixed-Capacity Ring Buffer Over a Slice

A ring buffer is the canonical case where you want a slice to *never* grow: a
bounded, allocation-free window over the most recent N items — the shape of an
in-memory audit-log tail, a metrics sample window, or a fixed replay buffer. You
build it over a `make([]T, capacity)` backing array and drive `head`, `tail`, and
a logical `size` by hand, precisely because `append`'s reallocation is the thing
you must avoid.

This module is fully self-contained: its own module, its own demo, its own tests.
Nothing here imports another exercise.

## What you'll build

```text
ring/                      independent module: example.com/ring
  go.mod                   go 1.26
  ring.go                  Ring[T]; New, Push, Pop, Len, Cap; ErrEmpty
  cmd/
    demo/
      main.go              push past capacity, drain, observe overwrite
  ring_test.go             table + property tests, ErrEmpty via errors.Is, Example
```

Files: `ring.go`, `cmd/demo/main.go`, `ring_test.go`.
Implement: a generic `Ring[T any]` backed by a fixed slice, with `Push` overwriting the oldest element when full, `Pop` returning `ErrEmpty`, and `Len`/`Cap` accessors.
Test: push-until-full, overwrite-oldest pop order, pop-empty sentinel, wraparound after a pop, and a property that `Cap` never grows after 100 pushes.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ring/cmd/demo
cd ~/go-exercises/ring
go mod init example.com/ring
go mod edit -go=1.26
```

### Why the backing array is allocated once and never appended to

The entire point of a ring buffer is a hard capacity bound. If you reached for
`append` to add elements, the moment `len == cap` it would allocate a new, larger
array — exactly the unbounded growth the ring exists to prevent. So `New`
allocates the backing array exactly once with `make([]T, capacity)`, and `Push`
*writes into that array by index* (`r.data[r.head] = v`), advancing `head`
modulo the capacity. There is no `append` anywhere in the hot path.

Because the backing array is always full-length, `len(r.data)` is useless as a
measure of how many elements the ring logically holds — it equals `cap` from the
first allocation. That is why the ring carries a separate `size` counter. When
`size < cap`, a `Push` is filling previously-empty slots and `size` grows. Once
`size == cap`, the buffer is full: a further `Push` overwrites the oldest element
and advances `tail` in lockstep with `head`, so the window slides forward while
`size` stays pinned at `cap`. `Pop` reads at `tail`, zeroes that slot (so a
retained `T` containing pointers does not leak), advances `tail`, and decrements
`size`; on an empty buffer it returns the sentinel `ErrEmpty`.

Create `ring.go`:

```go
package ring

import "errors"

// ErrEmpty is returned by Pop when the buffer holds no elements.
var ErrEmpty = errors.New("ring buffer is empty")

// Ring is a fixed-capacity FIFO over a single backing array that never
// reallocates. When full, Push overwrites the oldest element.
type Ring[T any] struct {
	data []T
	head int // index of the next write
	tail int // index of the oldest element
	size int // logical element count, distinct from len(data)
}

// New returns a Ring holding at most capacity elements. A capacity <= 0 is
// clamped to 1 so the backing array is always valid.
func New[T any](capacity int) *Ring[T] {
	if capacity <= 0 {
		capacity = 1
	}
	return &Ring[T]{data: make([]T, capacity)}
}

// Push writes v at head. When the buffer is full it overwrites the oldest
// element and advances tail, sliding the window forward.
func (r *Ring[T]) Push(v T) {
	r.data[r.head] = v
	r.head = (r.head + 1) % cap(r.data)
	if r.size < cap(r.data) {
		r.size++
	} else {
		r.tail = (r.tail + 1) % cap(r.data)
	}
}

// Pop removes and returns the oldest element, or ErrEmpty if none remain. The
// vacated slot is zeroed so a stored pointer does not stay reachable.
func (r *Ring[T]) Pop() (T, error) {
	var zero T
	if r.size == 0 {
		return zero, ErrEmpty
	}
	v := r.data[r.tail]
	r.data[r.tail] = zero
	r.tail = (r.tail + 1) % cap(r.data)
	r.size--
	return v, nil
}

// Len reports the number of elements currently stored.
func (r *Ring[T]) Len() int { return r.size }

// Cap reports the fixed capacity of the backing array.
func (r *Ring[T]) Cap() int { return cap(r.data) }
```

### The runnable demo

The demo pushes five values into a buffer of capacity three, so the two oldest are
overwritten, then drains it to show the surviving window is `3, 4, 5`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/ring"
)

func main() {
	r := ring.New[int](3)
	for _, v := range []int{1, 2, 3, 4, 5} {
		r.Push(v)
	}
	fmt.Printf("len=%d cap=%d after 5 pushes\n", r.Len(), r.Cap())

	for {
		v, err := r.Pop()
		if errors.Is(err, ring.ErrEmpty) {
			break
		}
		fmt.Printf("popped %d\n", v)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
len=3 cap=3 after 5 pushes
popped 3
popped 4
popped 5
```

### Tests

`TestOverwritesOldestWhenFull` is the core proof: five pushes into capacity three
yield pop order `3, 4, 5`. `TestWraparoundAfterPop` interleaves a pop before more
pushes so `head` and `tail` wrap independently. `TestPopEmpty` asserts the sentinel
via `errors.Is`. `TestCapDoesNotGrow` pushes a hundred times and proves `Cap` is
still three — the fixed-capacity guarantee.

Create `ring_test.go`:

```go
package ring

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func drain[T any](r *Ring[T]) []T {
	var out []T
	for {
		v, err := r.Pop()
		if errors.Is(err, ErrEmpty) {
			return out
		}
		out = append(out, v)
	}
}

func TestPushUntilFull(t *testing.T) {
	t.Parallel()
	r := New[int](3)
	r.Push(1)
	r.Push(2)
	r.Push(3)
	if r.Len() != 3 {
		t.Fatalf("Len = %d, want 3", r.Len())
	}
	if r.Cap() != 3 {
		t.Fatalf("Cap = %d, want 3", r.Cap())
	}
}

func TestOverwritesOldestWhenFull(t *testing.T) {
	t.Parallel()
	r := New[int](3)
	for _, v := range []int{1, 2, 3, 4, 5} {
		r.Push(v)
	}
	if r.Len() != 3 {
		t.Fatalf("Len = %d, want 3", r.Len())
	}
	got := drain(r)
	want := []int{3, 4, 5}
	if !slices.Equal(got, want) {
		t.Fatalf("pop order = %v, want %v", got, want)
	}
}

func TestPopEmpty(t *testing.T) {
	t.Parallel()
	r := New[int](3)
	if _, err := r.Pop(); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
}

func TestWraparoundAfterPop(t *testing.T) {
	t.Parallel()
	r := New[int](3)
	r.Push(1)
	r.Push(2)
	r.Push(3)

	if v, _ := r.Pop(); v != 1 {
		t.Fatalf("first Pop = %d, want 1", v)
	}
	r.Push(4)
	r.Push(5)

	got := drain(r)
	want := []int{3, 4, 5}
	if !slices.Equal(got, want) {
		t.Fatalf("pop order = %v, want %v", got, want)
	}
}

func TestPartialFillPreservesOrder(t *testing.T) {
	t.Parallel()
	r := New[int](5)
	for _, v := range []int{1, 2, 3, 4} {
		r.Push(v)
	}
	got := drain(r)
	want := []int{1, 2, 3, 4}
	if !slices.Equal(got, want) {
		t.Fatalf("pop order = %v, want %v", got, want)
	}
}

func TestCapDoesNotGrow(t *testing.T) {
	t.Parallel()
	r := New[int](3)
	for i := range 100 {
		r.Push(i)
	}
	if r.Cap() != 3 {
		t.Fatalf("Cap = %d, want 3 (ring must never grow)", r.Cap())
	}
	if r.Len() != 3 {
		t.Fatalf("Len = %d, want 3", r.Len())
	}
}

func Example() {
	r := New[string](2)
	r.Push("a")
	r.Push("b")
	r.Push("c") // overwrites "a"
	v, _ := r.Pop()
	fmt.Println(v)
	// Output: b
}
```

## Review

The ring is correct when logical size is tracked independently of the backing
array's length and `append` never appears in `Push` or `Pop`. The two failure
modes this design forecloses are the ones the concepts file names: using
`len(r.data)` as the logical size (it is always `cap` and tells you nothing after
the array fills), and reaching for `r.data = append(r.data, v)` when full (which
would reallocate and break the capacity bound). `TestOverwritesOldestWhenFull`
pins the overwrite semantics and `TestCapDoesNotGrow` pins the no-growth
guarantee; if either fails, one of those two mistakes has crept in. Run
`go test -race` to confirm — this type is not internally synchronized, so the race
test documents that concurrent use needs an external lock.

## Resources

- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices)
- [Go Specification: Slice types](https://go.dev/ref/spec#Slice_types)
- [`slices.Equal`](https://pkg.go.dev/slices#Equal)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-preallocate-repository-result-slice.md](02-preallocate-repository-result-slice.md)
