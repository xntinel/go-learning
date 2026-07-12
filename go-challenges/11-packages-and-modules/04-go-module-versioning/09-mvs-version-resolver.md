# Exercise 9: Implement Minimal Version Selection to explain the build list

"Why did `go` pick that version?" has a precise answer, and it is not "the latest."
The build list is computed by Minimal Version Selection: for each module, take the
maximum over every minimum that anyone in the graph requires. Here you implement that
algorithm over a synthetic requirement graph, which is the fastest way to internalize
why MVS is deterministic, monotonic, and never reaches for a version no one asked for.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
mvs/                        independent module: example.com/mvs
  go.mod
  mvs.go                    Module, Graph, Resolve() computing the build list
  cmd/
    demo/
      main.go               runnable: resolve a small graph, print the sorted list
  mvs_test.go               max-of-minimums; never-latest; never-downgrade; invalid input
```

- Files: `mvs.go`, `cmd/demo/main.go`, `mvs_test.go`.
- Implement: `Resolve(root Module, graph Graph) ([]Module, error)` selecting, per module path, the maximum required version via `semver.Compare`, output sorted by path.
- Test: `A` requires `C v1.2.0`, `B` requires `C v1.5.0` → `C` resolves to `v1.5.0`; prove it is max-of-minimums (never a higher unrequested version) and never below any requirement; a requirement without a leading `v` errors.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/04-go-module-versioning/09-mvs-version-resolver/cmd/demo
cd go-solutions/11-packages-and-modules/04-go-module-versioning/09-mvs-version-resolver
go mod edit -go=1.26
```

### The algorithm, and why it is max of the minimums

Every `require` line states a *minimum*: "I need at least this version." MVS turns
the set of minimums into a single selection per module by taking the maximum of them.
The graph is keyed by a specific module *version* — `example.com/b` at `v1.0.0` may
require different things than at `v2.0.0` — so the resolver walks from the main
module, follows each required `module@version` node, and for every module path keeps
the highest version anyone requires. Reachability plus a running max.

Two properties fall out, and they are the whole reason the algorithm exists. It is
**deterministic**: the selection depends only on the graph, not on what happens to be
newest in a registry at build time, so the same `go.mod`/`go.sum` builds the same
bytes forever — the reproducibility guarantee. And it is **monotonic**: adding a
requirement can only hold a version equal or higher, never silently downgrade an
unrelated module, because the max of a superset is `>=` the max of the subset. When
`A` requires `C v1.2.0` and `B` requires `C v1.5.0`, MVS selects `C v1.5.0` — the
minimum that satisfies *both* — and it does so even if `C v1.9.0` exists, because no
one *required* `v1.9.0`. "Newest required," never "newest available."

The validity check matters for the same reason it did in the compatibility gate:
`semver.Compare` no-ops on a version without a leading `v`, so an unvalidated `"1.5.0"`
would compare as `0` and could be selected over a real `v1.2.0`. Validate every
requirement with `semver.IsValid` and fail loudly instead.

(This models MVS's selection pass — the max over the reachable requirement graph. A
full toolchain implementation then re-reads the *selected* version's requirements to
prune versions no longer referenced; the selection rule you implement here is the
core.)

Create `mvs.go`:

```go
package mvs

import (
	"fmt"
	"sort"

	"golang.org/x/mod/semver"
)

// Module is a module path at a specific version.
type Module struct {
	Path    string
	Version string
}

// Graph maps each module@version to the modules it directly requires.
type Graph map[Module][]Module

// Resolve computes the build list for root: for every module path reachable in
// the graph, the maximum version any requirement demands. Output is sorted by
// path for determinism. It errors on a requirement that is not valid semver.
func Resolve(root Module, graph Graph) ([]Module, error) {
	selected := map[string]string{} // path -> highest required version
	seen := map[Module]bool{}

	var visit func(m Module) error
	visit = func(m Module) error {
		if seen[m] {
			return nil
		}
		seen[m] = true
		for _, dep := range graph[m] {
			if !semver.IsValid(dep.Version) {
				return fmt.Errorf("mvs: %s requires %s at invalid version %q", m.Path, dep.Path, dep.Version)
			}
			cur, ok := selected[dep.Path]
			if !ok || semver.Compare(dep.Version, cur) > 0 {
				selected[dep.Path] = dep.Version
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(root); err != nil {
		return nil, err
	}

	out := make([]Module, 0, len(selected))
	for path, version := range selected {
		out = append(out, Module{Path: path, Version: version})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
```

### The runnable demo

The demo resolves a graph where two modules require different minimums of `C`; MVS
picks the higher one, and `D` rides in through `B`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mvs"
)

func main() {
	root := mvs.Module{Path: "example.com/app", Version: "v1.0.0"}
	a := mvs.Module{Path: "example.com/a", Version: "v1.0.0"}
	b := mvs.Module{Path: "example.com/b", Version: "v1.0.0"}

	graph := mvs.Graph{
		root: {a, b},
		a:    {{Path: "example.com/c", Version: "v1.2.0"}},
		b: {
			{Path: "example.com/c", Version: "v1.5.0"},
			{Path: "example.com/d", Version: "v0.3.0"},
		},
	}

	list, err := mvs.Resolve(root, graph)
	if err != nil {
		fmt.Println("resolve failed:", err)
		return
	}
	fmt.Println("build list:")
	for _, m := range list {
		fmt.Printf("  %s %s\n", m.Path, m.Version)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
build list:
  example.com/a v1.0.0
  example.com/b v1.0.0
  example.com/c v1.5.0
  example.com/d v0.3.0
```

### Tests

The tests pin the three MVS invariants — max-of-minimums, never-latest,
never-downgrade — and the invalid-version failure.

Create `mvs_test.go`:

```go
package mvs

import (
	"slices"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
)

func buildGraph() (Module, Graph) {
	root := Module{Path: "example.com/app", Version: "v1.0.0"}
	a := Module{Path: "example.com/a", Version: "v1.0.0"}
	b := Module{Path: "example.com/b", Version: "v1.0.0"}
	graph := Graph{
		root: {a, b},
		a:    {{Path: "example.com/c", Version: "v1.2.0"}},
		b:    {{Path: "example.com/c", Version: "v1.5.0"}},
	}
	return root, graph
}

func selectedVersion(list []Module, path string) (string, bool) {
	for _, m := range list {
		if m.Path == path {
			return m.Version, true
		}
	}
	return "", false
}

func TestResolveMaxOfMinimums(t *testing.T) {
	t.Parallel()
	root, graph := buildGraph()
	list, err := Resolve(root, graph)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, ok := selectedVersion(list, "example.com/c")
	if !ok {
		t.Fatal("C not in build list")
	}
	if got != "v1.5.0" {
		t.Fatalf("selected C = %s, want v1.5.0 (max of v1.2.0 and v1.5.0)", got)
	}
}

func TestNeverLatestAvailable(t *testing.T) {
	t.Parallel()
	// v1.9.0 exists in the registry but nobody requires it; MVS must not pick it.
	root, graph := buildGraph()
	list, err := Resolve(root, graph)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, _ := selectedVersion(list, "example.com/c")
	if semver.Compare(got, "v1.9.0") >= 0 {
		t.Fatalf("selected C = %s, must not reach an unrequested v1.9.0", got)
	}
}

func TestNeverDowngrades(t *testing.T) {
	t.Parallel()
	root, graph := buildGraph()
	list, err := Resolve(root, graph)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, _ := selectedVersion(list, "example.com/c")
	// The selection is at least every required minimum.
	for _, min := range []string{"v1.2.0", "v1.5.0"} {
		if semver.Compare(got, min) < 0 {
			t.Fatalf("selected C = %s downgraded below required %s", got, min)
		}
	}
}

func TestSortedDeterministic(t *testing.T) {
	t.Parallel()
	root, graph := buildGraph()
	list, err := Resolve(root, graph)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	paths := make([]string, len(list))
	for i, m := range list {
		paths[i] = m.Path
	}
	if !slices.IsSorted(paths) {
		t.Fatalf("build list not sorted by path: %v", paths)
	}
}

func TestRejectsInvalidVersion(t *testing.T) {
	t.Parallel()
	root := Module{Path: "example.com/app", Version: "v1.0.0"}
	graph := Graph{
		root: {{Path: "example.com/c", Version: "1.5.0"}}, // missing leading v
	}
	_, err := Resolve(root, graph)
	if err == nil || !strings.Contains(err.Error(), "invalid version") {
		t.Fatalf("Resolve err = %v, want an invalid-version error", err)
	}
}
```

## Review

The resolver is correct when, per module path, it selects the maximum version any
requirement demands and nothing higher — `C` resolves to `v1.5.0` because `B`
required it and `A` required only `v1.2.0`, and it never reaches an unrequested
`v1.9.0`. `TestNeverDowngrades` proves the selection is `>=` every minimum, which is
the monotonicity that lets you add a dependency without fear of silently regressing
another. Determinism comes from sorting the output by path; `semver.IsValid` guards
against the no-op comparison that a `v`-less version would cause. The mental model to
keep: `go.mod` records minimums, MVS returns the max of them, and "newest available"
never enters the computation — which is exactly why a build from an old `go.mod` is
reproducible.

## Resources

- [Go Modules Reference: Minimal Version Selection](https://go.dev/ref/mod#minimal-version-selection) — the normative algorithm.
- [Russ Cox: Minimal Version Selection](https://research.swtch.com/vgo-mvs) — the original design essay.
- [`golang.org/x/mod/semver`](https://pkg.go.dev/golang.org/x/mod/semver) — `Compare` and `IsValid` used by the resolver.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-pseudo-version-inspector.md](10-pseudo-version-inspector.md)
