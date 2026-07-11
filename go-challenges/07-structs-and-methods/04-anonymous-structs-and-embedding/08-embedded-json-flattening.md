# Exercise 8: Controlling JSON Shape — Promoted Fields vs Nested Structs

The wire shape of an API response is a contract, and embedding quietly decides it
for you: an embedded struct's fields *flatten* into the parent JSON object by
default. This surprises engineers who expected a nested `meta` object. This
exercise makes the three shapes explicit — flatten, nest via a named field, nest
via a tagged embed — so you choose the wire format on purpose, and it pins the
key-collision rule that promotion imposes.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
envelope/                   independent module: example.com/envelope
  go.mod                    module example.com/envelope
  envelope.go               Meta; FlatResponse, NestedResponse, TaggedResponse, Collision
  cmd/
    demo/
      main.go               marshal each variant; print the three wire shapes
  envelope_test.go          golden JSON per variant, round-trip, collision rule
```

Files: `envelope.go`, `cmd/demo/main.go`, `envelope_test.go`.
Implement: a `Meta` struct (`RequestID`, `Version`); a `FlatResponse` embedding
`Meta` untagged (flattens), a `NestedResponse` with a named `Meta` field (nests),
a `TaggedResponse` embedding `Meta` with a `json:"meta"` tag (nests), and a
`Collision` where an embedded field's JSON name clashes with a parent field.
Test: golden-JSON assertions that the flat variant hoists keys to the top level
while the named and tagged variants nest under `meta`; round-trip `Unmarshal`
reconstructs each; the collision resolves to the parent (shallower) field.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/envelope/cmd/demo
cd ~/go-exercises/envelope
go mod init example.com/envelope
```

### The three shapes, and why they differ

`encoding/json` treats an *embedded* struct specially: its exported fields are
promoted, and by default they marshal as if declared on the parent, so they
*flatten* to the top level. That is `FlatResponse`: embedding `Meta` untagged
yields `{"request_id":...,"version":...,"data":...}` with no `meta` wrapper. Two
things override the flattening. A *named* (non-embedded) field always nests under
its key: `NestedResponse` has `Meta Meta json:"meta"`, so its output is
`{"meta":{...},"data":...}`. And an *embedded* field carrying a `json` tag is
treated as if it had that name, which forces nesting: `TaggedResponse` embeds
`Meta` with `json:"meta"` and also produces `{"meta":{...},"data":...}`. So the
same inner type gives three wire shapes, chosen by how you attach it.

The collision rule is the other half. Promotion resolves JSON key clashes by the
same shallowest-depth-wins logic: if a parent field and a promoted field share a
JSON name, the shallower (parent) field wins and the deeper one is *omitted* from
the output. `Collision` embeds `Meta` (whose `Version` has JSON name `version`)
and also declares its own `Version string json:"version"` at depth 0; the parent
string wins, and `Meta`'s int `version` never appears. Knowing this prevents the
bug where an embedded field silently shadows or is shadowed on the wire.

Create `envelope.go`:

```go
package envelope

// Meta is a small metadata block reused across responses.
type Meta struct {
	RequestID string `json:"request_id"`
	Version   int    `json:"version"`
}

// FlatResponse embeds Meta untagged: its fields FLATTEN to the top level.
type FlatResponse struct {
	Meta
	Data string `json:"data"`
}

// NestedResponse uses a NAMED field: Meta NESTS under "meta".
type NestedResponse struct {
	Meta Meta   `json:"meta"`
	Data string `json:"data"`
}

// TaggedResponse embeds Meta but with a json tag, which forces NESTING.
type TaggedResponse struct {
	Meta `json:"meta"`
	Data string `json:"data"`
}

// Collision embeds Meta and also declares its own version at depth 0. The
// shallower parent field wins the "version" key; Meta.Version is omitted.
type Collision struct {
	Meta
	Version string `json:"version"`
}
```

### The runnable demo

The demo marshals one value of each variant so the three wire shapes are visible
side by side.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/envelope"
)

func main() {
	m := envelope.Meta{RequestID: "r1", Version: 2}

	flat, _ := json.Marshal(envelope.FlatResponse{Meta: m, Data: "hello"})
	nested, _ := json.Marshal(envelope.NestedResponse{Meta: m, Data: "hello"})
	tagged, _ := json.Marshal(envelope.TaggedResponse{Meta: m, Data: "hello"})

	fmt.Printf("flat:   %s\n", flat)
	fmt.Printf("nested: %s\n", nested)
	fmt.Printf("tagged: %s\n", tagged)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flat:   {"request_id":"r1","version":2,"data":"hello"}
nested: {"meta":{"request_id":"r1","version":2},"data":"hello"}
tagged: {"meta":{"request_id":"r1","version":2},"data":"hello"}
```

### Tests

The golden-JSON tests pin each wire shape exactly; round-trip tests prove each
variant decodes back; the collision test reads the emitted `version` from a
generic map to prove the parent (string) field won.

Create `envelope_test.go`:

```go
package envelope

import (
	"encoding/json"
	"testing"
)

func TestFlatHoistsKeysToTopLevel(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(FlatResponse{Meta: Meta{RequestID: "r1", Version: 2}, Data: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"request_id":"r1","version":2,"data":"hello"}`
	if string(data) != want {
		t.Fatalf("flat JSON = %s, want %s", data, want)
	}
}

func TestNamedFieldNests(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(NestedResponse{Meta: Meta{RequestID: "r1", Version: 2}, Data: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"meta":{"request_id":"r1","version":2},"data":"hello"}`
	if string(data) != want {
		t.Fatalf("nested JSON = %s, want %s", data, want)
	}
}

func TestTaggedEmbedNests(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(TaggedResponse{Meta: Meta{RequestID: "r1", Version: 2}, Data: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"meta":{"request_id":"r1","version":2},"data":"hello"}`
	if string(data) != want {
		t.Fatalf("tagged JSON = %s, want %s", data, want)
	}
}

func TestRoundTripReconstructsEachVariant(t *testing.T) {
	t.Parallel()

	m := Meta{RequestID: "r1", Version: 2}

	var flat FlatResponse
	b, _ := json.Marshal(FlatResponse{Meta: m, Data: "hello"})
	if err := json.Unmarshal(b, &flat); err != nil {
		t.Fatal(err)
	}
	if flat.RequestID != "r1" || flat.Data != "hello" {
		t.Fatalf("flat round-trip lost data: %+v", flat)
	}

	var nested NestedResponse
	b, _ = json.Marshal(NestedResponse{Meta: m, Data: "hello"})
	if err := json.Unmarshal(b, &nested); err != nil {
		t.Fatal(err)
	}
	if nested.Meta.RequestID != "r1" || nested.Data != "hello" {
		t.Fatalf("nested round-trip lost data: %+v", nested)
	}
}

func TestCollisionParentWins(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(Collision{
		Meta:    Meta{RequestID: "r1", Version: 2},
		Version: "v9",
	})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	// The parent's string "v9" wins the "version" key; Meta.Version (int 2) is
	// omitted because it is at a deeper embedding level.
	if out["version"] != "v9" {
		t.Fatalf("version = %v, want v9 (parent field wins)", out["version"])
	}
	if out["request_id"] != "r1" {
		t.Fatalf("request_id = %v, want r1", out["request_id"])
	}
}
```

## Review

The variants are correct when the flat one hoists `request_id`/`version` to the
top level and the named and tagged ones nest them under `meta`, and when a
key collision resolves to the shallower parent field. The central lesson: the wire
shape is a design decision, not an accident of embedding. The mistakes to avoid:
embedding a metadata struct and being surprised the keys flattened (add a tag or
use a named field to nest); assuming both colliding fields appear in the output
(only the shallower one does); and letting the wire contract drift because a
refactor turned a named field into an embed or vice versa — the golden tests exist
to catch exactly that.

## Resources

- [encoding/json: Marshal](https://pkg.go.dev/encoding/json#Marshal) — anonymous (embedded) struct-field marshaling, tags, and the flattening rules.
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — embedded fields and JSON tag interaction.
- [JSON and Go](https://go.dev/blog/json) — the Go blog's tour of struct/JSON mapping.

---

Prev: [07-ambiguous-embedded-selector.md](07-ambiguous-embedded-selector.md) | Back to [00-concepts.md](00-concepts.md) | Next: [09-anonymous-struct-webhook-handler.md](09-anonymous-struct-webhook-handler.md)
