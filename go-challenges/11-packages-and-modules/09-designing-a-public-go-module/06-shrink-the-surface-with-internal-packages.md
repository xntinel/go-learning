# Exercise 6: Hide helpers behind internal/ to keep the public surface minimal

Anything exported is frozen the moment it ships; anything unexported can be
refactored forever. This exercise moves the rune-classification and
hyphen-collapse helpers into an `internal/normalize` package the module can import
but no external consumer can, shrinking `publicstr`'s exported surface to exactly
the entry points consumers need. A test then pins the exported set, so a helper
cannot leak into the contract by accident.

This module is fully self-contained: its own `go mod init`, the public package,
an `internal/normalize` package with its own tests, a demo, and a surface-guard
test.

## What you'll build

```text
publicstr/                       independent module: example.com/publicstr
  go.mod                         go 1.26
  strings.go                     Slugify, Truncate, Reverse, ErrEmpty (thin, public)
  internal/
    normalize/
      normalize.go               IsSlugChar, IsSeparator, Slug (module-private helpers)
      normalize_test.go          unit tests for the helpers, reached directly
  cmd/
    demo/
      main.go                    runnable demo
  surface_test.go                asserts the exported set is exactly the intended one
```

- Files: `strings.go`, `internal/normalize/normalize.go`, `internal/normalize/normalize_test.go`, `cmd/demo/main.go`, `surface_test.go`.
- Implement: `internal/normalize` holding the classification and collapse helpers; `publicstr` delegating to them and exposing only `Slugify`, `Truncate`, `Reverse`, `ErrEmpty`.
- Test: unit tests inside `internal/normalize`; a `go/doc` guard asserting `publicstr`'s exported set equals `{Slugify, Truncate, Reverse, ErrEmpty}` exactly.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/09-designing-a-public-go-module/06-shrink-the-surface-with-internal-packages/internal/normalize go-solutions/11-packages-and-modules/09-designing-a-public-go-module/06-shrink-the-surface-with-internal-packages/cmd/demo
cd go-solutions/11-packages-and-modules/09-designing-a-public-go-module/06-shrink-the-surface-with-internal-packages
```

### The internal/ rule, and why the smallest surface is the safest

The Go compiler enforces a special rule for any import path containing an
`internal/` element: it is importable *only* by code rooted at the parent of that
`internal` directory. So `example.com/publicstr/internal/normalize` can be imported
by `example.com/publicstr` and any of its subpackages, but an *outside* module that
writes `import "example.com/publicstr/internal/normalize"` fails to compile with
`use of internal package ... not allowed`. The compiler, not a linter, guarantees
those helpers never become part of anyone's contract.

That guarantee is the point. `IsSlugChar`, `IsSeparator`, and `Slug` are exported
*within the module* (so the public package can call them across the package
boundary), yet invisible outside it. You get clean, testable, capital-letter
helpers with their own focused unit tests, while retaining total freedom to rename,
retype, or delete them on any afternoon — because no external consumer could have
depended on them. Export intent (the four entry points) and hide implementation
(the helpers). The smallest public surface is the one you can most safely refactor
later.

The `surface_test.go` guard turns "keep the surface minimal" into a mechanical
check. It parses the public package with `go/doc` and asserts the set of exported
names is *exactly* `{Slugify, Truncate, Reverse, ErrEmpty}` — no more. If a future
edit accidentally exports a helper (say a public `CollapseHyphens`), the set no
longer matches and the test fails, catching the leak before it ships and freezes.

Create `internal/normalize/normalize.go`:

```go
package normalize

import (
	"strings"
	"unicode"
)

// IsSlugChar reports whether r is kept verbatim in a slug: an ASCII lowercase
// letter or digit. Exported for use within the module only (internal/).
func IsSlugChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
}

// IsSeparator reports whether r introduces a word boundary that collapses to a
// single separator in a slug.
func IsSeparator(r rune) bool {
	return unicode.IsSpace(r) || r == '-' || r == '_'
}

// Slug lowercases s and builds a slug joined by sep, collapsing separator runs
// and trimming trailing separators. It returns "" when nothing slug-able remains.
func Slug(s string, sep rune) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevSep := false
	for _, r := range s {
		switch {
		case IsSlugChar(r):
			b.WriteRune(r)
			prevSep = false
		case IsSeparator(r):
			if !prevSep && b.Len() > 0 {
				b.WriteRune(sep)
				prevSep = true
			}
		}
	}
	return strings.TrimRight(b.String(), string(sep))
}
```

Create `internal/normalize/normalize_test.go`:

```go
package normalize

import "testing"

func TestIsSlugChar(t *testing.T) {
	t.Parallel()
	for _, r := range "abz09" {
		if !IsSlugChar(r) {
			t.Errorf("IsSlugChar(%q) = false, want true", r)
		}
	}
	for _, r := range "AZ -_!." {
		if IsSlugChar(r) {
			t.Errorf("IsSlugChar(%q) = true, want false", r)
		}
	}
}

func TestSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		sep  rune
		want string
	}{
		{"Hello, World!", '-', "hello-world"},
		{"foo---bar", '-', "foo-bar"},
		{"Foo 123 Bar", '_', "foo_123_bar"},
		{"  --Go--  ", '-', "go"},
		{"!!!", '-', ""},
	}
	for _, tc := range cases {
		if got := Slug(tc.in, tc.sep); got != tc.want {
			t.Errorf("Slug(%q,%q) = %q, want %q", tc.in, tc.sep, got, tc.want)
		}
	}
}
```

Now the public package delegates to the hidden helpers and stays thin.

Create `strings.go`:

```go
package publicstr

import (
	"errors"
	"strings"

	"example.com/publicstr/internal/normalize"
)

// ErrEmpty is returned when the input reduces to no slug-able characters.
var ErrEmpty = errors.New("publicstr: empty string")

// Slugify converts s to a URL-safe slug. The classification and collapse logic
// lives in an internal package, so it can be refactored without touching this
// public contract.
func Slugify(s string) (string, error) {
	out := normalize.Slug(s, '-')
	if out == "" {
		return "", ErrEmpty
	}
	return out, nil
}

// Truncate returns s bounded to at most n bytes; when longer, "..." is appended
// and counts toward the limit. It returns ErrEmpty on empty input.
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

// Reverse returns s with its bytes reversed. Not rune-safe.
func Reverse(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}
```

An outside module cannot reach the helpers. This would fail to compile if a
consumer tried it, which is the guarantee the `internal/` rule gives you:

```text
$ cat > /tmp/consumer/main.go <<'EOF'
package main

import "example.com/publicstr/internal/normalize" // outside the module

func main() { _ = normalize.Slug("x", '-') }
EOF
$ go build
main.go:3:8: use of internal package example.com/publicstr/internal/normalize not allowed
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
	slug, _ := publicstr.Slugify("Internal Packages!")
	fmt.Println(slug)
	trunc, _ := publicstr.Truncate("normalization", 8)
	fmt.Println(trunc)
	fmt.Println(publicstr.Reverse("abc"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
internal-packages
norma...
cba
```

### The surface-guard test

Create `surface_test.go`:

```go
package publicstr

import (
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

func TestExportedSurfaceIsMinimal(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", name, err)
		}
		files = append(files, f)
	}
	pkg, err := doc.NewFromFiles(fset, files, "example.com/publicstr")
	if err != nil {
		t.Fatalf("doc.NewFromFiles: %v", err)
	}

	var got []string
	for _, f := range pkg.Funcs {
		got = append(got, f.Name)
	}
	for _, ty := range pkg.Types {
		got = append(got, ty.Name)
	}
	for _, v := range pkg.Vars {
		got = append(got, v.Names...)
	}
	sort.Strings(got)

	want := []string{"ErrEmpty", "Reverse", "Slugify", "Truncate"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("exported surface = %v, want exactly %v (a helper leaked?)", got, want)
	}
}
```

## Review

The surface is minimal when the guard passes: the public package exports exactly
`Slugify`, `Truncate`, `Reverse`, and `ErrEmpty`, and the classification and
collapse helpers live under `internal/normalize` where the compiler forbids
outside imports. The value is asymmetry — the helpers are capitalized and unit
tested for your convenience, yet remain refactorable forever because no external
consumer could import them. The failure mode this prevents is the quiet leak: a
helper exported "because it was handy", which the moment it ships becomes frozen
contract you can never change. Keep the guard test green and that can never happen
by accident. Note the surface test parses only the public package directory, not
`internal/`, so it measures exactly what a consumer sees.

## Resources

- [Go Modules Reference: internal packages](https://go.dev/ref/mod#internal-packages) — the compiler-enforced import rule.
- [go command: Internal Directories](https://pkg.go.dev/cmd/go#hdr-Internal_Directories) — the exact scoping of `internal/`.
- [Go Blog: Package names](https://go.dev/blog/package-names) — designing a small, intention-revealing surface.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-errors-as-a-stable-api-surface.md](05-errors-as-a-stable-api-surface.md) | Next: [07-deprecate-and-migrate-without-breaking.md](07-deprecate-and-migrate-without-breaking.md)
