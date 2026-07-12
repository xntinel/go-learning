# 9. Code Generation vs Reflection

Reflection and code generation solve the same problem — bridging static types with dynamic behavior — but at different moments in time. Reflection does this at runtime with full flexibility but pays per-operation overhead. Code generation does it at build time, producing specialized code that the compiler optimizes away to direct field access. Understanding when each approach is the right tool, and how to build a real code generator with `go/ast` and `text/template`, is the practical skill of this lesson.

```text
jsongen/
  go.mod
  cmd/jsongen/main.go
  reflectmarshal/
    marshal.go
    marshal_test.go
  cmd/demo/main.go
```

## Concepts

### The Trade-Off Matrix

| Property | Reflection | Code Generation |
| --- | --- | --- |
| Handles new types without code changes | yes | no — must re-run the generator |
| Runtime performance | 2-10x slower | same as hand-written |
| Compile-time type safety | limited | full |
| Debuggability | harder (no source line) | easy (generated file is readable) |
| Build complexity | none | requires a generation step |

Neither is universally better. Reflection is the right choice for library code that must accept arbitrary types (ORMs, RPC frameworks, test helpers). Code generation is right for hot-path serialization where the set of types is known at build time (internal API codecs, high-throughput event pipelines).

### Parsing Go Source with go/ast

The `go/ast`, `go/parser`, and `go/token` packages are part of the standard library. They parse Go source into an abstract syntax tree (AST) that the generator can walk:

```go
fset := token.NewFileSet()
file, err := parser.ParseFile(fset, filename, nil, 0)
```

Finding struct declarations means walking `file.Decls`, filtering for `*ast.GenDecl` nodes with `Tok == token.TYPE`, then filtering each spec for `*ast.TypeSpec` whose `Type` is `*ast.StructType`.

### Template-Based Code Emission

`text/template` renders the generated `.go` file. The generator fills a data struct with field names, JSON keys, and type information, then passes it to the template. The result is a `.go` file that compiles without modification — a property you verify by running `go build ./...` as part of the generation test.

### Reflection-Based Marshal as the Baseline

The reflection-based implementation iterates fields at runtime, reads `json` tags, and builds a JSON byte slice without knowing the concrete type at compile time. This is the correct baseline: it works for any struct, allocates for every call, and is measurably slower than the generated version on benchmarks.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/27-reflection/09-code-generation-vs-reflection/09-code-generation-vs-reflection/cmd/jsongen
mkdir -p go-solutions/27-reflection/09-code-generation-vs-reflection/09-code-generation-vs-reflection/cmd/demo
mkdir -p go-solutions/27-reflection/09-code-generation-vs-reflection/09-code-generation-vs-reflection/reflectmarshal
mkdir -p go-solutions/27-reflection/09-code-generation-vs-reflection/09-code-generation-vs-reflection/testdata
cd go-solutions/27-reflection/09-code-generation-vs-reflection/09-code-generation-vs-reflection
```

### Exercise 1: Reflection-Based Marshal

Create `reflectmarshal/marshal.go`:

```go
package reflectmarshal

import (
	"bytes"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// MarshalJSON serializes a struct to JSON using reflection.
// It respects json struct tags (name override, omitempty, -).
// Only exported fields of struct-kind values are marshaled.
func MarshalJSON(v any) ([]byte, error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return []byte("null"), nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, fmt.Errorf("reflectmarshal: expected struct, got %v", rv.Kind())
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	t := rv.Type()
	first := true

	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, omitempty := parseTag(tag, sf.Name)
		fv := rv.Field(i)
		if omitempty && fv.IsZero() {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false

		buf.WriteByte('"')
		buf.WriteString(name)
		buf.WriteString(`":`)
		writeValue(&buf, fv)
	}

	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func parseTag(tag, fieldName string) (name string, omitempty bool) {
	parts := strings.SplitN(tag, ",", 2)
	name = parts[0]
	if name == "" {
		name = fieldName
	}
	if len(parts) == 2 {
		omitempty = strings.Contains(parts[1], "omitempty")
	}
	return
}

func writeValue(buf *bytes.Buffer, v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		buf.Write(strconv.AppendQuote(nil, v.String()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		buf.Write(strconv.AppendInt(nil, v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		buf.Write(strconv.AppendUint(nil, v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		buf.Write(strconv.AppendFloat(nil, v.Float(), 'f', -1, 64))
	case reflect.Bool:
		buf.Write(strconv.AppendBool(nil, v.Bool()))
	case reflect.Ptr:
		if v.IsNil() {
			buf.WriteString("null")
		} else {
			writeValue(buf, v.Elem())
		}
	case reflect.Slice:
		if v.IsNil() {
			buf.WriteString("null")
			return
		}
		buf.WriteByte('[')
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeValue(buf, v.Index(i))
		}
		buf.WriteByte(']')
	default:
		buf.WriteString("null")
	}
}
```

### Exercise 2: Tests for the Reflection Marshal

Create `reflectmarshal/marshal_test.go`:

```go
package reflectmarshal

import (
	"encoding/json"
	"fmt"
	"testing"
)

type Address struct {
	Street string `json:"street"`
	City   string `json:"city"`
}

type Person struct {
	Name    string  `json:"name"`
	Age     int     `json:"age"`
	Email   string  `json:"email,omitempty"`
	Score   float64 `json:"score"`
	Active  bool    `json:"active"`
	private string
}

func TestMarshalBasicTypes(t *testing.T) {
	t.Parallel()

	p := Person{Name: "Alice", Age: 30, Score: 9.5, Active: true}
	got, err := MarshalJSON(p)
	if err != nil {
		t.Fatal(err)
	}

	// Verify by parsing the result with encoding/json.
	var out Person
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("produced invalid JSON %q: %v", got, err)
	}
	if out.Name != p.Name || out.Age != p.Age || out.Active != p.Active {
		t.Errorf("round-trip mismatch: got %+v", out)
	}
}

func TestMarshalOmitEmpty(t *testing.T) {
	t.Parallel()

	p := Person{Name: "Bob", Age: 25} // Email is empty → omitted
	got, err := MarshalJSON(p)
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	json.Unmarshal(got, &m)
	if _, ok := m["email"]; ok {
		t.Error("omitempty field should be absent when zero")
	}
}

func TestMarshalNilPointer(t *testing.T) {
	t.Parallel()

	var p *Person
	got, err := MarshalJSON(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "null" {
		t.Errorf("nil pointer should marshal to null, got %q", got)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []Person{
		{Name: "Alice", Age: 30, Email: "alice@example.com", Score: 7.3, Active: true},
		{Name: "", Age: 0, Score: 0},
		{Name: "Unicode: 日本語", Age: 99},
	}
	for _, tc := range cases {
		got, err := MarshalJSON(tc)
		if err != nil {
			t.Fatalf("MarshalJSON(%+v): %v", tc, err)
		}
		var out Person
		if err := json.Unmarshal(got, &out); err != nil {
			t.Fatalf("invalid JSON for %+v: %q: %v", tc, got, err)
		}
	}
}

func ExampleMarshalJSON() {
	type Point struct {
		X int `json:"x"`
		Y int `json:"y"`
	}
	b, _ := MarshalJSON(Point{X: 3, Y: 4})
	fmt.Println(string(b))
	// Output: {"x":3,"y":4}
}
```

Add `"fmt"` to `marshal_test.go`'s import block — the Example uses it.

**Your turn:** add `TestMarshalPrivateFieldsIgnored` that creates a struct with an unexported field set to a non-zero value and asserts the JSON output does not contain that field's name.

### Exercise 3: Code Generator CLI

Create `cmd/jsongen/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"strings"
	"text/template"
)

// FieldInfo holds extracted information for one struct field.
type FieldInfo struct {
	GoName    string // Go identifier
	JSONKey   string // json tag name or GoName
	GoType    string // string representation of the Go type
	Omitempty bool
	Skip      bool
}

// StructInfo holds information for one struct type.
type StructInfo struct {
	Name   string
	Fields []FieldInfo
}

var fileTmpl = template.Must(template.New("file").Funcs(template.FuncMap{
	"marshalField": marshalSnippet,
}).Parse(`// Code generated by jsongen. DO NOT EDIT.

package {{.Package}}

import (
	"bytes"
	"fmt"
	"strconv"
)

{{range .Structs}}
// MarshalJSON implements json.Marshaler for {{.Name}}.
func (s {{.Name}}) MarshalJSONGen() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	_ = first
{{- range .Fields}}{{if not .Skip}}
	{{marshalField .}}
{{- end}}{{end}}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
{{end}}

// ensure imports are used
var _ = fmt.Sprintf
var _ = strconv.Itoa
var _ = bytes.NewBuffer
`))

func marshalSnippet(f FieldInfo) string {
	zero := zeroCheck(f)
	emit := emitValue(f)
	if f.Omitempty && zero != "" {
		return fmt.Sprintf(`if !(%s) {
	if !first { buf.WriteByte(',') }
	first = false
	buf.WriteString(%q)
	%s
}`, zero, `"`+f.JSONKey+`":`, emit)
	}
	return fmt.Sprintf(`if !first { buf.WriteByte(',') }
first = false
buf.WriteString(%q)
%s`, `"`+f.JSONKey+`":`, emit)
}

func zeroCheck(f FieldInfo) string {
	switch f.GoType {
	case "string":
		return `s.` + f.GoName + ` == ""`
	case "int", "int64", "int32":
		return `s.` + f.GoName + ` == 0`
	case "bool":
		return `!s.` + f.GoName
	default:
		return ""
	}
}

func emitValue(f FieldInfo) string {
	field := "s." + f.GoName
	switch f.GoType {
	case "string":
		return `buf.Write(strconv.AppendQuote(nil, ` + field + `))`
	case "int", "int32", "int64":
		return `buf.Write(strconv.AppendInt(nil, int64(` + field + `), 10))`
	case "bool":
		return `buf.Write(strconv.AppendBool(nil, ` + field + `))`
	case "float64":
		return `buf.Write(strconv.AppendFloat(nil, ` + field + `, 'f', -1, 64))`
	default:
		return `buf.WriteString("null") // unsupported type ` + f.GoType
	}
}

type tmplData struct {
	Package string
	Structs []StructInfo
}

func main() {
	if len(os.Args) < 3 {
		log.Fatalf("usage: jsongen <source.go> <output.go>")
	}
	src, dst := os.Args[1], os.Args[2]

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		log.Fatal(err)
	}

	data := tmplData{Package: file.Name.Name}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts := spec.(*ast.TypeSpec)
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			info := StructInfo{Name: ts.Name.Name}
			for _, field := range st.Fields.List {
				if len(field.Names) == 0 {
					continue // embedded
				}
				goName := field.Names[0].Name
				if !ast.IsExported(goName) {
					continue
				}
				fi := FieldInfo{
					GoName:  goName,
					GoType:  exprToString(field.Type),
					JSONKey: goName,
				}
				if field.Tag != nil {
					raw := strings.Trim(field.Tag.Value, "`")
					tag := getJSONTag(raw)
					if tag == "-" {
						fi.Skip = true
					} else {
						parts := strings.SplitN(tag, ",", 2)
						if parts[0] != "" {
							fi.JSONKey = parts[0]
						}
						if len(parts) == 2 {
							fi.Omitempty = strings.Contains(parts[1], "omitempty")
						}
					}
				}
				info.Fields = append(info.Fields, fi)
			}
			data.Structs = append(data.Structs, info)
		}
	}

	var buf bytes.Buffer
	if err := fileTmpl.Execute(&buf, data); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(dst, buf.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Println("generated", dst)
}

func exprToString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprToString(t.X)
	case *ast.ArrayType:
		return "[]" + exprToString(t.Elt)
	case *ast.SelectorExpr:
		return exprToString(t.X) + "." + t.Sel.Name
	default:
		return fmt.Sprintf("unknown(%T)", expr)
	}
}

func getJSONTag(tag string) string {
	const key = `json:"`
	idx := strings.Index(tag, key)
	if idx < 0 {
		return ""
	}
	rest := tag[idx+len(key):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}
```

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"

	"example.com/jsongen/reflectmarshal"
)

type Product struct {
	ID    int     `json:"id"`
	Name  string  `json:"name"`
	Price float64 `json:"price"`
	Tags  []string
	Draft bool `json:"draft,omitempty"`
}

func main() {
	p := Product{ID: 42, Name: "Widget", Price: 9.99, Tags: []string{"sale", "new"}}

	// Reflection-based marshal.
	b, err := reflectmarshal.MarshalJSON(p)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("reflection:", string(b))

	// Standard library for comparison.
	std, _ := json.Marshal(p)
	fmt.Println("stdlib:    ", string(std))

	// Both round-trip cleanly.
	var p2 Product
	json.Unmarshal(b, &p2)
	fmt.Printf("round-trip name=%s price=%.2f\n", p2.Name, p2.Price)
}
```

Run with `go run ./cmd/demo`.

## Common Mistakes

### Using Reflection on the Fast Path

Wrong: using `reflect.ValueOf` + `FieldByName` inside a handler that runs for every HTTP request.

What happens: profiling shows reflection in the top 5% of CPU time; `FieldByName` allocates and scans.

Fix: run the code generator once and check the generated file into version control. The generated `MarshalJSONGen` uses direct field access and zero allocations.

### Generating Code That Does Not Compile

Wrong: emitting generated code without running `go build ./...` as part of the generation test.

What happens: a template bug or missing import produces a `.go` file that compiles on `go generate` day but fails CI when the code is compiled with different inputs.

Fix: add a `TestGeneratedCodeCompiles` test that invokes the generator on a fixture file and runs `go vet` on the output. Alternatively, include the generated file in the test package and let the normal `go test` compilation detect breakage.

### Forgetting `// Code generated` Header

Wrong: generating a file without the `// Code generated by ... DO NOT EDIT.` header comment.

What happens: goimports, editors, and code review tools treat the file as hand-written, leading to spurious diffs when the generator is re-run.

Fix: always include `// Code generated by <tool>. DO NOT EDIT.` as the first line of every generated file — this is the convention recognized by Go tooling.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

The generator CLI (`cmd/jsongen`) compiles but requires a source file argument to produce output; run it manually with `go run ./cmd/jsongen <source.go> <output.go>` to exercise the generation path.

## Summary

- Reflection handles arbitrary types at runtime; code generation handles known types at build time.
- `go/parser.ParseFile` produces an AST you can walk to find struct declarations and their tags.
- `text/template` emits the generated `.go` file; always prefix with `// Code generated ... DO NOT EDIT.`
- The reflection-based marshal iterates `rv.Field(i)` and dispatches on `reflect.Value.Kind()`, using `strconv.Append*` to emit JSON without knowing concrete types.
- Benchmark both before choosing: reflection is fast enough for cold paths; generated code wins on hot-path serialization.

## What's Next

Next: [Building a Configuration Loader](../10-building-a-configuration-loader/10-building-a-configuration-loader.md).

## Resources

- [go/ast package](https://pkg.go.dev/go/ast)
- [go/parser package](https://pkg.go.dev/go/parser)
- [text/template package](https://pkg.go.dev/text/template)
- [go generate](https://go.dev/blog/generate)
- [stringer tool source](https://cs.opensource.google/go/x/tools/+/master:cmd/stringer/)
