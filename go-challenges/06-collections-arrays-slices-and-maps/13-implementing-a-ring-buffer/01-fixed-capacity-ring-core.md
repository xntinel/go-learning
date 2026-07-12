# Exercise 1: Implement the Fixed-Capacity Generic Ring[T]

Every bounded backend component in the rest of this lesson is built on one type:
a generic ring buffer over a fixed backing array. This module builds that core —
`Push` (overwrite-oldest when full), `Pop` (zeroing the evicted slot), `Snapshot`
(a fresh independent copy), `Len`, and `Cap` — with the head/tail/size wraparound
math as the load-bearing detail.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
ringcore/                  independent module: example.com/ringcore
  go.mod                   go 1.24
  ring.go                  type Ring[T]; New, Push, Pop, Snapshot, Len, Cap
  cmd/
    demo/
      main.go              push past capacity, snapshot, drain
  ring_test.go             FIFO order, eviction, ErrEmpty, zeroed-slot, invariants
```

Files: `ring.go`, `cmd/demo/main.go`, `ring_test.go`.
Implement: `New[T](capacity int)` clamping non-positive capacity to 1; `Push`, `Pop`, `Snapshot`, `Len`, `Cap`.
Test: FIFO push/pop order, overwrite-oldest eviction, `ErrEmpty` via `errors.Is`, the evicted slot is zeroed, `Len`/`Cap` invariants.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/01-fixed-capacity-ring-core/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/01-fixed-capacity-ring-core
go mod edit -go=1.24
```

### The state and its invariants

The buffer is a slice `data` of fixed length `cap`, plus three integers. `head` is
the next write index, `tail` the next read index, and `size` the number of live
elements. The invariants that must hold after every operation are: `0 <= size <=
cap`, `head == (tail + size) % cap`, and the logical FIFO order is `data[tail]`,
`data[(tail+1)%cap]`, ... for `size` steps. `Len` returns `size`; `Cap` returns
`cap`. Empty is `size == 0`; full is `size == cap`. Note we never use `head ==
tail` to decide empty-versus-full — that equality holds in both states, which is
the canonical ring bug.

### Push: write, wrap, and evict on the correct branch

`Push` always writes the new value at `head` and advances `head` modulo `cap`.
Then exactly one of two branches runs. If `size < cap` there was room, so we
increment `size`. Otherwise the buffer was full: the write at `head` just
overwrote the oldest element (because when full, `head == tail`), so we advance
`tail` past it to keep the logical order correct, and `size` stays at `cap`. Getting
this branch wrong — advancing `tail` when there was room, or incrementing `size`
past `cap` — corrupts FIFO order after the first wrap.

### Pop: read, zero the slot, advance

`Pop` returns `ErrEmpty` when `size == 0`. Otherwise it reads `data[tail]`, then
writes the zero value back into that slot before advancing. That zeroing is not
cosmetic: for a `Ring[*Request]` or `Ring[[]byte]` the backing array would
otherwise keep the popped referent reachable for the buffer's whole lifetime,
leaking heap memory bounded by capacity. Writing `var zero T` drops the reference.
Then `tail` advances modulo `cap` and `size` decrements.

### Snapshot: copy the logical range, never alias it

`Snapshot` allocates a fresh slice of length `size` and copies element `i` from
`data[(tail+i) % cap]`. The result is independent of internal storage: the caller
can mutate, sort, marshal, or outlive it without touching or racing the buffer.
Returning a sub-slice of `data` instead would pin the backing array and alias the
live cells — the mistake the later `/debug` and percentile modules must avoid.

Create `ring.go`:

```go
package ring

import "errors"

// ErrEmpty is returned by Pop when the buffer holds no elements.
var ErrEmpty = errors.New("ring: buffer is empty")

// Ring is a fixed-capacity FIFO buffer over a backing array. When full, Push
// overwrites the oldest element (drop-front). It is not safe for concurrent use;
// wrap it in a mutex for shared access.
type Ring[T any] struct {
	data []T
	head int // next write index
	tail int // next read index
	size int // number of live elements
}

// New returns a Ring with the given capacity. A non-positive capacity is clamped
// to 1, so the buffer always holds at least one element.
func New[T any](capacity int) *Ring[T] {
	if capacity <= 0 {
		capacity = 1
	}
	return &Ring[T]{data: make([]T, capacity)}
}

// Push appends v. When the buffer is full it overwrites the oldest element and
// advances the tail, keeping bounded memory under unbounded input.
func (r *Ring[T]) Push(v T) {
	r.data[r.head] = v
	r.head = (r.head + 1) % len(r.data)
	if r.size < len(r.data) {
		r.size++
	} else {
		r.tail = (r.tail + 1) % len(r.data)
	}
}

// Pop removes and returns the oldest element, or ErrEmpty if the buffer is empty.
// It zeroes the evicted slot so a Ring of pointers does not pin the referent.
func (r *Ring[T]) Pop() (T, error) {
	var zero T
	if r.size == 0 {
		return zero, ErrEmpty
	}
	v := r.data[r.tail]
	r.data[r.tail] = zero
	r.tail = (r.tail + 1) % len(r.data)
	r.size--
	return v, nil
}

// Snapshot returns a fresh copy of the live elements in FIFO order. The result is
// independent of the buffer's internal storage.
func (r *Ring[T]) Snapshot() []T {
	out := make([]T, r.size)
	for i := range r.size {
		out[i] = r.data[(r.tail+i)%len(r.data)]
	}
	return out
}

// Len reports the number of live elements.
func (r *Ring[T]) Len() int { return r.size }

// Cap reports the fixed capacity.
func (r *Ring[T]) Cap() int { return len(r.data) }
```

### The runnable demo

The demo pushes five values into a capacity-3 ring so the first two are evicted,
snapshots the survivors, then drains with `Pop` until `ErrEmpty`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/ringcore"
)

func main() {
	r := ring.New[int](3)
	for i := 1; i <= 5; i++ {
		r.Push(i)
	}
	fmt.Printf("cap=%d len=%d snapshot=%v\n", r.Cap(), r.Len(), r.Snapshot())

	for {
		v, err := r.Pop()
		if errors.Is(err, ring.ErrEmpty) {
			break
		}
		fmt.Printf("pop %d\n", v)
	}
	fmt.Printf("drained len=%d\n", r.Len())
}
```

The module path is `example.com/ringcore` but the package is `ring`, so the import
is `example.com/ringcore` and the qualifier is `ring`.

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cap=3 len=3 snapshot=[3 4 5]
pop 3
pop 4
pop 5
drained len=0
```

The first line reports the state after five pushes into a cap-3 ring: values 1 and
2 were evicted, leaving `{3, 4, 5}`. The drain loop then pops those three in FIFO
order until `Pop` returns `ErrEmpty`, so the final `Len` is 0.

### Tests

The tests pin the contract: FIFO order out, eviction of the oldest when full,
`ErrEmpty` via `errors.Is`, the invariant that `Cap` never changes and `Len` never
exceeds it, and the memory-safety detail that a popped slot is zeroed. The zeroed-
slot test uses a `Ring[*int]` and reaches into the unexported `data` field (same-
package test) to prove the pointer was cleared.

Create `ring_test.go`:

```go
package ring

import (
	"errors"
	"slices"
	"testing"
)

func TestPushPopFIFO(t *testing.T) {
	t.Parallel()
	r := New[int](4)
	for i := 1; i <= 3; i++ {
		r.Push(i)
	}
	for _, want := range []int{1, 2, 3} {
		v, err := r.Pop()
		if err != nil {
			t.Fatalf("Pop: unexpected error %v", err)
		}
		if v != want {
			t.Fatalf("Pop = %d, want %d", v, want)
		}
	}
}

func TestOverwriteOldestWhenFull(t *testing.T) {
	t.Parallel()
	r := New[int](3)
	for i := 1; i <= 5; i++ {
		r.Push(i)
	}
	got := r.Snapshot()
	want := []int{3, 4, 5}
	if !slices.Equal(got, want) {
		t.Fatalf("Snapshot = %v, want %v", got, want)
	}
	if r.Len() != 3 || r.Cap() != 3 {
		t.Fatalf("Len=%d Cap=%d, want 3 and 3", r.Len(), r.Cap())
	}
}

func TestPopEmptyReturnsErrEmpty(t *testing.T) {
	t.Parallel()
	r := New[int](2)
	if _, err := r.Pop(); !errors.Is(err, ErrEmpty) {
		t.Fatalf("Pop on empty: err = %v, want ErrEmpty", err)
	}
}

func TestPopZeroesEvictedSlot(t *testing.T) {
	t.Parallel()
	r := New[*int](2)
	x := 7
	r.Push(&x)
	if _, err := r.Pop(); err != nil {
		t.Fatalf("Pop: %v", err)
	}
	// The popped slot must hold nil, or a Ring[*T] would pin the referent.
	for i, p := range r.data {
		if p != nil {
			t.Fatalf("data[%d] = %v after Pop, want nil (slot not zeroed)", i, p)
		}
	}
}

func TestCapClampAndNeverGrows(t *testing.T) {
	t.Parallel()
	r := New[int](0) // clamped to 1
	if r.Cap() != 1 {
		t.Fatalf("New(0).Cap() = %d, want 1 (clamped)", r.Cap())
	}
	for i := range 1000 {
		r.Push(i)
		if r.Len() > r.Cap() {
			t.Fatalf("Len %d exceeded Cap %d", r.Len(), r.Cap())
		}
	}
	if r.Cap() != 1 || r.Len() != 1 {
		t.Fatalf("after 1000 pushes: Cap=%d Len=%d, want 1 and 1", r.Cap(), r.Len())
	}
}
```

## Review

The core is correct when the three invariants hold after every operation:
`0 <= size <= cap`, `head == (tail + size) % cap`, and FIFO order preserved across
at least one wrap. `TestOverwriteOldestWhenFull` proves the wrap: pushing 1..5 into
a cap-3 ring must leave exactly `{3, 4, 5}`, which only happens if the eviction
branch advances `tail` and never lets `size` exceed `cap`. `TestPopZeroesEvictedSlot`
is the one that catches a real production leak — it fails if `Pop` advances without
clearing the slot. The two traps to avoid are deciding empty-versus-full from
`head == tail` (ambiguous — use `size`) and returning a sub-slice from `Snapshot`
(aliases the live array — allocate and copy). Run `go test -race` even though this
type is single-goroutine; it costs nothing and catches an accidental shared write.

## Resources

- [Go generics tutorial](https://go.dev/doc/tutorial/generics) — type parameters, exactly what `Ring[T]` uses.
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices) — how slices back a fixed array and why aliasing matters.
- [The Go Memory Model](https://go.dev/ref/mem) — why a shared ring needs synchronization (background for the later mutex module).

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-eviction-and-snapshot-contract-tests.md](02-eviction-and-snapshot-contract-tests.md)
