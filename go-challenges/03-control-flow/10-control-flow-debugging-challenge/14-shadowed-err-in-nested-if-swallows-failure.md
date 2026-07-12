# Exercise 14: The Inventory Updater Whose Shadowed err Hid Every Save Failure

**Nivel: Intermedio** — validacion rapida (un test corto).

An inventory updater looks up an item, adjusts its quantity, and saves it
back. It shipped with an outer `var err error` that was never actually
assigned: both the lookup and the save used `:=` inside nested `if`
statements, which declared *new*, block-scoped `err` variables that shadowed
the outer one. The function's final `return err` returned the outer variable
— permanently `nil` — no matter what the save call reported. You will
reproduce it with a forced save failure, diagnose the shadowing, and fix it
by returning directly from the scope that owns each error.

## What you'll build

```text
inventory/                  module example.com/inventory
  go.mod
  inventory.go              Store, Item, UpdateInventory
  inventory_test.go          forced save failure + success-path assertion
```

- Files: `inventory.go`, `inventory_test.go`.
- Implement: `UpdateInventory(*Store, sku string, delta int) error` that propagates both a lookup failure and a save failure to the caller.
- Test: force `Store.SaveErr`, call `UpdateInventory`, and assert the returned error wraps it via `errors.Is`; a second test asserts the quantity is actually updated on success.
- Verify: `go test -count=1 ./...`.

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/14-shadowed-err-in-nested-if-swallows-failure
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/14-shadowed-err-in-nested-if-swallows-failure
```

### The artifact and the planted bug

```go
func UpdateInventory(s *Store, sku string, delta int) error {
	var err error
	if item, err := s.Get(sku); err != nil { // := shadows the outer err
		return err
	} else {
		item.Qty += delta
		if err := s.Save(item); err != nil { // := shadows it again
			err = fmt.Errorf("save %s: %w", sku, err) // only the inner shadow changes
		}
	}
	return err // BUG: outer err, never assigned, always nil
}
```

`if item, err := s.Get(sku); err != nil` declares a *new* `item` and a *new*
`err`, scoped to the `if`/`else` block — it does not assign the outer `err`
declared by `var err error` one line above, even though both are spelled
`err`. The nested `if err := s.Save(item); err != nil` block does the same
thing a second time: `err = fmt.Errorf(...)` inside it reassigns that
innermost shadow, which then goes out of scope the moment the block ends.
The outer `err` — the one actually returned — is never touched by either
check, so `return err` at the bottom always returns `nil`, even when the save
call genuinely failed. It compiles cleanly (the outer `err` is "used" by the
final return, so `go vet` sees no unused variable) and passes any test that
only checks the happy path, which is exactly how it reached production.

The failing assertion reads:

```text
--- FAIL: TestUpdateInventoryPropagatesSaveFailure
    inventory_test.go:15: UpdateInventory error = nil, want wrapped "disk full"
```

The fix drops the outer `var err error` entirely and returns directly from
the scope where each error is actually produced, so there is no shadow to
lose track of:

```go
func UpdateInventory(s *Store, sku string, delta int) error {
	item, err := s.Get(sku)
	if err != nil {
		return err
	}
	item.Qty += delta
	if err := s.Save(item); err != nil {
		return fmt.Errorf("save %s: %w", sku, err)
	}
	return nil
}
```

Create `inventory.go`:

```go
package inventory

import "fmt"

// Item is a stocked SKU and its on-hand quantity.
type Item struct {
	SKU string
	Qty int
}

// Store is a tiny in-memory inventory store; SaveErr simulates a downstream
// write failure.
type Store struct {
	items   map[string]*Item
	SaveErr error
}

func NewStore() *Store {
	return &Store{items: map[string]*Item{"sku-1": {SKU: "sku-1", Qty: 10}}}
}

func (s *Store) Get(sku string) (*Item, error) {
	it, ok := s.items[sku]
	if !ok {
		return nil, fmt.Errorf("sku %q not found", sku)
	}
	return it, nil
}

func (s *Store) Save(*Item) error { return s.SaveErr }

// UpdateInventory adjusts an item's quantity by delta and persists it,
// propagating any lookup or save failure to the caller.
func UpdateInventory(s *Store, sku string, delta int) error {
	item, err := s.Get(sku)
	if err != nil {
		return err
	}
	item.Qty += delta
	if err := s.Save(item); err != nil {
		return fmt.Errorf("save %s: %w", sku, err)
	}
	return nil
}
```

### Tests

Create `inventory_test.go`:

```go
package inventory

import (
	"errors"
	"testing"
)

func TestUpdateInventoryPropagatesSaveFailure(t *testing.T) {
	s := NewStore()
	wantErr := errors.New("disk full")
	s.SaveErr = wantErr

	err := UpdateInventory(s, "sku-1", 5)
	if err == nil {
		t.Fatalf("UpdateInventory error = nil, want wrapped %q", wantErr)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("UpdateInventory error = %v, want wrapping %q", err, wantErr)
	}
}

func TestUpdateInventoryAppliesDeltaOnSuccess(t *testing.T) {
	s := NewStore()
	if err := UpdateInventory(s, "sku-1", 5); err != nil {
		t.Fatalf("UpdateInventory error = %v, want nil", err)
	}
	it, _ := s.Get("sku-1")
	if it.Qty != 15 {
		t.Fatalf("Qty = %d, want 15", it.Qty)
	}
}
```

Run: `go test -count=1 ./...`.

## Review

`:=` inside an `if` init clause or a nested block always declares fresh
variables for any name that is not already declared in *that exact* scope —
it does not matter that an outer variable with the same name exists one line
up. The tell is a named outer variable (`var err error`) that only ever
appears at declaration and at the final `return`: if nothing between those
two points visibly assigns it, the shadow is the bug. The fix pattern is to
return directly from the scope where each error is produced instead of
threading a value through an outer variable the compiler will happily let you
shadow. `go vet` will not catch this on its own; only a test that forces the
inner failure and checks the value actually returned closes the gap.

## Resources

- [Go Specification: Declarations and scope](https://go.dev/ref/spec#Declarations_and_scope) — a `:=` inside a nested block scopes new bindings to that block, shadowing any outer identifier of the same name.
- [Effective Go: Redeclaration and reassignment](https://go.dev/doc/effective_go#redeclaration) — when `:=` reassigns versus when it redeclares.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-range-over-map-assumed-ordered-pagination.md](13-range-over-map-assumed-ordered-pagination.md) | Next: [15-request-timeout-select-race.md](15-request-timeout-select-race.md)
