# Exercise 6: Keeping JSON at the Boundary with a DTO Mapping

The wire format must not dictate the domain model, and decoding must never bypass
the constructor. This module builds a domain `User` with unexported, tag-free
fields and a separate `userDTO` carrying the json tags, wired together by
`MarshalJSON`/`UnmarshalJSON` so that serialization is an internal detail and
every decoded object is re-validated through the constructor.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
userjson/                   independent module: example.com/userjson
  go.mod                    go 1.26
  user.go                   domain User (no json tags) + userDTO (json tags) + Marshal/Unmarshal
  cmd/
    demo/
      main.go               runnable demo: marshal, unmarshal, reject bad payload
  user_test.go              tests: round-trip, invalid payload rejected, no json tags on domain, unknown field policy
```

- Files: `user.go`, `cmd/demo/main.go`, `user_test.go`.
- Implement: a domain `User` with unexported `id`/`name`/`email` and a `NewUser` constructor; a `userDTO` with json tags; `toDTO`/`fromDTO`; `MarshalJSON`/`UnmarshalJSON` that re-validate on decode and reject unknown fields.
- Test: a Marshal-then-Unmarshal round-trip preserves the value; an `UnmarshalJSON` of a payload with an empty required field returns the constructor's error; the domain struct carries no json tags; an unknown wire field is rejected per policy.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/12-designing-a-domain-model/06-domain-dto-json-boundary/cmd/demo
cd go-solutions/07-structs-and-methods/12-designing-a-domain-model/06-domain-dto-json-boundary
```

### Two shapes, mapped explicitly, re-validated on decode

The domain `User` has unexported fields and no struct tags at all. That is
deliberate: the domain type is a contract with your invariants, and json tags on
it would make the API's field names a property of the model — rename a JSON key
and you would be editing the domain. Worse, if the domain fields were exported and
tagged, a caller could `json.Unmarshal` a raw payload straight into a `User` and
skip `NewUser` entirely, letting a message with an empty email create an illegal
object over the network. Keeping the fields unexported makes that impossible: the
only way to get a `User` is through the constructor or through `UnmarshalJSON`,
and `UnmarshalJSON` routes through the constructor.

The wire shape lives on a separate `userDTO` with json tags. `toDTO` copies the
domain into the DTO for encoding; `fromDTO` copies the DTO into the domain *by
calling `NewUser`*, so the same validation that guards direct construction guards
decoding. `MarshalJSON` marshals the DTO; `UnmarshalJSON` decodes into the DTO and
then calls `fromDTO`, so a payload with a missing required field fails with the
constructor's sentinel error rather than producing a half-built object. This is
the whole discipline in two functions: map explicitly at the boundary, and re-run
the constructor on the way in.

Unknown wire fields are a policy decision, and here the policy is strict: an
incoming payload with a field the DTO does not declare is rejected. `UnmarshalJSON`
uses a `json.Decoder` with `DisallowUnknownFields`, which is the right default for
a service that wants to catch a client sending `emial` (a typo) rather than
silently dropping it. A more lenient service would use plain `json.Unmarshal` and
ignore extras; the point is to choose deliberately, not by accident.

Create `user.go`:

```go
package userjson

import (
	"bytes"
	"encoding/json"
	"errors"
)

var (
	ErrEmptyID    = errors.New("user: id is required")
	ErrEmptyName  = errors.New("user: name is required")
	ErrEmptyEmail = errors.New("user: email is required")
)

// User is the domain entity. Fields are unexported and carry NO json tags: the
// wire format is not allowed to dictate the model, and no caller can decode
// straight into it and bypass the constructor.
type User struct {
	id    string
	name  string
	email string
}

// NewUser is the only door into a valid User.
func NewUser(id, name, email string) (User, error) {
	if id == "" {
		return User{}, ErrEmptyID
	}
	if name == "" {
		return User{}, ErrEmptyName
	}
	if email == "" {
		return User{}, ErrEmptyEmail
	}
	return User{id: id, name: name, email: email}, nil
}

func (u User) ID() string    { return u.id }
func (u User) Name() string  { return u.name }
func (u User) Email() string { return u.email }

// userDTO is the wire shape. It, not the domain type, owns the json tags.
type userDTO struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (u User) toDTO() userDTO {
	return userDTO{ID: u.id, Name: u.name, Email: u.email}
}

// fromDTO maps the wire shape into the domain BY CALLING the constructor, so a
// decoded object is re-validated and can never be illegal.
func fromDTO(d userDTO) (User, error) {
	return NewUser(d.ID, d.Name, d.Email)
}

// MarshalJSON emits the DTO shape.
func (u User) MarshalJSON() ([]byte, error) {
	return json.Marshal(u.toDTO())
}

// UnmarshalJSON decodes into the DTO (rejecting unknown fields, per policy) and
// re-validates through the constructor.
func (u *User) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var d userDTO
	if err := dec.Decode(&d); err != nil {
		return err
	}
	decoded, err := fromDTO(d)
	if err != nil {
		return err
	}
	*u = decoded
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"example.com/userjson"
)

func main() {
	u, _ := userjson.NewUser("u1", "Alice", "alice@example.com")

	data, _ := json.Marshal(u)
	fmt.Printf("wire: %s\n", data)

	var back userjson.User
	if err := json.Unmarshal(data, &back); err != nil {
		panic(err)
	}
	fmt.Printf("decoded: %s / %s\n", back.Name(), back.Email())

	bad := []byte(`{"id":"u2","name":"Bob","email":""}`)
	if err := json.Unmarshal(bad, &back); errors.Is(err, userjson.ErrEmptyEmail) {
		fmt.Println("illegal payload rejected on decode")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wire: {"id":"u1","name":"Alice","email":"alice@example.com"}
decoded: Alice / alice@example.com
illegal payload rejected on decode
```

### Tests

The tests prove the four guarantees. The round-trip marshals a `User` and
unmarshals it back into an equal value. The invalid-payload test decodes a JSON
object with an empty required field and asserts the constructor's sentinel comes
back — illegal state cannot be decoded. The no-tags test reflects over the domain
struct and asserts no field has a json tag. The unknown-field test sends an extra
key and asserts it is rejected under the strict policy.

Create `user_test.go`:

```go
package userjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	u, err := NewUser("u1", "Alice", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var back User
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back != u {
		t.Fatalf("round-trip changed value: got %+v, want %+v", back, u)
	}
}

func TestDecodeRejectsIllegalState(t *testing.T) {
	t.Parallel()
	var u User
	err := json.Unmarshal([]byte(`{"id":"u1","name":"Alice","email":""}`), &u)
	if !errors.Is(err, ErrEmptyEmail) {
		t.Fatalf("decode err = %v, want ErrEmptyEmail", err)
	}
}

func TestDomainHasNoJSONTags(t *testing.T) {
	t.Parallel()
	rt := reflect.TypeOf(User{})
	for i := range rt.NumField() {
		if tag := rt.Field(i).Tag.Get("json"); tag != "" {
			t.Fatalf("domain field %q carries a json tag %q; tags belong on the DTO", rt.Field(i).Name, tag)
		}
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	t.Parallel()
	var u User
	err := json.Unmarshal([]byte(`{"id":"u1","name":"Alice","email":"a@b","admin":true}`), &u)
	if err == nil {
		t.Fatal("expected unknown field to be rejected under strict policy")
	}
}

func ExampleUser_MarshalJSON() {
	u, _ := NewUser("u1", "Alice", "alice@example.com")
	data, _ := json.Marshal(u)
	fmt.Println(string(data))
	// Output: {"id":"u1","name":"Alice","email":"alice@example.com"}
}
```

## Review

The boundary is correct when the domain type has no json tags, the DTO owns them,
and `UnmarshalJSON` re-validates through `NewUser` so a decoded object is always
valid. The test that matters most is `TestDecodeRejectsIllegalState`: it proves an
empty-email payload cannot become a `User` through the network, which is the exact
failure that json tags on an exported domain struct would allow. The mistakes to
avoid: tagging the domain struct (coupling the wire to the model) and decoding
straight into it without re-validating (letting illegal states in). The unknown-
field policy is strict here by choice; a lenient service would use plain
`json.Unmarshal` — the point is to decide, not to default silently.

## Resources

- [`encoding/json`: Marshaler and Unmarshaler](https://pkg.go.dev/encoding/json#Marshaler) — the interfaces the domain type implements.
- [`json.Decoder.DisallowUnknownFields`](https://pkg.go.dev/encoding/json#Decoder.DisallowUnknownFields) — strict unknown-field policy.
- [`reflect.Type`](https://pkg.go.dev/reflect#Type) — inspecting struct tags in the test.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-constructor-functional-options.md](05-constructor-functional-options.md) | Next: [07-aggregate-root-ledger-invariant.md](07-aggregate-root-ledger-invariant.md)
