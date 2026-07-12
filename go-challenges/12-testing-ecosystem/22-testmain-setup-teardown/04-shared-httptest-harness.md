# Exercise 4: One httptest.Server for the Whole Package, Started and Closed in TestMain

When a package has dozens of client-side tests against an HTTP API, starting a
fresh `httptest.Server` in each one wastes time and port churn. The idiomatic
harness starts one server in `TestMain`, wrapping the *real* production handler,
publishes its base URL through a package var, and closes it in teardown. Every
test then issues requests against the one shared server.

This module is fully self-contained: its own `go mod init`, handler, demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
apiharness/                    independent module: example.com/apiharness
  go.mod                       go 1.26
  api.go                       NewHandler: GET /healthz and GET /widgets/{id}
  cmd/
    demo/
      main.go                  runnable demo: start a server, hit both routes
  api_test.go                  TestMain starts one server; tests share its URL
```

Files: `api.go`, `cmd/demo/main.go`, `api_test.go`.
Implement: `NewHandler() http.Handler` — a stateless router serving `GET /healthz` and `GET /widgets/{id}`.
Test: a `TestMain`/`run()` that starts one `httptest.NewServer(NewHandler())`, stores `srv.URL` in a package var, and closes it in teardown; tests build requests with `http.NewRequestWithContext(t.Context(), ...)` against that URL.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/22-testmain-setup-teardown/04-shared-httptest-harness/cmd/demo
cd go-solutions/12-testing-ecosystem/22-testmain-setup-teardown/04-shared-httptest-harness
```

### Why one server, and why it must be stateless to share safely

`httptest.NewServer` starts a real HTTP server on a real loopback port and returns
its base URL. Starting and stopping it is the expensive part; the handler itself
is cheap. So we pay the start cost once in `TestMain`, publish `srv.URL`, and reuse
it across all tests — including tests marked `t.Parallel()`, which will hit the
one server concurrently.

Concurrency against a single shared server is only safe if the handler is
stateless (or internally synchronized). This exercise's handler is pure: `/healthz`
returns a constant, and `/widgets/{id}` derives its response entirely from the
path parameter with no shared mutable state. That is why parallel tests are safe
here. The moment a handler keeps a shared in-memory store, parallel tests must
isolate their data (unique ids per test) or the shared server stops being safe to
hammer — the same isolation rule as the shared database in the previous exercise.

### Wrapping the real handler, not a test double

The point of the harness is that tests exercise the *production* `http.Handler`,
built by the same `NewHandler` that `main` would call. `TestMain` does not
reimplement routing; it wraps `NewHandler()` in `httptest.NewServer` so the tests
cover real routing, real status codes, and real serialization. `srv.URL` is the
scheme+host+port of the running server; tests append the path.

Create `api.go`. It uses Go 1.22 method+path routing patterns:

```go
package apiharness

import (
	"encoding/json"
	"net/http"
)

// Widget is the resource returned by GET /widgets/{id}.
type Widget struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// NewHandler builds the production router. It is stateless, so a single instance
// is safe to serve concurrent requests.
func NewHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /widgets/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(Widget{ID: id, Name: "widget-" + id})
	})

	return mux
}
```

### The runnable demo

The demo starts its own short-lived server (the same pattern `TestMain` uses) and
hits both routes so you can see real responses.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/apiharness"
)

func main() {
	srv := httptest.NewServer(apiharness.NewHandler())
	defer srv.Close()

	for _, path := range []string{"/healthz", "/widgets/42"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("%s -> %d %s\n", path, resp.StatusCode, body)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/healthz -> 200 ok
/widgets/42 -> 200 {"id":"42","name":"widget-42"}
```

### Tests

`TestMain` starts one server via `run()` (so `srv.Close` is deferred where it
actually executes) and publishes `baseURL`. Tests build requests with
`http.NewRequestWithContext(t.Context(), ...)` — carrying the test's context so a
cancelled/timed-out test cancels its request — and assert status and body.

Create `api_test.go`:

```go
package apiharness

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// baseURL is the shared server's URL, published by run().
var baseURL string

func run(m *testing.M) int {
	srv := httptest.NewServer(NewHandler())
	defer srv.Close()
	baseURL = srv.URL
	return m.Run()
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(body)
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	resp, body := get(t, "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
}

func TestGetWidget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		id   string
	}{
		{"numeric", "42"},
		{"alpha", "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, body := get(t, "/widgets/"+tc.id)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var w Widget
			if err := json.Unmarshal([]byte(body), &w); err != nil {
				t.Fatalf("decode: %v (body %q)", err, body)
			}
			if w.ID != tc.id || w.Name != "widget-"+tc.id {
				t.Fatalf("widget = %+v, want id=%s name=widget-%s", w, tc.id, tc.id)
			}
		})
	}
}

func TestUnknownRoute(t *testing.T) {
	t.Parallel()
	resp, _ := get(t, "/nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestContentTypeJSON(t *testing.T) {
	t.Parallel()
	resp, _ := get(t, "/widgets/7")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}
```

## Review

The harness is correct when exactly one server is started — in `TestMain`, via a
`run()` wrapper so `srv.Close()` actually runs — and every test reuses `baseURL`
instead of starting its own server. The server wraps the real `NewHandler()`, so
the tests cover production routing and serialization, not a stand-in. Parallel
tests are safe here only because the handler is stateless; the `TestGetWidget`
subtests run in parallel against the shared server precisely because each derives
its response from its own path. The two mistakes to avoid: starting a server per
test (slow, and defeats the point of `TestMain`), and hammering a *stateful*
shared handler from parallel tests without per-test isolation. Run `go test -race`
to confirm the concurrent requests do not race.

## Resources

- [`net/http/httptest.NewServer`](https://pkg.go.dev/net/http/httptest#NewServer) — starting a real loopback server and its `URL`/`Close`.
- [`net/http.ServeMux` method patterns](https://pkg.go.dev/net/http#ServeMux) — the Go 1.22 `GET /path/{id}` routing and `Request.PathValue`.
- [`http.NewRequestWithContext`](https://pkg.go.dev/net/http#NewRequestWithContext) — attaching the test's context to outgoing requests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-shared-postgres-pool-and-migrations.md](03-shared-postgres-pool-and-migrations.md) | Next: [05-flag-parse-integration-gate.md](05-flag-parse-integration-gate.md)
