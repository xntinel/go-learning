# 5. Template-Based Code Generation

Real-world generators combine two pieces: an AST front-end that extracts structural information from Go source, and a template back-end that renders that information into new Go source. The result is code that writes code — a generator that reads struct definitions and emits constructors, builders, and validators without any hand-written boilerplate.

The difficulty is in the coupling between the two layers. The template must produce syntactically valid Go, `go/format.Source` verifies this, and any whitespace or punctuation mistake fails the build. Getting the template right, keeping it readable, and making the whole thing testable in-process without shelling out are the skills this lesson builds.

```text
structgen/
  go.mod
  structgen.go
  structgen_test.go
  cmd/structgen/main.go
```

## Concepts

### Why Combine AST Parsing With Templates

A pure-template approach (reading struct definitions from a config file) is fragile — you duplicate information that already exists in Go source. A pure-AST approach (building the output string by concatenating `ast.Expr` values) produces unreadable generator code. The standard pattern, used by `protoc-gen-go`, `ent`, and `sqlc`, is: parse the source into typed data structures, feed those into `text/template`, then format the output with `go/format.Source`.

### `parser.ParseFile` With In-Memory Source

`parser.ParseFile(fset, name, src, mode)` accepts a source argument of type `any`. If `src` is `nil`, it reads from the file named by the first argument. If `src` is a `[]byte`, a `string`, or an `io.Reader`, it parses that content directly and uses the name argument only for error messages. This lets generators operate in-process on bytes without touching the filesystem, which is essential for testability.

```go
fset := token.NewFileSet()
f, err := parser.ParseFile(fset, "input.go", srcBytes, 0)
```

### `reflect.StructTag` Parses Raw Tag Strings

The AST stores struct tags as raw Go string literals including the surrounding backtick characters. After stripping the backticks, you have the tag content. `reflect.StructTag(content).Get("json")` returns the value for the `json` key — or an empty string if absent. The value may contain comma-separated options like `omitempty`:

```go
raw := strings.Trim(field.Tag.Value, "`")
jsonVal := reflect.StructTag(raw).Get("json")
parts := strings.SplitN(jsonVal, ",", 2)
jsonName := parts[0]
omitempty := len(parts) > 1 && strings.Contains(parts[1], "omitempty")
```

### `text/template` and `FuncMap`

Register helper functions before parsing the template — function names are resolved at parse time, not at execution time:

```go
tmpl := template.Must(template.New("gen").Funcs(template.FuncMap{
	"lower": strings.ToLower,
}).Parse(templateText))
```

### Named Range Variables in Nested Loops

Inside `{{ range $s := .Structs }}`, `$s` holds the current `StructInfo`. If an inner `{{ range $s.Fields }}` changes the dot to each `FieldInfo`, you can still reference the struct name via `$s.Name`. This is the standard Go template idiom for multi-level range loops. Using `$.Name` fails when `$` is the root data and has no `Name` field.

### `go/format.Source` as a Validity Gate

`go/format.Source(src []byte) ([]byte, error)` reformats Go source and returns an error if the input is not syntactically valid Go. This makes it a free correctness check: if your template produces something that is not valid Go, `format.Source` tells you before you write a file. When it fails, log the raw template output alongside the error so you can locate the offending line.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/structgen/cmd/structgen
cd ~/go-exercises/structgen
go mod init example.com/structgen
```

### Exercise 1: Implement the library

Create `structgen.go`:

```go
package structgen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"text/template"
)

// StructInfo holds the parsed metadata for one struct type.
type StructInfo struct {
	Name   string
	Fields []FieldInfo
}

// FieldInfo holds metadata for one exported struct field.
type FieldInfo struct {
	Name     string
	Type     string
	JSONName string
	Required bool
}

// ParseStructs parses Go source bytes and returns metadata for each exported struct.
func ParseStructs(src []byte) ([]StructInfo, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "input.go", src, 0)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	var structs []StructInfo

	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		if !ast.IsExported(ts.Name.Name) {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}

		si := StructInfo{Name: ts.Name.Name}
		for _, field := range st.Fields.List {
			if len(field.Names) == 0 {
				continue
			}
			if !ast.IsExported(field.Names[0].Name) {
				continue
			}
			fi := FieldInfo{
				Name:     field.Names[0].Name,
				Type:     exprString(field.Type),
				Required: true,
			}
			if field.Tag != nil {
				raw := strings.Trim(field.Tag.Value, "`")
				jsonVal := reflect.StructTag(raw).Get("json")
				parts := strings.SplitN(jsonVal, ",", 2)
				fi.JSONName = parts[0]
				if len(parts) > 1 && strings.Contains(parts[1], "omitempty") {
					fi.Required = false
				}
			}
			if fi.JSONName == "" {
				fi.JSONName = strings.ToLower(fi.Name)
			}
			si.Fields = append(si.Fields, fi)
		}
		structs = append(structs, si)
		return true
	})

	return structs, nil
}

func requiredFields(fields []FieldInfo) []FieldInfo {
	var out []FieldInfo
	for _, f := range fields {
		if f.Required {
			out = append(out, f)
		}
	}
	return out
}

var funcMap = template.FuncMap{
	"lower":          strings.ToLower,
	"requiredFields": requiredFields,
}

// genTemplateText uses named range variables ($s for struct, $f for field)
// so the struct name remains accessible inside the nested field range.
const genTemplateText = `// Code generated by structgen; DO NOT EDIT.

package {{ .Package }}

import "fmt"
{{ range $s := .Structs }}
// New{{ $s.Name }} creates a new {{ $s.Name }} with required fields set.
func New{{ $s.Name }}({{ range $i, $f := requiredFields $s.Fields }}{{ if $i }}, {{ end }}{{ lower $f.Name }} {{ $f.Type }}{{ end }}) *{{ $s.Name }} {
	return &{{ $s.Name }}{
		{{- range $s.Fields }}{{ if .Required }}
		{{ .Name }}: {{ lower .Name }},{{ end }}{{ end }}
	}
}

// {{ $s.Name }}Builder provides a fluent API to build {{ $s.Name }}.
type {{ $s.Name }}Builder struct {
	v {{ $s.Name }}
}

// Build{{ $s.Name }} starts building a new {{ $s.Name }}.
func Build{{ $s.Name }}() *{{ $s.Name }}Builder {
	return &{{ $s.Name }}Builder{}
}
{{ range $s.Fields }}
// With{{ .Name }} sets the {{ .Name }} field.
func (b *{{ $s.Name }}Builder) With{{ .Name }}(val {{ .Type }}) *{{ $s.Name }}Builder {
	b.v.{{ .Name }} = val
	return b
}
{{ end }}
// Build returns the constructed {{ $s.Name }}.
func (b *{{ $s.Name }}Builder) Build() *{{ $s.Name }} {
	v := b.v
	return &v
}

// Validate checks that all required fields are set.
func (s *{{ $s.Name }}) Validate() error {
	var missing []string
	{{- range $s.Fields }}{{ if .Required }}{{ if eq .Type "string" }}
	if s.{{ .Name }} == "" {
		missing = append(missing, "{{ .JSONName }}")
	}{{ else if eq .Type "int" }}
	if s.{{ .Name }} == 0 {
		missing = append(missing, "{{ .JSONName }}")
	}{{ end }}{{ end }}{{ end }}
	if len(missing) > 0 {
		return fmt.Errorf("missing required fields: %v", missing)
	}
	return nil
}
{{ end }}`

var genTemplate = template.Must(template.New("structgen").Funcs(funcMap).Parse(genTemplateText))

type templateData struct {
	Package string
	Structs []StructInfo
}

// Generate renders Go source for the given package name and struct list.
// The returned bytes are gofmt-formatted.
func Generate(pkg string, structs []StructInfo) ([]byte, error) {
	data := templateData{Package: pkg, Structs: structs}
	var buf bytes.Buffer
	if err := genTemplate.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("template: %w", err)
	}
	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format: %w\nraw output:\n%s", err, buf.String())
	}
	return out, nil
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
		return "[]" + exprString(e.Elt)
	default:
		return fmt.Sprintf("%T", expr)
	}
}
```

The template uses `{{ range $s := .Structs }}` to name the outer variable. Inside `{{ range $s.Fields }}`, the dot changes to each `FieldInfo`, but `$s.Name` still refers to the current struct. This is the standard idiom for multi-level range in Go templates.

### Exercise 2: Write the tests

Create `structgen_test.go`:

```go
package structgen

import (
	"strings"
	"testing"
)

var sampleSrc = []byte(`package models

type Config struct {
	Host     string ` + "`" + `json:"host"` + "`" + `
	Port     int    ` + "`" + `json:"port"` + "`" + `
	Database string ` + "`" + `json:"database"` + "`" + `
	Timeout  int    ` + "`" + `json:"timeout,omitempty"` + "`" + `
	Debug    bool   ` + "`" + `json:"debug,omitempty"` + "`" + `
}

type Endpoint struct {
	Path   string ` + "`" + `json:"path"` + "`" + `
	Method string ` + "`" + `json:"method"` + "`" + `
	Auth   bool   ` + "`" + `json:"auth,omitempty"` + "`" + `
}
`)

func TestParseStructsFindsAllStructs(t *testing.T) {
	t.Parallel()

	structs, err := ParseStructs(sampleSrc)
	if err != nil {
		t.Fatal(err)
	}
	if len(structs) != 2 {
		t.Fatalf("got %d structs, want 2", len(structs))
	}
	if structs[0].Name != "Config" {
		t.Errorf("structs[0].Name = %q, want Config", structs[0].Name)
	}
	if structs[1].Name != "Endpoint" {
		t.Errorf("structs[1].Name = %q, want Endpoint", structs[1].Name)
	}
}

func TestParseStructsRequiredFields(t *testing.T) {
	t.Parallel()

	structs, err := ParseStructs(sampleSrc)
	if err != nil {
		t.Fatal(err)
	}

	cfg := structs[0]
	cases := []struct {
		name     string
		required bool
	}{
		{"Host", true},
		{"Port", true},
		{"Database", true},
		{"Timeout", false},
		{"Debug", false},
	}
	if len(cfg.Fields) != len(cases) {
		t.Fatalf("Config field count = %d, want %d", len(cfg.Fields), len(cases))
	}
	for i, tc := range cases {
		f := cfg.Fields[i]
		if f.Name != tc.name {
			t.Errorf("field[%d].Name = %q, want %q", i, f.Name, tc.name)
		}
		if f.Required != tc.required {
			t.Errorf("field %q required = %v, want %v", f.Name, f.Required, tc.required)
		}
	}
}

func TestParseStructsJSONNames(t *testing.T) {
	t.Parallel()

	structs, err := ParseStructs(sampleSrc)
	if err != nil {
		t.Fatal(err)
	}
	cfg := structs[0]
	want := []string{"host", "port", "database", "timeout", "debug"}
	for i, w := range want {
		if cfg.Fields[i].JSONName != w {
			t.Errorf("field[%d].JSONName = %q, want %q", i, cfg.Fields[i].JSONName, w)
		}
	}
}

func TestGenerateProducesValidGo(t *testing.T) {
	t.Parallel()

	structs, err := ParseStructs(sampleSrc)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Generate("models", structs)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("Generate returned empty output")
	}
}

func TestGenerateContainsCodeGeneratedHeader(t *testing.T) {
	t.Parallel()

	structs, err := ParseStructs(sampleSrc)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Generate("models", structs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "// Code generated by structgen; DO NOT EDIT.") {
		t.Errorf("output does not start with canonical generated header:\n%s", string(out[:minInt(200, len(out))]))
	}
}

func TestGenerateContainsConstructors(t *testing.T) {
	t.Parallel()

	structs, err := ParseStructs(sampleSrc)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Generate("models", structs)
	if err != nil {
		t.Fatal(err)
	}
	src := string(out)
	for _, want := range []string{
		"func NewConfig(",
		"func NewEndpoint(",
		"func BuildConfig()",
		"func BuildEndpoint()",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated output missing %q", want)
		}
	}
}

func TestGenerateValidateChecksRequired(t *testing.T) {
	t.Parallel()

	structs, err := ParseStructs(sampleSrc)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Generate("models", structs)
	if err != nil {
		t.Fatal(err)
	}
	src := string(out)
	if !strings.Contains(src, `"host"`) {
		t.Error("Validate should check host")
	}
	if strings.Contains(src, `"timeout"`) {
		t.Error("Validate should not check timeout (omitempty)")
	}
}

func TestParseStructsSyntaxError(t *testing.T) {
	t.Parallel()

	bad := []byte(`package pkg
func bad( {
`)
	_, err := ParseStructs(bad)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestParseStructsSkipsUnexported(t *testing.T) {
	t.Parallel()

	src := []byte(`package pkg

type exported struct {
	Field string
}

type hidden struct {
	Field string
}
`)
	structs, err := ParseStructs(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(structs) != 0 {
		t.Fatalf("expected 0 exported structs, got %d", len(structs))
	}
}

func TestGenerateWithNoRequiredFields(t *testing.T) {
	t.Parallel()

	src := []byte(`package pkg

type Optional struct {
	Name string ` + "`" + `json:"name,omitempty"` + "`" + `
}
`)
	structs, err := ParseStructs(src)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Generate("pkg", structs)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(string(out), "func NewOptional()") {
		t.Errorf("expected NewOptional() with no params:\n%s", string(out))
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
```

### Exercise 3: CLI driver

Create `cmd/structgen/main.go`:

```go
package main

import (
	"flag"
	"fmt"
	"os"

	"example.com/structgen"
)

func main() {
	input := flag.String("input", "", "input Go source file")
	output := flag.String("output", "", "output file (default: stdout)")
	pkg := flag.String("pkg", "main", "package name for generated file")
	flag.Parse()

	if *input == "" {
		fmt.Fprintln(os.Stderr, "usage: structgen -input=file.go [-output=out.go] [-pkg=name]")
		os.Exit(1)
	}

	src, err := os.ReadFile(*input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", *input, err)
		os.Exit(1)
	}

	structs, err := structgen.ParseStructs(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse: %v\n", err)
		os.Exit(1)
	}

	if len(structs) == 0 {
		fmt.Fprintln(os.Stderr, "no exported structs found")
		os.Exit(1)
	}

	out, err := structgen.Generate(*pkg, structs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate: %v\n", err)
		os.Exit(1)
	}

	if *output == "" {
		os.Stdout.Write(out)
		return
	}

	if err := os.WriteFile(*output, out, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *output, err)
		os.Exit(1)
	}
	fmt.Printf("Generated %s (%d struct(s))\n", *output, len(structs))
}
```

## Common Mistakes

### Registering template functions after parsing

Wrong: calling `template.New("x").Parse(text)` and then `tmpl.Funcs(...)` before `Execute`.

What happens: `text/template` resolves function names at parse time for pipeline calls like `{{ requiredFields .Fields }}`. If the function is not in the `FuncMap` when `Parse` is called, it panics with "function requiredFields not defined".

Fix: always call `Funcs(map)` before `Parse`:

```go
tmpl := template.Must(template.New("x").Funcs(funcMap).Parse(text))
```

### Using `$.FieldName` instead of a named range variable in nested loops

Wrong: inside `{{ range .Fields }}` (nested inside `{{ range .Structs }}`), using `$.Name` to access the struct name.

What happens: `$` is the root data (`templateData`), which has no `Name` field. Execution fails with "can't evaluate field Name in type yourpkg.templateData".

Fix: name the outer range variable and use it in the inner loop:

```
{{ range $s := .Structs }}
{{ range $s.Fields }}
func (b *{{ $s.Name }}Builder) ...
{{ end }}
{{ end }}
```

### Forgetting to strip backticks from tag values

Wrong: `reflect.StructTag(field.Tag.Value).Get("json")` when `field.Tag.Value` is `` `json:"name"` `` (with backticks).

What happens: `reflect.StructTag` receives an invalid tag string; `Get` always returns `""`.

Fix: `raw := strings.Trim(field.Tag.Value, "`")` before passing to `reflect.StructTag`.

### Using `format.Source` without reading the raw output on failure

Wrong: printing only the format error message when `format.Source` returns an error.

What happens: you see "1:5: expected ';' after statement" with no context about which statement it refers to.

Fix: when `format.Source` fails, also print the raw template output so you can find the offending line.

## Verification

Run from the module root:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must exit 0. Your turn: add `TestParseStructsFieldTypes` that verifies `Config.Port` has `Type == "int"` and `Config.Host` has `Type == "string"`.

## Summary

- `parser.ParseFile` accepts a `[]byte` source argument, enabling in-process parsing without touching the filesystem.
- `reflect.StructTag(raw).Get("json")` parses struct tag strings; strip the surrounding backticks from `field.Tag.Value` first.
- Register all `text/template` functions with `.Funcs(map)` before calling `.Parse(text)`; function lookup happens at parse time.
- Use `{{ range $s := .Structs }}` to name the outer range variable so nested loops can reference `$s.Name` when the dot changes.
- `go/format.Source` reformats and validates the generated output; on failure, log the raw template output to find the offending line.
- Splitting the generator into a testable library (`ParseStructs`, `Generate`) and a thin CLI driver lets you verify the core logic with table-driven in-process tests.

## What's Next

Next: [Build Constraints and File Suffixes](../06-build-constraints-and-file-suffixes/06-build-constraints-and-file-suffixes.md).

## Resources

- [go/parser package](https://pkg.go.dev/go/parser)
- [go/format package](https://pkg.go.dev/go/format)
- [text/template package](https://pkg.go.dev/text/template)
- [reflect.StructTag](https://pkg.go.dev/reflect#StructTag)
- [Go blog: Generating code](https://go.dev/blog/generate)
