# Exercise 7: Secondary / inverted index over an in-memory repository

An in-memory repository — the kind that backs a test double or a small service —
stores entities in a `map[ID]Entity`. Looking one up by primary key is O(1), but
"give me all orders for this customer" is an O(n) scan unless you maintain a
secondary index: a second map from the field value to the list of matching IDs.
The catch is that the index is *derived* state; it must be updated in lockstep
with the primary store or it drifts and returns deleted rows. This module builds
the index and keeps it honest.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
repo/                      independent module: example.com/repo
  go.mod
  repo.go                  type Repository; Insert, Delete, ByCustomer, DeleteCustomer
  cmd/
    demo/
      main.go              index orders by customer, query, delete
  repo_test.go             query by field, delete prunes index, re-insert no dup
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: a `Repository` holding `map[ID]Order` plus a secondary index `map[customerID][]orderID`, kept consistent on `Insert`/`Delete`, with `ByCustomer(value)` in O(1)+O(k).
- Test: insert then query returns all matching IDs (order-independent), delete removes the ID from the index and drops the key when the slice empties, and re-inserting after delete does not duplicate.
- Verify: `go test -count=1 -race ./...`

### The index is derived state — update it in the same critical section

The primary store is `map[orderID]Order`. The secondary index is
`map[customerID][]orderID`: for each customer, the list of their order IDs. A
query by customer becomes an O(1) map lookup for the ID list plus O(k) to gather
the k orders — no scan of the whole store. The price is that every mutation of the
primary store must mutate the index too, atomically with it. `Insert` adds the new
ID to the customer's slice (and, if an existing order's customer changed, moves it
between slices). `Delete` removes the ID from the customer's slice.

The detail that separates a correct index from a leaky one is cleanup. When you
remove the last order for a customer, the customer's slice becomes empty — and if
you leave the empty slice (and its key) in the index, the index accumulates dead
entries. Because a Go map never shrinks its bucket array, those empty entries are a
slow memory leak. So `unindex` prunes the key entirely when its slice empties,
using `slices.Index` to find the ID and `slices.Delete` to remove it. Re-inserting
after a delete must not create a duplicate ID in the slice — `Insert` distinguishes
a brand-new ID (index it) from an update to an existing one (do not re-index unless
the customer changed).

Create `repo.go`:

```go
package repo

import (
	"cmp"
	"maps"
	"slices"
)

// Order is the entity stored in the repository.
type Order struct {
	ID         string
	CustomerID string
	Amount     int
}

// Repository stores orders and maintains a secondary index from customer ID to
// that customer's order IDs, so ByCustomer is O(1)+O(k) instead of an O(n) scan.
type Repository struct {
	orders map[string]Order
	byCust map[string][]string
}

// New returns an empty repository.
func New() *Repository {
	return &Repository{
		orders: make(map[string]Order),
		byCust: make(map[string][]string),
	}
}

// Insert stores o and keeps the index consistent. Re-inserting an existing ID
// updates the order without duplicating its index entry; if the customer
// changed, the index entry moves.
func (r *Repository) Insert(o Order) {
	if existing, ok := r.orders[o.ID]; ok {
		if existing.CustomerID != o.CustomerID {
			r.unindex(existing.CustomerID, o.ID)
			r.index(o.CustomerID, o.ID)
		}
		r.orders[o.ID] = o
		return
	}
	r.orders[o.ID] = o
	r.index(o.CustomerID, o.ID)
}

// Delete removes the order with the given ID and drops it from the index.
func (r *Repository) Delete(id string) {
	o, ok := r.orders[id]
	if !ok {
		return
	}
	delete(r.orders, id)
	r.unindex(o.CustomerID, id)
}

// DeleteCustomer removes every order belonging to a customer.
func (r *Repository) DeleteCustomer(customerID string) {
	maps.DeleteFunc(r.orders, func(_ string, o Order) bool {
		return o.CustomerID == customerID
	})
	delete(r.byCust, customerID)
}

// ByCustomer returns the customer's orders, sorted by ID for determinism. Cost is
// an O(1) index lookup plus O(k) to gather the k matches.
func (r *Repository) ByCustomer(customerID string) []Order {
	ids := r.byCust[customerID]
	out := make([]Order, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.orders[id])
	}
	slices.SortFunc(out, func(a, b Order) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

func (r *Repository) index(customerID, id string) {
	r.byCust[customerID] = append(r.byCust[customerID], id)
}

// unindex removes id from the customer's slice and prunes the key when the slice
// empties, so the index does not accumulate empty entries.
func (r *Repository) unindex(customerID, id string) {
	ids := r.byCust[customerID]
	if i := slices.Index(ids, id); i >= 0 {
		ids = slices.Delete(ids, i, i+1)
	}
	if len(ids) == 0 {
		delete(r.byCust, customerID)
	} else {
		r.byCust[customerID] = ids
	}
}
```

### The runnable demo

The demo indexes three orders across two customers, queries one customer, then
deletes an order and re-queries to show the index staying in sync.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/repo"
)

func main() {
	r := repo.New()
	r.Insert(repo.Order{ID: "o-1", CustomerID: "alice", Amount: 100})
	r.Insert(repo.Order{ID: "o-2", CustomerID: "bob", Amount: 50})
	r.Insert(repo.Order{ID: "o-3", CustomerID: "alice", Amount: 75})

	fmt.Println("alice orders:")
	for _, o := range r.ByCustomer("alice") {
		fmt.Printf("  %s $%d\n", o.ID, o.Amount)
	}

	r.Delete("o-1")
	fmt.Println("after deleting o-1, alice orders:")
	for _, o := range r.ByCustomer("alice") {
		fmt.Printf("  %s $%d\n", o.ID, o.Amount)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alice orders:
  o-1 $100
  o-3 $75
after deleting o-1, alice orders:
  o-3 $75
```

### Tests

The delete test asserts both halves of correct cleanup: the order is gone from the
query *and* the empty index key is pruned (checked by reaching into the unexported
`byCust` map, which same-package tests can do). The re-insert test proves an update
does not duplicate the ID in the index.

Create `repo_test.go`:

```go
package repo

import (
	"fmt"
	"slices"
	"testing"
)

func ids(orders []Order) []string {
	out := make([]string, len(orders))
	for i, o := range orders {
		out[i] = o.ID
	}
	return out
}

func TestInsertThenQueryByCustomer(t *testing.T) {
	t.Parallel()

	r := New()
	r.Insert(Order{ID: "o-1", CustomerID: "alice"})
	r.Insert(Order{ID: "o-2", CustomerID: "bob"})
	r.Insert(Order{ID: "o-3", CustomerID: "alice"})

	got := ids(r.ByCustomer("alice"))
	if want := []string{"o-1", "o-3"}; !slices.Equal(got, want) {
		t.Fatalf("ByCustomer(alice) = %v, want %v", got, want)
	}
}

func TestDeletePrunesIndexKey(t *testing.T) {
	t.Parallel()

	r := New()
	r.Insert(Order{ID: "o-1", CustomerID: "alice"})
	r.Delete("o-1")

	if got := r.ByCustomer("alice"); len(got) != 0 {
		t.Fatalf("ByCustomer(alice) = %v after delete, want empty", ids(got))
	}
	if _, ok := r.byCust["alice"]; ok {
		t.Fatal("empty index key for alice was not pruned")
	}
}

func TestReinsertDoesNotDuplicate(t *testing.T) {
	t.Parallel()

	r := New()
	r.Insert(Order{ID: "o-1", CustomerID: "alice", Amount: 100})
	r.Insert(Order{ID: "o-1", CustomerID: "alice", Amount: 200}) // update in place

	got := r.ByCustomer("alice")
	if len(got) != 1 {
		t.Fatalf("ByCustomer(alice) has %d entries, want 1 (no duplicate)", len(got))
	}
	if got[0].Amount != 200 {
		t.Fatalf("Amount = %d, want 200 (updated)", got[0].Amount)
	}
}

func TestReindexOnCustomerChange(t *testing.T) {
	t.Parallel()

	r := New()
	r.Insert(Order{ID: "o-1", CustomerID: "alice"})
	r.Insert(Order{ID: "o-1", CustomerID: "bob"}) // order moved to bob

	if got := r.ByCustomer("alice"); len(got) != 0 {
		t.Fatalf("alice still has %v after order moved", ids(got))
	}
	if got := ids(r.ByCustomer("bob")); !slices.Equal(got, []string{"o-1"}) {
		t.Fatalf("bob orders = %v, want [o-1]", got)
	}
}

func TestDeleteCustomerRemovesAll(t *testing.T) {
	t.Parallel()

	r := New()
	r.Insert(Order{ID: "o-1", CustomerID: "alice"})
	r.Insert(Order{ID: "o-2", CustomerID: "alice"})
	r.DeleteCustomer("alice")

	if got := r.ByCustomer("alice"); len(got) != 0 {
		t.Fatalf("alice has %v after DeleteCustomer", ids(got))
	}
}

func Example() {
	r := New()
	r.Insert(Order{ID: "o-1", CustomerID: "alice", Amount: 100})
	r.Insert(Order{ID: "o-2", CustomerID: "alice", Amount: 75})
	fmt.Println(len(r.ByCustomer("alice")))
	// Output: 2
}
```

## Review

The index is correct when it never drifts from the primary store: every `Insert`
and `Delete` updates both, an update does not duplicate an ID, and removing the
last order for a customer prunes the index key rather than leaving an empty slice.
The mistake to avoid is treating the index as fire-and-forget — updating the store
but forgetting the index (a query then returns a deleted row), or removing the ID
but leaving the empty key (the index grows without bound, and maps never shrink).
Note this repository is not concurrency-safe by design; a shared instance would
wrap these methods in a `sync.RWMutex`, updating store and index inside the same
locked section so they never diverge. Run `go test -count=1 -race ./...`.

## Resources

- [`slices` package](https://pkg.go.dev/slices) — `slices.Index`, `slices.Delete`, `slices.SortFunc`, `slices.Equal`.
- [`maps` package](https://pkg.go.dev/maps) — `maps.DeleteFunc` for bulk removal.
- [`cmp` package](https://pkg.go.dev/cmp) — `cmp.Compare` for the stable sort.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-sharded-map.md](06-sharded-map.md) | Next: [08-idempotency-store.md](08-idempotency-store.md)
