# Exercise 6: Propagate Request ID and Tenant via Typed Context Keys

Request-scoped metadata — a correlation ID, the tenant, the authenticated subject
— needs to reach a structured log line in the repository layer without being
threaded through every function signature. `context.WithValue` is the right tool
for exactly this, and only this. This exercise builds a metadata carrier with an
unexported key type and typed accessors, wires it through an HTTP middleware, and
proves the values ride the context safely across package boundaries.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
reqmeta/                     independent module: example.com/reqmeta
  go.mod                     go 1.24
  reqmeta.go                 unexported ctxKey; WithRequestID/RequestID; WithTenant/Tenant;
                             Middleware; repo log helper reading the ID back
  cmd/
    demo/
      main.go                middleware injects an ID; handler and repo read it back
  reqmeta_test.go            round-trip, missing-key zero, key isolation, survives derivation
```

Files: `reqmeta.go`, `cmd/demo/main.go`, `reqmeta_test.go`.
Implement: an unexported `type ctxKey int`; `WithRequestID`/`RequestID` and
`WithTenant`/`Tenant` accessors using comma-ok assertions; an HTTP `Middleware`
that injects a correlation ID; a repo helper that reads it back.
Test: values round-trip across a layer boundary; a missing key returns
`("", false)` without panicking; a foreign string key cannot read the typed key;
values survive `WithTimeout`/`WithCancel` derivation.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why an unexported key type, and where WithValue stops being appropriate

`context.WithValue(ctx, key, val)` stores `val` under `key`, and `ctx.Value(key)`
retrieves it by equality. If `key` is a bare string like `"request_id"`, any other
package that happens to use the same string reads or clobbers your value — the
context is a single global namespace and strings collide. The fix, spelled out in
the `context` documentation, is to key with an **unexported package-local type**:
`type ctxKey int`, with constants `requestIDKey`, `tenantKey`. A value of an
unexported type from your package cannot be named by any other package, so no
external code can construct the same key, and even a foreign key with the *same
underlying type* is a different type and therefore unequal. That is the isolation
`TestKeyTypeIsolation` demonstrates.

Wrap the raw `WithValue`/`Value` calls in typed accessors and never expose the raw
key. `WithRequestID(ctx, id)` returns a derived context; `RequestID(ctx)` does a
comma-ok type assertion and returns `("", false)` when the key is absent or holds
the wrong type, so a caller never panics on a missing correlation ID. This is the
whole safe API surface.

The boundary of legitimate use is just as important as the mechanism.
`context.WithValue` is for request-scoped, *immutable* metadata that genuinely
crosses many layers — a correlation ID that a log line five frames down needs, a
tenant that authorization checks everywhere consult. It is *not* for optional
function parameters (pass those explicitly), not for mutable state (a context is
read-only after derivation), and not for dependency injection (wire dependencies
through constructors). The test suite exercises the legitimate case; the "Common
Mistakes" in the concepts file names the abuses.

Create `reqmeta.go`:

```go
package reqmeta

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// ctxKey is unexported, so no other package can construct these keys and collide
// with the values this package stores.
type ctxKey int

const (
	requestIDKey ctxKey = iota
	tenantKey
)

// WithRequestID returns a context carrying the correlation id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID reads the correlation id. The comma-ok result is ("", false) when
// the key is absent or holds an unexpected type, so callers never panic.
func RequestID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey).(string)
	return id, ok
}

// WithTenant returns a context carrying the tenant identifier.
func WithTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, tenantKey, tenant)
}

// Tenant reads the tenant identifier with the same safe comma-ok contract.
func Tenant(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(tenantKey).(string)
	return t, ok
}

// Middleware injects a correlation ID into the request context: it reuses an
// inbound X-Request-ID header when present, otherwise mints one. Downstream
// handlers and the repo layer read it back with RequestID.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = newID()
		}
		ctx := WithRequestID(r.Context(), id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RepoLogLine models a repository that reads the correlation ID from ctx for a
// structured log line, without the ID ever appearing in its function signature.
func RepoLogLine(ctx context.Context, query string) string {
	id, ok := RequestID(ctx)
	if !ok {
		id = "unknown"
	}
	return "request_id=" + id + " query=" + query
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
```

### The runnable demo

The demo runs a request through the middleware into a handler that calls the repo
helper, showing the correlation ID crossing from middleware to the leaf without a
parameter.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/reqmeta"
)

func main() {
	handler := reqmeta.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		line := reqmeta.RepoLogLine(r.Context(), "SELECT 1")
		fmt.Fprintln(w, line)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "corr-123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	fmt.Print(rec.Body.String())
	fmt.Println("echoed header:", rec.Header().Get("X-Request-ID"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request_id=corr-123 query=SELECT 1
echoed header: corr-123
```

### Tests

Create `reqmeta_test.go`:

```go
package reqmeta

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRoundTripsRequestID(t *testing.T) {
	t.Parallel()

	ctx := WithRequestID(context.Background(), "corr-abc")
	// Simulate crossing a layer boundary by passing the context along.
	line := RepoLogLine(ctx, "SELECT 1")
	if got, want := line, "request_id=corr-abc query=SELECT 1"; got != want {
		t.Fatalf("RepoLogLine = %q, want %q", got, want)
	}
}

func TestMissingValueReturnsZeroSafely(t *testing.T) {
	t.Parallel()

	id, ok := RequestID(context.Background())
	if ok || id != "" {
		t.Fatalf("RequestID on empty ctx = (%q, %v), want (\"\", false)", id, ok)
	}
	if line := RepoLogLine(context.Background(), "q"); line != "request_id=unknown query=q" {
		t.Fatalf("RepoLogLine without ID = %q", line)
	}
}

// otherKey mimics a different package's key. Even with a string underlying type,
// it is a distinct type and cannot read the typed ctxKey value.
type otherKey string

func TestKeyTypeIsolation(t *testing.T) {
	t.Parallel()

	ctx := WithRequestID(context.Background(), "corr-xyz")

	// A foreign key (even a plain string) cannot read our typed value.
	if v := ctx.Value("requestID"); v != nil {
		t.Fatalf("string key read the typed value: %v", v)
	}
	if v := ctx.Value(otherKey("requestID")); v != nil {
		t.Fatalf("foreign-typed key read the typed value: %v", v)
	}
	// Our accessor still works.
	if id, ok := RequestID(ctx); !ok || id != "corr-xyz" {
		t.Fatalf("RequestID = (%q, %v), want (corr-xyz, true)", id, ok)
	}
}

func TestValueSurvivesDerivation(t *testing.T) {
	t.Parallel()

	ctx := WithTenant(WithRequestID(context.Background(), "corr-1"), "acme")
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	ctx, cancel2 := context.WithCancel(ctx)
	defer cancel2()

	if id, ok := RequestID(ctx); !ok || id != "corr-1" {
		t.Fatalf("RequestID after derivation = (%q, %v)", id, ok)
	}
	if tenant, ok := Tenant(ctx); !ok || tenant != "acme" {
		t.Fatalf("Tenant after derivation = (%q, %v)", tenant, ok)
	}
}

func TestMiddlewareMintsIDWhenAbsent(t *testing.T) {
	t.Parallel()

	var seen string
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := RequestID(r.Context())
		if !ok {
			t.Error("handler saw no request ID")
		}
		seen = id
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if seen == "" {
		t.Fatal("middleware did not mint an ID")
	}
	if echoed := rec.Header().Get("X-Request-ID"); echoed != seen {
		t.Fatalf("echoed header %q != injected ID %q", echoed, seen)
	}
}

func ExampleRepoLogLine() {
	ctx := WithRequestID(context.Background(), "corr-123")
	fmt.Println(RepoLogLine(ctx, "SELECT 1"))
	// Output: request_id=corr-123 query=SELECT 1
}
```

## Review

The carrier is correct when a value put in at the edge is readable at the leaf and
unreadable by anyone using a different key. `TestKeyTypeIsolation` is the load-
bearing test: it proves that neither a bare string `"requestID"` nor a foreign
type with a string underlying type can reach the value, which is the entire reason
the key type is unexported. `TestValueSurvivesDerivation` proves the other half of
the model — values flow through `WithTimeout`/`WithCancel` derivation untouched, so
adding a deadline downstream never drops the correlation ID. Keep the accessors'
comma-ok contract: returning `("", false)` rather than panicking on an absent key
is what lets a log helper degrade to `request_id=unknown` instead of crashing a
request. Run `go test -race`; the middleware and accessors are read-only over the
context, so a clean race build confirms no accidental shared mutation.

## Resources

- [`context.WithValue`](https://pkg.go.dev/context#WithValue) — the request-scoped-only contract and the unexported-key guidance.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) — the original request-scoped-values example.
- [Go Code Review Comments: Contexts](https://go.dev/wiki/CodeReviewComments#contexts) — why values are for request-scoped data, not optional parameters.

---

Prev: [05-errgroup-bounded-fanout.md](05-errgroup-bounded-fanout.md) | Back to [00-concepts.md](00-concepts.md) | Next: [07-detached-cleanup-withoutcancel.md](07-detached-cleanup-withoutcancel.md)
