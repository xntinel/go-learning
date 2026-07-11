# Exercise 24: Resolve Semantic-Versioned Deps with Backtracking Memoization

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Resolving a package's dependency tree to a single, consistent set of
versions is a classic backtracking problem: pick the newest candidate that
satisfies the current requirement, recurse into whatever *that* version
itself requires, and if anything downstream turns out unsatisfiable, undo
the pick and try the next-best candidate instead. Two sibling packages that
both, through several layers of their own dependencies, end up needing an
incompatible version of some shared package will cause the same downstream
failure to be rediscovered over and over as the search tries different
combinations of the siblings' own versions — unless the resolver recognizes
it has already seen this exact remaining subproblem before.

This module is fully self-contained: its own `go mod init`, the resolver
inline, its own demo and tests.

## What you'll build

```text
depresolve/                   independent module: example.com/depresolve
  go.mod                        go 1.24
  depresolve.go                  type Version; type Requirement; type Registry; type Resolver
  depresolve_test.go              version compare, no-backtrack case, backtrack case, unsatisfiable, memo pruning, empty root
  cmd/
    demo/
      main.go                     resolves lib+util where the newest lib conflicts, forcing a backtrack
```

- Files: `depresolve.go`, `cmd/demo/main.go`, `depresolve_test.go`.
- Implement: `Version` (parse, compare, string), `Requirement` (a package
  name plus an inclusive min/max range), `Registry` (available versions and
  each version's own requirements), and `Resolver` with
  `func (r *Resolver) Resolve(root []Requirement) (map[string]Version, error)`
  backed by recursive backtracking memoized on the remaining subproblem.
- Test: version comparison; a trivial single-package case; a case that must
  backtrack off the newest version of a dependency; two directly conflicting
  root requirements; a constructed diamond where the memo measurably prunes
  repeated failure; an empty requirement set.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/depresolve/cmd/demo
cd ~/go-exercises/depresolve
go mod init example.com/depresolve
go mod edit -go=1.24
```

### Memoizing on the whole subproblem, not just a version pick

A tempting first design memoizes failure by `(package, version)` alone:
"once `util@1.5.0` fails, never try it again." That is unsound here, and
it is worth seeing why concretely. Suppose `util@1.5.0` fails while `lib` is
pinned at `3.0.0`, because `lib@3.0.0`'s own requirement on `util`
conflicts with the app's direct requirement. Backtracking then tries
`lib@2.0.0` instead — which requires a *different* range of `util`, one
that `util@1.5.0` actually satisfies. A memo keyed only on `(util, 1.5.0)`
would incorrectly skip retrying it, because the fact that it failed
earlier had nothing to do with `util@1.5.0` itself — it was a conflict
between two *other* requirements that happened to be in play at the time.

The fix is to memoize on the actual thing that determines the outcome: the
remaining requirement queue, plus the already-chosen versions of any
package that queue will still ask about. `stateKey` builds exactly that —
and deliberately leaves out chosen versions of packages that no longer
appear anywhere in the queue, because those are done and cannot affect what
happens next. That last part is what turns the memo from "technically
sound but rarely fires" into something that actually prunes real work: two
different combinations of already-resolved siblings can leave behind the
*exact same* remaining subproblem, once whatever made them different has
been fully resolved and dropped from the queue, and the memo recognizes
that recurrence.

Create `depresolve.go`:

```go
// Package depresolve resolves a set of semantic-versioned package
// requirements by recursive backtracking: pick a candidate version for the
// next unresolved requirement, recurse into its own transitive
// requirements, and if that fails, undo the pick and try the next
// candidate. A memo of already-failed subproblems prunes the search: when
// backtracking causes the exact same "remaining requirements, given what's
// already chosen" state to be reached a second time by a different path,
// the resolver reuses the earlier failure instead of re-deriving it.
package depresolve

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ErrUnsatisfiable is returned when no combination of versions satisfies
// every requirement.
var ErrUnsatisfiable = errors.New("depresolve: no version satisfies all constraints")

// Version is a semantic version (major.minor.patch).
type Version struct {
	Major, Minor, Patch int
}

// ParseVersion parses "major.minor.patch", e.g. "1.2.0".
func ParseVersion(s string) (Version, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("depresolve: %q is not major.minor.patch", s)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, fmt.Errorf("depresolve: %q: %w", s, err)
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2]}, nil
}

// MustVersion is ParseVersion, panicking on error. Intended for tests and
// registry setup with literal, known-good version strings.
func MustVersion(s string) Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return v
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Compare returns -1, 0, or 1 as v is less than, equal to, or greater than o.
func (v Version) Compare(o Version) int {
	switch {
	case v.Major != o.Major:
		return sign(v.Major - o.Major)
	case v.Minor != o.Minor:
		return sign(v.Minor - o.Minor)
	default:
		return sign(v.Patch - o.Patch)
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// Requirement constrains a package to versions in [Min, Max] inclusive.
type Requirement struct {
	Package  string
	Min, Max Version
}

// Satisfies reports whether v falls within the requirement's range.
func (req Requirement) Satisfies(v Version) bool {
	return v.Compare(req.Min) >= 0 && v.Compare(req.Max) <= 0
}

// Registry holds every package's available versions and each version's own
// dependency requirements.
type Registry struct {
	Versions map[string][]Version
	Deps     map[string]map[Version][]Requirement
}

// Resolver runs backtracking resolution against a fixed Registry.
type Resolver struct {
	reg      *Registry
	failed   map[string]bool // memoized subproblem states already known unsatisfiable
	Attempts int             // candidate versions actually tried (not served from the memo)
	MemoHits int             // times a subproblem was skipped via the memo
}

// NewResolver returns a Resolver for reg.
func NewResolver(reg *Registry) *Resolver {
	return &Resolver{reg: reg, failed: make(map[string]bool)}
}

// Resolve finds one version per package that satisfies root and every
// transitive requirement introduced by the versions chosen, or
// ErrUnsatisfiable if no such assignment exists.
func (r *Resolver) Resolve(root []Requirement) (map[string]Version, error) {
	chosen := make(map[string]Version)
	return r.resolve(append([]Requirement(nil), root...), chosen)
}

func (r *Resolver) resolve(queue []Requirement, chosen map[string]Version) (map[string]Version, error) {
	if len(queue) == 0 {
		return copyChosen(chosen), nil
	}

	key := stateKey(queue, chosen)
	if r.failed[key] {
		r.MemoHits++
		return nil, fmt.Errorf("%s: %w (cached)", queue[0].Package, ErrUnsatisfiable)
	}

	req, rest := queue[0], queue[1:]

	if v, ok := chosen[req.Package]; ok {
		if !req.Satisfies(v) {
			r.failed[key] = true
			return nil, fmt.Errorf("%s@%s: %w: conflicts with already-chosen version", req.Package, v, ErrUnsatisfiable)
		}
		result, err := r.resolve(rest, chosen)
		if err != nil {
			r.failed[key] = true
		}
		return result, err
	}

	for _, v := range candidatesDesc(r.reg, req.Package) {
		if !req.Satisfies(v) {
			continue
		}
		r.Attempts++
		chosen[req.Package] = v
		next := append(append([]Requirement(nil), rest...), r.reg.Deps[req.Package][v]...)
		result, err := r.resolve(next, chosen)
		delete(chosen, req.Package)
		if err == nil {
			return result, nil
		}
	}

	r.failed[key] = true
	return nil, fmt.Errorf("%s: %w", req.Package, ErrUnsatisfiable)
}

// stateKey canonicalizes a subproblem as the remaining requirement queue
// plus only the already-chosen versions of packages that still appear
// somewhere in that queue. Chosen versions of packages that will never be
// looked at again are irrelevant to what happens next, so leaving them out
// lets equivalent subproblems reached via different backtracking paths
// (different earlier choices that happened not to matter) be recognized as
// the same state — this is what makes the memo hit in practice, not just in
// principle.
func stateKey(queue []Requirement, chosen map[string]Version) string {
	names := make(map[string]bool, len(queue))
	parts := make([]string, len(queue))
	for i, req := range queue {
		parts[i] = req.Package + ":" + req.Min.String() + "-" + req.Max.String()
		names[req.Package] = true
	}
	sort.Strings(parts)

	var chosenParts []string
	for name := range names {
		if v, ok := chosen[name]; ok {
			chosenParts = append(chosenParts, name+"="+v.String())
		}
	}
	sort.Strings(chosenParts)

	return strings.Join(parts, ",") + "|" + strings.Join(chosenParts, ",")
}

func candidatesDesc(reg *Registry, pkg string) []Version {
	versions := append([]Version(nil), reg.Versions[pkg]...)
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Compare(versions[j]) > 0
	})
	return versions
}

func copyChosen(chosen map[string]Version) map[string]Version {
	out := make(map[string]Version, len(chosen))
	for k, v := range chosen {
		out[k] = v
	}
	return out
}
```

### The runnable demo

The demo has `lib` (versions 3.0.0, 2.0.0, 1.0.0) and `util` (2.0.0, 1.5.0).
`lib@3.0.0` requires a `util` range that conflicts with the app's own
direct `util` requirement, forcing a backtrack to `lib@2.0.0`, whose own
`util` requirement agrees with the app's.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/depresolve"
)

func main() {
	v := depresolve.MustVersion

	reg := &depresolve.Registry{
		Versions: map[string][]depresolve.Version{
			"lib":  {v("3.0.0"), v("2.0.0"), v("1.0.0")},
			"util": {v("2.0.0"), v("1.5.0")},
		},
		Deps: map[string]map[depresolve.Version][]depresolve.Requirement{
			"lib": {
				// lib 3.0.0 needs a newer util than the app itself allows.
				v("3.0.0"): {{Package: "util", Min: v("2.0.0"), Max: v("2.9.9")}},
				v("2.0.0"): {{Package: "util", Min: v("1.0.0"), Max: v("1.9.9")}},
				v("1.0.0"): {{Package: "util", Min: v("1.0.0"), Max: v("1.9.9")}},
			},
		},
	}

	root := []depresolve.Requirement{
		{Package: "lib", Min: v("1.0.0"), Max: v("3.0.0")},
		{Package: "util", Min: v("1.0.0"), Max: v("1.9.9")},
	}

	resolver := depresolve.NewResolver(reg)
	chosen, err := resolver.Resolve(root)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	names := make([]string, 0, len(chosen))
	for name := range chosen {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%s: %s\n", name, chosen[name])
	}
	fmt.Printf("attempts: %d\n", resolver.Attempts)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
lib: 2.0.0
util: 1.5.0
attempts: 4
```

### Tests

`TestVersionCompare` and `TestResolveNoBacktrackingNeeded` check the small
building blocks. `TestResolveBacktracksToLowerVersion` is the demo's
scenario as an assertion. `TestResolveUnsatisfiableConflictingDirectRequirements`
checks the simplest failure shape. `TestResolveMemoPrunesRepeatedFailure` is
the test that justifies the whole exercise: two packages with two versions
each, all four combinations funneling into the same unsatisfiable
downstream requirement, and it asserts both that the resolver still fails
correctly *and* that `Attempts` stays low while `MemoHits` is nonzero —
proof the repeated failure was served from the memo rather than
re-derived. `TestResolveEmptyRootIsTriviallySatisfied` covers the trivial
input.

Create `depresolve_test.go`:

```go
package depresolve

import (
	"errors"
	"testing"
)

func v(s string) Version { return MustVersion(s) }

func TestVersionCompare(t *testing.T) {
	t.Parallel()

	if v("1.2.0").Compare(v("1.2.0")) != 0 {
		t.Fatal("equal versions should compare 0")
	}
	if v("2.0.0").Compare(v("1.9.9")) <= 0 {
		t.Fatal("2.0.0 should be greater than 1.9.9")
	}
	if v("1.0.0").Compare(v("1.0.1")) >= 0 {
		t.Fatal("1.0.0 should be less than 1.0.1")
	}
}

func TestResolveNoBacktrackingNeeded(t *testing.T) {
	t.Parallel()

	reg := &Registry{
		Versions: map[string][]Version{"a": {v("1.0.0")}},
		Deps:     map[string]map[Version][]Requirement{},
	}
	root := []Requirement{{Package: "a", Min: v("1.0.0"), Max: v("1.0.0")}}

	r := NewResolver(reg)
	got, err := r.Resolve(root)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got["a"] != v("1.0.0") {
		t.Fatalf("a = %v, want 1.0.0", got["a"])
	}
}

func TestResolveBacktracksToLowerVersion(t *testing.T) {
	t.Parallel()

	reg := &Registry{
		Versions: map[string][]Version{
			"lib":  {v("3.0.0"), v("2.0.0"), v("1.0.0")},
			"util": {v("2.0.0"), v("1.5.0")},
		},
		Deps: map[string]map[Version][]Requirement{
			"lib": {
				v("3.0.0"): {{Package: "util", Min: v("2.0.0"), Max: v("2.9.9")}},
				v("2.0.0"): {{Package: "util", Min: v("1.0.0"), Max: v("1.9.9")}},
				v("1.0.0"): {{Package: "util", Min: v("1.0.0"), Max: v("1.9.9")}},
			},
		},
	}
	root := []Requirement{
		{Package: "lib", Min: v("1.0.0"), Max: v("3.0.0")},
		{Package: "util", Min: v("1.0.0"), Max: v("1.9.9")},
	}

	r := NewResolver(reg)
	got, err := r.Resolve(root)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got["lib"] != v("2.0.0") {
		t.Errorf("lib = %v, want 2.0.0 (3.0.0 conflicts with util's own required range)", got["lib"])
	}
	if got["util"] != v("1.5.0") {
		t.Errorf("util = %v, want 1.5.0", got["util"])
	}
}

func TestResolveUnsatisfiableConflictingDirectRequirements(t *testing.T) {
	t.Parallel()

	reg := &Registry{
		Versions: map[string][]Version{"solo": {v("1.0.0"), v("2.0.0")}},
		Deps:     map[string]map[Version][]Requirement{},
	}
	root := []Requirement{
		{Package: "solo", Min: v("1.0.0"), Max: v("1.0.0")},
		{Package: "solo", Min: v("2.0.0"), Max: v("2.0.0")},
	}

	r := NewResolver(reg)
	_, err := r.Resolve(root)
	if !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("Resolve() error = %v, want %v", err, ErrUnsatisfiable)
	}
}

// TestResolveMemoPrunesRepeatedFailure builds a case where two independent
// packages (p1, p2), each with two candidate versions, all funnel into the
// same unsatisfiable requirement on "shared". Without memoization, every
// one of the 2x2 version combinations would independently rediscover that
// "shared" cannot be satisfied; with it, only the first combination that
// reaches the "shared" subproblem actually computes the failure, and every
// other combination that reaches the identical remaining subproblem reuses
// it.
func TestResolveMemoPrunesRepeatedFailure(t *testing.T) {
	t.Parallel()

	sharedReq := Requirement{Package: "shared", Min: v("9.9.9"), Max: v("9.9.9")}
	reg := &Registry{
		Versions: map[string][]Version{
			"p1":     {v("2.0.0"), v("1.0.0")},
			"p2":     {v("2.0.0"), v("1.0.0")},
			"shared": {v("2.0.0"), v("1.0.0")},
		},
		Deps: map[string]map[Version][]Requirement{
			"p1": {
				v("2.0.0"): {sharedReq},
				v("1.0.0"): {sharedReq},
			},
			"p2": {
				v("2.0.0"): {sharedReq},
				v("1.0.0"): {sharedReq},
			},
		},
	}
	root := []Requirement{
		{Package: "p1", Min: v("1.0.0"), Max: v("2.0.0")},
		{Package: "p2", Min: v("1.0.0"), Max: v("2.0.0")},
	}

	r := NewResolver(reg)
	_, err := r.Resolve(root)
	if !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("Resolve() error = %v, want %v", err, ErrUnsatisfiable)
	}
	if r.MemoHits == 0 {
		t.Fatal("MemoHits = 0, want at least one repeated subproblem served from the memo")
	}
	if r.Attempts > 4 {
		t.Fatalf("Attempts = %d, want at most 4 (one per p1/p2 version actually tried); the memo should prevent re-deriving \"shared\" failures", r.Attempts)
	}
}

func TestResolveEmptyRootIsTriviallySatisfied(t *testing.T) {
	t.Parallel()

	reg := &Registry{Versions: map[string][]Version{}, Deps: map[string]map[Version][]Requirement{}}
	r := NewResolver(reg)
	got, err := r.Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve(nil) error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %v, want empty", got)
	}
}
```

Run it: `go test -count=1 ./...`

## Review

`Resolve` is correct when it finds a genuinely consistent version
assignment whenever one exists, correctly backtracking off a version whose
transitive requirements conflict rather than getting stuck on the first
(highest) candidate, and reports `ErrUnsatisfiable` honestly when no
assignment exists. `TestResolveMemoPrunesRepeatedFailure` is the test that
would fail (via a much higher `Attempts` count and zero `MemoHits`) on a
version of this exercise that either omits the memo entirely or — the more
interesting mistake — memoizes on `(package, version)` alone instead of on
the full remaining-subproblem signature. That narrower key is the mistake
this exercise targets: it looks like a reasonable simplification, and it
would even pass the earlier, simpler tests, but it silently discards
correct solutions whenever the same version of a package is reachable
through two different upstream choices that impose different downstream
requirements — exactly the `lib@3.0.0` versus `lib@2.0.0` situation the
backtracking test is built around.

## Resources

- [Semantic Versioning 2.0.0 specification](https://semver.org/)
- [Backtracking (general technique)](https://en.wikipedia.org/wiki/Backtracking)
- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-sql-query-ast-optimization-rewrite.md](23-sql-query-ast-optimization-rewrite.md) | Next: [25-jwt-claim-nesting-depth-validation.md](25-jwt-claim-nesting-depth-validation.md)
