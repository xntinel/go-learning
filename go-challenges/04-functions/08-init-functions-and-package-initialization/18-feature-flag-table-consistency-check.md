# Exercise 18: Fail-Fast init() — Cross-Validating a Feature Flag Table

**Nivel: Intermedio** — validacion rapida (un test corto).

A feature-flag package keeps a list of known flag names and a table of
default values. This exercise validates at `init()` that the two stay in
sync — every declared flag has a default, and every default belongs to a
declared flag — so a typo in either list is caught at load instead of a flag
silently defaulting to the zero value or a stale default lingering for a flag
nobody checks anymore.

## What you'll build

```text
featureflags/              independent module: example.com/featureflags
  go.mod                    module example.com/featureflags
  flags.go                   flagNames, defaults map, init() validation, IsEnabled
  cmd/
    demo/
      main.go                checks a couple of known flags and one unknown one
  flags_test.go               package-loaded proof + broken-table rejection + IsEnabled table
```

Files: `flags.go`, `cmd/demo/main.go`, `flags_test.go`.
Implement: `flagNames []string`, `defaults map[string]bool`, an extracted `validateFlags` helper called from `init()`, and `IsEnabled(flag string) bool`.
Test: the package loads without panicking; `validateFlags` rejects a table missing a default or holding an orphaned one; `IsEnabled` returns each flag's default and `false` for an unknown name.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why two tables need a consistency check, not just a lookup

`defaults[flag]` alone hides two distinct mistakes. A flag added to
`flagNames` but never given a default silently reports `false` for it forever
— indistinguishable from a real, intentional default — so nobody notices the
flag was never properly wired up. A leftover entry in `defaults` for a flag
removed from `flagNames` is dead configuration nobody is checking anymore,
which invites confusion the next time someone reads the table and assumes it
still matters. `validateFlags` closes both directions of that gap at once,
at load, instead of leaving them to be discovered by an engineer squinting
at a diff months later.

Create `flags.go`:

```go
// flags.go
// Package featureflags declares the set of feature flags this service knows
// about and their default values, cross-validated at init so a typo in
// either list is caught at load instead of silently defaulting a flag to the
// wrong thing.
package featureflags

import "fmt"

// flagNames lists every feature flag this service can check.
var flagNames = []string{"new-checkout", "dark-mode", "beta-search", "async-export"}

// defaults maps a flag name to its default value. Every name in flagNames
// must have an entry here, and every entry here must appear in flagNames.
var defaults = map[string]bool{
	"new-checkout": false,
	"dark-mode":    true,
	"beta-search":  false,
	"async-export": false,
}

func init() {
	if err := validateFlags(flagNames, defaults); err != nil {
		panic(fmt.Errorf("featureflags: %w", err))
	}
}

// validateFlags fails if a declared flag has no default, or a default
// exists for a flag that was never declared. Extracted from init so a test
// can drive it directly with a deliberately broken table.
func validateFlags(names []string, table map[string]bool) error {
	known := make(map[string]bool, len(names))
	for _, n := range names {
		known[n] = true
		if _, ok := table[n]; !ok {
			return fmt.Errorf("flag %q has no default value", n)
		}
	}
	for n := range table {
		if !known[n] {
			return fmt.Errorf("default table has an entry for undeclared flag %q", n)
		}
	}
	return nil
}

// IsEnabled reports whether flag is enabled by default. An unknown flag
// name reports false; validateFlags having already run at init means every
// name in flagNames is guaranteed to have a default, so this only guards
// against a caller checking a name that was never declared at all.
func IsEnabled(flag string) bool {
	return defaults[flag]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/featureflags"
)

func main() {
	for _, flag := range []string{"dark-mode", "new-checkout", "unknown-flag"} {
		fmt.Printf("%s: %v\n", flag, featureflags.IsEnabled(flag))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
dark-mode: true
new-checkout: false
unknown-flag: false
```

### Tests

Create `flags_test.go`:

```go
// flags_test.go
package featureflags

import "testing"

// TestPackageLoaded proves init did not panic: if the shipped flag table
// were inconsistent, this test binary would never reach a test body.
func TestPackageLoaded(t *testing.T) {
	if !featureFlagDeclared("dark-mode") {
		t.Fatal("package failed to load a working flag table")
	}
}

func featureFlagDeclared(name string) bool {
	for _, n := range flagNames {
		if n == name {
			return true
		}
	}
	return false
}

func TestIsEnabled(t *testing.T) {
	tests := []struct {
		flag string
		want bool
	}{
		{"dark-mode", true},
		{"new-checkout", false},
		{"beta-search", false},
		{"async-export", false},
		{"does-not-exist", false},
	}
	for _, tt := range tests {
		if got := IsEnabled(tt.flag); got != tt.want {
			t.Errorf("IsEnabled(%q) = %v, want %v", tt.flag, got, tt.want)
		}
	}
}

func TestValidateFlagsRejectsMissingDefault(t *testing.T) {
	names := []string{"a", "b"}
	broken := map[string]bool{"a": true}
	if err := validateFlags(names, broken); err == nil {
		t.Fatal("validateFlags did not reject a flag with no default")
	}
}

func TestValidateFlagsRejectsOrphanDefault(t *testing.T) {
	names := []string{"a"}
	broken := map[string]bool{"a": true, "b": false}
	if err := validateFlags(names, broken); err == nil {
		t.Fatal("validateFlags did not reject a default for an undeclared flag")
	}
}
```

## Review

The check runs in both directions on purpose: `TestValidateFlagsRejectsMissingDefault`
catches a declared flag nobody gave a default, and
`TestValidateFlagsRejectsOrphanDefault` catches a default for a flag nobody
declared. Either mistake alone would pass a naive check that only walks one
of the two collections. Panicking at `init()` on either failure means the
binary never starts serving traffic with a flag table it cannot trust, which
is a far cheaper place to catch the bug than a production rollout that
silently behaves as if a new flag were permanently off.

## Resources

- [Go spec — Package initialization](https://go.dev/ref/spec#Package_initialization) — when `init()` runs relative to package-level variables.
- [maps package](https://pkg.go.dev/maps) — iterating a map's keys, used here to walk `defaults` for orphaned entries.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-connection-pool-lazy-oncevalue-per-dialect.md](17-connection-pool-lazy-oncevalue-per-dialect.md) | Next: [19-schema-migration-ordering-dag-validator.md](19-schema-migration-ordering-dag-validator.md)
