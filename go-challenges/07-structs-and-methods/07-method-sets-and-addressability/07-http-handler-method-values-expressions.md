# Exercise 7: Wiring HTTP Handlers with Method Values and Method Expressions

Every time you write `mux.HandleFunc("/users", h.HandleGet)` you rely on a method
value: `h.HandleGet` binds the receiver `h` and produces a plain function that
still routes to your struct's state. This module wires a real handler both ways —
as a method value and as a method expression `(*UserHandler).HandleGet` — and
proves the two forms are the same binding, then shows why the captured receiver is
shared state under concurrent requests.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
userapi/                       independent module: example.com/userapi
  go.mod                       module path + go directive
  handler.go                   *UserHandler with HandleGet(w, r); atomic request counter
  cmd/
    demo/
      main.go                  register a method value on a ServeMux, serve a request
  handler_test.go              method-value test, method-expression test, -race shared handler
```

- Files: `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
- Implement: `*UserHandler` holding a user map and an atomic served-count, with `HandleGet(w, r)` shaped like `http.HandlerFunc`.
- Test: register `h.HandleGet` (method value) on a `ServeMux` and drive it with `httptest`; call `(*UserHandler).HandleGet(h, w, r)` (method expression) and assert identical behavior; a `-race` test sharing one handler across concurrent requests.
- Verify: `go vet ./...`, `go test -count=1 -race ./...`.

### Method value vs method expression

`HandleGet` is a method on `*UserHandler` with the signature
`func(http.ResponseWriter, *http.Request)` — the same shape as `http.HandlerFunc`.
There are two ways to turn that method into a first-class function value, and both
show up in real router wiring.

A method value is `h.HandleGet`, where `h` is a `*UserHandler`. Writing that
expression evaluates `h` immediately and produces a `func(w, r)` with the receiver
baked in. `mux.HandleFunc("/users", h.HandleGet)` hands the router that closure;
when a request arrives, the closure calls `HandleGet` with the captured `h`, so
the handler reaches your struct's fields. The receiver is captured by that pointer
at binding time — every request through this route shares the same `*UserHandler`
and therefore the same state (the user map, the served-count). That sharing is the
point: it is how a handler accumulates metrics or reads a shared cache. It is also
why the handler's shared fields must be safe for concurrent access — `net/http`
serves each request in its own goroutine.

A method expression is `(*UserHandler).HandleGet`, which produces a
`func(*UserHandler, http.ResponseWriter, *http.Request)` — the receiver becomes an
explicit first parameter. You call it as `(*UserHandler).HandleGet(h, w, r)`. This
is the same binding written differently: `h.HandleGet(w, r)` and
`(*UserHandler).HandleGet(h, w, r)` invoke exactly the same method on exactly the
same receiver and do exactly the same thing. Method expressions are handy when you
want to choose the receiver per call, or to build middleware that takes the
receiver as a parameter.

The subtle trap with method values: the capture happens when you write `h.M`, not
when the resulting function runs. If you later reassign `h` to a different handler,
a function value you already created still points at the original receiver. For
router wiring that is exactly what you want (stable handler for the route), but it
surprises people who expect the stored function to follow a reassigned variable.

Create `handler.go`:

```go
package userapi

import (
	"net/http"
	"sync/atomic"
)

// UserHandler serves user lookups. It holds shared state (a user map and a
// served-count) that every request through a bound method value shares, so the
// state must be safe for concurrent access.
type UserHandler struct {
	users  map[string]string // id -> name; read-only after construction
	served atomic.Int64
}

// NewUserHandler builds a handler over a fixed user set.
func NewUserHandler(users map[string]string) *UserHandler {
	return &UserHandler{users: users}
}

// Served reports how many requests this handler has answered.
func (h *UserHandler) Served() int64 { return h.served.Load() }

// HandleGet is shaped like http.HandlerFunc: func(w, r). As a method value
// (h.HandleGet) it satisfies http.HandlerFunc with the receiver captured.
func (h *UserHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	h.served.Add(1)
	id := r.URL.Query().Get("id")
	name, ok := h.users[id]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(name))
}
```

### The runnable demo

The demo registers the handler as a method value on a `ServeMux`, serves one
request through the mux with `httptest`, and prints the body and the served-count
to show the captured receiver's state advancing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/userapi"
)

func main() {
	h := userapi.NewUserHandler(map[string]string{"u1": "alice"})

	mux := http.NewServeMux()
	// Method value: h.HandleGet captures the receiver h and satisfies the
	// func(w, r) shape HandleFunc wants.
	mux.HandleFunc("GET /users", h.HandleGet)

	req := httptest.NewRequest(http.MethodGet, "/users?id=u1", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	fmt.Printf("status=%d body=%s served=%d\n", rec.Code, rec.Body.String(), h.Served())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=200 body=alice served=1
```

### Tests

One test drives the handler as a method value on a `ServeMux`; a second calls it
through the method expression `(*UserHandler).HandleGet(h, w, r)` and asserts
identical behavior, proving the two forms are the same binding. A `-race` test
fires many concurrent requests at one shared handler to prove the captured
receiver's state is safe under concurrency.

Create `handler_test.go`:

```go
package userapi

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func newTestHandler() *UserHandler {
	return NewUserHandler(map[string]string{"u1": "alice", "u2": "bob"})
}

func TestMethodValueOnServeMux(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users", h.HandleGet) // method value

	req := httptest.NewRequest(http.MethodGet, "/users?id=u2", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "bob" {
		t.Fatalf("status=%d body=%q, want 200/bob", rec.Code, rec.Body.String())
	}
}

func TestMethodExpressionEquivalent(t *testing.T) {
	t.Parallel()
	h := newTestHandler()

	// Method expression: receiver is an explicit first argument. Same binding as
	// h.HandleGet(w, r).
	fn := (*UserHandler).HandleGet
	req := httptest.NewRequest(http.MethodGet, "/users?id=u1", nil)
	rec := httptest.NewRecorder()
	fn(h, rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "alice" {
		t.Fatalf("method expression: status=%d body=%q, want 200/alice", rec.Code, rec.Body.String())
	}
}

func TestSharedHandlerConcurrent(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	handler := http.HandlerFunc(h.HandleGet) // one captured receiver, shared

	const n = 200
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/users?id=u1", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
		}()
	}
	wg.Wait()

	if got := h.Served(); got != n {
		t.Fatalf("served = %d, want %d", got, n)
	}
}
```

## Review

The wiring is correct when the method value and the method expression produce the
same behavior: `TestMethodValueOnServeMux` and `TestMethodExpressionEquivalent`
serve the same handler two ways and assert the same response, which is the
concrete meaning of "a method value binds the receiver, a method expression takes
it as a first parameter". The `-race` test proves the shared captured receiver is
safe: 200 concurrent requests all land on one `*UserHandler` and the atomic
served-count reaches exactly 200.

The trap to remember is that a method value captures its receiver at the moment
you write `h.M`, not when the function later runs, so reassigning `h` afterward
does not change what the stored function operates on. For router wiring that is
the desired behavior. Because the captured receiver is shared across every request
goroutine, any mutable field it holds must be concurrency-safe — here the served
count is an `atomic.Int64`. Run `go vet` and `go test -race`.

## Resources

- [Go Language Specification: Method values](https://go.dev/ref/spec#Method_values) — how `x.M` binds and captures the receiver.
- [Go Language Specification: Method expressions](https://go.dev/ref/spec#Method_expressions) — how `T.M` yields a function taking the receiver explicitly.
- [`net/http.HandlerFunc`](https://pkg.go.dev/net/http#HandlerFunc) — the adapter that turns a `func(w, r)` into a `Handler`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-stringer-pointer-vs-value-logging.md](08-stringer-pointer-vs-value-logging.md)
