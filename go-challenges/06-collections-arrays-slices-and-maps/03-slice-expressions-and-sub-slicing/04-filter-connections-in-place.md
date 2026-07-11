# Exercise 4: Drop Closed Connections by Filtering a Slice In Place

A connection-pool sweep removes dead entries from a `[]*Conn`. The zero-allocation
idiom `out := s[:0]` re-appends the survivors onto the same backing array, but it
leaves stale pointers in the tail that keep the removed connections reachable — a
GC leak hiding inside a "cheap" filter. This exercise builds the in-place filter
correctly (clearing the tail) and cross-checks it against `slices.DeleteFunc`,
which handles the zeroing for you.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
connpool/                  independent module: example.com/connpool
  go.mod                   go 1.24
  connpool.go              type Conn; SweepInPlace; SweepDeleteFunc
  cmd/
    demo/
      main.go              runnable demo sweeping a small pool
  connpool_test.go         order/len test, backing-array reuse test, tail-nil leak test
```

- Files: `connpool.go`, `cmd/demo/main.go`, `connpool_test.go`.
- Implement: `SweepInPlace` using `s[:0]` reuse plus `clear` of the tail;
  `SweepDeleteFunc` using `slices.DeleteFunc`.
- Test: order preserved, length correct, the reused backing array is the same one;
  after the in-place sweep the tail `s[len:cap]` is nil so removed conns are
  collectable; the two variants agree.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/connpool/cmd/demo
cd ~/go-exercises/connpool
go mod init example.com/connpool
go mod edit -go=1.24
```

## Why the cheap filter leaks, and how clear fixes it

The filter-in-place idiom is genuinely useful: `out := s[:0]` gives a length-0 slice
sharing `s`'s array, and re-appending the kept elements overwrites the front of that
array with the survivors. No allocation, order preserved. For a slice of plain
values that is the whole story.

For a slice of *pointers* it is not. After the loop, `out` has the survivors in
`[0:len(out)]`, but the array still physically holds elements in `[len(out):len(s)]`
— the old tail. Those slots contain stale pointers: some are the removed
connections, some are duplicate copies of survivors left over from the shift. As
long as the original array is reachable (and it is, through `out`, which shares it),
those tail pointers are reachable too, so the GC cannot collect the connections they
name. A pool that sweeps thousands of dead connections this way leaks every one of
them until the whole array is replaced.

The fix is one line: `clear(s[len(out):])` sets the tail slots to `nil` before you
return `out`. Now the only live references are the survivors in the front. This is
exactly what `slices.DeleteFunc` does internally for pointer-containing element
types — it zeroes the vacated tail — which is why the two variants must agree not
just on the surviving elements but on the tail being nil.

Create `connpool.go`:

```go
package connpool

import "slices"

// Conn is a pooled connection. Closed marks it for removal by a sweep.
type Conn struct {
	ID     int
	Closed bool
}

// SweepInPlace removes closed connections using the zero-allocation s[:0] reuse
// idiom, then clears the stale tail so the removed *Conn values are collectable.
// The returned slice shares conns' backing array.
func SweepInPlace(conns []*Conn) []*Conn {
	out := conns[:0]
	for _, c := range conns {
		if !c.Closed {
			out = append(out, c)
		}
	}
	clear(conns[len(out):]) // nil the tail: without this, removed conns leak
	return out
}

// SweepDeleteFunc removes closed connections with slices.DeleteFunc, which shifts
// survivors down and zeroes the vacated tail for pointer element types.
func SweepDeleteFunc(conns []*Conn) []*Conn {
	return slices.DeleteFunc(conns, func(c *Conn) bool { return c.Closed })
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/connpool"
)

func main() {
	pool := []*connpool.Conn{
		{ID: 1},
		{ID: 2, Closed: true},
		{ID: 3},
		{ID: 4, Closed: true},
		{ID: 5},
	}
	origCap := cap(pool)

	live := connpool.SweepInPlace(pool)

	fmt.Printf("kept %d of 5\n", len(live))
	for _, c := range live {
		fmt.Printf("  conn %d\n", c.ID)
	}
	fmt.Println("reused backing array:", cap(live) == origCap)

	// The tail slots were cleared, so removed conns are collectable.
	tail := live[len(live):cap(live)]
	allNil := true
	for _, c := range tail {
		if c != nil {
			allNil = false
		}
	}
	fmt.Println("tail cleared:", allNil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
kept 3 of 5
  conn 1
  conn 3
  conn 5
reused backing array: true
tail cleared: true
```

## Tests

The tests pin four properties: order and length of the survivors, that the in-place
variant really reused the same array (same first-element address and same
capacity), that the tail is nil after the sweep (the leak fix), and that
`slices.DeleteFunc` produces the same survivors and also nils its tail.

Create `connpool_test.go`:

```go
package connpool

import (
	"slices"
	"testing"
)

func pool() []*Conn {
	return []*Conn{
		{ID: 1},
		{ID: 2, Closed: true},
		{ID: 3},
		{ID: 4, Closed: true},
		{ID: 5},
	}
}

func ids(conns []*Conn) []int {
	out := make([]int, len(conns))
	for i, c := range conns {
		out[i] = c.ID
	}
	return out
}

func TestSweepInPlaceKeepsOrderAndReusesArray(t *testing.T) {
	t.Parallel()
	conns := pool()
	firstAddr := &conns[0]
	origCap := cap(conns)

	live := SweepInPlace(conns)

	if !slices.Equal(ids(live), []int{1, 3, 5}) {
		t.Fatalf("survivors = %v; want [1 3 5]", ids(live))
	}
	if &live[0] != firstAddr {
		t.Fatal("SweepInPlace did not reuse the backing array")
	}
	if cap(live) != origCap {
		t.Fatalf("cap changed: got %d want %d (should reuse array)", cap(live), origCap)
	}
}

func TestSweepInPlaceClearsTail(t *testing.T) {
	t.Parallel()
	conns := pool()

	live := SweepInPlace(conns)

	tail := live[len(live):cap(live)]
	if len(tail) == 0 {
		t.Fatal("expected a non-empty tail to check for leaks")
	}
	for i, c := range tail {
		if c != nil {
			t.Fatalf("tail[%d] = %v; want nil (removed conn leaked)", i, c)
		}
	}
}

func TestSweepDeleteFuncMatches(t *testing.T) {
	t.Parallel()
	manual := SweepInPlace(pool())
	viaDelete := SweepDeleteFunc(pool())

	if !slices.Equal(ids(manual), ids(viaDelete)) {
		t.Fatalf("DeleteFunc %v != manual %v", ids(viaDelete), ids(manual))
	}

	// slices.DeleteFunc also zeroes the vacated tail for pointer types.
	tail := viaDelete[len(viaDelete):cap(viaDelete)]
	for i, c := range tail {
		if c != nil {
			t.Fatalf("DeleteFunc tail[%d] = %v; want nil", i, c)
		}
	}
}

func TestSweepAllClosed(t *testing.T) {
	t.Parallel()
	conns := []*Conn{{ID: 1, Closed: true}, {ID: 2, Closed: true}}
	live := SweepInPlace(conns)
	if len(live) != 0 {
		t.Fatalf("len = %d; want 0", len(live))
	}
	// Every original slot must be cleared.
	full := live[:cap(live)]
	for i, c := range full {
		if c != nil {
			t.Fatalf("slot %d not cleared: %v", i, c)
		}
	}
}
```

## Review

The sweep is correct when survivors keep their order, the backing array is reused
(no allocation), and every removed connection is unreferenced afterward. The
tail-nil tests are the ones that matter: a filter that passes the "right survivors"
check can still leak, because leaking is about what lingers in `s[len:cap]`, not
about what is in `s[:len]`. The wrong version omits `clear(conns[len(out):])` and
looks perfectly correct until a heap profile shows connections that should have been
collected. When in doubt, prefer `slices.DeleteFunc`, which does the zeroing for
you; reach for the hand-rolled `s[:0]` filter only when you have measured the
allocation and need it gone, and then always clear the tail. Run `go test -race`.

## Resources

- [`slices.DeleteFunc`](https://pkg.go.dev/slices#DeleteFunc)
- [`clear` builtin](https://pkg.go.dev/builtin#clear)
- [Go Wiki: SliceTricks (filtering in place)](https://go.dev/wiki/SliceTricks#filtering-without-allocating)
- [Go blog: Go Slices: usage and internals](https://go.dev/blog/slices-intro)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-request-header-slice-aliasing.md](03-request-header-slice-aliasing.md) | Next: [05-sliding-window-rate-limiter.md](05-sliding-window-rate-limiter.md)
