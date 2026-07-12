# Exercise 2: Status-Code Matrix for an HTTP Handler

An HTTP endpoint has a contract that is really a matrix: this method on this path
with this content-type and this body produces that status code. Written as
one-off tests it sprawls; written as a table it is a single readable grid. This
module builds a `POST /users` create endpoint and drives its whole
200/201/400/404/405/415 matrix through `httptest`, with a fresh recorder per row.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
usersapi/                  independent module: example.com/usersapi
  go.mod                   go 1.26
  handler.go               Handler() http.Handler; POST /users create, GET /healthz
  cmd/
    demo/
      main.go              fires a few requests through the handler, prints codes
  handler_test.go          table over {name,method,path,ct,body,wantStatus,wantBody}
```

- Files: `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
- Implement: `Handler()` returning an `http.Handler` that creates a user on `POST /users` (201), health-checks on `GET /healthz` (200), rejects bad content-type (415), bad JSON and missing fields (400), unknown paths (404), and wrong methods (405).
- Test: one table whose rows are `{name, method, path, contentType, body, wantStatus, wantBodyContains}`, each driven with `httptest.NewRequest` + a fresh `httptest.NewRecorder`.
- Verify: `go test -count=1 -race ./...`

### Why httptest makes the matrix a table

`httptest.NewRequest(method, target, body)` builds a `*http.Request` with no
network, and `httptest.NewRecorder()` returns a recorder that captures the status,
headers, and body. Passing both to `handler.ServeHTTP(rec, req)` runs the handler
as a pure function: request in, `rec.Code` and `rec.Body` out. That is exactly the
shape a table wants — every row is an input request and an expected `(status,
body-substring)` pair, and the fresh recorder per row keeps the cases hermetic so
they can run in parallel.

The status codes come from two sources, and knowing which is which is the point.
The Go 1.22 enhanced `http.ServeMux` supplies 404 and 405 *for free*: register
`POST /users` and `GET /healthz`, and the mux answers an unknown path with 404 and
a known path hit with the wrong method with 405 (setting the `Allow` header). You
do not write those branches. What you *do* write are the 415 and 400 checks inside
the create handler: reject a request whose `Content-Type` is not
`application/json` with 415, reject an undecodable body or one missing required
fields with 400, and only then create the user and return 201. Splitting the
responsibility this way — routing errors from the mux, semantic errors from the
handler — is how real Go services are structured, and the table documents both
halves in one grid.

One subtlety the table pins: the 415 check must come *before* decoding the body,
because rejecting on content-type is cheaper and more correct than trying to parse
a body you have already decided you will not accept. The row ordering in the table
does not matter (cases are independent), but the *check* ordering in the handler
does, and the `wrong_content_type` row proves it fires.

Create `handler.go`:

```go
package usersapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
)

var nextID atomic.Int64

type createUserRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type createUserResponse struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Handler returns the API router. GET /healthz reports liveness; POST /users
// creates a user. Unknown paths and wrong methods are handled by the mux.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /users", createUser)
	return mux
}

func createUser(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "unsupported media type: want application/json", http.StatusUnsupportedMediaType)
		return
	}

	var req createUserRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Email == "" {
		http.Error(w, "name and email are required", http.StatusBadRequest)
		return
	}

	resp := createUserResponse{ID: nextID.Add(1), Name: req.Name, Email: req.Email}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}
```

### The runnable demo

The demo drives the same handler with in-memory requests and prints each status,
so you can watch the matrix without a browser or `curl`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http/httptest"
	"strings"

	"example.com/usersapi"
)

func main() {
	h := usersapi.Handler()

	type call struct {
		method, path, ct, body string
	}
	calls := []call{
		{"GET", "/healthz", "", ""},
		{"POST", "/users", "application/json", `{"name":"alice","email":"a@example.com"}`},
		{"POST", "/users", "application/json", `{"name":""}`},
		{"POST", "/users", "text/plain", `hello`},
		{"GET", "/users", "", ""},
		{"DELETE", "/missing", "", ""},
	}
	for _, c := range calls {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		if c.ct != "" {
			req.Header.Set("Content-Type", c.ct)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		fmt.Printf("%-6s %-10s -> %d\n", c.method, c.path, rec.Code)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET    /healthz   -> 200
POST   /users     -> 201
POST   /users     -> 400
POST   /users     -> 415
GET    /users     -> 405
DELETE /missing   -> 404
```

### The tests

Each row builds a request with `httptest.NewRequest`, sets the content-type when
the row supplies one, gets a *fresh* recorder, calls `ServeHTTP`, and asserts both
the status and a body substring. The substring assertion catches the difference
between two rejections that share a status: bad JSON and missing fields are both
400, but they say different things, and the table pins which. `t.Parallel` is safe
because no recorder or request is shared across rows.

Create `handler_test.go`:

```go
package usersapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateUserMatrix(t *testing.T) {
	t.Parallel()
	h := Handler()

	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		body        string
		wantStatus  int
		wantBody    string
	}{
		{"health_ok", "GET", "/healthz", "", "", http.StatusOK, "ok"},
		{"create_ok", "POST", "/users", "application/json", `{"name":"alice","email":"a@example.com"}`, http.StatusCreated, `"id":`},
		{"bad_json", "POST", "/users", "application/json", `{not json`, http.StatusBadRequest, "invalid json"},
		{"missing_fields", "POST", "/users", "application/json", `{"name":"alice"}`, http.StatusBadRequest, "required"},
		{"unknown_field", "POST", "/users", "application/json", `{"name":"a","email":"b","role":"admin"}`, http.StatusBadRequest, "invalid json"},
		{"wrong_content_type", "POST", "/users", "text/plain", `name=alice`, http.StatusUnsupportedMediaType, "unsupported media type"},
		{"wrong_method", "GET", "/users", "", "", http.StatusMethodNotAllowed, ""},
		{"unknown_path", "DELETE", "/missing", "", "", http.StatusNotFound, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("%s %s = %d, want %d (body %q)", tc.method, tc.path, rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("%s %s body = %q, want contains %q", tc.method, tc.path, rec.Body.String(), tc.wantBody)
			}
		})
	}
}
```

## Review

The handler is correct when each of the six status codes is produced by exactly
the request shape that should produce it, and the table is the readable proof.
The two failure modes to watch are shared state and check ordering. Reusing one
recorder across rows would make the cases order-dependent and break under
`t.Parallel`; building a fresh `httptest.NewRecorder()` inside each subtest is
non-negotiable. And the 415 check must precede body decoding, or a `text/plain`
request with malformed JSON would wrongly report 400 — the `wrong_content_type`
row is what keeps that ordering honest.

Note where each status originates: 404 and 405 come from the enhanced
`http.ServeMux` (Go 1.22+) matching path-then-method, not from code you wrote, so
the `wrong_method` and `unknown_path` rows are really testing that you registered
the routes with methods. 415, 400, and 201 come from the create handler. Run
`go test -race` to confirm the parallel rows share nothing through the handler.

## Resources

- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRequest`, `NewRecorder`, `ResponseRecorder`.
- [net/http.ServeMux](https://pkg.go.dev/net/http#ServeMux) — method-and-path patterns, 404 and 405 behavior.
- [Go 1.22 release notes: routing enhancements](https://go.dev/blog/routing-enhancements) — how the mux answers method mismatches with 405.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-request-validation-rules.md](03-request-validation-rules.md)
