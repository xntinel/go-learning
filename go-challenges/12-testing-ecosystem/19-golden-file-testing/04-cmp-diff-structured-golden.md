# Exercise 4: Reviewable Diffs on Decoded Structures with go-cmp

A byte diff on a large JSON blob is unreadable; a reviewer cannot tell which
field changed. Decode both the actual output and the golden into the same struct
and compare with `cmp.Diff`, and a failure prints a `-want +got` diff that names
the exact field. You build a DTO mapper and golden-test it semantically.

This module imports `github.com/google/go-cmp`. It is otherwise fully
self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
userdto/                   independent module: example.com/userdto
  go.mod                   go 1.26; requires github.com/google/go-cmp
  dto.go                   User (internal) -> UserDTO (public); MarshalDTO
  testdata/
    user_dto.golden        canonical golden JSON
    user_dto_reordered.golden  same value, different key order/whitespace
  cmd/
    demo/
      main.go              maps a user and prints the DTO JSON
  dto_test.go              decode-both + cmp.Diff, semantic-vs-byte contrast
```

Files: `dto.go`, two `testdata/*.golden`, `cmd/demo/main.go`, `dto_test.go`.
Implement: `MapUserDTO(User) UserDTO` and `MarshalDTO(User) ([]byte, error)`.
Test: decode the golden and the actual into `UserDTO` and compare with `cmp.Diff`; prove semantic comparison ignores key order and whitespace where byte comparison does not.
Verify: `go test -count=1 -race ./...`

Set up the module. It depends on go-cmp:

```bash
mkdir -p go-solutions/12-testing-ecosystem/19-golden-file-testing/04-cmp-diff-structured-golden/cmd/demo go-solutions/12-testing-ecosystem/19-golden-file-testing/04-cmp-diff-structured-golden/testdata
cd go-solutions/12-testing-ecosystem/19-golden-file-testing/04-cmp-diff-structured-golden
go get github.com/google/go-cmp/cmp@v0.7.0
```

### Byte-exact versus decoded-semantic, and when each is right

The previous exercise byte-compared a canonicalized body. That is the correct
contract when the exact serialization matters — a file another tool parses, a
wire format. But for an API DTO you often care about the *value*, not its exact
textual encoding: whether the JSON key order happens to be `id, name` or `name,
id`, or whether an object is indented two spaces or four, is not a contract your
consumers depend on. Byte-comparing such output produces flaky failures on
formatting churn and, worse, an unreadable diff when it does fail.

The semantic approach decodes both sides into the same Go type and compares the
*values*. `cmp.Diff(want, got)` returns an empty string on equality and a compact
field-level `-want +got` diff otherwise, so a reviewer sees exactly which field
changed rather than hunting through a byte blob. Decoding also erases
insignificant formatting: two JSON documents that differ only in key order or
whitespace decode to equal structs. The trade-off is deliberate — you have given
up the ability to catch a pure formatting change, in exchange for a diff that
speaks in fields. Choose this contract for DTOs and value objects; keep the
byte-exact contract for wire formats.

`MapUserDTO` is the artifact under test: it projects an internal `User` (with
fields you do not expose) into the public `UserDTO`, joining the name and copying
only the public fields. The golden pins the DTO shape; a change to the mapping —
say, dropping `email` from the projection — shows up as a named field diff.

Create `dto.go`:

```go
package userdto

import (
	"encoding/json"
	"strings"
)

// User is the internal domain model, with fields the API does not expose.
type User struct {
	ID        string
	FirstName string
	LastName  string
	Email     string
	Roles     []string
	Password  string // never serialized
}

// UserDTO is the public projection. Its json tags are the API contract.
type UserDTO struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Email string   `json:"email"`
	Roles []string `json:"roles"`
}

// MapUserDTO projects a User onto its public DTO.
func MapUserDTO(u User) UserDTO {
	return UserDTO{
		ID:    u.ID,
		Name:  strings.TrimSpace(u.FirstName + " " + u.LastName),
		Email: u.Email,
		Roles: u.Roles,
	}
}

// MarshalDTO renders the DTO as indented JSON.
func MarshalDTO(u User) ([]byte, error) {
	return json.MarshalIndent(MapUserDTO(u), "", "  ")
}
```

Now two goldens that hold the *same value* in different textual forms, to prove
the comparison is semantic.

Create `testdata/user_dto.golden`:

```text
{
  "id": "u-7",
  "name": "Ada Lovelace",
  "email": "ada@example.com",
  "roles": [
    "admin",
    "engineer"
  ]
}
```

Create `testdata/user_dto_reordered.golden`:

```text
{"name":"Ada Lovelace","roles":["admin","engineer"],"email":"ada@example.com","id":"u-7"}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/userdto"
)

func main() {
	u := userdto.User{
		ID:        "u-7",
		FirstName: "Ada",
		LastName:  "Lovelace",
		Email:     "ada@example.com",
		Roles:     []string{"admin", "engineer"},
		Password:  "secret",
	}
	b, err := userdto.MarshalDTO(u)
	if err != nil {
		fmt.Println("error:", err)
		return
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
{
  "id": "u-7",
  "name": "Ada Lovelace",
  "email": "ada@example.com",
  "roles": [
    "admin",
    "engineer"
  ]
}
```

### Tests

`TestDTOGolden` decodes the golden and the actual into `UserDTO` and compares
with `cmp.Diff`, so any field-level difference prints a readable diff.
`TestSemanticIgnoresFormatting` decodes the reordered golden and asserts it is
`cmp.Equal` to the actual even though it is *not* `bytes.Equal` — the concrete
demonstration that semantic comparison sees values, not text.

Create `dto_test.go`:

```go
package userdto

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/")

func decode(t *testing.T, b []byte) UserDTO {
	t.Helper()
	var d UserDTO
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return d
}

func sampleUser() User {
	return User{
		ID:        "u-7",
		FirstName: "Ada",
		LastName:  "Lovelace",
		Email:     "ada@example.com",
		Roles:     []string{"admin", "engineer"},
		Password:  "secret",
	}
}

func TestDTOGolden(t *testing.T) {
	got, err := MarshalDTO(sampleUser())
	if err != nil {
		t.Fatalf("MarshalDTO: %v", err)
	}
	path := filepath.Join("testdata", "user_dto.golden")
	if *update {
		if err := os.WriteFile(path, append(bytes.TrimRight(got, "\n"), '\n'), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run: go test -update)", path, err)
	}
	if diff := cmp.Diff(decode(t, golden), decode(t, got)); diff != "" {
		t.Errorf("DTO mismatch (-want +got):\n%s", diff)
	}
}

func TestSemanticIgnoresFormatting(t *testing.T) {
	got, err := MarshalDTO(sampleUser())
	if err != nil {
		t.Fatalf("MarshalDTO: %v", err)
	}
	reordered, err := os.ReadFile(filepath.Join("testdata", "user_dto_reordered.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if bytes.Equal(bytes.TrimSpace(reordered), bytes.TrimSpace(got)) {
		t.Fatal("expected the reordered golden to differ byte-wise from the actual")
	}
	if !cmp.Equal(decode(t, reordered), decode(t, got)) {
		t.Errorf("semantic compare should treat the reordered golden as equal")
	}
}

func ExampleMapUserDTO() {
	d := MapUserDTO(User{ID: "u-1", FirstName: "Grace", LastName: "Hopper"})
	os.Stdout.WriteString(d.ID + " " + d.Name + "\n")
	// Output: u-1 Grace Hopper
}
```

## Review

The semantic contract is correct when a failure names the field: `cmp.Diff`
prints `-want +got` with the changed field, which is the readability payoff over a
byte blob. The `TestSemanticIgnoresFormatting` case makes the trade-off concrete —
the reordered golden is deliberately not byte-equal yet decodes equal, proving the
comparison is over values. The trap is choosing this contract when you actually
needed byte-exactness: if the artifact is consumed by a tool that cares about key
order or whitespace, semantic comparison will silently pass a formatting change
that breaks that tool. Pick per artifact: DTOs and value objects semantic, wire
formats byte-exact. Regenerate with `go test -update`; because the compare is
semantic, the update only needs to produce a value-equal document, but keeping it
canonical (indented, stable key order) makes the committed diff readable.

## Resources

- [go-cmp: cmp.Diff / cmp.Equal](https://pkg.go.dev/github.com/google/go-cmp/cmp) — the empty-on-equal, field-level diff.
- [encoding/json.Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal) — decoding both sides into the same struct.
- [encoding/json.MarshalIndent](https://pkg.go.dev/encoding/json#MarshalIndent) — producing the canonical committed form.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-normalize-nondeterministic-fields.md](05-normalize-nondeterministic-fields.md)
