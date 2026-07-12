# Exercise 2: Pin the FIFO-Eviction and Snapshot-Isolation Contracts with Tests

A ring buffer's value is its contract, and a contract that is not pinned by a test
rots the first time someone "optimizes" the wraparound math. This module is the
test artifact: a behavioral suite that locks down FIFO order, overwrite-oldest
eviction, `ErrEmpty` via `errors.Is`, snapshot isolation, generic reuse over
`string`, and bounded growth under a flood of pushes.

The module is self-contained: it ships a compact copy of the `Ring[T]` from
Exercise 1 so the tests have something to bind to, then the suite that verifies it.

## What you'll build

```text
ringcontract/              independent module: example.com/ringcontract
  go.mod                   go 1.24
  ring.go                  the Ring[T] under test (New, Push, Pop, Snapshot, Len, Cap)
  cmd/
    demo/
      main.go              runs the eviction + snapshot-isolation scenarios
  ring_test.go             the contract suite (this module IS the test artifact)
```

Files: `ring.go`, `cmd/demo/main.go`, `ring_test.go`.
Implement: the `Ring[T]` copy, then the full behavioral suite.
Test: FIFO order, overwrite-oldest, `ErrEmpty` via `errors.Is`, snapshot-mutation isolation, generic-over-string, `TestRingCapDoesNotGrow`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/02-eviction-and-snapshot-contract-tests/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/02-eviction-and-snapshot-contract-tests
go mod edit -go=1.24
```

### What each contract test proves, and why it is the right assertion

The suite is the deliverable, so the design of each test matters as much as the
code under test.

FIFO order (`TestFIFOOrder`): push a known sequence, pop it all, assert the pop
order equals the push order. This is the base guarantee; if it fails, wraparound
math or the `size` accounting is broken.

Overwrite-oldest (`TestOverwriteOldest`): push 5 into a cap-3 ring, snapshot, and
assert exactly `{3, 4, 5}`. This is the single most important test — it proves the
eviction *policy* (drop-front) and the eviction *mechanism* (advancing `tail` on
the full branch) at once. A subtle off-by-one that only bites after the first wrap
shows up here and nowhere else.

`ErrEmpty` via `errors.Is` (`TestErrEmptyIsWrapped`): assert with
`errors.Is(err, ErrEmpty)`, not `err == ErrEmpty`. Using `errors.Is` is the
correct idiom even for an un-wrapped sentinel, because it keeps working if a caller
later wraps the error with `%w`. A test that uses `==` silently breaks under
wrapping; a test that uses `errors.Is` does not.

Snapshot isolation (`TestSnapshotIsolation`): take a snapshot, mutate the returned
slice, then `Pop` and assert the popped value is unchanged. This proves the
snapshot is a *copy*, not a view aliasing the backing array. It is the exact test
that fails if someone changes `Snapshot` to `return r.data[r.tail:]`.

Generic reuse (`TestGenericOverString`): the same buffer over `string`, proving the
type parameter is not accidentally specialized to `int`.

Bounded growth (`TestRingCapDoesNotGrow`): push 1000 into a cap-3 ring and assert
`Cap() == 3 && Len() == 3` throughout. This pins the property that makes a ring
safe in a long-running server: bounded memory under unbounded input.

Create `ring.go` (the type under test — the same core as Exercise 1):

```go
package ring

import "errors"

// ErrEmpty is returned by Pop when the buffer holds no elements.
var ErrEmpty = errors.New("ring: buffer is empty")

// Ring is a fixed-capacity FIFO buffer; when full, Push overwrites the oldest.
type Ring[T any] struct {
	data []T
	head int
	tail int
	size int
}

func New[T any](capacity int) *Ring[T] {
	if capacity <= 0 {
		capacity = 1
	}
	return &Ring[T]{data: make([]T, capacity)}
}

func (r *Ring[T]) Push(v T) {
	r.data[r.head] = v
	r.head = (r.head + 1) % len(r.data)
	if r.size < len(r.data) {
		r.size++
	} else {
		r.tail = (r.tail + 1) % len(r.data)
	}
}

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

func (r *Ring[T]) Snapshot() []T {
	out := make([]T, r.size)
	for i := range r.size {
		out[i] = r.data[(r.tail+i)%len(r.data)]
	}
	return out
}

func (r *Ring[T]) Len() int { return r.size }
func (r *Ring[T]) Cap() int { return len(r.data) }
```

### The runnable demo

The demo runs the two headline scenarios in prose form so you can watch them: the
eviction that leaves `{3, 4, 5}`, and the snapshot that stays `{3, 4, 5}` even after
you scribble on the returned slice.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ringcontract"
)

func main() {
	r := ring.New[int](3)
	for i := 1; i <= 5; i++ {
		r.Push(i)
	}
	snap := r.Snapshot()
	fmt.Printf("after 1..5 into cap-3: %v\n", snap)

	snap[0] = 999 // mutate the caller's copy
	fmt.Printf("mutated snapshot:      %v\n", snap)
	fmt.Printf("ring still holds:      %v\n", r.Snapshot())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after 1..5 into cap-3: [3 4 5]
mutated snapshot:      [999 4 5]
ring still holds:      [3 4 5]
```

The third line proves isolation: scribbling `999` into the caller's snapshot leaves
the ring's own contents intact.

### The contract suite

Create `ring_test.go`:

```go
package ring

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestFIFOOrder(t *testing.T) {
	t.Parallel()
	r := New[int](8)
	in := []int{10, 20, 30, 40, 50}
	for _, v := range in {
		r.Push(v)
	}
	var out []int
	for {
		v, err := r.Pop()
		if errors.Is(err, ErrEmpty) {
			break
		}
		out = append(out, v)
	}
	if !slices.Equal(out, in) {
		t.Fatalf("pop order = %v, want %v", out, in)
	}
}

func TestOverwriteOldest(t *testing.T) {
	t.Parallel()
	r := New[int](3)
	for i := 1; i <= 5; i++ {
		r.Push(i)
	}
	got := r.Snapshot()
	want := []int{3, 4, 5}
	if !slices.Equal(got, want) {
		t.Fatalf("Snapshot = %v, want %v (eviction policy wrong)", got, want)
	}
}

func TestErrEmptyIsWrapped(t *testing.T) {
	t.Parallel()
	r := New[int](2)
	_, err := r.Pop()
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("Pop on empty: err = %v, want errors.Is(err, ErrEmpty)", err)
	}
	// errors.Is keeps working even if a caller wraps the sentinel with %w.
	wrapped := fmt.Errorf("dequeue failed: %w", err)
	if !errors.Is(wrapped, ErrEmpty) {
		t.Fatalf("wrapped err no longer Is ErrEmpty")
	}
}

func TestSnapshotIsolation(t *testing.T) {
	t.Parallel()
	r := New[int](3)
	r.Push(1)
	r.Push(2)
	r.Push(3)
	snap := r.Snapshot()
	snap[0] = 99 // must not touch internal storage

	v, err := r.Pop()
	if err != nil {
		t.Fatalf("Pop: %v", err)
	}
	if v != 1 {
		t.Fatalf("Pop = %d, want 1 (snapshot aliased the ring)", v)
	}
}

func TestGenericOverString(t *testing.T) {
	t.Parallel()
	r := New[string](2)
	r.Push("hello")
	r.Push("world")
	r.Push("!") // evicts "hello"
	got := r.Snapshot()
	want := []string{"world", "!"}
	if !slices.Equal(got, want) {
		t.Fatalf("Snapshot = %v, want %v", got, want)
	}
}

func TestRingCapDoesNotGrow(t *testing.T) {
	t.Parallel()
	r := New[int](3)
	for i := range 1000 {
		r.Push(i)
		if r.Cap() != 3 {
			t.Fatalf("Cap grew to %d at push %d", r.Cap(), i)
		}
		if r.Len() > 3 {
			t.Fatalf("Len %d exceeded Cap 3 at push %d", r.Len(), i)
		}
	}
	if r.Cap() != 3 || r.Len() != 3 {
		t.Fatalf("after 1000 pushes: Cap=%d Len=%d, want 3 and 3", r.Cap(), r.Len())
	}
}
```

## Review

The suite is complete when it pins every clause of the contract: order out equals
order in, the full buffer keeps the freshest `cap` elements, an empty `Pop` reports
`ErrEmpty` through `errors.Is` (so it survives wrapping), a mutated snapshot cannot
reach back into the buffer, the type is truly generic, and capacity never grows.
`TestOverwriteOldest` and `TestSnapshotIsolation` are the two that catch the
highest-value regressions — an eviction off-by-one and an aliasing `Snapshot`
respectively. Prefer `errors.Is` over `==` for sentinels as a matter of habit, run
the whole suite under `-race`, and treat `TestRingCapDoesNotGrow` as the guardrail
that keeps a "small tweak" from quietly turning the ring into an unbounded queue.

## Resources

- [`errors` package](https://pkg.go.dev/errors) — `errors.Is` and error wrapping with `%w`.
- [`slices` package](https://pkg.go.dev/slices) — `slices.Equal` for comparing result slices in tests.
- [Go blog: Table-driven tests / testing techniques](https://go.dev/wiki/TableDrivenTests) — behavioral test structure.

---

Back to [01-fixed-capacity-ring-core.md](01-fixed-capacity-ring-core.md) | Next: [03-recent-log-ring-debug-endpoint.md](03-recent-log-ring-debug-endpoint.md)
