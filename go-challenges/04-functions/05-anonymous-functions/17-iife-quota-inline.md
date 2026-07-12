# Exercise 17: IIFE for Inline Quota Configuration Builder and Validation

**Nivel: Intermedio** — validacion rapida (un test corto).

Building a quota configuration is a good fit for an immediately invoked function
literal: a temporary map of per-key limits needs assembling and validating before it
becomes an immutable `Config`, and none of that scaffolding belongs in the caller's
scope once construction is done. This module builds that configuration with an IIFE
that panics immediately on a misconfigured limit instead of letting a bad value
reach request handling.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
quota/                        module example.com/quota
  go.mod
  quota.go                     Config, BuildConfig (IIFE), Limit
  quota_test.go                limits and default applied, panic on bad limit, panic on bad default
  cmd/demo/main.go             build a config and look up three keys
```

- Files: `quota.go`, `quota_test.go`, `cmd/demo/main.go`.
- Implement: `Config` holding per-key limits and a default; `BuildConfig(raw, def)` that validates and assembles the `Config` inside an IIFE, panicking on any non-positive limit; `Config.Limit(key)` falling back to the default.
- Test: valid limits and default are applied correctly; an invalid per-key limit panics; an invalid default panics.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/17-iife-quota-inline/cmd/demo
cd go-solutions/04-functions/05-anonymous-functions/17-iife-quota-inline
go mod edit -go=1.24
```

### Scoping the temporary map to the IIFE

`BuildConfig` wraps its entire body in `func() Config { ... }()` — called immediately,
never stored in a variable. Everything inside that literal, including the `limits`
map being assembled key by key, is local to the literal; only the finished `Config`
value escapes when the call returns. That is different from writing the same logic
directly in `BuildConfig`'s own body: the IIFE draws an explicit boundary around
"construction and validation," which reads clearly at the call site and would let a
future version wrap the same literal in a `recover` without touching `BuildConfig`'s
signature.

The validation itself panics rather than returning an error, deliberately: a
misconfigured quota is treated as a programmer error caught at startup, not a
runtime condition a caller is expected to handle — the same reasoning a package-level
`var` initializer or an `init` function would use.

Create `quota.go`:

```go
package quota

import "fmt"

// Config holds a per-key request quota and a default applied to unlisted
// keys.
type Config struct {
	Limits  map[string]int
	Default int
}

// Limit returns the quota for key, falling back to the default when key has
// no explicit entry.
func (c Config) Limit(key string) int {
	if v, ok := c.Limits[key]; ok {
		return v
	}
	return c.Default
}

// BuildConfig validates raw and def and assembles a Config. Construction and
// validation are done inside an immediately invoked function literal (IIFE):
// the temporary "limits" map being built is scoped entirely to that literal
// and is never visible outside it — only the finished Config escapes. A
// misconfigured limit panics immediately, at build time, instead of letting a
// bad value silently reach request handling.
func BuildConfig(raw map[string]int, def int) Config {
	return func() Config {
		if def <= 0 {
			panic(fmt.Sprintf("quota: invalid default limit: %d (must be > 0)", def))
		}
		limits := make(map[string]int, len(raw))
		for k, v := range raw {
			if v <= 0 {
				panic(fmt.Sprintf("quota: invalid limit for %q: %d (must be > 0)", k, v))
			}
			limits[k] = v
		}
		return Config{Limits: limits, Default: def}
	}()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/quota"
)

func main() {
	cfg := quota.BuildConfig(map[string]int{
		"free": 10,
		"pro":  1000,
	}, 100)

	fmt.Println("free:", cfg.Limit("free"))
	fmt.Println("pro:", cfg.Limit("pro"))
	fmt.Println("unknown:", cfg.Limit("unknown"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
free: 10
pro: 1000
unknown: 100
```

### Tests

`TestBuildConfigAppliesLimitsAndDefault` checks known keys return their own limit and
an unknown key falls back to the default. `TestBuildConfigPanicsOnInvalidLimit` and
`TestBuildConfigPanicsOnInvalidDefault` use `recover` to assert the IIFE panics
immediately on a non-positive value in either place.

Create `quota_test.go`:

```go
package quota

import "testing"

func TestBuildConfigAppliesLimitsAndDefault(t *testing.T) {
	t.Parallel()
	cfg := BuildConfig(map[string]int{"free": 10, "pro": 1000}, 100)

	if got := cfg.Limit("free"); got != 10 {
		t.Fatalf("Limit(free) = %d, want 10", got)
	}
	if got := cfg.Limit("pro"); got != 1000 {
		t.Fatalf("Limit(pro) = %d, want 1000", got)
	}
	if got := cfg.Limit("unknown"); got != 100 {
		t.Fatalf("Limit(unknown) = %d, want default 100", got)
	}
}

func TestBuildConfigPanicsOnInvalidLimit(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for a non-positive limit, got none")
		}
	}()
	BuildConfig(map[string]int{"free": 0}, 100)
}

func TestBuildConfigPanicsOnInvalidDefault(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for a non-positive default, got none")
		}
	}()
	BuildConfig(map[string]int{"free": 10}, -1)
}
```

## Review

The IIFE draws a hard line between "still being validated" and "safe to hand to a
caller": nothing outside the literal ever sees a half-built `limits` map, and nothing
outside `BuildConfig` ever sees a `Config` with a non-positive limit or default,
because the panic happens before the `return` that would produce one. Choosing
`panic` over an `error` return here is a deliberate call about who is expected to
react to a bad value — a hardcoded or config-file typo, not a condition the caller's
business logic should branch on.

## Resources

- [Go Language Specification: Function literals](https://go.dev/ref/spec#Function_literals)
- [Effective Go: Panic](https://go.dev/doc/effective_go#panic)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-background-workers-heartbeat.md](16-background-workers-heartbeat.md) | Next: [18-deferred-audit-logger.md](18-deferred-audit-logger.md)
