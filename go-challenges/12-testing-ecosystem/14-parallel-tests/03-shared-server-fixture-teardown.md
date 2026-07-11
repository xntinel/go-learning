# Exercise 3: One Expensive httptest Backend Shared Across Parallel Subtests

Standing up a backend for tests is expensive — a migrated database, a seeded
server, a warmed cache. You want to build it once and let many parallel subtests
hit it read-only, then tear it down exactly once after all of them finish. Do
this wrong with a parent-level `defer` and the fixture is destroyed while the
paused subtests are still queued. This module builds an API client against one
shared `httptest.Server` and tears it down correctly.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
sharedfixture/              independent module: example.com/sharedfixture
  go.mod
  client.go                 Client over an injected base URL; GetProduct
  cmd/
    demo/
      main.go               runnable demo against an httptest.Server
  client_test.go            one shared server, parallel subtests, single teardown
```

Files: `client.go`, `cmd/demo/main.go`, `client_test.go`.
Implement: a `Client` with a base URL and `GetProduct(ctx, id) (Product, error)`
issuing a GET and decoding JSON.
Test: build one `httptest.Server` in a parent test, register `t.Cleanup` to
close it, run read-only subtests in parallel inside a `t.Run("group", ...)`, and
prove Close runs exactly once, after all children.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/sharedfixture/cmd/demo
cd ~/go-exercises/sharedfixture
go mod init example.com/sharedfixture
```

### Why the server is a shared read-only fixture

An `httptest.Server` binds a real ephemeral port and serves real HTTP over
loopback. Building one per subtest is wasteful when the subtests only *read* — a
GET against a fixed catalog does not mutate anything, so the same server can serve
all of them concurrently. `net/http.Server` handles each request on its own
goroutine, so concurrent reads from parallel subtests are safe as long as the
handler itself does not mutate shared state (ours serves from an immutable map).
Sharing the server turns N port binds and N handler setups into one, which is the
whole economic argument for the pattern.

The correctness hazard is teardown timing. Recall the two-phase model: when a
subtest calls `t.Parallel()`, it pauses and control returns to the parent. If the
parent then falls off the end of its function body, any `defer srv.Close()` there
fires *now* — before the paused subtests resume. The subtests then dial a closed
server and fail with connection-refused flakes that look like the server "randomly"
died. The `t.Run("group", ...)` wrapper fixes the timing: `t.Run` does not return
until its subtests — including paused parallel ones — complete, so code after the
group, and `t.Cleanup` registered before it, runs only after every child is done.

Here is the broken shape, for contrast only (do not use it):

```go
func TestBroken(t *testing.T) {
	srv := httptest.NewServer(handler())
	defer srv.Close() // fires when TestBroken returns, BEFORE the paused
	                   // subtests resume: they hit a closed server.
	t.Run("a", func(t *testing.T) { t.Parallel(); /* dials srv */ })
	t.Run("b", func(t *testing.T) { t.Parallel(); /* dials srv */ })
}
```

Create `client.go`:

```go
package sharedfixture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Product is the JSON shape returned by GET /products/{id}.
type Product struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Price int    `json:"price"`
}

// Client talks to a product API at BaseURL. BaseURL is injected so the same
// client is used verbatim in tests (pointing at an httptest.Server) and in
// production (pointing at the real service).
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a Client using the default http.Client.
func NewClient(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: http.DefaultClient}
}

// GetProduct fetches one product by id.
func (c *Client) GetProduct(ctx context.Context, id string) (Product, error) {
	url := c.BaseURL + "/products/" + id
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Product{}, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Product{}, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Product{}, fmt.Errorf("get product %s: status %d", id, resp.StatusCode)
	}
	var p Product
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return Product{}, fmt.Errorf("decode product: %w", err)
	}
	return p, nil
}
```

### The runnable demo

The demo builds a throwaway `httptest.Server` in `main`, points a client at it,
and fetches one product — the exact wiring the tests use, in miniature.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/sharedfixture"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(sharedfixture.Product{ID: "42", Name: "Widget", Price: 999})
	}))
	defer srv.Close()

	c := sharedfixture.NewClient(srv.URL)
	p, err := c.GetProduct(context.Background(), "42")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%s costs %d cents\n", p.Name, p.Price)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Widget costs 999 cents
```

### Tests

The parent test builds the catalog and one server, then wires teardown so it is
provably single and provably last. Two cleanups are registered: an assertion
cleanup *first* (LIFO makes it run *last*) that checks the close count, and the
close cleanup *second* (runs *first*) that increments an atomic counter and closes
the server. The parallel subtests run inside `t.Run("group", ...)`; they all
complete before the parent function returns, so both cleanups fire after every
child, close-then-assert, and the assertion sees exactly one close.

Create `client_test.go`:

```go
package sharedfixture

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// newCatalogServer builds the expensive shared fixture: one server serving an
// immutable product catalog. Concurrent reads are safe.
func newCatalogServer(catalog map[string]Product) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /products/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := catalog[r.PathValue("id")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(p)
	})
	return httptest.NewServer(mux)
}

func TestSharedServer(t *testing.T) {
	catalog := map[string]Product{
		"1": {ID: "1", Name: "Widget", Price: 100},
		"2": {ID: "2", Name: "Gadget", Price: 250},
		"3": {ID: "3", Name: "Gizmo", Price: 375},
	}

	var closeCount atomic.Int32

	// Registered FIRST -> runs LAST (LIFO): after the close cleanup below, so it
	// observes the final count.
	t.Cleanup(func() {
		if got := closeCount.Load(); got != 1 {
			t.Errorf("server closed %d times, want exactly 1", got)
		}
	})

	srv := newCatalogServer(catalog)
	// Registered SECOND -> runs FIRST: closes the shared server exactly once,
	// after all parallel subtests have finished (they run before this cleanup).
	t.Cleanup(func() {
		closeCount.Add(1)
		srv.Close()
	})

	client := NewClient(srv.URL)

	// The Run-group: control does not pass this call until every parallel
	// subtest below completes.
	t.Run("group", func(t *testing.T) {
		for id, want := range catalog {
			t.Run("product_"+id, func(t *testing.T) {
				t.Parallel() // overlaps siblings; all share the one server
				got, err := client.GetProduct(t.Context(), id)
				if err != nil {
					t.Fatalf("GetProduct(%s): %v", id, err)
				}
				if got != want {
					t.Fatalf("GetProduct(%s) = %+v, want %+v", id, got, want)
				}
			})
		}
	})

	// Reaching here proves all subtests finished while the server was still up.
	if closeCount.Load() != 0 {
		t.Fatalf("server closed before subtests finished: count=%d", closeCount.Load())
	}
}

func TestMissingProduct(t *testing.T) {
	t.Parallel()

	srv := newCatalogServer(map[string]Product{})
	t.Cleanup(srv.Close)

	_, err := NewClient(srv.URL).GetProduct(context.Background(), "nope")
	if err == nil {
		t.Fatal("GetProduct on empty catalog: want error, got nil")
	}
}
```

## Review

The fixture is shared correctly when the server is built once, all parallel
subtests read it concurrently, and it is closed exactly once after they all
finish. The proof is threefold: the two cleanups run close-then-assert by LIFO, so
the assertion sees `closeCount == 1`; the post-group check confirms
`closeCount == 0` while subtests were running, meaning nothing closed early; and
the subtests themselves pass, meaning they hit a live server. Swap the `t.Cleanup`
close for a parent-level `defer srv.Close()` and the subtests start failing with
connection-refused, because the defer fires when the parent returns — before the
paused children resume.

The reusable rule: teardown of anything shared with parallel children goes in
`t.Cleanup` or after a `t.Run("group", ...)`, never in a parent-level `defer`.
`defer` is for cleanup local to one non-parent test.

## Resources

- [`net/http/httptest.NewServer`](https://pkg.go.dev/net/http/httptest#NewServer) — the loopback test server and its `Close`.
- [Using Subtests and Sub-benchmarks](https://go.dev/blog/subtests) — the Run-group teardown pattern for parallel subtests.
- [`testing.T.Cleanup`](https://pkg.go.dev/testing#T.Cleanup) — cleanup that runs after a test and all its subtests complete.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-parallel-table-http-validation.md](02-parallel-table-http-validation.md) | Next: [04-config-loader-setenv-vs-parallel.md](04-config-loader-setenv-vs-parallel.md)
