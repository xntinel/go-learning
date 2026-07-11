# Exercise 10: AoS vs SoA layout for a batch-processing hot loop

A batch of events can be stored as an array of structs (`[]Event`) or as a struct
of arrays (`IDs []uint64; Amounts []int64; Flags []uint8`). When a hot loop
touches one field per element, the columnar layout reads a dense run of exactly
that field, improving cache locality and eliminating per-element padding. This
module implements the same aggregation over both and proves the columnar form is
tighter and gives identical results.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test, and a benchmark that documents the locality win.

## What you'll build

```text
aossoa/                    independent module: example.com/aossoa
  go.mod                   go 1.26
  batch.go                 Event, Batch (AoS), Columns (SoA), SumWhereFlagged, ToColumns
  cmd/
    demo/
      main.go              aggregates both ways; prints per-element sizes
  batch_test.go            SoA per-element <= AoS Sizeof; AoS==SoA results; benchmarks
```

- Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
- Implement: an `Event{ID, Amount, Flag}`, a `Batch []Event` (AoS) and a `Columns` (SoA), both with `SumWhereFlagged`, and `ToColumns` converting AoS to SoA.
- Test: assert the SoA columns' combined per-element byte cost is no larger than the AoS per-element `Sizeof`, that both representations produce identical aggregations (table + fuzz), and add benchmarks documenting the scan.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/aossoa/cmd/demo
cd ~/go-exercises/aossoa
go mod init example.com/aossoa
```

### Why columnar helps a single-field scan

The array-of-structs layout, `[]Event`, is the natural one: each element is a
whole `Event` with its `ID`, `Amount`, and `Flag` side by side. On a 64-bit
platform an `Event` is 24 bytes — 8 for `ID`, 8 for `Amount`, 1 for `Flag`, and 7
of trailing padding so the next element's `ID` stays 8-aligned. A hot loop that
sums `Amount` only where `Flag` is set has to stride 24 bytes per element and
pulls the `ID` and the padding into cache alongside each `Amount` and `Flag` it
actually reads. Most of every cache line it loads is data the loop does not use.

The struct-of-arrays layout stores each field in its own contiguous slice:
`IDs []uint64`, `Amounts []int64`, `Flags []uint8`. The same aggregation now walks
`Flags` (one dense byte per element, 64 per cache line) and, for the flagged ones,
`Amounts` (a dense run of `int64`s). It never touches `IDs` at all, and the
`Flags` column has no per-element padding — a byte per element, not a byte plus
seven. Per element, SoA costs 8 + 8 + 1 = 17 bytes across the three columns versus
24 for the padded AoS struct, and the hot scan touches only the columns it needs.
This is why analytics engines and any code you want the compiler to vectorize use
columnar storage. The trade-off is the mirror image: when you process one whole
element at a time (per-request work touching every field), AoS keeps that
element's fields together on one line and SoA scatters them across three, so AoS
wins there. Choose by the access pattern of the hot path, not by habit.

Create `batch.go`:

```go
// Package aossoa contrasts array-of-structs and struct-of-arrays layouts for the
// same batch aggregation, showing the columnar form's tighter packing.
package aossoa

import "unsafe"

// Event is one record. On a 64-bit platform it is 24 bytes (8+8+1 plus 7 bytes
// of trailing padding to keep the next element's ID 8-aligned).
type Event struct {
	ID     uint64
	Amount int64
	Flag   uint8
}

// Batch is the array-of-structs representation.
type Batch []Event

// SumWhereFlagged sums Amount over the events whose Flag is set.
func (b Batch) SumWhereFlagged() int64 {
	var sum int64
	for i := range b {
		if b[i].Flag != 0 {
			sum += b[i].Amount
		}
	}
	return sum
}

// Columns is the struct-of-arrays (columnar) representation. Each field lives in
// its own contiguous slice, so a single-field scan reads a dense run with no
// per-element padding.
type Columns struct {
	IDs     []uint64
	Amounts []int64
	Flags   []uint8
}

// Len reports the number of events.
func (c Columns) Len() int { return len(c.Flags) }

// SumWhereFlagged sums Amounts over the events whose Flag is set, touching only
// the Flags and Amounts columns (never IDs).
func (c Columns) SumWhereFlagged() int64 {
	var sum int64
	for i := range c.Flags {
		if c.Flags[i] != 0 {
			sum += c.Amounts[i]
		}
	}
	return sum
}

// ToColumns converts an AoS batch to the columnar layout.
func ToColumns(b Batch) Columns {
	c := Columns{
		IDs:     make([]uint64, len(b)),
		Amounts: make([]int64, len(b)),
		Flags:   make([]uint8, len(b)),
	}
	for i := range b {
		c.IDs[i] = b[i].ID
		c.Amounts[i] = b[i].Amount
		c.Flags[i] = b[i].Flag
	}
	return c
}

// AoSPerElement is the per-element byte cost of the AoS layout (with padding).
func AoSPerElement() uintptr { return unsafe.Sizeof(Event{}) }

// SoAPerElement is the combined per-element byte cost of the SoA columns (no
// per-element padding).
func SoAPerElement() uintptr {
	return unsafe.Sizeof(uint64(0)) + unsafe.Sizeof(int64(0)) + unsafe.Sizeof(uint8(0))
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/aossoa"
)

func main() {
	batch := aossoa.Batch{
		{ID: 1, Amount: 100, Flag: 1},
		{ID: 2, Amount: 200, Flag: 0},
		{ID: 3, Amount: 50, Flag: 1},
		{ID: 4, Amount: 999, Flag: 0},
		{ID: 5, Amount: 25, Flag: 1},
	}
	cols := aossoa.ToColumns(batch)

	fmt.Printf("AoS sum: %d\n", batch.SumWhereFlagged())
	fmt.Printf("SoA sum: %d\n", cols.SumWhereFlagged())
	fmt.Printf("per-element bytes: AoS=%d SoA=%d\n", aossoa.AoSPerElement(), aossoa.SoAPerElement())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (on a 64-bit platform):

```
AoS sum: 175
SoA sum: 175
per-element bytes: AoS=24 SoA=17
```

### Tests

The layout test asserts the columnar per-element cost is no larger than the padded
struct's. The equality tests — a table and a fuzz — prove the two representations
compute the same aggregation for any input. The benchmarks scan a large batch each
way; they assert nothing (timing is not a contract) but document the locality
difference under `go test -bench`.

Create `batch_test.go`:

```go
package aossoa

import (
	"testing"
)

func TestSoAIsNoLargerPerElement(t *testing.T) {
	t.Parallel()

	if SoAPerElement() > AoSPerElement() {
		t.Errorf("SoA per-element = %d > AoS per-element = %d; columnar must not be larger", SoAPerElement(), AoSPerElement())
	}
}

func TestAoSAndSoAAgree(t *testing.T) {
	t.Parallel()

	tests := map[string]Batch{
		"empty":     {},
		"none set":  {{ID: 1, Amount: 10, Flag: 0}, {ID: 2, Amount: 20, Flag: 0}},
		"all set":   {{ID: 1, Amount: 10, Flag: 1}, {ID: 2, Amount: 20, Flag: 1}},
		"mixed":     {{ID: 1, Amount: 10, Flag: 1}, {ID: 2, Amount: 20, Flag: 0}, {ID: 3, Amount: 30, Flag: 2}},
		"negatives": {{ID: 1, Amount: -100, Flag: 1}, {ID: 2, Amount: 100, Flag: 1}},
	}
	for name, b := range tests {
		aos := b.SumWhereFlagged()
		soa := ToColumns(b).SumWhereFlagged()
		if aos != soa {
			t.Errorf("%s: AoS = %d, SoA = %d", name, aos, soa)
		}
	}
}

func FuzzAoSEqualsSoA(f *testing.F) {
	f.Add([]byte{1, 1, 2, 0, 3, 1, 0, 5})
	f.Fuzz(func(t *testing.T, data []byte) {
		var b Batch
		for i := 0; i+1 < len(data); i += 2 {
			b = append(b, Event{
				ID:     uint64(i),
				Amount: int64(int8(data[i])),
				Flag:   data[i+1],
			})
		}
		if aos, soa := b.SumWhereFlagged(), ToColumns(b).SumWhereFlagged(); aos != soa {
			t.Errorf("AoS = %d, SoA = %d for %d events", aos, soa, len(b))
		}
	})
}

func makeBatch(n int) Batch {
	b := make(Batch, n)
	for i := range n {
		b[i] = Event{ID: uint64(i), Amount: int64(i), Flag: uint8(i % 2)}
	}
	return b
}

func BenchmarkAoS(b *testing.B) {
	batch := makeBatch(1 << 16)
	b.ResetTimer()
	for range b.N {
		_ = batch.SumWhereFlagged()
	}
}

func BenchmarkSoA(b *testing.B) {
	cols := ToColumns(makeBatch(1 << 16))
	b.ResetTimer()
	for range b.N {
		_ = cols.SumWhereFlagged()
	}
}
```

## Review

Both layouts are correct — the table and fuzz tests prove they compute the same
flagged sum for every input — so the choice is about the access pattern, not
correctness. For a hot loop that touches one field per element, the columnar
struct-of-arrays reads a dense run of exactly that field with no per-element
padding (17 bytes per element across the columns versus 24 for the padded AoS
struct) and leaves the untouched columns out of cache entirely; run `go test
-bench .` to see the scan difference at 64K elements. The mistake is defaulting to
one layout for everything: AoS is better when you process whole elements at a time
(per-request work), SoA when you scan one column across many elements (analytics).
Measure the hot path and pick accordingly.

## Resources

- [Data-oriented design (AoS vs SoA)](https://en.wikipedia.org/wiki/AoS_and_SoA) — the layout contrast and when each wins.
- [Go slices: usage and internals](https://go.dev/blog/slices-intro) — how each column's contiguous backing array makes a single-field scan dense.
- [unsafe.Sizeof](https://pkg.go.dev/unsafe#Sizeof) — measuring the per-element cost of each layout.

---

Back to [09-empty-and-trailing-zero-size-fields.md](09-empty-and-trailing-zero-size-fields.md) | Next: [../10-implementing-stringer/00-concepts.md](../10-implementing-stringer/00-concepts.md)
