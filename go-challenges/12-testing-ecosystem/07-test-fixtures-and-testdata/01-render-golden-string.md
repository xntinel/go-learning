# Exercise 1: Golden-string test for a markdown renderer (inline fixtures)

A golden test asserts a function's output against a known-good reference value. Before a fixture ever moves into a file, it lives as an inline constant next to the test. This module builds a small markdown renderer and pins its output against inline golden strings, establishing exactly what a fixture and a golden value are.

## What you'll build

```text
render/                       independent module: example.com/goldenstring
  go.mod                      go 1.26
  markdown.go                 Render(input) (string, error); ErrEmpty sentinel
  cmd/
    demo/
      main.go                 renders a sample document and prints the HTML
  markdown_test.go            inline golden constants; empty-input error; multi-heading contract
```

Files: `markdown.go`, `cmd/demo/main.go`, `markdown_test.go`.
Implement: a `Render` function converting a markdown subset (`# Heading`, `*em*`) with an `ErrEmpty` sentinel returned for blank input.
Test: compare rendered output against inline golden constants, assert the empty-input path with `errors.Is`, and pin multi-heading behavior.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/07-test-fixtures-and-testdata/01-render-golden-string/cmd/demo
cd go-solutions/12-testing-ecosystem/07-test-fixtures-and-testdata/01-render-golden-string
```

### Why start with an inline golden

A fixture is any externalized test input or expected output; a golden value is specifically the expected output a function is asserted against. The cheapest form of a golden is a `const` string sitting beside the test. It costs nothing to read, it lives in the same file as the assertion, and a reviewer sees the exact contract in one place. This is the right form when the expected output is a line or two.

It stops scaling the moment the output grows: a fifty-line HTML document embedded as a `"\n"`-spliced string literal is unreadable, and every intentional change forces a manual edit of a brittle literal. The later exercises move the golden into `testdata/` files precisely to escape that. But the mechanics are identical in both forms — render the input, compare bytes against a reference, fail with both sides printed — so the inline version is where the pattern is learned.

The renderer itself is a real (if small) production shape: a transform with a sentinel error for the one invalid input. `Render` rejects blank input with `ErrEmpty` (wrapped-friendly, asserted via `errors.Is`), converts `# Heading` lines to `<h1>Heading</h1>`, and rewrites `*text*` spans to `<em>text</em>` on every other line. Trailing newlines are trimmed so the golden has no ragged whitespace tail — the single most common cause of a golden that fails for a trivial reason.

Create `markdown.go`:

```go
package render

import (
	"errors"
	"regexp"
	"strings"
)

// ErrEmpty is returned by Render when the input is blank after trimming.
var ErrEmpty = errors.New("empty markdown")

var (
	h1 = regexp.MustCompile(`^# (.+)$`)
	em = regexp.MustCompile(`\*([^*]+)\*`)
)

// Render converts a tiny markdown subset to HTML: "# Heading" becomes an
// <h1> element and "*text*" spans become <em>. Blank input returns ErrEmpty.
func Render(input string) (string, error) {
	if strings.TrimSpace(input) == "" {
		return "", ErrEmpty
	}
	var out strings.Builder
	for _, line := range strings.Split(input, "\n") {
		if m := h1.FindStringSubmatch(line); m != nil {
			out.WriteString("<h1>")
			out.WriteString(m[1])
			out.WriteString("</h1>\n")
			continue
		}
		out.WriteString(em.ReplaceAllString(line, "<em>$1</em>"))
		out.WriteString("\n")
	}
	return strings.TrimRight(out.String(), "\n"), nil
}
```

### The runnable demo

The demo renders a two-line document with a heading and an emphasis span so you can see the exact HTML the golden must match.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/goldenstring"
)

func main() {
	html, err := render.Render("# Release Notes\nShipped the *cache* rewrite.\n")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(html)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
<h1>Release Notes</h1>
Shipped the <em>cache</em> rewrite.
```

### The test

`helloInput` and `helloGolden` are the fixture pair: the input document and its expected HTML. `TestRenderFromFixture` renders and compares, printing both sides on mismatch so a failure is diagnosable. `TestRenderRejectsEmpty` asserts the sentinel with `errors.Is` — the correct way to check a sentinel, since a caller may wrap it with `%w`. `TestRenderMultipleHeadings` pins the multi-heading contract so a regex or loop change that collapses headings is caught. The `Example` documents the API and is verified by `go test` against its `// Output:` line.

Create `markdown_test.go`:

```go
package render

import (
	"errors"
	"fmt"
	"testing"
)

const (
	helloInput  = "# Hello\nThis is *markdown*.\n"
	helloGolden = "<h1>Hello</h1>\nThis is <em>markdown</em>."

	multiInput  = "# One\n# Two\nbody *x*\n"
	multiGolden = "<h1>One</h1>\n<h1>Two</h1>\nbody <em>x</em>"
)

func TestRenderFromFixture(t *testing.T) {
	t.Parallel()

	got, err := Render(helloInput)
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	if got != helloGolden {
		t.Fatalf("Render mismatch:\ngot:\n%s\nwant:\n%s", got, helloGolden)
	}
}

func TestRenderRejectsEmpty(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"", "   ", "\n\t\n"} {
		if _, err := Render(in); !errors.Is(err, ErrEmpty) {
			t.Fatalf("Render(%q) err = %v, want ErrEmpty", in, err)
		}
	}
}

func TestRenderMultipleHeadings(t *testing.T) {
	t.Parallel()

	got, err := Render(multiInput)
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	if got != multiGolden {
		t.Fatalf("Render mismatch:\ngot:\n%s\nwant:\n%s", got, multiGolden)
	}
}

func ExampleRender() {
	html, _ := Render("# Title\nplain *word* here\n")
	fmt.Println(html)
	// Output:
	// <h1>Title</h1>
	// plain <em>word</em> here
}
```

## Review

The renderer is correct when its output is a pure function of the input: the same document always produces the same HTML, so an inline golden is a stable contract. The two traps this exercise inoculates against are structural. First, do not hardcode a sprawling expected string inside the assertion body — name it as a `const` so the intent reads clearly and a change touches one place. Second, keep the golden free of a trailing-newline tail; `Render` trims it deliberately, and a golden that carries one would fail on the next run for a reason unrelated to the logic. Assert the sentinel with `errors.Is`, never `==`, so a wrapped error still matches.

## Resources

- [testing package](https://pkg.go.dev/testing) — `T.Parallel`, `T.Fatalf`, and testable `Example` functions with `// Output:`.
- [regexp package](https://pkg.go.dev/regexp) — `MustCompile`, `FindStringSubmatch`, and `ReplaceAllString`.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a sentinel through wrapping.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-load-fixture-from-testdata.md](02-load-fixture-from-testdata.md)
