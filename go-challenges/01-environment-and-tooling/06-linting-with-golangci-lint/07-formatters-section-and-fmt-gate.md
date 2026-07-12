# Exercise 7: Formatting as a Gate — the formatters Section and golangci-lint fmt

In v2, formatting is not a separate `gofmt` step you might forget; it is part of the
same gate as linting. This exercise takes a small slug generator, configures the v2
`formatters` section (`gofumpt` for stricter formatting, `gci` for import grouping),
uses `golangci-lint fmt --diff` to preview and `golangci-lint fmt` to fix, and shows
that an enabled formatter also fails `golangci-lint run`.

This module is self-contained: its own `go mod init`, a `slug` package, a demo, and
a table test.

## What you'll build

```text
slugfmt/                      independent module: example.com/slugfmt
  go.mod                      go 1.24
  slug.go                     Make(s) -> URL-safe slug (gofumpt-clean)
  slug_test.go                table test over slug normalization
  cmd/
    demo/
      main.go                 prints a few slugs
  .golangci.yml               formatters section (shown in prose)
```

- Files: `slug.go`, `slug_test.go`, `cmd/demo/main.go`, plus the config in prose.
- Implement: `Make(s string) string` producing a lowercase, hyphen-separated, ASCII slug.
- Test: a table over punctuation, spacing, and empty input.
- Verify: `go test -count=1 -race ./...`; then `golangci-lint fmt --diff`, `golangci-lint fmt`, and `golangci-lint run ./...`.

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/06-linting-with-golangci-lint/07-formatters-section-and-fmt-gate/cmd/demo
cd go-solutions/01-environment-and-tooling/06-linting-with-golangci-lint/07-formatters-section-and-fmt-gate
```

### Why formatting belongs in the lint gate

In v1, `gofmt`/`goimports` were linters, awkwardly reusing the linter machinery to
report "this file is not formatted". v2 makes them a first-class `formatters`
section with two entry points. `golangci-lint fmt` rewrites files in place (like
`gofmt -w`), and `golangci-lint fmt --diff` prints the unified diff without touching
anything — the mode you run in CI to *check* formatting. The key property: any
formatter you *enable* also runs as a check during `golangci-lint run`, so an
unformatted file fails the exact same gate that catches an unchecked error. There is
no separate "did someone run gofmt" question; the answer is "the gate would have
failed".

`gofumpt` is a stricter superset of `gofmt` — it enforces the additional
conventions `gofmt` leaves optional (no empty lines at the start of a block,
grouped `var` blocks, and so on). `gci` deterministically groups and orders imports
into sections (standard, default/third-party, and a local-module section), which
removes the recurring "import block churn" from diffs. Together they make formatting
a mechanical, non-negotiable property of every file.

### The misformatted starting point

Imagine the file arrives like this — space-indented, imports ungrouped and out of
order, an empty line at the top of a block:

```
package slug

import (
    "strings"
    "unicode"
)

func Make(s string) string {

    var b strings.Builder
    _ = unicode.IsLetter
    return b.String()
}
```

`golangci-lint fmt --diff` reports the delta (indentation, the stray blank line, the
import grouping) without changing the file; `golangci-lint run ./...` *fails* on the
same issues because the formatters are enabled as checks; and `golangci-lint fmt`
rewrites the file to the canonical form. After the fix, `gofmt -l .` prints nothing
and the run is clean.

Create the properly formatted file. Create `slug.go`:

```go
package slug

import "strings"

// Make converts s into a URL-safe slug: it lowercases the input, keeps ASCII
// letters and digits, and collapses every run of other characters into a single
// hyphen, with no leading or trailing hyphen.
func Make(s string) string {
	var b strings.Builder
	pendingHyphen := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
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

### The config

Create `.golangci.yml` with a `formatters` section alongside the linters:

```yaml
version: "2"

linters:
  default: none
  enable:
    - errcheck
    - govet
    - staticcheck

formatters:
  enable:
    - gofumpt
    - gci
  settings:
    gofumpt:
      extra-rules: true
    gci:
      sections:
        - standard
        - default
        - localmodule
```

`formatters.settings.gci.sections` fixes the import order: standard-library imports
first, third-party (`default`) next, then this module's own packages
(`localmodule`). If you prefer `goimports` to `gci`, enable it instead and set
`formatters.settings.goimports.local-prefixes: [example.com/slugfmt]` to push local
imports into their own trailing group; enabling both `gci` and `goimports` is
discouraged because they both reorder imports and will fight. Preview and fix:

```bash
golangci-lint fmt --diff   # show what would change, exit non-zero if anything
golangci-lint fmt          # rewrite files in place
golangci-lint run ./...    # now clean; formatters run as checks here too
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/slugfmt"
)

func main() {
	for _, s := range []string{"Hello, World!", "  Go 1.24  ", "already-a-slug"} {
		fmt.Printf("%q -> %q\n", s, slug.Make(s))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"Hello, World!" -> "hello-world"
"  Go 1.24  " -> "go-1-24"
"already-a-slug" -> "already-a-slug"
```

### Tests

Create `slug_test.go`. The table pins the normalization rules: punctuation and
spacing collapse to single hyphens, leading/trailing separators are dropped, and
all-punctuation input yields an empty slug.

```go
package slug

import "testing"

func TestMake(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "punctuation and space", in: "Hello, World!", want: "hello-world"},
		{name: "trim and collapse", in: "  Go 1.24  ", want: "go-1-24"},
		{name: "already a slug", in: "already-a-slug", want: "already-a-slug"},
		{name: "collapse runs", in: "a---b___c", want: "a-b-c"},
		{name: "all punctuation", in: "!!!", want: ""},
		{name: "empty", in: "", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Make(tc.in); got != tc.want {
				t.Fatalf("Make(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
```

## Review

Formatting is correct when `golangci-lint fmt --diff` reports nothing and
`golangci-lint run ./...` passes — because in v2 those are the same gate, a file that
is not `gofumpt`/`gci`-clean fails the run just as an unchecked error would. The
`slug` code is the vehicle; the deliverable is the `formatters` section and the habit
of running `fmt --diff` in CI to *check* and `fmt` locally to *fix*. The mistakes to
avoid: leaving `gofmt`/`goimports` under `linters` after a migration (they mean
nothing there and formatting silently stops), enabling both `gci` and `goimports`
(they reorder imports differently and produce a never-stable file), and treating
formatting as a courtesy rather than a gate — if it is not enforced, one editor
without format-on-save reintroduces churn into every diff.

## Resources

- [golangci-lint: Formatters](https://golangci-lint.run/docs/formatters/) — the `formatters` section, `fmt`, and `fmt --diff`.
- [golangci-lint: Formatters configuration](https://golangci-lint.run/docs/formatters/configuration/) — `gofumpt`, `gci.sections`, `goimports.local-prefixes`.
- [gofumpt](https://github.com/mvdan/gofumpt) — the stricter `gofmt` superset and its added rules.

---

Back to [06-migrate-v1-config-to-v2.md](06-migrate-v1-config-to-v2.md) | Next: [08-disciplined-nolint-with-nolintlint.md](08-disciplined-nolint-with-nolintlint.md)
