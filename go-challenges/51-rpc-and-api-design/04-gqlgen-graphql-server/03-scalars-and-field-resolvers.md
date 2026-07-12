# Exercise 3: Custom Scalars, Model Autobinding, and Nested Field Resolvers

Real schemas outgrow the built-in scalars and the generated models. This exercise
adds a `Money` custom scalar, binds the GraphQL `Product` and `Review` types to
hand-written domain structs via autobind, and adds a nested `Product.reviews`
field resolver that fetches related data lazily — which builds the exact N+1
pattern that lesson 05 solves with dataloaders.

This module is self-contained and bar-mode: it depends on `github.com/99designs/
gqlgen` and its generated code, so it will not build in an offline gate. The Go is
written to correct APIs and is gofmt-clean.

## What you'll build

```text
catalogapi/                      module example.com/catalogapi
  go.mod                         go 1.26; requires github.com/99designs/gqlgen
  gqlgen.yml                     autobind domain package; map Money scalar
  domain/
    money.go                     Money scalar: MarshalMoney/UnmarshalMoney
    money_test.go                round-trip + shape tests + Example
    product.go                   Product, Review, stores; ReviewStore counts fetches
  graph/
    schema.graphqls              scalar Money; Product.reviews as a field resolver
    resolver.go                  Resolver holding the two stores
    schema.resolvers.go          Query.products + Product.reviews (field resolver)
    generated.go                 MACHINE-GENERATED (committed, not edited)
    server.go                    NewServer wiring
    server_test.go               nested query + N+1 fetch-count assertion
  cmd/
    demo/main.go                 query with nested reviews; prints the fetch count
```

Files: `gqlgen.yml`, `domain/money.go`, `domain/product.go`, `graph/schema.graphqls`, `graph/resolver.go`, `graph/schema.resolvers.go`, `graph/server.go`, `domain/money_test.go`, `graph/server_test.go`, `cmd/demo/main.go`.
Implement: `MarshalMoney`/`UnmarshalMoney` (handling `string`, `float64`, `json.Number`, `int`), autobound `Product`/`Review` domain structs, and the `Product.reviews` field resolver.
Test: round-trip the scalar (marshal, feed the literal back, assert equality) and reject malformed input; query the nested field and assert related reviews resolve; assert the field resolver runs once per product — the N+1 fetch count.
Verify: `go run github.com/99designs/gqlgen generate` then `go test -race ./...`.

Set up the module:

```bash
go mod edit -go=1.26
go get github.com/99designs/gqlgen@latest
```

### The custom scalar contract

A custom scalar is two functions. `MarshalMoney(Money) graphql.Marshaler` returns
a `graphql.WriterFunc` that writes the wire form; here `Money` is integer cents,
serialized as a *quoted decimal string* ("12.99") so a monetary value never rides
across the wire as a float. `UnmarshalMoney(interface{}) (Money, error)` is the
sharp edge: the argument is whatever the JSON decoder produced, and a client can
send more shapes than you expect. A quoted literal arrives as `string`; a bare
number arrives as `float64` (not `int`); you may also see `json.Number` or `int`.
An unmarshaler that handles only `string` breaks the first time a client sends a
number. Every unrecognized shape returns an error, never a silent zero.

Create `domain/money.go`:

```go
package domain

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/99designs/gqlgen/graphql"
)

// Money is an amount in integer cents. It is a GraphQL custom scalar serialized
// as a quoted decimal string ("12.99") so a monetary value never rides on a
// float across the wire.
type Money int64

// MarshalMoney writes the amount as a quoted decimal string. gqlgen calls it for
// every Money value in a response.
func MarshalMoney(m Money) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		neg := ""
		v := int64(m)
		if v < 0 {
			neg = "-"
			v = -v
		}
		io.WriteString(w, strconv.Quote(fmt.Sprintf("%s%d.%02d", neg, v/100, v%100)))
	})
}

// UnmarshalMoney parses a client-supplied Money value. A JSON number arrives as
// float64, a quoted literal as string; json.Number and int are handled too.
// Unrecognized input is a hard error, never a silent zero.
func UnmarshalMoney(v interface{}) (Money, error) {
	switch t := v.(type) {
	case string:
		return parseMoney(t)
	case json.Number:
		return parseMoney(t.String())
	case float64:
		return Money(math.Round(t * 100)), nil
	case int:
		return Money(int64(t) * 100), nil
	case int64:
		return Money(t * 100), nil
	default:
		return 0, fmt.Errorf("Money: cannot unmarshal %T", v)
	}
}

func parseMoney(s string) (Money, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "$")
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")

	whole, frac, hasFrac := strings.Cut(s, ".")
	dollars, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("Money: bad amount %q: %w", s, err)
	}
	var cents int64
	if hasFrac {
		if len(frac) == 1 {
			frac += "0"
		}
		if len(frac) != 2 {
			return 0, fmt.Errorf("Money: expected two decimal places, got %q", frac)
		}
		if cents, err = strconv.ParseInt(frac, 10, 64); err != nil {
			return 0, fmt.Errorf("Money: bad cents %q: %w", frac, err)
		}
	}
	total := dollars*100 + cents
	if neg {
		total = -total
	}
	return Money(total), nil
}
```

### The autobound domain, and the store that counts fetches

The domain types are yours, not generated. `Product` deliberately has *no*
`Reviews` field: gqlgen matches schema fields to struct fields by name, and any
schema field with no matching struct field becomes a field resolver. So
`Product.reviews` becomes a resolver method — and that is the N+1 hook. The
`ReviewStore` counts every `ByProduct` call so a test can observe the fan-out:
one products query over N products invokes the field resolver N times, one fetch
each.

Create `domain/product.go`:

```go
package domain

import (
	"sort"
	"sync"
	"sync/atomic"
)

// Product is autobound to the GraphQL Product type. It has no Reviews field, so
// gqlgen makes Product.reviews a field resolver -- the N+1 hook.
type Product struct {
	ID    string
	Name  string
	Price Money
}

// Review is autobound to the GraphQL Review type. ProductID is a domain field the
// schema does not expose; autobind ignores struct fields with no schema field.
type Review struct {
	ID        string
	ProductID string
	Author    string
	Body      string
}

// ProductStore holds products in memory.
type ProductStore struct {
	mu   sync.RWMutex
	byID map[string]*Product
}

// NewProductStore returns an empty product store.
func NewProductStore() *ProductStore {
	return &ProductStore{byID: make(map[string]*Product)}
}

// Add stores a product under its id.
func (s *ProductStore) Add(p *Product) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[p.ID] = p
}

// All returns every product, id-sorted.
func (s *ProductStore) All() []*Product {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Product, 0, len(s.byID))
	for _, p := range s.byID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ReviewStore holds reviews and counts fetches so the N+1 pattern is observable.
type ReviewStore struct {
	mu      sync.RWMutex
	byProd  map[string][]*Review
	fetches atomic.Int64
}

// NewReviewStore returns an empty review store.
func NewReviewStore() *ReviewStore {
	return &ReviewStore{byProd: make(map[string][]*Review)}
}

// Add stores a review under its product id.
func (s *ReviewStore) Add(r *Review) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byProd[r.ProductID] = append(s.byProd[r.ProductID], r)
}

// ByProduct returns the reviews for one product and counts the call. Each call is
// one fetch; a list query that calls it once per product is the N+1 pattern.
func (s *ReviewStore) ByProduct(productID string) []*Review {
	s.fetches.Add(1)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byProd[productID]
}

// Fetches reports how many times ByProduct has been called.
func (s *ReviewStore) Fetches() int64 { return s.fetches.Load() }
```

### The schema, with a scalar and a nested field

`scalar Money` declares the custom scalar. `Product.reviews` is a list field with
no matching struct field, so gqlgen generates a field resolver for it.

Create `graph/schema.graphqls`:

```graphql
scalar Money

type Product {
  id: ID!
  name: String!
  price: Money!
  reviews: [Review!]!
}

type Review {
  id: ID!
  author: String!
  body: String!
}

type Query {
  products: [Product!]!
}
```

### The codegen config: autobind and the scalar map

`autobind` points gqlgen at the domain package so it binds by matching type and
field names; the explicit `models:` entries map `Money`, `Product`, and `Review`
to the exact domain types (and `Money` to the scalar with its marshaler pair).

Create `gqlgen.yml`:

```yaml
schema:
  - graph/*.graphqls

exec:
  filename: graph/generated.go
  package: graph

model:
  filename: graph/model/models_gen.go
  package: model

resolver:
  layout: follow-schema
  dir: graph
  package: graph
  filename_template: "{name}.resolvers.go"

autobind:
  - example.com/catalogapi/domain

models:
  ID:
    model:
      - github.com/99designs/gqlgen/graphql.ID
  Money:
    model: example.com/catalogapi/domain.Money
  Product:
    model: example.com/catalogapi/domain.Product
  Review:
    model: example.com/catalogapi/domain.Review
```

Generate and commit the machine-owned files:

```bash
go run github.com/99designs/gqlgen generate
```

### The resolver and the nested field resolver

The root `Resolver` holds both stores. `Query.products` returns the list.
`Product.reviews` is the field resolver: it receives the parent `*domain.Product`
and fetches its reviews. The store fields are named `ProductStore`/`ReviewStore`
to avoid colliding with the generated resolver methods `Products` and `Reviews`.

Create `graph/resolver.go`:

```go
package graph

import "example.com/catalogapi/domain"

// Resolver is the root resolver. It owns the product and review stores; the
// review store counts fetches so the N+1 pattern is observable.
type Resolver struct {
	ProductStore *domain.ProductStore
	ReviewStore  *domain.ReviewStore
}

// NewResolver builds a Resolver over fresh stores.
func NewResolver() *Resolver {
	return &Resolver{
		ProductStore: domain.NewProductStore(),
		ReviewStore:  domain.NewReviewStore(),
	}
}
```

Create `graph/schema.resolvers.go`:

```go
package graph

import (
	"context"

	"example.com/catalogapi/domain"
)

// Products returns every product.
func (r *queryResolver) Products(ctx context.Context) ([]*domain.Product, error) {
	return r.ProductStore.All(), nil
}

// Reviews resolves Product.reviews. Because domain.Product has no Reviews field,
// gqlgen makes this a field resolver: it runs ONCE PER PARENT product. A products
// query over N products calls it N times -- the canonical N+1 that lesson 05
// batches away with a dataloader.
func (r *productResolver) Reviews(ctx context.Context, obj *domain.Product) ([]*domain.Review, error) {
	return r.ReviewStore.ByProduct(obj.ID), nil
}

// Product returns the resolver bound to Product field resolvers.
func (r *Resolver) Product() ProductResolver { return &productResolver{r} }

// Query returns the resolver bound to Query fields.
func (r *Resolver) Query() QueryResolver { return &queryResolver{r} }

type productResolver struct{ *Resolver }
type queryResolver struct{ *Resolver }
```

Create `graph/server.go`:

```go
package graph

import (
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/vektah/gqlparser/v2/ast"
)

// NewServer wires a handler over the given resolver.
func NewServer(r *Resolver) *handler.Server {
	srv := handler.New(NewExecutableSchema(Config{Resolvers: r}))
	srv.AddTransport(transport.Options{})
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})
	srv.SetQueryCache(lru.New[*ast.QueryDocument](1000))
	return srv
}
```

### The demo

The demo seeds two products and two reviews, runs one query that selects the
nested `reviews` and the `price` scalar, prints the JSON, and then prints the
fetch count — which equals the number of products, making the N+1 visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/catalogapi/domain"
	"example.com/catalogapi/graph"
)

func main() {
	r := graph.NewResolver()
	r.ProductStore.Add(&domain.Product{ID: "1", Name: "Keyboard", Price: 4999})
	r.ProductStore.Add(&domain.Product{ID: "2", Name: "Mouse", Price: 2500})
	r.ReviewStore.Add(&domain.Review{ID: "r1", ProductID: "1", Author: "ana", Body: "great"})
	r.ReviewStore.Add(&domain.Review{ID: "r2", ProductID: "2", Author: "bob", Body: "ok"})

	ts := httptest.NewServer(graph.NewServer(r))
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"query": `{ products { id name price reviews { author } } }`})
	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)

	fmt.Println(string(bytes.TrimSpace(out)))
	fmt.Printf("review fetches: %d (one per product -> N+1)\n", r.ReviewStore.Fetches())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"data":{"products":[{"id":"1","name":"Keyboard","price":"49.99","reviews":[{"author":"ana"}]},{"id":"2","name":"Mouse","price":"25.00","reviews":[{"author":"bob"}]}]}}
review fetches: 2 (one per product -> N+1)
```

### Tests

The scalar tests live in the `domain` package. `TestMoneyRoundTrip` marshals a
`Money`, asserts the wire form, then feeds the literal back through
`UnmarshalMoney` and asserts equality — a true round trip. `TestUnmarshalMoneyShapes`
proves every JSON shape a client might send is handled and that malformed input
errors. The `Example` pins the marshaled form with an `// Output` line.

Create `domain/money_test.go`:

```go
package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestMoneyRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Money
		want string
	}{
		{"dollars and cents", 1299, `"12.99"`},
		{"whole dollars", 5000, `"50.00"`},
		{"sub dollar", 5, `"0.05"`},
		{"negative", -1299, `"-12.99"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			MarshalMoney(tc.in).MarshalGQL(&buf)
			if buf.String() != tc.want {
				t.Fatalf("MarshalMoney(%d) = %s, want %s", tc.in, buf.String(), tc.want)
			}
			got, err := UnmarshalMoney(tc.want[1 : len(tc.want)-1]) // strip quotes
			if err != nil {
				t.Fatalf("UnmarshalMoney(%s) error: %v", tc.want, err)
			}
			if got != tc.in {
				t.Fatalf("round trip = %d, want %d", got, tc.in)
			}
		})
	}
}

func TestUnmarshalMoneyShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      interface{}
		want    Money
		wantErr bool
	}{
		{"string", "12.99", 1299, false},
		{"json number", json.Number("12.99"), 1299, false},
		{"float64", float64(12.99), 1299, false},
		{"int dollars", 50, 5000, false},
		{"bad string", "twelve", 0, true},
		{"bad type", true, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := UnmarshalMoney(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("UnmarshalMoney(%v) = %d, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalMoney(%v) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("UnmarshalMoney(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func ExampleMarshalMoney() {
	var buf bytes.Buffer
	MarshalMoney(1299).MarshalGQL(&buf)
	fmt.Println(buf.String())
	// Output: "12.99"
}
```

The server test proves the nested field resolves and measures the N+1. It seeds
three products, queries `products { ... reviews { ... } }`, asserts the reviews
resolve and the `Money` scalar renders as a string, and then asserts the review
store was fetched once per product — three products, three fetches.

Create `graph/server_test.go`:

```go
package graph

import (
	"testing"

	"example.com/catalogapi/domain"
	"github.com/99designs/gqlgen/client"
)

func seeded() (*Resolver, *client.Client) {
	r := NewResolver()
	r.ProductStore.Add(&domain.Product{ID: "1", Name: "Keyboard", Price: 4999})
	r.ProductStore.Add(&domain.Product{ID: "2", Name: "Mouse", Price: 2500})
	r.ProductStore.Add(&domain.Product{ID: "3", Name: "Monitor", Price: 19999})
	r.ReviewStore.Add(&domain.Review{ID: "r1", ProductID: "1", Author: "ana", Body: "great"})
	r.ReviewStore.Add(&domain.Review{ID: "r2", ProductID: "1", Author: "bob", Body: "solid"})
	r.ReviewStore.Add(&domain.Review{ID: "r3", ProductID: "2", Author: "cy", Body: "ok"})
	return r, client.New(NewServer(r))
}

func TestNestedFieldResolvesAndN1(t *testing.T) {
	t.Parallel()
	r, c := seeded()

	var resp struct {
		Products []struct {
			ID      string
			Price   string
			Reviews []struct {
				Author string
			}
		}
	}
	c.MustPost(`{ products { id price reviews { author } } }`, &resp)

	if len(resp.Products) != 3 {
		t.Fatalf("got %d products, want 3", len(resp.Products))
	}
	if resp.Products[0].Price != "49.99" {
		t.Fatalf("price = %q, want 49.99 from the Money scalar", resp.Products[0].Price)
	}
	if len(resp.Products[0].Reviews) != 2 {
		t.Fatalf("product 1 reviews = %d, want 2", len(resp.Products[0].Reviews))
	}
	if got := r.ReviewStore.Fetches(); got != 3 {
		t.Fatalf("review fetches = %d; the field resolver runs once per product (N+1), want 3", got)
	}
}
```

## Review

The scalar is correct when marshal and unmarshal are inverses over the wire form:
`TestMoneyRoundTrip` marshals to `"12.99"` and reads it back to the same cents, and
`TestUnmarshalMoneyShapes` proves the `float64`, `json.Number`, `string`, and `int`
shapes all parse while junk errors. If a client sent a bare number and the
unmarshaler only handled `string`, that test would fail — which is the whole point
of writing it. The autobind is correct when resolvers speak `*domain.Product`
directly rather than a generated twin; the nested field resolves because
`Product.reviews` has no struct field and therefore became a resolver method.

The mistake to internalize is the N+1 itself. `TestNestedFieldResolvesAndN1`
asserts the review store was fetched three times for three products — one fetch
per parent, because a field resolver runs per parent object. That is fine at three
products and catastrophic at three thousand: it is one query for the parents plus
one per parent for the children, hammering the database in lockstep with your
result size. Recognizing this shape on sight is the senior skill; batching those N
fetches into one is the job of the dataloader in lesson 05. Do not "fix" it by
denormalizing reviews onto the product model — that trades one problem for stale
data and a fatter parent fetch; the right fix is batching.

## Resources

- [gqlgen Reference: Custom Scalars](https://gqlgen.com/reference/scalars/) — the `Marshal`/`Unmarshal` contract and mapping a scalar in `gqlgen.yml`.
- [gqlgen Reference: Configuration](https://gqlgen.com/config/) — `autobind` and `models:` for binding your own domain structs.
- [gqlgen: Resolvers](https://gqlgen.com/reference/resolvers/) — when a schema field becomes a field resolver and its method signature.
- [`graphql.Marshaler` / `WriterFunc`](https://pkg.go.dev/github.com/99designs/gqlgen/graphql#Marshaler) — the interface a custom scalar marshaler returns.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-graphql-hardening.md](02-graphql-hardening.md) | Next: [../05-graphql-dataloaders-n-plus-1/00-concepts.md](../05-graphql-dataloaders-n-plus-1/00-concepts.md)
