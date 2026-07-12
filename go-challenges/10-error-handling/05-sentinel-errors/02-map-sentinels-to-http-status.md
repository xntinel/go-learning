# Exercise 2: Map Domain Sentinels To HTTP Status Codes

A domain sentinel means nothing to an HTTP client until something turns it into
a status code. This exercise builds that something: an `http.Handler` over a
small store where *every* error passes through one `classify(err)` function that
maps each sentinel to its status — and where a genuine internal error becomes a
500 with a generic body while its details go only to the log.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
api/                          independent module: example.com/api
  go.mod                      go 1.26
  api.go                      sentinels; Store; classify(err) (int,string); Handler
  cmd/
    demo/
      main.go                 drives each status path via httptest, prints codes
  api_test.go                 httptest table over statuses + 500-not-leaked test
```

- Files: `api.go`, `cmd/demo/main.go`, `api_test.go`.
- Implement: a `classify(err) (int, string)` seam mapping `ErrInvalidID`->400, `ErrNotFound`->404, `ErrPermission`->403, `ErrAlreadyExists`->409, default->500; a `Handler` that funnels all errors through it.
- Test: an `httptest` table asserting `rr.Code` per sentinel, plus a test that the 500 body never echoes the internal error text.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/05-sentinel-errors/02-map-sentinels-to-http-status/cmd/demo
cd go-solutions/10-error-handling/05-sentinel-errors/02-map-sentinels-to-http-status
```

### One seam, decided once

The reason to have a single `classify` function is that the sentinel-to-status
mapping is a policy, and policy scattered across handlers drifts:
`ErrNotFound` becomes 404 in one handler and 400 in another, and the API's
behavior stops being predictable. Concentrating it in one `switch` over
`errors.Is` means there is exactly one place to read, change, or test the
mapping. Handlers stay ignorant of HTTP status codes; they produce sentinels and
hand them to the seam.

The 500 path carries a second responsibility. When the error is *not* a known
domain sentinel — a disk write failed, a nil map — the client must not see the
raw message: it can leak internal paths, table names, or worse. So `classify`
returns a generic public string for the 500 case, and the handler logs the raw
error server-side and writes only the generic string to the response body. The
test pins that: the internal detail must appear in neither the body nor,
crucially, be the thing the client reads.

Create `api.go`:

```go
package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrInvalidID     = errors.New("invalid id")
	ErrPermission    = errors.New("permission denied")
)

// errDisk is an internal infrastructure error. It must never reach the client;
// classify maps it to a generic 500 and the handler logs it server-side only.
var errDisk = errors.New("disk write failed: /var/data busy")

// Record is a stored item owned by a caller.
type Record struct {
	ID    string
	Owner string
}

// Store is a minimal in-memory backend returning wrapped domain sentinels.
type Store struct {
	byID map[string]Record
}

func NewStore() *Store { return &Store{byID: make(map[string]Record)} }

func (s *Store) Create(rec Record) error {
	if rec.ID == "" {
		return fmt.Errorf("create: %w", ErrInvalidID)
	}
	if rec.ID == "boom" {
		return fmt.Errorf("create %q: %w", rec.ID, errDisk)
	}
	if _, ok := s.byID[rec.ID]; ok {
		return fmt.Errorf("create %q: %w", rec.ID, ErrAlreadyExists)
	}
	s.byID[rec.ID] = rec
	return nil
}

func (s *Store) Get(caller, id string) (Record, error) {
	if id == "" {
		return Record{}, fmt.Errorf("get %q: %w", id, ErrInvalidID)
	}
	rec, ok := s.byID[id]
	if !ok {
		return Record{}, fmt.Errorf("get %q: %w", id, ErrNotFound)
	}
	if rec.Owner != caller {
		return Record{}, fmt.Errorf("get %q: %w", id, ErrPermission)
	}
	return rec, nil
}

// classify is the single translation seam from domain sentinel to transport
// status. It returns the HTTP status and the public message safe to send to the
// client. The default case returns a generic message so internal errors never
// leak.
func classify(err error) (status int, public string) {
	switch {
	case errors.Is(err, ErrInvalidID):
		return http.StatusBadRequest, "invalid id"
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not found"
	case errors.Is(err, ErrPermission):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, ErrAlreadyExists):
		return http.StatusConflict, "already exists"
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

// Handler serves record create/read requests. id is a query param, owner an
// X-Owner header. Every error is funneled through fail -> classify.
type Handler struct {
	Store  *Store
	Logger *slog.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	caller := r.Header.Get("X-Owner")

	var err error
	switch r.Method {
	case http.MethodPost:
		err = h.Store.Create(Record{ID: id, Owner: caller})
	case http.MethodGet:
		_, err = h.Store.Get(caller, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	status, public := classify(err)
	if status >= http.StatusInternalServerError {
		// Log the raw error server-side; never echo it to the client.
		h.Logger.Error("request failed", "status", status, "err", err)
	}
	http.Error(w, public, status)
}
```

### The runnable demo

The demo drives the handler in-process with `httptest`, hitting each sentinel
path and printing the resulting status, then triggers the 500 path with the
special `boom` id.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"example.com/api"
)

func main() {
	h := &api.Handler{
		Store:  api.NewStore(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	call := func(method, target, owner string) {
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("X-Owner", owner)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		fmt.Printf("%s %s -> %d %s\n", method, target, rr.Code, http.StatusText(rr.Code))
	}

	call(http.MethodPost, "/records?id=u1", "alice")
	call(http.MethodPost, "/records?id=u1", "alice")
	call(http.MethodPost, "/records?id=", "alice")
	call(http.MethodGet, "/records?id=u1", "bob")
	call(http.MethodGet, "/records?id=missing", "alice")
	call(http.MethodPost, "/records?id=boom", "alice")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
POST /records?id=u1 -> 200 OK
POST /records?id=u1 -> 409 Conflict
POST /records?id= -> 400 Bad Request
GET /records?id=u1 -> 403 Forbidden
GET /records?id=missing -> 404 Not Found
POST /records?id=boom -> 500 Internal Server Error
```

### Tests

The table crafts one request per sentinel and asserts the mapped status.
`TestInternalErrorNotLeaked` is the one that protects a real production
invariant: the 500 response body must be the generic message, and must not
contain the internal error's text (`disk`, `/var/data`).

Create `api_test.go`:

```go
package api

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newHandler(setup func(*Store)) *Handler {
	s := NewStore()
	if setup != nil {
		setup(s)
	}
	return &Handler{Store: s, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestClassifyStatus(t *testing.T) {
	t.Parallel()

	seed := func(s *Store) { _ = s.Create(Record{ID: "x1", Owner: "alice"}) }

	tests := []struct {
		name   string
		method string
		id     string
		owner  string
		setup  func(*Store)
		want   int
	}{
		{"invalid id", http.MethodGet, "", "alice", nil, http.StatusBadRequest},
		{"not found", http.MethodGet, "missing", "alice", nil, http.StatusNotFound},
		{"forbidden", http.MethodGet, "x1", "bob", seed, http.StatusForbidden},
		{"conflict", http.MethodPost, "x1", "alice", seed, http.StatusConflict},
		{"internal", http.MethodPost, "boom", "alice", nil, http.StatusInternalServerError},
		{"ok", http.MethodGet, "x1", "alice", seed, http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHandler(tt.setup)
			req := httptest.NewRequest(tt.method, "/records?id="+tt.id, nil)
			req.Header.Set("X-Owner", tt.owner)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %q)", rr.Code, tt.want, rr.Body.String())
			}
		})
	}
}

func TestInternalErrorNotLeaked(t *testing.T) {
	t.Parallel()

	h := newHandler(nil)
	req := httptest.NewRequest(http.MethodPost, "/records?id=boom", nil)
	req.Header.Set("X-Owner", "alice")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "disk") || strings.Contains(body, "/var/data") {
		t.Fatalf("internal error leaked into body: %q", body)
	}
	if strings.TrimSpace(body) != "internal error" {
		t.Fatalf("body = %q, want generic message", body)
	}
}

func Example_classify() {
	status, public := classify(fmt.Errorf("get: %w", ErrNotFound))
	fmt.Println(status, public)
	// Output: 404 not found
}
```

## Review

The handler is correct when every response status is produced by `classify` and
nowhere else, so the mapping lives in one auditable place. The status table
proves each sentinel maps to its code through `%w` wrapping;
`TestInternalErrorNotLeaked` proves the 500 path is safe — the client sees
`internal error`, and `disk`/`/var/data` appear only in the (discarded here)
log. The trap to avoid is inlining `http.Error(w, err.Error(), ...)` in a
handler branch: it both scatters the policy and leaks internals. Keep every
error flowing through the one seam.

## Resources

- [`net/http` status constants](https://pkg.go.dev/net/http#pkg-constants) — `StatusNotFound`, `StatusConflict`, `StatusForbidden`, `StatusBadRequest`.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest` and `NewRecorder` for handler tests.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — the chain walk behind the classify switch.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-sentinel-repository.md](01-sentinel-repository.md) | Next: [03-translate-driver-sentinels.md](03-translate-driver-sentinels.md)
