# Exercise 8: Resolve Config Precedence With cmp.Or (Generic Inference)

Twelve-factor config resolves the same setting from several sources in priority
order: env beats flag beats default. `cmp.Or` collapses the usual nil-check ladder
into one call, and infers its type parameter from the arguments. You will build a
settings resolver on top of it and confront its one sharp edge: it selects the
first *non-zero* value, so an explicit zero override is indistinguishable from
unset.

## What you'll build

```text
settings/                   independent module: example.com/settings
  go.mod                    go 1.26
  settings.go               Resolve helpers via cmp.Or; struct-level merge
  cmd/
    demo/
      main.go               resolves a setting from three sources
  settings_test.go          precedence, all-zero default, zero-ambiguity trade-off
```

Files: `settings.go`, `cmd/demo/main.go`, `settings_test.go`.
Implement: precedence resolution with `cmp.Or`, and a `Merge` that overlays a
partial `Settings` over defaults.
Test: first non-empty string, first non-zero int, all-zero falls to default, the
documented zero-value ambiguity, and a `var _ string = cmp.Or(...)` pin.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

## What cmp.Or infers, and what it cannot know

`cmp.Or[T comparable](vals ...T) T` (Go 1.22) returns the first argument that is not
the zero value of `T`, or the zero value if all are zero. Its type parameter `T` is
*inferred from the arguments*: `cmp.Or(envHost, flagHost, "0.0.0.0")` infers
`T = string` because every argument is a `string`, so you never write
`cmp.Or[string](...)`. All variadic elements must share one `T`; mixing a `string`
and an `int` in one call fails inference, because the compiler cannot pick a single
`T` that fits both. An *untyped constant* argument is fine — it conforms to whatever
`T` the typed arguments fix — which is why `cmp.Or(port, 8080)` works when `port` is
an `int`.

This makes precedence resolution a single readable expression:
`cmp.Or(fromEnv, fromFlag, defaultValue)` reads top-to-bottom as the priority order
and returns the winner. The `Merge` helper applies the same idea field by field: it
overlays a partial override struct onto defaults, taking the override's field when it
is non-zero and the default otherwise.

The trap is semantic, not about inference: `cmp.Or` is a *first-non-zero* selector,
not a *presence* check. It cannot distinguish "the operator explicitly set this to
`0`/`""`/`false`" from "nobody set it". If `0` is a legal, meaningful override — a
timeout of `0` meaning "no timeout", a feature flag explicitly set to `false` —
`cmp.Or` will skip it and fall through to the next source, which is wrong. For those
settings you must carry presence separately: a pointer (`*int`, where `nil` means
unset) or an `ok` bool. The test documents this ambiguity directly so the trade-off
is explicit rather than a surprise in production.

Create `settings.go`:

```go
package settings

import "cmp"

// ResolveString returns the first non-empty value in priority order: env, then
// flag, then the default. T is inferred as string from the arguments.
func ResolveString(fromEnv, fromFlag, def string) string {
	return cmp.Or(fromEnv, fromFlag, def)
}

// ResolveInt returns the first non-zero value in priority order. T is inferred as
// int. Beware: an explicit 0 override cannot be distinguished from unset.
func ResolveInt(fromEnv, fromFlag, def int) int {
	return cmp.Or(fromEnv, fromFlag, def)
}

// Settings is a small config surface. A zero field means "not set" for the merge
// below -- which is exactly the ambiguity to keep in mind.
type Settings struct {
	Host    string
	Port    int
	Workers int
}

// Merge overlays a partial override onto defaults, taking each override field
// when it is non-zero. Built on cmp.Or per field.
func Merge(def, override Settings) Settings {
	return Settings{
		Host:    cmp.Or(override.Host, def.Host),
		Port:    cmp.Or(override.Port, def.Port),
		Workers: cmp.Or(override.Workers, def.Workers),
	}
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/settings"
)

func main() {
	// env empty, flag set, default present: flag wins.
	host := settings.ResolveString("", "10.0.0.5", "0.0.0.0")
	fmt.Printf("host = %s\n", host)

	// all sources zero except default: default wins.
	port := settings.ResolveInt(0, 0, 8080)
	fmt.Printf("port = %d\n", port)

	merged := settings.Merge(
		settings.Settings{Host: "0.0.0.0", Port: 8080, Workers: 4},
		settings.Settings{Port: 9090}, // override only the port
	)
	fmt.Printf("merged = %+v\n", merged)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
host = 10.0.0.5
port = 8080
merged = {Host:0.0.0.0 Port:9090 Workers:4}
```

## Tests

The tests cover precedence for strings and ints, the all-zero fallthrough to the
default, and the documented zero-value ambiguity. `var _ string = ResolveString(...)`
pins the inferred return type.

Create `settings_test.go`:

```go
package settings

import (
	"cmp"
	"testing"
)

func TestResolveStringPrecedence(t *testing.T) {
	t.Parallel()

	var _ string = ResolveString("a", "b", "c") // pin

	cases := []struct {
		env, flag, def, want string
	}{
		{"env", "flag", "def", "env"},
		{"", "flag", "def", "flag"},
		{"", "", "def", "def"},
		{"", "", "", ""},
	}
	for _, c := range cases {
		if got := ResolveString(c.env, c.flag, c.def); got != c.want {
			t.Errorf("ResolveString(%q,%q,%q) = %q, want %q", c.env, c.flag, c.def, got, c.want)
		}
	}
}

func TestResolveIntPrecedence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		env, flag, def, want int
	}{
		{5, 6, 7, 5},
		{0, 6, 7, 6},
		{0, 0, 7, 7},
	}
	for _, c := range cases {
		if got := ResolveInt(c.env, c.flag, c.def); got != c.want {
			t.Errorf("ResolveInt(%d,%d,%d) = %d, want %d", c.env, c.flag, c.def, got, c.want)
		}
	}
}

// TestZeroOverrideAmbiguity documents the trade-off: an explicit 0 override is
// indistinguishable from unset, so cmp.Or falls through to the default. This is a
// property to design around, not a bug.
func TestZeroOverrideAmbiguity(t *testing.T) {
	t.Parallel()

	// Operator wants "0 workers" but cmp.Or reads 0 as unset and returns 4.
	got := cmp.Or(0, 4)
	if got != 4 {
		t.Fatalf("cmp.Or(0, 4) = %d, want 4 (0 treated as unset)", got)
	}
	// The remedy when zero is legal: carry presence with a pointer.
	zero := 0
	override := &zero
	resolved := 4
	if override != nil {
		resolved = *override // now 0 wins, as intended
	}
	if resolved != 0 {
		t.Fatalf("pointer-based resolve = %d, want 0", resolved)
	}
}

func TestMerge(t *testing.T) {
	t.Parallel()

	got := Merge(
		Settings{Host: "0.0.0.0", Port: 8080, Workers: 4},
		Settings{Port: 9090},
	)
	want := Settings{Host: "0.0.0.0", Port: 9090, Workers: 4}
	if got != want {
		t.Fatalf("Merge = %+v, want %+v", got, want)
	}
}
```

## Review

The resolver is correct when precedence reads top-to-bottom as the argument order to
`cmp.Or` and `T` is inferred rather than spelled out. The one thing you must design
around is that `cmp.Or` selects the first non-zero value, so a meaningful `0`, `""`,
or `false` override is silently skipped — `TestZeroOverrideAmbiguity` makes that
concrete and shows the pointer-based remedy. Reach for `cmp.Or` when the zero value
genuinely means "unset" (a host, a positive port, a worker count); reach for a
pointer or an `ok` bool the moment zero is a legal configured value.

## Resources

- [cmp.Or](https://pkg.go.dev/cmp#Or) — first non-zero argument, with `T` inferred from the variadic args.
- [Go Specification: Type inference](https://go.dev/ref/spec#Type_inference) — how a single `T` is deduced from the arguments.
- [The Go Blog: Everything you always wanted to know about type inference](https://go.dev/blog/type-inference) — the inference algorithm for generic calls.

---

Back to [07-exponential-backoff-duration-arith.md](07-exponential-backoff-duration-arith.md) | Next: [09-json-number-float64-boundary.md](09-json-number-float64-boundary.md)
