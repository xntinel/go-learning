# Exercise 8: Detect accidental breaking changes and pick the right version with gorelease

Relying on a human reviewer to notice that a return tuple changed does not scale
past a handful of symbols. `gorelease` compares your working tree against a tagged
base, classifies the API diff, and *names the correct next version* — refusing to
let an incompatible change ship as a minor bump. This exercise builds the decision
logic `gorelease` automates as a small, testable release-gate package, then shows
how to wire the real `gorelease` invocation into CI.

This module is fully self-contained: its own `go mod init`, a `release` package
that classifies an API diff into the correct SemVer bump, a demo, and tests over
the classification rules.

## What you'll build

```text
releasegate/               independent module: example.com/publicstr
  go.mod                   go 1.26
  release.go               Diff, Change, Classify, NextVersion (the bump decision)
  cmd/
    demo/
      main.go              runnable demo classifying three diffs
  release_test.go          patch/minor/major and v0 rules, table-driven
```

- Files: `release.go`, `cmd/demo/main.go`, `release_test.go`.
- Implement: a `Diff` describing added/removed/changed exported symbols, `Classify` mapping it to `Change` (none/additive/breaking), and `NextVersion(base, diff)` returning the required next version and whether a new major import path is needed.
- Test: no change gives a patch, an addition gives a minor, an incompatible change at v1+ requires a new major path, and v0 allows a break as a minor bump.
- Verify: `go test -count=1 -race ./...`

### The three tiers, as a decision function

`gorelease` embodies the same three-tier rule this whole lesson turns on, and it
is worth building the rule as code so it is unambiguous:

- No visible API change gives a patch bump (`v1.4.2` to `v1.4.3`).
- A backward-compatible addition (a new symbol, a new option) gives a minor bump
  (`v1.4.2` to `v1.5.0`).
- Any incompatible change (a removed, renamed, or retyped symbol) at major version
  1 or higher requires a *new major version with a new import path* (`v1.4.2` to
  `v2.0.0` at `.../v2`) — `gorelease` exits non-zero rather than suggesting a
  minor. At major version 0, the same incompatible change is permitted as a minor
  bump, because v0 carries no compatibility promise.

`Classify` reduces a `Diff` to one of three `Change` values by that priority: any
removed or changed symbol is `Breaking`; otherwise any added symbol is `Additive`;
otherwise `None`. `NextVersion` then applies the SemVer rule, parsing the base
`vMAJOR.MINOR.PATCH`, and returns the next version plus a `NewMajorPath` flag that
is true exactly when a breaking change at v1+ forces a `/vN` import path. This is
the decision the CI gate makes on every proposed release; expressing it as a pure
function is what makes it testable and unambiguous.

The real `gorelease` computes the `Diff` for you by analyzing the actual package
API against the tagged base (it uses `golang.org/x/exp/apidiff` underneath), so in
practice you do not hand-build a `Diff` — you run the tool. The value of modeling
the decision here is that you understand exactly what verdict the tool is
producing and why a given change forces a major bump.

Create `release.go`:

```go
// Package release models the API-compatibility decision that gorelease automates:
// given the diff between a released base and the working tree, it classifies the
// change and names the correct next version.
package release

import (
	"fmt"
	"strconv"
	"strings"
)

// Change is the compatibility tier of an API diff.
type Change int

const (
	// None means no visible API change: a patch release.
	None Change = iota
	// Additive means only backward-compatible additions: a minor release.
	Additive
	// Breaking means an incompatible change: a new major version (at v1+).
	Breaking
)

func (c Change) String() string {
	switch c {
	case None:
		return "none"
	case Additive:
		return "additive"
	case Breaking:
		return "breaking"
	default:
		return "unknown"
	}
}

// Diff describes how the working tree's exported surface differs from the base.
type Diff struct {
	Added   []string // newly exported symbols
	Removed []string // removed or renamed exported symbols
	Changed []string // symbols whose signature or type changed
}

// Classify reduces a Diff to its compatibility tier. Removals and changes are
// breaking; otherwise additions are additive; otherwise there is no change.
func Classify(d Diff) Change {
	if len(d.Removed) > 0 || len(d.Changed) > 0 {
		return Breaking
	}
	if len(d.Added) > 0 {
		return Additive
	}
	return None
}

type semver struct{ major, minor, patch int }

func parseSemver(v string) (semver, error) {
	s := strings.TrimPrefix(v, "v")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("release: bad version %q", v)
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, fmt.Errorf("release: bad version %q", v)
		}
		out[i] = n
	}
	return semver{out[0], out[1], out[2]}, nil
}

// NextVersion returns the smallest version that base may advance to given the
// change tier, and whether the change forces a new major import path (a /vN
// module). A breaking change at v0 is allowed as a minor bump; at v1+ it forces a
// new major version.
func NextVersion(base string, d Diff) (next string, newMajorPath bool, err error) {
	v, err := parseSemver(base)
	if err != nil {
		return "", false, err
	}
	switch Classify(d) {
	case None:
		return fmt.Sprintf("v%d.%d.%d", v.major, v.minor, v.patch+1), false, nil
	case Additive:
		return fmt.Sprintf("v%d.%d.0", v.major, v.minor+1), false, nil
	default: // Breaking
		if v.major == 0 {
			return fmt.Sprintf("v0.%d.0", v.minor+1), false, nil
		}
		return fmt.Sprintf("v%d.0.0", v.major+1), true, nil
	}
}
```

### Wiring the real gorelease into CI

The package above is the decision; `gorelease` is the tool that computes the diff
from real code and enforces it. In CI you run it against the last released tag and
fail the job if the proposed `-version` is not consistent with the change:

```bash
# Install once (module tool, run via go run to avoid pinning a global binary).
go run golang.org/x/exp/cmd/gorelease@latest -base=v1.0.0 -version=v1.1.0

# Typical outputs:
#   "Inferred base version: v1.0.0"        added an Option -> minor is accepted
#   "v1.1.0 is a valid semantic version..." exit 0
#
# If you changed an exported signature but proposed a minor bump, gorelease
# reports the incompatible change and exits non-zero:
#   "func Truncate: changed from func(string, int) (string, error) to
#    func(string, int) (string, bool)"
#   "This is an incompatible change. Increment the major version..."
```

Because `gorelease` exits non-zero on a mismatch, a single CI line — `go run
golang.org/x/exp/cmd/gorelease@latest -base="$(git describe --tags --abbrev=0)"
-version="$PROPOSED"` — turns "did we break someone" from a manual review into a
mechanical gate.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	release "example.com/publicstr"
)

func main() {
	diffs := []struct {
		name string
		diff release.Diff
	}{
		{"bugfix only", release.Diff{}},
		{"added an option", release.Diff{Added: []string{"WithMaxLen"}}},
		{"changed a signature", release.Diff{Changed: []string{"Truncate"}}},
	}
	for _, d := range diffs {
		next, newPath, _ := release.NextVersion("v1.4.2", d.diff)
		fmt.Printf("%-20s %-9s -> %s (new major path: %v)\n",
			d.name, release.Classify(d.diff), next, newPath)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
bugfix only          none      -> v1.4.3 (new major path: false)
added an option      additive  -> v1.5.0 (new major path: false)
changed a signature  breaking  -> v2.0.0 (new major path: true)
```

### Tests

The tests encode the gate's own pass/fail: adding an option must yield a minor
bump; changing a signature at v1 must force a new major path; the same break at v0
is only a minor bump.

Create `release_test.go`:

```go
package release

import "testing"

func TestClassify(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		diff Diff
		want Change
	}{
		{"empty", Diff{}, None},
		{"added", Diff{Added: []string{"WithMaxLen"}}, Additive},
		{"removed", Diff{Removed: []string{"Reverse"}}, Breaking},
		{"changed", Diff{Changed: []string{"Truncate"}}, Breaking},
		{"added and changed", Diff{Added: []string{"X"}, Changed: []string{"Y"}}, Breaking},
	}
	for _, tc := range cases {
		if got := Classify(tc.diff); got != tc.want {
			t.Errorf("%s: Classify = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNextVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		base       string
		diff       Diff
		wantNext   string
		wantNewMaj bool
	}{
		{"patch", "v1.4.2", Diff{}, "v1.4.3", false},
		{"minor on add", "v1.4.2", Diff{Added: []string{"WithMaxLen"}}, "v1.5.0", false},
		{"major on break", "v1.4.2", Diff{Changed: []string{"Truncate"}}, "v2.0.0", true},
		{"remove is major", "v3.0.0", Diff{Removed: []string{"Old"}}, "v4.0.0", true},
		{"v0 break is minor", "v0.3.1", Diff{Changed: []string{"Truncate"}}, "v0.4.0", false},
	}
	for _, tc := range cases {
		next, newMaj, err := NextVersion(tc.base, tc.diff)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if next != tc.wantNext || newMaj != tc.wantNewMaj {
			t.Errorf("%s: NextVersion = %s,%v; want %s,%v",
				tc.name, next, newMaj, tc.wantNext, tc.wantNewMaj)
		}
	}
}

func TestNextVersionRejectsBadBase(t *testing.T) {
	t.Parallel()
	if _, _, err := NextVersion("1.2", Diff{}); err == nil {
		t.Fatal("want error on malformed base version")
	}
}
```

## Review

The gate is correct when its classification matches the SemVer rule exactly: no
change is a patch, an addition is a minor, and a removal or signature change at v1+
forces `v(N+1).0.0` with a new major import path — while the same break at v0 is
only a minor bump because v0 promises nothing. Building this as a pure function is
what makes the rule unambiguous and testable; in production you do not hand-build
the `Diff`, you let `gorelease` compute it from the real API against the tagged
base and exit non-zero on a mismatch. The failure mode this prevents is the whole
reason the tool exists: an engineer changes a return tuple, proposes a minor bump,
review misses it, and every downstream build breaks on the next `go get`. Wire the
`gorelease` line into CI and that cannot happen silently.

## Resources

- [gorelease](https://pkg.go.dev/golang.org/x/exp/cmd/gorelease) — the pre-release API-compatibility and version check.
- [`golang.org/x/exp/apidiff`](https://pkg.go.dev/golang.org/x/exp/apidiff) — the API-diff engine `gorelease` builds on.
- [Go Blog: Keeping your modules compatible](https://go.dev/blog/module-compatibility) — the compatibility rules the gate enforces.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-deprecate-and-migrate-without-breaking.md](07-deprecate-and-migrate-without-breaking.md) | Next: [09-ship-a-breaking-change-as-v2.md](09-ship-a-breaking-change-as-v2.md)
