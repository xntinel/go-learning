# Exercise 1: Request Router

The router is the part of an L7 proxy that turns a parsed HTTP request into a routing decision: given a host and a path, which upstream gets the request. This exercise builds it as a standalone module — an ordered rule table, first-match-wins evaluation, port-suffix stripping on the host, and a sentinel error for the no-match case.

This module is fully self-contained: its own `go mod init`, every type it needs defined inline, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
router.go            Router, Route, HeaderRule, NewRouter, Match (first match wins)
cmd/
  demo/
    main.go          probe a four-rule table; show catch-all and ErrNoRoute
router_test.go       host match, port stripping, path prefix, first-rule-wins, ErrNoRoute
```

- Files: `router.go`, `cmd/demo/main.go`, `router_test.go`.
- Implement: `Router` with `NewRouter(routes []Route) *Router` and `Match(host, path string) (Route, error)`; the `Route` and `HeaderRule` value types; and the `ErrNoRoute` sentinel.
- Test: match by host, port-suffix stripping, match by path prefix, declaration-order first match, catch-all, and `errors.Is(err, ErrNoRoute)` on no match.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Ordered rules, port stripping, and a sentinel error

A routing table is just a slice of `Route` values, and the whole policy is encoded in their order. `Match` walks the slice and returns the first rule whose constraints the request satisfies. A `Route` has two optional constraints: `Host` and `PathPrefix`. An empty constraint matches anything, so a `Route` with both empty is a catch-all. Because evaluation is first-match-wins, the table must be ordered most-specific first: a broad `/api/` rule placed ahead of a narrow `/api/v2/` rule would shadow it, and the narrow rule would never run. The catch-all, matching everything, belongs last or it shadows every rule after it.

One parsing detail makes host routing actually work. An HTTP/1.1 `Host` header frequently carries a port suffix — `api.example.com:443` — but route rules are written against the bare hostname. So `Match` strips a trailing `:port` before comparing. The strip uses `strings.LastIndexByte(host, ':')` guarded by `i > 0`, which finds the port separator without tripping over the leading colon of an IPv6 literal at position zero.

When nothing matches, `Match` returns `ErrNoRoute` wrapped with the host and path via `fmt.Errorf("%w: ...")`. Wrapping with `%w` keeps the sentinel identity intact, so a caller can write `errors.Is(err, ErrNoRoute)` to distinguish "no route" from any other failure while still getting a descriptive message in logs.

Create `router.go`:

```go
package router

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ErrNoRoute is returned by Router.Match when no rule matches.
var ErrNoRoute = errors.New("router: no matching route")

// Route describes one routing rule. An empty Host or PathPrefix matches anything.
type Route struct {
	Host       string        // empty matches any host
	PathPrefix string        // empty matches any path
	Upstream   *url.URL      // where to forward matching requests
	Timeout    time.Duration // per-route deadline; 0 inherits the proxy default
	Headers    []HeaderRule  // applied after hop-by-hop headers are stripped
}

// HeaderRule adds, sets, or removes one header on the outbound request.
type HeaderRule struct {
	Action string // "add", "set", or "remove"
	Name   string
	Value  string // ignored for "remove"
}

// Router holds an ordered list of routes and returns the first match.
// Rules are evaluated in declaration order; the first match wins.
type Router struct {
	routes []Route
}

// NewRouter returns a Router backed by routes.
func NewRouter(routes []Route) *Router {
	return &Router{routes: routes}
}

// Match returns the first route whose Host and PathPrefix constraints are
// satisfied by host and path. Port suffixes on host are stripped before
// comparison.
func (r *Router) Match(host, path string) (Route, error) {
	// Strip the port suffix present in HTTP/1.1 Host header values.
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	for _, route := range r.routes {
		if route.Host != "" && route.Host != host {
			continue
		}
		if route.PathPrefix != "" && !strings.HasPrefix(path, route.PathPrefix) {
			continue
		}
		return route, nil
	}
	return Route{}, fmt.Errorf("%w: host=%q path=%q", ErrNoRoute, host, path)
}
```

`Match` reads top to bottom: skip a rule whose non-empty `Host` differs from the request host, skip a rule whose non-empty `PathPrefix` is not a prefix of the path, and otherwise return it. An empty `Host` or `PathPrefix` falls through both guards and so matches unconditionally — that is what makes the trailing empty-empty rule a catch-all.

### The runnable demo

The demo builds a four-rule table — two host-scoped API versions, a static prefix, and a catch-all — and probes it with four requests, including one whose host carries a `:443` suffix. It then shows a second, catch-all-free router reporting `ErrNoRoute` through `errors.Is`. Output is deterministic: only upstream host names and a boolean are printed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"net/url"

	"example.com/request-router"
)

func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

func main() {
	rt := router.NewRouter([]router.Route{
		{Host: "api.example.com", PathPrefix: "/v2/", Upstream: mustURL("http://api-v2:8080")},
		{Host: "api.example.com", Upstream: mustURL("http://api-v1:8080")},
		{PathPrefix: "/static/", Upstream: mustURL("http://cdn:80")},
		{Upstream: mustURL("http://catch-all:80")}, // default, matches anything
	})

	type probe struct{ host, path string }
	for _, p := range []probe{
		{"api.example.com:443", "/v2/orders"},
		{"api.example.com", "/users"},
		{"web.example.com", "/static/logo.png"},
		{"other.example.com", "/"},
	} {
		route, err := rt.Match(p.host, p.path)
		if err != nil {
			fmt.Printf("host=%-20s path=%-18s -> ERROR %v\n", p.host, p.path, err)
			continue
		}
		fmt.Printf("host=%-20s path=%-18s -> %s\n", p.host, p.path, route.Upstream.Host)
	}

	// A router with no catch-all reports ErrNoRoute via errors.Is.
	strict := router.NewRouter([]router.Route{
		{Host: "only.example.com", Upstream: mustURL("http://b:80")},
	})
	_, err := strict.Match("missing.example.com", "/")
	fmt.Printf("strict no-match is ErrNoRoute: %v\n", errors.Is(err, router.ErrNoRoute))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
host=api.example.com:443  path=/v2/orders         -> api-v2:8080
host=api.example.com      path=/users             -> api-v1:8080
host=web.example.com      path=/static/logo.png   -> cdn:80
host=other.example.com    path=/                  -> catch-all:80
strict no-match is ErrNoRoute: true
```

The first probe carries a `:443` port suffix and still matches `api.example.com`, because `Match` strips the port. The `/v2/` path lands on the version-two backend ahead of the bare-host version-one rule, since the more specific rule is declared first. The unmatched `other.example.com` request falls through to the catch-all. The strict router, having no catch-all, reports the miss as `ErrNoRoute`.

### Tests

The tests pin the behavior the design depends on: a host match, the port-suffix strip, a path-prefix match, declaration-order precedence, the catch-all, and the `ErrNoRoute` sentinel through `errors.Is`.

Create `router_test.go`:

```go
package router

import (
	"errors"
	"net/url"
	"testing"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

func TestMatchByHost(t *testing.T) {
	t.Parallel()
	u := mustParseURL(t, "http://backend:8080")
	r := NewRouter([]Route{{Host: "api.example.com", Upstream: u}})

	got, err := r.Match("api.example.com", "/any")
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got.Upstream != u {
		t.Fatal("upstream mismatch")
	}
}

func TestMatchStripsPort(t *testing.T) {
	t.Parallel()
	u := mustParseURL(t, "http://backend:8080")
	r := NewRouter([]Route{{Host: "api.example.com", Upstream: u}})

	// HTTP/1.1 Host headers frequently include the port.
	if _, err := r.Match("api.example.com:443", "/"); err != nil {
		t.Fatalf("Match with port: %v", err)
	}
}

func TestMatchByPathPrefix(t *testing.T) {
	t.Parallel()
	v1 := mustParseURL(t, "http://v1:80")
	v2 := mustParseURL(t, "http://v2:80")
	r := NewRouter([]Route{
		{PathPrefix: "/api/v2/", Upstream: v2},
		{PathPrefix: "/api/v1/", Upstream: v1},
	})

	cases := []struct {
		path string
		want *url.URL
	}{
		{"/api/v1/users", v1},
		{"/api/v2/orders", v2},
	}
	for _, tc := range cases {
		got, err := r.Match("", tc.path)
		if err != nil {
			t.Fatalf("Match(%q): %v", tc.path, err)
		}
		if got.Upstream != tc.want {
			t.Errorf("path=%q: upstream = %v, want %v", tc.path, got.Upstream, tc.want)
		}
	}
}

func TestFirstRuleWins(t *testing.T) {
	t.Parallel()
	u1 := mustParseURL(t, "http://first:80")
	u2 := mustParseURL(t, "http://second:80")
	r := NewRouter([]Route{
		{PathPrefix: "/api/", Upstream: u1},
		{PathPrefix: "/api/v1/", Upstream: u2},
	})

	got, err := r.Match("", "/api/v1/users")
	if err != nil {
		t.Fatal(err)
	}
	if got.Upstream != u1 {
		t.Errorf("first rule should win, got %v", got.Upstream)
	}
}

func TestCatchAllMatchesAnything(t *testing.T) {
	t.Parallel()
	specific := mustParseURL(t, "http://api:80")
	def := mustParseURL(t, "http://default:80")
	r := NewRouter([]Route{
		{PathPrefix: "/api/", Upstream: specific},
		{Upstream: def}, // empty Host and PathPrefix: catch-all
	})

	got, err := r.Match("anything.example.com", "/random/path")
	if err != nil {
		t.Fatalf("catch-all should match: %v", err)
	}
	if got.Upstream != def {
		t.Errorf("upstream = %v, want catch-all %v", got.Upstream, def)
	}
}

func TestNoMatchReturnsErrNoRoute(t *testing.T) {
	t.Parallel()
	r := NewRouter([]Route{{Host: "specific.example.com", Upstream: mustParseURL(t, "http://b:80")}})
	_, err := r.Match("other.example.com", "/")
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("err = %v, want ErrNoRoute", err)
	}
}
```

## Review

The router is correct when order is honored and the host is normalized. The single most common bug is table order: a broad rule placed before a narrow one silently wins every time, and the narrow rule becomes dead code — `TestFirstRuleWins` is the guard, asserting that the `/api/` rule declared first beats the `/api/v1/` rule declared second for a `/api/v1/...` request. The second easy mistake is forgetting that the `Host` header carries a port; `TestMatchStripsPort` sends `api.example.com:443` and expects the bare-host rule to match. The catch-all contract — empty host and empty path prefix match anything — is pinned by `TestCatchAllMatchesAnything`, and the sentinel contract by `TestNoMatchReturnsErrNoRoute` using `errors.Is`, which is what lets callers branch on "no route" without string-matching the message.

## Resources

- [`strings.HasPrefix`](https://pkg.go.dev/strings#HasPrefix) — the path-prefix test at the heart of prefix routing.
- [`errors.Is` and `%w` wrapping](https://pkg.go.dev/errors#Is) — how the `ErrNoRoute` sentinel survives `fmt.Errorf` wrapping so callers can branch on it.
- [`net/url.URL`](https://pkg.go.dev/net/url#URL) — the upstream target type each route carries.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-header-rewriting.md](02-header-rewriting.md)
