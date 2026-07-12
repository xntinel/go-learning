# Exercise 4: Slugify: A First Test With String Diffs

Turning a human title into a clean URL path segment is a job every CMS, blog, and
docs site does. The unit is a pure string transform, which makes it the right
place to learn why string assertions print with `%q`, not `%s`.

## What you'll build

```text
slugify/                   independent module: example.com/slugify
  go.mod
  slugify.go               func Slugify(title string) string
  slugify_test.go          TestSlugify (discrete %q assertions), ExampleSlugify
  cmd/
    demo/
      main.go              slugifies a few titles
```

- Files: `slugify.go`, `slugify_test.go`, `cmd/demo/main.go`.
- Implement: `Slugify(title string) string` — lowercase, collapse runs of non-alphanumerics to single hyphens, trim trailing hyphens.
- Test: exact string outputs with `%q` in the failure message so trailing hyphens and empty strings are visible.
- Verify: `gofmt -l .`, `go vet ./...`, `go test -count=1 -race ./...`.

### The transform, and why `%q` not `%s`

A slug is lowercase, contains only letters, digits, and single hyphens as
separators, and has no leading or trailing hyphen. `Slugify` walks the
lowercased title rune by rune: a letter or digit is appended; any run of
non-alphanumeric characters (spaces, punctuation, symbols) collapses to a single
hyphen, and a leading hyphen is suppressed by only emitting a separator once real
content exists. A trailing hyphen — produced when the title ends in punctuation —
is trimmed at the end. So `"Go 1.26 Release Notes!"` becomes
`"go-1-26-release-notes"`: the spaces and the `.` and the `!` all become
separators, the trailing `!`'s hyphen is trimmed.

The `strings.Builder` is the right accumulator: it grows a byte buffer without the
quadratic copying of repeated `+`. Using `unicode.IsLetter` and `unicode.IsDigit`
(rather than an ASCII range check) means the classification is correct for any
rune, and ranging over the string yields runes, not bytes.

The test lesson here is the format verb. String outputs must be printed with
`%q`, not `%s`. `%q` wraps the value in quotes and escapes it, so a failure line
reads `Slugify("Trailing!!") = "trailing-" want "trailing"` — the stray trailing
hyphen is *visible* inside the quotes. With `%s` the same line would read
`... = trailing- want trailing` and, worse, an empty-string result prints as
nothing at all, so `= want` looks like a formatting bug rather than a real diff.
For strings, `%q` is not a preference; it is how you keep the assertion legible
when the difference is whitespace, an escape, or emptiness.

Create `slugify.go`:

```go
package slugify

import (
	"strings"
	"unicode"
)

// Slugify converts a title into a URL-safe slug: lowercase, with runs of
// non-alphanumeric characters collapsed to single hyphens and no leading or
// trailing hyphen. "Go 1.26 Release Notes!" becomes "go-1-26-release-notes".
func Slugify(title string) string {
	var b strings.Builder
	pendingHyphen := false
	for _, r := range strings.ToLower(title) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if pendingHyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingHyphen = false
			b.WriteRune(r)
		default:
			pendingHyphen = true
		}
	}
	return b.String()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/slugify"
)

func main() {
	for _, title := range []string{
		"Go 1.26 Release Notes!",
		"  Hello,  World  ",
		"Already-a-slug",
		"!!!",
	} {
		fmt.Printf("%-24q -> %q\n", title, slugify.Slugify(title))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"Go 1.26 Release Notes!" -> "go-1-26-release-notes"
"  Hello,  World  "      -> "hello-world"
"Already-a-slug"         -> "already-a-slug"
"!!!"                    -> ""
```

### The tests

Create `slugify_test.go`:

```go
package slugify

import (
	"fmt"
	"testing"
)

func TestSlugify(t *testing.T) {
	t.Parallel()

	// Each assertion is written out (not a loop) so the mechanics stay visible;
	// the table-driven form is lesson 02. %q makes trailing hyphens and empty
	// strings legible in the failure line.
	if got, want := Slugify("Go 1.26 Release Notes!"), "go-1-26-release-notes"; got != want {
		t.Errorf("Slugify(%q) = %q, want %q", "Go 1.26 Release Notes!", got, want)
	}
	if got, want := Slugify("  Hello,  World  "), "hello-world"; got != want {
		t.Errorf("Slugify(%q) = %q, want %q", "  Hello,  World  ", got, want)
	}
	if got, want := Slugify("!!!"), ""; got != want {
		t.Errorf("Slugify(%q) = %q, want %q", "!!!", got, want)
	}
}

func ExampleSlugify() {
	fmt.Printf("%q\n", Slugify("Go 1.26 Release Notes!"))
	// Output: "go-1-26-release-notes"
}
```

## Review

The transform is correct when the output contains only lowercase alphanumerics
and single interior hyphens, with no leading or trailing hyphen, for every input
— including the degenerate `"!!!"` which must yield `""`, not `"-"`. That
empty-string case is exactly why the failure message uses `%q`: an empty result
printed with `%s` would be invisible and the diff unreadable. The `pendingHyphen`
flag with the `b.Len() > 0` guard is what suppresses the leading separator and
lets the trailing one simply never be written. Gate with `gofmt -l .`,
`go vet ./...`, and `go test -count=1 -race ./...`.

## Resources

- [strings.Builder](https://pkg.go.dev/strings#Builder) — the efficient string accumulator.
- [unicode package](https://pkg.go.dev/unicode) — `IsLetter`, `IsDigit`.
- [fmt package](https://pkg.go.dev/fmt) — the `%q` verb for quoted, escaped strings.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-parse-price-to-cents.md](03-parse-price-to-cents.md) | Next: [05-pagination-offset-guard.md](05-pagination-offset-guard.md)
