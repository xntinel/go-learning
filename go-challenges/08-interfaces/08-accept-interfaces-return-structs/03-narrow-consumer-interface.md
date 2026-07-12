# Exercise 3: Depend On The Narrow Interface The Consumer Actually Needs

A service that only reads should not depend on a repository that also writes and
deletes. This module refactors a read-only reporter to depend on a one-method
`ItemGetter` it defines itself, demonstrating interface segregation and consumer-
owned boundaries: the wide store still satisfies the narrow port structurally, but
a read-only fake no longer has to fake writes.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
narrowiface/                independent module: example.com/narrowiface
  go.mod                    go 1.26
  repo.go                   Item, ErrNotFound; wide Repository; MemoryRepository
  reporter.go               narrow ItemGetter port; PriceReporter depends only on Get
  cmd/
    demo/
      main.go               wires the wide store into the narrow reporter
  reporter_test.go          read-only fake satisfies ItemGetter; wide store does too
```

Files: `repo.go`, `reporter.go`, `cmd/demo/main.go`, `reporter_test.go`.
Implement: a wide `Repository` (`Get`/`Put`/`Delete`) with a `MemoryRepository`, and a `PriceReporter` that depends on a narrow `ItemGetter` (just `Get`).
Test: a test-local fake implementing only `Get` is accepted by `NewPriceReporter`; assert `*MemoryRepository` (which has all methods) also satisfies `ItemGetter` structurally, via a compile-time assertion.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/08-accept-interfaces-return-structs/03-narrow-consumer-interface/cmd/demo
cd go-solutions/08-interfaces/08-accept-interfaces-return-structs/03-narrow-consumer-interface
go mod edit -go=1.26
```

### Why the reporter defines its own one-method port

The store exposes three methods, and that is right for a store. But `PriceReporter`
only ever calls `Get`. If it depended on the whole `Repository`, three costs would
follow: its test double would have to implement `Put` and `Delete` it never uses; a
genuinely read-only backend (a cache, a read replica, a static price list) could not
be passed without pretending to support writes; and a reader of the reporter's type
would have to guess which of the three methods it actually touches. Depending on a
one-method `ItemGetter` erases all three. The fake implements one method. Any
read-only source qualifies. The dependency is legible: this type reads, nothing else.

`ItemGetter` lives in `reporter.go` — the *consumer's* file — not next to the store,
because the consumer owns the interface it needs. `MemoryRepository`, defined in
`repo.go`, knows nothing about `ItemGetter`, yet satisfies it structurally: having a
`Get(string) (Item, error)` method is enough. The compile-time assertion
`var _ ItemGetter = (*MemoryRepository)(nil)` proves the wide type still fits the
narrow port, so a refactor that changed `Get`'s signature would break the build here
rather than at a call site. This is interface segregation in the small: several
narrow role interfaces, each owned by the code that uses it, in place of one fat port
reused everywhere.

Create `repo.go`:

```go
package narrowiface

import (
	"errors"
	"sync"
)

var ErrNotFound = errors.New("narrowiface: item not found")

type Item struct {
	ID    string
	Name  string
	Price int64
}

// Repository is the full store surface. It is the right shape for a store, but a
// heavy dependency for a consumer that only reads.
type Repository interface {
	Get(id string) (Item, error)
	Put(item Item) error
	Delete(id string) error
}

type MemoryRepository struct {
	mu    sync.RWMutex
	items map[string]Item
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{items: make(map[string]Item)}
}

var _ Repository = (*MemoryRepository)(nil)

func (m *MemoryRepository) Get(id string) (Item, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.items[id]
	if !ok {
		return Item{}, ErrNotFound
	}
	return item, nil
}

func (m *MemoryRepository) Put(item Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[item.ID] = item
	return nil
}

func (m *MemoryRepository) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		return ErrNotFound
	}
	delete(m.items, id)
	return nil
}
```

Create `reporter.go`. Note `ItemGetter` is defined here, in the consumer, and names
only the one method the reporter uses:

```go
package narrowiface

import "fmt"

// ItemGetter is the narrow, consumer-owned port. PriceReporter depends on this
// single method, not on the full Repository. Any read-only source satisfies it.
type ItemGetter interface {
	Get(id string) (Item, error)
}

// PriceReporter renders a human price line for an item. It only reads, so it only
// depends on ItemGetter.
type PriceReporter struct {
	src ItemGetter
}

// NewPriceReporter accepts the narrow interface and returns the struct.
func NewPriceReporter(src ItemGetter) *PriceReporter {
	return &PriceReporter{src: src}
}

// Report returns a formatted price line, or the underlying error (e.g. ErrNotFound).
func (r *PriceReporter) Report(id string) (string, error) {
	item, err := r.src.Get(id)
	if err != nil {
		return "", err
	}
	dollars := item.Price / 100
	cents := item.Price % 100
	return fmt.Sprintf("%s: $%d.%02d", item.Name, dollars, cents), nil
}
```

### The runnable demo

The demo shows the wide store flowing into the narrow reporter: `MemoryRepository`
has three methods, but where a `PriceReporter` is built it is seen only as an
`ItemGetter`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/narrowiface"
)

func main() {
	store := narrowiface.NewMemoryRepository()
	_ = store.Put(narrowiface.Item{ID: "sku-1", Name: "widget", Price: 1299})

	// The wide store satisfies the narrow ItemGetter; the reporter sees only Get.
	reporter := narrowiface.NewPriceReporter(store)

	line, err := reporter.Report("sku-1")
	if err != nil {
		fmt.Println("report:", err)
		return
	}
	fmt.Println(line)

	if _, err := reporter.Report("missing"); err != nil {
		fmt.Println("missing:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
widget: $12.99
missing: narrowiface: item not found
```

### Tests

The key test injects a `readOnlyPrices` fake that implements *only* `Get` — it
cannot `Put` or `Delete`, and it does not need to, because `PriceReporter` depends on
`ItemGetter`. A second compile-time assertion proves the wide `*MemoryRepository`
also satisfies the narrow port, so both a read-only source and the full store are
interchangeable at the reporter's boundary.

Create `reporter_test.go`:

```go
package narrowiface

import (
	"errors"
	"testing"
)

// readOnlyPrices implements ONLY Get. It is a valid ItemGetter and could not be a
// Repository (no Put/Delete) — the whole point of segregating the port.
type readOnlyPrices struct {
	prices map[string]Item
}

func (r readOnlyPrices) Get(id string) (Item, error) {
	item, ok := r.prices[id]
	if !ok {
		return Item{}, ErrNotFound
	}
	return item, nil
}

// compile-time proof: the narrow fake satisfies the narrow port...
var _ ItemGetter = readOnlyPrices{}

// ...and so does the wide concrete store, structurally, without importing the port.
var _ ItemGetter = (*MemoryRepository)(nil)

func TestReporterAcceptsReadOnlySource(t *testing.T) {
	t.Parallel()
	src := readOnlyPrices{prices: map[string]Item{
		"sku-1": {ID: "sku-1", Name: "widget", Price: 1299},
	}}
	reporter := NewPriceReporter(src)

	line, err := reporter.Report("sku-1")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if line != "widget: $12.99" {
		t.Fatalf("Report = %q, want %q", line, "widget: $12.99")
	}
}

func TestReporterAcceptsWideStore(t *testing.T) {
	t.Parallel()
	store := NewMemoryRepository()
	_ = store.Put(Item{ID: "sku-2", Name: "gadget", Price: 500})
	reporter := NewPriceReporter(store) // wide store, narrow view

	line, err := reporter.Report("sku-2")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if line != "gadget: $5.00" {
		t.Fatalf("Report = %q, want %q", line, "gadget: $5.00")
	}
}

func TestReporterPropagatesNotFound(t *testing.T) {
	t.Parallel()
	reporter := NewPriceReporter(readOnlyPrices{prices: map[string]Item{}})
	if _, err := reporter.Report("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Report(missing) err = %v, want ErrNotFound", err)
	}
}
```

## Review

The refactor is correct when the reporter depends on `ItemGetter`, not `Repository`,
and both a `Get`-only fake and the full `*MemoryRepository` satisfy that port — the
two compile-time assertions pin exactly this. The value is concrete: the read-only
fake in the test implements one method rather than three, and a real read replica or
static price list could be dropped in with no fake writes. The mistake this module
guards against is the fat interface — injecting a six-method `Repository` into a
service that calls one method, forcing every double and every alternate backend to
stub the rest. Segregate the port at the consumer, size it to the calls the consumer
actually makes, and let the wide concrete type satisfy it structurally.

## Resources

- [Go Proverbs: The bigger the interface, the weaker the abstraction](https://go-proverbs.github.io/) — Rob Pike on narrow interfaces.
- [`io` package](https://pkg.go.dev/io#Reader) — `Reader`, `Writer`, `Closer`: the canonical one-method role interfaces composed where needed.
- [SOLID Go Design (Dave Cheney)](https://dave.cheney.net/2016/08/20/solid-go-design) — interface segregation applied to Go.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-inject-mock-repository.md](02-inject-mock-repository.md) | Next: [04-caching-decorator.md](04-caching-decorator.md)
