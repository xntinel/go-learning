# Exercise 2: Streaming Redactor at the jsontext Token Layer

Redacting secrets out of a log or audit payload is a job you never want to do by
decoding into a Go value and re-encoding: you lose field order, you lose unknown
fields, and you buy the whole document into memory. This exercise builds a
path-based redactor that works purely at the `jsontext` token layer — it copies
JSON from a reader to a writer token by token, replacing the value at each
configured JSON pointer and dropping members entirely at others, all in bounded
memory and preserving member order.

The files carry the `//go:build goexperiment.jsonv2` constraint;
`encoding/json/jsontext` exists only under `GOEXPERIMENT=jsonv2`.

## What you'll build

```text
jsonredact/                   independent module: example.com/jsonredact
  go.mod                      go 1.26
  redact.go                   Rules; Redact + recursive transform over the token stream
  cmd/
    demo/
      main.go                 runnable demo redacting a credential document
  redact_test.go              golden output tests, deep-nesting, duplicate-name behavior
```

Files: `redact.go`, `cmd/demo/main.go`, `redact_test.go`.
Implement: `Redact(w, r, rules, opts...)` walking the token stream, using `dec.StackPointer()` to identify each object member, `dec.ReadValue()` to discard redacted/dropped subtrees, and a `jsontext.Encoder` with `WithIndent` to emit stable output.
Test: golden documents for redact, drop, nested, and deeply nested pointers; that duplicate object member names are rejected by default (unwrapping to `jsontext.ErrDuplicateName`) and accepted when `jsontext.AllowDuplicateNames(true)` is passed.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/48-modern-go-language-and-stdlib/12-encoding-json-v2/02-jsontext-token-rewriting/cmd/demo
cd go-solutions/48-modern-go-language-and-stdlib/12-encoding-json-v2/02-jsontext-token-rewriting
go mod edit -go=1.26
export GOEXPERIMENT=jsonv2
```

### Walking the token stream, tracking the pointer

The redactor is a recursive descent over tokens. A `jsontext.Decoder` exposes the
document as a stream: `dec.PeekKind()` tells you what the next token is without
consuming it, `dec.ReadToken()` consumes one token, and `dec.ReadValue()` consumes
one *complete* value — a scalar, or an entire object/array subtree — as raw bytes.
A `jsontext.Encoder` mirrors this with `WriteToken` and `WriteValue`. Configured
with `jsontext.WithIndent("  ")`, the encoder emits stable two-space-indented
output (a space after each colon, each element on its own line), which makes golden
comparisons in the tests exact.

The pointer machinery is what makes path-based redaction possible without threading
state by hand. As the decoder reads, it maintains an internal stack of the object
names and array indices it has descended through. `dec.StackPointer()` renders that
stack as an RFC 6901 JSON pointer. The load-bearing detail: **after `ReadToken`
returns a member's name token, `dec.StackPointer()` already reports that member's
pointer** — the same pointer the member's value occupies — because the decoder has
entered the member. (This is the same mechanism that lets `RejectUnknownMembers`
report the offending member as `/extra` before its value is read.) So the algorithm
is: in an object, read the name, ask for the pointer, decide from the rules, then
either drop the value, replace it, or recurse.

`transform` copies exactly one value. If it is an object, `transformObject` copies
the braces and handles each member by pointer; if it is an array, `transformArray`
copies the brackets and recurses per element (so a member deep inside arrays, like
`/items/3/secret`, is still matched because the decoder tracks the array index for
us). Any scalar is copied verbatim with `ReadValue`/`WriteValue`.

The token layer is also where RFC 7493 is enforced. By default the decoder rejects
duplicate object member names: a payload where an upstream proxy and your service
would disagree about which `role` wins is refused outright, and the error unwraps
to `jsontext.ErrDuplicateName`. That default is a security property, so re-enabling
it is a deliberate per-call choice: pass `jsontext.AllowDuplicateNames(true)` and
the redactor accepts (and faithfully re-emits) both members. Because the option is
handed to both the decoder and the encoder, the output — which would otherwise also
be rejected as duplicate on write — is allowed through too.

Create `redact.go`:

```go
//go:build goexperiment.jsonv2

package jsonredact

import (
	"encoding/json/jsontext"
	"io"
)

// Rules configures the streaming redactor. Redact replaces the value at each
// listed JSON pointer (RFC 6901) with Placeholder; Drop removes the member
// entirely. Pointers are matched exactly and address object members.
type Rules struct {
	Redact      map[jsontext.Pointer]bool
	Drop        map[jsontext.Pointer]bool
	Placeholder string
}

// Redact copies JSON from r to w at the token layer, applying rules, without
// materializing the whole document. opts tune both the decoder and encoder
// (e.g. jsontext.AllowDuplicateNames(true)); output is indented two spaces.
func Redact(w io.Writer, r io.Reader, rules Rules, opts ...jsontext.Options) error {
	dec := jsontext.NewDecoder(r, opts...)
	encOpts := append([]jsontext.Options{jsontext.WithIndent("  ")}, opts...)
	enc := jsontext.NewEncoder(w, encOpts...)
	return transform(enc, dec, rules)
}

// transform copies exactly one JSON value from dec to enc.
func transform(enc *jsontext.Encoder, dec *jsontext.Decoder, rules Rules) error {
	switch dec.PeekKind() {
	case jsontext.KindBeginObject:
		return transformObject(enc, dec, rules)
	case jsontext.KindBeginArray:
		return transformArray(enc, dec, rules)
	default:
		val, err := dec.ReadValue() // a scalar: copy it verbatim
		if err != nil {
			return err
		}
		return enc.WriteValue(val)
	}
}

func transformObject(enc *jsontext.Encoder, dec *jsontext.Decoder, rules Rules) error {
	if _, err := dec.ReadToken(); err != nil { // consume '{'
		return err
	}
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	for dec.PeekKind() != jsontext.KindEndObject {
		name, err := dec.ReadToken() // member name (a string token)
		if err != nil {
			return err
		}
		ptr := dec.StackPointer() // pointer to this member, e.g. /card/number
		switch {
		case rules.Drop[ptr]:
			// Discard the value and write nothing: the member disappears.
			if _, err := dec.ReadValue(); err != nil {
				return err
			}
		case rules.Redact[ptr]:
			// Emit the name (before ReadValue invalidates the token), discard
			// the real value, and emit the placeholder in its place.
			if err := enc.WriteToken(name); err != nil {
				return err
			}
			if _, err := dec.ReadValue(); err != nil {
				return err
			}
			if err := enc.WriteToken(jsontext.String(rules.Placeholder)); err != nil {
				return err
			}
		default:
			if err := enc.WriteToken(name); err != nil {
				return err
			}
			if err := transform(enc, dec, rules); err != nil {
				return err
			}
		}
	}
	if _, err := dec.ReadToken(); err != nil { // consume '}'
		return err
	}
	return enc.WriteToken(jsontext.EndObject)
}

func transformArray(enc *jsontext.Encoder, dec *jsontext.Decoder, rules Rules) error {
	if _, err := dec.ReadToken(); err != nil { // consume '['
		return err
	}
	if err := enc.WriteToken(jsontext.BeginArray); err != nil {
		return err
	}
	for dec.PeekKind() != jsontext.KindEndArray {
		if err := transform(enc, dec, rules); err != nil {
			return err
		}
	}
	if _, err := dec.ReadToken(); err != nil { // consume ']'
		return err
	}
	return enc.WriteToken(jsontext.EndArray)
}
```

### The runnable demo

The demo redacts a credential-shaped document: it masks `/password` and the nested
`/card/number`, drops the `/debug` subtree, and leaves everything else — including
the `roles` array and member order — untouched. The multiline encoder terminates
the top-level value with a newline, so the buffered output already ends in `\n` and
the demo prints it with `fmt.Print`.

Create `cmd/demo/main.go`:

```go
//go:build goexperiment.jsonv2

package main

import (
	"bytes"
	"encoding/json/jsontext"
	"fmt"
	"strings"

	"example.com/jsonredact"
)

func main() {
	const input = `{"user":"alice","password":"s3cr3t",` +
		`"card":{"number":"4111111111111111","exp":"12/29"},` +
		`"debug":{"trace":"x"},"roles":["admin","ops"]}`

	rules := jsonredact.Rules{
		Redact: map[jsontext.Pointer]bool{
			"/password":    true,
			"/card/number": true,
		},
		Drop:        map[jsontext.Pointer]bool{"/debug": true},
		Placeholder: "REDACTED",
	}

	var buf bytes.Buffer
	if err := jsonredact.Redact(&buf, strings.NewReader(input), rules); err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Print(buf.String())
}
```

Run it:

```bash
GOEXPERIMENT=jsonv2 go run ./cmd/demo
```

Expected output:

```
{
  "user": "alice",
  "password": "REDACTED",
  "card": {
    "number": "REDACTED",
    "exp": "12/29"
  },
  "roles": [
    "admin",
    "ops"
  ]
}
```

### Tests

The tests are golden comparisons against exact indented documents, which is only
reliable because `WithIndent` fixes the formatting. `TestRedact` covers a flat
redact-and-drop, a nested pointer, and an untouched array. `TestDeepNesting`
redacts `/a/b/c/secret` to exercise `StackPointer` several frames down.
`TestDuplicateNamesRejected` proves the RFC 7493 default by asserting the error
unwraps to `jsontext.ErrDuplicateName` with `errors.Is`, and
`TestDuplicateNamesAllowed` shows the deliberate opt-out via
`jsontext.AllowDuplicateNames(true)`.

Create `redact_test.go`:

```go
//go:build goexperiment.jsonv2

package jsonredact

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		rules Rules
		want  string
	}{
		{
			name:  "redact and drop",
			input: `{"user":"alice","password":"s3cr3t","debug":{"x":1}}`,
			rules: Rules{
				Redact:      map[jsontext.Pointer]bool{"/password": true},
				Drop:        map[jsontext.Pointer]bool{"/debug": true},
				Placeholder: "REDACTED",
			},
			want: "{\n  \"user\": \"alice\",\n  \"password\": \"REDACTED\"\n}\n",
		},
		{
			name:  "nested pointer",
			input: `{"card":{"number":"4111","exp":"12/29"},"amount":100}`,
			rules: Rules{
				Redact:      map[jsontext.Pointer]bool{"/card/number": true},
				Placeholder: "X",
			},
			want: "{\n  \"card\": {\n    \"number\": \"X\",\n    \"exp\": \"12/29\"\n  },\n  \"amount\": 100\n}\n",
		},
		{
			name:  "array preserved",
			input: `{"roles":["admin","ops"]}`,
			rules: Rules{Placeholder: "X"},
			want:  "{\n  \"roles\": [\n    \"admin\",\n    \"ops\"\n  ]\n}\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := Redact(&buf, strings.NewReader(tt.input), tt.rules); err != nil {
				t.Fatalf("Redact: %v", err)
			}
			if got := buf.String(); got != tt.want {
				t.Fatalf("output mismatch\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestDeepNesting(t *testing.T) {
	t.Parallel()
	const input = `{"a":{"b":{"c":{"secret":"top","keep":1}}}}`
	rules := Rules{
		Redact:      map[jsontext.Pointer]bool{"/a/b/c/secret": true},
		Placeholder: "X",
	}
	want := "{\n  \"a\": {\n    \"b\": {\n      \"c\": {\n" +
		"        \"secret\": \"X\",\n        \"keep\": 1\n" +
		"      }\n    }\n  }\n}\n"
	var buf bytes.Buffer
	if err := Redact(&buf, strings.NewReader(input), rules); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if got := buf.String(); got != want {
		t.Fatalf("output mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestDuplicateNamesRejected(t *testing.T) {
	t.Parallel()
	const input = `{"name":"a","name":"b"}`
	var buf bytes.Buffer
	err := Redact(&buf, strings.NewReader(input), Rules{Placeholder: "X"})
	if !errors.Is(err, jsontext.ErrDuplicateName) {
		t.Fatalf("err = %v, want jsontext.ErrDuplicateName", err)
	}
}

func TestDuplicateNamesAllowed(t *testing.T) {
	t.Parallel()
	const input = `{"name":"a","name":"b"}`
	var buf bytes.Buffer
	err := Redact(&buf, strings.NewReader(input), Rules{Placeholder: "X"},
		jsontext.AllowDuplicateNames(true))
	if err != nil {
		t.Fatalf("Redact with AllowDuplicateNames: %v", err)
	}
	want := "{\n  \"name\": \"a\",\n  \"name\": \"b\"\n}\n"
	if got := buf.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func ExampleRedact() {
	const input = `{"user":"bob","token":"abc123"}`
	rules := Rules{
		Redact:      map[jsontext.Pointer]bool{"/token": true},
		Placeholder: "REDACTED",
	}
	var buf bytes.Buffer
	_ = Redact(&buf, strings.NewReader(input), rules)
	fmt.Println(buf.String())
	// Output:
	// {
	//   "user": "bob",
	//   "token": "REDACTED"
	// }
}
```

## Review

The redactor is correct when the output is byte-for-byte the input with exactly
the configured members masked or dropped and everything else — member order,
untargeted fields, array contents — preserved. The golden tests are the proof, and
they are exact only because `WithIndent` pins the formatting; if you compared
against compact JSON the whitespace defaults would bite you. The deep-nesting test
confirms `StackPointer` resolves `/a/b/c/secret` correctly several frames down.

The mistakes cluster around token lifetime and the name-versus-value distinction.
A `jsontext.Token` returned by `ReadToken` is valid only until the next read, so in
the redact branch you must `WriteToken(name)` *before* calling `ReadValue`, which
is why the code writes the name first and the placeholder after. Do not treat every
string token whose pointer matches as a target — object *names* are string tokens
too, so matching happens on the member pointer obtained right after reading the
name, not on arbitrary string values. Do not reach for `AllowDuplicateNames`
globally to make a stubborn input parse; scope it to the one boundary that must
accept duplicates, since the default rejection is a security property. Confirm the
duplicate behavior with `errors.Is(err, jsontext.ErrDuplicateName)` and run
`go test -race`.

## Resources

- [`encoding/json/jsontext`](https://pkg.go.dev/encoding/json/jsontext) — `Decoder`, `Encoder`, `StackPointer`, `WithIndent`, `AllowDuplicateNames`, and `ErrDuplicateName`.
- [`jsontext.Pointer`](https://pkg.go.dev/encoding/json/jsontext#Pointer) — the RFC 6901 pointer type, `Contains`, and `LastToken`.
- [A new experimental Go API for JSON](https://go.dev/blog/jsonv2-exp) — why the token layer is separate from the semantic layer.

---

Prev: [01-streaming-decode-large-arrays.md](01-streaming-decode-large-arrays.md) | Back to [00-concepts.md](00-concepts.md) | Next: [03-v2-semantics-and-compat-shim.md](03-v2-semantics-and-compat-shim.md)
