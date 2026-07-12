# Exercise 4: Adding and Verifying a Third-Party Dependency

Adding a dependency is not just `go get`; it is a change to the build contract
that has to be reconciled, verified, and proven reproducible. This exercise adds
`golang.org/x/text/cases` to title-case names, then walks the reconcile-verify-
tidy loop that keeps `go.mod`/`go.sum` honest.

This module is fully self-contained: its own `go mod init`, all code inline, its
own tests. Nothing here imports another exercise.

## What you'll build

```text
titlecase/                  independent module: example.com/titlecase
  go.mod                    require golang.org/x/text after go get
  titlecase.go              Title(name, tag) using cases.Title; ErrEmptyName
  cmd/
    demo/
      main.go               title-cases a few names
  titlecase_test.go         table test: English vs Und vs already-cased
```

- Files: `titlecase.go`, `cmd/demo/main.go`, `titlecase_test.go`.
- Implement: `Title(name string, tag language.Tag) (string, error)` built on `cases.Title(tag).String`, rejecting a blank name with `ErrEmptyName`.
- Test: table over English, `Und`, and already-cased input.
- Verify: `go get`, then `go mod verify` (silent), then `go mod tidy` twice with a clean `git diff`, then `go test -race`.

Set up the module and add the dependency:

```bash
go get golang.org/x/text/cases
```

### What go get actually did

`go get golang.org/x/text/cases` did four things: it selected a version of
`golang.org/x/text`, downloaded the module zip, added a `require
golang.org/x/text vX.Y.Z` line to `go.mod`, and recorded the module's hash (and
its `go.mod` hash) in `go.sum`. It did not compile or install anything — since
Go 1.17 `go get` only edits the module graph. Inspect the result:

```bash
cat go.mod
```

The `require` block now names `golang.org/x/text`. If `x/text` pulled in modules
you do not import directly, they would appear with a trailing `// indirect`
comment — the marker for "in the graph, but not imported by your source". You do
not hand-edit these; the toolchain maintains them.

`golang.org/x/text/cases` is the canonical way to title-case Unicode text. The
signature is `cases.Title(t language.Tag, opts ...Option) Caser`, and a `Caser`
exposes `String(s string) string`. The `language.Tag` matters: casing rules are
language-specific (the Turkish dotless-i is the textbook example), so you pass an
explicit tag rather than assuming a locale. `language.English` applies
English rules; `language.Und` ("undetermined") applies root/default rules.

Create `titlecase.go`:

```go
package titlecase

import (
	"errors"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// ErrEmptyName is returned when the trimmed name is empty.
var ErrEmptyName = errors.New("name must not be empty")

// Title title-cases name using the casing rules of tag. Surrounding whitespace
// is trimmed and a blank name is rejected. Title-casing is language-specific,
// so the caller supplies the language.Tag explicitly.
func Title(name string, tag language.Tag) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ErrEmptyName
	}
	return cases.Title(tag).String(trimmed), nil
}
```

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"golang.org/x/text/language"

	"example.com/titlecase"
)

func main() {
	for _, name := range []string{"mary jane", "GO", "josé"} {
		out, _ := titlecase.Title(name, language.English)
		fmt.Printf("%s -> %s\n", name, out)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
mary jane -> Mary Jane
GO -> Go
josé -> José
```

Note `GO` becomes `Go`: title-casing upcases the first rune of each word and
downcases the rest, so an all-caps input is normalized, not preserved.

### The test

The table pins the casing contract: multi-word English input, already-cased
input (idempotent), the `Und` tag producing the same result for plain ASCII, and
the blank-name error asserted with `errors.Is`. These outputs were confirmed
against `golang.org/x/text` directly.

Create `titlecase_test.go`:

```go
package titlecase

import (
	"errors"
	"testing"

	"golang.org/x/text/language"
)

func TestTitle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		tag   language.Tag
		want  string
	}{
		{name: "english two words", input: "mary jane", tag: language.English, want: "Mary Jane"},
		{name: "english lowercases rest", input: "GO", tag: language.English, want: "Go"},
		{name: "already cased is stable", input: "Ada Lovelace", tag: language.English, want: "Ada Lovelace"},
		{name: "undetermined tag", input: "mary jane", tag: language.Und, want: "Mary Jane"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Title(tc.input, tc.tag)
			if err != nil {
				t.Fatalf("Title(%q) unexpected err = %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("Title(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestTitleRejectsBlank(t *testing.T) {
	t.Parallel()

	if _, err := Title("   ", language.English); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("Title(blank) err = %v, want ErrEmptyName", err)
	}
}
```

### Verify, tidy, prove idempotency

With the code compiling, run the reconcile-verify loop:

```bash
go mod verify
go mod tidy
go mod tidy
git diff go.mod go.sum
```

`go mod verify` re-hashes every module in the cache and compares against `go.sum`;
a clean run prints `all modules verified` and exits 0. `go mod tidy` reconciles
the graph with the imports — after the first run the second is a no-op, and
`git diff` shows nothing, which is the proof that `tidy` is idempotent and the
graph is settled. Any diff on the second `tidy` means the first did not converge,
which is a red flag worth investigating before merging.

## Review

The dependency is wired correctly when `go.mod` names `golang.org/x/text`,
`go mod verify` is silent, and a second `go mod tidy` produces no diff. The traps:
do not assume `go build` will add the `require` for a new import — under the
default `-mod=readonly` it fails with "inconsistent" instead; run `go get` or
`go mod tidy`. Do not hand-edit `// indirect` lines; the toolchain owns them. Do
not skip the `language.Tag` argument by imagining `cases.Title` has a
locale-free form — it does not; casing is language-specific and the tag is
required. And commit `go.sum`: without it, another machine cannot verify the bytes
it downloaded match what you built against.

## Resources

- [`golang.org/x/text/cases`](https://pkg.go.dev/golang.org/x/text/cases) — `cases.Title`, `Caser.String`, and the casing options.
- [`golang.org/x/text/language`](https://pkg.go.dev/golang.org/x/text/language) — `language.Tag`, `language.English`, `language.Und`.
- [Managing dependencies](https://go.dev/doc/modules/managing-dependencies) — `go get`, `go mod tidy`, and the reconcile workflow.
- [go mod verify](https://go.dev/ref/mod#go-mod-verify) — what the checksum re-verification checks.

---

Back to [03-testable-examples-as-documentation.md](03-testable-examples-as-documentation.md) | Next: [05-pin-and-inspect-the-build-list.md](05-pin-and-inspect-the-build-list.md)
