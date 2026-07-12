# Exercise 2: Structured access logging with slog

Every backend service needs an access log: one structured record per request with
the method, path, and how long it took. This module builds that as a middleware
over `log/slog` — the layer you compose into every chain and later extend to also
capture the status code and response size.

This module is fully self-contained: its own `go mod init`, its own `Chain`, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
accesslog/                   independent module: example.com/accesslog
  go.mod                     go 1.26
  middleware.go              type Middleware; Handler alias; Chain; Logger(*slog.Logger)
  cmd/demo/main.go           runnable demo emitting one structured access line
  middleware_test.go         asserts the log line contains method+path and next still runs
```

- Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: `Logger(*slog.Logger) Middleware` that records the start time, calls `next`, then emits an `Info` record with `method`, `path`, and `elapsed`.
- Test: log into a `bytes.Buffer`-backed `slog.TextHandler`, assert the emitted line contains the request path and method, and that `next` still runs and a 200 is unchanged.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/14-interface-based-middleware-chain/02-request-logging-middleware/cmd/demo
cd go-solutions/08-interfaces/14-interface-based-middleware-chain/02-request-logging-middleware
go mod edit -go=1.26
```

### Why timing wraps next on both sides

Access logging is the canonical "do something after `next` returns" middleware.
You capture `start := time.Now()` before calling `next`, let the entire inner
chain and handler run, then log `time.Since(start)` after it returns. Because the
logger is normally the *outermost or near-outermost* layer, that elapsed time
covers the whole request including every inner middleware — which is what you want
for latency metrics. Emitting the log *after* `next` (not before) is deliberate:
you cannot know the elapsed time until the handler is done, and in the next
exercise you also cannot know the final status code until the handler has
committed it.

Using `slog` rather than `log.Printf` matters for production: the record is
structured (key/value attributes), so a log pipeline can index on `path`, filter
by `method`, and compute latency percentiles from `elapsed` without regex-parsing
a free-form string. The middleware takes a `*slog.Logger` so the caller controls
the handler (text for local dev, JSON for production) and the destination.

Create `middleware.go`:

```go
package accesslog

import (
	"log/slog"
	"net/http"
	"time"
)

// Handler alias keeps the Middleware signature readable and lets any
// http.Handler satisfy it directly.
type Handler = http.Handler

// Middleware takes the next handler and returns a wrapping handler.
type Middleware func(Handler) Handler

// Chain composes middlewares; the first declared is the outermost.
type Chain struct{ middlewares []Middleware }

func NewChain(mws ...Middleware) *Chain {
	cp := make([]Middleware, len(mws))
	copy(cp, mws)
	return &Chain{middlewares: cp}
}

func (c *Chain) Then(h Handler) Handler {
	for i := len(c.middlewares) - 1; i >= 0; i-- {
		h = c.middlewares[i](h)
	}
	return h
}

// Logger returns a middleware that times the request and emits one structured
// access record after the inner handler returns.
func Logger(logger *slog.Logger) Middleware {
	return func(next Handler) Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			next.ServeHTTP(w, r)
			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"elapsed", time.Since(start),
			)
		})
	}
}
```

### The runnable demo

The demo sends one request through the chain with a `slog` text handler writing to
stderr, then prints the handler's body to stdout so the Expected-output block is
deterministic (the access line, with its variable `elapsed`, goes to stderr).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"

	"example.com/accesslog"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s", r.URL.Path[1:])
	})

	chain := accesslog.NewChain(accesslog.Logger(logger))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/world", nil)
	chain.Then(final).ServeHTTP(rec, req)

	fmt.Println(rec.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (stdout; the access record is written to stderr):

```
hello world
```

### Tests

`TestLoggerRecordsMethodAndPath` sends a request and asserts the buffered log line
contains both `path=/ping` and `method=GET`. `TestLoggerCallsNextAndPreservesStatus`
proves the logging layer is transparent to the response: the final handler runs
(observed via a flag) and its 200 status is unchanged. `TestLoggerEmitsExactlyOnce`
pins that a single request produces a single record — a logger that emitted before
*and* after `next` would double-log.

Create `middleware_test.go`:

```go
package accesslog

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoggerRecordsMethodAndPath(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	NewChain(Logger(logger)).Then(final).ServeHTTP(rec, req)

	line := buf.String()
	if !strings.Contains(line, "path=/ping") {
		t.Errorf("log = %q, want path=/ping", line)
	}
	if !strings.Contains(line, "method=GET") {
		t.Errorf("log = %q, want method=GET", line)
	}
}

func TestLoggerCallsNextAndPreservesStatus(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	called := false
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "body")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	NewChain(Logger(logger)).Then(final).ServeHTTP(rec, req)

	if !called {
		t.Fatal("Logger did not call next")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (Logger must not alter status)", rec.Code)
	}
	if rec.Body.String() != "body" {
		t.Fatalf("body = %q, want body", rec.Body.String())
	}
}

func TestLoggerEmitsExactlyOnce(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	NewChain(Logger(logger)).Then(final).ServeHTTP(rec, req)

	if n := strings.Count(buf.String(), "msg=request"); n != 1 {
		t.Fatalf("emitted %d records, want exactly 1", n)
	}
}
```

## Review

The logging middleware is correct when it is *transparent*: it must call `next`
exactly once, must not touch the status or body, and must emit exactly one record
per request. `TestLoggerCallsNextAndPreservesStatus` guards transparency;
`TestLoggerEmitsExactlyOnce` guards the single-record invariant, which fails the
moment someone adds a second `logger.Info` on the entry path. The record is
emitted *after* `next` because neither the elapsed time nor (in the next exercise)
the final status is known until the handler returns. Prefer `slog`'s structured
attributes over a formatted string so downstream tooling can index and aggregate;
that is the whole reason to log with key/value pairs instead of `log.Printf`.

## Resources

- [log/slog](https://pkg.go.dev/log/slog) — `slog.New`, `slog.Logger.Info`, and the attribute model.
- [slog.NewTextHandler](https://pkg.go.dev/log/slog#NewTextHandler) — the human-readable `key=value` handler the tests parse.
- [The Go Blog: Structured Logging with slog](https://go.dev/blog/slog) — why structured records beat formatted strings.
- [time.Since](https://pkg.go.dev/time#Since) — the elapsed-time measurement the middleware logs.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-chain-composition-and-order.md](01-chain-composition-and-order.md) | Next: [03-require-token-auth-middleware.md](03-require-token-auth-middleware.md)
