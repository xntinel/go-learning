# Exercise 6: Nested Composite Literals: a Route/Middleware Table

A composite literal is the only construct that builds a whole configuration tree
in one expression. This exercise declares an HTTP routing table as a single nested
literal — a slice of `Route`, each with its own middleware slice — and builds a
`map[string][]Route` index from map and slice literals in one expression. It shows
what `new(T)` plus assignment simply cannot express readably.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
routetable/                   independent module: example.com/routetable
  go.mod                      go 1.26
  routes.go                   Route, Middleware, HandlerFunc, Routes() table, IndexByPrefix
  cmd/
    demo/
      main.go                 runnable demo: list routes, dispatch one request through middleware
  routes_test.go              route-count/method-path test + prefix-index grouping + per-route middleware isolation
```

Files: `routes.go`, `cmd/demo/main.go`, `routes_test.go`.
Implement: a `Routes()` returning a nested `[]Route` literal with per-route
`[]Middleware`, and `IndexByPrefix` building a `map[string][]Route` with map and
slice literals.
Test: the literal builds the expected route count and method/path pairs; the map
index groups routes by prefix; per-route middleware slices are independent;
a golden assertion on the built table.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/03-new-vs-composite-literal/06-nested-composite-route-table/cmd/demo
cd go-solutions/09-pointers/03-new-vs-composite-literal/06-nested-composite-route-table
```

### Why a nested literal

A routing table is a tree: routes, each carrying a handler and an ordered list of
middleware. Expressing that with `new(Route)` and field assignments would mean a
temporary variable per route, a `make([]Middleware, ...)` and an `append` per
chain, and a wall of assignment statements that buries the shape of the table.
The composite literal expresses the whole tree directly: `[]Route{ {...}, {...} }`,
where each `{...}` is a `Route` value and the inner middleware is written inline as
`Middleware: []Middleware{...}`. Because the element type of the outer slice is
already `Route`, the inner `Route{...}` type name is elided to `{...}` — that
elision is what keeps a nested literal readable. Handler and middleware are
func-valued fields, so a whole dispatch pipeline is data you can range over,
count, and index.

`IndexByPrefix` is the same idea building a map: it groups routes by the first path
segment into a `map[string][]Route`. It could be written as a map literal for a
static grouping; here it is built by ranging the table and appending, which shows
the other half of "maps and slices are values you compose," and returns a map
whose values are independent slices. The key property the tests pin is
independence: because each route's `Middleware` is its own slice literal, mutating
one route's middleware never touches another's — there is no accidental shared
backing array, which is exactly the bug you get if you build all chains by
reslicing one shared slice.

Create `routes.go`:

```go
package routetable

import "strings"

// HandlerFunc handles a request, represented here as a string in, string out.
type HandlerFunc func(req string) string

// Middleware wraps a HandlerFunc.
type Middleware func(HandlerFunc) HandlerFunc

// Route is one entry in the routing table.
type Route struct {
	Method     string
	Path       string
	Handler    HandlerFunc
	Middleware []Middleware
}

// tag returns a middleware that prefixes the response with a label, so a chain's
// effect is observable in tests.
func tag(label string) Middleware {
	return func(next HandlerFunc) HandlerFunc {
		return func(req string) string {
			return label + ":" + next(req)
		}
	}
}

// Routes returns the whole routing table as one nested composite literal. Each
// Route's Middleware is its own slice literal, so the chains are independent.
func Routes() []Route {
	return []Route{
		{
			Method:     "GET",
			Path:       "/users",
			Handler:    func(req string) string { return "list" },
			Middleware: []Middleware{tag("auth"), tag("log")},
		},
		{
			Method:     "POST",
			Path:       "/users",
			Handler:    func(req string) string { return "create" },
			Middleware: []Middleware{tag("auth")},
		},
		{
			Method:     "GET",
			Path:       "/health",
			Handler:    func(req string) string { return "ok" },
			Middleware: []Middleware{},
		},
	}
}

// Chain applies a route's middleware around its handler, outermost first, and
// returns the final HandlerFunc.
func (r Route) Chain() HandlerFunc {
	h := r.Handler
	for i := len(r.Middleware) - 1; i >= 0; i-- {
		h = r.Middleware[i](h)
	}
	return h
}

// IndexByPrefix groups routes by their first path segment into a map built from
// map and slice values. Every value slice is independent.
func IndexByPrefix(routes []Route) map[string][]Route {
	index := map[string][]Route{}
	for _, rt := range routes {
		prefix := firstSegment(rt.Path)
		index[prefix] = append(index[prefix], rt)
	}
	return index
}

func firstSegment(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i]
	}
	return trimmed
}
```

### The runnable demo

The demo lists the routes, then dispatches one request through the `/users` GET
route so the middleware chain's effect is visible in the output.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/routetable"
)

func main() {
	routes := routetable.Routes()
	fmt.Printf("registered %d routes\n", len(routes))
	for _, r := range routes {
		fmt.Printf("%-4s %-8s middleware=%d\n", r.Method, r.Path, len(r.Middleware))
	}

	index := routetable.IndexByPrefix(routes)
	fmt.Printf("users prefix has %d routes\n", len(index["users"]))

	// Dispatch through the first route's full chain.
	first := routes[0]
	fmt.Printf("GET /users -> %s\n", first.Chain()("req"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
registered 3 routes
GET  /users   middleware=2
POST /users   middleware=1
GET  /health  middleware=0
users prefix has 2 routes
GET /users -> auth:log:list
```

### Tests

The tests assert the literal built the expected shape (route count and
method/path pairs), that the prefix index groups correctly, and — the important
one — that per-route middleware slices are independent, so mutating one route's
chain does not leak into another's.

Create `routes_test.go`:

```go
package routetable

import (
	"fmt"
	"testing"
)

func TestTableShape(t *testing.T) {
	t.Parallel()

	routes := Routes()
	if len(routes) != 3 {
		t.Fatalf("got %d routes, want 3", len(routes))
	}

	want := []struct{ method, path string }{
		{"GET", "/users"},
		{"POST", "/users"},
		{"GET", "/health"},
	}
	for i, w := range want {
		if routes[i].Method != w.method || routes[i].Path != w.path {
			t.Errorf("route %d = %s %s, want %s %s",
				i, routes[i].Method, routes[i].Path, w.method, w.path)
		}
	}
}

func TestIndexByPrefixGroups(t *testing.T) {
	t.Parallel()

	index := IndexByPrefix(Routes())
	if got := len(index["users"]); got != 2 {
		t.Errorf("users prefix has %d routes, want 2", got)
	}
	if got := len(index["health"]); got != 1 {
		t.Errorf("health prefix has %d routes, want 1", got)
	}
	if _, ok := index["missing"]; ok {
		t.Error("missing prefix should not be present")
	}
}

func TestMiddlewareSlicesAreIndependent(t *testing.T) {
	t.Parallel()

	routes := Routes()
	// Mutating route 0's middleware slice must not affect route 1's.
	before := len(routes[1].Middleware)
	routes[0].Middleware = append(routes[0].Middleware, tag("extra"))
	after := len(routes[1].Middleware)

	if before != after {
		t.Fatalf("route 1 middleware changed from %d to %d after mutating route 0",
			before, after)
	}
}

func TestChainAppliesMiddlewareOutermostFirst(t *testing.T) {
	t.Parallel()

	routes := Routes()
	got := routes[0].Chain()("req") // GET /users: auth, log around "list"
	if got != "auth:log:list" {
		t.Fatalf("chain = %q, want auth:log:list", got)
	}
}

// ExampleRoutes shows the nested literal's route count and the first route's full
// middleware chain, both deterministic.
func ExampleRoutes() {
	routes := Routes()
	fmt.Println(len(routes))
	fmt.Println(routes[0].Chain()("req"))
	// Output:
	// 3
	// auth:log:list
}
```

## Review

The table is correct when the nested literal yields exactly the routes declared —
the shape test golden-asserts count and method/path pairs — and when the prefix
index groups them without inventing keys. The load-bearing property is middleware
independence: because each route's `Middleware` is written as its own
`[]Middleware{...}` literal, the three chains have three distinct backing arrays,
and `TestMiddlewareSlicesAreIndependent` proves appending to one does not disturb
another. The bug this guards against is building every chain by reslicing one
shared base slice, which makes an `append` in one route silently overwrite
another's middleware. Note the `Chain` order: middleware is applied innermost-last
so the outermost wrapper runs first, which is why the GET `/users` chain renders
`auth:log:list`.

## Resources

- [Go Specification: Composite literals](https://go.dev/ref/spec#Composite_literals) — nested literals and inner-type elision.
- [Go blog: Slices intro](https://go.dev/blog/slices-intro) — why two slices can share a backing array, and how to avoid it.
- [net/http.ServeMux](https://pkg.go.dev/net/http#ServeMux) — the real router this table models.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-escape-analysis-constructor-cost.md](07-escape-analysis-constructor-cost.md)
