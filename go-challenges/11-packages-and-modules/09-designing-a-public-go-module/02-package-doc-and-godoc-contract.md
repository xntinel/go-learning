# Exercise 2: Write the documentation contract and verify it with go doc

Documentation on a shared library is not decoration; it is the contract consumers
read before they depend on you. This exercise writes the package-level doc comment
and per-symbol comments that make `go doc` and pkg.go.dev render a coherent
contract — and then adds a parse-time doc-lint test that *fails the build* if any
exported symbol is undocumented or its comment does not start with the symbol name.
Guarding docs with a test is how a library keeps them from rotting as it grows.

This module is fully self-contained: its own `go mod init`, a `doc.go`, the
library, a demo, and a doc-lint test that parses the package with `go/doc`.

## What you'll build

```text
publicstr/                 independent module: example.com/publicstr
  go.mod                   go 1.26
  doc.go                   package-level doc comment ("Package publicstr ...")
  strings.go               Slugify, Truncate; var ErrEmpty (each documented)
  cmd/
    demo/
      main.go              prints go-doc-style output for the package
  doclint_test.go          parses the package; asserts every export is documented
```

- Files: `doc.go`, `strings.go`, `cmd/demo/main.go`, `doclint_test.go`.
- Implement: a `doc.go` package comment starting with `Package publicstr`, and doc comments on every exported symbol that start with the symbol's name.
- Test: a `go/doc`-based lint that asserts every exported func, type, and single-name var has a non-empty `Doc` beginning with its name.
- Verify: `go test -count=1 -race ./...` then `go doc ./...`

### The two conventions that make docs a contract

`go doc` and pkg.go.dev associate a comment with a symbol only when the comment
*immediately precedes* the declaration and, by convention, *starts with the symbol
name*. `// Slugify converts ...` renders as the documentation of `Slugify`;
`// This function slugifies ...` still renders, but tooling and readers lose the
name-anchored structure, and gofmt/vet-adjacent linters and `go doc`'s own
heading logic expect the prefix. The package comment is special: it lives on the
`package` clause (conventionally in a dedicated `doc.go`) and starts with
`Package <name>`. Put these two conventions under test and they cannot regress: a
new exported symbol added without a proper comment fails CI instead of shipping
undocumented.

The doc-lint test uses the same machinery `go doc` itself is built on. `go/parser`
turns the package's source files into ASTs; `go/doc.NewFromFiles` builds the
documentation model from them — the same model pkg.go.dev renders. Walking
`pkg.Funcs`, `pkg.Types` (and each type's constructor `Funcs` and `Methods`), and
`pkg.Vars`, the test asserts each has a `Doc` that begins with its `Name`. Because
it reads the real source in the module directory, it verifies exactly what
consumers will see.

Create `doc.go`:

```go
// Package publicstr provides small, focused, URL-and-storage-oriented string
// utilities intended to be imported across many services. The surface is
// intentionally minimal: each exported symbol does one thing, documents its
// exact behavior, and is covered by a runnable example so the documentation
// cannot drift from the implementation.
//
// The compatibility promise: exported identifiers and their observable behavior
// are stable within a major version. New behavior is added through new symbols
// and options, never by changing an existing signature.
package publicstr
```

Create `strings.go`. Note every exported symbol's comment starts with its name:

```go
package publicstr

import (
	"errors"
	"strings"
	"unicode"
)

// ErrEmpty is returned when the input is empty or reduces to no slug-able
// characters. It is a sentinel: match it with errors.Is. Its identity is part
// of the public contract.
var ErrEmpty = errors.New("publicstr: empty string")

// Slugify converts s to a URL-safe slug: lowercase, ASCII letters and digits
// only, with non-alphanumeric runs collapsed to a single hyphen and leading and
// trailing hyphens trimmed. It returns ErrEmpty when the result would be empty.
func Slugify(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", ErrEmpty
	}
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case unicode.IsSpace(r), r == '-', r == '_':
			if !prevHyphen && b.Len() > 0 {
				b.WriteRune('-')
				prevHyphen = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return "", ErrEmpty
	}
	return out, nil
}

// Truncate returns s bounded to at most n bytes; when s is longer, "..." is
// appended and counts toward the limit. It returns ErrEmpty on empty input.
func Truncate(s string, n int) (string, error) {
	if s == "" {
		return "", ErrEmpty
	}
	if n <= 3 {
		return strings.Repeat(".", n), nil
	}
	if len(s) <= n {
		return s, nil
	}
	return s[:n-3] + "...", nil
}
```

### The runnable demo

The demo prints a `go doc`-style rendering of the package's own documentation, so
you can see the same contract text `go doc ./...` would produce, driven from the
exported API.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/publicstr"
)

func main() {
	slug, _ := publicstr.Slugify("Release Notes v2!")
	trunc, _ := publicstr.Truncate("changelog entry", 10)
	fmt.Printf("Slugify -> %s\n", slug)
	fmt.Printf("Truncate -> %s\n", trunc)
	fmt.Printf("ErrEmpty -> %v\n", publicstr.ErrEmpty)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Slugify -> release-notes-v2
Truncate -> changel...
ErrEmpty -> publicstr: empty string
```

After the tests pass, read the real rendered contract:

```bash
go doc ./...
go doc Slugify
```

### The doc-lint test

The test parses every non-test `.go` file in the package directory, builds the
`go/doc` model, and asserts that each exported func, type (and its
constructors/methods), and single-name var has a doc comment beginning with its
name. It is the mechanical guard that a future undocumented export fails the
build.

Create `doclint_test.go`:

```go
package publicstr

import (
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// loadDoc parses the current package directory (excluding _test.go files) and
// returns the go/doc model, the same one pkg.go.dev renders.
func loadDoc(t *testing.T) *doc.Package {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", name, err)
		}
		files = append(files, f)
	}
	pkg, err := doc.NewFromFiles(fset, files, "example.com/publicstr")
	if err != nil {
		t.Fatalf("doc.NewFromFiles: %v", err)
	}
	return pkg
}

func TestPackageIsDocumented(t *testing.T) {
	t.Parallel()
	pkg := loadDoc(t)
	if pkg.Doc == "" {
		t.Fatal("package has no doc comment")
	}
	if !strings.HasPrefix(pkg.Doc, "Package publicstr") {
		t.Fatalf("package doc = %q, want prefix %q", firstLine(pkg.Doc), "Package publicstr")
	}
}

func TestEveryExportStartsWithItsName(t *testing.T) {
	t.Parallel()
	pkg := loadDoc(t)
	check := func(kind, name, docText string) {
		if docText == "" {
			t.Errorf("%s %s has no doc comment", kind, name)
			return
		}
		if !strings.HasPrefix(docText, name) {
			t.Errorf("%s %s doc = %q, want prefix %q", kind, name, firstLine(docText), name)
		}
	}
	for _, f := range pkg.Funcs {
		check("func", f.Name, f.Doc)
	}
	for _, ty := range pkg.Types {
		check("type", ty.Name, ty.Doc)
		for _, f := range ty.Funcs {
			check("func", f.Name, f.Doc)
		}
		for _, m := range ty.Methods {
			check("method", m.Name, m.Doc)
		}
	}
	for _, v := range pkg.Vars {
		if len(v.Names) != 1 {
			continue // grouped var blocks share one comment; skip
		}
		check("var", v.Names[0], v.Doc)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
```

## Review

The contract is documented correctly when the doc-lint test passes: the package
comment starts with `Package publicstr`, and every exported func, type, and
single-name var has a comment beginning with its own name. The failure mode this
guards against is the most common docs regression — a new export merged with a
`// This function ...` comment or no comment at all, which `go doc` cannot anchor
and pkg.go.dev renders blank. Because the test uses `go/doc.NewFromFiles`, the same
model the official renderer uses, a passing test means the rendered docs are
coherent. Note the test filters out `_test.go` files: examples and test helpers are
not part of the consumer-facing surface, so linting them would be noise. Run
`go doc ./...` after the tests to read the exact text a consumer sees.

## Resources

- [`go/doc`](https://pkg.go.dev/go/doc) — `NewFromFiles` and the `Package`/`Type`/`Func`/`Value` model that pkg.go.dev renders.
- [cmd/go: Show documentation (go doc)](https://pkg.go.dev/cmd/go#hdr-Show_documentation_for_package_or_symbol) — how `go doc` selects and prints comments.
- [Go Doc Comments](https://go.dev/doc/comment) — the official specification for doc-comment syntax and the name-prefix convention.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-slug-library-core-and-sentinel-error.md](01-slug-library-core-and-sentinel-error.md) | Next: [03-testable-examples-that-cannot-rot.md](03-testable-examples-that-cannot-rot.md)
