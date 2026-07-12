# Exercise 7: Wire An HTTP Handler To A Service Interface

An HTTP handler is a consumer like any other: it should depend on the narrow service
interface it calls, not a concrete service glued to a database. This module builds an
`*ItemHandler` that accepts an `ItemService` interface, satisfies `http.Handler`, and
is tested end to end with `httptest` and a fake service — no real store, no network.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
httphandler/                independent module: example.com/httphandler
  go.mod                    go 1.26
  handler.go                Item, ErrNotFound; ItemService iface; ItemHandler (http.Handler)
  cmd/
    demo/
      main.go               runs the handler under httptest.NewServer and calls it
  handler_test.go           httptest recorder: 200/JSON, 404, 400; Content-Type golden
```

Files: `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
Implement: an `ItemService` interface (`GetItem`/`PutItem`), and an `ItemHandler` that accepts it, routes `GET /items/{id}` and `POST /items`, and maps `errors.Is(err, ErrNotFound)` to 404, malformed JSON to 400, success to 200/201.
Test: a fake `ItemService`; assert 200 + JSON body + `Content-Type`; assert 404 on `ErrNotFound`; assert 400 on malformed JSON.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/08-accept-interfaces-return-structs/07-http-handler-service-interface/cmd/demo
cd go-solutions/08-interfaces/08-accept-interfaces-return-structs/07-http-handler-service-interface
go mod edit -go=1.26
```

### Why the handler depends on an interface, and how outcomes map to status

`NewItemHandler` accepts an `ItemService` — a two-method interface — and returns
`*ItemHandler`, which satisfies `http.Handler`. That is the chapter rule applied to the
web layer: the handler is testable against a fake service that returns exactly the
outcome each test needs, and it is decoratable (an auth or logging service wrapper drops
in unchanged). The handler never imports a database driver; it depends only on the two
methods it calls.

The interesting logic is the mapping from domain outcome to HTTP status, and the seam
that makes it possible is the sentinel error. On `GET`, the handler calls
`svc.GetItem`; if that returns an error matching `ErrNotFound` via `errors.Is`, it
writes 404; any other error is 500; success is 200 with a JSON body and an
`application/json` content type. On `POST`, a body that fails to decode is a client
error — 400 — before the service is ever called; a successful decode and `PutItem`
yields 201. Routing uses the Go 1.22 method-and-wildcard patterns
(`GET /items/{id}`), and the id comes from `r.PathValue("id")`, populated because the
request is routed through the handler's own `http.ServeMux`.

Create `handler.go`:

```go
package httphandler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// ErrNotFound is the domain sentinel the handler maps to 404.
var ErrNotFound = errors.New("httphandler: item not found")

type Item struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Price int64  `json:"price"`
}

// ItemService is the consumer-owned port the handler depends on.
type ItemService interface {
	GetItem(ctx context.Context, id string) (Item, error)
	PutItem(ctx context.Context, item Item) error
}

// ItemHandler accepts an ItemService and is an http.Handler.
type ItemHandler struct {
	svc ItemService
	mux *http.ServeMux
}

// NewItemHandler accepts the interface and returns the struct. It wires routes onto
// an internal mux so PathValue works.
func NewItemHandler(svc ItemService) *ItemHandler {
	h := &ItemHandler{svc: svc}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", h.get)
	mux.HandleFunc("POST /items", h.create)
	h.mux = mux
	return h
}

var _ http.Handler = (*ItemHandler)(nil)

func (h *ItemHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *ItemHandler) get(w http.ResponseWriter, r *http.Request) {
	item, err := h.svc.GetItem(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(item)
}

func (h *ItemHandler) create(w http.ResponseWriter, r *http.Request) {
	var item Item
	if err := json.NewDecoder(r.Body).Decode(&item); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if err := h.svc.PutItem(r.Context(), item); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(item)
}
```

### The runnable demo

The demo backs the handler with a tiny in-memory service, serves it under
`httptest.NewServer`, and makes two real requests — one hit, one miss — printing the
status codes. It exercises the exact code path a production server would.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"example.com/httphandler"
)

// memService is a minimal in-memory ItemService for the demo.
type memService struct {
	items map[string]httphandler.Item
}

func (s *memService) GetItem(_ context.Context, id string) (httphandler.Item, error) {
	item, ok := s.items[id]
	if !ok {
		return httphandler.Item{}, httphandler.ErrNotFound
	}
	return item, nil
}

func (s *memService) PutItem(_ context.Context, item httphandler.Item) error {
	s.items[item.ID] = item
	return nil
}

func main() {
	svc := &memService{items: map[string]httphandler.Item{
		"sku-1": {ID: "sku-1", Name: "widget", Price: 1299},
	}}
	srv := httptest.NewServer(httphandler.NewItemHandler(svc))
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/items/sku-1")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("GET sku-1 -> %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	fmt.Println()

	miss, _ := http.Get(srv.URL + "/items/nope")
	miss.Body.Close()
	fmt.Printf("GET nope  -> %d\n", miss.StatusCode)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET sku-1 -> 200 {"id":"sku-1","name":"widget","price":1299}
GET nope  -> 404
```

### Tests

`fakeService` returns whatever the test scripts: a known item, or `ErrNotFound`. The
tests drive the handler with `httptest.NewRequest`/`NewRecorder` and assert the status,
the JSON body, and the `Content-Type` header on success; a 404 on the miss; and a 400
on a malformed POST body.

Create `handler_test.go`:

```go
package httphandler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeService is a scripted ItemService.
type fakeService struct {
	item   Item
	getErr error
}

func (f fakeService) GetItem(context.Context, string) (Item, error) {
	if f.getErr != nil {
		return Item{}, f.getErr
	}
	return f.item, nil
}

func (f fakeService) PutItem(context.Context, Item) error { return nil }

var _ ItemService = fakeService{}

func TestGetItemOK(t *testing.T) {
	t.Parallel()
	h := NewItemHandler(fakeService{item: Item{ID: "sku-1", Name: "widget", Price: 1299}})

	req := httptest.NewRequest(http.MethodGet, "/items/sku-1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var got Item
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Name != "widget" || got.Price != 1299 {
		t.Fatalf("body = %+v, want widget/1299", got)
	}
}

func TestGetItemNotFound(t *testing.T) {
	t.Parallel()
	h := NewItemHandler(fakeService{getErr: ErrNotFound})

	req := httptest.NewRequest(http.MethodGet, "/items/missing", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestCreateItemMalformedBody(t *testing.T) {
	t.Parallel()
	h := NewItemHandler(fakeService{})

	req := httptest.NewRequest(http.MethodPost, "/items", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCreateItemOK(t *testing.T) {
	t.Parallel()
	h := NewItemHandler(fakeService{})

	body := strings.NewReader(`{"id":"sku-9","name":"gadget","price":500}`)
	req := httptest.NewRequest(http.MethodPost, "/items", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
}
```

## Review

The handler is correct when a found item yields 200 with a JSON body and an
`application/json` content type, `ErrNotFound` yields 404, and a malformed POST body
yields 400 before the service is consulted — all asserted with `httptest` and a fake
service, so the branches are pinned in microseconds with no store and no socket. The
seam that makes the status mapping possible is the sentinel matched with `errors.Is`,
not a string compare or a concrete error type; that is what keeps the handler decoupled
from how the service reports "absent". Because `NewItemHandler` accepts the interface
and returns the `*ItemHandler` struct, the same handler runs against the in-memory demo
service, the fakes, and a real database-backed service with no change.

## Resources

- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest`, `NewRecorder`, `NewServer` for hermetic handler tests.
- [Routing enhancements for Go 1.22](https://go.dev/blog/routing-enhancements) — method-and-wildcard patterns and `Request.PathValue`.
- [`encoding/json`](https://pkg.go.dev/encoding/json) — `NewEncoder`/`NewDecoder` and struct tags used for the request and response bodies.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-typed-nil-interface-trap.md](06-typed-nil-interface-trap.md) | Next: [08-config-loader-io-reader.md](08-config-loader-io-reader.md)
