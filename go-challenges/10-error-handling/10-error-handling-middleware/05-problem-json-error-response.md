# Exercise 5: Emit RFC 9457 problem+json error bodies

An ad-hoc `{"error": "..."}` body is not a contract. This exercise replaces it
with an `application/problem+json` document per RFC 9457 (type, title, status,
detail, instance) carrying a correlation id — with the redaction rule that 4xx may
surface a specific detail while 5xx must show a generic one and log the real error.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
problemjson/                 independent module: example.com/problemjson
  go.mod                     go 1.26
  problem.go                 ProblemDetails; sentinels; WriteProblem (redacts 5xx); correlation id
  cmd/
    demo/
      main.go                runnable demo: a 400 with detail and a 500 redacted
  problem_test.go            decode into ProblemDetails; 5xx generic, 4xx specific; id echoed
```

Files: `problem.go`, `cmd/demo/main.go`, `problem_test.go`.
Implement: a `ProblemDetails` struct, sentinel-to-problem mapping, and
`WriteProblem` that sets `Content-Type: application/problem+json`, redacts the
detail on 5xx, and includes a correlation id.
Test: decode the body into `ProblemDetails`; assert type/title/status match the
mapped sentinel; a 500 carries a generic detail (no internal string leaks) and a
400 carries the validation detail; the instance/correlation id from the header
appears in the body.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/10-error-handling-middleware/05-problem-json-error-response/cmd/demo
cd go-solutions/10-error-handling/10-error-handling-middleware/05-problem-json-error-response
```

### Why RFC 9457 and why redaction

RFC 9457 (Problem Details for HTTP APIs, which obsoletes RFC 7807) defines a
standard error document so clients can branch on machine-readable fields instead
of scraping a free-form string. The members:

- `type` — a URI identifying the *problem class* (e.g.
  `https://example.com/problems/not-found`). Stable across occurrences; a client
  keys its handling off this.
- `title` — a short, stable, human-readable summary of the class ("Not Found").
- `status` — the HTTP status code, duplicated in the body so the document is
  self-contained.
- `detail` — a human explanation of *this specific occurrence*.
- `instance` — a URI/id identifying this occurrence; a natural home for the
  correlation id.

The response `Content-Type` is `application/problem+json` (not plain
`application/json`), which is how a client's error middleware recognizes the shape.

The redaction discipline is the senior part. A 4xx is the *client's* fault, and a
specific `detail` ("field 'email' is required") helps them fix the request — safe
to surface. A 5xx is the *server's* fault, and its underlying error frequently
contains internal detail: a SQL fragment, a file path, a downstream host name, a
stack hint. Shipping `err.Error()` in a 5xx body leaks that to anyone who can send
a request. So `WriteProblem` branches on the status: for 5xx it writes a fixed
generic detail ("an internal error occurred") and logs the *full* error server-side
under the correlation id; for 4xx it writes the actual detail. The correlation id
is the bridge — the client sees it in the header and body, the operator finds the
full error in the logs by that same id, and the internal detail never crosses the
boundary.

The instance/id is derived from an incoming `X-Correlation-Id` header if present,
else generated; it is echoed back in the response header so a client can quote it
in a support ticket.

Create `problem.go`:

```go
package problemjson

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// Sentinels mapped to problem types.
var (
	ErrNotFound     = errors.New("not found")
	ErrInvalidInput = errors.New("invalid input")
)

// ProblemDetails is an RFC 9457 application/problem+json document.
type ProblemDetails struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
}

const correlationHeader = "X-Correlation-Id"

// classify maps an error to (type, title, status). Unmapped errors are 500.
func classify(err error) (typ, title string, status int) {
	switch {
	case errors.Is(err, ErrNotFound):
		return "https://example.com/problems/not-found", "Not Found", http.StatusNotFound
	case errors.Is(err, ErrInvalidInput):
		return "https://example.com/problems/invalid-input", "Invalid Input", http.StatusBadRequest
	default:
		return "https://example.com/problems/internal", "Internal Server Error", http.StatusInternalServerError
	}
}

// correlationID returns the inbound id or a fresh random one.
func correlationID(r *http.Request) string {
	if id := r.Header.Get(correlationHeader); id != "" {
		return id
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// WriteProblem renders err as an RFC 9457 document. It redacts the detail for
// 5xx (logging the full error) and surfaces the real detail for 4xx. The
// correlation id is echoed in a header and stored in instance.
func WriteProblem(w http.ResponseWriter, r *http.Request, err error) {
	typ, title, status := classify(err)
	id := correlationID(r)

	detail := err.Error()
	if status >= 500 {
		// The full error may contain internal detail; log it, do not ship it.
		slog.ErrorContext(r.Context(), "request failed",
			"correlation_id", id, "method", r.Method, "path", r.URL.Path, "err", err)
		detail = "an internal error occurred"
	}

	prob := ProblemDetails{
		Type:     typ,
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: "urn:correlation:" + id,
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set(correlationHeader, id)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(prob)
}
```

### The runnable demo

The demo silences logs and drives one 400 (detail surfaced) and one 500 (detail
redacted), printing the decoded problem for each.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"example.com/problemjson"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	mux := http.NewServeMux()
	mux.HandleFunc("/bad", func(w http.ResponseWriter, r *http.Request) {
		problemjson.WriteProblem(w, r, fmt.Errorf("field email is required: %w", problemjson.ErrInvalidInput))
	})
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		problemjson.WriteProblem(w, r, errors.New("pq: relation \"users\" does not exist"))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{"/bad", "/boom"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("X-Correlation-Id", "demo-123")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			panic(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("%s -> %d %s\n", path, resp.StatusCode, bytes.TrimSpace(body))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/bad -> 400 {"type":"https://example.com/problems/invalid-input","title":"Invalid Input","status":400,"detail":"field email is required: invalid input","instance":"urn:correlation:demo-123"}
/boom -> 500 {"type":"https://example.com/problems/internal","title":"Internal Server Error","status":500,"detail":"an internal error occurred","instance":"urn:correlation:demo-123"}
```

The 500 body shows `an internal error occurred`, never the `pq: relation ...`
string, which went only to the log.

### Tests

The tests decode the body into a `ProblemDetails` and assert the mapped
`type`/`title`/`status`. The redaction rows are the point: the 400 body must
*contain* the validation detail, and the 500 body must *not contain* the internal
error substring and must equal the generic message. A final assertion proves the
inbound correlation id is echoed in both the header and the `instance` field.

Create `problem_test.go`:

```go
package problemjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func doProblem(t *testing.T, id string, err error) (*http.Response, ProblemDetails) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if id != "" {
		req.Header.Set(correlationHeader, id)
	}
	WriteProblem(rec, req, err)
	resp := rec.Result()
	var prob ProblemDetails
	if derr := json.NewDecoder(resp.Body).Decode(&prob); derr != nil {
		t.Fatalf("decode problem: %v", derr)
	}
	return resp, prob
}

func TestProblemMappingAndRedaction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantTitle  string
		mustHave   string // detail substring that must appear
		mustNot    string // substring that must NOT appear (leak check)
	}{
		{
			name:       "invalid input surfaces detail",
			err:        fmt.Errorf("field email is required: %w", ErrInvalidInput),
			wantStatus: http.StatusBadRequest,
			wantTitle:  "Invalid Input",
			mustHave:   "field email is required",
		},
		{
			name:       "internal error is redacted",
			err:        errors.New("pq: relation users does not exist"),
			wantStatus: http.StatusInternalServerError,
			wantTitle:  "Internal Server Error",
			mustHave:   "an internal error occurred",
			mustNot:    "pq: relation",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp, prob := doProblem(t, "", tc.err)

			if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
				t.Fatalf("Content-Type = %q, want application/problem+json", ct)
			}
			if prob.Status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", prob.Status, tc.wantStatus)
			}
			if prob.Title != tc.wantTitle {
				t.Fatalf("title = %q, want %q", prob.Title, tc.wantTitle)
			}
			if !strings.Contains(prob.Detail, tc.mustHave) {
				t.Fatalf("detail = %q, want it to contain %q", prob.Detail, tc.mustHave)
			}
			if tc.mustNot != "" && strings.Contains(prob.Detail, tc.mustNot) {
				t.Fatalf("detail = %q leaked internal substring %q", prob.Detail, tc.mustNot)
			}
		})
	}
}

func TestCorrelationIDEchoed(t *testing.T) {
	t.Parallel()

	resp, prob := doProblem(t, "trace-abc", fmt.Errorf("bad: %w", ErrInvalidInput))

	if got := resp.Header.Get(correlationHeader); got != "trace-abc" {
		t.Fatalf("header id = %q, want trace-abc", got)
	}
	if prob.Instance != "urn:correlation:trace-abc" {
		t.Fatalf("instance = %q, want urn:correlation:trace-abc", prob.Instance)
	}
}

func ExampleWriteProblem() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(correlationHeader, "id-1")
	WriteProblem(rec, req, fmt.Errorf("missing id: %w", ErrNotFound))
	fmt.Println(rec.Code, rec.Header().Get("Content-Type"))
	// Output: 404 application/problem+json
}
```

## Review

The problem document is correct when the machine-readable fields
(`type`/`title`/`status`) are a deterministic function of the sentinel and the
`Content-Type` is `application/problem+json` — a client's error handler keys off
exactly those. The redaction test is the one that protects production: a 500 body
that ever contains `pq: relation` (or any `mustNot` substring) is a leak, and the
generic-detail assertion pins it shut while the full error still reaches the log.
The correlation id closes the loop: the client quotes the `instance` id, the
operator greps the logs for it, and the internal error is joinable without ever
having crossed the wire. If you extend this, keep `type` URIs stable — clients
depend on them — and never move a specific detail from a 4xx into a 5xx.

## Resources

- [RFC 9457: Problem Details for HTTP APIs](https://www.rfc-editor.org/rfc/rfc9457) — the document format and its members (obsoletes RFC 7807).
- [`errors.Is`](https://pkg.go.dev/errors#Is) — sentinel classification behind `classify`.
- [`log/slog#Logger.ErrorContext`](https://pkg.go.dev/log/slog#Logger.ErrorContext) — logging the full error server-side under the correlation id.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-status-capturing-responsewriter.md](06-status-capturing-responsewriter.md)
