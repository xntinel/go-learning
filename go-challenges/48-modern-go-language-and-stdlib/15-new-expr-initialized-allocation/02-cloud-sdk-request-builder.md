# Exercise 2: SDK-style request builder replacing a Ptr[T] helper

Cloud SDK request structs carry a handful of required fields and dozens of
optional scalars, all modelled as pointers so "unset" is distinct from "the zero
value". Historically every such SDK shipped a `Ptr[T]` helper (or `aws.String`,
`aws.Int64`) to populate those pointers. This exercise builds a PutObject-style
request with a fluent builder, validates it with sentinel errors, marshals it with
`omitempty`, and shows that `new(expr)` retires the helper entirely.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests.

## What you'll build

```text
sdkbuild/                    independent module: example.com/sdkbuild
  go.mod                     go 1.26
  request.go                 PutObjectInput (optional *scalar fields); builder;
                             Validate (sentinels); JSON(); ErrNoBucket/ErrNoKey/ErrBadACL
  cmd/
    demo/
      main.go                builds a request and prints its wire JSON
  request_test.go            builder+marshal tests (compare via map); Validate; Example
```

- Files: `request.go`, `cmd/demo/main.go`, `request_test.go`.
- Implement: a `PutObjectInput` with `*string`/`*int64`/`*bool` optionals, a fluent `PutObjectBuilder` that fills them with `new(expr)`, a `Validate() error` returning wrapped sentinels, and a `JSON()` marshal that drops unset optionals via `omitempty`.
- Test: build requests via the builder, assert set fields appear and unset fields are omitted (compare against expected by unmarshalling both to maps), and assert `Validate` returns the correct sentinel with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Retiring the Ptr[T] helper

Almost every large Go SDK contains this, or imports something that does:

```go
// The helper new(expr) replaces. aws-sdk-go has aws.String/aws.Int64;
// k8s.io/utils/ptr has ptr.To. All exist only to address a value.
func Ptr[T any](v T) *T { return &v }
```

Call sites looked like `CacheControl: Ptr("no-store")` or, with the vendor
helpers, `ContentLength: aws.Int64(1024)`. Each is a function call whose entire
body is `return &v`. `new(expr)` does the same thing as a language builtin, so the
helper package can be deleted and every call site rewritten:

```go
CacheControl:  new("no-store"),   // was Ptr("no-store")
ContentLength: new(int64(1024)),  // was aws.Int64(1024)
```

The `int64` conversion is not optional decoration. `ContentLength` is `*int64`,
but `new(1024)` would be `*int` because an untyped constant defaults to `int`.
`new(int64(1024))` types the expression so the result is `*int64` and the
assignment compiles. Inside the builder, a method parameter already has the field
type, so `new(v)` where `v int64` yields `*int64` directly — the conversion is
only needed when the source is an untyped constant.

Create `request.go`:

```go
package sdkbuild

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Sentinel errors returned (wrapped) by Validate.
var (
	ErrNoBucket = errors.New("bucket is required")
	ErrNoKey    = errors.New("object key is required")
	ErrBadACL   = errors.New("unsupported ACL")
)

// PutObjectInput mirrors a cloud-SDK request: two required scalars and several
// optional ones. Each optional is a pointer so an unset field is distinct from
// one set to the zero value, and omitempty drops the unset ones from the wire.
type PutObjectInput struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`

	CacheControl     *string `json:"cache_control,omitempty"`
	ContentType      *string `json:"content_type,omitempty"`
	ContentLength    *int64  `json:"content_length,omitempty"`
	ACL              *string `json:"acl,omitempty"`
	BucketKeyEnabled *bool   `json:"bucket_key_enabled,omitempty"`
}

// PutObjectBuilder assembles a PutObjectInput fluently. Each optional setter uses
// new(expr) instead of a Ptr helper.
type PutObjectBuilder struct {
	in PutObjectInput
}

// NewPutObject starts a builder with the two required fields.
func NewPutObject(bucket, key string) *PutObjectBuilder {
	return &PutObjectBuilder{in: PutObjectInput{Bucket: bucket, Key: key}}
}

func (b *PutObjectBuilder) CacheControl(v string) *PutObjectBuilder {
	b.in.CacheControl = new(v)
	return b
}

func (b *PutObjectBuilder) ContentType(v string) *PutObjectBuilder {
	b.in.ContentType = new(v)
	return b
}

// ContentLength stores a *int64. v is already int64, so new(v) is a *int64 with
// no conversion needed.
func (b *PutObjectBuilder) ContentLength(v int64) *PutObjectBuilder {
	b.in.ContentLength = new(v)
	return b
}

func (b *PutObjectBuilder) ACL(v string) *PutObjectBuilder {
	b.in.ACL = new(v)
	return b
}

func (b *PutObjectBuilder) BucketKeyEnabled(v bool) *PutObjectBuilder {
	b.in.BucketKeyEnabled = new(v)
	return b
}

// Build returns the assembled request value.
func (b *PutObjectBuilder) Build() PutObjectInput { return b.in }

var validACLs = map[string]bool{
	"private":                   true,
	"public-read":               true,
	"bucket-owner-full-control": true,
}

// Validate reports the first problem with the request as a wrapped sentinel.
func (in PutObjectInput) Validate() error {
	if in.Bucket == "" {
		return fmt.Errorf("put object: %w", ErrNoBucket)
	}
	if in.Key == "" {
		return fmt.Errorf("put object: %w", ErrNoKey)
	}
	if in.ACL != nil && !validACLs[*in.ACL] {
		return fmt.Errorf("put object: %w: %q", ErrBadACL, *in.ACL)
	}
	return nil
}

// JSON marshals the request to its wire form. Unset optionals are omitted.
func (in PutObjectInput) JSON() ([]byte, error) {
	return json.Marshal(in)
}
```

### The runnable demo

The demo builds a request with two optionals set and validates it, then prints the
wire JSON to show that the three unset optionals never appear.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sdkbuild"
)

func main() {
	req := sdkbuild.NewPutObject("photos-bucket", "2026/cat.png").
		ContentType("image/png").
		ContentLength(4096).
		Build()

	if err := req.Validate(); err != nil {
		panic(err)
	}

	b, err := req.JSON()
	if err != nil {
		panic(err)
	}
	fmt.Println(string(b))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"bucket":"photos-bucket","key":"2026/cat.png","content_type":"image/png","content_length":4096}
```

Only the fields that were set appear; `cache_control`, `acl`, and
`bucket_key_enabled` are nil and `omitempty` drops them.

### Tests

Comparing marshalled JSON byte-for-byte is brittle because it couples the test to
field order. The table instead unmarshals both the produced JSON and the expected
JSON into `map[string]any` and compares the maps with `reflect.DeepEqual`, so the
assertion is order-independent and checks exactly which keys are present.
`TestValidate` asserts each sentinel with `errors.Is`. The `Example` pins a
minimal request's compact JSON. The final test is the "your turn" width-trap
proof.

Create `request_test.go`:

```go
package sdkbuild

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func toMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return m
}

func TestBuilderMarshal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		build func() PutObjectInput
		want  string
	}{
		{
			name:  "only required fields",
			build: func() PutObjectInput { return NewPutObject("b", "k").Build() },
			want:  `{"bucket":"b","key":"k"}`,
		},
		{
			name: "some optionals set, unset omitted",
			build: func() PutObjectInput {
				return NewPutObject("b", "k").
					ContentType("text/plain").
					ContentLength(12).
					Build()
			},
			want: `{"bucket":"b","key":"k","content_type":"text/plain","content_length":12}`,
		},
		{
			name: "explicit zero optionals still marshal",
			build: func() PutObjectInput {
				return NewPutObject("b", "k").
					ContentLength(0).
					BucketKeyEnabled(false).
					Build()
			},
			want: `{"bucket":"b","key":"k","content_length":0,"bucket_key_enabled":false}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.build().JSON()
			if err != nil {
				t.Fatalf("JSON() error: %v", err)
			}
			if g, w := toMap(t, got), toMap(t, []byte(tt.want)); !reflect.DeepEqual(g, w) {
				t.Errorf("JSON = %s; want keys of %s", got, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   PutObjectInput
		want error
	}{
		{"ok", NewPutObject("b", "k").ACL("private").Build(), nil},
		{"no bucket", NewPutObject("", "k").Build(), ErrNoBucket},
		{"no key", NewPutObject("b", "").Build(), ErrNoKey},
		{"bad acl", NewPutObject("b", "k").ACL("world-writable").Build(), ErrBadACL},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.in.Validate()
			if tt.want == nil {
				if err != nil {
					t.Fatalf("Validate() = %v; want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("Validate() = %v; want %v", err, tt.want)
			}
		})
	}
}

// TestWidthTrap is the "your turn" case: new of an untyped constant defaults to
// int, while a typed conversion produces the intended width.
func TestWidthTrap(t *testing.T) {
	t.Parallel()

	pInt := new(1024)          // untyped constant defaults to int -> *int
	pInt64 := new(int64(1024)) // conversion -> *int64

	// These declarations only compile with the stated pointer types.
	var _ *int = pInt
	var _ *int64 = pInt64

	if got := fmt.Sprintf("%T %T", pInt, pInt64); got != "*int *int64" {
		t.Fatalf("types = %q; want %q", got, "*int *int64")
	}
}

func ExamplePutObjectInput() {
	req := NewPutObject("photos", "cat.png").ContentType("image/png").Build()
	b, _ := req.JSON()
	fmt.Println(string(b))
	// Output: {"bucket":"photos","key":"cat.png","content_type":"image/png"}
}
```

## Review

The builder is correct when a set optional appears in the wire JSON and an unset
one is dropped, which is why the fields are pointers with `omitempty`: the third
table case proves an *explicit* `0`/`false` still marshals, unlike a value field
with `omitempty` that would silently drop them. The most common real bug is the
width trap — assigning `new(1024)` to an `*int64` field, which does not compile
because the untyped constant defaults to `int`; the builder avoids it by taking a
typed `int64` parameter, and the standalone `new(int64(1024))` shows the explicit
fix. Prefer comparing marshalled output through a map rather than by byte equality
so the test does not break when a field is added. Confirm with `go test -count=1
-race ./...`: the marshal table, the `Validate` sentinels via `errors.Is`, and the
`Example` together cover the builder, the omit behavior, and validation.

## Resources

- [Go language specification — Allocation](https://go.dev/ref/spec#Allocation) — the built-in `new`, including the expression form.
- [Proposal #45624 — spec: expression to create pointer to simple types](https://github.com/golang/go/issues/45624) — the accepted design and its motivation.
- [`k8s.io/utils/ptr`](https://pkg.go.dev/k8s.io/utils/ptr) — the `ptr.To[T]` helper `new(expr)` replaces.
- [`encoding/json` — Marshal and omitempty](https://pkg.go.dev/encoding/json#Marshal) — how `omitempty` treats nil pointers.

---

Back to [01-optional-patch-fields.md](01-optional-patch-fields.md) | Next: [03-config-overlay-merge.md](03-config-overlay-merge.md)
