# Exercise 2: Parse vendor/modules.txt into a dependency inventory

`vendor/modules.txt` is the in-repo manifest of exactly what was vendored, and
every downstream supply-chain gate — SBOM generation, license scanning, CVE
denylists, consistency checks — starts by parsing it into structured records.
This exercise builds that parser: the front end of the whole toolchain.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
modtxt/                      independent module: example.com/modtxt
  go.mod                     go 1.26
  modtxt.go                  type Module; Parse(io.Reader) ([]Module, error); ErrMalformed
  cmd/
    demo/
      main.go                parses a sample modules.txt and prints the inventory
  modtxt_test.go             table-driven fixtures + malformed-input error case
```

- Files: `modtxt.go`, `cmd/demo/main.go`, `modtxt_test.go`.
- Implement: `Parse`, turning a `modules.txt` body into `[]Module` records carrying path, version, an `Explicit` flag, the per-module `GoVersion`, an optional `Replace`, and the list of vendored `Packages`.
- Test: fixtures covering explicit-only, mixed explicit/indirect, both `## go 1.21` and `## explicit; go 1.21` forms, a replace-annotated entry, and a malformed body that returns a wrapped `ErrMalformed`.
- Verify: `go test -count=1 -race ./...`

### The grammar of modules.txt

The file is line-oriented and has exactly three line shapes. A module header
opens with `# ` followed by the module path, a space, and the version — and,
for a replaced module, ` => ` and the replacement path and version:
`# example.com/old v1.2.0 => example.com/fork v1.2.1`. Annotation lines open with
`## ` and carry one or more semicolon-separated tokens: `explicit` marks a module
the main module imports directly, and `go X.Y` records the dependency's own `go`
directive. The two common forms are `## explicit; go 1.21` and a bare `## go
1.17` for a purely transitive module. Every other non-blank line is a package
import path belonging to the module whose header most recently appeared.

The parser is a small state machine: the "current module" is whatever the last
`# ` header opened, `## ` lines annotate it, and package lines append to it. The
one hard invariant is ordering — a package line or an annotation line before any
`# ` header is malformed, because there is no module to attach it to. That case
must return an error, wrapped over a sentinel so callers can branch on it with
`errors.Is`.

### Why `strings.CutPrefix` and not `TrimPrefix`

`strings.CutPrefix(s, "# ")` returns both the remainder and a boolean reporting
whether the prefix was actually present. That boolean is exactly the branch
selector the state machine needs: it distinguishes a header line from an
annotation line from a package line in one pass, without re-scanning. Using
`TrimPrefix` would silently return the original string on a non-match, forcing a
second `HasPrefix` check. `CutPrefix` folds the test and the strip together, which
is the idiomatic Go 1.20+ way to write this kind of dispatch.

Create `modtxt.go`:

```go
package modtxt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrMalformed is returned (wrapped) when a modules.txt body has a package or
// annotation line before any module header.
var ErrMalformed = errors.New("modules.txt: malformed")

// Module is one vendored module as recorded in vendor/modules.txt.
type Module struct {
	Path      string   // module path, e.g. golang.org/x/mod
	Version   string   // module version, e.g. v0.37.0
	Explicit  bool     // true if the main module imports it directly
	GoVersion string   // the module's own go directive, e.g. 1.21 (may be empty)
	Replace   string   // "path version" of a replacement, if any (may be empty)
	Packages  []string // vendored import paths from this module
}

// Parse reads a vendor/modules.txt body and returns the vendored modules in the
// order they appear. A package or annotation line before the first module
// header yields an error wrapping ErrMalformed.
func Parse(r io.Reader) ([]Module, error) {
	var (
		mods []Module
		cur  *Module // index into mods via &mods[len-1]
	)
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if strings.TrimSpace(text) == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(text, "# "); ok {
			mods = append(mods, parseHeader(rest))
			cur = &mods[len(mods)-1]
			continue
		}
		if rest, ok := strings.CutPrefix(text, "## "); ok {
			if cur == nil {
				return nil, fmt.Errorf("%w: annotation at line %d before any module", ErrMalformed, line)
			}
			applyAnnotation(cur, rest)
			continue
		}
		// A package import path.
		if cur == nil {
			return nil, fmt.Errorf("%w: package %q at line %d before any module", ErrMalformed, strings.TrimSpace(text), line)
		}
		cur.Packages = append(cur.Packages, strings.TrimSpace(text))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("modules.txt: read: %w", err)
	}
	return mods, nil
}

// parseHeader parses "path version" or "path version => rpath rversion".
func parseHeader(rest string) Module {
	m := Module{}
	if before, after, found := strings.Cut(rest, " => "); found {
		rest = before
		m.Replace = strings.TrimSpace(after)
	}
	fields := strings.Fields(rest)
	if len(fields) > 0 {
		m.Path = fields[0]
	}
	if len(fields) > 1 {
		m.Version = fields[1]
	}
	return m
}

// applyAnnotation applies one "## ..." line's tokens to the current module.
func applyAnnotation(m *Module, rest string) {
	for _, tok := range strings.Split(rest, ";") {
		tok = strings.TrimSpace(tok)
		switch {
		case tok == "explicit":
			m.Explicit = true
		case strings.HasPrefix(tok, "go "):
			m.GoVersion = strings.TrimSpace(strings.TrimPrefix(tok, "go "))
		}
	}
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

	"example.com/modtxt"
)

const sample = `# github.com/pkg/errors v0.9.1
## explicit
github.com/pkg/errors
# golang.org/x/mod v0.37.0
## explicit; go 1.23
golang.org/x/mod/modfile
golang.org/x/mod/semver
# golang.org/x/text v0.3.7
## go 1.17
golang.org/x/text/unicode/norm
`

func main() {
	mods, err := modtxt.Parse(strings.NewReader(sample))
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		os.Exit(1)
	}
	for _, m := range mods {
		kind := "indirect"
		if m.Explicit {
			kind = "explicit"
		}
		fmt.Printf("%s %s [%s go=%s pkgs=%d]\n", m.Path, m.Version, kind, m.GoVersion, len(m.Packages))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
github.com/pkg/errors v0.9.1 [explicit go= pkgs=1]
golang.org/x/mod v0.37.0 [explicit go=1.23 pkgs=2]
golang.org/x/text v0.3.7 [indirect go=1.17 pkgs=1]
```

### Tests

The table feeds `modules.txt` bodies as `strings.NewReader` and asserts the full
`[]Module` with `reflect.DeepEqual`, covering the explicit-only, mixed, both
`## go` forms, and replace-annotated shapes. A separate case feeds a package line
with no preceding header and asserts the error is `ErrMalformed` via `errors.Is`.

Create `modtxt_test.go`:

```go
package modtxt

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want []Module
	}{
		{
			name: "explicit only",
			in: "# github.com/pkg/errors v0.9.1\n" +
				"## explicit\n" +
				"github.com/pkg/errors\n",
			want: []Module{
				{Path: "github.com/pkg/errors", Version: "v0.9.1", Explicit: true, Packages: []string{"github.com/pkg/errors"}},
			},
		},
		{
			name: "mixed explicit and indirect with go versions",
			in: "# golang.org/x/mod v0.37.0\n" +
				"## explicit; go 1.23\n" +
				"golang.org/x/mod/modfile\n" +
				"golang.org/x/mod/semver\n" +
				"# golang.org/x/text v0.3.7\n" +
				"## go 1.17\n" +
				"golang.org/x/text/unicode/norm\n",
			want: []Module{
				{Path: "golang.org/x/mod", Version: "v0.37.0", Explicit: true, GoVersion: "1.23",
					Packages: []string{"golang.org/x/mod/modfile", "golang.org/x/mod/semver"}},
				{Path: "golang.org/x/text", Version: "v0.3.7", GoVersion: "1.17",
					Packages: []string{"golang.org/x/text/unicode/norm"}},
			},
		},
		{
			name: "replace annotated entry",
			in: "# example.com/old v1.2.0 => example.com/fork v1.2.1\n" +
				"## explicit\n" +
				"example.com/old\n",
			want: []Module{
				{Path: "example.com/old", Version: "v1.2.0", Explicit: true,
					Replace: "example.com/fork v1.2.1", Packages: []string{"example.com/old"}},
			},
		},
		{
			name: "blank lines ignored",
			in:   "\n# a.com/x v1.0.0\n\n## explicit\n\na.com/x\n\n",
			want: []Module{
				{Path: "a.com/x", Version: "v1.0.0", Explicit: true, Packages: []string{"a.com/x"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(strings.NewReader(tc.in))
			if err != nil {
				t.Fatalf("Parse: unexpected error %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Parse mismatch:\n got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestParseMalformed(t *testing.T) {
	t.Parallel()
	// A package path before any module header is malformed.
	_, err := Parse(strings.NewReader("github.com/pkg/errors\n# a.com/x v1.0.0\n"))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("Parse error = %v; want wrapped ErrMalformed", err)
	}
}

func Example() {
	mods, _ := Parse(strings.NewReader("# a.com/x v1.0.0\n## explicit; go 1.21\na.com/x\n"))
	m := mods[0]
	fmt.Printf("%s %s explicit=%v go=%s\n", m.Path, m.Version, m.Explicit, m.GoVersion)
	// Output: a.com/x v1.0.0 explicit=true go=1.21
}
```

## Review

The parser is correct when it is a faithful state machine over the three line
shapes: a `# ` header opens a module (splitting a `=>` replacement off first so
the version field is clean), `## ` annotations mutate the current module's flags,
and every other non-blank line is a package on that module. The malformed case is
the load-bearing invariant — content before the first header has nowhere to
attach, so it must error, and wrapping `ErrMalformed` with `%w` is what lets an
SBOM pipeline distinguish "this file is corrupt" from an I/O error. The subtle
trap is aliasing: `cur` must point into the live `mods` slice, so take
`&mods[len(mods)-1]` *after* the append, never a pointer captured before a
subsequent append reallocates the backing array.

## Resources

- [`modules.txt` format](https://go.dev/ref/mod#vendoring) — the header, `## explicit`, and `## go` annotations.
- [`strings.CutPrefix`](https://pkg.go.dev/strings#CutPrefix) — the prefix-strip-and-test primitive the dispatch uses.
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — line-oriented scanning with `Scan`, `Text`, and `Err`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-vendor-consistency-guard.md](03-vendor-consistency-guard.md)
