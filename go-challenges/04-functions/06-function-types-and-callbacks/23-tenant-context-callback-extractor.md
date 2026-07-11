# Exercise 23: Multi-Tenant Context Extraction via Callback Functions

**Nivel: Intermedio** — validacion rapida (un test corto).

A multi-tenant API resolves which tenant a request belongs to from more
than one place — an explicit `X-Tenant-ID` header for API clients, a
subdomain like `acme.example.com` for browser traffic — and every handler
downstream just wants the tenant ID already sitting in `context.Context`.
This module builds that as composable `Extractor` callbacks plus a
`Middleware` that resolves one and stores it in context before the real
handler runs.

## What you'll build

```text
tenantctx/                  independent module: example.com/tenant-context-callback-extractor
  go.mod                     go 1.24
  tenantctx.go                  type Extractor, ErrNotFound, FromHeader, FromSubdomain, func FirstMatch, WithTenant, TenantFromContext, func Middleware
  cmd/
    demo/
      main.go                  runnable demo: header wins over subdomain, subdomain fallback, then a rejected request
  tenantctx_test.go            table test: each extractor hit/miss, FirstMatch precedence and fallback, context round trip, middleware accept/reject
```

Files: `tenantctx.go`, `cmd/demo/main.go`, `tenantctx_test.go`.
Implement: `type Extractor func(r *http.Request) (string, error)`, a sentinel `ErrNotFound`, `FromHeader(name string) Extractor`, `FromSubdomain() Extractor`, `func FirstMatch(extractors ...Extractor) Extractor`, an unexported context key with `WithTenant`/`TenantFromContext`, and `func Middleware(extract Extractor, next http.Handler) http.Handler`.
Test: `FromHeader` hit/miss, `FromSubdomain` on a real subdomain, on a bare domain (miss), and with a port suffix, `FirstMatch` preferring the header over the subdomain when both would succeed and falling back to the subdomain when the header is absent, a context round trip through `WithTenant`/`TenantFromContext`, and the middleware both accepting a resolvable request and rejecting one with 400 when no extractor matches.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tenant-context-callback-extractor/cmd/demo
cd ~/go-exercises/tenant-context-callback-extractor
go mod init example.com/tenant-context-callback-extractor
go mod edit -go=1.24
```

### Why extraction is a chain of callbacks, and the context key is unexported

Different traffic sources carry the tenant identity in different places,
and a single handler shouldn't have to know all of them — nor should it
matter to `FirstMatch` which specific strategy actually succeeded, only
that one did. `Extractor func(r *http.Request) (string, error)` gives every
strategy the same shape, and `FirstMatch` is nothing more than "try these
in order, return the first one that didn't error" — the exact fallback
pattern used everywhere from DNS resolution to config loading. The context
key is a private `struct{}` type rather than a `string` like `"tenant"`
specifically so no other package can accidentally (or maliciously) read or
overwrite the tenant by using the same string key in its own
`context.WithValue` call — this is the standard library's own documented
pattern for context keys, and it is the difference between "only
`tenantctx.TenantFromContext` can read this" and "any package that guesses
the string wins."

Create `tenantctx.go`:

```go
// Package tenantctx extracts a tenant identifier from an inbound HTTP
// request via composable Extractor callbacks, and carries it through
// context.Context for the rest of the request's handling.
package tenantctx

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Extractor pulls a tenant ID out of a request, or reports that it found
// none.
type Extractor func(r *http.Request) (string, error)

// ErrNotFound is returned by an Extractor (or FirstMatch) that has no
// tenant to report for the request.
var ErrNotFound = fmt.Errorf("tenantctx: no tenant found")

// FromHeader builds an Extractor that reads the tenant ID from a fixed
// request header, e.g. "X-Tenant-ID".
func FromHeader(name string) Extractor {
	return func(r *http.Request) (string, error) {
		v := r.Header.Get(name)
		if v == "" {
			return "", fmt.Errorf("%w: header %q not set", ErrNotFound, name)
		}
		return v, nil
	}
}

// FromSubdomain builds an Extractor that reads the tenant ID from the
// first label of the request's Host header, e.g. "acme" from
// "acme.example.com".
func FromSubdomain() Extractor {
	return func(r *http.Request) (string, error) {
		host := r.Host
		if i := strings.IndexByte(host, ':'); i != -1 {
			host = host[:i] // strip a port, if present
		}
		labels := strings.Split(host, ".")
		if len(labels) < 3 || labels[0] == "" {
			return "", fmt.Errorf("%w: host %q has no tenant subdomain", ErrNotFound, r.Host)
		}
		return labels[0], nil
	}
}

// FirstMatch tries each extractor in order and returns the first
// successful tenant ID. If none succeed, it returns ErrNotFound.
func FirstMatch(extractors ...Extractor) Extractor {
	return func(r *http.Request) (string, error) {
		for _, extract := range extractors {
			if tenant, err := extract(r); err == nil {
				return tenant, nil
			}
		}
		return "", ErrNotFound
	}
}

// contextKey is an unexported type so tenantctx's context key can never
// collide with a key set by another package.
type contextKey struct{}

// WithTenant returns a copy of ctx carrying the given tenant ID.
func WithTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, contextKey{}, tenant)
}

// TenantFromContext retrieves the tenant ID stored by WithTenant, if any.
func TenantFromContext(ctx context.Context) (string, bool) {
	tenant, ok := ctx.Value(contextKey{}).(string)
	return tenant, ok
}

// Middleware extracts a tenant ID from each request using extract and
// stores it in the request's context before calling next. A request with
// no resolvable tenant is rejected with 400 Bad Request.
func Middleware(extract Extractor, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, err := extract(r)
		if err != nil {
			http.Error(w, "tenant not found", http.StatusBadRequest)
			return
		}
		ctx := WithTenant(r.Context(), tenant)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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

	"example.com/tenant-context-callback-extractor"
)

func main() {
	extract := tenantctx.FirstMatch(
		tenantctx.FromHeader("X-Tenant-ID"),
		tenantctx.FromSubdomain(),
	)

	handler := tenantctx.Middleware(extract, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, _ := tenantctx.TenantFromContext(r.Context())
		fmt.Fprintf(w, "tenant=%s", tenant)
	}))

	requests := []*http.Request{
		mustRequest("GET", "http://any-host.example.com/", map[string]string{"X-Tenant-ID": "acme"}),
		mustRequest("GET", "http://globex.example.com/", nil),
		mustRequest("GET", "http://example.com/", nil),
	}

	for _, req := range requests {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		fmt.Printf("host=%s -> status=%d body=%q\n", req.Host, rec.Code, rec.Body.String())
	}
}

func mustRequest(method, url string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(method, url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
host=any-host.example.com -> status=200 body="tenant=acme"
host=globex.example.com -> status=200 body="tenant=globex"
host=example.com -> status=400 body="tenant not found\n"
```

### Tests

Create `tenantctx_test.go`:

```go
package tenantctx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFromHeaderFindsTenant(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	req.Header.Set("X-Tenant-ID", "acme")

	tenant, err := FromHeader("X-Tenant-ID")(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tenant != "acme" {
		t.Fatalf("tenant = %q, want acme", tenant)
	}
}

func TestFromHeaderMissingReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	_, err := FromHeader("X-Tenant-ID")(req)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want wrapping ErrNotFound", err)
	}
}

func TestFromSubdomainExtractsFirstLabel(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "http://acme.example.com/", nil)
	tenant, err := FromSubdomain()(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tenant != "acme" {
		t.Fatalf("tenant = %q, want acme", tenant)
	}
}

func TestFromSubdomainRejectsBareDomain(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	_, err := FromSubdomain()(req)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want wrapping ErrNotFound", err)
	}
}

func TestFromSubdomainStripsPort(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "http://acme.example.com:8080/", nil)
	tenant, err := FromSubdomain()(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tenant != "acme" {
		t.Fatalf("tenant = %q, want acme", tenant)
	}
}

func TestFirstMatchPrefersEarlierExtractor(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "http://globex.example.com/", nil)
	req.Header.Set("X-Tenant-ID", "acme")

	extract := FirstMatch(FromHeader("X-Tenant-ID"), FromSubdomain())
	tenant, err := extract(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tenant != "acme" {
		t.Fatalf("tenant = %q, want acme (header should win over subdomain)", tenant)
	}
}

func TestFirstMatchFallsBackToLaterExtractor(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "http://globex.example.com/", nil)

	extract := FirstMatch(FromHeader("X-Tenant-ID"), FromSubdomain())
	tenant, err := extract(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tenant != "globex" {
		t.Fatalf("tenant = %q, want globex", tenant)
	}
}

func TestFirstMatchReturnsErrNotFoundWhenNoneMatch(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	extract := FirstMatch(FromHeader("X-Tenant-ID"), FromSubdomain())
	_, err := extract(req)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want wrapping ErrNotFound", err)
	}
}

func TestWithTenantAndTenantFromContext(t *testing.T) {
	t.Parallel()
	ctx := WithTenant(t.Context(), "acme")
	tenant, ok := TenantFromContext(ctx)
	if !ok || tenant != "acme" {
		t.Fatalf("TenantFromContext = (%q, %v), want (acme, true)", tenant, ok)
	}
}

func TestTenantFromContextMissing(t *testing.T) {
	t.Parallel()
	_, ok := TenantFromContext(t.Context())
	if ok {
		t.Fatal("expected ok=false when no tenant was stored")
	}
}

func TestMiddlewareStoresTenantForHandler(t *testing.T) {
	t.Parallel()
	var gotTenant string
	handler := Middleware(FromHeader("X-Tenant-ID"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant, _ = TenantFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "http://example.com/", nil)
	req.Header.Set("X-Tenant-ID", "acme")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotTenant != "acme" {
		t.Fatalf("handler saw tenant %q, want acme", gotTenant)
	}
}

func TestMiddlewareRejectsRequestWithNoTenant(t *testing.T) {
	t.Parallel()
	handler := Middleware(FromHeader("X-Tenant-ID"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "http://example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
```

## Review

`FirstMatch` never inspects which extractor produced a tenant, only whether
it returned a nil error — which is exactly why `TestFirstMatchPrefers
EarlierExtractor` matters: it proves precedence is a property of argument
order, not of some strategy being "smarter." `Middleware` is the piece that
turns extraction into an actual guarantee for every handler behind it: a
handler that calls `TenantFromContext` never has to check `ok` defensively
for a well-formed request, because `Middleware` already rejected anything it
couldn't resolve before `next` ever ran. The unexported `contextKey{}` is
what makes that guarantee airtight — no other package's `context.WithValue`
call, however similarly named its own key might be, can shadow or be
mistaken for this one.

## Resources

- [context package: WithValue and key type guidance](https://pkg.go.dev/context#WithValue)
- [net/http: Request.Host](https://pkg.go.dev/net/http#Request)
- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-sql-query-filter-builder-callback.md](22-sql-query-filter-builder-callback.md) | Next: [24-auth-validator-callback-chain.md](24-auth-validator-callback-chain.md)
