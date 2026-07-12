# Exercise 10: Golden-Test Guardrail That Fails When Map Order Leaks into a Response

The most insidious map bug is a handler that ranges a map straight into a JSON list: it
passes every hand test (the order looks fine once) and then returns a different byte
order on the next request, breaking any client that relied on it and any golden test
that pinned it. This module builds a guardrail test that runs a handler many times and
fails if two responses ever differ — turning a Hyrum's-law time bomb into a caught
regression — and the fixed handler that sorts keys first.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
listapi/                   independent module: example.com/listapi
  go.mod                   go 1.26
  listapi.go               type Item; store; OrderedHandler (sorted, correct)
  cmd/
    demo/
      main.go              serves the list via httptest, prints the JSON
  listapi_test.go          determinism guardrail (100 calls), exact-JSON golden
```

- Files: `listapi.go`, `cmd/demo/main.go`, `listapi_test.go`.
- Implement: `OrderedHandler` that sorts the store's keys before encoding, so the JSON
  list order is defined.
- Test: invoke the handler ~100 times and assert every response body is byte-identical;
  a second test pins the exact ordered JSON.
- Verify: `go test -count=1 -race ./...`

### The bug, and why hand-testing misses it

Here is the handler almost everyone writes first. It ranges the store map directly into
the response slice:

```go
// BUGGY: order of items is whatever the map range produced this call.
func buggyHandler(w http.ResponseWriter, r *http.Request) {
	items := make([]Item, 0, len(store))
	for _, it := range store { // randomized start group every call
		items = append(items, it)
	}
	json.NewEncoder(w).Encode(items)
}
```

It compiles, it returns the right *set* of items, and if you curl it once the order
looks reasonable. But Go randomizes the map range's starting group on every call, so the
JSON array order changes between requests. A consumer that (against the contract) indexed
`items[0]`, or a golden-file test that pinned the body, breaks intermittently — the
classic Hyrum's-law failure where an unpromised behavior became load-bearing. Because a
single manual test only observes one order, the bug ships.

The guardrail is a test that *invokes the handler many times and asserts all responses
are byte-identical*. Run against the buggy handler, it fails on most runs (some pair of
the ~100 responses differs). Run against the fixed handler, it always passes. That test
is cheap insurance you add wherever a handler serializes a collection.

The fix is the lesson's one rule: collect the keys, sort them, then build the response in
sorted order. `slices.Sorted(maps.Keys(store))` gives the sorted key list; iterating it
produces a deterministic slice. Now the response is a pure function of the store's
contents.

Create `listapi.go`:

```go
package listapi

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
)

// Item is one list element.
type Item struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// store is the in-memory data the handler serves. A real service would inject a
// repository; a package-level map keeps the exercise self-contained.
var store = map[int]Item{
	3: {ID: 3, Name: "gamma"},
	1: {ID: 1, Name: "alpha"},
	2: {ID: 2, Name: "beta"},
}

// OrderedHandler writes the items as a JSON array in ascending ID order. It sorts
// the keys before encoding, so the response is deterministic across calls.
func OrderedHandler(w http.ResponseWriter, r *http.Request) {
	ids := slices.Sorted(maps.Keys(store))
	items := make([]Item, 0, len(ids))
	for _, id := range ids {
		items = append(items, store[id])
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}
```

### The runnable demo

Create `cmd/demo/main.go`. It drives the handler through `httptest` so the demo needs no
listening socket:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/listapi"
)

func main() {
	req := httptest.NewRequest(http.MethodGet, "/items", nil)
	rec := httptest.NewRecorder()

	listapi.OrderedHandler(rec, req)

	fmt.Print(rec.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[{"id":1,"name":"alpha"},{"id":2,"name":"beta"},{"id":3,"name":"gamma"}]
```

(`json.Encoder.Encode` appends a trailing newline, so the printed line ends with one.)

### Tests

`TestResponseIsDeterministic` is the guardrail: it calls the handler 100 times, captures
each body, and fails if any body differs from the first. Against `OrderedHandler` it
always passes; swap in the buggy range-the-map version and it fails on most runs.
`TestExactJSON` pins the precise ordered body so a future reordering is caught even if it
somehow stayed self-consistent.

Create `listapi_test.go`:

```go
package listapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func callHandler(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/items", nil)
	rec := httptest.NewRecorder()
	OrderedHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func TestResponseIsDeterministic(t *testing.T) {
	t.Parallel()

	first := callHandler(t)
	for i := range 100 {
		if got := callHandler(t); got != first {
			t.Fatalf("response %d differs from the first:\n first = %s\n got   = %s", i, first, got)
		}
	}
}

func TestExactJSON(t *testing.T) {
	t.Parallel()

	want := `[{"id":1,"name":"alpha"},{"id":2,"name":"beta"},{"id":3,"name":"gamma"}]` + "\n"
	if got := callHandler(t); got != want {
		t.Fatalf("body mismatch:\n got:  %q\n want: %q", got, want)
	}
}

func TestContentType(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/items", nil)
	rec := httptest.NewRecorder()
	OrderedHandler(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}
```

Note the test reads a package-level `store` that is never written after init, so the
handler only *reads* the map concurrently across the parallel subtests — which is safe.
If the store were mutated at runtime you would need one of the concurrency-safe maps from
the earlier exercises.

## Review

The handler is correct when its body is a deterministic function of the store, and the
guardrail test is what proves it stays that way. The failure mode it defends against is
the subtlest in the chapter: ranging a map into a response passes a one-shot manual
check and then flaps in production because Go deliberately randomizes the range start.
The 100-call determinism test converts that latent Hyrum's-law bug into a red test on the
first run after someone "simplifies" the handler back to a raw range. The fix is always
the same — `slices.Sorted(maps.Keys(m))`, then iterate — and it is worth adding a
determinism guardrail to any endpoint that serializes a collection. Run `go test -race`.

## Resources

- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest` and `NewRecorder` for handler tests.
- [`slices.Sorted`](https://pkg.go.dev/slices#Sorted) and [`maps.Keys`](https://pkg.go.dev/maps#Keys) — the deterministic key order.
- [Hyrum's Law](https://www.hyrumslaw.com/) — why an unpromised order becomes a dependency.

---

Back to [00-concepts.md](00-concepts.md) | Next: [11-sharded-connection-pool-registry.md](11-sharded-connection-pool-registry.md)
