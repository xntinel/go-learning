# Exercise 7: Audit go.sum for completeness and drift against the module graph

An untidy `go.sum` — missing a `/go.mod` hash, missing an entry for a required
module, or carrying a stale hash for a module nothing needs — passes locally from
a warm cache and fails reproducibly in clean CI. This exercise builds the CI gate
that reproduces what `go mod tidy`/`go mod verify` would change, so drift fails the
PR instead of the deploy.

## What you'll build

```text
gosumaudit/                independent module: example.com/gosumaudit
  go.mod                   go 1.26 (requires golang.org/x/mod/modfile)
  audit.go                 Finding/FindingKind; Audit(gomod, gosum) []Finding
  cmd/
    demo/
      main.go              audits a drifted go.mod/go.sum pair
  audit_test.go            tidy/missing-gomod/missing-entry/stale/order tests
  example_test.go          ExampleAudit with // Output
```

- Files: `audit.go`, `cmd/demo/main.go`, `audit_test.go`, `example_test.go`.
- Implement: `Audit(gomod, gosum []byte) ([]Finding, error)` that parses the `go.mod` require graph and `go.sum` records, flagging missing entries, missing `/go.mod` hashes, and stale entries, in deterministic sorted order.
- Test: a tidy pair yields nothing; dropping a `/go.mod` line yields `missing-gomod-hash`; a require with no checksum yields `missing-entry`; an orphan checksum yields `stale-entry`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get golang.org/x/mod/modfile
```

### Three shapes of drift

`go.sum` should hold, for every module in the build graph, both a module (zip)
hash and a `/go.mod` hash; and it should hold nothing for a module that is no
longer required. Three deviations are what a tidy gate catches. A `missing-entry`
is a required module with no usable zip hash — someone added a `require` (or bumped
a version) without running `go mod tidy`, so the clean build cannot verify it. A
`missing-gomod-hash` is a module that has its zip hash but not its `/go.mod` hash;
this happens when a `go.sum` is hand-edited or partially merged, and it breaks the
graph load even though the module itself downloads. A `stale-entry` is a hash for a
`module@version` that nothing in the require graph references — the residue of a
downgrade or a removed dependency that `go mod tidy` would prune.

Parsing the require graph correctly is why this uses `golang.org/x/mod/modfile`
rather than a hand-rolled scanner: `modfile.Parse` understands single and
block `require` forms, `// indirect` markers, and validates the version syntax
(a v2+ module must carry its `/vN` suffix). The `go.sum` side is a simple
three-field line format, grouped per `module@version` into "has zip" and
"has /go.mod" flags. The output is sorted by module, then version, then kind, so
the gate's report is deterministic and diff-friendly across runs.

Create `audit.go`:

```go
// Package gosumaudit audits a go.sum against a go.mod require graph, reporting the
// drift that `go mod tidy`/`go mod verify` would fix so a PR fails on an untidy
// go.sum instead of at deploy.
package gosumaudit

import (
	"bufio"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
)

// FindingKind classifies one drift finding.
type FindingKind string

const (
	// MissingEntry: a required module has no usable go.sum zip hash.
	MissingEntry FindingKind = "missing-entry"
	// MissingGoMod: a required module has a zip hash but no /go.mod hash.
	MissingGoMod FindingKind = "missing-gomod-hash"
	// StaleEntry: go.sum pins a module@version that nothing requires.
	StaleEntry FindingKind = "stale-entry"
)

// Finding is one drift record.
type Finding struct {
	Kind    FindingKind
	Module  string
	Version string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s %s %s", f.Kind, f.Module, f.Version)
}

type sums struct {
	hasZip   bool
	hasGoMod bool
}

// Audit parses go.mod and go.sum and returns drift findings in deterministic
// order (by module, then version, then kind).
func Audit(gomod, gosum []byte) ([]Finding, error) {
	mf, err := modfile.Parse("go.mod", gomod, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}
	required := make(map[string]bool) // key: module@version
	for _, r := range mf.Require {
		required[r.Mod.Path+"@"+r.Mod.Version] = true
	}

	got := make(map[string]*sums) // key: module@version
	keyMV := make(map[string][2]string)
	sc := bufio.NewScanner(strings.NewReader(string(gosum)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("malformed go.sum line: %q", line)
		}
		module := f[0]
		version := strings.TrimSuffix(f[1], "/go.mod")
		isGoMod := strings.HasSuffix(f[1], "/go.mod")
		key := module + "@" + version
		s := got[key]
		if s == nil {
			s = &sums{}
			got[key] = s
			keyMV[key] = [2]string{module, version}
		}
		if isGoMod {
			s.hasGoMod = true
		} else {
			s.hasZip = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	var findings []Finding

	// Required modules missing hashes.
	for _, r := range mf.Require {
		key := r.Mod.Path + "@" + r.Mod.Version
		s := got[key]
		switch {
		case s == nil || !s.hasZip:
			findings = append(findings, Finding{MissingEntry, r.Mod.Path, r.Mod.Version})
		case !s.hasGoMod:
			findings = append(findings, Finding{MissingGoMod, r.Mod.Path, r.Mod.Version})
		}
	}

	// go.sum entries for modules nothing requires.
	for key, mv := range keyMV {
		if !required[key] {
			findings = append(findings, Finding{StaleEntry, mv[0], mv[1]})
		}
	}

	slices.SortFunc(findings, func(a, b Finding) int {
		if a.Module != b.Module {
			return strings.Compare(a.Module, b.Module)
		}
		if a.Version != b.Version {
			return strings.Compare(a.Version, b.Version)
		}
		return strings.Compare(string(a.Kind), string(b.Kind))
	})
	return findings, nil
}
```

### The runnable demo

The demo audits a pair with two planted defects: module `b` is missing its
`/go.mod` hash, and a stale `c` lingers in `go.sum` although nothing requires it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/gosumaudit"
)

const goMod = "module example.com/app\n\ngo 1.26\n\nrequire (\n\texample.com/a v1.0.0\n\texample.com/b v1.5.0\n)\n"

// go.sum drift: b is missing its /go.mod hash, and a stale c entry lingers.
const goSum = "example.com/a v1.0.0 h1:aaaa=\n" +
	"example.com/a v1.0.0/go.mod h1:aamod=\n" +
	"example.com/b v1.5.0 h1:bbbb=\n" +
	"example.com/c v0.9.0 h1:cccc=\n" +
	"example.com/c v0.9.0/go.mod h1:ccmod=\n"

func main() {
	findings, err := gosumaudit.Audit([]byte(goMod), []byte(goSum))
	if err != nil {
		panic(err)
	}
	fmt.Printf("%d finding(s):\n", len(findings))
	for _, f := range findings {
		fmt.Printf("  %s\n", f)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
2 finding(s):
  missing-gomod-hash example.com/b v1.5.0
  stale-entry example.com/c v0.9.0
```

### Tests

Each test isolates one drift shape against a tidy baseline, and a final test pins
the deterministic ordering — module `a` (missing `/go.mod`), module `m` (stale),
module `z` (missing entry) come out sorted by module regardless of discovery
order.

Create `audit_test.go`:

```go
package gosumaudit

import (
	"testing"
)

const tidyGoMod = "module example.com/app\n\ngo 1.26\n\nrequire (\n\texample.com/a v1.0.0\n\texample.com/b v1.5.0\n)\n"

const tidyGoSum = "example.com/a v1.0.0 h1:aaaa=\n" +
	"example.com/a v1.0.0/go.mod h1:aamod=\n" +
	"example.com/b v1.5.0 h1:bbbb=\n" +
	"example.com/b v1.5.0/go.mod h1:bbmod=\n"

func findingSet(fs []Finding) map[Finding]bool {
	m := make(map[Finding]bool, len(fs))
	for _, f := range fs {
		m[f] = true
	}
	return m
}

func TestAuditTidyPair(t *testing.T) {
	t.Parallel()
	got, err := Audit([]byte(tidyGoMod), []byte(tidyGoSum))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("tidy pair produced findings: %v", got)
	}
}

func TestAuditMissingGoModHash(t *testing.T) {
	t.Parallel()
	// Drop b's /go.mod line.
	gosum := "example.com/a v1.0.0 h1:aaaa=\n" +
		"example.com/a v1.0.0/go.mod h1:aamod=\n" +
		"example.com/b v1.5.0 h1:bbbb=\n"
	got, err := Audit([]byte(tidyGoMod), []byte(gosum))
	if err != nil {
		t.Fatal(err)
	}
	set := findingSet(got)
	want := Finding{MissingGoMod, "example.com/b", "v1.5.0"}
	if !set[want] {
		t.Errorf("missing %v; got %v", want, got)
	}
}

func TestAuditMissingEntry(t *testing.T) {
	t.Parallel()
	// Add a require with no checksum at all.
	gomod := "module example.com/app\n\ngo 1.26\n\nrequire (\n\texample.com/a v1.0.0\n\texample.com/b v1.5.0\n\texample.com/c v0.4.0\n)\n"
	got, err := Audit([]byte(gomod), []byte(tidyGoSum))
	if err != nil {
		t.Fatal(err)
	}
	set := findingSet(got)
	want := Finding{MissingEntry, "example.com/c", "v0.4.0"}
	if !set[want] {
		t.Errorf("missing %v; got %v", want, got)
	}
}

func TestAuditStaleEntry(t *testing.T) {
	t.Parallel()
	// An orphan checksum for a module nothing requires.
	gosum := tidyGoSum +
		"example.com/orphan v0.1.0 h1:oooo=\n" +
		"example.com/orphan v0.1.0/go.mod h1:oomod=\n"
	got, err := Audit([]byte(tidyGoMod), []byte(gosum))
	if err != nil {
		t.Fatal(err)
	}
	set := findingSet(got)
	want := Finding{StaleEntry, "example.com/orphan", "v0.1.0"}
	if !set[want] {
		t.Errorf("missing %v; got %v", want, got)
	}
}

func TestAuditDeterministicOrder(t *testing.T) {
	t.Parallel()
	// Two independent findings; order must be by module then version then kind.
	gomod := "module example.com/app\n\ngo 1.26\n\nrequire (\n\texample.com/a v1.0.0\n\texample.com/z v1.0.0\n)\n"
	gosum := "example.com/a v1.0.0 h1:aaaa=\n" +
		"example.com/m v0.1.0 h1:mmmm=\n" +
		"example.com/m v0.1.0/go.mod h1:mmmod=\n"
	got, err := Audit([]byte(gomod), []byte(gosum))
	if err != nil {
		t.Fatal(err)
	}
	// Expect: a missing /go.mod (a), stale m, missing-entry z, sorted by module.
	want := []Finding{
		{MissingGoMod, "example.com/a", "v1.0.0"},
		{StaleEntry, "example.com/m", "v0.1.0"},
		{MissingEntry, "example.com/z", "v1.0.0"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding %d = %v; want %v", i, got[i], want[i])
		}
	}
}
```

Create `example_test.go`:

```go
package gosumaudit

import "fmt"

func ExampleAudit() {
	gomod := "module example.com/app\n\ngo 1.26\n\nrequire example.com/a v1.0.0\n"
	gosum := "example.com/a v1.0.0 h1:aaaa=\n"
	findings, _ := Audit([]byte(gomod), []byte(gosum))
	for _, f := range findings {
		fmt.Println(f)
	}
	// Output: missing-gomod-hash example.com/a v1.0.0
}
```

## Review

The auditor is correct when a tidy pair produces zero findings and each planted
defect produces exactly its finding, with a stable sorted order. The design choice
that matters is parsing `go.mod` with `modfile` rather than by hand: it handles the
block and single `require` forms, the `// indirect` marker, and the v2+ path-suffix
rule that a naive scanner would miss, so the require set is the same one the `go`
command sees. Keep the output deterministic — sort before returning — because a CI
gate whose report reorders between runs produces noisy, untrustworthy diffs. The
trap to avoid is treating a missing `/go.mod` hash as harmless: it is exactly the
drift that loads fine from a warm cache and fails a clean checkout. Run
`go test -race`.

## Resources

- [`golang.org/x/mod/modfile`](https://pkg.go.dev/golang.org/x/mod/modfile) — `Parse` and the `Require` graph the audit reads.
- [Go Modules Reference: go.sum files](https://go.dev/ref/mod#go-sum-files) — the two-hashes-per-module structure this checks for completeness.
- [`go mod tidy`](https://go.dev/ref/mod#go-mod-tidy) — the command whose effect the gate reproduces before it reaches CI.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-module-ziphash-verifier.md](06-module-ziphash-verifier.md) | Next: [08-proxy-failover-healthcheck.md](08-proxy-failover-healthcheck.md)
