# Exercise 7: A Fluent Request Builder With Pointer-Receiver Chaining

A fluent builder — `b.Method("POST").URL("/x").JSONBody(v).Build()` — only works
because every setter mutates one shared struct and returns it. That demands pointer
receivers: a value receiver would mutate and return copies, and each link in the
chain would silently drop the previous link's work. This exercise builds an HTTP
`RequestBuilder` and proves the chain shares one struct.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
reqbuilder/               independent module: example.com/reqbuilder
  go.mod
  builder.go              RequestBuilder; Method/URL/Header/JSONBody (chaining); Build
  cmd/
    demo/
      main.go             build a POST request and print method/url/header/body
  builder_test.go         chain builds a correct *http.Request; validation; identity
```

Files: `builder.go`, `cmd/demo/main.go`, `builder_test.go`.
Implement: a `RequestBuilder` with pointer-receiver setters `Method`, `URL`, `Header`, `JSONBody` that each mutate `*RequestBuilder` and return it, plus `Build() (*http.Request, error)`.
Test: the chain produces a `*http.Request` with the right method, URL, headers, and body; `Build` rejects a missing method or URL; each setter returns the same `*RequestBuilder` (identity), so chaining shares one struct.
Verify: `go test -count=1 -race ./...`

### Why the receiver must be a pointer

A fluent (chained) API returns the receiver from each method so the next call
operates on the same object: `b.Method("POST").URL("/x")` calls `URL` on whatever
`Method` returned. For that to accumulate state, `Method` must return the *same*
builder it mutated. With a pointer receiver `func (b *RequestBuilder) Method(...) *RequestBuilder`,
the method mutates `*b` in place and returns `b` — the same pointer — so every link
sees every prior mutation. With a *value* receiver `func (b RequestBuilder) Method(...) RequestBuilder`,
each method gets a copy, mutates the copy, and returns the copy; the next link then
copies *that*, and while the final `Build` would see the last method's write, any
setter whose result you did not chain from is lost — and, more importantly, the whole
design is a copy-per-call trap that defeats the purpose of a builder. Pointer
receivers make the chain share one struct; that shared-struct identity is what the
test asserts (`b.Method(...) == b`).

`Build` is where accumulated state becomes a real `*http.Request`. It validates the
required fields (method and URL must be set), constructs the request via
`http.NewRequest` (which parses the URL and sets up the body), copies the accumulated
headers onto the request, and returns it. Deferring construction to `Build` — rather
than building the request incrementally — keeps validation in one place and lets the
setters stay trivial. The builder collects a `bytes.Reader` body from `JSONBody` by
marshaling the value up front, so `Build` can hand `http.NewRequest` an `io.Reader`.

Create `builder.go`:

```go
package reqbuilder

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

var ErrIncomplete = errors.New("request is incomplete")

// RequestBuilder accumulates request parts and is shared as *RequestBuilder so the
// fluent chain mutates one struct. Setters use pointer receivers and return b.
type RequestBuilder struct {
	method   string
	url      string
	header   http.Header
	body     io.Reader
	buildErr error
}

func New() *RequestBuilder {
	return &RequestBuilder{header: make(http.Header)}
}

func (b *RequestBuilder) Method(m string) *RequestBuilder {
	b.method = m
	return b
}

func (b *RequestBuilder) URL(u string) *RequestBuilder {
	b.url = u
	return b
}

func (b *RequestBuilder) Header(key, value string) *RequestBuilder {
	b.header.Set(key, value)
	return b
}

// JSONBody marshals v now and stores the bytes; a marshal error is deferred to Build.
func (b *RequestBuilder) JSONBody(v any) *RequestBuilder {
	data, err := json.Marshal(v)
	if err != nil {
		b.buildErr = err
		return b
	}
	b.body = bytes.NewReader(data)
	b.header.Set("Content-Type", "application/json")
	return b
}

// Build validates and constructs the *http.Request from the accumulated state.
func (b *RequestBuilder) Build() (*http.Request, error) {
	if b.buildErr != nil {
		return nil, b.buildErr
	}
	if b.method == "" || b.url == "" {
		return nil, ErrIncomplete
	}
	req, err := http.NewRequest(b.method, b.url, b.body)
	if err != nil {
		return nil, err
	}
	for k, vs := range b.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req, nil
}
```

### The runnable demo

The demo chains all four setters, builds the request, and prints its fields — reading
the body back out to show `JSONBody` populated it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"

	"example.com/reqbuilder"
)

func main() {
	req, err := reqbuilder.New().
		Method("POST").
		URL("https://api.example.com/v1/orders").
		Header("X-Request-ID", "abc123").
		JSONBody(map[string]any{"sku": "A", "qty": 2}).
		Build()
	if err != nil {
		fmt.Println("build error:", err)
		return
	}

	body, _ := io.ReadAll(req.Body)
	fmt.Printf("method=%s\n", req.Method)
	fmt.Printf("url=%s\n", req.URL.String())
	fmt.Printf("request-id=%s\n", req.Header.Get("X-Request-ID"))
	fmt.Printf("content-type=%s\n", req.Header.Get("Content-Type"))
	fmt.Printf("body=%s\n", body)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
method=POST
url=https://api.example.com/v1/orders
request-id=abc123
content-type=application/json
body={"qty":2,"sku":"A"}
```

(`encoding/json` sorts map keys, so the body is deterministic.)

### Tests

The tests build a request through the full chain and assert every field, check that
`Build` rejects a missing method or URL with `ErrIncomplete` via `errors.Is`, and
prove the identity property: each setter returns the same `*RequestBuilder`, so the
chain shares one struct rather than copying.

Create `builder_test.go`:

```go
package reqbuilder

import (
	"errors"
	"io"
	"testing"
)

func TestChainBuildsRequest(t *testing.T) {
	t.Parallel()
	req, err := New().
		Method("PUT").
		URL("https://example.com/x").
		Header("X-Test", "1").
		JSONBody(map[string]string{"k": "v"}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "PUT" {
		t.Fatalf("method = %q", req.Method)
	}
	if req.URL.String() != "https://example.com/x" {
		t.Fatalf("url = %q", req.URL.String())
	}
	if req.Header.Get("X-Test") != "1" {
		t.Fatalf("X-Test = %q", req.Header.Get("X-Test"))
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q", req.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(req.Body)
	if string(body) != `{"k":"v"}` {
		t.Fatalf("body = %s", body)
	}
}

func TestBuildRejectsMissingFields(t *testing.T) {
	t.Parallel()
	if _, err := New().URL("https://x").Build(); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("missing method: err = %v, want ErrIncomplete", err)
	}
	if _, err := New().Method("GET").Build(); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("missing url: err = %v, want ErrIncomplete", err)
	}
}

func TestSettersReturnSameBuilder(t *testing.T) {
	t.Parallel()
	b := New()
	if b.Method("GET") != b {
		t.Fatal("Method must return the same *RequestBuilder (identity)")
	}
	if b.URL("https://x") != b {
		t.Fatal("URL must return the same *RequestBuilder")
	}
	if b.Header("A", "b") != b {
		t.Fatal("Header must return the same *RequestBuilder")
	}
}

func TestJSONBodyMarshalErrorSurfacesAtBuild(t *testing.T) {
	t.Parallel()
	// A channel cannot be marshaled; the error must surface at Build.
	_, err := New().Method("POST").URL("https://x").JSONBody(make(chan int)).Build()
	if err == nil {
		t.Fatal("expected a marshal error at Build")
	}
	if errors.Is(err, ErrIncomplete) {
		t.Fatal("marshal error should not be reported as ErrIncomplete")
	}
}
```

## Review

The builder is correct when the full chain yields a `*http.Request` with the right
method, URL, headers, and body, when `Build` rejects an incomplete request with
`ErrIncomplete`, and when each setter returns the same builder. The identity test
(`b.Method("GET") == b`) is the one that proves the receiver is a pointer: with a
value receiver it would return a copy and the comparison would be false (and the
chain would lose state).

The mistakes: value receivers on the setters (mutations land on copies, the chain
breaks); building the request eagerly in each setter instead of once in `Build` (harder
to validate, and re-parsing the URL repeatedly); and swallowing the `JSONBody` marshal
error instead of deferring it to `Build`, where the caller actually checks `err`. Note
`ErrIncomplete` is a sentinel wrapped-comparable via `errors.Is`, and the marshal-error
test confirms the two failure modes stay distinct.

## Resources

- [`http.NewRequest`](https://pkg.go.dev/net/http#NewRequest) — constructing a request from method, URL, and body.
- [`http.Header`](https://pkg.go.dev/net/http#Header) — the header map and `Set`/`Add`/`Get`.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — why a mutating/chaining method needs a pointer receiver.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-sync-pool-reusable-buffers.md](08-sync-pool-reusable-buffers.md)
