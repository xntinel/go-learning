# Exercise 3: Config-Driven Rule Engine with Untrusted Patterns

An alert or routing engine loads its rules from config: each rule is a name and a
regex, and a matching line triggers the rule. The patterns are data, written by
operators, so a bad one must be a rejected config — not a panicked process. This
module builds that engine: it compiles each rule with `Compile` (surfacing a
per-rule error), rejects patterns that blow a size/complexity budget by inspecting
them with `regexp/syntax` before compiling, and precompiles everything once so the
hot matching path never recompiles.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
ruleengine/                 independent module: example.com/ruleengine
  go.mod                    go 1.26
  ruleengine.go             type Rule, Engine; Load (Compile + budget); Match; LiteralRule (QuoteMeta)
  cmd/
    demo/
      main.go               runnable demo: load rules, match a line
  ruleengine_test.go        valid load, bad pattern names the rule, budget rejects, QuoteMeta, no recompile
```

- Files: `ruleengine.go`, `cmd/demo/main.go`, `ruleengine_test.go`.
- Implement: `Load([]Rule) (*Engine, error)` compiling with `regexp.Compile`, enforcing a length and nesting budget via `regexp/syntax.Parse`; `Engine.Match(s) []string` returning matched rule names; `LiteralRule(name, literal)` using `regexp.QuoteMeta`; a `Compiles()` counter proving no recompile.
- Test: a valid config compiles; one invalid pattern fails the load with an error naming the bad rule (`errors.Is` + `errors.As`); an over-long or over-nested pattern is rejected by the budget; a `QuoteMeta`-wrapped literal matches metacharacters literally; the compile count stays at the number of rules under repeated matching.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ruleengine/cmd/demo
cd ~/go-exercises/ruleengine
go mod init example.com/ruleengine
```

### Compile at load, budget before compile, match with no allocation

Three ideas drive the design. First, **`Compile`, not `MustCompile`**: the
patterns come from config, so a malformed one is a handleable error. `Load`
returns it wrapped in a `RuleError` that carries the rule name, so the operator's
log says which rule to fix instead of a bare `error parsing regexp`. Second, a
**budget enforced before compile**: RE2 bounds match time in input length, but not
the size or nesting of the pattern itself, and there is no per-match timeout — so
an untrusted pattern needs a cap. `regexp/syntax.Parse` turns the pattern into a
tree of `*syntax.Regexp` whose `.Sub` children you can walk to measure nesting
depth; combined with a length cap, that rejects a pathologically nested or huge
pattern before it ever reaches the compiler. Third, **compile once**: `Load`
builds the automata into an `Engine`; `Match` only runs them. A `Compiles()`
counter lets a test prove that matching a thousand lines does not recompile
anything.

`LiteralRule` shows the QuoteMeta discipline: when a rule should match a literal
string that may contain metacharacters (a filename like `app.log`, an IP with
dots), wrap it in `regexp.QuoteMeta` so `.` means a literal dot, not "any
character." Without it, `app.log` would also match `appXlog`.

Create `ruleengine.go`:

```go
package ruleengine

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
)

// Budget caps on an untrusted pattern. RE2 bounds match time, not pattern size,
// so these guard compile-time cost and memory.
const (
	maxPatternLen = 512
	maxNestDepth  = 20
)

// ErrBudget is returned when a pattern exceeds the size or nesting budget.
var ErrBudget = errors.New("pattern exceeds budget")

// Rule is a named pattern loaded from config.
type Rule struct {
	Name    string
	Pattern string
}

// RuleError names the rule whose pattern failed to load.
type RuleError struct {
	Rule string
	Err  error
}

func (e *RuleError) Error() string { return fmt.Sprintf("rule %q: %v", e.Rule, e.Err) }
func (e *RuleError) Unwrap() error { return e.Err }

// LiteralRule builds a Rule that matches literal text, escaping metacharacters so
// a dot means a dot. This is the QuoteMeta discipline for user-provided literals.
func LiteralRule(name, literal string) Rule {
	return Rule{Name: name, Pattern: regexp.QuoteMeta(literal)}
}

type compiled struct {
	name string
	re   *regexp.Regexp
}

// Engine holds the compiled rules. Match never recompiles.
type Engine struct {
	rules    []compiled
	compiles int
}

// Compiles reports how many patterns were compiled, for tests that assert the
// hot path does not recompile.
func (e *Engine) Compiles() int { return e.compiles }

// Load validates and compiles every rule once. A single bad rule fails the whole
// load with a *RuleError naming it.
func Load(rules []Rule) (*Engine, error) {
	e := &Engine{}
	for _, r := range rules {
		if err := checkBudget(r.Pattern); err != nil {
			return nil, &RuleError{Rule: r.Name, Err: err}
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, &RuleError{Rule: r.Name, Err: err}
		}
		e.compiles++
		e.rules = append(e.rules, compiled{name: r.Name, re: re})
	}
	return e, nil
}

// Match returns the names of every rule whose pattern matches s, in load order.
func (e *Engine) Match(s string) []string {
	var hits []string
	for _, c := range e.rules {
		if c.re.MatchString(s) {
			hits = append(hits, c.name)
		}
	}
	return hits
}

// checkBudget rejects a pattern that is too long or too deeply nested, inspecting
// it with regexp/syntax before it is compiled.
func checkBudget(pattern string) error {
	if len(pattern) > maxPatternLen {
		return fmt.Errorf("%w: length %d > %d", ErrBudget, len(pattern), maxPatternLen)
	}
	tree, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return err // invalid syntax; surfaced as a RuleError too
	}
	if d := depth(tree); d > maxNestDepth {
		return fmt.Errorf("%w: nesting depth %d > %d", ErrBudget, d, maxNestDepth)
	}
	return nil
}

// depth is the maximum nesting depth of the parsed pattern tree.
func depth(re *syntax.Regexp) int {
	max := 0
	for _, sub := range re.Sub {
		if d := depth(sub); d > max {
			max = d
		}
	}
	return max + 1
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/ruleengine"
)

func main() {
	eng, err := ruleengine.Load([]ruleengine.Rule{
		{Name: "oom", Pattern: `(?i)out of memory`},
		{Name: "5xx", Pattern: `status=5\d\d`},
		ruleengine.LiteralRule("app-log", "app.log"),
	})
	if err != nil {
		log.Fatal(err)
	}
	line := "app.log: request failed status=503 out of memory"
	fmt.Printf("compiles=%d\n", eng.Compiles())
	fmt.Printf("hits=%v\n", eng.Match(line))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
compiles=3
hits=[oom 5xx app-log]
```

### Tests

Create `ruleengine_test.go`:

```go
package ruleengine

import (
	"errors"
	"strings"
	"testing"
)

func TestLoadValid(t *testing.T) {
	t.Parallel()
	eng, err := Load([]Rule{
		{Name: "oom", Pattern: `(?i)out of memory`},
		{Name: "5xx", Pattern: `status=5\d\d`},
	})
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if got := eng.Match("status=503 out of memory"); len(got) != 2 {
		t.Fatalf("Match hits = %v, want 2", got)
	}
}

func TestLoadBadPatternNamesRule(t *testing.T) {
	t.Parallel()
	_, err := Load([]Rule{
		{Name: "good", Pattern: `ok`},
		{Name: "broken", Pattern: `(unclosed`},
	})
	if err == nil {
		t.Fatal("Load succeeded, want error")
	}
	var re *RuleError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want *RuleError", err)
	}
	if re.Rule != "broken" {
		t.Fatalf("RuleError.Rule = %q, want %q", re.Rule, "broken")
	}
}

func TestLoadRejectsOverLong(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", maxPatternLen+1)
	_, err := Load([]Rule{{Name: "huge", Pattern: long}})
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v, want ErrBudget", err)
	}
}

func TestLoadRejectsOverNested(t *testing.T) {
	t.Parallel()
	nested := strings.Repeat("(", maxNestDepth+2) + "a" + strings.Repeat(")", maxNestDepth+2)
	_, err := Load([]Rule{{Name: "deep", Pattern: nested}})
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v, want ErrBudget", err)
	}
}

func TestLiteralRuleEscapesMeta(t *testing.T) {
	t.Parallel()
	eng, err := Load([]Rule{LiteralRule("f", "app.log")})
	if err != nil {
		t.Fatal(err)
	}
	if got := eng.Match("app.log"); len(got) != 1 {
		t.Fatalf("literal should match itself, hits = %v", got)
	}
	if got := eng.Match("appXlog"); len(got) != 0 {
		t.Fatalf("literal dot must not match any char, hits = %v", got)
	}
}

func TestMatchDoesNotRecompile(t *testing.T) {
	t.Parallel()
	eng, err := Load([]Rule{{Name: "r", Pattern: `a\d+`}})
	if err != nil {
		t.Fatal(err)
	}
	for range 1000 {
		eng.Match("a42")
	}
	if eng.Compiles() != 1 {
		t.Fatalf("Compiles() = %d after 1000 matches, want 1", eng.Compiles())
	}
}

func BenchmarkMatch(b *testing.B) {
	eng, err := Load([]Rule{{Name: "r", Pattern: `status=5\d\d`}})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		eng.Match("request failed status=503")
	}
}
```

## Review

The engine is correct when a bad rule is *data*: `Load` uses `Compile`, wraps the
failure in a `RuleError` naming the rule, and the whole config is rejected while
the process keeps running — `errors.As` recovers the rule name for the operator.
The budget is enforced *before* compilation by parsing with `regexp/syntax` and
walking `.Sub` for depth, which is the discipline RE2's linear-time guarantee does
not give you for free: it bounds match time, not pattern size. `LiteralRule`'s
`QuoteMeta` is the injection defense — `app.log` matches only `app.log`, never
`appXlog`. `TestMatchDoesNotRecompile` and the benchmark pin the last property:
compilation happens once at load, the hot path only runs the automata. Run
`go test -race` since one `Engine` is shared read-only across matching goroutines.

## Resources

- [`regexp` package](https://pkg.go.dev/regexp) — `Compile`, `MatchString`, `QuoteMeta`.
- [`regexp/syntax` package](https://pkg.go.dev/regexp/syntax) — `Parse` and the `Regexp` tree you walk to budget a pattern.
- [Russ Cox: Regular Expression Matching Can Be Simple And Fast](https://swtch.com/~rsc/regexp/regexp1.html) — why RE2 is linear-time and what that does and does not protect.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-structured-log-line-parser.md](02-structured-log-line-parser.md) | Next: [04-log-secret-redactor.md](04-log-secret-redactor.md)
