# Exercise 1: A JSON Patch Builder with an Anonymous Struct Field

A JSON Patch document is a real wire format (RFC 6902) that services accept to
mutate a resource: an ordered list of operations plus, in practice, a small
metadata envelope. This exercise builds a `Patch` builder whose `Meta` field is
an *anonymous struct* — the metadata deserves no package-level name — and whose
`Operations` slice preserves the order operations were added, because a patch
applied out of order is a different patch.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
patch/                      independent module: example.com/patch
  go.mod                    module example.com/patch
  patch.go                  type Patch{Meta anon struct; Operations []Op}; Add, OpCount, SetMeta
  cmd/
    demo/
      main.go               build a patch, marshal it, print the wire JSON
  patch_test.go             table tests, JSON round-trip, ordering test, Example
```

Files: `patch.go`, `cmd/demo/main.go`, `patch_test.go`.
Implement: a `Patch` with an anonymous-struct `Meta` field (`ID`, `CreatedAt`), an
`Operations []Op` slice, and `Add(op, path string, value any) error`,
`OpCount() int`, `SetMeta(id, createdAt string)`.
Test: empty patch has `OpCount` 0; `Add` appends and rejects empty op/path;
`SetMeta` mutates through a pointer receiver; a JSON round-trip preserves
`Meta.ID` and the operation count; multiple operations keep their order on the
wire.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/patch/cmd/demo
cd ~/go-exercises/patch
go mod init example.com/patch
```

### Why the metadata is an anonymous struct

The `Meta` block — an ID and a creation timestamp — exists only as part of a
`Patch`. Nothing else in the package constructs a bare `Meta`, no caller needs to
hold one by itself, and giving it a package-level name (`type PatchMeta struct
{...}`) would add a symbol to the package surface that no one references. That is
exactly the scope where an anonymous struct is the right tool: it is a field
whose *type* is written inline. The trade-off is that the inner fields are *not*
promoted — you reach them as `patch.Meta.ID`, never `patch.ID` — which is fine,
because `Meta` is a genuine sub-object, not something you want flattened onto the
patch.

Contrast this with `Op`, which *is* a named type: operations are constructed,
appended, iterated, and (in a fuller implementation) validated individually, and
callers legitimately want to name the type. The rule of thumb is visible right
here: name a type when it has an independent life; leave it anonymous when it is a
one-off shape local to its container.

### Why order is part of the contract

`Add` appends to a slice, and a slice preserves insertion order. That is not an
implementation detail — for JSON Patch it is the semantics. `[{"op":"remove",
"path":"/a"},{"op":"add","path":"/a"}]` and its reverse produce different
documents. So the ordering test is not busywork; it pins a behavioral contract
that a future refactor (say, switching `Operations` to a map keyed by path) would
silently break. `Add` also validates: an empty `op` or `path` is a client error,
returned as a wrapped error rather than silently appended, because a patch with a
blank path is not a patch a server should ever try to apply.

Create `patch.go`:

```go
package patch

import "fmt"

// Op is a single JSON Patch operation (RFC 6902 shape). It is a named type
// because operations are constructed, iterated, and validated on their own.
type Op struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// Patch is an ordered list of operations plus a small metadata envelope. Meta
// is an anonymous struct: a one-off shape that deserves no package-level name.
type Patch struct {
	Meta struct {
		ID        string `json:"id"`
		CreatedAt string `json:"created_at"`
	} `json:"meta"`
	Operations []Op `json:"operations"`
}

// Add appends an operation, rejecting an empty op or path. Order is preserved.
func (p *Patch) Add(op, path string, value any) error {
	if op == "" {
		return fmt.Errorf("patch: op is required")
	}
	if path == "" {
		return fmt.Errorf("patch: path is required")
	}
	p.Operations = append(p.Operations, Op{Op: op, Path: path, Value: value})
	return nil
}

// OpCount reports how many operations the patch holds.
func (p Patch) OpCount() int { return len(p.Operations) }

// SetMeta fills the anonymous Meta field through a pointer receiver so the
// mutation is visible to the caller.
func (p *Patch) SetMeta(id, createdAt string) {
	p.Meta.ID = id
	p.Meta.CreatedAt = createdAt
}
```

### The runnable demo

The demo builds a two-operation patch and marshals it, so you can see the wire
shape: `meta` nests (it is a named field with an anonymous-struct type), and
`operations` is an ordered array.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/patch"
)

func main() {
	var p patch.Patch
	p.SetMeta("patch-42", "2026-07-02T09:00:00Z")
	if err := p.Add("replace", "/name", "Alice"); err != nil {
		panic(err)
	}
	if err := p.Add("add", "/tags/0", "go"); err != nil {
		panic(err)
	}

	data, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	fmt.Printf("operations: %d\n", p.OpCount())
	fmt.Println(string(data))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
operations: 2
{"meta":{"id":"patch-42","created_at":"2026-07-02T09:00:00Z"},"operations":[{"op":"replace","path":"/name","value":"Alice"},{"op":"add","path":"/tags/0","value":"go"}]}
```

### Tests

The table cases pin the small contracts (empty patch, append, both validation
rejections, `SetMeta` mutation). `TestPatchRoundTripsThroughJSON` is the anchor:
it proves the anonymous `Meta` field encodes and decodes cleanly.
`TestPatchWithMultipleOperationsPreservesOrder` pins the ordering contract by
checking the byte positions of each path in the marshaled output.

Create `patch_test.go`:

```go
package patch

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestNewPatchIsEmpty(t *testing.T) {
	t.Parallel()

	var p Patch
	if p.OpCount() != 0 {
		t.Fatalf("OpCount = %d, want 0", p.OpCount())
	}
}

func TestAddValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		op      string
		path    string
		wantErr bool
	}{
		{"valid", "replace", "/name", false},
		{"empty op", "", "/name", true},
		{"empty path", "replace", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var p Patch
			err := p.Add(tc.op, tc.path, "x")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Add(%q,%q) = nil, want error", tc.op, tc.path)
				}
				if p.OpCount() != 0 {
					t.Fatalf("rejected op still appended: OpCount = %d", p.OpCount())
				}
				return
			}
			if err != nil {
				t.Fatalf("Add(%q,%q) = %v, want nil", tc.op, tc.path, err)
			}
			if p.OpCount() != 1 {
				t.Fatalf("OpCount = %d, want 1", p.OpCount())
			}
		})
	}
}

func TestSetMetaMutatesTheAnonymousField(t *testing.T) {
	t.Parallel()

	var p Patch
	p.SetMeta("patch-1", "2024-01-15T10:30:00Z")
	if p.Meta.ID != "patch-1" {
		t.Fatalf("Meta.ID = %q, want patch-1", p.Meta.ID)
	}
	if p.Meta.CreatedAt != "2024-01-15T10:30:00Z" {
		t.Fatalf("Meta.CreatedAt = %q, want 2024-01-15T10:30:00Z", p.Meta.CreatedAt)
	}
}

func TestPatchRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()

	var original Patch
	original.SetMeta("patch-1", "2024-01-15T10:30:00Z")
	if err := original.Add("replace", "/name", "Alice"); err != nil {
		t.Fatal(err)
	}
	if err := original.Add("add", "/tags/0", "go"); err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var got Patch
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Meta.ID != original.Meta.ID {
		t.Fatalf("Meta.ID mismatch: %q vs %q", got.Meta.ID, original.Meta.ID)
	}
	if got.OpCount() != 2 {
		t.Fatalf("OpCount = %d, want 2", got.OpCount())
	}
}

func TestPatchWithMultipleOperationsPreservesOrder(t *testing.T) {
	t.Parallel()

	var p Patch
	for _, path := range []string{"/first", "/second", "/third"} {
		if err := p.Add("replace", path, "x"); err != nil {
			t.Fatal(err)
		}
	}

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	first := strings.Index(s, "/first")
	second := strings.Index(s, "/second")
	third := strings.Index(s, "/third")
	if first < 0 || second < 0 || third < 0 {
		t.Fatalf("a path is missing from the output: %s", s)
	}
	if !(first < second && second < third) {
		t.Fatalf("operations out of order in JSON: %s", s)
	}
}

func ExamplePatch_OpCount() {
	var p Patch
	_ = p.Add("test", "/name", "Alice")
	_ = p.Add("replace", "/name", "Bob")
	fmt.Println(p.OpCount())
	// Output: 2
}
```

## Review

The builder is correct when `Add` preserves order and rejects blank input, when
`SetMeta` mutates through its pointer receiver so the caller sees the change, and
when the anonymous `Meta` field survives a JSON round trip. The most common
mistakes this exercise guards against: giving `Meta` a package-level name it does
not need (anonymous is deliberate here); giving `SetMeta` a value receiver, which
would mutate a copy and lose the write; and treating `Operations` as an unordered
collection, which the ordering test exists to prevent. Run `go test -race` — even
though this type is not itself concurrent, the flag is free insurance and keeps
the module honest as it grows.

## Resources

- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — anonymous struct types and field declarations.
- [encoding/json: Marshal](https://pkg.go.dev/encoding/json#Marshal) — how struct fields and tags map to JSON.
- [RFC 6902: JavaScript Object Notation (JSON) Patch](https://www.rfc-editor.org/rfc/rfc6902) — the operation-list format this builder models.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-responsewriter-status-capture.md](02-responsewriter-status-capture.md)
