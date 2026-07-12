# Exercise 13: Alert Rule Engine: Callbacks Capturing a Mutable Config Pointer

**Nivel: Intermedio** — validacion rapida (un test corto).

An alerting service registers one threshold rule per alert name, closing
over a shared `*Config` so operators can tune the threshold at runtime. That
live-read is a feature for rules that should track the CURRENT setting — but
it is a bug for rules meant to be a durable snapshot of the threshold that
was in effect when they were registered, such as queued alerts evaluated
later against whatever the config says NOW.

## What you'll build

```text
rules/                       independent module: example.com/rules
  go.mod                     go 1.24
  rules.go                    Config, Rule, RegisterRules, RegisterRulesBuggy
  rules_test.go               table test: snapshot vs. live-read after mutation
```

- Files: `rules.go`, `rules_test.go`.
- Implement: `RegisterRules(cfg, names) []Rule` snapshotting `cfg.Threshold` at registration time; `RegisterRulesBuggy` closing over the `*Config` pointer directly.
- Test: one table test that registers rules, mutates `cfg.Threshold` afterward, then evaluates.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/13-rule-engine-config-pointer-mutation
cd go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/13-rule-engine-config-pointer-mutation
go mod edit -go=1.24
```

### Capturing a pointer is capturing the mutation, not just the value

`RegisterRulesBuggy` builds each `Rule` as `func(value int) bool { return
value > cfg.Threshold }`. There is no loop variable involved at all — `range
names` only drives how many rules to build. The capture that matters is
`cfg`, a pointer to config shared and mutable for the process's lifetime.
Every rule reads `cfg.Threshold` live, so a threshold change made AFTER
registration retroactively changes what an already-registered rule considers
a breach. `RegisterRules` fixes it by reading `cfg.Threshold` into a local
`threshold` at registration time and closing over that snapshot instead.

Create `rules.go`:

```go
package rules

// Config holds mutable settings shared across a running process, such as an
// alert threshold an operator can tune at runtime.
type Config struct {
	Threshold int
}

// Rule evaluates a value against a threshold that was fixed at registration
// time.
type Rule func(value int) bool

// RegisterRulesBuggy builds one rule per name, each closing over a POINTER
// to the shared Config. Because the closure reads cfg.Threshold live, any
// later mutation to the shared Config retroactively changes what an
// already-registered rule considers a breach.
func RegisterRulesBuggy(cfg *Config, names []string) []Rule {
	rules := make([]Rule, 0, len(names))
	for range names {
		rules = append(rules, func(value int) bool {
			return value > cfg.Threshold // BUG: reads the live, mutable config
		})
	}
	return rules
}

// RegisterRules builds one rule per name, each closing over a snapshot of
// the threshold taken AT REGISTRATION TIME, so later mutation of the shared
// Config cannot change how an already-registered rule evaluates.
func RegisterRules(cfg *Config, names []string) []Rule {
	rules := make([]Rule, 0, len(names))
	for range names {
		threshold := cfg.Threshold // snapshot taken now, not read later
		rules = append(rules, func(value int) bool {
			return value > threshold
		})
	}
	return rules
}
```

### Test

One table test registers a single rule against `Threshold: 10`, mutates the
config to `100`, then evaluates `50` against both variants.

Create `rules_test.go`:

```go
package rules

import "testing"

func TestRegisterRules(t *testing.T) {
	tests := []struct {
		name     string
		register func(*Config, []string) []Rule
		value    int
		want     bool // result of rule(value) after cfg.Threshold is mutated to 100
	}{
		{
			name:     "snapshot at registration keeps the threshold=10 decision",
			register: RegisterRules,
			value:    50,
			want:     true, // 50 > 10 (the snapshot), unaffected by the later mutation
		},
		{
			name:     "live config read follows the mutated threshold=100",
			register: RegisterRulesBuggy,
			value:    50,
			want:     false, // 50 > 100 is false: reads the mutated threshold
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Threshold: 10}
			registered := tt.register(cfg, []string{"only-rule"})

			cfg.Threshold = 100 // later setup code mutates the shared config

			if got := registered[0](tt.value); got != tt.want {
				t.Fatalf("rule(%d) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

Both table rows evaluate the SAME value, `50`, against rules registered from
the SAME starting config — only the mutation afterward diverges their
results. `RegisterRulesBuggy` proves a captured pointer is a captured
mutation stream, not a captured value: it does not matter that no loop
variable is involved, because `cfg` itself is the shared, escaping state.
`RegisterRules` shows the same discipline as everywhere else in this lesson —
read what you need into a local variable at the moment you need it, and
close over that, rather than trusting a shared reference to still mean what
it meant when you captured it.

## Resources

- [Go spec: Pointer types](https://go.dev/ref/spec#Pointer_types) — what capturing `*Config` actually captures.
- [Go blog: Closures](https://go.dev/tour/moretypes/25) — capture-by-reference as both the feature and the gotcha.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-per-tenant-billing-shared-accumulator.md](12-per-tenant-billing-shared-accumulator.md) | Next: [14-router-setup-shared-config-mutation.md](14-router-setup-shared-config-mutation.md)
