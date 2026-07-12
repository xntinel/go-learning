# Exercise 7: Fixture builders — construct valid domain aggregates with overrides

A valid `Order` aggregate — customer, line items, status, totals — is tedious to hand-build in every test, and rebuilding it inline in fifty tests means one schema change breaks all fifty. The test-data-builder (object-mother) pattern gives you one valid default plus functional-option overrides, so each test mutates only the field under test. This module builds that pattern around an order domain.

## What you'll build

```text
order/                        independent module: example.com/orderbuilder
  go.mod                      go 1.26 (requires github.com/google/go-cmp)
  order.go                    Order, Item, Status; Validate; Total; ErrInvalidOrder
  cmd/
    demo/
      main.go                 builds an order via the exported API and prints it
  order_test.go               newOrder(t, opts...) builder; override tests; cmp.Diff
```

Files: `order.go`, `cmd/demo/main.go`, `order_test.go`.
Implement: an `Order` aggregate with `Validate` (wrapping `ErrInvalidOrder`) and `Total`.
Test: a `newOrder(t, opts ...option)` helper that returns a valid default and applies overrides (`withStatus`, `withItems`, `withExtraItem`); tests assert only the varied behavior, an override produces an invalid order, and `cmp.Diff` compares two builds ignoring the volatile `CreatedAt`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/07-test-fixtures-and-testdata/07-test-data-builder/cmd/demo
cd go-solutions/12-testing-ecosystem/07-test-fixtures-and-testdata/07-test-data-builder
go get github.com/google/go-cmp/cmp
```

### Why a builder, and why functional options

The problem a builder solves is fixture drift. When every test constructs a full valid `Order` by hand, the construction is duplicated dozens of times, and a schema change — a new required field, a renamed status — forces a mechanical edit across all of them, with copy-paste divergence guaranteed. Worse, the hand-built fixtures obscure intent: a test about shipping logic that spells out a customer, three line items, and a timestamp buries the one field it actually cares about in boilerplate.

A builder centralizes that drift into one place. `newOrder(t)` returns a valid default order; each test passes only the overrides relevant to it. `newOrder(t, withStatus(StatusPaid))` reads as "a valid order that happens to be paid", and the reader's eye goes straight to the varied field. A schema change touches the one default in the builder, not every test. This is the object-mother pattern, and functional options are the idiomatic Go spelling of it: an `option` is a `func(*Order)`, the builder applies each in turn after establishing the default, so overrides compose and order-independence is natural.

Mark the builder `t.Helper()` so that when an override produces something the builder itself rejects, the failure line points at the calling test, not into the builder. And be careful with slice aliasing: an option that appends to `Items` must clone first (`slices.Clone`) so mutating one built order never disturbs another that shares the backing array. That is the kind of subtle cross-test contamination a shared builder can otherwise introduce.

`cmp.Diff` earns its place here. Two orders built from the same default differ only in `CreatedAt` (a live timestamp); comparing them with `cmp.Diff` under `cmpopts.IgnoreFields` proves the builder is deterministic in everything else and yields a readable field-level diff when it is not — far better than a boolean `reflect.DeepEqual` that only tells you *that* they differ.

Create `order.go`:

```go
package order

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidOrder wraps every domain-rule violation for an order.
var ErrInvalidOrder = errors.New("invalid order")

// Status is an order's lifecycle state.
type Status string

const (
	StatusPending Status = "pending"
	StatusPaid    Status = "paid"
	StatusShipped Status = "shipped"
)

// Item is a single ordered SKU.
type Item struct {
	SKU       string
	Quantity  int
	UnitCents int64
}

// Order is a customer order aggregate.
type Order struct {
	ID        string
	Customer  string
	Status    Status
	Items     []Item
	CreatedAt time.Time
}

// Total is the sum of quantity times unit price across all items, in cents.
func (o Order) Total() int64 {
	var t int64
	for _, it := range o.Items {
		t += int64(it.Quantity) * it.UnitCents
	}
	return t
}

// Validate enforces the order's domain invariants.
func (o Order) Validate() error {
	switch {
	case o.ID == "":
		return fmt.Errorf("%w: missing id", ErrInvalidOrder)
	case o.Customer == "":
		return fmt.Errorf("%w: missing customer", ErrInvalidOrder)
	case len(o.Items) == 0:
		return fmt.Errorf("%w: no items", ErrInvalidOrder)
	}
	for _, it := range o.Items {
		if it.Quantity < 1 {
			return fmt.Errorf("%w: item %s quantity %d", ErrInvalidOrder, it.SKU, it.Quantity)
		}
		if it.UnitCents < 0 {
			return fmt.Errorf("%w: item %s negative price", ErrInvalidOrder, it.SKU)
		}
	}
	switch o.Status {
	case StatusPending, StatusPaid, StatusShipped:
		return nil
	default:
		return fmt.Errorf("%w: status %q", ErrInvalidOrder, o.Status)
	}
}
```

### The runnable demo

The demo constructs an order through the exported API (the builder is a test helper) and prints its total.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/orderbuilder"
)

func main() {
	o := order.Order{
		ID:       "ord-100",
		Customer: "bob",
		Status:   order.StatusPaid,
		Items: []order.Item{
			{SKU: "A", Quantity: 2, UnitCents: 1500},
			{SKU: "B", Quantity: 1, UnitCents: 500},
		},
	}
	if err := o.Validate(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s total=%d status=%s\n", o.ID, o.Total(), o.Status)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ord-100 total=3500 status=paid
```

### The test

The builder and its options live in the test file. Tests read as "a valid order, except X": one overrides status, one overrides items to empty to force a validation failure, one appends an item and asserts only the total moved. The final test compares two builds with `cmp.Diff`, ignoring the volatile `CreatedAt`.

Create `order_test.go`:

```go
package order

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

type option func(*Order)

func withStatus(s Status) option { return func(o *Order) { o.Status = s } }
func withItems(items ...Item) option {
	return func(o *Order) { o.Items = items }
}
func withExtraItem(it Item) option {
	return func(o *Order) { o.Items = append(slices.Clone(o.Items), it) }
}

// newOrder returns a valid default order with the given overrides applied.
func newOrder(t *testing.T, opts ...option) Order {
	t.Helper()
	o := Order{
		ID:        "ord-001",
		Customer:  "cust-alice",
		Status:    StatusPending,
		Items:     []Item{{SKU: "SKU-1", Quantity: 2, UnitCents: 1500}},
		CreatedAt: time.Now().UTC(),
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

func TestBuilderDefaultIsValid(t *testing.T) {
	t.Parallel()
	if err := newOrder(t).Validate(); err != nil {
		t.Fatalf("default order invalid: %v", err)
	}
}

func TestOverrideStatus(t *testing.T) {
	t.Parallel()
	o := newOrder(t, withStatus(StatusPaid))
	if o.Status != StatusPaid {
		t.Fatalf("status = %q, want paid", o.Status)
	}
	if err := o.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestInvalidViaOverride(t *testing.T) {
	t.Parallel()
	o := newOrder(t, withItems()) // force zero items
	if err := o.Validate(); !errors.Is(err, ErrInvalidOrder) {
		t.Fatalf("Validate err = %v, want ErrInvalidOrder", err)
	}
}

func TestExtraItemMovesOnlyTotal(t *testing.T) {
	t.Parallel()
	base := newOrder(t)
	more := newOrder(t, withExtraItem(Item{SKU: "SKU-2", Quantity: 1, UnitCents: 500}))
	if more.Total() != base.Total()+500 {
		t.Fatalf("total = %d, want %d", more.Total(), base.Total()+500)
	}
}

func TestBuildsAreEqualIgnoringCreatedAt(t *testing.T) {
	t.Parallel()
	a := newOrder(t)
	b := newOrder(t)
	if diff := cmp.Diff(a, b, cmpopts.IgnoreFields(Order{}, "CreatedAt")); diff != "" {
		t.Fatalf("builds differ (-a +b):\n%s", diff)
	}
}

func ExampleOrder_Total() {
	o := Order{Items: []Item{{SKU: "x", Quantity: 3, UnitCents: 200}}}
	fmt.Println(o.Total())
	// Output: 600
}
```

## Review

The builder is doing its job when a test states only what it varies and a schema change lands in exactly one place. The pattern's discipline is small but real: mark the builder `t.Helper()` so failures point at the test; clone slices in append-style options so built orders never share a backing array; and use `cmp.Diff` with `cmpopts.IgnoreFields` for volatile fields instead of a bare `reflect.DeepEqual`, so a mismatch is a readable field diff. The anti-pattern this replaces — rebuilding a full valid aggregate by hand in every test — is what turns one domain change into a dozen broken tests and invites copy-paste fixture drift.

## Resources

- [github.com/google/go-cmp/cmp](https://pkg.go.dev/github.com/google/go-cmp/cmp) — `cmp.Diff` for readable structural comparison.
- [cmp/cmpopts: IgnoreFields](https://pkg.go.dev/github.com/google/go-cmp/cmp/cmpopts#IgnoreFields) — ignoring volatile fields in a diff.
- [slices.Clone](https://pkg.go.dev/slices#Clone) — cloning a slice to avoid shared-backing-array aliasing.

---

Back to [06-normalize-nondeterministic.md](06-normalize-nondeterministic.md) | Next: [08-seed-repository-fixtures.md](08-seed-repository-fixtures.md)
