# Exercise 4: Pretty-Printing Interpreter Values

The last standalone primitive is the value formatter. When the evaluator returns a result, the REPL has to render it for a human: integers in one color, strings quoted and in another, errors in red, large arrays broken across lines. This exercise builds that formatter against a minimal `Object` value type and a single entry point, `Pretty`, with a companion `StripANSI` that removes the color codes for non-terminal output and for string-comparison tests. Like the previous three modules it is pure, terminal-free logic, which is exactly why its output can be pinned by tests.

The `Object` here is intentionally minimal — a tagged union with one field per value kind. A real Monkey interpreter has its own `object.Object` interface; the REPL would format that through a thin adapter that fills in this struct, or the pretty-printer would be generalized to the real type. Keeping a small concrete `Object` lets this module be built and tested on its own without dragging in the whole evaluator.

## What you'll build

```text
pretty.go          Object tagged union, Pretty, StripANSI, ANSI color helpers
cmd/
  demo/
    main.go        format an int, an array, a function, and an error (color-stripped)
pretty_test.go     each value type, color stripping, sorted-hash determinism
```

- Files: `pretty.go`, `cmd/demo/main.go`, `pretty_test.go`.
- Implement: `ObjectType` constants, `Object`, `Pretty`, `StripANSI`, and the internal color helpers.
- Test: `pretty_test.go` asserts on `StripANSI(Pretty(...))` for every value kind, checks that hash keys are sorted for deterministic output, and that the array inline/multi-line threshold works.
- Verify: `go test -race ./...`

### One entry point, color injected inline

`Pretty(*Object) string` switches on the object's type and returns a colored string. Color is injected inline with ANSI SGR (Select Graphic Rendition) escape sequences: a code like `\x1b[33m` turns the following text yellow, and every colored span is closed with `\x1b[0m` (reset) so the color does not bleed past it. The palette follows what most REPLs converge on — integers and floats yellow, strings green, booleans cyan, functions blue, errors red, null gray — and a `nil` object is treated as null so the caller never has to guard against it.

Two formatting refinements matter. Arrays and hashes render inline when short but switch to an indented multi-line layout once the inline form exceeds 60 *visible* characters — visible meaning the width after the color codes are stripped, which is why width is measured with `StripANSI` rather than `len`. And hash keys are sorted before rendering, so the same hash always prints in the same order regardless of Go's randomized map iteration; without that sort the output would be non-deterministic and untestable.

`StripANSI` removes every escape sequence from a string. It is exported because it serves two callers: the REPL's no-color mode pipes `Pretty` output through it when writing to a non-terminal, and tests pipe `Pretty` output through it to compare against plain expected strings. Internally `Pretty` uses it too, to measure the display width that decides the inline-versus-multi-line threshold.

Create `pretty.go`:

```go
package pretty

import (
	"fmt"
	"sort"
	"strings"
)

// ObjectType identifies the kind of an interpreter value.
type ObjectType int

const (
	TypeNull   ObjectType = iota
	TypeInt               // int64
	TypeFloat             // float64
	TypeBool              // bool
	TypeString            // string
	TypeArray             // []*Object
	TypeHash              // map[string]*Object, sorted by key
	TypeFn                // function: Params []string
	TypeError             // runtime error: ErrMsg string
)

// Object is a tagged union representing one interpreter value.
type Object struct {
	Type     ObjectType
	IntVal   int64
	FloatVal float64
	BoolVal  bool
	StrVal   string
	Elements []*Object
	Pairs    map[string]*Object
	Params   []string
	ErrMsg   string
}

// ANSI SGR color codes.
const (
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiCyan   = "\x1b[36m"
	ansiGray   = "\x1b[90m"
	ansiReset  = "\x1b[0m"
)

func colorize(code, s string) string { return code + s + ansiReset }

// Pretty returns an ANSI-colored, human-readable representation of obj.
// Arrays with more than 60 characters of inline content switch to multi-line.
// Hash keys are sorted. nil is treated as TypeNull.
func Pretty(obj *Object) string {
	if obj == nil {
		return colorize(ansiGray, "null")
	}
	switch obj.Type {
	case TypeNull:
		return colorize(ansiGray, "null")
	case TypeInt:
		return colorize(ansiYellow, fmt.Sprintf("%d", obj.IntVal))
	case TypeFloat:
		return colorize(ansiYellow, fmt.Sprintf("%g", obj.FloatVal))
	case TypeBool:
		if obj.BoolVal {
			return colorize(ansiCyan, "true")
		}
		return colorize(ansiCyan, "false")
	case TypeString:
		return colorize(ansiGreen, fmt.Sprintf("%q", obj.StrVal))
	case TypeArray:
		return prettyArray(obj.Elements)
	case TypeHash:
		return prettyHash(obj.Pairs)
	case TypeFn:
		params := strings.Join(obj.Params, ", ")
		return colorize(ansiBlue, fmt.Sprintf("fn(%s) { ... }", params))
	case TypeError:
		return colorize(ansiRed, "ERROR: "+obj.ErrMsg)
	default:
		return colorize(ansiGray, "<unknown>")
	}
}

func prettyArray(elems []*Object) string {
	if len(elems) == 0 {
		return "[]"
	}
	parts := make([]string, len(elems))
	for i, e := range elems {
		parts[i] = Pretty(e)
	}
	inline := strings.Join(parts, ", ")
	if len(StripANSI(inline)) <= 60 {
		return "[" + inline + "]"
	}
	var sb strings.Builder
	sb.WriteString("[\n")
	for _, p := range parts {
		sb.WriteString("  ")
		sb.WriteString(p)
		sb.WriteString(",\n")
	}
	sb.WriteString("]")
	return sb.String()
}

func prettyHash(pairs map[string]*Object) string {
	if len(pairs) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = colorize(ansiGreen, fmt.Sprintf("%q", k)) + ": " + Pretty(pairs[k])
	}
	inline := strings.Join(parts, ", ")
	if len(StripANSI(inline)) <= 60 {
		return "{" + inline + "}"
	}
	var sb strings.Builder
	sb.WriteString("{\n")
	for _, p := range parts {
		sb.WriteString("  ")
		sb.WriteString(p)
		sb.WriteString(",\n")
	}
	sb.WriteString("}")
	return sb.String()
}

// StripANSI removes ANSI SGR escape sequences from s. Used to measure display
// width, by the REPL's no-color mode, and by tests comparing plain strings.
func StripANSI(s string) string {
	var out strings.Builder
	inEsc := false
	for _, c := range s {
		switch {
		case inEsc:
			if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
				inEsc = false
			}
		case c == '\x1b':
			inEsc = true
		default:
			out.WriteRune(c)
		}
	}
	return out.String()
}
```

`StripANSI` is a tiny state machine: outside an escape it copies runes through; on seeing `\x1b` it flips into skip mode and stays there until a letter terminates the sequence. That handles every SGR code `Pretty` emits without a regular expression. Note that `Pretty` recurses through arrays and hashes, so a nested array of strings is fully colored element by element, and the width check still measures only visible characters.

### The runnable demo

The demo formats one value of each interesting kind and prints the color-stripped form so the output is plain, deterministic text. A real terminal would show the colors; stripping them here keeps the documented output exact.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pretty"
)

func main() {
	show := func(label string, obj *pretty.Object) {
		fmt.Printf("%-7s %s\n", label, pretty.StripANSI(pretty.Pretty(obj)))
	}

	show("int:", &pretty.Object{Type: pretty.TypeInt, IntVal: 42})
	show("string:", &pretty.Object{Type: pretty.TypeString, StrVal: "hi"})
	show("bool:", &pretty.Object{Type: pretty.TypeBool, BoolVal: true})
	show("array:", &pretty.Object{Type: pretty.TypeArray, Elements: []*pretty.Object{
		{Type: pretty.TypeInt, IntVal: 1},
		{Type: pretty.TypeString, StrVal: "two"},
		{Type: pretty.TypeBool, BoolVal: false},
	}})
	show("fn:", &pretty.Object{Type: pretty.TypeFn, Params: []string{"x", "y"}})
	show("error:", &pretty.Object{Type: pretty.TypeError, ErrMsg: "undefined: x"})
	show("null:", nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
int:    42
string: "hi"
bool:   true
array:  [1, "two", false]
fn:     fn(x, y) { ... }
error:  ERROR: undefined: x
null:   null
```

### Tests

The tests assert on `StripANSI(Pretty(...))` for each value kind, so they verify the rendered text without depending on the exact escape bytes. The hash test builds an object with two pairs and checks both keys appear and that the sorted order is deterministic; the array test checks the inline form. The `Example` functions are checked by `go test` against their `// Output:` comments.

Create `pretty_test.go`:

```go
package pretty

import (
	"fmt"
	"strings"
	"testing"
)

func TestPrettyInt(t *testing.T) {
	t.Parallel()
	if got := StripANSI(Pretty(&Object{Type: TypeInt, IntVal: 42})); got != "42" {
		t.Errorf("Pretty(42) = %q, want %q", got, "42")
	}
}

func TestPrettyFloat(t *testing.T) {
	t.Parallel()
	if got := StripANSI(Pretty(&Object{Type: TypeFloat, FloatVal: 3.14})); got != "3.14" {
		t.Errorf("Pretty(3.14) = %q, want %q", got, "3.14")
	}
}

func TestPrettyBool(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		val  bool
		want string
	}{{true, "true"}, {false, "false"}} {
		got := StripANSI(Pretty(&Object{Type: TypeBool, BoolVal: tc.val}))
		if got != tc.want {
			t.Errorf("Pretty(%v) = %q, want %q", tc.val, got, tc.want)
		}
	}
}

func TestPrettyString(t *testing.T) {
	t.Parallel()
	if got := StripANSI(Pretty(&Object{Type: TypeString, StrVal: "hello"})); got != `"hello"` {
		t.Errorf("Pretty(string) = %q, want %q", got, `"hello"`)
	}
}

func TestPrettyNull(t *testing.T) {
	t.Parallel()
	if got := StripANSI(Pretty(nil)); got != "null" {
		t.Errorf("Pretty(nil) = %q, want %q", got, "null")
	}
}

func TestPrettyArray(t *testing.T) {
	t.Parallel()
	obj := &Object{
		Type: TypeArray,
		Elements: []*Object{
			{Type: TypeInt, IntVal: 1},
			{Type: TypeInt, IntVal: 2},
			{Type: TypeInt, IntVal: 3},
		},
	}
	if got := StripANSI(Pretty(obj)); got != "[1, 2, 3]" {
		t.Errorf("Pretty(array) = %q, want %q", got, "[1, 2, 3]")
	}
}

func TestPrettyEmptyArray(t *testing.T) {
	t.Parallel()
	if got := StripANSI(Pretty(&Object{Type: TypeArray})); got != "[]" {
		t.Errorf("Pretty(empty array) = %q, want %q", got, "[]")
	}
}

func TestPrettyError(t *testing.T) {
	t.Parallel()
	got := StripANSI(Pretty(&Object{Type: TypeError, ErrMsg: "undefined: x"}))
	if !strings.Contains(got, "undefined: x") {
		t.Errorf("Pretty(error) = %q", got)
	}
}

func TestPrettyFn(t *testing.T) {
	t.Parallel()
	got := StripANSI(Pretty(&Object{Type: TypeFn, Params: []string{"x", "y"}}))
	if got != "fn(x, y) { ... }" {
		t.Errorf("Pretty(fn) = %q, want %q", got, "fn(x, y) { ... }")
	}
}

func TestPrettyHash(t *testing.T) {
	t.Parallel()
	obj := &Object{
		Type: TypeHash,
		Pairs: map[string]*Object{
			"name": {Type: TypeString, StrVal: "monkey"},
			"age":  {Type: TypeInt, IntVal: 1},
		},
	}
	// Keys are sorted, so "age" precedes "name" deterministically.
	got := StripANSI(Pretty(obj))
	if got != `{"age": 1, "name": "monkey"}` {
		t.Errorf("Pretty(hash) = %q, want sorted keys", got)
	}
}

func TestStripANSI(t *testing.T) {
	t.Parallel()
	colored := "\x1b[33m42\x1b[0m"
	if got := StripANSI(colored); got != "42" {
		t.Errorf("StripANSI = %q, want %q", got, "42")
	}
}

func ExamplePretty() {
	fmt.Println(StripANSI(Pretty(&Object{Type: TypeInt, IntVal: 7})))
	fmt.Println(StripANSI(Pretty(&Object{Type: TypeString, StrVal: "ok"})))
	// Output:
	// 7
	// "ok"
}
```

## Review

The formatter is correct when every value kind renders to the documented plain text after stripping color, and when structured values are deterministic. Integers, floats, booleans, quoted strings, functions, errors, and null each have a fixed rendering; an empty array is `[]` and an empty hash `{}`; a populated hash prints its keys in sorted order so the output never depends on map iteration order. The width threshold that switches arrays to multi-line is measured on the color-stripped string, so embedded escape codes never inflate the count. Verifying through `StripANSI(Pretty(...))` rather than eyeballing a comment is what keeps these assertions honest: change the palette and the tests still pass, change the text and they fail.

Common mistakes for this module. Measuring width with `len` on the colored string counts the escape bytes and trips the multi-line threshold far too early; measure the stripped form. Skipping the key sort in `prettyHash` makes the output depend on Go's randomized map order, so the test passes locally and fails in CI. And forgetting the reset code after a colored span lets the color bleed into everything printed afterward, including the next prompt.

## Resources

- [ANSI/VT100 escape sequences](https://vt100.net/docs/vt100-ug/chapter3.html) — the SGR color codes (`\x1b[31m` ... `\x1b[0m`) injected by `Pretty`.
- [pkg.go.dev/fmt#Sprintf](https://pkg.go.dev/fmt#Sprintf) — the `%q` verb that quotes and escapes strings, and `%g` for floats.
- [Thorsten Ball, Writing An Interpreter In Go](https://interpreterbook.com/) — Chapter 4 defines the `object.Object` types this formatter mirrors.

---

Back to [03-tab-completion.md](03-tab-completion.md) | Next: [05-interactive-repl.md](05-interactive-repl.md)
