# Exercise 6: CI linter that enforces go.mod supply-chain policy

Some `go.mod` states must never reach production: a dependency pinned to a
pseudo-version (an unreleased commit), a `+incompatible` major (a repo that never
adopted module versioning), an old `go` directive, or a `replace` pointing at a
local filesystem path (which is ignored downstream and often a developer's forgotten
hotfix). Here you build the CI gate that parses `go.mod` with
`golang.org/x/mod/modfile` and fails the build on each.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
modlint/                    independent module: example.com/billing/modlint
  go.mod
  modlint.go                Lint([]byte) ([]Finding, error) over modfile.Parse
  cmd/
    demo/
      main.go               runnable: lint a dirty go.mod, print ordered findings
  modlint_test.go           clean fixture → 0 findings; dirty fixture → 3 ordered findings
```

- Files: `modlint.go`, `cmd/demo/main.go`, `modlint_test.go`.
- Implement: `Lint(goMod []byte) ([]Finding, error)` flagging pseudo-version requires, `+incompatible` requires, an old/missing `go` directive, and local-path `replace`s.
- Test: a clean fixture yields zero findings; a fixture with a pseudo-version require, a `+incompatible` require, and a `=> ./local` replace yields three findings in file order, each with its module path and reason.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Parsing go.mod as data, and what each rule catches

`go.mod` is not free-form text; it is a structured file, and `modfile.Parse(name,
data, fix)` gives you the typed AST — `File.Go` (the `go` directive), `File.Require`
(each `Require` with a `Mod module.Version` and an `Indirect` flag), and
`File.Replace` (each `Replace` with `Old` and `New` `module.Version`s). Linting the
parsed structure, not a regexp over the bytes, is what makes the checks robust to
formatting.

Four policy rules, each mapping to one structural fact:

- **Pseudo-version require.** `module.IsPseudoVersion(v)` is true for a
  `vX.Y.Z-timestamp-revision` string. A pseudo-version in a `require` means the
  dependency is pinned to an *unreleased* commit — deliberate during development,
  a red flag in a shipped `go.mod`.
- **+incompatible require.** A version ending in `+incompatible`
  (`strings.HasSuffix`) is a v2+ tag from a repo that never adopted the `/vN` module
  path. The toolchain can auto-upgrade across a breaking boundary under a stable
  import path, so CI should surface it.
- **Old or missing go directive.** `File.Go` is nil when there is no `go` line;
  otherwise `File.Go.Version` is a string like `"1.26"`. Comparing it needs the
  leading `v` that `semver` demands, so prefix it: `semver.Compare("v"+got,
  "v"+min)`. Below the floor (or absent) is a finding.
- **Local-path replace.** A `replace` to another *module* carries a version
  (`New.Version != ""`); a `replace` to a *filesystem path* has an empty
  `New.Version`. That empty-version signal is exactly how you distinguish
  `=> example.com/other v1.2.3` (a module swap) from `=> ./fork` (a local path that
  is ignored when your module is consumed downstream, and must never ship).

The findings come out in a deterministic order — the `go` directive first, then
requires in file order, then replaces — so a CI diff is stable and a test can assert
the exact sequence.

Create `modlint.go`:

```go
package modlint

import (
	"fmt"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// minGoDirective is the oldest go directive CI accepts.
const minGoDirective = "1.21"

// Finding is one policy violation: the offending module (or "go directive") and a
// human-readable reason.
type Finding struct {
	Module string
	Reason string
}

// Lint parses a go.mod and returns policy violations in a deterministic order:
// the go directive, then requires in file order, then replaces.
func Lint(goMod []byte) ([]Finding, error) {
	f, err := modfile.Parse("go.mod", goMod, nil)
	if err != nil {
		return nil, fmt.Errorf("modlint: parse go.mod: %w", err)
	}

	var findings []Finding

	switch {
	case f.Go == nil:
		findings = append(findings, Finding{Module: "go directive", Reason: "missing go directive"})
	case semver.Compare("v"+f.Go.Version, "v"+minGoDirective) < 0:
		findings = append(findings, Finding{
			Module: "go directive",
			Reason: fmt.Sprintf("go directive %s below minimum %s", f.Go.Version, minGoDirective),
		})
	}

	for _, r := range f.Require {
		if module.IsPseudoVersion(r.Mod.Version) {
			findings = append(findings, Finding{
				Module: r.Mod.Path,
				Reason: fmt.Sprintf("require pinned to pseudo-version %s", r.Mod.Version),
			})
		}
		if strings.HasSuffix(r.Mod.Version, "+incompatible") {
			findings = append(findings, Finding{
				Module: r.Mod.Path,
				Reason: fmt.Sprintf("require is +incompatible (%s)", r.Mod.Version),
			})
		}
	}

	for _, rep := range f.Replace {
		if rep.New.Version == "" {
			findings = append(findings, Finding{
				Module: rep.Old.Path,
				Reason: fmt.Sprintf("replace points at local path %s", rep.New.Path),
			})
		}
	}

	return findings, nil
}
```

### The runnable demo

The demo lints a deliberately dirty `go.mod` and prints each finding.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/billing/modlint"
)

const dirty = `module example.com/app

go 1.26

require (
	example.com/legacy v0.0.0-20200101000000-abcdefabcdef
	github.com/old/lib v3.1.0+incompatible
)

replace example.com/dep => ./fork
`

func main() {
	findings, err := modlint.Lint([]byte(dirty))
	if err != nil {
		fmt.Println("lint failed:", err)
		return
	}
	fmt.Printf("findings: %d\n", len(findings))
	for _, f := range findings {
		fmt.Printf("  %s: %s\n", f.Module, f.Reason)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
findings: 3
  example.com/legacy: require pinned to pseudo-version v0.0.0-20200101000000-abcdefabcdef
  github.com/old/lib: require is +incompatible (v3.1.0+incompatible)
  example.com/dep: replace points at local path ./fork
```

### Tests

The clean fixture must produce zero findings; the dirty one must produce exactly
three, in order, each with the right module and reason.

Create `modlint_test.go`:

```go
package modlint

import "testing"

const cleanMod = `module example.com/app

go 1.26

require golang.org/x/text v0.14.0
`

const dirtyMod = `module example.com/app

go 1.26

require (
	example.com/legacy v0.0.0-20200101000000-abcdefabcdef
	github.com/old/lib v3.1.0+incompatible
)

replace example.com/dep => ./fork
`

const oldGoMod = `module example.com/app

go 1.18
`

func TestLintClean(t *testing.T) {
	t.Parallel()
	findings, err := Lint([]byte(cleanMod))
	if err != nil {
		t.Fatalf("Lint(clean): %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("clean go.mod produced %d findings: %+v", len(findings), findings)
	}
}

func TestLintDirty(t *testing.T) {
	t.Parallel()
	findings, err := Lint([]byte(dirtyMod))
	if err != nil {
		t.Fatalf("Lint(dirty): %v", err)
	}
	want := []Finding{
		{Module: "example.com/legacy", Reason: "require pinned to pseudo-version v0.0.0-20200101000000-abcdefabcdef"},
		{Module: "github.com/old/lib", Reason: "require is +incompatible (v3.1.0+incompatible)"},
		{Module: "example.com/dep", Reason: "replace points at local path ./fork"},
	}
	if len(findings) != len(want) {
		t.Fatalf("got %d findings, want %d: %+v", len(findings), len(want), findings)
	}
	for i, w := range want {
		if findings[i] != w {
			t.Fatalf("finding[%d] = %+v, want %+v", i, findings[i], w)
		}
	}
}

func TestLintOldGoDirective(t *testing.T) {
	t.Parallel()
	findings, err := Lint([]byte(oldGoMod))
	if err != nil {
		t.Fatalf("Lint(oldGo): %v", err)
	}
	if len(findings) != 1 || findings[0].Module != "go directive" {
		t.Fatalf("old go directive: got %+v, want one go-directive finding", findings)
	}
}
```

## Review

The linter is correct when each rule reads a structural fact from the parsed
`modfile`, not a substring of the bytes: `IsPseudoVersion` for an unreleased commit,
`HasSuffix("+incompatible")` for a non-modules major, a `semver`-with-`v`-prefix
compare for the `go` floor, and an empty `New.Version` for a local-path `replace`.
The ordering is fixed — go directive, requires, replaces — so `TestLintDirty` can
assert the exact three-finding sequence and CI diffs stay stable. The subtle rule is
the last one: a `replace` to another module carries a version and is legitimate; only
the version-less `=> ./path` form is the ignored-downstream local hotfix the policy
bans. Miss that distinction and you either flag every replace or none.

## Resources

- [`golang.org/x/mod/modfile`](https://pkg.go.dev/golang.org/x/mod/modfile) — `Parse`, `File`, `Require`, `Replace`.
- [`module.IsPseudoVersion`](https://pkg.go.dev/golang.org/x/mod/module#IsPseudoVersion) — detecting an unreleased-commit pin.
- [Go Modules Reference: +incompatible versions](https://go.dev/ref/mod#incompatible-versions) — what the suffix means.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-retract-a-broken-release.md](07-retract-a-broken-release.md)
