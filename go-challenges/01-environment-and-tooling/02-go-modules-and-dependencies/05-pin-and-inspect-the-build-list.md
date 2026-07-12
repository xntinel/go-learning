# Exercise 5: Pinning a Version and Inspecting the Build List

The build list — the exact set of module versions the toolchain selected — is the
data a CI pipeline reads to generate an SBOM and the data you read during a
dependency-forensics investigation. This exercise pins a dependency to an exact
version and then inspects the selection with `go list -m`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own tests. Nothing here imports another exercise.

## What you'll build

```text
buildlist/                  independent module: example.com/buildlist
  go.mod                    require golang.org/x/text pinned to an exact version
  normalize.go              Normalize(s) using cases.Fold (case-insensitive key)
  cmd/
    demo/
      main.go               normalizes a couple of strings
  normalize_test.go         table test over folding
```

- Files: `normalize.go`, `cmd/demo/main.go`, `normalize_test.go`.
- Implement: `Normalize(s string) string` that folds case with `cases.Fold`, for building case-insensitive lookup keys.
- Test: table asserting `Normalize` collapses case.
- Verify: pin with `go get pkg@version`, inspect with `go list -m all`, `-versions`, `-json`, `-u`; `go test -race`.

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/02-go-modules-and-dependencies/05-pin-and-inspect-the-build-list/cmd/demo
cd go-solutions/01-environment-and-tooling/02-go-modules-and-dependencies/05-pin-and-inspect-the-build-list
go get golang.org/x/text/cases
```

### Discover versions, then pin one

MVS selects the maximum of the required minimums, not the newest release, so a
dependency stays where the graph puts it until you deliberately raise the floor.
To choose a version, list what exists:

```bash
go list -m -versions golang.org/x/text
```

That prints every tagged version on one line. Pick one and pin it exactly (pinning
to an explicit `@vX.Y.Z` sets the `require` floor to precisely that version):

```bash
go get golang.org/x/text@v0.38.0
go mod tidy
```

`go get pkg@v0.38.0` rewrites the `require` to that version and updates `go.sum`;
`go mod tidy` reconciles. Now `go.mod` pins `golang.org/x/text v0.38.0` and every
build on every machine selects that exact version.

The library folds case with `cases.Fold()`, a `Caser` with no language argument
(case-folding is the language-independent operation used to build
case-insensitive comparison keys — distinct from lowercasing).

Create `normalize.go`:

```go
package buildlist

import (
	"strings"

	"golang.org/x/text/cases"
)

// Normalize returns a case-folded, space-trimmed form of s, suitable as a
// case-insensitive lookup key. Folding is language-independent, so no
// language.Tag is needed.
func Normalize(s string) string {
	return cases.Fold().String(strings.TrimSpace(s))
}
```

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/buildlist"
)

func main() {
	for _, s := range []string{"HELLO", "Hello", "  hello  "} {
		fmt.Printf("%-9q -> %q\n", s, buildlist.Normalize(s))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"HELLO"   -> "hello"
"Hello"   -> "hello"
"  hello  " -> "hello"
```

### The test

Create `normalize_test.go`:

```go
package buildlist

import "testing"

func TestNormalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "upper folds to lower", input: "HELLO", want: "hello"},
		{name: "mixed folds", input: "HeLLo", want: "hello"},
		{name: "trims then folds", input: "  World  ", want: "world"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := Normalize(tc.input); got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
```

### Inspect the build list

With the version pinned, read what the toolchain selected. `go list -m all` prints
the whole build list; `go list -m -json` emits structured records; `go list -m -u`
annotates each module with an available upgrade.

```bash
go list -m all
go list -m -json golang.org/x/text
go list -m -u all
```

`go list -m all` prints one line per module, for example:

```text
example.com/buildlist
golang.org/x/text v0.38.0
```

`go list -m -json golang.org/x/text` prints exactly the structured data a pipeline
consumes to build an SBOM — `Path`, `Version`, `Dir` (the cache location),
`Time` (the version's timestamp), and the checksum `Sum`:

```text
{
	"Path": "golang.org/x/text",
	"Version": "v0.38.0",
	"Time": "2026-06-08T15:10:20Z",
	"Dir": "/Users/you/go/pkg/mod/golang.org/x/text@v0.38.0",
	"GoMod": "/Users/you/go/pkg/mod/cache/download/golang.org/x/text/@v/v0.38.0.mod",
	"GoVersion": "1.25.0",
	"Sum": "h1:sXmwo9DwP3OK9EZ7PqAdaooSGozfl/3a6/xJcbzPRhE=",
	"GoModSum": "h1:YXZt3QhHUKYT53r2lLKFIVi6Ao1jdzrTR/KQ09qyxF4="
}
```

(Exact `Dir`, `Time`, and `Sum` depend on your machine and the version you pin.)
`go list -m -u all` adds a bracketed `[vX.Y.Z]` upgrade hint after any module with
a newer release, which is how you audit "how far behind is this dependency"
without changing anything.

## Review

Pinning is correct when `go.mod` names the exact `@vX.Y.Z` you requested after
`go mod tidy`, and `go list -m all` shows that version in the build list. The
mental model to internalize: MVS does not float you to the latest — pinning is how
you set the floor, and `go list -m -u all` is how you see the ceiling you are
choosing not to take. Treat `go list -m -json` as the canonical machine-readable
inventory: it is what belongs in an SBOM and what you grep during an incident to
answer "which version shipped". The trap is assuming an unpinned dependency
auto-updates; it does not, so an old, quietly-vulnerable version can persist until
some `require` raises the floor — which is exactly why the next module builds a
security gate around this data.

## Resources

- [go list -m](https://go.dev/ref/mod#go-list-m) — the `-versions`, `-json`, and `-u` flags and their output fields.
- [Minimal Version Selection](https://go.dev/ref/mod#minimal-version-selection) — why pinning sets a floor rather than a pin-to-latest.
- [`golang.org/x/text/cases`](https://pkg.go.dev/golang.org/x/text/cases) — `cases.Fold` and the `Caser` type.

---

Back to [04-add-and-verify-third-party-dependency.md](04-add-and-verify-third-party-dependency.md) | Next: [06-replace-directive-for-local-and-fork-development.md](06-replace-directive-for-local-and-fork-development.md)
