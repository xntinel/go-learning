# Exercise 7: Approval-test an HTTP handler's JSON response body

Driving a real `http.Handler` with `httptest`, capturing the status, content-type,
and body, normalizing the volatile fields, and pinning the result as a golden is
the highest-value use of snapshot testing: it turns accidental API contract drift
into a failing test before it reaches a client. This module builds that pipeline
end to end.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
handlersnap/               independent module: example.com/handlersnap
  go.mod                   go 1.26
  handler.go               Handler(), DriftHandler(), Normalize([]byte)
  testdata/
    get_user.golden        approved response contract (status + header + body)
  cmd/
    demo/
      main.go              serves one request and prints the normalized snapshot
  handler_test.go          httptest capture + -update golden + drift-breaks-snapshot
```

Files: `handler.go`, `testdata/get_user.golden`, `cmd/demo/main.go`, `handler_test.go`.
Implement: an `http.Handler` for `GET /users/{id}` returning JSON with a volatile request id and timestamp, plus a `Normalize` that redacts them.
Test: serve a request with `httptest`, snapshot status+header+normalized body against a golden; assert a drifted handler (renamed field) breaks the snapshot.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/23-snapshot-approval-testing/07-http-handler-approval/cmd/demo go-solutions/12-testing-ecosystem/23-snapshot-approval-testing/07-http-handler-approval/testdata
cd go-solutions/12-testing-ecosystem/23-snapshot-approval-testing/07-http-handler-approval
```

### Capturing a handler's response as a contract

`httptest` gives you the whole request/response cycle in-process with no socket.
`httptest.NewRequest` builds a `*http.Request` as if it arrived over the wire;
`httptest.NewRecorder` gives a `ResponseRecorder` that captures everything the
handler writes. After `handler.ServeHTTP(rec, req)` returns, `rec.Code` holds the
status, `rec.Header()` holds the response headers, and `rec.Body` is a
`*bytes.Buffer` with the exact bytes written. Snapshotting all three — not just the
body — is deliberate: the status code and the `Content-Type` are as much a part of
the API contract as any field, and a handler that silently starts returning
`text/plain` or a 500 is a real regression the snapshot should catch.

The response carries a request id (a genuine v4 UUID from `crypto/rand`) and a
`served_at` timestamp, so every request produces different bytes — exactly the
volatility that makes an un-normalized snapshot flake. `Normalize` redacts both to
placeholders before comparison, so the golden captures the *structure* of the
contract without pinning the ephemeral values. The handler uses
`json.Encoder.SetIndent` so the body is pretty-printed and `Encode` appends the
trailing newline, which is the golden's newline convention.

The drift check is the payoff. `DriftHandler` is identical except one JSON tag
changed from `user` to `account`. `TestDriftBreaksSnapshot` serves it and asserts
its snapshot differs from the golden — proving that renaming a field, the kind of
accidental contract break that ships silently, is caught here as a failing test.

Create `handler.go`:

```go
package handlersnap

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// User is the resource served by the API.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type response struct {
	RequestID string `json:"request_id"`
	User      User   `json:"user"`
	ServedAt  string `json:"served_at"`
}

// driftResponse is response with one tag renamed, standing in for an accidental
// contract change.
type driftResponse struct {
	RequestID string `json:"request_id"`
	User      User   `json:"account"`
	ServedAt  string `json:"served_at"`
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// Handler serves GET /users/{id} with a volatile request id and timestamp.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		writeJSON(w, response{
			RequestID: newRequestID(),
			User:      User{ID: id, Name: "Alice", Email: "alice@example.com"},
			ServedAt:  time.Now().UTC().Format(time.RFC3339),
		})
	})
	return mux
}

// DriftHandler is Handler with one JSON tag renamed (user -> account), standing
// in for an accidental contract change a snapshot must catch.
func DriftHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		writeJSON(w, driftResponse{
			RequestID: newRequestID(),
			User:      User{ID: id, Name: "Alice", Email: "alice@example.com"},
			ServedAt:  time.Now().UTC().Format(time.RFC3339),
		})
	})
	return mux
}

var (
	reTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)
	reUUID      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
)

// Normalize redacts the volatile request id and timestamp to stable placeholders.
func Normalize(b []byte) []byte {
	b = reTimestamp.ReplaceAll(b, []byte("<TIMESTAMP>"))
	b = reUUID.ReplaceAll(b, []byte("<UUID>"))
	return b
}
```

Now the approved response contract:

Create `testdata/get_user.golden`:

```text
HTTP 200
Content-Type: application/json

{
  "request_id": "<UUID>",
  "user": {
    "id": "u1",
    "name": "Alice",
    "email": "alice@example.com"
  },
  "served_at": "<TIMESTAMP>"
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http/httptest"
	"os"

	"example.com/handlersnap"
)

func main() {
	req := httptest.NewRequest("GET", "/users/u1", nil)
	rec := httptest.NewRecorder()
	handlersnap.Handler().ServeHTTP(rec, req)

	fmt.Printf("HTTP %d\n", rec.Code)
	fmt.Printf("Content-Type: %s\n\n", rec.Header().Get("Content-Type"))
	os.Stdout.Write(handlersnap.Normalize(rec.Body.Bytes()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
HTTP 200
Content-Type: application/json

{
  "request_id": "<UUID>",
  "user": {
    "id": "u1",
    "name": "Alice",
    "email": "alice@example.com"
  },
  "served_at": "<TIMESTAMP>"
}
```

### Tests

`snapshot` composes the status line, the content-type header, and the normalized
body into one document. `TestGetUserGolden` serves a request and byte-compares that
document against the golden, regenerating under `-update`. `TestDriftBreaksSnapshot`
serves the renamed-field handler and asserts its snapshot differs, proving the
contract break is caught.

Create `handler_test.go`:

```go
package handlersnap

import (
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/")

func snapshot(rec *httptest.ResponseRecorder) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "HTTP %d\n", rec.Code)
	fmt.Fprintf(&b, "Content-Type: %s\n\n", rec.Header().Get("Content-Type"))
	b.Write(Normalize(rec.Body.Bytes()))
	return b.Bytes()
}

func serve(h http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/users/u1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func goldenFile(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run: go test -run %s -update)", path, err, t.Name())
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("golden mismatch for %s (run: go test -run %s -update)\n--- got ---\n%s\n--- want ---\n%s",
			path, t.Name(), got, want)
	}
}

func TestGetUserGolden(t *testing.T) {
	rec := serve(Handler())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	goldenFile(t, "get_user.golden", snapshot(rec))
}

func TestDriftBreaksSnapshot(t *testing.T) {
	t.Parallel()

	want, err := os.ReadFile(filepath.Join("testdata", "get_user.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	got := snapshot(serve(DriftHandler()))
	if bytes.Equal(got, want) {
		t.Fatal("drifted handler (renamed field) matched the golden; contract drift not detected")
	}
}
```

## Review

This is the pattern to reach for first on any JSON API. The snapshot captures the
status, the content-type, and the normalized body, so a change to any part of the
observable contract — a flipped status, a dropped header, a renamed field — shows
up as a diff a reviewer reads. Normalization is what makes it possible: without
redacting the request id and timestamp the golden would flake on every request,
and a flaky snapshot gets disabled or blindly re-approved, which is worse than no
snapshot. `TestDriftBreaksSnapshot` is the honesty check on the whole scheme — if a
renamed field did *not* break the snapshot, the test would be pinning nothing. Keep
the snapshot scoped to the contract fields; capturing an entire multi-kilobyte
response would drown the meaningful diff in noise. Run `go test -race`, then read
the `git diff` on any golden you regenerate.

## Resources

- [net/http/httptest: NewRequest / NewRecorder](https://pkg.go.dev/net/http/httptest#NewRecorder) — driving a handler in-process and capturing its response.
- [net/http: ServeMux method and wildcard patterns](https://pkg.go.dev/net/http#ServeMux) — the `GET /users/{id}` routing and `Request.PathValue`.
- [encoding/json: Encoder.SetIndent](https://pkg.go.dev/encoding/json#Encoder.SetIndent) — pretty-printing the response body deterministically.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-canonical-json-snapshot.md](06-canonical-json-snapshot.md) | Next: [08-table-driven-golden.md](08-table-driven-golden.md)
