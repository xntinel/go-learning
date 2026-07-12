# Exercise 6: Tenant-Scoped Repository Access

Multi-tenancy is where the boundary rule gets genuinely subtle. The tenant ID is
read by every query, so threading it through every repository method as a parameter
is noisy — yet it is also load-bearing for correctness, so it cannot be a
free-for-all. This exercise resolves the tension: middleware sets the tenant in the
context once at the edge, and the repository reads it to scope every query, failing
loudly with a sentinel error when it is absent rather than leaking cross-tenant
data.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
tenantrepo/                  independent module: example.com/tenantrepo
  go.mod
  tenant.go                  WithTenant, TenantFromContext, ErrNoTenant; Resolve middleware
  repo.go                    in-memory Repo; List(ctx) scopes rows by context tenant
  cmd/
    demo/
      main.go                lists rows for two tenants; shows ErrNoTenant
  tenantrepo_test.go         acme sees only acme rows; missing tenant -> ErrNoTenant; middleware
```

Files: `tenant.go`, `repo.go`, `cmd/demo/main.go`, `tenantrepo_test.go`.
Implement: `WithTenant`/`TenantFromContext`, `ErrNoTenant`, a `Resolve` middleware mapping a header/host to the tenant, and `Repo.List(ctx)` that scopes rows by the context tenant.
Test: a context with tenant `acme` returns only acme rows; a context missing the tenant returns `ErrNoTenant` (via `errors.Is`) and no rows; the middleware maps a header to the tenant.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/06-context-withvalue/06-tenant-scoped-repository/cmd/demo
cd go-solutions/14-select-and-context/06-context-withvalue/06-tenant-scoped-repository
```

### Why tenant-in-context is defensible — but still a guard

Apply the boundary rule honestly. The tenant *does* affect correctness — a query run
for the wrong tenant returns the wrong rows — which by the strict reading argues it
should be a parameter. What tips it into legitimate context data is that it is a
cross-cutting fact of the *entire request*, established once at the edge from the
authenticated session (subdomain, header, or token claim), and consulted by *every*
data-access call uniformly. Threading `tenantID string` through every repository
method, service method, and helper is the kind of parameter pollution the context
exists to prevent, and it invites a subtle bug where one call site passes the wrong
tenant. Setting it once and reading it uniformly is safer.

The crucial discipline that keeps this from becoming the `ctx.Value("productID")`
anti-pattern is the failure mode. Because the tenant is load-bearing for
correctness, its absence must be a hard error, never a silent "return everything".
`Repo.List` reads `TenantFromContext` and, on absence, returns `ErrNoTenant` wrapped
with `%w` — so a caller that forgot to run the middleware gets a loud, testable
failure instead of a cross-tenant data leak. That is the difference between a
scoping guard and a hidden optional parameter: a guard fails closed. `errors.Is`
against the sentinel is how callers and tests detect it.

`Resolve` is the edge middleware that maps the transport-level tenant signal — here
an `X-Tenant-ID` header — into the context, so nothing below the edge has to know
where the tenant came from.

Create `tenant.go`:

```go
package tenantrepo

import (
	"context"
	"errors"
	"net/http"
)

// ErrNoTenant is returned by data-access calls when the context carries no
// tenant. Failing closed prevents cross-tenant data leaks.
var ErrNoTenant = errors.New("tenantrepo: no tenant in context")

type ctxKey struct{}

// WithTenant attaches a tenant ID to ctx.
func WithTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, ctxKey{}, tenant)
}

// TenantFromContext returns the tenant ID, or "" and false if absent.
func TenantFromContext(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(ctxKey{}).(string)
	return t, ok
}

// Resolve maps the X-Tenant-ID header into the request context.
func Resolve(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tenant := r.Header.Get("X-Tenant-ID"); tenant != "" {
			r = r.WithContext(WithTenant(r.Context(), tenant))
		}
		next.ServeHTTP(w, r)
	})
}
```

Create `repo.go`:

```go
package tenantrepo

import (
	"context"
	"fmt"
)

// Row is a tenant-owned record.
type Row struct {
	Tenant string
	ID     string
	Name   string
}

// Repo is an in-memory store standing in for a database.
type Repo struct {
	rows []Row
}

// NewRepo builds a repo over the given rows.
func NewRepo(rows []Row) *Repo {
	return &Repo{rows: rows}
}

// List returns only the rows owned by the context's tenant. With no tenant it
// returns ErrNoTenant (wrapped) and never leaks rows.
func (r *Repo) List(ctx context.Context) ([]Row, error) {
	tenant, ok := TenantFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("list: %w", ErrNoTenant)
	}
	var out []Row
	for _, row := range r.rows {
		if row.Tenant == tenant {
			out = append(out, row)
		}
	}
	return out, nil
}
```

### The demo

The demo lists rows for two tenants over the same store, showing each sees only its
own rows, then calls `List` with a bare context to show `ErrNoTenant`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/tenantrepo"
)

func main() {
	repo := tenantrepo.NewRepo([]tenantrepo.Row{
		{Tenant: "acme", ID: "1", Name: "acme-order"},
		{Tenant: "globex", ID: "2", Name: "globex-order"},
		{Tenant: "acme", ID: "3", Name: "acme-invoice"},
	})

	for _, tenant := range []string{"acme", "globex"} {
		ctx := tenantrepo.WithTenant(context.Background(), tenant)
		rows, _ := repo.List(ctx)
		fmt.Printf("%s: %d rows\n", tenant, len(rows))
	}

	if _, err := repo.List(context.Background()); errors.Is(err, tenantrepo.ErrNoTenant) {
		fmt.Println("no-tenant call: ErrNoTenant")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the store holds two acme rows and one globex row, and each
tenant sees only its own):

```
acme: 2 rows
globex: 1 rows
no-tenant call: ErrNoTenant
```

### The tests

`TestListScopesToTenant` proves acme sees exactly its two rows and none of globex's.
`TestListNoTenantReturnsSentinel` proves the fail-closed contract with `errors.Is`
and asserts no rows leak. `TestResolveMiddlewareMapsHeader` proves the edge maps the
header into the context that the handler (and thus the repo) reads.

Create `tenantrepo_test.go`:

```go
package tenantrepo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func fixtureRepo() *Repo {
	return NewRepo([]Row{
		{Tenant: "acme", ID: "1", Name: "a1"},
		{Tenant: "globex", ID: "2", Name: "g1"},
		{Tenant: "acme", ID: "3", Name: "a2"},
	})
}

func TestListScopesToTenant(t *testing.T) {
	t.Parallel()

	repo := fixtureRepo()
	ctx := WithTenant(context.Background(), "acme")

	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, row := range rows {
		if row.Tenant != "acme" {
			t.Fatalf("leaked row from tenant %q", row.Tenant)
		}
	}
}

func TestListNoTenantReturnsSentinel(t *testing.T) {
	t.Parallel()

	repo := fixtureRepo()

	rows, err := repo.List(context.Background())
	if !errors.Is(err, ErrNoTenant) {
		t.Fatalf("err = %v, want ErrNoTenant", err)
	}
	if rows != nil {
		t.Fatalf("rows = %v, want nil (no leak)", rows)
	}
}

func TestResolveMiddlewareMapsHeader(t *testing.T) {
	t.Parallel()

	var seen string
	var ok bool
	handler := Resolve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, ok = TenantFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Tenant-ID", "globex")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !ok || seen != "globex" {
		t.Fatalf("handler saw tenant %q,%v; want globex,true", seen, ok)
	}
}
```

## Review

The repository is correct when a tenant context returns exactly that tenant's rows
and a tenant-less context returns `ErrNoTenant` with zero rows — the fail-closed
behavior is the whole point, because the alternative (returning all rows on a
missing tenant) is a cross-tenant data breach. The `%w` wrapping lets callers match
the sentinel with `errors.Is` while still adding operation context to the message.
The judgment worth internalizing: tenant-in-context is defensible precisely because
it is a request-wide, set-once, uniformly-read fact whose absence is a hard error —
not because "context is convenient". A value you fish out of the context and
silently default on absence is the `productID` anti-pattern in disguise; a value you
fish out and *fail closed* on is a scoping guard.

## Resources

- [errors.Is / fmt.Errorf %w](https://pkg.go.dev/errors#Is) — sentinel-error matching through wrapping.
- [Go Blog: Contexts and structs](https://go.dev/blog/context-and-structs) — the guidance on request-scoped versus dependency data.
- [context.Context](https://pkg.go.dev/context#Context) — the request-scoped-data contract.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-trace-propagation-across-boundary.md](05-trace-propagation-across-boundary.md) | Next: [07-idempotency-key-guard.md](07-idempotency-key-guard.md)
