# Exercise 6: Sharing a BaseModel Across Entities With JSON Promotion

Domain models in a real service usually share a common core — an id and audit
timestamps — that you factor into a `BaseModel` and embed. Embedding makes those
fields promote in Go and, crucially, *flatten* in JSON. This exercise pins the
flattening, the collision rules, and how to force nesting when you don't want it.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
models/                    independent module: example.com/models
  go.mod                   go 1.26
  models.go                BaseModel embedded into User/Order; Event shadows id; AuditedUser nests
  cmd/
    demo/
      main.go              runnable demo: marshal a User, show flattened JSON
  models_test.go           flattening, shadowing (outer wins), forced nesting, round-trip
```

- Files: `models.go`, `cmd/demo/main.go`, `models_test.go`.
- Implement: `BaseModel{ID, CreatedAt, UpdatedAt}` embedded into `User` and `Order`; an `Event` that redeclares `ID`; an `AuditedUser` that uses a named `BaseModel` field to force nesting.
- Test: marshalled `User` has `id`/`created_at` at the top level (flattened); the redeclared field wins (shadowing); the named-field type nests; a round-trip `Unmarshal` repopulates embedded fields.
- Verify: `go test -count=1 -race ./...`

### Flattening, shadowing, and forced nesting

`encoding/json` treats an embedded struct's exported fields as if they were
declared directly on the outer struct. Embed `BaseModel` into `User` and the
marshalled JSON is one flat object — `{"id":...,"created_at":...,"updated_at":...,"email":...}` —
not a nested `{"base":{...},"email":...}`. The `json` tags on `BaseModel` apply, so
`ID string` with tag `json:"id"` serializes as `"id"` at the top level of every
type that embeds `BaseModel`. This is usually what you want for domain models:
every entity carries `id`/`created_at`/`updated_at` at the object root, matching a
typical API envelope, with no per-type boilerplate.

Collisions follow Go's shadowing rules, applied to JSON names. If the outer type
declares a field that serializes to a name an embedded field also uses, the
shallower (outer) field wins and the embedded one is dropped from the output — no
error, no duplicate key, the deeper one is simply suppressed. `Event` here embeds
`BaseModel` (which has `ID` tagged `id`) and also declares its own `ID string`
tagged `id`; marshalling an `Event` emits the outer `ID`, and the embedded
`BaseModel.ID` does not appear. This is `encoding/json` honoring the same
depth-based shadowing the language uses for selectors.

When flattening is *not* what you want — you want the shared core as a nested
sub-object — do not embed. Use a *named* field with a `json` tag: `Meta BaseModel `json:"meta"`` 
serializes as `{"meta":{"id":...},"email":...}`. The distinction is exactly
embedding vs a named field: embed to flatten and promote, name to nest and keep
separate. `AuditedUser` shows the nested form.

Create `models.go`:

```go
package models

import "time"

// BaseModel is the shared core every entity carries. Embedded, its fields flatten
// into the parent JSON object and promote onto the parent type.
type BaseModel struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// User embeds BaseModel: id/created_at/updated_at flatten to the top level.
type User struct {
	BaseModel
	Email string `json:"email"`
	Name  string `json:"name"`
}

// Order also embeds BaseModel, reusing the same flattened core.
type Order struct {
	BaseModel
	TotalCents int    `json:"total_cents"`
	Currency   string `json:"currency"`
}

// Event embeds BaseModel but also declares its own ID at a shallower depth. The
// outer ID shadows BaseModel.ID; marshalling emits only the outer one.
type Event struct {
	BaseModel
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// AuditedUser uses a NAMED BaseModel field instead of embedding, forcing the
// shared core to serialize as a nested "meta" object rather than flattening.
type AuditedUser struct {
	Meta  BaseModel `json:"meta"`
	Email string    `json:"email"`
}
```

### The runnable demo

The demo marshals a `User` built with a fixed timestamp so the JSON is
deterministic, showing the flattened shape.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"time"

	"example.com/models"
)

func main() {
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	u := models.User{
		BaseModel: models.BaseModel{ID: "u_1", CreatedAt: ts, UpdatedAt: ts},
		Email:     "alice@example.com",
		Name:      "Alice",
	}
	b, err := json.Marshal(u)
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
{"id":"u_1","created_at":"2024-01-02T03:04:05Z","updated_at":"2024-01-02T03:04:05Z","email":"alice@example.com","name":"Alice"}
```

### Tests

`TestUserFlattens` marshals a `User` and asserts `id`/`created_at`/`email` sit at
the top level and there is no nested `basemodel` key. `TestEventShadowing` sets a
distinct value on the embedded `BaseModel.ID` and the outer `Event.ID`, marshals,
and asserts only the outer value appears — proving JSON honors shadowing.
`TestAuditedUserNests` asserts the named-field type produces a nested `meta`
object. `TestRoundTrip` marshals a `User` and unmarshals it back, asserting the
embedded fields repopulate.

Create `models_test.go`:

```go
package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUserFlattens(t *testing.T) {
	t.Parallel()
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	u := User{
		BaseModel: BaseModel{ID: "u_1", CreatedAt: ts, UpdatedAt: ts},
		Email:     "a@b.com",
		Name:      "A",
	}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, key := range []string{`"id":"u_1"`, `"created_at":`, `"email":"a@b.com"`} {
		if !strings.Contains(got, key) {
			t.Errorf("flattened JSON missing %s; got %s", key, got)
		}
	}
	if strings.Contains(got, `"basemodel"`) || strings.Contains(got, `"BaseModel"`) {
		t.Errorf("embedded fields should flatten, not nest; got %s", got)
	}
}

func TestEventShadowing(t *testing.T) {
	t.Parallel()
	e := Event{
		BaseModel: BaseModel{ID: "base-id"},
		ID:        "outer-id",
		Kind:      "created",
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, `"id":"outer-id"`) {
		t.Errorf("outer ID should win the id key; got %s", got)
	}
	if strings.Contains(got, "base-id") {
		t.Errorf("shadowed BaseModel.ID should not serialize; got %s", got)
	}
}

func TestAuditedUserNests(t *testing.T) {
	t.Parallel()
	au := AuditedUser{
		Meta:  BaseModel{ID: "u_9"},
		Email: "c@d.com",
	}
	b, err := json.Marshal(au)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, `"meta":{`) {
		t.Errorf("named field should nest under meta; got %s", got)
	}
	if !strings.Contains(got, `"id":"u_9"`) {
		t.Errorf("nested id missing; got %s", got)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	ts := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	in := User{
		BaseModel: BaseModel{ID: "u_7", CreatedAt: ts, UpdatedAt: ts},
		Email:     "e@f.com",
		Name:      "E",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out User
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "u_7" {
		t.Errorf("embedded ID did not round-trip: got %q", out.ID)
	}
	if !out.CreatedAt.Equal(ts) {
		t.Errorf("embedded CreatedAt did not round-trip: got %v", out.CreatedAt)
	}
	if out.Email != "e@f.com" {
		t.Errorf("Email did not round-trip: got %q", out.Email)
	}
}
```

## Review

The models are correct when the shared core serializes exactly where you intend:
flat at the object root for the embedded `BaseModel`, nested under `meta` for the
named field. `TestUserFlattens` and `TestAuditedUserNests` are the two poles of the
embedding-vs-named-field choice, and `TestEventShadowing` proves that a name
collision resolves silently in favor of the outer field — which is convenient when
deliberate and a data-loss bug when accidental, because the shadowed field just
disappears from the payload with no error. Round-tripping confirms `Unmarshal`
fills the embedded fields the same way `Marshal` flattens them. When two entities
share a core, embed it; when you want it namespaced, name it.

## Resources

- [`encoding/json.Marshal`](https://pkg.go.dev/encoding/json#Marshal) — the documented rule that anonymous (embedded) struct fields are marshalled as if their fields were part of the outer struct.
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — embedded fields and the promotion/shadowing rules JSON mirrors.
- [`time.Time` JSON format](https://pkg.go.dev/time#Time.MarshalJSON) — the RFC 3339 encoding used for the timestamps.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-interface-embedding-decorator.md](07-interface-embedding-decorator.md)
