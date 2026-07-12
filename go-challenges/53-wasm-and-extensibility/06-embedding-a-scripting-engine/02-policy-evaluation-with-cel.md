# Exercise 2: Authorization Policy Evaluation with CEL and Cost Limits

CEL is the expression language Kubernetes admission and Envoy authorization
already use. This exercise builds an ABAC-style policy checker in that shape:
compile and type-check a policy against a declared environment, expose one custom
host function, and evaluate it with a runtime cost budget and context-based
cancellation — failing closed on any error.

This module is fully self-contained: its own `go mod init`, its own types, demo,
and tests. Nothing here imports another exercise. It is a bar-mode module: it
depends on `github.com/google/cel-go`, so the offline gate cannot fetch it and
fails at build; the value is the correct two-phase shape, the verified APIs, and
tests that assert the real cost-limit and interrupt behavior once the dependency
is present.

## What you'll build

```text
policy/                      independent module: example.com/policy
  go.mod                     go 1.26; requires github.com/google/cel-go
  policy.go                  NewEnv; Compile -> *Policy; (*Policy).Allow (fail-closed); contains_ci
  cmd/
    demo/
      main.go                evaluate principal/resource/action requests against a policy
  policy_test.go             allow/deny table, compile type-error, cost-limit, cancellation, Example
```

Files: `policy.go`, `cmd/demo/main.go`, `policy_test.go`.
Implement: `NewEnv()` declaring typed variables and one `contains_ci` function via `cel.Function`/`cel.Overload`/`cel.BinaryBinding`; `Compile(env, src)` doing the two-phase `Env.Compile` then `Env.Program` with `CostLimit` and `InterruptCheckFrequency`; `(*Policy).Allow(ctx, req)` using `ContextEval` and failing closed.
Test: an allow/deny table over activation maps; a compile-time type-error rejection via `Issues.Err()`; a cost-limit-exceeded evaluation; a cancellation via `ContextEval` with a cancelled context.
Verify: `go test -race ./...` with the dependency present (bar-mode: the offline gate fails to build).

Set up the module:

```bash
mkdir -p go-solutions/53-wasm-and-extensibility/06-embedding-a-scripting-engine/02-policy-evaluation-with-cel/cmd/demo
cd go-solutions/53-wasm-and-extensibility/06-embedding-a-scripting-engine/02-policy-evaluation-with-cel
go mod edit -go=1.26
go get github.com/google/cel-go@v0.24.1
```

### The two-phase model

CEL splits work exactly where the concepts file says the cost lives. `Env.Compile`
parses and *type-checks* the source against the declared environment, returning a
type-checked AST and an `*Issues`. If `Issues.Err()` is non-nil the policy is
malformed — a type error, an unknown variable — and that is a config-load failure
you surface to the author, not a request-path 500. `Env.Program` then turns the
checked AST into an executable `cel.Program`, and this is where you attach runtime
options: `CostLimit` to bound work units, `InterruptCheckFrequency` to make long
comprehensions cancellable, and `EvalOptions(OptTrackCost)` so cost is actually
tracked. Compile once at load, cache the `Program`, evaluate many.

The declared environment is the sandbox surface. `cel.Variable` names each input
and its type: `principal` and `resource` are maps of string to `Dyn`, `action` is
a string. A policy can read only these. `cel.Function` with a `cel.Overload` and a
`cel.BinaryBinding` adds exactly one host function, `contains_ci`, whose binding
receives `ref.Val` arguments and returns a `ref.Val`. The binding is pure and
returns a `types.Err` rather than panicking on a bad argument — a host function
must never tear down the request goroutine.

### Failing closed

`Allow` is the security boundary, so it fails closed. It calls `ContextEval` (not
plain `Eval`) so a cancelled context can interrupt a running comprehension. Any
error — a cost-limit breach, an interrupt, a host-function failure — returns
`(false, err)`: deny. A result that is not a Go `bool` also denies. The only path
that returns `true` is an evaluation that completed and produced boolean true.
Mapping any other outcome to "allow" would be a security bug.

Create `policy.go`:

```go
package policy

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// costLimit bounds total work units per evaluation. Cost is a work-units model,
// not a wall-clock timeout.
const costLimit = 1000

// interruptCheckEvery makes ContextEval poll the context every N comprehension
// iterations, so a cancelled context can abort a long evaluation.
const interruptCheckEvery = 100

// NewEnv builds the type-checked environment policies are compiled against. The
// declared variables are the entire surface a policy can read; contains_ci is the
// only host function exposed.
func NewEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("principal", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("resource", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("action", cel.StringType),
		cel.Function("contains_ci",
			cel.Overload("contains_ci_string_string",
				[]*cel.Type{cel.StringType, cel.StringType},
				cel.BoolType,
				cel.BinaryBinding(containsCI),
			),
		),
	)
}

// containsCI reports case-insensitive substring containment. It returns a
// types.Err on a bad argument rather than panicking.
func containsCI(lhs, rhs ref.Val) ref.Val {
	h, ok := lhs.Value().(string)
	if !ok {
		return types.NewErr("contains_ci: first argument is not a string")
	}
	n, ok := rhs.Value().(string)
	if !ok {
		return types.NewErr("contains_ci: second argument is not a string")
	}
	return types.Bool(strings.Contains(strings.ToLower(h), strings.ToLower(n)))
}

// Policy is a compiled, type-checked, cost-bounded program, safe to cache and
// evaluate concurrently.
type Policy struct {
	Source  string
	program cel.Program
}

// Compile type-checks src against env and builds a cost-bounded, cancellable
// program. A type error fails here, at config-load time.
func Compile(env *cel.Env, src string) (*Policy, error) {
	ast, issues := env.Compile(src)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compile policy %q: %w", src, issues.Err())
	}
	program, err := env.Program(ast,
		cel.EvalOptions(cel.OptTrackCost),
		cel.CostLimit(costLimit),
		cel.InterruptCheckFrequency(interruptCheckEvery),
	)
	if err != nil {
		return nil, fmt.Errorf("program policy %q: %w", src, err)
	}
	return &Policy{Source: src, program: program}, nil
}

// Allow evaluates the policy against req and fails closed: any error, any
// non-bool result, or a false result denies. Only a completed evaluation that
// yields boolean true allows.
func (p *Policy) Allow(ctx context.Context, req map[string]any) (bool, error) {
	out, _, err := p.program.ContextEval(ctx, req)
	if err != nil {
		return false, fmt.Errorf("eval policy %q: %w", p.Source, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("policy %q returned %T, want bool", p.Source, out.Value())
	}
	return b, nil
}
```

### The runnable demo

The demo compiles one policy and evaluates three principal/resource/action
requests against it, showing an admin override and an owner-read rule.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"example.com/policy"
)

func main() {
	env, err := policy.NewEnv()
	if err != nil {
		log.Fatalf("env: %v", err)
	}
	src := `principal.role == "admin" || (action == "read" && resource.owner == principal.id)`
	p, err := policy.Compile(env, src)
	if err != nil {
		log.Fatalf("compile: %v", err)
	}

	reqs := []map[string]any{
		{"principal": map[string]any{"id": "u1", "role": "user"}, "resource": map[string]any{"owner": "u1"}, "action": "read"},
		{"principal": map[string]any{"id": "u2", "role": "user"}, "resource": map[string]any{"owner": "u1"}, "action": "read"},
		{"principal": map[string]any{"id": "u3", "role": "admin"}, "resource": map[string]any{"owner": "u1"}, "action": "write"},
	}
	for _, r := range reqs {
		ok, err := p.Allow(context.Background(), r)
		if err != nil {
			log.Fatalf("eval: %v", err)
		}
		pr := r["principal"].(map[string]any)
		res := r["resource"].(map[string]any)
		fmt.Printf("%s/%s owner=%s -> %v\n", pr["id"], r["action"], res["owner"], ok)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
u1/read owner=u1 -> true
u2/read owner=u1 -> false
u3/write owner=u1 -> true
```

### Tests

`TestAllow` runs an allow/deny table over activation maps, including a
`contains_ci` rule. `TestCompileRejectsTypeError` proves `Issues.Err()` catches a
string-plus-int type error at compile. `TestCostLimitExceeded` builds a program
with a low `CostLimit` over a comprehension against a large list and asserts the
`cost limit exceeded` error. `TestCancellation` builds a program with
`InterruptCheckFrequency(1)`, calls `ContextEval` with an already-cancelled
context, and asserts the `interrupted` error. The last two build their own
programs from the shared env so each limit is isolated from the other.

Create `policy_test.go`:

```go
package policy

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
)

func req(role, id, owner, action string) map[string]any {
	return map[string]any{
		"principal": map[string]any{"id": id, "role": role},
		"resource":  map[string]any{"owner": owner, "name": "Prod-DB"},
		"action":    action,
	}
}

func TestAllow(t *testing.T) {
	t.Parallel()
	env, err := NewEnv()
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}

	tests := []struct {
		name   string
		policy string
		req    map[string]any
		want   bool
	}{
		{"admin override", `principal.role == "admin"`, req("admin", "u3", "u1", "write"), true},
		{"owner read", `action == "read" && resource.owner == principal.id`, req("user", "u1", "u1", "read"), true},
		{"non-owner denied", `action == "read" && resource.owner == principal.id`, req("user", "u2", "u1", "read"), false},
		{"contains_ci match", `contains_ci(resource.name, "prod")`, req("user", "u1", "u1", "read"), true},
		{"contains_ci miss", `contains_ci(resource.name, "staging")`, req("user", "u1", "u1", "read"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := Compile(env, tc.policy)
			if err != nil {
				t.Fatalf("Compile(%q): %v", tc.policy, err)
			}
			got, err := p.Allow(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("Allow: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Allow(%q) = %v, want %v", tc.policy, got, tc.want)
			}
		})
	}
}

func TestCompileRejectsTypeError(t *testing.T) {
	t.Parallel()
	env, err := NewEnv()
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	// action is a string; adding an int is a type error caught by Issues.Err().
	if _, err := Compile(env, `action + 1 == "x"`); err == nil {
		t.Fatal("Compile accepted a string+int policy; want type error")
	}
}

func bigListActivation(n int) map[string]any {
	items := make([]any, n)
	for i := range n {
		items[i] = i
	}
	return map[string]any{
		"principal": map[string]any{"id": "u1", "role": "user"},
		"resource":  map[string]any{"owner": "u1", "items": items},
		"action":    "read",
	}
}

func TestCostLimitExceeded(t *testing.T) {
	t.Parallel()
	env, err := NewEnv()
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	ast, issues := env.Compile(`resource.items.all(i, i >= 0)`)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile: %v", issues.Err())
	}
	program, err := env.Program(ast, cel.EvalOptions(cel.OptTrackCost), cel.CostLimit(10))
	if err != nil {
		t.Fatalf("program: %v", err)
	}
	_, _, err = program.Eval(bigListActivation(1000))
	if err == nil {
		t.Fatal("comprehension ran within cost limit; want cost error")
	}
	if !strings.Contains(err.Error(), "cost limit exceeded") {
		t.Fatalf("error = %v, want cost limit exceeded", err)
	}
}

func TestCancellation(t *testing.T) {
	t.Parallel()
	env, err := NewEnv()
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	ast, issues := env.Compile(`resource.items.all(i, i >= 0)`)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile: %v", issues.Err())
	}
	program, err := env.Program(ast, cel.EvalOptions(cel.OptTrackCost), cel.InterruptCheckFrequency(1))
	if err != nil {
		t.Fatalf("program: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the first interrupt check aborts
	_, _, err = program.ContextEval(ctx, bigListActivation(1000))
	if err == nil {
		t.Fatal("evaluation completed despite cancelled context; want interrupt")
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("error = %v, want interrupted", err)
	}
}

func Example() {
	env, _ := NewEnv()
	p, _ := Compile(env, `principal.role == "admin"`)
	ok, _ := p.Allow(context.Background(), req("admin", "u3", "u1", "write"))
	fmt.Println(ok)
	// Output: true
}
```

## Review

The checker is correct when the only way to get `true` out of `Allow` is a
completed evaluation that produced boolean true, and every other outcome denies.
Confirm the fail-closed contract by reading `Allow`: a `ContextEval` error returns
`(false, err)`, a non-bool result returns `(false, err)`, and neither ever leaks a
`true`. Confirm the two runtime guards are distinct — `TestCostLimitExceeded`
trips a work-units bound with `cost limit exceeded`, `TestCancellation` trips a
context interrupt with `interrupted` — because cost is not time and a passing cost
test says nothing about cancellation.

The mistakes to avoid: setting `CostLimit` without `EvalOptions(OptTrackCost)` so
cost is never tracked and the limit never fires; expecting `CostLimit` to be a
wall-clock timeout (it is not — that is what `ContextEval` plus
`InterruptCheckFrequency` are for); using plain `Eval` on the request path, which
cannot be cancelled; recompiling per request instead of caching the `Program`;
and writing a host function that panics on a bad argument instead of returning a
`types.Err`. Report a config author's type mistake from `Issues.Err()` at load
time, never as a runtime 500.

## Resources

- [cel-go `cel` package](https://pkg.go.dev/github.com/google/cel-go/cel) — `NewEnv`, `Variable`, `Function`, `Overload`, `CostLimit`, `InterruptCheckFrequency`, `Program.ContextEval`.
- [CEL specification](https://github.com/google/cel-spec/blob/master/doc/langdef.md) — the language, its type system, and the non-Turing-complete design.
- [cel-go `common/types/ref`](https://pkg.go.dev/github.com/google/cel-go/common/types/ref) — the `ref.Val` interface and `Value()`.
- [Kubernetes CEL for admission](https://kubernetes.io/docs/reference/using-api/cel/) — a production embedding of CEL with cost limits.

---

Back to [01-rules-engine-with-expr.md](01-rules-engine-with-expr.md) | Next: [03-config-as-code-with-starlark.md](03-config-as-code-with-starlark.md)
