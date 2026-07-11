# Exercise 6: Allocation-Free Flush Cycles with s[:0] in a Write Batcher

A write batcher accumulates records, flushes at a threshold, and resets. The
reset is where allocation behavior is decided: `s = s[:0]` keeps the backing
array so the next cycle refills it for free, while `s = nil` throws the array
away and reallocates every cycle. This exercise builds both and measures the
difference — the exact pattern behind `sync.Pool`-style buffer reuse.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
writebatch/                 independent module: example.com/writebatch
  go.mod
  writebatch.go             type Batcher; Add, flush; TruncateReset vs NilReset
  cmd/
    demo/
      main.go               runnable demo: reuse vs discard across cycles
  writebatch_test.go        s[:0] keeps cap and array; nil resets cap; steady-state 0 allocs
```

Files: `writebatch.go`, `cmd/demo/main.go`, `writebatch_test.go`.
Implement: a `Batcher` that appends records and flushes at a threshold, resetting with `s = s[:0]`; plus free functions `TruncateReset` and `NilReset` to contrast the two.
Test: after `s[:0]` reset, `len == 0` but `cap` unchanged and `SliceData` identical across cycles; after `nil` reset, `cap == 0` and the next append allocates; steady-state cycles amortize to zero allocations.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/writebatch/cmd/demo
cd ~/go-exercises/writebatch
go mod init example.com/writebatch
```

### Two resets that look the same and are opposite

`s = s[:0]` produces a slice with the same pointer, `len == 0`, and the same
`cap`. The backing array is untouched — its contents are logically discarded but
the memory is retained. The next `append` writes back into that same array from
index zero, allocating nothing. Across many flush cycles the batcher reaches a
steady state where each cycle's appends fit in the retained capacity, so
`testing.AllocsPerRun` reports zero allocations per cycle. That is the whole
point of buffer reuse and of `sync.Pool`: keep the array, refill it.

`s = nil` produces the zero slice: pointer `nil`, `len == 0`, `cap == 0`. The
backing array is dropped and becomes garbage. The next `append` has nowhere to
write, so it allocates a fresh array — and if the batch grows, it reallocates
several times again as it did on the first cycle. Every cycle pays the full
allocation cost. The only time you want `nil` is when you genuinely want the
memory reclaimed (e.g. the batcher is going idle for a long time and you do not
want to pin a large array).

`Batcher.Flush` uses the reuse reset. It hands the accumulated records to a sink
(here, a count), then does `b.buf = b.buf[:0]`. Because the array survives, a
long-running batcher settles into allocation-free steady state — which is what
you want on a hot write path.

Create `writebatch.go`:

```go
package writebatch

// Batcher accumulates records and flushes them in bulk once a threshold is
// reached. It resets its buffer with buf[:0] so the backing array is reused
// across flush cycles, reaching an allocation-free steady state.
type Batcher struct {
	buf       []int
	threshold int
	flushed   int // total records flushed, for observation
	cycles    int // number of flushes performed
}

// NewBatcher returns a Batcher that flushes once it holds threshold records.
func NewBatcher(threshold int) *Batcher {
	if threshold <= 0 {
		threshold = 1
	}
	return &Batcher{threshold: threshold}
}

// Add appends a record, flushing automatically when the threshold is reached.
func (b *Batcher) Add(record int) {
	b.buf = append(b.buf, record)
	if len(b.buf) >= b.threshold {
		b.Flush()
	}
}

// Flush "writes out" the batch (here, counts it) and resets the buffer with
// buf[:0], preserving the backing array for the next cycle.
func (b *Batcher) Flush() {
	if len(b.buf) == 0 {
		return
	}
	b.flushed += len(b.buf)
	b.cycles++
	b.buf = b.buf[:0] // reuse: keep the array
}

// Cap reports the current backing-array capacity of the buffer.
func (b *Batcher) Cap() int { return cap(b.buf) }

// Flushed reports the total number of records flushed so far.
func (b *Batcher) Flushed() int { return b.flushed }

// TruncateReset resets s with s[:0]: length 0, same array, same capacity.
func TruncateReset(s []int) []int { return s[:0] }

// NilReset resets s with nil: the array is discarded and cap becomes 0.
func NilReset(s []int) []int { return nil }
```

### The runnable demo

The demo runs several flush cycles and shows the buffer's capacity stays put
(the array is reused), then contrasts a nil reset dropping the capacity to zero.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/writebatch"
)

func main() {
	b := writebatch.NewBatcher(4)
	for i := range 12 { // exactly 3 flush cycles
		b.Add(i)
	}
	fmt.Printf("flushed %d records; buffer cap retained = %d\n", b.Flushed(), b.Cap())

	s := []int{1, 2, 3, 4}
	s = writebatch.TruncateReset(s)
	fmt.Printf("after s[:0]: len=%d cap=%d\n", len(s), cap(s))

	s = []int{1, 2, 3, 4}
	s = writebatch.NilReset(s)
	fmt.Printf("after nil:  len=%d cap=%d\n", len(s), cap(s))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flushed 12 records; buffer cap retained = 4
after s[:0]: len=0 cap=4
after nil:  len=0 cap=0
```

The `s[:0]` line shows the essential contrast: length collapses to zero but the
capacity of 4 is retained (the array survives), whereas the `nil` line drops both
to zero (the array is discarded).

### Tests

`TestTruncateResetKeepsArray` proves `s[:0]` keeps `len==0`, the capacity, and
the same backing array across cycles. `TestNilResetDropsCapacity` proves `nil`
drops `cap` to zero and forces a fresh allocation. `TestSteadyStateZeroAllocs`
uses `testing.AllocsPerRun` to show the reuse batcher amortizes to zero
allocations once warmed up.

Create `writebatch_test.go`:

```go
package writebatch

import (
	"fmt"
	"testing"
	"unsafe"
)

func TestTruncateResetKeepsArray(t *testing.T) {
	t.Parallel()
	s := make([]int, 0, 8)
	for i := range 8 {
		s = append(s, i)
	}
	first := unsafe.SliceData(s)
	firstCap := cap(s)

	for range 100 { // many flush cycles
		s = TruncateReset(s)
		if len(s) != 0 {
			t.Fatalf("after s[:0], len = %d, want 0", len(s))
		}
		if cap(s) != firstCap {
			t.Fatalf("s[:0] changed cap: got %d, want %d", cap(s), firstCap)
		}
		for i := range 8 {
			s = append(s, i)
		}
		if unsafe.SliceData(s) != first {
			t.Fatal("s[:0] reuse reallocated the backing array")
		}
	}
}

func TestNilResetDropsCapacity(t *testing.T) {
	t.Parallel()
	s := make([]int, 0, 8)
	for i := range 8 {
		s = append(s, i)
	}
	before := unsafe.SliceData(s)

	s = NilReset(s)
	if cap(s) != 0 || len(s) != 0 {
		t.Fatalf("after nil, len=%d cap=%d, want 0/0", len(s), cap(s))
	}
	s = append(s, 1) // must allocate a fresh array
	if unsafe.SliceData(s) == before {
		t.Fatal("append after nil reset reused the old array; it must allocate")
	}
}

func TestSteadyStateZeroAllocs(t *testing.T) {
	var buf []int
	// Warm up so the array reaches full cycle capacity.
	for i := range 64 {
		buf = append(buf, i)
	}
	buf = buf[:0]

	allocs := testing.AllocsPerRun(200, func() {
		for i := range 64 {
			buf = append(buf, i)
		}
		buf = buf[:0] // reuse
	})
	if allocs != 0 {
		t.Fatalf("steady-state reuse cycle made %v allocations, want 0", allocs)
	}
}

func TestBatcherFlushesAtThreshold(t *testing.T) {
	t.Parallel()
	b := NewBatcher(4)
	for i := range 12 {
		b.Add(i)
	}
	if b.Flushed() != 12 {
		t.Fatalf("flushed %d records, want 12", b.Flushed())
	}
	if b.Cap() != 4 {
		t.Fatalf("retained cap = %d, want 4 (array reused across cycles)", b.Cap())
	}
}

func ExampleBatcher() {
	b := NewBatcher(4)
	for i := range 8 {
		b.Add(i)
	}
	fmt.Println(b.Flushed(), b.Cap())
	// Output: 8 4
}
```

## Review

The two resets are the crux. `s = s[:0]` keeps the pointer, sets `len` to 0, and
keeps `cap`, so the next cycle refills the same array for free —
`TestSteadyStateZeroAllocs` measures exactly zero allocations per warmed-up
cycle, and `TestTruncateResetKeepsArray` confirms the backing pointer never
changes. `s = nil` drops the array (`cap == 0`) and reallocates on the next
append. Choose `s[:0]` on a hot write path where you want reuse; choose `nil`
only when you deliberately want the memory reclaimed. Do not call
`testing.AllocsPerRun` from a parallel test. Run `go test -race` to confirm the
reuse cycle is correct under the race detector.

## Resources

- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices) — truncation keeps the backing array.
- [pkg.go.dev: sync.Pool](https://pkg.go.dev/sync#Pool) — the buffer-reuse pattern this exercise underpins.
- [pkg.go.dev: testing.AllocsPerRun](https://pkg.go.dev/testing#AllocsPerRun) — measuring steady-state allocations.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-slices-delete-eviction.md](07-slices-delete-eviction.md)
