# Exercise 5: Pooling Buffers in a Hot JSON HTTP Handler

In a JSON API the per-request encode buffer is one of the largest sources of
allocation pressure: every request allocates one, fills it, writes it, and throws
it away. This module builds an `http.Handler` that encodes each response through
a pooled `*bytes.Buffer` + `json.Encoder`, and in doing so fixes a second bug
most handlers have — writing a partial body before an encoding error is noticed.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
jsonhandler/                independent module: example.com/jsonhandler
  go.mod                    go 1.26
  api/
    server.go               type Server; pooled buffer + json.Encoder per request
  cmd/
    demo/
      main.go               drives the handler with httptest and prints the response
  api/server_test.go        status, headers, JSON round-trip, concurrent no-bleed
```

Files: `api/server.go`, `cmd/demo/main.go`, `api/server_test.go`.
Implement: a `Server` whose `ServeHTTP` looks up a user, encodes it into a pooled buffer, sets `Content-Type` and `Content-Length` from `buf.Len()`, then copies the buffer to the `ResponseWriter`.
Test: `httptest` asserts 200, `Content-Type`, `Content-Length` == body length, and a JSON round-trip; a concurrent test proves no buffer bleeds between requests under `-race`.
Verify: `go test -count=1 -race ./...`

### Why encode into a buffer first, not straight to the ResponseWriter

The naive handler writes directly:
`json.NewEncoder(w).Encode(resp)`. It has two problems. The GC problem is that
`json.NewEncoder` and the encoding machinery allocate per call, and at high RPS
those allocations dominate. The correctness problem is subtler and worse: the
encoder streams bytes straight to the socket, so if `Encode` fails halfway
through a large struct (an `UnsupportedTypeError`, a `MarshalJSON` that errors),
the client has already received a `200 OK` and half a JSON document. You cannot
retract the status line. The response is now a malformed body under a success
status — the kind of bug that corrupts a downstream parser and is nearly
impossible to reproduce.

Encoding into a buffer first fixes both. You `Get` a buffer from the pool,
`Encode` into it, and only if that succeeds do you write the status and copy the
buffer to the client. A mid-encode failure yields a clean `500` with no body
written to the socket, because nothing reached the socket yet. And because you
now hold the complete encoded body in the buffer, you know its exact length and
can set an honest `Content-Length` header — which lets the client and any proxy
avoid chunked transfer encoding.

The buffer is reset before it goes back to the pool, so the next request's `Get`
starts clean. `defer` guarantees the return happens on every path, including the
`500`. `buf.WriteTo(w)` drains the buffer into the `ResponseWriter` in one copy.

Create `api/server.go`:

```go
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
)

// User is the response payload.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Server serves users as JSON, encoding each response through a pooled buffer
// so the per-request buffer allocation is reused rather than churned.
type Server struct {
	users map[string]User
	pool  sync.Pool
}

// NewServer returns a Server backed by the given users. The pool's New returns a
// pointer (*bytes.Buffer), avoiding the interface-boxing allocation of a value.
func NewServer(users map[string]User) *Server {
	s := &Server{users: users}
	s.pool.New = func() any { return new(bytes.Buffer) }
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, ok := s.users[r.URL.Query().Get("id")]
	if !ok {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	buf := s.pool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		s.pool.Put(buf)
	}()

	// Encode into the buffer first. If this fails, nothing has been written to
	// the client yet, so we can still send a clean 500 instead of a truncated
	// 200 body.
	if err := json.NewEncoder(buf).Encode(u); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	buf.WriteTo(w)
}
```

### The runnable demo

The demo wires the handler to an in-process `httptest` recorder (no network) and
prints the status, content type, and body of one request.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"example.com/jsonhandler/api"
)

func main() {
	srv := api.NewServer(map[string]api.User{
		"u1": {ID: "u1", Name: "Ada Lovelace", Email: "ada@example.com"},
	})

	req := httptest.NewRequest(http.MethodGet, "/user?id=u1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	res := rec.Result()
	fmt.Printf("status=%d\n", res.StatusCode)
	fmt.Printf("content-type=%s\n", res.Header.Get("Content-Type"))
	fmt.Printf("body=%s\n", strings.TrimSpace(rec.Body.String()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=200
content-type=application/json; charset=utf-8
body={"id":"u1","name":"Ada Lovelace","email":"ada@example.com"}
```

### Tests

The tests assert the full response contract and then fire many concurrent
requests with distinct payloads under `-race`, decoding each and checking it
matches the user that request asked for — which fails loudly if one request's
buffer ever bleeds into another's response.

Create `api/server_test.go`:

```go
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

func testServer() *Server {
	return NewServer(map[string]User{
		"u1": {ID: "u1", Name: "Ada Lovelace", Email: "ada@example.com"},
		"u2": {ID: "u2", Name: "Alan Turing", Email: "alan@example.com"},
	})
}

func TestHandlerOK(t *testing.T) {
	t.Parallel()

	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/user?id=u1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length = %q, want %d", cl, rec.Body.Len())
	}

	var got User
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if want := (User{ID: "u1", Name: "Ada Lovelace", Email: "ada@example.com"}); got != want {
		t.Errorf("decoded = %+v, want %+v", got, want)
	}
}

func TestHandlerNotFound(t *testing.T) {
	t.Parallel()

	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/user?id=nope", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestConcurrentNoBufferBleed(t *testing.T) {
	t.Parallel()

	srv := testServer()
	const n = 500
	ids := []string{"u1", "u2"}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			id := ids[i%len(ids)]
			req := httptest.NewRequest(http.MethodGet, "/user?id="+id, nil)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			var got User
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Errorf("bad JSON for id=%s: %v", id, err)
				return
			}
			if got.ID != id {
				t.Errorf("buffer bleed: asked id=%s, got id=%s", id, got.ID)
			}
		}()
	}
	wg.Wait()
}

func ExampleServer() {
	srv := NewServer(map[string]User{"u1": {ID: "u1", Name: "Ada", Email: "ada@x.io"}})
	req := httptest.NewRequest(http.MethodGet, "/user?id=u1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	fmt.Print(rec.Body.String())
	// Output: {"id":"u1","name":"Ada","email":"ada@x.io"}
}
```

## Review

The handler is correct when a success writes a complete, well-formed body with an
accurate `Content-Length`, a missing user yields a clean `404`, and an encoding
failure would yield a clean `500` with nothing written to the socket — which is
exactly why you encode into the buffer *before* touching the `ResponseWriter`.
`TestConcurrentNoBufferBleed` is the load-bearing test: 500 concurrent requests
for two distinct users, each asserting the decoded `id` matches what it asked
for, catches any reset-contract violation that would let one request's bytes
appear in another's response. Run `go test -race`; a bleed shows up as a
mismatched id or invalid JSON.

## Resources

- [`json.Encoder`](https://pkg.go.dev/encoding/json#Encoder) — `NewEncoder(w).Encode`, and the trailing newline it appends.
- [`bytes.Buffer.WriteTo`](https://pkg.go.dev/bytes#Buffer.WriteTo) — drains the buffer into an `io.Writer` in one call.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest` and `NewRecorder` for handler tests with no network.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-contract-tests-and-benchmark.md](04-contract-tests-and-benchmark.md) | Next: [06-gzip-response-compression-middleware.md](06-gzip-response-compression-middleware.md)
