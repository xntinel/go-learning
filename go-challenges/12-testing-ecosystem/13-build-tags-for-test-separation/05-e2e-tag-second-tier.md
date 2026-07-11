# Exercise 5: A third tier — end-to-end API smoke tests behind //go:build e2e

Integration is not the only slow tier. A repository test talks to one database;
an end-to-end smoke test drives a *deployed* service over real HTTP. This module
adds a second custom tag, `e2e`, distinct from `integration`, so a repo runs
three separate CI stages — unit, integration, e2e — and shows a boolean tag
expression (`e2e && !race`) selecting a precise slice of the matrix.

Self-contained module: an untagged HTTP client tested with `httptest`, an
untagged demo, and the tagged `e2e` smoke tests.

## What you'll build

```text
apismoke/                  independent module: example.com/apismoke
  go.mod
  client.go                Client.Health, Client.GetWidget over net/http; Widget JSON type
  client_test.go           untagged: httptest server exercises the client, ExampleClient_Health
  smoke_e2e_test.go        //go:build e2e && !race: real HTTP against BASE_URL, skips if unset
  cmd/
    demo/
      main.go              spins an httptest server and drives the client (deterministic)
```

- Files: `client.go`, `client_test.go`, `smoke_e2e_test.go`, `cmd/demo/main.go`.
- Implement: a `Client` with `Health` (GET `/healthz`, expect 200) and `GetWidget` (GET `/widgets/{id}`, decode JSON).
- Test: the untagged suite drives the client against `httptest.NewServer` (hermetic); `smoke_e2e_test.go` hits a live `BASE_URL` and skips when it is unset.
- Verify: `go test -race ./...` (default); `BASE_URL=... go test -tags=e2e ./...` (live, no `-race` by the `e2e && !race` constraint).

Set up the module:

```bash
mkdir -p ~/go-exercises/apismoke/cmd/demo
cd ~/go-exercises/apismoke
go mod init example.com/apismoke
go mod edit -go=1.26
```

### Three tiers, three tags, three stages

The default suite is hermetic and gates every PR. The `integration` tier from the
previous exercise talks to a real database in its own stage. The `e2e` tier is a
third, separately-tagged stage that assumes a fully deployed service reachable at
`BASE_URL` and checks that its public HTTP contract answers: `/healthz` returns
200, a resource endpoint returns a decodable body. Each tier is compiled and run
only under its own tag, with its own environment; none of them are in the default
build graph, so the fast PR gate never pays for any of them.

The `e2e` file carries a compound constraint, `//go:build e2e && !race`. The
`&& !race` half encodes a real operational decision: end-to-end tests drive a
remote process over the network, where the race detector — which instruments
*this* binary's memory accesses — buys nothing and only slows the run. `!race`
makes the file compile in the `e2e` tier but drop out automatically whenever the
build runs under `-race`, so a CI job that adds `-race` globally cannot
accidentally drag the e2e smoke tests under the detector. This is the boolean
grammar earning its place: one line targets "the e2e stage, but never the
race-instrumented one".

### The client, and why httptest makes the default tier hermetic

The `Client` is ordinary `net/http`: `http.NewRequestWithContext` so every call
honors a deadline, `Client.Do`, a `StatusCode` check, and a `json.Decoder` for
the body. The trick that keeps the *default* suite fast and offline is that the
same client is testable against `httptest.NewServer`, an in-process HTTP server
on a loopback address. The unit test wires up a handler that mimics the real
service's contract and drives the real client code against it — no tag, no
network, no deployed service. The `e2e` file reuses the identical client against
a real `BASE_URL`; only the target differs.

Create `client.go`:

```go
package apismoke

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Client is a thin HTTP client for a deployed service.
type Client struct {
	base string
	hc   *http.Client
}

// NewClient targets base (e.g. "http://localhost:8080"), using the default
// HTTP client. Callers control per-call deadlines via the request context.
func NewClient(base string) *Client {
	return &Client{base: base, hc: http.DefaultClient}
}

// Health GETs /healthz and returns nil only when it answers 200.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: status %d", resp.StatusCode)
	}
	return nil
}

// Widget is the decoded resource body.
type Widget struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// GetWidget GETs /widgets/{id} and decodes the JSON body.
func (c *Client) GetWidget(ctx context.Context, id int) (Widget, error) {
	url := fmt.Sprintf("%s/widgets/%d", c.base, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Widget{}, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return Widget{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Widget{}, fmt.Errorf("get widget %d: status %d", id, resp.StatusCode)
	}
	var w Widget
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
		return Widget{}, fmt.Errorf("decode widget: %w", err)
	}
	return w, nil
}
```

### The runnable demo

The demo stands up an in-process `httptest` server so it needs no deployed
service, giving a deterministic offline run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/apismoke"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/widgets/1":
			fmt.Fprint(w, `{"id":1,"name":"gadget"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := apismoke.NewClient(srv.URL)
	ctx := context.Background()

	if err := c.Health(ctx); err != nil {
		fmt.Println("health:", err)
		return
	}
	fmt.Println("health: ok")

	w, err := c.GetWidget(ctx, 1)
	if err != nil {
		fmt.Println("widget:", err)
		return
	}
	fmt.Printf("widget: %d %s\n", w.ID, w.Name)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
health: ok
widget: 1 gadget
```

### The untagged test

Create `client_test.go`:

```go
package apismoke

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/widgets/1":
			fmt.Fprint(w, `{"id":1,"name":"gadget"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestHealth(t *testing.T) {
	t.Parallel()
	srv := newTestServer()
	defer srv.Close()

	if err := NewClient(srv.URL).Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestGetWidget(t *testing.T) {
	t.Parallel()
	srv := newTestServer()
	defer srv.Close()

	w, err := NewClient(srv.URL).GetWidget(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetWidget: %v", err)
	}
	if w.ID != 1 || w.Name != "gadget" {
		t.Fatalf("GetWidget = %+v, want {1 gadget}", w)
	}
}

func TestGetWidgetMissing(t *testing.T) {
	t.Parallel()
	srv := newTestServer()
	defer srv.Close()

	if _, err := NewClient(srv.URL).GetWidget(context.Background(), 42); err == nil {
		t.Fatal("GetWidget(42) = nil error, want a 404 status error")
	}
}

func ExampleClient_Health() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fmt.Println(NewClient(srv.URL).Health(context.Background()))
	// Output: <nil>
}
```

### The tagged e2e tier

Create `smoke_e2e_test.go`:

```go
//go:build e2e && !race

package apismoke

import (
	"os"
	"testing"
)

// baseURL returns the deployed service URL, skipping the tier when unset so the
// e2e file stays build-safe (it compiles and vets) without a live endpoint.
func baseURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("BASE_URL")
	if u == "" {
		t.Skip("BASE_URL not set; skipping e2e smoke test")
	}
	return u
}

func TestHealthzE2E(t *testing.T) {
	c := NewClient(baseURL(t))
	if err := c.Health(t.Context()); err != nil {
		t.Fatalf("live /healthz: %v", err)
	}
}

func TestGetWidgetE2E(t *testing.T) {
	c := NewClient(baseURL(t))
	w, err := c.GetWidget(t.Context(), 1)
	if err != nil {
		t.Fatalf("live /widgets/1: %v", err)
	}
	if w.ID != 1 {
		t.Fatalf("widget id = %d, want 1", w.ID)
	}
}
```

## Review

The three tiers are proven distinct when `go test ./...` runs only `TestHealth`,
`TestGetWidget`, and `TestGetWidgetMissing`; `go test -tags=integration ./...`
adds no e2e tests (they carry `e2e`, not `integration`); and
`go test -tags=e2e ./...` compiles `smoke_e2e_test.go` and skips it cleanly
without `BASE_URL`. The `e2e && !race` constraint is doing real work: run
`go build -tags=e2e ./...` and it compiles the file, but `go test -tags=e2e -race ./...`
drops it, because `!race` is false under the detector — exactly the guard that
keeps network smoke tests out of a race-instrumented run. Vet each tier under its
own tag (`go vet -tags=e2e ./...`) so the tagged file cannot rot: the default
`go vet ./...` never sees it.

## Resources

- [net/http](https://pkg.go.dev/net/http) — `http.NewRequestWithContext`, `Client.Do`, `Response.StatusCode`.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — the in-process server that keeps the default tier hermetic.
- [go command: Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — boolean tag expressions like `e2e && !race`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-testmain-postgres-fixture.md](04-testmain-postgres-fixture.md) | Next: [06-driver-import-isolation.md](06-driver-import-isolation.md)
