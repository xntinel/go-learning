# Exercise 6: A List Endpoint That Returns [] Not null

A list endpoint whose result slice can be nil ships `[]` when rows matched and
`null` when they did not — and the `null` breaks every client that does
`resp.items.map(...)` or `for...of resp.items`. This exercise builds a response
encoder that guarantees an empty-but-non-nil slice so the wire format is a stable
`[]`, and shows where `omitempty` deliberately relies on the opposite behavior.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
listapi/                   independent module: example.com/listapi
  go.mod
  listapi.go               Item, ListResponse, NewListResponse, Handler
  cmd/
    demo/
      main.go              marshals nil vs empty vs populated, prints JSON
  listapi_test.go          exact-bytes marshal tests + httptest handler
```

Files: `listapi.go`, `cmd/demo/main.go`, `listapi_test.go`.
Implement: `NewListResponse(items)` that forces a non-nil slice so it marshals to `[]`, an HTTP `Handler` that writes it, and an `Item` with an `omitempty` field to contrast.
Test: a nil slice under naive encoding produces `null` (demonstrated), the fixed constructor produces `[]`, a populated slice produces the expected array, and the `omitempty` field is dropped when empty. Assert exact JSON bytes.
Verify: `go test -count=1 -race ./...`

## Why nil marshals to null, and the boundary fix

`encoding/json` distinguishes a nil slice from an empty non-nil slice on the
wire: `json.Marshal([]Item(nil))` produces `null`, while `json.Marshal([]Item{})`
produces `[]`. Both have `len == 0`, so the difference is invisible in Go and
loud in JSON. A repository query that returns `nil` on "no rows" (a very common
shape — `var items []Item` filled by `append`, never appended to) therefore emits
`null` exactly when the result is empty, which is the case clients are least
prepared for.

The fix belongs at the API boundary, not scattered through query code:
`NewListResponse` normalizes a possibly-nil slice to a guaranteed non-nil one
(`if items == nil { items = []Item{} }`) before wrapping it in the response
struct. Now the field always marshals to an array — `[]` when empty, `[...]` when
populated — and the contract "this field is always a JSON array" holds for every
response. The handler writes that response, so the endpoint's wire shape is
stable regardless of what the data layer handed up.

`omitempty` is the other half of the story, and it is not a contradiction: it is
*defined* in terms of the zero value. A field tagged `json:",omitempty"` is
omitted entirely when its value is the zero value — for a slice, when it is nil
or empty. That is exactly what you want for a genuinely optional field (drop
`tags` when there are none) and exactly what you must *not* put on a required
array field (or it disappears when empty, which is a different broken contract).
The `Item.Tags` field demonstrates the deliberate use.

Create `listapi.go`:

```go
package listapi

import (
	"encoding/json"
	"net/http"
)

// Item is one element of the list. Tags is optional and uses omitempty, so an
// empty Tags is dropped from the JSON entirely.
type Item struct {
	ID   int      `json:"id"`
	Name string   `json:"name"`
	Tags []string `json:"tags,omitempty"`
}

// ListResponse is the endpoint's envelope. Items is always a JSON array.
type ListResponse struct {
	Items []Item `json:"items"`
}

// NewListResponse normalizes a possibly-nil slice to a non-nil one so Items
// marshals to [] rather than null when empty.
func NewListResponse(items []Item) ListResponse {
	if items == nil {
		items = []Item{}
	}
	return ListResponse{Items: items}
}

// Handler writes the given items as a stable JSON list.
type Handler struct {
	items []Item
}

// NewHandler returns a Handler that serves the given items (may be nil).
func NewHandler(items []Item) *Handler {
	return &Handler{items: items}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(NewListResponse(h.items))
}
```

## The runnable demo

The demo marshals three cases side by side: a raw nil slice (the bug), the same
nil routed through `NewListResponse` (the fix), and a populated slice with and
without tags. It prints the exact JSON so you can see `null` become `[]`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/listapi"
)

func main() {
	var raw []listapi.Item // nil

	naive, _ := json.Marshal(raw)
	fmt.Printf("naive nil slice:   %s\n", naive)

	fixed, _ := json.Marshal(listapi.NewListResponse(raw))
	fmt.Printf("fixed empty list:  %s\n", fixed)

	populated, _ := json.Marshal(listapi.NewListResponse([]listapi.Item{
		{ID: 1, Name: "alpha", Tags: []string{"x"}},
		{ID: 2, Name: "beta"},
	}))
	fmt.Printf("populated list:    %s\n", populated)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
naive nil slice:   null
fixed empty list:  {"items":[]}
populated list:    {"items":[{"id":1,"name":"alpha","tags":["x"]},{"id":2,"name":"beta"}]}
```

## Tests

`TestNaiveNilMarshalsNull` documents the bug: a bare nil slice marshals to the
bytes `null`. `TestNewListResponse` is the table asserting the fixed encoder
produces `[]` for nil and empty inputs and the exact array for a populated one.
`TestOmitemptyDropsField` proves the `Tags` field vanishes when empty.
`TestHandler` drives the endpoint through `httptest` and asserts the body is
`{"items":[]}` for a nil backing slice.

Create `listapi_test.go`:

```go
package listapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNaiveNilMarshalsNull(t *testing.T) {
	t.Parallel()

	var raw []Item // nil
	got, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != "null" {
		t.Fatalf("naive nil slice marshaled to %s, want null", got)
	}
}

func TestNewListResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		items []Item
		want  string
	}{
		{"nil becomes empty array", nil, `{"items":[]}`},
		{"empty stays empty array", []Item{}, `{"items":[]}`},
		{
			name:  "populated array",
			items: []Item{{ID: 1, Name: "alpha"}},
			want:  `{"items":[{"id":1,"name":"alpha"}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(NewListResponse(tt.items))
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("Marshal = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestOmitemptyDropsField(t *testing.T) {
	t.Parallel()

	got, err := json.Marshal(Item{ID: 1, Name: "alpha"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(got), "tags") {
		t.Fatalf("Marshal = %s, want no tags field for empty Tags", got)
	}
}

func TestHandler(t *testing.T) {
	t.Parallel()

	h := NewHandler(nil) // nil backing slice
	req := httptest.NewRequest(http.MethodGet, "/items", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := strings.TrimSpace(rec.Body.String()); got != `{"items":[]}` {
		t.Fatalf("body = %s, want {\"items\":[]}", got)
	}
}
```

## Review

The endpoint is correct when its list field is always a JSON array, no matter
what the data layer returned. The bug is subtle because Go treats a nil and an
empty slice identically for `len`/`range`/`append`, so it hides until a client
chokes on `null`; the fix is to normalize to non-nil at the single boundary where
the response is built, not to hope every query returns `[]Item{}`. Do not put
`omitempty` on a required array — it reintroduces a different broken contract
(the field disappears when empty). The tests assert exact bytes on purpose: a
test that only checks `len` would pass against the `null` bug. Note `TestHandler`
trims the trailing newline that `json.Encoder.Encode` appends.

## Resources

- [`encoding/json.Marshal`](https://pkg.go.dev/encoding/json#Marshal) — nil slice/map encode to `null`; empty non-nil encode to `[]`/`{}`.
- [JSON and Go](https://go.dev/blog/json) — how the encoder maps Go values to JSON.
- [`json.Encoder.Encode`](https://pkg.go.dev/encoding/json#Encoder.Encode) — streams a value and appends a newline.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-last-seen-time-iszero.md](05-last-seen-time-iszero.md) | Next: [07-sync-once-lazy-singleton.md](07-sync-once-lazy-singleton.md)
