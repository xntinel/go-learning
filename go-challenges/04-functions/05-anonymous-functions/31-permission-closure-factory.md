# Exercise 31: Authorization Middleware Built from Permission Closure Factory

**Nivel: Intermedio** — validacion rapida (un test corto).

Checking a role against a policy at every call site means every call site
needs a reference to the policy and re-derives the same lookup. This
module builds two closure factories instead: `NewChecker` closes over a
`Policy` and returns a permission check, and `Authorize` closes over both a
checker and a fixed `Role` to return a middleware that already knows
exactly who is calling.

This module is fully self-contained. Nothing here imports another
exercise.

## What you'll build

```text
authz/                        module example.com/authz
  go.mod
  authz.go                      Role, Policy, NewChecker, Middleware, Authorize
  authz_test.go                   checker table, gate runs/blocks next, per-role gates
  cmd/demo/main.go              admin and operator gates against one policy
```

- Files: `authz.go`, `authz_test.go`, `cmd/demo/main.go`.
- Implement: `Policy map[string][]Role`; `NewChecker(policy) func(Role, string) bool` closing over `policy`; `Authorize(policy, role) Middleware` closing over a checker and `role`, running or blocking `next`.
- Test: `NewChecker` against a table of role/action pairs; `Authorize`'s gate runs `next` when permitted and never runs it when denied; `next`'s own error propagates; two gates built from the same policy stay independent.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/31-permission-closure-factory/cmd/demo
cd go-solutions/04-functions/05-anonymous-functions/31-permission-closure-factory
go mod edit -go=1.24
```

### Two closure factories, stacked

`NewChecker(policy)` is the first factory: it returns a closure that has
`policy` baked in, so every subsequent call only needs `role` and `action`
— the policy never has to be passed around again. `Authorize(policy, role)`
is a second factory built on top of the first: it calls `NewChecker` once
internally to get `check`, then returns a `Middleware` closure that closes
over *both* `check` and `role`. The returned closure is what a call site
actually holds onto — one per role, typically built once at startup — and
it only ever needs an `action` and a `next` to run. Because `role` is fixed
inside the closure rather than passed as an argument to the middleware
itself, two gates built from `Authorize(policy, "admin")` and
`Authorize(policy, "operator")` are two independent closures that can never
be confused with each other or accidentally share a mutable `role`
variable — each one's `role` was captured once, at construction, and never
changes.

Create `authz.go`:

```go
package authz

import "fmt"

// Role identifies who is asking.
type Role string

// Policy maps an action name to the roles allowed to perform it.
type Policy map[string][]Role

func (p Policy) allows(role Role, action string) bool {
	for _, r := range p[action] {
		if r == role {
			return true
		}
	}
	return false
}

// NewChecker is a closure factory: given a policy, it returns a closure
// that closes over that policy and answers permission questions against
// it, without the policy ever needing to be threaded through every call
// site by hand.
func NewChecker(policy Policy) func(role Role, action string) bool {
	return func(role Role, action string) bool {
		return policy.allows(role, action)
	}
}

// Middleware is the shape of an authorization gate: given the action being
// attempted and the next step to run if it's permitted, it either runs next
// or returns a permission error.
type Middleware func(action string, next func() error) error

// Authorize is a second closure factory built on top of the first: it
// closes over both policy (via the checker it builds) and role, returning
// a Middleware that already knows exactly who is calling and what they're
// allowed to do, so call sites only ever supply the action and the work to
// run.
func Authorize(policy Policy, role Role) Middleware {
	check := NewChecker(policy)
	return func(action string, next func() error) error {
		if !check(role, action) {
			return fmt.Errorf("role %q is not permitted to %q", role, action)
		}
		return next()
	}
}
```

### The runnable demo

The demo builds an admin gate and an operator gate from the same policy and
runs them against two actions.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/authz"
)

func main() {
	policy := authz.Policy{
		"deploy":       {"admin"},
		"view-metrics": {"admin", "operator"},
	}

	adminGate := authz.Authorize(policy, "admin")
	operatorGate := authz.Authorize(policy, "operator")

	err := adminGate("deploy", func() error {
		fmt.Println("admin: deploy executed")
		return nil
	})
	fmt.Println("admin deploy err:", err)

	err = operatorGate("deploy", func() error {
		fmt.Println("operator: deploy executed") // must not print
		return nil
	})
	fmt.Println("operator deploy err:", err)

	err = operatorGate("view-metrics", func() error {
		fmt.Println("operator: view-metrics executed")
		return nil
	})
	fmt.Println("operator view-metrics err:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
admin: deploy executed
admin deploy err: <nil>
operator deploy err: role "operator" is not permitted to "deploy"
operator: view-metrics executed
operator view-metrics err: <nil>
```

### Tests

`TestNewCheckerClosesOverPolicy` runs a table of role/action pairs through
one checker. `TestAuthorizeRunsNextWhenPermitted` and `TestAuthorizeBlocksNextWhenDenied`
check the gate's core short-circuit behavior, using a `ran` flag to prove
`next` is never invoked on a denial. `TestAuthorizePropagatesNextsError`
checks a permitted-but-failing `next` still surfaces its own error.
`TestEachGateClosesOverItsOwnRole` builds three gates from one policy and
checks each is judged independently.

Create `authz_test.go`:

```go
package authz

import (
	"errors"
	"testing"
)

func TestNewCheckerClosesOverPolicy(t *testing.T) {
	t.Parallel()
	policy := Policy{"read": {"viewer", "admin"}}
	check := NewChecker(policy)

	cases := []struct {
		role   Role
		action string
		want   bool
	}{
		{"viewer", "read", true},
		{"admin", "read", true},
		{"guest", "read", false},
		{"viewer", "write", false},
	}
	for _, tc := range cases {
		if got := check(tc.role, tc.action); got != tc.want {
			t.Errorf("check(%q, %q) = %v, want %v", tc.role, tc.action, got, tc.want)
		}
	}
}

func TestAuthorizeRunsNextWhenPermitted(t *testing.T) {
	t.Parallel()
	policy := Policy{"deploy": {"admin"}}
	gate := Authorize(policy, "admin")

	ran := false
	err := gate("deploy", func() error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("gate() err = %v, want nil", err)
	}
	if !ran {
		t.Fatal("next was not run for a permitted action")
	}
}

func TestAuthorizeBlocksNextWhenDenied(t *testing.T) {
	t.Parallel()
	policy := Policy{"deploy": {"admin"}}
	gate := Authorize(policy, "operator")

	ran := false
	err := gate("deploy", func() error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("gate() err = nil, want a permission error")
	}
	if ran {
		t.Fatal("next ran for a denied action -- the gate must short-circuit")
	}
}

func TestAuthorizePropagatesNextsError(t *testing.T) {
	t.Parallel()
	policy := Policy{"deploy": {"admin"}}
	gate := Authorize(policy, "admin")
	sentinel := errors.New("deploy failed")

	err := gate("deploy", func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("gate() err = %v, want %v", err, sentinel)
	}
}

func TestEachGateClosesOverItsOwnRole(t *testing.T) {
	t.Parallel()
	policy := Policy{"view-metrics": {"admin", "operator"}}
	adminGate := Authorize(policy, "admin")
	operatorGate := Authorize(policy, "operator")
	guestGate := Authorize(policy, "guest")

	noop := func() error { return nil }
	if err := adminGate("view-metrics", noop); err != nil {
		t.Fatalf("adminGate: %v", err)
	}
	if err := operatorGate("view-metrics", noop); err != nil {
		t.Fatalf("operatorGate: %v", err)
	}
	if err := guestGate("view-metrics", noop); err == nil {
		t.Fatal("guestGate: want a permission error, got nil")
	}
}
```

## Review

`Authorize` is correct when its returned gate runs `next` on every
permitted action, never runs it on a denied one, and lets `next`'s own
error through untouched. The layering is what to internalize: `NewChecker`
alone is already useful wherever code just needs a yes/no answer, while
`Authorize` adds the `next`-running middleware shape on top without
`NewChecker` needing to know that shape exists. The mistake this pattern
guards against is building one gate per policy but reusing it across
roles by passing `role` as an argument to the gate instead of closing over
it at construction — that would let a caller accidentally invoke the gate
with the wrong role for a given call site, exactly the confusion that
baking `role` into the closure at `Authorize` time rules out.

## Resources

- [Go Language Specification: Function types](https://go.dev/ref/spec#Function_types)
- [Effective Go: Closures](https://go.dev/doc/effective_go#closures)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-batch-timeout-callback.md](30-batch-timeout-callback.md) | Next: [32-dlq-handler-literal.md](32-dlq-handler-literal.md)
