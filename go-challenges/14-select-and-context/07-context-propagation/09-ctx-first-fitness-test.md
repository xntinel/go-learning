# Exercise 9: A Fitness Test That Enforces the ctx-First Convention

Every rule in this lesson depends on one structural convention: `ctx` is the first
parameter of every method that does work. A convention enforced only by code
review eventually rots — one merged pull request with a `context.Background()`
mid-chain undoes it. This final exercise turns the convention into a test:
reflection walks a registry of services and fails when any exported method breaks
the context-first shape, so CI catches the propagation regression before it ships.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
ctxfit/                      independent module: example.com/ctxfit
  go.mod                     go 1.24
  ctxfit.go                  CheckContextFirst (reflection over a registry); Violation;
                             MustHaveContextFirst; sample good + broken services
  cmd/
    demo/
      main.go                run the check over a mixed registry, print violations
  ctxfit_test.go             good registry is clean, getters skipped, broken type detected
```

Files: `ctxfit.go`, `cmd/demo/main.go`, `ctxfit_test.go`.
Implement: `CheckContextFirst(registry, allow)` that, for every exported method
taking parameters, asserts `method.Type.In(1)` implements `context.Context`,
returning a `[]Violation`; a `MustHaveContextFirst` that panics on any violation;
sample conforming and non-conforming services.
Test: a registry of conforming services yields no violations; zero-argument
accessors are exempt; a deliberately broken type yields a non-empty violation list.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/07-context-propagation/09-ctx-first-fitness-test/cmd/demo
cd go-solutions/14-select-and-context/07-context-propagation/09-ctx-first-fitness-test
go mod edit -go=1.24
```

### Turning a convention into a gate

"ctx is the first parameter" is a *structural* property of a method's signature,
and structural properties are exactly what `reflect` can check. The interface type
to compare against is obtained with the standard idiom
`reflect.TypeOf((*context.Context)(nil)).Elem()` — you cannot call
`reflect.TypeOf` on an interface value directly (it would report the dynamic type),
so you take the type of a nil `*context.Context` and unwrap one pointer with
`Elem()` to get the interface type itself.

For each value in the registry, `reflect.TypeOf(v).NumMethod()` and `.Method(i)`
enumerate its exported methods (reflection only surfaces exported methods on a
concrete type, which is precisely the surface a caller can reach). A method's
`Type` is a function type whose `In(0)` is the receiver, so the first *real*
parameter is `In(1)`. The check has two exemptions. A method with no parameters
(`NumIn() == 1`, receiver only) is an accessor or getter — `Name()`, `ID()` — and
has nothing to propagate, so it is skipped. And a small named allowlist covers the
rare documented exception (a `String()` for `fmt.Stringer`, say). Everything else
must have `In(1).Implements(contextType)`, or it is a violation.

`CheckContextFirst` returns every violation rather than stopping at the first, so
one test run reports all offenders. `MustHaveContextFirst` wraps it for use in a
package `init` or a `TestMain`, panicking with the full list — the harsh version
you wire into a package that must never regress. The payoff is that adding a method
like `func (s *Service) Delete(id int) error` — no context, the seed of a
propagation bug — turns from a thing a reviewer might miss into a red CI run.

Create `ctxfit.go`:

```go
package ctxfit

import (
	"context"
	"fmt"
	"reflect"
	"strings"
)

// contextType is the reflect.Type of the context.Context interface.
var contextType = reflect.TypeOf((*context.Context)(nil)).Elem()

// Violation records one method that breaks the context-first convention.
type Violation struct {
	Type   string
	Method string
	Reason string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s.%s: %s", v.Type, v.Method, v.Reason)
}

// CheckContextFirst inspects every exported method of each value in registry and
// returns a violation for any method that takes parameters whose first parameter
// is not a context.Context. Zero-argument methods (accessors) are exempt, as are
// method names present in allow.
func CheckContextFirst(registry []any, allow map[string]bool) []Violation {
	var violations []Violation
	for _, v := range registry {
		t := reflect.TypeOf(v)
		typeName := t.String()
		for i := range t.NumMethod() {
			m := t.Method(i)
			if allow[m.Name] {
				continue
			}
			// m.Type.In(0) is the receiver; In(1) is the first real parameter.
			if m.Type.NumIn() < 2 {
				continue // zero-arg accessor: nothing to propagate
			}
			if !m.Type.In(1).Implements(contextType) {
				violations = append(violations, Violation{
					Type:   typeName,
					Method: m.Name,
					Reason: fmt.Sprintf("first parameter is %s, want context.Context", m.Type.In(1)),
				})
			}
		}
	}
	return violations
}

// MustHaveContextFirst panics if any registered value violates the convention.
// Wire it into a package init or TestMain to fail fast.
func MustHaveContextFirst(registry []any, allow map[string]bool) {
	if vs := CheckContextFirst(registry, allow); len(vs) > 0 {
		lines := make([]string, len(vs))
		for i, v := range vs {
			lines[i] = v.String()
		}
		panic("ctxfit: context-first violations:\n" + strings.Join(lines, "\n"))
	}
}

// --- sample services used by the demo and tests ---

// OrderService conforms: every worker method takes ctx first.
type OrderService struct{ region string }

// Get takes ctx first (conforming).
func (s *OrderService) Get(ctx context.Context, id string) (string, error) { return id, nil }

// List takes ctx first (conforming).
func (s *OrderService) List(ctx context.Context) ([]string, error) { return nil, nil }

// Region is a zero-arg accessor and is exempt from the rule.
func (s *OrderService) Region() string { return s.region }

// BrokenService violates the convention in two ways.
type BrokenService struct{}

// Save puts ctx second (violation).
func (b *BrokenService) Save(id string, ctx context.Context) error { return nil }

// Delete takes no context at all (violation).
func (b *BrokenService) Delete(id int) error { return nil }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ctxfit"
)

func main() {
	registry := []any{
		&ctxfit.OrderService{},
		&ctxfit.BrokenService{},
	}
	violations := ctxfit.CheckContextFirst(registry, nil)

	fmt.Printf("violations: %d\n", len(violations))
	for _, v := range violations {
		fmt.Println(" -", v)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
violations: 2
 - *ctxfit.BrokenService.Delete: first parameter is int, want context.Context
 - *ctxfit.BrokenService.Save: first parameter is string, want context.Context
```

Methods are reported in reflection's lexical order (`Delete` before `Save`), and
the conforming `OrderService` — including its zero-arg `Region` accessor —
contributes nothing.

### Tests

Create `ctxfit_test.go`:

```go
package ctxfit

import (
	"fmt"
	"strings"
	"testing"
)

func TestAllExportedMethodsTakeContextFirst(t *testing.T) {
	t.Parallel()

	registry := []any{&OrderService{region: "eu"}}
	if vs := CheckContextFirst(registry, nil); len(vs) != 0 {
		t.Fatalf("conforming registry reported violations: %v", vs)
	}
}

func TestAllowlistSkipsGetters(t *testing.T) {
	t.Parallel()

	// Region is already zero-arg (auto-exempt); prove an explicit allowlist entry
	// also suppresses a would-be violation.
	registry := []any{&BrokenService{}}
	allow := map[string]bool{"Save": true, "Delete": true}
	if vs := CheckContextFirst(registry, allow); len(vs) != 0 {
		t.Fatalf("allowlisted methods still reported: %v", vs)
	}
}

func TestDetectsBrokenType(t *testing.T) {
	t.Parallel()

	registry := []any{&BrokenService{}}
	vs := CheckContextFirst(registry, nil)
	if len(vs) != 2 {
		t.Fatalf("got %d violations, want 2: %v", len(vs), vs)
	}
	// Both offending methods must be named.
	joined := vs[0].String() + " " + vs[1].String()
	for _, name := range []string{"Save", "Delete"} {
		if !strings.Contains(joined, name) {
			t.Errorf("violations %q missing method %q", joined, name)
		}
	}
}

func TestMustPanicsOnViolation(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustHaveContextFirst did not panic on a broken registry")
		}
	}()
	MustHaveContextFirst([]any{&BrokenService{}}, nil)
}

func TestMustPassesCleanRegistry(t *testing.T) {
	t.Parallel()

	// Should not panic.
	MustHaveContextFirst([]any{&OrderService{}}, nil)
}

func ExampleCheckContextFirst() {
	registry := []any{&OrderService{}, &BrokenService{}}
	fmt.Println(len(CheckContextFirst(registry, nil)))
	// Output: 2
}
```

## Review

The checker is correct when it flags exactly the methods that break the shape and
nothing else: the two `BrokenService` methods are caught, the conforming
`OrderService` methods pass, and the zero-arg `Region` accessor is exempt without
needing an allowlist entry. `TestDetectsBrokenType` is the load-bearing test —
without it, a checker that silently returns an empty slice would "pass" while
enforcing nothing, so proving the guardrail catches a real violation is as
important as proving it accepts good code. The reflection details are the easy
things to get wrong: `In(1)` (not `In(0)`, which is the receiver) is the first real
parameter, and `Implements(contextType)` (not `==`) is required because a method
takes the `context.Context` *interface*, which no concrete type equals. Wired into
CI over your real handler registry, this converts "we agreed ctx goes first" into a
build that fails when someone forgets. Run `go test -race`; the checker is pure and
allocation-only, so a clean race build is expected.

## Resources

- [`reflect` package](https://pkg.go.dev/reflect) — `Type.NumMethod`, `Type.Method`, `Method.Type.In`, `Type.Implements`.
- [Go Blog: The Laws of Reflection](https://go.dev/blog/laws-of-reflection) — how `reflect.Type` and interface types relate.
- [`context` package](https://pkg.go.dev/context) — the first-parameter convention this test enforces.

---

Prev: [08-deadline-budget-propagation.md](08-deadline-budget-propagation.md) | Back to [00-concepts.md](00-concepts.md) | Next: [../08-select-priority-and-starvation/00-concepts.md](../08-select-priority-and-starvation/00-concepts.md)
