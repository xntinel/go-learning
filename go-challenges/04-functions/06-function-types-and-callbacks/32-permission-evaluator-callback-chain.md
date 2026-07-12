# Exercise 32: Hierarchical Permission Evaluation via Composable Callbacks

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

Policy-as-code systems build authorization decisions from small, reusable
predicates — "has this role", "owns this resource" — combined with
boolean logic into policies that read like the rule they encode: an admin
may always act, *or* a member who owns the resource may. This module
builds that as `Predicate` callbacks composed with `All`, `Any`, and
`Not`, each one short-circuiting correctly and propagating an evaluation
error (a failed downstream lookup) as something distinct from a plain
deny.

## What you'll build

```text
permz/                        independent module: example.com/permission-evaluator-callback-chain
  go.mod                       go 1.24
  permz.go                     type Predicate, func All, Any, Not, HasRole, ResourceIs, IsOwner, AuditingPredicate
  cmd/
    demo/
      main.go                    runnable demo: an admin-or-owner policy evaluated across four contexts
  permz_test.go                  All/Any short-circuit, Not, error propagation, a hierarchical table, concurrency (-race)
```

Files: `permz.go`, `cmd/demo/main.go`, `permz_test.go`.
Implement: `type Predicate func(ctx Context) (bool, error)`, `func All(preds ...Predicate) Predicate`, `func Any(preds ...Predicate) Predicate`, `func Not(p Predicate) Predicate`, plus `HasRole`, `ResourceIs`, `IsOwner` predicate constructors and `AuditingPredicate` for observability; `All` stops at the first `false` or error, `Any` stops at the first `true` or error, and an error from any predicate must propagate rather than be swallowed as a deny.
Test: `All` never evaluates a predicate after one reports false; `Any` never evaluates a predicate after one reports true; `Not` inverts a predicate's result; an error from a predicate propagates through both `All` and `Any` and stops evaluation; a table of contexts against a hierarchical `Any(HasRole("admin"), All(ResourceIs(...), IsOwner))` policy; concurrent evaluation of one policy tree with an audit-logging predicate is race-free.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why an evaluation error must never collapse into "denied"

`Predicate` returns `(bool, error)`, not just `bool`, because "the caller
does not have this permission" and "we could not determine whether the
caller has this permission" are different failures with different
correct responses — the first is a normal deny, the second should
probably fail the whole request rather than silently deny it and look
like a policy decision. `All` and `Any` both treat an error the same way:
return it immediately, before even inspecting the boolean, and never call
another predicate in the chain. That is what makes `Predicate` trees
compose the way boolean expressions do in any language with short-circuit
evaluation — `All(a, b, c)` is exactly `a() && b() && c()`, and `Any(a, b,
c)` is exactly `a() || b() || c()`, error-propagation included. Nesting
`All` inside `Any` (`Any(HasRole("admin"), All(ResourceIs("reports"),
IsOwner))`) reads as the actual policy sentence it encodes — "admin, or
(resource is reports and caller owns it)" — with no case statement or
policy engine in sight, just function composition. `AuditingPredicate`
wraps any predicate to log its evaluation under a mutex without changing
its decision at all, which is the same "callback wraps callback"
technique used for logging and metrics middleware everywhere else in this
chapter, applied to a boolean-returning callback instead of one that
returns a response.

Create `permz.go`:

```go
// Package permz evaluates hierarchical permission policies built from
// small Predicate callbacks composed with All, Any, and Not, the shape
// policy-as-code systems use to combine org-level, role-level, and
// resource-level rules into one decision.
package permz

import (
	"sync"
)

// Context is everything a Predicate needs to decide.
type Context struct {
	UserID   string
	Roles    []string
	Resource string
	Action   string
	Attrs    map[string]any
}

// Predicate decides whether ctx is allowed, or reports an evaluation
// error (e.g. a downstream lookup failed) that must abort evaluation
// rather than be treated as a silent deny.
type Predicate func(ctx Context) (bool, error)

// All composes preds so every one must report true, short-circuiting at
// the first false or the first error.
func All(preds ...Predicate) Predicate {
	return func(ctx Context) (bool, error) {
		for _, p := range preds {
			ok, err := p(ctx)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	}
}

// Any composes preds so at least one must report true, short-circuiting
// at the first true or the first error.
func Any(preds ...Predicate) Predicate {
	return func(ctx Context) (bool, error) {
		for _, p := range preds {
			ok, err := p(ctx)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
}

// Not inverts p's boolean result and passes an error through unchanged.
func Not(p Predicate) Predicate {
	return func(ctx Context) (bool, error) {
		ok, err := p(ctx)
		if err != nil {
			return false, err
		}
		return !ok, nil
	}
}

// HasRole reports whether ctx.Roles contains role.
func HasRole(role string) Predicate {
	return func(ctx Context) (bool, error) {
		for _, r := range ctx.Roles {
			if r == role {
				return true, nil
			}
		}
		return false, nil
	}
}

// ResourceIs reports whether ctx.Resource equals resource.
func ResourceIs(resource string) Predicate {
	return func(ctx Context) (bool, error) {
		return ctx.Resource == resource, nil
	}
}

// IsOwner reports whether ctx.Attrs["owner"] equals ctx.UserID.
func IsOwner(ctx Context) (bool, error) {
	owner, _ := ctx.Attrs["owner"].(string)
	return owner != "" && owner == ctx.UserID, nil
}

// AuditingPredicate wraps p so every evaluation is appended to log under
// mu, for observability without changing p's own decision.
func AuditingPredicate(mu *sync.Mutex, log *[]string, name string, p Predicate) Predicate {
	return func(ctx Context) (bool, error) {
		ok, err := p(ctx)
		mu.Lock()
		*log = append(*log, name)
		mu.Unlock()
		return ok, err
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/permission-evaluator-callback-chain"
)

func main() {
	// Hierarchical policy: an admin may always act, OR (the resource is
	// "reports" AND the caller owns it).
	canAccessReport := permz.Any(
		permz.HasRole("admin"),
		permz.All(permz.ResourceIs("reports"), permz.IsOwner),
	)

	cases := []permz.Context{
		{UserID: "u1", Roles: []string{"admin"}, Resource: "reports"},
		{UserID: "u2", Roles: []string{"member"}, Resource: "reports", Attrs: map[string]any{"owner": "u2"}},
		{UserID: "u3", Roles: []string{"member"}, Resource: "reports", Attrs: map[string]any{"owner": "u2"}},
		{UserID: "u4", Roles: []string{"member"}, Resource: "invoices", Attrs: map[string]any{"owner": "u4"}},
	}

	for _, ctx := range cases {
		ok, err := canAccessReport(ctx)
		fmt.Printf("user=%s resource=%s allowed=%v err=%v\n", ctx.UserID, ctx.Resource, ok, err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user=u1 resource=reports allowed=true err=<nil>
user=u2 resource=reports allowed=true err=<nil>
user=u3 resource=reports allowed=false err=<nil>
user=u4 resource=invoices allowed=false err=<nil>
```

### Tests

Create `permz_test.go`:

```go
package permz

import (
	"errors"
	"sync"
	"testing"
)

func TestAllShortCircuitsAtFirstFalseWithoutEvaluatingLater(t *testing.T) {
	t.Parallel()
	laterCalled := false
	later := func(ctx Context) (bool, error) {
		laterCalled = true
		return true, nil
	}
	always := func(ctx Context) (bool, error) { return false, nil }

	all := All(always, later)
	ok, err := all(Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("All should report false")
	}
	if laterCalled {
		t.Fatal("predicate after a false one should not run")
	}
}

func TestAnyShortCircuitsAtFirstTrueWithoutEvaluatingLater(t *testing.T) {
	t.Parallel()
	laterCalled := false
	later := func(ctx Context) (bool, error) {
		laterCalled = true
		return false, nil
	}
	always := func(ctx Context) (bool, error) { return true, nil }

	any := Any(always, later)
	ok, err := any(Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("Any should report true")
	}
	if laterCalled {
		t.Fatal("predicate after a true one should not run")
	}
}

func TestNotInvertsResult(t *testing.T) {
	t.Parallel()
	alwaysTrue := func(ctx Context) (bool, error) { return true, nil }
	ok, err := Not(alwaysTrue)(Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("Not(true) should be false")
	}
}

func TestErrorFromPredicatePropagatesAndStopsEvaluation(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("lookup failed")
	failing := func(ctx Context) (bool, error) { return false, sentinel }
	laterCalled := false
	later := func(ctx Context) (bool, error) {
		laterCalled = true
		return true, nil
	}

	_, err := All(failing, later)(Context{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if laterCalled {
		t.Fatal("predicate after a failing one should not run")
	}

	laterCalled = false
	_, err = Any(failing, later)(Context{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if laterCalled {
		t.Fatal("predicate after a failing one should not run")
	}
}

func TestHierarchicalPolicyTableDriven(t *testing.T) {
	t.Parallel()
	policy := Any(
		HasRole("admin"),
		All(ResourceIs("reports"), IsOwner),
	)

	cases := []struct {
		name string
		ctx  Context
		want bool
	}{
		{
			name: "admin always allowed",
			ctx:  Context{UserID: "u1", Roles: []string{"admin"}, Resource: "invoices"},
			want: true,
		},
		{
			name: "owner of reports allowed",
			ctx:  Context{UserID: "u2", Roles: []string{"member"}, Resource: "reports", Attrs: map[string]any{"owner": "u2"}},
			want: true,
		},
		{
			name: "non-owner of reports denied",
			ctx:  Context{UserID: "u3", Roles: []string{"member"}, Resource: "reports", Attrs: map[string]any{"owner": "u2"}},
			want: false,
		},
		{
			name: "owner of a different resource denied",
			ctx:  Context{UserID: "u4", Roles: []string{"member"}, Resource: "invoices", Attrs: map[string]any{"owner": "u4"}},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := policy(tc.ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.want {
				t.Fatalf("policy(%+v) = %v, want %v", tc.ctx, ok, tc.want)
			}
		})
	}
}

func TestConcurrentEvaluationWithAuditingPredicateIsRaceFree(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var log []string
	policy := All(
		AuditingPredicate(&mu, &log, "role-check", HasRole("member")),
		AuditingPredicate(&mu, &log, "resource-check", ResourceIs("reports")),
	)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = policy(Context{Roles: []string{"member"}, Resource: "reports"})
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(log) != 100 {
		t.Fatalf("log entries = %d, want 100 (2 predicates x 50 evaluations)", len(log))
	}
}
```

## Review

The composition is correct exactly when it behaves like the boolean
operators it mirrors: `TestAllShortCircuitsAtFirstFalseWithoutEvaluatingLater`
and `TestAnyShortCircuitsAtFirstTrueWithoutEvaluatingLater` both prove
short-circuiting by asserting a predicate placed after the deciding one
never runs at all, which is the only way to tell "short-circuits" apart
from "evaluates everything but ignores the rest". `TestErrorFromPredicatePropagatesAndStopsEvaluation`
is the test that would fail first if someone "simplified" `All`/`Any` to
ignore the error return and just check the bool — a policy-as-code system
that quietly denies on a broken downstream lookup instead of failing loud
is a production incident waiting to happen.
`TestHierarchicalPolicyTableDriven` demonstrates the actual selling point:
nesting `All` inside `Any` produces exactly the rule described in
English, with the four table cases covering admin-bypass, legitimate
ownership, illegitimate ownership claim, and ownership of the wrong
resource. The concurrency test does not need a real audit sink — it only
needs to prove that wrapping a `Predicate` with logging does not
introduce a race, which holds because `AuditingPredicate` never touches
anything outside the mutex-protected `log` slice.

## Resources

- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [Cedar Policy Language (a production policy-as-code engine)](https://www.cedarpolicy.com/en/documentation)
- [Open Policy Agent: Rego (predicate-based policy language)](https://www.openpolicyagent.org/docs/latest/policy-language/)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-payment-processor-adapter.md](31-payment-processor-adapter.md) | Next: [33-resource-factory-lifecycle-callback.md](33-resource-factory-lifecycle-callback.md)
