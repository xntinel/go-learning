# Exercise 33: HTTP Header Deduplication Merger Preserving Order

**Nivel: Intermedio** — validacion rapida (un test corto).

A reverse proxy builds the final request headers in layers: a base set
from the original client request, then overrides from routing rules, then
overrides from an auth middleware — and HTTP header names are
case-insensitive, so `Content-Type` and `content-type` from two different
layers must collapse into one header, not both survive as separate
entries. `MergeHeaders(sets ...[]Header)` merges any number of header
lists into one, letting later lists override earlier values while never
losing the original name casing or position.

## What you'll build

```text
headers/                   independent module: example.com/headers
  go.mod                   go 1.24
  headers.go               package headers; type Header struct{Name, Value string}; MergeHeaders(sets ...[]Header) []Header
  cmd/
    demo/
      main.go              runnable demo: base headers plus a proxy layer overriding content-type and adding one more
  headers_test.go          table tests: order + casing preserved, last value wins across three layers, empty input, nil sets skipped
```

- Files: `headers.go`, `cmd/demo/main.go`, `headers_test.go`.
- Implement: `type Header struct{ Name, Value string }` and `MergeHeaders(sets ...[]Header) []Header`.
- Test: merging a base set and an override set keeps `Content-Type`'s original casing and position while taking the override's value; three layers redefining the same header (with varying casing) collapse to the last value under the first-seen name; zero sets, and `nil` sets mixed with real ones, both behave sensibly.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/33-header-dedup-merge-preserve-order/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/33-header-dedup-merge-preserve-order
go mod edit -go=1.24
```

### Why case-insensitive keying, and why casing and position come from the *first* occurrence

RFC 7230 is explicit that HTTP header field names are case-insensitive,
so a merger that compares `Header.Name` with plain `==` would treat
`Content-Type` and `content-type` as two unrelated headers and emit both
— which is a real, if occasionally intermittent, bug: whichever HTTP
library serializes the final request might coalesce them anyway, might
send both and let the server pick one arbitrarily, or might reject the
request outright for a duplicated header it doesn't expect twice.
`MergeHeaders` keys its internal lookup on `strings.ToLower(h.Name)`,
which is the RFC-correct notion of "the same header."

The choice of whose casing and position wins is deliberate and asymmetric
from whose value wins. Position and casing come from the *first* set that
mentions a header — so `Content-Type` first defined in the base layer
keeps that title-case spelling and stays at its original index in the
output — while the *value* comes from the *last* set that mentions it,
because later layers are expected to be more specific (a proxy's routing
rule is more authoritative about the effective `Content-Type` than
whatever the original client happened to send). This mirrors the same
"insertion order preserved, value overwritten" behavior seen in the
telemetry tag encoder exercise, applied here across multiple *lists*
merged in sequence rather than multiple functional options applied to one
event.

Create `headers.go`:

```go
// headers.go
package headers

import "strings"

// Header is one HTTP header name/value pair.
type Header struct {
	Name  string
	Value string
}

// MergeHeaders merges any number of header lists into one, in the order
// the lists (and the headers within each list) were given. Header names
// are compared case-insensitively per RFC 7230 (Content-Type and
// content-type are the same header): the first time a name is seen, its
// original casing and its position in the result are fixed; every later
// occurrence of that name (in the same or a later list) only overwrites
// its value, so the last value given for a name wins while the header
// never moves and never duplicates.
func MergeHeaders(sets ...[]Header) []Header {
	var merged []Header
	index := make(map[string]int) // lowercased name -> index in merged

	for _, set := range sets {
		for _, h := range set {
			key := strings.ToLower(h.Name)
			if i, ok := index[key]; ok {
				merged[i].Value = h.Value
				continue
			}
			index[key] = len(merged)
			merged = append(merged, h)
		}
	}
	return merged
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/headers"
)

func main() {
	base := []headers.Header{
		{Name: "Content-Type", Value: "text/plain"},
		{Name: "X-Request-ID", Value: "abc-1"},
	}
	proxyOverrides := []headers.Header{
		{Name: "content-type", Value: "application/json"}, // overwrites, keeps "Content-Type" casing+position
		{Name: "Cache-Control", Value: "no-store"},
	}

	merged := headers.MergeHeaders(base, proxyOverrides)
	for _, h := range merged {
		fmt.Printf("%s: %s\n", h.Name, h.Value)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Content-Type: application/json
X-Request-ID: abc-1
Cache-Control: no-store
```

### Tests

`TestMergeHeadersLastValueWins` is the strictest one: it feeds `X-Trace`
under three different casings across three separate lists and asserts the
result has exactly one entry, spelled the way it was first written
(`X-Trace`), holding the value from the third and final list (`"3"`).

Create `headers_test.go`:

```go
// headers_test.go
package headers

import "testing"

func TestMergeHeadersPreservesOrderAndCasing(t *testing.T) {
	t.Parallel()

	merged := MergeHeaders(
		[]Header{{Name: "Content-Type", Value: "text/plain"}, {Name: "X-Request-ID", Value: "abc-1"}},
		[]Header{{Name: "content-type", Value: "application/json"}, {Name: "Cache-Control", Value: "no-store"}},
	)

	want := []Header{
		{Name: "Content-Type", Value: "application/json"},
		{Name: "X-Request-ID", Value: "abc-1"},
		{Name: "Cache-Control", Value: "no-store"},
	}
	if len(merged) != len(want) {
		t.Fatalf("merged = %v, want %v", merged, want)
	}
	for i, h := range merged {
		if h != want[i] {
			t.Errorf("merged[%d] = %v, want %v", i, h, want[i])
		}
	}
}

func TestMergeHeadersLastValueWins(t *testing.T) {
	t.Parallel()

	merged := MergeHeaders(
		[]Header{{Name: "X-Trace", Value: "1"}},
		[]Header{{Name: "X-Trace", Value: "2"}},
		[]Header{{Name: "X-TRACE", Value: "3"}},
	)
	if len(merged) != 1 {
		t.Fatalf("merged = %v, want 1 header", merged)
	}
	if merged[0].Name != "X-Trace" {
		t.Errorf("Name = %q, want first-seen casing %q", merged[0].Name, "X-Trace")
	}
	if merged[0].Value != "3" {
		t.Errorf("Value = %q, want last value %q", merged[0].Value, "3")
	}
}

func TestMergeHeadersNoSetsIsEmpty(t *testing.T) {
	t.Parallel()

	merged := MergeHeaders()
	if len(merged) != 0 {
		t.Fatalf("merged = %v, want empty", merged)
	}
}

func TestMergeHeadersEmptySetsAreSkipped(t *testing.T) {
	t.Parallel()

	merged := MergeHeaders(nil, []Header{{Name: "A", Value: "1"}}, nil)
	if len(merged) != 1 || merged[0].Name != "A" {
		t.Fatalf("merged = %v, want [{A 1}]", merged)
	}
}
```

## Review

`MergeHeaders` is correct when header names are compared case-
insensitively, every header ends up at the position and casing of its
first occurrence, and its final value comes from the last list that
mentioned it. The senior point is separating *identity* (case-insensitive
name, first-seen casing and position) from *value* (last-write-wins) —
conflating the two, by keying on the exact-case string or by letting the
last occurrence also relocate the header, produces output that is
technically mergeable but behaves unpredictably the moment two layers use
different casing for the same header, which real proxies, load
balancers, and HTTP client libraries do constantly.

## Resources

- [RFC 7230 §3.2: header field case-insensitivity](https://www.rfc-editor.org/rfc/rfc7230#section-3.2)
- [`net/http.Header.Set` and canonicalization](https://pkg.go.dev/net/http#CanonicalHeaderKey)
- [`strings.ToLower`](https://pkg.go.dev/strings#ToLower)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-event-telemetry-encoder-tags.md](32-event-telemetry-encoder-tags.md) | Next: [34-json-schema-validator-rules-aggregate.md](34-json-schema-validator-rules-aggregate.md)
