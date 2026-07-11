# Exercise 3: Reimplement the go command's vendor consistency check

Before a vendored build, the go command cross-checks `vendor/modules.txt` against
`go.mod` and refuses to build on drift — the "inconsistent vendoring" error. This
exercise reimplements that check as a reusable function: parse the explicit
`require` set from `go.mod`, parse the `## explicit` set from `modules.txt`, and
report exactly how they disagree.

This module is fully self-contained: its own `go mod init`, a bundled minimal
`modules.txt` parser, its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
vendorcheck/                 independent module: example.com/vendorcheck
  go.mod                     go 1.26 (requires golang.org/x/mod)
  vendorcheck.go             type Drift; Check(goMod []byte, modulesTxt io.Reader) ([]Drift, error)
  cmd/
    demo/
      main.go                runs Check on a drifted pair and prints findings
  vendorcheck_test.go        in-sync, added, removed, and version-bump fixtures
```

- Files: `vendorcheck.go`, `cmd/demo/main.go`, `vendorcheck_test.go`.
- Implement: `Check`, comparing the direct (`non-indirect`) requires in `go.mod` — parsed with `golang.org/x/mod/modfile` — against the `## explicit` modules in `modules.txt`, returning a sorted `[]Drift` of missing, extra, and version-mismatch findings.
- Test: fixtures pairing a `go.mod` with a `modules.txt` for in-sync (nil), added-require (missing in vendor), removed-require (extra in vendor), and a version bump (mismatch).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/vendorcheck/cmd/demo
cd ~/go-exercises/vendorcheck
go mod init example.com/vendorcheck
go get golang.org/x/mod
```

### What "consistent" means, precisely

Vendoring records, per module, whether the main module imports it directly
(`## explicit`) or only transitively. The go command's consistency rule is that
the set of directly-required modules declared in `go.mod` must equal the set of
`## explicit` modules in `modules.txt`, matched by both path and version. Three
ways they can disagree map to three findings. A `require` added to `go.mod` but
not yet vendored is *missing in vendor*. A module still marked `## explicit` in
`modules.txt` but no longer required in `go.mod` is *extra in vendor*. A module
present in both but at different versions is a *version mismatch*. Any of these is
what the go command reports as "inconsistent vendoring"; producing them as
structured data is what lets a CI gate render a precise, actionable diff.

Note the deliberate scope: the check compares the *explicit/direct* sets, not the
full transitive graph. Transitive modules appear in `modules.txt` without an
`## explicit` marker and are not part of `go.mod`'s `require` block's direct
entries, so comparing them would produce false drift. Matching the go command
means comparing like with like: direct requires against `## explicit` modules.

### Why `modfile.Parse` and not a hand-rolled go.mod reader

`go.mod` has real grammar — block `require (...)` forms, `// indirect` comments,
`replace` and `exclude` directives, line continuations. `golang.org/x/mod/modfile`
is the same parser the go toolchain uses; `modfile.Parse(name, data, fix)` returns
a `*modfile.File` whose `Require` field is `[]*modfile.Require`, each carrying a
`module.Version` (`.Path`, `.Version`) and an `Indirect bool` set from the `//
indirect` comment. Reading `Indirect` is exactly how you separate direct requires
from indirect ones without re-implementing comment parsing. Passing `nil` as the
`VersionFixer` is correct here: we are reading, not rewriting, so no version
canonicalization callback is needed.

Create `vendorcheck.go`:

```go
package vendorcheck

import (
	"bufio"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
)

// DriftKind classifies how go.mod and modules.txt disagree.
type DriftKind int

const (
	Missing  DriftKind = iota // required in go.mod, absent from vendor's explicit set
	Extra                     // explicit in vendor, no longer required in go.mod
	Mismatch                  // present in both, different versions
)

func (k DriftKind) String() string {
	switch k {
	case Missing:
		return "missing-in-vendor"
	case Extra:
		return "extra-in-vendor"
	case Mismatch:
		return "version-mismatch"
	default:
		return "unknown"
	}
}

// Drift is one disagreement between go.mod and vendor/modules.txt.
type Drift struct {
	Module        string
	Kind          DriftKind
	GoModVersion  string // version declared in go.mod ("" if absent)
	VendorVersion string // version recorded in modules.txt ("" if absent)
}

// Check compares the direct requires in go.mod against the ## explicit modules
// in modules.txt and returns the drift, sorted by module path. A nil slice
// means the vendored tree is consistent with go.mod.
func Check(goMod []byte, modulesTxt io.Reader) ([]Drift, error) {
	mf, err := modfile.Parse("go.mod", goMod, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}
	direct := map[string]string{}
	for _, r := range mf.Require {
		if !r.Indirect {
			direct[r.Mod.Path] = r.Mod.Version
		}
	}

	explicit, err := explicitModules(modulesTxt)
	if err != nil {
		return nil, err
	}

	var drifts []Drift
	for _, path := range slices.Sorted(maps.Keys(direct)) {
		gv := direct[path]
		vv, ok := explicit[path]
		switch {
		case !ok:
			drifts = append(drifts, Drift{Module: path, Kind: Missing, GoModVersion: gv})
		case vv != gv:
			drifts = append(drifts, Drift{Module: path, Kind: Mismatch, GoModVersion: gv, VendorVersion: vv})
		}
	}
	for _, path := range slices.Sorted(maps.Keys(explicit)) {
		if _, ok := direct[path]; !ok {
			drifts = append(drifts, Drift{Module: path, Kind: Extra, VendorVersion: explicit[path]})
		}
	}
	slices.SortFunc(drifts, func(a, b Drift) int {
		if a.Module != b.Module {
			return strings.Compare(a.Module, b.Module)
		}
		return int(a.Kind) - int(b.Kind)
	})
	return drifts, nil
}

// explicitModules returns path->version for every ## explicit module in a
// modules.txt body. It is a minimal parser scoped to the explicit set.
func explicitModules(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	var curPath, curVer string
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, "# "); ok {
			// strip any " => replacement" before splitting path/version
			if before, _, found := strings.Cut(rest, " => "); found {
				rest = before
			}
			fields := strings.Fields(rest)
			curPath, curVer = "", ""
			if len(fields) > 0 {
				curPath = fields[0]
			}
			if len(fields) > 1 {
				curVer = fields[1]
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "## "); ok {
			for _, tok := range strings.Split(rest, ";") {
				if strings.TrimSpace(tok) == "explicit" && curPath != "" {
					out[curPath] = curVer
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read modules.txt: %w", err)
	}
	return out, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"strings"

	"example.com/vendorcheck"
)

const goMod = `module example.com/service

go 1.26

require (
	github.com/pkg/errors v0.9.1
	golang.org/x/mod v0.37.0
)
`

// modules.txt is stale: x/mod is vendored at an older version and pkg/errors
// is still explicit even though a fresh go.mod might have bumped x/mod.
const modulesTxt = `# github.com/pkg/errors v0.9.1
## explicit
github.com/pkg/errors
# golang.org/x/mod v0.36.0
## explicit; go 1.23
golang.org/x/mod/modfile
`

func main() {
	drifts, err := vendorcheck.Check([]byte(goMod), strings.NewReader(modulesTxt))
	if err != nil {
		fmt.Fprintln(os.Stderr, "check:", err)
		os.Exit(1)
	}
	if len(drifts) == 0 {
		fmt.Println("vendor consistent")
		return
	}
	for _, d := range drifts {
		fmt.Printf("%s: %s (go.mod=%s vendor=%s)\n", d.Module, d.Kind, d.GoModVersion, d.VendorVersion)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
golang.org/x/mod: version-mismatch (go.mod=v0.37.0 vendor=v0.36.0)
```

### Tests

Each fixture pairs a `go.mod` with a `modules.txt`. The in-sync pair yields a nil
slice; added-require reports `Missing`; removed-require reports `Extra`; a bumped
version reports `Mismatch`. The comparison uses `reflect.DeepEqual` against the
expected `[]Drift`.

Create `vendorcheck_test.go`:

```go
package vendorcheck

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const baseGoMod = `module example.com/service

go 1.26

require (
	github.com/pkg/errors v0.9.1
	golang.org/x/mod v0.37.0
)
`

func TestCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		goMod      string
		modulesTxt string
		want       []Drift
	}{
		{
			name:  "in sync",
			goMod: baseGoMod,
			modulesTxt: "# github.com/pkg/errors v0.9.1\n## explicit\ngithub.com/pkg/errors\n" +
				"# golang.org/x/mod v0.37.0\n## explicit; go 1.23\ngolang.org/x/mod/modfile\n",
			want: nil,
		},
		{
			name:       "added require missing in vendor",
			goMod:      baseGoMod,
			modulesTxt: "# github.com/pkg/errors v0.9.1\n## explicit\ngithub.com/pkg/errors\n",
			want: []Drift{
				{Module: "golang.org/x/mod", Kind: Missing, GoModVersion: "v0.37.0"},
			},
		},
		{
			name: "removed require extra in vendor",
			goMod: `module example.com/service

go 1.26

require github.com/pkg/errors v0.9.1
`,
			modulesTxt: "# github.com/pkg/errors v0.9.1\n## explicit\ngithub.com/pkg/errors\n" +
				"# golang.org/x/mod v0.37.0\n## explicit; go 1.23\ngolang.org/x/mod/modfile\n",
			want: []Drift{
				{Module: "golang.org/x/mod", Kind: Extra, VendorVersion: "v0.37.0"},
			},
		},
		{
			name:  "version bump mismatch",
			goMod: baseGoMod,
			modulesTxt: "# github.com/pkg/errors v0.9.1\n## explicit\ngithub.com/pkg/errors\n" +
				"# golang.org/x/mod v0.36.0\n## explicit; go 1.23\ngolang.org/x/mod/modfile\n",
			want: []Drift{
				{Module: "golang.org/x/mod", Kind: Mismatch, GoModVersion: "v0.37.0", VendorVersion: "v0.36.0"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Check([]byte(tc.goMod), strings.NewReader(tc.modulesTxt))
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Check drift:\n got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestIndirectRequiresIgnored(t *testing.T) {
	t.Parallel()
	// An // indirect require is not part of the explicit set, so a vendor tree
	// without it as ## explicit is still consistent.
	goMod := `module example.com/service

go 1.26

require github.com/pkg/errors v0.9.1

require golang.org/x/sys v0.1.0 // indirect
`
	modulesTxt := "# github.com/pkg/errors v0.9.1\n## explicit\ngithub.com/pkg/errors\n" +
		"# golang.org/x/sys v0.1.0\ngolang.org/x/sys/unix\n"
	got, err := Check([]byte(goMod), strings.NewReader(modulesTxt))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no drift for indirect-only difference, got %#v", got)
	}
}

func Example() {
	goMod := "module m\n\ngo 1.26\n\nrequire golang.org/x/mod v0.37.0\n"
	modulesTxt := "# golang.org/x/mod v0.36.0\n## explicit\ngolang.org/x/mod/modfile\n"
	drifts, _ := Check([]byte(goMod), strings.NewReader(modulesTxt))
	fmt.Printf("%s %s\n", drifts[0].Module, drifts[0].Kind)
	// Output: golang.org/x/mod version-mismatch
}
```

## Review

The check is correct when it compares like with like: the direct (`!Indirect`)
requires from `go.mod` against the `## explicit` modules in `modules.txt`, matched
on path and version. Using `modfile` rather than a regex is what makes the
`Indirect` distinction reliable across block and single-line `require` forms. The
`TestIndirectRequiresIgnored` case pins the boundary that trips naive
implementations: a transitive module differing between the two files is *not*
drift, because it was never part of the explicit set. Deterministic ordering
matters for a CI diff, so the result is sorted by path then kind; an unsorted
result would produce flaky gate output even when the findings are identical.

## Resources

- [`golang.org/x/mod/modfile`](https://pkg.go.dev/golang.org/x/mod/modfile) — `Parse`, `File.Require`, and `Require.Indirect`.
- [Vendoring consistency](https://go.dev/ref/mod#vendoring) — what the go command verifies before a vendored build.
- [`maps`](https://pkg.go.dev/maps) and [`slices.Sorted`](https://pkg.go.dev/slices#Sorted) — deterministic iteration over map keys.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-stale-vendor-ci-gate.md](04-stale-vendor-ci-gate.md)
