# Exercise 10: Hermetic and Offline Builds with Vendoring

The strongest determinism is a build that touches no network at all:
`go mod vendor` copies every dependency into `vendor/`, and building with
`-mod=vendor` and `GOPROXY=off` proves the artifact came from the committed tree.
The manifest that makes this auditable is `vendor/modules.txt`. This exercise
builds a parser for it — the check a CI job runs to confirm the vendored tree
matches `go.mod` — and walks the hermetic-build workflow around it.

This module is self-contained and uses only the standard library.

## What you'll build

```text
vendorcheck/                    independent module: example.com/vendorcheck
  go.mod
  vendorcheck.go                ParseModulesTxt(r) ([]Module, error)
  vendorcheck_test.go           parses a sample manifest; Example
  cmd/demo/
    main.go                     parses an embedded modules.txt sample
```

Files: `vendorcheck.go`, `vendorcheck_test.go`, `cmd/demo/main.go`.
Implement: `ParseModulesTxt(r io.Reader) ([]Module, error)` reading module lines, `## explicit` flags, and package lines.
Test: a sample manifest parses into the expected modules with correct `Explicit` flags and package lists.
Verify: `go test -count=1 -race ./...`

### The hermetic workflow

Two roads lead to a build that does not depend on the network answering the same
way twice:

```bash
# Road 1: vendor the source into the repo, build strictly from it.
go mod vendor
GOPROXY=off go build -mod=vendor ./...     # -mod=vendor ignores the cache/network

# Road 2: prefetch the module cache as a cacheable layer (e.g. a Docker stage).
go mod download -json                       # one JSON object per module, with Dir/Zip
go build ./...                              # later builds reuse the warm cache
```

Vendoring commits the dependency source under `vendor/` and writes
`vendor/modules.txt`, a manifest recording exactly which `module@version` every
vendored package came from and which modules are `## explicit` (directly required
by your `go.mod`). Building with `-mod=vendor` then reads *only* `vendor/`, so
`GOPROXY=off` proving no network access is a formality — the build is hermetic by
construction and reproducible from `git` alone. The prefetched-cache road trades a
smaller repo for a warm-up step; `go mod download -json` emits a machine-readable
record (`Path`, `Version`, `Dir`, `Zip`) ideal for scripting a Docker layer. Both
buy the same thing: a build that fails loudly when a dependency is missing rather
than silently reaching out to a proxy.

The manifest is the audit surface. A CI job that parses `vendor/modules.txt` and
compares it against `go.mod` catches the classic bug — a `vendor/` directory that
drifted out of sync because someone changed a dependency but forgot to re-run
`go mod vendor`.

### The modules.txt format

`vendor/modules.txt` is line-oriented:

- `# module version` starts a module entry;
- `## explicit; go 1.x` (or just `## go 1.x`) annotates the current module —
  `explicit` marks it as directly required by `go.mod`;
- any other non-blank line is a package path belonging to the current module.

Create `vendorcheck.go`:

```go
// vendorcheck.go
package vendorcheck

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Module is one entry from vendor/modules.txt: a module at a version, whether it
// is explicitly required by go.mod, and the vendored packages it provides.
type Module struct {
	Path     string
	Version  string
	Explicit bool
	Packages []string
}

// ParseModulesTxt parses a vendor/modules.txt manifest. It reads "# path version"
// module lines, "## explicit; go 1.x" annotation lines, and bare package lines.
// It errors if an annotation or package appears before any module line.
func ParseModulesTxt(r io.Reader) ([]Module, error) {
	var mods []Module
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "## "):
			if len(mods) == 0 {
				return nil, fmt.Errorf("annotation before any module: %q", line)
			}
			if annotationHasExplicit(line[len("## "):]) {
				mods[len(mods)-1].Explicit = true
			}
		case strings.HasPrefix(line, "# "):
			fields := strings.Fields(line[len("# "):])
			if len(fields) < 2 {
				return nil, fmt.Errorf("malformed module line: %q", line)
			}
			mods = append(mods, Module{Path: fields[0], Version: fields[1]})
		case strings.TrimSpace(line) == "":
			// blank line: ignore
		default:
			if len(mods) == 0 {
				return nil, fmt.Errorf("package before any module: %q", line)
			}
			m := &mods[len(mods)-1]
			m.Packages = append(m.Packages, strings.TrimSpace(line))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return mods, nil
}

// annotationHasExplicit reports whether an annotation body (after "## ")
// contains the "explicit" flag, which may be followed by "; go 1.x".
func annotationHasExplicit(body string) bool {
	for _, part := range strings.Split(body, ";") {
		if strings.TrimSpace(part) == "explicit" {
			return true
		}
	}
	return false
}
```

### The demo

The demo parses an embedded manifest sample so its output is stable.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"os"
	"strings"

	"example.com/vendorcheck"
)

const sample = `# golang.org/x/text v0.14.0
## explicit; go 1.23
golang.org/x/text/cases
golang.org/x/text/language
# rsc.io/sampler v1.3.0
## go 1.17
rsc.io/sampler
`

func main() {
	mods, err := vendorcheck.ParseModulesTxt(strings.NewReader(sample))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	for _, m := range mods {
		fmt.Printf("%s@%s explicit=%v packages=%d\n",
			m.Path, m.Version, m.Explicit, len(m.Packages))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
golang.org/x/text@v0.14.0 explicit=true packages=2
rsc.io/sampler@v1.3.0 explicit=false packages=1
```

(`x/text` is `## explicit` — directly required; `sampler` is not, so it is a
vendored indirect dependency.)

### The test

The table parses the sample manifest and asserts each module's path, version,
`Explicit` flag, and package list, plus the error path when a package line
precedes any module.

Create `vendorcheck_test.go`:

```go
// vendorcheck_test.go
package vendorcheck

import (
	"fmt"
	"strings"
	"testing"
)

const sampleTxt = `# golang.org/x/text v0.14.0
## explicit; go 1.23
golang.org/x/text/cases
golang.org/x/text/language
# rsc.io/sampler v1.3.0
## go 1.17
rsc.io/sampler
`

func TestParseModulesTxt(t *testing.T) {
	t.Parallel()

	mods, err := ParseModulesTxt(strings.NewReader(sampleTxt))
	if err != nil {
		t.Fatalf("ParseModulesTxt: %v", err)
	}
	if len(mods) != 2 {
		t.Fatalf("got %d modules, want 2", len(mods))
	}

	xtext := mods[0]
	if xtext.Path != "golang.org/x/text" || xtext.Version != "v0.14.0" {
		t.Fatalf("module 0 = %s@%s, want golang.org/x/text@v0.14.0", xtext.Path, xtext.Version)
	}
	if !xtext.Explicit {
		t.Fatal("golang.org/x/text should be explicit")
	}
	if len(xtext.Packages) != 2 {
		t.Fatalf("x/text packages = %v, want 2", xtext.Packages)
	}

	sampler := mods[1]
	if sampler.Explicit {
		t.Fatal("rsc.io/sampler should not be explicit (indirect)")
	}
}

func TestParseModulesTxtPackageBeforeModule(t *testing.T) {
	t.Parallel()

	_, err := ParseModulesTxt(strings.NewReader("some/orphan/package\n"))
	if err == nil {
		t.Fatal("expected error for a package line before any module")
	}
}

func ExampleParseModulesTxt() {
	mods, _ := ParseModulesTxt(strings.NewReader("# example.com/x v1.2.0\n## explicit; go 1.24\nexample.com/x/pkg\n"))
	m := mods[0]
	fmt.Printf("%s@%s explicit=%v\n", m.Path, m.Version, m.Explicit)
	// Output: example.com/x@v1.2.0 explicit=true
}
```

## Review

The parser is correct when it splits the manifest into modules whose `Explicit`
flag reflects the `## explicit` annotation and whose `Packages` list holds the
bare package lines — and when it rejects a manifest that names a package before
any module. The operational trap is committing `vendor/` but building without
`-mod=vendor` (or a `GOFLAGS` default), so the build ignores the vendored tree and
reaches the network, letting `vendor/` drift silently out of sync with `go.mod`.
Confirm the parser with `go test -race ./...`; confirm hermeticity on a real
project with `GOPROXY=off go build -mod=vendor ./...` (it must succeed with the
network unplugged) and by re-running `go mod vendor` to check it produces no diff.

## Resources

- [Go Modules Reference: vendoring and `vendor/modules.txt`](https://go.dev/ref/mod#vendoring) — the manifest format and `-mod=vendor`.
- [`go mod download`](https://go.dev/ref/mod#go-mod-download) — prefetching the module cache and the `-json` output.
- [Go Modules Reference: build commands and `-mod`](https://go.dev/ref/mod#build-commands) — `-mod=vendor`, `-mod=readonly`, and `GOFLAGS`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-supply-chain-integrity-gosum-and-checksumdb.md](09-supply-chain-integrity-gosum-and-checksumdb.md) | Next: [../06-linting-with-golangci-lint/00-concepts.md](../06-linting-with-golangci-lint/00-concepts.md)
