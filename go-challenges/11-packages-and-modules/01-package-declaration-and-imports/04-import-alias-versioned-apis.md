# Exercise 4: Aliased Imports to Resolve Package-Name Collisions

Sooner or later two packages you need in one file share a base name: `api/v1/user`
and `api/v2/user` are both `package user`; two vendored libraries both named their
package `client`. You cannot refer to both by the bare name in one file — the
identifiers collide. Import aliasing is the fix, and it is the everyday mechanic of
API/DTO versioning. This exercise builds a migration gateway that consumes two
same-named `user` packages side by side.

This module is self-contained. Nothing here imports another exercise.

## What you'll build

```text
userapi/                           module: example.com/userapi
  go.mod
  api/v1/user/user.go              package user (v1): User{ID int, Name string}, Name const
  api/v2/user/user.go              package user (v2): User{ID string, FullName, Locale}, Name const
  gateway/gateway.go               imports both via aliases userv1, userv2; Migrate(v1)->v2
  gateway/gateway_test.go          verifies field mapping and that aliasing is name-local
  cmd/demo/main.go                 migrates a sample record and prints it
```

- Files: the five above.
- Implement: two `package user` versions and a `gateway` that aliases both imports and maps v1 to v2, defaulting the new `Locale` field.
- Test: `Migrate` maps `ID`/`Name` to `ID`/`FullName` and defaults `Locale`; assert the alias does not change either package's own declared name.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
  go-solutions/11-packages-and-modules/01-package-declaration-and-imports/04-import-alias-versioned-apis/gateway go-solutions/11-packages-and-modules/01-package-declaration-and-imports/04-import-alias-versioned-apis/cmd/demo
go mod edit -go=1.26
```

### Two packages, one base name

Both packages are literally `package user`. They live at different import paths
(`.../api/v1/user` and `.../api/v2/user`), which is legal and normal — the import
path is unique even though the package name is not. Each exports a `User` whose
shape differs across the version bump: v1 keys users by an integer ID and stores a
single `Name`; v2 switches to a string ID (the classic "we outgrew int IDs"
migration), splits the name into `FullName`, and adds a `Locale` field that did not
exist in v1. Each package also exports a `Name` constant so the test can prove a
point later.

Create `api/v1/user/user.go`:

```go
package user

// Name is this package's own declared name, for demonstrating that an import
// alias does not change it.
const Name = "user"

// User is the v1 shape: integer ID and a single Name field.
type User struct {
	ID   int
	Name string
}
```

Create `api/v2/user/user.go`:

```go
package user

// Name is this package's own declared name; identical base name as v1.
const Name = "user"

// User is the v2 shape: string ID, split name, and a new Locale field.
type User struct {
	ID       string
	FullName string
	Locale   string
}
```

### The gateway aliases both imports

A single file cannot write `user.User` and mean two different packages. So the
gateway imports each under an explicit alias — `userv1` and `userv2` — and every
reference is unambiguous. Aliasing changes only the *local* identifier in this
file; it does not rename the packages themselves (which both still declare
`package user`). `Migrate` performs the field mapping a real versioned API layer
does at the boundary: convert the integer ID to a string, move `Name` to
`FullName`, and default the field that did not exist in the source version.

Create `gateway/gateway.go`:

```go
package gateway

import (
	"strconv"

	userv1 "example.com/userapi/api/v1/user"
	userv2 "example.com/userapi/api/v2/user"
)

// DefaultLocale is applied to migrated records, which predate the Locale field.
const DefaultLocale = "en-US"

// Migrate converts a v1 user into a v2 user, defaulting fields new in v2.
func Migrate(v1 userv1.User) userv2.User {
	return userv2.User{
		ID:       strconv.Itoa(v1.ID),
		FullName: v1.Name,
		Locale:   DefaultLocale,
	}
}
```

The un-aliased form does not compile. If you instead wrote:

```text
import (
	"example.com/userapi/api/v1/user"
	"example.com/userapi/api/v2/user"
)
```

the compiler rejects it with `user redeclared in this block` / `user redeclared as
imported package name`: two imports would bind the same identifier `user`. That
error is precisely why aliasing exists.

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	userv1 "example.com/userapi/api/v1/user"
	"example.com/userapi/gateway"
)

func main() {
	old := userv1.User{ID: 42, Name: "Ada Lovelace"}
	migrated := gateway.Migrate(old)
	fmt.Printf("v2: id=%s name=%q locale=%s\n",
		migrated.ID, migrated.FullName, migrated.Locale)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
v2: id=42 name="Ada Lovelace" locale=en-US
```

### Tests

The test verifies the field mapping and pins the conceptual point: the alias is a
*local* name, so both packages still report their own declared name (`Name ==
"user"`) regardless of what the gateway called them on import.

Create `gateway/gateway_test.go`:

```go
package gateway

import (
	"testing"

	userv1 "example.com/userapi/api/v1/user"
	userv2 "example.com/userapi/api/v2/user"
)

func TestMigrateMapsFields(t *testing.T) {
	t.Parallel()

	got := Migrate(userv1.User{ID: 7, Name: "Grace Hopper"})
	want := userv2.User{ID: "7", FullName: "Grace Hopper", Locale: DefaultLocale}
	if got != want {
		t.Fatalf("Migrate = %+v, want %+v", got, want)
	}
}

func TestMigrateDefaultsLocale(t *testing.T) {
	t.Parallel()

	got := Migrate(userv1.User{ID: 1, Name: "x"})
	if got.Locale != "en-US" {
		t.Fatalf("Locale = %q, want en-US", got.Locale)
	}
}

func TestAliasDoesNotRenamePackages(t *testing.T) {
	t.Parallel()

	// Aliased here as userv1/userv2, but each package still declares "user".
	if userv1.Name != "user" || userv2.Name != "user" {
		t.Fatalf("names = %q, %q, want both %q", userv1.Name, userv2.Name, "user")
	}
}
```

## Review

The gateway is correct when `Migrate` is a total function from the v1 shape to the
v2 shape, converting the ID type and defaulting every field new in v2. The alias
lesson is structural: two same-named packages cannot coexist in one file
unaliased, and the alias is a file-local identifier that leaves each package's
declared name untouched — `TestAliasDoesNotRenamePackages` makes that concrete.
Resist the dot-import shortcut here; `import . "…/v2/user"` would drop `User` into
the file scope and make the collision *worse*, not better, and would ruin
greppability. Aliasing is the disciplined tool; dot imports belong only in
test-only DSLs.

## Resources

- [Go Spec: Import declarations](https://go.dev/ref/spec#Import_declarations) — the alias and dot-import forms and their scoping rules.
- [Go Code Review Comments: import dot](https://go.dev/wiki/CodeReviewComments#import-dot) — why dot imports are discouraged outside tests.
- [`strconv.Itoa`](https://pkg.go.dev/strconv#Itoa) — the int-to-string conversion used in the migration.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-codec-registry-blank-import.md](03-codec-registry-blank-import.md) | Next: [05-observability-side-effect-imports.md](05-observability-side-effect-imports.md)
