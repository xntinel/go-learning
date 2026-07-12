# Exercise 1: Shape an API Response Type With json, omitempty, and json:"-"

Every JSON API starts here: a response type whose Go field names, wire keys,
optional fields, and never-serialized internals are all controlled by struct
tags. This module builds the `Response` and `ErrorResponse` types a handler
returns, and proves with tests that the wire contract is exactly what the tags
promise.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
apiresp/                       independent module: example.com/apiresp
  go.mod                       go 1.24
  api/
    response.go                Response, ErrorResponse with json/omitempty/json:"-"
  cmd/
    demo/
      main.go                  marshal a Response and an ErrorResponse, print bytes
  api/response_test.go         wire-key assertions, omission, round-trip, Example
```

Files: `api/response.go`, `cmd/demo/main.go`, `api/response_test.go`.
Implement: `Response{ID, Name, Email omitempty, CreatedAt, Internal json:"-"}` and `ErrorResponse{Code, Message, Details omitempty}`.
Test: correct wire keys, omission of empty `Email`/`Details`, skipping of `Internal`, a full `Marshal`/`Unmarshal` round-trip, and that `Details` appears when set.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

## The wire contract lives in the tags

The whole point of this type is that a reader can look at the struct and know the
exact JSON a client receives. `ID` and `Name` map to `id` and `name`. `Email`
carries `omitempty`, so a user with no email produces JSON with no `email` key at
all rather than `"email":""` — the difference between "we have no email on file"
and "the email is the empty string". `CreatedAt` is a `time.Time`, which marshals
to an RFC 3339 string (`2024-01-15T10:30:00Z`) because `time.Time` implements
`json.Marshaler` itself. `Internal` is tagged `json:"-"`: it exists in Go for
server-side bookkeeping and is guaranteed never to appear on the wire, in either
direction — `Marshal` skips it and `Unmarshal` ignores any `Internal` key a
client tries to send.

`ErrorResponse` is the error envelope: a stable machine-readable `Code`, a
human-readable `Message`, and an optional `Details` string that only appears when
the handler actually has extra context to attach. `omitempty` on `Details` keeps
the common error small.

A subtle correctness note the round-trip test pins: `omitempty` on `Email` affects
only *encoding*. On the way back in, an absent `email` key simply leaves the field
at its zero value, and a present one fills it. So marshaling and unmarshaling a
`Response` reproduces every field that was set, and leaves `Internal` at its zero
value because it never crossed the wire.

Create `api/response.go`:

```go
// api/response.go
package api

import "time"

// Response is the success envelope a handler returns to a client. The struct
// tags are the wire contract: Email is omitted when empty, Internal never
// serializes in either direction.
type Response struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Internal  string    `json:"-"`
}

// ErrorResponse is the error envelope. Details is optional and omitted when the
// handler has no extra context to attach.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}
```

## The runnable demo

The demo marshals one of each type so you can see the exact bytes: a `Response`
with no email (the `email` key is absent) and an `ErrorResponse` with no details.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"encoding/json"
	"fmt"
	"time"

	"example.com/apiresp/api"
)

func main() {
	r := api.Response{
		ID:        "u1",
		Name:      "Alice",
		CreatedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Internal:  "audit-token-never-serialized",
	}
	rb, _ := json.Marshal(r)
	fmt.Printf("response: %s\n", rb)

	e := api.ErrorResponse{Code: "NOT_FOUND", Message: "user not found"}
	eb, _ := json.Marshal(e)
	fmt.Printf("error:    %s\n", eb)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
response: {"id":"u1","name":"Alice","created_at":"2024-01-15T10:30:00Z"}
error:    {"code":"NOT_FOUND","message":"user not found"}
```

Note what is absent: no `email`, no `Internal`, no `audit-token`, no `details`.

## Tests

The tests assert the wire contract directly. `TestResponseMarshalsWithCorrectKeys`
checks that each expected `"key":value` substring is present.
`TestResponseOmitsEmptyEmail` and `TestErrorResponseOmitsEmptyDetails` are the
negative assertions that `omitempty` works. `TestResponseSkipsInternalField`
proves `json:"-"` drops both the key and the value.
`TestResponseRoundTripsThroughJSON` is the central proof that encoding preserves
data: it marshals, unmarshals into a fresh value, and compares every field,
comparing `CreatedAt` with `time.Time.Equal` (never `==`, which also compares the
monotonic-clock reading and location). `TestErrorResponseDetailsWhenSet` is the
positive counterpart that pins the "omit only when empty" contract from the other
side.

Create `api/response_test.go`:

```go
// api/response_test.go
package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestResponseMarshalsWithCorrectKeys(t *testing.T) {
	t.Parallel()
	r := Response{
		ID:        "u1",
		Name:      "Alice",
		CreatedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, key := range []string{`"id":"u1"`, `"name":"Alice"`, `"created_at":"2024-01-15T10:30:00Z"`} {
		if !strings.Contains(got, key) {
			t.Fatalf("missing key %s in %s", key, got)
		}
	}
}

func TestResponseOmitsEmptyEmail(t *testing.T) {
	t.Parallel()
	r := Response{ID: "u1", Name: "Alice", CreatedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)}
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), `"email"`) {
		t.Fatalf("email should be omitted: %s", data)
	}
}

func TestResponseSkipsInternalField(t *testing.T) {
	t.Parallel()
	r := Response{ID: "u1", Name: "Alice", Internal: "secret", CreatedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)}
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), `"Internal"`) || strings.Contains(string(data), `secret`) {
		t.Fatalf("internal field should be skipped: %s", data)
	}
}

func TestResponseRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()
	original := Response{
		ID:        "u1",
		Name:      "Alice",
		Email:     "alice@example.com",
		CreatedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Internal:  "must-not-survive",
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var got Response
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != original.ID || got.Name != original.Name || got.Email != original.Email {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, original)
	}
	if !got.CreatedAt.Equal(original.CreatedAt) {
		t.Fatalf("CreatedAt mismatch: %v vs %v", got.CreatedAt, original.CreatedAt)
	}
	if got.Internal != "" {
		t.Fatalf("internal field should be zero after round trip, got %q", got.Internal)
	}
}

func TestErrorResponseMarshalsWithCorrectKeys(t *testing.T) {
	t.Parallel()
	e := ErrorResponse{Code: "NOT_FOUND", Message: "user not found"}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, key := range []string{`"code":"NOT_FOUND"`, `"message":"user not found"`} {
		if !strings.Contains(got, key) {
			t.Fatalf("missing key %s in %s", key, got)
		}
	}
}

func TestErrorResponseOmitsEmptyDetails(t *testing.T) {
	t.Parallel()
	e := ErrorResponse{Code: "NOT_FOUND", Message: "user not found"}
	data, _ := json.Marshal(e)
	if strings.Contains(string(data), `"details"`) {
		t.Fatalf("details should be omitted: %s", data)
	}
}

func TestErrorResponseDetailsWhenSet(t *testing.T) {
	t.Parallel()
	e := ErrorResponse{Code: "CONFLICT", Message: "email taken", Details: "email alice@example.com already registered"}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"details":"email alice@example.com already registered"`) {
		t.Fatalf("details should be present when set: %s", data)
	}
}

func ExampleResponse() {
	r := Response{ID: "u1", Name: "Alice", CreatedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)}
	b, _ := json.Marshal(r)
	fmt.Println(string(b))
	// Output: {"id":"u1","name":"Alice","created_at":"2024-01-15T10:30:00Z"}
}
```

## Review

The type is correct when the tags and the tests agree: `id`, `name`, `created_at`
always present; `email` and `details` present exactly when non-empty; `Internal`
never present in either direction; and a marshal/unmarshal round-trip reproduces
every field that crossed the wire. The two traps this exercise inoculates against
are exporting a field that should never serialize (fixed by `json:"-"`) and
asserting on JSON with a whole-string `==` (fixed by substring checks and, for
`time.Time`, `Equal` rather than `==`). Everything past this module is a variation
on owning the wire contract more precisely: omitting zero timestamps, rejecting
unknown fields, and taking over encoding entirely with a custom marshaler.

## Resources

- [`encoding/json`](https://pkg.go.dev/encoding/json) — `Marshal`, `Unmarshal`, and the `json` struct-tag options.
- [Go spec: Struct types](https://go.dev/ref/spec#Struct_types) — the struct-tag grammar.
- [`time.Time.MarshalJSON`](https://pkg.go.dev/time#Time.MarshalJSON) — why a `time.Time` becomes an RFC 3339 string.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-omitzero-vs-omitempty-timestamps.md](02-omitzero-vs-omitempty-timestamps.md)
