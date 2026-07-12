# Exercise 2: The (value, ok) Shape And Method Promotion Via Embedding

Not every absence is an error. A query parameter the client did not send is a
normal outcome the caller should branch on, not log. This exercise builds the
`(value, ok)` accessors — `First` and `All` — over a `Query` that embeds
`url.Values`, so `Get`, `Has`, `Add`, `Set`, `Del`, and `Encode` are promoted for
free.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
lookup/                    independent module: example.com/lookup
  go.mod                   go 1.25
  lookup.go                type Query embeds url.Values; First -> (string, bool); All -> []string
  cmd/
    demo/
      main.go              shows present/absent/repeated keys and a promoted method
  lookup_test.go           table tests for First/All; a promoted-method test; -race
```

- Files: `lookup.go`, `cmd/demo/main.go`, `lookup_test.go`.
- Implement: `Query` embedding `url.Values`; `First(key) (string, bool)` returning the first value and presence; `All(key) []string` returning all values or `nil`.
- Test: `First` returns `("", false)` for a missing key, `(v, true)` for a present key (including an empty-string value), and the first of repeated keys; `All` returns `nil` for a missing key and the full slice otherwise; a promoted-method test proves `q.Get` and `q.Has` work through the embed.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/01-function-declaration-and-multiple-return-values/02-value-ok-lookup-and-embedding/cmd/demo
cd go-solutions/04-functions/01-function-declaration-and-multiple-return-values/02-value-ok-lookup-and-embedding
go mod edit -go=1.25
```

### When (value, ok) is the right shape

The `(value, ok)` shape says: absence is part of the contract, so the caller
branches on `ok` and does not treat `false` as a failure. The two built-in sites —
map index `v, ok := m[key]` and type assertion `v, ok := x.(T)` — are the
templates. `First` imitates the map form directly: a repeated query key like
`?tag=go&tag=senior` stores `["go","senior"]`, and `First` returns the first plus
`true`; a key that was never sent returns `("", false)`, while a client sending
`?q=` (present, empty) returns `("", true)`. The distinction that trips people up
is exactly that *empty-string value*: `?q=` (present, empty) must return
`("", true)`, versus not sending `q` at all, which returns `("", false)`. A `bool` return is the only way
to tell those apart, because the zero value of the result is itself a legal value.

`All` returns the raw slice for a present key and `nil` for an absent one.
Returning `nil` rather than an empty non-nil slice is deliberate and idiomatic:
`len(nil) == 0`, `range nil` is a no-op, and the caller distinguishes "no such
key" from "key present with zero values" by checking `== nil` only if it cares.

### Embedding promotes the stdlib methods

`Query` embeds `url.Values` (which is `map[string][]string`). Because the field is
*anonymous*, Go promotes every method of `url.Values` onto `Query`: `q.Get(k)`,
`q.Has(k)`, `q.Add(k, v)`, `q.Set(k, v)`, `q.Del(k)`, and `q.Encode()` all work
with no forwarding code. A senior codebase does not re-declare `Get` or reinvent
`Encode`; it embeds the stdlib type and adds only the two shapes the stdlib lacks.
The trade-off to know: embedding also promotes the *underlying map operations*, so
`q.Values[key]` is reachable — the wrapper is thin, not a sealed abstraction.

Create `lookup.go`:

```go
package qparser

import "net/url"

// Query embeds url.Values so Get, Has, Add, Set, Del, and Encode are promoted.
// It adds First (the (value, ok) shape) and All.
type Query struct {
	url.Values
}

// Parse wraps already-parsed url.Values.
func Parse(v url.Values) Query {
	return Query{Values: v}
}

// First returns the first value for key and whether the key was present. A key
// sent with an empty value (?q=) returns ("", true); a key never sent returns
// ("", false). The bool is the only way to tell those apart.
func (q Query) First(key string) (string, bool) {
	values := q.Values[key]
	if len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// All returns every value for key, or nil if the key was not present.
func (q Query) All(key string) []string {
	return q.Values[key]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/url"

	"example.com/lookup"
)

func main() {
	v, _ := url.ParseQuery("tag=go&tag=senior&q=")
	q := qparser.Parse(v)

	first, ok := q.First("tag")
	fmt.Printf("First(tag)=%q ok=%t\n", first, ok)

	empty, ok := q.First("q")
	fmt.Printf("First(q)=%q ok=%t\n", empty, ok)

	_, ok = q.First("missing")
	fmt.Printf("First(missing) ok=%t\n", ok)

	fmt.Printf("All(tag)=%v\n", q.All("tag"))
	fmt.Printf("All(missing) is nil=%t\n", q.All("missing") == nil)

	// Promoted from the embedded url.Values, no forwarding code written:
	fmt.Printf("Get(tag)=%q Has(q)=%t\n", q.Get("tag"), q.Has("q"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
First(tag)="go" ok=true
First(q)="" ok=true
First(missing) ok=false
All(tag)=[go senior]
All(missing) is nil=true
Get(tag)="go" Has(q)=true
```

### Tests

Create `lookup_test.go`:

```go
package qparser

import (
	"fmt"
	"net/url"
	"testing"
)

func mustParse(t *testing.T, raw string) Query {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", raw, err)
	}
	return Parse(v)
}

func TestFirst(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		raw     string
		key     string
		wantVal string
		wantOK  bool
	}{
		{"present", "page=1", "page", "1", true},
		{"empty value present", "q=", "q", "", true},
		{"first of many", "tag=go&tag=senior", "tag", "go", true},
		{"missing", "page=1", "nope", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotVal, gotOK := mustParse(t, tc.raw).First(tc.key)
			if gotVal != tc.wantVal || gotOK != tc.wantOK {
				t.Fatalf("First(%q) = (%q, %t), want (%q, %t)", tc.key, gotVal, gotOK, tc.wantVal, tc.wantOK)
			}
		})
	}
}

func TestAll(t *testing.T) {
	t.Parallel()

	q := mustParse(t, "tag=go&tag=senior")
	if got := q.All("tag"); len(got) != 2 || got[0] != "go" || got[1] != "senior" {
		t.Fatalf("All(tag) = %v", got)
	}
	if got := q.All("missing"); got != nil {
		t.Fatalf("All(missing) = %v, want nil", got)
	}
}

func TestPromotedMethods(t *testing.T) {
	t.Parallel()

	q := mustParse(t, "a=1&a=2&q=")
	if got := q.Get("a"); got != "1" {
		t.Fatalf("promoted Get(a) = %q, want 1", got)
	}
	if !q.Has("a") {
		t.Fatal("promoted Has(a) = false, want true")
	}
	if !q.Has("q") {
		t.Fatal("promoted Has(q) = false, want true (empty value is still present)")
	}
	if q.Has("missing") {
		t.Fatal("promoted Has(missing) = true, want false")
	}
}

func ExampleQuery_First() {
	v, _ := url.ParseQuery("tag=go&tag=senior")
	first, ok := Parse(v).First("tag")
	fmt.Println(first, ok)
	// Output: go true
}
```

## Review

The `(value, ok)` shape is correct when a present key with an empty value returns
`("", true)` and an absent key returns `("", false)` — `TestFirst` pins exactly
that boundary, and it is the reason a bare `string` return would be wrong here.
`All` returning `nil` for a missing key is idiomatic, not a bug: the caller ranges
over it safely and only checks `== nil` if it needs to distinguish absence from an
empty list.

The embedding test is the other half of the lesson. `TestPromotedMethods` proves
that `Get` and `Has` came from the embedded `url.Values` with no code written for
them; if you had declared `Query` as a named type `type Query url.Values` instead
of embedding, those methods would *not* be promoted and the test would fail to
compile. Embedding, not aliasing, is what buys the promotion.

## Resources

- [net/url.Values](https://pkg.go.dev/net/url#Values) — the embedded type and its `Get`/`Has`/`Encode` methods.
- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — how anonymous fields promote methods.
- [Go Spec: Struct types](https://go.dev/ref/spec#Struct_types) — the promotion rules for embedded fields.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-typed-accessors-value-error.md](01-typed-accessors-value-error.md) | Next: [03-http-handler-multi-return-unpack.md](03-http-handler-multi-return-unpack.md)
