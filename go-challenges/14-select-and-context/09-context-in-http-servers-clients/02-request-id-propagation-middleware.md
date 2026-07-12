# Exercise 2: Request-ID Middleware and Context Accessor

A request id is the join key that stitches server logs, client logs, and
distributed traces into one story. This exercise builds the middleware that
reads an inbound `X-Request-ID` or mints a fresh one, stores it under an
unexported context key, and echoes it back as a response header — plus the typed
accessor `RequestIDFrom(ctx)` that downstream code uses to tag every log line.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
requestid/                 independent module: example.com/requestid
  go.mod                   go 1.26
  requestid.go             WithRequestID middleware; RequestIDFrom accessor; ctxKey type
  cmd/
    demo/
      main.go              one request with a header, one without
  requestid_test.go        generate/echo/empty-contract tests, an Example
```

Files: `requestid.go`, `cmd/demo/main.go`, `requestid_test.go`.
Implement: `WithRequestID(next http.Handler) http.Handler` and `RequestIDFrom(ctx context.Context) string`, keyed by an unexported `ctxKey`; generate a `crypto/rand` hex id when the header is absent.
Test: absent header yields a 16-hex-char id in body and response header; an incoming `abc-123` is echoed in both; `RequestIDFrom(context.Background())` returns `""`.
Verify: `go test -count=1 -race ./...`

## The design

Three decisions carry this exercise.

First, the key type. The id is stored with `context.WithValue`, and the key is an
unexported named type (`type ctxKey int` with a private constant), not a bare
string. A bare string key collides across packages that happen to pick the same
literal; an unexported named type cannot be produced or read by any other
package, so there is no collision surface at all. External code reaches the value
only through the exported `RequestIDFrom` accessor.

Second, the accessor's contract. `RequestIDFrom(ctx)` returns `""` when no id is
present, rather than panicking or returning a sentinel. That "no metadata" return
is what lets a logger call it unconditionally: a request that never went through
the middleware simply logs an empty id instead of crashing. The type assertion is
guarded (`v, ok := ...`) so a value of the wrong type also yields `""` rather than
a panic.

Third, id generation. When the inbound header is absent, generate eight random
bytes with `crypto/rand.Read` and hex-encode them into a 16-character id.
`crypto/rand` (not `math/rand`) matters because request ids sometimes leak into
places where predictability is a weakness; there is no cost to using the secure
source here. The generated id goes into *both* the context and the response
header, so the same value the handler logs is the value the client sees.

Create `requestid.go`:

```go
package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

type ctxKey int

const requestIDKey ctxKey = 0

// RequestIDFrom returns the request id stored in ctx, or "" if none is present.
// The empty-string return is the no-metadata contract: callers (loggers) may
// call it unconditionally without a nil check.
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// generateID returns a 16-character hex id from 8 crypto/rand bytes. In modern
// Go, crypto/rand.Read never returns an error, so we do not have a fallback
// branch to keep dead code out of the package.
func generateID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// WithRequestID reads an inbound X-Request-ID (or generates one), stores it in
// the request context, and echoes it as a response header so it is the join key
// across server logs, client logs, and traces.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = generateID()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
```

Note the comment about `crypto/rand.Read`: since Go 1.24 it is documented to
never fail (it panics internally only if the OS entropy source is unavailable at
startup), so there is no error branch to write — a fallback branch here would be
dead code the gate forbids.

## The runnable demo

The demo hits the middleware twice through an in-process server: once with an
explicit `X-Request-ID` (which must be echoed verbatim) and once without (which
must produce a fresh 16-hex-char id).

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/requestid"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, requestid.RequestIDFrom(r.Context()))
	})

	srv := httptest.NewServer(requestid.WithRequestID(mux))
	defer srv.Close()

	// With an explicit id: echoed verbatim.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("X-Request-ID", "abc-123")
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("given    body=%s header=%s\n", body, resp.Header.Get("X-Request-ID"))

	// Without an id: a fresh 16-hex-char id is generated.
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/", nil)
	resp2, _ := http.DefaultClient.Do(req2)
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	fmt.Printf("generated len=%d matches-header=%v\n", len(body2), string(body2) == resp2.Header.Get("X-Request-ID"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
given    body=abc-123 header=abc-123
generated len=16 matches-header=true
```

## Tests

`TestGeneratesWhenAbsent` and `TestEchoesIncoming` cover the two branches of the
middleware. `TestGeneratedMatchesHeader` pins the invariant that the body id and
the response-header id are the same value. `TestRequestIDFromEmpty` pins the
no-metadata contract. The `Example` documents the echo path with verified output.

Create `requestid_test.go`:

```go
package requestid

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func echoServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, RequestIDFrom(r.Context()))
	})
	return httptest.NewServer(WithRequestID(mux))
}

func do(t *testing.T, url, incoming string) (string, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	if incoming != "" {
		req.Header.Set("X-Request-ID", incoming)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(body), resp.Header.Get("X-Request-ID")
}

func TestGeneratesWhenAbsent(t *testing.T) {
	t.Parallel()
	srv := echoServer()
	defer srv.Close()

	body, header := do(t, srv.URL+"/echo", "")
	if len(body) != 16 {
		t.Fatalf("body = %q (len %d), want a 16-char hex id", body, len(body))
	}
	if header != body {
		t.Fatalf("response header %q != body %q", header, body)
	}
}

func TestEchoesIncoming(t *testing.T) {
	t.Parallel()
	srv := echoServer()
	defer srv.Close()

	body, header := do(t, srv.URL+"/echo", "abc-123")
	if body != "abc-123" {
		t.Fatalf("body = %q, want abc-123", body)
	}
	if header != "abc-123" {
		t.Fatalf("response header = %q, want abc-123", header)
	}
}

func TestRequestIDFromEmpty(t *testing.T) {
	t.Parallel()
	if got := RequestIDFrom(context.Background()); got != "" {
		t.Fatalf("RequestIDFrom(Background) = %q, want empty string", got)
	}
}

func Example() {
	srv := echoServer()
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/echo", nil)
	req.Header.Set("X-Request-ID", "fixed-id")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body), resp.Header.Get("X-Request-ID"))
	// Output: fixed-id fixed-id
}
```

## Review

The middleware is correct when the key is unexported (no cross-package
collision), the accessor returns `""` on a missing id (the no-metadata contract),
and the same id lands in the context and the response header. The classic bug
this replaces is hanging the request id off a request-scoped struct that outlives
the request, which leaks one request's id into the next — the context is the
right home for request-scoped metadata precisely because it dies with the
request. Do not use a bare string key; a named unexported type is the only
collision-proof choice. Run with `-race` since the middleware runs concurrently
across requests.

## Resources

- [`context.WithValue`](https://pkg.go.dev/context#WithValue) — and the guidance to key by an unexported type.
- [`crypto/rand`](https://pkg.go.dev/crypto/rand) — `Read` and its never-fails contract in modern Go.
- [`encoding/hex.EncodeToString`](https://pkg.go.dev/encoding/hex#EncodeToString) — turning random bytes into a printable id.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-per-request-timeout-middleware.md](01-per-request-timeout-middleware.md) | Next: [03-client-fetch-with-context.md](03-client-fetch-with-context.md)
