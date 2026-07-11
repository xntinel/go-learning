# Exercise 3: A Repository That Returns Copies To Stop Callers Mutating Stored State

An in-memory repository that returns its stored `*Order` hands every caller a
mutable handle on the store's own data. This exercise shows the aliasing bug that
causes — including the shallow-copy trap where even `return *stored` still shares
the `Items` slice — and then the fix: a deep copy that clones the slice fields so a
caller's mutations cannot reach the store.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
orderrepo/                independent module: example.com/orderrepo
  go.mod
  repo.go                 Order{Items []Item}; Repo; GetAliased (buggy); Get (deep copy)
  cmd/
    demo/
      main.go             mutate an aliased result vs a safe result, print the store
  repo_test.go            aliasing hazard documented; deep copy proven isolated under -race
```

Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
Implement: an `Order` with an `Items []Item` field, a `Repo` over `map[string]*Order`, a `GetAliased` that returns the stored pointer (the hazard), and a safe `Get` that returns a deep copy with `Items` cloned.
Test: mutating a `GetAliased` result changes the store (documents the bug); mutating a `Get` result — including its `Items` elements — does NOT change the store; run under `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/orderrepo/cmd/demo
cd ~/go-exercises/orderrepo
go mod init example.com/orderrepo
```

### Aliasing, and why a shallow copy is not enough

A repository that stores `map[string]*Order` and returns the stored pointer is
handing out a live handle on its internal state. The caller thinks it received "the
order"; it actually received a pointer to the store's order, and
`o.Status = "cancelled"` rewrites the stored record. That is accidental aliasing:
two owners (the store and the caller) mutating one struct with no coordination. In a
real service this surfaces as a cache that mysteriously changes, or one request's
edits leaking into another's read.

The naive fix — "return a copy, `return *stored`" — is a trap when the struct has a
slice or map field. A struct value copy is *shallow*: it duplicates the `Items`
slice header (pointer, len, cap) but not the backing array. So the returned copy's
`Items` field points at the *same* array as the stored order's `Items`. The caller
cannot reassign `o.Status` into the store anymore (that field was copied), but
`o.Items[0].SKU = "hacked"` still mutates the shared backing array and corrupts the
stored order. The demonstration below returns the stored pointer directly for the
"aliased" path to make the hazard unmistakable, but the same corruption would occur
through a shallow value copy's `Items` slice.

The real fix is a *deep* copy: shallow-copy the struct, then replace each slice/map
field with a clone. `slices.Clone(o.Items)` allocates a new backing array and copies
the elements, so the returned order's `Items` is independent. Because `Item` here is
a flat value struct (no nested pointers/slices), cloning the one slice is enough; if
`Item` itself held a slice, the clone would have to recurse. After the deep copy, a
caller can mutate the returned order and its items freely without touching the store.

Create `repo.go`:

```go
package orderrepo

import (
	"errors"
	"slices"
	"sync"
)

var ErrNotFound = errors.New("order not found")

type Item struct {
	SKU string
	Qty int
}

// Order has a slice field, so a shallow struct copy still shares the backing array.
type Order struct {
	ID     string
	Status string
	Items  []Item
}

type Repo struct {
	mu sync.RWMutex
	m  map[string]*Order
}

func NewRepo() *Repo {
	return &Repo{m: make(map[string]*Order)}
}

// Put stores a deep copy so the caller cannot mutate the stored order afterward
// through the pointer they passed in.
func (r *Repo) Put(o *Order) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[o.ID] = deepCopy(o)
}

// GetAliased returns the stored pointer directly. This is the HAZARD: the caller
// can now mutate the store's own Order. Kept to demonstrate the bug the safe Get
// avoids; do not ship this shape.
func (r *Repo) GetAliased(id string) (*Order, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	o, ok := r.m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return o, nil
}

// Get returns a deep copy: the Order struct is copied and its Items slice is
// cloned, so caller mutations cannot reach the stored order.
func (r *Repo) Get(id string) (*Order, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	o, ok := r.m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return deepCopy(o), nil
}

// deepCopy shallow-copies the struct, then clones the slice field to break the
// backing-array aliasing a plain value copy would leave.
func deepCopy(o *Order) *Order {
	cp := *o // shallow: copies the Items header, not its backing array
	cp.Items = slices.Clone(o.Items)
	return &cp
}
```

### The runnable demo

The demo stores an order, mutates a `GetAliased` result to show it corrupts the
store, then mutates a `Get` result (including an item) to show the store is
untouched.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/orderrepo"
)

func main() {
	r := orderrepo.NewRepo()
	r.Put(&orderrepo.Order{
		ID:     "o1",
		Status: "open",
		Items:  []orderrepo.Item{{SKU: "A", Qty: 1}},
	})

	// Aliased path: mutating the result corrupts the store.
	bad, _ := r.GetAliased("o1")
	bad.Status = "corrupted"
	bad.Items[0].SKU = "X"
	after, _ := r.Get("o1")
	fmt.Printf("after aliased mutation: status=%s sku=%s\n", after.Status, after.Items[0].SKU)

	// Reset and use the safe path.
	r.Put(&orderrepo.Order{
		ID:     "o2",
		Status: "open",
		Items:  []orderrepo.Item{{SKU: "A", Qty: 1}},
	})
	safe, _ := r.Get("o2")
	safe.Status = "cancelled"
	safe.Items[0].SKU = "X"
	stored, _ := r.Get("o2")
	fmt.Printf("after safe mutation: status=%s sku=%s\n", stored.Status, stored.Items[0].SKU)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after aliased mutation: status=corrupted sku=X
after safe mutation: status=open sku=A
```

### Tests

The tests document the hazard and prove the fix. `TestGetAliasedCorruptsStore`
asserts that mutating a `GetAliased` result *does* change the store — pinning the
bug so a future refactor that "helpfully" makes `GetAliased` copy would notice it
changed behavior. `TestGetIsIsolated` mutates both a scalar field and an `Items`
element of a `Get` result and asserts the store is unchanged, proving the slice
clone broke the aliasing. Running under `-race` confirms concurrent readers each get
their own copy.

Create `repo_test.go`:

```go
package orderrepo

import (
	"sync"
	"testing"
)

func seed() *Repo {
	r := NewRepo()
	r.Put(&Order{ID: "o1", Status: "open", Items: []Item{{SKU: "A", Qty: 1}}})
	return r
}

func TestGetAliasedCorruptsStore(t *testing.T) {
	t.Parallel()
	r := seed()

	bad, err := r.GetAliased("o1")
	if err != nil {
		t.Fatal(err)
	}
	bad.Status = "corrupted"
	bad.Items[0].SKU = "X"

	got, _ := r.Get("o1")
	if got.Status != "corrupted" {
		t.Fatalf("aliased path should leak the mutation; status = %q", got.Status)
	}
	if got.Items[0].SKU != "X" {
		t.Fatalf("aliased slice mutation should leak; sku = %q", got.Items[0].SKU)
	}
}

func TestGetIsIsolated(t *testing.T) {
	t.Parallel()
	r := seed()

	safe, err := r.Get("o1")
	if err != nil {
		t.Fatal(err)
	}
	safe.Status = "cancelled"
	safe.Items[0].SKU = "X"
	safe.Items = append(safe.Items, Item{SKU: "B", Qty: 9})

	stored, _ := r.Get("o1")
	if stored.Status != "open" {
		t.Fatalf("store scalar mutated: status = %q, want open", stored.Status)
	}
	if stored.Items[0].SKU != "A" {
		t.Fatalf("store item mutated: sku = %q, want A", stored.Items[0].SKU)
	}
	if len(stored.Items) != 1 {
		t.Fatalf("store items grew: len = %d, want 1", len(stored.Items))
	}
}

func TestPutStoresDeepCopy(t *testing.T) {
	t.Parallel()
	r := NewRepo()
	src := &Order{ID: "o1", Status: "open", Items: []Item{{SKU: "A", Qty: 1}}}
	r.Put(src)

	// Mutating the source after Put must not change the store.
	src.Status = "tampered"
	src.Items[0].SKU = "Z"

	got, _ := r.Get("o1")
	if got.Status != "open" || got.Items[0].SKU != "A" {
		t.Fatalf("Put must deep-copy: got status=%q sku=%q", got.Status, got.Items[0].SKU)
	}
}

func TestConcurrentReadersAreIsolated(t *testing.T) {
	t.Parallel()
	r := seed()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o, err := r.Get("o1")
			if err != nil {
				return
			}
			o.Items[0].Qty = i // each copy is private; no race, no store mutation
		}(i)
	}
	wg.Wait()
	stored, _ := r.Get("o1")
	if stored.Items[0].Qty != 1 {
		t.Fatalf("store qty mutated by a reader: %d, want 1", stored.Items[0].Qty)
	}
}
```

## Review

The repository is safe when a caller cannot reach stored state through a returned
value. The proof is the pair of tests: the aliased path leaks both a scalar and a
slice-element mutation into the store, while the deep-copy `Get` isolates both.
`TestPutStoresDeepCopy` closes the other door — a store that copied on `Get` but
aliased on `Put` would still be corruptible through the pointer the caller passed in.

The subtle mistake is trusting a shallow struct copy. `return *stored` copies the
`Status` field but shares the `Items` backing array, so item-level mutations still
leak. The clone of every slice/map field is what makes the copy deep; for nested
composite fields the clone must recurse. `slices.Clone` is the right tool for the flat
`[]Item` here. The `-race` reader test confirms that because each caller gets a
private copy, concurrent readers never contend on the same order and never touch the
store.

## Resources

- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — allocating a fresh backing array to break slice aliasing.
- [Go Blog: Slices — usage and internals](https://go.dev/blog/slices-intro) — why copying a slice header shares the backing array.
- [Google Go Style: Copying](https://google.github.io/styleguide/go/decisions#copying) — the hazard of copying a value that aliases shared state.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-functional-options-config.md](04-functional-options-config.md)
