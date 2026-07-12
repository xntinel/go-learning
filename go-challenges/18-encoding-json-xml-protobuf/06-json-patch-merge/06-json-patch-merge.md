# 6. JSON Patch and Merge

Partial JSON updates are easy to describe and easy to implement incorrectly. This lesson builds a `jsonpatch` library that implements JSON Merge Patch plus a small top-level JSON Patch subset, with tests that prove deletion, replacement, invalid paths, and missing keys.

## Concepts

### Merge Patch Is an Object

JSON Merge Patch represents the desired changes as an object. A present key replaces the target value, a nested object recurses into a nested object, and a `null` value deletes the key. That means Merge Patch cannot distinguish "set this value to JSON null" from "delete this key".

### JSON Patch Is an Operation List

JSON Patch is an array of operations such as `add`, `remove`, and `replace`. This lesson supports top-level paths like `/name` only. Rejecting nested paths keeps the contract honest until full JSON Pointer support is implemented.

### Dynamic JSON Still Needs Typed Errors

`map[string]any` is appropriate for dynamic object updates, but errors should still be typed. Sentinel errors wrapped with `%w` let a caller separate malformed JSON, unsupported operations, missing keys, and invalid paths.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/18-encoding-json-xml-protobuf/06-json-patch-merge/06-json-patch-merge/cmd/demo
cd go-solutions/18-encoding-json-xml-protobuf/06-json-patch-merge/06-json-patch-merge
go mod edit -go=1.26
```

### Exercise 1: Implement Merge Patch and Top-Level Patch Operations

Create `patch.go`:

```go
package jsonpatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrInvalidJSON          = errors.New("invalid json")
	ErrInvalidPath          = errors.New("invalid path")
	ErrMissingKey           = errors.New("missing key")
	ErrUnsupportedOperation = errors.New("unsupported operation")
)

type Document struct {
	data map[string]any
}

type Operation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

func NewDocument(data map[string]any) Document { return Document{data: cloneMap(data)} }
func (d Document) Value(key string) any        { return d.data[key] }
func (d Document) Map() map[string]any         { return cloneMap(d.data) }
func (d Document) JSON() ([]byte, error)       { return json.Marshal(d.data) }

func MergePatchJSON(original, patch []byte) (Document, error) {
	var base map[string]any
	if err := json.Unmarshal(original, &base); err != nil {
		return Document{}, fmt.Errorf("%w: original: %v", ErrInvalidJSON, err)
	}
	var delta map[string]any
	if err := json.Unmarshal(patch, &delta); err != nil {
		return Document{}, fmt.Errorf("%w: patch: %v", ErrInvalidJSON, err)
	}
	return NewDocument(mergePatch(base, delta)), nil
}

func ApplyPatchJSON(original, patch []byte) (Document, error) {
	var doc map[string]any
	if err := json.Unmarshal(original, &doc); err != nil {
		return Document{}, fmt.Errorf("%w: document: %v", ErrInvalidJSON, err)
	}
	var ops []Operation
	if err := json.Unmarshal(patch, &ops); err != nil {
		return Document{}, fmt.Errorf("%w: patch: %v", ErrInvalidJSON, err)
	}
	for _, op := range ops {
		key, err := topLevelKey(op.Path)
		if err != nil {
			return Document{}, err
		}
		switch op.Op {
		case "add":
			doc[key] = op.Value
		case "remove":
			if _, ok := doc[key]; !ok {
				return Document{}, fmt.Errorf("%w: %s", ErrMissingKey, key)
			}
			delete(doc, key)
		case "replace":
			if _, ok := doc[key]; !ok {
				return Document{}, fmt.Errorf("%w: %s", ErrMissingKey, key)
			}
			doc[key] = op.Value
		default:
			return Document{}, fmt.Errorf("%w: %s", ErrUnsupportedOperation, op.Op)
		}
	}
	return NewDocument(doc), nil
}

func mergePatch(base, patch map[string]any) map[string]any {
	for key, value := range patch {
		if value == nil {
			delete(base, key)
			continue
		}
		patchObject, patchOK := value.(map[string]any)
		baseObject, baseOK := base[key].(map[string]any)
		if patchOK && baseOK {
			base[key] = mergePatch(baseObject, patchObject)
			continue
		}
		base[key] = value
	}
	return base
}

func topLevelKey(path string) (string, error) {
	if !strings.HasPrefix(path, "/") || strings.Count(path, "/") != 1 || len(path) == 1 {
		return "", fmt.Errorf("%w: %s", ErrInvalidPath, path)
	}
	return strings.TrimPrefix(path, "/"), nil
}

func cloneMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for key, value := range src {
		if nested, ok := value.(map[string]any); ok {
			dst[key] = cloneMap(nested)
			continue
		}
		dst[key] = value
	}
	return dst
}
```

### Exercise 2: Test the Contract

Create `patch_test.go`:

```go
package jsonpatch

import (
	"errors"
	"fmt"
	"testing"
)

func TestMergePatchJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		original string
		patch    string
		wantKey  string
		want     any
		wantGone string
		wantErr  error
	}{
		{name: "merge nested object and delete key", original: `{"name":"Server-1","config":{"cpu":4,"memory":8},"status":"running"}`, patch: `{"config":{"memory":16,"gpu":1},"status":null,"owner":"ops-team"}`, wantKey: "owner", want: "ops-team", wantGone: "status"},
		{name: "invalid patch json", original: `{"name":"Server-1"}`, patch: `{`, wantErr: ErrInvalidJSON},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := MergePatchJSON([]byte(tt.original), []byte(tt.patch))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("MergePatchJSON() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Value(tt.wantKey) != tt.want {
				t.Fatalf("Value(%q) = %v, want %v", tt.wantKey, got.Value(tt.wantKey), tt.want)
			}
			if _, ok := got.Map()[tt.wantGone]; ok {
				t.Fatalf("Map()[%q] exists, want deleted", tt.wantGone)
			}
		})
	}
}

func TestApplyPatchJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		document string
		patch    string
		wantKey  string
		want     any
		wantGone string
		wantErr  error
	}{
		{name: "apply add remove replace", document: `{"name":"App","version":"1.0","debug":true}`, patch: `[{"op":"replace","path":"/version","value":"2.0"},{"op":"add","path":"/author","value":"team"},{"op":"remove","path":"/debug"}]`, wantKey: "version", want: "2.0", wantGone: "debug"},
		{name: "unsupported operation", document: `{"name":"App"}`, patch: `[{"op":"copy","path":"/name"}]`, wantErr: ErrUnsupportedOperation},
		{name: "missing remove key", document: `{"name":"App"}`, patch: `[{"op":"remove","path":"/debug"}]`, wantErr: ErrMissingKey},
		{name: "nested path rejected", document: `{"name":"App"}`, patch: `[{"op":"add","path":"/config/debug","value":true}]`, wantErr: ErrInvalidPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ApplyPatchJSON([]byte(tt.document), []byte(tt.patch))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ApplyPatchJSON() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Value(tt.wantKey) != tt.want {
				t.Fatalf("Value(%q) = %v, want %v", tt.wantKey, got.Value(tt.wantKey), tt.want)
			}
			if _, ok := got.Map()[tt.wantGone]; ok {
				t.Fatalf("Map()[%q] exists, want deleted", tt.wantGone)
			}
		})
	}
}

func ExampleApplyPatchJSON() {
	doc, _ := ApplyPatchJSON([]byte(`{"name":"App","version":"1.0","debug":true}`), []byte(`[{"op":"replace","path":"/version","value":"2.0"},{"op":"remove","path":"/debug"}]`))
	out, _ := doc.JSON()
	fmt.Println(string(out))
	// Output:
	// {"name":"App","version":"2.0"}
}
```

Your turn: add a test proving `replace` on a missing key returns `ErrMissingKey`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/jsonpatch"
)

func main() {
	doc, err := jsonpatch.ApplyPatchJSON([]byte(`{"name":"App","version":"1.0","debug":true}`), []byte(`[{"op":"replace","path":"/version","value":"2.0"},{"op":"add","path":"/author","value":"team"},{"op":"remove","path":"/debug"}]`))
	if err != nil {
		log.Fatal(err)
	}
	out, err := doc.JSON()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(out))
}
```

## Common Mistakes

- Wrong: treating JSON Merge Patch and JSON Patch as the same format. What happens: operation arrays and merge objects get decoded into the wrong shape. Fix: use separate entry points.
- Wrong: treating `null` in a merge patch as a value. What happens: deletes are not applied. Fix: delete when the patch map value is `nil`.
- Wrong: accepting nested paths while only implementing `strings.TrimPrefix`. What happens: `/a/b` becomes the literal key `a/b`. Fix: reject unsupported paths until full JSON Pointer is implemented.

## Verification

From `~/go-exercises/jsonpatch`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All commands must pass. Add at least one test of your own before considering the lesson complete.

## Summary

- JSON Merge Patch is object-shaped and uses `null` as delete.
- JSON Patch is operation-shaped and has explicit `add`, `remove`, and `replace` operations.
- Dynamic JSON maps still need explicit error contracts.
- Rejecting unsupported path syntax is safer than pretending to support it.

## What's Next

Next: [XML Encoding and Decoding](../07-xml-encoding-decoding/07-xml-encoding-decoding.md).

## Resources

- [encoding/json package documentation](https://pkg.go.dev/encoding/json)
- [RFC 7396: JSON Merge Patch](https://www.rfc-editor.org/rfc/rfc7396)
- [RFC 6902: JSON Patch](https://www.rfc-editor.org/rfc/rfc6902)
- [errors.Is documentation](https://pkg.go.dev/errors#Is)
