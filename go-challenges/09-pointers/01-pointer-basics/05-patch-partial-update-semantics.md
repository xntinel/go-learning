# Exercise 5: PATCH handler — apply a partial update using pointer fields

HTTP PATCH touches only the fields the client provided; PUT replaces the whole
resource. Modeling PATCH correctly needs pointer fields: a `nil` patch field means
"leave it alone" and a non-nil field means "write this, even if it is the zero
value". This module builds an `ApplyPatch` that mutates the target in place through
its pointer.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
userpatch/                 independent module: example.com/userpatch
  go.mod                   module example.com/userpatch
  patch.go                 User{Name,Email,Age}; UserPatch{*string,*string,*int}; ApplyPatch(*User, UserPatch)
  cmd/
    demo/
      main.go              patches only Email, prints before and after
  patch_test.go            only-Email; explicit Age=0 clears; nil leaves alone; mutation is in place
```

- Files: `patch.go`, `cmd/demo/main.go`, `patch_test.go`.
- Implement: `UserPatch` with `*string`/`*int` fields and `ApplyPatch(current *User, patch UserPatch)` that writes only non-nil fields onto `*current`.
- Test: a patch with only `Email` set changes `Email` and leaves `Name`/`Age`; a patch setting `Age` to a pointer-to-`0` writes `0`; a `nil` `Age` leaves it; the function mutates the caller's struct.
- Verify: `go test -count=1 -race ./...`

### PATCH is "touch only what was provided"

`User` is the resource: plain value fields. `UserPatch` is the request body: every
field is a pointer, so a field the client omitted arrives as `nil` and a field the
client sent — including `""` or `0` — arrives as a non-nil pointer. `ApplyPatch`
takes `current *User` (a pointer, because it mutates the caller's struct in place)
and, for each patch field, writes it onto `*current` only if it is non-nil:

```go
if patch.Email != nil {
	current.Email = *patch.Email
}
```

This gives the exact PATCH semantics: an unset field is left alone, and a field set
to the zero value is an *explicit clear* that overwrites. Age is the sharpest case
— `Age: nil` leaves the current age, while `Age: ptr(0)` writes `0`. With a plain
`int` patch field the two would be indistinguishable, so "reset age to 0" would be
impossible to express, and every omitted age would wrongly zero the record. The
mutation is in place through `current`: the caller passes `&user` and reads its own
`user` afterward to see the change, exactly as a handler mutates the loaded model
before saving it.

Create `patch.go`:

```go
package userpatch

// User is the resource being patched: plain value fields.
type User struct {
	Name  string
	Email string
	Age   int
}

// UserPatch is a partial update. A nil field means "leave the current value
// alone"; a non-nil field means "write this value", even if it is the zero
// value (an explicit clear).
type UserPatch struct {
	Name  *string `json:"name"`
	Email *string `json:"email"`
	Age   *int    `json:"age"`
}

// ApplyPatch writes each non-nil patch field onto the user through the pointer,
// mutating the caller's struct in place. Nil fields are left untouched.
func ApplyPatch(current *User, patch UserPatch) {
	if patch.Name != nil {
		current.Name = *patch.Name
	}
	if patch.Email != nil {
		current.Email = *patch.Email
	}
	if patch.Age != nil {
		current.Age = *patch.Age
	}
}
```

### The runnable demo

The demo patches only `Email` and shows `Name` and `Age` surviving untouched, the
defining behavior of PATCH versus PUT.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/userpatch"
)

func ptr[T any](v T) *T { return &v }

func main() {
	user := userpatch.User{Name: "alice", Email: "alice@old.example", Age: 30}
	fmt.Printf("before: %+v\n", user)

	patch := userpatch.UserPatch{Email: ptr("alice@new.example")}
	userpatch.ApplyPatch(&user, patch)

	fmt.Printf("after:  %+v\n", user)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before: {Name:alice Email:alice@old.example Age:30}
after:  {Name:alice Email:alice@new.example Age:30}
```

### Tests

`TestPatchOnlyEmail` asserts `Email` changed and `Name`/`Age` did not.
`TestExplicitZeroClears` asserts a patch with `Age: ptr(0)` writes `0` while a
`nil` `Age` leaves the current value. `TestMutatesCaller` passes `&user` and reads
`user` afterward to confirm the mutation is in place, not on a copy.

Create `patch_test.go`:

```go
package userpatch

import (
	"fmt"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestPatchOnlyEmail(t *testing.T) {
	t.Parallel()

	user := User{Name: "alice", Email: "old@example.com", Age: 30}
	ApplyPatch(&user, UserPatch{Email: ptr("new@example.com")})

	if user.Email != "new@example.com" {
		t.Fatalf("Email = %q, want new@example.com", user.Email)
	}
	if user.Name != "alice" {
		t.Fatalf("Name = %q, want alice (untouched)", user.Name)
	}
	if user.Age != 30 {
		t.Fatalf("Age = %d, want 30 (untouched)", user.Age)
	}
}

func TestExplicitZeroClears(t *testing.T) {
	t.Parallel()

	// nil Age leaves the current value.
	keep := User{Age: 42}
	ApplyPatch(&keep, UserPatch{})
	if keep.Age != 42 {
		t.Fatalf("Age = %d, want 42 (nil patch leaves it)", keep.Age)
	}

	// Pointer-to-0 explicitly clears.
	clear := User{Age: 42}
	ApplyPatch(&clear, UserPatch{Age: ptr(0)})
	if clear.Age != 0 {
		t.Fatalf("Age = %d, want 0 (explicit clear)", clear.Age)
	}
}

func TestMutatesCaller(t *testing.T) {
	t.Parallel()

	user := User{Name: "bob"}
	ApplyPatch(&user, UserPatch{Name: ptr("carol")})
	if user.Name != "carol" {
		t.Fatalf("caller's user.Name = %q, want carol (in-place mutation)", user.Name)
	}
}

func Example() {
	user := User{Name: "alice", Age: 30}
	ApplyPatch(&user, UserPatch{Age: ptr(0)})
	fmt.Printf("%s age=%d\n", user.Name, user.Age)
	// Output: alice age=0
}
```

## Review

The handler is correct when a patch touches exactly the fields whose pointers are
non-nil and nothing else. The Age case is the proof that pointer fields are
required: `nil` leaves `42`, `ptr(0)` writes `0`, and no value-typed field could
express both. `ApplyPatch` takes `current *User` precisely because it must mutate
the caller's model in place — passing `User` by value would update a throwaway copy
and the saved record would never change, the classic "my PATCH silently did
nothing" bug. Note that `ApplyPatch` does not return anything; the pointer *is* the
output channel. If you later add fields, each gets the same `if patch.F != nil {
current.F = *patch.F }` shape.

## Resources

- [JSON and Go](https://go.dev/blog/json) — pointer fields decode absent/null/present distinctly, the basis for PATCH.
- [RFC 5789: PATCH method for HTTP](https://www.rfc-editor.org/rfc/rfc5789) — partial-update semantics.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-repository-lookup-nil-return.md](04-repository-lookup-nil-return.md) | Next: [06-large-request-copy-cost.md](06-large-request-copy-cost.md)
