# Exercise 9: Route request paths with allocation-free prefix matching

Stripping a versioned API prefix and peeling off the next path segment is the core
of every hand-rolled router. Done with manual index slicing it panics on the
inputs that lack the prefix; done with `strings.CutPrefix` and `strings.Cut` it is
prefix-safe by construction. This module builds that router.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
router/                     independent module: example.com/router
  go.mod                    go 1.26
  router.go                 Route(path) (resource, rest string, ok bool)
  cmd/
    demo/
      main.go               route a few real request paths
  router_test.go            prefix/segment table incl. the no-prefix no-panic case
```

Files: `router.go`, `cmd/demo/main.go`, `router_test.go`.
Implement: `Route(path string) (resource, rest string, ok bool)`.
Test: a full path, a missing prefix (`ok=false`, no panic), a trailing slash, the
root path, and a resource-only path with no rest.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/01-string-basics/09-prefix-router/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/01-string-basics/09-prefix-router
```

## Why CutPrefix and Cut instead of index math

The manual version is a panic waiting to happen:

```go
// Wrong: assumes the prefix is present and there is a slash after it.
rest := path[len("/api/v1"):]        // out of range if path is shorter
resource := rest[:strings.Index(rest, "/")]  // panics when Index returns -1
```

`strings.CutPrefix(path, "/api/v1")` returns `(rest, found)`: `found` reports
whether the prefix was there, and when it was not, `rest` is the untouched
original — no slice-out-of-range risk. We reject a non-matching prefix by
returning `ok=false` instead of slicing blindly.

After the prefix is stripped, the remaining path looks like `/users/123`. We drop
the single leading slash with another `CutPrefix(rest, "/")`, then split off the
first segment with `strings.Cut(rest, "/")`: `resource, sub, _ := Cut(rest, "/")`
gives `resource == "users"` and `sub == "123"`. When there is no further slash
(`/users`), `Cut` returns the whole thing as `resource` and an empty `rest` with
`found == false` — exactly the right shape, and again no index arithmetic. The
root path `/api/v1` or `/api/v1/` yields an empty resource, which we report as
`ok=false` because there is no resource to route to.

Every branch here is expressed through a `found`/`ok` bool rather than an index,
so no input — however short or malformed — can trigger an out-of-range panic. That
is the property the test pins.

Create `router.go`:

```go
package router

import "strings"

const apiPrefix = "/api/v1"

// Route strips the versioned API prefix and peels off the first path segment.
// It returns the resource, the remaining sub-path, and ok=false when the prefix
// is absent or no resource segment follows it. It never panics on any input.
func Route(path string) (resource, rest string, ok bool) {
	after, found := strings.CutPrefix(path, apiPrefix)
	if !found {
		return "", "", false
	}
	// The prefix must end on a segment boundary: "/api/v1" followed by "/" or
	// the end of the path. This rejects "/api/v11/x", which shares the literal
	// prefix but is a different version segment.
	if after != "" && !strings.HasPrefix(after, "/") {
		return "", "", false
	}
	// Drop the single leading slash between the prefix and the resource.
	after, _ = strings.CutPrefix(after, "/")

	resource, rest, _ = strings.Cut(after, "/")
	if resource == "" {
		return "", "", false
	}
	return resource, rest, true
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/router"
)

func main() {
	paths := []string{
		"/api/v1/users/123",
		"/api/v1/health",
		"/api/v1/",
		"/status",
	}
	for _, p := range paths {
		resource, rest, ok := router.Route(p)
		fmt.Printf("%-22q resource=%-8q rest=%-8q ok=%v\n", p, resource, rest, ok)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"/api/v1/users/123"    resource="users"  rest="123"    ok=true
"/api/v1/health"       resource="health" rest=""       ok=true
"/api/v1/"             resource=""       rest=""       ok=false
"/status"              resource=""       rest=""       ok=false
```

## Tests

The table pins each shape, and the critical case is `"/status"`: a path lacking
the prefix must return `ok=false` and must not panic. That is the whole argument
for `CutPrefix` over `path[len(prefix):]` — the `found` bool turns a would-be
out-of-range slice into a clean rejection.

Create `router_test.go`:

```go
package router

import (
	"fmt"
	"testing"
)

func TestRoute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		path         string
		wantResource string
		wantRest     string
		wantOK       bool
	}{
		{"full path", "/api/v1/users/123", "users", "123", true},
		{"resource only", "/api/v1/health", "health", "", true},
		{"nested sub-path", "/api/v1/users/123/roles", "users", "123/roles", true},
		{"trailing slash resource", "/api/v1/users/", "users", "", true},
		{"prefix with slash only", "/api/v1/", "", "", false},
		{"prefix no slash", "/api/v1", "", "", false},
		{"missing prefix", "/status", "", "", false},
		{"empty path", "", "", "", false},
		{"partial prefix", "/api", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resource, rest, ok := Route(tc.path)
			if resource != tc.wantResource || rest != tc.wantRest || ok != tc.wantOK {
				t.Fatalf("Route(%q) = %q,%q,%v; want %q,%q,%v",
					tc.path, resource, rest, ok, tc.wantResource, tc.wantRest, tc.wantOK)
			}
		})
	}
}

func TestRouteNeverPanics(t *testing.T) {
	t.Parallel()

	// No input, however short or malformed, may panic.
	for _, p := range []string{"", "/", "/a", "/api", "/api/", "/api/v", "/api/v1", "/api/v11/x"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Route(%q) panicked: %v", p, r)
				}
			}()
			Route(p)
		}()
	}
}

func ExampleRoute() {
	resource, rest, ok := Route("/api/v1/orders/42")
	fmt.Println(resource, rest, ok)
	// Output: orders 42 true
}
```

## Review

The router is correct when it strips the version prefix, returns the first segment
as the resource and the remainder as `rest`, and reports `ok=false` for any input
that lacks the prefix or carries no resource — all without a single index-based
slice. `TestRouteNeverPanics` is the guarantee that matters: the `found`/`ok`
bools from `CutPrefix` and `Cut` make every malformed input a clean rejection
rather than an out-of-range panic. Note the segment-boundary check: `"/api/v11/x"`
literally starts with `"/api/v1"`, but it is a different version segment, so the
router rejects it rather than routing `1` as a resource — the subtle bug that
naive literal-prefix routing ships. Run `go test -race`.

## Resources

- [strings.CutPrefix (pkg.go.dev)](https://pkg.go.dev/strings#CutPrefix)
- [strings.Cut (pkg.go.dev)](https://pkg.go.dev/strings#Cut)
- [strings.TrimPrefix (pkg.go.dev)](https://pkg.go.dev/strings#TrimPrefix)
- [The Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-scope-tokenizer.md](08-scope-tokenizer.md) | Next: [../02-byte-slices-vs-strings/00-concepts.md](../02-byte-slices-vs-strings/00-concepts.md)
