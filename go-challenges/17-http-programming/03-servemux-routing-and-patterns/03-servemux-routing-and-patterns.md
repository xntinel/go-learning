# 3. ServeMux Routing and Patterns

Go's standard `net/http` router can match methods, path wildcards, and catch-all wildcards. This lesson builds a reusable library package around `http.NewServeMux` instead of putting all routing logic in `package main`.

## Concepts

Go 1.22 enhanced `http.ServeMux` patterns. A pattern such as `GET /items` restricts the handler to GET requests. A wildcard such as `/items/{id}` captures one path segment, and a catch-all wildcard such as `/files/{path...}` captures the rest of the path. Read captured values with `Request.PathValue`.

Registering `GET` also handles `HEAD`. When a path matches but the method does not, `ServeMux` returns `405 Method Not Allowed`. When patterns overlap, the most specific pattern wins; conflicting patterns panic during registration.

Keep routing construction in a library function so tests and demos can share the same behavior. Use sentinel errors such as `ErrNotFound` and `ErrInvalidItem` in the data layer, wrap them with `%w`, and check them with `errors.Is` in handlers and tests.

## Exercises

Create this module layout:

```text
servemux-routing/
  go.mod
  routing.go
  routing_example_test.go
  routing_test.go
  cmd/demo/main.go
```

Create `go.mod`:

```go
module example.com/routing

go 1.26
```

Create `routing.go`:

```go
package routing

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
)

var (
	ErrInvalidItem = errors.New("invalid item")
	ErrNotFound    = errors.New("item not found")
)

type Item struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Store struct {
	mu     sync.Mutex
	nextID int
	items  map[string]Item
}

func NewStore() *Store {
	return &Store{nextID: 1, items: make(map[string]Item)}
}

func (s *Store) Create(name string) (Item, error) {
	if name == "" {
		return Item{}, fmt.Errorf("create item: %w", ErrInvalidItem)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	item := Item{ID: strconv.Itoa(s.nextID), Name: name}
	s.nextID++
	s.items[item.ID] = item
	return item, nil
}

func (s *Store) List() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()

	items := make([]Item, 0, len(s.items))
	for _, item := range s.items {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

func (s *Store) Get(id string) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	item, ok := s.items[id]
	if !ok {
		return Item{}, fmt.Errorf("get item %q: %w", id, ErrNotFound)
	}
	return item, nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.items[id]; !ok {
		return fmt.Errorf("delete item %q: %w", id, ErrNotFound)
	}
	delete(s.items, id)
	return nil
}

func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

func NewRouter(store *Store) http.Handler {
	if store == nil {
		store = NewStore()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /items", listItems(store))
	mux.HandleFunc("POST /items", createItem(store))
	mux.HandleFunc("GET /items/{id}", getItem(store))
	mux.HandleFunc("DELETE /items/{id}", deleteItem(store))
	mux.HandleFunc("GET /files/{path...}", getFilePath)
	return mux
}

func listItems(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, store.List())
	}
}

func createItem(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		item, err := store.Create(input.Name)
		if errors.Is(err, ErrInvalidItem) {
			http.Error(w, "name is required", http.StatusUnprocessableEntity)
			return
		}
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		writeJSON(w, http.StatusCreated, item)
	}
}

func getItem(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		item, err := store.Get(r.PathValue("id"))
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		writeJSON(w, http.StatusOK, item)
	}
}

func deleteItem(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := store.Delete(r.PathValue("id")); errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		} else if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func getFilePath(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"path": r.PathValue("path")})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
```

Create `routing_example_test.go`:

```go
package routing_test

import (
	"fmt"

	"example.com/routing"
)

func ExampleStore() {
	store := routing.NewStore()
	item, _ := store.Create("Widget")

	fmt.Println(item.ID, item.Name)
	fmt.Println(store.Count())

	// Output:
	// 1 Widget
	// 1
}
```

Create `routing_test.go`:

```go
package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStoreErrors(t *testing.T) {
	t.Parallel()

	store := NewStore()

	if _, err := store.Create(""); !errors.Is(err, ErrInvalidItem) {
		t.Fatalf("Create error = %v, want ErrInvalidItem", err)
	}
	if _, err := store.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestRouter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		statusCode int
	}{
		{name: "list", method: http.MethodGet, path: "/items", statusCode: http.StatusOK},
		{name: "create", method: http.MethodPost, path: "/items", body: `{"name":"Widget"}`, statusCode: http.StatusCreated},
		{name: "invalid create", method: http.MethodPost, path: "/items", body: `{"name":""}`, statusCode: http.StatusUnprocessableEntity},
		{name: "file catch all", method: http.MethodGet, path: "/files/docs/readme.txt", statusCode: http.StatusOK},
		{name: "method not allowed", method: http.MethodPatch, path: "/items", statusCode: http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := NewStore()
			_, err := store.Create("Existing")
			if err != nil {
				t.Fatalf("seed store: %v", err)
			}

			req := httptest.NewRequest(tt.method, tt.path, bytes.NewBufferString(tt.body))
			rec := httptest.NewRecorder()

			NewRouter(store).ServeHTTP(rec, req)

			res := rec.Result()
			defer res.Body.Close()

			if res.StatusCode != tt.statusCode {
				t.Fatalf("status = %d, want %d", res.StatusCode, tt.statusCode)
			}
		})
	}
}

func TestItemLifecycle(t *testing.T) {
	t.Parallel()

	store := NewStore()
	router := NewRouter(store)

	createReq := httptest.NewRequest(http.MethodPost, "/items", bytes.NewBufferString(`{"name":"Widget"}`))
	createRec := httptest.NewRecorder()
	router.ServeHTTP(createRec, createReq)

	if createRec.Result().StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", createRec.Result().StatusCode)
	}

	var item Item
	if err := json.NewDecoder(createRec.Result().Body).Decode(&item); err != nil {
		t.Fatalf("decode item: %v", err)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/items/"+item.ID, nil)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)

	if getRec.Result().StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", getRec.Result().StatusCode)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/items/"+item.ID, nil)
	deleteRec := httptest.NewRecorder()
	router.ServeHTTP(deleteRec, deleteReq)

	if deleteRec.Result().StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", deleteRec.Result().StatusCode)
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"log"
	"net/http"

	"example.com/routing"
)

func main() {
	store := routing.NewStore()
	if _, err := store.Create("Demo item"); err != nil {
		log.Fatal(err)
	}

	log.Println("listening on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", routing.NewRouter(store)))
}
```

## Common Mistakes

Using `mux.HandleFunc("/items", handler)` when the route must only accept GET allows every method. Use `GET /items`, `POST /items`, and other method-specific patterns.

Calling `r.PathValue("id")` when the registered pattern does not contain `{id}` returns an empty string. Keep wildcard names and handler code in sync.

Registering conflicting patterns panics. Prefer clear, non-overlapping patterns and rely on the documented specificity rules when overlap is intentional.

Putting the store in package-level variables makes tests influence each other. Pass a store into `NewRouter` so every test can use isolated state.

## Verification

Run these commands from the module root:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

`http.NewServeMux` supports method routing, single-segment wildcards, and catch-all wildcards. `Request.PathValue` reads matched path values. A library-level `NewRouter` function makes the router testable with `httptest`, and wrapped sentinel errors keep handler decisions explicit.

## What's Next

Next: [Middleware Chains](../04-middleware-chains/04-middleware-chains.md).

## Resources

- [net/http ServeMux](https://pkg.go.dev/net/http#ServeMux)
- [Request.PathValue](https://pkg.go.dev/net/http#Request.PathValue)
- [Go 1.22 enhanced routing patterns](https://go.dev/doc/go1.22#enhanced_routing_patterns)
- [httptest package](https://pkg.go.dev/net/http/httptest)
