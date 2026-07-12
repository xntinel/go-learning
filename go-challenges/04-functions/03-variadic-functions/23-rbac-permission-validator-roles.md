# Exercise 23: Role-Based Access Control Permission Validator

**Nivel: Intermedio** — validacion rapida (un test corto).

An authorization check for a given action is often "grant access if the user
is an admin, OR the owner, OR has the specific role for this action" — any
one of several independent conditions is enough. The natural signature is a
variadic list of predicates checked in order, stopping the moment one of
them grants access, since predicates further down the list (an expensive
one that queries a policy service, say) should never run once a cheap one
already answered yes.

## What you'll build

```text
rbac/                       independent module: example.com/rbac
  go.mod                    go 1.24
  rbac.go                   package rbac; type Predicate func([]string) bool; HasRole(role string) Predicate; Allow(userRoles []string, predicates ...Predicate) bool
  cmd/
    demo/
      main.go               runnable demo: an editor allowed to publish, denied to delete a tenant
  rbac_test.go              table tests: grants and denials, no predicates, and a call-counting short-circuit test
```

- Files: `rbac.go`, `cmd/demo/main.go`, `rbac_test.go`.
- Implement: `type Predicate func(roles []string) bool`, `HasRole(role string) Predicate`, and `Allow(userRoles []string, predicates ...Predicate) bool` that returns true at the first predicate that grants access.
- Test: a user roles set matching any one of several predicates is allowed; a user matching none is denied; zero predicates always denies; a predicate after the first one that grants access is never called.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/23-rbac-permission-validator-roles/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/23-rbac-permission-validator-roles
go mod edit -go=1.24
```

### `Predicate` as the unit of "OR", and why short-circuiting is the contract

`Allow(userRoles []string, predicates ...Predicate)` treats its variadic
list as a chain of independent, OR'd conditions: access is granted the
moment *any one* predicate returns true, so `Allow` returns from inside the
loop as soon as that happens instead of evaluating the rest. That is the
opposite default from the WHERE-predicate combinator in exercise 20, which
deliberately evaluates every predicate to aggregate failures — here the
predicates aren't reporting failures to collect, they are alternative ways
to grant the *same* action, and a real system might list a cheap in-memory
role check first and a slower remote policy-service predicate last. Once one
predicate grants, running the rest would be pure wasted work (and, for a
predicate with side effects like an audit log write, could even be
incorrect to run).

Zero predicates is a deliberate "deny by default": `Allow(roles)` with no
predicates at all returns `false`, not `true` — an action that nobody has
wired any grant rule for must never silently pass every check by having no
checks to fail. This mirrors the wider RBAC principle that the *absence* of
an explicit grant rule is denial, not an accident to special-case around.

Create `rbac.go`:

```go
// rbac.go
package rbac

// Predicate reports whether a user holding roles should be granted access
// under some rule (a single required role, a combination, and so on).
type Predicate func(roles []string) bool

// HasRole returns a Predicate that grants access when roles contains role.
func HasRole(role string) Predicate {
	return func(roles []string) bool {
		for _, r := range roles {
			if r == role {
				return true
			}
		}
		return false
	}
}

// Allow reports whether userRoles satisfies at least one of predicates. It
// evaluates predicates in order and stops at the first one that grants
// access — later predicates are never called once one has already granted,
// so an expensive predicate (one that calls out to a policy service, say)
// listed last only runs when every cheaper check before it has failed. Zero
// predicates always denies: an action with no predicate that can grant it is
// denied by default.
func Allow(userRoles []string, predicates ...Predicate) bool {
	for _, p := range predicates {
		if p(userRoles) {
			return true
		}
	}
	return false
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/rbac"
)

func main() {
	userRoles := []string{"editor"}

	allowed := rbac.Allow(userRoles,
		rbac.HasRole("admin"),
		rbac.HasRole("owner"),
		rbac.HasRole("editor"),
	)
	fmt.Println("editor can publish:", allowed)

	denied := rbac.Allow(userRoles, rbac.HasRole("admin"), rbac.HasRole("owner"))
	fmt.Println("editor can delete tenant:", denied)

	fmt.Println("no predicates:", rbac.Allow(userRoles))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
editor can publish: true
editor can delete tenant: false
no predicates: false
```

### Tests

`TestAllowShortCircuitsOnFirstGrant` is the one that proves the contract in
the exercise title: three call-counting predicates are given where the
second one grants, and the test asserts the third predicate's name never
appears in the recorded call list.

Create `rbac_test.go`:

```go
// rbac_test.go
package rbac

import (
	"slices"
	"testing"
)

func TestAllowGrantsWhenAnyPredicateMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		roles []string
		preds []Predicate
		want  bool
	}{
		{"matches last predicate", []string{"editor"}, []Predicate{HasRole("admin"), HasRole("owner"), HasRole("editor")}, true},
		{"matches first predicate", []string{"admin"}, []Predicate{HasRole("admin"), HasRole("owner")}, true},
		{"matches none", []string{"viewer"}, []Predicate{HasRole("admin"), HasRole("owner")}, false},
		{"no roles at all", nil, []Predicate{HasRole("admin")}, false},
		{"no predicates always denies", []string{"admin"}, nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Allow(tc.roles, tc.preds...)
			if got != tc.want {
				t.Fatalf("Allow(%v, ...) = %v, want %v", tc.roles, got, tc.want)
			}
		})
	}
}

func TestAllowShortCircuitsOnFirstGrant(t *testing.T) {
	t.Parallel()

	var calls []string
	track := func(name string, grant bool) Predicate {
		return func(roles []string) bool {
			calls = append(calls, name)
			return grant
		}
	}

	got := Allow([]string{"editor"}, track("admin", false), track("owner", true), track("editor", true))
	if !got {
		t.Fatalf("Allow = false, want true")
	}
	if want := []string{"admin", "owner"}; !slices.Equal(calls, want) {
		t.Fatalf("predicate calls = %v, want %v (editor predicate must never run)", calls, want)
	}
}
```

## Review

`Allow` is correct when it returns true as soon as any predicate grants
access and false when none do, when zero predicates always deny rather than
vacuously granting, and — the property a naive "range over predicates and
OR the results together" implementation misses — when a predicate listed
after one that already granted is never invoked at all. The senior point is
that short-circuiting here is not a micro-optimization but a correctness and
safety property: predicates are ordinary Go functions and can have side
effects (metrics, audit logs, calls to a remote policy engine), so "never
calls what it doesn't need to" is part of the contract, not just an
implementation detail. The mistake to avoid is writing `Allow` with a
`granted := false` accumulator and a loop that always runs to completion
(`if p(userRoles) { granted = true }` with no `return`/`break`) — it computes
the same boolean but silently drops the short-circuit guarantee.

## Resources

- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)
- [NIST: Role-Based Access Control](https://csrc.nist.gov/projects/role-based-access-control) — the deny-by-default principle this validator follows.
- [`slices.Equal`](https://pkg.go.dev/slices#Equal) — comparing the recorded call order in the short-circuit test.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-dns-resolver-fallback-chain.md](22-dns-resolver-fallback-chain.md) | Next: [24-oauth-scope-claim-verifier.md](24-oauth-scope-claim-verifier.md)
