# Exercise 10: http.Handler — Implementing ServeHTTP for a Readiness Probe

`http.Handler` is the one interface an entire Go service is built from: leaf
handlers, middleware, routers, and test harnesses all speak `ServeHTTP`. This
module implements a readiness probe as a handler that aggregates dependency checks
and returns 200 or 503 with a JSON body, wraps it in a middleware handler, and
tests both in-process with `httptest` — no sockets.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
readiness/                  independent module: example.com/readiness
  go.mod
  readiness.go              Readiness (ServeHTTP); WithSecurityHeaders middleware
  cmd/
    demo/
      main.go               drives the handler with httptest, prints status and body
  readiness_test.go         200 healthy, 503 on failure, middleware delegation, content-type
```

- Files: `readiness.go`, `cmd/demo/main.go`, `readiness_test.go`.
- Implement: a `Readiness` type with `ServeHTTP(w, r)` aggregating checks into 200/503 + JSON, and a `WithSecurityHeaders(next http.Handler) http.Handler` middleware.
- Test: 200 and healthy JSON when all checks pass; 503 when one fails; the middleware wraps and delegates to `next` and sets its header; `Content-Type` is `application/json`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/readiness/cmd/demo
cd ~/go-exercises/readiness
go mod init example.com/readiness
```

### One interface, three roles

`http.Handler` is `ServeHTTP(w http.ResponseWriter, r *http.Request)`. Everything
in an HTTP stack is an implementation of it. `Readiness` is a *leaf* handler: its
`ServeHTTP` runs each registered dependency probe (a DB ping, a cache ping,
whatever), collects the results, and writes a status code and JSON body — 200 when
all pass, 503 when any fails, which is exactly what a Kubernetes readiness probe or
a load balancer health check wants. `WithSecurityHeaders` is *middleware*: it is a
function `http.Handler -> http.Handler` that returns a handler which sets a header
and then calls the wrapped handler's `ServeHTTP`. `http.HandlerFunc` adapts a
plain function to the interface so the middleware does not need its own named type.

The write order in `ServeHTTP` is not incidental: set headers first, call
`w.WriteHeader(code)` once to commit the status line, then write the body. Call
`WriteHeader` after writing body bytes and the status is already 200; set a header
after `WriteHeader` and it is ignored. `Content-Type: application/json` must
therefore be set before `WriteHeader`. Each probe runs against the request's
context (`r.Context()`), so a probe can honour cancellation and deadlines.

Because `http.Handler` is a pure in-process contract, `net/http/httptest` tests it
without a listening socket: `httptest.NewRequest` builds a `*http.Request`,
`httptest.NewRecorder` is a `http.ResponseWriter` that records everything, and you
call `ServeHTTP(rec, req)` directly, then read `rec.Code`, `rec.Header()`, and
`rec.Body`. This is how every handler in a real codebase is unit-tested.

Create `readiness.go`:

```go
package readiness

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
)

// ProbeFunc reports whether a dependency is ready; a non-nil error means not.
type ProbeFunc func(context.Context) error

type namedProbe struct {
	name  string
	probe ProbeFunc
}

// Readiness aggregates dependency probes into a single http.Handler.
type Readiness struct {
	mu     sync.RWMutex
	checks []namedProbe
}

// New creates an empty Readiness handler.
func New() *Readiness { return &Readiness{} }

// Add registers a named probe and returns the receiver for chaining.
func (rd *Readiness) Add(name string, probe ProbeFunc) *Readiness {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	rd.checks = append(rd.checks, namedProbe{name: name, probe: probe})
	return rd
}

type report struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func (rd *Readiness) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rd.mu.RLock()
	checks := rd.checks
	rd.mu.RUnlock()

	res := report{Status: "ok", Checks: make(map[string]string, len(checks))}
	code := http.StatusOK
	for _, c := range checks {
		if err := c.probe(r.Context()); err != nil {
			res.Checks[c.name] = err.Error()
			res.Status = "unavailable"
			code = http.StatusServiceUnavailable
		} else {
			res.Checks[c.name] = "ok"
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(res)
}

// WithSecurityHeaders wraps next, setting a security header before delegating.
func WithSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

var _ http.Handler = (*Readiness)(nil)
```

### The runnable demo

The demo builds a readiness handler with two passing checks, wraps it in the
middleware, and drives it with `httptest` — printing the status, content type, and
JSON body without opening a port.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/readiness"
)

func main() {
	rd := readiness.New().
		Add("db", func(context.Context) error { return nil }).
		Add("cache", func(context.Context) error { return nil })

	h := readiness.WithSecurityHeaders(rd)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.ServeHTTP(rec, req)

	fmt.Printf("status=%d\n", rec.Code)
	fmt.Printf("content-type=%s\n", rec.Header().Get("Content-Type"))
	fmt.Printf("nosniff=%s\n", rec.Header().Get("X-Content-Type-Options"))
	fmt.Print(rec.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=200
content-type=application/json
nosniff=nosniff
{"status":"ok","checks":{"cache":"ok","db":"ok"}}
```

`encoding/json` sorts map keys, so `cache` precedes `db`, and `json.NewEncoder`
adds the trailing newline.

### Tests

`TestHealthy` asserts 200, the `application/json` content type, and an `"ok"`
status when every probe passes. `TestUnavailable` fails one probe and asserts 503
and the failing check's message in the body. `TestMiddlewareDelegates` wraps a
sentinel handler and asserts both that the wrapped handler ran and that the header
was set. All three use `httptest` with no socket.

Create `readiness_test.go`:

```go
package readiness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthy(t *testing.T) {
	t.Parallel()

	rd := New().
		Add("db", func(context.Context) error { return nil }).
		Add("cache", func(context.Context) error { return nil })

	rec := httptest.NewRecorder()
	rd.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var res report
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("status field = %q, want ok", res.Status)
	}
	if res.Checks["db"] != "ok" || res.Checks["cache"] != "ok" {
		t.Fatalf("checks = %v, want both ok", res.Checks)
	}
}

func TestUnavailable(t *testing.T) {
	t.Parallel()

	rd := New().
		Add("db", func(context.Context) error { return nil }).
		Add("cache", func(context.Context) error { return errors.New("connection refused") })

	rec := httptest.NewRecorder()
	rd.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}

	var res report
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "unavailable" {
		t.Fatalf("status field = %q, want unavailable", res.Status)
	}
	if res.Checks["cache"] != "connection refused" {
		t.Fatalf("cache check = %q, want the error message", res.Checks["cache"])
	}
}

func TestMiddlewareDelegates(t *testing.T) {
	t.Parallel()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})

	h := WithSecurityHeaders(next)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !called {
		t.Fatal("middleware did not delegate to the wrapped handler")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("code = %d, want 418 (from the wrapped handler)", rec.Code)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff header = %q, want nosniff", got)
	}
}

func ExampleReadiness() {
	rd := New().Add("db", func(context.Context) error { return nil })
	rec := httptest.NewRecorder()
	rd.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	fmt.Println(rec.Code)
	// Output: 200
}
```

## Review

The probe is correct when it returns 200 with an `"ok"` body while every check
passes and 503 with the failing check's message the instant one fails, always with
`Content-Type: application/json` set before `WriteHeader`. The middleware is
correct when it sets its header and delegates to the wrapped handler — proven by
the sentinel handler flipping `called` and returning its own status through the
wrapper. The structural lesson is that `http.Handler` is the single seam of the
whole service: the same `ServeHTTP` contract is the leaf, the middleware, and the
test surface, and `httptest` exercises all of it in-process. Run `go test -race`.

## Resources

- [net/http.Handler](https://pkg.go.dev/net/http#Handler) — `ServeHTTP`, `HandlerFunc`, and the middleware pattern.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRecorder` and `NewRequest` for socket-free handler tests.
- [Kubernetes readiness probes](https://kubernetes.io/docs/concepts/configuration/liveness-readiness-startup-probes/) — how the 200/503 signal is consumed in production.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-valuer-scanner-db-enum.md](09-valuer-scanner-db-enum.md) | Next: [../05-interface-composition-and-embedding/00-concepts.md](../05-interface-composition-and-embedding/00-concepts.md)
