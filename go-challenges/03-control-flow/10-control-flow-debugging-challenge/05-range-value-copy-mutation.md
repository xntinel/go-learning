# Exercise 5: The Inventory Update That Silently Did Nothing

An inventory reconciler ranged over `[]Item` and updated each item's quantity and
status through the loop value variable. It ran without error, logged success, and
changed nothing: the writes hit a copy. You will reproduce with a test showing the
slice unchanged, diagnose the range copy semantics, and fix it by indexing.

## What you'll build

```text
inventory/                 module example.com/inventory
  go.mod
  inventory.go             Item, Status; Reconcile([]Item) in place; ReconcilePtrs([]*Item)
  cmd/demo/
    main.go                runnable demo: reconcile a small catalog and print results
  inventory_test.go        in-place mutation, idempotence, and pointer-slice contrast
```

- Files: `inventory.go`, `cmd/demo/main.go`, `inventory_test.go`.
- Implement: `Reconcile([]Item)` that subtracts reserved units and recomputes status by indexing (`items[i].X = ...`), plus `ReconcilePtrs([]*Item)` to contrast pointer-slice semantics.
- Test: assert the *original* slice reflects the mutations; assert idempotence; a subtest over `[]*Item` showing the range value works there.
- Verify: `go test -count=1 -race ./...`.

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/05-range-value-copy-mutation/cmd/demo
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/05-range-value-copy-mutation
```

### The artifact and the planted bug

The reconciler settles each item: subtract the reserved units from quantity, clear
the reservation, and recompute the stock status. The version that shipped wrote
through the range value variable:

```go
func Reconcile(items []Item) {
	for _, it := range items {
		it.Quantity -= it.Reserved // BUG: it is a copy of the element
		it.Reserved = 0
		switch {
		case it.Quantity <= 0:
			it.Status = StatusBackorder
		case it.Quantity < 10:
			it.Status = StatusLow
		default:
			it.Status = StatusOK
		}
	}
}
```

`for _, it := range items` binds `it` to a *copy* of each element. Every
assignment to `it.Quantity`, `it.Reserved`, `it.Status` mutates that copy and is
discarded when the iteration ends. The backing array of `items` is never touched.
The function returns cleanly, the caller believes stock was reconciled, and the
next read shows the old numbers. This is the single most common silent no-op in Go
slice processing, and it survives review because the code *looks* like it mutates.

The failing test reads:

```text
--- FAIL: TestReconcileMutatesInPlace (0.00s)
    inventory_test.go:33: Reconcile did not mutate the slice
         got [{A 100 5 } {B 8 3 } {C 4 4 }]
        want [{A 95 0 ok} {B 5 0 low} {C 0 0 backorder}]
```

`got` still shows the untouched input — the writes went nowhere. The fix is to
index the slice so the writes land in the backing array.

Create `inventory.go`:

```go
package inventory

// Status is the stock state of an inventory item.
type Status string

const (
	StatusOK        Status = "ok"
	StatusLow       Status = "low"
	StatusBackorder Status = "backorder"
)

// Item is a stock-keeping unit with an on-hand quantity and a reservation.
type Item struct {
	SKU      string
	Quantity int
	Reserved int
	Status   Status
}

// Reconcile settles each item in place: it subtracts the reserved units from the
// on-hand quantity, clears the reservation, and recomputes the status. It writes
// through the slice index items[i], not a range copy, so the mutations persist.
func Reconcile(items []Item) {
	for i := range items {
		items[i].Quantity -= items[i].Reserved
		items[i].Reserved = 0
		switch {
		case items[i].Quantity <= 0:
			items[i].Status = StatusBackorder
		case items[i].Quantity < 10:
			items[i].Status = StatusLow
		default:
			items[i].Status = StatusOK
		}
	}
}

// ReconcilePtrs is the same logic over a slice of pointers. Here the range value
// is a copy of the *Item, but it still addresses the shared struct, so writing
// through it does persist. This contrasts with Reconcile's value slice.
func ReconcilePtrs(items []*Item) {
	for _, it := range items {
		it.Quantity -= it.Reserved
		it.Reserved = 0
		switch {
		case it.Quantity <= 0:
			it.Status = StatusBackorder
		case it.Quantity < 10:
			it.Status = StatusLow
		default:
			it.Status = StatusOK
		}
	}
}
```

The two functions are the lesson in one file: `Reconcile` must index because the
elements are values, while `ReconcilePtrs` may use the range variable because the
elements are pointers and the copied pointer still points at the shared struct.
Clearing `Reserved` to zero also makes the operation idempotent — a second
reconcile subtracts nothing and recomputes the same status.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/inventory"
)

func main() {
	items := []inventory.Item{
		{SKU: "A-100", Quantity: 100, Reserved: 5},
		{SKU: "B-200", Quantity: 8, Reserved: 3},
		{SKU: "C-300", Quantity: 4, Reserved: 4},
	}
	inventory.Reconcile(items)
	for _, it := range items {
		fmt.Printf("%s qty=%d status=%s\n", it.SKU, it.Quantity, it.Status)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
A-100 qty=95 status=ok
B-200 qty=5 status=low
C-300 qty=0 status=backorder
```

### Tests

`TestReconcileMutatesInPlace` is the reproducer: build a slice, reconcile it, and
assert the *original* slice — not a returned copy — holds the new values via
`reflect.DeepEqual`. `TestReconcileIdempotent` runs the reconcile twice and
asserts the second run is a no-op, which only holds because `Reserved` is cleared.
`TestReconcilePtrsMutates` shows the contrast: ranging over `[]*Item` and writing
through the value variable *does* persist.

Create `inventory_test.go`:

```go
package inventory

import (
	"fmt"
	"reflect"
	"testing"
)

func TestReconcileMutatesInPlace(t *testing.T) {
	t.Parallel()

	items := []Item{
		{SKU: "A", Quantity: 100, Reserved: 5},
		{SKU: "B", Quantity: 8, Reserved: 3},
		{SKU: "C", Quantity: 4, Reserved: 4},
	}
	Reconcile(items)

	want := []Item{
		{SKU: "A", Quantity: 95, Reserved: 0, Status: StatusOK},
		{SKU: "B", Quantity: 5, Reserved: 0, Status: StatusLow},
		{SKU: "C", Quantity: 0, Reserved: 0, Status: StatusBackorder},
	}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("Reconcile did not mutate the slice\n got %+v\nwant %+v", items, want)
	}
}

func TestReconcileIdempotent(t *testing.T) {
	t.Parallel()

	items := []Item{{SKU: "A", Quantity: 20, Reserved: 5}}
	Reconcile(items)
	first := items[0]
	Reconcile(items)
	if items[0] != first {
		t.Fatalf("not idempotent: first %+v, then %+v", first, items[0])
	}
}

func TestReconcilePtrsMutates(t *testing.T) {
	t.Parallel()

	a := &Item{SKU: "A", Quantity: 100, Reserved: 5}
	ReconcilePtrs([]*Item{a})
	if a.Quantity != 95 || a.Reserved != 0 || a.Status != StatusOK {
		t.Fatalf("ReconcilePtrs left %+v, want qty=95 reserved=0 status=ok", a)
	}
}

func ExampleReconcile() {
	items := []Item{{SKU: "widget", Quantity: 12, Reserved: 5}}
	Reconcile(items)
	// reading items[0] proves the write reached the backing array
	fmt.Printf("%s qty=%d status=%s\n", items[0].SKU, items[0].Quantity, items[0].Status)
	// Output: widget qty=7 status=low
}
```

## Review

The reconciler is correct when the caller's slice — the one it passed in — shows
the new quantities and statuses after the call. That is why the test asserts on
the original slice, not on a return value: a range value-copy mutation returns
cleanly and changes nothing, so any test that reads the loop variable or a copy
would pass while production silently loses every update. The rule to internalize:
`for _, v := range slice` gives you a copy; index with `slice[i]` to mutate in
place, or range over `[]*T` when you want the loop variable to address the shared
struct. Clearing the reservation is what makes the operation safe to retry.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — range clause and how the iteration value is assigned.
- [Go Blog: Go range loops](https://go.dev/blog/range-functions) — range semantics over collections.
- [reflect.DeepEqual](https://pkg.go.dev/reflect#DeepEqual) — comparing the full slice against the expected result.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-status-class-switch-fallthrough.md](04-status-class-switch-fallthrough.md) | Next: [06-worker-select-cancel-leak.md](06-worker-select-cancel-leak.md)
