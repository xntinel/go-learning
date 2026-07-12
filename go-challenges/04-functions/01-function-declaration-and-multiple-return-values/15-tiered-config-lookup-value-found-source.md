# Exercise 15: A Tiered Config Lookup Returning (value, found, source)

**Nivel: Intermedio** — validacion rapida (un test corto).

Feature flags and per-request settings are rarely one flat map — a value can
come from a request override, an org-level config, or a hard-coded default,
checked in that priority order. This exercise builds `Resolve(key, override,
org, defaults) (value string, found bool, source string)`, a three-value
return where the third value is not an error but provenance: which tier the
value actually came from.

This module is fully self-contained: its own `go mod init`, all code inline,
one quick test file.

## What you'll build

```text
tieredconfig/               independent module: example.com/tiered-config-lookup
  go.mod                    go 1.24
  resolve.go                package tieredconfig; Resolve(key, override, org, defaults) (value, found, source)
  resolve_test.go           one table test over all four outcomes
```

- Files: `resolve.go`, `resolve_test.go`.
- Implement: `Resolve(key string, override, org, defaults map[string]string) (value string, found bool, source string)` checking `override`, then `org`, then `defaults` in that order using the comma-ok map form, returning the tier name that matched.
- Test: a table over a key present in `override`, a key present only in `org`, a key present only in `defaults`, and a key present nowhere, asserting all three return values.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Not every multi-return needs an error

`Resolve` never fails in the sense the rest of this lesson has been building
toward — there is no malformed input, no I/O, nothing to wrap with `%w`. A
missing key across all three tiers is a completely ordinary outcome, exactly
the "expected, non-exceptional absence" the concepts file describes for
`(value, ok)`. This exercise extends that shape by one value: instead of just
`(value, ok)`, it returns `(value, found, source)`, where `source` answers a
question `ok` alone cannot — *which* tier supplied the value. That third
return turns "is this flag on" into "is this flag on, and did the override
win or did we fall back to the default", which is exactly the question a
support engineer asks when a customer says a setting "isn't working".

The lookup itself is three comma-ok map reads in priority order, each an
early return the moment one hits:

```go
if v, ok := override[key]; ok {
    return v, true, "override"
}
```

There is no error path at all — the built-in comma-ok form already gives a
clean non-panicking way to ask "is this key present", and chaining three of
them in priority order is the entire function.

Create `resolve.go`:

```go
package tieredconfig

// Resolve looks up key across three config tiers, in priority order:
// a per-request override, an org-level setting, and a global default. It
// returns the value, whether it was found at all, and which tier supplied
// it — "override", "org", or "default" — so a caller can log or assert on
// which layer won, not just whether the key existed.
func Resolve(key string, override, org, defaults map[string]string) (value string, found bool, source string) {
	if v, ok := override[key]; ok {
		return v, true, "override"
	}
	if v, ok := org[key]; ok {
		return v, true, "org"
	}
	if v, ok := defaults[key]; ok {
		return v, true, "default"
	}
	return "", false, ""
}
```

At the call site: `value, found, source := tieredconfig.Resolve("timeout",
override, org, defaults)`. When only the boolean matters, the rest are
discarded with the blank identifier: `_, found, _ :=
tieredconfig.Resolve(key, override, org, defaults)`.

### Test

Create `resolve_test.go`:

```go
package tieredconfig

import "testing"

func TestResolve(t *testing.T) {
	t.Parallel()

	override := map[string]string{"timeout": "5s"}
	org := map[string]string{"timeout": "10s", "retries": "3"}
	defaults := map[string]string{"timeout": "30s", "retries": "1", "region": "us-east-1"}

	tests := []struct {
		name       string
		key        string
		wantValue  string
		wantFound  bool
		wantSource string
	}{
		{"override wins", "timeout", "5s", true, "override"},
		{"falls through to org", "retries", "3", true, "org"},
		{"falls through to default", "region", "us-east-1", true, "default"},
		{"not found anywhere", "missing", "", false, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			value, found, source := Resolve(tc.key, override, org, defaults)
			if value != tc.wantValue || found != tc.wantFound || source != tc.wantSource {
				t.Fatalf("Resolve(%q) = (%q, %t, %q), want (%q, %t, %q)",
					tc.key, value, found, source, tc.wantValue, tc.wantFound, tc.wantSource)
			}
		})
	}
}
```

## Review

`Resolve` is correct when the override tier wins whenever it has the key,
falling through to org and then defaults only when the higher tiers do not
have it, and returning `("", false, "")` when no tier has it. The table test's
`"timeout"` case is deliberately present in all three maps to prove priority
order, not just presence — a lookup that checked `defaults` first would pass
a naive test but fail this one. The mistake this avoids: collapsing the tiers
into one merged map before lookup, which answers "what is the value" but
destroys the "which tier decided" information the moment the merge happens.

## Resources

- [Go spec: index expressions](https://go.dev/ref/spec#Index_expressions) — the comma-ok form for map reads, `v, ok := m[key]`.
- [errors.Is](https://pkg.go.dev/errors#Is) — for contrast: this exercise's absence is not an error at all, unlike the sentinel-based exercises earlier in this lesson.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-semver-parse-major-minor-patch.md](14-semver-parse-major-minor-patch.md) | Next: [16-database-cursor-position-pagination.md](16-database-cursor-position-pagination.md)
