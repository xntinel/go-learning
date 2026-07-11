# Exercise 4: HTTP Route Table: Registering Per-Route Handlers in a Loop

A router builds its handlers by iterating a table of route definitions and
registering one `http.HandlerFunc` closure per route on a `ServeMux`. Each
closure captures that route's config. This is real router-setup code, and it is
where loop capture used to make every path serve the last route's payload. You
build it and prove, with `httptest`, that each registered handler serves ITS
route.

## What you'll build

```text
routetable/                  independent module: example.com/routetable
  go.mod                     go 1.26
  router.go                  Route, BuildMux registering a closure per route
  cmd/
    demo/
      main.go                runnable demo: build mux, hit each route with httptest
  router_test.go             per-route body assertion, last-route regression guard
```

- Files: `router.go`, `cmd/demo/main.go`, `router_test.go`.
- Implement: `BuildMux(routes)` that registers one handler closure per `Route`, each capturing its own route config and writing that route's payload.
- Test: register N routes from a table, drive each path with `httptest.NewRecorder`, and assert each response body and header reflect that route's captured config; a shared capture would make every path return the last route's payload.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/routetable/cmd/demo
cd ~/go-exercises/routetable
go mod init example.com/routetable
go mod edit -go=1.26
```

### Why each handler must capture its own route

`http.ServeMux` maps a pattern to a `http.Handler`. When you register handlers in
a loop, each `HandlerFunc` closes over the loop variable `route`. Before Go 1.22,
all N closures aliased the same `route` variable, so after the loop every handler
read the final route's config — every path returned the last route's body and
status. That produced the classic "all my endpoints return the same thing" bug
that only shows up at request time, long after registration looked fine.

On a `go 1.26` module each iteration has its own `route`, so the direct capture
is correct. This exercise keeps the direct capture (that is the modern idiom) and
proves it with a test that would fail loudly under the old semantics: it drives
every path and asserts the body names that specific route. The test is the
safety net that a version downgrade or a hand-rolled `for i := 0; ...` loop over
`routes[i]` (which would reintroduce a shared index) cannot pass.

Each `Route` carries a method, a path, and a payload. The handler checks the
method (returning 405 on mismatch, using `http.StatusText` rather than a
hard-coded string), sets an `X-Route` header to the route's path so a test can
confirm identity even on the header, and writes the payload. Go 1.22's `ServeMux`
supports method-and-path patterns like `"GET /health"`, but to keep the method
check explicit and testable we register on the path and validate the method in
the handler.

Create `router.go`:

```go
package routetable

import (
	"fmt"
	"net/http"
)

// Route is one row of the route table.
type Route struct {
	Method  string
	Path    string
	Payload string
}

// BuildMux registers one handler closure per route. Each closure captures its
// own per-iteration route, so every path serves its own payload.
func BuildMux(routes []Route) *http.ServeMux {
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.HandleFunc(route.Path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != route.Method {
				w.Header().Set("Allow", route.Method)
				http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("X-Route", route.Path)
			fmt.Fprint(w, route.Payload)
		})
	}
	return mux
}
```

### The runnable demo

The demo builds a three-route table and drives each path with an in-memory
`httptest` recorder — no network — printing the path, status, and body so you can
see each route serving its own payload.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http/httptest"

	"example.com/routetable"
)

func main() {
	routes := []routetable.Route{
		{Method: "GET", Path: "/health", Payload: "ok"},
		{Method: "GET", Path: "/version", Payload: "v1.4.2"},
		{Method: "GET", Path: "/ready", Payload: "ready"},
	}
	mux := routetable.BuildMux(routes)

	for _, route := range routes {
		req := httptest.NewRequest(route.Method, route.Path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		fmt.Printf("%s %d %s\n", route.Path, rec.Code, rec.Body.String())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/health 200 ok
/version 200 v1.4.2
/ready 200 ready
```

### Tests

`TestEachRouteServesOwnPayload` is the capture guard: it drives every path and
asserts the body and `X-Route` header match that route, not the last-registered
one. `TestMethodMismatchReturns405` confirms the per-route method check by hitting
a GET route with POST.

Create `router_test.go`:

```go
package routetable

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testRoutes() []Route {
	return []Route{
		{Method: "GET", Path: "/a", Payload: "payload-a"},
		{Method: "GET", Path: "/b", Payload: "payload-b"},
		{Method: "GET", Path: "/c", Payload: "payload-c"},
		{Method: "GET", Path: "/d", Payload: "payload-d"},
	}
}

func TestEachRouteServesOwnPayload(t *testing.T) {
	t.Parallel()

	routes := testRoutes()
	mux := BuildMux(routes)

	for _, route := range routes {
		req := httptest.NewRequest(route.Method, route.Path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", route.Path, rec.Code)
		}
		if got := rec.Body.String(); got != route.Payload {
			t.Errorf("%s: body = %q, want %q (loop-capture bug serves last route)", route.Path, got, route.Payload)
		}
		if got := rec.Header().Get("X-Route"); got != route.Path {
			t.Errorf("%s: X-Route = %q, want %q", route.Path, got, route.Path)
		}
	}
}

func TestMethodMismatchReturns405(t *testing.T) {
	t.Parallel()

	mux := BuildMux([]Route{{Method: "GET", Path: "/only-get", Payload: "x"}})
	req := httptest.NewRequest(http.MethodPost, "/only-get", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET" {
		t.Fatalf("Allow = %q, want GET", got)
	}
}

func ExampleBuildMux() {
	mux := BuildMux([]Route{{Method: "GET", Path: "/ping", Payload: "pong"}})
	req := httptest.NewRequest("GET", "/ping", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	fmt.Println(rec.Body.String())
	// Output: pong
}
```

## Review

The router is correct when driving every registered path returns that route's own
payload and `X-Route` header, and a method mismatch returns 405 with the right
`Allow`. `TestEachRouteServesOwnPayload` is the regression guard: under the
pre-1.22 shared-variable semantics every path would return `payload-d` and the
test would fail on the first three routes. Keep it a `range` loop over the routes,
not a `for i := 0; ...` over `routes[i]` that captures a shared `i`. The 405 path
uses `http.StatusText` and sets `Allow`, matching what a real handler owes a
client. Run `go test -race`; `httptest` keeps it fully in-memory with no network.

## Resources

- [`net/http.ServeMux`](https://pkg.go.dev/net/http#ServeMux) — pattern registration and matching.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest`, `NewRecorder` for in-memory handler tests.
- [Go 1.22 release notes: enhanced ServeMux patterns](https://go.dev/doc/go1.22#enhanced_routing_patterns) — method-and-path routing.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-timer-expiry-scheduler.md](05-timer-expiry-scheduler.md)
