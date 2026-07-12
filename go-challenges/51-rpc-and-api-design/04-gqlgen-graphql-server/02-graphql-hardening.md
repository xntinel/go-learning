# Exercise 2: Hardening a GraphQL Endpoint — Errors, Complexity, and Introspection

The wiring from Exercise 1 works but is not safe to expose. This exercise adds the
four things a GraphQL endpoint needs before it faces the internet: an error
presenter that masks internals, a recover func that turns panics into safe errors,
a complexity limit that rejects expensive queries before any resolver runs, and
introspection gated behind a flag.

This module is self-contained and bar-mode: it depends on `github.com/99designs/
gqlgen` and its generated code, so it will not build in an offline gate. The Go is
written to correct APIs and is gofmt-clean.

## What you'll build

```text
catalogedge/                     module example.com/catalogedge
  go.mod                         go 1.26; requires github.com/99designs/gqlgen
  gqlgen.yml                     codegen config
  graph/
    schema.graphqls              Query with product, riskyProduct, boom, batch
    errors.go                    SafeError: a client-safe typed error
    resolver.go                  Resolver + store + an Invocations counter
    schema.resolvers.go          resolvers: safe error, raw error, panic, partial
    generated.go                 MACHINE-GENERATED (committed, not edited)
    model/models_gen.go          MACHINE-GENERATED
    server.go                    NewServer: presenter, recover, complexity, gating
    server_test.go               masking, complexity, introspection, panic tests
  cmd/
    demo/main.go                 in-process, shows masked vs client-safe errors
```

Files: `gqlgen.yml`, `graph/schema.graphqls`, `graph/errors.go`, `graph/resolver.go`, `graph/schema.resolvers.go`, `graph/server.go`, `graph/server_test.go`, `cmd/demo/main.go`.
Implement: `errorPresenter` (map `SafeError` via `errors.As`, mask the rest), `recoverFunc`, a per-field complexity function for `batch`, `FixedComplexityLimit`, and flag-gated `extension.Introspection`.
Test: a wrapped internal error surfaces only the masked message; an over-complex query is rejected before any resolver runs; an introspection query works only when the flag enables it; a panicking resolver returns a generic error, not a crash.
Verify: `go run github.com/99designs/gqlgen generate` then `go test -race ./...`.

Set up the module:

```bash
mkdir -p go-solutions/51-rpc-and-api-design/04-gqlgen-graphql-server/02-graphql-hardening/graph
cd go-solutions/51-rpc-and-api-design/04-gqlgen-graphql-server/02-graphql-hardening
go mod edit -go=1.26
go get github.com/99designs/gqlgen@latest
```

### The schema exposes the four failure shapes

Each Query field exercises one hardening concern: `product` returns a
client-safe typed error on a miss, `riskyProduct` returns a raw internal error
that must be masked, `boom` panics, and `batch` partially fails and must return
the successes alongside per-element errors. Every result field is nullable so a
field error surfaces as `null` in that slot rather than nulling a parent.

Create `graph/schema.graphqls`:

```graphql
type Product {
  id: ID!
  name: String!
  priceCents: Int!
}

type Query {
  product(id: ID!): Product
  riskyProduct(id: ID!): Product
  boom: Boolean
  batch(ids: [ID!]!): [Product]!
}
```

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

models:
  ID:
    model:
      - github.com/99designs/gqlgen/graphql.ID
  Int:
    model:
      - github.com/99designs/gqlgen/graphql.Int
```

Generate and commit the machine-owned files:

```bash
go run github.com/99designs/gqlgen generate
```

### A typed, client-safe error

The presenter needs a way to tell "this message is safe to show a client" from
"this is an internal error to mask". A typed error carries that distinction. A
resolver returns a `*SafeError` (wrapped, so it can travel through `%w`) for a
client-facing condition; the presenter matches it with `errors.As` and shows its
message plus a code. Any error that is *not* a `SafeError` is masked.

Create `graph/errors.go`:

```go
package graph

// SafeError is an error whose message is safe to show a client. The error
// presenter surfaces its Msg and Code and masks every other error type.
type SafeError struct {
	Code string
	Msg  string
}

func (e *SafeError) Error() string { return e.Msg }
```

### The resolver, with an invocation counter

The `Invocations` counter is the instrument that proves the complexity limit
rejects a query *before* resolvers run: after a rejected query it must still read
zero. It is an `atomic.Int64` because gqlgen resolves fields concurrently.

Create `graph/resolver.go`:

```go
package graph

import (
	"sort"
	"sync"
	"sync/atomic"

	"example.com/catalogedge/graph/model"
)

// Resolver is the root resolver. Invocations counts resolver-method entries so a
// test can assert that a complexity-rejected query never reaches a resolver.
type Resolver struct {
	store       *ProductStore
	Invocations atomic.Int64
}

// NewResolver builds a Resolver over a fresh store.
func NewResolver() *Resolver { return &Resolver{store: NewProductStore()} }

// Seed inserts one product; a convenience for tests and the demo.
func (r *Resolver) Seed(id, name string, priceCents int) {
	r.store.Seed(&model.Product{ID: id, Name: name, PriceCents: priceCents})
}

// ProductStore is an in-memory, concurrency-safe catalog.
type ProductStore struct {
	mu   sync.RWMutex
	byID map[string]*model.Product
}

// NewProductStore returns an empty store.
func NewProductStore() *ProductStore {
	return &ProductStore{byID: make(map[string]*model.Product)}
}

// Seed inserts products under their ids.
func (s *ProductStore) Seed(products ...*model.Product) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range products {
		s.byID[p.ID] = p
	}
}

// Get returns the product for id and whether it existed.
func (s *ProductStore) Get(id string) (*model.Product, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byID[id]
	return p, ok
}

// All returns every product, id-sorted.
func (s *ProductStore) All() []*model.Product {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Product, 0, len(s.byID))
	for _, p := range s.byID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
```

### Resolvers that fail in four different ways

`Product` returns a `*SafeError` on a miss — a message the client is meant to see.
`RiskyProduct` returns a raw internal error (`errDBUnavailable`) whose text must
never reach the client; the presenter masks it. `Boom` panics; the recover func
catches it. `Batch` accumulates one error per missing id with the correct array
path via `graphql.GetPath` plus the element index, and still returns the products
it did find — the canonical partial-success list.

There is one non-obvious detail in `Batch`. `graphql.AddError` does not append the
error verbatim: it first runs it through the configured `errorPresenter` (see
`AddError` -> `ErrorOnPath` -> `errorPresenter` in gqlgen's `context_response.go`).
Our presenter masks every error that is not a `SafeError`, so a bare
`&gqlerror.Error{Message: ...}` would be rewritten to `internal server error`
before it ever reached the client. To make the curated per-element message
survive masking, the resolver sets the gqlerror's `Err` field to a `*SafeError`.
Because `gqlerror.Error` implements `Unwrap()`, the presenter's
`errors.As(e, &safe)` unwraps through the gqlerror, matches the `SafeError`, and
keeps the message and `NOT_FOUND` code — while the manually built `Path` (already
non-nil) is left untouched by `ErrorOnPath`.

Create `graph/schema.resolvers.go`:

```go
package graph

import (
	"context"
	"errors"
	"fmt"

	"example.com/catalogedge/graph/model"
	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// errDBUnavailable simulates a low-level, client-unsafe internal error whose
// text must never reach a client.
var errDBUnavailable = errors.New("pq: connection refused (host=db-primary:5432)")

// Product returns one product, or a client-safe SafeError on a miss.
func (r *queryResolver) Product(ctx context.Context, id string) (*model.Product, error) {
	r.Invocations.Add(1)
	p, ok := r.store.Get(id)
	if !ok {
		return nil, fmt.Errorf("lookup id=%s: %w", id, &SafeError{
			Code: "NOT_FOUND",
			Msg:  fmt.Sprintf("product %q does not exist", id),
		})
	}
	return p, nil
}

// RiskyProduct returns a raw internal error to exercise the presenter's masking.
func (r *queryResolver) RiskyProduct(ctx context.Context, id string) (*model.Product, error) {
	r.Invocations.Add(1)
	return nil, fmt.Errorf("query catalog: %w", errDBUnavailable)
}

// Boom panics to exercise the recover func.
func (r *queryResolver) Boom(ctx context.Context) (*bool, error) {
	r.Invocations.Add(1)
	panic("resolver bug: write to nil map")
}

// Batch resolves each id, adding a path-scoped error for every miss while still
// returning the products that were found.
func (r *queryResolver) Batch(ctx context.Context, ids []string) ([]*model.Product, error) {
	r.Invocations.Add(1)
	out := make([]*model.Product, 0, len(ids))
	for i, id := range ids {
		p, ok := r.store.Get(id)
		if !ok {
			// AddError runs the error through errorPresenter, which masks
			// anything that is not a SafeError. Carry a *SafeError in Err so
			// errors.As matches (gqlerror.Error implements Unwrap) and the
			// curated message and code survive masking; Path is preserved
			// because ErrorOnPath leaves an already-set path untouched.
			safe := &SafeError{Code: "NOT_FOUND", Msg: fmt.Sprintf("product %q does not exist", id)}
			graphql.AddError(ctx, &gqlerror.Error{
				Err:     safe,
				Path:    append(graphql.GetPath(ctx), ast.PathIndex(i)),
				Message: safe.Msg,
			})
			out = append(out, nil)
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// Query returns the resolver bound to Query fields.
func (r *Resolver) Query() QueryResolver { return &queryResolver{r} }

type queryResolver struct{ *Resolver }
```

### The hardened server

`NewServer` is the whole lesson. `SetErrorPresenter(errorPresenter)` makes every
error pass through masking. `SetRecoverFunc(recoverFunc)` turns panics into a
generic error. `extension.FixedComplexityLimit(ComplexityLimit)` costs each query
before execution and rejects the too-expensive ones. `batch`'s cost scales with
its argument, so a per-field complexity function multiplies the child cost by
`len(ids)`. Introspection is applied only when the `introspection` flag is set,
which in production comes from an environment variable or role check.

Create `graph/server.go`:

```go
package graph

import (
	"context"
	"errors"
	"log"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// ComplexityLimit bounds the cost of any single query. A query costed above it is
// rejected before any resolver runs.
const ComplexityLimit = 5

// NewServer wires a hardened handler: masked errors, recovered panics, a
// complexity ceiling, and introspection gated behind the introspection flag.
func NewServer(r *Resolver, introspection bool) *handler.Server {
	cfg := Config{Resolvers: r}
	// batch's cost scales with the number of ids requested; multiply the child
	// cost by len(ids) so a large batch is charged accordingly.
	cfg.Complexity.Query.Batch = func(childComplexity int, ids []string) int {
		return childComplexity * len(ids)
	}

	srv := handler.New(NewExecutableSchema(cfg))
	srv.AddTransport(transport.Options{})
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})
	srv.SetQueryCache(lru.New[*ast.QueryDocument](1000))

	srv.SetErrorPresenter(errorPresenter)
	srv.SetRecoverFunc(recoverFunc)
	srv.Use(extension.FixedComplexityLimit(ComplexityLimit))

	if introspection {
		srv.Use(extension.Introspection{})
	}
	return srv
}

// errorPresenter surfaces a client-safe SafeError with its code and masks every
// other error behind a generic message. A raw internal error string never
// reaches a client.
func errorPresenter(ctx context.Context, e error) *gqlerror.Error {
	err := graphql.DefaultErrorPresenter(ctx, e)

	var safe *SafeError
	if errors.As(e, &safe) {
		err.Message = safe.Msg
		err.Extensions = map[string]interface{}{"code": safe.Code}
		return err
	}

	err.Message = "internal server error"
	err.Extensions = map[string]interface{}{"code": "INTERNAL"}
	return err
}

// recoverFunc turns a resolver panic into a generic gqlerror instead of a crash
// or a leaked stack trace. The masked error then flows through errorPresenter.
func recoverFunc(ctx context.Context, err interface{}) error {
	log.Printf("recovered resolver panic: %v", err)
	return gqlerror.Errorf("internal server error")
}
```

### The demo

The demo starts the hardened server (introspection off) and posts four queries,
decoding each into a small envelope so the printed output is deterministic. Note
every response is HTTP 200 even when a resolver failed: the failure is in the
`errors` array, and the raw `pq: connection refused` never appears.

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

	"example.com/catalogedge/graph"
)

type gqlResp struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message    string         `json:"message"`
		Extensions map[string]any `json:"extensions"`
	} `json:"errors"`
}

func run(url, label, query string) {
	body, _ := json.Marshal(map[string]string{"query": query})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var gr gqlResp
	_ = json.Unmarshal(raw, &gr)
	if len(gr.Errors) > 0 {
		code, _ := gr.Errors[0].Extensions["code"].(string)
		fmt.Printf("%s: HTTP %d error: %s [%s]\n", label, resp.StatusCode, gr.Errors[0].Message, code)
		return
	}
	fmt.Printf("%s: HTTP %d data: %s\n", label, resp.StatusCode, gr.Data)
}

func main() {
	r := graph.NewResolver()
	r.Seed("1", "Keyboard", 4999)

	ts := httptest.NewServer(graph.NewServer(r, false))
	defer ts.Close()

	run(ts.URL, "product", `{ product(id:"1") { name } }`)
	run(ts.URL, "notFound", `{ product(id:"999") { name } }`)
	run(ts.URL, "riskyProduct", `{ riskyProduct(id:"1") { name } }`)
	run(ts.URL, "boom", `{ boom }`)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
product: HTTP 200 data: {"product":{"name":"Keyboard"}}
notFound: HTTP 200 error: product "999" does not exist [NOT_FOUND]
riskyProduct: HTTP 200 error: internal server error [INTERNAL]
boom: HTTP 200 error: internal server error [INTERNAL]
```

### Tests

The tests use `client.Post` (not `MustPost`) so the returned error can be
inspected. `TestMaskedInternalError` proves the raw `connection refused` never
escapes while the code `INTERNAL` does. `TestComplexityRejected` sends a batch
costed above the limit and asserts both the complexity rejection and that
`Invocations` is still zero — the resolver never ran. `TestIntrospectionGating`
runs the same `__schema` query against a server with introspection on and one
with it off. `TestPanicRecovered` shows a panic becomes a safe error, not a crash.
`TestPartialErrors` shows a partial-success list: one product resolved, one slot
null, with an error carrying the element path.

Create `graph/server_test.go`:

```go
package graph

import (
	"fmt"
	"strings"
	"testing"

	"github.com/99designs/gqlgen/client"
)

func seededClient(introspection bool) (*Resolver, *client.Client) {
	r := NewResolver()
	r.Seed("1", "Keyboard", 4999)
	r.Seed("2", "Mouse", 2500)
	return r, client.New(NewServer(r, introspection))
}

func TestMaskedInternalError(t *testing.T) {
	t.Parallel()
	_, c := seededClient(false)

	var resp struct {
		RiskyProduct *struct{ Name string }
	}
	err := c.Post(`{ riskyProduct(id:"1") { name } }`, &resp)
	if err == nil {
		t.Fatal("expected an error for riskyProduct")
	}
	if !strings.Contains(err.Error(), "internal server error") {
		t.Fatalf("error = %q, want the masked message", err.Error())
	}
	if strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error leaked the raw internal message: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "INTERNAL") {
		t.Fatalf("error = %q, want the INTERNAL extensions code", err.Error())
	}
}

func TestSafeErrorSurfaced(t *testing.T) {
	t.Parallel()
	_, c := seededClient(false)

	var resp struct {
		Product *struct{ Name string }
	}
	err := c.Post(`{ product(id:"999") { name } }`, &resp)
	if err == nil {
		t.Fatal("expected an error for a missing product")
	}
	if !strings.Contains(err.Error(), `product "999" does not exist`) {
		t.Fatalf("error = %q, want the client-safe message", err.Error())
	}
	if !strings.Contains(err.Error(), "NOT_FOUND") {
		t.Fatalf("error = %q, want the NOT_FOUND code", err.Error())
	}
	if strings.Contains(err.Error(), "lookup id=") {
		t.Fatalf("error leaked the internal wrap: %q", err.Error())
	}
}

func TestComplexityRejected(t *testing.T) {
	t.Parallel()
	r, c := seededClient(false)

	var resp struct {
		Batch []*struct{ ID string }
	}
	err := c.Post(`{ batch(ids:["1","2","3","4","5","6"]) { id } }`, &resp)
	if err == nil {
		t.Fatal("expected the over-complex query to be rejected")
	}
	if !strings.Contains(err.Error(), "complexity") {
		t.Fatalf("error = %q, want a complexity rejection", err.Error())
	}
	if got := r.Invocations.Load(); got != 0 {
		t.Fatalf("resolver ran %d times; a rejected query must not reach resolvers", got)
	}
}

func TestIntrospectionGating(t *testing.T) {
	t.Parallel()

	_, on := seededClient(true)
	var resp struct {
		Schema struct {
			QueryType struct{ Name string }
		} `json:"__schema"`
	}
	on.MustPost(`{ __schema { queryType { name } } }`, &resp)
	if resp.Schema.QueryType.Name != "Query" {
		t.Fatalf("queryType.name = %q, want Query when introspection is on", resp.Schema.QueryType.Name)
	}

	_, off := seededClient(false)
	if err := off.Post(`{ __schema { queryType { name } } }`, &struct{}{}); err == nil {
		t.Fatal("expected introspection to be rejected when the flag is off")
	}
}

func TestPanicRecovered(t *testing.T) {
	t.Parallel()
	_, c := seededClient(false)

	err := c.Post(`{ boom }`, &struct {
		Boom *bool
	}{})
	if err == nil {
		t.Fatal("expected a recovered error, got nil")
	}
	if !strings.Contains(err.Error(), "internal server error") {
		t.Fatalf("error = %q, want the recovered generic message", err.Error())
	}
	if strings.Contains(err.Error(), "nil map") {
		t.Fatalf("error leaked the panic value: %q", err.Error())
	}
}

func TestPartialErrors(t *testing.T) {
	t.Parallel()
	_, c := seededClient(false)

	var resp struct {
		Batch []*struct{ ID string }
	}
	err := c.Post(`{ batch(ids:["1","999"]) { id } }`, &resp)
	if err == nil {
		t.Fatal("expected a partial error for the missing id")
	}
	if !strings.Contains(err.Error(), `product "999" does not exist`) {
		t.Fatalf("error = %q, want the per-element message", err.Error())
	}
	if len(resp.Batch) != 2 || resp.Batch[0] == nil || resp.Batch[0].ID != "1" {
		t.Fatalf("batch = %+v, want product 1 present in slot 0", resp.Batch)
	}
	if resp.Batch[1] != nil {
		t.Fatalf("batch[1] = %+v, want null for the missing id", resp.Batch[1])
	}
}

func Example() {
	r := NewResolver()
	c := client.New(NewServer(r, false))

	var resp struct {
		RiskyProduct *struct{ Name string }
	}
	err := c.Post(`{ riskyProduct(id:"1") { name } }`, &resp)
	fmt.Println(strings.Contains(err.Error(), "internal server error"))
	fmt.Println(strings.Contains(err.Error(), "connection refused"))
	// Output:
	// true
	// false
}
```

## Review

The endpoint is hardened when a client can learn nothing you did not choose to
tell it and cannot make the server do unbounded work. Confirm masking with
`TestMaskedInternalError` (the raw `connection refused` is absent; only `internal
server error` and the `INTERNAL` code appear) and `TestSafeErrorSurfaced` (the
curated message and `NOT_FOUND` code appear; the internal `lookup id=` wrap does
not). Confirm the DoS bound with `TestComplexityRejected`: the query is rejected
and `Invocations` is zero, proving rejection happens before resolvers run.
Confirm introspection is a decision, not a default, with `TestIntrospectionGating`.
Confirm a bug is contained with `TestPanicRecovered`.

The mistakes to avoid are exactly the defaults. An unconfigured server has no
presenter, so `RiskyProduct`'s raw error would ship to the client — never rely on
resolvers to self-censor; centralize it in `errorPresenter`. Without a complexity
limit, `batch` with a large id list (or any deep query) is a free DoS; the
per-field complexity function is what makes an argument-scaled field costed
correctly. And do not leave `extension.Introspection{}` applied unconditionally:
gate it, because it publishes your whole schema. Remember that all of this returns
HTTP 200 — monitoring must read the `errors` array, not the status code.

## Resources

- [gqlgen Reference: Handling Errors](https://gqlgen.com/reference/errors/) — `SetErrorPresenter`, `DefaultErrorPresenter`, `SetRecoverFunc`, and `graphql.AddError`.
- [gqlgen Reference: Query Complexity](https://gqlgen.com/reference/complexity/) — `FixedComplexityLimit`, per-field complexity functions, and options.
- [gqlgen Reference: Introspection](https://gqlgen.com/reference/introspection/) — enabling `extension.Introspection` and why to gate it.
- [`vektah/gqlparser/v2/gqlerror`](https://pkg.go.dev/github.com/vektah/gqlparser/v2/gqlerror) — `Error`, `Errorf`, `Path`, and `Extensions`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-schema-first-resolvers.md](01-schema-first-resolvers.md) | Next: [03-scalars-and-field-resolvers.md](03-scalars-and-field-resolvers.md)
