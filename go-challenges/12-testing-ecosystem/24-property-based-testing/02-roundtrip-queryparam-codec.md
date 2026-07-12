# Exercise 2: Round-Trip Property for a URL Query-Param Codec

Every list endpoint has a typed filter that must survive a trip through the URL:
the client encodes it into a query string, the server decodes it back. The
round-trip property `Decode(Encode(x)) == x` is the single most valuable property
for any serialization boundary, and it is where reserved characters, empty-vs-absent
distinctions, and slice ordering quietly corrupt data. This exercise builds the
codec and tests it with `pgregory.net/rapid`, whose generator manufactures the
filters no one writes a table row for.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
qcodec/                     independent module: example.com/qcodec
  go.mod                    go 1.26, requires pgregory.net/rapid
  qcodec.go                 type Filter; Encode(Filter) string; Decode(string) (Filter, error)
  cmd/
    demo/
      main.go               runnable demo: encode a filter, print it, decode it back
  qcodec_test.go            rapid round-trip property with a Custom[Filter] generator
```

Files: `qcodec.go`, `cmd/demo/main.go`, `qcodec_test.go`.
Implement: a `Filter` struct and an `Encode`/`Decode` pair to and from a URL query string using `net/url`.
Test: a `rapid.Check` round-trip property over a `rapid.Custom[Filter]` generator producing adversarial-but-valid filters; assert `Decode(Encode(f))` deep-equals `f`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get pgregory.net/rapid@latest
```

### The round-trip property and the direction that matters

The `Filter` carries a free-text search term, a page number and size, an ordered
list of tags, and an `Active` flag. `Encode` turns it into a query string;
`Decode` parses one back. The property is `Decode(Encode(f)) == f`: whatever the
client had, the server reconstructs exactly.

The direction is deliberate. `Decode(Encode(x)) == x` starts from a typed value
and is the property you want, because `Encode` produces a *canonical* string for
each value. The reverse direction, `Encode(Decode(s)) == s`, is weaker and often
false: many different query strings decode to the same `Filter` (different key
order, `%20` versus `+`, an omitted default), so re-encoding produces one
canonical string that need not equal the arbitrary input `s`. When you test a
codec, test the value-first direction; expecting the byte-first direction to hold
is a common way to chase a non-bug.

Three details make or break the round trip, and each is a real production bug when
missed. First, reserved characters — `&`, `=`, `?`, `#`, spaces, Unicode — must be
percent-escaped by `Encode` and unescaped by `Decode`; `net/url` does this, which
is exactly why you build on it instead of concatenating strings by hand. Second,
the empty-versus-absent distinction: a present-but-empty search term (`q=`) must
decode back to `""`, not to "absent", so `Encode` always emits the `q` key even
when the value is empty. Third, slice handling: the tags are an ordered list
encoded as repeated `tag=` parameters, and `net/url` preserves the left-to-right
order of repeated keys, so the order round-trips — but an *empty* tag list must
decode back to a `nil` slice, not an empty non-nil slice, or `reflect.DeepEqual`
will report a false failure. The generator below produces `nil` for the empty case
so the comparison is honest.

Create `qcodec.go`:

```go
package qcodec

import (
	"net/url"
	"strconv"
)

// Filter is a typed list-endpoint filter that travels in the URL query string.
type Filter struct {
	Search string   // free text; may contain reserved characters or be empty
	Page   int      // zero-based page index
	Size   int      // page size
	Tags   []string // ordered list, encoded as repeated tag= params; nil when empty
	Active bool
}

// Encode renders a Filter as a canonical URL query string. Every scalar field is
// always emitted so that a present-but-empty value round-trips distinctly.
func Encode(f Filter) string {
	v := url.Values{}
	v.Set("q", f.Search)
	v.Set("page", strconv.Itoa(f.Page))
	v.Set("size", strconv.Itoa(f.Size))
	v.Set("active", strconv.FormatBool(f.Active))
	for _, tag := range f.Tags {
		v.Add("tag", tag)
	}
	return v.Encode()
}

// Decode parses a query string produced by Encode back into a Filter.
func Decode(s string) (Filter, error) {
	v, err := url.ParseQuery(s)
	if err != nil {
		return Filter{}, err
	}
	page, err := strconv.Atoi(v.Get("page"))
	if err != nil {
		return Filter{}, err
	}
	size, err := strconv.Atoi(v.Get("size"))
	if err != nil {
		return Filter{}, err
	}
	active, err := strconv.ParseBool(v.Get("active"))
	if err != nil {
		return Filter{}, err
	}
	f := Filter{
		Search: v.Get("q"),
		Page:   page,
		Size:   size,
		Active: active,
	}
	if tags := v["tag"]; len(tags) > 0 {
		f.Tags = tags
	}
	return f, nil
}
```

### The runnable demo

The demo encodes a filter whose search term contains reserved characters, prints
the escaped query string, decodes it, and shows the fields survived intact.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/qcodec"
)

func main() {
	f := qcodec.Filter{
		Search: "a&b=c",
		Page:   2,
		Size:   50,
		Tags:   []string{"go", "testing"},
		Active: true,
	}
	s := qcodec.Encode(f)
	fmt.Println("encoded:", s)

	back, err := qcodec.Decode(s)
	if err != nil {
		fmt.Println("decode error:", err)
		return
	}
	fmt.Printf("decoded: %+v\n", back)
	fmt.Println("round-trip ok:", back.Search == f.Search && back.Page == f.Page)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
encoded: active=true&page=2&q=a%26b%3Dc&size=50&tag=go&tag=testing
decoded: {Search:a&b=c Page:2 Size:50 Tags:[go testing] Active:true}
round-trip ok: true
```

### The property test

`rapid.Custom[Filter]` composes a generator from per-field generators drawn inside
the closure. `rapid.String()` produces strings including reserved characters,
Unicode, and empty; `rapid.IntRange` bounds page and size to realistic values;
`rapid.SliceOf(rapid.String())` produces the tag list, which the generator maps to
`nil` when empty so the round-trip comparison is exact. The property draws one
`Filter`, round-trips it, and asserts the result deep-equals the original. When a
codec bug exists — say `Encode` dropped the empty-search key — rapid shrinks the
counterexample to the minimal filter that exposes it (a `Filter{}` with an empty
search), which is far easier to debug than a random 4 KB filter.

Create `qcodec_test.go`:

```go
package qcodec

import (
	"fmt"
	"reflect"
	"testing"

	"pgregory.net/rapid"
)

func genFilter() *rapid.Generator[Filter] {
	return rapid.Custom(func(t *rapid.T) Filter {
		tags := rapid.SliceOf(rapid.String()).Draw(t, "tags")
		if len(tags) == 0 {
			tags = nil // an empty list decodes to nil; keep the generator honest
		}
		return Filter{
			Search: rapid.String().Draw(t, "search"),
			Page:   rapid.IntRange(0, 1_000_000).Draw(t, "page"),
			Size:   rapid.IntRange(1, 500).Draw(t, "size"),
			Tags:   tags,
			Active: rapid.Bool().Draw(t, "active"),
		}
	})
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		f := genFilter().Draw(t, "filter")
		got, err := Decode(Encode(f))
		if err != nil {
			t.Fatalf("Decode(Encode(%+v)) error: %v", f, err)
		}
		if !reflect.DeepEqual(got, f) {
			t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, f)
		}
	})
}

// TestDecodeRejectsGarbage confirms Decode surfaces an error on a malformed page
// value rather than silently returning a zero Filter.
func TestDecodeRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := Decode("page=notanumber&size=10&active=true&q="); err == nil {
		t.Fatal("Decode accepted a non-numeric page")
	}
}

func ExampleEncode() {
	fmt.Println(Encode(Filter{Search: "hi", Page: 1, Size: 20, Active: false}))
	// Output: active=false&page=1&q=hi&size=20
}
```

## Review

The codec is correct when `Decode(Encode(f))` deep-equals `f` for every generated
`Filter`, which holds because `Encode` always emits every scalar key (preserving
present-but-empty values), builds on `net/url` for escaping, and encodes tags as
ordered repeated keys, while `Decode` reverses each step and maps an absent tag
key back to `nil`. The rapid property proves this over generated filters with
reserved characters and Unicode that a table would never include.

The mistakes to avoid are the three that break real query codecs. First, do not
concatenate the query string by hand — a `&` or `=` in the search term silently
corrupts the parse; `net/url` escaping exists for exactly this. Second, do not
conflate present-empty with absent: emit the key even when the value is empty, or
`""` and "missing" become indistinguishable after a round trip. Third, mind
`nil`-versus-empty slices: `reflect.DeepEqual(nil, []string{})` is false, so either
`Decode` must produce `nil` for an absent list (as here) or the generator must
never produce an empty non-nil slice. Test the value-first direction only;
expecting `Encode(Decode(s)) == s` to hold for arbitrary `s` is chasing a non-bug,
because encoding is canonical.

## Resources

- [`net/url`](https://pkg.go.dev/net/url) — `url.Values`, `Encode`, and `ParseQuery`, the escaping and key-ordering rules this codec relies on.
- [`pgregory.net/rapid`](https://pkg.go.dev/pgregory.net/rapid) — `Check`, `Custom`, `String`, `IntRange`, `SliceOf`, and `Bool`.
- [Choosing properties for property-based testing](https://fsharpforfunandprofit.com/posts/property-based-testing-2/) — the round-trip pattern and why the value-first direction is the one to assert.

---

Back to [01-money-algebraic-invariants.md](01-money-algebraic-invariants.md) | Next: [03-idempotent-canonicalization.md](03-idempotent-canonicalization.md)
