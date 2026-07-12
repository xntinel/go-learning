# Exercise 4: Pointer Fields for PATCH Semantics: Absent vs Zero

A PATCH request must distinguish "this field was omitted, leave it alone" from
"this field was explicitly set to its zero value." A plain `string` field cannot;
a `*string` field can. This exercise builds an `UpdateUserRequest` DTO whose
optional fields are pointers, a generic `Ptr[T]` helper, and an `Apply` that
mutates a `User` only for the fields that were actually supplied.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
patchdto/                     independent module: example.com/patchdto
  go.mod                      go 1.26
  patch.go                    User, UpdateUserRequest (*string/*bool/*int fields), Ptr[T], Apply
  cmd/
    demo/
      main.go                 runnable demo: three payloads unmarshalled and applied
  patch_test.go               absent vs zero vs set table + Apply selectivity + Ptr distinctness
```

Files: `patch.go`, `cmd/demo/main.go`, `patch_test.go`.
Implement: an `UpdateUserRequest` with `*string`/`*bool`/`*int` optional fields, a
generic `Ptr[T any](v T) *T`, and `Apply(*User)` that overwrites only non-nil
fields.
Test: unmarshalling `{}`, `{"name":""}`, and `{"name":"x"}` yields nil,
non-nil-empty, and set respectively; `Apply` only overwrites fields whose pointer
is non-nil; `Ptr(v)` returns a distinct heap pointer whose deref equals `v` for
string/bool/int.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/03-new-vs-composite-literal/04-nullable-patch-dto/cmd/demo
cd go-solutions/09-pointers/03-new-vs-composite-literal/04-nullable-patch-dto
```

### Why the fields are pointers

`PATCH /users/{id}` with body `{"name": ""}` means "set the name to the empty
string." The same endpoint with body `{}` means "change nothing." A `Name string`
field collapses these: both bodies leave `Name` at `""` after unmarshalling, and
`Apply` has no way to tell whether the client asked for an empty name or said
nothing. A `Name *string` field keeps them apart. `encoding/json` leaves an
omitted key's pointer field `nil`, and for a present key it allocates a value and
sets the pointer to it — so `{"name": ""}` yields a non-nil `*string` pointing at
`""`, and `{}` yields a nil `*string`. `Apply` then reads the presence off the
pointer: nil means "not supplied, skip"; non-nil means "supplied, overwrite with
the pointed-to value, even if that value is the zero value."

Building those pointers by hand — `s := "x"; req.Name = &s` — is noise, and you
cannot take the address of a literal (`&"x"` is a compile error). The generic
helper `func Ptr[T any](v T) *T { return &v }` fixes both: it works for any type,
returns a distinct heap pointer each call, and reads cleanly at the call site as
`Ptr("x")`, `Ptr(0)`, `Ptr(false)`. It is the modern replacement for the
per-type `stringPtr`/`intPtr` helpers people used to scatter around, and for
`new(T)` followed by an assignment.

Create `patch.go`:

```go
package patchdto

// User is the stored record a PATCH updates.
type User struct {
	Name   string
	Email  string
	Age    int
	Active bool
}

// UpdateUserRequest is a PATCH body. Optional fields are pointers so JSON
// unmarshalling distinguishes an omitted key (nil) from a key set to the zero
// value (non-nil pointer to ""/0/false).
type UpdateUserRequest struct {
	Name   *string `json:"name,omitempty"`
	Email  *string `json:"email,omitempty"`
	Age    *int    `json:"age,omitempty"`
	Active *bool   `json:"active,omitempty"`
}

// Ptr returns a pointer to a copy of v. It replaces scattered new(T)+assign and
// works for any type; &literal is not legal Go, so this helper is how you build
// a *string/*int/*bool inline.
func Ptr[T any](v T) *T {
	return &v
}

// Apply overwrites only the fields of u whose corresponding request pointer is
// non-nil. A non-nil pointer to a zero value (e.g. Name -> "") still overwrites;
// that is the "explicitly set to zero" case a PATCH must honor.
func (r UpdateUserRequest) Apply(u *User) {
	if r.Name != nil {
		u.Name = *r.Name
	}
	if r.Email != nil {
		u.Email = *r.Email
	}
	if r.Age != nil {
		u.Age = *r.Age
	}
	if r.Active != nil {
		u.Active = *r.Active
	}
}
```

### The runnable demo

The demo unmarshals three payloads against a seeded user and prints the result of
applying each, so the absent-vs-zero-vs-set distinction is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/patchdto"
)

func applyJSON(body string) patchdto.User {
	u := patchdto.User{Name: "alice", Email: "a@x.com", Age: 30, Active: true}
	var req patchdto.UpdateUserRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		panic(err)
	}
	req.Apply(&u)
	return u
}

func main() {
	fmt.Printf("empty {}:            %+v\n", applyJSON(`{}`))
	fmt.Printf("name to empty:       %+v\n", applyJSON(`{"name":""}`))
	fmt.Printf("name set + age:      %+v\n", applyJSON(`{"name":"bob","age":41}`))
	fmt.Printf("active false:        %+v\n", applyJSON(`{"active":false}`))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
empty {}:            {Name:alice Email:a@x.com Age:30 Active:true}
name to empty:       {Name: Email:a@x.com Age:30 Active:true}
name set + age:      {Name:bob Email:a@x.com Age:41 Active:true}
active false:        {Name:alice Email:a@x.com Age:30 Active:false}
```

### Tests

The table test proves the three unmarshalling outcomes: `{}` leaves `Name` nil,
`{"name":""}` gives a non-nil pointer to `""`, and `{"name":"x"}` gives a non-nil
pointer to `"x"`. A second test proves `Apply` only touches supplied fields, and a
third proves `Ptr` returns distinct, correct pointers for several types.

Create `patch_test.go`:

```go
package patchdto

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestUnmarshalAbsentVsZeroVsSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantNil bool
		wantVal string // only checked when non-nil
	}{
		{"absent", `{}`, true, ""},
		{"zero", `{"name":""}`, false, ""},
		{"set", `{"name":"x"}`, false, "x"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var req UpdateUserRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("Unmarshal(%s): %v", tc.body, err)
			}
			if tc.wantNil {
				if req.Name != nil {
					t.Fatalf("Name = %q, want nil (absent)", *req.Name)
				}
				return
			}
			if req.Name == nil {
				t.Fatal("Name = nil, want non-nil (present)")
			}
			if *req.Name != tc.wantVal {
				t.Fatalf("*Name = %q, want %q", *req.Name, tc.wantVal)
			}
		})
	}
}

func TestApplyOnlyTouchesSuppliedFields(t *testing.T) {
	t.Parallel()

	base := User{Name: "alice", Email: "a@x.com", Age: 30, Active: true}

	// Supply only Age and Active(false); Name and Email must be untouched.
	req := UpdateUserRequest{Age: Ptr(41), Active: Ptr(false)}
	u := base
	req.Apply(&u)

	if u.Name != "alice" {
		t.Errorf("Name = %q, want unchanged alice", u.Name)
	}
	if u.Email != "a@x.com" {
		t.Errorf("Email = %q, want unchanged", u.Email)
	}
	if u.Age != 41 {
		t.Errorf("Age = %d, want 41", u.Age)
	}
	if u.Active != false {
		t.Errorf("Active = %v, want false", u.Active)
	}
}

func TestApplyHonorsExplicitZero(t *testing.T) {
	t.Parallel()

	u := User{Name: "alice"}
	req := UpdateUserRequest{Name: Ptr("")} // explicitly set to empty
	req.Apply(&u)

	if u.Name != "" {
		t.Fatalf("Name = %q, want empty (explicit zero must overwrite)", u.Name)
	}
}

func TestPtrReturnsDistinctPointers(t *testing.T) {
	t.Parallel()

	s1, s2 := Ptr("v"), Ptr("v")
	if s1 == s2 {
		t.Fatal("Ptr returned the same address for two calls")
	}
	if *s1 != "v" || *s2 != "v" {
		t.Fatalf("deref = %q,%q, want v,v", *s1, *s2)
	}

	if b := Ptr(true); b == nil || *b != true {
		t.Fatal("Ptr(true) wrong")
	}
	if n := Ptr(7); n == nil || *n != 7 {
		t.Fatal("Ptr(7) wrong")
	}
}

func ExampleUpdateUserRequest_Apply() {
	u := User{Name: "alice", Age: 30}
	req := UpdateUserRequest{Age: Ptr(31)}
	req.Apply(&u)
	fmt.Printf("%s is %d\n", u.Name, u.Age)
	// Output: alice is 31
}
```

## Review

The DTO is correct when the three unmarshalling cases land on nil,
non-nil-to-zero, and non-nil-to-value respectively, and when `Apply` overwrites a
field if and only if its pointer is non-nil — including the delicate case where
the pointer points at the zero value and must still overwrite, which
`TestApplyHonorsExplicitZero` pins. The `omitempty` tag matters on the *marshal*
side (a nil pointer is omitted from output), and pointer nil-ness carries the
presence signal on the *unmarshal* side; together they make the round trip
lossless. The mistake to avoid is a value field (`Name string`) for an optional
parameter, which throws away the absent-vs-zero distinction and silently blanks
fields the client never mentioned — a real data-loss bug in PATCH endpoints. `Ptr`
replaces `new(T)`-then-assign and, because it returns `&v` of a fresh copy,
guarantees a distinct pointer per call.

## Resources

- [encoding/json.Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal) — how pointer fields and omitted keys interact.
- [JSON Merge Patch (RFC 7386)](https://datatracker.ietf.org/doc/html/rfc7386) — the semantics of absent vs null vs present in partial updates.
- [Go Specification: Type parameters](https://go.dev/ref/spec#Type_parameter_declarations) — the generics behind `Ptr[T any]`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-make-vs-new-worker-queue.md](05-make-vs-new-worker-queue.md)
