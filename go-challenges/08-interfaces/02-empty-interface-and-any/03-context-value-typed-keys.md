# Exercise 3: Request-Scoped Values in `context.Context` with Collision-Proof Keys

`context.Context.Value` stores and returns `any`, and that is exactly where teams
get burned: two packages pick the same key, one silently shadows the other, and the
bug surfaces months later as a mysterious wrong user ID in a log. This module builds
the senior discipline — an unexported key type plus typed accessors — as a real HTTP
middleware that injects a request ID and reads it back in the handler.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
reqcontext/                independent module: example.com/reqcontext
  go.mod                   go 1.26
  reqcontext.go            unexported ctxKey; WithRequestID/RequestID; Middleware
  cmd/
    demo/
      main.go              runnable demo: middleware -> handler, ID flows through context
  reqcontext_test.go       collision safety, missing-value (no panic), httptest end-to-end
```

- Files: `reqcontext.go`, `cmd/demo/main.go`, `reqcontext_test.go`.
- Implement: an unexported key type, `WithRequestID(ctx, id)` and `RequestID(ctx) (string, bool)`, and an `http.Handler` middleware that generates an ID and injects it.
- Test: a value stored under this package's key is invisible under a different package's identically-underlying key; `RequestID` on a bare context returns `("", false)` not a panic; and an `httptest` round-trip proving the ID flows middleware to handler.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why the key must be an unexported type

`context.WithValue(parent, key, val)` compares keys with `==`, and interface
equality compares the dynamic type first. If two packages both use the built-in
string `"request_id"` as a key, those keys are equal — same dynamic type `string`,
same value — so the second `WithValue` shadows the first and `ctx.Value` returns the
wrong package's data. The fix is to make the key a type no other package can name.
An unexported `type ctxKey struct{}` (or `type ctxKey int` with unexported
constants) has a type identity private to this package: even if another package
declares its own `type ctxKey struct{}`, the two are distinct types, so their keys
can never be `==`. This is why the standard library and every serious framework key
context values with a private zero-size type, not a string.

The second half of the discipline is typed accessors. Callers never write
`ctx.Value(someKey).(string)` — that leaks the key, invites the panic form, and
duplicates the assertion at every call site. Instead the package exports
`WithRequestID(ctx, id) context.Context` to store and `RequestID(ctx) (string, bool)`
to read, and the accessor does the comma-ok assertion once, returning `("", false)`
for a context that never carried the value. A handler that logs the request ID reads
it through `RequestID` and handles the absent case; it never touches the key or the
assertion.

Create `reqcontext.go`:

```go
package reqcontext

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// ctxKey is a private, zero-size key type. Because it is unexported, no other
// package can construct a value of this type, so a key from this package can
// never collide with a key from any other package.
type ctxKey struct{}

// requestIDKey is the single instance used as the context key for the request ID.
var requestIDKey ctxKey

// WithRequestID returns a child context carrying id under this package's private
// key.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request ID stored in ctx, or ("", false) if none. It does
// the comma-ok assertion once so callers never touch the key or risk a panic.
func RequestID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey).(string)
	return id, ok
}

// newID returns a random 8-byte hex request ID.
func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Middleware injects a fresh request ID into the request context and echoes it in
// the response header, so downstream handlers can read it through RequestID.
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
```

### The runnable demo

The demo wires the middleware around a tiny handler and drives one request through
`httptest`, so you can see the ID that the middleware injected show up inside the
handler and in the response header.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/reqcontext"
)

func main() {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := reqcontext.RequestID(r.Context()); ok {
			fmt.Fprintf(w, "handled request %s", id)
		} else {
			fmt.Fprint(w, "no request id")
		}
	})

	srv := reqcontext.Middleware(handler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "abc123")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	fmt.Println("body:  ", rec.Body.String())
	fmt.Println("header:", rec.Header().Get("X-Request-ID"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
body:   handled request abc123
header: abc123
```

### Tests

`TestCollisionSafety` is the point of the whole module: it defines a second key type
with the same underlying representation (`struct{}`) and proves that a value stored
under this package's key is invisible under the other type's key — the types differ,
so the keys are not `==`. `TestMissingValue` proves `RequestID` on a bare context
returns `("", false)` rather than panicking. `TestMiddlewareFlow` drives a real
request through `httptest` and asserts the injected ID reaches the handler.

Create `reqcontext_test.go`:

```go
package reqcontext

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// otherKey mimics a DIFFERENT package that happens to use the same underlying
// struct{} representation for its context key.
type otherKey struct{}

func TestCollisionSafety(t *testing.T) {
	t.Parallel()

	ctx := WithRequestID(context.Background(), "real-id")

	// The other package's key has the same underlying type but a distinct type
	// identity, so it must NOT retrieve this package's value.
	var other otherKey
	if v := ctx.Value(other); v != nil {
		t.Fatalf("other package's key retrieved our value: %v", v)
	}

	// Our own accessor still sees it.
	if id, ok := RequestID(ctx); !ok || id != "real-id" {
		t.Fatalf("RequestID = %q,%v; want real-id,true", id, ok)
	}
}

func TestMissingValue(t *testing.T) {
	t.Parallel()

	id, ok := RequestID(context.Background())
	if ok {
		t.Fatalf("RequestID on bare context returned ok=true (%q)", id)
	}
	if id != "" {
		t.Fatalf("RequestID on bare context returned %q, want empty", id)
	}
}

func TestMiddlewareFlow(t *testing.T) {
	t.Parallel()

	var seen string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := RequestID(r.Context())
		if !ok {
			t.Error("handler saw no request id")
		}
		seen = id
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "fixed-123")
	rec := httptest.NewRecorder()
	Middleware(handler).ServeHTTP(rec, req)

	if seen != "fixed-123" {
		t.Fatalf("handler saw id %q, want fixed-123", seen)
	}
	if got := rec.Header().Get("X-Request-ID"); got != "fixed-123" {
		t.Fatalf("response header = %q, want fixed-123", got)
	}
}

func TestMiddlewareGeneratesID(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := RequestID(r.Context()); !ok || id == "" {
			t.Errorf("expected a generated id, got %q,%v", id, ok)
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil) // no X-Request-ID header
	rec := httptest.NewRecorder()
	Middleware(handler).ServeHTTP(rec, req)

	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("middleware did not generate a request id")
	}
}
```

## Review

The key discipline is correct when a value stored under this package's key cannot be
read through any key another package could construct — `TestCollisionSafety` proves
that with a same-underlying-type-but-distinct-type key. The accessor is correct when
a bare context yields `("", false)`, never a panic, which is why `RequestID` uses the
comma-ok form on the way out. The mistake this module prevents is keying context with
a bare string (`context.WithValue(ctx, "request_id", id)`): it compiles, it works in
a demo, and it silently collides the moment another package or a third-party library
uses the same string. The second mistake is exporting the key and letting callers
assert — that scatters the assertion and re-opens the panic risk. Run `go test -race`
to confirm the middleware and accessors are safe under concurrent requests.

## Resources

- [`context.WithValue`](https://pkg.go.dev/context#WithValue) — the documented guidance to use an unexported key type.
- [`context.Context`](https://pkg.go.dev/context#Context) — `Value` takes and returns `any`.
- [Go blog: Contexts and structs](https://go.dev/blog/context-and-structs) — request-scoped values and key hygiene.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-sql-driver-valuer-scanner.md](04-sql-driver-valuer-scanner.md)
