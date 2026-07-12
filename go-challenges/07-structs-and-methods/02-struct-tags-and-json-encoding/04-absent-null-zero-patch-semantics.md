# Exercise 4: Distinguish Absent vs null vs Zero in a PATCH Handler

A `PATCH` is not a `PUT`: it carries only the fields the client wants to change,
and "change" has three meanings — set to a value, clear to null, or (implicitly)
leave alone by omitting the field. A patch struct built from plain value fields
collapses all three into one zero value and silently clobbers stored data. This
module builds a patch type that preserves the distinction with pointer fields and
`json.RawMessage`, then applies it to an entity correctly.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
patchsem/                      independent module: example.com/patchsem
  go.mod                       go 1.24
  patch/
    patch.go                   User, UserPatch (*string, *bool, json.RawMessage), Apply
  cmd/
    demo/
      main.go                  apply three patches: set, clear, leave-unchanged
  patch/patch_test.go          absent/null/value across two fields, tabular
```

Files: `patch/patch.go`, `cmd/demo/main.go`, `patch/patch_test.go`.
Implement: `UserPatch` with `Name *string`, `Active *bool`, and `Bio json.RawMessage`, plus `Apply(*User) error` honoring absent/null/value.
Test: absent leaves the field unchanged (nil pointer / nil raw), null clears (`bytes.Equal(raw, []byte("null"))`), a concrete value is applied; a table over both fields for all states.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

## Why value fields clobber data

Consider the wrong design first: `type UserPatch struct { Active bool json:"active,omitempty" }`.
A client that sends `{"name":"..."}` and never mentions `active` decodes to
`Active=false`. A client that sends `{"active":false}` also decodes to
`Active=false`. The handler cannot tell "leave active as it is" from "set active to
false", so applying the patch overwrites a stored `true` with `false` for a client
that never touched the field. That is a data-clobbering bug, and it is invisible
until a customer complains that something got disabled on its own.

The fix is a field type with more than one "empty" representation:

- A **pointer** (`*bool`, `*string`) is `nil` when the key is absent and non-nil
  when the client sent a concrete value. That distinguishes absent from set, which
  is enough for the common two-state case where "clear" equals the zero value
  anyway.
- A **`json.RawMessage`** captures the exact bytes the client sent and gives you
  the full three states: it is `nil` when the key is absent, `[]byte("null")` when
  the client sent an explicit `null`, and the encoded value otherwise. This is what
  you need when "clear this field" is a distinct intent from "set it to its zero
  value" — for example clearing an optional `Bio` back to empty versus leaving it
  untouched.

`Apply` then reads intent from the representation: skip a `nil` field (unchanged),
treat `[]byte("null")` as clear, and otherwise decode and set. The `omitempty` tags
on the patch fields matter only for *re-encoding* a patch; on decode, an absent key
simply leaves the pointer `nil` and the raw message `nil`.

Create `patch/patch.go`:

```go
// patch/patch.go
package patch

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// User is the stored entity a patch mutates.
type User struct {
	Name   string
	Active bool
	Bio    string
}

// UserPatch is a partial update. Each field encodes intent by representation:
//
//	Name/Active (pointer): nil = absent (unchanged); non-nil = set to the value.
//	Bio (RawMessage):      nil = absent (unchanged); "null" = clear to "";
//	                       otherwise = set to the decoded string.
type UserPatch struct {
	Name   *string         `json:"name,omitempty"`
	Active *bool           `json:"active,omitempty"`
	Bio    json.RawMessage `json:"bio,omitempty"`
}

// Apply mutates u in place according to the patch's intent.
func (p UserPatch) Apply(u *User) error {
	if p.Name != nil {
		u.Name = *p.Name
	}
	if p.Active != nil {
		u.Active = *p.Active
	}
	if p.Bio != nil {
		if bytes.Equal(p.Bio, []byte("null")) {
			u.Bio = "" // explicit clear
		} else {
			var s string
			if err := json.Unmarshal(p.Bio, &s); err != nil {
				return fmt.Errorf("patch bio: %w", err)
			}
			u.Bio = s
		}
	}
	return nil
}
```

## The runnable demo

The demo starts from a fully-populated user and applies three patches: one that
sets a field, one that clears `bio` with an explicit null, and one that touches
nothing (leaving every field intact).

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/patchsem/patch"
)

func apply(u patch.User, body string) patch.User {
	var p patch.UserPatch
	_ = json.Unmarshal([]byte(body), &p)
	_ = p.Apply(&u)
	return u
}

func main() {
	base := patch.User{Name: "Alice", Active: true, Bio: "hello"}

	fmt.Printf("set active=false: %+v\n", apply(base, `{"active":false}`))
	fmt.Printf("clear bio (null): %+v\n", apply(base, `{"bio":null}`))
	fmt.Printf("empty patch:      %+v\n", apply(base, `{}`))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
set active=false: {Name:Alice Active:false Bio:hello}
clear bio (null): {Name:Alice Active:true Bio:}
empty patch:      {Name:Alice Active:true Bio:hello}
```

The empty patch changes nothing — the property a value-field design would break.

## Tests

`TestAbsentLeavesFieldsUnchanged` decodes `{}` and asserts every pointer is `nil`
and applying it is a no-op. `TestNullClearsBio` decodes `{"bio":null}` and asserts
`bytes.Equal(p.Bio, []byte("null"))` and that apply clears the field.
`TestValueIsApplied` sets each field. `TestPatchMatrix` is the table asserting the
resulting entity across absent/null/value for the two interesting fields.

Create `patch/patch_test.go`:

```go
// patch/patch_test.go
package patch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func decode(t *testing.T, body string) UserPatch {
	t.Helper()
	var p UserPatch
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	return p
}

func TestAbsentLeavesFieldsUnchanged(t *testing.T) {
	t.Parallel()
	p := decode(t, `{}`)
	if p.Name != nil || p.Active != nil || p.Bio != nil {
		t.Fatalf("absent fields should be nil, got %+v", p)
	}
	u := User{Name: "Alice", Active: true, Bio: "hi"}
	if err := p.Apply(&u); err != nil {
		t.Fatal(err)
	}
	if u.Name != "Alice" || !u.Active || u.Bio != "hi" {
		t.Fatalf("empty patch mutated the entity: %+v", u)
	}
}

func TestNullClearsBio(t *testing.T) {
	t.Parallel()
	p := decode(t, `{"bio":null}`)
	if !bytes.Equal(p.Bio, []byte("null")) {
		t.Fatalf("expected raw null, got %q", p.Bio)
	}
	u := User{Bio: "old"}
	if err := p.Apply(&u); err != nil {
		t.Fatal(err)
	}
	if u.Bio != "" {
		t.Fatalf("null should clear bio, got %q", u.Bio)
	}
}

func TestValueIsApplied(t *testing.T) {
	t.Parallel()
	p := decode(t, `{"name":"Bob","active":false,"bio":"new"}`)
	u := User{Name: "Alice", Active: true, Bio: "old"}
	if err := p.Apply(&u); err != nil {
		t.Fatal(err)
	}
	if u.Name != "Bob" || u.Active || u.Bio != "new" {
		t.Fatalf("value patch not applied: %+v", u)
	}
}

func TestPatchMatrix(t *testing.T) {
	t.Parallel()
	base := User{Active: true, Bio: "orig"}
	cases := []struct {
		name string
		body string
		want User
	}{
		{"absent both", `{}`, User{Active: true, Bio: "orig"}},
		{"set active false", `{"active":false}`, User{Active: false, Bio: "orig"}},
		{"clear bio", `{"bio":null}`, User{Active: true, Bio: ""}},
		{"set bio", `{"bio":"fresh"}`, User{Active: true, Bio: "fresh"}},
		{"set both", `{"active":false,"bio":"x"}`, User{Active: false, Bio: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := base
			if err := decode(t, tc.body).Apply(&u); err != nil {
				t.Fatal(err)
			}
			if u != tc.want {
				t.Fatalf("got %+v, want %+v", u, tc.want)
			}
		})
	}
}

func ExampleUserPatch_Apply() {
	u := User{Name: "Alice", Active: true, Bio: "hi"}
	var p UserPatch
	_ = json.Unmarshal([]byte(`{"bio":null}`), &p)
	_ = p.Apply(&u)
	fmt.Printf("%+v\n", u)
	// Output: {Name:Alice Active:true Bio:}
}
```

## Review

The patch is correct when an empty body is a no-op, a `null` clears, and a value
sets — the three-way distinction a value field cannot represent. The mechanism is
that a pointer separates absent from set and a `json.RawMessage` additionally
separates null from value, which is why `Bio` is raw while `Name`/`Active` are
pointers: `Bio` is the field where "clear to empty" is a meaningful, distinct
command. If you find yourself unable to explain what an incoming zero value means,
that is the signal your patch field has too few states; promote it to a pointer or
a raw message.

## Resources

- [`json.RawMessage`](https://pkg.go.dev/encoding/json#RawMessage) — deferring and capturing raw bytes, including an explicit `null`.
- [`encoding/json.Unmarshal`](https://pkg.go.dev/encoding/json#Unmarshal) — how absent keys leave a field at its zero value.
- [RFC 5789: PATCH](https://www.rfc-editor.org/rfc/rfc5789) — the semantics of a partial-update request.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-strict-decoding-disallow-unknown-fields.md](03-strict-decoding-disallow-unknown-fields.md) | Next: [05-custom-marshaler-money-type.md](05-custom-marshaler-money-type.md)
