# Exercise 5: A Defined Map Type for Canonical HTTP-Style Headers

HTTP header names are case-insensitive, but a bare `map[string][]string` is not:
`m["Content-Type"]` and `m["content-type"]` are different keys, and callers who
disagree on casing silently miss each other's values. This exercise builds a
mini `net/http.Header` — `type Header map[string][]string` with `Get`/`Set`/`Add`/
`Del` methods that canonicalize keys via `net/textproto`, so the case-insensitive
behavior lives on the type instead of being re-derived at every call site.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
header/                   independent module: example.com/header
  go.mod                  go 1.24
  header.go               type Header map[string][]string; Get/Set/Add/Del/Values
  cmd/
    demo/
      main.go             sets a header with one casing, reads it with another
  header_test.go          case-insensitivity, accumulation, deletion, range tests
```

- Files: `header.go`, `cmd/demo/main.go`, `header_test.go`.
- Implement: `Header` with `Set`, `Add`, `Get`, `Del`, and `Values`, all canonicalizing the key with `textproto.CanonicalMIMEHeaderKey`.
- Test: `Set` then `Get` with differently-cased keys returns the value; `Add` accumulates; `Del` removes; `Get` on a missing key returns `""`; the underlying map still ranges.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/06-type-aliases-vs-type-definitions/05-header-canonicalization-defined-map/cmd/demo
cd go-solutions/02-variables-types-and-constants/06-type-aliases-vs-type-definitions/05-header-canonicalization-defined-map
go mod edit -go=1.24
```

### Why the behavior belongs on the type

You can attach methods to any type defined in your package, including a map type.
`type Header map[string][]string` is a defined map: it has the same
representation as a plain map, but it carries methods, and its methods are the
*only* thing that should touch the map so the canonicalization invariant holds.
The invariant is: every key stored or looked up is first run through
`textproto.CanonicalMIMEHeaderKey`, which maps `content-type`, `Content-Type`, and
`CONTENT-TYPE` all to the canonical `Content-Type`. Do the canonicalization inside
`Set`/`Add`/`Get`/`Del` and a caller can use any casing and still hit the same
entry.

This is exactly how `net/http.Header` is defined in the standard library — a
defined map type with `Get`/`Set`/`Add`/`Del` that call
`textproto.CanonicalMIMEHeaderKey`. The lesson is that the domain rule (headers are
case-insensitive) is a property of the *type*, so it lives on the type; scattering
`strings.ToLower` calls at every read and write is how you end up with one forgotten
call site and a header that silently does not match.

`Get` returns the first value or `""` for a missing key, matching
`http.Header.Get`; `Values` returns the full slice for callers that need every
value of a repeated header. Because `Header` is still a map underneath, a caller can
`range` it directly for iteration — the methods add behavior without hiding the
representation.

Create `header.go`:

```go
package header

import "net/textproto"

// Header is a defined map type modeling case-insensitive HTTP-style headers. Its
// methods canonicalize every key, so lookups are casing-agnostic. It mirrors the
// shape of net/http.Header.
type Header map[string][]string

// Set replaces any existing values for key with a single value.
func (h Header) Set(key, value string) {
	h[textproto.CanonicalMIMEHeaderKey(key)] = []string{value}
}

// Add appends a value to the (possibly empty) list for key.
func (h Header) Add(key, value string) {
	ck := textproto.CanonicalMIMEHeaderKey(key)
	h[ck] = append(h[ck], value)
}

// Get returns the first value associated with key, or "" if there is none.
func (h Header) Get(key string) string {
	if h == nil {
		return ""
	}
	vs := h[textproto.CanonicalMIMEHeaderKey(key)]
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}

// Values returns all values associated with key, or nil if there are none.
func (h Header) Values(key string) []string {
	if h == nil {
		return nil
	}
	return h[textproto.CanonicalMIMEHeaderKey(key)]
}

// Del removes all values associated with key.
func (h Header) Del(key string) {
	delete(h, textproto.CanonicalMIMEHeaderKey(key))
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/header"
)

func main() {
	h := header.Header{}
	h.Set("content-type", "application/json") // lower-case write
	h.Add("X-Trace-Id", "abc")
	h.Add("x-trace-id", "def") // mixed casing, same header

	fmt.Println("Content-Type:", h.Get("Content-Type")) // upper-case read
	fmt.Println("X-Trace-Id count:", len(h.Values("X-TRACE-ID")))

	h.Del("CONTENT-TYPE")
	fmt.Printf("after delete, Content-Type = %q\n", h.Get("content-type"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Content-Type: application/json
X-Trace-Id count: 2
after delete, Content-Type = ""
```

### Tests

The tests prove that casing does not matter across `Set`/`Get`/`Del`, that `Add`
accumulates repeated values, that a missing key reads as `""`, and that the value
is stored under exactly one canonical key (so the underlying map has one entry, not
one per casing). A final test ranges the map to confirm the defined type is still
iterable as a plain map.

Create `header_test.go`:

```go
package header

import (
	"fmt"
	"testing"
)

func TestSetGetIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	h := Header{}
	h.Set("content-type", "application/json")

	for _, key := range []string{"Content-Type", "content-type", "CONTENT-TYPE"} {
		if got := h.Get(key); got != "application/json" {
			t.Errorf("Get(%q) = %q, want application/json", key, got)
		}
	}
	// Stored under exactly one canonical key.
	if len(h) != 1 {
		t.Fatalf("map has %d keys, want 1", len(h))
	}
	if _, ok := h["Content-Type"]; !ok {
		t.Fatal("value not stored under canonical key Content-Type")
	}
}

func TestAddAccumulates(t *testing.T) {
	t.Parallel()

	h := Header{}
	h.Add("X-Trace-Id", "one")
	h.Add("x-trace-id", "two")
	h.Add("X-TRACE-ID", "three")

	vs := h.Values("x-Trace-id")
	if len(vs) != 3 {
		t.Fatalf("Values len = %d, want 3", len(vs))
	}
	if h.Get("X-Trace-Id") != "one" {
		t.Errorf("Get = %q, want first value 'one'", h.Get("X-Trace-Id"))
	}
}

func TestDelAndMissing(t *testing.T) {
	t.Parallel()

	h := Header{}
	h.Set("Accept", "text/html")
	h.Del("accept")

	if got := h.Get("Accept"); got != "" {
		t.Errorf("after Del, Get = %q, want empty", got)
	}
	if got := h.Get("Never-Set"); got != "" {
		t.Errorf("missing key Get = %q, want empty", got)
	}
}

func TestNilHeaderReads(t *testing.T) {
	t.Parallel()

	var h Header // nil map
	if got := h.Get("Content-Type"); got != "" {
		t.Errorf("nil Get = %q, want empty", got)
	}
	if vs := h.Values("Content-Type"); vs != nil {
		t.Errorf("nil Values = %v, want nil", vs)
	}
}

func TestUnderlyingMapRanges(t *testing.T) {
	t.Parallel()

	h := Header{}
	h.Set("A", "1")
	h.Set("B", "2")

	keys := 0
	for range h { // the defined type is still a rangeable map
		keys++
	}
	if keys != 2 {
		t.Fatalf("ranged %d keys, want 2", keys)
	}
}

func ExampleHeader_Get() {
	h := Header{}
	h.Set("content-type", "application/json")
	fmt.Println(h.Get("Content-Type"))
	// Output: application/json
}
```

## Review

The type is correct when every method canonicalizes its key, so a value written
as `content-type` is readable as `Content-Type` and stored under exactly one map
entry. The mistake this exercise guards against is doing canonicalization at the
call site instead of on the type: one handler lowercases, another uses
`textproto`, a third forgets, and lookups miss. Putting the rule in `Set`/`Get`/
`Add`/`Del` makes it impossible to store a non-canonical key through the API.
Guard the read methods against a nil `Header` (a zero-value map is legal to read),
and remember the type is still a plain map you can `range` for iteration.

## Resources

- [`net/http.Header`](https://pkg.go.dev/net/http#Header) — the standard library's defined map type this mirrors.
- [`net/textproto.CanonicalMIMEHeaderKey`](https://pkg.go.dev/net/textproto#CanonicalMIMEHeaderKey) — the exact canonicalization used.
- [Go Language Spec: Method sets](https://go.dev/ref/spec#Method_sets) — methods on a defined map type.

---

Prev: [04-byte-size-unit-type.md](04-byte-size-unit-type.md) | Next: [06-trusted-untrusted-string-safety-types.md](06-trusted-untrusted-string-safety-types.md)
