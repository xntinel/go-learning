# Exercise 6: A supply-chain denylist that blocks a pinned vulnerable dependency

When a dependency release is found compromised or carries a known CVE, the
incident response is to pin it out across the fleet: any build whose vendored tree
contains the bad version must fail until it is bumped. This exercise builds that
policy engine — a denylist of `module@version` constraints evaluated against the
vendored inventory, with correct semantic-version comparison.

This module is fully self-contained: its own `go mod init`, its own demo and
tests. Nothing here imports another exercise.

## What you'll build

```text
denylist/                    independent module: example.com/denylist
  go.mod                     go 1.26 (requires golang.org/x/mod)
  denylist.go                Constraint; ParseConstraint; Evaluate; ErrBadConstraint
  cmd/
    demo/
      main.go                evaluates an inventory against a denylist
  denylist_test.go           exact, range, pseudo-version, +incompatible cases
```

- Files: `denylist.go`, `cmd/demo/main.go`, `denylist_test.go`.
- Implement: `ParseConstraint`, parsing `path@version` (exact) and `path@<version` (range) forms with `golang.org/x/mod/semver` validation, and `Evaluate`, returning a `Violation` for every inventory module a constraint matches.
- Test: exact and range denylist entries, inventory that does and does not match, plus a pseudo-version and a `+incompatible` version edge case; assert the matched violations and that `semver.Compare` orders them correctly.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/denylist/cmd/demo
cd ~/go-exercises/denylist
go mod init example.com/denylist
go get golang.org/x/mod
```

### Why semver comparison, not string comparison

A denylist entry like "block anything below `v1.2.4` of this module" (the range
that captures a CVE fixed in `v1.2.4`) cannot be evaluated with string comparison:
lexically, `"v1.2.10" < "v1.2.4"` is true, which is the wrong answer. Semantic
versions order by numeric components, and `golang.org/x/mod/semver` — the same
package the go command uses — implements that ordering. `semver.Compare(a, b)`
returns `-1`, `0`, or `+1`, and `semver.IsValid` gates malformed input. Two edge
cases matter in a real inventory. A pseudo-version like
`v0.0.0-20210101000000-abcdef123456` (a commit not yet tagged) is a valid semver
that orders below any tagged release. A `+incompatible` build tag (a v2+ module
without `/v2` in its path) is valid, and its build metadata is ignored by
comparison: `semver.Compare("v2.0.0+incompatible", "v2.0.0") == 0`, so an exact
denylist entry for `v2.0.0` correctly matches the `+incompatible` form.

### Parsing a constraint

A constraint is `path@spec`, where `spec` is either a bare version (exact match)
or an operator followed by a version (`<`, `<=`, `>`, `>=`, `=`). `ParseConstraint`
splits on the last `@` with `strings.Cut`, peels a leading operator (checking the
two-character operators before the one-character ones), and validates the version
with `semver.IsValid`. An invalid version or an empty path is an error wrapping
`ErrBadConstraint`, so a typo in a policy file fails loudly rather than silently
matching nothing.

Create `denylist.go`:

```go
package denylist

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/mod/semver"
)

// ErrBadConstraint is returned (wrapped) when a denylist entry is malformed.
var ErrBadConstraint = errors.New("denylist: bad constraint")

// Module is one vendored module (path and version) from the inventory.
type Module struct {
	Path    string
	Version string
}

// Constraint blocks a module path at versions satisfying Op/Version.
// Op is one of "=", "<", "<=", ">", ">=" (default "=" for a bare version).
type Constraint struct {
	Path    string
	Op      string
	Version string
}

// String renders the constraint in its canonical path@opversion form.
func (c Constraint) String() string {
	if c.Op == "=" {
		return c.Path + "@" + c.Version
	}
	return c.Path + "@" + c.Op + c.Version
}

// Violation records an inventory module blocked by a constraint.
type Violation struct {
	Path       string
	Version    string
	Constraint string
}

// ParseConstraint parses "path@version" or "path@<version" (and the other
// operators) into a Constraint, validating the version as semver.
func ParseConstraint(s string) (Constraint, error) {
	path, spec, found := strings.Cut(s, "@")
	if !found || path == "" || spec == "" {
		return Constraint{}, fmt.Errorf("%w: %q: want path@version", ErrBadConstraint, s)
	}
	op := "="
	for _, cand := range []string{"<=", ">=", "<", ">", "="} {
		if strings.HasPrefix(spec, cand) {
			op = cand
			spec = spec[len(cand):]
			break
		}
	}
	if !semver.IsValid(spec) {
		return Constraint{}, fmt.Errorf("%w: %q: %q is not a valid semver", ErrBadConstraint, s, spec)
	}
	return Constraint{Path: path, Op: op, Version: spec}, nil
}

// matches reports whether module version modVer satisfies the constraint.
func (c Constraint) matches(modPath, modVer string) bool {
	if c.Path != modPath || !semver.IsValid(modVer) {
		return false
	}
	cmp := semver.Compare(modVer, c.Version)
	switch c.Op {
	case "=":
		return cmp == 0
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	}
	return false
}

// Evaluate returns a Violation for every inventory module that any constraint
// blocks. Order follows the inventory, then the denylist.
func Evaluate(inventory []Module, deny []Constraint) []Violation {
	var vs []Violation
	for _, m := range inventory {
		for _, c := range deny {
			if c.matches(m.Path, m.Version) {
				vs = append(vs, Violation{Path: m.Path, Version: m.Version, Constraint: c.String()})
			}
		}
	}
	return vs
}

// AnyBlocked reports whether the inventory contains at least one blocked module.
func AnyBlocked(inventory []Module, deny []Constraint) bool {
	return slices.ContainsFunc(inventory, func(m Module) bool {
		return slices.ContainsFunc(deny, func(c Constraint) bool { return c.matches(m.Path, m.Version) })
	})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/denylist"
)

func main() {
	inventory := []denylist.Module{
		{Path: "github.com/pkg/errors", Version: "v0.9.1"},
		{Path: "github.com/evil/lib", Version: "v1.2.3"},
		{Path: "golang.org/x/text", Version: "v0.3.7"},
	}
	// x/text < v0.3.8 carried CVE-2021-38561; evil/lib v1.2.3 is a known-bad pin.
	specs := []string{"github.com/evil/lib@v1.2.3", "golang.org/x/text@<v0.3.8"}

	var deny []denylist.Constraint
	for _, s := range specs {
		c, err := denylist.ParseConstraint(s)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		deny = append(deny, c)
	}

	violations := denylist.Evaluate(inventory, deny)
	if len(violations) == 0 {
		fmt.Println("no blocked dependencies")
		return
	}
	for _, v := range violations {
		fmt.Printf("BLOCKED %s %s by %s\n", v.Path, v.Version, v.Constraint)
	}
	os.Exit(1)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
BLOCKED github.com/evil/lib v1.2.3 by github.com/evil/lib@v1.2.3
BLOCKED golang.org/x/text v0.3.7 by golang.org/x/text@<v0.3.8
```

### Tests

The table covers exact and range constraints, a pseudo-version, and a
`+incompatible` version. It asserts both the matched violations and, in a
dedicated case, that `semver.Compare` orders a pseudo-version below a tagged one
so the range constraint fires.

Create `denylist_test.go`:

```go
package denylist

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"golang.org/x/mod/semver"
)

func TestParseConstraint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    Constraint
		wantErr bool
	}{
		{"m@v1.2.3", Constraint{Path: "m", Op: "=", Version: "v1.2.3"}, false},
		{"m@<v1.2.4", Constraint{Path: "m", Op: "<", Version: "v1.2.4"}, false},
		{"m@>=v2.0.0", Constraint{Path: "m", Op: ">=", Version: "v2.0.0"}, false},
		{"noversion", Constraint{}, true},
		{"m@notsemver", Constraint{}, true},
		{"@v1.0.0", Constraint{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseConstraint(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrBadConstraint) {
					t.Fatalf("err = %v; want ErrBadConstraint", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if got != tc.want {
				t.Fatalf("ParseConstraint(%q) = %+v; want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestEvaluate(t *testing.T) {
	t.Parallel()
	inventory := []Module{
		{Path: "github.com/evil/lib", Version: "v1.2.3"},
		{Path: "github.com/ok/lib", Version: "v2.0.0+incompatible"},
		{Path: "golang.org/x/text", Version: "v0.3.7"},
		{Path: "example.com/edge", Version: "v0.0.0-20210101000000-abcdef123456"},
	}
	deny := mustParse(t,
		"github.com/evil/lib@v1.2.3", // exact
		"github.com/ok/lib@v2.0.0",   // matches +incompatible via canonical
		"golang.org/x/text@<v0.3.8",  // range: v0.3.7 blocked
		"example.com/edge@<v0.1.0",   // range: pseudo-version below tag blocked
	)
	got := Evaluate(inventory, deny)
	want := []Violation{
		{Path: "github.com/evil/lib", Version: "v1.2.3", Constraint: "github.com/evil/lib@v1.2.3"},
		{Path: "github.com/ok/lib", Version: "v2.0.0+incompatible", Constraint: "github.com/ok/lib@v2.0.0"},
		{Path: "golang.org/x/text", Version: "v0.3.7", Constraint: "golang.org/x/text@<v0.3.8"},
		{Path: "example.com/edge", Version: "v0.0.0-20210101000000-abcdef123456", Constraint: "example.com/edge@<v0.1.0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate:\n got %#v\nwant %#v", got, want)
	}
}

func TestNonMatch(t *testing.T) {
	t.Parallel()
	inventory := []Module{{Path: "golang.org/x/text", Version: "v0.3.8"}}
	deny := mustParse(t, "golang.org/x/text@<v0.3.8") // v0.3.8 is not < v0.3.8
	if got := Evaluate(inventory, deny); len(got) != 0 {
		t.Fatalf("expected no violation for fixed version, got %#v", got)
	}
}

func TestSemverOrdersPseudoVersion(t *testing.T) {
	t.Parallel()
	if semver.Compare("v0.0.0-20210101000000-abcdef123456", "v0.1.0") >= 0 {
		t.Fatal("pseudo-version should order below a tagged release")
	}
}

func mustParse(t *testing.T, specs ...string) []Constraint {
	t.Helper()
	var cs []Constraint
	for _, s := range specs {
		c, err := ParseConstraint(s)
		if err != nil {
			t.Fatalf("ParseConstraint(%q): %v", s, err)
		}
		cs = append(cs, c)
	}
	return cs
}

func Example() {
	inv := []Module{{Path: "x/text", Version: "v0.3.7"}}
	c, _ := ParseConstraint("x/text@<v0.3.8")
	fmt.Println(Evaluate(inv, []Constraint{c})[0].Constraint)
	// Output: x/text@<v0.3.8
}
```

## Review

The engine is correct when version matching goes through `semver.Compare`, never
string comparison, so `v1.2.10` is correctly ordered above `v1.2.4` and a range
constraint captures exactly the intended window. The two edge cases prove the
point: a pseudo-version orders below any tag (so a `<v0.1.0` range catches an
untagged commit), and a `+incompatible` build tag compares equal to its base
version (so an exact entry blocks it). Parsing fails closed on a malformed
constraint via `ErrBadConstraint`, because a policy typo that silently matched
nothing would be worse than a loud failure. The remaining trap is validating the
module version too: `matches` returns false for a non-semver module version rather
than letting `semver.Compare` misbehave on garbage input.

## Resources

- [`golang.org/x/mod/semver`](https://pkg.go.dev/golang.org/x/mod/semver) — `Compare`, `IsValid`, and `Canonical`.
- [Go module version numbering](https://go.dev/ref/mod#versions) — semantic versions, pseudo-versions, and `+incompatible`.
- [`slices.ContainsFunc`](https://pkg.go.dev/slices#ContainsFunc) — the any-match primitive used by `AnyBlocked`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-build-mod-resolver.md](07-build-mod-resolver.md)
