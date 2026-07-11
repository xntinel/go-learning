# Exercise 4: Release gate — audit replace, exclude and retract directives

The classic release incident: someone left a local `replace => ../fork` in `go.mod`
and the release built against their laptop. This exercise is the pre-release linter
that catches it — a checker that parses `go.mod` and rejects filesystem replaces and
malformed directives while allowing legitimate version-to-version replaces and
retract blocks.

This module is fully self-contained. It has its own `go mod init`, its own demo,
and its own tests, and imports nothing from the other exercises.

## What you'll build

```text
gomodaudit/                independent module: example.com/gomodaudit
  go.mod                   go 1.26; requires golang.org/x/mod
  gomodaudit.go            Audit(data) ([]Finding, error) over Replace/Exclude/Retract
  cmd/
    demo/
      main.go              audits a go.mod with a local replace and a version replace
  gomodaudit_test.go       flags exactly the local/malformed replaces; passes the rest
```

- Files: `gomodaudit.go`, `cmd/demo/main.go`, `gomodaudit_test.go`.
- Implement: `Audit` returning a `Finding` for each local filesystem replace (`=> ./fork`) and each malformed replace (empty target path), and passing version-to-version replaces and retract/exclude blocks.
- Test: parse fixtures with a local replace, a version-to-version replace, and a retract block; assert only the local replace is flagged, and that an empty-target replace is reported.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/gomodaudit/cmd/demo
cd ~/go-exercises/gomodaudit
go mod init example.com/gomodaudit
go get golang.org/x/mod
```

### Local replace versus module replace

`modfile` models every `replace` as an `Old` and a `New`, both `module.Version`
values (`Path` plus `Version`). The two legal forms are distinguished entirely by
`New.Version`:

- A *module* replace — `replace foo => bar v1.2.3` — has `New.Path = "bar"` and
  `New.Version = "v1.2.3"`. This is fine to ship: it redirects to another published
  module version, which the proxy and checksum database can verify.
- A *filesystem* replace — `replace foo => ./fork` — has `New.Path = "./fork"` and
  `New.Version = ""`. A replacement with no version is, by definition, a directory
  path. This is the release hazard: it builds against a path that exists on one
  machine and is invisible to the proxy. The auditor must reject it.

So the rule is precise and needs no path-shape heuristics: `New.Version == ""` means
a filesystem replace. A `New.Path == ""` is malformed (a replace with no target at
all). `replace` and `exclude` are ignored when your module is a dependency, so this
is purely a main-module release check. Retract and exclude blocks are legitimate and
must pass — `modfile.Parse` already rejects a *syntactically* malformed retract, so
reaching the auditor means they parsed.

Create `gomodaudit.go`:

```go
package gomodaudit

import (
	"fmt"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// Finding is one problem the release auditor found in go.mod.
type Finding struct {
	Directive string // "replace"
	Module    string // the module path being replaced
	Detail    string
}

// Audit parses go.mod and reports release hazards: local filesystem replace
// directives (which must never ship to production) and malformed replaces.
// It returns an empty slice for a clean go.mod.
func Audit(data []byte) ([]Finding, error) {
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}
	return AuditFile(f), nil
}

// AuditFile runs the directive audit on an already-parsed modfile.File. Splitting
// it out lets tests exercise the malformed-replace branch by constructing a File
// with an empty replacement path directly, which text cannot express.
func AuditFile(f *modfile.File) []Finding {
	var findings []Finding
	for _, r := range f.Replace {
		switch {
		case r.New.Path == "":
			findings = append(findings, Finding{
				Directive: "replace",
				Module:    r.Old.Path,
				Detail:    "malformed replace: empty target path",
			})
		case r.New.Version == "":
			findings = append(findings, Finding{
				Directive: "replace",
				Module:    r.Old.Path,
				Detail:    fmt.Sprintf("local filesystem replace => %s must not ship to production", r.New.Path),
			})
		default:
			// version-to-version replace: New is a published module.Version, allowed.
			var _ module.Version = r.New
		}
	}
	return findings
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/gomodaudit"
)

const goMod = `module example.com/orders

go 1.24

require (
	github.com/google/uuid v1.6.0
	github.com/example/lib v1.0.0
)

replace github.com/example/lib => ../lib-fork

replace github.com/google/uuid => github.com/gofrs/uuid v4.4.0+incompatible

retract v1.0.1 // published with a broken migration
`

func main() {
	findings, err := gomodaudit.Audit([]byte(goMod))
	if err != nil {
		panic(err)
	}
	fmt.Printf("%d finding(s)\n", len(findings))
	for _, f := range findings {
		fmt.Printf("  [%s %s] %s\n", f.Directive, f.Module, f.Detail)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
1 finding(s)
  [replace github.com/example/lib] local filesystem replace => ../lib-fork must not ship to production
```

### Tests

Create `gomodaudit_test.go`:

```go
package gomodaudit

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

func TestAuditLocalReplaceFlagged(t *testing.T) {
	t.Parallel()
	const gm = `module example.com/x

go 1.24

require github.com/example/lib v1.0.0

replace github.com/example/lib => ./fork
`
	findings, err := Audit([]byte(gm))
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Module != "github.com/example/lib" {
		t.Errorf("Module = %q, want github.com/example/lib", findings[0].Module)
	}
	if !strings.Contains(findings[0].Detail, "local filesystem replace") {
		t.Errorf("Detail = %q, want local-replace message", findings[0].Detail)
	}
}

func TestAuditVersionReplaceAndRetractPass(t *testing.T) {
	t.Parallel()
	const gm = `module example.com/x

go 1.24

require github.com/google/uuid v1.6.0

replace github.com/google/uuid => github.com/gofrs/uuid v4.4.0+incompatible

exclude github.com/broken/dep v1.2.3

retract [v1.0.0, v1.0.5] // bad releases
`
	findings, err := Audit([]byte(gm))
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("want no findings for version replace/exclude/retract, got %+v", findings)
	}
}

func TestAuditEmptyTargetReported(t *testing.T) {
	t.Parallel()
	// A replace whose target path is empty is malformed. Text cannot express an
	// empty target, so build the modfile.File directly and audit it.
	f := &modfile.File{
		Replace: []*modfile.Replace{
			{Old: module.Version{Path: "github.com/example/lib"}, New: module.Version{Path: ""}},
		},
	}
	findings := AuditFile(f)
	if len(findings) != 1 {
		t.Fatalf("want 1 finding for empty target, got %+v", findings)
	}
	if !strings.Contains(findings[0].Detail, "malformed") {
		t.Errorf("Detail = %q, want malformed-replace message", findings[0].Detail)
	}
}

func ExampleAudit() {
	const gm = "module example.com/x\n\ngo 1.24\n\nrequire github.com/example/lib v1.0.0\n\nreplace github.com/example/lib => ../fork\n"
	findings, _ := Audit([]byte(gm))
	fmt.Println(len(findings))
	// Output: 1
}
```

## Review

The auditor is correct when a `go.mod` carrying a filesystem replace fails with the
replaced module named, while version-to-version replaces, `exclude`, and `retract`
all pass. The single reliable signal is `New.Version == ""`: a replacement with no
version is a directory path, so no fragile "does it start with `./`" string matching
is needed. This gate belongs at release time specifically because `replace` is
main-module-only — it never affects your consumers, so it is invisible until a build
uses it, which is exactly why a human forgets it is there. Pair it with the tidy and
go.sum gates so a release cannot ship with a local fork wired in. Run `go test -race`.

## Resources

- [`replace` directive](https://go.dev/ref/mod#go-mod-file-replace) — module vs filesystem replacements and their main-module-only scope.
- [`retract` directive](https://go.dev/ref/mod#go-mod-file-retract) — marking a module's own bad releases.
- [`golang.org/x/mod/modfile`](https://pkg.go.dev/golang.org/x/mod/modfile) — `File.Replace`, `Replace.New`, `File.Retract`, and `module.Version`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-reproducible-add-and-vendor.md](03-reproducible-add-and-vendor.md) | Next: [05-build-list-mvs-resolver.md](05-build-list-mvs-resolver.md)
