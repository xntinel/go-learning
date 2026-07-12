# Exercise 9: Feature Flags — The bool Zero-Value Trap and Explicit Tri-State

A feature flag has three states, not two: on, off, and *unset*. A plain `bool` can
only hold two, and its zero value silently conflates "unset" with "explicitly false",
so an absent request override clobbers a tenant's real setting. This module builds a
strict flag parser and a `*bool` tri-state resolver that gets precedence right.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports another exercise.

## What you'll build

```text
flags/                     independent module: example.com/flags
  go.mod                   go 1.26
  flags.go                 ParseFlag (strict), ParseOptionalFlag (*bool), Resolve
  cmd/
    demo/
      main.go              precedence: request override > tenant > default
  flags_test.go            strict tokens, unset-vs-false, precedence table
```

- Files: `flags.go`, `cmd/demo/main.go`, `flags_test.go`.
- Implement: `ParseFlag` (strict: `true/false/1/0/on/off`), `ParseOptionalFlag` returning `*bool` (nil = unset), and `Resolve(override, tenant *bool, def bool)`.
- Test: the strict parser accepts documented tokens and rejects `"yes"`/`""`/`"t"`; a `*bool` distinguishes unset from false where a `bool` cannot; the resolver keeps `tenant=true` when the override is unset but flips to false when the override is explicitly false.
- Verify: `go test -count=1 -race ./...`

### Why a plain bool cannot carry "unset"

The zero value of `bool` is `false`. That is fine when the only two states are true and
false, but a feature-flag override has a third: "the caller did not specify one". If you
model the override as a `bool`, an unset override is `false`, which is indistinguishable
from a caller who explicitly turned the flag off — and in a precedence chain
(request override > tenant config > default) that means an absent override silently
forces the flag off, clobbering a tenant that had it on. The bug is not in the resolution
logic; it is in the representation, which threw away the "unset" state before resolution
ran.

The fix is a three-state representation. `*bool` is the idiomatic one: `nil` is unset,
`&true` and `&false` are the two set states. `ParseOptionalFlag` returns `nil` for an
empty value and a pointer otherwise, so "unset" survives all the way to `Resolve`, which
consults the override only when it is non-nil and otherwise defers to the tenant, then
the default. The parser itself is deliberately *strict*: it accepts exactly
`true/false/1/0/on/off` and rejects everything else, including `"yes"` and `"t"`. The
standard library's `strconv.ParseBool` is looser — it accepts `"t"`, `"T"`, `"TRUE"` but
*not* `"on"`/`"off"` — so it is the wrong tool for a config surface where you want a small,
documented, unambiguous set of tokens. Strictness at the boundary means an operator typo
like `"yef"` fails loudly instead of quietly defaulting to false.

Create `flags.go`:

```go
package flags

import (
	"errors"
	"fmt"
)

// ErrBadToken marks a flag value outside the accepted token set.
var ErrBadToken = errors.New("unrecognized flag token")

// ParseFlag strictly parses one of true/false/1/0/on/off. Unlike
// strconv.ParseBool it rejects "t"/"T" and accepts on/off, giving a small,
// documented token set for a config surface.
func ParseFlag(s string) (bool, error) {
	switch s {
	case "true", "1", "on":
		return true, nil
	case "false", "0", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%w: %q", ErrBadToken, s)
	}
}

// ParseOptionalFlag returns nil for an empty (unset) value, or a *bool for a
// parsed value. The nil is the third state a plain bool cannot represent.
func ParseOptionalFlag(s string) (*bool, error) {
	if s == "" {
		return nil, nil
	}
	b, err := ParseFlag(s)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// Resolve applies precedence: an explicit request override wins, else the tenant
// setting, else the default. A nil override or tenant means "unset" and defers.
func Resolve(override, tenant *bool, def bool) bool {
	if override != nil {
		return *override
	}
	if tenant != nil {
		return *tenant
	}
	return def
}
```

### The runnable demo

The demo shows the two cases that matter: an unset override leaves a tenant's `true`
intact, while an explicit `off` override flips it to false. It also shows the all-unset
fallback to the default and a rejected token.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/flags"
)

func main() {
	tenantOn := true

	unset, _ := flags.ParseOptionalFlag("") // nil: caller did not specify
	fmt.Println("unset override, tenant true ->", flags.Resolve(unset, &tenantOn, false))

	off, _ := flags.ParseOptionalFlag("off") // explicit false
	fmt.Println("override off, tenant true ->", flags.Resolve(off, &tenantOn, false))

	fmt.Println("all unset -> default:", flags.Resolve(nil, nil, true))

	_, err := flags.ParseFlag("yes")
	fmt.Println("'yes' rejected:", errors.Is(err, flags.ErrBadToken))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
unset override, tenant true -> true
override off, tenant true -> false
all unset -> default: true
'yes' rejected: true
```

### Tests

`TestParseFlag` covers the accepted tokens and rejects `"yes"`, `""`, and `"t"`.
`TestUnsetVsFalse` is the demonstration that a `*bool` distinguishes the two states a
`bool` collapses: `ParseOptionalFlag("")` is nil while `ParseOptionalFlag("false")` is a
pointer to false. `TestStrictnessVsParseBool` shows `strconv.ParseBool("t")` succeeds
where the strict parser rejects it, justifying the custom parser. `TestResolvePrecedence`
walks all three layers.

Create `flags_test.go`:

```go
package flags

import (
	"errors"
	"strconv"
	"testing"
)

func TestParseFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{in: "true", want: true},
		{in: "1", want: true},
		{in: "on", want: true},
		{in: "false", want: false},
		{in: "0", want: false},
		{in: "off", want: false},
		{in: "yes", wantErr: true},
		{in: "", wantErr: true},
		{in: "t", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			got, err := ParseFlag(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrBadToken) {
					t.Fatalf("ParseFlag(%q) error = %v, want ErrBadToken", tt.in, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ParseFlag(%q) = %v,%v; want %v", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestUnsetVsFalse(t *testing.T) {
	t.Parallel()

	unset, err := ParseOptionalFlag("")
	if err != nil || unset != nil {
		t.Fatalf("empty = %v,%v; want nil,nil (unset)", unset, err)
	}
	explicit, err := ParseOptionalFlag("false")
	if err != nil || explicit == nil || *explicit != false {
		t.Fatalf("false = %v,%v; want *false", explicit, err)
	}
	// A plain bool would render both as false; the *bool keeps them distinct.
}

func TestStrictnessVsParseBool(t *testing.T) {
	t.Parallel()

	if _, err := strconv.ParseBool("t"); err != nil {
		t.Fatalf("strconv.ParseBool(t) should accept: %v", err)
	}
	if _, err := ParseFlag("t"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("strict ParseFlag(t) should reject, got %v", err)
	}
}

func TestResolvePrecedence(t *testing.T) {
	t.Parallel()

	ptr := func(b bool) *bool { return &b }

	tests := []struct {
		name     string
		override *bool
		tenant   *bool
		def      bool
		want     bool
	}{
		{name: "unset override keeps tenant true", override: nil, tenant: ptr(true), def: false, want: true},
		{name: "explicit false override wins", override: ptr(false), tenant: ptr(true), def: false, want: false},
		{name: "override true beats tenant false", override: ptr(true), tenant: ptr(false), def: false, want: true},
		{name: "all unset uses default", override: nil, tenant: nil, def: true, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := Resolve(tt.override, tt.tenant, tt.def); got != tt.want {
				t.Fatalf("Resolve = %v, want %v", got, tt.want)
			}
		})
	}
}
```

## Review

The design is correct when "unset" is a first-class state that survives to resolution.
`TestUnsetVsFalse` shows the `*bool` carrying it and `TestResolvePrecedence`'s first row
proves an unset override leaves a tenant's `true` intact — the exact case a plain `bool`
override would clobber. The strict parser is a deliberate choice: `TestStrictnessVsParseBool`
contrasts it with `strconv.ParseBool`, which is convenient but too permissive for a config
surface where you want a small, documented token set and a loud failure on anything else.
This same tri-state shape applies to PATCH semantics and any optional config where "omitted"
must differ from "set to the zero value".

## Resources

- [strconv.ParseBool](https://pkg.go.dev/strconv#ParseBool) — the looser stdlib parser and its accepted tokens.
- [Go Specification: The zero value](https://go.dev/ref/spec#The_zero_value) — why an unset `bool` is `false`.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — using `*bool` to model an optional value.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-request-id-hex-encoding.md](10-request-id-hex-encoding.md)
