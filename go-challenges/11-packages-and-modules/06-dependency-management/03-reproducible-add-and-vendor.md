# Exercise 3: Reproducible builds — add-with-pin and vendor consistency

Adding a dependency reproducibly means pinning a version, and vendoring means
`vendor/modules.txt` must stay consistent with `go.mod`. This exercise builds the
CI gate for the vendoring half: a checker that parses `vendor/modules.txt` and
cross-checks it against the `go.mod` requires — the offline core of the
`go mod vendor && git diff --exit-code vendor/modules.txt` pattern.

This module is fully self-contained. It has its own `go mod init`, its own demo,
and its own tests, and imports nothing from the other exercises.

## What you'll build

```text
vendorcheck/               independent module: example.com/vendorcheck
  go.mod                   go 1.26; requires golang.org/x/mod
  vendorcheck.go           ParseModulesTxt(data); Check(gomod, modulesTxt) ([]string, error)
  cmd/
    demo/
      main.go              checks a consistent pair, then a drifted one
  vendorcheck_test.go      asserts pass on agreement, precise failure on drift
```

- Files: `vendorcheck.go`, `cmd/demo/main.go`, `vendorcheck_test.go`.
- Implement: a `modules.txt` parser (module headers, `## explicit` annotations) and a `Check` that verifies every direct require is vendored and every `## explicit` vendored module is required.
- Test: pass a consistent fixture pair; then drop a require from `modules.txt` and add an extra explicit module, asserting the named drift.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/vendorcheck/cmd/demo
cd ~/go-exercises/vendorcheck
go mod init example.com/vendorcheck
go get golang.org/x/mod
```

### The vendor/modules.txt format

`go mod vendor` writes a manifest that the build trusts. Each vendored module is a
header line followed by an annotation and its package list:

```text
# github.com/google/uuid v1.6.0
## explicit
github.com/google/uuid
# golang.org/x/text v0.14.0
## explicit; go 1.18
golang.org/x/text/language
```

A `# path version` line names a vendored module. The `## explicit` annotation on
the next line marks a module that `go.mod` requires *directly* (a transitive-only
dependency is vendored without `explicit`). The plain lines after are the vendored
package import paths, which this gate ignores. The consistency invariant is exact:
every module `go.mod` requires directly must be vendored and marked explicit, and
every explicitly-vendored module must be a direct require. A mismatch means someone
edited `go.mod` without re-running `go mod vendor`, so the build is compiling code
that no longer matches the declared dependencies.

Parse the manifest line by line with a `bufio.Scanner`, using `strings.HasPrefix`
to classify each line and `strings.Fields` to split a header into path and version.
Track the "current" module so an `## explicit` annotation attaches to the header
above it.

Create `vendorcheck.go`:

```go
package vendorcheck

import (
	"bufio"
	"bytes"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

// VendoredModule is one entry from vendor/modules.txt.
type VendoredModule struct {
	Path     string
	Version  string
	Explicit bool // "## explicit": a direct require, not a transitive-only dep
}

// ParseModulesTxt parses a vendor/modules.txt manifest into its module entries.
func ParseModulesTxt(data []byte) ([]VendoredModule, error) {
	var mods []VendoredModule
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "## "):
			if len(mods) == 0 {
				return nil, fmt.Errorf("annotation %q before any module header", line)
			}
			if strings.Contains(line, "explicit") {
				mods[len(mods)-1].Explicit = true
			}
		case strings.HasPrefix(line, "# "):
			fields := strings.Fields(line)
			if len(fields) < 3 {
				return nil, fmt.Errorf("malformed module header: %q", line)
			}
			mods = append(mods, VendoredModule{Path: fields[1], Version: fields[2]})
		default:
			// a package import path under the current module; ignored here
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return mods, nil
}

// Check verifies that vendor/modules.txt is consistent with go.mod: every direct
// require is vendored-and-explicit, and every explicit vendored module is a direct
// require. It returns a sorted list of problems; empty means consistent.
func Check(gomod, modulesTxt []byte) ([]string, error) {
	f, err := modfile.Parse("go.mod", gomod, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}
	mods, err := ParseModulesTxt(modulesTxt)
	if err != nil {
		return nil, fmt.Errorf("parse modules.txt: %w", err)
	}

	explicit := make(map[string]bool)
	for _, m := range mods {
		if m.Explicit {
			explicit[m.Path] = true
		}
	}
	direct := make(map[string]bool)
	var problems []string
	for _, req := range f.Require {
		if req.Indirect {
			continue
		}
		direct[req.Mod.Path] = true
		if !explicit[req.Mod.Path] {
			problems = append(problems, fmt.Sprintf("require %s missing from vendor/modules.txt", req.Mod.Path))
		}
	}
	for _, m := range mods {
		if m.Explicit && !direct[m.Path] {
			problems = append(problems, fmt.Sprintf("vendored module %s marked explicit but not required", m.Path))
		}
	}
	sort.Strings(problems)
	return problems, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/vendorcheck"
)

const goMod = `module example.com/orders

go 1.24

require github.com/google/uuid v1.6.0
`

const consistent = `# github.com/google/uuid v1.6.0
## explicit
github.com/google/uuid
`

const drifted = `# golang.org/x/text v0.14.0
## explicit; go 1.18
golang.org/x/text/language
`

func main() {
	ok, _ := vendorcheck.Check([]byte(goMod), []byte(consistent))
	fmt.Printf("consistent: %d problems\n", len(ok))

	bad, _ := vendorcheck.Check([]byte(goMod), []byte(drifted))
	fmt.Printf("drifted: %d problems\n", len(bad))
	for _, p := range bad {
		fmt.Println("  " + p)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
consistent: 0 problems
drifted: 2 problems
  require github.com/google/uuid missing from vendor/modules.txt
  vendored module golang.org/x/text marked explicit but not required
```

### Tests

Create `vendorcheck_test.go`:

```go
package vendorcheck

import (
	"fmt"
	"strings"
	"testing"
)

const goMod = `module example.com/orders

go 1.24

require (
	github.com/google/uuid v1.6.0
	golang.org/x/text v0.14.0 // indirect
)
`

const consistentTxt = `# github.com/google/uuid v1.6.0
## explicit
github.com/google/uuid
# golang.org/x/text v0.14.0
## go 1.18
golang.org/x/text/language
`

func TestCheckConsistent(t *testing.T) {
	t.Parallel()
	problems, err := Check([]byte(goMod), []byte(consistentTxt))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("want no problems, got %v", problems)
	}
}

func TestCheckMissingVendor(t *testing.T) {
	t.Parallel()
	// uuid required but not present in the manifest at all.
	txt := "# golang.org/x/text v0.14.0\n## go 1.18\ngolang.org/x/text/language\n"
	problems, err := Check([]byte(goMod), []byte(txt))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "github.com/google/uuid") {
		t.Fatalf("want uuid flagged missing, got %v", problems)
	}
}

func TestCheckExtraExplicit(t *testing.T) {
	t.Parallel()
	// An extra module marked explicit that go.mod does not require directly.
	txt := consistentTxt + "# github.com/rs/zerolog v1.33.0\n## explicit\ngithub.com/rs/zerolog\n"
	problems, err := Check([]byte(goMod), []byte(txt))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "github.com/rs/zerolog") {
		t.Fatalf("want zerolog flagged as extra explicit, got %v", problems)
	}
}

func TestParseModulesTxt(t *testing.T) {
	t.Parallel()
	mods, err := ParseModulesTxt([]byte(consistentTxt))
	if err != nil {
		t.Fatalf("ParseModulesTxt: %v", err)
	}
	if len(mods) != 2 {
		t.Fatalf("len = %d, want 2", len(mods))
	}
	if mods[0].Path != "github.com/google/uuid" || !mods[0].Explicit {
		t.Errorf("mods[0] = %+v, want explicit uuid", mods[0])
	}
	if mods[1].Explicit {
		t.Errorf("mods[1] = %+v, want x/text NOT explicit", mods[1])
	}
}

func ExampleCheck() {
	gm := "module example.com/x\n\ngo 1.24\n\nrequire github.com/google/uuid v1.6.0\n"
	txt := "# github.com/google/uuid v1.6.0\n## explicit\ngithub.com/google/uuid\n"
	problems, _ := Check([]byte(gm), []byte(txt))
	fmt.Println(len(problems))
	// Output: 0
}
```

## Review

The gate is correct when a freshly `go mod vendor`-ed tree passes and any manual
drift fails with the offending module named. The two directions matter equally: a
direct require missing from `modules.txt` means the manifest is stale (a dependency
was added without re-vendoring), and an explicit vendored module absent from
`go.mod` means a dependency was removed without re-vendoring. The `## explicit`
annotation is the load-bearing signal — dropping it would make the gate treat every
transitive dependency as a direct require and produce false failures. This exercise
stays narrowly on the consistency invariant; the mechanics of what `go mod vendor`
copies belong to the vendoring lesson. Run `go test -race`.

## Resources

- [Vendoring](https://go.dev/ref/mod#vendoring) — `go mod vendor`, the `vendor/modules.txt` format, and `## explicit`.
- [`golang.org/x/mod/modfile`](https://pkg.go.dev/golang.org/x/mod/modfile) — `Parse` and `File.Require`.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — line-oriented parsing of the manifest.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-gomod-inspect-and-tidy-gate.md](02-gomod-inspect-and-tidy-gate.md) | Next: [04-gomod-directive-auditor.md](04-gomod-directive-auditor.md)
