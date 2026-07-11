# Exercise 1: Per-Request Timeout Middleware (504 on Deadline)

The first artifact every HTTP service needs is a timeout boundary: a piece of
middleware that gives each request a deadline derived from the request's own
context, so a slow handler surfaces a `504 Gateway Timeout` instead of hanging a
goroutine indefinitely. This is the canonical "middleware derives, handler
consumes" pattern in its smallest honest form.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
timeoutmw/                 independent module: example.com/timeoutmw
  go.mod                   go 1.26
  timeout.go               WithTimeout(d, next) middleware
  cmd/
    demo/
      main.go              runs a slow and a fast request through the middleware
  timeout_test.go          504-on-deadline and 200-happy-path tests, an Example
```

Files: `timeout.go`, `cmd/demo/main.go`, `timeout_test.go`.
Implement: `WithTimeout(d time.Duration, next http.Handler) http.Handler` that derives `context.WithTimeout(r.Context(), d)`, defers `cancel()`, and calls the next handler via `r.WithContext(ctx)`.
Test: an `httptest.NewServer` around the middleware; a slow handler that selects on `time.After` vs `r.Context().Done()` asserts 504 on a 50 ms timeout; a fast handler asserts the happy path is untouched.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/timeoutmw/cmd/demo
cd ~/go-exercises/timeoutmw
go mod init example.com/timeoutmw
```

## The design

The middleware does exactly three things, in order: derive a child context with
the deadline off `r.Context()` (never off `Background()` — that would sever
client-disconnect and shutdown), `defer cancel()` so the timer is released the
moment the handler returns, and hand the derived request down with
`r.WithContext(ctx)`. That is the entire "derive" half.

The "consume" half lives in the handler, and it is what actually makes the
deadline real. A handler that ignores the context runs to completion regardless
of the deadline — the middleware cannot forcibly stop a running goroutine, it can
only make cancellation observable. So the slow handler `select`s: whichever of
its real work (`time.After` here, standing in for a DB call) or `r.Context().Done()`
fires first wins. When the 50 ms deadline beats the 500 ms work, the handler sees
`ctx.Done()` and writes `504`.

Note what the middleware does *not* do: it does not itself write a response on
timeout. Responsibility for the status stays with the handler, which knows what a
timed-out version of its own work should return. (Exercise 4 contrasts this with
`http.TimeoutHandler`, which does write the response for you but cannot preempt a
blocked write.)

Create `timeout.go`:

```go
package timeoutmw

import (
	"context"
	"net/http"
	"time"
)

// WithTimeout wraps next so every request runs under a context derived from
// r.Context() with the given deadline. The derived context is cancelled when
// the handler returns (defer cancel) or when d elapses, whichever comes first.
// The handler must select on r.Context().Done() to honor the deadline; this
// middleware makes cancellation observable, it cannot preempt a running
// goroutine.
func WithTimeout(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
```

## The runnable demo

The demo wires two handlers behind the middleware, drives them with an in-process
`httptest.Server`, and prints each status so you can watch a real 504 happen
against a real (short) deadline.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/timeoutmw"
)

func fetch(url string) int {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(500 * time.Millisecond):
			_, _ = io.WriteString(w, "ok")
		case <-r.Context().Done():
			http.Error(w, "timeout", http.StatusGatewayTimeout)
		}
	})
	mux.HandleFunc("/fast", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	srv := httptest.NewServer(timeoutmw.WithTimeout(50*time.Millisecond, mux))
	defer srv.Close()

	fmt.Printf("slow -> %d\n", fetch(srv.URL+"/slow"))
	fmt.Printf("fast -> %d\n", fetch(srv.URL+"/fast"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
slow -> 504
fast -> 200
```

## Tests

The two tests pin the contract from both sides. `TestReturns504OnDeadline` proves
the deadline is honored: a 50 ms timeout against a 500 ms handler yields `504`
and the timeout body, and — just as importantly — the request *returns* rather
than hanging. `TestAllowsFastRequests` proves the middleware does not break the
happy path: a fast handler still returns `200 ok`. The `Example` documents the
end-to-end 504 with verified output.

Create `timeout_test.go`:

```go
package timeoutmw

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func slowMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(500 * time.Millisecond):
			_, _ = io.WriteString(w, "ok")
		case <-r.Context().Done():
			http.Error(w, "timeout", http.StatusGatewayTimeout)
		}
	})
	return mux
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
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
	return resp.StatusCode, string(body)
}

func TestReturns504OnDeadline(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(WithTimeout(50*time.Millisecond, slowMux()))
	defer srv.Close()

	status, body := get(t, srv.URL+"/slow")
	if status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", status, http.StatusGatewayTimeout)
	}
	if body != "timeout\n" {
		t.Fatalf("body = %q, want %q", body, "timeout\n")
	}
}

func TestAllowsFastRequests(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/fast", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	srv := httptest.NewServer(WithTimeout(50*time.Millisecond, mux))
	defer srv.Close()

	status, body := get(t, srv.URL+"/fast")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if body != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
}

func Example() {
	srv := httptest.NewServer(WithTimeout(50*time.Millisecond, slowMux()))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/slow", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer resp.Body.Close()
	fmt.Println(resp.StatusCode)
	// Output: 504
}
```

## Review

The middleware is correct when it derives strictly from `r.Context()`, always
`defer cancel()`s, and never writes the response itself. The two traps are
structural: deriving from `context.Background()` (which silently breaks
client-disconnect and shutdown cancellation) and forgetting `defer cancel()`
(which leaks the timer until the deadline fires even on a fast request). The deep
point the tests encode is that the deadline is only as real as the handler's
willingness to check it: `TestReturns504OnDeadline` passes only because the slow
handler selects on `r.Context().Done()`. A handler that blocks without checking
the context would run the full 500 ms regardless — the middleware makes
cancellation *observable*, it cannot *force* it. Run with `-race` to confirm the
concurrent handler and client see no data races.

## Resources

- [`http.Request.WithContext`](https://pkg.go.dev/net/http#Request.WithContext) — replacing a request's context in middleware.
- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) — deriving a deadline and the mandatory `cancel`.
- [`net/http/httptest.NewServer`](https://pkg.go.dev/net/http/httptest#NewServer) — the in-process server the tests use.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-request-id-propagation-middleware.md](02-request-id-propagation-middleware.md)
