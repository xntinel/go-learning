# Exercise 1: Bounded Window Queue That Owns Its Data

A ring-indexed queue of fixed capacity that keeps the most recent sliding windows
for a metrics or anomaly-detection pipeline. The contract that makes it safe under
a concurrent producer is that the queue owns its storage: `Push` deep-copies the
caller's window, so the producer can keep reusing and mutating its own buffer
without ever touching what the queue holds.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
windowq/                        independent module: example.com/windowq
  go.mod                        go 1.26
  internal/windowq/windowq.go   type Windowq; New, Push (copy on push), Pop, Len, Cap
  cmd/
    demo/
      main.go                   producer reuses one buffer; queue keeps distinct copies
  internal/windowq/windowq_test.go   order, ErrTooFull, ErrEmpty, independent-copy, empty-window
```

Files: `internal/windowq/windowq.go`, `cmd/demo/main.go`, `internal/windowq/windowq_test.go`.
Implement: a fixed-capacity queue whose `Push` copies the input window with `slices.Clone(window)` and whose `Pop` returns the stored slice and clears the slot.
Test: push/pop order, `ErrTooFull` at capacity, `ErrEmpty` on empty, an independent-copy test that mutates a popped window and proves storage is untouched, and an empty-but-not-nil round-trip.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/windowq/internal/windowq ~/go-exercises/windowq/cmd/demo
cd ~/go-exercises/windowq
go mod init example.com/windowq
```

### Why the queue must copy on push

The producer in a real pipeline computes a sliding window into a buffer it owns
and reuses: it fills `buf`, hands it off, then overwrites `buf` for the next
window. If `Push` stored the caller's slice directly (`q.data[i] = window`), every
slot in the queue would alias that one reused buffer, and by the time you `Pop`
them they would all show the producer's *latest* contents — the classic
shared-backing-array corruption. Copying on push with `slices.Clone(window)`
(equivalently `append([]int(nil), window...)` for a non-empty input) gives the
queue a fresh backing array per window, severing it from the producer's buffer.
That is the ownership boundary: data crossing from producer into the queue is
copied, so the two sides can never mutate each other.

`slices.Clone` is preferred over the `append([]int(nil), …)` form here for a
second reason: it preserves the *shape* of the input. `append([]int(nil), empty...)`
of a non-nil empty window returns `nil`, collapsing an empty-but-present window
into a nil one; `slices.Clone` of a non-nil empty slice returns a non-nil empty
slice, so the queue round-trips an empty window faithfully.

The queue is a ring: `data` is a fixed-size `[][]int`, `head` is the logical index
of the oldest window, and `size` is how many are stored. `Push` writes at
`(head+size) % cap`, `Pop` reads at `head` and advances it. `Pop` also nils the
slot it vacated, both to drop the queue's reference to that backing array (so it
can be collected) and to keep the ring's dead slots clean.

Create `internal/windowq/windowq.go`:

```go
package windowq

import (
	"errors"
	"slices"
)

// ErrEmpty is returned by Pop when the queue holds no windows.
var ErrEmpty = errors.New("queue is empty")

// ErrTooFull is returned by Push when the queue is at capacity.
var ErrTooFull = errors.New("queue is full")

// Windowq is a fixed-capacity ring of integer windows. It owns its storage:
// Push copies the caller's window so the producer cannot mutate queue state.
type Windowq struct {
	data [][]int
	head int
	size int
}

// New returns a queue that holds at most capacity windows. A non-positive
// capacity is clamped to 1.
func New(capacity int) *Windowq {
	if capacity <= 0 {
		capacity = 1
	}
	return &Windowq{data: make([][]int, capacity)}
}

// Push copies window into the queue. It returns ErrTooFull at capacity. The
// stored slice has its own backing array, independent of the caller's.
func (q *Windowq) Push(window []int) error {
	if q.size == cap(q.data) {
		return ErrTooFull
	}
	copied := slices.Clone(window)
	q.data[(q.head+q.size)%cap(q.data)] = copied
	q.size++
	return nil
}

// Pop removes and returns the oldest window. It returns ErrEmpty when empty.
func (q *Windowq) Pop() ([]int, error) {
	if q.size == 0 {
		return nil, ErrEmpty
	}
	w := q.data[q.head]
	q.data[q.head] = nil
	q.head = (q.head + 1) % cap(q.data)
	q.size--
	return w, nil
}

// Len reports the number of stored windows.
func (q *Windowq) Len() int { return q.size }

// Cap reports the fixed capacity.
func (q *Windowq) Cap() int { return cap(q.data) }
```

### The runnable demo

The demo makes the ownership boundary visible. A producer reuses a single buffer:
it fills it with `[10 20 30]`, pushes, then overwrites the *same* buffer with
`[40 50 60]` and pushes again. Because `Push` copied each time, the popped windows
are the two distinct snapshots, not two views of the final buffer contents.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/windowq/internal/windowq"
)

func main() {
	q := windowq.New(3)

	buf := make([]int, 3)

	copy(buf, []int{10, 20, 30})
	_ = q.Push(buf)

	// Producer reuses the SAME buffer for the next window.
	copy(buf, []int{40, 50, 60})
	_ = q.Push(buf)

	// Mutate the buffer once more after both pushes.
	copy(buf, []int{0, 0, 0})

	for q.Len() > 0 {
		w, _ := q.Pop()
		fmt.Println(w)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[10 20 30]
[40 50 60]
```

### Tests

`TestPopReturnsIndependentCopy` is the lesson's key test: mutate a popped window
and prove the queue's storage is unaffected. `TestQueueHandlesEmptyWindows` pins
that an empty window round-trips as `len == 0` and non-nil (`append([]int(nil))`
of an empty slice yields an empty, non-nil slice). The order, `ErrTooFull`, and
`ErrEmpty` tests pin the ring mechanics and the sentinel errors, asserted with
`errors.Is`.

Create `internal/windowq/windowq_test.go`:

```go
package windowq

import (
	"errors"
	"testing"
)

func TestPushAndPop(t *testing.T) {
	t.Parallel()

	q := New(3)
	if err := q.Push([]int{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	got, err := q.Pop()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("Pop = %v, want [1 2 3]", got)
	}
}

func TestPushAndPopInOrder(t *testing.T) {
	t.Parallel()

	q := New(3)
	for i := 1; i <= 3; i++ {
		if err := q.Push([]int{i, i * 10, i * 100}); err != nil {
			t.Fatal(err)
		}
	}

	want := [][]int{
		{1, 10, 100},
		{2, 20, 200},
		{3, 30, 300},
	}
	for _, w := range want {
		got, err := q.Pop()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(w) {
			t.Fatalf("Pop len = %d, want %d", len(got), len(w))
		}
		for i, v := range w {
			if got[i] != v {
				t.Fatalf("Pop[%d] = %d, want %d", i, got[i], v)
			}
		}
	}
}

func TestPopReturnsIndependentCopy(t *testing.T) {
	t.Parallel()

	q := New(3)
	src := []int{1, 2, 3}
	if err := q.Push(src); err != nil {
		t.Fatal(err)
	}

	// Mutating the source after Push must not reach queue storage.
	src[0] = -1

	got, err := q.Pop()
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 1 {
		t.Fatalf("Push did not copy: Pop[0] = %d, want 1", got[0])
	}

	// Mutating the popped window must not corrupt any remaining storage.
	got[0] = 99
	if _, err := q.Pop(); !errors.Is(err, ErrEmpty) {
		t.Fatalf("expected empty queue after single pop, err = %v", err)
	}
}

func TestPushReturnsErrTooFullWhenFull(t *testing.T) {
	t.Parallel()

	q := New(2)
	if err := q.Push([]int{1}); err != nil {
		t.Fatal(err)
	}
	if err := q.Push([]int{2}); err != nil {
		t.Fatal(err)
	}
	if err := q.Push([]int{3}); !errors.Is(err, ErrTooFull) {
		t.Fatalf("err = %v, want ErrTooFull", err)
	}
}

func TestPopRejectsEmptyQueue(t *testing.T) {
	t.Parallel()

	q := New(3)
	if _, err := q.Pop(); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
}

func TestQueueHandlesEmptyWindows(t *testing.T) {
	t.Parallel()

	q := New(2)
	if err := q.Push([]int{}); err != nil {
		t.Fatal(err)
	}
	got, err := q.Pop()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty window round-trip len = %d, want 0", len(got))
	}
	if got == nil {
		t.Fatal("empty window round-trip returned nil, want empty non-nil slice")
	}
}
```

## Review

The queue is correct when its storage is provably disjoint from the producer's:
`TestPopReturnsIndependentCopy` mutates the source both before and after the copy
crosses the boundary and sees the stored window unchanged. If that test regresses,
`Push` is storing the caller's slice by reference — the single most common
ownership bug in Go. The empty-window test guards a subtle secondary contract:
`slices.Clone` of a non-nil empty slice yields a non-nil zero-length slice, so the
queue preserves the *shape* of an empty window rather than collapsing it to `nil`
(which the `append([]int(nil), empty...)` form would do). The
`ErrTooFull`/`ErrEmpty` tests use `errors.Is` so the sentinels can later be
wrapped without breaking callers. Run `go test -race` to confirm nothing shares
state under the race detector.

## Resources

- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices)
- [slices package (`Clone`)](https://pkg.go.dev/slices#Clone)
- [`errors.Is`](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-repository-getall-defensive-clone.md](02-repository-getall-defensive-clone.md)
