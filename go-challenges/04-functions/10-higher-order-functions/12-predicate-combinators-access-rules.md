# Exercise 12: All / Any / Not — Composing Access-Control Predicates

**Nivel: Intermedio** — validacion rapida (un test corto).

An authorization rule like "an admin may edit anything; otherwise the
caller must own the resource and hold the write scope" is a boolean
expression over smaller checks. `All` and `Any` fold a list of
`Predicate[T]` into one, short-circuiting exactly like `&&` and `||`, so the
rule reads as data instead of a nested if-ladder.

## What you'll build

```text
accessrules/                independent module: example.com/accessrules
  go.mod                    go 1.24
  access.go                 type Predicate[T]; All, Any, Not; CanEdit rule
  access_test.go            CanEdit table test, short-circuit tests, empty-case tests
```

- Files: `access.go`, `access_test.go`.
- Implement: `Predicate[T any] func(T) bool`, `All[T](preds ...Predicate[T]) Predicate[T]`, `Any[T](preds ...Predicate[T]) Predicate[T]`, `Not[T](pred Predicate[T]) Predicate[T]`, and a `CanEdit` rule built from them over a `Request` type.
- Test: `CanEdit` against five request shapes (admin, owner-with-scope, owner-without-scope, non-owner-with-scope, neither); `All`/`Any` short-circuit, proven by a predicate that must not run; the empty-predicate-list base cases.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Predicates as data, not as branches

`Predicate[T]` is the strategy shape: `func(T) bool`. `All` and `Any` are
factories that take a slice of predicates and return one predicate closed
over them — call `CanEdit(req)` and the fold runs underneath, but the call
site never sees the loop. Composing this way means the *rule* — who may
edit what — is expressed once, near the domain types, instead of scattered
across every handler that needs an authorization check.

Both combinators short-circuit for the same reason a hand-written `&&`/`||`
chain does: `All` stops at the first `false` (there is no way the whole
expression can still be true), and `Any` stops at the first `true`. This
matters when a later predicate is expensive — a database check for
ownership should not run if an earlier, cheaper admin check already
resolved the decision.

Create `access.go`:

```go
package accessrules

// Predicate reports whether a value satisfies some access rule.
type Predicate[T any] func(T) bool

// All returns a Predicate that is true only when every pred is true. It
// short-circuits on the first false, so an expensive later predicate never
// runs once an earlier one has already failed. All() with no predicates is
// vacuously true.
func All[T any](preds ...Predicate[T]) Predicate[T] {
	return func(v T) bool {
		for _, p := range preds {
			if !p(v) {
				return false
			}
		}
		return true
	}
}

// Any returns a Predicate that is true when at least one pred is true. It
// short-circuits on the first true. Any() with no predicates is vacuously
// false.
func Any[T any](preds ...Predicate[T]) Predicate[T] {
	return func(v T) bool {
		for _, p := range preds {
			if p(v) {
				return true
			}
		}
		return false
	}
}

// Not negates a Predicate.
func Not[T any](pred Predicate[T]) Predicate[T] {
	return func(v T) bool { return !pred(v) }
}

// Request is the subject of an access-control decision: the caller's role
// and granted scopes, and the resource owner being checked against.
type Request struct {
	Role    string
	Scopes  []string
	UserID  string
	OwnerID string
}

// IsAdmin is true when the caller's role is "admin".
func IsAdmin(r Request) bool {
	return r.Role == "admin"
}

// IsOwner is true when the caller is the resource owner.
func IsOwner(r Request) bool {
	return r.UserID != "" && r.UserID == r.OwnerID
}

// HasScope returns a Predicate true when scope is among the caller's
// granted scopes.
func HasScope(scope string) Predicate[Request] {
	return func(r Request) bool {
		for _, s := range r.Scopes {
			if s == scope {
				return true
			}
		}
		return false
	}
}

// CanEdit is the access rule for editing a resource: an admin may always
// edit; otherwise the caller must be the owner and hold the "write" scope.
var CanEdit = Any(
	IsAdmin,
	All(IsOwner, HasScope("write")),
)
```

### Tests

`TestCanEdit` walks five request shapes through the same rule: admin,
owner with the right scope, owner missing the scope, non-owner with the
scope, and a caller who is neither. `TestAllShortCircuits` and
`TestAnyShortCircuits` prove the short-circuit contract directly, by
failing if a second predicate that should never run sets a flag.
`TestEmptyCombinators` locks down the base cases, and `TestNot` checks the
negation.

Create `access_test.go`:

```go
package accessrules

import "testing"

func TestCanEdit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  Request
		want bool
	}{
		{
			name: "admin may edit anything",
			req:  Request{Role: "admin", UserID: "u1", OwnerID: "u2"},
			want: true,
		},
		{
			name: "owner with write scope may edit",
			req:  Request{Role: "member", UserID: "u1", OwnerID: "u1", Scopes: []string{"read", "write"}},
			want: true,
		},
		{
			name: "owner without write scope may not edit",
			req:  Request{Role: "member", UserID: "u1", OwnerID: "u1", Scopes: []string{"read"}},
			want: false,
		},
		{
			name: "non-owner with write scope may not edit",
			req:  Request{Role: "member", UserID: "u1", OwnerID: "u2", Scopes: []string{"write"}},
			want: false,
		},
		{
			name: "non-owner non-admin with no scopes may not edit",
			req:  Request{Role: "member", UserID: "u1", OwnerID: "u2"},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := CanEdit(tc.req); got != tc.want {
				t.Errorf("CanEdit(%+v) = %v, want %v", tc.req, got, tc.want)
			}
		})
	}
}

func TestAllShortCircuits(t *testing.T) {
	t.Parallel()

	var second bool
	pred := All(
		func(int) bool { return false },
		func(int) bool { second = true; return true },
	)

	if pred(0) {
		t.Fatal("All(false, true) = true, want false")
	}
	if second {
		t.Error("All did not short-circuit: second predicate ran after an earlier false")
	}
}

func TestAnyShortCircuits(t *testing.T) {
	t.Parallel()

	var second bool
	pred := Any(
		func(int) bool { return true },
		func(int) bool { second = true; return false },
	)

	if !pred(0) {
		t.Fatal("Any(true, false) = false, want true")
	}
	if second {
		t.Error("Any did not short-circuit: second predicate ran after an earlier true")
	}
}

func TestEmptyCombinators(t *testing.T) {
	t.Parallel()

	if !All[int]()(0) {
		t.Error("All()(0) = false, want true (vacuous truth)")
	}
	if Any[int]()(0) {
		t.Error("Any()(0) = true, want false (vacuous falsehood)")
	}
}

func TestNot(t *testing.T) {
	t.Parallel()

	always := func(int) bool { return true }
	if Not(always)(0) {
		t.Error("Not(always)(0) = true, want false")
	}
}
```

## Review

`CanEdit` is correct when it reads as the policy statement itself: "admin,
or (owner and write scope)" maps directly onto `Any(IsAdmin, All(IsOwner,
HasScope("write")))` with no translation step. The short-circuit tests are
the ones worth trusting over intuition — it is easy to assume Go evaluates
a variadic loop lazily without checking, and a predicate with a side effect
(a database call, in real code) makes the assumption observable. `All()`
and `Any()` with no predicates return the two extremes of boolean folds —
vacuous true and vacuous false — the same identities `&&` and `||` have
over an empty list.

## Resources

- [Go spec: Short variable declarations and closures](https://go.dev/ref/spec#Function_literals) — how `preds` is captured by the returned closure.
- [OWASP Authorization Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authorization_Cheat_Sheet.html) — the ownership/scope/role shape this exercise's `Request` models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-memoize-lookup-cache.md](11-memoize-lookup-cache.md) | Next: [13-instrumentation-decorator.md](13-instrumentation-decorator.md)
