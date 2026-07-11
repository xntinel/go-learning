# Exercise 7: Test The HTTP Handler That Fronts The Cache

A cache rarely lives alone; it sits behind an HTTP handler implementing the
cache-aside pattern — serve from cache on a hit, load from the backing store and
populate on a miss. This module builds that handler with Go 1.22 method-and-wildcard
routing and tests it end-to-end with `httptest`, proving the cache-aside contract
by counting how often the loader is called.

## What you'll build

```text
cachehttp/                  independent module: example.com/cachehttp
  go.mod
  cache.go                  the cache under test
  handler.go                cache-aside http.Handler with an injected loader
  cmd/
    demo/
      main.go               runnable demo against httptest.Server
  handler_test.go           httptest.NewRecorder tests: hit/miss/404/410
```

Files: `cache.go`, `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
Implement: `NewServer(c, loader)` returning an `http.Handler`; `GET /items/{key}` returns 200 on hit, loads and 200 on miss, 404 when the loader has no value, 410 on expiry.
Test: `httptest.NewRecorder` + `httptest.NewRequest`; a counting loader proving miss-then-hit; status assertions for 404 and 410.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cachehttp/cmd/demo
cd ~/go-exercises/cachehttp
go mod init example.com/cachehttp
```

### The cache-aside contract and its four status codes

The handler routes `GET /items/{key}` and maps the cache's two sentinels plus the
loader's outcome onto four HTTP responses. On a cache hit it writes `200` with the
stored bytes. On `ErrExpired` it writes `410 Gone` — the entry existed but its TTL
lapsed, a distinct signal from "never existed" that lets a client distinguish a
stale key from an unknown one. On `ErrNotFound` it falls through to the loader: the
loader either returns the value (which the handler stores with a TTL and serves as
`200`) or reports that no such item exists (which becomes `404`). This is the
cache-aside pattern: the cache is a read-through veneer over a backing store, and a
miss is a normal event that triggers a load, not an error.

The routing uses the Go 1.22 `ServeMux`, which matches on method and path pattern
in one line — `mux.HandleFunc("GET /items/{key}", ...)` — and exposes the wildcard
via `r.PathValue("key")`. No third-party router is needed, and the routing itself
is testable. The loader is injected as a function value, which is the seam that
makes the test decisive: a *counting* loader proves the cache-aside semantics
directly. The first request for a key misses, calls the loader, and populates the
cache; the second request for the same key is served from the cache and does not
call the loader again. Asserting the loader's call count is exactly `1` after two
requests is the test that a hand-wavy "it seems cached" check cannot replace.

`httptest.NewRecorder` and `httptest.NewRequest` exercise the handler in-process:
no socket, no port, no flakiness from a real listener. You build a request, serve
it against the handler with `ServeHTTP`, and read `rec.Code` and `rec.Body`. The
request carries `t.Context()` so its lifetime is tied to the test.

Create `cache.go`:

```go
package cachehttp

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("cache: key not found")
	ErrExpired  = errors.New("cache: key expired")
)

type entry struct {
	value     []byte
	expiresAt time.Time
}

type Cache struct {
	mu   sync.RWMutex
	data map[string]entry
	now  func() time.Time
}

func New() *Cache {
	return &Cache{data: make(map[string]entry), now: time.Now}
}

func (c *Cache) Get(key string) ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	if !e.expiresAt.IsZero() && c.now().After(e.expiresAt) {
		return nil, ErrExpired
	}
	return e.value, nil
}

func (c *Cache) Set(key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = c.now().Add(ttl)
	}
	c.data[key] = entry{value: value, expiresAt: expiresAt}
}
```

### The handler

The `Loader` returns the bytes for a key or `false` when the backing store has no
such item. `NewServer` wires the route and closes over the cache and loader.

Create `handler.go`:

```go
package cachehttp

import (
	"errors"
	"net/http"
	"time"
)

// Loader fetches a key from the backing store on a cache miss. ok=false means
// the item does not exist anywhere, which the handler turns into a 404.
type Loader func(key string) (value []byte, ok bool)

// NewServer returns a cache-aside handler for GET /items/{key}.
func NewServer(c *Cache, load Loader) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")

		value, err := c.Get(key)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusOK)
			w.Write(value)
			return
		case errors.Is(err, ErrExpired):
			http.Error(w, "gone", http.StatusGone)
			return
		case errors.Is(err, ErrNotFound):
			// cache miss: fall through to the loader
		default:
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		loaded, ok := load(key)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		c.Set(key, loaded, time.Hour)
		w.WriteHeader(http.StatusOK)
		w.Write(loaded)
	})
	return mux
}
```

### The runnable demo

The demo starts a real in-process `httptest.Server`, requests a key twice, and
prints how often the loader ran — one load for two requests demonstrates
cache-aside.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"example.com/cachehttp"
	"net/http/httptest"
)

func main() {
	var loads atomic.Int64
	load := func(key string) ([]byte, bool) {
		loads.Add(1)
		return []byte("payload-for-" + key), true
	}

	srv := httptest.NewServer(cachehttp.NewServer(cachehttp.New(), load))
	defer srv.Close()

	for range 2 {
		resp, err := http.Get(srv.URL + "/items/abc")
		if err != nil {
			panic(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("status=%d body=%s\n", resp.StatusCode, body)
	}
	fmt.Printf("loader called %d time(s)\n", loads.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=200 body=payload-for-abc
status=200 body=payload-for-abc
loader called 1 time(s)
```

### The tests

Create `handler_test.go`:

```go
package cachehttp

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheAsideMissThenHit(t *testing.T) {
	t.Parallel()
	var loads atomic.Int64
	load := func(key string) ([]byte, bool) {
		loads.Add(1)
		return []byte("loaded:" + key), true
	}
	h := NewServer(New(), load)

	for i := range 2 {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/items/k1", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d; want 200", i, rec.Code)
		}
		if got := rec.Body.String(); got != "loaded:k1" {
			t.Fatalf("request %d: body = %q; want %q", i, got, "loaded:k1")
		}
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("loader called %d times; want 1 (miss then hit)", got)
	}
}

func TestNotFound(t *testing.T) {
	t.Parallel()
	load := func(string) ([]byte, bool) { return nil, false }
	h := NewServer(New(), load)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/items/missing", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", rec.Code)
	}
}

func TestExpiredReturnsGone(t *testing.T) {
	t.Parallel()
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New()
	c.now = func() time.Time { return epoch }
	c.Set("k", []byte("v"), time.Minute)
	c.now = func() time.Time { return epoch.Add(2 * time.Minute) }

	// Loader must not be consulted for an expired hit.
	h := NewServer(c, func(string) ([]byte, bool) {
		t.Error("loader called for an expired entry; want 410 without load")
		return nil, false
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/items/k", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d; want 410", rec.Code)
	}
}
```

## Review

The handler is correct when each outcome maps to the right status: a hit is `200`,
an expired entry is `410` without consulting the loader, a miss loads and either
serves `200` or returns `404` when the backing store has nothing. The decisive
test is the counting loader: two requests for the same key must call the loader
exactly once, which is the operational definition of cache-aside — the second
request is served from the cache. `httptest.NewRecorder` runs all of this
in-process with no sockets, so there is no port to bind and nothing to flake.
Tie the request to `t.Context()` so a cancelled test cancels the request, and run
`-race` because the handler's loader and cache are touched concurrently under load.

## Resources

- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRecorder`, `NewRequestWithContext`, and `NewServer`.
- [`http.ServeMux` (1.22 routing)](https://pkg.go.dev/net/http#ServeMux) — method-and-wildcard patterns and `PathValue`.
- [Go 1.22 release notes: routing enhancements](https://go.dev/doc/go1.22#enhanced_routing_patterns) — the `GET /items/{key}` syntax.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-testmain-and-short.md](06-testmain-and-short.md) | Next: [08-synctest-deterministic-ttl.md](08-synctest-deterministic-ttl.md)
