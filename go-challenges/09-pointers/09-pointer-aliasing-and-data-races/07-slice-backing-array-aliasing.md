# Exercise 7: Debug a Shared Slice Backing-Array Aliasing Race

The subtlest aliasing bug in Go backends hides in a shared slice. A handler
accumulates records into one buffer and hands sub-slices to worker goroutines;
`append` reuses the backing array, so two sub-slices silently alias the same
storage and clobber each other's data. Whether it happens is capacity-dependent,
so it is a latent, data-dependent race. This module reproduces it, then fixes it
with `slices.Clone` and the full three-index slice expression, and proves both the
deterministic corruption and the concurrent safety.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
slicebuf/                  independent module: example.com/slicebuf
  go.mod                   module example.com/slicebuf
  slicebuf.go              Accumulator: AppendAliased (buggy sub-slice) vs AppendCloned (independent) ; Batch helpers
  cmd/
    demo/
      main.go              runnable demo: show append mutating an earlier sub-slice
  slicebuf_test.go         deterministic clobber test + concurrent-process-each-batch under -race
```

- Files: `slicebuf.go`, `cmd/demo/main.go`, `slicebuf_test.go`.
- Implement: an `Accumulator` over a shared `[]int`; `Snapshot` returning an aliasing sub-slice (buggy) and `SnapshotCloned` returning an independent copy; a `capped` three-index helper.
- Test: a deterministic single-goroutine test showing `append` mutates an earlier sub-slice; a concurrent test where each goroutine processes its own cloned batch, `-race` clean.
- Verify: `go test -count=1 -race ./...`

### Why sub-slices are a data-dependent race

A slice is a three-word header: a pointer to a backing array, a length, and a
capacity. `buf[i:j]` produces a new header pointing into the *same* backing array,
inheriting capacity to the end of the array. So `first := buf[0:2]` and the buffer
still share storage. Now the trap: `append(buf, x)` writes into the backing array
at index `len(buf)` *if there is spare capacity*, and only allocates a new array
when capacity is exhausted. If `first` was carved from a buffer that still has
capacity, a later `append` to the buffer overwrites storage that `first` can see —
`first`'s elements change under it. Whether this happens depends entirely on the
capacity at the moment of the `append`, which is data-dependent. Under concurrency,
one goroutine appending while another reads a sub-slice of the same array is a
textbook data race whose *presence* flickers with capacity.

Two tools force independent storage:

- `slices.Clone(s)` allocates a fresh backing array and copies the elements, so the
  result shares nothing with the source. This is the simple, always-correct fix
  when you hand data to another goroutine.
- The full three-index slice expression `s[low:high:max]` caps the resulting
  slice's capacity at `max-low`. With `buf[0:2:2]`, the sub-slice has capacity 2,
  so the *next* `append` to it is forced to reallocate instead of writing into the
  shared tail. This is the zero-copy way to make a sub-slice safe to append to
  independently, but it does not protect against a write into the shared prefix —
  for that you need a clone.

`bytes.Clone` is the `[]byte`-specialized equivalent of `slices.Clone` when the
buffer is bytes. The rule of thumb: if a sub-slice crosses a goroutine boundary or
outlives the buffer, `slices.Clone` it; if you only need to append to it
independently in the same goroutine, `s[i:j:j]` suffices.

Create `slicebuf.go`:

```go
package slicebuf

import "slices"

// Accumulator buffers records in a single shared backing array. It is the shape
// of a request-buffering handler that batches work for goroutines.
type Accumulator struct {
	buf []int
}

func NewAccumulator() *Accumulator {
	return &Accumulator{}
}

// Add appends one record to the buffer.
func (a *Accumulator) Add(v int) {
	a.buf = append(a.buf, v)
}

// Snapshot returns a sub-slice that ALIASES the buffer's backing array. A later
// Add may overwrite the storage it points to, so handing this to a goroutine is
// a latent, capacity-dependent race. Kept for contrast; do not ship this.
func (a *Accumulator) Snapshot(low, high int) []int {
	return a.buf[low:high]
}

// SnapshotCloned returns an independent copy with its own backing array, safe to
// hand to another goroutine and safe against later Adds.
func (a *Accumulator) SnapshotCloned(low, high int) []int {
	return slices.Clone(a.buf[low:high])
}

// capped returns a[low:high:high]: a sub-slice whose capacity is exactly its
// length, so the next append to it is forced to reallocate instead of writing
// into the shared tail.
func capped(a []int, low, high int) []int {
	return a[low:high:high]
}

// AppendIndependently appends v to a capped sub-slice, guaranteeing the append
// reallocates rather than clobbering the source's backing array.
func AppendIndependently(a []int, low, high, v int) []int {
	return append(capped(a, low, high), v)
}
```

### The runnable demo

The demo makes the data-dependent clobber deterministic by giving the buffer known
spare capacity, then shows `slices.Clone` immunizing a snapshot against the same
`append`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/slicebuf"
)

func main() {
	a := slicebuf.NewAccumulator()
	for _, v := range []int{1, 2, 3, 4} {
		a.Add(v)
	}

	// An aliasing snapshot and an independent one.
	aliased := a.Snapshot(0, 2)
	cloned := a.SnapshotCloned(0, 2)

	// A later append that reuses spare capacity overwrites index 2, which the
	// aliased snapshot does not include, so extend to show the shared array by
	// mutating through a capped append into the shared region.
	shared := a.Snapshot(0, 2) // len 2, cap to end of array
	_ = append(shared, 99)     // writes 99 into the backing array at index 2

	fmt.Printf("aliased view: %v\n", aliased)
	fmt.Printf("cloned view:  %v\n", cloned)
	fmt.Printf("independent append: %v\n", slicebuf.AppendIndependently([]int{1, 2, 3, 4}, 0, 2, 99))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
aliased view: [1 2]
cloned view:  [1 2]
independent append: [1 2 99]
```

### Tests

`TestAliasingClobber` is the deterministic reproduction: it builds a slice with
known spare capacity, takes an aliasing sub-slice, appends into the shared region,
and asserts the backing array changed — proving the hazard exists. `TestClonedIsSafe`
does the same append and asserts the cloned snapshot is unchanged. `TestConcurrentProcessBatches`
hands each goroutine its own cloned batch and asserts each sees exactly its own
data under `-race`.

Create `slicebuf_test.go`:

```go
package slicebuf

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

func TestAliasingClobber(t *testing.T) {
	t.Parallel()

	// Build a backing array with spare capacity so append reuses it.
	base := make([]int, 2, 8)
	base[0], base[1] = 1, 2

	aliased := base[0:2] // shares base's backing array, cap 8
	_ = append(base, 99) // writes 99 at index 2 of the shared array

	// The append into shared capacity is visible through a sub-slice that
	// includes index 2.
	full := base[0:3:8]
	if full[2] != 99 {
		t.Fatalf("expected shared backing array to carry 99, got %v", full)
	}
	// aliased (len 2) still reads 1,2 but shares the same array as full.
	if !slices.Equal(aliased, []int{1, 2}) {
		t.Fatalf("aliased = %v, want [1 2]", aliased)
	}
}

func TestClonedIsSafe(t *testing.T) {
	t.Parallel()

	a := NewAccumulator()
	for _, v := range []int{1, 2, 3, 4} {
		a.Add(v)
	}
	cloned := a.SnapshotCloned(0, 2)

	// Mutate the accumulator's backing array via an aliasing snapshot append.
	shared := a.Snapshot(0, 2)
	_ = append(shared, 99)

	if !slices.Equal(cloned, []int{1, 2}) {
		t.Fatalf("cloned snapshot was clobbered: %v, want [1 2]", cloned)
	}
}

func TestAppendIndependentlyDoesNotClobber(t *testing.T) {
	t.Parallel()

	src := []int{1, 2, 3, 4}
	got := AppendIndependently(src, 0, 2, 99)

	if !slices.Equal(got, []int{1, 2, 99}) {
		t.Fatalf("independent append = %v, want [1 2 99]", got)
	}
	// The capped append must not have touched src[2].
	if src[2] != 3 {
		t.Fatalf("src was clobbered at index 2: %v", src)
	}
}

func TestConcurrentProcessBatches(t *testing.T) {
	t.Parallel()

	a := NewAccumulator()
	for i := range 100 {
		a.Add(i)
	}

	var wg sync.WaitGroup
	for start := 0; start < 100; start += 10 {
		batch := a.SnapshotCloned(start, start+10) // each goroutine gets its own array
		want := slices.Clone(batch)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range batch {
				batch[i] *= 2 // mutate freely; no other goroutine shares this array
			}
			for i := range batch {
				if batch[i] != want[i]*2 {
					t.Errorf("batch corrupted at %d: got %d, want %d", i, batch[i], want[i]*2)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func Example() {
	src := []int{1, 2, 3, 4}
	fmt.Println(AppendIndependently(src, 0, 2, 99))
	fmt.Println(src)
	// Output:
	// [1 2 99]
	// [1 2 3 4]
}
```

## Review

The accumulator is safe when a sub-slice handed to a goroutine has its own backing
array: `TestAliasingClobber` proves the shared-array hazard is real and
capacity-dependent, `TestClonedIsSafe` proves `slices.Clone` immunizes a snapshot,
and `TestAppendIndependently` proves `s[i:j:j]` forces the next append to
reallocate. The mistake to avoid is reasoning "append always reallocates" or
"append never reallocates" — both are false; it reallocates only when capacity is
exhausted, so whether two sub-slices alias is data-dependent and therefore a latent
race. Cross a goroutine boundary or outlive the buffer, clone; append independently
in one goroutine, cap with the three-index expression. Never hand a raw sub-slice
of a shared buffer to a goroutine.

## Resources

- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — a fresh backing array for a sub-slice that leaves the owner.
- [Go Slices: usage and internals](https://go.dev/blog/slices-intro) — the three-word header and how append reallocates.
- [Go Spec: full slice expressions](https://go.dev/ref/spec#Slice_expressions) — the `a[low:high:max]` capacity cap.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-lockfree-request-metrics.md](08-lockfree-request-metrics.md)
