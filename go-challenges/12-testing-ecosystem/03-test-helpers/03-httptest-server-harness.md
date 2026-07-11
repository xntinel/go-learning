# Exercise 3: An HTTP Handler Test Harness with t.Cleanup

Testing an `http.Handler` end-to-end means standing up a server, wiring a client
to it, and tearing both down without leaking a listener. A harness helper does all
three so each test body reads as three declarative lines: build the server, make a
request, assert the response. The key discipline is that the helper registers
`srv.Close` via `t.Cleanup`, so no caller can forget to release the port.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
apiharness/                  independent module: example.com/apiharness
  go.mod                     go 1.26
  api.go                     an in-memory user API: GET/POST /users on a ServeMux
  cmd/
    demo/
      main.go                starts the server, issues a couple of requests
  api_test.go                newTestServer/assertStatus/decodeJSON helpers; 200/400/404 subtests
```

- Files: `api.go`, `cmd/demo/main.go`, `api_test.go`.
- Implement: a small JSON API (`GET /users/{id}` → 200 or 404, `POST /users` → 201 or 400) served by an `http.Handler`.
- Test: `newTestServer(t)` mounts the handler on `httptest.NewServer`, registers `srv.Close` via `t.Cleanup`, and returns a client; `assertStatus` and `decodeJSON` helpers keep each subtest to a few lines.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/apiharness/cmd/demo
cd ~/go-exercises/apiharness
go mod init example.com/apiharness
```

### The handler and why `httptest.NewServer`

The API is an in-memory user store behind a `net/http.ServeMux` using the Go 1.22
method-and-wildcard pattern syntax (`GET /users/{id}`, `POST /users`). `GET`
returns the user as JSON with 200, or 404 with a JSON error if unknown. `POST`
decodes a JSON body, rejects an empty name with 400, and otherwise stores the user
and returns 201.

`httptest.NewServer(handler)` starts this handler on a real loopback listener at a
*random* port and returns a `*httptest.Server`. Two properties make it the
canonical choice. First, the random port means many servers (across parallel
tests) never collide — a fixed `:8080` would. Second, `srv.Client()` returns an
`*http.Client` already configured to reach `srv.URL`, so you exercise the real
HTTP path (routing, status codes, header handling, body encoding) rather than
calling the handler function directly. The cost is a real listener and real
goroutines, which is exactly why the harness must register `srv.Close` — otherwise
each test leaks a listener and the suite eventually exhausts file descriptors
under `-count=N`.

Create `api.go`:

```go
package apiharness

import (
	"encoding/json"
	"net/http"
	"sync"
)

// User is the resource the API serves.
type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// API is an in-memory user store exposed over HTTP.
type API struct {
	mu    sync.Mutex
	users map[string]User
}

// NewAPI returns an API seeded with the given users.
func NewAPI(seed ...User) *API {
	a := &API{users: make(map[string]User)}
	for _, u := range seed {
		a.users[u.ID] = u
	}
	return a
}

// Handler returns the routed http.Handler for the API.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", a.getUser)
	mux.HandleFunc("POST /users", a.createUser)
	return mux
}

func (a *API) getUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.mu.Lock()
	u, ok := a.users[id]
	a.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request) {
	var u User
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if u.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
		return
	}
	a.mu.Lock()
	a.users[u.ID] = u
	a.mu.Unlock()
	writeJSON(w, http.StatusCreated, u)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
```

### The runnable demo

The demo starts the server itself, issues a GET for a seeded user and a GET for an
unknown one, and prints the status codes it observes against the wall clock.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strings"

	"example.com/apiharness"
)

func main() {
	api := apiharness.NewAPI(apiharness.User{ID: "1", Name: "alice"})
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	client := srv.Client()

	get, _ := client.Get(srv.URL + "/users/1")
	fmt.Printf("GET /users/1 -> %d\n", get.StatusCode)
	get.Body.Close()

	miss, _ := client.Get(srv.URL + "/users/999")
	fmt.Printf("GET /users/999 -> %d\n", miss.StatusCode)
	miss.Body.Close()

	bad, _ := client.Post(srv.URL+"/users", "application/json", strings.NewReader(`{"id":"2"}`))
	body, _ := io.ReadAll(bad.Body)
	bad.Body.Close()
	fmt.Printf("POST /users (no name) -> %d %s", bad.StatusCode, body)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /users/1 -> 200
GET /users/999 -> 404
POST /users (no name) -> 400 {"error":"name required"}
```

### The tests

`newTestServer(t)` is the fixture helper: it builds the API, mounts it on
`httptest.NewServer`, registers `srv.Close` via `t.Cleanup`, and returns the
server and its client. Because teardown is owned by the helper, no test body has a
`defer` to forget. `assertStatus(t, resp, want)` and `decodeJSON(t, resp, &v)` are
assertion helpers calling `t.Helper()`, so a status mismatch points at the subtest
line. The subtests exercise the 200, 404, and 400 paths through the wired client.

Create `api_test.go`:

```go
package apiharness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer mounts the API on an httptest.Server and owns its teardown.
func newTestServer(t *testing.T, seed ...User) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(NewAPI(seed...).Handler())
	t.Cleanup(srv.Close)
	return srv, srv.Client()
}

// assertStatus fails the test unless resp has the wanted status code.
func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d", resp.StatusCode, want)
	}
}

// decodeJSON decodes resp.Body into v, failing the test on error.
func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

func TestGetUserFound(t *testing.T) {
	t.Parallel()
	srv, client := newTestServer(t, User{ID: "1", Name: "alice"})

	resp, err := client.Get(srv.URL + "/users/1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	assertStatus(t, resp, http.StatusOK)
	var got User
	decodeJSON(t, resp, &got)
	if got.Name != "alice" {
		t.Fatalf("name = %q, want alice", got.Name)
	}
}

func TestGetUserNotFound(t *testing.T) {
	t.Parallel()
	srv, client := newTestServer(t)

	resp, err := client.Get(srv.URL + "/users/missing")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	assertStatus(t, resp, http.StatusNotFound)
}

func TestCreateUserBadRequest(t *testing.T) {
	t.Parallel()
	srv, client := newTestServer(t)

	resp, err := client.Post(srv.URL+"/users", "application/json", strings.NewReader(`{"id":"2"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	assertStatus(t, resp, http.StatusBadRequest)
}

func TestCreateUserCreated(t *testing.T) {
	t.Parallel()
	srv, client := newTestServer(t)

	resp, err := client.Post(srv.URL+"/users", "application/json", strings.NewReader(`{"id":"2","name":"bob"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	assertStatus(t, resp, http.StatusCreated)
	var got User
	decodeJSON(t, resp, &got)
	if got.ID != "2" || got.Name != "bob" {
		t.Fatalf("created = %+v, want {2 bob}", got)
	}
}
```

## Review

The harness is correct when every subtest reaches the real handler through
`srv.Client()` and the listener is released by `t.Cleanup` rather than a caller's
`defer`. The tell that the helper owns lifecycle is that no test body closes the
server — only the response bodies. Because each `newTestServer` builds a fresh API
with its own seed, the four subtests run in parallel without cross-contamination:
`TestCreateUserCreated` mutating its store cannot be seen by `TestGetUserFound`.
Run `go test -race -count=1`: `-race` would flag a shared mutable store, and
`-count=1` defeats caching so the listeners are actually opened and closed each
run. The mistake to avoid is returning a `cleanup func` for the caller to defer;
one forgotten defer under `-count=N` leaks listeners until the process runs out of
file descriptors.

## Resources

- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewServer`, `Server.URL`, `Server.Client`, `Server.Close`.
- [testing.T.Cleanup](https://pkg.go.dev/testing#T.Cleanup) — LIFO teardown registered at acquisition.
- [net/http.ServeMux](https://pkg.go.dev/net/http#ServeMux) — the Go 1.22 method-and-wildcard routing patterns.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-tempdir-repository-fixture.md](04-tempdir-repository-fixture.md)
