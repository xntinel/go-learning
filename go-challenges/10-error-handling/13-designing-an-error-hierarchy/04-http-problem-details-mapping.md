# Exercise 4: Mapping Error Categories to HTTP Status and RFC 7807 Bodies

The transport edge is where domain categories become HTTP. This exercise builds an
`httptest`-backed handler that maps `ErrUserNotFound` to 404, `ErrUserExists` to
409, `ErrUserInvalid` to 422, and everything else to 500 — writing an
`application/problem+json` body per RFC 7807, and hiding the internal error from
the client on 500 while logging it.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
http-problem-details/              module example.com/http-problem-details
  go.mod
  api.go                           domain categories; Problem; classify(); WriteProblem(); Handler
  cmd/demo/main.go                 run each case through httptest, print status + body
  api_test.go                      status per category; problem+json content-type; 500 hides internal
```

- Files: `api.go`, `cmd/demo/main.go`, `api_test.go`.
- Implement: a `classify(err)` that maps categories to `(status, title)` via `errors.Is` with a 500 default, a `WriteProblem` that renders an RFC 7807 body and hides internal detail on 500, and a `Handler` that runs a service call and translates the result.
- Test: a recorder per case asserting the status code, the `application/problem+json` content-type, that the 500 body does not contain the internal error text while 4xx carry a stable title, and that an unmapped error defaults to 500.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/http-problem-details/cmd/demo
cd ~/go-exercises/http-problem-details
go mod init example.com/http-problem-details
```

### Why errors.Is and not a type switch

The temptation at the transport is a `switch e := err.(type)` over concrete error
types. Resist it. A type switch couples the handler to every concrete type the
domain might return, and — worse — a new error type that no `case` names falls into
`default` silently, so a freshly added failure maps to 500 (or nothing) without a
compile error to warn you. `classify` instead branches with `errors.Is` on stable
*categories*. Those matches survive wrapping (a service that annotates the repo's
error with `%w` still matches), and the `default` is an explicit, intentional 500
rather than an accident of the switch.

The 500 branch carries the security-critical rule. A domain error may contain SQL
text, a file path, a driver message like `password authentication failed for user
"admin"` — none of which a client should ever see. So `WriteProblem` splits the
audiences: on any status ≥ 500 it logs the real error (for operators) and replaces
the client-facing `detail` with a generic string, while on 4xx the `detail` is the
stable, deliberately human-safe title. RFC 7807's `application/problem+json` gives
the body a predictable shape — `type`, `title`, `status`, `detail` — that clients
can parse uniformly regardless of which error occurred.

The `Handler` is a thin adapter: it runs a `Do` function (standing in for a service
call) and, on error, hands off to `WriteProblem`. Injecting `Do` as a field keeps
the exercise focused on the mapping and lets each test drive a specific category
without a real service.

Create `api.go`:

```go
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// Domain categories the transport maps. They arrive already translated by lower
// layers; the handler only branches on them, never on concrete storage types.
var (
	ErrDomain       = errors.New("domain error")
	ErrUserNotFound = fmt.Errorf("user: not found: %w", ErrDomain)
	ErrUserExists   = fmt.Errorf("user: already exists: %w", ErrDomain)
	ErrUserInvalid  = fmt.Errorf("user: invalid: %w", ErrDomain)
)

// Problem is an RFC 7807 application/problem+json body.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// classify maps a domain category to a status and a stable, client-safe title.
// Unknown errors default to 500 so a new error can never silently map to nothing.
func classify(err error) (status int, title string) {
	switch {
	case errors.Is(err, ErrUserNotFound):
		return http.StatusNotFound, "user not found"
	case errors.Is(err, ErrUserExists):
		return http.StatusConflict, "user already exists"
	case errors.Is(err, ErrUserInvalid):
		return http.StatusUnprocessableEntity, "user invalid"
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

// WriteProblem renders err as a problem+json response. On 500 it hides the
// internal error from the client and logs it instead; on 4xx the detail is the
// stable title, safe to show.
func WriteProblem(w http.ResponseWriter, log *slog.Logger, err error) {
	status, title := classify(err)

	detail := title
	if status >= 500 {
		log.Error("request failed", "status", status, "err", err)
		detail = "an internal error occurred"
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{
		Type:   "about:blank",
		Title:  title,
		Status: status,
		Detail: detail,
	})
}

// Handler runs a service call and translates its result into HTTP.
type Handler struct {
	Log *slog.Logger
	Do  func(r *http.Request) error
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.Do(r); err != nil {
		WriteProblem(w, h.Log, err)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"ok":true}`)
}
```

### The runnable demo

The demo runs four cases through `httptest` — the three mapped categories and one
leaky internal error whose message names a database user — and prints the status
and body. Watch the 500 body: it says nothing about the real error.

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

	"example.com/http-problem-details"
)

func main() {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cases := []struct {
		name string
		err  error
	}{
		{"not found", api.ErrUserNotFound},
		{"conflict", api.ErrUserExists},
		{"invalid", api.ErrUserInvalid},
		{"leaky internal", fmt.Errorf(`pq: password authentication failed for user "admin"`)},
	}

	for _, c := range cases {
		h := api.Handler{Log: log, Do: func(*http.Request) error { return c.err }}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/users/1", nil))
		body, _ := io.ReadAll(rec.Result().Body)
		fmt.Printf("%-14s %d %s\n", c.name, rec.Code, strings.TrimSpace(string(body)))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
not found      404 {"type":"about:blank","title":"user not found","status":404,"detail":"user not found"}
conflict       409 {"type":"about:blank","title":"user already exists","status":409,"detail":"user already exists"}
invalid        422 {"type":"about:blank","title":"user invalid","status":422,"detail":"user invalid"}
leaky internal 500 {"type":"about:blank","title":"internal error","status":500,"detail":"an internal error occurred"}
```

### Tests

Each case gets a `httptest.NewRecorder`. The status and content-type assertions pin
the mapping; the leak assertion is the security one — the 500 body must not contain
the internal error's text — and the unmapped-error case proves the `default`
branch is a real 500, not an accident.

Create `api_test.go`:

```go
package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func run(t *testing.T, err error) *httptest.ResponseRecorder {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := Handler{Log: log, Do: func(*http.Request) error { return err }}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/users/1", nil))
	return rec
}

func TestCategoryStatusMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		status int
		title  string
	}{
		{"not found", ErrUserNotFound, http.StatusNotFound, "user not found"},
		{"exists", ErrUserExists, http.StatusConflict, "user already exists"},
		{"invalid", ErrUserInvalid, http.StatusUnprocessableEntity, "user invalid"},
		{"unmapped", errors.New("kaboom"), http.StatusInternalServerError, "internal error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := run(t, tc.err)
			if rec.Code != tc.status {
				t.Errorf("status = %d; want %d", rec.Code, tc.status)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type = %q; want application/problem+json", ct)
			}
			if !strings.Contains(rec.Body.String(), tc.title) {
				t.Errorf("body %q does not contain title %q", rec.Body.String(), tc.title)
			}
		})
	}
}

func TestInternalErrorIsNotLeaked(t *testing.T) {
	t.Parallel()
	secret := "password authentication failed for user admin"
	rec := run(t, errors.New(secret))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("500 body leaked internal error: %q", rec.Body.String())
	}
}

func TestWrappedCategoryStillMaps(t *testing.T) {
	t.Parallel()
	// A service that annotated the repo error with %w must still map to 404.
	wrapped := errors.Join(errors.New("service.load"), ErrUserNotFound)
	rec := run(t, wrapped)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 for wrapped ErrUserNotFound", rec.Code)
	}
}
```

## Review

The mapping is correct when every known category produces its status and stable
title, an unknown error produces a 500, and — the assertion that matters most — the
500 body carries none of the internal error's text while the log does. Use
`errors.Is` on categories, never a type switch on concrete types: `Is` matches
through the `%w` wrapping that lower layers add, and the `default` is an explicit
500 rather than a silent gap. The RFC 7807 shape is not decoration; a fixed
`type`/`title`/`status`/`detail` envelope lets clients parse failures uniformly. If
you later need machine-readable codes on the envelope for clients to branch on, that
is the `Coder` interface of Exercise 8 — the status is for humans and proxies, the
code is for programs.

## Resources

- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRecorder`/`NewRequest` for handler tests.
- [RFC 7807 — Problem Details for HTTP APIs](https://www.rfc-editor.org/rfc/rfc7807) — the `application/problem+json` body shape.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — category matching that survives wrapping.

---

Back to [03-repository-error-translation.md](03-repository-error-translation.md) | Next: [05-aggregate-validation-errors.md](05-aggregate-validation-errors.md)
