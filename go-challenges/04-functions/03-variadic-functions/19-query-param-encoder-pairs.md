# Exercise 19: Query Parameter String Encoder from Variadic Pairs

**Nivel: Intermedio** — validacion rapida (un test corto).

Building a request URL by hand often looks like `"?q=" + escape(q) + "&page="
+ ...`, which is easy to get subtly wrong (forgetting to escape one value, or
getting the `&` placement off by one). An alternating variadic list — key,
value, key, value — reads at the call site almost like a literal query
string, and a single small function turns it into a correctly escaped one,
in the exact order the caller wrote it.

## What you'll build

```text
qparams/                   independent module: example.com/qparams
  go.mod                   go 1.24
  qparams.go               package qparams; func Encode(pairs ...string) (string, error)
  cmd/
    demo/
      main.go              runnable demo: encode a query, then trigger the odd-count error
  qparams_test.go          table tests: order preserved, escaping, zero pairs, odd count
```

- Files: `qparams.go`, `cmd/demo/main.go`, `qparams_test.go`.
- Implement: `Encode(pairs ...string) (string, error)`, treating `pairs` as alternating key, value, key, value, ..., percent-encoding each piece with `net/url.QueryEscape`, and erroring on an odd argument count.
- Test: three key/value pairs encode in argument order (not alphabetically sorted); a value containing `&` and `=` is escaped; zero pairs return an empty string with no error; an odd number of arguments is an error.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why order-preserving, and why the odd-count check comes first

`net/url.Values.Encode` exists already, but it sorts by key — every call
allocates a `url.Values` map, then a key slice, then sorts that slice. On a
hot request-construction path where the caller already knows the order it
wants query parameters in (search term first, then pagination, then sort),
that sort is wasted work and a wasted allocation. `Encode(pairs ...string)`
instead streams straight through the variadic slice two elements at a time,
writing `key=value` pairs joined by `&` in exactly the order given — no map,
no sort.

The odd-count check runs before anything is written to the builder, on
purpose: `pairs` is really a sequence of pairs, not just any slice of
strings, and a caller who passes `"q", "go", "page"` has a bug — a key with
no matching value — that should surface immediately as an error rather than
silently dropping the trailing key or panicking on an out-of-range index
inside the loop.

Create `qparams.go`:

```go
// qparams.go
package qparams

import (
	"fmt"
	"net/url"
	"strings"
)

// Encode builds a URL query string from an alternating key, value, key,
// value, ... variadic list, percent-encoding each piece with
// url.QueryEscape. Unlike url.Values.Encode, which sorts by key, Encode
// preserves argument order: on a hot request path the caller already knows
// the order it wants (and sorting means allocating and sorting a key slice
// on every call), so Encode just streams pairs through in the order given.
func Encode(pairs ...string) (string, error) {
	if len(pairs)%2 != 0 {
		return "", fmt.Errorf("qparams: odd number of arguments (%d), want alternating key/value pairs", len(pairs))
	}

	var b strings.Builder
	for i := 0; i < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(pairs[i]))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(pairs[i+1]))
	}
	return b.String(), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/qparams"
)

func main() {
	q, err := qparams.Encode("q", "go generics", "page", "2", "sort", "name")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(q)

	_, err = qparams.Encode("q")
	fmt.Println("error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
q=go+generics&page=2&sort=name
error: qparams: odd number of arguments (1), want alternating key/value pairs
```

### Tests

`TestEncode`'s first case is the one that distinguishes this from
`url.Values.Encode`: three pairs given in a deliberately non-alphabetical
order (`q`, `page`, `sort`) must come back out in that same order, proving
`Encode` never sorts.

Create `qparams_test.go`:

```go
// qparams_test.go
package qparams

import "testing"

func TestEncode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pairs   []string
		want    string
		wantErr bool
	}{
		{
			name:  "preserves argument order, not sorted",
			pairs: []string{"q", "go generics", "page", "2", "sort", "name"},
			want:  "q=go+generics&page=2&sort=name",
		},
		{
			name:  "escapes reserved characters",
			pairs: []string{"filter", "a&b=c"},
			want:  "filter=a%26b%3Dc",
		},
		{
			name:  "zero pairs returns empty string",
			pairs: nil,
			want:  "",
		},
		{
			name:    "odd argument count is an error",
			pairs:   []string{"q"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Encode(tc.pairs...)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Encode(%v) error = nil, want an error", tc.pairs)
				}
				return
			}
			if err != nil {
				t.Fatalf("Encode(%v) unexpected error: %v", tc.pairs, err)
			}
			if got != tc.want {
				t.Fatalf("Encode(%v) = %q, want %q", tc.pairs, got, tc.want)
			}
		})
	}
}
```

## Review

`Encode` is correct when the output preserves argument order (never
alphabetized), every key and value is percent-escaped through
`url.QueryEscape` so reserved characters like `&` and `=` inside a value
cannot be mistaken for query-string structure, zero pairs yields an empty
string rather than an error, and an odd argument count is rejected before any
output is built. The senior point is recognizing when the standard library's
general-purpose tool (`url.Values.Encode`, which sorts and allocates a map)
is the wrong fit for a hot path with a known, meaningful order — a plain
variadic loop over pairs is both faster and more correct for this specific
contract. The mistake to avoid is skipping `url.QueryEscape` on the key: a
key can contain `&` or `=` just as easily as a value can, and an unescaped
key silently corrupts every parameter that follows it in the string.

## Resources

- [`net/url.QueryEscape`](https://pkg.go.dev/net/url#QueryEscape) — percent-encoding for use in a URL query string.
- [`net/url.Values.Encode`](https://pkg.go.dev/net/url#Values.Encode) — the standard library's sorted alternative, useful to contrast with here.
- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-bulk-insert-args-placeholder-builder.md](18-bulk-insert-args-placeholder-builder.md) | Next: [20-where-predicate-combinator-and-or.md](20-where-predicate-combinator-and-or.md)
