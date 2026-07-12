# Exercise 2: Map sentinel errors to HTTP status and a JSON body

The boundary's job is to turn a Go error into the right HTTP status and a
machine-readable body. This exercise builds `WriteError`: an `errors.Is` switch
that maps package sentinels to statuses, sets `Content-Type: application/json`,
and encodes a small `ErrorResponse`. Because handlers wrap sentinels with
`fmt.Errorf("...: %w", ...)`, the mapping must survive arbitrary wrapping.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
sentinelmap/                 independent module: example.com/sentinelmap
  go.mod                     go 1.26
  api.go                     sentinels; ErrorResponse; WriteError (errors.Is switch); Handler + WithError
  cmd/
    demo/
      main.go                runnable demo: each sentinel through the boundary
  api_test.go                table test (wrapped sentinel -> status), doubly-wrapped case
```

Files: `api.go`, `cmd/demo/main.go`, `api_test.go`.
Implement: `ErrNotFound`/`ErrInvalidInput` sentinels, an `ErrorResponse` struct,
and `WriteError` mapping `ErrNotFound->404`, `ErrInvalidInput->400`, default->500
via `errors.Is`, encoding JSON.
Test: a table over (wrapped sentinel -> expected status) asserting status,
`Content-Type: application/json`, and the decoded body message; include a
doubly-wrapped error to prove `errors.Is` traverses the whole chain.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/10-error-handling-middleware/02-sentinel-status-mapping/cmd/demo
cd go-solutions/10-error-handling/10-error-handling-middleware/02-sentinel-status-mapping
```

### Why errors.Is and not a type switch

The failure happens deep — a repository returns `ErrNotFound`, a validator
returns `ErrInvalidInput` — and each layer above adds context with
`fmt.Errorf("load user %s: %w", id, ErrNotFound)`. By the time the error reaches
`WriteError` it is a chain: `load user 7: not found`, whose `Unwrap` leads back to
the sentinel. `errors.Is(err, ErrNotFound)` walks that chain link by link and
reports true if the sentinel is anywhere in it, so the boundary maps correctly no
matter how deeply the handler wrapped it. Comparing with `==` would only match the
bare sentinel and fall through to 500 the instant anyone adds context — which is
always. This is why the switch is `switch { case errors.Is(...) }`, not `switch
err { case ErrNotFound }`.

The `default` arm is the safety net: an error that matches no sentinel is an
*unexpected* server failure and maps to 500. That is also where the redaction
discipline starts — here the body still echoes `err.Error()` for teaching
visibility, but Exercise 5 splits 4xx (safe to detail) from 5xx (must be generic).
For now the contract is: a known sentinel gets its status, everything else is a
500.

`WriteError` sets `Content-Type: application/json` *before* `WriteHeader`, because
headers are frozen once the status is written — set them after and they are
silently dropped. Then it encodes the `ErrorResponse`. The order (header, then
`WriteHeader`, then `Encode`) is not cosmetic; it is the only order that produces
a correct response.

Create `api.go`:

```go
package sentinelmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Sentinels the boundary knows how to map. Deep code wraps these with %w.
var (
	ErrNotFound     = errors.New("not found")
	ErrInvalidInput = errors.New("invalid input")
)

// ErrorResponse is the machine-readable error body.
type ErrorResponse struct {
	Error string `json:"error"`
}

// StatusForError maps an error (possibly wrapped) to an HTTP status via
// errors.Is, so the mapping survives fmt.Errorf("...: %w", sentinel) wrapping.
func StatusForError(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// WriteError renders err as a JSON ErrorResponse with the mapped status. It sets
// Content-Type before WriteHeader; headers set after WriteHeader are dropped.
func WriteError(w http.ResponseWriter, err error) {
	status := StatusForError(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Error: err.Error()})
}

// Handler and WithError give the module a working boundary to demonstrate the
// mapping end to end.
type Handler func(w http.ResponseWriter, r *http.Request) error

func WithError(h Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			WriteError(w, err)
		}
	})
}

// GetUser is a realistic handler that wraps sentinels with context.
func GetUser(w http.ResponseWriter, r *http.Request) error {
	id := r.URL.Query().Get("id")
	switch id {
	case "":
		return fmt.Errorf("get user: %w", ErrInvalidInput)
	case "missing":
		return fmt.Errorf("get user %q: %w", id, ErrNotFound)
	}
	fmt.Fprintf(w, "user %s", id)
	return nil
}
```

### The runnable demo

The demo drives the three outcomes through an in-process server and prints the
status and body for each.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"example.com/sentinelmap"
)

func main() {
	mux := http.NewServeMux()
	mux.Handle("/user", sentinelmap.WithError(sentinelmap.GetUser))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, q := range []string{"?id=alice", "", "?id=missing"} {
		resp, err := http.Get(srv.URL + "/user" + q)
		if err != nil {
			panic(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("%-12s -> %d %s\n", "/user"+q, resp.StatusCode, strings.TrimSpace(string(body)))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/user?id=alice -> 200 user alice
/user        -> 400 {"error":"get user: invalid input"}
/user?id=missing -> 404 {"error":"get user \"missing\": not found"}
```

### Tests

The table drives `WriteError` directly with an `httptest.NewRecorder`, asserting
the status, the `Content-Type`, and the decoded message for each sentinel — plus a
*doubly-wrapped* error (`fmt.Errorf("%w", fmt.Errorf("%w", ErrNotFound))`) to
prove `errors.Is` traverses more than one hop. A default-case row proves an
unmapped error becomes a 500.

Create `api_test.go`:

```go
package sentinelmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantMsgSub string
	}{
		{"not found", fmt.Errorf("repo: %w", ErrNotFound), http.StatusNotFound, "not found"},
		{"invalid", fmt.Errorf("validate: %w", ErrInvalidInput), http.StatusBadRequest, "invalid input"},
		{"doubly wrapped", fmt.Errorf("h: %w", fmt.Errorf("repo: %w", ErrNotFound)), http.StatusNotFound, "not found"},
		{"unmapped", errors.New("disk on fire"), http.StatusInternalServerError, "disk on fire"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			WriteError(rec, tc.err)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			var got ErrorResponse
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if !strings.Contains(got.Error, tc.wantMsgSub) {
				t.Fatalf("body message = %q, want it to contain %q", got.Error, tc.wantMsgSub)
			}
		})
	}
}

func ExampleStatusForError() {
	err := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", ErrNotFound))
	fmt.Println(StatusForError(err))
	// Output: 404
}
```

## Review

The mapping is correct when it is a pure function of the sentinels reachable from
the error, computed with `errors.Is`. The doubly-wrapped test row is the one that
matters most: if it ever returns 500, someone changed the switch to compare with
`==` or to unwrap only one level, and every real (wrapped) error in production
would misroute to 500. Keep `Content-Type` set before `WriteHeader`; a test that
checks the header after `WriteError` catches the ordering bug where the header is
set too late and dropped. The `default` arm is deliberately a 500 and deliberately
the place where, in Exercise 5, the body stops echoing the raw error — here it
still does so you can see the mapping, but a real 5xx must not leak `err.Error()`.

## Resources

- [`errors.Is`](https://pkg.go.dev/errors#Is) — chain traversal for sentinel classification.
- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `%w` wrapping and `Is`/`As`.
- [`net/http` status constants](https://pkg.go.dev/net/http#pkg-constants) — `StatusNotFound`, `StatusBadRequest`, `StatusInternalServerError`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-panic-recoverer.md](03-panic-recoverer.md)
