# Exercise 2: Querying With Composable Specifications

A repository that only offers `List` forces callers to filter in their own code, and adding a `GetByX` method for every query combination turns the interface into a combinatorial mess. The specification pattern replaces that with one `Find(ctx, spec)` method plus a small algebra of predicates that compose into arbitrarily complex criteria. This exercise builds an in-memory product repository whose only query method takes a specification tree.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
catalog.go               Product, Specification interface, SpecFunc adapter,
                         atomic specs (InCategory/PriceAtMost/InStock),
                         And/Or/Not combinators, ProductRepository.Find
cmd/
  demo/
    main.go              build a spec tree and query the in-memory catalog
catalog_test.go          atomic specs, combinator truth tables, Find integration
```

- Files: `catalog.go`, `cmd/demo/main.go`, `catalog_test.go`.
- Implement: the `Specification` interface and `SpecFunc` adapter, the atomic specs, the `And`/`Or`/`Not` combinators, and `ProductRepository` with `Add` and `Find`.
- Test: truth tables for each combinator plus an integration test that runs a composed spec through `Find`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/05-repository-pattern/02-specification-queries/cmd/demo && cd go-solutions/24-design-patterns-in-go/05-repository-pattern/02-specification-queries
```

### Why a predicate object instead of more methods

The instinct when a caller needs "books under twenty dollars that are in stock" is to add `ListBooksUnderPriceInStock`. The next caller needs a different combination, and the interface grows one method per query shape until it is unmaintainable, and each method is a separate query to write and test. A specification inverts that: instead of the repository knowing every question, the caller hands the repository the question as an object, and the repository's single `Find` method evaluates it.

The `Specification` interface has exactly one method, `IsSatisfiedBy(*Product) bool` — it is a predicate wearing an interface. Because a one-method interface is tedious to satisfy with a named type for every trivial predicate, the `SpecFunc` adapter lets a plain function become a `Specification`, the same trick `http.HandlerFunc` uses to turn a function into an `http.Handler`. Each atomic spec is then a one-liner: `InCategory("books")` returns a `SpecFunc` that compares the category field.

The power comes from the three combinators. `And`, `Or`, and `Not` are themselves specifications that hold other specifications and combine their verdicts, so they nest: `And(InCategory("books"), Or(PriceAtMost(2000), InStock()))` is a tree the repository walks by calling the root's `IsSatisfiedBy`, which recurses. `And` with no arguments is vacuously true and `Or` with no arguments is vacuously false — the standard identities, and the reason the loops are written to start from those defaults. This is the composite pattern applied to predicates: a uniform interface for both leaves (atomic specs) and internal nodes (combinators).

`Find` itself is a guarded full scan: take the read lock, walk every product, keep the ones the spec is satisfied by, sort by SKU for a deterministic result, and return. A `nil` spec means "match everything", which makes `Find(ctx, nil)` a clean equivalent of `List`. The scan returns pointers into the repository's own storage under the read lock; callers must treat them as read-only snapshots, because writing through them would mutate stored state and race other readers. In an in-memory repository this `IsSatisfiedBy`-per-item scan is the whole cost; the deeper payoff of keeping the criterion as a data structure rather than fixed code is that a SQL-backed repository could translate the very same spec tree into a `WHERE` clause and let an index do the work, without the calling code changing at all.

Create `catalog.go`:

```go
package catalog

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// Product is the domain entity stored in the catalog. Price is in cents.
type Product struct {
	SKU      string
	Name     string
	Category string
	Price    int
	InStock  bool
}

// Domain sentinels.
var (
	ErrEmptySKU     = errors.New("catalog: sku is required")
	ErrDuplicateSKU = errors.New("catalog: duplicate sku")
)

// Specification is a predicate over a Product. Atomic specs and the And/Or/Not
// combinators all satisfy it, so a whole criterion is one Specification tree.
type Specification interface {
	IsSatisfiedBy(p *Product) bool
}

// SpecFunc adapts a plain predicate function to the Specification interface,
// the same adapter trick as http.HandlerFunc.
type SpecFunc func(p *Product) bool

func (f SpecFunc) IsSatisfiedBy(p *Product) bool { return f(p) }

// InCategory matches products in the given category.
func InCategory(category string) Specification {
	return SpecFunc(func(p *Product) bool { return p.Category == category })
}

// PriceAtMost matches products priced at or below max (cents).
func PriceAtMost(max int) Specification {
	return SpecFunc(func(p *Product) bool { return p.Price <= max })
}

// InStock matches products currently in stock.
func InStock() Specification {
	return SpecFunc(func(p *Product) bool { return p.InStock })
}

type andSpec struct{ specs []Specification }

// And is satisfied only when every child spec is. With no children it is
// vacuously true.
func And(specs ...Specification) Specification { return andSpec{specs} }

func (a andSpec) IsSatisfiedBy(p *Product) bool {
	for _, s := range a.specs {
		if !s.IsSatisfiedBy(p) {
			return false
		}
	}
	return true
}

type orSpec struct{ specs []Specification }

// Or is satisfied when any child spec is. With no children it is vacuously
// false.
func Or(specs ...Specification) Specification { return orSpec{specs} }

func (o orSpec) IsSatisfiedBy(p *Product) bool {
	for _, s := range o.specs {
		if s.IsSatisfiedBy(p) {
			return true
		}
	}
	return false
}

type notSpec struct{ spec Specification }

// Not inverts a specification.
func Not(s Specification) Specification { return notSpec{s} }

func (n notSpec) IsSatisfiedBy(p *Product) bool { return !n.spec.IsSatisfiedBy(p) }

// ProductRepository is an in-memory catalog queried by Specification.
type ProductRepository struct {
	mu       sync.RWMutex
	products map[string]*Product
}

// NewProductRepository returns a ready-to-use in-memory catalog.
func NewProductRepository() *ProductRepository {
	return &ProductRepository{products: make(map[string]*Product)}
}

// Add stores a product, rejecting an empty or duplicate SKU.
func (r *ProductRepository) Add(ctx context.Context, p *Product) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || p.SKU == "" {
		return ErrEmptySKU
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.products[p.SKU]; exists {
		return ErrDuplicateSKU
	}
	r.products[p.SKU] = p
	return nil
}

// Find returns every stored product the spec is satisfied by, sorted by SKU.
// A nil spec matches everything. Returned pointers are read-only snapshots.
func (r *ProductRepository) Find(ctx context.Context, spec Specification) ([]*Product, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Product
	for _, p := range r.products {
		if spec == nil || spec.IsSatisfiedBy(p) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SKU < out[j].SKU })
	return out, nil
}
```

`InCategory`, `PriceAtMost`, and `InStock` return `Specification` values built from `SpecFunc`, so they slot into `And`/`Or`/`Not` exactly like the combinators do — there is no distinction between a leaf and a node from `Find`'s point of view, which is the property that makes the tree work.

### The runnable demo

The demo loads a small catalog and runs one composed query: in-stock books priced at or below twenty dollars (2000 cents). It prints the matching SKUs in order, so the output is the proof that the spec tree filtered correctly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/specification-queries"
)

func main() {
	ctx := context.Background()
	repo := catalog.NewProductRepository()

	_ = repo.Add(ctx, &catalog.Product{SKU: "b1", Name: "Go in Action", Category: "books", Price: 1800, InStock: true})
	_ = repo.Add(ctx, &catalog.Product{SKU: "b2", Name: "The Go PL", Category: "books", Price: 3500, InStock: true})
	_ = repo.Add(ctx, &catalog.Product{SKU: "b3", Name: "Used Paperback", Category: "books", Price: 900, InStock: false})
	_ = repo.Add(ctx, &catalog.Product{SKU: "m1", Name: "Mug", Category: "merch", Price: 1200, InStock: true})

	cheapBooksInStock := catalog.And(
		catalog.InCategory("books"),
		catalog.PriceAtMost(2000),
		catalog.InStock(),
	)

	results, _ := repo.Find(ctx, cheapBooksInStock)
	fmt.Printf("matches: %d\n", len(results))
	for _, p := range results {
		fmt.Printf("%s %s %d\n", p.SKU, p.Name, p.Price)
	}

	all, _ := repo.Find(ctx, nil)
	fmt.Printf("total in catalog: %d\n", len(all))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
matches: 1
b1 Go in Action 1800
total in catalog: 4
```

The composed query matches one product; `Find(ctx, nil)` returns the whole four-product catalog, which is the `List`-equivalent the nil spec provides.

### Tests

The tests cover two layers. The truth-table tests verify each combinator in isolation against trivial always-true and always-false specs, which is where the vacuous-`And`/vacuous-`Or` identities and the `Not` inversion are pinned. The integration test loads a catalog and runs a composed spec through `Find`, asserting both the count and the exact SKUs so an off-by-one in the filter is caught.

Create `catalog_test.go`:

```go
package catalog

import (
	"context"
	"testing"
)

func alwaysTrue() Specification  { return SpecFunc(func(*Product) bool { return true }) }
func alwaysFalse() Specification { return SpecFunc(func(*Product) bool { return false }) }

func TestAtomicSpecs(t *testing.T) {
	t.Parallel()
	p := &Product{SKU: "x", Category: "books", Price: 1500, InStock: true}

	cases := []struct {
		name string
		spec Specification
		want bool
	}{
		{"in category match", InCategory("books"), true},
		{"in category miss", InCategory("merch"), false},
		{"price at most over", PriceAtMost(1000), false},
		{"price at most equal", PriceAtMost(1500), true},
		{"price at most under", PriceAtMost(2000), true},
		{"in stock", InStock(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.IsSatisfiedBy(p); got != tc.want {
				t.Errorf("IsSatisfiedBy = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCombinators(t *testing.T) {
	t.Parallel()
	p := &Product{SKU: "x"}

	cases := []struct {
		name string
		spec Specification
		want bool
	}{
		{"and all true", And(alwaysTrue(), alwaysTrue()), true},
		{"and one false", And(alwaysTrue(), alwaysFalse()), false},
		{"and empty is true", And(), true},
		{"or one true", Or(alwaysFalse(), alwaysTrue()), true},
		{"or all false", Or(alwaysFalse(), alwaysFalse()), false},
		{"or empty is false", Or(), false},
		{"not true", Not(alwaysTrue()), false},
		{"not false", Not(alwaysFalse()), true},
		{"nested", And(alwaysTrue(), Or(alwaysFalse(), Not(alwaysFalse()))), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.IsSatisfiedBy(p); got != tc.want {
				t.Errorf("IsSatisfiedBy = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFindComposedSpec(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := NewProductRepository()

	seed := []*Product{
		{SKU: "b1", Name: "Go in Action", Category: "books", Price: 1800, InStock: true},
		{SKU: "b2", Name: "The Go PL", Category: "books", Price: 3500, InStock: true},
		{SKU: "b3", Name: "Used Paperback", Category: "books", Price: 900, InStock: false},
		{SKU: "m1", Name: "Mug", Category: "merch", Price: 1200, InStock: true},
	}
	for _, p := range seed {
		if err := repo.Add(ctx, p); err != nil {
			t.Fatalf("Add %s: %v", p.SKU, err)
		}
	}

	spec := And(InCategory("books"), PriceAtMost(2000), InStock())
	got, err := repo.Find(ctx, spec)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(got) != 1 || got[0].SKU != "b1" {
		t.Fatalf("Find = %v, want [b1]", skus(got))
	}

	all, _ := repo.Find(ctx, nil)
	if len(all) != 4 {
		t.Errorf("Find(nil) returned %d, want 4", len(all))
	}

	cheapOrMerch := Or(PriceAtMost(1000), InCategory("merch"))
	got = mustFind(t, repo, cheapOrMerch)
	if len(got) != 2 || got[0].SKU != "b3" || got[1].SKU != "m1" {
		t.Errorf("Or query = %v, want [b3 m1]", skus(got))
	}
}

func TestAddRejectsDuplicateSKU(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := NewProductRepository()
	if err := repo.Add(ctx, &Product{SKU: "a"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := repo.Add(ctx, &Product{SKU: "a"}); err != ErrDuplicateSKU {
		t.Errorf("err = %v, want ErrDuplicateSKU", err)
	}
	if err := repo.Add(ctx, &Product{SKU: ""}); err != ErrEmptySKU {
		t.Errorf("err = %v, want ErrEmptySKU", err)
	}
}

func mustFind(t *testing.T, repo *ProductRepository, spec Specification) []*Product {
	t.Helper()
	got, err := repo.Find(context.Background(), spec)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	return got
}

func skus(ps []*Product) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.SKU
	}
	return out
}
```

## Review

The specification layer is correct when leaves and combinators are indistinguishable to `Find`: every atomic spec and every `And`/`Or`/`Not` node is a `Specification`, so a tree of any depth is evaluated by a single `IsSatisfiedBy` call at the root. Confirm the vacuous identities — `And()` is true, `Or()` is false — because the combinator loops depend on them, and confirm `Not` inverts. The integration test is the one that matters most: it proves a composed three-clause `And` selects exactly the right SKUs out of a mixed catalog, which is the behavior a forest of `GetByX` methods would have required many separate implementations to provide.

Common mistakes for this feature. The first is writing `And` to start from `false` or `Or` from `true`, which inverts the empty-set identity and quietly breaks every composed query that bottoms out in an empty clause. The second is letting `Find` return without sorting, which makes results nondeterministic across runs and turns the count-and-SKU assertions flaky. The third is treating the pointers `Find` returns as mutable: writing through them mutates stored products and races other readers, so they must be handled as read-only snapshots. Running under `go test -race ./...` confirms the read lock actually serializes scans against writes.

## Resources

- [Specification pattern (Fowler & Evans, PDF)](https://martinfowler.com/apsupp/spec.pdf) — the canonical write-up of specifications as composable, reusable predicate objects.
- [`http.HandlerFunc`](https://pkg.go.dev/net/http#HandlerFunc) — the standard-library function-to-interface adapter that `SpecFunc` mirrors.
- [`sort.Slice`](https://pkg.go.dev/sort#Slice) — the standard-library sort `Find` uses for deterministic ordering.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-user-repository.md](01-user-repository.md) | Next: [03-decorator-cache-and-logging.md](03-decorator-cache-and-logging.md)
