# Exercise 5: Dispatch by HTTP Method and Emit a Correct 405

Even with Go 1.22's method-aware `ServeMux` patterns, seniors still hand-roll
per-method dispatch for a single resource when they need control over the 405
response — most importantly, the `Allow` header that a correct
`405 Method Not Allowed` must carry. This module builds that resource handler
with an expression switch on `r.Method` and a default that emits a spec-compliant
405.

This module is fully self-contained: its own `go mod init`, code, demo, and
tests.

## What you'll build

```text
resource/                  independent module: example.com/method-router-405
  go.mod                   go 1.24
  resource.go              type Resource; ServeHTTP with method switch + 405 default
  cmd/
    demo/
      main.go              runnable demo hitting supported and unsupported methods
  resource_test.go         httptest table: each verb routes; PATCH gets 405 + Allow
```

- Files: `resource.go`, `cmd/demo/main.go`, `resource_test.go`.
- Implement: a `Resource` with per-method `http.HandlerFunc`s and a `ServeHTTP` that switches on `r.Method`, with a default that sets `Allow` and writes `StatusMethodNotAllowed`.
- Test: an httptest table where each supported method reaches its handler with a 2xx and an unsupported method (PATCH) gets 405 with an `Allow` header listing exactly the supported verbs (order-insensitive).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/resource/cmd/demo
cd ~/go-exercises/resource
go mod init example.com/method-router-405
go mod edit -go=1.24
```

### The 405 that most hand-rolled routers get wrong

RFC 9110 requires a `405 Method Not Allowed` response to include an `Allow`
header listing the methods the resource *does* support. This is the part
hand-rolled routers routinely omit: they write `http.StatusMethodNotAllowed` and
stop, leaving a client (or a caching proxy) with no way to learn which verbs are
valid. The `default` case here does both jobs — sets `Allow` and writes the
status — which is exactly the concepts-file point that a default on a closed
dispatcher is a real control, not a formality.

The switch resolves the matching `http.HandlerFunc` for the method. A method that
either is not in the switch (`PATCH`) or is in it but has no registered handler
(a nil field) both funnel to the same `methodNotAllowed` helper, so a resource
that only implements GET and POST correctly rejects PUT with a 405 whose `Allow`
lists just `GET, POST`. The `Allow` value is built from the non-nil handlers, so
it always reflects what the resource can actually do.

Using the method constants (`http.MethodGet`, ...) rather than raw string
literals is the small correctness habit: it is compiler-checked and immune to a
`"GET"` typo that a string case would silently never match.

Create `resource.go`:

```go
package resource

import (
	"net/http"
	"strings"
)

// Resource is a single REST resource with an optional handler per method. A nil
// handler means the method is not supported.
type Resource struct {
	Get    http.HandlerFunc
	Post   http.HandlerFunc
	Put    http.HandlerFunc
	Delete http.HandlerFunc
}

// ServeHTTP dispatches on the request method with an expression switch. The
// default emits a spec-compliant 405 with an Allow header; a supported method
// whose handler is nil funnels to the same 405.
func (rs Resource) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rs.call(rs.Get, w, r)
	case http.MethodPost:
		rs.call(rs.Post, w, r)
	case http.MethodPut:
		rs.call(rs.Put, w, r)
	case http.MethodDelete:
		rs.call(rs.Delete, w, r)
	default:
		rs.methodNotAllowed(w)
	}
}

func (rs Resource) call(h http.HandlerFunc, w http.ResponseWriter, r *http.Request) {
	if h == nil {
		rs.methodNotAllowed(w)
		return
	}
	h(w, r)
}

func (rs Resource) methodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", strings.Join(rs.allowed(), ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// allowed lists the methods this resource actually implements.
func (rs Resource) allowed() []string {
	var methods []string
	if rs.Get != nil {
		methods = append(methods, http.MethodGet)
	}
	if rs.Post != nil {
		methods = append(methods, http.MethodPost)
	}
	if rs.Put != nil {
		methods = append(methods, http.MethodPut)
	}
	if rs.Delete != nil {
		methods = append(methods, http.MethodDelete)
	}
	return methods
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/method-router-405"
)

func main() {
	rs := resource.Resource{
		Get:  func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "read") },
		Post: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) },
	}

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch} {
		req := httptest.NewRequest(method, "/widgets/1", nil)
		rec := httptest.NewRecorder()
		rs.ServeHTTP(rec, req)
		fmt.Printf("%-6s -> %d Allow=%q\n", method, rec.Code, rec.Header().Get("Allow"))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET    -> 200 Allow=""
POST   -> 201 Allow=""
PATCH  -> 405 Allow="GET, POST"
```

### Tests

`TestSupportedMethods` drives each implemented verb and asserts its handler ran
with a 2xx. `TestMethodNotAllowed` sends `PATCH` and asserts a 405 whose `Allow`
header lists exactly the supported verbs, compared as a set so the assertion does
not depend on header ordering.

Create `resource_test.go`:

```go
package resource

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func newResource(hit *string) Resource {
	mk := func(name string, code int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			*hit = name
			w.WriteHeader(code)
		}
	}
	return Resource{
		Get:    mk("get", http.StatusOK),
		Post:   mk("post", http.StatusCreated),
		Put:    mk("put", http.StatusOK),
		Delete: mk("delete", http.StatusNoContent),
	}
}

func TestSupportedMethods(t *testing.T) {
	t.Parallel()

	tests := []struct {
		method   string
		wantHit  string
		wantCode int
	}{
		{http.MethodGet, "get", http.StatusOK},
		{http.MethodPost, "post", http.StatusCreated},
		{http.MethodPut, "put", http.StatusOK},
		{http.MethodDelete, "delete", http.StatusNoContent},
	}

	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			t.Parallel()

			var hit string
			rs := newResource(&hit)
			req := httptest.NewRequest(tc.method, "/r/1", nil)
			rec := httptest.NewRecorder()
			rs.ServeHTTP(rec, req)

			if hit != tc.wantHit {
				t.Fatalf("%s reached handler %q, want %q", tc.method, hit, tc.wantHit)
			}
			if rec.Code != tc.wantCode {
				t.Fatalf("%s status = %d, want %d", tc.method, rec.Code, tc.wantCode)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()

	var hit string
	rs := newResource(&hit)
	req := httptest.NewRequest(http.MethodPatch, "/r/1", nil)
	rec := httptest.NewRecorder()
	rs.ServeHTTP(rec, req)

	if hit != "" {
		t.Fatalf("PATCH reached handler %q, want no handler", hit)
	}
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}

	got := parseAllow(rec.Header().Get("Allow"))
	want := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Allow = %v, want set %v", got, want)
	}
}

func parseAllow(header string) []string {
	var out []string
	for _, part := range strings.Split(header, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
```

## Review

The router is correct when every supported verb reaches its handler and every
unsupported one gets a 405 that a client can act on. The `Allow` header is the
detail that separates a compliant 405 from a lazy one, and `TestMethodNotAllowed`
asserts it as a set so the test does not couple to header ordering. Building
`Allow` from the non-nil handlers keeps it honest as the resource's supported set
changes, and routing a nil-handler method through the same `methodNotAllowed`
path means a partially-implemented resource still rejects the right verbs.

## Resources

- [net/http method constants](https://pkg.go.dev/net/http#pkg-constants) — `MethodGet`, `MethodPost`, and the rest.
- [RFC 9110: 405 Method Not Allowed](https://www.rfc-editor.org/rfc/rfc9110#name-405-method-not-allowed) — the requirement to send an `Allow` header.
- [http.Error](https://pkg.go.dev/net/http#Error) — writing a status with a plain-text body.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-log-level-config-loader.md](04-log-level-config-loader.md) | Next: [06-order-state-machine.md](06-order-state-machine.md)
