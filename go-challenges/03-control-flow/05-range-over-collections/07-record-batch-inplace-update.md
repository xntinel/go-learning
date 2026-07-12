# Exercise 7: Batch-update a Slice of Records — the range value-copy trap

Reconciling a batch of records and writing a status flag back before persisting is
everyday work, and it is where the number-one `range` gotcha bites: `for _, r := range orders { r.Processed = true }`
mutates a *copy* and the write vanishes. This module demonstrates the trap with a
function that documents it, then the correct index form, then a predicate-driven
variant — all backed by tests that pin each behavior.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
batchupdate/                independent module: example.com/batchupdate
  go.mod                    go 1.24
  batchupdate.go            Order; MarkProcessedLost (trap), MarkProcessed, MarkProcessedIf, MarkProcessedAll
  cmd/
    demo/
      main.go               runnable demo: show the lost write vs the real one
  batchupdate_test.go       trap leaves records unchanged; index form flips; predicate; slices.All variant
```

- Files: `batchupdate.go`, `cmd/demo/main.go`, `batchupdate_test.go`.
- Implement: `MarkProcessedLost` (value-copy, documents the trap), `MarkProcessed` (index form), `MarkProcessedIf(pred)`, and `MarkProcessedAll` (via `slices.All`).
- Test: the value-copy form leaves records unchanged; the index form flips `Processed`; the predicate only updates matches; the `slices.All` variant matches the index form.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/07-record-batch-inplace-update/cmd/demo
cd go-solutions/03-control-flow/05-range-over-collections/07-record-batch-inplace-update
go mod edit -go=1.24
```

### Why the value-copy write is lost

`for _, r := range orders` binds `r` to a *copy* of `orders[i]` on each iteration.
Slices of structs hold the struct values inline in the backing array, so `r` is a
by-value snapshot; assigning `r.Processed = true` writes to that local copy and
discards it when the iteration ends. The backing array — the thing you are about to
persist — never changes. This compiles, passes a smoke test that only checks the
return of some unrelated call, and then silently ships a batch job that marks
nothing.

The fix is to write through the index into the backing array:

```go
for i := range orders {
	orders[i].Processed = true
}
```

`orders[i]` is the actual element, not a copy, so the assignment sticks. The index
form is also the cheaper one when `Order` is large: `for _, r := range` copies the
whole struct every iteration even if you only read one field, whereas indexing does
not. A third option is `for i, r := range orders` where you read fields from the
copy `r` for convenience but write through `orders[i]` — just never assign to `r`
expecting it to persist.

The `slices.All(orders)` adapter returns an `iter.Seq2[int, Order]` yielding the
same `(index, valueCopy)` pairs, so the identical rule applies: use the yielded
index to write through, never the yielded value copy. It is shown here to make the
point that the iterator adapters carry the exact same value semantics as the classic
range.

Create `batchupdate.go`:

```go
package batchupdate

import "slices"

// Order is a batch record. It is deliberately more than a couple of words wide to
// make the "copy per iteration" cost concrete.
type Order struct {
	ID        string
	Amount    int
	Processed bool
}

// MarkProcessedLost demonstrates the trap: it ranges by value, so the write lands
// on a copy and orders is left unchanged. Do not use this; it exists to be tested.
func MarkProcessedLost(orders []Order) {
	for _, o := range orders {
		o.Processed = true // written to a copy, discarded
	}
}

// MarkProcessed flips Processed on every order by writing through the index.
func MarkProcessed(orders []Order) {
	for i := range orders {
		orders[i].Processed = true
	}
}

// MarkProcessedIf flips Processed only on orders matching pred. It reads fields
// from the value copy for the predicate but writes through the index.
func MarkProcessedIf(orders []Order, pred func(Order) bool) {
	for i, o := range orders {
		if pred(o) {
			orders[i].Processed = true
		}
	}
}

// MarkProcessedAll is MarkProcessed written with the slices.All adapter, which
// yields (index, valueCopy) pairs: still write through the index.
func MarkProcessedAll(orders []Order) {
	for i := range slices.All(orders) {
		orders[i].Processed = true
	}
}
```

### The runnable demo

The demo runs the trap form and the correct form on identical batches and prints
how many ended up processed, so the lost write is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batchupdate"
)

func batch() []batchupdate.Order {
	return []batchupdate.Order{
		{ID: "o1", Amount: 100},
		{ID: "o2", Amount: 250},
		{ID: "o3", Amount: 75},
	}
}

func countProcessed(orders []batchupdate.Order) int {
	n := 0
	for _, o := range orders {
		if o.Processed {
			n++
		}
	}
	return n
}

func main() {
	lost := batch()
	batchupdate.MarkProcessedLost(lost)
	fmt.Printf("value-copy form processed=%d of %d\n", countProcessed(lost), len(lost))

	ok := batch()
	batchupdate.MarkProcessed(ok)
	fmt.Printf("index form processed=%d of %d\n", countProcessed(ok), len(ok))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
value-copy form processed=0 of 3
index form processed=3 of 3
```

### Tests

The trap test pins that `MarkProcessedLost` leaves every record unprocessed — the
documented, wrong behavior — so nobody "fixes" that function without realizing it is
the counterexample. The index test proves `MarkProcessed` flips all. The predicate
test proves only matching records change. The `slices.All` test proves that variant
matches the index form.

Create `batchupdate_test.go`:

```go
package batchupdate

import "testing"

func sample() []Order {
	return []Order{
		{ID: "o1", Amount: 100},
		{ID: "o2", Amount: 250},
		{ID: "o3", Amount: 75},
	}
}

func processedCount(orders []Order) int {
	n := 0
	for _, o := range orders {
		if o.Processed {
			n++
		}
	}
	return n
}

func TestValueCopyLosesTheWrite(t *testing.T) {
	t.Parallel()
	orders := sample()
	MarkProcessedLost(orders)
	if got := processedCount(orders); got != 0 {
		t.Fatalf("value-copy form marked %d records; the trap should mark 0", got)
	}
}

func TestIndexFormFlipsAll(t *testing.T) {
	t.Parallel()
	orders := sample()
	MarkProcessed(orders)
	if got := processedCount(orders); got != len(orders) {
		t.Fatalf("index form marked %d of %d", got, len(orders))
	}
}

func TestPredicateUpdatesOnlyMatches(t *testing.T) {
	t.Parallel()
	orders := sample()
	MarkProcessedIf(orders, func(o Order) bool { return o.Amount >= 100 })

	if !orders[0].Processed || !orders[1].Processed {
		t.Fatalf("o1/o2 (>=100) should be processed: %+v", orders)
	}
	if orders[2].Processed {
		t.Fatalf("o3 (75) should not be processed: %+v", orders[2])
	}
	if got := processedCount(orders); got != 2 {
		t.Fatalf("processed = %d, want 2", got)
	}
}

func TestSlicesAllMatchesIndexForm(t *testing.T) {
	t.Parallel()
	a := sample()
	b := sample()
	MarkProcessed(a)
	MarkProcessedAll(b)
	for i := range a {
		if a[i].Processed != b[i].Processed {
			t.Fatalf("mismatch at %d: index=%v all=%v", i, a[i].Processed, b[i].Processed)
		}
	}
	if processedCount(b) != len(b) {
		t.Fatalf("slices.All form marked %d of %d", processedCount(b), len(b))
	}
}
```

## Review

The lesson is a single sentence made concrete: you cannot mutate slice elements
through the `range` value copy. `MarkProcessedLost` compiles and leaves the batch
untouched; `MarkProcessed` writes through `orders[i]` and sticks. The predicate
variant shows the common middle ground — read from the copy, write through the index
— and the `slices.All` variant shows the iterator adapters carry the same
semantics. Beyond correctness, the index form avoids copying a wide `Order` every
iteration, which matters in a hot reconciliation loop. Run `go test` and confirm the
trap test still reports zero; if it ever reports non-zero, someone changed the
counterexample.

## Resources

- [Go Specification: For statements (range clause)](https://go.dev/ref/spec#For_range)
- [package slices (All)](https://pkg.go.dev/slices#All)
- [Go Wiki: Range and value copies](https://go.dev/wiki/CommonMistakes)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-retry-backoff-counted-loop.md](06-retry-backoff-counted-loop.md) | Next: [08-ndjson-stream-iterator.md](08-ndjson-stream-iterator.md)
