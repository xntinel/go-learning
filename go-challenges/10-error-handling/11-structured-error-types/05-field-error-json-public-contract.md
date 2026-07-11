# Exercise 5: A Stable Public JSON Contract Via json.Marshaler

If you `json.Marshal` your error struct straight to the client, the public wire
schema becomes an accident of your Go field names — rename `Field` to `Path` in a
refactor and every client silently breaks, and the internal `Err` cause leaks
onto the wire. This module hand-writes `MarshalJSON` so the public schema is a
versioned API: `{code, path, detail}` per field, wrapped in the RFC 9457
`problem+json` envelope.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
problemjson/               independent module: example.com/problemjson
  go.mod                   go 1.26
  problem.go               FieldError.MarshalJSON ({code,path,detail}); ValidationError.MarshalJSON (RFC 9457)
  cmd/
    demo/
      main.go              runnable demo: marshal an aggregate, print the wire body
  problem_test.go          golden-shape test; byte-for-byte round-trip stability
```

- Files: `problem.go`, `cmd/demo/main.go`, `problem_test.go`.
- Implement: `FieldError.MarshalJSON` emitting `{code, path, detail}` (renaming `Field` to `path`, never leaking the cause, rendering `detail` from `Code`+`Params`); `ValidationError.MarshalJSON` emitting `{type, title, status, errors}` per RFC 9457, sorting field errors for determinism.
- Test: golden shape (contains `path`, `code`, top-level `status: 422`, and no internal Go field name); byte-identical on repeated marshal of the same input.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/problemjson/cmd/demo
cd ~/go-exercises/problemjson
go mod init example.com/problemjson
go mod edit -go=1.26
```

### The wire shape is decoupled from the struct

The internal `FieldError` keeps whatever fields the domain needs — `Code`,
`Field`, `Params`, and an unexported `cause`. The public JSON is something else
entirely, produced by `MarshalJSON`: three keys, `code`, `path`, `detail`.
`path` is `Field` renamed (the public name), `code` is the stable machine
constant, and `detail` is *rendered from* `Code`+`Params` through a small catalog
rather than copied from any stored string — so the human text is localizable and
stable, and there is no way for the unexported `cause` to escape onto the wire.
Because `MarshalJSON` names every key explicitly, a rename of the Go `Field` field
does not touch the contract.

`ValidationError.MarshalJSON` wraps the set in the RFC 9457 shape:
`type`, `title`, `status` (422), and an `errors` array. Two determinism details:
it marshals a *sorted clone* of the field errors (sorted by path) so the output
byte order does not depend on validation order, and it clones rather than sorting
in place so marshaling does not mutate the error. Determinism is what makes the
golden test and any HTTP cache or diff meaningful.

Create `problem.go`:

```go
package problemjson

import (
	"encoding/json"
	"fmt"
	"slices"
)

type Code string

const (
	CodeRequired Code = "required"
	CodeMaxLen   Code = "max_len"
	CodeRange    Code = "range"
)

// FieldError is the internal type. cause is unexported and must never reach the
// wire.
type FieldError struct {
	Code   Code
	Field  string
	Params map[string]any
	cause  error
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Code)
}

func (e *FieldError) Unwrap() error { return e.cause }

// detail renders human text from Code+Params, not from a stored string, so it is
// stable and localizable.
func (e *FieldError) detail() string {
	switch e.Code {
	case CodeRequired:
		return fmt.Sprintf("%s is required", e.Field)
	case CodeMaxLen:
		return fmt.Sprintf("must be at most %v characters", e.Params["max"])
	case CodeRange:
		return fmt.Sprintf("must be between %v and %v", e.Params["min"], e.Params["max"])
	default:
		return "is invalid"
	}
}

// MarshalJSON emits the public per-field shape {code, path, detail}. It renames
// Field to path and never exposes Params or cause directly.
func (e *FieldError) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Code   Code   `json:"code"`
		Path   string `json:"path"`
		Detail string `json:"detail"`
	}{
		Code:   e.Code,
		Path:   e.Field,
		Detail: e.detail(),
	})
}

type ValidationError struct {
	Errors []*FieldError
}

func (e *ValidationError) Error() string { return "validation failed" }

// MarshalJSON emits the RFC 9457 problem+json envelope with a deterministic,
// sorted errors array. It sorts a clone so marshaling does not mutate the error.
func (e *ValidationError) MarshalJSON() ([]byte, error) {
	sorted := slices.Clone(e.Errors)
	slices.SortFunc(sorted, func(a, b *FieldError) int {
		if a.Field != b.Field {
			if a.Field < b.Field {
				return -1
			}
			return 1
		}
		return 0
	})
	return json.Marshal(struct {
		Type   string        `json:"type"`
		Title  string        `json:"title"`
		Status int           `json:"status"`
		Errors []*FieldError `json:"errors"`
	}{
		Type:   "about:blank",
		Title:  "Your request is invalid.",
		Status: 422,
		Errors: sorted,
	})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/problemjson"
)

func main() {
	ve := &problemjson.ValidationError{
		Errors: []*problemjson.FieldError{
			{Code: problemjson.CodeMaxLen, Field: "name", Params: map[string]any{"max": 3}},
			{Code: problemjson.CodeRequired, Field: "email"},
		},
	}

	data, _ := json.Marshal(ve)
	fmt.Println(string(data))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"type":"about:blank","title":"Your request is invalid.","status":422,"errors":[{"code":"required","path":"email","detail":"email is required"},{"code":"max_len","path":"name","detail":"must be at most 3 characters"}]}
```

The field errors come out sorted by `path` (`email` before `name`) even though
they were supplied in the opposite order, because `MarshalJSON` sorts a clone.

### Tests

The golden test asserts the exact envelope: the top-level `"status":422`, the
presence of `path`, `code`, and `detail`, and — crucially — the *absence* of any
internal Go field name (`"Field"`, `"Params"`, `"cause"`, `"Err"`). The
round-trip test marshals the same input twice and asserts the two byte slices are
identical, proving the sort makes the output deterministic.

Create `problem_test.go`:

```go
package problemjson

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func sample() *ValidationError {
	return &ValidationError{
		Errors: []*FieldError{
			{Code: CodeMaxLen, Field: "name", Params: map[string]any{"max": 3}, cause: nil},
			{Code: CodeRequired, Field: "email"},
		},
	}
}

func TestGoldenShape(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(sample())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)

	want := `{"type":"about:blank","title":"Your request is invalid.","status":422,` +
		`"errors":[{"code":"required","path":"email","detail":"email is required"},` +
		`{"code":"max_len","path":"name","detail":"must be at most 3 characters"}]}`
	if got != want {
		t.Fatalf("golden mismatch:\n got=%s\nwant=%s", got, want)
	}
}

func TestNoInternalNamesLeak(t *testing.T) {
	t.Parallel()

	data, _ := json.Marshal(sample())
	got := string(data)

	for _, leak := range []string{"Field", "Params", "cause", "Err", `"message"`} {
		if strings.Contains(got, leak) {
			t.Fatalf("wire body leaked internal name %q: %s", leak, got)
		}
	}
	for _, want := range []string{`"path"`, `"code"`, `"status":422`} {
		if !strings.Contains(got, want) {
			t.Fatalf("wire body missing %q: %s", want, got)
		}
	}
}

func TestRoundTripStable(t *testing.T) {
	t.Parallel()

	a, _ := json.Marshal(sample())
	b, _ := json.Marshal(sample())
	if !bytes.Equal(a, b) {
		t.Fatalf("marshal not deterministic:\n a=%s\n b=%s", a, b)
	}
}
```

An `Example` verified against its `// Output:` comment:

```go
// problem_example_test.go
package problemjson

import (
	"encoding/json"
	"fmt"
)

func ExampleFieldError_MarshalJSON() {
	fe := &FieldError{Code: CodeRequired, Field: "email"}
	data, _ := json.Marshal(fe)
	fmt.Println(string(data))
	// Output: {"code":"required","path":"email","detail":"email is required"}
}
```

## Review

The contract is correct when the marshaled body contains `code`, `path`, and
`detail` and does not contain a single internal Go identifier — that is what
proves the wire schema is decoupled from the struct. `detail` must be rendered
from `Code`+`Params` (so it is stable and localizable), never copied from a stored
message. The envelope must match RFC 9457 (`type`, `title`, `status`, `errors`),
and the errors array must be sorted so the output is byte-deterministic across
runs — the round-trip test is the guard against a map or slice order sneaking
nondeterminism onto the wire. The mistake this closes is `json.Marshal(rawStruct)`
with JSON tags: it welds the public API to Go field names and leaks the cause.
Hand-write `MarshalJSON`. Run `go test -race`.

## Resources

- [`json.Marshaler`](https://pkg.go.dev/encoding/json#Marshaler) — the `MarshalJSON` interface that decouples wire shape from struct layout.
- [RFC 9457: Problem Details for HTTP APIs](https://www.rfc-editor.org/rfc/rfc9457.html) — the `type`/`title`/`status`/`detail` members and `application/problem+json`.
- [`slices.SortFunc`](https://pkg.go.dev/slices#SortFunc) — deterministic ordering of the errors array.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-validation-rule-registry.md](04-validation-rule-registry.md) | Next: [06-validation-http-422-handler.md](06-validation-http-422-handler.md)
