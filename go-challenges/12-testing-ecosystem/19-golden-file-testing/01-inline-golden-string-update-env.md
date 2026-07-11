# Exercise 1: Inline Golden String with an UPDATE Env Var

The simplest golden test keeps the expected output as a constant in the test
file and prints the actual output on demand so you can paste it back in. You
build a small markdown renderer and pin its HTML output against an inline golden
string, with an `UPDATE=1` env var to regenerate it.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
mdrender/                  independent module: example.com/mdrender
  go.mod                   go 1.26
  markdown.go              Render(subset) -> HTML; ErrEmpty sentinel
  cmd/
    demo/
      main.go              renders a small document and prints the HTML
  markdown_test.go         inline golden compare, UPDATE=1 support, round-trip
```

Files: `markdown.go`, `cmd/demo/main.go`, `markdown_test.go`.
Implement: `Render(input string) (string, error)` converting a markdown subset (`# H` to `<h1>H</h1>`, `*x*` to `<em>x</em>`), returning `ErrEmpty` on blank input.
Test: compare `Render` output to an inline `helloGolden` constant, log the actual output and return early when `UPDATE=1`, reject empty input via `errors.Is`, and prove the update round-trip is deterministic and green.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/mdrender/cmd/demo
cd ~/go-exercises/mdrender
go mod init example.com/mdrender
```

### Why start with an inline string

An inline golden string is the whole technique in miniature, and its limits are
what motivate everything that follows. The expected output is a `const` right
next to the assertion, so a reviewer sees both the code and its expected output
in one screen, and there is no `testdata/` file to manage. The `UPDATE=1` env
var is the regeneration mechanism: instead of hand-editing the string when the
renderer legitimately changes, you run the test with the variable set, it prints
the actual output, and you paste that back into the constant. That paste is the
reviewed step — you read what changed before committing it.

The pattern does not scale. One small HTML fragment fits in a constant; a
multi-kilobyte JSON body or a rendered email does not, and a string literal with
embedded newlines and quotes becomes unreadable and un-diffable. That is exactly
why the next exercise moves the expectation to an on-disk `testdata/*.golden`
file. But the discipline is identical at both scales: the golden is the reviewed
source of truth, and regeneration is a deliberate, audited act.

`Render` is deterministic — a pure function of its input string with no clock, no
map iteration, no randomness — which is the precondition for any golden test. It
splits the input into lines, rewrites a leading `# ` into an `<h1>` element, and
rewrites `*text*` spans into `<em>` elements on every other line, then trims the
trailing newline so the output has a single stable shape. Blank input is a real
error, surfaced through the `ErrEmpty` sentinel so callers can match it with
`errors.Is`.

Create `markdown.go`:

```go
package mdrender

import (
	"errors"
	"regexp"
	"strings"
)

// ErrEmpty is returned by Render when the input has no non-space content.
var ErrEmpty = errors.New("empty markdown")

var (
	h1 = regexp.MustCompile(`^# (.+)$`)
	em = regexp.MustCompile(`\*([^*]+)\*`)
)

// Render converts a tiny markdown subset to HTML: a line beginning with "# "
// becomes an <h1> element, and *text* spans become <em> elements. The output
// has no trailing newline, so it is a stable target for a golden comparison.
func Render(input string) (string, error) {
	if strings.TrimSpace(input) == "" {
		return "", ErrEmpty
	}
	var out strings.Builder
	for _, line := range strings.Split(input, "\n") {
		switch {
		case h1.MatchString(line):
			m := h1.FindStringSubmatch(line)
			out.WriteString("<h1>")
			out.WriteString(m[1])
			out.WriteString("</h1>\n")
		default:
			out.WriteString(em.ReplaceAllString(line, "<em>$1</em>"))
			out.WriteString("\n")
		}
	}
	return strings.TrimRight(out.String(), "\n"), nil
}
```

### The runnable demo

The demo renders a small document and prints the HTML, so you can see the exact
shape a golden test would pin.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mdrender"
)

func main() {
	out, err := mdrender.Render("# Report\n*bold* status")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
<h1>Report</h1>
<em>bold</em> status
```

### Tests

`TestRenderGolden` is the core: it renders `helloInput` and compares against
`helloGolden`. When `UPDATE=1` is set it logs the actual output and returns
before the comparison, so you can copy the printed value into the constant.
`TestRenderRejectsEmpty` asserts the sentinel with `errors.Is`.
`TestRenderGoldenUpdateRoundTrip` documents the contract that regenerating then
re-running is green: it renders twice to prove determinism, and checks that the
committed golden already equals what an update would capture, so `UPDATE=1`
followed by a normal run passes both times.

Create `markdown_test.go`:

```go
package mdrender

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

const (
	helloInput  = "# Hello\nThis is *markdown*.\n"
	helloGolden = "<h1>Hello</h1>\nThis is <em>markdown</em>."
)

func TestRenderGolden(t *testing.T) {
	t.Parallel()

	got, err := Render(helloInput)
	if err != nil {
		t.Fatalf("Render(helloInput) error: %v", err)
	}
	if os.Getenv("UPDATE") == "1" {
		t.Logf("UPDATE=1: paste this into helloGolden:\n%s", got)
		return
	}
	if got != helloGolden {
		t.Fatalf("Render mismatch:\ngot:\n%s\nwant:\n%s", got, helloGolden)
	}
}

func TestRenderRejectsEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"spaces", "   "},
		{"newlines", "\n\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Render(tc.input); !errors.Is(err, ErrEmpty) {
				t.Fatalf("Render(%q) err = %v, want ErrEmpty", tc.input, err)
			}
		})
	}
}

func TestRenderGoldenUpdateRoundTrip(t *testing.T) {
	t.Parallel()

	// What UPDATE=1 would capture:
	actual, err := Render(helloInput)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	// A second run must produce identical output (determinism), or the golden
	// would flip back and forth on every -update.
	again, err := Render(helloInput)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if actual != again {
		t.Fatalf("non-deterministic output: %q vs %q", actual, again)
	}
	// The committed golden already equals the captured value, so an UPDATE=1
	// run followed by a normal run is green.
	if actual != helloGolden {
		t.Fatalf("committed golden is stale; UPDATE=1 would change it to:\n%s", actual)
	}
}

func ExampleRender() {
	out, _ := Render("# Title\n*hi*")
	fmt.Println(out)
	// Output:
	// <h1>Title</h1>
	// <em>hi</em>
}
```

## Review

The renderer is correct when its output is a pure function of the input: the same
markdown always yields the same HTML, with no clock, randomness, or map ordering
in the path — that determinism is what makes any golden test possible. The inline
golden is correct when `TestRenderGolden` compares against `helloGolden` on the
normal path and only logs on `UPDATE=1`, and when the round-trip test confirms a
regenerate-then-run cycle stays green. The trap this exercise inoculates against
is treating `UPDATE=1` as a rubber stamp: the printed value must be read before it
is pasted, because whatever you paste becomes the new expectation, bug included.
The inline form is deliberately small; the moment the expected output grows past a
readable constant, move it to `testdata/`, which is the next exercise.

## Resources

- [testing](https://pkg.go.dev/testing) — the `*testing.T` API, `t.Logf`, and `t.Parallel`.
- [regexp](https://pkg.go.dev/regexp) — `MustCompile`, `FindStringSubmatch`, and `ReplaceAllString`.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel like `ErrEmpty`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-testdata-golden-file-update-flag.md](02-testdata-golden-file-update-flag.md)
