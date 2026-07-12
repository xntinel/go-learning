# Exercise 3: Parse Content-Type Into Media Type and Parameters

Before a handler decodes a request body it must know the media type: only
`application/json` bodies go to the JSON decoder. The `Content-Type` header
carries a media type and optional parameters (`charset`, `boundary`), and both
the type and the parameter names are case-insensitive while values mostly are
not.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
contenttype/                    independent module: example.com/contenttype
  go.mod                        go 1.26
  contenttype.go                ParseContentType -> (mediaType, params, err)
  contenttype_test.go           table test over real header shapes
  cmd/
    demo/
      main.go                   runnable demo
```

Files: `contenttype.go`, `contenttype_test.go`, `cmd/demo/main.go`.
Implement: `ParseContentType(v string) (mediaType string, params map[string]string, err error)`.
Test: bare type, `charset` parameter, quoted and case-varying charset, uppercase
type lowercased, a parameter missing its value (error), duplicate params, and
surrounding spaces.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/05-strings-package/03-content-type-header-parser/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/05-strings-package/03-content-type-header-parser
```

### The parse, step by step

`strings.Cut(v, ";")` splits the media type from the parameter list on the first
semicolon. The media type is lower-cased (`APPLICATION/JSON` and
`application/json` are the same type) after trimming. Then the remaining text is
walked one parameter at a time: `Cut` off the next `;`, `TrimSpace` the piece,
`Cut` it on the first `=` into name and value, lower-case the *name* (parameter
names are case-insensitive), and strip one layer of optional double quotes from
the *value* with `strings.Trim`. A parameter with no `=` is malformed and is a
hard error, not a silently dropped token — a caller that wrote `;charset` without
a value has a bug you want surfaced.

Two things are deliberately case-preserving on the value side. The charset value
`UTF-8` is compared case-insensitively when you *use* it (`strings.EqualFold(v,
"utf-8")`), but it is *stored* verbatim, because a boundary value or a custom
parameter is case-significant. Lower-casing every value would corrupt those. This
is the same discipline as the Bearer token: fold the parts the spec says are
case-insensitive, and store the rest as sent.

Duplicate parameters resolve last-wins, which matches how a `map` assignment
behaves and is the least surprising choice for a permissive parser.

Create `contenttype.go`:

```go
package contenttype

import (
	"fmt"
	"strings"
)

// ErrEmpty means the header had no media type.
var ErrEmpty = fmt.Errorf("content-type: empty media type")

// ErrMalformedParam means a parameter was not in name=value form.
var ErrMalformedParam = fmt.Errorf("content-type: malformed parameter")

// ParseContentType splits a Content-Type value into a lower-cased media type and
// a map of lower-cased parameter names to their (verbatim, unquoted) values.
func ParseContentType(v string) (string, map[string]string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil, ErrEmpty
	}
	typePart, rest, _ := strings.Cut(v, ";")
	mediaType := strings.ToLower(strings.TrimSpace(typePart))
	if mediaType == "" {
		return "", nil, ErrEmpty
	}

	params := map[string]string{}
	for rest != "" {
		var part string
		part, rest, _ = strings.Cut(rest, ";")
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, found := strings.Cut(part, "=")
		if !found {
			return "", nil, fmt.Errorf("parameter %q: %w", part, ErrMalformedParam)
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			return "", nil, fmt.Errorf("empty parameter name in %q: %w", part, ErrMalformedParam)
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"`)
		params[name] = value
	}
	return mediaType, params, nil
}

// IsJSON reports whether the parsed media type is application/json.
func IsJSON(mediaType string) bool {
	return strings.EqualFold(mediaType, "application/json")
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/contenttype"
)

func main() {
	headers := []string{
		"application/json",
		"application/json; charset=utf-8",
		`text/plain;charset="UTF-8"`,
		"APPLICATION/JSON",
	}
	for _, h := range headers {
		mt, params, err := contenttype.ParseContentType(h)
		if err != nil {
			fmt.Printf("%-34q -> error: %v\n", h, err)
			continue
		}
		fmt.Printf("%-34q -> type=%s json=%v charset=%q\n",
			h, mt, contenttype.IsJSON(mt), params["charset"])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"application/json"                 -> type=application/json json=true charset=""
"application/json; charset=utf-8"  -> type=application/json json=true charset="utf-8"
"text/plain;charset=\"UTF-8\""     -> type=text/plain json=false charset="UTF-8"
"APPLICATION/JSON"                 -> type=application/json json=true charset=""
```

### Tests

Create `contenttype_test.go`:

```go
package contenttype

import (
	"errors"
	"fmt"
	"maps"
	"testing"
)

func TestParseContentType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         string
		wantType   string
		wantParams map[string]string
		wantErr    error
	}{
		{
			name:       "bare type",
			in:         "application/json",
			wantType:   "application/json",
			wantParams: map[string]string{},
		},
		{
			name:       "charset param",
			in:         "application/json; charset=utf-8",
			wantType:   "application/json",
			wantParams: map[string]string{"charset": "utf-8"},
		},
		{
			name:       "quoted charset",
			in:         `text/plain;charset="UTF-8"`,
			wantType:   "text/plain",
			wantParams: map[string]string{"charset": "UTF-8"},
		},
		{
			name:       "uppercase type lowered",
			in:         "APPLICATION/JSON",
			wantType:   "application/json",
			wantParams: map[string]string{},
		},
		{
			name:       "duplicate last wins",
			in:         "text/plain; charset=ascii; charset=utf-8",
			wantType:   "text/plain",
			wantParams: map[string]string{"charset": "utf-8"},
		},
		{
			name:       "surrounding spaces",
			in:         "  application/json ;  charset = utf-8 ",
			wantType:   "application/json",
			wantParams: map[string]string{"charset": "utf-8"},
		},
		{name: "missing value", in: "text/plain; charset", wantErr: ErrMalformedParam},
		{name: "empty", in: "   ", wantErr: ErrEmpty},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mt, params, err := ParseContentType(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseContentType(%q) err = %v, want %v", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseContentType(%q) unexpected err: %v", tc.in, err)
			}
			if mt != tc.wantType {
				t.Fatalf("mediaType = %q, want %q", mt, tc.wantType)
			}
			if !maps.Equal(params, tc.wantParams) {
				t.Fatalf("params = %v, want %v", params, tc.wantParams)
			}
		})
	}
}

func ExampleParseContentType() {
	mt, params, _ := ParseContentType("application/json; charset=utf-8")
	fmt.Println(mt, params["charset"], IsJSON(mt))
	// Output: application/json utf-8 true
}
```

## Review

The parser is correct when the media type and parameter names are folded to lower
case but parameter values are stored verbatim (only unquoted), and when a
parameter with no `=` is a hard error rather than a dropped token. The design
mirror to keep in mind: like the Bearer scheme, only the case-insensitive parts
are normalized; the value bytes are preserved. `maps.Equal` (Go 1.21) gives the
map comparison for free. Confirm with `go test -race`; the `mime.ParseMediaType`
standard function solves the fuller RFC 2045 grammar in production, but building
this by hand is how you learn where `Cut` and `Trim` fit.

## Resources

- [strings.Cut](https://pkg.go.dev/strings#Cut) and [strings.Trim](https://pkg.go.dev/strings#Trim).
- [mime.ParseMediaType](https://pkg.go.dev/mime#ParseMediaType) — the production-grade parser for the same header.
- [RFC 9110 media-type](https://www.rfc-editor.org/rfc/rfc9110#name-media-type) — type and parameter grammar.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-bearer-token-header-parser.md](02-bearer-token-header-parser.md) | Next: [04-logfmt-line-parser.md](04-logfmt-line-parser.md)
