# Exercise 9: HTTP middleware with an optional nil audit hook

An `http.Handler` middleware takes an optional `AuditHook`. In tests and
low-tier environments the hook is nil; the middleware normalizes it to a
`noopHook` at construction so the request path never branches on nil. This is
boundary normalization from Exercise 1, in the concrete shape of `net/http`
middleware, with a status-capturing `ResponseWriter`.

## What you'll build

```text
auditmw/                   independent module: example.com/auditmw
  go.mod                   go 1.26
  middleware.go            AuditHook; noopHook; statusRecorder; Audit
  cmd/
    demo/
      main.go              wire a printing hook; serve one request
  middleware_test.go       nil hook serves; spy hook records one req/resp with status
```

- Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: an `AuditHook` interface (`OnRequest`/`OnResponse`), a `noopHook`, a `statusRecorder` wrapping `http.ResponseWriter`, and `Audit(hook AuditHook, next http.Handler) http.Handler` that maps nil to `noopHook{}`.
- Test: with a nil hook the middleware serves the request without panic; with a spy hook it records exactly one `OnRequest`/`OnResponse` per call and reports the response status.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/auditmw/cmd/demo
cd ~/go-exercises/auditmw
go mod init example.com/auditmw
```

### Why normalize the hook, and why wrap the ResponseWriter

`Audit` returns a handler that calls `hook.OnRequest` before the request and
`hook.OnResponse` after. If the hook were an optional nil interface, the request
path would need `if hook != nil` around both calls, and a forgotten guard is a
panic on every request. Normalizing `nil -> noopHook{}` once in `Audit` makes
the returned closure branch-free: it always calls the hook, and when there is no
real hook the calls are no-ops.

`OnResponse` needs the status code, but `http.ResponseWriter` does not expose the
code that was written. The standard technique is a wrapper: `statusRecorder`
embeds the real `http.ResponseWriter` and overrides `WriteHeader` to remember the
code before delegating. It defaults to `http.StatusOK`, because a handler that
writes a body without ever calling `WriteHeader` implicitly sends 200 — matching
`net/http`'s own behavior. After `next.ServeHTTP(rec, r)` returns, `rec.status`
holds the code the handler produced, and the middleware passes it to
`OnResponse`.

Create `middleware.go`:

```go
package auditmw

import "net/http"

// AuditHook observes requests and responses. It is optional: production wires a
// real hook, tests and low-tier environments pass nil.
type AuditHook interface {
	OnRequest(r *http.Request)
	OnResponse(r *http.Request, status int)
}

// noopHook is the Null Object: it satisfies AuditHook with no-op methods.
type noopHook struct{}

func (noopHook) OnRequest(r *http.Request) {}

func (noopHook) OnResponse(r *http.Request, status int) {}

// statusRecorder captures the status code written by the wrapped handler.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Audit wraps next with request/response auditing. A nil hook is normalized to
// noopHook{} so the request path never branches on nil.
func Audit(hook AuditHook, next http.Handler) http.Handler {
	if hook == nil {
		hook = noopHook{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hook.OnRequest(r)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		hook.OnResponse(r, rec.status)
	})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/auditmw"
)

// printHook logs each request and response to stdout.
type printHook struct{}

func (printHook) OnRequest(r *http.Request) {
	fmt.Printf("audit: request %s %s\n", r.Method, r.URL.Path)
}

func (printHook) OnResponse(r *http.Request, status int) {
	fmt.Printf("audit: response %s %s -> %d\n", r.Method, r.URL.Path, status)
}

func main() {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintln(w, "created")
	})

	h := auditmw.Audit(printHook{}, handler)

	req := httptest.NewRequest(http.MethodPost, "/users", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	fmt.Println("final status:", rec.Code)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
audit: request POST /users
audit: response POST /users -> 201
final status: 201
```

### Tests

Create `middleware_test.go`:

```go
package auditmw

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// spyHook records how many times each callback fired and the last status.
type spyHook struct {
	mu         sync.Mutex
	requests   int
	responses  int
	lastStatus int
}

func (s *spyHook) OnRequest(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
}

func (s *spyHook) OnResponse(r *http.Request, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses++
	s.lastStatus = status
}

func newOKHandler(code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	})
}

func TestNilHookServesWithoutPanic(t *testing.T) {
	t.Parallel()

	h := Audit(nil, newOKHandler(http.StatusOK))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
}

func TestSpyHookRecordsOnePerCall(t *testing.T) {
	t.Parallel()

	spy := &spyHook{}
	h := Audit(spy, newOKHandler(http.StatusCreated))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/users", nil)

	h.ServeHTTP(rec, req)

	spy.mu.Lock()
	defer spy.mu.Unlock()
	if spy.requests != 1 {
		t.Fatalf("OnRequest fired %d times; want 1", spy.requests)
	}
	if spy.responses != 1 {
		t.Fatalf("OnResponse fired %d times; want 1", spy.responses)
	}
	if spy.lastStatus != http.StatusCreated {
		t.Fatalf("captured status = %d; want 201", spy.lastStatus)
	}
}

func TestDefaultStatusIsOK(t *testing.T) {
	t.Parallel()

	spy := &spyHook{}
	// A handler that writes a body without WriteHeader implicitly sends 200.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hi"))
	})
	h := Audit(spy, handler)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	spy.mu.Lock()
	defer spy.mu.Unlock()
	if spy.lastStatus != http.StatusOK {
		t.Fatalf("default status = %d; want 200", spy.lastStatus)
	}
}
```

## Review

The middleware is correct when the request path never branches on nil and the
captured status matches what the handler wrote. `Audit` normalizes a nil hook to
`noopHook{}`, so `TestNilHookServesWithoutPanic` serves cleanly with no hook
wired. The `statusRecorder` overrides `WriteHeader` to remember the code:
`TestSpyHookRecordsOnePerCall` sees exactly one `OnRequest`/`OnResponse` and the
201 the handler wrote, and `TestDefaultStatusIsOK` confirms the 200 default when
the handler writes a body without `WriteHeader`. The mistake to avoid is the
optional hook as a nil interface guarded at each call site; one missed guard
panics on a live request. Note also that the `statusRecorder` default must be
200, or a handler that never calls `WriteHeader` would report a bogus zero
status to the hook.

## Resources

- [`net/http` Handler and middleware](https://pkg.go.dev/net/http#Handler) — the `Handler`/`HandlerFunc` contract this middleware wraps.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRecorder` and `NewRequest` for driving handlers in tests.
- [`http.ResponseWriter`](https://pkg.go.dev/net/http#ResponseWriter) — why capturing the status needs a wrapper.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../08-accept-interfaces-return-structs/00-concepts.md](../08-accept-interfaces-return-structs/00-concepts.md)
