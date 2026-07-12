# Exercise 3: Context-Aware Client Fetch Helper

The outbound half of context discipline: a `FetchWithTimeout(ctx, client, url)`
helper that builds its request with `http.NewRequestWithContext` (never
`http.NewRequest`), executes it, fully drains and closes the body, and surfaces
`context.DeadlineExceeded` when the caller's deadline expires before the server
responds. This is the shape every resilient client wraps.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
clientfetch/               independent module: example.com/clientfetch
  go.mod                   go 1.26
  fetch.go                 FetchWithTimeout(ctx, client, url) (int, string, error)
  cmd/
    demo/
      main.go              a fast fetch and a deadline-exceeded fetch
  fetch_test.go            deadline test, happy-path test, an Example
```

Files: `fetch.go`, `cmd/demo/main.go`, `fetch_test.go`.
Implement: `FetchWithTimeout(ctx context.Context, client *http.Client, url string) (int, string, error)` using `http.NewRequestWithContext`, `client.Do`, `io.ReadAll`, and `defer resp.Body.Close()`.
Test: a slow handler + a 30 ms caller context asserts `errors.Is(err, context.DeadlineExceeded)`; a fast handler asserts `200`+body; plus the shipped `TestClientTimeoutReturnsDeadline` (1 ms context vs 50 ms handler).
Verify: `go test -count=1 -race ./...`

## The design

`http.NewRequest` hardcodes `context.Background()` into the request, so a deadline
you compute never reaches the round trip — the dial, TLS handshake, header wait,
and body read all run uncancellable. `http.NewRequestWithContext(ctx, ...)` is the
only constructor whose deadline the transport honors. If a helper accepts a `ctx`
and then calls `NewRequest`, the `ctx` is a lie; a reviewer should reject it.

Two body-handling details are load-bearing. First, `defer resp.Body.Close()` runs
only when `client.Do` returned a non-nil response and a nil error; on an error
`resp` is nil and closing it would panic, so the close is placed after the error
check. Second, the helper reads the body to completion with `io.ReadAll` before
returning. Draining and closing the body is what lets the underlying connection
be reused from the pool; abandoning a half-read body leaks the connection. (In a
larger client you would also cap this read with `MaxBytesReader` — see
Exercise 9 — but the shape here keeps the focus on context.)

When the caller's context expires mid-flight, `client.Do` returns a `*url.Error`
whose chain includes `context.DeadlineExceeded`; `errors.Is` sees through the
wrapping. The helper returns that error untouched so the caller can classify it.

Create `fetch.go`:

```go
package clientfetch

import (
	"context"
	"io"
	"net/http"
)

// FetchWithTimeout performs an HTTP GET against url under ctx. It builds the
// request with http.NewRequestWithContext so the caller's deadline and
// cancellation reach the round trip, drains and closes the body, and returns
// the status code and body. When ctx expires before the server responds, the
// returned error's chain contains context.DeadlineExceeded (see errors.Is).
func FetchWithTimeout(ctx context.Context, client *http.Client, url string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(body), nil
}
```

## The runnable demo

The demo runs two fetches against an in-process server: one with a generous
context (returns `200 pong`) and one whose 20 ms context expires before the
handler's 200 ms sleep completes (returns a `DeadlineExceeded` error).

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/clientfetch"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
			_, _ = w.Write([]byte("late"))
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{}

	status, body, err := clientfetch.FetchWithTimeout(context.Background(), client, srv.URL+"/ping")
	fmt.Printf("fast: status=%d body=%s err=%v\n", status, body, err)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err = clientfetch.FetchWithTimeout(ctx, client, srv.URL+"/slow")
	fmt.Printf("slow: deadline-exceeded=%v\n", errors.Is(err, context.DeadlineExceeded))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast: status=200 body=pong err=<nil>
slow: deadline-exceeded=true
```

## Tests

`TestReturnsDeadlineExceeded` is the central contract: a 30 ms caller context
against a 500 ms handler must fail with `context.DeadlineExceeded` (asserted via
`errors.Is`, which sees through the `*url.Error` wrapper). `TestHappyPath` proves
a fast fetch returns `200` and the exact body. `TestClientTimeoutReturnsDeadline`
is the previously-"your turn" case now shipped: a 1 ms context against a 50 ms
handler. The `Example` documents a successful fetch with verified output.

Create `fetch_test.go`:

```go
package clientfetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func slowServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(500 * time.Millisecond):
			_, _ = w.Write([]byte("late"))
		case <-r.Context().Done():
		}
	})
	return httptest.NewServer(mux)
}

func TestReturnsDeadlineExceeded(t *testing.T) {
	t.Parallel()
	srv := slowServer(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	status, _, err := FetchWithTimeout(ctx, &http.Client{}, srv.URL+"/slow")
	if err == nil {
		t.Fatalf("expected error from deadline, got nil (status=%d)", status)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestClientTimeoutReturnsDeadline(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(50 * time.Millisecond):
			_, _ = w.Write([]byte("late"))
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	_, _, err := FetchWithTimeout(ctx, &http.Client{}, srv.URL+"/")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestHappyPath(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	status, body, err := FetchWithTimeout(context.Background(), &http.Client{}, srv.URL+"/ping")
	if err != nil {
		t.Fatalf("FetchWithTimeout: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body != "pong" {
		t.Fatalf("body = %q, want pong", body)
	}
}

func Example() {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	status, body, err := FetchWithTimeout(context.Background(), &http.Client{}, srv.URL+"/ping")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(status, body)
	// Output: 200 pong
}
```

## Review

The helper is correct when it uses `http.NewRequestWithContext` (so the deadline
is real), closes the body only after the error check (so it never dereferences a
nil `resp`), and drains the body before returning (so the connection returns to
the pool). The two failure modes it defends against are the silent one — a
`ctx` that never reaches the round trip because someone reached for `NewRequest`
— and the leak — an unread body that pins a connection. `errors.Is` is the right
comparison because `client.Do` wraps the cause in a `*url.Error`; a `==` check
against `context.DeadlineExceeded` would miss it. Run with `-race` to confirm the
concurrent tests share no state.

## Resources

- [`http.NewRequestWithContext`](https://pkg.go.dev/net/http#NewRequestWithContext) — the only context-honoring request constructor.
- [`http.Client.Do`](https://pkg.go.dev/net/http#Client.Do) — and its documented body-close / connection-reuse contract.
- [`context.DeadlineExceeded`](https://pkg.go.dev/context#pkg-variables) — the sentinel `errors.Is` matches through the `*url.Error` wrapper.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-request-id-propagation-middleware.md](02-request-id-propagation-middleware.md) | Next: [04-timeouthandler-and-write-deadline.md](04-timeouthandler-and-write-deadline.md)
