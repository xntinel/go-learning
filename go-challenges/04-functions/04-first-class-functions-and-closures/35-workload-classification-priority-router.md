# Exercise 35: Workload Classification and Priority-Based Pool Router

**Nivel: Intermedio** — validacion rapida (un test corto).

A request-processing system with multiple worker pools — a fast-path for
small reads, a batch pool for large writes, a dedicated analytics pool —
needs to decide which pool handles each request without a giant hand-rolled
`if`/`else` chain scattered across the codebase. `NewRouter` closes over an
ordered list of classifiers, each pairing a pool name with a predicate;
the returned closure checks classifiers in priority order and routes to the
first one that matches, falling back to a default pool if none do.

## What you'll build

```text
workload-router/             independent module: example.com/workload-router
  go.mod                      go 1.24
  workrouter.go                Request, Classifier, NewRouter returns func(Request) string
  cmd/
    demo/
      main.go                   four requests routed to fast-path/batch/analytics/default
  workrouter_test.go            table test: routing by classifier, priority order, statelessness
```

- Files: `workrouter.go`, `cmd/demo/main.go`, `workrouter_test.go`.
- Implement: `NewRouter(classifiers []Classifier, fallback string) func(req Request) string`, where `Classifier` pairs a pool name with a `func(Request) bool` predicate.
- Test: a table of requests routes to the pool named by the first classifier whose predicate matches; two classifiers that could both match the same request resolve to whichever is listed first; a request matching nothing routes to `fallback`; the router is a pure function — the same request always routes the same way.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/workload-router/cmd/demo
cd ~/go-exercises/workload-router
go mod init example.com/workload-router
go mod edit -go=1.24
```

### A slice of predicates instead of a switch statement

`NewRouter` closes over `classifiers`, an ordered `[]Classifier`, and
`fallback`, a pool name. The returned closure loops over `classifiers` in
order and returns the `Pool` of the first one whose `Match` predicate
returns `true` for the request; if none match, it returns `fallback`. The
order of the slice *is* the priority order — a classifier listed earlier
gets first refusal on every request, exactly like a chain of `if`/`else if`
branches, except each branch is a value that can be built, tested, and
reordered independently instead of hardcoded into one function body. Adding
a new pool for a new workload shape means appending one `Classifier`
literal, not touching a monolithic conditional.

The router keeps no state between calls — no counters, no map, nothing
captured except the read-only `classifiers` slice and `fallback` string —
so it is trivially safe to call from as many goroutines as want to route
requests concurrently, and the same `Request` always resolves to the same
pool.

Create `workrouter.go`:

```go
// Package workrouter routes requests to a named worker pool based on
// configured classifiers, with no per-request state stored anywhere.
package workrouter

// Request is the minimal shape a router classifies: a kind label and a size
// hint (payload bytes, row count, whatever the classifiers care about).
type Request struct {
	Kind string
	Size int
}

// Classifier names a pool and the predicate that routes a Request to it.
type Classifier struct {
	Pool  string
	Match func(Request) bool
}

// NewRouter returns a closure over an ordered list of classifiers and a
// fallback pool name. The router evaluates classifiers in order and routes
// to the first one whose Match returns true for the request; a request
// matching none of them routes to fallback. Classifier order is the
// priority order: put the most specific or most urgent classifier first.
// No per-request state is stored anywhere -- the router is a pure function
// of its captured classifiers and each call's Request.
func NewRouter(classifiers []Classifier, fallback string) func(req Request) string {
	return func(req Request) string {
		for _, c := range classifiers {
			if c.Match(req) {
				return c.Pool
			}
		}
		return fallback
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/workload-router"
)

func main() {
	classifiers := []workrouter.Classifier{
		{Pool: "fast-path", Match: func(r workrouter.Request) bool {
			return r.Kind == "read" && r.Size < 100
		}},
		{Pool: "batch", Match: func(r workrouter.Request) bool {
			return r.Kind == "write" && r.Size >= 1000
		}},
		{Pool: "analytics", Match: func(r workrouter.Request) bool {
			return r.Kind == "analytics"
		}},
	}

	route := workrouter.NewRouter(classifiers, "default")

	requests := []workrouter.Request{
		{Kind: "read", Size: 10},
		{Kind: "write", Size: 5000},
		{Kind: "analytics", Size: 50},
		{Kind: "write", Size: 20},
	}

	for _, r := range requests {
		fmt.Printf("%+v -> %s\n", r, route(r))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{Kind:read Size:10} -> fast-path
{Kind:write Size:5000} -> batch
{Kind:analytics Size:50} -> analytics
{Kind:write Size:20} -> default
```

### Tests

Create `workrouter_test.go`:

```go
package workrouter

import "testing"

func testClassifiers() []Classifier {
	return []Classifier{
		{Pool: "fast-path", Match: func(r Request) bool {
			return r.Kind == "read" && r.Size < 100
		}},
		{Pool: "batch", Match: func(r Request) bool {
			return r.Kind == "write" && r.Size >= 1000
		}},
		{Pool: "analytics", Match: func(r Request) bool {
			return r.Kind == "analytics"
		}},
	}
}

func TestRouterRoutesByClassifier(t *testing.T) {
	route := NewRouter(testClassifiers(), "default")

	tests := []struct {
		name string
		req  Request
		want string
	}{
		{"small read: fast-path", Request{Kind: "read", Size: 10}, "fast-path"},
		{"large write: batch", Request{Kind: "write", Size: 5000}, "batch"},
		{"analytics kind: analytics", Request{Kind: "analytics", Size: 50}, "analytics"},
		{"small write: no classifier matches, falls back", Request{Kind: "write", Size: 20}, "default"},
		{"large read: no classifier matches, falls back", Request{Kind: "read", Size: 500}, "default"},
	}

	for _, tc := range tests {
		if got := route(tc.req); got != tc.want {
			t.Fatalf("%s: route(%+v) = %q, want %q", tc.name, tc.req, got, tc.want)
		}
	}
}

func TestRouterUsesFirstMatchingClassifier(t *testing.T) {
	// Two classifiers that could both match the same request; priority
	// order (first in the slice) must win.
	classifiers := []Classifier{
		{Pool: "urgent", Match: func(r Request) bool { return r.Kind == "alert" }},
		{Pool: "generic", Match: func(r Request) bool { return true }}, // matches everything
	}
	route := NewRouter(classifiers, "default")

	if got := route(Request{Kind: "alert", Size: 1}); got != "urgent" {
		t.Fatalf("route(alert) = %q, want %q (first matching classifier wins)", got, "urgent")
	}
	if got := route(Request{Kind: "other", Size: 1}); got != "generic" {
		t.Fatalf("route(other) = %q, want %q", got, "generic")
	}
}

func TestRouterIsStatelessAcrossCalls(t *testing.T) {
	route := NewRouter(testClassifiers(), "default")

	req := Request{Kind: "read", Size: 10}
	first := route(req)
	for range 5 {
		if got := route(req); got != first {
			t.Fatalf("route(%+v) = %q on repeat call, want stable %q", req, got, first)
		}
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The routing table pins down five representative requests, including two
that fall through every classifier to `fallback` — the case a hand-rolled
`if`/`else` chain most often gets wrong by forgetting a final `else`
branch. The priority test proves classifier order is meaningful: a
catch-all classifier listed second never shadows a more specific one listed
first. The statelessness test is the structural guarantee this router
relies on instead of a cache or a counter: the same request always resolves
to the same pool, purely from the captured, read-only classifier slice.

## Resources

- [Go spec: Function types](https://go.dev/ref/spec#Function_types) — the `func(Request) bool` predicate type each `Classifier` carries.
- [Kubernetes docs: PriorityClass](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-priority-preemption/) — priority-ordered routing to different resource pools, the same shape this router implements for requests.
- [pkg.go.dev: sort.Slice](https://pkg.go.dev/sort#Slice) — an alternative way to think about "priority order" as an explicit ordering property of a slice, the same property `classifiers` relies on here.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [34-multi-destination-event-publisher-dlq.md](34-multi-destination-event-publisher-dlq.md) | Next: [../05-anonymous-functions/00-concepts.md](../05-anonymous-functions/00-concepts.md)
