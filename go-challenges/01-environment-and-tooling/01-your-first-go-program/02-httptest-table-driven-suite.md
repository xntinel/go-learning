# Exercise 2: Test an HTTP checker without mocks

Testing HTTP code well is a distinct skill from writing it. This module restates
a minimal checker so it stands alone, then builds a comprehensive table-driven
suite around a real `httptest.Server` — no mock framework — including a
deterministic cancellation test that proves the request honors its context.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
httptestsuite/              independent module: example.com/httptestsuite
  go.mod                    go 1.26
  check.go                  minimal Client, Result, ErrEmptyURL, URL(ctx, client, rawURL)
  cmd/
    demo/
      main.go               runnable demo exercising the checker over httptest
  check_test.go             parallel table suite + t.Context() cancellation test
```

Files: `check.go`, `cmd/demo/main.go`, `check_test.go`.
Implement: the same minimal checker as Exercise 1 (restated so this module is
independent).
Test: a table-driven suite that spins one `httptest.Server` per case, runs
subtests in parallel with `t.Context()`, exercises the `Client` via
`server.Client()`, asserts error identity with `errors.Is`, and a separate test
that cancels the context and asserts `context.Canceled` — with no `time.Sleep`.
Verify: `go test -count=1 -race ./...`, `gofmt -l` empty.

Set up the module:

```bash
mkdir -p ~/go-exercises/httptestsuite/cmd/demo
cd ~/go-exercises/httptestsuite
go mod init example.com/httptestsuite
```

### httptest gives you a real server, not a mock

`httptest.NewServer(handler)` starts an actual HTTP server on a loopback port and
returns a `*httptest.Server`. Two members matter: `server.URL` is the base URL to
request, and `server.Client()` is an `*http.Client` preconfigured to talk to it
(and, for a TLS server, to trust its certificate). Because the client and server
are real, your test exercises the same transport, header handling, and body
lifecycle as production. The only thing that is fake is the *handler*, which is
the part you want to control. This is strictly better than a hand-written mock of
`http.Client`: there is nothing to drift out of sync with the real type.

One `httptest.Server` per case, registered for teardown with `t.Cleanup`, keeps
the cases isolated so they can run under `t.Parallel()`. `t.Cleanup(server.Close)`
runs at the end of that subtest regardless of pass or fail, so no server leaks.

### Proving the context is honored, deterministically

A checker that ignores its context is a latent goroutine leak and a missing
timeout. The naive way to test cancellation is to sleep, which is slow and flaky.
The deterministic way uses two channels and no clock: the handler signals that it
has started and then blocks on `r.Context().Done()`; a helper goroutine waits for
that signal and cancels the client's context; the blocked handler returns and the
client's `Do` fails with an error that wraps `context.Canceled`. Every step is
causally ordered by a channel, so there is no wall-clock timing and no flake. The
assertion is `errors.Is(err, context.Canceled)`.

Create `check.go`:

```go
package httptestsuite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrEmptyURL is returned when URL is called with an empty target.
var ErrEmptyURL = errors.New("url is required")

// Client is the one method of *http.Client the checker needs.
type Client interface {
	Do(*http.Request) (*http.Response, error)
}

// Result is the outcome of a single health check.
type Result struct {
	URL        string
	StatusCode int
	Duration   time.Duration
}

// URL performs a context-carrying GET and returns a structured Result. An
// empty target returns ErrEmptyURL; transport failures are wrapped with %w.
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
	_, _ = io.Copy(io.Discard, resp.Body)
	return Result{URL: rawURL, StatusCode: resp.StatusCode, Duration: time.Since(start)}, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/httptestsuite"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	res, err := httptestsuite.URL(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("status=%d\n", res.StatusCode)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=418
```

### Tests

Create `check_test.go`:

```go
package httptestsuite

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestURLStatuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		wantStatus int
	}{
		{name: "ok", statusCode: http.StatusOK, wantStatus: 200},
		{name: "created", statusCode: http.StatusCreated, wantStatus: 201},
		{name: "not found is a result", statusCode: http.StatusNotFound, wantStatus: 404},
		{name: "server error is a result", statusCode: http.StatusBadGateway, wantStatus: 502},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.statusCode)
			}))
			t.Cleanup(srv.Close)

			got, err := URL(t.Context(), srv.Client(), srv.URL)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.StatusCode != tc.wantStatus {
				t.Fatalf("StatusCode = %d, want %d", got.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestEmptyURL(t *testing.T) {
	t.Parallel()
	_, err := URL(t.Context(), http.DefaultClient, "")
	if !errors.Is(err, ErrEmptyURL) {
		t.Fatalf("err = %v, want ErrEmptyURL", err)
	}
}

// TestContextCancellation proves the request honors its context without any
// wall-clock sleep. The handler blocks until the client's context is cancelled;
// a helper goroutine cancels it only after the handler has started, so Do
// returns an error that wraps context.Canceled.
func TestContextCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()

	_, err := URL(ctx, srv.Client(), srv.URL)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
```

## Review

The suite is correct when each case is isolated (its own server, its own cleanup)
and when the assertions test identity, not text: statuses compared as integers,
errors compared with `errors.Is`. The cancellation test is the centerpiece — it
runs in microseconds and never sleeps, because a channel, not a timer, orders the
"handler started" and "context cancelled" events. If you find yourself adding a
`time.Sleep` to a concurrency test, that is the signal you are missing a
synchronization point.

Two traps. First, do not share one `httptest.Server` across parallel subtests
that need different responses; give each case its own and close it with
`t.Cleanup`. Second, do not write `tc := tc` — the Go 1.22 loop variable is
already per-iteration, and the redundant copy is a tell of code that predates the
fix. Run `go test -race` to confirm the parallel subtests share no state.

## Resources

- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewServer`, `Server.URL`, and `Server.Client`.
- [testing.T.Context](https://pkg.go.dev/testing#T.Context) — the per-test context cancelled at test end.
- [testing.T.Cleanup](https://pkg.go.dev/testing#T.Cleanup) — deterministic per-test teardown.
- [context package](https://pkg.go.dev/context) — `WithCancel` and the `context.Canceled` sentinel.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-check-package-and-error-contract.md](01-check-package-and-error-contract.md) | Next: [03-urlcheck-command-adapter.md](03-urlcheck-command-adapter.md)
