# Exercise 6: Dependabot-style upgrade policy — classify and gate version bumps

Automated dependency PRs arrive constantly, and merging them blindly is how a
breaking change lands at 2 a.m. This exercise builds the policy engine a senior
writes to tame them: given a current and candidate version, classify the bump as
patch / minor / major and decide whether it is safe to auto-merge — with major bumps
flagged as breaking because Semantic Import Versioning makes them a source change.

This module is fully self-contained. It has its own `go mod init`, its own demo,
and its own tests, and imports nothing from the other exercises.

## What you'll build

```text
upgrade/                   independent module: example.com/upgrade
  go.mod                   go 1.26; requires golang.org/x/mod
  upgrade.go               Classify(modPath, current, candidate) (Decision, error)
  cmd/
    demo/
      main.go              classifies a set of candidate bumps and prints decisions
  upgrade_test.go          table over (current, candidate): kind + auto-merge decision
```

- Files: `upgrade.go`, `cmd/demo/main.go`, `upgrade_test.go`.
- Implement: `Classify` returning the bump kind, an auto-merge decision, and — for a major bump — the required new `/vN` import path, using `semver.Major`/`MajorMinor`/`Prerelease` and `module.SplitPathVersion`.
- Test: a table asserting patch/minor/major classification and the auto-merge decision, including an excluded prerelease and a v1→v2 case flagged breaking with an import-path change.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/06-dependency-management/06-semver-upgrade-policy/cmd/demo
cd go-solutions/11-packages-and-modules/06-dependency-management/06-semver-upgrade-policy
go get golang.org/x/mod
```

### The policy

The classification is defined entirely by comparing the major and minor components:

- Same major, same minor, higher patch → **patch**. Auto-merge.
- Same major, higher minor → **minor**. Auto-merge (backward-compatible by semver
  contract, within one major).
- Different major → **major**. Never auto-merge: the import path must change to
  `/vN`, so this is a migration a human owns.

Two overrides sit on top. A candidate that is not actually higher than current
(equal or a downgrade) is not an upgrade at all — reject it. And a candidate with a
prerelease tag (`v1.3.0-rc.1`, detected by a non-empty `semver.Prerelease`) is
excluded from auto-merge regardless of its numeric position, because prereleases are
not stable.

`semver.Major("v1.4.2")` returns `"v1"`; `semver.MajorMinor("v1.4.2")` returns
`"v1.4"`; `semver.Prerelease("v1.3.0-rc.1")` returns `"-rc.1"`. For a major bump the
new import path is the module prefix plus the new `/vN`: `module.SplitPathVersion`
strips any existing `/vN` from the current path to give the stable prefix, and the
new major from `semver.Major(candidate)` completes it.

Create `upgrade.go`:

```go
package upgrade

import (
	"errors"
	"fmt"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// Kind classifies a version bump.
type Kind string

const (
	Patch Kind = "patch"
	Minor Kind = "minor"
	Major Kind = "major"
)

// ErrNotUpgrade is returned when the candidate is not strictly newer than current.
var ErrNotUpgrade = errors.New("candidate is not an upgrade")

// ErrBadVersion is returned when a version is not valid semver.
var ErrBadVersion = errors.New("invalid semantic version")

// Decision is the policy verdict for one candidate bump.
type Decision struct {
	Kind             Kind
	AutoMerge        bool
	ImportPathChange bool
	NewImportPath    string // set only for a major bump
	Reason           string
}

// Classify compares current and candidate versions of the module at modPath and
// returns the bump kind and whether policy permits an auto-merge: patch and minor
// bumps within the same major auto-merge; a major bump requires human review (the
// import path changes to /vN); a prerelease candidate is never auto-merged.
func Classify(modPath, current, candidate string) (Decision, error) {
	if !semver.IsValid(current) || !semver.IsValid(candidate) {
		return Decision{}, fmt.Errorf("%w: current=%q candidate=%q", ErrBadVersion, current, candidate)
	}
	if semver.Compare(candidate, current) <= 0 {
		return Decision{}, fmt.Errorf("%w: %s -> %s", ErrNotUpgrade, current, candidate)
	}

	switch {
	case semver.Major(current) != semver.Major(candidate):
		prefix, _, ok := module.SplitPathVersion(modPath)
		if !ok {
			prefix = modPath
		}
		newPath := prefix + "/" + semver.Major(candidate)
		return Decision{
			Kind:             Major,
			AutoMerge:        false,
			ImportPathChange: true,
			NewImportPath:    newPath,
			Reason:           "major bump: import path changes to " + semver.Major(candidate) + ", requires migration",
		}, nil
	case semver.MajorMinor(current) != semver.MajorMinor(candidate):
		return decideStable(Minor, candidate), nil
	default:
		return decideStable(Patch, candidate), nil
	}
}

// decideStable applies the prerelease-exclusion rule to a patch/minor bump.
func decideStable(kind Kind, candidate string) Decision {
	if semver.Prerelease(candidate) != "" {
		return Decision{
			Kind:      kind,
			AutoMerge: false,
			Reason:    "prerelease candidate excluded from auto-merge",
		}
	}
	return Decision{
		Kind:      kind,
		AutoMerge: true,
		Reason:    string(kind) + " bump within same major: auto-merge eligible",
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/upgrade"
)

type bump struct {
	path      string
	current   string
	candidate string
}

func main() {
	cases := []bump{
		{"github.com/google/uuid", "v1.5.0", "v1.5.1"},
		{"github.com/google/uuid", "v1.5.0", "v1.6.0"},
		{"github.com/google/uuid", "v1.6.0", "v2.0.0"},
		{"github.com/google/uuid", "v1.6.0", "v1.7.0-rc.1"},
	}
	for _, c := range cases {
		d, err := upgrade.Classify(c.path, c.current, c.candidate)
		if err != nil {
			fmt.Printf("%s -> %s: %v\n", c.current, c.candidate, err)
			continue
		}
		fmt.Printf("%s -> %s: %s auto=%v %s\n", c.current, c.candidate, d.Kind, d.AutoMerge, d.Reason)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
v1.5.0 -> v1.5.1: patch auto=true patch bump within same major: auto-merge eligible
v1.5.0 -> v1.6.0: minor auto=true minor bump within same major: auto-merge eligible
v1.6.0 -> v2.0.0: major auto=false major bump: import path changes to v2, requires migration
v1.6.0 -> v1.7.0-rc.1: minor auto=false prerelease candidate excluded from auto-merge
```

### Tests

Create `upgrade_test.go`:

```go
package upgrade

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassify(t *testing.T) {
	t.Parallel()
	const path = "github.com/google/uuid"
	tests := []struct {
		name      string
		current   string
		candidate string
		wantKind  Kind
		wantAuto  bool
	}{
		{"patch", "v1.5.0", "v1.5.1", Patch, true},
		{"minor", "v1.5.0", "v1.6.0", Minor, true},
		{"major", "v1.6.0", "v2.0.0", Major, false},
		{"prerelease-minor", "v1.6.0", "v1.7.0-rc.1", Minor, false},
		{"prerelease-patch", "v1.6.0", "v1.6.1-beta", Patch, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := Classify(path, tc.current, tc.candidate)
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if d.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", d.Kind, tc.wantKind)
			}
			if d.AutoMerge != tc.wantAuto {
				t.Errorf("AutoMerge = %v, want %v", d.AutoMerge, tc.wantAuto)
			}
		})
	}
}

func TestMajorImportPathChange(t *testing.T) {
	t.Parallel()
	d, err := Classify("github.com/google/uuid", "v1.6.0", "v2.0.0")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if !d.ImportPathChange {
		t.Error("ImportPathChange = false, want true for v1->v2")
	}
	if d.NewImportPath != "github.com/google/uuid/v2" {
		t.Errorf("NewImportPath = %q, want github.com/google/uuid/v2", d.NewImportPath)
	}
}

func TestMajorFromV2Prefix(t *testing.T) {
	t.Parallel()
	// A module already at /v2 bumping to v3 must yield the /v3 path, not /v2/v3.
	d, err := Classify("github.com/foo/bar/v2", "v2.4.0", "v3.0.0")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if d.NewImportPath != "github.com/foo/bar/v3" {
		t.Errorf("NewImportPath = %q, want github.com/foo/bar/v3", d.NewImportPath)
	}
}

func TestNotUpgrade(t *testing.T) {
	t.Parallel()
	if _, err := Classify("github.com/foo/bar", "v1.6.0", "v1.6.0"); !errors.Is(err, ErrNotUpgrade) {
		t.Errorf("equal versions: err = %v, want ErrNotUpgrade", err)
	}
	if _, err := Classify("github.com/foo/bar", "v1.6.0", "v1.5.0"); !errors.Is(err, ErrNotUpgrade) {
		t.Errorf("downgrade: err = %v, want ErrNotUpgrade", err)
	}
}

func TestBadVersion(t *testing.T) {
	t.Parallel()
	if _, err := Classify("github.com/foo/bar", "1.6.0", "v1.7.0"); !errors.Is(err, ErrBadVersion) {
		t.Errorf("err = %v, want ErrBadVersion", err)
	}
}

func ExampleClassify() {
	d, _ := Classify("github.com/google/uuid", "v1.6.0", "v2.0.0")
	fmt.Printf("%s auto=%v path=%s\n", d.Kind, d.AutoMerge, d.NewImportPath)
	// Output: major auto=false path=github.com/google/uuid/v2
}
```

## Review

The engine is correct when its verdict matches what a careful reviewer would decide:
patch and minor bumps within one major auto-merge, a major bump is blocked and names
the new `/vN` import path, and a prerelease is never auto-merged even when its number
is a valid patch or minor step. The subtle, load-bearing case is the major bump:
because Semantic Import Versioning encodes the major in the path, an automated tool
that just edits the version number produces a build that still imports v1 — so the
policy must surface `NewImportPath` and mark the change as human work.
`module.SplitPathVersion` is what makes the path math right for a module already at
`/v2` bumping to `/v3` (prefix stays `github.com/foo/bar`). Run `go test -race`.

## Resources

- [`golang.org/x/mod/semver`](https://pkg.go.dev/golang.org/x/mod/semver) — `Major`, `MajorMinor`, `Prerelease`, `Compare`.
- [`module.SplitPathVersion`](https://pkg.go.dev/golang.org/x/mod/module#SplitPathVersion) — splitting the `/vN` suffix from a module path.
- [Module version numbering](https://go.dev/doc/modules/version-numbers) — patch/minor/major semantics and Semantic Import Versioning.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-build-list-mvs-resolver.md](05-build-list-mvs-resolver.md) | Next: [07-gosum-integrity-verifier.md](07-gosum-integrity-verifier.md)
