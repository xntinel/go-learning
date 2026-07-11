# Exercise 4: PATCH Semantics — Pointer Fields to Tell Absent from Explicit Zero

A partial-update endpoint has to answer a question value types cannot: did the
client omit this field, or set it to its zero value? This module builds an
`UpdateUserRequest` DTO with `*string`/`*int`/`*bool` fields decoded from JSON and
an `Apply` method that writes only the fields the client actually sent, so an
omitted `name` is left alone while an explicit `""` overwrites.

This module is fully self-contained: its own `go mod init`, its own types, its own
demo, and its own tests. Nothing here imports another exercise.

## What you'll build

```text
patch/                      independent module: example.com/patch
  go.mod                    go 1.25
  patch.go                  User; UpdateUserRequest (pointer fields); Apply(dst *User)
  cmd/
    demo/
      main.go               decode three PATCH bodies and show what each changes
  patch_test.go             absent vs explicit-zero vs full-body table tests
```

- Files: `patch.go`, `cmd/demo/main.go`, `patch_test.go`.
- Implement: `UpdateUserRequest` with `Name *string`, `Age *int`, `Active *bool`; `Apply(dst *User)` that writes a field only when its pointer is non-nil.
- Test: decode `{}` and assert `Apply` leaves the target untouched; decode `{"name":""}` and assert `Name` is written to `""`; decode `{"active":false}` and assert `false` is applied (where a value field could not tell it from absent); decode a full body and assert every field updated.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/patch/cmd/demo
cd ~/go-exercises/patch
go mod init example.com/patch
go mod edit -go=1.25
```

### Why value fields cannot express PATCH

PATCH is a partial update: the client sends only the fields it wants to change,
and the server leaves everything else as-is. Model the request with value fields —
`Name string`, `Active bool` — and decoding `{}` gives you a struct full of zero
values, indistinguishable from a client that sent `{"name":"","active":false}`.
You literally cannot tell "leave name unchanged" from "set name to empty." Apply
that struct and you clobber every field the client never mentioned. This is one of
the most common real PATCH bugs: a client updates a user's email and accidentally
blanks their name, because the value-typed DTO could not represent absence.

A pointer field carries the third state. `Name *string` is `nil` when the JSON had
no `"name"` key, and non-nil when it did — even if the value was `""`. That is the
tri-state from the concepts file made concrete: `nil` = absent, non-nil = present
(possibly zero). `encoding/json` gives you this for free: unmarshaling into a
`*string` leaves it `nil` when the key is missing and allocates a pointer to the
decoded value when the key is present.

### Apply writes only non-nil fields

`Apply(dst *User)` walks each pointer field and writes to `dst` only when the
pointer is non-nil, dereferencing to get the value. An omitted field's pointer is
`nil`, so its `dst` field is left untouched; an explicit field's pointer is
non-nil, so its value — zero or not — is written. The `*bool Active` field is the
sharpest case: a value `bool` could never distinguish "deactivate" (`false`
explicitly) from "no change," but `*bool` nails it — `nil` leaves the flag,
`&false` clears it.

Create `patch.go`:

```go
package patch

// User is the stored record a PATCH updates in place.
type User struct {
	Name   string
	Age    int
	Active bool
}

// UpdateUserRequest is a partial-update DTO. A nil field means "absent, leave
// unchanged"; a non-nil field means "explicitly set to this value (even zero)".
type UpdateUserRequest struct {
	Name   *string `json:"name,omitempty"`
	Age    *int    `json:"age,omitempty"`
	Active *bool   `json:"active,omitempty"`
}

// Apply writes only the fields the client actually sent (non-nil pointers) onto
// dst, leaving omitted fields unchanged.
func (r UpdateUserRequest) Apply(dst *User) {
	if r.Name != nil {
		dst.Name = *r.Name
	}
	if r.Age != nil {
		dst.Age = *r.Age
	}
	if r.Active != nil {
		dst.Active = *r.Active
	}
}
```

### The runnable demo

The demo decodes three bodies against a seeded user: an empty body (nothing
changes), an explicit-empty name (name is cleared), and an explicit `active:false`
(the flag is cleared) — each starting from the same seed so the difference is
visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/patch"
)

func apply(body string) patch.User {
	u := patch.User{Name: "alice", Age: 30, Active: true}
	var req patch.UpdateUserRequest
	_ = json.Unmarshal([]byte(body), &req)
	req.Apply(&u)
	return u
}

func main() {
	fmt.Printf("empty body:      %+v\n", apply(`{}`))
	fmt.Printf("explicit name:   %+v\n", apply(`{"name":""}`))
	fmt.Printf("explicit active: %+v\n", apply(`{"active":false}`))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
empty body:      {Name:alice Age:30 Active:true}
explicit name:   {Name: Age:30 Active:true}
explicit active: {Name:alice Age:30 Active:false}
```

### Tests

The table pins the exact behavior that value fields cannot deliver. The `{}` row
asserts the target is byte-for-byte unchanged. The `{"name":""}` row asserts the
name is written to `""` — proving an explicit zero is applied. The
`{"active":false}` row is the decisive one: it asserts `false` is applied to a
field that started `true`, which a value-typed DTO could never distinguish from an
omitted field. The full-body row asserts every field updates.

Create `patch_test.go`:

```go
package patch

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestApply(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want User
	}{
		{
			name: "empty body leaves target unchanged",
			body: `{}`,
			want: User{Name: "alice", Age: 30, Active: true},
		},
		{
			name: "explicit empty name is applied",
			body: `{"name":""}`,
			want: User{Name: "", Age: 30, Active: true},
		},
		{
			name: "explicit false active is applied",
			body: `{"active":false}`,
			want: User{Name: "alice", Age: 30, Active: false},
		},
		{
			name: "explicit zero age is applied",
			body: `{"age":0}`,
			want: User{Name: "alice", Age: 0, Active: true},
		},
		{
			name: "full body updates all fields",
			body: `{"name":"bob","age":41,"active":false}`,
			want: User{Name: "bob", Age: 41, Active: false},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := User{Name: "alice", Age: 30, Active: true}
			var req UpdateUserRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatal(err)
			}
			req.Apply(&u)
			if u != tc.want {
				t.Fatalf("Apply() = %+v, want %+v", u, tc.want)
			}
		})
	}
}

func TestOmittedFieldPointerIsNil(t *testing.T) {
	t.Parallel()
	var req UpdateUserRequest
	if err := json.Unmarshal([]byte(`{"name":"x"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Name == nil {
		t.Fatal("Name pointer = nil for a present field, want non-nil")
	}
	if req.Age != nil || req.Active != nil {
		t.Fatal("omitted fields should decode to nil pointers")
	}
}

func ExampleUpdateUserRequest_Apply() {
	u := User{Name: "alice", Age: 30, Active: true}
	var req UpdateUserRequest
	_ = json.Unmarshal([]byte(`{"active":false}`), &req)
	req.Apply(&u)
	fmt.Printf("%+v\n", u)
	// Output: {Name:alice Age:30 Active:false}
}
```

## Review

The DTO is correct when `Apply` changes exactly the fields whose JSON keys were
present, leaving omitted fields untouched — verified by the `{}` row (nothing
changes) and the explicit-zero rows (`""`, `0`, `false` all applied). The whole
reason for the pointer fields is the `active:false` case: it is the one a
value-typed DTO cannot represent, so the test that asserts `false` overwrites
`true` is the load-bearing one. The mistake to avoid is modeling optional fields
with value types "for simplicity," which silently converts every PATCH into a
full-replace of the zero-valued fields. A secondary trap is forgetting that
`omitempty` on the *pointer* is about the encode side; on decode, absence is
signaled by the pointer staying `nil` regardless of tags.

## Resources

- [`encoding/json.Unmarshal`](https://pkg.go.dev/encoding/json#Unmarshal) — how a missing key leaves a pointer field nil.
- [JSON and Go](https://go.dev/blog/json) — decoding into pointer fields.
- [RFC 5789: PATCH method for HTTP](https://www.rfc-editor.org/rfc/rfc5789) — the partial-update semantics this models.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-defensive-copy-break-aliasing.md](03-defensive-copy-break-aliasing.md) | Next: [05-functional-options-constructor.md](05-functional-options-constructor.md)
