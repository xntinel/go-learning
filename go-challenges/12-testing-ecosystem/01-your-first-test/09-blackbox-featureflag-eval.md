# Exercise 9: Black-Box Testing a Feature-Flag Evaluator

A feature-flag evaluator decides whether a given user sees a given feature. This
module tests it from an *external* package — `package featureflag_test` — which
forces the test to consume only the exported API, pinning the public contract the
way a real caller experiences it.

## What you'll build

```text
featureflag/               independent module: example.com/featureflag
  go.mod
  featureflag.go           type Rule, Ruleset; func Evaluate(flag string, userID int, rules Ruleset) bool
  featureflag_test.go      package featureflag_test: TestEvaluate, ExampleEvaluate
  cmd/
    demo/
      main.go              evaluates a flag for a couple of users
```

- Files: `featureflag.go`, `featureflag_test.go`, `cmd/demo/main.go`.
- Implement: `Evaluate(flag string, userID int, rules Ruleset) bool` — unknown flag off; disabled rule off; 100% rollout on; partial rollout by `userID`.
- Test: from `package featureflag_test`, an enabled flag returns true and an unknown flag returns false.
- Verify: `gofmt -l .`, `go vet ./...`, `go test -count=1 -race ./...`.

Set up the module:

```bash
mkdir -p ~/go-exercises/featureflag/cmd/demo
cd ~/go-exercises/featureflag
go mod init example.com/featureflag
```

### White-box versus black-box, as an API-design pressure

A `_test.go` file can declare one of two packages. `package featureflag`
(white-box) compiles into the same package and can read unexported fields and
call unexported functions. `package featureflag_test` (black-box) is a separate
external package, compiled into the same test binary, that can only see what
`featureflag` exports. Both live in the same directory; the difference is the
package clause at the top of the test file.

Choosing black-box here is deliberate. It forces the test to build the `Ruleset`,
call `Evaluate`, and read the boolean using nothing but the exported API — exactly
what a real service integrating this package would do. That constraint is a design
pressure: if the test cannot express a realistic scenario without reaching inside
the package, the exported surface is wrong, and you learn it now rather than after
three teams depend on it. It also keeps the test honest about the contract — it
cannot accidentally couple to an internal detail that a refactor is free to
change. White-box tests still have their place (an invariant with no exported
witness), but the default for a library's public behavior is black-box.

The evaluator itself is deterministic, which is what keeps the test trustworthy.
An unknown flag is off (fail closed — a typo in a flag name must never silently
enable a feature). A rule that exists but is disabled is off. A rule at 100%
rollout is on for everyone. A partial rollout is decided by `userID % 100 <
Rollout`, a stable bucketing: the *same* user always gets the *same* answer, with
no `rand` and no clock, so the test can assert an exact boolean. Real evaluators
hash the user ID with the flag name for a better distribution; the modulo here
keeps the arithmetic obvious while staying deterministic.

Create `featureflag.go`:

```go
package featureflag

// Rule describes how a single flag is evaluated. Enabled gates the flag on or
// off; Rollout is the percentage (0-100) of users who see it when enabled.
type Rule struct {
	Enabled bool
	Rollout int
}

// Ruleset maps a flag name to its Rule.
type Ruleset map[string]Rule

// Evaluate reports whether the flag is on for the given user. An unknown or
// disabled flag is off (fail closed). A full rollout is on for everyone; a
// partial rollout buckets the user deterministically by userID.
func Evaluate(flag string, userID int, rules Ruleset) bool {
	r, ok := rules[flag]
	if !ok || !r.Enabled {
		return false
	}
	if r.Rollout >= 100 {
		return true
	}
	if r.Rollout <= 0 {
		return false
	}
	return userID%100 < r.Rollout
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/featureflag"
)

func main() {
	rules := featureflag.Ruleset{
		"new-checkout": {Enabled: true, Rollout: 100},
		"beta-search":  {Enabled: true, Rollout: 25},
		"dark-mode":    {Enabled: false, Rollout: 100},
	}
	for _, flag := range []string{"new-checkout", "beta-search", "dark-mode", "unknown"} {
		fmt.Printf("%-13s user=10 -> %v\n", flag, featureflag.Evaluate(flag, 10, rules))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
new-checkout  user=10 -> true
beta-search   user=10 -> true
dark-mode     user=10 -> false
unknown       user=10 -> false
```

### The tests

Note the package clause: `featureflag_test`, not `featureflag`. The test imports
the package and sees only its exported API.

Create `featureflag_test.go`:

```go
package featureflag_test

import (
	"fmt"
	"testing"

	"example.com/featureflag"
)

func TestEvaluate(t *testing.T) {
	t.Parallel()

	rules := featureflag.Ruleset{
		"new-checkout": {Enabled: true, Rollout: 100},
		"dark-mode":    {Enabled: false, Rollout: 100},
	}

	// An enabled, fully-rolled-out flag is on.
	if got := featureflag.Evaluate("new-checkout", 42, rules); !got {
		t.Errorf("Evaluate(new-checkout, 42) = false, want true")
	}

	// An unknown flag fails closed.
	if got := featureflag.Evaluate("missing", 42, rules); got {
		t.Errorf("Evaluate(missing, 42) = true, want false")
	}

	// A disabled flag is off even at 100% rollout.
	if got := featureflag.Evaluate("dark-mode", 42, rules); got {
		t.Errorf("Evaluate(dark-mode, 42) = true, want false")
	}
}

func ExampleEvaluate() {
	rules := featureflag.Ruleset{"new-checkout": {Enabled: true, Rollout: 100}}
	fmt.Println(featureflag.Evaluate("new-checkout", 1, rules))
	fmt.Println(featureflag.Evaluate("unknown", 1, rules))
	// Output:
	// true
	// false
}
```

## Review

The evaluator is correct when an unknown or disabled flag is off (fail closed), a
100% rollout is on, and a partial rollout is a deterministic function of
`userID` — never `rand`, so the test can pin an exact boolean. The design lesson
is the package clause: because the test is `package featureflag_test`, it proves
the *exported* contract and cannot lean on an internal, which is the discipline
you want for any package other teams import. If a black-box test cannot express a
scenario, that is the exported API telling you it is incomplete. Gate with
`gofmt -l .`, `go vet ./...`, and `go test -count=1 -race ./...`.

## Resources

- [cmd/go: Test packages](https://pkg.go.dev/cmd/go#hdr-Test_packages) — the `foo` versus `foo_test` package rule.
- [Go Blog: Package names](https://go.dev/blog/package-names) — designing an exported surface worth black-box testing.
- [testing package](https://pkg.go.dev/testing) — running external test packages.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-humanize-bytes.md](08-humanize-bytes.md) | Next: [10-skippable-slow-check.md](10-skippable-slow-check.md)
