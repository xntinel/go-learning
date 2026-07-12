# Exercise 5: Reproduce go list -m all — a Minimal Version Selection resolver

MVS is the single most misunderstood thing about Go modules: the build uses the
*highest required* version of each module, not the *latest published* one. This
exercise implements the core algorithm over a requirement graph, so the property
that makes builds deterministic — and the reason an unrelated bump can move a shared
transitive dependency — becomes code you can test.

This module is fully self-contained. It has its own `go mod init`, its own demo,
and its own tests, and imports nothing from the other exercises.

## What you'll build

```text
mvs/                       independent module: example.com/mvs
  go.mod                   go 1.26; requires golang.org/x/mod
  mvs.go                   BuildList(root, graph) ([]module.Version, error)
  cmd/
    demo/
      main.go              resolves a diamond dependency and prints the build list
  mvs_test.go              diamond selects the higher version; unrelated release ignored
```

- Files: `mvs.go`, `cmd/demo/main.go`, `mvs_test.go`.
- Implement: `BuildList` that walks a requirement graph and selects, for each module path, the highest version required by anyone reachable from the root, ordered with `semver`.
- Test: a diamond where two paths require different versions of a shared module; assert the higher is selected and that a newer unrelated release does not change unrelated selections.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/06-dependency-management/05-build-list-mvs-resolver/cmd/demo
cd go-solutions/11-packages-and-modules/06-dependency-management/05-build-list-mvs-resolver
go get golang.org/x/mod
```

### The algorithm

A requirement graph maps each concrete module version to the versions it requires:
`example.com/a@v1.0.0` requires `example.com/c@v1.1.0`, and so on. MVS computes the
build list by a traversal from the root: for each module path, keep the *maximum*
version required by any node reachable from the root, and — crucially — when you
raise a module to a higher version, follow *that* version's requirements, because a
newer release can require different things. Versions only ever rise, never fall, and
they only rise to a version something explicitly required, so the result is
deterministic and terminates. This is why publishing a brand-new upstream release
changes nothing until something in your graph requires it: an unreferenced version is
simply never visited.

Compare versions with `semver.Compare` (which orders `v1.2.0 < v1.10.0` correctly,
unlike string comparison) and validate them with `semver.IsValid`. The traversal
only recurses when it finds a strictly higher version for a path, so each version's
requirement set is expanded at most once.

Create `mvs.go`:

```go
package mvs

import (
	"errors"
	"fmt"
	"sort"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// ErrBadVersion is returned when the graph contains a non-semver version.
var ErrBadVersion = errors.New("invalid semantic version")

// Graph maps a concrete module version to the module versions it requires.
type Graph map[module.Version][]module.Version

// BuildList computes the Minimal Version Selection build list for root over the
// requirement graph: for each module path, the highest version required by any
// node reachable from root. The result is sorted by module path and excludes the
// root itself, matching `go list -m all` minus the main module.
func BuildList(root module.Version, g Graph) ([]module.Version, error) {
	selected := make(map[string]string) // path -> highest required version

	var visit func(m module.Version) error
	visit = func(m module.Version) error {
		for _, req := range g[m] {
			if !semver.IsValid(req.Version) {
				return fmt.Errorf("%w: %s@%s", ErrBadVersion, req.Path, req.Version)
			}
			cur, ok := selected[req.Path]
			if !ok || semver.Compare(req.Version, cur) > 0 {
				selected[req.Path] = req.Version
				// Follow the newly-selected version's own requirements.
				if err := visit(req); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(root); err != nil {
		return nil, err
	}

	list := make([]module.Version, 0, len(selected))
	for path, version := range selected {
		list = append(list, module.Version{Path: path, Version: version})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
	return list, nil
}

// Select returns the version BuildList chose for a given module path.
func Select(list []module.Version, path string) (string, bool) {
	for _, m := range list {
		if m.Path == path {
			return m.Version, true
		}
	}
	return "", false
}
```

### The runnable demo

The demo builds a diamond: the root requires `a` and `b`; `a` requires `c@v1.1.0`,
`b` requires `c@v1.2.0`. MVS selects `c@v1.2.0` — the higher of the two required
versions — for the whole build.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mvs"
	"golang.org/x/mod/module"
)

func main() {
	root := module.Version{Path: "example.com/app", Version: "v0.0.0"}
	a := module.Version{Path: "example.com/a", Version: "v1.0.0"}
	b := module.Version{Path: "example.com/b", Version: "v1.0.0"}

	g := mvs.Graph{
		root: {a, b},
		a:    {{Path: "example.com/c", Version: "v1.1.0"}},
		b:    {{Path: "example.com/c", Version: "v1.2.0"}},
	}

	list, err := mvs.BuildList(root, g)
	if err != nil {
		panic(err)
	}
	for _, m := range list {
		fmt.Printf("%s %s\n", m.Path, m.Version)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
example.com/a v1.0.0
example.com/b v1.0.0
example.com/c v1.2.0
```

### Tests

Create `mvs_test.go`:

```go
package mvs

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/mod/module"
)

func mv(path, version string) module.Version {
	return module.Version{Path: path, Version: version}
}

func TestDiamondSelectsHigher(t *testing.T) {
	t.Parallel()
	root := mv("example.com/app", "v0.0.0")
	a := mv("example.com/a", "v1.0.0")
	b := mv("example.com/b", "v1.0.0")
	g := Graph{
		root: {a, b},
		a:    {mv("example.com/c", "v1.1.0")},
		b:    {mv("example.com/c", "v1.2.0")},
	}
	list, err := BuildList(root, g)
	if err != nil {
		t.Fatalf("BuildList: %v", err)
	}
	if v, _ := Select(list, "example.com/c"); v != "v1.2.0" {
		t.Errorf("c selected %q, want v1.2.0", v)
	}
}

func TestUnrelatedReleaseIgnored(t *testing.T) {
	t.Parallel()
	root := mv("example.com/app", "v0.0.0")
	a := mv("example.com/a", "v1.0.0")
	b := mv("example.com/b", "v1.0.0")
	// A newer a@v2.0.0 exists in the graph but nothing requires it; it must not
	// change the build list.
	g := Graph{}
	g[root] = []module.Version{a, b}
	g[a] = []module.Version{mv("example.com/c", "v1.1.0")}
	g[b] = []module.Version{mv("example.com/c", "v1.2.0")}
	g[mv("example.com/a", "v2.0.0")] = []module.Version{mv("example.com/c", "v1.9.0")}
	list, err := BuildList(root, g)
	if err != nil {
		t.Fatalf("BuildList: %v", err)
	}
	if v, _ := Select(list, "example.com/c"); v != "v1.2.0" {
		t.Errorf("c selected %q, want v1.2.0 (unrelated a@v2 must be ignored)", v)
	}
	if v, _ := Select(list, "example.com/a"); v != "v1.0.0" {
		t.Errorf("a selected %q, want v1.0.0", v)
	}
}

func TestNewRequirementRaisesShared(t *testing.T) {
	t.Parallel()
	root := mv("example.com/app", "v0.0.0")
	a := mv("example.com/a", "v1.0.0")
	// Adding dependency d that requires a higher c raises the shared selection.
	d := mv("example.com/d", "v1.0.0")
	g := Graph{
		root: {a, d},
		a:    {mv("example.com/c", "v1.1.0")},
		d:    {mv("example.com/c", "v1.5.0")},
	}
	list, err := BuildList(root, g)
	if err != nil {
		t.Fatalf("BuildList: %v", err)
	}
	if v, _ := Select(list, "example.com/c"); v != "v1.5.0" {
		t.Errorf("c selected %q, want v1.5.0", v)
	}
}

func TestInvalidVersion(t *testing.T) {
	t.Parallel()
	root := mv("example.com/app", "v0.0.0")
	g := Graph{root: {mv("example.com/c", "1.2.0")}} // missing leading v
	if _, err := BuildList(root, g); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("err = %v, want ErrBadVersion", err)
	}
}

func ExampleBuildList() {
	root := mv("example.com/app", "v0.0.0")
	g := Graph{}
	g[root] = []module.Version{mv("example.com/a", "v1.0.0"), mv("example.com/b", "v1.0.0")}
	g[mv("example.com/a", "v1.0.0")] = []module.Version{mv("example.com/c", "v1.1.0")}
	g[mv("example.com/b", "v1.0.0")] = []module.Version{mv("example.com/c", "v1.2.0")}
	list, _ := BuildList(root, g)
	for _, m := range list {
		fmt.Printf("%s %s\n", m.Path, m.Version)
	}
	// Output:
	// example.com/a v1.0.0
	// example.com/b v1.0.0
	// example.com/c v1.2.0
}
```

## Review

The resolver is correct when, for every module path, it returns the maximum version
required anywhere in the reachable graph and nothing else: the diamond test proves it
picks `c@v1.2.0` over `c@v1.1.0`, the unrelated-release test proves a published-but-
unrequired `a@v2.0.0` is invisible, and the new-requirement test proves adding a
dependency can raise a shared transitive version — the exact "why did this move?"
incident. The load-bearing details are `semver.Compare` (never string comparison, or
`v1.10.0` sorts below `v1.2.0`) and recursing into the *newly-selected* version's
requirements so a higher release's different needs are honored. This is a teaching
model of MVS, not the production graph loader (`golang.org/x/mod/mvs` is that); it
omits pruning and replace handling to keep the algorithm visible. Run `go test -race`.

## Resources

- [Minimal Version Selection](https://go.dev/ref/mod#minimal-version-selection) — the algorithm and why it is deterministic.
- [The original MVS design](https://research.swtch.com/vgo-mvs) — Russ Cox's write-up of the selection rule.
- [`golang.org/x/mod/semver`](https://pkg.go.dev/golang.org/x/mod/semver) — `Compare` and `IsValid` for correct version ordering.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-gomod-directive-auditor.md](04-gomod-directive-auditor.md) | Next: [06-semver-upgrade-policy.md](06-semver-upgrade-policy.md)
