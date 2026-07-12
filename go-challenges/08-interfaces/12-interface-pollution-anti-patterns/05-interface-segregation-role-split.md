# Exercise 5: Interface Segregation — Split a Fat Store into Reader and Writer

When two consumers use disjoint slices of a store, give each its own role
interface. This module refactors a fat concrete `Store` so an HTTP read handler
depends only on a `Reader` and a write handler depends only on a `Writer`. The
concrete type still implements everything; segregation controls what each handler
is allowed to reach — enforced least privilege as a compile error.

## What you'll build

```text
kvroles/                    independent module: example.com/kvroles
  go.mod                    go 1.26
  store.go                  concrete *Store (Get/Put/Delete/List/Len); ErrNotFound
  roles.go                  Reader{Get}; Writer{Put,Delete}; ReadWriter embeds both
  handlers.go              ReadHandler(Reader), WriteHandler(Writer); http.Handler
  cmd/
    demo/
      main.go               wires *Store into both handlers, exercises them
  handlers_test.go          role fakes; 200/404/500 table via httptest
```

- Files: `store.go`, `roles.go`, `handlers.go`, `cmd/demo/main.go`, `handlers_test.go`.
- Implement: a `Reader interface { Get }`, a `Writer interface { Put; Delete }`, a `ReadWriter interface { Reader; Writer }` by embedding, and a concrete `*Store` implementing all of them; a read handler whose constructor takes `Reader` and a write handler whose constructor takes `Writer`.
- Test: inject a fake implementing ONLY its role (the read fake has no `Put`); a `var _ Reader = (*Store)(nil)` compile-time assertion; a table covering 200, 404, and 500.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/12-interface-pollution-anti-patterns/05-interface-segregation-role-split/cmd/demo
cd go-solutions/08-interfaces/12-interface-pollution-anti-patterns/05-interface-segregation-role-split
```

### Why segregate, and how embedding composes roles

A single fat `DataStore` interface with ten methods forces every consumer to
depend on all ten. The read handler only reads, but if it takes a `DataStore` it
is nonetheless handed `Put` and `Delete` and could call them — a reviewer has to
notice that it does not. Interface segregation turns that review comment into a
compiler guarantee. Declare `Reader` with just `Get` and `Writer` with just `Put`
and `Delete`; give the read handler a `Reader`. Now the read handler physically
cannot call `Put`, because `Put` is not a method of its dependency's type. Least
privilege is enforced by the type system.

Roles compose by embedding: `type ReadWriter interface { Reader; Writer }` is the
union, for the occasional consumer that genuinely needs both. The concrete
`*Store` implements every method, so it satisfies `Reader`, `Writer`, and
`ReadWriter` at once — segregation is about the consumer's view, not about
fragmenting the implementation. The compile-time assertions
`var _ Reader = (*Store)(nil)` and `var _ Writer = (*Store)(nil)` pin that the
one concrete type still satisfies both roles; if a future edit breaks that, the
package fails to compile rather than failing at runtime when a handler is wired.

The handlers translate store outcomes to HTTP status. The read handler maps a
missing key (`errors.Is(err, ErrNotFound)`) to 404, any other error to 500, and a
hit to 200 with the value. That three-way branch is exactly the 200/404/500 the
table test drives, and injecting a fake `Reader` that returns each outcome is how
you cover all three paths without a real store.

Create `store.go`:

```go
package kvroles

import (
	"context"
	"errors"
	"sync"
)

// ErrNotFound is returned by Get when a key is absent.
var ErrNotFound = errors.New("kvroles: key not found")

// Store is the concrete, fat implementation: it has more methods than any single
// handler needs. Each handler depends on a narrow role interface instead.
type Store struct {
	mu   sync.RWMutex
	data map[string]string
}

func NewStore() *Store {
	return &Store{data: make(map[string]string)}
}

func (s *Store) Get(ctx context.Context, key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (s *Store) Put(ctx context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

func (s *Store) List(ctx context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.data))
	for k := range s.data {
		out = append(out, k)
	}
	return out, nil
}

func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}
```

Create `roles.go`:

```go
package kvroles

import "context"

// Reader is the read role: exactly the method a read handler calls.
type Reader interface {
	Get(ctx context.Context, key string) (string, error)
}

// Writer is the write role: exactly the methods a write handler calls.
type Writer interface {
	Put(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) error
}

// ReadWriter composes the two roles by embedding, for a consumer that needs both.
type ReadWriter interface {
	Reader
	Writer
}

// Compile-time proof that the one concrete *Store satisfies every role. If a
// future edit breaks any of these, the package fails to build.
var (
	_ Reader     = (*Store)(nil)
	_ Writer     = (*Store)(nil)
	_ ReadWriter = (*Store)(nil)
)
```

Create `handlers.go`:

```go
package kvroles

import (
	"errors"
	"io"
	"net/http"
)

// ReadHandler serves GET /kv?key=... . It depends only on Reader, so it cannot
// call Put or Delete even by mistake.
type ReadHandler struct {
	store Reader
}

func NewReadHandler(store Reader) *ReadHandler {
	return &ReadHandler{store: store}
}

func (h *ReadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	v, err := h.store.Get(r.Context(), key)
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, v)
	}
}

// WriteHandler serves PUT and DELETE. It depends only on Writer.
type WriteHandler struct {
	store Writer
}

func NewWriteHandler(store Writer) *WriteHandler {
	return &WriteHandler{store: store}
}

func (h *WriteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if err := h.store.Put(r.Context(), key, string(body)); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := h.store.Delete(r.Context(), key); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
```

### The runnable demo

The demo wires the one `*Store` into both handlers — the read handler sees it as
a `Reader`, the write handler as a `Writer` — and drives them with `httptest`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"example.com/kvroles"
)

func main() {
	store := kvroles.NewStore()
	read := kvroles.NewReadHandler(store)
	write := kvroles.NewWriteHandler(store)

	// PUT key=greeting body=hello
	req := httptest.NewRequest(http.MethodPut, "/kv?key=greeting", strings.NewReader("hello"))
	rec := httptest.NewRecorder()
	write.ServeHTTP(rec, req)
	fmt.Printf("PUT status: %d\n", rec.Code)

	// GET the key back
	req = httptest.NewRequest(http.MethodGet, "/kv?key=greeting", nil)
	rec = httptest.NewRecorder()
	read.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	fmt.Printf("GET status: %d body: %s\n", rec.Code, body)

	// GET a missing key
	req = httptest.NewRequest(http.MethodGet, "/kv?key=absent", nil)
	rec = httptest.NewRecorder()
	read.ServeHTTP(rec, req)
	fmt.Printf("GET absent status: %d\n", rec.Code)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
PUT status: 204
GET status: 200 body: hello
GET absent status: 404
```

### Tests

The read handler test injects `fakeReader`, which implements ONLY `Get` — it has
no `Put` method, so the handler could not call one even if it tried. That fake
returns a hit, `ErrNotFound`, or an arbitrary error to drive the 200/404/500
branches. The write handler test injects `fakeWriter`. `ExampleStore_Get` pins the
concrete store's output so `go test` verifies the snippet too.

Create `handlers_test.go`:

```go
package kvroles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeReader implements ONLY the Reader role. It deliberately has no Put/Delete,
// which is the point: a read handler wired to a Reader cannot write.
type fakeReader struct {
	value string
	err   error
}

func (f fakeReader) Get(ctx context.Context, key string) (string, error) {
	return f.value, f.err
}

func TestReadHandlerStatus(t *testing.T) {
	t.Parallel()

	boom := errors.New("db connection lost")

	cases := []struct {
		name       string
		reader     fakeReader
		wantStatus int
		wantBody   string
	}{
		{name: "hit 200", reader: fakeReader{value: "alice"}, wantStatus: http.StatusOK, wantBody: "alice"},
		{name: "missing 404", reader: fakeReader{err: ErrNotFound}, wantStatus: http.StatusNotFound},
		{name: "error 500", reader: fakeReader{err: boom}, wantStatus: http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := NewReadHandler(tc.reader)

			req := httptest.NewRequest(http.MethodGet, "/kv?key=u1", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantBody != "" {
				body, _ := io.ReadAll(rec.Result().Body)
				if string(body) != tc.wantBody {
					t.Fatalf("body = %q, want %q", body, tc.wantBody)
				}
			}
		})
	}
}

// fakeWriter implements ONLY the Writer role.
type fakeWriter struct {
	putErr error
	delErr error

	lastKey   string
	lastValue string
}

func (f *fakeWriter) Put(ctx context.Context, key, value string) error {
	f.lastKey, f.lastValue = key, value
	return f.putErr
}

func (f *fakeWriter) Delete(ctx context.Context, key string) error {
	f.lastKey = key
	return f.delErr
}

func TestWriteHandlerPut(t *testing.T) {
	t.Parallel()

	fw := &fakeWriter{}
	h := NewWriteHandler(fw)

	req := httptest.NewRequest(http.MethodPut, "/kv?key=greeting", strings.NewReader("hello"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if fw.lastKey != "greeting" || fw.lastValue != "hello" {
		t.Fatalf("stored (%q,%q), want (greeting,hello)", fw.lastKey, fw.lastValue)
	}
}

func TestWriteHandlerPutError(t *testing.T) {
	t.Parallel()

	fw := &fakeWriter{putErr: errors.New("disk full")}
	h := NewWriteHandler(fw)

	req := httptest.NewRequest(http.MethodPut, "/kv?key=k", strings.NewReader("v"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestConcreteStoreSatisfiesRoles mirrors the compile-time assertions at runtime.
func TestConcreteStoreSatisfiesRoles(t *testing.T) {
	t.Parallel()
	var s any = NewStore()
	if _, ok := s.(Reader); !ok {
		t.Fatal("*Store does not satisfy Reader")
	}
	if _, ok := s.(Writer); !ok {
		t.Fatal("*Store does not satisfy Writer")
	}
	if _, ok := s.(ReadWriter); !ok {
		t.Fatal("*Store does not satisfy ReadWriter")
	}
}

// ExampleStore_Get shows the concrete store, viewed as a Reader by a handler,
// returning a stored value; the // Output line is auto-verified by `go test`.
func ExampleStore_Get() {
	ctx := context.Background()
	s := NewStore()
	_ = s.Put(ctx, "greeting", "hello")
	v, _ := s.Get(ctx, "greeting")
	fmt.Println(v)
	// Output: hello
}
```

## Review

Segregation moved a review comment into the compiler. The read handler takes a
`Reader`, so "does this handler write?" is answered by the type: it cannot, there
is no `Put` in its dependency. The concrete `*Store` still implements everything,
and the `var _ Reader = (*Store)(nil)` block guarantees that stays true or the
build breaks. Two things to keep honest: keep the role interfaces in the package
that defines the handlers (they are consumer-side), and resist adding a method to
`Reader` just because `*Store` grows one — the role interface should track what
the handler calls, not what the store offers. The 200/404/500 table is the
behavioral proof that the handler's three-way branch maps store outcomes to HTTP
status correctly.

## Resources

- [Dave Cheney — SOLID Go Design](https://dave.cheney.net/2016/08/20/solid-go-design) — the Interface Segregation Principle in Go, with `io.Reader`/`io.Writer` as the model.
- [Effective Go — Interfaces and embedding](https://go.dev/doc/effective_go#embedding) — how interface embedding composes roles.
- [net/http — Handler](https://pkg.go.dev/net/http#Handler) — the `ServeHTTP` contract the handlers implement.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-consumer-defined-narrow-interface.md](04-consumer-defined-narrow-interface.md) | Next: [06-prefer-stdlib-io-interface.md](06-prefer-stdlib-io-interface.md)
