# Exercise 10: Expose the Ring as an iter.Seq[T] and Benchmark It vs a Channel

`Snapshot` allocates a slice on every read, which is wasteful when a caller only
wants to walk the contents once. Go 1.23's range-over-func lets the ring expose an
`iter.Seq[T]` that yields its elements in place, with no intermediate allocation and
correct early-`break` handling. This final module adds `All()`, proves it is
allocation-free, and benchmarks the ring against a buffered channel of equal capacity
so you can see when a hand-rolled ring is worth it and when a channel is the right
call.

Self-contained: its own module, the ring with `All()`, a demo, tests, and benchmarks.

## What you'll build

```text
ringiter/                  independent module: example.com/ringiter
  go.mod                   go 1.24
  ringiter.go              Ring[T] with All() iter.Seq[T]
  cmd/
    demo/
      main.go              range over All(), early break
  ringiter_test.go         FIFO via All(), early-break, zero-alloc, ring-vs-channel bench
```

Files: `ringiter.go`, `cmd/demo/main.go`, `ringiter_test.go`.
Implement: `Ring[T]` with `Push`/`Pop` and `All() iter.Seq[T]` yielding tail-to-head, stopping when `yield` returns false.
Test: range over `All()` visits FIFO order; early break stops iteration; `All()` heap-allocates zero times (`testing.AllocsPerRun`); a benchmark comparing ring vs buffered channel.
Verify: `go test -count=1 -race ./...` and `go test -bench=. -benchmem`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/10-range-over-func-iterator-and-benchmark/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/13-implementing-a-ring-buffer/10-range-over-func-iterator-and-benchmark
go mod edit -go=1.24
```

### iter.Seq and the yield protocol

`iter.Seq[T]` is a named type for `func(yield func(T) bool)`. When you write
`for v := range r.All()`, the compiler calls the sequence function, passing it a
`yield` closure it synthesizes from the loop body. For each element, the iterator
calls `yield(element)`. `yield` returns `true` to mean "keep going" and `false` to
mean "the caller is done" — which happens when the loop `break`s, `return`s, or
`panic`s. The iterator's contract is to *stop immediately* when `yield` returns
false: `if !yield(v) { return }`. Forget that check and the iterator keeps running the
loop body's `yield` after the caller has left, which the runtime turns into a panic.

`All()` walks the logical FIFO range — from `tail` for `size` steps, wrapping — and
yields each element. Because it reads the buffer's own storage directly rather than
copying into a new slice, it allocates nothing. That is the whole reason to prefer it
over `Snapshot` on a hot read path.

### Why All() is allocation-free, and the invalidation hazard

The range-over-func machinery is designed so that when the sequence function and the
yield closure do not escape — which they do not in a plain `for v := range r.All()` —
the compiler stack-allocates them. So iterating a ring with `All()` costs zero heap
allocations, unlike `Snapshot` which allocates one slice of length `size`. The test
below pins this with `testing.AllocsPerRun`.

The cost of not copying is the classic iterator-invalidation hazard: the yielded view
aliases live storage, so mutating the ring *during* iteration — a `Push` that
overwrites the slot you are about to yield, a `Pop` that moves `tail` — produces a
torn or stale walk. This mirrors the rule against mutating a map or slice while
ranging it. If you must mutate while reading, take a `Snapshot` first and pay the
allocation; `All()` is for the common read-only walk where you do not.

### When a ring beats a channel, and when it does not

The benchmark contrasts the ring's `Push`/`Pop` against a buffered channel's
send/receive at equal capacity. In a single goroutine, the ring wins: it is a couple
of index updates and array writes with no synchronization, while a channel takes its
internal lock on every operation even with no contention. But the moment you need
*blocking* hand-off between goroutines, a channel gives you that for free (it *is* a
mutex-guarded ring with blocking send/receive), and reimplementing it correctly with a
`sync.Cond` is the previous module's worth of subtle work. The takeaway: reach for a
raw ring when a single goroutine owns the buffer or when you need an operation a
channel lacks (overwrite-oldest, all-contents snapshot, indexed percentile window);
default to a channel for goroutine-to-goroutine communication.

Create `ringiter.go`:

```go
package ringiter

import (
	"errors"
	"iter"
)

// ErrEmpty is returned by Pop when the buffer is empty.
var ErrEmpty = errors.New("ringiter: buffer is empty")

// Ring is a fixed-capacity FIFO buffer that can be ranged as an iter.Seq.
type Ring[T any] struct {
	data []T
	head int
	tail int
	size int
}

// New returns a Ring with the given capacity (clamped to >= 1).
func New[T any](capacity int) *Ring[T] {
	if capacity <= 0 {
		capacity = 1
	}
	return &Ring[T]{data: make([]T, capacity)}
}

// Push adds v, overwriting the oldest element when full.
func (r *Ring[T]) Push(v T) {
	r.data[r.head] = v
	r.head = (r.head + 1) % len(r.data)
	if r.size < len(r.data) {
		r.size++
	} else {
		r.tail = (r.tail + 1) % len(r.data)
	}
}

// Pop removes and returns the oldest element, or ErrEmpty if empty.
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

// Len reports the current element count.
func (r *Ring[T]) Len() int { return r.size }

// All returns an iterator over the elements in FIFO order (tail to head) without
// allocating a snapshot. The iterator stops as soon as yield returns false.
// Do not mutate the ring while ranging over All: the view aliases live storage.
func (r *Ring[T]) All() iter.Seq[T] {
	return func(yield func(T) bool) {
		for i := range r.size {
			if !yield(r.data[(r.tail+i)%len(r.data)]) {
				return
			}
		}
	}
}
```

### The runnable demo

The demo pushes five values into a cap-4 ring (evicting the first), walks all of them
with `All()`, then shows an early `break` that stops after the first two.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ringiter"
)

func main() {
	r := ringiter.New[int](4)
	for i := 1; i <= 5; i++ {
		r.Push(i)
	}

	fmt.Print("all:")
	for v := range r.All() {
		fmt.Printf(" %d", v)
	}
	fmt.Println()

	fmt.Print("first two:")
	seen := 0
	for v := range r.All() {
		fmt.Printf(" %d", v)
		seen++
		if seen == 2 {
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
all: 2 3 4 5
first two: 2 3
```

The first push (1) was evicted by the fifth, so `All()` yields `2 3 4 5` in FIFO
order; the early `break` stops the second walk after `2 3`, exercising the
`yield`-returns-false path.

### Tests and benchmarks

The tests pin FIFO order through `All()`, prove the early-break path stops the
iterator (the loop count matches the break point and the program does not hang), and
assert zero heap allocations via `testing.AllocsPerRun`. The two benchmarks compare
the ring against a buffered channel of equal capacity; the gate runs the correctness
tests under `-race`, and you run the benchmarks separately with `-bench`.

Create `ringiter_test.go`:

```go
package ringiter

import (
	"slices"
	"testing"
)

func TestAllYieldsFIFO(t *testing.T) {
	t.Parallel()
	r := New[int](4)
	r.Push(10)
	r.Push(20)
	r.Push(30)
	var got []int
	for v := range r.All() {
		got = append(got, v)
	}
	if want := []int{10, 20, 30}; !slices.Equal(got, want) {
		t.Fatalf("All() = %v, want %v", got, want)
	}
}

func TestAllEarlyBreakStops(t *testing.T) {
	t.Parallel()
	r := New[int](8)
	for i := range 8 {
		r.Push(i)
	}
	visited := 0
	for range r.All() {
		visited++
		if visited == 3 {
			break
		}
	}
	if visited != 3 {
		t.Fatalf("visited %d elements, want 3 (early break not honored)", visited)
	}
}

func TestAllIsAllocationFree(t *testing.T) {
	// AllocsPerRun must not run under t.Parallel().
	r := New[int](128)
	for i := range 128 {
		r.Push(i)
	}
	allocs := testing.AllocsPerRun(100, func() {
		sum := 0
		for v := range r.All() {
			sum += v
		}
		sink = sum
	})
	if allocs != 0 {
		t.Fatalf("All() allocated %v times per run, want 0", allocs)
	}
}

var sink int

func BenchmarkRingPushPop(b *testing.B) {
	r := New[int](1024)
	b.ReportAllocs()
	for b.Loop() {
		r.Push(1)
		if _, err := r.Pop(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkChannelSendRecv(b *testing.B) {
	ch := make(chan int, 1024)
	b.ReportAllocs()
	for b.Loop() {
		ch <- 1
		<-ch
	}
}
```

Run the benchmarks:

```bash
go test -bench=. -benchmem
```

You will see the ring's `Push`/`Pop` run several times faster per operation than the
channel's send/receive and allocate nothing, because the channel takes its internal
lock on every operation while the single-goroutine ring takes none. That gap is the
argument for a raw ring in single-owner hot paths — and the reason to still prefer a
channel the moment you need blocking cross-goroutine hand-off.

## Review

The iterator is correct when `All()` yields the buffer in FIFO order, stops the
instant `yield` returns false, and allocates nothing. `TestAllYieldsFIFO` and
`TestAllEarlyBreakStops` pin the order and the break contract; `TestAllIsAllocationFree`
proves the zero-allocation property that justifies `All()` over `Snapshot`. The two
traps are forgetting the `if !yield(v) { return }` guard (the iterator runs past the
caller's `break` and panics) and mutating the ring during iteration (the aliased view
tears — snapshot first if you must mutate). Treat the benchmark result as guidance,
not dogma: a channel is a mutex-guarded ring with blocking built in and is the right
default; drop to a raw ring only when a profile or a missing operation (overwrite,
snapshot, indexed window) justifies it.

## Resources

- [`iter` package](https://pkg.go.dev/iter) — `iter.Seq`, `iter.Seq2`, and the yield protocol.
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions) — how range-over-func works and why it is allocation-free.
- [`testing` package: B.Loop and AllocsPerRun](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop and allocation measurement.

---

Back to [09-drop-counter-backpressure-metrics.md](09-drop-counter-backpressure-metrics.md) | Next: [11-power-of-two-reorder-buffer.md](11-power-of-two-reorder-buffer.md)
