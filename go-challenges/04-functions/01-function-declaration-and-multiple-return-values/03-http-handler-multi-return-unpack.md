# Exercise 3: Unpacking Multiple Returns In An HTTP Handler

The payoff of getting return shapes right is the call site. This exercise wires
the typed accessors into a real `GET /items` handler that unpacks each
`(value, error)` at the call site, reuses one `err` variable across sequential
calls, and returns `400` with the wrapped error on the first failure.

This module is fully self-contained: its own `go mod init`, all code inline
(including the accessors), its own demo and `httptest` tests.

## What you'll build

```text
queryparse/                independent module: example.com/queryparse
  go.mod                   go 1.25
  qparser.go               Query + Int/Bool/Duration accessors (bundled, no cross-import)
  handler.go               ItemsHandler: unpacks accessors, 400 on wrapped error, JSON on success
  cmd/
    demo/
      main.go              starts the server on :8080
  handler_test.go          httptest.ResponseRecorder: 200 body, 400 on bad input, 400 on missing key
```

- Files: `qparser.go`, `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
- Implement: `ItemsHandler(w, r)` that parses `r.URL.Query()`, unpacks `page`/`timeout`/`active` with `v, err :=` then `v, err =`, returns `400` on the first failure and a JSON body on success.
- Test: a good query yields `200` with the expected decoded JSON; `?page=abc` yields `400` with `strconv.ErrSyntax` text in the body; a missing required key yields `400`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/queryparse/cmd/demo
cd ~/go-exercises/queryparse
go mod init example.com/queryparse
go mod edit -go=1.25
```

### The := / = unpacking rule at the call site

The handler makes three fallible calls in a row. The idiom is to declare with `:=`
on the first and reuse the one `err` with `=` after that:

```go
page, err := Int(q, "page")
if err != nil { /* 400 */ }
timeout, err := Duration(q, "timeout")
if err != nil { /* 400 */ }
active, err := Bool(q, "active")
```

Each `:=` after the first is legal because at least one variable on its left
(`timeout`, then `active`) is new, so the pre-existing `err` is simply reassigned.
This is the standard Go pattern and it is why you rarely see a wall of `err1`,
`err2`, `err3`. The count on the left must match the accessor's arity exactly:
writing `page := Int(q, "page")` fails to compile with "multiple-value Int() in
single-value context" — a compile error by design, catching the bug before it
ships.

The handler returns on the *first* error rather than collecting all of them. For a
query parser that is the right call: the client sent a malformed request, one
concrete reason is enough for a `400`, and there is no partial success to report.

Create `qparser.go` (bundled so this module stands alone):

```go
package qparser

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

type Query struct {
	url.Values
}

func Parse(v url.Values) Query { return Query{Values: v} }

func (q Query) First(key string) (string, bool) {
	values := q.Values[key]
	if len(values) == 0 {
		return "", false
	}
	return values[0], true
}

func (q Query) All(key string) []string { return q.Values[key] }

func Int(q Query, key string) (int, error) {
	raw, ok := q.First(key)
	if !ok {
		return 0, fmt.Errorf("missing key %q", key)
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %q as int: %w", key, err)
	}
	return v, nil
}

func Bool(q Query, key string) (bool, error) {
	raw, ok := q.First(key)
	if !ok {
		return false, fmt.Errorf("missing key %q", key)
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("parse %q as bool: %w", key, err)
	}
	return v, nil
}

func Duration(q Query, key string) (time.Duration, error) {
	raw, ok := q.First(key)
	if !ok {
		return 0, fmt.Errorf("missing key %q", key)
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %q as duration: %w", key, err)
	}
	return d, nil
}
```

Create `handler.go`:

```go
package qparser

import (
	"encoding/json"
	"net/http"
)

type itemResponse struct {
	Page    int      `json:"page"`
	Timeout string   `json:"timeout"`
	Active  bool     `json:"active"`
	Tags    []string `json:"tags,omitempty"`
}

// ItemsHandler unpacks each typed accessor at the call site, reusing one err.
func ItemsHandler(w http.ResponseWriter, r *http.Request) {
	q := Parse(r.URL.Query())

	page, err := Int(q, "page")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	timeout, err := Duration(q, "timeout")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	active, err := Bool(q, "active")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp := itemResponse{
		Page:    page,
		Timeout: timeout.String(),
		Active:  active,
		Tags:    q.All("tag"),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// NewMux returns a ServeMux with GET /items mounted.
func NewMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items", ItemsHandler)
	return mux
}
```

### The runnable demo

The root package `qparser` is a library, so `cmd/demo` imports it and mounts its
mux — the demo touches only exported API.

Create `cmd/demo/main.go`:

```go
package main

import (
	"log"
	"net/http"

	"example.com/queryparse"
)

func main() {
	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", qparser.NewMux()))
}
```

Run the server:

```bash
go run ./cmd/demo
```

In another terminal:

```bash
curl -s 'http://localhost:8080/items?page=2&timeout=750ms&active=true&tag=go&tag=senior'
```

Expected output:

```
{"page":2,"timeout":"750ms","active":true,"tags":["go","senior"]}
```

A malformed request surfaces the wrapped error:

```bash
curl -i 'http://localhost:8080/items?page=abc&timeout=1s&active=true'
```

Expected first line and body:

```
HTTP/1.1 400 Bad Request
parse "page" as int: strconv.Atoi: parsing "abc": invalid syntax
```

### Tests

`httptest.ResponseRecorder` drives the handler in-process — no real socket, so the
test is fast and hermetic. The good-query test decodes the JSON body and asserts
each field; the bad-input test asserts a `400` and that the wrapped
`strconv.ErrSyntax` text reached the body.

Create `handler_test.go`:

```go
package qparser

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestItemsOK(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/items?page=2&timeout=750ms&active=true&tag=go&tag=senior", nil)
	rec := httptest.NewRecorder()
	NewMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var got itemResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := itemResponse{Page: 2, Timeout: "750ms", Active: true, Tags: []string{"go", "senior"}}
	if got.Page != want.Page || got.Timeout != want.Timeout || got.Active != want.Active {
		t.Fatalf("body = %+v, want %+v", got, want)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "go" || got.Tags[1] != "senior" {
		t.Fatalf("tags = %v, want [go senior]", got.Tags)
	}
}

func TestItemsBadPage(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/items?page=abc&timeout=1s&active=true", nil)
	rec := httptest.NewRecorder()
	NewMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid syntax") {
		t.Fatalf("body = %q, want it to mention 'invalid syntax'", rec.Body.String())
	}
}

func TestItemsMissingKey(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/items?page=2&active=true", nil)
	rec := httptest.NewRecorder()
	NewMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing timeout", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "missing key") {
		t.Fatalf("body = %q, want it to mention 'missing key'", rec.Body.String())
	}
}
```

## Review

The handler is correct when a well-formed query yields `200` with every field
decoded, and any malformed or missing parameter yields `400` with a message that
names the offending key — `TestItemsBadPage` and `TestItemsMissingKey` cover both
failure kinds. The structural point is the call-site unpacking: reusing one `err`
with `=` after the first `:=` keeps the handler readable, and returning on the
first error is the right policy for a request that is simply malformed.

The mistake to avoid is `page, _ := Int(q, "page")` "because page is probably
fine". On `?page=` (empty) or a missing `page`, that hands the response a silent
`0`, and the client gets results for page zero it never asked for. The error
return is part of the contract; unpack it and act on it. `go build ./...` proves
the `cmd/demo` server compiles; the `httptest` tests prove it serves correctly
without opening a port.

## Resources

- [net/http.ServeMux](https://pkg.go.dev/net/http#ServeMux) — method-and-path patterns like `GET /items` (Go 1.22+).
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRequest` and `NewRecorder` for in-process handler tests.
- [http.Error](https://pkg.go.dev/net/http#Error) — writing a status code and plain-text body in one call.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-value-ok-lookup-and-embedding.md](02-value-ok-lookup-and-embedding.md) | Next: [04-repository-notfound-vs-failure.md](04-repository-notfound-vs-failure.md)
