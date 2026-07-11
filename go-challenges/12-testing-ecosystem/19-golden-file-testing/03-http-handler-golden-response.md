# Exercise 3: Golden-Test an HTTP JSON Handler with httptest

The most valuable golden test in a backend pins the public API contract: the
exact JSON body a handler returns. You build a `GET /orders/{id}` handler, drive
it through `httptest`, and snapshot the status, the `Content-Type`, and the
normalized body against golden files — one for the found case, one for
not-found.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
ordersapi/                 independent module: example.com/ordersapi
  go.mod                   go 1.26
  handler.go               Handler() http.Handler; GET /orders/{id}
  testdata/
    orders_get.golden      body snapshot for the found case
    orders_notfound.golden body snapshot for the 404 case
  cmd/
    demo/
      main.go              drives the handler and prints status + body
  handler_test.go          httptest table, status/header asserts, body golden
```

Files: `handler.go`, two `testdata/*.golden`, `cmd/demo/main.go`, `handler_test.go`.
Implement: `Handler() http.Handler` routing `GET /orders/{id}`, returning the order JSON or a 404 error JSON.
Test: serve requests into an `httptest.ResponseRecorder`, assert status and `Content-Type` separately, then `json.Indent`-normalize the body and compare to a per-case golden.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ordersapi/cmd/demo ~/go-exercises/ordersapi/testdata
cd ~/go-exercises/ordersapi
go mod init example.com/ordersapi
```

### Why golden the body but assert status and headers separately

An HTTP response has three parts a consumer depends on: the status code, the
headers, and the body. A golden over the body alone is a classic incomplete
test — it would sail past a 200-to-500 regression or a `Content-Type` change to
`text/plain`, because those are not in the body bytes. So the contract splits: the
status code and the headers you care about are asserted explicitly and cheaply,
and only the body — the large, structured part — is snapshotted. That division
also keeps failures legible: a status regression fails with `status = 500, want
200`, not with a confusing body diff.

The body is normalized before comparison. The handler writes compact JSON via
`json.NewEncoder`, but two compact encodings that differ only in whitespace are
semantically identical, and you do not want the golden to churn on formatting. So
the test runs both the recorded body and (implicitly) the golden through
`json.Indent` with a fixed indent and a single-trailing-newline policy, producing
one canonical shape. Byte comparison against that canonical form still catches
every field rename, added field, or value change — which is exactly the API
contract you want to pin — while ignoring insignificant whitespace. The golden is
tied to the *public* JSON shape, not to the internal `Order` struct: a reviewer
reading the diff sees the wire contract change directly.

The handler uses the Go 1.22+ routing patterns (`GET /orders/{id}` with
`r.PathValue("id")`), so the method and path variable come from the mux, not
hand-rolled parsing.

Create `handler.go`:

```go
package ordersapi

import (
	"encoding/json"
	"net/http"
)

// Item is a line on an order.
type Item struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

// Order is the public JSON shape returned by the API. The struct tags ARE the
// contract the golden files pin.
type Order struct {
	ID         string `json:"id"`
	Customer   string `json:"customer"`
	Items      []Item `json:"items"`
	TotalCents int    `json:"total_cents"`
}

var orders = map[string]Order{
	"ord-42": {
		ID:         "ord-42",
		Customer:   "Globex",
		Items:      []Item{{SKU: "WIDGET", Qty: 3}, {SKU: "GADGET", Qty: 1}},
		TotalCents: 7947,
	},
}

// Handler routes GET /orders/{id}, returning the order JSON or a 404 error body.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		o, ok := orders[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "order not found", "id": id})
			return
		}
		_ = json.NewEncoder(w).Encode(o)
	})
	return mux
}
```

Now the committed body goldens.

Create `testdata/orders_get.golden`:

```text
{
  "id": "ord-42",
  "customer": "Globex",
  "items": [
    {
      "sku": "WIDGET",
      "qty": 3
    },
    {
      "sku": "GADGET",
      "qty": 1
    }
  ],
  "total_cents": 7947
}
```

Create `testdata/orders_notfound.golden`:

```text
{
  "error": "order not found",
  "id": "missing"
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http/httptest"

	"example.com/ordersapi"
)

func main() {
	for _, id := range []string{"ord-42", "missing"} {
		req := httptest.NewRequest("GET", "/orders/"+id, nil)
		rec := httptest.NewRecorder()
		ordersapi.Handler().ServeHTTP(rec, req)
		fmt.Printf("GET /orders/%s -> %d %s\n", id, rec.Code, rec.Header().Get("Content-Type"))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /orders/ord-42 -> 200 application/json; charset=utf-8
GET /orders/missing -> 404 application/json; charset=utf-8
```

### Tests

A table names each case with its expected status and golden file. Each subtest
serves the request into a recorder, asserts the status and `Content-Type`
explicitly, then normalizes the body and compares to the golden. The
normalization (`indent`) trims to exactly one trailing newline so the byte
compare matches the committed file's trailing-newline policy.

Create `handler_test.go`:

```go
package ordersapi

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/")

// indent canonicalizes JSON to a fixed indent with exactly one trailing newline.
func indent(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		t.Fatalf("indent %q: %v", b, err)
	}
	return append(bytes.TrimRight(out.Bytes(), "\n"), '\n')
}

func goldenFile(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
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
		t.Fatalf("body golden mismatch for %s (run: go test -update)\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func TestOrdersHandlerGolden(t *testing.T) {
	tests := []struct {
		name       string
		id         string
		wantStatus int
		golden     string
	}{
		{"found", "ord-42", http.StatusOK, "orders_get.golden"},
		{"not_found", "missing", http.StatusNotFound, "orders_notfound.golden"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/orders/"+tc.id, nil)
			rec := httptest.NewRecorder()
			Handler().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Fatalf("Content-Type = %q, want application/json; charset=utf-8", ct)
			}
			goldenFile(t, tc.golden, indent(t, rec.Body.Bytes()))
		})
	}
}

func ExampleHandler() {
	req := httptest.NewRequest(http.MethodGet, "/orders/ord-42", nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	os.Stdout.WriteString(rec.Header().Get("Content-Type") + "\n")
	// Output: application/json; charset=utf-8
}
```

## Review

The test pins the public contract, not the internals: the golden is the wire JSON
shape, so a field rename in `Order`'s tags or an added field surfaces as a
reviewable body diff, while an internal refactor that produces the same bytes
stays quiet. The three-part split is the point — status and `Content-Type`
asserted explicitly, body snapshotted — because a body-only golden misses a
status or header regression entirely. The one subtle correctness requirement is
determinism of the body: the map-based not-found response serializes its keys in
sorted order (a guarantee of `encoding/json`), and the order struct has no clock
or id, so both bodies are byte-stable across runs. If a handler ever embedded a
timestamp or request id, you would normalize it before this compare — the subject
of the next two exercises.

## Resources

- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRequest`, `NewRecorder`, and `ResponseRecorder`.
- [http.ServeMux patterns](https://pkg.go.dev/net/http#ServeMux) — method-and-path routing and `Request.PathValue`.
- [json.Indent](https://pkg.go.dev/encoding/json#Indent) — canonicalizing a response body for a stable golden.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-cmp-diff-structured-golden.md](04-cmp-diff-structured-golden.md)
