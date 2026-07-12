# Exercise 8: Method Sets — Why *T Satisfies a Repository but T Does Not

`InMemoryRepo does not implement Repository (method Save has pointer receiver)` is
one of Go's most-Googled compile errors, and it is not a bug — it is the method-set
rule. This module builds a `Repository` interface and an in-memory implementation
whose `Save` mutates a map (so it needs a pointer receiver), then shows that only
`*InMemoryRepo` satisfies the interface, and wires the repos by pointer.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
repo/                      independent module: example.com/repo
  go.mod
  repo.go                  interface Repository; InMemoryRepo (pointer receivers); ErrNotFound
  cmd/
    demo/
      main.go              wire []Repository by pointer, round-trip a user
  repo_test.go             compile-time satisfaction assertion; save/find; not-found error
```

Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
Implement: `Repository` interface (`Save(User) error`, `FindByID(string) (User, error)`); `InMemoryRepo` backed by `map[string]User` with pointer-receiver methods; `ErrNotFound`; a `var _ Repository = (*InMemoryRepo)(nil)` assertion.
Test: the compile-time assertion; a save-then-find round trip through the interface; `FindByID` of a missing id returns `ErrNotFound` via `errors.Is`.
Verify: `go test -count=1 -race ./...`

### The method-set rule, made concrete

`Save` mutates the repo's internal map, so it must have a pointer receiver — a
value receiver would drop every write (Exercise 5's bug). For consistency,
`FindByID` is a pointer receiver too. Now the method-set rule bites:

- The method set of `InMemoryRepo` (the value type) contains only value-receiver
  methods — here, none.
- The method set of `*InMemoryRepo` contains both — here, `Save` and `FindByID`.

`Repository` requires `Save` and `FindByID`, so only `*InMemoryRepo` is in the
interface. Write `var r Repository = InMemoryRepo{}` and the compiler rejects it
with `InMemoryRepo does not implement Repository (method Save has pointer
receiver)`. This is not the interface being wrong or a method being missing; the
methods exist, but they are in the *pointer's* method set, and a value cannot
reach them because Go cannot take the address of an arbitrary value to call a
pointer method on it.

The idiomatic guards are two. First, a package-scope compile-time assertion,
`var _ Repository = (*InMemoryRepo)(nil)`, which fails the build the instant the
implementation drifts out of the interface — far better than discovering it at a
call site. Second, always store the pointer: `[]Repository{NewInMemoryRepo()}`
where the constructor returns `*InMemoryRepo`. `FindByID` returns the package
sentinel `ErrNotFound` wrapped with `%w` so callers match it with `errors.Is`
rather than string-comparing an error message.

Create `repo.go`:

```go
package repo

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned by FindByID when no user has the given id.
var ErrNotFound = errors.New("repo: user not found")

// User is a minimal domain entity.
type User struct {
	ID   string
	Name string
}

// Repository is the persistence port. Save mutates storage, so implementations
// need pointer receivers, and therefore only *T values satisfy this interface.
type Repository interface {
	Save(u User) error
	FindByID(id string) (User, error)
}

// InMemoryRepo stores users in a map. Its methods use pointer receivers because
// Save mutates the map; consequently only *InMemoryRepo satisfies Repository.
type InMemoryRepo struct {
	users map[string]User
}

// NewInMemoryRepo returns a ready *InMemoryRepo (never a value: the type is
// mutable and only the pointer implements Repository).
func NewInMemoryRepo() *InMemoryRepo {
	return &InMemoryRepo{users: make(map[string]User)}
}

// Save inserts or replaces a user. Pointer receiver: the write persists.
func (r *InMemoryRepo) Save(u User) error {
	if u.ID == "" {
		return errors.New("repo: user id is required")
	}
	r.users[u.ID] = u
	return nil
}

// FindByID returns the user or ErrNotFound.
func (r *InMemoryRepo) FindByID(id string) (User, error) {
	u, ok := r.users[id]
	if !ok {
		return User{}, fmt.Errorf("find %q: %w", id, ErrNotFound)
	}
	return u, nil
}

// Compile-time proof that the POINTER type satisfies Repository. The value form
// "var _ Repository = InMemoryRepo{}" would NOT compile: it fails with
// "method Save has pointer receiver".
var _ Repository = (*InMemoryRepo)(nil)
```

### The runnable demo

The demo wires repositories into a `[]Repository` by pointer, saves a user through
the interface, reads it back, and then looks up a missing id to show the sentinel
error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"log"

	"example.com/repo"
)

func main() {
	// Stored by POINTER: []Repository{repo.NewInMemoryRepo()} — a value
	// InMemoryRepo{} would not satisfy Repository.
	repos := []repo.Repository{repo.NewInMemoryRepo()}
	store := repos[0]

	if err := store.Save(repo.User{ID: "u1", Name: "Ada"}); err != nil {
		log.Fatal(err)
	}

	u, err := store.FindByID("u1")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("found: %s (%s)\n", u.Name, u.ID)

	if _, err := store.FindByID("missing"); errors.Is(err, repo.ErrNotFound) {
		fmt.Printf("lookup miss: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
found: Ada (u1)
lookup miss: find "missing": repo: user not found
```

### Tests

The package-scope `var _ Repository = (*InMemoryRepo)(nil)` in `repo.go` is itself
a compile-time test — the build fails if the type stops satisfying the interface.
`TestSaveThenFind` round-trips a user through the `Repository` interface (not the
concrete type), proving the wiring works by pointer. `TestFindMissingReturnsErrNotFound`
pins the sentinel error path.

Create `repo_test.go`:

```go
package repo

import (
	"errors"
	"testing"
)

// A second compile-time assertion, in the test package, documenting the rule.
// Uncommenting "var _ Repository = InMemoryRepo{}" would fail to compile with
// "InMemoryRepo does not implement Repository (method Save has pointer receiver)".
var _ Repository = (*InMemoryRepo)(nil)

func TestSaveThenFind(t *testing.T) {
	t.Parallel()

	var r Repository = NewInMemoryRepo() // interface holds *InMemoryRepo
	if err := r.Save(User{ID: "u1", Name: "Ada"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := r.FindByID("u1")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Name != "Ada" {
		t.Fatalf("FindByID name = %q, want Ada", got.Name)
	}
}

func TestSaveRejectsEmptyID(t *testing.T) {
	t.Parallel()

	r := NewInMemoryRepo()
	if err := r.Save(User{Name: "no id"}); err == nil {
		t.Fatal("Save with empty ID should error")
	}
}

func TestFindMissingReturnsErrNotFound(t *testing.T) {
	t.Parallel()

	r := NewInMemoryRepo()
	_, err := r.FindByID("nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
```

## Review

The wiring is correct when a `*InMemoryRepo` slots into a `[]Repository` and a
value `InMemoryRepo{}` does not — the two compile-time assertions encode exactly
that. If you ever see "method Save has pointer receiver", the diagnosis is fixed:
your type has a pointer-receiver method, so store the pointer, not the value. The
`var _ Repository = (*InMemoryRepo)(nil)` idiom is worth adding to every
implementation of an interface you own; it converts a subtle wiring mistake into a
build failure at the definition site instead of a confusing error pages away.

## Resources

- [Go Spec: Method sets](https://go.dev/ref/spec#Method_sets) — the value/pointer method-set rule this exercise turns on.
- [Effective Go: Interfaces and methods](https://go.dev/doc/effective_go#interfaces_and_methods) — how method sets determine interface satisfaction.
- [Go FAQ: pointer vs value receiver method sets](https://go.dev/doc/faq#different_method_sets) — why a value cannot call a pointer-receiver method through an interface.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-method-value-capture-bug.md](07-method-value-capture-bug.md) | Next: [09-map-element-addressability.md](09-map-element-addressability.md)
