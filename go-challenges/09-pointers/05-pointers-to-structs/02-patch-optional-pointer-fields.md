# Exercise 2: An HTTP PATCH Handler Using *T Fields For Partial Updates

A PATCH endpoint must distinguish "the client did not send this field" from "the
client sent an explicit empty value". A plain `string` field cannot: both decode
to `""`. This exercise builds the standard fix — a patch struct of `*string` /
`*bool` / `*int` fields — and an `Apply` that mutates only the fields whose pointer
is non-nil, so omitting a key leaves the stored value untouched while sending `""`
clears it.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
patchprofile/             independent module: example.com/patchprofile
  go.mod
  patch.go                Profile; ProfilePatch{*string,*bool,*int}; DecodePatch; Apply
  cmd/
    demo/
      main.go             apply three different JSON bodies to one Profile
  patch_test.go           table test over omitted / explicit-empty / explicit-value bodies
```

Files: `patch.go`, `cmd/demo/main.go`, `patch_test.go`.
Implement: a `Profile` struct, a `ProfilePatch` with `*string`/`*bool`/`*int` fields and JSON tags, `DecodePatch(io.Reader) (*ProfilePatch, error)`, and `Apply(dst *Profile)` that writes only non-nil fields.
Test: body `{}` changes nothing; body `{"display_name":""}` clears the name; body `{"display_name":"Alice"}` sets it; a nil pointer means "no change", a non-nil-empty pointer means "set to zero"; `Apply` mutates the destination in place.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/05-pointers-to-structs/02-patch-optional-pointer-fields/cmd/demo
cd go-solutions/09-pointers/05-pointers-to-structs/02-patch-optional-pointer-fields
```

### The three-state problem, and why a pointer solves it

Consider updating a stored `Profile{DisplayName, Bio string; Public bool; Age int}`.
A REST PUT replaces the whole resource, so every field in the body is authoritative.
A PATCH is a *partial* update: the client sends only the fields it wants to change,
and the server must leave the rest alone. Now the encoding problem: JSON has no way
to say "this field is intentionally unchanged" — a field is either present with a
value or absent entirely. If the patch struct uses a plain `string DisplayName`,
`encoding/json` leaves it at its zero value `""` when the key is absent, which is
*indistinguishable* from a body that explicitly sent `"display_name": ""`. Applying
the patch unconditionally would then wipe the stored display name every time a
client PATCHes some unrelated field.

A pointer field fixes this by adding the third state. `DisplayName *string` decodes
to:

- `nil` when the key is absent (`{}` or `{"bio":"x"}`) — meaning "leave unchanged".
- a pointer to `""` when the key is present and empty (`{"display_name":""}`) —
  meaning "set it to empty".
- a pointer to the string when present and non-empty — meaning "set it".

`encoding/json` gives you exactly this: it only allocates the pointer when the key
appears in the input, so a nil pointer after decode reliably means "not provided".
`Apply` then reads each pointer: if non-nil, dereference and assign; if nil, skip.
That is the whole mechanism behind correct PATCH semantics, and it generalizes to
`*bool` (distinguish "leave the flag alone" from "set it to false") and `*int`
("leave the age" from "set it to 0").

Create `patch.go`:

```go
package patch

import (
	"encoding/json"
	"io"
)

// Profile is the stored resource. Its fields are plain values: once stored, each
// has a definite value. The three-state distinction lives in the patch, not here.
type Profile struct {
	DisplayName string
	Bio         string
	Public      bool
	Age         int
}

// ProfilePatch is a partial update. Every field is a pointer so a nil means
// "field omitted, leave unchanged" and a non-nil means "set to this value",
// including a non-nil pointer to the zero value meaning "set to empty/false/0".
type ProfilePatch struct {
	DisplayName *string `json:"display_name"`
	Bio         *string `json:"bio"`
	Public      *bool   `json:"public"`
	Age         *int    `json:"age"`
}

// DecodePatch reads a JSON body into a ProfilePatch. Keys absent from the body
// leave their pointer nil; keys present allocate a pointer to the decoded value.
func DecodePatch(r io.Reader) (*ProfilePatch, error) {
	var p ProfilePatch
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Apply mutates dst in place, writing only the fields the patch actually set.
// A nil pointer is skipped (leave unchanged); a non-nil pointer is dereferenced
// and assigned (set, even to the zero value).
func (p *ProfilePatch) Apply(dst *Profile) {
	if p.DisplayName != nil {
		dst.DisplayName = *p.DisplayName
	}
	if p.Bio != nil {
		dst.Bio = *p.Bio
	}
	if p.Public != nil {
		dst.Public = *p.Public
	}
	if p.Age != nil {
		dst.Age = *p.Age
	}
}
```

### The runnable demo

The demo starts with a populated `Profile`, then applies three bodies in turn: one
that omits `display_name` (leaves it), one that sends `"display_name":""` (clears
it), and one that sets it. `Apply` takes `*Profile`, so each call mutates the one
stored struct in place.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/patchprofile"
)

func main() {
	stored := &patch.Profile{DisplayName: "Alice", Bio: "hi", Public: true, Age: 30}

	// 1. Omit display_name: only bio changes, name stays "Alice".
	p1, _ := patch.DecodePatch(strings.NewReader(`{"bio":"updated bio"}`))
	p1.Apply(stored)
	fmt.Printf("after omit: name=%q bio=%q\n", stored.DisplayName, stored.Bio)

	// 2. Explicit empty display_name: clear it.
	p2, _ := patch.DecodePatch(strings.NewReader(`{"display_name":""}`))
	p2.Apply(stored)
	fmt.Printf("after empty: name=%q\n", stored.DisplayName)

	// 3. Explicit value plus flag flip.
	p3, _ := patch.DecodePatch(strings.NewReader(`{"display_name":"Alice A.","public":false}`))
	p3.Apply(stored)
	fmt.Printf("after set: name=%q public=%v\n", stored.DisplayName, stored.Public)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after omit: name="Alice" bio="updated bio"
after empty: name=""
after set: name="Alice A." public=false
```

### Tests

The table test drives the three cases that matter: an omitted key leaves its field,
an explicit empty value clears it, and an explicit value sets it. Each case starts
from the same populated `Profile` and asserts the exact post-`Apply` state, proving
the nil-vs-non-nil-empty distinction. A separate test checks that the decoder maps
absent keys to nil pointers.

Create `patch_test.go`:

```go
package patch

import (
	"fmt"
	"strings"
	"testing"
)

func TestApplyPartialUpdate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want Profile
	}{
		{
			name: "empty body changes nothing",
			body: `{}`,
			want: Profile{DisplayName: "Alice", Bio: "hi", Public: true, Age: 30},
		},
		{
			name: "omitted key leaves that field",
			body: `{"bio":"new"}`,
			want: Profile{DisplayName: "Alice", Bio: "new", Public: true, Age: 30},
		},
		{
			name: "explicit empty string clears the field",
			body: `{"display_name":""}`,
			want: Profile{DisplayName: "", Bio: "hi", Public: true, Age: 30},
		},
		{
			name: "explicit value sets the field",
			body: `{"display_name":"Bob"}`,
			want: Profile{DisplayName: "Bob", Bio: "hi", Public: true, Age: 30},
		},
		{
			name: "explicit false sets the flag to zero",
			body: `{"public":false}`,
			want: Profile{DisplayName: "Alice", Bio: "hi", Public: false, Age: 30},
		},
		{
			name: "explicit zero age sets it",
			body: `{"age":0}`,
			want: Profile{DisplayName: "Alice", Bio: "hi", Public: true, Age: 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dst := Profile{DisplayName: "Alice", Bio: "hi", Public: true, Age: 30}
			p, err := DecodePatch(strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("DecodePatch: %v", err)
			}
			p.Apply(&dst)
			if dst != tc.want {
				t.Fatalf("Apply = %+v, want %+v", dst, tc.want)
			}
		})
	}
}

func TestOmittedKeyLeavesPointerNil(t *testing.T) {
	t.Parallel()
	p, err := DecodePatch(strings.NewReader(`{"bio":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.DisplayName != nil {
		t.Fatal("omitted display_name must decode to a nil pointer")
	}
	if p.Bio == nil || *p.Bio != "x" {
		t.Fatal("present bio must decode to a non-nil pointer to its value")
	}
}

func TestExplicitEmptyIsNonNil(t *testing.T) {
	t.Parallel()
	p, err := DecodePatch(strings.NewReader(`{"display_name":""}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.DisplayName == nil {
		t.Fatal("explicit empty display_name must be a non-nil pointer to \"\"")
	}
	if *p.DisplayName != "" {
		t.Fatalf("*DisplayName = %q, want empty", *p.DisplayName)
	}
}

func ExampleProfilePatch_Apply() {
	dst := Profile{DisplayName: "Alice"}
	p, _ := DecodePatch(strings.NewReader(`{"bio":"engineer"}`))
	p.Apply(&dst)
	fmt.Printf("%q %q\n", dst.DisplayName, dst.Bio)
	// Output: "Alice" "engineer"
}
```

## Review

The handler is correct when applying a patch touches exactly the fields the client
sent: an omitted key leaves the stored field, an explicit empty value clears it, and
an explicit value sets it. The distinguishing evidence is the pair of tests showing
`DisplayName` decodes to a *nil* pointer when the key is omitted and a *non-nil*
pointer to `""` when the key is present and empty — the two states a plain `string`
field would collapse into one.

The mistake this exercise exists to prevent is using zero values as "not provided":
a plain-value patch struct silently wipes stored data on every partial update. The
pointer field is the idiom; `Apply`'s `if field != nil` guard is where the
three-state distinction becomes behavior. `DisallowUnknownFields` is a defensive
extra so a typo'd key is a 400 rather than a silent no-op. `Apply` takes `*Profile`
because it must mutate the caller's struct in place — a value receiver on the
destination would update a discarded copy.

## Resources

- [`encoding/json` Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal) — how absent keys leave pointer fields nil and present keys allocate them.
- [`json.Decoder.DisallowUnknownFields`](https://pkg.go.dev/encoding/json#Decoder.DisallowUnknownFields) — rejecting unexpected keys in a request body.
- [JSON Merge Patch (RFC 7386)](https://www.rfc-editor.org/rfc/rfc7386) — the semantics of partial updates this pattern implements.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-repository-defensive-copy-vs-aliasing.md](03-repository-defensive-copy-vs-aliasing.md)
