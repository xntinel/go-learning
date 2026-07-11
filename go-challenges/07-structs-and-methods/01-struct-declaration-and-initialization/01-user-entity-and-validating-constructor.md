# Exercise 1: A User Entity With a Validating Constructor

The `User` a repository loads and saves is the archetypal domain entity: exported
API fields, an invariant (non-empty ID and name), a constructor that validates
and normalizes inputs, and value-receiver methods that read but do not mutate.
This module builds that type and proves its contract with a real test.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
userentity/                 independent module: example.com/userentity
  go.mod                    go 1.24
  user.go                   type User; ErrEmptyID/ErrEmptyName; New; IsEmpty; DisplayName
  cmd/
    demo/
      main.go               constructs users, prints DisplayName, shows validation
  user_test.go              table + case tests, Example with // Output
```

- Files: `user.go`, `cmd/demo/main.go`, `user_test.go`.
- Implement: a value type `User` with exported fields, sentinel errors `ErrEmptyID`/`ErrEmptyName`, a `New(id, name, email string) (User, error)` that trims whitespace, validates, and stamps `CreatedAt` in UTC, plus value-receiver methods `IsEmpty` and `DisplayName`.
- Test: `New` returns a populated user, trims whitespace, rejects empty/whitespace ID and name via `errors.Is`, `IsEmpty` is true for a zero `User`, `DisplayName` prefers `Name` then falls back to `ID`, and two `New` calls with identical args are field-equal after zeroing `CreatedAt`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/userentity/cmd/demo
cd ~/go-exercises/userentity
go mod init example.com/userentity
go mod edit -go=1.24
```

### Why this type needs a constructor

`User` has an invariant: a valid user has a non-empty ID and a non-empty name,
and its inputs are normalized (whitespace trimmed, `CreatedAt` in UTC). That
invariant is exactly what justifies a constructor. `New` is the single place that
enforces it, so every other function in the codebase can accept a `User` and
trust it is well-formed instead of re-validating field by field.

The errors are **sentinel** values — package-level `var`s created with
`errors.New` — so callers can branch on them with `errors.Is` rather than string
matching. `New` returns them directly here; when a constructor wraps context
around a cause it should use `fmt.Errorf("...: %w", ErrEmptyID)` so `errors.Is`
still unwraps to the sentinel.

`User` is returned **by value**. The fields are all comparable (strings and a
`time.Time`), the type is small, and callers get an independent copy they cannot
use to corrupt anyone else's state. Because every field is comparable, `User`
supports `==` directly, which the equality test exploits. The two read-only
methods take a **value receiver** for the same reason: they never mutate, and a
copy is cheap.

Create `user.go`:

```go
package user

import (
	"errors"
	"strings"
	"time"
)

// Sentinel errors let callers branch with errors.Is instead of string matching.
var (
	ErrEmptyID   = errors.New("user id is required")
	ErrEmptyName = errors.New("user name is required")
)

// User is a repository-layer domain entity. Every field is exported API and
// comparable, so User supports == directly.
type User struct {
	ID        string
	Name      string
	Email     string
	CreatedAt time.Time
}

// New validates and normalizes its inputs, then returns a well-formed User.
// It is the single place the User invariant is established.
func New(id, name, email string) (User, error) {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" {
		return User{}, ErrEmptyID
	}
	if name == "" {
		return User{}, ErrEmptyName
	}
	return User{
		ID:        id,
		Name:      name,
		Email:     strings.TrimSpace(email),
		CreatedAt: time.Now().UTC(),
	}, nil
}

// IsEmpty reports whether the user carries no identifying data. It is true for
// the zero value, which is how callers detect an unset User.
func (u User) IsEmpty() bool {
	return u.ID == "" && u.Name == "" && u.Email == ""
}

// DisplayName prefers the human name and falls back to the ID.
func (u User) DisplayName() string {
	if u.Name != "" {
		return u.Name
	}
	return u.ID
}
```

### The runnable demo

The demo constructs a valid user, prints its display name and creation instant,
shows the ID fallback for a bare literal, and shows a validation failure.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/userentity"
)

func main() {
	u, err := user.New("  u1  ", "  Alice  ", "alice@example.com")
	if err != nil {
		fmt.Println("unexpected:", err)
		return
	}
	fmt.Printf("display=%s email=%s utc=%t\n", u.DisplayName(), u.Email, u.CreatedAt.Location() == u.CreatedAt.UTC().Location())

	bare := user.User{ID: "u2"}
	fmt.Printf("fallback=%s empty=%t\n", bare.DisplayName(), bare.IsEmpty())

	if _, err := user.New("   ", "Bob", ""); errors.Is(err, user.ErrEmptyID) {
		fmt.Println("rejected: empty id")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
display=Alice email=alice@example.com utc=true
fallback=u2 empty=false
rejected: empty id
```

### Tests

The tests pin every clause of the contract. The rejection cases assert against the
sentinels with `errors.Is` (whitespace-only inputs must be rejected too, proving
the trim runs before the check). The equality test is the centerpiece: it
constructs two users with identical arguments, zeroes `CreatedAt` on both (the one
field that legitimately differs run to run), and asserts `u1 == u2` — proving that
`==` on a struct compares field by field and that the constructor is
deterministic for its inputs.

Create `user_test.go`:

```go
package user

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestNewValidatesAndNormalizes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		id, uname, e string
		wantErr      error
		wantID       string
		wantName     string
	}{
		{name: "valid", id: "u1", uname: "Alice", e: "a@x.io", wantID: "u1", wantName: "Alice"},
		{name: "trims", id: "  u1  ", uname: "  Alice  ", e: "  a@x.io  ", wantID: "u1", wantName: "Alice"},
		{name: "empty id", id: "", uname: "Alice", wantErr: ErrEmptyID},
		{name: "whitespace id", id: "   ", uname: "Alice", wantErr: ErrEmptyID},
		{name: "empty name", id: "u1", uname: "", wantErr: ErrEmptyName},
		{name: "whitespace name", id: "u1", uname: "  ", wantErr: ErrEmptyName},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, err := New(tc.id, tc.uname, tc.e)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if u.ID != tc.wantID || u.Name != tc.wantName {
				t.Fatalf("u = %+v", u)
			}
			if u.CreatedAt.IsZero() {
				t.Fatal("CreatedAt should be set")
			}
			if u.CreatedAt.Location() != time.UTC {
				t.Fatalf("CreatedAt location = %v, want UTC", u.CreatedAt.Location())
			}
		})
	}
}

func TestIsEmptyZeroValue(t *testing.T) {
	t.Parallel()
	var u User
	if !u.IsEmpty() {
		t.Fatal("zero User should be empty")
	}
	full, _ := New("u1", "Alice", "")
	if full.IsEmpty() {
		t.Fatal("constructed User should not be empty")
	}
}

func TestDisplayNameFallback(t *testing.T) {
	t.Parallel()
	withName, _ := New("u1", "Alice", "")
	if got := withName.DisplayName(); got != "Alice" {
		t.Fatalf("DisplayName = %q, want Alice", got)
	}
	idOnly := User{ID: "u1"}
	if got := idOnly.DisplayName(); got != "u1" {
		t.Fatalf("DisplayName = %q, want u1", got)
	}
}

func TestStructEqualityComparesFields(t *testing.T) {
	t.Parallel()
	u1, _ := New("u1", "Alice", "alice@example.com")
	u2, _ := New("u1", "Alice", "alice@example.com")
	u1.CreatedAt = time.Time{}
	u2.CreatedAt = time.Time{}
	if u1 != u2 {
		t.Fatalf("users with identical fields should be ==: %+v vs %+v", u1, u2)
	}
}

func ExampleNew() {
	u, _ := New("u1", "Alice", "")
	fmt.Println(u.DisplayName())
	// Output: Alice
}
```

## Review

The type is correct when its invariant holds by construction: after a successful
`New`, `ID` and `Name` are non-empty and trimmed and `CreatedAt` is a UTC instant,
and no other code path produces a `User` claiming to be valid. The equality test
is the proof that `User` is a pure value type — two independently constructed
users with the same inputs are indistinguishable by `==` once the timestamp is
normalized away. The three traps this module guards against: never compare a
struct that later grows a slice field with `==` (write an `Equal` then); do not
mutate a returned `User` copy expecting other holders to see it (it is a copy);
and do not export a field that backs an invariant (keep a password hash
unexported and behind a method). Run `go vet` to confirm the literals are keyed
and `go test -race` to confirm the concurrency-free type has no surprises.

## Resources

- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — declaration, comparability, and the zero value.
- [Effective Go: composite literals](https://go.dev/doc/effective_go#composite_literals) — named vs positional initialization.
- [`errors` package](https://pkg.go.dev/errors) — `errors.New` sentinels and `errors.Is`.
- [`time` package](https://pkg.go.dev/time) — `time.Now`, `time.Time.UTC`, `time.Time.IsZero`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-server-config-functional-options.md](02-server-config-functional-options.md)
