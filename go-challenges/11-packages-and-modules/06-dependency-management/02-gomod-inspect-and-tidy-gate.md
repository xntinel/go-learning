# Exercise 2: CI gate — go.mod inspector and tidy-drift detector

`go list -m all` reads the build list, and `go mod tidy` keeps `go.mod` honest.
This exercise turns both into a CI gate written in Go: it parses `go.mod` with
`golang.org/x/mod/modfile` (the same parser the `go` command uses), reports the
module path, `go` directive, and requirement list, and models the failure a
pipeline must reject — `go mod tidy` would have changed the file, so the tree is
"tidy-dirty".

This module is fully self-contained. It has its own `go mod init`, its own demo,
and its own tests, and imports nothing from the other exercises.

## What you'll build

```text
gomodgate/                 independent module: example.com/gomodgate
  go.mod                   go 1.26; requires golang.org/x/mod
  gomodgate.go             Inspect(data) (*Report, error); TidyDrift(data, imports) ([]Drift, error)
  cmd/
    demo/
      main.go              inspects a fixture go.mod and prints the report + drift
  gomodgate_test.go        parses fixtures; asserts fields and tidy-drift detection
```

- Files: `gomodgate.go`, `cmd/demo/main.go`, `gomodgate_test.go`.
- Implement: `Inspect` returning the module path, `go` version, and each require (path, version, direct/indirect); `TidyDrift` comparing requires against the set of actually-imported module paths.
- Test: feed fixture `go.mod` bytes to `modfile.Parse`; assert extracted fields and that an unused direct require is flagged as drift and a missing one too.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get golang.org/x/mod
```

### Why parse with modfile, not a regex

A `go.mod` file has real grammar — factored `require (...)` blocks, `// indirect`
comments, line continuations, `replace`/`exclude`/`retract` directives. A regex
that "reads the requires" breaks on the first factored block or trailing comment.
`modfile.Parse(name, data, fix)` returns a `*modfile.File` whose `Module.Mod.Path`,
`Go.Version`, and `Require` slice are exactly what the `go` command sees; each
`*modfile.Require` carries `Mod` (a `module.Version` of `Path` and `Version`) and an
`Indirect` bool that is true when the line has the `// indirect` marker. That is the
offline equivalent of `go list -m all`, minus the network. The `fix` argument is a
`modfile.VersionFixer`; pass `nil` to accept versions as written.

### Modeling tidy-drift

`go mod tidy` adds a require for every imported module that lacks one and drops
every direct require nothing imports. CI encodes this as `go mod tidy -diff` (or
`go mod tidy` followed by `git diff --exit-code go.mod`) and fails on any change.
To exercise the logic offline without invoking the toolchain, `TidyDrift` takes the
set of module paths your code actually imports and compares it to the declared
requires: a *direct* require that is not imported is drift of kind "unused" (tidy
would drop it), and an imported module with no require is drift of kind "missing"
(tidy would add it). Indirect requires are exempt from the unused check because they
exist precisely to record transitive versions your code does not import directly.

Create `gomodgate.go`:

```go
package gomodgate

import (
	"fmt"
	"sort"

	"golang.org/x/mod/modfile"
)

// Require is one requirement extracted from go.mod.
type Require struct {
	Path     string
	Version  string
	Indirect bool
}

// Report is the offline equivalent of `go list -m all` for the main module.
type Report struct {
	Module    string
	GoVersion string
	Requires  []Require
}

// Inspect parses go.mod and returns its module path, go directive, and requires.
func Inspect(data []byte) (*Report, error) {
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}
	r := &Report{}
	if f.Module != nil {
		r.Module = f.Module.Mod.Path
	}
	if f.Go != nil {
		r.GoVersion = f.Go.Version
	}
	for _, req := range f.Require {
		r.Requires = append(r.Requires, Require{
			Path:     req.Mod.Path,
			Version:  req.Mod.Version,
			Indirect: req.Indirect,
		})
	}
	return r, nil
}

// Drift is one inconsistency between go.mod and the real import graph.
type Drift struct {
	Module string
	Kind   string // "unused" (require nothing imports) or "missing" (import with no require)
}

// TidyDrift compares the requires in go.mod against the set of module paths the
// code actually imports and returns what `go mod tidy` would change. An empty
// slice means the module is tidy; a non-empty slice is a CI failure.
func TidyDrift(data []byte, imports map[string]bool) ([]Drift, error) {
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}

	required := make(map[string]bool, len(f.Require))
	var drifts []Drift
	for _, req := range f.Require {
		required[req.Mod.Path] = true
		if !req.Indirect && !imports[req.Mod.Path] {
			drifts = append(drifts, Drift{Module: req.Mod.Path, Kind: "unused"})
		}
	}
	for imp := range imports {
		if !required[imp] {
			drifts = append(drifts, Drift{Module: imp, Kind: "missing"})
		}
	}

	sort.Slice(drifts, func(i, j int) bool {
		if drifts[i].Module != drifts[j].Module {
			return drifts[i].Module < drifts[j].Module
		}
		return drifts[i].Kind < drifts[j].Kind
	})
	return drifts, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/gomodgate"
)

const goMod = `module example.com/orders

go 1.24

require (
	github.com/google/uuid v1.6.0
	golang.org/x/sync v0.8.0 // indirect
)
`

func main() {
	r, err := gomodgate.Inspect([]byte(goMod))
	if err != nil {
		panic(err)
	}
	fmt.Printf("module %s (go %s)\n", r.Module, r.GoVersion)
	for _, req := range r.Requires {
		kind := "direct"
		if req.Indirect {
			kind = "indirect"
		}
		fmt.Printf("  %s %s [%s]\n", req.Path, req.Version, kind)
	}

	imports := map[string]bool{"golang.org/x/text": true}
	drifts, _ := gomodgate.TidyDrift([]byte(goMod), imports)
	for _, d := range drifts {
		fmt.Printf("drift: %s %s\n", d.Kind, d.Module)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
module example.com/orders (go 1.24)
  github.com/google/uuid v1.6.0 [direct]
  golang.org/x/sync v0.8.0 [indirect]
drift: unused github.com/google/uuid
drift: missing golang.org/x/text
```

### Tests

Create `gomodgate_test.go`:

```go
package gomodgate

import (
	"fmt"
	"testing"
)

const fixture = `module example.com/orders

go 1.24

require (
	github.com/google/uuid v1.6.0
	golang.org/x/sync v0.8.0 // indirect
)
`

func TestInspect(t *testing.T) {
	t.Parallel()
	r, err := Inspect([]byte(fixture))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if r.Module != "example.com/orders" {
		t.Errorf("Module = %q, want example.com/orders", r.Module)
	}
	if r.GoVersion != "1.24" {
		t.Errorf("GoVersion = %q, want 1.24", r.GoVersion)
	}
	if len(r.Requires) != 2 {
		t.Fatalf("len(Requires) = %d, want 2", len(r.Requires))
	}
	if r.Requires[0].Path != "github.com/google/uuid" || r.Requires[0].Indirect {
		t.Errorf("Requires[0] = %+v, want direct uuid", r.Requires[0])
	}
	if r.Requires[1].Path != "golang.org/x/sync" || !r.Requires[1].Indirect {
		t.Errorf("Requires[1] = %+v, want indirect x/sync", r.Requires[1])
	}
}

func TestTidyDrift(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		imports map[string]bool
		want    []Drift
	}{
		{
			name:    "tidy: uuid imported, indirect ignored",
			imports: map[string]bool{"github.com/google/uuid": true},
			want:    nil,
		},
		{
			name:    "unused: uuid required but not imported",
			imports: map[string]bool{},
			want:    []Drift{{Module: "github.com/google/uuid", Kind: "unused"}},
		},
		{
			name:    "missing: import with no require",
			imports: map[string]bool{"github.com/google/uuid": true, "github.com/rs/zerolog": true},
			want:    []Drift{{Module: "github.com/rs/zerolog", Kind: "missing"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := TidyDrift([]byte(fixture), tc.imports)
			if err != nil {
				t.Fatalf("TidyDrift: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("drift[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func ExampleInspect() {
	r, _ := Inspect([]byte("module example.com/x\n\ngo 1.24\n"))
	fmt.Printf("%s go%s\n", r.Module, r.GoVersion)
	// Output: example.com/x go1.24
}
```

## Review

The inspector is correct when its `Report` matches what `go list -m all` and
`go mod edit -json` would report for the same file: the module path from
`Module.Mod.Path`, the version from `Go.Version`, and the direct/indirect flag from
each `Require.Indirect`. The drift detector is correct when it flags exactly what
`go mod tidy` would change — an unused *direct* require and a missing import — while
leaving indirect requires alone, since those legitimately name modules your code
does not import. The trap this gate exists to catch is a merged PR that added an
import without running tidy (missing require, breaks a clean checkout) or removed
the last use of a dependency without dropping it (unused require, bloats the graph).
Run `go test -race`.

## Resources

- [`golang.org/x/mod/modfile`](https://pkg.go.dev/golang.org/x/mod/modfile) — `Parse`, `File.Module`, `File.Go`, `File.Require`, and `Require.Indirect`.
- [`go mod tidy`](https://go.dev/ref/mod#go-mod-tidy) — what tidy adds and drops, and the pruned-graph rules.
- [`go list -m`](https://go.dev/ref/mod#go-list-m) — the build-list view this gate reproduces offline.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-dep-hygiene-cli.md](01-dep-hygiene-cli.md) | Next: [03-reproducible-add-and-vendor.md](03-reproducible-add-and-vendor.md)
