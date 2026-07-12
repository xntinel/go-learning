# Exercise 1: Bounded Event History as a Ring-Buffer-Behind-a-Slice

Every long-lived service wants to keep the last N events — recent errors, the
tail of an audit log, the last few health probes — in memory for a debug
endpoint, without growing without bound. The right shape is a fixed-capacity
ring buffer hidden behind a slice: `Add` overwrites the oldest entry, and
`Snapshot` returns a fresh copy so a caller reading `/debug/events` can never
mutate your storage.

This module is fully self-contained. It has its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
eventhistory/               independent module: example.com/eventhistory
  go.mod
  history.go                type History; New, Add, Snapshot, Len, Cap (fixed backing array)
  cmd/
    demo/
      main.go               runnable demo: fill, overflow, snapshot
  history_test.go           in-order, overwrite-oldest, independent-copy, empty, wraparound, never-grows
```

Files: `history.go`, `cmd/demo/main.go`, `history_test.go`.
Implement: a fixed-capacity `History` with `New(capacity)`, `Add(v int)`, `Snapshot() []int`, `Len() int`, `Cap() int`, backed by one array that never grows.
Test: add-and-snapshot-in-order, overwrite-oldest-when-full, snapshot-returns-independent-copy, empty-snapshot, wraparound-at-boundary, never-grows-beyond-capacity.
Verify: `go test -count=1 -race ./...`

### Why a ring buffer behind a slice

The naive "keep the last N" implementation appends to a slice and trims the
front: `h.data = append(h.data, v); if len(h.data) > n { h.data = h.data[1:] }`.
That is wrong on two counts. `append` reallocates when the slice is full, so the
buffer is not actually bounded in allocation behavior, and `h.data = h.data[1:]`
walks the pointer forward through an ever-growing backing array — a capacity
leak that pins memory. The correct structure allocates the backing array **once**
with `make([]int, capacity)` and never grows it.

Instead of moving data, we move an index. `head` is where the next write goes.
`Add` writes at `head`, advances it modulo the capacity so it wraps around, and
bumps a separate `size` counter until the buffer is full. Once full, `size` stays
pinned at `cap` and every write overwrites the oldest entry — that is the ring.
Tracking `size` separately is essential: after the buffer fills, `len(h.data)`
equals `cap(h.data)`, so it can no longer tell you how many logical items exist.

`Snapshot` must return events in chronological order, oldest first. The oldest
element sits at index `head - size` (modulo capacity, adjusted to stay
non-negative). It walks `size` elements from there, copying each into a fresh
slice. Returning that fresh copy — never a sub-slice of `h.data` — is the
defensive-copy contract: a caller can mutate the snapshot freely and the history
is untouched.

Create `history.go`:

```go
package history

// History keeps the most recent Cap events in a fixed-size backing array.
// It behaves as a ring buffer: once full, each Add overwrites the oldest
// event. The backing array is allocated once by New and never grows.
type History struct {
	data []int
	head int // index where the next Add writes
	size int // logical number of events currently held (<= cap(data))
}

// New returns a History that retains the last capacity events. A capacity of
// zero or less is clamped to 1 so the backing array is always usable.
func New(capacity int) *History {
	if capacity <= 0 {
		capacity = 1
	}
	return &History{data: make([]int, capacity)}
}

// Add records v, overwriting the oldest event when the history is full.
func (h *History) Add(v int) {
	h.data[h.head] = v
	h.head = (h.head + 1) % cap(h.data)
	if h.size < cap(h.data) {
		h.size++
	}
}

// Snapshot returns a fresh slice of the held events in chronological order,
// oldest first. The result is independent of the History's internal storage.
func (h *History) Snapshot() []int {
	out := make([]int, h.size)
	start := (h.head - h.size + cap(h.data)) % cap(h.data)
	for i := range h.size {
		out[i] = h.data[(start+i)%cap(h.data)]
	}
	return out
}

// Len reports the number of events currently held.
func (h *History) Len() int { return h.size }

// Cap reports the fixed capacity of the history.
func (h *History) Cap() int { return cap(h.data) }
```

### The runnable demo

The demo fills a capacity-3 history, then pushes past it so the ring overwrites
the oldest entries, and prints the snapshot at each stage.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/eventhistory"
)

func main() {
	h := history.New(3)
	for _, code := range []int{100, 101, 102} {
		h.Add(code)
	}
	fmt.Printf("full:      %v (len=%d cap=%d)\n", h.Snapshot(), h.Len(), h.Cap())

	h.Add(103)
	h.Add(104)
	fmt.Printf("overflowed: %v (len=%d cap=%d)\n", h.Snapshot(), h.Len(), h.Cap())

	snap := h.Snapshot()
	snap[0] = -1
	fmt.Printf("after caller mutates snapshot: %v\n", h.Snapshot())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
full:      [100 101 102] (len=3 cap=3)
overflowed: [102 103 104] (len=3 cap=3)
after caller mutates snapshot: [102 103 104]
```

### Tests

The suite pins the whole contract. `TestOverwriteOldestWhenFull` is the central
case: pushing 1..5 into a capacity-3 history must yield `[3 4 5]`.
`TestSnapshotReturnsIndependentCopy` mutates a snapshot and re-snapshots to prove
the copy is defensive. `TestNeverGrowsBeyondCapacity` hammers 1000 events into a
capacity-10 history and asserts the array never grew.

Create `history_test.go`:

```go
package history

import (
	"fmt"
	"reflect"
	"testing"
)

func printSnapshot(h *History) {
	fmt.Println(h.Snapshot())
}

func TestAddAndSnapshotInOrder(t *testing.T) {
	t.Parallel()
	h := New(3)
	h.Add(1)
	h.Add(2)
	h.Add(3)
	if got, want := h.Snapshot(), []int{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot() = %v, want %v", got, want)
	}
}

func TestOverwriteOldestWhenFull(t *testing.T) {
	t.Parallel()
	h := New(3)
	for i := 1; i <= 5; i++ {
		h.Add(i)
	}
	if got, want := h.Snapshot(), []int{3, 4, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot() = %v, want %v", got, want)
	}
}

func TestSnapshotReturnsIndependentCopy(t *testing.T) {
	t.Parallel()
	h := New(3)
	h.Add(1)
	h.Add(2)
	got := h.Snapshot()
	got[0] = 99
	if got2 := h.Snapshot(); !reflect.DeepEqual(got2, []int{1, 2}) {
		t.Fatalf("mutating the snapshot changed the history: %v", got2)
	}
}

func TestEmptySnapshot(t *testing.T) {
	t.Parallel()
	h := New(3)
	if got := h.Snapshot(); len(got) != 0 {
		t.Fatalf("Snapshot() = %v, want empty", got)
	}
}

func TestWraparoundAtBoundary(t *testing.T) {
	t.Parallel()
	h := New(3)
	h.Add(1)
	h.Add(2)
	h.Add(3)
	h.Add(4)
	if got, want := h.Snapshot(), []int{2, 3, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot() = %v, want %v", got, want)
	}
}

func TestNeverGrowsBeyondCapacity(t *testing.T) {
	t.Parallel()
	h := New(10)
	for i := range 1000 {
		h.Add(i)
	}
	if h.Cap() != 10 {
		t.Fatalf("Cap() = %d, want 10 (backing array must not grow)", h.Cap())
	}
	if h.Len() != 10 {
		t.Fatalf("Len() = %d, want 10", h.Len())
	}
	if got, want := h.Snapshot(), []int{990, 991, 992, 993, 994, 995, 996, 997, 998, 999}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot() = %v, want %v", got, want)
	}
}

func ExampleHistory() {
	h := New(3)
	for _, v := range []int{1, 2, 3, 4} {
		h.Add(v)
	}
	// oldest (1) was overwritten by 4
	printSnapshot(h)
	// Output: [2 3 4]
}
```

## Review

The history is correct when three invariants hold. First, the backing array is
allocated once and never grows: `Cap()` is constant for the life of the value,
which `TestNeverGrowsBeyondCapacity` pins by adding 1000 events into capacity 10.
Second, the ring overwrites oldest-first, so 1..5 into capacity 3 yields
`[3 4 5]`. Third, `Snapshot` is a defensive copy — mutating the result leaves the
history unchanged. The two mistakes that break this are using `append` (which
grows the array and defeats the bound) and returning a sub-slice of `data` from
`Snapshot` (which lets the caller mutate internal state). Track `size`
separately from `len(data)`: once full they are equal, so `len` can no longer
report the item count. Run `go test -race` to confirm the copy semantics hold.

## Resources

- [Go blog: Go Slices: usage and internals](https://go.dev/blog/slices-intro) — the header, `make`, and why a sub-slice shares storage.
- [Go Specification: Slice types](https://go.dev/ref/spec#Slice_types) — `len`, `cap`, and slice expressions.
- [pkg.go.dev: container/ring](https://pkg.go.dev/container/ring) — the stdlib ring buffer, for comparison with the slice-backed approach here.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-slice-header-diagnostics.md](02-slice-header-diagnostics.md)
