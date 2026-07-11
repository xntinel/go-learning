# Exercise 1: Request Guard: Decision Tree with Init-Statement Ifs and Early Returns

Every service that faces the network runs an admission decision before it does any
work: is this request even shaped correctly? This module builds that decision as a
pure function `Check(*http.Request) error`, using init-statement `if`s to scope
each header to its own check, guard-clause early returns to keep the body flat, and
a typed `Rejection` so callers can inspect the cause without parsing a string.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
requestguard/               independent module: example.com/requestguard
  go.mod                    go 1.26
  guard.go                  Check, Rejection{Reason,Field} with Error/Unwrap, sentinels
  cmd/
    demo/
      main.go               runs Check over a few requests, prints the decision
  guard_test.go             table-driven accept/reject, errors.Is + errors.As
```

- Files: `guard.go`, `cmd/demo/main.go`, `guard_test.go`.
- Implement: `Check(*http.Request) error` returning `nil` or a `Rejection`; the `Rejection` type with `Error()` and `Unwrap()`; sentinel errors for each cause.
- Test: table-driven acceptance for GET-with-auth, POST JSON, POST form; rejection table asserting `errors.Is(err, sentinel)` and `Rejection.Field` via `errors.As`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/requestguard/cmd/demo
cd ~/go-exercises/requestguard
go mod init example.com/requestguard
```

## The decision tree, in order

`Check` is a sequence of guard clauses. Each precondition that fails returns a
`Rejection` immediately, so the function reads as a top-to-bottom list of reasons a
request can be turned away, and the success path (`return nil`) sits unindented at
the bottom.

The subtle part is the `Authorization` check. `req.Header.Get("Authorization")`
returns `""` in two very different situations: the header was never sent, or it was
sent with an empty value. Those are distinct operational signals — a client that
forgot to authenticate versus one whose token-injection produced an empty string —
so the guard distinguishes them. It first reads the value and trims it; if the
trimmed value is empty, it then consults the raw header map with the comma-ok idiom
(`_, present := req.Header["Authorization"]`) to decide between `ErrMissingAuth`
(absent) and `ErrEmptyAuthorization` (present but blank). `http.Header` canonicalizes
keys, so `"Authorization"` is the correct map key.

The timeout check uses the init-statement form to scope the raw value:
`if raw := req.Header.Get("X-Request-Timeout"); raw != ""`. A missing timeout is
allowed (it is optional), but a present one must parse with `time.ParseDuration` and
be strictly positive; both a malformed value and a non-positive one map to
`ErrInvalidTimeout`. Note this reads a *request* header — contrast with `Retry-After`,
which RFC 9110 defines as a *response* header and which has no business being parsed
off an inbound request.

The media check only applies when the request actually carries a body
(`ContentLength != 0`); an empty GET needs no `Content-Type`. When there is a body,
the declared type must be in a read-only allowlist. `isAllowedContentType` strips any
`; charset=...` parameter and lowercases before comparing, because a real client
sends `application/json; charset=utf-8`.

Create `guard.go`:

```go
package requestguard

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Sentinel causes. Callers match these with errors.Is; they never string-match
// the message.
var (
	ErrNilRequest         = errors.New("nil request")
	ErrMissingAuth        = errors.New("missing Authorization header")
	ErrEmptyAuthorization = errors.New("Authorization header is empty")
	ErrUnsupportedMethod  = errors.New("unsupported method")
	ErrInvalidTimeout     = errors.New("invalid X-Request-Timeout")
	ErrUnsupportedMedia   = errors.New("unsupported Content-Type")
)

// allowedContentTypes is read but never reassigned, so it is race-free even with
// the guard mounted behind many concurrent requests.
var allowedContentTypes = []string{
	"application/json",
	"application/x-www-form-urlencoded",
}

// Rejection is the typed error a decision returns. Field names the offending
// input; Reason is the matchable sentinel exposed through Unwrap.
type Rejection struct {
	Reason error
	Field  string
}

func (r Rejection) Error() string { return fmt.Sprintf("reject %s: %v", r.Field, r.Reason) }

func (r Rejection) Unwrap() error { return r.Reason }

// Check admits or rejects a request. It returns nil to admit, or a Rejection
// whose Reason is one of the package sentinels.
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

### The runnable demo

The demo builds a few requests by hand and prints the decision for each, so you can
watch the decision tree pick a reason.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"strings"

	"example.com/requestguard"
)

func decide(req *http.Request) string {
	if err := requestguard.Check(req); err != nil {
		return "reject: " + err.Error()
	}
	return "admit"
}

func main() {
	ok, _ := http.NewRequest(http.MethodGet, "https://api.example.com/v1/orders", nil)
	ok.Header.Set("Authorization", "Bearer token-123")
	fmt.Println(decide(ok))

	noAuth, _ := http.NewRequest(http.MethodGet, "https://api.example.com/v1/orders", nil)
	fmt.Println(decide(noAuth))

	badMedia, _ := http.NewRequest(http.MethodPost, "https://api.example.com/v1/orders", strings.NewReader("<x/>"))
	badMedia.Header.Set("Authorization", "Bearer token-123")
	badMedia.Header.Set("Content-Type", "text/xml")
	badMedia.ContentLength = 4
	fmt.Println(decide(badMedia))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
admit
reject: reject Authorization: missing Authorization header
reject: reject Content-Type: unsupported Content-Type
```

### Tests

The tests exercise the decision tree as a table. `TestCheckAcceptsValidRequest`
feeds the three shapes a real API accepts. `TestCheckRejectsByReason` asserts both
halves of the contract: `errors.Is` finds the sentinel cause, and `errors.As`
extracts the `Rejection` so the test can check `Field`. Since Go 1.22 each loop
iteration has its own variable, so no `tc := tc` capture is needed before
`t.Parallel()`.

Create `guard_test.go`:

```go
package requestguard

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func newRequest(method, auth, timeout, contentType, body string) *http.Request {
	var req *http.Request
	if body != "" {
		req, _ = http.NewRequest(method, "https://example.com/api", strings.NewReader(body))
		req.ContentLength = int64(len(body))
	} else {
		req, _ = http.NewRequest(method, "https://example.com/api", nil)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if timeout != "" {
		req.Header.Set("X-Request-Timeout", timeout)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func TestCheckAcceptsValidRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request *http.Request
	}{
		{"get with auth", newRequest(http.MethodGet, "Bearer token-123", "", "", "")},
		{"post json body", newRequest(http.MethodPost, "Bearer token-123", "750ms", "application/json; charset=utf-8", `{"ok":true}`)},
		{"post form body", newRequest(http.MethodPost, "Bearer token-123", "", "application/x-www-form-urlencoded", "a=1")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := Check(tc.request); err != nil {
				t.Fatalf("Check() = %v, want nil", err)
			}
		})
	}
}

func TestCheckRejectsByReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request *http.Request
		wantErr error
		field   string
	}{
		{"missing auth", newRequest(http.MethodGet, "", "", "", ""), ErrMissingAuth, "Authorization"},
		{"empty auth", newRequest(http.MethodGet, "   ", "", "", ""), ErrEmptyAuthorization, "Authorization"},
		{"bad method", newRequest(http.MethodDelete, "Bearer x", "", "", ""), ErrUnsupportedMethod, "method"},
		{"malformed timeout", newRequest(http.MethodGet, "Bearer x", "soon", "", ""), ErrInvalidTimeout, "X-Request-Timeout"},
		{"non-positive timeout", newRequest(http.MethodGet, "Bearer x", "0s", "", ""), ErrInvalidTimeout, "X-Request-Timeout"},
		{"unsupported media", newRequest(http.MethodPost, "Bearer x", "", "text/xml", "<x/>"), ErrUnsupportedMedia, "Content-Type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Check(tc.request)
			if err == nil {
				t.Fatal("expected a rejection, got nil")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("errors.Is(err, %v) = false; err = %v", tc.wantErr, err)
			}
			var rej Rejection
			if !errors.As(err, &rej) {
				t.Fatalf("errors.As failed; err = %v", err)
			}
			if rej.Field != tc.field {
				t.Fatalf("Field = %q, want %q", rej.Field, tc.field)
			}
		})
	}
}

func TestNilRequestRejected(t *testing.T) {
	t.Parallel()
	if err := Check(nil); !errors.Is(err, ErrNilRequest) {
		t.Fatalf("Check(nil) = %v, want ErrNilRequest", err)
	}
}
```

## Review

`Check` is correct when it is a pure decision over the request: no I/O, no side
effect, every branch returning either `nil` or a `Rejection` whose `Reason` is a
package sentinel. The two contracts the tests pin are that `errors.Is` finds the
cause through the `Unwrap` chain and `errors.As` recovers the `Rejection` struct, so
a caller never has to read `err.Error()`. The most common mistakes here are
conflating a missing header with an empty one (they are separate sentinels for a
reason), leaving an `else` after a guarded return, and forgetting to trim before the
emptiness test. Turning the decision into an HTTP response — the `statusFor` mapping
and the `func(http.Handler) http.Handler` wrapper — is the next module.

## Resources

- [Go Language Specification: If statements](https://go.dev/ref/spec#If_statements)
- [Effective Go: if (init statements and early returns)](https://go.dev/doc/effective_go#if)
- [errors.Is and errors.As](https://pkg.go.dev/errors#As)
- [RFC 9110: Retry-After is a response header](https://www.rfc-editor.org/rfc/rfc9110#name-retry-after)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-guard-middleware-status-mapping.md](02-guard-middleware-status-mapping.md)
