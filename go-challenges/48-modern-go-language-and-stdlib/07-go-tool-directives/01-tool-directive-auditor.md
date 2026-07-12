# Exercise 1: Audit go.mod Tool Directives for Pinning Drift

A build tool that has a `tool` line but no backing `require` is a landmine: the
`go.mod` looks like it pins the tool, but the version is not actually fixed. This
exercise builds a CI-grade auditor that parses `go.mod` with `x/mod/modfile`,
reports which tool directives are properly pinned and which have drifted, and can
rewrite `go.mod` deterministically to add or drop a tool.

This module is fully self-contained: it has its own `go mod init`, its own demo,
and its own tests. Nothing here imports any other exercise.

## What you'll build

```text
toolaudit/                    independent module: example.com/toolaudit
  go.mod                      go 1.25; require golang.org/x/mod
  toolaudit.go                Audit, AddTool, DropTool; Report, ToolStatus; ErrParse
  cmd/
    demo/
      main.go                 audits an embedded go.mod and prints pinned vs drifted
  toolaudit_test.go           table-driven audit, golden Add/Drop, ErrParse, Example
```

- Files: `toolaudit.go`, `cmd/demo/main.go`, `toolaudit_test.go`.
- Implement: `Audit([]byte) (*Report, error)` that lists every tool directive and matches it to a backing `require`; `AddTool`/`DropTool` that rewrite `go.mod` via `modfile` and return gofmt-stable bytes.
- Test: fixtures for well-pinned, unpinned, and block-syntax `go.mod`; a golden test proving stable serialization; an `errors.Is(err, ErrParse)` assertion.
- Verify: `go test -count=1 -race ./...`

Set up the module. The auditor depends on `golang.org/x/mod`:

```bash
go mod edit -go=1.25 -require=golang.org/x/mod@v0.37.0
go mod tidy
```

The module's `go.mod` after setup:

Create `go.mod`:

```go
// go.mod
module example.com/toolaudit

go 1.25

require golang.org/x/mod v0.37.0
```

### Why parse, never regex

`go.mod` has a real grammar: single-line directives, parenthesized blocks, and
canonical formatting the go command enforces. A regex that adds a `tool` line
works until someone uses the block form, and a regex that drops one leaves a
dangling blank line inside `tool ( ... )`. `golang.org/x/mod/modfile` is the
exact package the go command uses to read and write `go.mod`, so it round-trips
the file identically to `go mod edit`. `modfile.Parse(name, data, fix)` returns a
`*modfile.File` whose `Tool` field is a `[]*modfile.Tool` (each with a `Path`)
and whose `Require` field is a `[]*modfile.Require` (each `Require.Mod` is a
`module.Version` with `Path` and `Version`). We pass `nil` for the `VersionFixer`
because we are not canonicalizing versions.

### What "pinned" means and how we detect drift

A tool directive names a *package* import path; the version that pins it lives on
the `require` for the *module* that contains that package. So a tool is pinned
when some `require`'s module path is a prefix of the tool's package path — either
equal to it, or a proper prefix at a path boundary (`modpath + "/"`). Because a
tool package can be nested arbitrarily deep inside its module, we take the
*longest* matching require path so a nested tool binds to its real module rather
than to some shorter coincidental prefix. A tool with no matching require is
unpinned: the `go.mod` claims a tool dependency whose version nothing fixes, and
that is exactly the drift the auditor must flag.

The `Report` records a `ToolStatus` per tool with the resolved module, version,
and a `Pinned` flag; `Unpinned()` returns just the drifted paths for a CI gate to
fail on. Parse failures are wrapped with a package sentinel, `ErrParse`, so a
caller can distinguish "your go.mod is malformed" from "your go.mod is fine but
has drift" with `errors.Is`.

### Rewriting deterministically

`AddTool` adds both halves of a tool dependency — the `require` (via
`File.AddRequire`) and the `tool` line (via `File.AddTool`) — so the result is
never the invalid "tool line with no pin" state. `File.AddTool` calls
`SortBlocks` internally, so ordering is canonical. `DropTool` removes just the
tool line. The one non-obvious step is `File.Cleanup()`: `DropTool` clears the
entry lazily (to avoid quadratic edits), so without `Cleanup` a block-form
directive is left with a blank line inside its parentheses. Calling `Cleanup`
before `File.Format()` collapses the block and yields byte-stable output, which
is what lets the auditor also auto-fix `go.mod` in place.

Create `toolaudit.go`:

```go
package toolaudit

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/mod/modfile"
)

var ErrParse = errors.New("toolaudit: parse go.mod")

type ToolStatus struct {
	Path    string
	Module  string
	Version string
	Pinned  bool
}

type Report struct {
	Tools []ToolStatus
}

func (r *Report) Unpinned() []string {
	var out []string
	for _, t := range r.Tools {
		if !t.Pinned {
			out = append(out, t.Path)
		}
	}
	return out
}

func Audit(data []byte) (*Report, error) {
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParse, err)
	}
	rep := &Report{}
	for _, t := range f.Tool {
		st := ToolStatus{Path: t.Path}
		best := -1
		for _, req := range f.Require {
			mp := req.Mod.Path
			if t.Path == mp || strings.HasPrefix(t.Path, mp+"/") {
				if len(mp) > best {
					best = len(mp)
					st.Module = mp
					st.Version = req.Mod.Version
					st.Pinned = true
				}
			}
		}
		rep.Tools = append(rep.Tools, st)
	}
	return rep, nil
}

func AddTool(data []byte, toolPath, modPath, version string) ([]byte, error) {
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParse, err)
	}
	if err := f.AddRequire(modPath, version); err != nil {
		return nil, err
	}
	if err := f.AddTool(toolPath); err != nil {
		return nil, err
	}
	f.Cleanup()
	return f.Format()
}

func DropTool(data []byte, toolPath string) ([]byte, error) {
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParse, err)
	}
	if err := f.DropTool(toolPath); err != nil {
		return nil, err
	}
	f.Cleanup()
	return f.Format()
}
```

### The runnable demo

The demo audits an embedded `go.mod` that mixes a properly pinned tool with a
drifted one, so a real run shows both branches.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/toolaudit"
)

const sampleMod = `module example.com/service

go 1.24

require golang.org/x/tools v0.30.0

tool (
	golang.org/x/tools/cmd/stringer
	example.com/internal/cmd/mockgen
)
`

func main() {
	rep, err := toolaudit.Audit([]byte(sampleMod))
	if err != nil {
		fmt.Println("audit error:", err)
		return
	}
	for _, t := range rep.Tools {
		if t.Pinned {
			fmt.Printf("OK   %s -> %s %s\n", t.Path, t.Module, t.Version)
		} else {
			fmt.Printf("DRIFT %s (no backing require)\n", t.Path)
		}
	}
	fmt.Printf("unpinned: %v\n", rep.Unpinned())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
OK   golang.org/x/tools/cmd/stringer -> golang.org/x/tools v0.30.0
DRIFT example.com/internal/cmd/mockgen (no backing require)
unpinned: [example.com/internal/cmd/mockgen]
```

### Tests

The table covers the three shapes that matter: a single pinned tool, a tool with
no backing require (drift), and the parenthesized block form with two pinned
tools. `TestAddToolGolden` proves the serializer is stable — adding a tool to a
bare `go.mod` yields an exact expected string. `TestDropToolCollapsesBlock` is
the `Cleanup` proof: dropping one of two tools in a block collapses it to the
single-line form with no stray blank line. `TestParseError` asserts a malformed
`go.mod` wraps `ErrParse` via `errors.Is`.

Create `toolaudit_test.go`:

```go
package toolaudit

import (
	"errors"
	"fmt"
	"testing"
)

const pinnedMod = `module example.com/app

go 1.24

require golang.org/x/tools v0.30.0

tool golang.org/x/tools/cmd/stringer
`

const unpinnedMod = `module example.com/app

go 1.24

tool example.com/vendor/cmd/foo
`

const blockMod = `module example.com/app

go 1.24

require (
	golang.org/x/tools v0.30.0
	mvdan.cc/gofumpt v0.7.0
)

tool (
	golang.org/x/tools/cmd/stringer
	mvdan.cc/gofumpt
)
`

func TestAudit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		src      string
		want     []ToolStatus
		unpinned []string
	}{
		{
			name: "pinned single",
			src:  pinnedMod,
			want: []ToolStatus{
				{Path: "golang.org/x/tools/cmd/stringer", Module: "golang.org/x/tools", Version: "v0.30.0", Pinned: true},
			},
			unpinned: nil,
		},
		{
			name: "unpinned tool",
			src:  unpinnedMod,
			want: []ToolStatus{
				{Path: "example.com/vendor/cmd/foo"},
			},
			unpinned: []string{"example.com/vendor/cmd/foo"},
		},
		{
			name: "block syntax both pinned",
			src:  blockMod,
			want: []ToolStatus{
				{Path: "golang.org/x/tools/cmd/stringer", Module: "golang.org/x/tools", Version: "v0.30.0", Pinned: true},
				{Path: "mvdan.cc/gofumpt", Module: "mvdan.cc/gofumpt", Version: "v0.7.0", Pinned: true},
			},
			unpinned: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rep, err := Audit([]byte(tc.src))
			if err != nil {
				t.Fatalf("Audit: %v", err)
			}
			if len(rep.Tools) != len(tc.want) {
				t.Fatalf("Tools = %+v; want %+v", rep.Tools, tc.want)
			}
			for i, got := range rep.Tools {
				if got != tc.want[i] {
					t.Errorf("Tools[%d] = %+v; want %+v", i, got, tc.want[i])
				}
			}
			gotUn := rep.Unpinned()
			if len(gotUn) != len(tc.unpinned) {
				t.Fatalf("Unpinned = %v; want %v", gotUn, tc.unpinned)
			}
			for i := range gotUn {
				if gotUn[i] != tc.unpinned[i] {
					t.Errorf("Unpinned[%d] = %q; want %q", i, gotUn[i], tc.unpinned[i])
				}
			}
		})
	}
}

func TestAddToolGolden(t *testing.T) {
	t.Parallel()
	const base = `module example.com/app

go 1.24
`
	const want = `module example.com/app

go 1.24

require golang.org/x/tools v0.30.0

tool golang.org/x/tools/cmd/stringer
`
	got, err := AddTool([]byte(base), "golang.org/x/tools/cmd/stringer", "golang.org/x/tools", "v0.30.0")
	if err != nil {
		t.Fatalf("AddTool: %v", err)
	}
	if string(got) != want {
		t.Errorf("AddTool output:\n%q\nwant:\n%q", string(got), want)
	}
}

func TestDropToolCollapsesBlock(t *testing.T) {
	t.Parallel()
	const want = `module example.com/app

go 1.24

require (
	golang.org/x/tools v0.30.0
	mvdan.cc/gofumpt v0.7.0
)

tool mvdan.cc/gofumpt
`
	got, err := DropTool([]byte(blockMod), "golang.org/x/tools/cmd/stringer")
	if err != nil {
		t.Fatalf("DropTool: %v", err)
	}
	if string(got) != want {
		t.Errorf("DropTool output:\n%q\nwant:\n%q", string(got), want)
	}
}

func TestParseError(t *testing.T) {
	t.Parallel()
	_, err := Audit([]byte("this is not a go.mod !!!"))
	if !errors.Is(err, ErrParse) {
		t.Fatalf("err = %v; want wrapping ErrParse", err)
	}
}

func ExampleAudit() {
	rep, _ := Audit([]byte(unpinnedMod))
	fmt.Println(rep.Unpinned())
	// Output: [example.com/vendor/cmd/foo]
}
```

## Review

The auditor is correct when "pinned" is a pure function of the tool path and the
require set: a tool is pinned exactly when the longest require whose module path
is a path-boundary prefix of the tool path exists, and unpinned otherwise. If
`TestAddToolGolden` or `TestDropToolCollapsesBlock` drifts by even a blank line,
the rewrite is not gofmt-stable and would churn diffs in CI — that is the whole
value of using `modfile` over regex.

The traps to avoid: do not add a `tool` line without its `require` (that is an
invalid `go.mod`); `AddTool` here always adds both. Do not skip `File.Cleanup()`
before `Format()` after a drop — the block form will keep a dangling blank line.
And do not match the require by naive `strings.Contains`; the prefix must land on
a path boundary, or `golang.org/x/too` would wrongly match
`golang.org/x/tools`. Run `go test -race` to confirm the parser and rewriter are
race-free under `go test`.

## Resources

- [Go Modules Reference — the tool directive](https://go.dev/ref/mod#go-mod-file-tool) — grammar of `tool` and its relationship to `require`.
- [`golang.org/x/mod/modfile`](https://pkg.go.dev/golang.org/x/mod/modfile) — `Parse`, `File.Tool`, `File.AddTool`, `File.DropTool`, `File.Cleanup`, `File.Format`.
- [`golang.org/x/mod/module`](https://pkg.go.dev/golang.org/x/mod/module#Version) — the `Version` type with `Path` and `Version`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-pinned-code-generator.md](02-pinned-code-generator.md)
