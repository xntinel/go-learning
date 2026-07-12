# 4. AST Parsing

Go exposes its own compiler's parsing machinery in the standard library. The `go/parser`, `go/ast`, and `go/token` packages let you parse any valid Go source file into a typed tree, walk every node, and extract structural information without running the compiler or using reflection. This is the foundation of linters, code generators, refactoring tools, and IDE plugins.

The hard part is not calling `parser.ParseFile` — it is navigating the resulting tree. The AST has about forty node types arranged in an inheritance-free type hierarchy. You navigate it with type assertions or `ast.Inspect`. Knowing which node covers which syntactic construct, and when a node's fields can be nil, takes practice.

```text
astparser/
  go.mod
  parser.go
  parser_test.go
  testdata/models.go
  cmd/demo/main.go
```

## Concepts

### The Three-Layer Package Stack

`go/token` defines the `FileSet` and `Pos` type. Every position in every node is a `token.Pos` — an integer offset into the `FileSet`. A `FileSet` can hold multiple files; you create one per parsing session. `go/parser` reads bytes and produces an `*ast.File`. `go/ast` defines every node type and the `Inspect` walker.

You always create the `FileSet` before parsing, and pass it to any position-printing call:

```go
fset := token.NewFileSet()
f, err := parser.ParseFile(fset, "input.go", src, parser.ParseComments)
pos := fset.Position(f.Pos()) // converts token.Pos -> token.Position (file, line, col)
```

### `ast.File` and Top-Level Declarations

The root of a parsed file is `*ast.File`. Its `Decls` field is a `[]ast.Decl` holding two concrete types:

- `*ast.GenDecl` — grouped declarations: `import`, `const`, `type`, `var`.
- `*ast.FuncDecl` — function or method declarations.

A type switch on each element of `Decls` is the idiomatic entry point for top-level analysis.

### `ast.GenDecl` and the `Specs` Slice

A `GenDecl` wraps one or more specs. The `Tok` field says which keyword introduced the group (`token.CONST`, `token.TYPE`, `token.VAR`, `token.IMPORT`). Each spec is one of `*ast.ImportSpec`, `*ast.ValueSpec`, or `*ast.TypeSpec`. Within a `TypeSpec`, the `Type` field holds the concrete shape: `*ast.StructType`, `*ast.InterfaceType`, `*ast.Ident`, etc.

### Nil Guards Are Mandatory

Many AST fields are nil in valid code. A `Field` in a struct with no tag has `Tag == nil`. A function with no return values has `Results == nil`. A `FuncDecl` that is not a method has `Recv == nil`. Dereferencing any of these without checking causes a panic.

The discipline is: every time you write `.`, ask whether the field can be nil, and add an `if` guard.

### `ast.Inspect` vs Manual Walking

`ast.Inspect` does a depth-first walk and calls a function for every node. Returning `false` prunes the subtree below that node. It is convenient for searching; it is awkward when you need parent-child context. For structured extraction (parse a file and return a typed summary), manual iteration over `file.Decls` with type switches is clearer and lets you avoid visiting nodes you do not need.

### `ast.IsExported`

`ast.IsExported(name string) bool` returns true if the first rune of `name` is an uppercase Unicode letter. It is the canonical check; do not reimplement it with `unicode.IsUpper(rune(name[0]))` (that panics on empty strings and misses multi-byte runes).

### Type Expressions Are Recursive

The type of a field is itself an `ast.Expr`, which can be:

- `*ast.Ident` — a bare name like `int` or `User`.
- `*ast.SelectorExpr` — a qualified name like `time.Time` (X=`time`, Sel=`Time`).
- `*ast.StarExpr` — a pointer: `*User`.
- `*ast.ArrayType` — a slice or array: `[]byte`, `[4]byte`.
- `*ast.MapType`, `*ast.ChanType`, `*ast.FuncType` — maps, channels, function types.

Converting a type expression to a string requires a recursive helper. The standard library provides `go/printer` and `go/format` if you need round-trip fidelity, but a small recursive function is sufficient for the common cases.

## Exercises

### Exercise 1: Create the test fixture

Create `testdata/models.go`:

```go
// testdata/models.go
package sample

import "time"

// User represents a registered user.
type User struct {
	ID        int       `json:"id" db:"id"`
	Name      string    `json:"name" db:"name"`
	Email     string    `json:"email" db:"email"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	isActive  bool
}

// Role defines permission levels.
type Role int

const (
	RoleViewer Role = iota
	RoleEditor
	RoleAdmin
	RoleOwner
)

// Repository defines data access.
type Repository interface {
	FindByID(id int) (*User, error)
	Save(user *User) error
	Delete(id int) error
}

// NewUser creates a User with defaults.
func NewUser(name, email string) *User {
	return &User{
		Name:      name,
		Email:     email,
		CreatedAt: time.Now(),
	}
}

// Validate checks user data.
func (u *User) Validate() error {
	return nil
}
```

### Exercise 2: Implement the library

Create `parser.go`:

```go
// parser.go
package astparser

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// FileInfo holds the extracted top-level declarations from a parsed Go file.
type FileInfo struct {
	Package   string
	Structs   []StructInfo
	Constants []ConstInfo
	Functions []FuncInfo
}

// StructInfo describes a single struct type declaration.
type StructInfo struct {
	Name     string
	Exported bool
	Fields   []FieldInfo
}

// FieldInfo describes a single field in a struct.
type FieldInfo struct {
	Name     string
	TypeStr  string
	Tag      string
	Exported bool
}

// ConstInfo describes a single constant name.
type ConstInfo struct {
	Name     string
	Exported bool
}

// FuncInfo describes a function or method declaration.
type FuncInfo struct {
	Name     string
	Receiver string
	Params   string
	Results  string
	Exported bool
}

// ParseFile parses the Go source file at path and returns a FileInfo.
// src may be nil to read from disk, or a []byte / string / io.Reader.
func ParseFile(path string, src interface{}) (*FileInfo, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	info := &FileInfo{Package: f.Name.Name}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE:
				for _, spec := range d.Specs {
					ts := spec.(*ast.TypeSpec)
					if st, ok := ts.Type.(*ast.StructType); ok {
						info.Structs = append(info.Structs, extractStruct(ts.Name.Name, st))
					}
				}
			case token.CONST:
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for _, name := range vs.Names {
						info.Constants = append(info.Constants, ConstInfo{
							Name:     name.Name,
							Exported: ast.IsExported(name.Name),
						})
					}
				}
			}
		case *ast.FuncDecl:
			info.Functions = append(info.Functions, extractFunc(d))
		}
	}

	return info, nil
}

func extractStruct(name string, st *ast.StructType) StructInfo {
	si := StructInfo{
		Name:     name,
		Exported: ast.IsExported(name),
	}
	for _, field := range st.Fields.List {
		tag := ""
		if field.Tag != nil {
			tag = strings.Trim(field.Tag.Value, "`")
		}
		typeStr := exprString(field.Type)
		for _, n := range field.Names {
			si.Fields = append(si.Fields, FieldInfo{
				Name:     n.Name,
				TypeStr:  typeStr,
				Tag:      tag,
				Exported: ast.IsExported(n.Name),
			})
		}
	}
	return si
}

func extractFunc(d *ast.FuncDecl) FuncInfo {
	fi := FuncInfo{
		Name:     d.Name.Name,
		Exported: ast.IsExported(d.Name.Name),
		Params:   fieldListString(d.Type.Params),
		Results:  fieldListString(d.Type.Results),
	}
	if d.Recv != nil && len(d.Recv.List) > 0 {
		fi.Receiver = exprString(d.Recv.List[0].Type)
	}
	return fi
}

func exprString(expr ast.Expr) string {
	if expr == nil {
		return ""
	}
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(e.X)
	case *ast.ArrayType:
		if e.Len == nil {
			return "[]" + exprString(e.Elt)
		}
		return "[...]" + exprString(e.Elt)
	case *ast.MapType:
		return "map[" + exprString(e.Key) + "]" + exprString(e.Value)
	case *ast.InterfaceType:
		return "interface{}"
	default:
		return fmt.Sprintf("%T", expr)
	}
}

func fieldListString(fl *ast.FieldList) string {
	if fl == nil || len(fl.List) == 0 {
		return ""
	}
	var parts []string
	for _, f := range fl.List {
		typeName := exprString(f.Type)
		if len(f.Names) == 0 {
			parts = append(parts, typeName)
		} else {
			for _, name := range f.Names {
				parts = append(parts, name.Name+" "+typeName)
			}
		}
	}
	return strings.Join(parts, ", ")
}
```

### Exercise 3: Write the tests

Create `parser_test.go`:

```go
// parser_test.go
package astparser

import (
	"testing"
)

func TestParseFileStructs(t *testing.T) {
	t.Parallel()

	info, err := ParseFile("testdata/models.go", nil)
	if err != nil {
		t.Fatal(err)
	}

	if info.Package != "sample" {
		t.Fatalf("package = %q, want sample", info.Package)
	}

	var user *StructInfo
	for i := range info.Structs {
		if info.Structs[i].Name == "User" {
			user = &info.Structs[i]
			break
		}
	}
	if user == nil {
		t.Fatal("User struct not found")
	}
	if !user.Exported {
		t.Error("User should be exported")
	}
}

func TestParseFileFields(t *testing.T) {
	t.Parallel()

	info, err := ParseFile("testdata/models.go", nil)
	if err != nil {
		t.Fatal(err)
	}

	var user *StructInfo
	for i := range info.Structs {
		if info.Structs[i].Name == "User" {
			user = &info.Structs[i]
			break
		}
	}
	if user == nil {
		t.Fatal("User struct not found")
	}

	cases := []struct {
		name     string
		typeStr  string
		exported bool
		wantTag  string
	}{
		{"ID", "int", true, "json:\"id\" db:\"id\""},
		{"Name", "string", true, "json:\"name\" db:\"name\""},
		{"Email", "string", true, "json:\"email\" db:\"email\""},
		{"CreatedAt", "time.Time", true, "json:\"created_at\" db:\"created_at\""},
		{"isActive", "bool", false, ""},
	}

	if len(user.Fields) != len(cases) {
		t.Fatalf("field count = %d, want %d", len(user.Fields), len(cases))
	}

	for i, tc := range cases {
		f := user.Fields[i]
		if f.Name != tc.name {
			t.Errorf("field[%d].Name = %q, want %q", i, f.Name, tc.name)
		}
		if f.TypeStr != tc.typeStr {
			t.Errorf("field[%d].TypeStr = %q, want %q", i, f.TypeStr, tc.typeStr)
		}
		if f.Exported != tc.exported {
			t.Errorf("field[%d].Exported = %v, want %v", i, f.Exported, tc.exported)
		}
		if f.Tag != tc.wantTag {
			t.Errorf("field[%d].Tag = %q, want %q", i, f.Tag, tc.wantTag)
		}
	}
}

func TestParseFileConstants(t *testing.T) {
	t.Parallel()

	info, err := ParseFile("testdata/models.go", nil)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"RoleViewer", "RoleEditor", "RoleAdmin", "RoleOwner"}
	if len(info.Constants) != len(want) {
		t.Fatalf("constant count = %d, want %d", len(info.Constants), len(want))
	}
	for i, name := range want {
		if info.Constants[i].Name != name {
			t.Errorf("constant[%d] = %q, want %q", i, info.Constants[i].Name, name)
		}
		if !info.Constants[i].Exported {
			t.Errorf("constant[%d] should be exported", i)
		}
	}
}

func TestParseFileFunctions(t *testing.T) {
	t.Parallel()

	info, err := ParseFile("testdata/models.go", nil)
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, fn := range info.Functions {
		if fn.Name == "NewUser" {
			found = true
			if fn.Receiver != "" {
				t.Errorf("NewUser should have no receiver, got %q", fn.Receiver)
			}
			if !fn.Exported {
				t.Error("NewUser should be exported")
			}
		}
		if fn.Name == "Validate" {
			if fn.Receiver == "" {
				t.Error("Validate should have a receiver")
			}
		}
	}
	if !found {
		t.Fatal("NewUser function not found")
	}
}

func TestParseFileFromBytes(t *testing.T) {
	t.Parallel()

	src := []byte(`package pkg

type Point struct {
	X int
	Y int
}
`)
	info, err := ParseFile("point.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Structs) != 1 || info.Structs[0].Name != "Point" {
		t.Fatalf("expected Point struct, got %+v", info.Structs)
	}
	if len(info.Structs[0].Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(info.Structs[0].Fields))
	}
}

func TestParseFileSyntaxError(t *testing.T) {
	t.Parallel()

	src := []byte(`package pkg
func bad( {
`)
	_, err := ParseFile("bad.go", src)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestExprStringVariants(t *testing.T) {
	t.Parallel()

	src := []byte(`package pkg

type T struct {
	A *int
	B []string
	C map[string]int
}
`)
	info, err := ParseFile("t.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Structs) == 0 {
		t.Fatal("no structs found")
	}
	fields := info.Structs[0].Fields
	cases := []struct{ name, typeStr string }{
		{"A", "*int"},
		{"B", "[]string"},
		{"C", "map[string]int"},
	}
	if len(fields) != len(cases) {
		t.Fatalf("field count = %d, want %d", len(fields), len(cases))
	}
	for i, tc := range cases {
		if fields[i].Name != tc.name || fields[i].TypeStr != tc.typeStr {
			t.Errorf("field[%d] = {%q, %q}, want {%q, %q}",
				i, fields[i].Name, fields[i].TypeStr, tc.name, tc.typeStr)
		}
	}
}
```

### Exercise 4: CLI demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"os"

	"example.com/astparser"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: demo <file.go>")
		os.Exit(1)
	}

	info, err := astparser.ParseFile(os.Args[1], nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Package: %s\n\n", info.Package)

	for _, s := range info.Structs {
		fmt.Printf("Struct: %s (exported=%v)\n", s.Name, s.Exported)
		for _, f := range s.Fields {
			fmt.Printf("  field: %-12s type=%-15s exported=%v tag=%q\n",
				f.Name, f.TypeStr, f.Exported, f.Tag)
		}
		fmt.Println()
	}

	if len(info.Constants) > 0 {
		fmt.Println("Constants:")
		for _, c := range info.Constants {
			fmt.Printf("  %s (exported=%v)\n", c.Name, c.Exported)
		}
		fmt.Println()
	}

	for _, fn := range info.Functions {
		if fn.Receiver != "" {
			fmt.Printf("Method: (%s) %s(%s)", fn.Receiver, fn.Name, fn.Params)
		} else {
			fmt.Printf("Func: %s(%s)", fn.Name, fn.Params)
		}
		if fn.Results != "" {
			fmt.Printf(" (%s)", fn.Results)
		}
		fmt.Println()
	}
}
```

## Common Mistakes

### Forgetting nil guards on optional AST fields

Wrong: `tag := field.Tag.Value` when a field has no tag. This panics because `field.Tag` is nil.

What happens: runtime panic: nil pointer dereference.

Fix: always check `if field.Tag != nil` before accessing `field.Tag.Value`.

### Using `token.IDENT` instead of `ast.IsExported`

Wrong: `if name[0] >= 'A' && name[0] <= 'Z'` or `unicode.IsUpper(rune(name[0]))` on a possibly-empty string.

What happens: index out of range panic on empty identifier, or incorrect result for non-ASCII exported names (Go allows Unicode identifiers).

Fix: use `ast.IsExported(name)` which handles empty strings and full Unicode correctly.

### Forgetting that `GenDecl.Specs` requires a type assertion

Wrong: treating `decl.Specs[0]` as a `*ast.TypeSpec` when `decl.Tok == token.CONST`. For const groups the spec is `*ast.ValueSpec`.

What happens: panic: interface conversion: ast.Spec is *ast.TypeSpec, not *ast.ValueSpec.

Fix: switch on `decl.Tok` first, then assert the correct spec type for each case.

### Using `ast.Inspect` when you need parent context

Wrong: using `ast.Inspect` and trying to know whether a `*ast.TypeSpec` is inside a `GenDecl` with `token.TYPE`. The callback only receives the current node, not its parent.

What happens: you cannot distinguish a type declaration from a type inside a function without extra bookkeeping.

Fix: iterate `file.Decls` manually and use a type switch. This keeps the parent visible at every step.

### Stripping backticks from tag values

Wrong: comparing `field.Tag.Value` directly to `json:"id"`. The `Value` field includes the surrounding backtick characters.

What happens: tag comparison always fails.

Fix: use `strings.Trim(field.Tag.Value, "`")` to remove the backticks before parsing or comparing.

## Verification

Run from the module root:

```bash
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All three commands must exit 0.

## Summary

- `go/parser.ParseFile` accepts a path, an optional source (nil reads from disk), and mode flags; it returns `*ast.File` and an error.
- `token.NewFileSet` must be created before parsing; it maps `token.Pos` integers back to file/line/column.
- Top-level declarations in `*ast.File` are `*ast.GenDecl` (grouped import/const/type/var) or `*ast.FuncDecl`.
- `ast.GenDecl.Tok` distinguishes the keyword; each `Specs` element requires a type assertion matching the keyword.
- Every AST field that can be absent in valid code (tag, receiver, return list) can be nil; always check before dereferencing.
- `ast.IsExported(name)` is the canonical visibility check.
- Converting type expressions to strings requires a recursive helper that handles `*ast.Ident`, `*ast.SelectorExpr`, `*ast.StarExpr`, and container types.

## What's Next

Next: [Template-Based Code Generation](../05-template-based-code-generation/05-template-based-code-generation.md).

## Resources

- [go/ast package](https://pkg.go.dev/go/ast)
- [go/parser package](https://pkg.go.dev/go/parser)
- [go/token package](https://pkg.go.dev/go/token)
- [Go specification: Declarations and scope](https://go.dev/ref/spec#Declarations_and_scope)
- [golang.org/x/tools/go/analysis: building analyzers on top of go/ast](https://pkg.go.dev/golang.org/x/tools/go/analysis)
