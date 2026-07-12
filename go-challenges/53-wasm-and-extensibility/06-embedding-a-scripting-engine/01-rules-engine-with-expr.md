# Exercise 1: A Sandboxed Rules Engine with expr

Operators need to change eligibility and routing rules without a redeploy. This
exercise builds the engine that lets them: it compiles operator-supplied boolean
expressions once against a strongly-typed fact struct, type-checks them at load
time, and evaluates them per request under a memory budget and a builtin
allowlist.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports another
exercise. It is a bar-mode module: it depends on `github.com/expr-lang/expr`, so
the offline gate cannot fetch the dependency and fails at build; the value is the
correct shape, the verified APIs, and tests that assert the real limit behavior
once the dependency is present.

## What you'll build

```text
rules/                       independent module: example.com/rules
  go.mod                     go 1.26; requires github.com/expr-lang/expr
  rules.go                   type Fact; Compile -> *Rule; (*Rule).Eval; ErrNotBool; region()
  cmd/
    demo/
      main.go                compile a rule from -rule flag, eval against sample facts
  rules_test.go              table eval, compile type-error, MaxNodes, MemoryBudget bomb, Example
```

Files: `rules.go`, `cmd/demo/main.go`, `rules_test.go`.
Implement: a typed `Fact` environment, `Compile(src)` that type-checks with `expr.AsBool`, caps AST size with `expr.MaxNodes`, denies builtins with `DisableAllBuiltins` and re-enables a small allowlist, and exposes one vetted host function via `expr.Function`; and `(*Rule).Eval(fact)` that runs under a `vm.VM{MemoryBudget}`.
Test: a table of rules compiled once and evaluated against several facts; a compile-time type-mismatch rejection; a `MaxNodes` rejection; an adversarial memory-bomb evaluation that trips `MemoryBudget`.
Verify: `go test -race ./...` with the dependency present (bar-mode: the offline gate fails to build).

Set up the module:

```bash
go mod edit -go=1.26
go get github.com/expr-lang/expr@v1.17.5
```

### Why a typed environment is the first line of defense

The environment is the surface the rule can touch, and the safest surface is a
concrete struct. `Fact` declares exactly four fields; a rule can read
`Country`, `Age`, `Amount`, and `Tier` and nothing else, because the compiler
type-checks every identifier against that struct. There is no map of "whatever
the caller passed", no reflection into arbitrary host objects. This is also what
makes static typing work: because `Age` is declared `int`, the rule `Age == "old"`
fails to compile with a type error rather than blowing up at request time.

Passing `expr.Env(Fact{})` at compile time and `expr.AsBool()` together does two
jobs: it binds the identifier types, and it asserts the whole expression must
evaluate to a `bool`. A rule that returns a string or a number is rejected at
load. That is the property you want — a malformed rule is a config-load failure
with a message an operator can read, not a 500.

### Composing the sandbox

The engine builds a deny-by-default policy out of `expr`'s primitives:

- `expr.MaxNodes(maxRuleNodes)` caps the AST at compile time, so a
  pathologically large expression is rejected before it can run.
- `expr.DisableAllBuiltins()` turns *off* the whole builtin set, then
  `expr.EnableBuiltin("lower")` and `expr.EnableBuiltin("len")` re-enable a
  vetted few. Deny-by-default: a rule cannot reach a builtin you did not
  explicitly allow.
- `expr.Function("region", ...)` adds one pure host function. Its third argument
  is a typed nil of the function's signature (`new(func(string) string)`) so the
  type checker knows `region(Country)` returns a string.

At evaluation time, `vm.VM{MemoryBudget: evalMemoryBudget}` bounds the allocation
a single evaluation may perform. `expr`'s VM has no unbounded loops, so its real
DoS surface is memory — a large range or repetition — and this budget is what
stops it. Note the granularity fact from the concepts file: `expr` does *not*
poll a context between opcodes, so a timeout wrapper would do nothing here; the
memory budget and the node cap are the real guards.

Create `rules.go`:

```go
package rules

import (
	"errors"
	"fmt"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// Fact is the typed environment a rule evaluates against. A rule may read only
// these fields; the compiler type-checks every identifier against this struct,
// so nothing else in the host is reachable from an operator's expression.
type Fact struct {
	Country string
	Age     int
	Amount  float64
	Tier    string
}

// ErrNotBool is returned when a compiled rule yields a non-bool value. With
// expr.AsBool set at compile time this should be unreachable, but Eval asserts
// it so a misconfiguration fails closed rather than panicking.
var ErrNotBool = errors.New("rule did not evaluate to bool")

// maxRuleNodes caps AST complexity at compile time. A rule with more nodes than
// this is rejected at load, bounding parse/compile blowups.
const maxRuleNodes = 40

// evalMemoryBudget caps allocation during a single evaluation, stopping
// allocation bombs (large ranges, string repetition) from exhausting the heap.
const evalMemoryBudget = 1 << 16

// Rule is a compiled, reusable program plus its source for diagnostics. It is
// safe to cache and to evaluate concurrently with per-call facts.
type Rule struct {
	Source  string
	program *vm.Program
}

// region maps a country code to a coarse region. It is a pure host function
// exposed to rules through expr.Function.
func region(country string) string {
	switch country {
	case "US", "CA", "MX":
		return "NA"
	case "DE", "FR", "ES":
		return "EU"
	default:
		return "OTHER"
	}
}

// Compile type-checks src against Fact and returns a reusable Rule. It applies
// the sandbox policy: bool result, a node cap, deny-by-default builtins with a
// small allowlist, and one vetted host function. A type error or an over-complex
// expression fails here, at config-load time, not on the request path.
func Compile(src string) (*Rule, error) {
	program, err := expr.Compile(src,
		expr.Env(Fact{}),
		expr.AsBool(),
		expr.MaxNodes(maxRuleNodes),
		expr.DisableAllBuiltins(),
		expr.EnableBuiltin("lower"),
		expr.EnableBuiltin("len"),
		expr.Function(
			"region",
			func(params ...any) (any, error) {
				return region(params[0].(string)), nil
			},
			new(func(string) string),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("compile rule %q: %w", src, err)
	}
	return &Rule{Source: src, program: program}, nil
}

// Eval runs the compiled rule against fact under a per-evaluation memory budget.
// A fresh vm.VM is used per call so evaluation is safe under concurrency. If the
// budget is exceeded the VM panics internally; expr.Run's VM recovers it into a
// returned error, which Eval wraps.
func (r *Rule) Eval(fact Fact) (bool, error) {
	machine := vm.VM{MemoryBudget: evalMemoryBudget}
	out, err := machine.Run(r.program, fact)
	if err != nil {
		return false, fmt.Errorf("eval rule %q: %w", r.Source, err)
	}
	b, ok := out.(bool)
	if !ok {
		return false, fmt.Errorf("%w: %q returned %T", ErrNotBool, r.Source, out)
	}
	return b, nil
}
```

### The runnable demo

The demo compiles one rule from a flag and evaluates it against three sample
facts, showing compile-once/eval-many: one `*Rule`, many facts.

Create `cmd/demo/main.go`:

```go
package main

import (
	"flag"
	"fmt"
	"log"

	"example.com/rules"
)

func main() {
	src := flag.String("rule",
		`Country == "US" && Age >= 21 && Amount <= 1000.0`,
		"boolean rule over Fact")
	flag.Parse()

	rule, err := rules.Compile(*src)
	if err != nil {
		log.Fatalf("bad rule: %v", err)
	}

	facts := []rules.Fact{
		{Country: "US", Age: 30, Amount: 500, Tier: "gold"},
		{Country: "US", Age: 18, Amount: 500, Tier: "silver"},
		{Country: "DE", Age: 40, Amount: 500, Tier: "gold"},
	}
	for _, f := range facts {
		ok, err := rule.Eval(f)
		if err != nil {
			log.Fatalf("eval: %v", err)
		}
		fmt.Printf("%s/%d/%.0f -> %v\n", f.Country, f.Age, f.Amount, ok)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
US/30/500 -> true
US/18/500 -> false
DE/40/500 -> false
```

### Tests

The tests prove the three properties that matter. `TestEval` compiles a rule set
once and evaluates each rule against several facts, exercising the allowlisted
`lower`/`len` builtins and the `region` host function. `TestCompileRejectsTypeMismatch`
proves static typing catches a bad rule at load. `TestMaxNodesRejects` proves the
complexity cap fires at compile time. `TestMemoryBudgetStopsBomb` compiles a
*permissive* range expression (the shape a naive setup would allow) and shows the
runtime memory budget catching the allocation bomb with a `memory budget exceeded`
error — the range operand is a variable so the compiler cannot constant-fold the
allocation away.

Create `rules_test.go`:

```go
package rules

import (
	"fmt"
	"strings"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

func TestEval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rule string
		fact Fact
		want bool
	}{
		{"us adult under cap", `Country == "US" && Age >= 21 && Amount <= 1000.0`,
			Fact{Country: "US", Age: 30, Amount: 500}, true},
		{"us minor", `Country == "US" && Age >= 21`,
			Fact{Country: "US", Age: 18}, false},
		{"lower builtin", `lower(Country) == "us"`,
			Fact{Country: "US"}, true},
		{"host function region", `region(Country) == "EU"`,
			Fact{Country: "DE"}, true},
		{"tier length", `len(Tier) > 3`,
			Fact{Tier: "gold"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rule, err := Compile(tc.rule)
			if err != nil {
				t.Fatalf("Compile(%q) failed: %v", tc.rule, err)
			}
			got, err := rule.Eval(tc.fact)
			if err != nil {
				t.Fatalf("Eval failed: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Eval(%q) = %v, want %v", tc.rule, got, tc.want)
			}
		})
	}
}

func TestCompileRejectsTypeMismatch(t *testing.T) {
	t.Parallel()
	// Age is an int; comparing it to a string is a compile-time type error.
	if _, err := Compile(`Age == "old"`); err == nil {
		t.Fatal("Compile accepted an int-vs-string comparison; want type error")
	}
}

func TestMaxNodesRejects(t *testing.T) {
	t.Parallel()
	// Build a bool expression well past maxRuleNodes conjuncts.
	src := `Age >= 0` + strings.Repeat(` && Age >= 0`, maxRuleNodes)
	if _, err := Compile(src); err == nil {
		t.Fatal("Compile accepted an over-complex rule; want MaxNodes rejection")
	}
}

func TestMemoryBudgetStopsBomb(t *testing.T) {
	t.Parallel()
	// A naive, permissive compile that allows the range operator and len. The
	// operand n is a variable so the range cannot be constant-folded away.
	program, err := expr.Compile(`len(1..n) > 0`, expr.Env(map[string]int{"n": 0}))
	if err != nil {
		t.Fatalf("Compile bomb failed: %v", err)
	}
	machine := vm.VM{MemoryBudget: 1 << 12}
	_, err = machine.Run(program, map[string]int{"n": 5_000_000})
	if err == nil {
		t.Fatal("memory bomb ran to completion; want budget error")
	}
	if !strings.Contains(err.Error(), "memory budget exceeded") {
		t.Fatalf("error = %v, want memory budget exceeded", err)
	}
}

func Example() {
	rule, _ := Compile(`region(Country) == "NA" && Age >= 18`)
	ok, _ := rule.Eval(Fact{Country: "CA", Age: 25})
	fmt.Println(ok)
	// Output: true
}
```

## Review

The engine is correct when a rule's answer is a pure function of the `Fact` it is
given and nothing else, and when every failure lands in the right place. Confirm
static typing by watching `TestCompileRejectsTypeMismatch` reject `Age == "old"`
at compile time — if that ever compiles, the environment is not typed. Confirm the
two DoS guards independently: `TestMaxNodesRejects` must fail at *compile* (the
node cap), and `TestMemoryBudgetStopsBomb` must fail at *run* with `memory budget
exceeded` (the allocation cap). They are different limits and a passing test for
one says nothing about the other.

The mistakes to avoid are the ones the concepts file names. Do not skip
`DisableAllBuiltins` "for convenience" — the allowlist is the sandbox. Do not
reach for a `context.WithTimeout` around `Eval`; `expr`'s VM does not poll a
context, so it buys nothing, and the memory budget is the real protection. Do not
recompile a rule per request — `Compile` is the config-load step and `*Rule` is
safe to cache and evaluate concurrently, which is why `Eval` builds a fresh
`vm.VM` each call rather than sharing mutable VM state. Finally, keep `Eval`
failing closed: a non-bool result returns `ErrNotBool` and `false`, never a
silent `true`.

## Resources

- [expr — Getting started](https://expr-lang.org/docs/getting-started) — `Compile`/`Run`, typed environments, and static type checking.
- [expr — Configuration](https://expr-lang.org/docs/configuration) — `AsBool`, `MaxNodes`, `DisableAllBuiltins`, `Function`, and context handling.
- [expr on pkg.go.dev](https://pkg.go.dev/github.com/expr-lang/expr) — exact option and function signatures.
- [expr/vm on pkg.go.dev](https://pkg.go.dev/github.com/expr-lang/expr/vm) — `vm.VM`, the `MemoryBudget` field, and `Run`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-policy-evaluation-with-cel.md](02-policy-evaluation-with-cel.md)
