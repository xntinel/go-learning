# Exercise 1: Domain IDs with Defined Types and a Legacy Migration Alias

Every service that touches more than one kind of identifier eventually ships a bug
where a user id lands in a slot that wanted an account id. This exercise builds the
identity package that makes that bug impossible to compile: `UserID` and
`AccountID` are distinct defined string types with validating constructors, and
`LegacyUserID` is an alias that models an old public name kept alive during a
migration.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
identity/                 independent module: example.com/identity
  go.mod                  go 1.24
  identity.go             UserID, AccountID (defined); LegacyUserID (alias);
                          NewUserID, NewAccountID, Valid, String, lookup-key helpers
  cmd/
    demo/
      main.go             builds ids, prints lookup keys, exercises the alias
  identity_test.go        table tests: separation, rejection, alias method set
```

- Files: `identity.go`, `cmd/demo/main.go`, `identity_test.go`.
- Implement: `UserID`/`AccountID` as defined types with `Valid()`/`String()`, `NewUserID`/`NewAccountID` validating constructors, `UserLookupKey`/`AccountLookupKey`, and `LegacyUserID = UserID`.
- Test: lookup keys are correct; constructors reject wrong-prefix input; the alias carries `UserID`'s methods and is assignment-compatible with `UserID`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/06-type-aliases-vs-type-definitions/01-domain-ids-and-legacy-alias/cmd/demo
cd go-solutions/02-variables-types-and-constants/06-type-aliases-vs-type-definitions/01-domain-ids-and-legacy-alias
go mod edit -go=1.24
```

### Why definitions here and an alias there

`UserID` and `AccountID` are both `string` in storage, and if you modeled them
with `type UserID = string` the compiler would see nothing but `string` and could
not stop you from passing an account id to a user function. Defining them
(`type UserID string`) makes them distinct types: `UserLookupKey` takes a `UserID`
and will not accept an `AccountID`, so the cross-domain call is a compile error
that never becomes a runtime test. That is the entire value proposition, and it is
free.

`LegacyUserID` is different. It models a name that some older, still-deployed
callers use for the user identifier. During the migration window both names must
mean the identical type so those callers keep compiling while they move to
`UserID`. That is precisely what an alias does: `type LegacyUserID = UserID` makes
`LegacyUserID` a synonym, sharing `UserID`'s method set and assignment-compatible
with it in both directions. If you had written `type LegacyUserID UserID` instead,
the two would be separate types and every legacy call site would need a conversion
— the opposite of a smooth migration.

Validation lives on the type as a method (`Valid`), and the constructors are the
single audited path from a raw string to a typed id. `String()` satisfies
`fmt.Stringer`, so the ids format cleanly in logs.

Create `identity.go`:

```go
package identity

import (
	"fmt"
	"strings"
)

// UserID and AccountID are DEFINED types: both are strings underneath, but they
// are distinct types, so the compiler refuses to substitute one for the other.
type UserID string
type AccountID string

// LegacyUserID is an ALIAS: an old public name for the exact same type as
// UserID, kept assignment-compatible during a deprecation window.
//
// Deprecated: use UserID. LegacyUserID exists only for callers not yet migrated.
type LegacyUserID = UserID

// Compile-time proof that both defined types satisfy fmt.Stringer.
var (
	_ fmt.Stringer = UserID("")
	_ fmt.Stringer = AccountID("")
)

// NewUserID trims and validates raw, returning a typed UserID or an error.
func NewUserID(raw string) (UserID, error) {
	id := UserID(strings.TrimSpace(raw))
	if !id.Valid() {
		return "", fmt.Errorf("invalid user id: %q", raw)
	}
	return id, nil
}

// NewAccountID trims and validates raw, returning a typed AccountID or an error.
func NewAccountID(raw string) (AccountID, error) {
	id := AccountID(strings.TrimSpace(raw))
	if !id.Valid() {
		return "", fmt.Errorf("invalid account id: %q", raw)
	}
	return id, nil
}

// Valid reports whether the user id has the required prefix and a non-empty body.
func (id UserID) Valid() bool {
	return strings.HasPrefix(string(id), "usr_") && len(id) > len("usr_")
}

// Valid reports whether the account id has the required prefix and a non-empty body.
func (id AccountID) Valid() bool {
	return strings.HasPrefix(string(id), "acct_") && len(id) > len("acct_")
}

func (id UserID) String() string    { return string(id) }
func (id AccountID) String() string { return string(id) }

// UserLookupKey builds the storage key for a user. It takes a UserID, so an
// AccountID cannot be passed here without an explicit (and suspicious) conversion.
func UserLookupKey(id UserID) string {
	return "users/" + id.String()
}

// AccountLookupKey builds the storage key for an account.
func AccountLookupKey(id AccountID) string {
	return "accounts/" + id.String()
}
```

### The compile-time guarantee

The single most important line in this exercise is one you cannot write:

```go
// UserLookupKey(accountID) // does not compile: AccountID is not a UserID
```

That is the guarantee. It cannot be a runtime test because it is rejected before
the program is ever built. The tests below verify everything that *can* run; the
domain separation itself is proven by the fact that the mixing call is a
compile error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/identity"
)

func main() {
	userID, err := identity.NewUserID(" usr_123 ")
	if err != nil {
		panic(err)
	}
	accountID, err := identity.NewAccountID("acct_456")
	if err != nil {
		panic(err)
	}

	fmt.Println("user lookup key:", identity.UserLookupKey(userID))
	fmt.Println("account lookup key:", identity.AccountLookupKey(accountID))

	// The alias shares UserID's method set and assignment-compatibility.
	var legacy identity.LegacyUserID = userID
	fmt.Println("legacy still valid:", legacy.Valid())
	fmt.Println("legacy lookup key:", identity.UserLookupKey(legacy))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user lookup key: users/usr_123
account lookup key: accounts/acct_456
legacy still valid: true
legacy lookup key: users/usr_123
```

### Tests

The tests prove three things: defined types produce the right lookup keys, the
constructors reject wrong-prefix (including empty-body) input, and the alias is
genuinely the same type as `UserID`. The last test assigns a `LegacyUserID` into a
`UserID` and back with no conversion — which only compiles because they are one
type.

Create `identity_test.go`:

```go
package identity

import "testing"

func TestLookupKeys(t *testing.T) {
	t.Parallel()

	userID, err := NewUserID(" usr_123 ")
	if err != nil {
		t.Fatalf("NewUserID: %v", err)
	}
	accountID, err := NewAccountID("acct_456")
	if err != nil {
		t.Fatalf("NewAccountID: %v", err)
	}

	if got, want := UserLookupKey(userID), "users/usr_123"; got != want {
		t.Errorf("UserLookupKey = %q, want %q", got, want)
	}
	if got, want := AccountLookupKey(accountID), "accounts/acct_456"; got != want {
		t.Errorf("AccountLookupKey = %q, want %q", got, want)
	}
}

func TestConstructorsRejectBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		build   func() error
		wantErr bool
	}{
		{"user wrong prefix", func() error { _, err := NewUserID("acct_456"); return err }, true},
		{"user empty body", func() error { _, err := NewUserID("usr_"); return err }, true},
		{"user only spaces", func() error { _, err := NewUserID("   "); return err }, true},
		{"user ok", func() error { _, err := NewUserID("usr_1"); return err }, false},
		{"account wrong prefix", func() error { _, err := NewAccountID("usr_123"); return err }, true},
		{"account empty body", func() error { _, err := NewAccountID("acct_"); return err }, true},
		{"account ok", func() error { _, err := NewAccountID("acct_1"); return err }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.build()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestLegacyAliasIsSameType(t *testing.T) {
	t.Parallel()

	var legacy LegacyUserID = "usr_123"

	// The alias carries UserID's method set.
	if !legacy.Valid() {
		t.Fatal("legacy alias should have UserID.Valid")
	}
	// The alias is accepted where a UserID is required.
	if got := UserLookupKey(legacy); got != "users/usr_123" {
		t.Fatalf("UserLookupKey(legacy) = %q", got)
	}
	// Assignment-compatible in both directions with no conversion.
	var current UserID = legacy
	var back LegacyUserID = current
	if back != "usr_123" {
		t.Fatalf("round-trip = %q", back)
	}
}
```

Add an `Example` so the demonstrated behavior is verified by `go test`:

```go
package identity

import "fmt"

func ExampleUserLookupKey() {
	id, _ := NewUserID("usr_42")
	fmt.Println(UserLookupKey(id))
	// Output: users/usr_42
}
```

## Review

The package is correct when the two constructors reject anything without the right
prefix and a non-empty body, the lookup keys are exact, and the alias round-trips
into `UserID` with no conversion. The mistake to avoid is modeling the ids as
aliases of `string` "to keep it simple" — that discards the only thing this
package buys you, which is that `UserLookupKey(accountID)` will not compile. The
second mistake is misreading `LegacyUserID`: it is an alias precisely because a
migration needs the old and new names to be one type; a definition there would
force a conversion at every legacy call site and defeat the purpose. Run
`go test -race` and `go vet ./...` to confirm the constructors and the alias
behave as described.

## Resources

- [Go Language Spec: Type definitions](https://go.dev/ref/spec#Type_definitions) — why defined types are distinct.
- [Go Language Spec: Alias declarations](https://go.dev/ref/spec#Alias_declarations) — the `type X = Y` form.
- [Go Blog: Type aliases](https://go.dev/blog/type-aliases) — the migration motivation.

---

Prev: [00-concepts.md](00-concepts.md) | Next: [02-money-cents-defined-type.md](02-money-cents-defined-type.md)
