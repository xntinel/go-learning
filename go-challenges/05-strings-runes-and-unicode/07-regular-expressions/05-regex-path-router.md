# Exercise 5: Minimal HTTP Router with Named Path Params

Lightweight HTTP routers compile a route template like `/users/{id}/orders/{orderID}`
into an anchored regex with named capture groups, then match an incoming path and
hand back the extracted params. This module builds that core: a mux that turns
templates into `^...$` patterns, escapes the literal segments with `QuoteMeta`, and
returns a `map[string]string` of params — the mechanism under the hood of routers
you have used.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
router/                     independent module: example.com/router
  go.mod                    go 1.26
  router.go                 type Router; Add compiles template -> anchored named regex; Match
  cmd/
    demo/
      main.go               runnable demo: register routes, match a request path
  router_test.go            table-driven: two params, no-match, trailing segment, meta literal, first-wins
```

- Files: `router.go`, `cmd/demo/main.go`, `router_test.go`.
- Implement: `Router.Add(template string) error` compiling a template into an anchored `(?P<name>[^/]+)` regex with `QuoteMeta`-escaped literals; `Router.Match(path string) (map[string]string, bool)` returning named params, first-registered-wins.
- Test: two params extract by name; a non-matching path returns not-found; a trailing slash or extra segment does not match the anchored pattern; a literal segment with regex metacharacters is escaped so it matches literally; registration order is deterministic.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/router/cmd/demo
cd ~/go-exercises/router
go mod init example.com/router
```

### Compiling a template safely: anchors and QuoteMeta

Two correctness rules make this router safe. First, **anchor the pattern with
`^...$`.** A route regex that is not anchored matches any path that merely
*contains* the template shape, so `/users/{id}` would match `/admin/users/42` —
a routing bug and a potential authorization bypass. Anchoring makes matching a
whole-path test. Second, **`QuoteMeta` the literal segments.** The template's fixed
text (`users`, `orders`, a segment like `v1.2`) may contain regex metacharacters;
if you drop it into the pattern raw, a `.` becomes "any character" and a user could
hit `/usersX...`. Escaping the literals with `QuoteMeta` is the same discipline as
parameterizing a query: user-influenced literal text must not be able to change the
pattern's structure.

`Add` walks the template, replacing each `{name}` placeholder with
`(?P<name>[^/]+)` (one path segment, no slash) and escaping everything between
placeholders with `QuoteMeta`. It uses a placeholder-finding regex over the
template — a small, controlled internal use — and builds the final anchored
pattern with a `strings.Builder`. `Match` tries each compiled route in
registration order and returns the first hit's named captures, so overlap
resolution is deterministic and documented (first-registered-wins), the same rule
most routers use.

Create `router.go`:

```go
package router

import (
	"fmt"
	"regexp"
	"strings"
)

// placeholderRe finds {name} segments in a route template.
var placeholderRe = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

type route struct {
	template string
	re       *regexp.Regexp
}

// Router matches request paths against registered templates in registration order.
type Router struct {
	routes []route
}

// Add compiles template into an anchored, named-capture regex. Literal segments
// are QuoteMeta-escaped; {name} becomes (?P<name>[^/]+).
func (r *Router) Add(template string) error {
	var b strings.Builder
	b.WriteString("^")
	last := 0
	for _, loc := range placeholderRe.FindAllStringSubmatchIndex(template, -1) {
		// loc: [matchStart, matchEnd, nameStart, nameEnd]
		b.WriteString(regexp.QuoteMeta(template[last:loc[0]]))
		name := template[loc[2]:loc[3]]
		b.WriteString("(?P<" + name + ">[^/]+)")
		last = loc[1]
	}
	b.WriteString(regexp.QuoteMeta(template[last:]))
	b.WriteString("$")

	re, err := regexp.Compile(b.String())
	if err != nil {
		return fmt.Errorf("router: compile template %q: %w", template, err)
	}
	r.routes = append(r.routes, route{template: template, re: re})
	return nil
}

// Match returns the named params of the first registered template that matches
// path, or (nil, false) if none does.
func (r *Router) Match(path string) (map[string]string, bool) {
	for _, rt := range r.routes {
		m := rt.re.FindStringSubmatch(path)
		if m == nil {
			continue
		}
		params := make(map[string]string)
		for i, name := range rt.re.SubexpNames() {
			if name != "" {
				params[name] = m[i]
			}
		}
		return params, true
	}
	return nil, false
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/router"
)

func main() {
	var r router.Router
	for _, tmpl := range []string{
		"/users/{id}/orders/{orderID}",
		"/health",
	} {
		if err := r.Add(tmpl); err != nil {
			log.Fatal(err)
		}
	}

	params, ok := r.Match("/users/42/orders/1007")
	fmt.Printf("match=%v id=%s orderID=%s\n", ok, params["id"], params["orderID"])

	_, ok = r.Match("/users/42/orders")
	fmt.Printf("partial-path match=%v\n", ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
match=true id=42 orderID=1007
partial-path match=false
```

### Tests

Create `router_test.go`:

```go
package router

import (
	"testing"
)

func TestMatchTwoParams(t *testing.T) {
	t.Parallel()
	var r Router
	if err := r.Add("/users/{id}/orders/{orderID}"); err != nil {
		t.Fatal(err)
	}
	params, ok := r.Match("/users/42/orders/1007")
	if !ok {
		t.Fatal("expected match")
	}
	if params["id"] != "42" || params["orderID"] != "1007" {
		t.Fatalf("params = %v, want id=42 orderID=1007", params)
	}
}

func TestMatchNotFound(t *testing.T) {
	t.Parallel()
	var r Router
	if err := r.Add("/users/{id}"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Match("/accounts/42"); ok {
		t.Fatal("unexpected match on different path")
	}
}

func TestAnchoredRejectsExtraSegments(t *testing.T) {
	t.Parallel()
	var r Router
	if err := r.Add("/users/{id}"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/users/42/", "/users/42/extra", "/prefix/users/42"} {
		if _, ok := r.Match(path); ok {
			t.Fatalf("anchored route matched %q", path)
		}
	}
}

func TestLiteralMetacharsEscaped(t *testing.T) {
	t.Parallel()
	var r Router
	// The literal segment "v1.2" contains a '.', which must match a literal dot,
	// not any character.
	if err := r.Add("/api/v1.2/{resource}"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Match("/api/v1.2/widgets"); !ok {
		t.Fatal("expected literal dot to match itself")
	}
	if _, ok := r.Match("/api/v1X2/widgets"); ok {
		t.Fatal("unescaped dot matched any character")
	}
}

func TestFirstRegisteredWins(t *testing.T) {
	t.Parallel()
	var r Router
	if err := r.Add("/x/{a}"); err != nil {
		t.Fatal(err)
	}
	if err := r.Add("/x/{b}"); err != nil {
		t.Fatal(err)
	}
	params, ok := r.Match("/x/1")
	if !ok {
		t.Fatal("expected match")
	}
	if _, first := params["a"]; !first {
		t.Fatalf("params = %v, want first route (key a) to win", params)
	}
}
```

## Review

The router is correct when both safety rules hold. `TestAnchoredRejectsExtraSegments`
proves the `^...$` anchors turn matching into a whole-path test, so a longer or
prefixed path does not slip through — the class of bug that lets `/admin/users/42`
hit a `/users/{id}` route. `TestLiteralMetacharsEscaped` proves `QuoteMeta` makes
the literal `v1.2` match only `v1.2`, never `v1X2`, closing the pattern-injection
gap in the literal segments. Params come out by name via `SubexpNames`, and
`TestFirstRegisteredWins` pins the deterministic overlap rule. The one design note
worth stating: `[^/]+` binds a param to a single segment, so `{id}` never spans a
slash — matching how real routers scope path parameters. Run `go test -race`
because a `Router` is typically read concurrently after all routes are added.

## Resources

- [`regexp` package](https://pkg.go.dev/regexp) — `QuoteMeta`, `FindAllStringSubmatchIndex`, `FindStringSubmatch`, `SubexpNames`.
- [`net/http.ServeMux`](https://pkg.go.dev/net/http#ServeMux) — the stdlib router (Go 1.22+ method-and-wildcard patterns) to compare this mechanism against.
- [RE2 syntax reference](https://github.com/google/re2/wiki/Syntax) — anchors, character classes, and named captures.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-log-secret-redactor.md](04-log-secret-redactor.md) | Next: [06-semver-tag-validator.md](06-semver-tag-validator.md)
