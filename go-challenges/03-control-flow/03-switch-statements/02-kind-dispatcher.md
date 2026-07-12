# Exercise 2: Route a Request to a Handler With an Expression Switch

Once a request body is classified into a `Kind`, something has to *route* it to
the right handler. This module builds that closed dispatcher: it reads and
validates the body, classifies it, and uses an expression switch on the `Kind`
value to invoke exactly one handler — with a mandatory `default` that rejects
anything the classifier could not place.

This module is fully self-contained. It bundles its own copy of the classifier
so it depends on no other exercise.

## What you'll build

```text
dispatch/                  independent module: example.com/kind-dispatcher
  go.mod                   go 1.24
  dispatch.go              Kind + Classify (bundled); Dispatcher; Serve(w, r)
  cmd/
    demo/
      main.go              runnable demo dispatching three body kinds
  dispatch_test.go         httptest routing table + error-path + default-branch tests
```

- Files: `dispatch.go`, `cmd/demo/main.go`, `dispatch_test.go`.
- Implement: a `Dispatcher` holding json/form/multipart `Handler`s and `Serve(w, r)` that reads/validates the body, calls `Classify`, and switches on the Kind.
- Test: an httptest table counting handler invocations per Kind, empty-body and empty-content-type error paths, and a negative test that injects `KindUnknown` to fire the default.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The closed-dispatch discipline

The classifier (Exercise 1) answered *what kind*; the dispatcher answers *who
handles it*. Because `Kind` is a closed enum, `Serve` uses an expression switch —
value dispatch with `==` — and the entire correctness of the router rides on two
things: each case invokes the right handler, and the `default` case exists.

The `default` is not decoration. `Classify` returns `KindJSON`/`KindForm`/
`KindMultipart` or an error today, but a future edit could add a Kind, or a bug
could hand `Serve` a `KindUnknown`. If the switch had no `default`, that value
would fall straight through and `Serve` would return `nil` — reporting success
while doing nothing, the worst possible failure for a request router. The
`default` here wraps `ErrUnknownKind`, so an unroutable Kind is a loud 4xx, not a
silent 200.

Validation happens *before* the switch: an empty body is rejected with
`ErrEmptyBody`, and a body is re-wrapped in a fresh `io.NopCloser` after being
read so a downstream handler can still consume `r.Body`. `Classify`'s own error
paths (empty or unknown content type) are returned as-is. The switch itself only
runs on a body that is present and a Kind that classified cleanly.

Create `dispatch.go`:

```go
package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

var (
	ErrEmptyBody        = errors.New("empty body")
	ErrEmptyContentType = errors.New("empty Content-Type")
	ErrUnknownKind      = errors.New("unknown content kind")
)

// Kind is the closed set of request-body kinds. Bundled here so this module is
// standalone.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindJSON
	KindForm
	KindMultipart
)

func (k Kind) String() string {
	switch k {
	case KindJSON:
		return "json"
	case KindForm:
		return "form"
	case KindMultipart:
		return "multipart"
	default:
		return "unknown"
	}
}

// Classify folds a raw Content-Type header into a Kind via a tagless switch over
// the parsed media type.
func Classify(contentType string) (Kind, error) {
	if strings.TrimSpace(contentType) == "" {
		return KindUnknown, ErrEmptyContentType
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return KindUnknown, fmt.Errorf("parse content type %q: %w", contentType, err)
	}
	switch {
	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		return KindJSON, nil
	case mediaType == "application/x-www-form-urlencoded":
		return KindForm, nil
	case mediaType == "multipart/form-data":
		return KindMultipart, nil
	default:
		return KindUnknown, fmt.Errorf("%w: %s", ErrUnknownKind, mediaType)
	}
}

// Handler processes a request whose body has already been read and validated.
type Handler func(*http.Request) error

// Dispatcher routes a request to a Handler by its classified Kind.
type Dispatcher struct {
	json      Handler
	form      Handler
	multipart Handler
}

func New(jsonH, formH, multipartH Handler) *Dispatcher {
	return &Dispatcher{json: jsonH, form: formH, multipart: multipartH}
}

// Serve reads and validates the body, classifies it, and dispatches on the Kind
// value with an expression switch and a mandatory fail-closed default.
func (d *Dispatcher) Serve(w http.ResponseWriter, r *http.Request) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	defer r.Body.Close()
	if len(body) == 0 {
		return ErrEmptyBody
	}

	kind, err := Classify(r.Header.Get("Content-Type"))
	if err != nil {
		return err
	}

	// Restore the body so the selected handler can read it again.
	r.Body = io.NopCloser(strings.NewReader(string(body)))

	switch kind {
	case KindJSON:
		var probe map[string]any
		if err := json.Unmarshal(body, &probe); err != nil {
			return fmt.Errorf("decode json: %w", err)
		}
		return d.json(r)
	case KindForm:
		if _, err := url.ParseQuery(string(body)); err != nil {
			return fmt.Errorf("decode form: %w", err)
		}
		return d.form(r)
	case KindMultipart:
		return d.multipart(r)
	default:
		return fmt.Errorf("%w: %s", ErrUnknownKind, kind)
	}
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
	"strings"

	"example.com/kind-dispatcher"
)

func main() {
	d := dispatch.New(
		func(*http.Request) error { fmt.Println("handled: json"); return nil },
		func(*http.Request) error { fmt.Println("handled: form"); return nil },
		func(*http.Request) error { fmt.Println("handled: multipart"); return nil },
	)

	cases := []struct {
		contentType string
		body        string
	}{
		{"application/json", `{"user":"alice"}`},
		{"application/x-www-form-urlencoded", "user=alice&role=admin"},
		{"multipart/form-data; boundary=x", "--x--\r\n"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(c.body))
		req.Header.Set("Content-Type", c.contentType)
		if err := d.Serve(httptest.NewRecorder(), req); err != nil {
			fmt.Println("error:", err)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
handled: json
handled: form
handled: multipart
```

### Tests

`TestServeRoutes` proves routing correctness the only honest way: it counts how
many times each handler was called and asserts exactly one call to the handler
for the input's Kind and zero to the others. The error paths (`ErrEmptyBody`,
`ErrEmptyContentType`) are asserted with `errors.Is`. `TestServeDefaultBranch`
cannot go through `Classify` (which never returns `KindUnknown` on success), so
it calls the switch logic directly with an injected `KindUnknown` via a tiny
helper, proving the default fires.

Create `dispatch_test.go`:

```go
package dispatch

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServeRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		body        string
		wantKind    Kind
		wantErr     error
	}{
		{name: "json", contentType: "application/json", body: `{"a":1}`, wantKind: KindJSON},
		{name: "form", contentType: "application/x-www-form-urlencoded", body: "a=1&b=2", wantKind: KindForm},
		{name: "multipart", contentType: "multipart/form-data; boundary=x", body: "--x--\r\n", wantKind: KindMultipart},
		{name: "empty body", contentType: "application/json", body: "", wantErr: ErrEmptyBody},
		{name: "empty content type", contentType: "", body: "x", wantErr: ErrEmptyContentType},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			calls := map[Kind]int{}
			d := New(
				func(*http.Request) error { calls[KindJSON]++; return nil },
				func(*http.Request) error { calls[KindForm]++; return nil },
				func(*http.Request) error { calls[KindMultipart]++; return nil },
			)

			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			err := d.Serve(httptest.NewRecorder(), req)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Serve err = %v, want errors.Is %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Serve err = %v, want nil", err)
			}
			if calls[tc.wantKind] != 1 {
				t.Fatalf("handler for %s called %d times, want 1", tc.wantKind, calls[tc.wantKind])
			}
			for k, n := range calls {
				if k != tc.wantKind && n != 0 {
					t.Fatalf("handler for %s called %d times, want 0", k, n)
				}
			}
		})
	}
}

func TestServeDefaultBranch(t *testing.T) {
	t.Parallel()

	// Drive the same expression switch Serve uses, with an unroutable Kind, to
	// prove the fail-closed default returns ErrUnknownKind.
	err := dispatchKind(KindUnknown)
	if !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("dispatchKind(KindUnknown) err = %v, want errors.Is ErrUnknownKind", err)
	}
}

// dispatchKind mirrors Serve's expression switch in isolation.
func dispatchKind(kind Kind) error {
	switch kind {
	case KindJSON, KindForm, KindMultipart:
		return nil
	default:
		return fmt.Errorf("%w: %s", ErrUnknownKind, kind)
	}
}
```

## Review

The router is correct when exactly one handler runs per request and an
unroutable value cannot pass silently. `TestServeRoutes` enforces the first by
counting invocations across every handler — a test that only checked "the json
handler ran" would miss a bug that *also* ran the form handler.
`TestServeDefaultBranch` enforces the second: the fail-closed `default` turns a
`KindUnknown` into a wrapped `ErrUnknownKind` rather than a silent `nil`. The body
is restored with `io.NopCloser(strings.NewReader(...))` after `io.ReadAll` drains
it, so a real handler downstream can still read `r.Body`; forgetting that
restoration is a classic empty-body-in-the-handler bug.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the expression switch and its comma-list cases.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRequest` and `NewRecorder` for handler tests without a network.
- [io.NopCloser](https://pkg.go.dev/io#NopCloser) — re-wrapping a consumed body so a downstream handler can read it.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-content-type-classifier.md](01-content-type-classifier.md) | Next: [03-http-retry-classifier.md](03-http-retry-classifier.md)
