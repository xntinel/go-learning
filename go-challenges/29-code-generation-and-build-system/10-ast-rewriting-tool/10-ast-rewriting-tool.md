# 10. AST Rewriting Tool

AST rewriting is the technique behind `gofmt`, `goimports`, and the refactoring features in `gopls`. Unlike code generation (which creates new files), AST rewriting transforms existing source code in place. The hard part is not parsing or printing — those are single function calls — but performing correct targeted mutations on a live AST without corrupting comment positions, preserving all unaffected code exactly, and producing output that compiles.

This lesson builds a `rewriter` package using only standard library tools (`go/ast`, `go/parser`, `go/format`) that rewrites `fmt.Errorf` calls replacing `%v` with `%w` in the format string. The external package `golang.org/x/tools/go/ast/astutil` provides a more ergonomic cursor-based traversal for production use and is described in the Concepts section; the exercises use only stdlib to avoid the network dependency in the gate.

```text
rewriter/
  go.mod
  rewriter.go
  rewriter_test.go
  cmd/rewriter/main.go
```

## Concepts

### How AST Rewriting Differs From Code Generation

Code generation reads a model (structs, schemas, config) and writes new files. AST rewriting reads existing Go source, modifies the AST in place, and writes the transformed source back. The critical constraint: every unmodified token must land in exactly the same position in the output. A modification that shifts token positions causes `go/printer` to produce syntactically correct but visually garbled output.

`go/format.Source` is safer than `go/printer` for this reason. It runs the printer and then normalizes whitespace, so minor position corruption is absorbed rather than propagated.

### The `ast.Inspect` Mutation Pattern

`ast.Inspect(f, fn)` walks the AST depth-first, calling `fn` for each node. The function receives the node and returns a bool — `true` means continue descending, `false` means skip children.

To mutate during inspection, store a pointer to the parent node alongside the current node. When you find a node to replace, update the parent's field:

```go
ast.Inspect(f, func(n ast.Node) bool {
    call, ok := n.(*ast.CallExpr)
    if !ok {
        return true
    }
    // mutate call.Args, call.Fun, etc. in place
    return true
})
```

Direct mutation works correctly as long as you do not add or remove siblings in the same list during iteration. To add or remove elements from a slice, collect the changes first and apply them after inspection.

### Identifying `fmt.Errorf` Calls

A `fmt.Errorf(format, args...)` call in the AST is an `*ast.CallExpr` where:
- `call.Fun` is an `*ast.SelectorExpr`
- The selector's `X` is an `*ast.Ident` with `Name == "fmt"`
- The selector's `Sel` is an `*ast.Ident` with `Name == "Errorf"`
- `call.Args[0]` is an `*ast.BasicLit` with `Kind == token.STRING`

The format string is a Go string literal, so it includes surrounding quotes. Use `strconv.Unquote` to get the raw string content before analyzing verb sequences.

### Replacing `%v` With `%w`

The `%w` verb, introduced in Go 1.13, wraps an error for use with `errors.Is` and `errors.As`. It is valid only as the last verb in a `fmt.Errorf` format string and only when the corresponding argument implements `error`.

The rewrite rule for this lesson is conservative: replace `%v` with `%w` in the format string of `fmt.Errorf` calls when the last argument's type is `error`-typed (identified by name convention: variables named `err`, `Err`, or ending in `Err`). This avoids full type inference, which requires `go/types` and the type-checker.

### `go/format.Source` as the Output Stage

After mutating the AST, convert it back to source bytes:

```go
var buf bytes.Buffer
if err := format.Node(&buf, fset, f); err != nil {
    return nil, err
}
return format.Source(buf.Bytes())
```

`format.Node` renders the AST with its original token positions. `format.Source` normalizes the result. The two-step approach is more reliable than `go/printer` alone because `format.Source` absorbs minor positional drift.

### `golang.org/x/tools/go/ast/astutil` (Production Use)

The `astutil` package from `golang.org/x/tools` provides `astutil.Apply(f, pre, post)`, a cursor-based traversal that supplies a `Cursor` to the visitor. The cursor supports `cursor.Replace(node)`, `cursor.InsertBefore(node)`, and `cursor.Delete()` — safe operations that maintain correct parent-child linkages. It also provides `astutil.AddImport` and `astutil.DeleteImport` for correct import block management.

For production refactoring tools, prefer `astutil.Apply` over direct AST mutation. For the gate-testable exercises in this lesson, stdlib suffices.

## Exercises

### Exercise 1: Implement the rewriter library

Create `rewriter.go`:

```go
package rewriter

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// RewriteResult summarizes what changed.
type RewriteResult struct {
	// Changes is the number of format verbs rewritten.
	Changes int
}

// RewriteErrorfVerbs parses src as Go source, replaces "%v" with "%w" in
// fmt.Errorf format strings when the pattern is safe to wrap, and returns
// the formatted result. If no changes are made, the returned bytes equal
// the formatted input.
//
// The rewrite is conservative: it replaces only the last "%v" in the format
// string, and only when the last argument to fmt.Errorf is an identifier
// whose name contains "err" (case-insensitive). This heuristic avoids
// the need for full type-checking.
func RewriteErrorfVerbs(src []byte) ([]byte, RewriteResult, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "input.go", src, parser.ParseComments)
	if err != nil {
		return nil, RewriteResult{}, fmt.Errorf("parse: %w", err)
	}

	var result RewriteResult

	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isErrorf(call) {
			return true
		}
		if len(call.Args) < 2 {
			return true
		}

		fmtLit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || fmtLit.Kind != token.STRING {
			return true
		}

		lastArg := call.Args[len(call.Args)-1]
		if !looksLikeError(lastArg) {
			return true
		}

		raw, err := strconv.Unquote(fmtLit.Value)
		if err != nil {
			return true
		}

		// Replace only the last %v in the format string.
		idx := strings.LastIndex(raw, "%v")
		if idx == -1 {
			return true
		}

		newRaw := raw[:idx] + "%w" + raw[idx+2:]
		fmtLit.Value = strconv.Quote(newRaw)
		result.Changes++

		return true
	})

	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return nil, result, fmt.Errorf("print: %w", err)
	}
	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, result, fmt.Errorf("format: %w", err)
	}
	return out, result, nil
}

// isErrorf returns true when call is fmt.Errorf.
func isErrorf(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return pkg.Name == "fmt" && sel.Sel.Name == "Errorf"
}

// looksLikeError returns true when the expression is an identifier whose name
// contains "err" (case-insensitive). This is a heuristic that avoids
// full type-checking.
func looksLikeError(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	return strings.Contains(strings.ToLower(id.Name), "err")
}
```

### Exercise 2: Write the tests

Create `rewriter_test.go`:

```go
package rewriter

import (
	"strings"
	"testing"
)

func TestRewriteErrorfVerbsBasic(t *testing.T) {
	t.Parallel()

	src := []byte(`package p

import "fmt"

func open(err error) error {
	return fmt.Errorf("open db: %v", err)
}
`)
	out, res, err := RewriteErrorfVerbs(src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changes != 1 {
		t.Errorf("changes = %d, want 1", res.Changes)
	}
	if !strings.Contains(string(out), `"open db: %w"`) {
		t.Errorf("output missing %%w rewrite:\n%s", out)
	}
}

func TestRewriteErrorfVerbsNoChangeWhenNoErrArg(t *testing.T) {
	t.Parallel()

	src := []byte(`package p

import "fmt"

func f(msg string) error {
	return fmt.Errorf("wrapped: %v", msg)
}
`)
	out, res, err := RewriteErrorfVerbs(src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changes != 0 {
		t.Errorf("changes = %d, want 0 (last arg is not an error-like identifier)", res.Changes)
	}
	if strings.Contains(string(out), `%w`) {
		t.Error("output should not contain %%w when last arg is not err-like")
	}
}

func TestRewriteErrorfVerbsMultipleCalls(t *testing.T) {
	t.Parallel()

	src := []byte(`package p

import "fmt"

func a(err error) error {
	return fmt.Errorf("a: %v", err)
}

func b(err error) error {
	return fmt.Errorf("b: %v", err)
}
`)
	out, res, err := RewriteErrorfVerbs(src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changes != 2 {
		t.Errorf("changes = %d, want 2", res.Changes)
	}
	if strings.Count(string(out), `%w`) != 2 {
		t.Errorf("expected 2 occurrences of %%w:\n%s", out)
	}
}

func TestRewriteErrorfVerbsNoCallsUnchanged(t *testing.T) {
	t.Parallel()

	src := []byte(`package p

func f() string {
	return "hello"
}
`)
	out, res, err := RewriteErrorfVerbs(src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changes != 0 {
		t.Errorf("changes = %d, want 0", res.Changes)
	}
	if strings.Contains(string(out), "%w") {
		t.Error("output should be unchanged")
	}
}

func TestRewriteErrorfVerbsAlreadyWrapped(t *testing.T) {
	t.Parallel()

	src := []byte(`package p

import "fmt"

func f(err error) error {
	return fmt.Errorf("already: %w", err)
}
`)
	_, res, err := RewriteErrorfVerbs(src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changes != 0 {
		t.Errorf("changes = %d, want 0 (already uses %%w)", res.Changes)
	}
}

func TestRewriteErrorfVerbsPreservesComments(t *testing.T) {
	t.Parallel()

	src := []byte(`package p

import "fmt"

// f wraps the error.
func f(err error) error {
	// this is a comment
	return fmt.Errorf("wrap: %v", err)
}
`)
	out, _, err := RewriteErrorfVerbs(src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "// f wraps the error.") {
		t.Error("top-level comment should be preserved")
	}
	if !strings.Contains(string(out), "// this is a comment") {
		t.Error("inline comment should be preserved")
	}
}

func TestRewriteErrorfVerbsSyntaxError(t *testing.T) {
	t.Parallel()

	bad := []byte(`package p
func bad( {
`)
	_, _, err := RewriteErrorfVerbs(bad)
	if err == nil {
		t.Fatal("expected error for invalid Go source")
	}
}

func ExampleRewriteErrorfVerbs() {
	src := []byte(`package p

import "fmt"

func wrap(err error) error {
	return fmt.Errorf("context: %v", err)
}
`)
	out, res, _ := RewriteErrorfVerbs(src)
	_ = res
	_ = out
	// Output:
}
```

### Exercise 3: CLI entry point

Create `cmd/rewriter/main.go`:

```go
package main

import (
	"flag"
	"fmt"
	"os"

	"example.com/rewriter"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "print rewritten source to stdout instead of modifying the file")
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: rewriter [-dry-run] <file.go>")
		os.Exit(1)
	}

	path := flag.Arg(0)
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", path, err)
		os.Exit(1)
	}

	out, res, err := rewriter.RewriteErrorfVerbs(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rewrite %s: %v\n", path, err)
		os.Exit(1)
	}

	if res.Changes == 0 {
		fmt.Printf("%s: no changes\n", path)
		return
	}

	if *dryRun {
		os.Stdout.Write(out)
		return
	}

	if err := os.WriteFile(path, out, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Printf("%s: rewrote %d fmt.Errorf call(s)\n", path, res.Changes)
}
```

## Common Mistakes

### Mutating the AST While Iterating Over a Slice

Wrong: inside `ast.Inspect`, appending to or removing from `call.Args` while the same range is still being visited.

What happens: the iterator skips nodes or visits the same node twice, producing incorrect output or an infinite loop.

Fix: collect mutations in a separate pass or apply changes to a copy of the slice, then assign back after inspection.

### Not Stripping Quotes Before Analyzing Format Strings

Wrong: searching for `%v` in `fmtLit.Value` directly, where `fmtLit.Value` is `"\"open: %v\""` (with surrounding quote characters).

What happens: the search succeeds at the wrong index, and `strconv.Quote(newRaw)` produces an incorrectly escaped string literal.

Fix: use `strconv.Unquote(fmtLit.Value)` to get the raw string, modify it, then `strconv.Quote(modified)` to produce the new literal.

### Using `go/printer` Without `format.Source`

Wrong: rendering the AST with `printer.Fprint` and writing that output directly.

What happens: minor positional drift from AST mutations causes inconsistent indentation that fails `gofmt -d`.

Fix: pipe `format.Node` output through `format.Source`. The second pass normalizes whitespace and absorbs positional drift.

### Applying `%v` to `%w` Replacement Unconditionally

Wrong: replacing every `%v` in every `fmt.Errorf` call regardless of whether the corresponding argument is an error.

What happens: format strings like `fmt.Errorf("count: %v items", n)` get rewritten to `fmt.Errorf("count: %w items", n)`, which fails to compile because `n` (an int) does not implement `error`.

Fix: check that the last argument looks like an error before rewriting. A conservative heuristic is checking whether the identifier name contains "err" (case-insensitive). For exact correctness, use `go/types` to resolve the type.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Your turn: add `TestRewriteErrorfVerbsNamedErr` that verifies an argument named `parseErr` (ending in "Err") is also recognized as an error-like identifier and triggers the rewrite.

## Summary

- AST rewriting mutates an existing `*ast.File` in place and renders it back to source with `format.Node` + `format.Source`.
- `ast.Inspect` walks the AST depth-first; targeted mutations to the current node are safe as long as you do not add or remove siblings during traversal.
- Use `strconv.Unquote` to get the raw string from a `token.STRING` literal before analyzing it; use `strconv.Quote` to produce the new literal.
- `golang.org/x/tools/go/ast/astutil` provides `Apply` (cursor-based traversal with `Replace`/`Delete`) and `AddImport`/`DeleteImport` for production refactoring tools; it requires a network dependency and is not needed for basic rewriting.
- Always pipe `format.Node` output through `format.Source`; the second pass absorbs positional drift from mutations.

## What's Next

Next: [Graceful Shutdown](../../30-production-patterns/01-graceful-shutdown/01-graceful-shutdown.md).

## Resources

- [go/ast package](https://pkg.go.dev/go/ast)
- [go/parser package](https://pkg.go.dev/go/parser)
- [go/format package](https://pkg.go.dev/go/format)
- [go/printer package](https://pkg.go.dev/go/printer)
- [golang.org/x/tools/go/ast/astutil](https://pkg.go.dev/golang.org/x/tools/go/ast/astutil)
