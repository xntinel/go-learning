# Exercise 1: Build the check package as a testable unit

The foundation of the whole tool is a package that performs one HTTP health check
correctly and is trivial to test. It calls the clock and the transport directly,
returns a structured result, and exposes an error contract callers can branch on.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
check/                      independent module: example.com/check
  go.mod                    go 1.26
  check.go                  Client interface; Result; ErrEmptyURL; URL(ctx, client, rawURL)
  cmd/
    demo/
      main.go               runnable demo against an in-process httptest server
  check_test.go             table-driven httptest + roundTripFunc suite, errors.Is assertions
```

Files: `check.go`, `cmd/demo/main.go`, `check_test.go`.
Implement: a `Client` one-method interface, a `Result` struct, and
`URL(ctx, client, rawURL)` that constructs a context-carrying request, times the
call, drains and closes the body, returns `ErrEmptyURL`, and wraps causes with
`%w`.
Test: table-driven cases over real 2xx/4xx/5xx `httptest` responses plus a
`roundTripFunc` stub for the transport-error and empty-URL cases, asserting
`Result.StatusCode` and `errors.Is(err, ErrEmptyURL)`.
Verify: `go test -count=1 -race ./...` and `go vet ./...`, with `gofmt -l` empty.

### Why the interface is one method wide

The single design decision that makes this package testable is the `Client`
interface. It has exactly one method, `Do(*http.Request) (*http.Response, error)`,
which is the method `*http.Client` already has. So production hands `URL` an
`http.DefaultClient`, and a test hands it either the real client from an
`httptest.Server` or a tiny `roundTripFunc` stub — with no mock framework and no
mock drift. If instead `URL` called `http.Get` directly, there would be no seam:
the only way to test it would be to reach the network, which is slow, flaky, and
forbidden in the default test path.

`URL` returns `(Result, error)`. The `Result` carries the URL, the status code,
and the measured duration; the error carries identity. The empty-URL case returns
the package sentinel `ErrEmptyURL` unwrapped, so a caller writes
`errors.Is(err, ErrEmptyURL)`. The request-construction and transport failures are
wrapped with `%w`, so the same caller can still walk to the underlying cause with
`errors.Is`/`errors.As` while getting a message with context prepended.

### The body drain is not optional

After a successful `Do`, the code does two things to the body in order:
`io.Copy(io.Discard, resp.Body)` then `resp.Body.Close()` (the close via
`defer`). Draining before closing is what lets an HTTP/1.1 keep-alive connection
return to the client's pool for reuse. Omit the drain and every request tears
down its TCP connection and opens a fresh one; under load that is connection
churn and, eventually, ephemeral port exhaustion. A health checker that runs on a
tight loop is exactly the workload that exposes this.

Create `check.go`:

```go
package check

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrEmptyURL is returned when URL is called with an empty target. Callers
// branch on it with errors.Is.
var ErrEmptyURL = errors.New("url is required")

// Client is the one method of *http.Client that URL needs. Production passes
// http.DefaultClient; tests pass an httptest server client or a stub.
type Client interface {
	Do(*http.Request) (*http.Response, error)
}

// Result is the outcome of a single health check.
type Result struct {
	URL        string
	StatusCode int
	Duration   time.Duration
}

// URL performs a GET against rawURL using client, carrying ctx into the
// request so cancellation flows to the transport. Construction and transport
// failures are wrapped with %w; an empty rawURL returns ErrEmptyURL.
func URL(ctx context.Context, client Client, rawURL string) (Result, error) {
	if rawURL == "" {
		return Result{}, ErrEmptyURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Result{}, fmt.Errorf("new request %q: %w", rawURL, err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("get %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	// Drain before close so a keep-alive connection returns to the pool.
	_, _ = io.Copy(io.Discard, resp.Body)

	return Result{
		URL:        rawURL,
		StatusCode: resp.StatusCode,
		Duration:   time.Since(start),
	}, nil
}
```

### The runnable demo

The demo spins an in-process `httptest.Server` so the run is deterministic and
needs no network. It prints the status and whether a non-negative duration was
measured (the exact duration varies, so it is not printed literally).

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/check"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res, err := check.URL(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("status=%d measured=%v\n", res.StatusCode, res.Duration >= 0)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=200 measured=true
```

### Tests

The suite proves the two halves of the contract. The `httptest.Server` cases
drive real 2xx/4xx/5xx responses and assert `Result.StatusCode` — including that a
`4xx` is a *result*, not an error, because the check reached the server. The
`roundTripFunc` cases cover what an `httptest` server cannot: a transport failure
(assert the wrapped error is reachable with `errors.Is`) and the empty-URL
sentinel. Subtests run in parallel; since Go 1.22 the loop variable is per
iteration, so there is no `tc := tc`.

Create `check_test.go`:

```go
package check

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// roundTripFunc adapts a function to the Client interface for the cases an
// httptest server cannot produce (a transport error).
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestURL(t *testing.T) {
	t.Parallel()

	transportErr := errors.New("transport down")

	tests := []struct {
		name       string
		statusCode int
		rawURL     string
		client     Client
		wantStatus int
		wantErr    error
	}{
		{name: "ok 200", statusCode: http.StatusOK, wantStatus: http.StatusOK},
		{name: "no content 204", statusCode: http.StatusNoContent, wantStatus: http.StatusNoContent},
		{name: "client error is a result", statusCode: http.StatusNotFound, wantStatus: http.StatusNotFound},
		{name: "server error is a result", statusCode: http.StatusInternalServerError, wantStatus: http.StatusInternalServerError},
		{name: "empty url", rawURL: "", client: http.DefaultClient, wantErr: ErrEmptyURL},
		{name: "transport error", rawURL: "http://example.invalid", client: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, transportErr
		}), wantErr: transportErr},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client := tc.client
			rawURL := tc.rawURL
			if client == nil {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.statusCode)
				}))
				t.Cleanup(srv.Close)
				client = srv.Client()
				rawURL = srv.URL
			}

			got, err := URL(t.Context(), client, rawURL)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want errors.Is %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.StatusCode != tc.wantStatus {
				t.Fatalf("StatusCode = %d, want %d", got.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestEmptyURLIsSentinel(t *testing.T) {
	t.Parallel()
	_, err := URL(t.Context(), http.DefaultClient, "")
	if !errors.Is(err, ErrEmptyURL) {
		t.Fatalf("err = %v, want ErrEmptyURL", err)
	}
}
```

## Review

The package is correct when its error contract is exact: an empty URL returns
`ErrEmptyURL` and only that, request construction and transport failures come
back wrapped so `errors.Is` reaches the cause, and every other status — including
`4xx` and `5xx` — is a populated `Result` with a real `StatusCode`. The status is
a property of the response, not a Go error; only the transport failing to produce
a response is. Confirm that distinction by re-reading the `TestURL` table: the
`404` and `500` cases assert `wantStatus`, not `wantErr`.

The traps here are structural. Do not call `http.Get` inside `URL`; the
one-method `Client` interface is the seam that makes the whole thing testable
without the network. Do not skip the `io.Copy(io.Discard, resp.Body)` drain
before `Close`, or you defeat connection reuse. Do not compare `err.Error()`
strings — assert identity with `errors.Is`. Run `go test -race` to confirm there
is no accidental shared state across the parallel subtests.

## Resources

- [net/http.NewRequestWithContext](https://pkg.go.dev/net/http#NewRequestWithContext) — building a request that carries a cancellable context.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — a real in-process HTTP server and its client for tests.
- [errors package](https://pkg.go.dev/errors) — `errors.Is`/`errors.As` and the `%w` wrapping contract.
- [io.Copy and io.Discard](https://pkg.go.dev/io#Discard) — draining a response body before closing it.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-httptest-table-driven-suite.md](02-httptest-table-driven-suite.md)
