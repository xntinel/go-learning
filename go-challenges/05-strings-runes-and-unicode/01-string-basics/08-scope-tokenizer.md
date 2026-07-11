# Exercise 8: Tokenize an OAuth scope / tag string on mixed delimiters

Scopes and tag lists arrive in whatever shape the client sent — space-separated,
comma-separated, semicolon-separated, or a messy mix. This module builds the
tokenizer a backend uses on the `scope` form parameter: split on any of several
delimiters with `strings.FieldsFunc`, normalize, and de-duplicate while keeping
first-seen order.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
scopes/                     independent module: example.com/scopes
  go.mod                    go 1.26
  scopes.go                 ParseScopes
  cmd/
    demo/
      main.go               tokenize a mixed-delimiter scope string
  scopes_test.go            mixed-delimiter table + dedup/order assertions
```

Files: `scopes.go`, `cmd/demo/main.go`, `scopes_test.go`.
Implement: `ParseScopes(raw string) []string`.
Test: `read write`, `read,write;admin`, extra whitespace, empty and
all-delimiter input, and duplicates deduped with order preserved.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/scopes/cmd/demo
cd ~/go-exercises/scopes
go mod init example.com/scopes
```

## Why FieldsFunc, not Fields or Split

`strings.Fields` splits only on Unicode whitespace, so it handles `"read write"`
but not `"read,write"`. `strings.Split(raw, ",")` handles the comma but not spaces
or semicolons, and it emits empty strings for adjacent or trailing separators that
you then have to filter. Real input mixes them: a client may send
`"read write,admin;billing"`. The tool for "split on any of a set of delimiters"
is `strings.FieldsFunc(raw, pred)`, where `pred` is a rune predicate returning
true for every character that should act as a separator. Like `Fields`, it drops
empty fields automatically, so runs of delimiters and leading/trailing separators
produce no blank tokens — you get clean tokens with no post-filtering.

After splitting, two normalization steps make the output usable: lowercase each
scope (OAuth scope tokens are conventionally lowercase, and normalizing avoids
`Read` and `read` counting as two distinct grants), and de-duplicate while
preserving the order of first appearance so the result is deterministic and
readable. A `map[string]struct{}` of seen scopes gives O(n) dedup; appending to a
result slice only on first sight preserves order. The returned slice is always
non-nil-safe to range over (an all-delimiter or empty input yields an empty,
possibly nil, slice, which ranges as zero iterations).

Create `scopes.go`:

```go
package scopes

import "strings"

// isDelimiter reports whether r separates scopes: space, tab, newline, comma, or semicolon.
func isDelimiter(r rune) bool {
	switch r {
	case ' ', '\t', '\n', ',', ';':
		return true
	default:
		return false
	}
}

// ParseScopes tokenizes a scope/tag string on mixed delimiters (whitespace,
// comma, semicolon), lowercases each token, and de-duplicates while preserving
// first-seen order.
func ParseScopes(raw string) []string {
	fields := strings.FieldsFunc(raw, isDelimiter)

	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		s := strings.ToLower(f)
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
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

	"example.com/scopes"
)

func main() {
	inputs := []string{
		"read write",
		"read,write;admin",
		"  read   read  WRITE ",
		";,; ,;",
	}
	for _, in := range inputs {
		fmt.Printf("%-20q -> %v\n", in, scopes.ParseScopes(in))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"read write"         -> [read write]
"read,write;admin"   -> [read write admin]
"  read   read  WRITE " -> [read write]
";,; ,;"             -> []
```

## Tests

The table exercises each delimiter, mixed delimiters, surrounding and repeated
whitespace, an all-delimiter input (which must yield an empty result), and
duplicates that must be collapsed with order preserved. One case contrasts
`FieldsFunc` against `Fields`: a comma-joined input that `strings.Fields` would
return as a single un-split token, proving why the custom predicate is needed.

Create `scopes_test.go`:

```go
package scopes

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestParseScopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"space separated", "read write", []string{"read", "write"}},
		{"comma separated", "read,write", []string{"read", "write"}},
		{"mixed delimiters", "read,write;admin billing", []string{"read", "write", "admin", "billing"}},
		{"surrounding whitespace", "  read   write  ", []string{"read", "write"}},
		{"lowercased", "Read WRITE", []string{"read", "write"}},
		{"deduped in order", "write read write admin read", []string{"write", "read", "admin"}},
		{"all delimiters", " ,; , ", []string{}},
		{"empty", "", []string{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ParseScopes(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ParseScopes(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestFieldsWouldBeWrong documents why FieldsFunc is required: strings.Fields
// splits only on whitespace, so a comma-joined input stays a single token.
func TestFieldsWouldBeWrong(t *testing.T) {
	t.Parallel()

	const in = "read,write"
	if plain := strings.Fields(in); len(plain) != 1 {
		t.Fatalf("premise wrong: strings.Fields(%q) = %v", in, plain)
	}
	if got := ParseScopes(in); !slices.Equal(got, []string{"read", "write"}) {
		t.Fatalf("ParseScopes(%q) = %v, want [read write]", in, got)
	}
}

func ExampleParseScopes() {
	fmt.Println(ParseScopes("read,write;read WRITE"))
	// Output: [read write]
}
```

## Review

The tokenizer is correct when it splits on any delimiter in the set, drops empty
fields (so trailing and repeated separators produce no blanks), lowercases each
token, and de-duplicates with first-seen order preserved. The `FieldsFunc`
predicate is the whole reason mixed-delimiter input works; `TestFieldsWouldBeWrong`
records why the simpler `strings.Fields` is insufficient. An all-delimiter or empty
input yields an empty slice, which callers can range over safely. Run
`go test -race`; the function allocates fresh state per call and shares nothing.

## Resources

- [strings.FieldsFunc (pkg.go.dev)](https://pkg.go.dev/strings#FieldsFunc)
- [strings.Fields (pkg.go.dev)](https://pkg.go.dev/strings#Fields)
- [slices.Equal (pkg.go.dev)](https://pkg.go.dev/slices#Equal)
- [RFC 6749 §3.3 OAuth Access Token Scope](https://www.rfc-editor.org/rfc/rfc6749#section-3.3)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-sanitize-valid-utf8.md](07-sanitize-valid-utf8.md) | Next: [09-prefix-router.md](09-prefix-router.md)
