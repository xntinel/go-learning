# Exercise 3: Migrate a Legacy tools.go into go.mod Tool Directives

Moving to Go 1.24 confronted every repo with the same one-time chore: turn the
`//go:build tools` blank-import file into first-class `tool` directives. This
exercise automates it — parse the legacy `tools.go`, extract the blank-imported
packages, and rewrite `go.mod` to carry a `tool` directive for each. It is the
kind of codemod a platform team runs once across a monorepo.

This module is fully self-contained: its own `go mod init`, demo, and tests,
importing no other exercise.

## What you'll build

```text
toolsmig/                     independent module: example.com/toolsmig
  go.mod                      go 1.25; require golang.org/x/mod
  toolsmig.go                 ExtractTools, Migrate; ErrParseTools, ErrParseMod
  cmd/
    demo/
      main.go                 migrates an embedded tools.go + go.mod and prints the result
  toolsmig_test.go            extract table, golden go.mod, idempotency, Example
```

- Files: `toolsmig.go`, `cmd/demo/main.go`, `toolsmig_test.go`.
- Implement: `ExtractTools([]byte) ([]string, error)` that returns the blank-imported paths, and `Migrate(toolsGo, goMod []byte) ([]byte, []string, error)` that rewrites `go.mod` with a `tool` directive per package.
- Test: fixtures for pure blank imports, mixed blank/named imports, and no imports; a golden `go.mod` result; an idempotency check; both parse sentinels.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/toolsmig/cmd/demo
cd ~/go-exercises/toolsmig
go mod init example.com/toolsmig
go mod edit -go=1.25 -require=golang.org/x/mod@v0.37.0
go mod tidy
```

The module's `go.mod` after setup:

Create `go.mod`:

```go
// go.mod
module example.com/toolsmig

go 1.25

require golang.org/x/mod v0.37.0
```

### Why only the blank imports migrate

The `tools.go` convention was specifically *blank* imports: `_ "import/path"`
pulls the package's module into the graph without referencing any symbol, which
is exactly what you want for an executable you never call from Go code. A named
or regular import in that file would be real code, not a tool declaration, so the
migrator must extract only the blank ones. `go/parser.ParseFile` with the
`parser.ImportsOnly` mode parses just the package clause and import block —
faster and tolerant of a file that references packages not present — and the
resulting `*ast.File` exposes `Imports`, a `[]*ast.ImportSpec`. A blank import is
the spec whose `Name` is a non-nil `*ast.Ident` equal to `"_"`; a named import
has some other `Name`, and a plain import has a `nil` `Name`. The import path
lives in `ImportSpec.Path.Value` as a *quoted* string literal, so
`strconv.Unquote` turns `"golang.org/x/tools/cmd/stringer"` (with quotes) into
the bare path. The extracted paths are sorted for a stable result.

### Why Migrate adds only tool lines

When the repo used `tools.go`, the tools' modules were already in `go.mod` as
direct `require` entries — the blank imports forced them there. So migrating does
*not* need to add requires; the pins already exist. `Migrate` therefore calls
`File.AddTool` for each extracted package and nothing else, then `Cleanup` and
`Format`. `File.AddTool` is a no-op when the directive already exists, which makes
`Migrate` idempotent: running it a second time on its own output yields identical
bytes, so it is safe to run repeatedly in CI or across a monorepo without
churning diffs. The two sentinels, `ErrParseTools` and `ErrParseMod`, let a
caller tell which input was malformed.

Create `toolsmig.go`:

```go
package toolsmig

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"sort"
	"strconv"

	"golang.org/x/mod/modfile"
)

var (
	ErrParseTools = errors.New("toolsmig: parse tools.go")
	ErrParseMod   = errors.New("toolsmig: parse go.mod")
)

// ExtractTools parses a legacy tools.go and returns the import paths that were
// blank-imported (the "_" alias). Named and regular imports are ignored, since
// only blank imports are the tools.go convention. The result is sorted.
func ExtractTools(toolsGo []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "tools.go", toolsGo, parser.ImportsOnly)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParseTools, err)
	}
	var paths []string
	for _, imp := range f.Imports {
		if imp.Name == nil || imp.Name.Name != "_" {
			continue
		}
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("%w: bad import path %s: %v", ErrParseTools, imp.Path.Value, err)
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

// Migrate reads a legacy tools.go plus the module's go.mod and rewrites go.mod
// to carry a tool directive for each blank-imported package. The backing
// require entries are assumed already present (tools.go pulled them in as direct
// dependencies), so only tool lines are added. It returns the formatted go.mod
// and the sorted list of migrated packages. Migrate is idempotent: AddTool is a
// no-op when the directive already exists.
func Migrate(toolsGo, goMod []byte) ([]byte, []string, error) {
	paths, err := ExtractTools(toolsGo)
	if err != nil {
		return nil, nil, err
	}
	f, err := modfile.Parse("go.mod", goMod, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrParseMod, err)
	}
	for _, p := range paths {
		if err := f.AddTool(p); err != nil {
			return nil, nil, err
		}
	}
	f.Cleanup()
	out, err := f.Format()
	if err != nil {
		return nil, nil, err
	}
	return out, paths, nil
}
```

### The runnable demo

The demo migrates an embedded `tools.go` (two blank imports) against a `go.mod`
that already requires both modules, and prints the rewritten file.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/toolsmig"
)

const toolsGo = `//go:build tools

package tools

import (
	_ "golang.org/x/tools/cmd/stringer"
	_ "google.golang.org/protobuf/cmd/protoc-gen-go"
)
`

const goMod = `module example.com/service

go 1.24

require (
	golang.org/x/tools v0.30.0
	google.golang.org/protobuf v1.36.0
)
`

func main() {
	out, paths, err := toolsmig.Migrate([]byte(toolsGo), []byte(goMod))
	if err != nil {
		fmt.Println("migrate error:", err)
		return
	}
	fmt.Printf("migrated %d tools: %v\n\n", len(paths), paths)
	fmt.Print(string(out))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
migrated 2 tools: [golang.org/x/tools/cmd/stringer google.golang.org/protobuf/cmd/protoc-gen-go]

module example.com/service

go 1.24

require (
	golang.org/x/tools v0.30.0
	google.golang.org/protobuf v1.36.0
)

tool (
	golang.org/x/tools/cmd/stringer
	google.golang.org/protobuf/cmd/protoc-gen-go
)
```

### Tests

`TestExtractTools` covers the shapes: pure blank imports (sorted), a mix where
only the blank import migrates and the named/plain ones are ignored, an
import-less file, and a malformed file that wraps `ErrParseTools`.
`TestMigrateGolden` asserts the exact rewritten `go.mod`, including the collapsed
`tool ( ... )` block. `TestMigrateIdempotent` runs the migration twice and
asserts byte-identical output — the property that makes the codemod safe to rerun.

Create `toolsmig_test.go`:

```go
package toolsmig

import (
	"errors"
	"fmt"
	"testing"
)

const goMod = `module example.com/app

go 1.24

require (
	golang.org/x/tools v0.30.0
	mvdan.cc/gofumpt v0.7.0
)
`

func TestExtractTools(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		src     string
		want    []string
		wantErr error
	}{
		{
			name: "blank imports only",
			src: `//go:build tools

package tools

import (
	_ "mvdan.cc/gofumpt"
	_ "golang.org/x/tools/cmd/stringer"
)
`,
			want: []string{"golang.org/x/tools/cmd/stringer", "mvdan.cc/gofumpt"},
		},
		{
			name: "mixed blank and named",
			src: `package tools

import (
	_ "golang.org/x/tools/cmd/stringer"
	fmtpkg "fmt"
	"os"
)
`,
			want: []string{"golang.org/x/tools/cmd/stringer"},
		},
		{
			name: "no imports",
			src: `package tools
`,
			want: nil,
		},
		{
			name:    "not go",
			src:     "%%% not valid",
			wantErr: ErrParseTools,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ExtractTools([]byte(tc.src))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v; want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExtractTools: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v; want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got[%d] = %q; want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestMigrateGolden(t *testing.T) {
	t.Parallel()
	const toolsGo = `//go:build tools

package tools

import (
	_ "golang.org/x/tools/cmd/stringer"
	_ "mvdan.cc/gofumpt"
)
`
	const want = `module example.com/app

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
	got, paths, err := Migrate([]byte(toolsGo), []byte(goMod))
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if string(got) != want {
		t.Errorf("go.mod:\n%s\nwant:\n%s", got, want)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %v; want 2", paths)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	t.Parallel()
	const toolsGo = `package tools

import _ "golang.org/x/tools/cmd/stringer"
`
	once, _, err := Migrate([]byte(toolsGo), []byte(goMod))
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	twice, _, err := Migrate([]byte(toolsGo), once)
	if err != nil {
		t.Fatalf("Migrate (2nd): %v", err)
	}
	if string(once) != string(twice) {
		t.Errorf("not idempotent:\nfirst:\n%s\nsecond:\n%s", once, twice)
	}
}

func TestMigrateBadMod(t *testing.T) {
	t.Parallel()
	_, _, err := Migrate([]byte("package tools\n\nimport _ \"x\"\n"), []byte("!!! not a go.mod"))
	if !errors.Is(err, ErrParseMod) {
		t.Fatalf("err = %v; want ErrParseMod", err)
	}
}

func ExampleMigrate() {
	toolsGo := `package tools

import _ "golang.org/x/tools/cmd/stringer"
`
	goMod := `module example.com/app

go 1.24

require golang.org/x/tools v0.30.0
`
	out, paths, _ := Migrate([]byte(toolsGo), []byte(goMod))
	fmt.Printf("migrated: %v\n", paths)
	fmt.Print(string(out))
	// Output:
	// migrated: [golang.org/x/tools/cmd/stringer]
	// module example.com/app
	//
	// go 1.24
	//
	// require golang.org/x/tools v0.30.0
	//
	// tool golang.org/x/tools/cmd/stringer
}
```

## Review

The migrator is correct when it extracts exactly the blank imports — no more, no
less — and produces a `go.mod` byte-identical to what `go get -tool` would write
for the same packages. The golden test fixes that output; the idempotency test
guarantees a second run is a no-op, which is what lets a platform team run the
codemod across every module in a monorepo without fear of diff churn.

The traps: migrating named or plain imports (they are real code, not tool
declarations); forgetting `strconv.Unquote`, so the tool paths keep their
surrounding quotes; and skipping `File.Cleanup` before `Format`, which would
leave a blank line in the block form. Note that `Migrate` deliberately does not
add `require` lines — for a genuine `tools.go` the requires already exist; if you
were adding a brand-new tool you would use `go get -tool` (or `AddTool` plus
`AddRequire`) instead. Delete `tools.go` after migrating so the tool
dependencies are not counted twice. Run `go test -race` to confirm.

## Resources

- [Managing dependencies — Tools](https://go.dev/doc/modules/managing-dependencies#tools) — adding, running, and listing module tools.
- [Alex Edwards — Managing tool dependencies in Go 1.24+](https://www.alexedwards.net/blog/how-to-manage-tool-dependencies-in-go-1.24-plus) — the practical `tools.go` migration this exercise automates.
- [`go/parser`](https://pkg.go.dev/go/parser#ParseFile) — `ParseFile` with the `ImportsOnly` mode.
- [`go/ast`](https://pkg.go.dev/go/ast#ImportSpec) — `ImportSpec` with `Name` and `Path`.

---

Back to [02-pinned-code-generator.md](02-pinned-code-generator.md) | Next: [../08-go-mod-ignore-and-monorepo/00-concepts.md](../08-go-mod-ignore-and-monorepo/00-concepts.md)
