# Exercise 4: LIFO Teardown of a Layered Integration Harness

An integration fixture is a stack of resources: a backing store, a server over
that store, a client configured for that server. They must tear down in reverse
build order — client drains, then server closes, then store frees — or a closing
layer will yank the ground out from under a layer still using it. `t.Cleanup`'s
last-in first-out ordering gives you that reverse order for free, and this exercise
proves it by recording the actual teardown sequence.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
lifoharness/                 independent module: example.com/lifoharness
  go.mod                     go 1.24
  harness.go                 Store (Get/Close) + Handler over the store
  cmd/
    demo/
      main.go                runnable demo: build store -> server -> client, query
  harness_test.go            layered fixture recording teardown order; asserts LIFO
```

- Files: `harness.go`, `cmd/demo/main.go`, `harness_test.go`.
- Implement: a seeded `Store` and an `http.Handler` that serves values from it.
- Test: a fixture that builds store, then an `httptest.Server`, then a client, registering a `t.Cleanup` at each step that appends its name to a shared slice; assert the recorded teardown order is the reverse of registration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why LIFO is the order you want

You build a harness in dependency order: the store exists first, the server needs
the store, the client needs the server's URL. Teardown must go the other way. If
the store closed before the server, an in-flight request the server is handling
would hit a closed store. If the server closed before the client drained, the
client's idle connections would fault. The correct teardown is strictly reverse:
client, server, store.

`t.Cleanup` gives you this without any manual sequencing. Register a cleanup as you
build each layer — store cleanup when you make the store, server cleanup when you
make the server, client cleanup when you make the client — and because cleanups run
last-registered first, they run client, server, store. The ordering falls out of
the build order automatically; you never write "close the client, then the server,
then the store" anywhere. This exercise records each cleanup's name into a shared
slice as it runs, then asserts the slice is exactly the reverse of the registration
order — a direct, mechanical proof of the LIFO contract.

Create `harness.go`:

```go
package lifoharness

import (
	"fmt"
	"net/http"
	"sync"
)

// Store is a seeded, read-only key/value store standing in for a repository.
type Store struct {
	mu   sync.RWMutex
	data map[string]string
	open bool
}

// NewStore returns an open store seeded with the given entries.
func NewStore(seed map[string]string) *Store {
	data := make(map[string]string, len(seed))
	for k, v := range seed {
		data[k] = v
	}
	return &Store{data: data, open: true}
}

// Get returns the value for key and whether it was present in an open store.
func (s *Store) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.open {
		return "", false
	}
	v, ok := s.data[key]
	return v, ok
}

// Close releases the store; it is idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open = false
	s.data = nil
	return nil
}

// Handler serves GET /kv?key=... from the store.
func Handler(s *Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/kv", func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.Get(r.URL.Query().Get("key"))
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, v)
	})
	return mux
}
```

### The runnable demo

The demo builds the same three layers and, because it runs outside a test, tears
them down with `defer` — which is also LIFO, so the closes fire client, server,
store. It queries the live harness first.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http/httptest"

	"example.com/lifoharness"
)

func main() {
	store := lifoharness.NewStore(map[string]string{"region": "eu-west-1"})
	srv := httptest.NewServer(lifoharness.Handler(store))
	client := srv.Client()

	// Reverse build order via LIFO defers: client, then server, then store.
	defer store.Close()
	defer srv.Close()
	defer client.CloseIdleConnections()

	resp, err := client.Get(srv.URL + "/kv?key=region")
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("GET region -> %s\n", body)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET region -> eu-west-1
```

### The tests

`newHarness` builds the three layers and registers a `t.Cleanup` at each step that
records its name into the shared `order` slice. The build runs inside a subtest so
its cleanups run when the subtest returns; the parent then asserts `order` equals
`[client, server, store]` — the reverse of the `store, server, client` registration
order. Before teardown, the subtest hits the live server through the client to
prove the harness is actually wired up.

Create `harness_test.go`:

```go
package lifoharness

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

type harness struct {
	client *http.Client
	url    string
}

// newHarness builds store -> server -> client, recording each layer's teardown
// into order. Because t.Cleanup runs LIFO, teardown order is the reverse of the
// registration order below.
func newHarness(t *testing.T, order *[]string) *harness {
	t.Helper()

	store := NewStore(map[string]string{"region": "eu-west-1"})
	t.Cleanup(func() {
		*order = append(*order, "store")
		_ = store.Close()
	})

	srv := httptest.NewServer(Handler(store))
	t.Cleanup(func() {
		*order = append(*order, "server")
		srv.Close()
	})

	client := srv.Client()
	t.Cleanup(func() {
		*order = append(*order, "client")
		client.CloseIdleConnections()
	})

	return &harness{client: client, url: srv.URL}
}

func TestTeardownIsLIFO(t *testing.T) {
	t.Parallel()
	var order []string

	t.Run("live harness", func(t *testing.T) {
		h := newHarness(t, &order)
		resp, err := h.client.Get(h.url + "/kv?key=region")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(body) != "eu-west-1" {
			t.Fatalf("body = %q, want eu-west-1", body)
		}
	})

	want := []string{"client", "server", "store"}
	if !slices.Equal(order, want) {
		t.Fatalf("teardown order = %v, want %v (reverse of build order)", order, want)
	}
}

func TestStoreServesMissingKeyAs404(t *testing.T) {
	t.Parallel()
	var order []string
	t.Run("query", func(t *testing.T) {
		h := newHarness(t, &order)
		resp, err := h.client.Get(h.url + "/kv?key=absent")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
}
```

## Review

The harness is correct when teardown runs client, server, store — the reverse of
the order in which the layers were built and registered — and `TestTeardownIsLIFO`
proves it by comparing the recorded sequence against the expected reverse. The
mistake this prevents is a "use after close" teardown race: if you hand-wrote the
closes in build order, or scattered them across `defer`s in the wrong functions,
the store could free while the server still references it. Registering one cleanup
per layer as you build it gives the correct reverse order with no ordering logic at
all. Run `go test -race` to confirm the concurrent request against the httptest
server is clean.

## Resources

- [`testing.T.Cleanup`](https://pkg.go.dev/testing#T.Cleanup) — LIFO ordering is the documented contract.
- [`net/http/httptest.NewServer`](https://pkg.go.dev/net/http/httptest#NewServer) and [`Server.Close`](https://pkg.go.dev/net/http/httptest#Server.Close) — the middle layer.
- [`net/http/httptest.Server.Client`](https://pkg.go.dev/net/http/httptest#Server.Client) — a client preconfigured for the test server.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-cleanup-runs-on-failure.md](03-cleanup-runs-on-failure.md) | Next: [05-parallel-defer-vs-cleanup.md](05-parallel-defer-vs-cleanup.md)
