# Exercise 2: Parse Authorization and Cache-Control headers with strings.Cut

HTTP headers are the daily strings a backend decodes: pull the bearer token out
of `Authorization`, read the caching policy out of `Cache-Control`. This module
builds both with `strings.CutPrefix` and `strings.Cut`, the modern
first-separator idioms that replace `SplitN`-plus-index-guard boilerplate.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
header/                     independent module: example.com/header
  go.mod                    go 1.26
  header.go                 ParseBearer, ParseDirectives
  cmd/
    demo/
      main.go               decode a real Authorization and Cache-Control pair
  header_test.go            RFC-example directive tables, bearer edge cases
```

Files: `header.go`, `cmd/demo/main.go`, `header_test.go`.
Implement: `ParseBearer(auth string) (token string, ok bool)` and
`ParseDirectives(cacheControl string) map[string]string`.
Test: RFC 9110/7234 header strings, valueless and quoted directives, malformed
and empty inputs.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/01-string-basics/02-header-and-cut/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/01-string-basics/02-header-and-cut
```

## Why Cut and CutPrefix over SplitN and manual slicing

`ParseBearer` must strip the scheme off `"Bearer <token>"`. The wrong way is
`auth[len("Bearer "):]`, which panics on any input shorter than the prefix. When
the scheme casing is fixed, `strings.CutPrefix(auth, "Bearer ")` is the one-call,
prefix-safe answer: it returns `(rest, found)`, and when the prefix is absent
`found` is false and `rest` is the original string, so there is no out-of-range
risk. HTTP, though, defines the scheme token as case-insensitive (`bearer`,
`BEARER`, and `Bearer` are all valid), so here we cut on the *first space* with
`strings.Cut(auth, " ")` to separate the scheme token from the value, compare that
token with `strings.EqualFold(scheme, "Bearer")`, and trim the value. That keeps
the match case-correct where a plain `CutPrefix("Bearer ")` would reject a
lowercase scheme.

`ParseDirectives` splits `Cache-Control` into its comma-separated directives, then
each directive into `name=value` on the *first* `=`. `strings.Cut(item, "=")`
returns `(name, value, found)`; a valueless directive like `no-store` has
`found == false`, so we store it with an empty value. Directive names are
case-insensitive, so they are lowered for the map key. Values may be quoted
(`max-age="600"` is legal), so surrounding quotes are trimmed. Every field is
`TrimSpace`d because `public, max-age=600` has a space after the comma. Doing this
with `SplitN(item, "=", 2)` would work but forces a `len(parts)` check on every
iteration; `Cut`'s `found` bool expresses the same branch without the slice.

Create `header.go`:

```go
package header

import "strings"

// ParseBearer extracts the token from an "Authorization: Bearer <token>" value.
// The scheme is matched case-insensitively; ok is false when the scheme is
// absent or the token is empty.
func ParseBearer(auth string) (string, bool) {
	auth = strings.TrimSpace(auth)
	scheme, rest, found := strings.Cut(auth, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", false
	}
	return token, true
}

// ParseDirectives parses a Cache-Control header into a directive map. Directive
// names are lowercased; valueless directives (no-store) map to "". Quoted values
// are unquoted. Later duplicates win.
func ParseDirectives(cacheControl string) map[string]string {
	out := make(map[string]string)
	for _, item := range strings.Split(cacheControl, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, value, hasValue := strings.Cut(item, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if hasValue {
			value = strings.TrimSpace(value)
			value = strings.Trim(value, `"`)
		}
		out[name] = value
	}
	return out
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/header"
)

func main() {
	if token, ok := header.ParseBearer("Bearer eyJhbGciOi.J9.sig"); ok {
		fmt.Printf("token=%s\n", token)
	}

	d := header.ParseDirectives("public, max-age=600, must-revalidate, no-store")
	fmt.Printf("max-age=%s\n", d["max-age"])
	_, noStore := d["no-store"]
	fmt.Printf("no-store present=%v\n", noStore)
	fmt.Printf("directives=%d\n", len(d))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
token=eyJhbGciOi.J9.sig
max-age=600
no-store present=true
directives=4
```

## Tests

Create `header_test.go`:

```go
package header

import (
	"fmt"
	"reflect"
	"testing"
)

func TestParseBearer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		auth      string
		wantToken string
		wantOK    bool
	}{
		{"standard", "Bearer abc.def.ghi", "abc.def.ghi", true},
		{"lowercase scheme", "bearer abc", "abc", true},
		{"mixed case scheme", "BeArEr abc", "abc", true},
		{"leading space", "  Bearer abc  ", "abc", true},
		{"wrong scheme", "Basic dXNlcjpwYXNz", "", false},
		{"no token", "Bearer ", "", false},
		{"scheme only", "Bearer", "", false},
		{"empty", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			token, ok := ParseBearer(tc.auth)
			if token != tc.wantToken || ok != tc.wantOK {
				t.Fatalf("ParseBearer(%q) = %q,%v; want %q,%v",
					tc.auth, token, ok, tc.wantToken, tc.wantOK)
			}
		})
	}
}

func TestParseDirectives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want map[string]string
	}{
		{
			"public with max-age",
			"public, max-age=600",
			map[string]string{"public": "", "max-age": "600"},
		},
		{
			"no-cache no-store",
			"no-cache, no-store, must-revalidate",
			map[string]string{"no-cache": "", "no-store": "", "must-revalidate": ""},
		},
		{
			"private and quoted max-age",
			`private, max-age="3600"`,
			map[string]string{"private": "", "max-age": "3600"},
		},
		{
			"case-insensitive names",
			"Public, Max-Age=0",
			map[string]string{"public": "", "max-age": "0"},
		},
		{
			"trailing separator and blanks",
			"public, , max-age=60,",
			map[string]string{"public": "", "max-age": "60"},
		},
		{
			"duplicate directive last wins",
			"max-age=100, max-age=200",
			map[string]string{"max-age": "200"},
		},
		{
			"empty header",
			"",
			map[string]string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ParseDirectives(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseDirectives(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func ExampleParseDirectives() {
	d := ParseDirectives("public, max-age=600")
	fmt.Println(d["max-age"])
	// Output: 600
}
```

## Review

The parsers are correct when they never index past the end of a short header and
when the `found`/`ok` bool carries the "was it there?" decision instead of a
manual length check. `ParseBearer` returns `("", false)` for a missing scheme, a
missing token, or a non-bearer scheme, and matches the scheme case-insensitively
because HTTP says the scheme token is case-insensitive. `ParseDirectives` lowers
directive names, unquotes values, tolerates trailing and empty fields, and lets a
later duplicate win — all expressed through `strings.Cut`'s three return values
rather than `SplitN` plus index math. Run `go test -race`; the maps are built
fresh per call so the parallel subtests share nothing.

## Resources

- [strings.Cut (pkg.go.dev)](https://pkg.go.dev/strings#Cut)
- [strings.CutPrefix (pkg.go.dev)](https://pkg.go.dev/strings#CutPrefix)
- [RFC 9110 §11.6.2 Authorization / Bearer](https://www.rfc-editor.org/rfc/rfc9110#field.authorization)
- [RFC 9111 (HTTP Caching) Cache-Control](https://www.rfc-editor.org/rfc/rfc9111#name-cache-control)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-log-line-parser.md](01-log-line-parser.md) | Next: [03-bounded-builder.md](03-bounded-builder.md)
