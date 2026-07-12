# Exercise 8: Racing an HTTP Handler -- httptest.Server Hammered by Concurrent Clients

A handler that mutates shared in-memory state on every request -- a per-key hit
counter, an idempotency-seen set, a session map -- races the moment two requests
land at once, and the Go HTTP server serves every request on its own goroutine,
so concurrency is guaranteed in production. This exercise ties the race detector
to the real request path: it stands up an `httptest.Server`, hammers it with N
concurrent clients under `-race`, and asserts the server-side count matches the
number of successful requests.

This module is self-contained: its own `go mod init`, its own racy demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
hitserver/                  independent module: example.com/hitserver
  go.mod                    go 1.26
  server.go                 type Server (sync.Mutex + map): ServeHTTP, Hits -- fixed
  cmd/
    demo/
      main.go               3 sequential requests, print the hit count
    racy/
      main.go               unsynchronized-map handler under httptest; run with -race
  server_test.go            N concurrent clients; server count == successes, under -race
```

Files: `server.go`, `cmd/demo/main.go`, `cmd/racy/main.go`, `server_test.go`.
Implement: a `Server` whose `ServeHTTP` increments a per-key counter under a
mutex, with a `Hits` accessor.
Test: `TestHandlerConcurrentRequests` drives the `httptest.Server` from N clients
and asserts the server-side count equals the successful requests, under `-race`.
Verify: `go test -count=1 -race ./...`; then `go run -race ./cmd/racy`.

### Why the handler races, and how the test surfaces it

`net/http` serves each request in its own goroutine. So a handler that does
`s.hits[key]++` on a shared map is running that read-modify-write concurrently
across requests: a data race on the map, and on a `map` specifically a possible
`concurrent map writes` fatal crash. The bug is invisible under a single-client
test because there is no contention; it only appears under concurrent load, which
is exactly what production is. That is why the detector needs a test that creates
the concurrency.

The fix is the guarded-shared-state pattern: a `sync.Mutex` around the
increment, and a `Hits` accessor that reads under the same lock. (A per-key
counter of independent words could also be `atomic.Int64` values in a preallocated
map, but a mutex around the map handles arbitrary keys appearing at runtime, which
is the realistic case.) With the lock, every `ServeHTTP` increment is ordered
against every other, so the count is exact and the detector is quiet.

The test drives the handler through a real `httptest.Server`, so the race, if
present, originates inside the handler on the actual request path rather than in a
synthetic harness. Each of N client goroutines issues a fixed number of requests,
reading and closing every response body (so connections are reused, not leaked),
and counts its successes. After joining, the assertion is that the server's own
count equals the total successful requests -- an exact match only possible if
every concurrent increment landed, which requires the lock.

Create `server.go`:

```go
package hitserver

import (
	"net/http"
	"sync"
)

// Server is an HTTP handler that counts hits per key in a shared map. The map is
// guarded by a mutex because net/http serves every request on its own goroutine.
type Server struct {
	mu   sync.Mutex
	hits map[string]int
}

// NewServer returns a Server with an empty hit map.
func NewServer() *Server {
	return &Server{hits: make(map[string]int)}
}

// ServeHTTP increments the counter for the ?key= query parameter and returns 200.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")

	s.mu.Lock()
	s.hits[key]++
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

// Hits returns the number of requests seen for key.
func (s *Server) Hits(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[key]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/hitserver"
)

func main() {
	s := hitserver.NewServer()
	ts := httptest.NewServer(s)
	defer ts.Close()

	for range 3 {
		resp, err := http.Get(ts.URL + "?key=home")
		if err != nil {
			panic(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	fmt.Printf("hits(home): %d\n", s.Hits("home"))
}
```

Run it:

```bash
go run -race ./cmd/demo
```

Expected output:

```text
hits(home): 3
```

### The racy version, for the report

Create `cmd/racy/main.go`. Run with `go run -race ./cmd/racy` to see the report
originate inside the handler:

```go
// Command racy serves a handler that mutates an unguarded shared map on every
// request, then hammers it with concurrent clients. Run manually:
//
//	go run -race ./cmd/racy
//
// It is a main package with no test, so `go test -race ./...` only builds it.
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

func main() {
	hits := make(map[string]int) // shared, unguarded -- the bug

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Query().Get("key")]++ // concurrent map write inside the handler
		w.WriteHeader(http.StatusOK)
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(ts.URL + "?key=x")
			if err != nil {
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()

	fmt.Printf("hits: %d (concurrent map writes race; may also crash)\n", hits["x"])
}
```

### Tests

`TestHandlerConcurrentRequests` starts the fixed handler behind an
`httptest.Server`, launches N client goroutines that each send a fixed number of
requests, reads and closes every body, and counts successes atomically. Because
every request completes, the total successes equal `clients * perClient`, and the
assertion is that the server's own `Hits("x")` equals that total. It passes under
`-race` because the mutex orders every increment. Note the goroutines report
errors with `t.Errorf` (safe from any goroutine) rather than `t.Fatal` (which must
only be called from the test goroutine).

Create `server_test.go`:

```go
package hitserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestHandlerConcurrentRequests(t *testing.T) {
	t.Parallel()

	s := NewServer()
	ts := httptest.NewServer(s)
	defer ts.Close()

	const (
		clients   = 16
		perClient = 50
	)
	var success atomic.Int64
	var wg sync.WaitGroup
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perClient {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"?key=x", nil)
				if err != nil {
					t.Errorf("new request: %v", err)
					return
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Errorf("do request: %v", err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					success.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	want := int64(clients * perClient)
	if success.Load() != want {
		t.Fatalf("successful requests = %d, want %d", success.Load(), want)
	}
	if got := int64(s.Hits("x")); got != success.Load() {
		t.Fatalf("server counted %d hits, clients saw %d successes", got, success.Load())
	}
}

func TestHandlerSeparateKeys(t *testing.T) {
	t.Parallel()

	s := NewServer()
	ts := httptest.NewServer(s)
	defer ts.Close()

	for _, key := range []string{"a", "a", "b"} {
		resp, err := http.Get(ts.URL + "?key=" + key)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	if got := s.Hits("a"); got != 2 {
		t.Fatalf("Hits(a) = %d, want 2", got)
	}
	if got := s.Hits("b"); got != 1 {
		t.Fatalf("Hits(b) = %d, want 1", got)
	}
}
```

## Review

The handler is correct when the server-side count equals the number of successful
requests under concurrent load, with no torn map access. The proof is
`TestHandlerConcurrentRequests` passing under `-race`: N clients hit the real
`httptest.Server` at once, the detector finds no unordered map access because the
mutex guards every increment, and the exact count match confirms no increment was
lost. `TestHandlerSeparateKeys` pins the per-key semantics.

The mistakes to avoid: mutating a shared map in a handler with no lock (the
`net/http` per-request goroutine model guarantees the contention that exposes it),
and forgetting to read and close each response body (which leaks connections and
can stall the test). Guard the shared state with a mutex or atomics, and always
drain and close bodies. Run `go test -count=1 -race ./...`.

## Resources

- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) -- `NewServer` for driving a handler over a real loopback connection.
- [`net/http`](https://pkg.go.dev/net/http) -- the per-request-goroutine serving model that makes handler state shared by default.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) -- running `-race` against server code and reading handler-origin reports.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-mutex-token-bucket-limiter.md](07-mutex-token-bucket-limiter.md) | Next: [09-race-build-tag-and-gorace-ci.md](09-race-build-tag-and-gorace-ci.md)
