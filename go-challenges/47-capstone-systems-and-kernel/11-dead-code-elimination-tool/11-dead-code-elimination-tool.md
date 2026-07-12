# 11. Dead Code Elimination Tool

Dead code elimination in Go requires whole-program analysis: the compiler's
"declared and not used" check covers local variables only. Exported functions,
unexported package-level symbols, and interface-implementing methods need a
call-graph-based reachability analysis to determine whether they are ever
reached from a real entry point. This lesson builds a production-grade static
analysis tool using `go/packages`, `go/ssa`, and
`golang.org/x/tools/go/callgraph/rta`.

The hard parts are (a) building SSA correctly and calling `prog.Build()` before
touching any function bodies, (b) seeding RTA with all real entry points —
`main`, every `init`, and test functions when `-tests` is set — so that
nothing reachable from `init` is falsely reported as dead, and (c) trusting the
RTA call graph to handle interface dispatch rather than adding a second, incorrect
filter on top of it.

```text
deadcode/
  go.mod
  dce/
    dce.go
    dce_test.go
  cmd/dce/
    main.go
```

The `dce` package exposes an `Analyzer` that loads packages, builds SSA, runs
RTA, and returns a sorted slice of `Finding` values describing unreachable
functions. `cmd/dce` wraps it in a CLI with text and JSON output.

## Concepts

### Whole-Program Analysis vs. the Compiler's Local Check

The Go compiler rejects a local variable that is declared but never used. It
cannot make the equivalent judgment about exported functions: another binary
might import and call them. A dead code tool must load the entire program and
follow every call edge to determine what is reachable.

`golang.org/x/tools/go/packages` loads a package graph with full type
information. The `Mode` bitmask controls what is fetched; for SSA construction
at minimum:

```
packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
packages.NeedImports | packages.NeedDeps |
packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo
```

Missing `NeedDeps` is the most common mistake when first using this API: without
it, imported packages are loaded without type information and the SSA builder
produces empty function bodies for the majority of the program.

### SSA Form and prog.Build

SSA (static single assignment) is an intermediate representation in which every
variable is assigned exactly once. `golang.org/x/tools/go/ssa` converts the
typed AST into SSA functions with explicit phi-nodes at join points.
`ssautil.AllPackages` accepts the `[]*packages.Package` slice from
`packages.Load` and returns `(*ssa.Program, []*ssa.Package)`. The critical rule
is that `prog.Build()` must be called before reading any function bodies or
running any analysis: without it, every `fn.Blocks` is nil and the call graph
will be empty, causing every function to appear dead.

`ssa.InstantiateGenerics` passed as the `BuilderMode` causes generic functions
to be instantiated for each set of type arguments they are called with; without
it, generic functions appear as uninstantiated templates that RTA cannot
traverse.

### Rapid Type Analysis (RTA)

RTA (Rapid Type Analysis, Grove et al. 1997) is a flow-insensitive whole-program
call graph algorithm. It maintains a worklist of reachable functions and a set
of concrete types that are potentially instantiated. When it discovers a new
reachable function it inspects its call sites; for an interface call it adds
edges to every method of every concrete type that implements the interface AND
has been seen instantiated. This is what makes RTA avoid false positives on
interface methods: a method is only kept alive if both the interface call is
reachable AND a concrete type implementing the interface is reachable.

`rta.Analyze(roots []*ssa.Function, buildCallGraph bool) *rta.Result` returns
a `*rta.Result` whose `CallGraph` is a `*callgraph.Graph`. The nodes in the
graph are keyed on `*ssa.Function`; traverse from the roots to find the reachable
set explicitly, because the `Nodes` map may contain functions discovered as
potential callees that are not yet confirmed reachable:

```go
reachable := make(map[*ssa.Function]bool)
var dfs func(*callgraph.Node)
dfs = func(n *callgraph.Node) {
	if reachable[n.Func] {
		return
	}
	reachable[n.Func] = true
	for _, edge := range n.Out {
		if edge.Callee != nil {
			dfs(edge.Callee)
		}
	}
}
```

### Entry Points and Conservative Assumptions

RTA is only as good as its seed. Entry points that must be included:

- `main` in every main package — the explicit program start
- `init` — every package's synthesized init, which calls all user `func init()`
  declarations; drivers, codecs, and protocol handlers are commonly registered
  here via `_` imports
- `Test*`, `Benchmark*`, `Example*` — test binaries have no `main`; their entry
  points are test functions registered with the testing framework

`(*ssa.Package).Func("init")` returns the package-level init synthesized by the
SSA builder, which is exactly what is called at program startup.

### Synthetic and Generated Functions

SSA introduces wrapper functions (interface method wrappers, bound method
thunks, `make`-implicit constructors) with a non-empty `fn.Synthetic` string.
These are implementation artifacts of the SSA builder and must be excluded from
findings. A function with `fn.Pos() == 0` has no source position and is
similarly synthetic. Functions in files whose first comment is
`// Code generated` are also excluded; in practice, checking `fn.Synthetic` and
`fn.Pos() == 0` is sufficient for the common cases.

## Exercises

Set up the module:

```bash
go get golang.org/x/tools@latest
```

This is a library plus a CLI. `dce` is the library package; `cmd/dce` is the
binary. Verify with `go test -count=1 -race ./...` and
`go run ./cmd/dce -patterns ./...`.

### Exercise 1: The dce Package

Create `dce/dce.go` with the full implementation:

```go
package dce

import (
	"context"
	"errors"
	"fmt"
	"go/token"
	"sort"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// Kind classifies a dead entity.
type Kind string

const (
	KindFunction Kind = "function"
	KindType     Kind = "type"
	KindConst    Kind = "const"
	KindVar      Kind = "var"
)

// Sentinel errors returned by Config.Validate and New.
var (
	ErrNoPatterns = errors.New("dce: at least one package pattern is required")
	ErrNoEntries  = errors.New("dce: no entry points found")
)

// Finding describes one dead entity in the analyzed codebase.
type Finding struct {
	Kind   Kind   `json:"kind"`
	Name   string `json:"name"`
	Pkg    string `json:"pkg"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}

// String implements fmt.Stringer in the format "file:line: kind pkg.name is dead (reason)".
func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s %s.%s is dead (%s)",
		f.File, f.Line, f.Kind, f.Pkg, f.Name, f.Reason)
}

// Config controls what the Analyzer reports.
type Config struct {
	// Patterns are the package patterns passed to packages.Load, e.g. "./...".
	Patterns []string
	// Tests includes test functions (Test*, Benchmark*, Example*) as entry points.
	Tests bool
	// Tags are additional build tags passed to the Go toolchain.
	Tags []string
}

// Validate returns ErrNoPatterns if Patterns is empty.
func (c Config) Validate() error {
	if len(c.Patterns) == 0 {
		return fmt.Errorf("%w", ErrNoPatterns)
	}
	return nil
}

// Analyzer performs whole-program dead code analysis.
type Analyzer struct {
	cfg Config
}

// New returns a new Analyzer. It returns an error if cfg fails validation.
func New(cfg Config) (*Analyzer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Analyzer{cfg: cfg}, nil
}

// Run loads the packages named by cfg.Patterns, builds SSA, runs Rapid Type
// Analysis from all entry points, and returns findings for every function that
// is not reachable from any entry point.
func (a *Analyzer) Run(ctx context.Context) ([]Finding, error) {
	fset := token.NewFileSet()
	loadMode := packages.NeedName |
		packages.NeedFiles |
		packages.NeedCompiledGoFiles |
		packages.NeedImports |
		packages.NeedDeps |
		packages.NeedTypes |
		packages.NeedSyntax |
		packages.NeedTypesInfo

	loadCfg := &packages.Config{
		Mode:       loadMode,
		Fset:       fset,
		Tests:      a.cfg.Tests,
		BuildFlags: buildFlags(a.cfg.Tags),
		Context:    ctx,
	}

	pkgs, err := packages.Load(loadCfg, a.cfg.Patterns...)
	if err != nil {
		return nil, fmt.Errorf("dce: load packages: %w", err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		return nil, fmt.Errorf("dce: %d package error(s) (see above)", n)
	}

	// Build SSA for all packages and their transitive dependencies.
	// ssa.InstantiateGenerics causes each generic function to be instantiated
	// for every unique set of type arguments it is called with.
	prog, ssaPkgs := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	// prog.Build must be called before any analysis or access to fn.Blocks.
	prog.Build()

	// Collect entry points from every SSA package.
	var roots []*ssa.Function
	for _, sp := range ssaPkgs {
		if sp == nil {
			continue
		}
		roots = append(roots, entryPoints(sp, a.cfg.Tests)...)
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("%w", ErrNoEntries)
	}

	return a.analyze(fset, roots)
}

// analyze runs RTA from roots and returns findings for unreachable functions.
func (a *Analyzer) analyze(fset *token.FileSet, roots []*ssa.Function) ([]Finding, error) {
	// RTA builds a call graph that correctly includes virtual-dispatch edges
	// for interface method calls. Pass true so CallGraph is populated.
	rtaResult := rta.Analyze(roots, true)

	// Traverse the call graph by DFS from roots to collect the reachable set.
	// We do not rely on rtaResult.CallGraph.Nodes alone because it may contain
	// functions discovered as potential callees but not yet confirmed reachable.
	reachable := reachableFrom(rtaResult.CallGraph, roots)

	var findings []Finding
	for fn, node := range rtaResult.CallGraph.Nodes {
		if reachable[fn] {
			continue
		}
		// Exclude SSA-generated wrappers (bound method thunks, interface stubs).
		if fn.Synthetic != "" || fn.Pos() == 0 {
			continue
		}
		pos := fset.Position(fn.Pos())
		pkg := ""
		if fn.Package() != nil && fn.Package().Pkg != nil {
			pkg = fn.Package().Pkg.Path()
		}
		reason := "no callers"
		if node != nil && len(node.In) > 0 {
			reason = fmt.Sprintf("%d caller(s), none reachable from entry", len(node.In))
		}
		findings = append(findings, Finding{
			Kind:   KindFunction,
			Name:   fn.Name(),
			Pkg:    pkg,
			File:   pos.Filename,
			Line:   pos.Line,
			Reason: reason,
		})
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
}

// reachableFrom traverses the call graph by DFS from roots and returns the set
// of reachable functions.
func reachableFrom(cg *callgraph.Graph, roots []*ssa.Function) map[*ssa.Function]bool {
	reachable := make(map[*ssa.Function]bool)
	var dfs func(*callgraph.Node)
	dfs = func(n *callgraph.Node) {
		if reachable[n.Func] {
			return
		}
		reachable[n.Func] = true
		for _, edge := range n.Out {
			if edge.Callee != nil {
				dfs(edge.Callee)
			}
		}
	}
	for _, root := range roots {
		if node, ok := cg.Nodes[root]; ok {
			dfs(node)
		}
	}
	return reachable
}

// entryPoints returns all entry-point SSA functions in sp: main, init, and
// optionally test/benchmark/example functions.
func entryPoints(sp *ssa.Package, tests bool) []*ssa.Function {
	var fns []*ssa.Function
	if fn := sp.Func("main"); fn != nil {
		fns = append(fns, fn)
	}
	// "init" is the package-level init synthesized by the SSA builder.
	// It calls any user-written func init() declarations in order.
	if fn := sp.Func("init"); fn != nil {
		fns = append(fns, fn)
	}
	if tests {
		for name, member := range sp.Members {
			fn, ok := member.(*ssa.Function)
			if !ok {
				continue
			}
			if strings.HasPrefix(name, "Test") ||
				strings.HasPrefix(name, "Benchmark") ||
				strings.HasPrefix(name, "Example") {
				fns = append(fns, fn)
			}
		}
	}
	return fns
}

// buildFlags converts a list of build tags into a -tags= flag slice.
func buildFlags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	return []string{"-tags=" + strings.Join(tags, ",")}
}
```

`Finding.String()` follows the `file:line: message` convention used by the Go
toolchain and most linters, so editors and CI pipelines can parse the output
without additional configuration.

`Config.Validate()` returns a sentinel error wrapped with `%w` so callers can
use `errors.Is(err, ErrNoPatterns)` even after the error is wrapped further
up the call stack.

### Exercise 2: Test the dce Package

Create `dce/dce_test.go`. The tests for `Finding` and `Config` run fully
offline; the `Analyzer.Run` tests require a network fetch of `golang.org/x/tools`
and are left as a your-turn extension:

```go
package dce

import (
	"errors"
	"fmt"
	"testing"
)

func TestFindingString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		f    Finding
		want string
	}{
		{
			name: "function_no_callers",
			f: Finding{
				Kind:   KindFunction,
				Name:   "unusedFn",
				Pkg:    "example.com/mypkg",
				File:   "mypkg/foo.go",
				Line:   42,
				Reason: "no callers",
			},
			want: "mypkg/foo.go:42: function example.com/mypkg.unusedFn is dead (no callers)",
		},
		{
			name: "type_dead",
			f: Finding{
				Kind:   KindType,
				Name:   "OldConfig",
				Pkg:    "example.com/cfg",
				File:   "cfg/types.go",
				Line:   7,
				Reason: "no callers",
			},
			want: "cfg/types.go:7: type example.com/cfg.OldConfig is dead (no callers)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.f.String(); got != tc.want {
				t.Errorf("String() = %q\nwant     %q", got, tc.want)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{name: "empty_patterns", cfg: Config{}, wantErr: ErrNoPatterns},
		{name: "single_pattern", cfg: Config{Patterns: []string{"./..."}}, wantErr: nil},
		{name: "multi_pattern", cfg: Config{Patterns: []string{"./pkg/...", "./cmd/..."}}, wantErr: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.Validate()
			if tc.wantErr == nil && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewRejectsEmptyConfig(t *testing.T) {
	t.Parallel()

	_, err := New(Config{})
	if !errors.Is(err, ErrNoPatterns) {
		t.Fatalf("New() = %v, want ErrNoPatterns", err)
	}
}

func ExampleFinding_String() {
	f := Finding{
		Kind:   KindFunction,
		Name:   "deadHelper",
		Pkg:    "example.com/util",
		File:   "util/helper.go",
		Line:   17,
		Reason: "no callers",
	}
	fmt.Println(f)
	// Output: util/helper.go:17: function example.com/util.deadHelper is dead (no callers)
}
```

Your turn: add `TestFindingMarshalJSON` that imports `encoding/json`, creates a
`Finding` with `Kind: KindFunction` and `Name: "staleOp"`, calls
`json.Marshal` on it, and asserts that the result contains both `"function"` and
`"staleOp"` as substrings. The `json:` struct tags guarantee these values appear
in the output.

### Exercise 3: The CLI

Create `cmd/dce/main.go`. This is `package main`; it touches only exported API
from the `dce` package:

```go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"example.com/deadcode/dce"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dce:", err)
		os.Exit(1)
	}
}

func run() error {
	patternFlag := flag.String("patterns", "./...", "comma-separated package patterns")
	testsFlag := flag.Bool("tests", false, "include test functions as entry points")
	tagsFlag := flag.String("tags", "", "comma-separated build tags")
	formatFlag := flag.String("format", "text", "output format: text or json")
	flag.Parse()

	patterns := strings.Split(*patternFlag, ",")
	cfg := dce.Config{
		Patterns: patterns,
		Tests:    *testsFlag,
	}
	if *tagsFlag != "" {
		cfg.Tags = strings.Split(*tagsFlag, ",")
	}

	a, err := dce.New(cfg)
	if err != nil {
		return err
	}

	findings, err := a.Run(context.Background())
	if err != nil {
		return err
	}

	switch *formatFlag {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(findings)
	default:
		for _, f := range findings {
			fmt.Println(f)
		}
	}
	return nil
}
```

Run the tool against itself to see what it reports:

```bash
go run ./cmd/dce -patterns ./... -format text
go run ./cmd/dce -patterns ./... -format json | python3 -m json.tool
```

The JSON output is an array (possibly empty) on stdout. An empty result,
`[]`, is a valid output and means no dead code was found.

## Common Mistakes

### Forgetting prog.Build

Wrong: calling `ssautil.AllPackages` and then immediately calling
`rta.Analyze`.

What happens: every `fn.Blocks` is nil; RTA finds no call sites; the reachable
set contains only the roots themselves. The tool reports thousands of false
positives — every function in the entire program appears dead.

Fix: always call `prog.Build()` immediately after `ssautil.AllPackages` and
before any analysis.

### Seeding RTA With Only main

Wrong:

```go
roots := []*ssa.Function{mainFn}
rta.Analyze(roots, true)
```

What happens: every function called only from `init` — drivers registered via
`_` side-effect imports, SQL dialect registrations, encoding format registrations
— is reported as dead. Real programs depend heavily on `init` for this kind of
startup registration.

Fix: include every `init` function from every SSA package as a root, alongside
`main` and any test functions.

### Adding a Second Interface Filter on Top of RTA

Wrong: after running RTA, walking all types to find interface implementations
and filtering out their methods from the dead set.

What happens: the additional filter is applied on top of an already-correct
result. It re-introduces false negatives (methods that RTA correctly marked dead
are now kept alive) and misses the nuance that RTA only keeps a method alive if
BOTH the interface call is reachable AND a concrete implementer is reachable.

Fix: trust the call graph returned by `rta.Analyze`. RTA's virtual-dispatch
edges already encode interface-based reachability correctly.

### Reporting Synthetic SSA Functions

Wrong: iterating `rtaResult.CallGraph.Nodes` and reporting every non-reachable
entry without checking `fn.Synthetic`.

What happens: the output contains names like `(*sync.Mutex).Lock$bound` and
`(io.Writer).Write$interface` — SSA-internal wrappers that have no corresponding
source declaration.

Fix: skip any `*ssa.Function` where `fn.Synthetic != ""` or `fn.Pos() == 0`.

## Verification

From `~/go-exercises/deadcode`:

```bash
gofmt -l ./...
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/dce -patterns ./... -format text
go run ./cmd/dce -patterns ./... -format json | python3 -m json.tool
```

`gofmt -l` must print nothing. The JSON output must be valid even when the
result is empty. When run against its own source, the tool should report zero
dead functions for code in `dce/dce.go`: every function (`New`, `Run`,
`analyze`, `entryPoints`, `reachableFrom`, `buildFlags`) is reachable from the
`main` entry point in `cmd/dce/main.go` through the live call chain.

## Summary

- `packages.Load` with the full mode bitmask (including `NeedDeps` and
  `NeedTypesInfo`) is the correct way to load a package graph for SSA
  construction; missing either flag produces empty function bodies.
- `ssautil.AllPackages` + `prog.Build()` converts the package graph to SSA;
  skipping `Build` is the single most common mistake and produces no diagnostic.
- `rta.Analyze` builds a call graph that correctly handles virtual dispatch via
  the instantiated-types set; seeding it with main, every init, and test
  functions prevents false positives from startup registration patterns.
- Traverse the call graph by explicit DFS from the roots to get the confirmed
  reachable set; do not trust `callgraph.Graph.Nodes` to contain only reachable
  functions.
- Skip any `*ssa.Function` where `fn.Synthetic != ""` or `fn.Pos() == 0` to
  avoid reporting SSA-internal wrappers as dead code.

## What's Next

Next: [Go-to-WebAssembly Compiler](../12-go-to-wasm-compiler/12-go-to-wasm-compiler.md).

## Resources

- [golang.org/x/tools/go/packages](https://pkg.go.dev/golang.org/x/tools/go/packages) — package loading API with type information
- [golang.org/x/tools/go/ssa](https://pkg.go.dev/golang.org/x/tools/go/ssa) — SSA representation and builder for Go programs
- [golang.org/x/tools/go/callgraph/rta](https://pkg.go.dev/golang.org/x/tools/go/callgraph/rta) — Rapid Type Analysis call graph construction
- [golang.org/x/tools/go/analysis](https://pkg.go.dev/golang.org/x/tools/go/analysis) — pluggable analysis framework used by go vet and staticcheck
- [Grove et al., "Efficient Call Graph Construction Using Rapid Type Analysis", OOPSLA 1997](https://dl.acm.org/doi/10.1145/263698.264040) — the original RTA paper
