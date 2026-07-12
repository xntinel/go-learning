# Exercise 3: go get — Adding a Real Third-Party Dependency

`go get` is how a module acquires a dependency: it resolves a version, records it
in `go.mod`, pins its hash in `go.sum`, and fetches the source into the module
cache. This exercise does it for real with `golang.org/x/text`, wires the package
into a CLI, and reconciles the module files with `go mod tidy` so the tree is
clean and reproducible.

This module is self-contained; it bundles its own `hello` library and links one
external module.

## What you'll build

```text
greetget/                       independent module: example.com/greetget
  go.mod                        require golang.org/x/text after go get
  go.sum                        pinned hashes for x/text
  internal/hello/
    hello.go                    bundled Greet + ErrEmptyName
  cmd/demo/
    main.go                     uses cases.Title(language.English)
    main_test.go                asserts run() output with the external module linked
```

Files: `internal/hello/hello.go`, `cmd/demo/main.go`, `cmd/demo/main_test.go`.
Implement: `run(args, out)` that title-cases the greeting with `cases.Title(language.English).String(...)`.
Test: assert `run` output for several names with the external module linked.
Verify: `go mod tidy && go build ./... && go test -count=1 ./...`

Set up the module and add the dependency:

```bash
mkdir -p go-solutions/01-environment-and-tooling/05-go-install-and-third-party-packages/03-go-get-third-party-dependency/internal/hello go-solutions/01-environment-and-tooling/05-go-install-and-third-party-packages/03-go-get-third-party-dependency/cmd/demo
cd go-solutions/01-environment-and-tooling/05-go-install-and-third-party-packages/03-go-get-third-party-dependency
go get golang.org/x/text@latest
```

### What `go get` actually changed

Run `go get golang.org/x/text@latest` and four things happen at once. `go.mod`
gains a `require golang.org/x/text vX.Y.Z` line — the version pin. `go.sum` gains
two lines for that version: the `h1:` hash of the module's file tree and the hash
of its `go.mod`, both cross-checked against the checksum database on first
download. The source is fetched into `GOMODCACHE`. And because your code will
`import` the package directly, the `require` line carries *no* `// indirect`
comment (contrast Exercise 5, where a transitive module does). `go mod tidy` is
the reconciler: it adds any missing requires, drops unused ones, and fixes the
`// indirect` annotations to match the real import graph. A clean tree is one
where a second `go mod tidy` produces no diff.

`golang.org/x/text` is a genuine external module — the Go team's Unicode and
text-processing library, versioned like any other. `cases.Title(language.English)`
returns a `Caser` that title-cases per English rules; `Caser.String(s)` applies
it. The point of the exercise is not the title-casing itself (our greeting is
already capitalized) but that an external module is fetched, pinned, verified,
and linked exactly the same way a standard-library package is imported.

Create `internal/hello/hello.go` (bundled copy):

```go
// internal/hello/hello.go
package hello

import (
	"errors"
	"fmt"
	"strings"
)

// ErrEmptyName is returned by Greet when the name is empty after trimming.
var ErrEmptyName = errors.New("name must not be empty")

// Greet returns a greeting for name, trimming whitespace and rejecting an empty
// result with ErrEmptyName.
func Greet(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ErrEmptyName
	}
	return fmt.Sprintf("Hello, %s!", trimmed), nil
}
```

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"example.com/greetget/internal/hello"
)

func main() {
	if err := run(os.Args, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	name := "World"
	if len(args) > 1 {
		name = args[1]
	}
	g, err := hello.Greet(name)
	if err != nil {
		if errors.Is(err, hello.ErrEmptyName) {
			return errors.New("please pass a non-empty name")
		}
		return err
	}
	// Title-case with the external module to prove it is linked and working.
	title := cases.Title(language.English).String(g)
	fmt.Fprintln(out, title)
	return nil
}
```

Reconcile and run:

```bash
go mod tidy
go run ./cmd/demo gopher
```

Expected output:

```text
Hello, Gopher!
```

### The test

The test links the external module (building the test binary pulls in
`golang.org/x/text`) and asserts `run`'s output for several names. It does not
import `x/text` directly; it verifies the end result, which is what a caller
cares about.

Create `cmd/demo/main_test.go`:

```go
// cmd/demo/main_test.go
package main

import (
	"bytes"
	"testing"
)

func TestRunTitleCased(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "default", args: []string{"demo"}, want: "Hello, World!\n"},
		{name: "lower", args: []string{"demo", "gopher"}, want: "Hello, Gopher!\n"},
		{name: "already upper", args: []string{"demo", "Gopher"}, want: "Hello, Gopher!\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			if err := run(tc.args, &buf); err != nil {
				t.Fatalf("run(%v) unexpected err = %v", tc.args, err)
			}
			if got := buf.String(); got != tc.want {
				t.Fatalf("run(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
```

## Review

The dependency is wired correctly when `go.mod` contains a
`require golang.org/x/text ...` line with no `// indirect` comment (your code
imports it directly), `go.sum` has matching hash lines, and a second
`go mod tidy` produces no diff. The trap is committing a `go.mod` that `go get`
edited but `go mod tidy` was never run against, leaving unused or mis-annotated
requires; CI with `-mod=readonly` then fails because the build wants to edit a
file it is forbidden to touch. Confirm with `go build ./...`,
`go test -count=1 ./...`, and `grep 'golang.org/x/text' go.mod`.

## Resources

- [Managing dependencies (`go get`, `go mod tidy`)](https://go.dev/doc/modules/managing-dependencies) — the official workflow.
- [`golang.org/x/text/cases`](https://pkg.go.dev/golang.org/x/text/cases) — `Title`, `Caser`, and `Caser.String`.
- [`golang.org/x/text/language`](https://pkg.go.dev/golang.org/x/text/language) — `language.English` and tag semantics.
- [Go Modules Reference](https://go.dev/ref/mod) — how `go.mod`/`go.sum` are structured.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-demo-cli-with-run-pattern.md](02-demo-cli-with-run-pattern.md) | Next: [04-go-install-and-use-standalone-tool.md](04-go-install-and-use-standalone-tool.md)
