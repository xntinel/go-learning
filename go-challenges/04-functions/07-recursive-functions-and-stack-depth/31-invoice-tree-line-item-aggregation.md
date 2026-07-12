# Exercise 31: Calculate Invoice Totals from Tree of Line Items

**Nivel: Intermedio** — validacion rapida (un test corto).

An invoice's line items are not always flat: a "bundle" or "package" line
item is itself a small tree, wrapping several other line items under one
catalog ID. Summing an invoice is the natural recurrence — a line item's
total is its own unit price times its quantity, plus the total of every
child — which is exactly what recursion is for. What makes this worth an
exercise rather than a two-line fold is that billing catalogs reuse the
same bundle across many invoices: an "onboarding package" sold to a
thousand customers is the same catalog ID, the same fixed price and
contents, a thousand times over. Recomputing its total by walking its
children on every single invoice is correct but wasteful in exactly the
way that scales badly — a batch job totaling a month's invoices redoes
the same nested sum for every customer who bought the same bundle.

This module is fully self-contained: its own `go mod init`, the
calculator inline, its own demo and tests.

## What you'll build

```text
invoicetree/                  independent module: example.com/invoicetree
  go.mod                        go 1.24
  invoicetree.go                  type LineItem; type Invoice; type Calculator (recursive, memoized by ID)
  invoicetree_test.go              leaf total, nested bundle total, invoice sum, memo reuse across invoices, empty ID, negative quantity, nil item
  cmd/
    demo/
      main.go                     two invoices sharing a catalog bundle, computed with one Calculator
```

- Files: `invoicetree.go`, `cmd/demo/main.go`, `invoicetree_test.go`.
- Implement: `LineItem{ID string; UnitPriceCents int64; Quantity int; Children []*LineItem}`, `Invoice{ID string; Items []*LineItem}`, and `Calculator` with `(*Calculator) ItemTotal(item *LineItem) (int64, error)` and `(*Calculator) InvoiceTotal(inv Invoice) (int64, error)`, memoizing `ItemTotal` by `item.ID`.
- Test: a leaf item's total; a nested bundle's total; an invoice's top-level sum; a shared bundle ID reused across two invoices producing a memo hit; an empty ID, a negative quantity, and a nil item all rejected.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Memoizing by catalog ID, not by pointer

`Calculator.ItemTotal` memoizes on `item.ID` — a string, not the
`*LineItem` pointer. That choice is what makes the memo useful at all
across two different invoices: when an invoice is parsed from its own
JSON payload, its `bundle-onboarding` line item is a freshly allocated
`*LineItem` with the same ID and the same children as any other invoice
that references the same bundle, but it is never the *same* pointer.
Memoizing by pointer would never hit across invoices; memoizing by ID
does, because the ID is what the catalog actually promises is stable. That
promise is also the memo's only real assumption, worth stating plainly:
`ItemTotal` never re-derives a total for an ID it has already seen, so if
two different `*LineItem` values that reach the calculator under the same
ID ever disagreed about price, quantity, or children — a data bug, not
something this package can detect — the second one's true total would
silently never be computed. The design accepts that trade because the
alternative (memoizing by full content, recomputed from an ID-and-content
signature every time) throws away the benefit: verifying an item's content
hasn't changed costs as much as summing it did in the first place.

Create `invoicetree.go`:

```go
// Package invoicetree computes invoice totals from a tree of line items --
// a line item can itself be a bundle wrapping other line items, nested to
// whatever depth the catalog defines. Billing systems commonly reuse the
// same catalog bundle (a fixed-price "onboarding package", say) across many
// invoices, so a Calculator memoizes each line item's own total by its
// catalog ID: computing a shared bundle's total once and reusing it for
// every invoice that references the same ID, rather than re-summing its
// children on every invoice.
package invoicetree

import (
	"errors"
	"fmt"
)

// LineItem is one node of an invoice's line-item tree. A leaf item has
// Quantity units at UnitPriceCents each; a bundle item additionally
// carries Children, whose totals are summed into its own.
type LineItem struct {
	ID             string
	UnitPriceCents int64
	Quantity       int
	Children       []*LineItem
}

// Invoice is a named set of top-level line items.
type Invoice struct {
	ID    string
	Items []*LineItem
}

// Calculator computes line-item and invoice totals, memoizing each line
// item's total by ID. This assumes -- as most catalog systems do -- that a
// given item ID names a fixed definition: the same ID always means the
// same price, quantity, and children, wherever it is referenced from.
type Calculator struct {
	memo   map[string]int64
	Hits   int
	Misses int
}

// NewCalculator returns a Calculator with an empty memo.
func NewCalculator() *Calculator {
	return &Calculator{memo: make(map[string]int64)}
}

// ItemTotal recursively computes item's own total: its unit price times
// its quantity, plus the total of every child. Results are memoized by
// ID, so a bundle referenced by several invoices (or several times within
// one) is summed once.
func (c *Calculator) ItemTotal(item *LineItem) (int64, error) {
	if item == nil {
		return 0, errors.New("invoicetree: nil line item")
	}
	if item.ID == "" {
		return 0, errors.New("invoicetree: line item has empty ID")
	}
	if item.Quantity < 0 {
		return 0, fmt.Errorf("invoicetree: item %s has negative quantity %d", item.ID, item.Quantity)
	}
	if item.UnitPriceCents < 0 {
		return 0, fmt.Errorf("invoicetree: item %s has negative unit price %d", item.ID, item.UnitPriceCents)
	}

	if total, ok := c.memo[item.ID]; ok {
		c.Hits++
		return total, nil
	}
	c.Misses++

	total := item.UnitPriceCents * int64(item.Quantity)
	for _, child := range item.Children {
		childTotal, err := c.ItemTotal(child)
		if err != nil {
			return 0, err
		}
		total += childTotal
	}
	c.memo[item.ID] = total
	return total, nil
}

// InvoiceTotal sums ItemTotal over every top-level item on inv.
func (c *Calculator) InvoiceTotal(inv Invoice) (int64, error) {
	var total int64
	for _, item := range inv.Items {
		itemTotal, err := c.ItemTotal(item)
		if err != nil {
			return 0, fmt.Errorf("invoicetree: invoice %s: %w", inv.ID, err)
		}
		total += itemTotal
	}
	return total, nil
}
```

### The runnable demo

Two invoices share the same `bundle-onboarding` catalog item, each
constructed as a fresh `*LineItem` tree (as if independently parsed), and
are totaled with one shared `Calculator`. The second invoice's hit count
shows the bundle's total was reused rather than re-summed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/invoicetree"
)

// onboardingBundle returns a fresh *LineItem tree for the shared
// "onboarding package" bundle -- as if each invoice had independently
// deserialized it from its own JSON payload, so it is a distinct pointer
// with the same ID and content each time.
func onboardingBundle() *invoicetree.LineItem {
	return &invoicetree.LineItem{
		ID: "bundle-onboarding",
		Children: []*invoicetree.LineItem{
			{ID: "item-training", UnitPriceCents: 10000, Quantity: 1},
			{ID: "item-setup", UnitPriceCents: 5000, Quantity: 1},
		},
	}
}

func main() {
	calc := invoicetree.NewCalculator()

	invoiceA := invoicetree.Invoice{
		ID: "INV-A",
		Items: []*invoicetree.LineItem{
			onboardingBundle(),
			{ID: "item-support", UnitPriceCents: 3000, Quantity: 2},
		},
	}
	totalA, err := calc.InvoiceTotal(invoiceA)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s total: %d cents (hits=%d misses=%d)\n", invoiceA.ID, totalA, calc.Hits, calc.Misses)

	invoiceB := invoicetree.Invoice{
		ID: "INV-B",
		Items: []*invoicetree.LineItem{
			onboardingBundle(),
			{ID: "item-license", UnitPriceCents: 20000, Quantity: 1},
		},
	}
	totalB, err := calc.InvoiceTotal(invoiceB)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s total: %d cents (hits=%d misses=%d)\n", invoiceB.ID, totalB, calc.Hits, calc.Misses)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
INV-A total: 21000 cents (hits=0 misses=4)
INV-B total: 35000 cents (hits=1 misses=5)
```

### Tests

`TestItemTotalLeaf`, `TestItemTotalNestedBundle`, and
`TestInvoiceTotalSumsTopLevelItems` check the basic recursive contract.
`TestCalculatorMemoizesSharedBundleAcrossInvoices` is the test this
exercise exists for: it mirrors the demo, building the shared bundle as a
genuinely distinct `*LineItem` tree for each invoice, and asserts both
invoice totals are correct *and* that the second invoice records at least
one memo hit. `TestItemTotalRejectsEmptyID`, `TestItemTotalRejectsNegativeQuantity`,
and `TestItemTotalRejectsNilItem` cover the validation an ID-keyed memo
depends on — an empty ID would collide across unrelated items.

Create `invoicetree_test.go`:

```go
package invoicetree

import "testing"

func TestItemTotalLeaf(t *testing.T) {
	t.Parallel()

	c := NewCalculator()
	item := &LineItem{ID: "sku-1", UnitPriceCents: 250, Quantity: 4}
	got, err := c.ItemTotal(item)
	if err != nil {
		t.Fatalf("ItemTotal() error = %v", err)
	}
	if got != 1000 {
		t.Fatalf("ItemTotal() = %d, want 1000", got)
	}
}

func TestItemTotalNestedBundle(t *testing.T) {
	t.Parallel()

	c := NewCalculator()
	bundle := &LineItem{
		ID: "bundle-1",
		Children: []*LineItem{
			{ID: "child-a", UnitPriceCents: 1000, Quantity: 1},
			{ID: "child-b", UnitPriceCents: 500, Quantity: 2},
		},
	}
	got, err := c.ItemTotal(bundle)
	if err != nil {
		t.Fatalf("ItemTotal() error = %v", err)
	}
	if got != 2000 {
		t.Fatalf("ItemTotal() = %d, want 2000 (1000 + 500*2)", got)
	}
}

func TestInvoiceTotalSumsTopLevelItems(t *testing.T) {
	t.Parallel()

	c := NewCalculator()
	inv := Invoice{
		ID: "INV-1",
		Items: []*LineItem{
			{ID: "a", UnitPriceCents: 100, Quantity: 3},
			{ID: "b", UnitPriceCents: 50, Quantity: 1},
		},
	}
	got, err := c.InvoiceTotal(inv)
	if err != nil {
		t.Fatalf("InvoiceTotal() error = %v", err)
	}
	if got != 350 {
		t.Fatalf("InvoiceTotal() = %d, want 350", got)
	}
}

// TestCalculatorMemoizesSharedBundleAcrossInvoices is the test that
// justifies the whole exercise: two invoices referencing the same catalog
// bundle ID (via independently constructed *LineItem trees, as if each
// were parsed from its own JSON payload) must reuse the first invoice's
// computed bundle total on the second.
func TestCalculatorMemoizesSharedBundleAcrossInvoices(t *testing.T) {
	t.Parallel()

	newBundle := func() *LineItem {
		return &LineItem{
			ID: "bundle-onboarding",
			Children: []*LineItem{
				{ID: "item-training", UnitPriceCents: 10000, Quantity: 1},
				{ID: "item-setup", UnitPriceCents: 5000, Quantity: 1},
			},
		}
	}

	c := NewCalculator()
	invA := Invoice{ID: "INV-A", Items: []*LineItem{
		newBundle(),
		{ID: "item-support", UnitPriceCents: 3000, Quantity: 2},
	}}
	totalA, err := c.InvoiceTotal(invA)
	if err != nil {
		t.Fatalf("InvoiceTotal(A) error = %v", err)
	}
	if totalA != 21000 {
		t.Fatalf("InvoiceTotal(A) = %d, want 21000", totalA)
	}
	if c.Hits != 0 {
		t.Fatalf("Hits after first invoice = %d, want 0", c.Hits)
	}

	invB := Invoice{ID: "INV-B", Items: []*LineItem{
		newBundle(),
		{ID: "item-license", UnitPriceCents: 20000, Quantity: 1},
	}}
	totalB, err := c.InvoiceTotal(invB)
	if err != nil {
		t.Fatalf("InvoiceTotal(B) error = %v", err)
	}
	if totalB != 35000 {
		t.Fatalf("InvoiceTotal(B) = %d, want 35000", totalB)
	}
	if c.Hits == 0 {
		t.Fatal("Hits after second invoice = 0, want at least 1 (shared bundle reused)")
	}
}

func TestItemTotalRejectsEmptyID(t *testing.T) {
	t.Parallel()

	c := NewCalculator()
	if _, err := c.ItemTotal(&LineItem{UnitPriceCents: 100, Quantity: 1}); err == nil {
		t.Fatal("expected error for line item with empty ID")
	}
}

func TestItemTotalRejectsNegativeQuantity(t *testing.T) {
	t.Parallel()

	c := NewCalculator()
	if _, err := c.ItemTotal(&LineItem{ID: "x", UnitPriceCents: 100, Quantity: -1}); err == nil {
		t.Fatal("expected error for negative quantity")
	}
}

func TestItemTotalRejectsNilItem(t *testing.T) {
	t.Parallel()

	c := NewCalculator()
	if _, err := c.ItemTotal(nil); err == nil {
		t.Fatal("expected error for nil line item")
	}
}
```

## Review

`ItemTotal` is correct when it produces the same total whether or not a
given ID has been seen before, and `InvoiceTotal` is correct when it sums
exactly the top-level items on an invoice, recursing into bundles
transparently. `TestCalculatorMemoizesSharedBundleAcrossInvoices` is the
test that would fail (with `Hits` stuck at zero, though every total would
still happen to be correct) on a version of this exercise that memoizes
by `*LineItem` pointer instead of by `ID` — a natural-looking choice that
works fine within a single invoice's own tree (where children are only
visited once regardless) but never pays off across invoices, which is the
entire scenario a batch billing job cares about. The trade this design
makes explicit in its own documentation — that memoizing by ID assumes an
ID never changes what it names — is the mistake to watch for in the
other direction: a version of this exercise that tried to be "safer" by
re-verifying a cached item's content on every lookup would eliminate the
benefit entirely, since verifying costs what summing did.

## Resources

- [Go Specification: Function declarations](https://go.dev/ref/spec#Function_declarations)
- [Go Specification: Map types](https://go.dev/ref/spec#Map_types)
- [Wikipedia: Memoization](https://en.wikipedia.org/wiki/Memoization)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-hateoas-link-traversal-bounded.md](30-hateoas-link-traversal-bounded.md) | Next: [32-protobuf-message-nesting-validation.md](32-protobuf-message-nesting-validation.md)
