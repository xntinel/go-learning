# Exercise 3: Backward-Compatible Type Migration Across a Package Boundary

The moment a type is exported and another team imports it, you can no longer move
or rename it freely — their code references *your* type by name, and a naive move
breaks their build. This exercise performs the one refactor that keeps every caller
compiling: the real definition moves to a new `entities` package, and the old
`models` package keeps `type User = entities.User` (an alias) so old and new names
remain the identical type during a deprecation window.

This module is self-contained. It contains three packages in one module and its
own tests and demo.

## What you'll build

```text
apimigration/             independent module: example.com/apimigration
  go.mod                  go 1.24
  entities/
    user.go               package entities: the real User definition + methods
  models/
    user.go               package models: type User = entities.User (Deprecated)
    user_test.go          proves entities.User and models.User are one type
  cmd/
    demo/
      main.go             constructs via entities, passes to a models function
```

- Files: `entities/user.go`, `models/user.go`, `models/user_test.go`, `cmd/demo/main.go`.
- Implement: `entities.User` with a `DisplayName()` method; `models.User = entities.User` plus a `Greet(models.User) string` function that older callers still use.
- Test: a value built as `entities.User` is assignable to `models.User` and back with no conversion; `Greet` accepts an `entities.User` directly.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The migration, and why only an alias works

The starting point is a package `models` that other teams import for its `User`
type. You have decided the real home for domain entities should be a new `entities`
package. If you simply move the definition and leave nothing behind, every importer
breaks. If you leave a *redefinition* — `type User entities.User` — the two `User`
types become distinct, and every caller that passes a `models.User` to something
expecting an `entities.User` (or vice versa) must now insert a conversion, in
lockstep with your release. Either way you have forced a coordinated migration.

The alias avoids this entirely. `type User = entities.User` makes `models.User` a
second spelling of the *identical* type. A `models.User` value *is* an
`entities.User` value; they flow between the two package APIs with no conversion, so
existing call sites keep compiling untouched. You attach a `// Deprecated:` doc so
tooling nudges callers toward `entities.User`, and once everyone has migrated you
delete the alias. This is the only mechanism that gives you a multi-release window
instead of a flag day.

Note where the methods live: `DisplayName` is declared in `entities` on
`entities.User`. Because `models.User` is an alias, it shares that method set for
free — you do not (and cannot) redeclare the method in `models`, since you may only
declare methods on a type defined in the same package.

Create `entities/user.go`:

```go
// Package entities holds the canonical domain types. User moved here from the
// legacy models package; models now aliases this type during the migration.
package entities

import "fmt"

// User is the canonical user entity.
type User struct {
	ID    string
	Email string
}

// DisplayName renders a user for logs and UI. It lives on the real type, so the
// models alias inherits it automatically.
func (u User) DisplayName() string {
	return fmt.Sprintf("%s <%s>", u.ID, u.Email)
}
```

Create `models/user.go`:

```go
// Package models is the legacy home of User. The real definition now lives in
// entities; this alias keeps existing importers source-compatible.
package models

import "example.com/apimigration/entities"

// User is an alias for entities.User, kept so callers that still import models
// keep compiling while they migrate.
//
// Deprecated: use entities.User directly. This alias will be removed once all
// callers have migrated.
type User = entities.User

// Greet is a legacy helper that older callers reach through the models package.
// Because User is an alias, it accepts an entities.User with no conversion.
func Greet(u User) string {
	return "Hello, " + u.DisplayName()
}
```

### The compile-only contrast

If `models/user.go` had said `type User entities.User` (a definition, no `=`), the
following would stop compiling:

```go
// var m models.User = someEntitiesUser // would NOT compile with a definition
// models.Greet(someEntitiesUser)        // would require models.User(...) conversion
```

With the alias, both work as-is. That is the whole migration guarantee, and the
tests below exercise it directly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/apimigration/entities"
	"example.com/apimigration/models"
)

func main() {
	// A caller in the new world constructs an entities.User.
	u := entities.User{ID: "usr_7", Email: "ada@example.com"}

	// A legacy helper in models accepts it with no conversion, because
	// models.User and entities.User are the same type.
	fmt.Println(models.Greet(u))

	// And an entities.User is assignable to a models.User variable directly.
	var legacy models.User = u
	fmt.Println("display:", legacy.DisplayName())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Hello, usr_7 <ada@example.com>
display: usr_7 <ada@example.com>
```

### Tests

The tests live in `package models` and prove type identity: an `entities.User`
assigns to a `models.User` and back with no conversion, and `Greet` (which takes
`models.User`) accepts an `entities.User` argument directly. None of this would
compile if `models.User` were a redefinition.

Create `models/user_test.go`:

```go
package models

import (
	"testing"

	"example.com/apimigration/entities"
)

func TestAliasIsSameType(t *testing.T) {
	t.Parallel()

	built := entities.User{ID: "usr_1", Email: "grace@example.com"}

	// Assignable to models.User with no conversion (alias == same type).
	var m User = built
	// And back to entities.User with no conversion.
	var back entities.User = m

	if back != built {
		t.Fatalf("round-trip changed the value: %+v", back)
	}
}

func TestGreetAcceptsEntitiesUser(t *testing.T) {
	t.Parallel()

	// Greet's parameter is models.User, but an entities.User is accepted directly.
	got := Greet(entities.User{ID: "usr_2", Email: "linus@example.com"})
	want := "Hello, usr_2 <linus@example.com>"
	if got != want {
		t.Fatalf("Greet = %q, want %q", got, want)
	}
}

func TestAliasSharesMethodSet(t *testing.T) {
	t.Parallel()

	var m User = entities.User{ID: "usr_3", Email: "edsger@example.com"}
	if got := m.DisplayName(); got != "usr_3 <edsger@example.com>" {
		t.Fatalf("DisplayName via alias = %q", got)
	}
}
```

## Review

The refactor is correct when the definition lives in `entities`, `models` holds
only the alias plus legacy helpers, and values cross the two package boundaries
with no conversion. The mistake that motivates the whole lesson is using a
definition (`type User entities.User`) for the move and then discovering every
importer needs a conversion — which is exactly the breaking change the alias
exists to avoid. The second mistake is trying to keep a method in `models`: you
cannot declare a method on an alias to a type from another package, so the method
must live in `entities`, which the alias inherits. Add the `// Deprecated:` doc so
callers get a nudge, and delete the alias only once every importer has moved.

## Resources

- [Go Blog: Type aliases](https://go.dev/blog/type-aliases) — the gradual-code-repair use case this exercise implements.
- [Go Language Spec: Alias declarations](https://go.dev/ref/spec#Alias_declarations) — alias identity semantics.
- [Go wiki: Deprecated comments](https://go.dev/wiki/Deprecated) — the `// Deprecated:` convention tooling recognizes.

---

Prev: [02-money-cents-defined-type.md](02-money-cents-defined-type.md) | Next: [04-byte-size-unit-type.md](04-byte-size-unit-type.md)
