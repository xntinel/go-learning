# Exercise 1: Classify a Content-Type Header Into a Closed Kind Set

Every service that accepts a request body has to answer one question before it
can decode anything: what *kind* of body is this? This module builds the
classifier that answers it — parsing the `Content-Type` header the correct way
and folding the messy real-world header space into a small closed `Kind` enum
using a tagless switch, with the `Kind.String()` stringer implemented as an
expression switch.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
classify/                  independent module: example.com/content-type-classifier
  go.mod                   go 1.24
  classify.go              type Kind + String(); Classify(contentType) (Kind, error)
  cmd/
    demo/
      main.go              runnable demo classifying representative headers
  classify_test.go         table-driven classification + parameter-insensitivity tests
```

- Files: `classify.go`, `cmd/demo/main.go`, `classify_test.go`.
- Implement: `Classify(contentType string) (Kind, error)` that parses with `mime.ParseMediaType` and dispatches with a tagless switch, plus `Kind.String()` as an expression switch.
- Test: a table over json / json+charset / vendor `+json` / form / multipart+boundary / empty / unsupported / malformed inputs, and a dedicated test proving parameters do not change the Kind.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why parse the header instead of comparing it

The single most common bug in a hand-rolled classifier is `contentType ==
"application/json"`. It works in every quick test and fails in production the
first time a compliant client sends `application/json; charset=utf-8`, or a
form upload arrives as `multipart/form-data; boundary=------abc`. A media type is
a structured value — a type/subtype plus optional parameters — and the only
correct way to read it is `mime.ParseMediaType`, which returns the bare media
type (lowercased, parameters stripped) separately from the parameter map. We
switch on the bare media type; the parameters never touch the decision.

The decision itself is a tagless switch because it is *predicate* dispatch, not
value dispatch: `application/json` is one exact match, but the entire family of
vendor JSON types (`application/vnd.api+json`, `application/ld+json`, ...) is a
`+json` suffix test, which `==` cannot express. The first matching case wins;
the `default` fails closed with a wrapped `ErrUnknownKind` so an unsupported type
like `text/xml` surfaces loudly instead of being mistaken for something else.

`Kind.String()`, by contrast, *is* value dispatch over a closed enum, so it is a
plain expression switch. That contrast — tagless for the classify predicate,
expression for the stringer — is the whole point of the pair.

Create `classify.go`:

```go
package classify

import (
	"errors"
	"fmt"
	"mime"
	"strings"
)

// Sentinel errors callers assert with errors.Is.
var (
	ErrEmptyContentType = errors.New("empty Content-Type")
	ErrUnknownKind      = errors.New("unknown content kind")
)

// Kind is the closed set of request-body kinds this service understands.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindJSON
	KindForm
	KindMultipart
)

// String is value dispatch over a closed enum: an expression switch.
func (k Kind) String() string {
	switch k {
	case KindJSON:
		return "json"
	case KindForm:
		return "form"
	case KindMultipart:
		return "multipart"
	default:
		return "unknown"
	}
}

// Classify parses a raw Content-Type header and folds it into a Kind. It is a
// tagless switch over the parsed media type, so header parameters (charset,
// boundary) never change the result.
func Classify(contentType string) (Kind, error) {
	if strings.TrimSpace(contentType) == "" {
		return KindUnknown, ErrEmptyContentType
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return KindUnknown, fmt.Errorf("parse content type %q: %w", contentType, err)
	}
	switch {
	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		return KindJSON, nil
	case mediaType == "application/x-www-form-urlencoded":
		return KindForm, nil
	case mediaType == "multipart/form-data":
		return KindMultipart, nil
	default:
		return KindUnknown, fmt.Errorf("%w: %s", ErrUnknownKind, mediaType)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/content-type-classifier"
)

func main() {
	headers := []string{
		"application/json",
		"application/json; charset=utf-8",
		"application/vnd.api+json",
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=----x",
		"text/xml",
	}
	for _, h := range headers {
		kind, err := classify.Classify(h)
		if err != nil {
			fmt.Printf("%-40s -> error: %v\n", h, err)
			continue
		}
		fmt.Printf("%-40s -> %s\n", h, kind)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
application/json                         -> json
application/json; charset=utf-8          -> json
application/vnd.api+json                 -> json
application/x-www-form-urlencoded        -> form
multipart/form-data; boundary=----x      -> multipart
text/xml                                 -> error: unknown content kind: text/xml
```

### Tests

`TestClassify` walks representative headers, including the empty case
(`ErrEmptyContentType`), an unsupported type (`ErrUnknownKind`), and a malformed
media type (a non-nil parse error). `TestClassifyIgnoresParameters` is the proof
that matters: charset and boundary parameters, and mixed-case type names, all
collapse to the same Kind, because the switch runs on the parsed media type.

Create `classify_test.go`:

```go
package classify

import (
	"errors"
	"testing"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		contentType  string
		wantKind     Kind
		wantErr      error
		wantParseErr bool
	}{
		{name: "json", contentType: "application/json", wantKind: KindJSON},
		{name: "json with charset", contentType: "application/json; charset=utf-8", wantKind: KindJSON},
		{name: "vendor json", contentType: "application/vnd.api+json", wantKind: KindJSON},
		{name: "form", contentType: "application/x-www-form-urlencoded", wantKind: KindForm},
		{name: "multipart with boundary", contentType: "multipart/form-data; boundary=----xyz", wantKind: KindMultipart},
		{name: "empty", contentType: "", wantErr: ErrEmptyContentType},
		{name: "blank whitespace", contentType: "   ", wantErr: ErrEmptyContentType},
		{name: "unsupported xml", contentType: "text/xml", wantErr: ErrUnknownKind},
		{name: "malformed media type", contentType: "application/json; charset", wantParseErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Classify(tc.contentType)
			switch {
			case tc.wantParseErr:
				if err == nil {
					t.Fatalf("Classify(%q) err = nil, want a parse error", tc.contentType)
				}
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Classify(%q) err = %v, want errors.Is %v", tc.contentType, err, tc.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("Classify(%q) err = %v, want nil", tc.contentType, err)
				}
				if got != tc.wantKind {
					t.Fatalf("Classify(%q) = %v, want %v", tc.contentType, got, tc.wantKind)
				}
			}
		})
	}
}

func TestClassifyIgnoresParameters(t *testing.T) {
	t.Parallel()

	variants := []string{
		"application/json",
		"application/json; charset=utf-8",
		"application/json; charset=UTF-8; boundary=x",
		"Application/JSON; Charset=utf-8",
	}
	for _, v := range variants {
		got, err := Classify(v)
		if err != nil {
			t.Fatalf("Classify(%q) err = %v, want nil", v, err)
		}
		if got != KindJSON {
			t.Fatalf("Classify(%q) = %v, want KindJSON; parameters must not change the Kind", v, got)
		}
	}
}
```

The `Example` functions live in their own file so they can import `fmt` without
touching the table test's imports:

Create `classify_example_test.go`:

```go
package classify

import "fmt"

func ExampleClassify() {
	kind, _ := Classify("application/json; charset=utf-8")
	fmt.Println(kind)
	// Output: json
}

func ExampleKind_String() {
	fmt.Println(KindJSON, KindForm, KindMultipart, KindUnknown)
	// Output: json form multipart unknown
}
```

## Review

The classifier is correct when the Kind is a pure function of the *parsed* media
type and nothing else: `TestClassifyIgnoresParameters` is what proves a charset
or boundary parameter, or a capitalized type name, cannot move the result,
because `mime.ParseMediaType` strips and lowercases before the switch ever runs.
The two traps this exercise inoculates against are comparing the raw header with
`==` (broken by the first `; charset=`) and forgetting the `default`, which here
returns a wrapped `ErrUnknownKind` so an unsupported type fails loudly rather than
being silently classified as `KindUnknown`-shaped success. Note the two switch
forms doing different jobs: tagless for the `+json` suffix predicate in
`Classify`, expression for the closed-enum `Kind.String()`.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — expression, tagless, and init forms.
- [mime.ParseMediaType](https://pkg.go.dev/mime#ParseMediaType) — parses a media type and its parameters; the correct way to read a Content-Type.
- [RFC 9110: Media types](https://www.rfc-editor.org/rfc/rfc9110#name-media-type) — why a Content-Type carries parameters and why `==` is wrong.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-kind-dispatcher.md](02-kind-dispatcher.md)
