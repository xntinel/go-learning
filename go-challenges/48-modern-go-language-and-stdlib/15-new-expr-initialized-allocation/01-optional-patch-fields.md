# Exercise 1: PATCH DTO layer with tri-state pointer fields

A partial-update (HTTP PATCH) endpoint must distinguish a field the client did not
mention from a field the client explicitly set to its zero value. This exercise
builds that layer with pointer fields, decodes JSON into the DTO, and overlays
only the present fields onto a stored entity — populating optional fields inline
with `new(expr)` instead of a hand-rolled `Ptr` helper.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
patchfields/                 independent module: example.com/patchfields
  go.mod                     go 1.26 (new(expr) needs it)
  patch.go                   Account entity; AccountPatch DTO (pointer fields);
                             ParsePatch; (AccountPatch).Apply; ErrMalformedPatch
  cmd/
    demo/
      main.go                decodes a patch body and applies it to an account
  patch_test.go              table-driven decode+apply tests; sentinel path; Example
```

- Files: `patch.go`, `cmd/demo/main.go`, `patch_test.go`.
- Implement: an `AccountPatch` whose mutable fields are pointers, a `ParsePatch([]byte) (AccountPatch, error)` that decodes JSON, and an `Apply(Account) Account` that overlays only the non-nil fields.
- Test: decode bodies covering absent / present-zero / present-non-zero fields and assert `Apply` leaves absent fields unchanged while present-but-zero fields overwrite; assert the malformed-JSON path with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module. `new(expr)` requires Go 1.26, so pin the language version:

```bash
mkdir -p ~/go-exercises/patchfields/cmd/demo
cd ~/go-exercises/patchfields
go mod init example.com/patchfields
go mod edit -go=1.26
```

### Why the DTO fields are pointers

`Account` is the stored entity: every field holds a concrete value. `AccountPatch`
is the wire shape of a `PATCH` request, and its fields are pointers precisely so
the handler can read three states off each one. A nil `DisplayName` means the JSON
body had no `display_name` key, so the stored display name must be left alone. A
non-nil `DisplayName` pointing at `""` means the client sent `"display_name": ""`
and really wants the name cleared. A value field could not tell those apart —
both decode to the empty string.

`encoding/json` makes this work without any custom code: when a key is present,
the decoder allocates the pointer and sets it (even to a zero value); when a key
is absent, the pointer is left nil. So decoding *is* the tri-state parse. `Apply`
then folds the patch onto an existing account by copying only the fields whose
pointer is non-nil — an absent field falls through untouched, a present field
(zero or not) overwrites.

### Where new(expr) earns its place

The value of `new(expr)` here is in constructing patches in code — tests, internal
callers, fixtures — where you would otherwise litter the file with a `Ptr` helper.
Compare the illegal and awkward forms against the 1.26 form. All three of these
are compile errors, because a string literal, an arithmetic result, and a call
result are not addressable:

```go
p := AccountPatch{DisplayName: &"alice"}        // cannot take address of literal
p := AccountPatch{SeatLimit: &(base + delta)}   // cannot take address of (a+b)
p := AccountPatch{Email: &normalize(raw)}       // cannot take address of call
```

Before 1.26 you wrote a helper and called it, or introduced a temporary per field:

```go
name := "alice"
p := AccountPatch{DisplayName: &name}           // one temp per optional field
```

With `new(expr)` the value goes straight into the struct literal, including an
explicit zero, with no temporary and no helper package:

```go
p := AccountPatch{
	DisplayName: new("alice"),
	SeatLimit:   new(0),     // explicit zero: "set the seat limit to 0"
	Email:       new(normalize(raw)),
}
```

Note `new(0)` is a `*int` pointing at `0` — the "explicitly zero" state — which is
categorically different from leaving `SeatLimit` nil.

Create `patch.go`:

```go
package patchfields

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrMalformedPatch is returned (wrapped) when a patch body cannot be decoded.
var ErrMalformedPatch = errors.New("malformed patch")

// Account is the stored entity. Every field holds a concrete value.
type Account struct {
	DisplayName    string
	Email          string
	MarketingOptIn bool
	SeatLimit      int
}

// AccountPatch is the wire shape of a PATCH request. Each mutable field is a
// pointer so an absent key (nil, leave unchanged) is distinct from a key present
// with the zero value (non-nil, overwrite with "" / 0 / false).
type AccountPatch struct {
	DisplayName    *string `json:"display_name,omitempty"`
	Email          *string `json:"email,omitempty"`
	MarketingOptIn *bool   `json:"marketing_opt_in,omitempty"`
	SeatLimit      *int    `json:"seat_limit,omitempty"`
}

// ParsePatch decodes a PATCH body. An absent key leaves the corresponding
// pointer nil; a present key allocates it, even for a zero value. Unknown keys
// are rejected so a typo cannot silently drop an update.
func ParsePatch(body []byte) (AccountPatch, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var p AccountPatch
	if err := dec.Decode(&p); err != nil {
		return AccountPatch{}, fmt.Errorf("%w: %v", ErrMalformedPatch, err)
	}
	return p, nil
}

// Apply overlays the present fields of the patch onto a copy of a and returns
// it. A nil field is left unchanged; a non-nil field overwrites, including with
// a zero value.
func (p AccountPatch) Apply(a Account) Account {
	if p.DisplayName != nil {
		a.DisplayName = *p.DisplayName
	}
	if p.Email != nil {
		a.Email = *p.Email
	}
	if p.MarketingOptIn != nil {
		a.MarketingOptIn = *p.MarketingOptIn
	}
	if p.SeatLimit != nil {
		a.SeatLimit = *p.SeatLimit
	}
	return a
}

// Label renders an account's display name for logs, falling back when it is
// empty. cmp.Or returns the first non-zero argument.
func (a Account) Label() string {
	return cmp.Or(a.DisplayName, "(unnamed account)")
}
```

### The runnable demo

The demo constructs a patch two ways: by decoding a JSON body (the request path)
and by building one inline with `new(expr)` (the code path), then applies both to
a stored account. The inline patch clears the display name with an explicit
`new("")`, which the demo prints via `Label` to show the fallback.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/patchfields"
)

func main() {
	stored := patchfields.Account{
		DisplayName:    "Ada Lovelace",
		Email:          "ada@example.com",
		MarketingOptIn: true,
		SeatLimit:      10,
	}

	// Request path: only seat_limit is present, and it is an explicit zero.
	body := []byte(`{"seat_limit": 0}`)
	patch, err := patchfields.ParsePatch(body)
	if err != nil {
		panic(err)
	}
	afterReq := patch.Apply(stored)
	fmt.Printf("after request patch: seats=%d optin=%v\n",
		afterReq.SeatLimit, afterReq.MarketingOptIn)

	// Code path: clear the display name with new(expr), no Ptr helper.
	inline := patchfields.AccountPatch{DisplayName: new("")}
	afterInline := inline.Apply(afterReq)
	fmt.Printf("after inline patch: label=%q email=%q\n",
		afterInline.Label(), afterInline.Email)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after request patch: seats=0 optin=true
after inline patch: label="(unnamed account)" email="ada@example.com"
```

The seat limit became `0` (present-and-zero overwrote `10`), marketing opt-in
stayed `true` (absent, untouched), and the display name is now empty so `Label`
returns its fallback, while the email — never mentioned in either patch — is
unchanged.

### Tests

The table exercises the three states per field through the full decode-then-apply
path: a field absent from the body must leave the stored value; a field present
with its zero value must overwrite; a field present with a non-zero value must
overwrite. Assertions dereference the applied values rather than comparing pointer
identity. `TestParsePatch_Malformed` asserts the wrapped sentinel with
`errors.Is`. The `Example` shows an explicit zero override beating a non-zero
default. The last test composes two consecutive patches to prove `Apply` is a
clean fold.

Create `patch_test.go`:

```go
package patchfields

import (
	"errors"
	"fmt"
	"testing"
)

func TestParsePatchApply(t *testing.T) {
	t.Parallel()

	base := Account{
		DisplayName:    "Ada",
		Email:          "ada@example.com",
		MarketingOptIn: true,
		SeatLimit:      10,
	}

	tests := []struct {
		name string
		body string
		want Account
	}{
		{
			name: "all fields absent leaves entity unchanged",
			body: `{}`,
			want: base,
		},
		{
			name: "present zero values overwrite",
			body: `{"seat_limit": 0, "marketing_opt_in": false, "display_name": ""}`,
			want: Account{DisplayName: "", Email: "ada@example.com", MarketingOptIn: false, SeatLimit: 0},
		},
		{
			name: "present non-zero values overwrite",
			body: `{"seat_limit": 25, "display_name": "Grace"}`,
			want: Account{DisplayName: "Grace", Email: "ada@example.com", MarketingOptIn: true, SeatLimit: 25},
		},
		{
			name: "mixed absent and present",
			body: `{"email": "grace@example.com"}`,
			want: Account{DisplayName: "Ada", Email: "grace@example.com", MarketingOptIn: true, SeatLimit: 10},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			patch, err := ParsePatch([]byte(tt.body))
			if err != nil {
				t.Fatalf("ParsePatch(%q) error: %v", tt.body, err)
			}
			got := patch.Apply(base)
			if got != tt.want {
				t.Errorf("Apply = %+v; want %+v", got, tt.want)
			}
		})
	}
}

func TestParsePatch_Malformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{"broken json", `{"seat_limit":`},
		{"wrong type", `{"seat_limit": "lots"}`},
		{"unknown field", `{"nope": 1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParsePatch([]byte(tt.body))
			if !errors.Is(err, ErrMalformedPatch) {
				t.Fatalf("ParsePatch(%q) error = %v; want ErrMalformedPatch", tt.body, err)
			}
		})
	}
}

// TestComposedPatches is the "your turn" case: two consecutive patches must
// compose so that the second overlays the result of the first.
func TestComposedPatches(t *testing.T) {
	t.Parallel()

	base := Account{DisplayName: "Ada", SeatLimit: 10, MarketingOptIn: true}
	first := AccountPatch{SeatLimit: new(25)}
	second := AccountPatch{MarketingOptIn: new(false), SeatLimit: new(0)}

	got := second.Apply(first.Apply(base))
	want := Account{DisplayName: "Ada", SeatLimit: 0, MarketingOptIn: false}
	if got != want {
		t.Errorf("composed = %+v; want %+v", got, want)
	}
}

func ExampleAccountPatch_Apply() {
	base := Account{DisplayName: "Ada", SeatLimit: 10}
	// Explicit zero override wins over the non-zero stored default.
	patch := AccountPatch{SeatLimit: new(0)}
	got := patch.Apply(base)
	fmt.Printf("%s seats=%d\n", got.DisplayName, got.SeatLimit)
	// Output: Ada seats=0
}
```

## Review

The layer is correct when `Apply` copies a field exactly when its pointer is
non-nil, so an absent key never touches the stored value and a present-but-zero
key always overwrites it. The two failure modes to watch for are both about
conflating states. Using a value field with `omitempty` instead of a pointer
would drop a real `0` on the marshal side and make `"seat_limit": 0` on the
decode side indistinguishable from absent — the whole reason the fields are
pointers. Comparing patches or results by pointer identity (`got.SeatLimit ==
want.SeatLimit` on the `*int`) compares addresses, not values; the tests compare
whole `Account` values after `Apply`, which sidesteps that. Confirm correctness by
running `go test -count=1 -race ./...`: the table proves all three states per
field, the malformed cases prove the wrapped sentinel, and the `Example` pins the
explicit-zero-wins behavior.

## Resources

- [Go 1.26 release notes — changes to the language](https://go.dev/doc/go1.26) — `new` accepting an expression.
- [Go feature: new(expr)](https://antonz.org/accepted/new-expr/) — semantics, examples, and edge cases.
- [`encoding/json` Decoder](https://pkg.go.dev/encoding/json#Decoder) — `Decode` and `DisallowUnknownFields` behavior for pointer fields.
- [JSON Merge Patch (RFC 7386)](https://www.rfc-editor.org/rfc/rfc7386) — the partial-update semantics this layer implements.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-cloud-sdk-request-builder.md](02-cloud-sdk-request-builder.md)
