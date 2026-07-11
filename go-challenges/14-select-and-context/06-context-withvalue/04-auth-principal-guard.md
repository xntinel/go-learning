# Exercise 4: Authenticated Principal and Role Guard

The authenticated identity of a request is the archetypal request-scoped value: set
once after token validation at the edge, read by every layer that needs to know who
is acting. This exercise builds an auth layer — a `Principal` stored in context by
an authentication middleware, a `PrincipalFromContext` accessor, and a
`RequireRole` middleware that returns 401 when there is no principal and 403 when
the principal lacks the required role.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
authguard/                   independent module: example.com/authguard
  go.mod
  authguard.go               Principal; WithPrincipal, PrincipalFromContext;
                             Authenticate, RequireRole middleware
  cmd/
    demo/
      main.go                drives 401 / 403 / 200 through the chain
  authguard_test.go          no principal -> 401; wrong role -> 403; ok -> 200; forged key
```

Files: `authguard.go`, `cmd/demo/main.go`, `authguard_test.go`.
Implement: `Principal{Subject, Roles}`, `WithPrincipal`/`PrincipalFromContext`, an `Authenticate` middleware that validates a token and stores the principal, and `RequireRole(role, next)`.
Test: no principal -> 401 and inner handler not called; principal without the role -> 403; principal with the role -> 200 and handler sees the correct `Subject`; a forged string-key principal is not read.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/authguard/cmd/demo
cd ~/go-exercises/authguard
go mod init example.com/authguard
```

### Why identity is context data and how the guard short-circuits

The principal passes the boundary rule cleanly. Deleting it does not change what a
handler *computes* — it changes whether the handler is *allowed to run at all* — and
it is a cross-cutting fact every layer may consult (for authz, for audit logs, for
ownership checks). It is set exactly once, by `Authenticate`, after the token is
validated, and read everywhere below via `PrincipalFromContext`. The key is
unexported so no other package — and no forged `string` key in a request — can
inject or read a principal.

`Authenticate` takes a `validate` function rather than hard-coding a token format,
which is the real seam: production wires a JWT or session validator, tests wire a
map. On a valid token it stores the principal and calls `next`; on an invalid or
missing token it writes 401 and stops. `RequireRole` reads the principal: absent ->
401 (the request was never authenticated), present but missing the role -> 403
(authenticated but not authorized), present with the role -> call `next`. The
distinction between 401 and 403 is the HTTP-correct encoding of "who are you?"
versus "you may not do this", and the tests pin both.

Short-circuiting is the security-critical property: when the guard rejects a
request it must *not* call the protected handler. The tests capture a bool the inner
handler flips and assert it stays false on the 401 and 403 paths — proving the side
effect never happened. `slices.Contains` does the role check.

Create `authguard.go`:

```go
package authguard

import (
	"context"
	"net/http"
	"slices"
)

// Principal is the authenticated identity of a request. It is an immutable
// value snapshot set once at the edge.
type Principal struct {
	Subject string
	Roles   []string
}

type ctxKey struct{}

// WithPrincipal attaches p to ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFromContext returns the authenticated principal; ok is false when the
// request was never authenticated.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Authenticate validates the bearer token with validate and, on success, stores
// the resulting principal in the request context. On failure it writes 401 and
// does not call next.
func Authenticate(validate func(token string) (Principal, bool), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		p, ok := validate(token)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// RequireRole allows the request through only if the context carries a principal
// holding role. No principal -> 401; principal without role -> 403.
func RequireRole(role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFromContext(r.Context())
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !slices.Contains(p.Roles, role) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

### The demo

The demo builds the full chain `Authenticate -> RequireRole("admin") -> handler`
and drives three requests: a bad token, a valid non-admin, and a valid admin,
printing the status each produces.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/authguard"
)

func main() {
	validate := func(token string) (authguard.Principal, bool) {
		switch token {
		case "Bearer alice":
			return authguard.Principal{Subject: "alice", Roles: []string{"admin"}}, true
		case "Bearer bob":
			return authguard.Principal{Subject: "bob", Roles: []string{"viewer"}}, true
		default:
			return authguard.Principal{}, false
		}
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := authguard.PrincipalFromContext(r.Context())
		fmt.Fprintf(w, "welcome %s", p.Subject)
	})

	chain := authguard.Authenticate(validate, authguard.RequireRole("admin", handler))

	for _, token := range []string{"", "Bearer bob", "Bearer alice"} {
		req := httptest.NewRequest(http.MethodGet, "/admin", nil)
		if token != "" {
			req.Header.Set("Authorization", token)
		}
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		fmt.Printf("token=%q status=%d\n", token, rec.Code)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
token="" status=401
token="Bearer bob" status=403
token="Bearer alice" status=200
```

### The tests

The tests drive `httptest` requests through the chain and assert both the status
and — via a captured `called` bool — that the protected handler ran only on the
allowed path. `TestForgedStringKeyIsIgnored` proves the unexported key defends the
slot: a request whose context was seeded with a `string`-keyed "principal" is still
treated as unauthenticated.

Create `authguard_test.go`:

```go
package authguard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func validateFixed(p Principal, ok bool) func(string) (Principal, bool) {
	return func(string) (Principal, bool) { return p, ok }
}

func TestNoPrincipalReturns401AndShortCircuits(t *testing.T) {
	t.Parallel()

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

	rec := httptest.NewRecorder()
	RequireRole("admin", inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Fatal("inner handler ran on the 401 path")
	}
}

func TestWrongRoleReturns403AndShortCircuits(t *testing.T) {
	t.Parallel()

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

	validate := validateFixed(Principal{Subject: "bob", Roles: []string{"viewer"}}, true)
	chain := Authenticate(validate, RequireRole("admin", inner))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer bob")
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if called {
		t.Fatal("inner handler ran on the 403 path")
	}
}

func TestCorrectRoleReturns200AndSeesSubject(t *testing.T) {
	t.Parallel()

	var seen string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := PrincipalFromContext(r.Context())
		seen = p.Subject
		w.WriteHeader(http.StatusOK)
	})

	validate := validateFixed(Principal{Subject: "alice", Roles: []string{"admin"}}, true)
	chain := Authenticate(validate, RequireRole("admin", inner))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer alice")
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen != "alice" {
		t.Fatalf("handler saw subject %q, want alice", seen)
	}
}

func TestForgedStringKeyIsIgnored(t *testing.T) {
	t.Parallel()

	//nolint:staticcheck // intentionally using a string key to prove it cannot forge a principal
	ctx := context.WithValue(context.Background(), "principal", Principal{Subject: "attacker", Roles: []string{"admin"}})

	if p, ok := PrincipalFromContext(ctx); ok {
		t.Fatalf("forged string-key principal was read: %+v", p)
	}
}
```

## Review

The guard is correct when the three status codes map exactly to the three states —
unauthenticated (401), authenticated-but-unauthorized (403), authorized (200) — and
when the protected handler runs on the 200 path only. The captured `called` bool is
the proof of short-circuiting, which is the security property that matters: a guard
that returns the right status but still executes the handler has leaked the side
effect. The forged-key test is the reason the key is an unexported type: a request
that tries to inject a principal under the string `"principal"` cannot, because
`PrincipalFromContext` reads only the unexported `ctxKey{}` slot. Storing `Roles`
as part of an immutable `Principal` value set once at authentication time keeps it
safe to read across the request's goroutines without locking.

## Resources

- [net/http Handler](https://pkg.go.dev/net/http#Handler) — the middleware-chaining contract these guards follow.
- [http.StatusUnauthorized / StatusForbidden](https://pkg.go.dev/net/http#pkg-constants) — the 401-vs-403 distinction.
- [slices.Contains](https://pkg.go.dev/slices#Contains) — the role-membership check.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-context-aware-slog-handler.md](03-context-aware-slog-handler.md) | Next: [05-trace-propagation-across-boundary.md](05-trace-propagation-across-boundary.md)
