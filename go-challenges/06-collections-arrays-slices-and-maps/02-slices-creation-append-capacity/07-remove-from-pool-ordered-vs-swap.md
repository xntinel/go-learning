# Exercise 7: Removing a Connection — Ordered Delete vs O(1) Swap-Remove

Removing a dead connection from a pool slice has two idiomatic implementations,
and the choice is a real engineering decision. Order-preserving delete
(`slices.Delete`) shifts the tail down and is O(n); swap-remove moves the last
element into the gap and is O(1) but reorders. Both must clear the vacated tail
slot, or the removed `*Conn` leaks. You will implement both and prove their
distinct guarantees.

This module is self-contained: its own module, demo, and tests.

## What you'll build

```text
connpool/                  independent module: example.com/connpool
  go.mod                   go 1.26
  connpool.go              Conn; DeleteOrdered (slices.Delete), SwapRemove (O(1))
  cmd/
    demo/
      main.go              remove one connection each way, print resulting order
  connpool_test.go         order semantics, len-1, nil'd tail slot, bench contrast, Example
```

Files: `connpool.go`, `cmd/demo/main.go`, `connpool_test.go`.
Implement: `DeleteOrdered(pool, i)` via `slices.Delete` (preserves order), and `SwapRemove(pool, i)` (moves the last element into index `i`, nils the old last).
Test: ordered delete preserves order; swap-remove removes the target but may reorder; both shrink `len` by one and leave no dangling reference in the freed slot.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/07-remove-from-pool-ordered-vs-swap/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/07-remove-from-pool-ordered-vs-swap
go mod edit -go=1.26
```

### The two strategies and their trade-off

Order-preserving removal has to close the gap: after removing index `i`, every
element from `i+1` onward slides down one slot. `slices.Delete(pool, i, i+1)` does
exactly that with a single `copy` — O(n) in the number of elements after `i` — and,
since Go 1.22, it zeroes the now-unused tail slot so the removed pointer does not
linger in the backing array. Use it when the slice order is meaningful: a
priority-ordered list, a round-robin ring where position matters, anything a
consumer iterates in order.

Swap-remove trades order for speed. To remove index `i`, copy the *last* element
into slot `i`, nil the old last slot, and reslice to drop the tail:
`pool[i] = pool[last]; pool[last] = nil; return pool[:last]`. That is O(1) — no
shifting — but the element that was last is now where index `i` used to be, so the
order is scrambled. Use it for an unordered pool where any connection is
interchangeable and the only thing that matters is fast removal, which is the
common case for a connection pool. The nil of the old last slot is not optional:
skip it and the removed `*Conn` (with its open socket, buffers, and TLS state)
stays reachable through the array and never gets collected.

The shared rule across both: shrinking a pointer slice without clearing the freed
slot is a leak. `slices.Delete` clears it for you; swap-remove you clear by hand.

Create `connpool.go`:

```go
package connpool

import "slices"

// Conn is a pooled connection. In production it would wrap a net.Conn and its
// buffers; here the ID is enough to observe removal semantics.
type Conn struct {
	ID string
}

// DeleteOrdered removes the connection at index i, preserving the order of the
// rest. It is O(n) in the tail length. slices.Delete zeroes the vacated tail
// slot, so the removed *Conn is released for GC.
func DeleteOrdered(pool []*Conn, i int) []*Conn {
	return slices.Delete(pool, i, i+1)
}

// SwapRemove removes the connection at index i in O(1) by moving the last
// element into the gap. Order is not preserved. The old last slot is nil'd so
// the removed *Conn is released for GC.
func SwapRemove(pool []*Conn, i int) []*Conn {
	last := len(pool) - 1
	pool[i] = pool[last]
	pool[last] = nil
	return pool[:last]
}
```

### The runnable demo

The demo removes index 1 from a four-connection pool both ways so you can see the
order difference: ordered delete yields `a c d`; swap-remove yields `a d c`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/connpool"
)

func ids(pool []*connpool.Conn) []string {
	out := make([]string, len(pool))
	for i, c := range pool {
		out[i] = c.ID
	}
	return out
}

func main() {
	mk := func() []*connpool.Conn {
		return []*connpool.Conn{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	}

	ordered := connpool.DeleteOrdered(mk(), 1)
	fmt.Printf("ordered delete:  %v\n", ids(ordered))

	swapped := connpool.SwapRemove(mk(), 1)
	fmt.Printf("swap remove:     %v\n", ids(swapped))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ordered delete:  [a c d]
swap remove:     [a d c]
```

### Tests

`TestDeleteOrderedPreservesOrder` asserts `a c d`. `TestSwapRemoveRemovesTarget`
asserts the target is gone and length dropped, without asserting a specific order.
`TestBothNilFreedSlot` keeps the original full-length header and asserts the slot
past the new length is nil for each strategy — the leak-prevention guarantee.

Create `connpool_test.go`:

```go
package connpool

import (
	"fmt"
	"slices"
	"testing"
)

func mkPool() []*Conn {
	return []*Conn{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
}

func ids(pool []*Conn) []string {
	out := make([]string, len(pool))
	for i, c := range pool {
		out[i] = c.ID
	}
	return out
}

func TestDeleteOrderedPreservesOrder(t *testing.T) {
	t.Parallel()
	got := DeleteOrdered(mkPool(), 1)
	want := []string{"a", "c", "d"}
	if !slices.Equal(ids(got), want) {
		t.Fatalf("order = %v, want %v", ids(got), want)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
}

func TestSwapRemoveRemovesTarget(t *testing.T) {
	t.Parallel()
	got := SwapRemove(mkPool(), 1)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for _, c := range got {
		if c.ID == "b" {
			t.Fatalf("removed target still present: %v", ids(got))
		}
	}
	// Order is not preserved: the last element filled the gap.
	want := []string{"a", "d", "c"}
	if !slices.Equal(ids(got), want) {
		t.Fatalf("swap order = %v, want %v", ids(got), want)
	}
}

func TestBothNilFreedSlot(t *testing.T) {
	t.Parallel()
	ordered := mkPool()
	n := len(ordered)
	res := DeleteOrdered(ordered, 1)
	if ordered[len(res)] != nil {
		t.Fatalf("DeleteOrdered left tail slot %d non-nil: %v", len(res), ordered[len(res)])
	}
	_ = n

	swapped := mkPool()
	res2 := SwapRemove(swapped, 1)
	if swapped[len(res2)] != nil {
		t.Fatalf("SwapRemove left tail slot %d non-nil: %v", len(res2), swapped[len(res2)])
	}
}

func BenchmarkDeleteOrdered(b *testing.B) {
	base := make([]*Conn, 1000)
	for i := range base {
		base[i] = &Conn{ID: "x"}
	}
	work := make([]*Conn, len(base))
	for b.Loop() {
		copy(work, base)
		_ = DeleteOrdered(work, 0) // worst case: shift the whole tail
	}
}

func BenchmarkSwapRemove(b *testing.B) {
	base := make([]*Conn, 1000)
	for i := range base {
		base[i] = &Conn{ID: "x"}
	}
	work := make([]*Conn, len(base))
	for b.Loop() {
		copy(work, base)
		_ = SwapRemove(work, 0) // O(1) regardless of position
	}
}

func ExampleSwapRemove() {
	pool := []*Conn{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	pool = SwapRemove(pool, 0)
	fmt.Println(ids(pool))
	// Output: [c b]
}
```

## Review

Both removals are correct when they drop exactly the target, shrink the length by
one, and nil the freed slot. The tests encode the *difference* that drives the
choice: `TestDeleteOrderedPreservesOrder` pins order preservation, while
`TestSwapRemoveRemovesTarget` deliberately does not require the original order —
it only requires the target gone — because swap-remove reorders by design.
`TestBothNilFreedSlot` is the guarantee that unifies them: neither strategy may
leave a dangling pointer in the vacated slot. The benchmarks (run with
`go test -bench .`) make the O(n)-vs-O(1) cost visible. Pick ordered delete only
when order is load-bearing; otherwise swap-remove is the cheaper default for a
pool. Run `-race` to confirm the in-place mutation is sound.

## Resources

- [`slices.Delete`](https://pkg.go.dev/slices#Delete)
- [Go Wiki: SliceTricks (delete, delete without preserving order)](https://go.dev/wiki/SliceTricks)
- [Go blog: Go Slices: usage and internals](https://go.dev/blog/slices-intro)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-filter-expired-sessions-in-place.md](06-filter-expired-sessions-in-place.md) | Next: [08-trim-response-buffer-grow-clip.md](08-trim-response-buffer-grow-clip.md)
