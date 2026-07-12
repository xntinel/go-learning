# Exercise 3: Measuring append Reallocations in a Payload Builder

A payload assembler that concatenates encoded records with `append` looks
allocation-free but is not: as the buffer grows it reallocates its backing array
several times. This exercise quantifies that — recording every capacity change
as the buffer grows — so you can see amortized O(1) growth with your own eyes and
confirm the runtime's growth factor is *not* a hard 2x.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
payloadbuild/               independent module: example.com/payloadbuild
  go.mod
  payload.go                Assemble; GrowthSequence; Reallocations
  cmd/
    demo/
      main.go               runnable demo: print the capacity growth sequence
  payload_test.go           non-decreasing caps, logarithmic reallocs, AllocsPerRun
```

Files: `payload.go`, `cmd/demo/main.go`, `payload_test.go`.
Implement: `Assemble(records [][]byte) []byte`, `GrowthSequence(records [][]byte) []int` (capacity after each growth), and `Reallocations(records [][]byte) int`.
Test: capacity is non-decreasing, each growth strictly larger, reallocations logarithmic in N, and `testing.AllocsPerRun` counts the unpreallocated build's allocations.
Verify: `go test -count=1 -race ./...`

### Why a run of N appends is cheap but not free

`Assemble` starts from a `nil` slice and appends each record's bytes. Every time
the running buffer fills (`len == cap`), `append` allocates a larger backing
array, copies the existing bytes, and continues. If it grew by one element each
time, N appends would copy 1+2+...+N = O(N^2) bytes. Instead the runtime grows by
a factor, so the buffer reallocates only O(log N) times and the total copy work
is O(N) — amortized O(1) per append. That is why appending in a loop from `nil`
is acceptable when you do not know the size up front.

`GrowthSequence` makes the mechanism visible: it appends the same records but
records `cap` every time it changes. The sequence is strictly increasing (a
reallocation only happens to make room, so the new capacity always exceeds the
old) but the ratios are *not* a constant 2. Small buffers roughly double; large
ones grow by about 1.25x; and each request is rounded up to a size class, so you
will see values like 8, 16, 24, 32 rather than a clean power-of-two ladder. This
is the concrete reason never to hard-code "capacity doubles" — measure it.

`Reallocations` is just `len(GrowthSequence(...))` starting from empty: the count
of distinct backing arrays used. For N single-byte records it grows like log N,
not like N, which the test asserts against a generous logarithmic bound.

Create `payload.go`:

```go
package payload

import "math"

// Assemble concatenates the encoded records into one payload, growing the
// buffer with append. It starts from nil, so the backing array reallocates a
// logarithmic number of times as it fills.
func Assemble(records [][]byte) []byte {
	var buf []byte
	for _, r := range records {
		buf = append(buf, r...)
	}
	return buf
}

// GrowthSequence returns the capacity of the buffer after each time it grows,
// while assembling the records. The result is strictly increasing; its length
// is the number of reallocations. The growth factor is runtime-defined and is
// not a fixed 2x.
func GrowthSequence(records [][]byte) []int {
	var buf []byte
	var seq []int
	prev := cap(buf)
	for _, r := range records {
		buf = append(buf, r...)
		if cap(buf) != prev {
			seq = append(seq, cap(buf))
			prev = cap(buf)
		}
	}
	return seq
}

// Reallocations reports how many times the backing array was replaced while
// assembling the records from an empty buffer.
func Reallocations(records [][]byte) int {
	return len(GrowthSequence(records))
}

// LogBound returns a generous upper bound on the number of reallocations
// expected for n single-element appends: proportional to log2(n). Growth is
// logarithmic, so the real count stays well under this.
func LogBound(n int) int {
	if n < 2 {
		return 2
	}
	return int(math.Ceil(2*math.Log2(float64(n)))) + 2
}
```

### The runnable demo

The demo assembles 32 one-byte records and prints the capacity growth ladder and
the reallocation count, so you can see the factor is not exactly 2.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/payloadbuild"
)

func main() {
	records := make([][]byte, 32)
	for i := range records {
		records[i] = []byte{byte(i)}
	}

	seq := payload.GrowthSequence(records)
	out := payload.Assemble(records)

	fmt.Printf("assembled %d bytes\n", len(out))
	fmt.Printf("capacity growth: %v\n", seq)
	fmt.Printf("reallocations: %d (log bound %d)\n", payload.Reallocations(records), payload.LogBound(32))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (capacity ladder is runtime-defined; this is a real run on the
current toolchain):

```
assembled 32 bytes
capacity growth: [8 16 32]
reallocations: 3 (log bound 12)
```

### Tests

The tests assert *properties*, never an exact capacity ladder (which is
runtime-defined). `TestCapacityNonDecreasing` walks the growth sequence and
checks it is strictly increasing. `TestReallocationsAreLogarithmic` proves the
count grows like log N, not N. `TestAllocsCountsGrowth` uses
`testing.AllocsPerRun` to observe that the unpreallocated build really does
allocate several times.

Create `payload_test.go`:

```go
package payload

import (
	"fmt"
	"testing"
)

func oneByteRecords(n int) [][]byte {
	records := make([][]byte, n)
	for i := range records {
		records[i] = []byte{byte(i)}
	}
	return records
}

func TestCapacityNonDecreasing(t *testing.T) {
	t.Parallel()
	seq := GrowthSequence(oneByteRecords(1000))
	prev := 0
	for i, c := range seq {
		if c <= prev {
			t.Fatalf("growth step %d: cap %d not greater than previous %d", i, c, prev)
		}
		prev = c
	}
}

func TestReallocationsAreLogarithmic(t *testing.T) {
	t.Parallel()
	for _, n := range []int{16, 256, 4096, 65536} {
		got := Reallocations(oneByteRecords(n))
		if bound := LogBound(n); got > bound {
			t.Fatalf("N=%d: %d reallocations exceeds logarithmic bound %d", n, got, bound)
		}
		if got >= n {
			t.Fatalf("N=%d: %d reallocations is not sub-linear", n, got)
		}
	}
}

func TestFinalCapacityHoldsAllBytes(t *testing.T) {
	t.Parallel()
	records := oneByteRecords(100)
	out := Assemble(records)
	if len(out) != 100 {
		t.Fatalf("assembled %d bytes, want 100", len(out))
	}
	if cap(out) < 100 {
		t.Fatalf("final cap %d cannot hold %d bytes", cap(out), len(out))
	}
}

func TestAllocsCountsGrowth(t *testing.T) {
	records := oneByteRecords(1000)
	allocs := testing.AllocsPerRun(50, func() {
		sink := Assemble(records)
		_ = sink
	})
	// An unpreallocated build reallocates several times; each realloc is one
	// allocation. It is far more than one and far less than N.
	if allocs < 2 {
		t.Fatalf("unpreallocated build made %v allocations, expected several", allocs)
	}
	if allocs >= 1000 {
		t.Fatalf("unpreallocated build made %v allocations, expected sub-linear", allocs)
	}
}

func ExampleReallocations() {
	// Growth is logarithmic: 1000 one-byte appends reallocate only a handful
	// of times, never 1000 times.
	n := Reallocations(oneByteRecords(1000))
	fmt.Println(n < 20)
	// Output: true
}
```

## Review

The build is correct when the growth *properties* hold regardless of toolchain:
the capacity sequence is strictly increasing, and the number of reallocations is
logarithmic in N (far below N, and below `LogBound`). Resist asserting a specific
capacity ladder — `[8 16 32]` is what this toolchain does today, but the runtime
is free to change size classes and the ~1.25x large-slice factor, so a test that
pins exact capacities is brittle. `testing.AllocsPerRun` gives you the honest
allocation count of the unpreallocated path; the next exercise removes those
allocations entirely by preallocating. Run `go test -race`.

## Resources

- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices) — how `append` grows and copies.
- [pkg.go.dev: testing.AllocsPerRun](https://pkg.go.dev/testing#AllocsPerRun) — measuring allocations of a hot path.
- [Go runtime growslice source](https://github.com/golang/go/blob/master/src/runtime/slice.go) — the actual size-class-based growth, not a hard 2x.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-preallocated-bulk-insert-args.md](04-preallocated-bulk-insert-args.md)
