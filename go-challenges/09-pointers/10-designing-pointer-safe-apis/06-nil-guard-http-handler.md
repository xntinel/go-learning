# Exercise 6: Guarding a Nil Pointer in an HTTP Handler — 404, Not 500

The place a `(nil, ErrNotFound)` contract pays off — or fails to — is the HTTP
handler that consumes it. This module builds a handler that looks up an entity by
id and must distinguish a miss from a hit *before* it dereferences the pointer: a
naive handler touches the nil pointer on a miss and panics into a 500; the correct
one checks the error, writes 404 on a miss, and reads pointer fields only on the
non-nil hit path.

This module is fully self-contained: its own `go mod init`, its own types, its own
demo, and its own tests. Nothing here imports another exercise.

## What you'll build

```text
handler/                    independent module: example.com/handler
  go.mod                    go 1.25
  handler.go                Entity; store; UserHandler that returns 404 on miss, 200 on hit
  cmd/
    demo/
      main.go               drive the handler for a hit and a miss with httptest
  handler_test.go           404-no-panic, 200-body, and a regression test for the guard
```

- Files: `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
- Implement: an `http.HandlerFunc` that calls the store, checks `errors.Is(err, ErrNotFound)` and writes `http.StatusNotFound`, and only reads pointer fields on the hit path.
- Test: `httptest` a miss and assert 404 with no panic; `httptest` a hit and assert 200 with the entity body; a regression test that a raw dereference on the miss path would panic (proving the guard is load-bearing); assert a normal miss never yields 500.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/10-designing-pointer-safe-apis/06-nil-guard-http-handler/cmd/demo
cd go-solutions/09-pointers/10-designing-pointer-safe-apis/06-nil-guard-http-handler
go mod edit -go=1.25
```

### The miss path is where handlers panic

A lookup returns `(*Entity, error)`. The correct handler reads the error first:

```go
e, err := store.Get(id)
if errors.Is(err, ErrNotFound) {
	http.Error(w, "not found", http.StatusNotFound)
	return
}
// e is guaranteed non-nil here
fmt.Fprintf(w, "%s", e.Name)
```

The subtle failure is the version that reaches for `e.Name` before the guard, or
that guards on the wrong condition. On a miss, `e` is nil; dereferencing it panics;
`net/http` recovers the panic in the serving goroutine and turns it into a bare
`500 Internal Server Error` with no useful body. So a perfectly ordinary "user
does not exist" — which should be a clean 404 — becomes a 500 that pages someone.
The nil-guard is not defensive decoration; it is the difference between a correct
status code and an outage-shaped log line. Because `Get` honors the
`(nil, ErrNotFound)` contract, the guard is a single `errors.Is` check, and the
compiler-invisible invariant "e is non-nil past this point" holds by construction.

### Why the regression test matters

It is easy to write the correct handler and never prove the guard is doing
anything. The regression test in this module calls the store directly for a
missing key, asserts the returned pointer is nil, and documents that dereferencing
it *would* panic — so a future edit that moves the field read above the guard is
caught. Pairing "the handler returns 404" with "the raw miss result is a nil
pointer" makes the guard's necessity explicit rather than incidental.

Create `handler.go`:

```go
package handler

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
)

var ErrNotFound = errors.New("entity not found")

type Entity struct {
	ID   string
	Name string
}

// Store is a minimal repository honoring the (nil, ErrNotFound) contract.
type Store struct {
	mu sync.RWMutex
	m  map[string]*Entity
}

func NewStore() *Store {
	return &Store{m: make(map[string]*Entity)}
}

func (s *Store) Add(e *Entity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[e.ID] = e
}

func (s *Store) Get(id string) (*Entity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return e, nil
}

// UserHandler returns an http.HandlerFunc that looks up ?id= and writes 404 on a
// miss (never dereferencing the nil pointer) or 200 with the name on a hit.
func UserHandler(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")

		e, err := s.Get(id)
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// e is guaranteed non-nil here; only now do we read a pointer field.
		fmt.Fprintf(w, "user: %s", e.Name)
	}
}
```

### The runnable demo

The demo wires the handler with `httptest` and drives it for a hit and a miss,
printing the status code each returns.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http/httptest"

	"example.com/handler"
)

func main() {
	s := handler.NewStore()
	s.Add(&handler.Entity{ID: "u1", Name: "alice"})
	h := handler.UserHandler(s)

	for _, id := range []string{"u1", "u2"} {
		req := httptest.NewRequest("GET", "/user?id="+id, nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		body, _ := io.ReadAll(rec.Result().Body)
		fmt.Printf("id=%s status=%d body=%q\n", id, rec.Code, string(body))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id=u1 status=200 body="user: alice"
id=u2 status=404 body="user not found\n"
```

### Tests

`TestHandlerHit` asserts 200 and the entity body. `TestHandlerMiss` asserts 404
and — crucially — that serving a miss did not panic (the test completing is the
proof; a nil deref would have crashed the request). `TestMissNeverReturns500`
pins that an ordinary miss is a 404, never a 500. `TestMissResultIsNilPointer` is
the regression test: it proves `Get` returns a nil pointer on a miss, so the
handler's guard is load-bearing and any future reordering that dereferences before
guarding would panic.

Create `handler_test.go`:

```go
package handler

import (
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"testing"
)

func seed() *Store {
	s := NewStore()
	s.Add(&Entity{ID: "u1", Name: "alice"})
	return s
}

func TestHandlerHit(t *testing.T) {
	t.Parallel()
	h := UserHandler(seed())
	req := httptest.NewRequest("GET", "/user?id=u1", nil)
	rec := httptest.NewRecorder()

	h(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if string(body) != "user: alice" {
		t.Fatalf("body = %q, want %q", string(body), "user: alice")
	}
}

func TestHandlerMiss(t *testing.T) {
	t.Parallel()
	h := UserHandler(seed())
	req := httptest.NewRequest("GET", "/user?id=ghost", nil)
	rec := httptest.NewRecorder()

	h(rec, req) // must not panic on the nil pointer

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestMissNeverReturns500(t *testing.T) {
	t.Parallel()
	h := UserHandler(seed())
	req := httptest.NewRequest("GET", "/user?id=nope", nil)
	rec := httptest.NewRecorder()

	h(rec, req)

	if rec.Code == 500 {
		t.Fatal("ordinary miss returned 500; the nil-guard is missing")
	}
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestMissResultIsNilPointer(t *testing.T) {
	t.Parallel()
	e, err := seed().Get("ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if e != nil {
		t.Fatal("miss returned a non-nil pointer; guard would be unnecessary")
	}
	// Dereferencing e here (e.Name) would panic — which is exactly what the
	// handler's errors.Is guard prevents.
}

func ExampleUserHandler() {
	s := NewStore()
	s.Add(&Entity{ID: "u1", Name: "alice"})
	h := UserHandler(s)

	req := httptest.NewRequest("GET", "/user?id=u1", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	body, _ := io.ReadAll(rec.Result().Body)
	fmt.Printf("status=%d body=%q\n", rec.Code, string(body))
	// Output: status=200 body="user: alice"
}
```

## Review

The handler is correct when a miss produces a 404 with no panic and a hit produces
a 200 with the body, and when the error check precedes any pointer dereference. The
regression test proving the miss result is a nil pointer is what makes the guard's
necessity explicit: without it, a reader might "clean up" the handler by hoisting
`e.Name` above the guard and reintroduce the 500. The mistake to avoid is treating
`net/http`'s panic recovery as a safety net — it converts your logic bug into a
content-free 500, which is strictly worse than the honest 404 the contract makes
trivial. Read the error first; touch the pointer only on the branch where it is
guaranteed non-nil.

## Resources

- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest`/`NewRecorder` for driving a handler in tests.
- [`http.Error`](https://pkg.go.dev/net/http#Error) — writing a status code and message.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching `ErrNotFound` to choose 404 over 500.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-functional-options-constructor.md](05-functional-options-constructor.md) | Next: [07-atomic-pointer-config-hot-reload.md](07-atomic-pointer-config-hot-reload.md)
