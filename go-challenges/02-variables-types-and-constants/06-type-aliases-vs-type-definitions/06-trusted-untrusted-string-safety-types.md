# Exercise 6: Defined String Types as a Trusted/Untrusted Rendering Boundary

Cross-site scripting is, at bottom, a type confusion: untrusted user input reaches
a spot that expected already-escaped, trusted text. This exercise builds a tiny
rendering boundary inspired by `html/template`: `type SafeHTML string` means
"already escaped, safe to emit", a `Render` function accepts only `SafeHTML`, and
`MustEscape` is the single audited path from an untrusted `string` to `SafeHTML`.
The compiler makes injecting raw input a build error.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
safehtml/                 independent module: example.com/safehtml
  go.mod                  go 1.24
  safehtml.go             type SafeHTML string; MustEscape, Raw, Render, String
  cmd/
    demo/
      main.go             escapes an XSS payload, renders a trusted fragment
  safehtml_test.go        escaping, concatenation, XSS round-trip tests
```

- Files: `safehtml.go`, `cmd/demo/main.go`, `safehtml_test.go`.
- Implement: `SafeHTML` with `MustEscape(string) SafeHTML` (the audited constructor), `Raw(string) SafeHTML` (the explicit trust-me escape hatch), and `Render(...SafeHTML) SafeHTML`.
- Test: `MustEscape` neutralizes `<`, `>`, `&`, and quotes; `Render` concatenates `SafeHTML`; an XSS-shaped payload round-trips to escaped output.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/safehtml/cmd/demo
cd ~/go-exercises/safehtml
go mod init example.com/safehtml
go mod edit -go=1.24
```

### The type as a capability token

`html/template.HTML` is a `type HTML string`: a defined string type whose meaning
is "this content is known safe and will not be escaped again". The pattern this
exercise copies is to make the *trusted* form a distinct type from the *untrusted*
form. Raw user input is a plain `string`. Trusted, already-escaped content is a
`SafeHTML`. Because `SafeHTML` is a defined type, a `string` is not assignable to
it implicitly — you cannot pass raw input where `SafeHTML` is required. The type
becomes a capability token that says "this text has passed through escaping".

The only ordinary way to obtain a `SafeHTML` from untrusted input is `MustEscape`,
which runs `html.EscapeString` (turning `<`, `>`, `&`, `'`, `"` into their HTML
entities) and returns the trusted type. That single function is the audited
chokepoint: a security reviewer reads `MustEscape` once and knows every
untrusted-to-trusted transition in the program goes through it. `Render` takes
`...SafeHTML`, so a call site *must* have already escaped each fragment; it cannot
slip a raw `string` in.

There is one deliberate escape hatch, `Raw`, for content the programmer asserts is
already safe (a constant template chunk). It is a `SafeHTML(s)` conversion behind a
named function so that every trust assertion is greppable — you can audit all uses
of `Raw` to confirm none launder user input. This mirrors the real-world reality
that a trust boundary needs a visible, searchable "I promise this is safe" marker,
not a silent conversion sprinkled inline.

Create `safehtml.go`:

```go
package safehtml

import (
	"html"
	"strings"
)

// SafeHTML is HTML text that is already escaped and safe to emit. It is a DEFINED
// string type distinct from a raw, untrusted string, so a plain string cannot be
// used where SafeHTML is required without going through a constructor.
type SafeHTML string

// MustEscape is the single audited path from untrusted input to SafeHTML. It
// HTML-escapes the input, neutralizing <, >, &, ', and ".
func MustEscape(untrusted string) SafeHTML {
	return SafeHTML(html.EscapeString(untrusted))
}

// Raw asserts that s is already safe and wraps it without escaping. Every use is
// a trust assertion; audit callers of Raw to prove none launder user input.
func Raw(s string) SafeHTML {
	return SafeHTML(s)
}

// Render concatenates trusted fragments. Its signature only accepts SafeHTML, so
// a raw untrusted string cannot be passed here.
func Render(fragments ...SafeHTML) SafeHTML {
	var b strings.Builder
	for _, f := range fragments {
		b.WriteString(string(f))
	}
	return SafeHTML(b.String())
}

// String returns the underlying escaped text for emission.
func (h SafeHTML) String() string { return string(h) }
```

### The compile-time security guarantee

The guarantee is the call that does not build:

```go
// userInput := r.FormValue("name")
// Render(userInput) // does not compile: string is not SafeHTML
```

To render user input you are forced to write `Render(MustEscape(userInput))`,
which routes it through escaping. There is no accidental path from untrusted to
output; the type system closes it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/safehtml"
)

func main() {
	// An attacker-controlled comment field.
	untrusted := `<script>alert('xss')</script>`

	page := safehtml.Render(
		safehtml.Raw("<p>Comment: "),
		safehtml.MustEscape(untrusted), // the only path from untrusted to trusted
		safehtml.Raw("</p>"),
	)

	fmt.Println(page)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
<p>Comment: &lt;script&gt;alert(&#39;xss&#39;)&lt;/script&gt;</p>
```

The escaped payload sits between the two trusted `Raw` fragments, so the `<p>` and
`</p>` are emitted literally while the attacker's `<script>` is neutralized.

### Tests

The tests confirm `MustEscape` neutralizes each dangerous character, `Render`
concatenates trusted fragments in order, and a full XSS payload comes out escaped.
The escaped forms are the ones `html.EscapeString` produces: `<` becomes `&lt;`,
`>` becomes `&gt;`, `&` becomes `&amp;`, `"` becomes `&#34;`, and `'` becomes
`&#39;`.

Create `safehtml_test.go`:

```go
package safehtml

import (
	"fmt"
	"strings"
	"testing"
)

func TestMustEscapeNeutralizes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"<", "&lt;"},
		{">", "&gt;"},
		{"&", "&amp;"},
		{`"`, "&#34;"},
		{"'", "&#39;"},
		{"a<b>c", "a&lt;b&gt;c"},
		{"plain text", "plain text"},
	}
	for _, tc := range tests {
		if got := string(MustEscape(tc.in)); got != tc.want {
			t.Errorf("MustEscape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderConcatenates(t *testing.T) {
	t.Parallel()

	got := Render(Raw("<b>"), MustEscape("x & y"), Raw("</b>"))
	want := SafeHTML("<b>x &amp; y</b>")
	if got != want {
		t.Fatalf("Render = %q, want %q", got, want)
	}
}

func TestXSSPayloadIsEscaped(t *testing.T) {
	t.Parallel()

	payload := `<script>alert('xss')</script>`
	out := string(MustEscape(payload))

	if strings.Contains(out, "<script>") {
		t.Fatalf("payload not escaped: %q", out)
	}
	want := "&lt;script&gt;alert(&#39;xss&#39;)&lt;/script&gt;"
	if out != want {
		t.Errorf("MustEscape = %q, want %q", out, want)
	}
}

func ExampleMustEscape() {
	fmt.Println(MustEscape(`<a href="#">`))
	// Output: &lt;a href=&#34;#&#34;&gt;
}
```

## Review

The boundary is correct when raw input cannot reach `Render` without passing
through `MustEscape`, and when `MustEscape` produces exactly the entities
`html.EscapeString` defines. The mistake to avoid is exposing the conversion
directly — if callers can write `SafeHTML(userInput)` inline, the type buys you
nothing; keep the conversion inside named, auditable functions (`MustEscape` for
the escaping path, `Raw` for the explicit trust assertion) so every transition is
greppable. The second mistake is treating the demo's `Raw` fragments as user input;
`Raw` is only for content the programmer controls. The security property is
enforced statically: passing a raw `string` where `SafeHTML` is required does not
compile.

## Resources

- [`html/template.HTML`](https://pkg.go.dev/html/template#HTML) — the defined-string trust type this imitates.
- [`html.EscapeString`](https://pkg.go.dev/html#EscapeString) — the exact escaping and entity forms used.
- [OWASP: Cross Site Scripting Prevention](https://cheatsheetseries.owasp.org/cheatsheets/Cross_Site_Scripting_Prevention_Cheat_Sheet.html) — why an audited escaping boundary matters.

---

Prev: [05-header-canonicalization-defined-map.md](05-header-canonicalization-defined-map.md) | Next: [07-generic-set-alias-vs-defined.md](07-generic-set-alias-vs-defined.md)
