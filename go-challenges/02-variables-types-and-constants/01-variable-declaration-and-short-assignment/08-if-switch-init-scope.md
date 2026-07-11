# Exercise 8: Init-Statement Scope in an HTTP Router and Middleware

Init statements are how a request handler confines a parsed id or a derived role to
exactly the branch that consumes it: `if id, err := parseID(r); err != nil { ... }`
and `switch role := auth(r); role { ... }`. The value dies with the branch, so it
cannot be reused stale later — a discipline that prevents a whole class of routing
bugs.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests using `net/http/httptest`. Nothing
here imports any other exercise.

## What you'll build

```text
dispatch/                      independent module: example.com/dispatch
  go.mod                       module example.com/dispatch
  dispatch.go                  Handler using if-init and switch-init scoping
  cmd/
    demo/
      main.go                  drives the handler with httptest and prints results
  dispatch_test.go             valid id -> 200, bad id -> 400, role routing
```

- Files: `dispatch.go`, `cmd/demo/main.go`, `dispatch_test.go`.
- Implement: an `http.Handler` that parses an id in an `if` init clause and switches on a role in a `switch` init clause, keeping each derived value scoped to its branch.
- Test: `httptest`-driven — valid id returns 200, malformed id returns 400, role init routes admin vs user; assert via the response recorder, not internal state.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dispatch/cmd/demo
cd ~/go-exercises/dispatch
go mod init example.com/dispatch
```

### Why the init clause is the right scope

A parsed id is meaningful only inside the branch that validated it. Declaring it in
the `if` init clause — `if id, err := parseID(r); err != nil { ... }` — makes `id`
and `err` live only across that `if`/`else`. They cannot leak into a later branch
where a *different* request field is in play, so nobody can accidentally route on a
stale id. The contrast is a wide-scoped `var id int` at the top of the handler: it
is visible everywhere, invites reuse, and lets a bug where branch B reads branch A's
id compile silently. Scope is the guard rail.

The same reasoning drives `switch role := auth(r); role { ... }`. The derived role
exists only for the duration of the dispatch decision; it is not a handler-wide
variable that some later code might read after it has ceased to be accurate.

### The handler

`ServeHTTP` parses the id from the query in an `if` init clause; on failure it
writes 400 and returns, so the invalid id never escapes. On success it authorizes
the request and switches on the role in a `switch` init clause, dispatching to the
admin or user path. Each derived value is born and dies inside its statement.

Create `dispatch.go`:

```go
package dispatch

import (
	"fmt"
	"net/http"
	"strconv"
)

// Router dispatches requests, deriving a resource id and a caller role in tightly
// scoped init statements so neither value can leak into an unrelated branch.
type Router struct{}

func New() *Router { return &Router{} }

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// id and err live only inside this if/else; a bad id never escapes.
	if id, err := strconv.Atoi(r.URL.Query().Get("id")); err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	} else if id <= 0 {
		http.Error(w, "id must be positive", http.StatusBadRequest)
		return
	}

	// role is scoped to the switch; it is the dispatch key and nothing more.
	switch role := roleOf(r); role {
	case "admin":
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "admin dashboard")
	case "user":
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "user home")
	default:
		http.Error(w, "forbidden", http.StatusForbidden)
	}
}

// roleOf derives a role from a header; a real service would validate a token.
func roleOf(r *http.Request) string {
	switch r.Header.Get("X-Role") {
	case "admin":
		return "admin"
	case "user":
		return "user"
	default:
		return "anonymous"
	}
}
```

### The runnable demo

The demo drives the handler with `httptest.NewRecorder` and a synthetic request,
which is how you exercise an `http.Handler` without binding a socket.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/dispatch"
)

func call(rt http.Handler, target, role string) string {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if role != "" {
		req.Header.Set("X-Role", role)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	return fmt.Sprintf("status=%d", rec.Code)
}

func main() {
	rt := dispatch.New()
	fmt.Println("admin ok:  ", call(rt, "/?id=42", "admin"))
	fmt.Println("user ok:   ", call(rt, "/?id=42", "user"))
	fmt.Println("bad id:    ", call(rt, "/?id=oops", "admin"))
	fmt.Println("anon role: ", call(rt, "/?id=42", ""))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
admin ok:   status=200
user ok:    status=200
bad id:     status=400
anon role:  status=403
```

A valid id with an admin or user role returns 200; a malformed id returns 400
before the role switch is even reached; an unknown role is 403.

### Tests

Create `dispatch_test.go`:

```go
package dispatch

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDispatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		target     string
		role       string
		wantStatus int
		wantBody   string
	}{
		{"admin routes to dashboard", "/?id=42", "admin", http.StatusOK, "admin dashboard"},
		{"user routes to home", "/?id=7", "user", http.StatusOK, "user home"},
		{"malformed id is 400", "/?id=oops", "admin", http.StatusBadRequest, ""},
		{"nonpositive id is 400", "/?id=0", "admin", http.StatusBadRequest, ""},
		{"missing id is 400", "/", "admin", http.StatusBadRequest, ""},
		{"unknown role is 403", "/?id=42", "", http.StatusForbidden, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if tc.role != "" {
				req.Header.Set("X-Role", tc.role)
			}
			rec := httptest.NewRecorder()
			New().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body = %q, want it to contain %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

func ExampleRouter() {
	req := httptest.NewRequest(http.MethodGet, "/?id=1", nil)
	req.Header.Set("X-Role", "user")
	rec := httptest.NewRecorder()
	New().ServeHTTP(rec, req)
	fmt.Println(rec.Code)
	// Output: 200
}
```

The tests assert only through
the response recorder — status and body — never any internal handler state, because
the scoped values deliberately do not escape to be inspected.

## Review

The handler is correct when every derived value is confined to the statement that
uses it: the id lives in the `if` init clause and never leaks past a 400, and the
role lives in the `switch` init clause as the dispatch key. That scoping is what
prevents a later branch from reusing a stale id or a role that no longer applies.

The mistakes to avoid: hoisting `id` or `role` to a wide `var` at the top of the
handler (inviting stale reuse), and asserting on internal state rather than the HTTP
response. Run `go test -race`; the handler holds no shared mutable state, so it is
safe under concurrent requests by construction.

## Resources

- [Go Specification: If statements (init statement)](https://go.dev/ref/spec#If_statements)
- [Go Specification: Switch statements (init statement)](https://go.dev/ref/spec#Switch_statements)
- [net/http/httptest](https://pkg.go.dev/net/http/httptest)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-multiple-assignment-address-parsing.md](07-multiple-assignment-address-parsing.md) | Next: [09-named-returns-deferred-cleanup.md](09-named-returns-deferred-cleanup.md)
