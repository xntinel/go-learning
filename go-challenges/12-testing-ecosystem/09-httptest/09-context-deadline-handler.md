# Exercise 9: Test context deadline and cancellation propagation

A handler that calls a slow dependency must not keep working after the client has
gone or the deadline has passed — it should short-circuit and return `503`. This
is directly testable: feed the handler a request whose context is already canceled
or carries a short deadline, and assert it returns early without doing (or
finishing) the downstream call. This module builds such a handler and tests all
three cases, including a real client that cancels mid-flight.

## What you'll build

```text
deadlinehandler/                independent module: example.com/context-deadline-handler
  go.mod                        go 1.26
  deadline.go                   Dependency interface; Handler returning 503 on ctx cancel/deadline
  cmd/
    demo/
      main.go                   normal 200 vs already-canceled 503, counting dependency calls
  deadline_test.go              canceled -> 503 + zero calls; deadline -> early 503; server cancel mid-flight
```

- Files: `deadline.go`, `cmd/demo/main.go`, `deadline_test.go`.
- Implement: `Handler(dep Dependency)` that returns `503` if `r.Context()` is already done (without calling `dep`), and `503` if the dependency returns because the deadline/cancel fired.
- Test: (a) an already-canceled context yields `503` and a spy dependency records zero calls; (b) a short deadline against a slow dependency returns before the dependency's own work completes; (c) via `httptest.NewServer`, a client that cancels its context makes the server-side handler observe `ctx.Done`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/09-httptest/09-context-deadline-handler/cmd/demo
cd go-solutions/12-testing-ecosystem/09-httptest/09-context-deadline-handler
```

### r.Context() is the cancellation signal

Every request carries a context via `r.Context()`. The server cancels it when the
client disconnects, and if you wrapped the request with a deadline
(`http.TimeoutHandler` or your own middleware) it is canceled when that deadline
passes. A handler that does meaningful downstream work has two obligations. First,
*check before starting*: if `r.Context().Err()` is already non-nil, the caller has
given up — return `503` immediately and do not touch the dependency. Second,
*propagate the context into the call*: pass `r.Context()` to `dep.Fetch(ctx)` so a
cancellation or deadline that fires mid-call aborts the work instead of running to
completion. When the dependency returns a `context.Canceled`/`DeadlineExceeded`,
the handler maps it to `503` (service unavailable) rather than a generic `500`.

The tests make each obligation observable with a *spy* dependency that counts calls
and records whether it saw a cancellation. Case (a) proves the early check: an
already-canceled request must produce `503` with *zero* dependency calls. Case (b)
proves propagation: a slow dependency under a short deadline returns early (via
`ctx.Done`) rather than finishing its multi-second work. Case (c) proves the wire:
a real client that cancels mid-request causes the server-side handler's context to
fire, which the spy observes.

Because these tests are about cancellation, base their contexts on `t.Context()`
(Go 1.24+), which is canceled automatically when the test ends — so a stuck
goroutine cannot outlive the test.

Create `deadline.go`:

```go
package deadline

import (
	"context"
	"errors"
	"net/http"
)

// Dependency is a downstream call the handler makes. Implementations must honor
// the passed context.
type Dependency interface {
	Fetch(ctx context.Context) (string, error)
}

// Handler serves the result of dep.Fetch, but short-circuits to 503 when the
// request context is already canceled or is canceled/expires during the call.
func Handler(dep Dependency) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if ctx.Err() != nil {
			// Caller already gave up: do no downstream work.
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}

		result, err := dep.Fetch(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(result))
	}
}
```

### The demo

The demo runs the handler normally (200) and with an already-canceled context
(503), and prints the dependency's call count to show the canceled path did no
work.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/context-deadline-handler"
)

type stubDep struct {
	result string
	calls  int
}

func (d *stubDep) Fetch(ctx context.Context) (string, error) {
	d.calls++
	return d.result, nil
}

func main() {
	dep := &stubDep{result: "payload"}
	h := deadline.Handler(dep)

	// Normal request.
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Printf("normal: status=%d body=%s\n", rec.Code, rec.Body.String())

	// Already-canceled request.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec2 := httptest.NewRecorder()
	h(rec2, httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil))
	fmt.Printf("canceled: status=%d\n", rec2.Code)

	fmt.Printf("dependency calls: %d\n", dep.calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
normal: status=200 body=payload
canceled: status=503
dependency calls: 1
```

### Tests

The spy dependency counts calls and, for a nonzero delay, blocks on a `select`
between its work timer and `ctx.Done`, recording whether cancellation won. Case (a)
uses an already-canceled context and asserts `503` with zero calls. Case (b) uses a
20 ms deadline against a 5 s dependency and asserts an early `503`. Case (c) runs a
real server and cancels the client context, polling until the server-side spy
records the cancellation.

Create `deadline_test.go`:

```go
package deadline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type spyDep struct {
	mu       sync.Mutex
	calls    int
	canceled bool
	delay    time.Duration
	result   string
}

func (d *spyDep) Fetch(ctx context.Context) (string, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()

	if d.delay == 0 {
		return d.result, nil
	}
	select {
	case <-time.After(d.delay):
		return d.result, nil
	case <-ctx.Done():
		d.mu.Lock()
		d.canceled = true
		d.mu.Unlock()
		return "", ctx.Err()
	}
}

func (d *spyDep) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *spyDep) sawCancel() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.canceled
}

func TestAlreadyCanceledDoesNoWork(t *testing.T) {
	t.Parallel()

	dep := &spyDep{result: "data"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	Handler(dep)(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if dep.count() != 0 {
		t.Fatalf("dependency calls = %d, want 0 (handler must short-circuit)", dep.count())
	}
}

func TestDeadlineReturnsEarly(t *testing.T) {
	t.Parallel()

	dep := &spyDep{delay: 5 * time.Second, result: "data"}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	start := time.Now()
	Handler(dep)(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if elapsed > time.Second {
		t.Fatalf("handler took %v, want it to return near the 20ms deadline", elapsed)
	}
	if !dep.sawCancel() {
		t.Fatal("dependency did not observe the deadline")
	}
}

func TestServerSeesClientCancel(t *testing.T) {
	t.Parallel()

	dep := &spyDep{delay: 5 * time.Second, result: "data"}
	srv := httptest.NewServer(Handler(dep))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	resp, err := srv.Client().Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("Do returned nil error, want a cancellation error")
	}

	// The server-side handler should observe the canceled context.
	deadlineAt := time.Now().Add(2 * time.Second)
	for !dep.sawCancel() {
		if time.Now().After(deadlineAt) {
			t.Fatal("server handler did not observe client cancellation")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if dep.count() != 1 {
		t.Fatalf("dependency calls = %d, want 1", dep.count())
	}
}
```

As a "your turn" addition, add a case where the dependency returns a non-context
error and assert the handler maps it to `502` (`StatusBadGateway`), not `503`.

## Review

The handler has two duties and each has its own test. The early `ctx.Err()` check
is what makes case (a) pass with *zero* downstream calls — a handler that skipped it
would call the dependency even though the client already gave up, wasting work and,
worse, possibly triggering a side effect nobody will observe. Passing `r.Context()`
into `dep.Fetch` is what makes cases (b) and (c) pass — the deadline or client
disconnect aborts the in-flight call rather than letting it run to completion. The
spy dependency's call count and cancel flag turn both duties into concrete
assertions, and the mutex keeps them race-clean when the server goroutine touches
them. Basing every context on `t.Context()` guarantees nothing outlives the test.

## Resources

- [context package](https://go.dev/blog/context) — cancellation and deadline propagation across API boundaries.
- [net/http `Request.Context`](https://pkg.go.dev/net/http#Request.Context) — the request's cancellation signal.
- [httptest `NewRequestWithContext`](https://pkg.go.dev/net/http/httptest#NewRequestWithContext) — feeding a handler a request with a specific context.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-sse-streaming-flush.md](08-sse-streaming-flush.md) | Next: [10-httptrace-connection-reuse.md](10-httptrace-connection-reuse.md)
