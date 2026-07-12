# Exercise 4: Make Slugify configurable without ever changing its signature

Every configuration knob you add as a positional parameter is a breaking change.
The functional-options pattern exists precisely to avoid that: `SlugifyWith(s
string, opts ...Option)` can grow new options forever, each a purely additive,
source- and binary-compatible change. This exercise builds that evolvable entry
point and keeps the original `Slugify` as a stable one-liner delegating to it —
the "add, do not change" discipline made concrete.

This module is fully self-contained: its own `go mod init`, the library with the
options machinery, a demo, and tests proving options compose and stay compatible.

## What you'll build

```text
publicstr/                 independent module: example.com/publicstr
  go.mod                   go 1.26
  strings.go               Slugify, SlugifyWith, Option, WithMaxLen, WithSeparator, WithFallback
  cmd/
    demo/
      main.go              runnable demo showing default and configured slugs
  strings_test.go          defaults == legacy; options compose; zero opts == Slugify
```

- Files: `strings.go`, `cmd/demo/main.go`, `strings_test.go`.
- Implement: `SlugifyWith(s string, opts ...Option)`, an unexported `config`, and the `Option` constructors `WithMaxLen`, `WithSeparator`, `WithFallback`; keep `Slugify` as a shim delegating to `SlugifyWith`.
- Test: zero options equals legacy `Slugify`; `WithMaxLen` truncates on a rune boundary; `WithSeparator('_')` swaps the joiner; `WithFallback` replaces `ErrEmpty`; options compose in order.
- Verify: `go test -count=1 -race ./...`

### How the options pattern preserves the signature forever

The mechanism is three moving parts. An unexported `config` struct holds every
knob with a sensible zero-or-default value. An exported `Option` is a function
that mutates a `*config`. Each `WithX` constructor is a closure that captures its
argument and returns such a function. `SlugifyWith` starts from the defaults,
applies every option in order, then runs the algorithm against the resulting
config. Adding a new knob later means adding a new `WithX` constructor and a field
on the unexported `config` — the signature of `SlugifyWith` never changes, so no
consumer's build breaks and no recompilation is forced. That is why this pattern,
not positional parameters, is the standard way Go libraries stay evolvable.

Two design rules keep it honest. First, the `config` struct is *unexported*: if it
were exported, its fields would become part of the frozen contract and adding one
could break struct-literal consumers, defeating the purpose. Consumers configure
only through `Option` constructors. Second, the defaults must reproduce the legacy
behavior exactly, so `SlugifyWith(s)` with zero options is indistinguishable from
`Slugify(s)` — which lets `Slugify` become a one-line shim delegating to
`SlugifyWith`, collapsing the two code paths into one.

The three options here are representative of real knobs:

- `WithSeparator(sep rune)` swaps the `-` joiner (e.g. `_` for filesystem-safe
  names).
- `WithMaxLen(n int)` bounds the slug to at most `n` runes, cutting on a rune
  boundary and trimming a trailing separator so the result never ends in a stray
  joiner. It uses `[]rune` so a future non-ASCII separator does not split a
  multi-byte character.
- `WithFallback(slug string)` returns a caller-supplied default instead of
  `ErrEmpty` when the input has no slug-able characters — the "never fail, use a
  placeholder" policy some call sites want.

Create `strings.go`:

```go
package publicstr

import (
	"errors"
	"strings"
	"unicode"
)

// ErrEmpty is returned when the input reduces to no slug-able characters and no
// WithFallback option was supplied.
var ErrEmpty = errors.New("publicstr: empty string")

// config holds Slugify's tunable behavior. It is unexported on purpose: consumers
// configure it only through Option constructors, so new knobs stay additive.
type config struct {
	separator rune
	maxLen    int    // 0 means no limit
	fallback  string // returned instead of ErrEmpty when set
	hasFall   bool
}

func defaults() config {
	return config{separator: '-', maxLen: 0}
}

// Option configures SlugifyWith. New options can be added indefinitely without
// changing SlugifyWith's signature, so every addition is backward compatible.
type Option func(*config)

// WithSeparator sets the rune used to join words. The default is '-'.
func WithSeparator(sep rune) Option {
	return func(c *config) { c.separator = sep }
}

// WithMaxLen bounds the slug to at most n runes, cutting on a rune boundary and
// trimming a trailing separator. A non-positive n means no limit.
func WithMaxLen(n int) Option {
	return func(c *config) { c.maxLen = n }
}

// WithFallback returns slug instead of ErrEmpty when the input has no slug-able
// characters.
func WithFallback(slug string) Option {
	return func(c *config) {
		c.fallback = slug
		c.hasFall = true
	}
}

// Slugify converts s to a URL-safe slug with default settings. It is a stable
// shim over SlugifyWith; new behavior is added through Option, not here.
func Slugify(s string) (string, error) {
	return SlugifyWith(s)
}

// SlugifyWith converts s to a URL-safe slug, applying opts in order. With no
// options it is identical to Slugify. It returns ErrEmpty when the result would
// be empty and no WithFallback was supplied.
func SlugifyWith(s string, opts ...Option) (string, error) {
	c := defaults()
	for _, opt := range opts {
		opt(&c)
	}

	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevSep := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevSep = false
		case unicode.IsSpace(r), r == '-', r == '_':
			if !prevSep && b.Len() > 0 {
				b.WriteRune(c.separator)
				prevSep = true
			}
		}
	}
	out := strings.TrimRight(b.String(), string(c.separator))

	if c.maxLen > 0 {
		runes := []rune(out)
		if len(runes) > c.maxLen {
			out = strings.TrimRight(string(runes[:c.maxLen]), string(c.separator))
		}
	}

	if out == "" {
		if c.hasFall {
			return c.fallback, nil
		}
		return "", ErrEmpty
	}
	return out, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/publicstr"
)

func main() {
	def, _ := publicstr.Slugify("Hello, World!")
	fmt.Printf("default:   %s\n", def)

	under, _ := publicstr.SlugifyWith("Hello, World!", publicstr.WithSeparator('_'))
	fmt.Printf("separator: %s\n", under)

	capped, _ := publicstr.SlugifyWith("the quick brown fox", publicstr.WithMaxLen(9))
	fmt.Printf("maxlen:    %s\n", capped)

	fallback, _ := publicstr.SlugifyWith("!!!", publicstr.WithFallback("untitled"))
	fmt.Printf("fallback:  %s\n", fallback)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
default:   hello-world
separator: hello_world
maxlen:    the-quick
fallback:  untitled
```

### Tests

The tests prove the compatibility properties the pattern exists to guarantee:
zero options is identical to `Slugify`, each option has its documented effect,
`WithMaxLen` cuts on a rune boundary and trims a trailing separator, and options
apply in order.

Create `strings_test.go`:

```go
package publicstr

import (
	"errors"
	"testing"
)

func TestZeroOptionsEqualsLegacy(t *testing.T) {
	t.Parallel()
	inputs := []string{"Hello, World!", "foo---bar", "Foo 123 Bar", "  --Go--  "}
	for _, in := range inputs {
		legacy, lerr := Slugify(in)
		withZero, werr := SlugifyWith(in)
		if legacy != withZero || (lerr == nil) != (werr == nil) {
			t.Fatalf("SlugifyWith(%q) = %q,%v; Slugify = %q,%v", in, withZero, werr, legacy, lerr)
		}
	}
}

func TestWithSeparator(t *testing.T) {
	t.Parallel()
	got, err := SlugifyWith("Hello, World!", WithSeparator('_'))
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello_world" {
		t.Fatalf("got %q, want hello_world", got)
	}
}

func TestWithMaxLenRuneBoundary(t *testing.T) {
	t.Parallel()
	// "the quick brown fox" -> "the-quick-brown-fox"; capped at 9 runes -> "the-quick".
	got, err := SlugifyWith("the quick brown fox", WithMaxLen(9))
	if err != nil {
		t.Fatal(err)
	}
	if got != "the-quick" {
		t.Fatalf("got %q, want the-quick", got)
	}
	if len([]rune(got)) > 9 {
		t.Fatalf("got %q exceeds 9 runes", got)
	}
}

func TestWithMaxLenTrimsTrailingSeparator(t *testing.T) {
	t.Parallel()
	// "the-quick" capped at 4 -> "the-" -> trimmed to "the".
	got, err := SlugifyWith("the quick", WithMaxLen(4))
	if err != nil {
		t.Fatal(err)
	}
	if got != "the" {
		t.Fatalf("got %q, want the", got)
	}
}

func TestWithFallback(t *testing.T) {
	t.Parallel()
	got, err := SlugifyWith("!!!", WithFallback("untitled"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "untitled" {
		t.Fatalf("got %q, want untitled", got)
	}
	// Without the option, the same input yields ErrEmpty.
	if _, err := SlugifyWith("!!!"); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
}

func TestOptionsComposeInOrder(t *testing.T) {
	t.Parallel()
	got, err := SlugifyWith("the quick brown fox",
		WithSeparator('_'), WithMaxLen(9))
	if err != nil {
		t.Fatal(err)
	}
	if got != "the_quick" {
		t.Fatalf("got %q, want the_quick", got)
	}
}
```

## Review

The options are correct when zero of them reproduce `Slugify` exactly and each
one has only its documented effect — that equivalence is what lets `Slugify`
delegate and lets you add a tenth option next quarter without breaking a single
consumer. The subtle points are two: keep the `config` struct unexported so its
fields never become frozen contract, and make `WithMaxLen` cut on a rune boundary
and trim a trailing separator so a capped slug never ends in a stray joiner. The
composition test proves options apply in order, which matters when two options
touch the same field. If you find yourself tempted to add a `maxLen int` parameter
to `Slugify` instead, that is exactly the breaking change this pattern eliminates:
a new `Option` is additive; a new parameter is not.

## Resources

- [Go Blog: Self-referential functions and the design of options (Rob Pike)](https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html) — the original articulation of the pattern.
- [Uber Go Style Guide: Functional Options](https://github.com/uber-go/guide/blob/master/style.md#functional-options) — when to prefer options over a config struct.
- [Go Blog: Keeping your modules compatible](https://go.dev/blog/module-compatibility) — why additive change (new options) is the compatible path.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-testable-examples-that-cannot-rot.md](03-testable-examples-that-cannot-rot.md) | Next: [05-errors-as-a-stable-api-surface.md](05-errors-as-a-stable-api-surface.md)
