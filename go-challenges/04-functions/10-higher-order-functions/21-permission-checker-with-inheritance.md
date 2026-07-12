# Exercise 21: Permission Evaluator with Scope Inheritance Chain

**Nivel: Intermedio** — validacion rapida (un test corto).

A permission system rarely grants everything at the user level: most
grants come from an organization-wide policy, and some come from a
system-wide default that applies unless something more specific overrides
it. `WithInheritance` composes a chain of scope predicates — user, org,
system — into one `Checker` that walks them most-specific-first and grants
as soon as any scope says yes.

## What you'll build

```text
permission/                  independent module: example.com/permission
  go.mod                     go 1.24
  permission.go               type Checker; func WithInheritance, FromSet
  permission_test.go          user short-circuit, org/system fallback, deny-all, empty chain
```

- Files: `permission.go`, `permission_test.go`.
- Implement: `Checker func(permission string) bool`, `WithInheritance(scopes ...Checker) Checker`, and `FromSet(granted map[string]bool) Checker`.
- Test: a grant at the user scope short-circuits and never consults the org or system scope; a scope with no grant falls back to the next one in the chain; a chain where no scope grants the permission denies; an empty chain always denies.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/10-higher-order-functions/21-permission-checker-with-inheritance
cd go-solutions/04-functions/10-higher-order-functions/21-permission-checker-with-inheritance
go mod edit -go=1.24
```

### Fallback, not override — order encodes the hierarchy

`WithInheritance` takes any number of `Checker` values in most-specific-first
order and folds them into one: it loops through the scopes and returns
`true` the moment any of them grants the permission, short-circuiting
before consulting anything less specific. This is deliberately a fallback
chain, not a veto chain: a `Checker` here can only say "yes, granted" or
"no opinion" (`false`) — there is no way for a broader scope to *revoke* a
permission a narrower scope granted. That asymmetry mirrors how most real
permission systems work: an org-wide policy can hand out a permission a
specific user was never assigned directly, but a system-wide default
cannot take away something a user was explicitly granted.

The order of the arguments *is* the hierarchy. Passing `user, org, system`
means user grants are checked first (and short-circuit immediately, so a
common case resolves without ever touching the org or system checkers);
passing them in the wrong order would make an org-wide policy check happen
before — and mask timing/cost differences aside, functionally identically
to — a user-specific one, which is usually still correct here since this
chain has no veto semantics, but would misrepresent which scope "decided"
if a caller logs or audits which scope granted access.

`FromSet` is the simplest possible `Checker`: a fixed map standing in for
a real per-scope permission store (a user's row in a database, a
cached org policy document, a system-wide config file). Because `Checker`
is just `func(string) bool`, any of those real backends slots in without
changing `WithInheritance` at all.

Create `permission.go`:

```go
package permission

// Checker reports whether permission is granted at some scope — a user's
// own grants, an organization's, a system-wide default.
type Checker func(permission string) bool

// WithInheritance composes scopes, most specific first, into a single
// Checker that walks them in order and grants as soon as any scope
// grants — later scopes act as a fallback, not an override, so a broad
// system-level grant can cover a permission no single user or org was
// ever assigned directly. Scopes after the first one that grants are
// never consulted.
func WithInheritance(scopes ...Checker) Checker {
	scopes = append([]Checker(nil), scopes...)
	return func(permission string) bool {
		for _, scope := range scopes {
			if scope(permission) {
				return true
			}
		}
		return false
	}
}

// FromSet returns a Checker backed by a fixed set of granted permissions —
// a stand-in for a real per-scope permission store.
func FromSet(granted map[string]bool) Checker {
	return func(permission string) bool {
		return granted[permission]
	}
}
```

### Tests

`TestWithInheritanceUserGrantShortCircuits` proves the short-circuit
directly, the same way Exercise 14's `Fallback` proved it: the org
`Checker` is wrapped with a call counter, and the test asserts that
counter stays at zero when the user scope already grants the permission —
the return value alone couldn't distinguish "org was never asked" from
"org happened to agree." `TestWithInheritanceFallsBackToOrg` and
`TestWithInheritanceFallsBackThroughAllScopes` cover falling through one
and two empty scopes respectively. `TestWithInheritanceDeniesWhenNoScopeGrants`
and `TestWithInheritanceEmptyScopesAlwaysDenies` cover the two ways a
chain can end up with no grant: every scope says no, or there are no
scopes at all.

Create `permission_test.go`:

```go
package permission

import "testing"

// counting wraps a Checker so a test can assert whether it was ever
// consulted, proving the short-circuit behavior directly.
func counting(c Checker) (wrapped Checker, calls *int) {
	n := 0
	return func(permission string) bool {
		n++
		return c(permission)
	}, &n
}

func TestWithInheritanceUserGrantShortCircuits(t *testing.T) {
	t.Parallel()

	user := FromSet(map[string]bool{"invoices:read": true})
	org, orgCalls := counting(FromSet(map[string]bool{"invoices:read": true}))

	check := WithInheritance(user, org)

	if !check("invoices:read") {
		t.Fatal("expected the user scope to grant invoices:read")
	}
	if *orgCalls != 0 {
		t.Fatalf("org scope was called %d times, want 0 (user already granted)", *orgCalls)
	}
}

func TestWithInheritanceFallsBackToOrg(t *testing.T) {
	t.Parallel()

	user := FromSet(map[string]bool{}) // no direct grant
	org := FromSet(map[string]bool{"invoices:read": true})

	check := WithInheritance(user, org)

	if !check("invoices:read") {
		t.Fatal("expected the org scope to grant invoices:read as a fallback")
	}
}

func TestWithInheritanceFallsBackThroughAllScopes(t *testing.T) {
	t.Parallel()

	user := FromSet(map[string]bool{})
	org := FromSet(map[string]bool{})
	system := FromSet(map[string]bool{"invoices:read": true})

	check := WithInheritance(user, org, system)

	if !check("invoices:read") {
		t.Fatal("expected the system scope to grant invoices:read")
	}
}

func TestWithInheritanceDeniesWhenNoScopeGrants(t *testing.T) {
	t.Parallel()

	user := FromSet(map[string]bool{})
	org := FromSet(map[string]bool{})
	system := FromSet(map[string]bool{})

	check := WithInheritance(user, org, system)

	if check("invoices:delete") {
		t.Fatal("expected invoices:delete to be denied when no scope grants it")
	}
}

func TestWithInheritanceEmptyScopesAlwaysDenies(t *testing.T) {
	t.Parallel()

	check := WithInheritance()
	if check("anything") {
		t.Fatal("a Checker with no scopes must always deny")
	}
}
```

## Review

`WithInheritance` is correct when it grants on the first scope that says
yes and never consults anything after it — the `orgCalls` counter in the
short-circuit test is what actually proves that, since the return value
alone cannot tell "org was skipped" from "org agreed." Because a
`Checker` can only grant or abstain, never revoke, composing scopes in the
wrong order changes nothing about the final grant decision, only about
which scope gets credit for it — a distinction that matters the moment a
caller logs or audits which scope granted access, but not for the
boolean result itself. The same `Checker` shape composes over any number
of scope levels: adding a fourth scope (a project-level policy, say)
between org and system is one more argument to `WithInheritance`, not a
new code path.

## Resources

- [Effective Go: Interfaces and other types](https://go.dev/doc/effective_go#interfaces_and_types) — depending on a function type instead of an interface with one method.
- [AWS IAM: Policy evaluation logic](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_evaluation-logic.html) — a real-world permission system with explicit deny vs. implicit-deny-as-fallback semantics, worth contrasting with this chain's grant-only fallback.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-filter-and-map-with-transform-error.md](20-filter-and-map-with-transform-error.md) | Next: [22-reduce-fold-with-early-stop.md](22-reduce-fold-with-early-stop.md)
