# Exercise 2: Rejection-to-Status Middleware: statusFor and httptest End-to-End

A decision function is only usable once something turns its typed error into a
protocol response. This module builds the `func(http.Handler) http.Handler` wrapper
that mounts the guard in production and the single `statusFor(err) int` helper that
maps each rejection cause to 401/405/415/400/500 — the only place that mapping is
allowed to live. It is proven end-to-end with `httptest.NewServer` and under
`-race` with 64 concurrent requests.

This module is fully self-contained: it re-declares the guard it needs (so it gates
alone), then adds the middleware, the mapping, and the integration tests.

## What you'll build

```text
guardmw/                    independent module: example.com/guardmw
  go.mod                    go 1.26
  guard.go                  Check + Rejection (same decision core as Exercise 1)
  middleware.go             Guard(http.Handler) http.Handler, statusFor(err) int
  cmd/
    demo/
      main.go               mounts Guard, drives it with httptest, prints statuses
  middleware_test.go        httptest.NewServer end-to-end + concurrency + statusFor table
```

- Files: `guard.go`, `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: `Guard(next http.Handler) http.Handler` and `statusFor(err error) int` using `errors.As` + a switch over `errors.Is`.
- Test: `httptest.NewServer(Guard(okHandler))` returns 202 for a valid request and the mapped status for each rejection; 64 parallel requests all return 202 under `-race`; `statusFor` maps every sentinel plus unknown to 500.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/guardmw/cmd/demo
cd ~/go-exercises/guardmw
go mod init example.com/guardmw
```

## One place for the mapping, one path for the leak

The middleware owns exactly one decision: continue or stop. It does not own the
response body, the status mapping, or the next handler. When `Check` returns an
error, `Guard` asks `statusFor` for the status, logs the full error server-side
through `slog`, and writes only `http.StatusText(status)` to the client. That split
is deliberate: the internal `Rejection.Field` and the cause sentinel are useful for
an operator reading logs, and dangerous in a response body where they leak the shape
of your validation to an attacker. The client learns "401 Unauthorized"; the log
learns which field and why.

`statusFor` is the single source of truth for the rejection-to-status mapping. It
first uses `errors.As` to recover the `Rejection`; if the error is not a `Rejection`
at all, it is an unexpected internal error and maps to 500. Otherwise a switch over
`errors.Is` maps each cause. Keeping this in one function is what makes it testable
in isolation (feed it a `Rejection`, assert the int) and what stops the mapping from
drifting as handlers are copied around a codebase.

The allowlist table is package-level and read-only. Many goroutines reading an
immutable slice never race; there is deliberately no setter. Per-instance
configuration is a dependency-injection concern for a different lesson — here the
point is that a static policy table needs no lock.

Create `guard.go` (the decision core, so this module stands alone):

```go
package guardmw

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var (
	ErrNilRequest         = errors.New("nil request")
	ErrMissingAuth        = errors.New("missing Authorization header")
	ErrEmptyAuthorization = errors.New("Authorization header is empty")
	ErrUnsupportedMethod  = errors.New("unsupported method")
	ErrInvalidTimeout     = errors.New("invalid X-Request-Timeout")
	ErrUnsupportedMedia   = errors.New("unsupported Content-Type")
)

var allowedContentTypes = []string{
	"application/json",
	"application/x-www-form-urlencoded",
}

type Rejection struct {
	Reason error
	Field  string
}

func (r Rejection) Error() string { return fmt.Sprintf("reject %s: %v", r.Field, r.Reason) }

func (r Rejection) Unwrap() error { return r.Reason }

func Check(req *http.Request) error {
	if req == nil {
		return Rejection{Reason: ErrNilRequest, Field: "request"}
	}
	if req.Method != http.MethodGet && req.Method != http.MethodPost {
		return Rejection{Reason: ErrUnsupportedMethod, Field: "method"}
	}
	if auth := strings.TrimSpace(req.Header.Get("Authorization")); auth == "" {
		if _, present := req.Header["Authorization"]; !present {
			return Rejection{Reason: ErrMissingAuth, Field: "Authorization"}
		}
		return Rejection{Reason: ErrEmptyAuthorization, Field: "Authorization"}
	}
	if raw := req.Header.Get("X-Request-Timeout"); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil || timeout <= 0 {
			return Rejection{Reason: ErrInvalidTimeout, Field: "X-Request-Timeout"}
		}
	}
	if hasBody, contentType := requestHasBody(req); hasBody && !isAllowedContentType(contentType) {
		return Rejection{Reason: ErrUnsupportedMedia, Field: "Content-Type"}
	}
	return nil
}

func requestHasBody(req *http.Request) (bool, string) {
	if req.ContentLength == 0 {
		return false, ""
	}
	return true, req.Header.Get("Content-Type")
}

func isAllowedContentType(contentType string) bool {
	if contentType == "" {
		return false
	}
	if i := strings.Index(contentType, ";"); i >= 0 {
		contentType = contentType[:i]
	}
	contentType = strings.TrimSpace(strings.ToLower(contentType))
	for _, allowed := range allowedContentTypes {
		if strings.EqualFold(allowed, contentType) {
			return true
		}
	}
	return false
}
```

Create `middleware.go`:

```go
package guardmw

import (
	"errors"
	"log/slog"
	"net/http"
)

// Guard is the middleware you mount in production. It runs Check, and on a
// rejection maps the cause to a status, logs the full error, and writes only the
// status text to the client so internal field names never leak.
func Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := Check(r); err != nil {
			status := statusFor(err)
			slog.Warn("request rejected",
				"status", status,
				"method", r.Method,
				"path", r.URL.Path,
				"err", err,
			)
			http.Error(w, http.StatusText(status), status)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusFor is the single source of truth for the rejection-to-status mapping.
func statusFor(err error) int {
	var rej Rejection
	if !errors.As(err, &rej) {
		return http.StatusInternalServerError
	}
	switch {
	case errors.Is(rej.Reason, ErrMissingAuth), errors.Is(rej.Reason, ErrEmptyAuthorization):
		return http.StatusUnauthorized
	case errors.Is(rej.Reason, ErrUnsupportedMethod):
		return http.StatusMethodNotAllowed
	case errors.Is(rej.Reason, ErrUnsupportedMedia):
		return http.StatusUnsupportedMediaType
	case errors.Is(rej.Reason, ErrInvalidTimeout):
		return http.StatusBadRequest
	case errors.Is(rej.Reason, ErrNilRequest):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
```

### The runnable demo

The demo mounts `Guard` on an `httptest.NewServer` and drives it with a valid and
an invalid request, printing the status codes so you can see the mapping in action.
It silences the guard's `slog` output so the demo prints only the two status lines.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"

	"example.com/guardmw"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(guardmw.Guard(ok))
	defer srv.Close()

	valid, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"x":1}`))
	valid.Header.Set("Authorization", "Bearer token-123")
	valid.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(valid)
	fmt.Printf("valid POST -> %d\n", resp.StatusCode)
	resp.Body.Close()

	noAuth, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, _ = http.DefaultClient.Do(noAuth)
	fmt.Printf("no auth   -> %d\n", resp.StatusCode)
	resp.Body.Close()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid POST -> 202
no auth   -> 401
```

### Tests

`TestGuardAllowsValidRequest` proves the happy path passes through to the wrapped
handler (202). `TestGuardRejectsWithMappedStatus` table-drives the mapped statuses
through a real server. `TestGuardIsSafeUnderConcurrentRequests` fires 64 requests in
parallel through `t.Context()` and asserts every one is 202 — the test that makes
`-race` meaningful, so any future shared mutable state in the guard is caught.
`TestStatusForMapsRejectionToStatus` maps each sentinel directly, including an
unknown error to 500. `TestMain` redirects `slog` to `io.Discard` so the
deliberately-triggered rejections do not clutter test output.

Create `middleware_test.go`:

```go
package guardmw

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "ok")
	})
}

func TestGuardAllowsValidRequest(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(Guard(okHandler()))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader(`{"x":1}`))
	req.Header.Set("Authorization", "Bearer token-123")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
}

func TestGuardRejectsWithMappedStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(Guard(okHandler()))
	t.Cleanup(srv.Close)

	tests := []struct {
		name       string
		method     string
		auth       string
		timeout    string
		contentT   string
		body       string
		wantStatus int
	}{
		{"missing auth 401", http.MethodGet, "", "", "", "", http.StatusUnauthorized},
		{"empty auth 401", http.MethodGet, " ", "", "", "", http.StatusUnauthorized},
		{"delete 405", http.MethodDelete, "Bearer x", "", "", "", http.StatusMethodNotAllowed},
		{"bad timeout 400", http.MethodGet, "Bearer x", "soon", "", "", http.StatusBadRequest},
		{"bad media 415", http.MethodPost, "Bearer x", "", "text/xml", "<x/>", http.StatusUnsupportedMediaType},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var req *http.Request
			if tc.body != "" {
				req, _ = http.NewRequestWithContext(t.Context(), tc.method, srv.URL, strings.NewReader(tc.body))
			} else {
				req, _ = http.NewRequestWithContext(t.Context(), tc.method, srv.URL, nil)
			}
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			if tc.timeout != "" {
				req.Header.Set("X-Request-Timeout", tc.timeout)
			}
			if tc.contentT != "" {
				req.Header.Set("Content-Type", tc.contentT)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d (body=%s)", resp.StatusCode, tc.wantStatus, body)
			}
		})
	}
}

func TestGuardIsSafeUnderConcurrentRequests(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(Guard(okHandler()))
	t.Cleanup(srv.Close)

	const requests = 64
	var wg sync.WaitGroup
	statuses := make([]int, requests)
	errs := make([]error, requests)
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader(`{"x":1}`))
			req.Header.Set("Authorization", "Bearer token-123")
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs[i] = err
				return
			}
			defer resp.Body.Close()
			statuses[i] = resp.StatusCode
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	for i, status := range statuses {
		if status != http.StatusAccepted {
			t.Fatalf("request %d: status = %d, want 202", i, status)
		}
	}
}

func TestStatusForMapsRejectionToStatus(t *testing.T) {
	t.Parallel()
	tests := map[error]int{
		ErrMissingAuth:        http.StatusUnauthorized,
		ErrEmptyAuthorization: http.StatusUnauthorized,
		ErrUnsupportedMethod:  http.StatusMethodNotAllowed,
		ErrUnsupportedMedia:   http.StatusUnsupportedMediaType,
		ErrInvalidTimeout:     http.StatusBadRequest,
		ErrNilRequest:         http.StatusBadRequest,
		errors.New("unknown"): http.StatusInternalServerError,
	}
	for sentinel, want := range tests {
		if got := statusFor(Rejection{Reason: sentinel, Field: "x"}); got != want {
			t.Fatalf("statusFor(%v) = %d, want %d", sentinel, got, want)
		}
	}
	// A non-Rejection error maps to 500 as well.
	if got := statusFor(errors.New("bare")); got != http.StatusInternalServerError {
		t.Fatalf("statusFor(bare) = %d, want 500", got)
	}
}
```

## Review

The middleware is correct when the client never sees more than `http.StatusText`
and every rejection cause maps to its status in exactly one place. The end-to-end
test with `httptest.NewServer` is what proves usability: it drives the mapping the
same way the production server will. The concurrency test plus `-race` is what keeps
the guard honest as it grows — the day someone adds an unsynchronized counter, that
test flags it. The mistakes to avoid are scattering the status mapping across
handlers (untestable, drifts), leaking `err.Error()` into the response body, and
adding a mutable package-level table with a setter that a handler reads under load.

## Resources

- [net/http Handler interface](https://pkg.go.dev/net/http#Handler)
- [net/http/httptest.NewServer](https://pkg.go.dev/net/http/httptest#NewServer)
- [errors.As](https://pkg.go.dev/errors#As)
- [log/slog](https://pkg.go.dev/log/slog)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-request-guard-check.md](01-request-guard-check.md) | Next: [03-env-config-loader.md](03-env-config-loader.md)
