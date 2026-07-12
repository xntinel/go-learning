# Exercise 6: Longest-Prefix Match for API Version Routing

An API gateway routes a request to a backend by matching the request path
against a table of registered prefixes, choosing the longest match. The subtle
bug is the segment boundary: `/api/v1` must not match `/api/v10`. This exercise
builds that matcher with `HasPrefix`, `CutPrefix`, and an explicit boundary
check.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
router/                         independent module: example.com/router
  go.mod                        go 1.26
  router.go                     Match(path, routes) -> (prefix, rest, ok)
  router_test.go                table test incl. the /api/v1 vs /api/v10 boundary
  cmd/
    demo/
      main.go                   runnable demo
```

Files: `router.go`, `router_test.go`, `cmd/demo/main.go`.
Implement: `Match(path string, routes []string) (prefix string, rest string, ok bool)`.
Test: exact prefix, longest of two overlapping prefixes, the `/api/v1` vs
`/api/v10` boundary, trailing slash, no match, empty path.
Verify: `go test -count=1 -race ./...`

### The boundary bug that HasPrefix alone creates

`strings.HasPrefix("/api/v10/x", "/api/v1")` is `true` — `/api/v1` really is a
byte prefix of `/api/v10/x`. If you route on raw `HasPrefix`, a request for
`/api/v10` is misrouted to the `/api/v1` backend. The fix is a segment-boundary
rule: a route matches a path only when the path equals the route exactly, or the
character immediately after the route is a `/`. So `/api/v1` matches `/api/v1`
and `/api/v1/users` but not `/api/v10`.

`matches` encodes exactly that. `strings.CutPrefix(path, route)` returns the
remainder after the prefix and a bool for whether the prefix was present; if it
was, the match is valid only when the remainder is empty (exact) or begins with
`/` (segment boundary). Among all matching routes, the longest one wins — a
gateway with both `/api` and `/api/v1` registered sends `/api/v1/users` to the
more specific `/api/v1`. The remaining path segment is what `CutPrefix` already
returned, so the caller gets `(prefix, rest, true)` and can dispatch on the rest.

Trailing slashes are normalized once up front: a single trailing `/` on a
non-root path is trimmed so `/api/v1/` and `/api/v1` route identically. An empty
path matches nothing.

Create `router.go`:

```go
package router

import "strings"

// Match returns the longest registered route that is a segment-prefix of path,
// the remaining path after that prefix, and whether any route matched. A route
// matches only at a segment boundary: "/api/v1" matches "/api/v1/users" but not
// "/api/v10".
func Match(path string, routes []string) (string, string, bool) {
	if path == "" {
		return "", "", false
	}
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		path = strings.TrimSuffix(path, "/")
	}

	best := ""
	for _, route := range routes {
		if route == "" {
			continue
		}
		if matches(path, route) && len(route) > len(best) {
			best = route
		}
	}
	if best == "" {
		return "", "", false
	}
	rest, _ := strings.CutPrefix(path, best)
	return best, rest, true
}

// matches reports whether route is a segment-prefix of path.
func matches(path, route string) bool {
	if path == route {
		return true
	}
	rest, ok := strings.CutPrefix(path, route)
	return ok && strings.HasPrefix(rest, "/")
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/router"
)

func main() {
	routes := []string{"/api", "/api/v1", "/api/v10"}
	paths := []string{"/api/v1/users", "/api/v10/users", "/api/health", "/status"}
	for _, p := range paths {
		prefix, rest, ok := router.Match(p, routes)
		if !ok {
			fmt.Printf("%-16s -> no route\n", p)
			continue
		}
		fmt.Printf("%-16s -> route %-9s rest %q\n", p, prefix, rest)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/api/v1/users    -> route /api/v1   rest "/users"
/api/v10/users   -> route /api/v10  rest "/users"
/api/health      -> route /api      rest "/health"
/status          -> no route
```

### Tests

Create `router_test.go`:

```go
package router

import (
	"fmt"
	"testing"
)

func TestMatch(t *testing.T) {
	t.Parallel()

	routes := []string{"/api", "/api/v1", "/api/v10"}

	tests := []struct {
		name       string
		path       string
		wantPrefix string
		wantRest   string
		wantOK     bool
	}{
		{name: "exact", path: "/api/v1", wantPrefix: "/api/v1", wantRest: "", wantOK: true},
		{name: "longest wins", path: "/api/v1/users", wantPrefix: "/api/v1", wantRest: "/users", wantOK: true},
		{name: "boundary not v10", path: "/api/v10/x", wantPrefix: "/api/v10", wantRest: "/x", wantOK: true},
		{name: "falls back to /api", path: "/api/health", wantPrefix: "/api", wantRest: "/health", wantOK: true},
		{name: "trailing slash", path: "/api/v1/", wantPrefix: "/api/v1", wantRest: "", wantOK: true},
		{name: "no match", path: "/status", wantOK: false},
		{name: "empty path", path: "", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prefix, rest, ok := Match(tc.path, routes)
			if ok != tc.wantOK {
				t.Fatalf("Match(%q) ok = %v, want %v", tc.path, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if prefix != tc.wantPrefix || rest != tc.wantRest {
				t.Fatalf("Match(%q) = (%q, %q), want (%q, %q)", tc.path, prefix, rest, tc.wantPrefix, tc.wantRest)
			}
		})
	}
}

func TestMatchBoundaryDoesNotFalseMatch(t *testing.T) {
	t.Parallel()

	// With only /api/v1 registered, /api/v10 must NOT match.
	if _, _, ok := Match("/api/v10", []string{"/api/v1"}); ok {
		t.Fatal("/api/v10 wrongly matched /api/v1")
	}
}

func ExampleMatch() {
	prefix, rest, ok := Match("/api/v1/users", []string{"/api", "/api/v1"})
	fmt.Println(prefix, rest, ok)
	// Output: /api/v1 /users true
}
```

## Review

The matcher is correct when the longest matching route wins and matching happens
only at a segment boundary — the `/api/v1` vs `/api/v10` case is the whole point,
and `TestMatchBoundaryDoesNotFalseMatch` pins it. The trap is routing on bare
`strings.HasPrefix`, which treats `/api/v1` as a prefix of `/api/v10` and
misroutes. `CutPrefix` gives you the remainder and the found bool in one call, so
the boundary check is a single `HasPrefix(rest, "/")`. Confirm with
`go test -race`; a production router (`http.ServeMux` since Go 1.22) does this
segment-aware matching for you, but knowing the rule is how you debug a
misrouted request.

## Resources

- [strings.HasPrefix](https://pkg.go.dev/strings#HasPrefix) and [strings.CutPrefix](https://pkg.go.dev/strings#CutPrefix).
- [net/http.ServeMux](https://pkg.go.dev/net/http#ServeMux) — Go 1.22 pattern matching with method and path segments.
- [Routing enhancements for Go 1.22](https://go.dev/blog/routing-enhancements) — how the standard mux handles precedence.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-dotenv-config-loader.md](05-dotenv-config-loader.md) | Next: [07-secret-log-redactor.md](07-secret-log-redactor.md)
