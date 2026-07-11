# Exercise 9: Snapshot the API error envelope shape

Error responses are a contract too, and often a more security-sensitive one than
the happy path. This module pins the serialized error envelope — code, message,
details — plus the status code, so that renaming a field or flipping a code becomes
a reviewable diff instead of a silent break that surfaces as a confused client.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
errsnap/                   independent module: example.com/errsnap
  go.mod                   go 1.26
  errors.go                ErrorEnvelope, WriteError, Handler()
  testdata/
    bad_request.golden     approved 400 envelope
    server_error.golden    approved 500 envelope
  cmd/
    demo/
      main.go              triggers the 400 path and prints the snapshot
  errors_test.go           httptest 400 + 500 goldens, code-change-breaks-snapshot
```

Files: `errors.go`, `testdata/{bad_request,server_error}.golden`, `cmd/demo/main.go`, `errors_test.go`.
Implement: a JSON error envelope, a `WriteError` writer, and a `Handler` whose `POST /users` returns 400 on a missing name and whose `GET /boom` returns 500.
Test: snapshot status + envelope for the 400 and 500 paths against goldens; assert a changed error code breaks the snapshot.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/errsnap/cmd/demo ~/go-exercises/errsnap/testdata
cd ~/go-exercises/errsnap
go mod init example.com/errsnap
```

### Why error envelopes are prime snapshot candidates

Error envelopes are exactly the output snapshot testing is best at. They are small,
so the diff stays readable. They are stable — no timestamps or ids in the envelope
itself, so no normalization is needed. And they are a contract clients program
against: a mobile app switches on `error.code`, an operator greps for a message, a
retry layer keys off the status. A change to any of those is a change someone
downstream depends on, and it is precisely the kind of change that ships silently
because the happy-path tests still pass. Pinning the envelope turns "someone
renamed `code` to `error_code`" or "the 400 quietly became a 500" into a failing
test with a diff.

`WriteError` serializes an `ErrorEnvelope` with `json.MarshalIndent`, sets the
content-type, writes the status, and appends the trailing newline. The `details`
field uses `omitempty`, so the 500 envelope with no details omits the key entirely
— a real serialization behavior the golden captures. The `Handler` exercises two
status paths: `POST /users` with a missing name returns 400 with code
`invalid_argument`, and `GET /boom` returns 500 with code `internal`, standing in
for an unexpected failure. Both are snapshotted as `status + body`.

`TestCodeChangeBreaksSnapshot` is the honesty check: it renders an envelope with a
deliberately different code and asserts its snapshot differs from the golden. If a
changed code did not break the snapshot, the test would be pinning nothing — and a
silently changed error code is a genuine client break.

Create `errors.go`:

```go
package errsnap

import (
	"encoding/json"
	"net/http"
)

// ErrorEnvelope is the API's stable error contract.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries the machine code, a human message, and optional field-level
// details. Details is omitempty, so an envelope without details omits the key.
type ErrorBody struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

// WriteError serializes an error envelope with the given status.
func WriteError(w http.ResponseWriter, status int, code, message string, details ...string) {
	env := ErrorEnvelope{Error: ErrorBody{Code: code, Message: message, Details: details}}
	body, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// User is the create payload.
type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Handler routes the two error paths this module snapshots.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /users", func(w http.ResponseWriter, r *http.Request) {
		var u User
		if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
			WriteError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON", "field: body")
			return
		}
		if u.Name == "" {
			WriteError(w, http.StatusBadRequest, "invalid_argument", "name is required", "field: name")
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, http.StatusInternalServerError, "internal", "unexpected error")
	})
	return mux
}
```

Now the approved envelopes:

Create `testdata/bad_request.golden`:

```text
HTTP 400
{
  "error": {
    "code": "invalid_argument",
    "message": "name is required",
    "details": [
      "field: name"
    ]
  }
}
```

Create `testdata/server_error.golden`:

```text
HTTP 500
{
  "error": {
    "code": "internal",
    "message": "unexpected error"
  }
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
	"strings"

	"example.com/errsnap"
)

func main() {
	req := httptest.NewRequest("POST", "/users", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	errsnap.Handler().ServeHTTP(rec, req)

	fmt.Printf("HTTP %d\n", rec.Code)
	os.Stdout.Write(rec.Body.Bytes())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
HTTP 400
{
  "error": {
    "code": "invalid_argument",
    "message": "name is required",
    "details": [
      "field: name"
    ]
  }
}
```

### Tests

`snapshot` joins the status line and the envelope body. `TestBadRequestGolden` and
`TestServerErrorGolden` drive the two error paths and compare against their
goldens. `TestCodeChangeBreaksSnapshot` renders an envelope with a changed code and
asserts it no longer matches the 400 golden.

Create `errors_test.go`:

```go
package errsnap

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/")

func snapshot(rec *httptest.ResponseRecorder) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "HTTP %d\n", rec.Code)
	b.Write(rec.Body.Bytes())
	return b.Bytes()
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
		t.Fatalf("read golden %s: %v (run: go test -update)", path, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("golden mismatch for %s (run: go test -update)\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func TestBadRequestGolden(t *testing.T) {
	req := httptest.NewRequest("POST", "/users", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	goldenFile(t, "bad_request.golden", snapshot(rec))
}

func TestServerErrorGolden(t *testing.T) {
	req := httptest.NewRequest("GET", "/boom", nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	goldenFile(t, "server_error.golden", snapshot(rec))
}

func TestCodeChangeBreaksSnapshot(t *testing.T) {
	t.Parallel()

	want, err := os.ReadFile(filepath.Join("testdata", "bad_request.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	// Same envelope with a changed code stands in for an accidental contract break.
	env := ErrorEnvelope{Error: ErrorBody{Code: "invalid_arg", Message: "name is required", Details: []string{"field: name"}}}
	body, _ := json.MarshalIndent(env, "", "  ")
	var b bytes.Buffer
	fmt.Fprintf(&b, "HTTP %d\n", http.StatusBadRequest)
	b.Write(append(body, '\n'))
	if bytes.Equal(b.Bytes(), want) {
		t.Fatal("changed error code still matched the golden; contract drift not detected")
	}
}
```

## Review

Error envelopes are the sweet spot for snapshotting: small, stable, no
normalization required, and a contract clients depend on. Capturing the status
alongside the body is essential, because a code changing from 400 to 500 is exactly
the drift you want caught and the body alone would miss it. `omitempty` on
`details` is a real serialization behavior the golden pins — the 500 envelope
legitimately omits the key, and if a refactor accidentally started emitting
`"details": null`, the snapshot would flag it. `TestCodeChangeBreaksSnapshot` keeps
the suite honest by proving a changed code really does break the golden. Run
`go test -race`, and treat any regenerated error golden as a security-relevant diff
worth a careful read.

## Resources

- [encoding/json: MarshalIndent and omitempty](https://pkg.go.dev/encoding/json#MarshalIndent) — serializing the envelope and omitting empty optional fields.
- [net/http/httptest: ResponseRecorder](https://pkg.go.dev/net/http/httptest#ResponseRecorder) — capturing `Code` and `Body` for the snapshot.
- [net/http: StatusText and status constants](https://pkg.go.dev/net/http#StatusText) — the standard status codes the envelope pairs with.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-table-driven-golden.md](08-table-driven-golden.md) | Next: [10-received-approved-workflow.md](10-received-approved-workflow.md)
