# Exercise 3: Parallel Batch Enrichment Using the Go 1.25 WaitGroup.Go Idiom

Manual `Add`/`Done` bookkeeping is the number-one `WaitGroup` bug class: an
`Add` that miscounts, a `Done` skipped on an early return, an `Add` placed inside
the goroutine. Go 1.25 added `(*sync.WaitGroup).Go`, which fuses `Add(1)`, the
`go` statement, and `defer Done()` into one call and deletes that whole class of
mistakes. Here you re-implement fan-out with it to enrich a batch of order
records in parallel.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
enrich/                      independent module: example.com/enrich
  go.mod                     go 1.25 (WaitGroup.Go)
  enrich.go                  EnrichBatch([]Order, func(Order) Order) []Order
  cmd/
    demo/
      main.go                enrich a small batch and print totals
  enrich_test.go             every slot written + correct transform, under -race
```

Files: `enrich.go`, `cmd/demo/main.go`, `enrich_test.go`.
Implement: `EnrichBatch(orders []Order, enrich func(Order) Order) []Order` that fills one output slot per goroutine with `WaitGroup.Go`, then `Wait`s and returns the enriched batch.
Test: assert every output slot is written (no zero-value holes) and equals the expected transform; each goroutine writes only its own `out[i]`, so `-race` proves there is no shared write.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/01-your-first-goroutine/03-waitgroup-go-modern-idiom/cmd/demo
cd go-solutions/13-goroutines-and-channels/01-your-first-goroutine/03-waitgroup-go-modern-idiom
go mod edit -go=1.25
```

### Why WaitGroup.Go exists

Read the manual form and count the ways it breaks: `wg.Add(1)` might be `Add(2)`
by mistake, or moved inside the goroutine where `Wait` can race past it; the
`defer wg.Done()` might be a plain `wg.Done()` that an early `return` or a panic
skips, leaving `Wait` deadlocked forever. `wg.Go(f)` does the `Add(1)` in the
calling goroutine (correct ordering), runs `f` in a new goroutine, and runs
`Done` in a `defer` that fires no matter how `f` returns. There is nothing left to
miscount. Prefer it for all new code; the exercises earlier in this lesson show
the manual form precisely because you must be able to read and debug it in
existing codebases.

The enrichment pattern is the canonical safe fan-out write: allocate the output
slice up front with `make([]Order, len(orders))`, and have each goroutine write
*only* `out[i]`. Because the indices are disjoint, there is no shared mutable
write and therefore no data race — no mutex, no atomic needed for the slice
itself. The result is deterministic regardless of completion order: slot `i`
always holds the enrichment of `orders[i]`. Ranging with `for i, o := range
orders` and passing `o` into the closure argument makes each goroutine's input a
per-iteration copy, independent of Go version.

Create `enrich.go`:

```go
package enrich

import "sync"

// Order is a minimal batch record. Enrichment computes Total from Qty*Unit.
type Order struct {
	ID    string
	Qty   int
	Unit  int
	Total int
}

// EnrichBatch enriches every order in parallel, one goroutine per record, and
// returns a new slice in the same order. Each goroutine writes only its own
// output slot, so there is no shared mutable state and no lock is required.
func EnrichBatch(orders []Order, enrich func(Order) Order) []Order {
	out := make([]Order, len(orders))
	var wg sync.WaitGroup
	for i, o := range orders {
		wg.Go(func() {
			out[i] = enrich(o)
		})
	}
	wg.Wait()
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/enrich"
)

func main() {
	orders := []enrich.Order{
		{ID: "A", Qty: 2, Unit: 50},
		{ID: "B", Qty: 1, Unit: 125},
		{ID: "C", Qty: 3, Unit: 10},
	}

	enriched := enrich.EnrichBatch(orders, func(o enrich.Order) enrich.Order {
		o.Total = o.Qty * o.Unit
		return o
	})

	for _, o := range enriched {
		fmt.Printf("%s total=%d\n", o.ID, o.Total)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
A total=100
B total=125
C total=30
```

### Tests

`TestEnrichBatchFillsEverySlot` asserts there are no zero-value holes: every
output `Total` is nonzero and equals `Qty*Unit`, proving each goroutine ran and
wrote its slot. `TestEnrichBatchPreservesOrder` checks the output IDs line up with
the input IDs, proving slot `i` maps to input `i` regardless of completion order.
`TestEnrichBatchEmpty` pins the empty-slice edge case. The `-race` run proves the
disjoint-slot writes are genuinely unshared.

Create `enrich_test.go`:

```go
package enrich

import (
	"fmt"
	"testing"
)

func doubleUnit(o Order) Order {
	o.Total = o.Qty * o.Unit
	return o
}

func TestEnrichBatchFillsEverySlot(t *testing.T) {
	t.Parallel()

	orders := make([]Order, 200)
	for i := range orders {
		orders[i] = Order{ID: fmt.Sprintf("o%d", i), Qty: i + 1, Unit: 2}
	}

	got := EnrichBatch(orders, doubleUnit)
	if len(got) != len(orders) {
		t.Fatalf("len = %d, want %d", len(got), len(orders))
	}
	for i, o := range got {
		want := (i + 1) * 2
		if o.Total != want {
			t.Fatalf("slot %d Total = %d, want %d (zero-value hole?)", i, o.Total, want)
		}
	}
}

func TestEnrichBatchPreservesOrder(t *testing.T) {
	t.Parallel()

	orders := []Order{
		{ID: "x", Qty: 1, Unit: 1},
		{ID: "y", Qty: 1, Unit: 1},
		{ID: "z", Qty: 1, Unit: 1},
	}
	got := EnrichBatch(orders, doubleUnit)
	for i := range orders {
		if got[i].ID != orders[i].ID {
			t.Fatalf("slot %d ID = %q, want %q", i, got[i].ID, orders[i].ID)
		}
	}
}

func TestEnrichBatchEmpty(t *testing.T) {
	t.Parallel()

	got := EnrichBatch(nil, doubleUnit)
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func ExampleEnrichBatch() {
	got := EnrichBatch([]Order{{ID: "A", Qty: 2, Unit: 50}}, doubleUnit)
	fmt.Println(got[0].Total)
	// Output: 100
}
```

## Review

The enrichment is correct when there are no zero-value holes and slot `i` always
holds the transform of input `i` — both independent of the order the goroutines
happened to finish in. The pattern that makes this race-free is the disjoint
write: every goroutine touches only `out[i]`, so `-race` stays clean without any
lock. The reason to prefer `wg.Go` over manual `Add`/`Done` is not brevity but
correctness: it makes the miscount and skipped-`Done` bugs unrepresentable. If you
ever see a `Wait` that returns too early or deadlocks, the manual bookkeeping is
the first suspect, and `wg.Go` is the fix.

## Resources

- [sync.WaitGroup.Go](https://pkg.go.dev/sync#WaitGroup.Go)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [Go 1.25 release notes](https://go.dev/doc/go1.25)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-fanout-n-workers-loopvar.md](02-fanout-n-workers-loopvar.md) | Next: [04-per-request-audit-dispatch-capture.md](04-per-request-audit-dispatch-capture.md)
