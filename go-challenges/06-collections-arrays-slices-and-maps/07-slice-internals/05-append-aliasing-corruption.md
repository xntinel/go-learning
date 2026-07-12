# Exercise 5: The Spare-Capacity append Footgun in Per-Tenant Views

A fan-out that carves per-tenant sub-slices out of one shared decode buffer is a
common way to avoid copying. It is also a classic corruption bug: appending into
one tenant's sub-slice, which still has spare capacity in the shared array,
silently overwrites the *next* tenant's data. This exercise reproduces the
corruption and fixes it with the three-index slice expression and `slices.Clip`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
tenantfanout/               independent module: example.com/tenantfanout
  go.mod
  fanout.go                 ViewUnsafe (buf[lo:hi]); ViewSafe (slices.Clip)
  cmd/
    demo/
      main.go               runnable demo: show corruption vs safe append
  fanout_test.go            corruption observable; clip forces new array; parent intact
```

Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
Implement: `ViewUnsafe(buf []int, lo, hi int) []int` (a plain reslice) and `ViewSafe(buf []int, lo, hi int) []int` (clipped to length).
Test: appending to `ViewUnsafe` corrupts the neighbor; appending to `ViewSafe` allocates a new array (`SliceData` differs) and leaves the parent untouched; concurrent readers of the shared array stay race-free on the safe path.
Verify: `go test -count=1 -race ./...`

### The bug: append into shared spare capacity

`ViewUnsafe(buf, lo, hi)` returns `buf[lo:hi]`. That sub-slice has length
`hi-lo` but its **capacity runs to the end of `buf`** — it inherits all the spare
capacity of the shared array. So when a caller does `view = append(view, x)`, and
`len(view) < cap(view)`, `append` does *not* allocate. It writes `x` in place at
index `hi` of the backing array — which is the first element of the *next*
tenant's view. One tenant's append silently overwrites another tenant's first
record. This is the spare-capacity footgun, and under concurrency it is also a
data race: one goroutine writing where another is reading.

`ViewSafe(buf, lo, hi)` returns `slices.Clip(buf[lo:hi])`, which is
`buf[lo:hi:hi]` — the three-index full slice expression that sets capacity equal
to length. Now `len(view) == cap(view)`, so the very next `append` is forced to
allocate a fresh backing array, copy the view's elements into it, and write the
new element there. The shared buffer is never touched; the neighbor is safe. The
cost is one allocation per growing append, which is exactly the price of
isolation — pay it at the boundary where you split a shared buffer among
independent consumers.

Create `fanout.go`:

```go
package fanout

import "slices"

// ViewUnsafe returns buf[lo:hi]. The returned slice inherits the spare capacity
// of buf, so appending to it writes into buf beyond index hi and can overwrite
// a neighbouring view. This is the corruption footgun; use ViewSafe instead.
func ViewUnsafe(buf []int, lo, hi int) []int {
	return buf[lo:hi]
}

// ViewSafe returns a view of buf[lo:hi] whose capacity equals its length
// (slices.Clip, i.e. buf[lo:hi:hi]). The next append on the result must
// allocate a new array, so it cannot corrupt buf or any neighbouring view.
func ViewSafe(buf []int, lo, hi int) []int {
	return slices.Clip(buf[lo:hi])
}
```

### The runnable demo

The demo builds a shared buffer holding two tenants' records back to back, then
appends to the first tenant's view two ways: unsafe (corrupts tenant B) and safe
(tenant B intact).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/tenantfanout"
)

func main() {
	// buf: tenant A occupies [0:2], tenant B occupies [2:4]; spare capacity to 8.
	buf := make([]int, 4, 8)
	copy(buf, []int{10, 11, 20, 21}) // A={10,11}, B={20,21}

	unsafeView := fanout.ViewUnsafe(buf, 0, 2)
	unsafeView = append(unsafeView, 99) // writes into buf[2] — tenant B's first record
	fmt.Printf("unsafe: tenant B first record is now %d (corrupted)\n", buf[2])

	// reset and repeat with the safe view
	copy(buf, []int{10, 11, 20, 21})
	safeView := fanout.ViewSafe(buf, 0, 2)
	safeView = append(safeView, 99) // allocates a new array
	fmt.Printf("safe:   tenant B first record is still %d (intact)\n", buf[2])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
unsafe: tenant B first record is now 99 (corrupted)
safe:   tenant B first record is still 20 (intact)
```

### Tests

`TestUnsafeViewCorruptsNeighbor` pins the bug: appending to the unsafe view
overwrites `buf[2]`. `TestSafeViewForcesAllocation` proves the clipped view
appends into a *new* array (`SliceData` differs from the parent) and leaves the
parent untouched. `TestSafeViewConcurrentReaders` runs readers of the shared
buffer while appending to a safe (isolated) view under `-race`.

Create `fanout_test.go`:

```go
package fanout

import (
	"fmt"
	"sync"
	"testing"
	"unsafe"
)

func TestUnsafeViewCorruptsNeighbor(t *testing.T) {
	t.Parallel()
	buf := make([]int, 4, 8)
	copy(buf, []int{10, 11, 20, 21})

	view := ViewUnsafe(buf, 0, 2)
	view = append(view, 99) // spare capacity: writes into buf[2]

	if buf[2] != 99 {
		t.Fatalf("expected the unsafe append to corrupt buf[2]; got %d", buf[2])
	}
}

func TestSafeViewForcesAllocation(t *testing.T) {
	t.Parallel()
	buf := make([]int, 4, 8)
	copy(buf, []int{10, 11, 20, 21})

	parentData := unsafe.SliceData(buf)
	view := ViewSafe(buf, 0, 2)
	if cap(view) != 2 {
		t.Fatalf("clipped view cap = %d, want 2 (cap == len)", cap(view))
	}

	view = append(view, 99) // must allocate a new array
	if unsafe.SliceData(view) == parentData {
		t.Fatal("safe append reused the parent array; it must allocate a new one")
	}
	if buf[2] != 20 {
		t.Fatalf("safe append corrupted the parent: buf[2] = %d, want 20", buf[2])
	}
}

func TestSafeViewConcurrentReaders(t *testing.T) {
	t.Parallel()
	buf := make([]int, 4, 8)
	copy(buf, []int{10, 11, 20, 21})

	var wg sync.WaitGroup
	// Readers observe tenant B's region while we grow tenant A's isolated view.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = buf[2] + buf[3]
		}()
	}
	view := ViewSafe(buf, 0, 2)
	view = append(view, 99) // isolated array: no write to buf, so no race
	wg.Wait()

	if buf[2] != 20 {
		t.Fatalf("shared buffer was mutated: buf[2] = %d, want 20", buf[2])
	}
}

func ExampleViewSafe() {
	buf := []int{1, 2, 3, 4}
	v := ViewSafe(buf, 0, 2)
	v = append(v, 99)      // new array
	fmt.Println(buf[2], v) // parent intact, view grew
	// Output: 3 [1 2 99]
}
```

## Review

The bug is real and deterministic: `buf[lo:hi]` inherits the parent's spare
capacity, so an in-place `append` writes past `hi` into the neighbor's data —
`TestUnsafeViewCorruptsNeighbor` proves it. The fix is to cap the capacity at the
split point with `slices.Clip` (or `buf[lo:hi:hi]`), which forces the next
`append` to allocate; `TestSafeViewForcesAllocation` confirms the new array
(`SliceData` differs) and the intact parent. This is why any function that hands
out a sub-slice of a shared buffer to an independent consumer should clip it
first. The concurrent test is race-free only because the safe view no longer
writes into the shared array — the unsafe version under concurrency would be a
genuine data race, which is exactly the failure this pattern prevents. Run
`go test -race`.

## Resources

- [pkg.go.dev: slices.Clip](https://pkg.go.dev/slices#Clip) — set `cap == len` to force the next append to allocate.
- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — the three-index `a[low:high:max]` form.
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices) — in-place append into spare capacity.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-buffer-reuse-truncation.md](06-buffer-reuse-truncation.md)
