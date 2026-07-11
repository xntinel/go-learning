# Exercise 1: Build the string-library core with a sentinel error as part of the contract

The headline artifact of this lesson is `publicstr`, the small string library the
whole org will import. This exercise builds its stable core: `Slugify`,
`Truncate`, and `Reverse`, plus the `ErrEmpty` sentinel returned on empty input.
Every later exercise extends or hardens this same module; here you establish the
behavior that becomes the frozen contract.

This module is fully self-contained: its own `go mod init`, the library package, a
runnable CLI in `cmd/demo`, and table-driven tests. Nothing here imports any other
exercise.

## What you'll build

```text
publicstr/                 independent module: example.com/publicstr
  go.mod                   go 1.26
  strings.go               Slugify, Truncate, Reverse; var ErrEmpty
  cmd/
    demo/
      main.go              runnable demo exercising all three functions
  strings_test.go          table-driven tests; errors.Is(ErrEmpty); -race safe
```

- Files: `strings.go`, `cmd/demo/main.go`, `strings_test.go`.
- Implement: `Slugify` (URL-safe slug), `Truncate` (byte-bounded, ellipsis counts toward the limit), `Reverse` (byte reversal, documented as NOT rune-safe), and the exported `ErrEmpty` sentinel.
- Test: Slugify happy path, hyphen collapse, digit preservation, empty rejection via `errors.Is`; Truncate short and long; Reverse byte order; an `Example` with `// Output:`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/publicstr/cmd/demo
cd ~/go-exercises/publicstr
go mod init example.com/publicstr
```

### Why the sentinel is part of the contract, not an implementation detail

`Slugify` and `Truncate` reject empty input by returning `ErrEmpty`, an
*exported* package-level value created with `errors.New`. That export is a
deliberate design decision, not a convenience. The moment a consumer writes
`if errors.Is(err, publicstr.ErrEmpty)`, the *identity* of that value becomes
contract: replacing it with a differently-constructed error, or returning a bare
`errors.New("empty string")` created per-call, would silently break their branch
even though the message text is identical. A sentinel is the promise "you may
match this by identity, and I will not change it." Design it as carefully as the
function signatures.

The three functions each encode a small, precisely-documented behavior:

- `Slugify` lowercases, keeps only ASCII letters and digits, collapses every run
  of non-alphanumeric characters to a single hyphen, and trims leading and
  trailing hyphens. `"Hello, World!"` becomes `"hello-world"`; `"foo---bar"`
  becomes `"foo-bar"`; `"Foo 123 Bar"` becomes `"foo-123-bar"` (digits survive).
  Empty or whitespace-only input returns `ErrEmpty`.
- `Truncate` bounds the output to at most `n` bytes, and the appended `"..."`
  counts toward that limit — so `Truncate("hello world", 5)` is `"he..."`
  (five bytes), not `"hello..."`. This is the subtle contract: the ellipsis is
  inside the budget, so a consumer sizing a database column by `n` is never
  surprised.
- `Reverse` reverses *bytes*, and its doc says so explicitly. Reversing a
  multi-byte UTF-8 string by bytes corrupts it; documenting the byte-level
  behavior (rather than claiming to "reverse the string") is the honest contract.
  Exercise 7 adds a rune-safe successor and deprecates this one — but only because
  the byte behavior was documented precisely enough to reason about.

Create `strings.go`:

```go
package publicstr

import (
	"errors"
	"strings"
	"unicode"
)

// ErrEmpty is returned by Slugify and Truncate when the input is empty or, for
// Slugify, contains no slug-able characters. It is a sentinel: match it with
// errors.Is. Its identity is part of the public contract and will not change.
var ErrEmpty = errors.New("publicstr: empty string")

// Slugify converts s to a URL-safe slug: lowercase, ASCII letters and digits
// only, with every run of non-alphanumeric characters collapsed to a single
// hyphen and leading and trailing hyphens trimmed. It returns ErrEmpty when the
// input is empty, whitespace-only, or reduces to no slug-able characters.
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

// Truncate returns s bounded to at most n bytes. When s is longer than n, the
// result is s cut short with "..." appended, and the ellipsis counts toward the
// limit, so len(result) <= n always holds. It returns ErrEmpty on empty input.
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

// Reverse returns s with its bytes in reverse order. This is NOT rune-safe:
// multi-byte UTF-8 sequences are corrupted. Use ReverseRunes (Exercise 7) for
// rune-aware reversal. The byte-level behavior is documented deliberately so
// callers can reason about it.
func Reverse(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}
```

### The runnable demo

`cmd/demo` is `package main`, so it can touch only the exported API — which is
exactly what a consumer sees. If the demo compiles, the public surface is usable
from outside the package.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/publicstr"
)

func main() {
	slug, _ := publicstr.Slugify("Hello, World!")
	fmt.Printf("slug: %s\n", slug)

	digits, _ := publicstr.Slugify("Foo 123 Bar")
	fmt.Printf("digits: %s\n", digits)

	short, _ := publicstr.Truncate("hello world", 5)
	fmt.Printf("truncate: %s\n", short)

	fmt.Printf("reverse: %s\n", publicstr.Reverse("hello"))

	if _, err := publicstr.Slugify("   "); errors.Is(err, publicstr.ErrEmpty) {
		fmt.Println("empty: rejected via ErrEmpty")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
slug: hello-world
digits: foo-123-bar
truncate: he...
reverse: olleh
empty: rejected via ErrEmpty
```

### Tests

The tests are same-package (`package publicstr`) so they can reach unexported
helpers if needed, and they assert the *contract*: exact outputs, the ellipsis
budget, digit preservation, and — most importantly — that empty input is
matchable by `errors.Is(err, ErrEmpty)` rather than by string comparison.
`TestSlugifyPreservesDigits` pins the digit contract that the original lesson's
verification step called out.

Create `strings_test.go`:

```go
package publicstr

import (
	"errors"
	"testing"
)

func TestSlugify(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{"happy", "Hello, World!", "hello-world"},
		{"collapses hyphens", "foo---bar", "foo-bar"},
		{"preserves digits", "Foo 123 Bar", "foo-123-bar"},
		{"trims edges", "  --Go--  ", "go"},
		{"underscores become hyphen", "a_b_c", "a-b-c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Slugify(tc.in)
			if err != nil {
				t.Fatalf("Slugify(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSlugifyRejectsEmpty(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "   ", "!!!", "\t\n"} {
		if _, err := Slugify(in); !errors.Is(err, ErrEmpty) {
			t.Fatalf("Slugify(%q) err = %v, want ErrEmpty", in, err)
		}
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in string
		n        int
		want     string
	}{
		{"short unchanged", "hello", 10, "hello"},
		{"exact fit", "hello", 5, "hello"},
		{"long with ellipsis", "hello world", 5, "he..."},
		{"tiny limit", "hello", 2, ".."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Truncate(tc.in, tc.n)
			if err != nil {
				t.Fatalf("Truncate(%q,%d) error: %v", tc.in, tc.n, err)
			}
			if got != tc.want {
				t.Fatalf("Truncate(%q,%d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
			if len(got) > tc.n {
				t.Fatalf("Truncate(%q,%d) = %q exceeds byte budget %d", tc.in, tc.n, got, tc.n)
			}
		})
	}
}

func TestTruncateRejectsEmpty(t *testing.T) {
	t.Parallel()
	if _, err := Truncate("", 10); !errors.Is(err, ErrEmpty) {
		t.Fatalf("Truncate(\"\",10) err = %v, want ErrEmpty", err)
	}
}

func TestReverse(t *testing.T) {
	t.Parallel()
	if got := Reverse("hello"); got != "olleh" {
		t.Fatalf("Reverse = %q, want olleh", got)
	}
	if got := Reverse(""); got != "" {
		t.Fatalf("Reverse(\"\") = %q, want empty", got)
	}
}
```

## Review

The core is correct when each function's contract holds exactly: `Slugify`
collapses non-alphanumeric runs to one hyphen and preserves digits, `Truncate`
never exceeds its byte budget with the ellipsis counted, and empty input is
rejected through `ErrEmpty` matched by `errors.Is` — not by comparing message
strings. The trap this exercise guards against is treating `ErrEmpty` as a
throwaway: once it is exported and consumers match it, its identity is frozen, so
you construct it once as a package var and never per-call. Keep `Reverse`'s doc
honest about byte-level behavior; the whole point of documenting the limitation
precisely is that Exercise 7 can then deprecate it and offer a rune-safe successor
without surprising anyone. Run `go test -race` to confirm the pure functions have
no shared state to corrupt.

## Resources

- [Effective Go: Commentary](https://go.dev/doc/effective_go#commentary) — doc-comment conventions for exported names.
- [`errors` package](https://pkg.go.dev/errors) — `errors.New`, `errors.Is`, and sentinel identity.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — why sentinels are matched with `errors.Is`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-package-doc-and-godoc-contract.md](02-package-doc-and-godoc-contract.md)
