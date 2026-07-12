# Exercise 9: Audit that Go 1.24 tool directives are vendored for offline dev

Go 1.24 records developer tools (linters, mock and code generators) as `tool`
directives in `go.mod`, and `go mod vendor` copies them into `vendor/` so `go
tool` runs offline. The classic gap: a tool added *after* the last `go mod vendor`
is missing from `vendor/`, and `go tool` silently reaches for the network. This
exercise builds the auditor that catches that, closing the lesson on the same note
it opened — the machinery a platform team ships to keep vendoring honest.

This module is fully self-contained: its own `go mod init`, a bundled
`modules.txt` parser, its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
toolaudit/                   independent module: example.com/toolaudit
  go.mod                     go 1.26 (requires golang.org/x/mod)
  toolaudit.go               type Gap; Audit(goMod []byte, modulesTxt io.Reader) ([]Gap, error)
  cmd/
    demo/
      main.go                audits a go.mod with tool directives against modules.txt
  toolaudit_test.go          all-vendored, missing-module, not-explicit fixtures
```

- Files: `toolaudit.go`, `cmd/demo/main.go`, `toolaudit_test.go`.
- Implement: `Audit`, parsing the `tool` directives from `go.mod` with `golang.org/x/mod/modfile` and verifying each tool's providing module is present and marked `## explicit` in `modules.txt`.
- Test: fixtures pairing a `go.mod` carrying `tool` directives with a `modules.txt` — all tools vendored and explicit (no gaps), one tool's module missing (gap), and one present but not `## explicit` (gap).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get golang.org/x/mod
```

### Why a tool must be explicit in modules.txt

A `tool` directive names a *package* (`golang.org/x/tools/cmd/stringer`), which is
provided by a *module* (`golang.org/x/tools`). For `go tool` to run that package
from the vendored tree with no network, two things must hold in
`vendor/modules.txt`: the providing module must be present at all, and it must be
marked `## explicit` — because a tool directive is a direct dependency of the main
module, exactly like an import, and vendoring records direct dependencies as
explicit. If the module is present only transitively (no `## explicit`), or absent
entirely, `go tool` cannot resolve the tool offline. The auditor reports each
tool that fails either condition.

### Mapping a tool package to its providing module

`modfile.Parse` returns `File.Tool` as `[]*modfile.Tool`, each with a `Path` that
is the tool's package path. There is no direct module field, so the auditor infers
the providing module the way the go command does: the vendored module whose path
is the longest prefix of the tool package path. Given tool
`golang.org/x/tools/cmd/stringer` and vendored modules `golang.org/x/tools` and
`golang.org/x`, the former (longer prefix) is the provider. Longest-prefix
matching avoids a false positive where a shorter, unrelated module path also
happens to be a prefix.

Create `toolaudit.go`:

```go
package toolaudit

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"golang.org/x/mod/modfile"
)

// Gap is one tool directive that cannot be resolved from the vendored tree.
type Gap struct {
	Tool   string
	Reason string
}

// vendoredModule is a module as recorded in vendor/modules.txt.
type vendoredModule struct {
	Path     string
	Explicit bool
}

// Audit parses the tool directives from go.mod and reports each tool whose
// providing module is not vendored, or is vendored but not marked ## explicit.
func Audit(goMod []byte, modulesTxt io.Reader) ([]Gap, error) {
	mf, err := modfile.Parse("go.mod", goMod, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}
	vendored, err := parseVendored(modulesTxt)
	if err != nil {
		return nil, err
	}

	var gaps []Gap
	for _, tool := range mf.Tool {
		mod, ok := providerFor(tool.Path, vendored)
		switch {
		case !ok:
			gaps = append(gaps, Gap{Tool: tool.Path, Reason: "providing module not vendored"})
		case !mod.Explicit:
			gaps = append(gaps, Gap{Tool: tool.Path, Reason: "providing module " + mod.Path + " not marked ## explicit"})
		}
	}
	return gaps, nil
}

// providerFor returns the vendored module that is the longest path prefix of the
// tool package path.
func providerFor(toolPkg string, vendored []vendoredModule) (vendoredModule, bool) {
	var best vendoredModule
	found := false
	for _, m := range vendored {
		if m.Path == toolPkg || strings.HasPrefix(toolPkg, m.Path+"/") {
			if !found || len(m.Path) > len(best.Path) {
				best, found = m, true
			}
		}
	}
	return best, found
}

// parseVendored extracts modules and their ## explicit flag from modules.txt.
func parseVendored(r io.Reader) ([]vendoredModule, error) {
	var mods []vendoredModule
	sc := bufio.NewScanner(r)
	cur := -1
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, "# "); ok {
			if before, _, found := strings.Cut(rest, " => "); found {
				rest = before
			}
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				mods = append(mods, vendoredModule{Path: fields[0]})
				cur = len(mods) - 1
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "## "); ok && cur >= 0 {
			for _, tok := range strings.Split(rest, ";") {
				if strings.TrimSpace(tok) == "explicit" {
					mods[cur].Explicit = true
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read modules.txt: %w", err)
	}
	return mods, nil
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

	"example.com/toolaudit"
)

const goMod = `module example.com/service

go 1.26

tool (
	golang.org/x/tools/cmd/stringer
	github.com/golang/mock/mockgen
)
`

// modules.txt: x/tools is vendored and explicit; mock is vendored but NOT
// explicit (only pulled in transitively), so mockgen cannot run offline.
const modulesTxt = `# github.com/golang/mock v1.6.0
github.com/golang/mock/mockgen
# golang.org/x/tools v0.20.0
## explicit; go 1.21
golang.org/x/tools/cmd/stringer
`

func main() {
	gaps, err := toolaudit.Audit([]byte(goMod), strings.NewReader(modulesTxt))
	if err != nil {
		fmt.Fprintln(os.Stderr, "audit:", err)
		os.Exit(2)
	}
	if len(gaps) == 0 {
		fmt.Println("all tool directives are vendored for offline use")
		return
	}
	for _, g := range gaps {
		fmt.Printf("%s: %s\n", g.Tool, g.Reason)
	}
	os.Exit(1)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
github.com/golang/mock/mockgen: providing module github.com/golang/mock not marked ## explicit
```

### Tests

The fixtures pair a `go.mod` carrying two `tool` directives with a `modules.txt`:
all providers vendored and explicit (no gaps), one provider's module absent (a
`not vendored` gap), and one provider present but not `## explicit` (an `not
marked explicit` gap).

Create `toolaudit_test.go`:

```go
package toolaudit

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const twoToolsMod = `module example.com/service

go 1.26

tool (
	golang.org/x/tools/cmd/stringer
	github.com/golang/mock/mockgen
)
`

func TestAudit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		modulesTxt string
		want       []Gap
	}{
		{
			name: "all vendored and explicit",
			modulesTxt: "# github.com/golang/mock v1.6.0\n## explicit\ngithub.com/golang/mock/mockgen\n" +
				"# golang.org/x/tools v0.20.0\n## explicit; go 1.21\ngolang.org/x/tools/cmd/stringer\n",
			want: nil,
		},
		{
			name:       "provider module absent",
			modulesTxt: "# golang.org/x/tools v0.20.0\n## explicit; go 1.21\ngolang.org/x/tools/cmd/stringer\n",
			want: []Gap{
				{Tool: "github.com/golang/mock/mockgen", Reason: "providing module not vendored"},
			},
		},
		{
			name: "provider present but not explicit",
			modulesTxt: "# github.com/golang/mock v1.6.0\ngithub.com/golang/mock/mockgen\n" +
				"# golang.org/x/tools v0.20.0\n## explicit; go 1.21\ngolang.org/x/tools/cmd/stringer\n",
			want: []Gap{
				{Tool: "github.com/golang/mock/mockgen", Reason: "providing module github.com/golang/mock not marked ## explicit"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Audit([]byte(twoToolsMod), strings.NewReader(tc.modulesTxt))
			if err != nil {
				t.Fatalf("Audit: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Audit gaps:\n got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestLongestPrefixWins(t *testing.T) {
	t.Parallel()
	// A shorter unrelated module path is also a prefix, but the exact provider
	// (longer prefix) must be selected, and it is explicit, so no gap.
	goMod := "module m\n\ngo 1.26\n\ntool golang.org/x/tools/cmd/stringer\n"
	modulesTxt := "# golang.org/x v0.1.0\ngolang.org/x/whatever\n" +
		"# golang.org/x/tools v0.20.0\n## explicit\ngolang.org/x/tools/cmd/stringer\n"
	got, err := Audit([]byte(goMod), strings.NewReader(modulesTxt))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no gap (x/tools is explicit), got %#v", got)
	}
}

func Example() {
	goMod := "module m\n\ngo 1.26\n\ntool golang.org/x/tools/cmd/stringer\n"
	modulesTxt := "# golang.org/x/tools v0.20.0\ngolang.org/x/tools/cmd/stringer\n"
	gaps, _ := Audit([]byte(goMod), strings.NewReader(modulesTxt))
	fmt.Println(gaps[0].Reason)
	// Output: providing module golang.org/x/tools not marked ## explicit
}
```

## Review

The auditor is correct when it checks both conditions a tool needs to run offline:
its providing module must be vendored *and* marked `## explicit`, because a tool
directive is a direct dependency and vendoring records direct dependencies as
explicit. Inferring the provider by longest path prefix mirrors how the go command
attributes a package to a module, and `TestLongestPrefixWins` pins the case a
naive "any prefix" match would get wrong. The common real-world gap this catches
is temporal: `go mod vendor` was last run before a `tool` directive was added, so
the tool is simply absent from `modules.txt` — the auditor turns that silent
network fallback into a loud, actionable finding. Relying on `modfile.File.Tool`
rather than scraping raw lines is what makes the parse robust across the block and
single-line `tool` forms.

## Resources

- [`golang.org/x/mod/modfile`](https://pkg.go.dev/golang.org/x/mod/modfile) — `Parse`, `File.Tool`, and the `Tool` type.
- [Go 1.24 tool directives](https://go.dev/doc/go1.24#tools) — `go get -tool`, `go tool`, and the `tool` directive.
- [Managing tool dependencies in Go 1.24+](https://www.alexedwards.net/blog/how-to-manage-tool-dependencies-in-go-1.24-plus) — the tool directive and vendoring workflow end to end.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../09-designing-a-public-go-module/00-concepts.md](../09-designing-a-public-go-module/00-concepts.md)
