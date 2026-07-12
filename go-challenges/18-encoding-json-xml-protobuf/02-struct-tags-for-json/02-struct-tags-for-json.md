# 2. Struct Tags for JSON

Tags are the contract between a Go struct and the outside world. By default, `json.Marshal` uses the field name as the JSON key, only considers exported fields, and writes zero values verbatim. Real APIs want lowercase keys, optional fields, fields that are never serialized, and numbers encoded as strings when the other side cannot safely parse 64-bit integers. The `json:"..."` struct tag is the single mechanism that controls all of that. The trick is that the tag is just a string with a defined grammar: a name, an optional comma, and a comma-separated list of options (`omitempty`, `omitzero`, `string`). Every other JSON feature in this chapter builds on tags.

## Concepts

### Tag Grammar

A struct field tag is a backtick-quoted string attached to a field declaration. The JSON package reads the value under the `json` key:

```go
Field int `json:"myName"`
```

The grammar of the value is `[name][,option1[,option2...]]`:

- **name**: the JSON key. Empty name keeps the default (the field name). A name of `-` means "omit this field"; a name of `-,` (note the trailing comma) means "use the literal key `-`".
- **omitempty**: omit the field when its value is the zero value for its type — `false`, `0`, `nil` pointer, `nil` interface, and any array, slice, map, or string of length zero.
- **omitzero**: omit when the value reports zero. The package first checks for an `IsZero() bool` method, then falls back to the type's zero value. Combining `omitempty` and `omitzero` is allowed; the field is omitted if either condition holds.
- **string**: encode the value as a JSON string. Applies to string, integer, floating-point, and boolean fields. Most common use case is `int64` going to a JavaScript client that loses precision on values above 2^53.

### The `omitempty` Trap On Slices And Maps

`omitempty` on a slice or map drops it when the length is zero, which makes the field look "absent" — but a `nil` slice and an empty (length-zero) slice both satisfy that rule. If the receiving side needs to distinguish "field not sent" from "field sent as empty", a pointer is the only way. `*[]int` with `omitempty` is `null` when the pointer is `nil` and `[]` (the literal empty array) when the pointer points to a zero-length slice. This pattern is verbose; the alternative is to always send the field and let the receiver treat absence the same as presence-with-zero.

### Embedded Structs: Flatten Or Nest

An anonymous (embedded) struct field is marshaled as if its exported fields were members of the outer struct. The fields are promoted up one level. To keep the embedded struct as a nested object, give the field an explicit name and tag:

```go
type Person struct {
    Name    string
    Address        // promoted: city and country appear at the top level
}

type Person struct {
    Name    string
    Address Address `json:"address"` // nested under "address"
}
```

There is one rule to remember: an anonymous field with a name in its JSON tag is treated as having that name, not as anonymous.

### The `string` Option And The JavaScript Problem

JavaScript numbers are IEEE-754 doubles, which have 53 bits of integer precision. A `int64` value with bits 53-63 set loses precision when parsed as a JavaScript number. The `string` option encodes the value as a JSON string so the receiving side can parse it as a `BigInt` or a string and keep the bits. This is the standard fix for any JSON API that needs to cross the JavaScript boundary safely.

### `json.RawMessage` And Why Tags Are Not Always Enough

Tags control the encoding of values whose static type is in the struct definition. They cannot help when the data is polymorphic — when the same field can hold two different shapes. For that case, `json.RawMessage` (covered in the lesson on unknown fields) holds the raw bytes so the caller can decode based on a discriminator. The rule of thumb: tags for known shapes, `RawMessage` for shapes chosen at runtime.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/18-encoding-json-xml-protobuf/02-struct-tags-for-json/02-struct-tags-for-json/internal/response
mkdir -p go-solutions/18-encoding-json-xml-protobuf/02-struct-tags-for-json/02-struct-tags-for-json/cmd/demo
cd go-solutions/18-encoding-json-xml-protobuf/02-struct-tags-for-json/02-struct-tags-for-json
```

This is a library plus a small demo. The verification is `go test -race`.

### Exercise 1: The Response Type And Its Tags

Create `internal/response/response.go`:

```go
package response

import "errors"

var (
	ErrEmptyStatus = errors.New("response status must not be empty")
	ErrInvalidCode = errors.New("response code must be in [100,599]")
)

type APIResponse struct {
	Status        string `json:"status"`
	Code          int    `json:"code"`
	Message       string `json:"message,omitempty"`
	Data          any    `json:"data,omitempty"`
	InternalTrace string `json:"-"`
	RequestID     int64  `json:"request_id,string"`
}

func New(status string, code int, message string, data any, internal string, requestID int64) (APIResponse, error) {
	if status == "" {
		return APIResponse{}, ErrEmptyStatus
	}
	if code < 100 || code > 599 {
		return APIResponse{}, ErrInvalidCode
	}
	return APIResponse{
		Status:        status,
		Code:          code,
		Message:       message,
		Data:          data,
		InternalTrace: internal,
		RequestID:     requestID,
	}, nil
}

func (r APIResponse) RequestIDInt() int64 {
	return r.RequestID
}
```

Six tag behaviors in one struct: rename, keep, omit-when-empty, omit-when-empty (the `Data` field, an `any` interface), exclude entirely (`-`), and string-encode a number. `RequestIDInt` is an exported accessor so the demo can read the field without exposing it.

### Exercise 2: Test The Tag Behavior

Create `internal/response/response_test.go`:

```go
package response

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestNewRejectsEmptyStatus(t *testing.T) {
	t.Parallel()

	if _, err := New("", 200, "", nil, "", 0); !errors.Is(err, ErrEmptyStatus) {
		t.Fatalf("err = %v, want ErrEmptyStatus", err)
	}
}

func TestNewRejectsBadCode(t *testing.T) {
	t.Parallel()

	for _, code := range []int{0, 99, 600, 1000} {
		if _, err := New("ok", code, "", nil, "", 0); !errors.Is(err, ErrInvalidCode) {
			t.Errorf("code=%d: err = %v, want ErrInvalidCode", code, err)
		}
	}
}

func TestNewAcceptsValidCode(t *testing.T) {
	t.Parallel()

	for _, code := range []int{100, 200, 404, 500, 599} {
		r, err := New("ok", code, "", nil, "", 0)
		if err != nil {
			t.Errorf("code=%d: unexpected error: %v", code, err)
			continue
		}
		if r.Code != code {
			t.Errorf("Code = %d, want %d", r.Code, code)
		}
	}
}

func TestSuccessMarshalsWithTraceHidden(t *testing.T) {
	t.Parallel()

	r, err := New("ok", 200, "User created", map[string]string{"id": "abc123"}, "trace-xyz-internal", 42)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent error: %v", err)
	}

	want := "{\n" +
		"  \"status\": \"ok\",\n" +
		"  \"code\": 200,\n" +
		"  \"message\": \"User created\",\n" +
		"  \"data\": {\n" +
		"    \"id\": \"abc123\"\n" +
		"  },\n" +
		"  \"request_id\": \"42\"\n" +
		"}"
	if string(data) != want {
		t.Fatalf("MarshalIndent mismatch\n got: %s\nwant: %s", data, want)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if _, ok := raw["InternalTrace"]; ok {
		t.Fatalf("InternalTrace must not appear: %s", data)
	}
	if _, ok := raw["internal_trace"]; ok {
		t.Fatalf("InternalTrace must not appear in any case form: %s", data)
	}
}

func TestErrorMarshalsWithOmitemptyFieldsAbsent(t *testing.T) {
	t.Parallel()

	r, err := New("error", 404, "", nil, "", 43)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent error: %v", err)
	}

	want := "{\n" +
		"  \"status\": \"error\",\n" +
		"  \"code\": 404,\n" +
		"  \"request_id\": \"43\"\n" +
		"}"
	if string(data) != want {
		t.Fatalf("MarshalIndent mismatch\n got: %s\nwant: %s", data, want)
	}
}

func TestStringTagEncodesInt64AsString(t *testing.T) {
	t.Parallel()

	r, _ := New("ok", 200, "", nil, "", 9223372036854775807)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	got, ok := raw["request_id"].(string)
	if !ok {
		t.Fatalf("request_id type = %T, want string (raw=%s)", raw["request_id"], data)
	}
	if got != "9223372036854775807" {
		t.Fatalf("request_id = %q, want 9223372036854775807", got)
	}
}

func TestUnmarshalReadsStringTagAsInt64(t *testing.T) {
	t.Parallel()

	in := []byte(`{"status":"ok","code":200,"data":{"items":["a","b"]},"request_id":"99"}`)
	var r APIResponse
	if err := json.Unmarshal(in, &r); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if r.Status != "ok" || r.Code != 200 || r.RequestID != 99 {
		t.Fatalf("r = %+v", r)
	}
	if r.Message != "" {
		t.Fatalf("Message = %q, want empty", r.Message)
	}
	if r.Data == nil {
		t.Fatal("Data is nil, want non-nil")
	}
}

func TestEmbeddedStructFlattens(t *testing.T) {
	t.Parallel()

	type Address struct {
		City    string `json:"city"`
		Country string `json:"country"`
	}
	type Person struct {
		Name string `json:"name"`
		Address
	}

	p := Person{Name: "Alice", Address: Address{City: "London", Country: "UK"}}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	want := `{"name":"Alice","city":"London","country":"UK"}`
	if string(data) != want {
		t.Fatalf("got %s, want %s", data, want)
	}
}

func TestNamedEmbedNestsInsteadOfFlatten(t *testing.T) {
	t.Parallel()

	type Address struct {
		City    string `json:"city"`
		Country string `json:"country"`
	}
	type Person struct {
		Name    string  `json:"name"`
		Address Address `json:"address"`
	}

	p := Person{Name: "Alice", Address: Address{City: "London", Country: "UK"}}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	want := `{"name":"Alice","address":{"city":"London","country":"UK"}}`
	if string(data) != want {
		t.Fatalf("got %s, want %s", data, want)
	}
}

func TestPointerOmitEmptyDistinguishesAbsentFromZero(t *testing.T) {
	t.Parallel()

	type Config struct {
		Retries *int `json:"retries,omitempty"`
	}

	zero := 0
	c1, _ := json.Marshal(Config{Retries: &zero})
	c2, _ := json.Marshal(Config{Retries: nil})

	if string(c1) != `{"retries":0}` {
		t.Fatalf("present-zero = %s, want {\"retries\":0}", c1)
	}
	if string(c2) != `{}` {
		t.Fatalf("absent = %s, want {}", c2)
	}
}

func ExampleAPIResponse() {
	r, _ := New("ok", 200, "User created", map[string]string{"id": "abc123"}, "trace-xyz-internal", 42)
	data, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(data))
	// Output:
	// {
	//   "status": "ok",
	//   "code": 200,
	//   "message": "User created",
	//   "data": {
	//     "id": "abc123"
	//   },
	//   "request_id": "42"
	// }
}
```

The interesting tests are `TestEmbeddedStructFlattens` and `TestNamedEmbedNestsInsteadOfFlatten` — they pin the two valid styles against each other so a future refactor cannot quietly change the JSON shape. `TestStringTagEncodesInt64AsString` uses a value of `9223372036854775807` (max int64) to prove the bits survive the round-trip.

### Exercise 3: A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"

	"example.com/apiresponse/internal/response"
)

func main() {
	success, err := response.New(
		"ok",
		200,
		"User created",
		map[string]string{"id": "abc123"},
		"trace-xyz-internal",
		42,
	)
	if err != nil {
		log.Fatal(err)
	}
	errResp, err := response.New("error", 404, "", nil, "", 43)
	if err != nil {
		log.Fatal(err)
	}

	for _, r := range []response.APIResponse{success, errResp} {
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(string(data))
		fmt.Println()
	}

	in := `{"status":"ok","code":200,"data":{"items":["a","b"]},"request_id":"99"}`
	var parsed response.APIResponse
	if err := json.Unmarshal([]byte(in), &parsed); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Parsed: %+v\n", parsed)
}
```

Run it:

```bash
go run ./cmd/demo
```

The first two blocks show the indented JSON; the last line shows the round-tripped struct with `InternalTrace` empty (it was never sent) and `RequestID: 99` (parsed from the string `"99"`).

Your turn: add a `TestStringTagPreserves64Bits` test that marshals an `int64` with value `9223372036854775807` and an `int64` with value `-9223372036854775808`, then unmarshals both into a fresh `APIResponse`, and asserts that `RequestIDInt()` returns the original values bit-for-bit. The test pins the contract that the `string` option preserves precision across the wire.

## Common Mistakes

### Forgetting The Trailing Comma In `json:"-,"`

Wrong: a field whose literal JSON key should be the single character `-`. The naive tag `json:"-"` excludes the field; the data is silently lost.

Fix: write `json:"-,"`. The trailing comma tells the parser that `-` is the name, not the omit marker. This is rare but appears in legacy systems that use `-` as a separator.

### `omitempty` On A Field Whose Zero Value Is Meaningful

Wrong: `Retries int \`json:"retries,omitempty"\`` and the application distinguishes "0 retries" from "retries not set". The encoder drops the field in both cases, and the receiver sees a missing key.

Fix: use `*int` with `omitempty`. The pointer is `nil` for "not set" and points to `0` for "explicitly zero". The receiving side can tell the cases apart.

### Believing `string` Is A Generic Quoting Option

Wrong: a developer reads the `string` option and assumes it can wrap any value in quotes. They apply it to a struct field, a slice, or a map, and the encoder either errors out or produces nonsense.

Fix: the `string` option applies only to string, integer, floating-point, and boolean fields. For other types, you need a custom `MarshalJSON` method (covered in the next lesson).

### Embedding Without Realizing The Field Gets Promoted

Wrong: a developer embeds a `Base` struct for code reuse, and the JSON output suddenly has every field of `Base` at the top level — including ones with names that collide with the outer struct.

Fix: give the embedded field a JSON tag with a name when you want it to nest. Audit the marshaled output once after any embedding change; the test in this lesson pins the two valid shapes.

## Verification

From `~/go-exercises/apiresponse`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass. The `ExampleAPIResponse` test in the suite is auto-verified under `go test` because it carries an `// Output:` comment.

## Summary

- A JSON tag has the grammar `[name][,option...]`: a name, an optional comma, and a list of options.
- `omitempty` drops a field when it holds the zero value for its type; on a slice or map, "zero" means length zero.
- `omitzero` drops a field when its value reports zero via `IsZero() bool` or the type's zero value.
- The `string` option wraps a numeric or boolean value in a JSON string; useful for `int64` going to a JavaScript client.
- An anonymous embedded struct is flattened by default; give it a tag with a name to nest it.
- `json:"-"` excludes a field; `json:"-,"` writes the literal key `-`.

## What's Next

Next: [Custom JSON Marshaler](../03-custom-json-marshaler/03-custom-json-marshaler.md).

## Resources

- [encoding/json package](https://pkg.go.dev/encoding/json)
- [JSON and Go (Go blog)](https://go.dev/blog/json)
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types)
