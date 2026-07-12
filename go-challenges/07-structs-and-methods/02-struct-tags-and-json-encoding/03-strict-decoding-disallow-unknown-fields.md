# Exercise 3: Reject Unknown Fields on an Inbound Request (Mass-Assignment Guard)

The default JSON decoder silently ignores fields it does not recognize. On an
inbound request that is a security hole: a client can smuggle `"is_admin":true`
or `"account_id":"other"` into a body your struct never declared, and if any
layer later trusts those raw bytes, you have a mass-assignment bug. This module
builds a reusable `DecodeStrict[T]` that rejects unknown fields and trailing
garbage, the guard every write handler should route its body through.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
strictdecode/                  independent module: example.com/strictdecode
  go.mod                       go 1.24
  httpjson/
    decode.go                  DecodeStrict[T](io.Reader) (T, error); ErrTrailingData
  cmd/
    demo/
      main.go                  decode a good body, an unknown-field body, a doubled body
  httpjson/decode_test.go      unknown-field, valid, trailing-data, truncated cases
```

Files: `httpjson/decode.go`, `cmd/demo/main.go`, `httpjson/decode_test.go`.
Implement: `DecodeStrict[T any](r io.Reader) (T, error)` using `Decoder.DisallowUnknownFields` plus a trailing-data check via a second `Decode` that must return `io.EOF`.
Test: extra key errors mentioning the unknown field; valid body decodes; two concatenated objects hit `ErrTrailingData`; truncated JSON errors.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

## Two checks, not one

Strict inbound decoding is two separate guards, and skipping either leaves a hole.

The first is `Decoder.DisallowUnknownFields()`. With it set, a key in the input
that maps to no exported field of the target type is a decode error rather than a
silent no-op. That is the mass-assignment guard: the client cannot mention a field
you did not declare. Note this is a decoder-level setting, so you must use
`json.NewDecoder(r)` and configure it — plain `json.Unmarshal` has no equivalent
knob.

The second is a trailing-data check. A `Decoder` reads one JSON value and stops at
its end; it does not care what follows. So a body like `{"name":"a"}{"name":"b"}`
or `{"name":"a"} garbage` decodes the first object and returns success, leaving the
rest unread. To reject that, decode a second time into a throwaway
`json.RawMessage` and require the result to be `io.EOF` — meaning the stream held
exactly one value and nothing else. If the second `Decode` returns `nil`, there
was a second value (doubled object); if it returns any other error, there was
malformed trailing content. Both are rejected with a sentinel `ErrTrailingData`
wrapped with `%w`, so a caller can classify it with `errors.Is`.

Decoding the trailing check into `json.RawMessage` rather than into `T` again is
deliberate: `RawMessage` accepts any well-formed value without triggering the
unknown-field machinery, so the check reports "there is more data" cleanly instead
of failing for the wrong reason.

Create `httpjson/decode.go`:

```go
// httpjson/decode.go
package httpjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrTrailingData is returned when the reader holds more than one JSON value.
var ErrTrailingData = errors.New("unexpected trailing data after JSON value")

// DecodeStrict decodes exactly one JSON value from r into a T, rejecting any
// field not declared on T (mass-assignment guard) and any trailing bytes after
// the value. It is the decoder every write handler should route its body
// through.
func DecodeStrict[T any](r io.Reader) (T, error) {
	var v T
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	if err := dec.Decode(&v); err != nil {
		return v, fmt.Errorf("decode request: %w", err)
	}

	// Require exactly one value: a second decode must hit EOF.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return v, fmt.Errorf("%w: more than one JSON value", ErrTrailingData)
		}
		return v, fmt.Errorf("%w: %v", ErrTrailingData, err)
	}
	return v, nil
}
```

## The runnable demo

The demo decodes a valid create-user body, a body carrying an undeclared
`is_admin` field, and a doubled body, printing what each attempt yields.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"strings"

	"example.com/strictdecode/httpjson"
)

type CreateUser struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func main() {
	good, err := httpjson.DecodeStrict[CreateUser](strings.NewReader(`{"name":"Alice","email":"a@example.com"}`))
	fmt.Printf("valid:   %+v err=%v\n", good, err)

	_, err = httpjson.DecodeStrict[CreateUser](strings.NewReader(`{"name":"Mallory","is_admin":true}`))
	fmt.Printf("unknown: err=%v\n", err)

	_, err = httpjson.DecodeStrict[CreateUser](strings.NewReader(`{"name":"A"}{"name":"B"}`))
	fmt.Printf("doubled: err=%v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid:   {Name:Alice Email:a@example.com} err=<nil>
unknown: err=decode request: json: unknown field "is_admin"
doubled: err=unexpected trailing data after JSON value: more than one JSON value
```

## Tests

`TestRejectsUnknownField` feeds an undeclared key and asserts the error mentions
`unknown field`. `TestAcceptsValidBody` asserts a clean body decodes with the right
values. `TestRejectsTrailingData` and `TestRejectsGarbageAfterValue` assert the
second guard fires with `ErrTrailingData` (checked via `errors.Is`).
`TestRejectsTruncatedJSON` asserts a cut-off body errors.

Create `httpjson/decode_test.go`:

```go
// httpjson/decode_test.go
package httpjson

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type createUser struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func TestRejectsUnknownField(t *testing.T) {
	t.Parallel()
	_, err := DecodeStrict[createUser](strings.NewReader(`{"name":"m","role":"admin"}`))
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error should mention the unknown field, got: %v", err)
	}
}

func TestAcceptsValidBody(t *testing.T) {
	t.Parallel()
	got, err := DecodeStrict[createUser](strings.NewReader(`{"name":"Alice","email":"a@example.com"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "Alice" || got.Email != "a@example.com" {
		t.Fatalf("wrong decode: %+v", got)
	}
}

func TestRejectsTrailingData(t *testing.T) {
	t.Parallel()
	_, err := DecodeStrict[createUser](strings.NewReader(`{"name":"a"}{"name":"b"}`))
	if !errors.Is(err, ErrTrailingData) {
		t.Fatalf("expected ErrTrailingData, got: %v", err)
	}
}

func TestRejectsGarbageAfterValue(t *testing.T) {
	t.Parallel()
	_, err := DecodeStrict[createUser](strings.NewReader(`{"name":"a"} !!!`))
	if !errors.Is(err, ErrTrailingData) {
		t.Fatalf("expected ErrTrailingData for garbage tail, got: %v", err)
	}
}

func TestRejectsTruncatedJSON(t *testing.T) {
	t.Parallel()
	_, err := DecodeStrict[createUser](strings.NewReader(`{"name":"a`))
	if err == nil {
		t.Fatal("expected error for truncated JSON")
	}
}

func ExampleDecodeStrict() {
	u, err := DecodeStrict[createUser](strings.NewReader(`{"name":"Alice","email":"a@x.com"}`))
	fmt.Printf("%s %s %v\n", u.Name, u.Email, err)
	// Output: Alice a@x.com <nil>
}
```

## Review

The guard is correct when a declared body decodes, an undeclared field errors, and
anything after the first value — a second object or trailing garbage — is rejected
with `ErrTrailingData`. The lesson is that lenient decoding is fine for a response
you produced but dangerous for a request a client controls: `DisallowUnknownFields`
turns silent spec drift and mass assignment into a loud `400`, and the
one-value-only check closes the request-smuggling gap where a second body rides
along unnoticed. Wire this in front of every mutating handler; the small strictness
cost is far cheaper than an escalation-of-privilege incident.

## Resources

- [`json.Decoder.DisallowUnknownFields`](https://pkg.go.dev/encoding/json#Decoder.DisallowUnknownFields) — the unknown-field guard.
- [`json.Decoder.Decode`](https://pkg.go.dev/encoding/json#Decoder.Decode) — streaming one value at a time and the `io.EOF` contract.
- [OWASP: Mass Assignment](https://cheatsheetseries.owasp.org/cheatsheets/Mass_Assignment_Cheat_Sheet.html) — why binding unfiltered input to internal fields is a vulnerability.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-omitzero-vs-omitempty-timestamps.md](02-omitzero-vs-omitempty-timestamps.md) | Next: [04-absent-null-zero-patch-semantics.md](04-absent-null-zero-patch-semantics.md)
