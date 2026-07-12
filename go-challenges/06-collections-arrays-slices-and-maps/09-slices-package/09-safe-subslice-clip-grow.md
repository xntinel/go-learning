# Exercise 9: Prevent Append Aliasing When Sharing A Sub-Slice (Clip + Grow)

A request-batching buffer builder hands out sub-slices of one shared backing array
to workers. If a worker appends to its window, the append can write into capacity
that belongs to a sibling window and silently corrupt it — the classic
append-aliasing bug. `slices.Clip` caps a sub-slice's capacity so a downstream
append must allocate instead of overwriting a sibling; `slices.Grow` pre-reserves
capacity before a known number of appends so a hot path does not reallocate
repeatedly. This module reproduces the corruption, then fixes it.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
subslice/                      module example.com/subslice
  go.mod                       go 1.24
  buffer.go                    Window (unclipped, corrupting), SafeWindow (Clip), Prealloc (Grow)
  cmd/
    demo/
      main.go                  runnable demo: show corruption, then the Clip fix
  buffer_test.go               reproduce corruption; Clip prevents it; Grow avoids realloc; -race readers
```

- Files: `buffer.go`, `cmd/demo/main.go`, `buffer_test.go`.
- Implement: `Window(buf, i, j)` returning the raw sub-slice; `SafeWindow(buf, i, j)` returning `slices.Clip` of it; `Prealloc(s, n)` using `slices.Grow`.
- Test: appending into an unclipped window clobbers a sibling; the same append into a clipped window does not (cap == len after Clip, append allocates); `Grow` makes `cap >= len+n` and a following append does not reallocate; concurrent readers over clipped windows are `-race` clean.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The corruption, and why Clip fixes it

A sub-slice `buf[i:j]` has length `j-i` but inherits capacity all the way to the
end of `buf`'s backing array. So `first := buf[0:2]` over a length-4 array has
length 2 but capacity 4. Appending a third element to `first` does not allocate —
there is spare capacity — so it writes into `buf[2]`, which is the first element of
`second := buf[2:4]`. The sibling window is silently overwritten. Nothing errors;
the corruption surfaces later as a wrong value in an unrelated code path. This is
one of the most confusing bugs in Go precisely because the two slices look
independent.

`slices.Clip(s)` returns `s[:len(s):len(s)]` — it caps capacity at length. After
`first = slices.Clip(buf[0:2])`, `first` has length 2 and capacity 2, so the next
`append` finds no spare capacity and is forced to allocate a fresh backing array
and copy. The append now lands in `first`'s private array, and `second` is
untouched. `SafeWindow` returns a clipped window; the test appends into it and
asserts the sibling is intact, and that `cap == len` post-Clip.

`slices.Grow(s, n)` is the complementary tool for the append-heavy path. It
guarantees `cap(s) >= len(s)+n`, reallocating once up front if needed, so a loop
that appends `n` elements does not reallocate-and-copy on each growth step. It
returns the (possibly reallocated) slice, so you reassign. `Prealloc` wraps it, and
the test proves that after `Grow(s, n)` the capacity covers `n` more elements and a
subsequent append does not move the backing array (the address of the first
element is unchanged).

Create `buffer.go`:

```go
package subslice

import "slices"

// Window returns the raw sub-slice buf[i:j]. It inherits buf's capacity, so a
// later append into it can overwrite whatever follows in buf: aliasing hazard.
func Window(buf []int, i, j int) []int {
	return buf[i:j]
}

// SafeWindow returns buf[i:j] with capacity clipped to its length, so a later
// append allocates a fresh array instead of clobbering a sibling window.
func SafeWindow(buf []int, i, j int) []int {
	return slices.Clip(buf[i:j])
}

// Prealloc reserves capacity for n more appends, reallocating at most once now
// so the following appends do not each trigger a grow-and-copy.
func Prealloc(s []int, n int) []int {
	return slices.Grow(s, n)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/subslice"
)

func main() {
	// One backing array shared by two windows.
	buf := []int{10, 20, 30, 40}
	second := buf[2:4] // {30, 40}

	// Unclipped window: appending clobbers second[0].
	first := subslice.Window(buf, 0, 2) // {10, 20}, cap 4
	first = append(first, 99)
	fmt.Printf("unclipped: second[0]=%d (corrupted)\n", second[0])

	// Reset and use a clipped window: the append allocates, second is safe.
	buf = []int{10, 20, 30, 40}
	second = buf[2:4]
	safe := subslice.SafeWindow(buf, 0, 2) // {10, 20}, cap 2
	safe = append(safe, 99)
	fmt.Printf("clipped:   second[0]=%d (intact)\n", second[0])
	_ = first
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
unclipped: second[0]=99 (corrupted)
clipped:   second[0]=30 (intact)
```

The unclipped append wrote 99 into the shared backing slot that `second[0]` reads;
the clipped append allocated a new array, so `second[0]` stays 30.

### Tests

`TestUnclippedCorrupts` reproduces the bug. `TestClipPreventsCorruption` proves
`SafeWindow` isolates the sibling and that cap equals len after Clip.
`TestGrowAvoidsRealloc` proves `Grow` reserves capacity and a following append does
not move the backing array. `TestConcurrentClippedReaders` hands clipped windows to
goroutines under `-race`.

Create `buffer_test.go`:

```go
package subslice

import (
	"sync"
	"testing"
)

func TestUnclippedCorrupts(t *testing.T) {
	t.Parallel()

	buf := []int{10, 20, 30, 40}
	second := buf[2:4]

	first := Window(buf, 0, 2)
	first = append(first, 99) // writes buf[2], which is second[0]

	if second[0] != 99 {
		t.Fatalf("expected corruption: second[0]=%d, want 99", second[0])
	}
	if len(first) != 3 {
		t.Fatalf("first len = %d, want 3", len(first))
	}
}

func TestClipPreventsCorruption(t *testing.T) {
	t.Parallel()

	buf := []int{10, 20, 30, 40}
	second := buf[2:4]

	safe := SafeWindow(buf, 0, 2)
	if cap(safe) != len(safe) {
		t.Fatalf("after Clip cap=%d len=%d, want equal", cap(safe), len(safe))
	}

	safe = append(safe, 99) // must allocate, not touch buf
	if second[0] != 30 {
		t.Fatalf("sibling corrupted: second[0]=%d, want 30", second[0])
	}
	if safe[2] != 99 {
		t.Fatalf("append lost: safe[2]=%d, want 99", safe[2])
	}
}

func TestGrowAvoidsRealloc(t *testing.T) {
	t.Parallel()

	s := make([]int, 3, 3)
	for i := range s {
		s[i] = i
	}
	s = Prealloc(s, 5)
	if cap(s) < len(s)+5 {
		t.Fatalf("after Grow cap=%d, want >= %d", cap(s), len(s)+5)
	}

	// A following append must NOT reallocate: the first element keeps its address.
	addr := &s[0]
	s = append(s, 100)
	if &s[0] != addr {
		t.Fatal("append after Grow reallocated the backing array")
	}
}

func TestConcurrentClippedReaders(t *testing.T) {
	t.Parallel()

	buf := make([]int, 100)
	for i := range buf {
		buf[i] = i
	}

	var wg sync.WaitGroup
	for start := 0; start < 100; start += 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := SafeWindow(buf, start, start+10)
			// Each goroutine appends to its own clipped window: allocates,
			// never writes into buf, so no data race on the shared array.
			w = append(w, -1)
			if w[len(w)-1] != -1 {
				t.Errorf("append lost in window at %d", start)
			}
		}()
	}
	wg.Wait()
}
```

## Review

The bug is real and reproducible: `TestUnclippedCorrupts` shows an append into
`buf[0:2]` overwriting `buf[2:4]`'s first element because the unclipped window
carried the full backing capacity. `slices.Clip` is the fix — it caps capacity at
length, so the next append allocates and the sibling is safe, which
`TestClipPreventsCorruption` pins along with `cap == len`. `Grow` is the other side
of capacity control: reserve once, append many times without realloc, verified by
the unchanged element address. The concurrency test is `-race` clean precisely
because every window is clipped, so no goroutine's append writes into the shared
array. Run `go test -race`.

## Resources

- [`slices.Clip`](https://pkg.go.dev/slices#Clip) — caps capacity at length to force allocation on the next append.
- [`slices.Grow`](https://pkg.go.dev/slices#Grow) — reserves capacity for n appends.
- [Go slice internals: aliasing and capacity](https://go.dev/blog/slices-intro) — the backing-array model behind the corruption.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-slo-extremes-max-min-func.md](10-slo-extremes-max-min-func.md)
