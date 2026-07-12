# Exercise 1: Shared internal/ Packages With a Tested Library Core

The foundation of a well-laid-out Go project is a library core that lives under
`internal/` and is tested as a normal package. This exercise builds that
skeleton — `internal/config` constants and an `internal/greeting` package with a
sentinel error, `Greet`, `Default`, and `AppPrefix` — and its table-driven test,
proving that an internal package is a normal package in every respect except who
may import it.

This module is fully self-contained: its own `go mod init`, every type it needs
inline, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
myapp/                         module github.com/example/myapp
  go.mod                       go 1.24
  internal/
    config/
      config.go                AppName, Version constants
    greeting/
      greeting.go              ErrEmptyName, Greet, Default, AppPrefix
      greeting_test.go         table-driven, errors.Is on the sentinel
  cmd/
    demo/
      main.go                  runnable: prints Default() and a named greeting
```

- Files: `internal/config/config.go`, `internal/greeting/greeting.go`, `internal/greeting/greeting_test.go`, `cmd/demo/main.go`.
- Implement: `config.AppName`/`config.Version`; `greeting.ErrEmptyName`, `Greet(name) (string, error)`, `Default() string`, `AppPrefix() string`.
- Test: table-driven `TestGreet` over {plain, trims, default, empty, whitespace-only} asserting exact strings on success and `errors.Is(err, ErrEmptyName)` on failure; `TestDefault` on the canonical output.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/03-go-workspace-and-project-layout/01-shared-internal-packages/internal/config go-solutions/01-environment-and-tooling/03-go-workspace-and-project-layout/01-shared-internal-packages/internal/greeting go-solutions/01-environment-and-tooling/03-go-workspace-and-project-layout/01-shared-internal-packages/cmd/demo
cd go-solutions/01-environment-and-tooling/03-go-workspace-and-project-layout/01-shared-internal-packages
go mod edit -go=1.24
```

The module path follows the published-code convention — a path you control — even
though this project is local-only. That is what makes the `internal/` boundary
meaningful: it is defined relative to the module root `github.com/example/myapp`.

### An internal package is a normal package

The only thing special about `internal/greeting` is *who may import it*: code
rooted at `github.com/example/myapp`, and nothing else. In every other respect it
is an ordinary package. It has exported symbols, it has a test in the same
package that reaches unexported helpers, and it is compiled and vetted exactly
like any other package. Merging the original "write the package" and "test the
package" steps into one module makes the point concrete: the test is part of the
same unit as the code, not a separate exercise.

`config` is the simplest kind of shared library — a package of constants. It
exists to show that `internal/config` is a normal package directory; the CLI and
server binaries in later exercises read `AppName` and `Version` from it.

`greeting` carries the one piece of real design in this module: a *sentinel
error*. `ErrEmptyName` is a package-level `error` value created with `errors.New`.
`Greet` trims the input and, if nothing is left, returns that sentinel so callers
can branch on the specific failure with `errors.Is(err, greeting.ErrEmptyName)`
rather than string-matching the message. This is the idiomatic way a library
signals a distinguishable, expected failure. `Default` is a convenience wrapper
that greets `"World"`; because `"World"` can never trip the empty check, a
failure there would be a programming error, so it panics — a panic on an
impossible input is a legitimate assertion, not error handling you skipped.

Create `internal/config/config.go`:

```go
package config

const (
	// AppName is the application name shared across binaries.
	AppName = "myapp"
	// Version is the application version. Bump on each release.
	Version = "0.1.0"
)
```

Create `internal/greeting/greeting.go`:

```go
package greeting

import (
	"errors"
	"fmt"
	"strings"
)

// ErrEmptyName is returned by Greet when the name is empty after trimming.
// Callers branch on it with errors.Is rather than matching the message text.
var ErrEmptyName = errors.New("name must not be empty")

// Greet formats a greeting for name. Surrounding whitespace is trimmed; an
// empty or whitespace-only name is rejected with ErrEmptyName.
func Greet(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ErrEmptyName
	}
	return fmt.Sprintf("%s %s says hello", AppPrefix(), trimmed), nil
}

// Default is the canonical greeting for "World".
func Default() string {
	g, err := Greet("World")
	if err != nil {
		// "World" can never be empty, so an error here means Greet itself
		// is broken. Fail loudly rather than return a wrong value.
		panic(err)
	}
	return g
}

// AppPrefix is the bracketed name-and-version prefix shared by every greeting.
func AppPrefix() string {
	return fmt.Sprintf("[%s %s]", "myapp", "0.1.0")
}
```

`greeting` deliberately does not import `internal/config`; the prefix string is
built from literals so the package stays usable on its own, with no coupling the
binaries do not need.

### The runnable demo

`cmd/demo` is a separate `package main`, so it can only touch the exported API of
`greeting` and `config` — exactly the surface an external caller (were one
allowed) would see. It prints the default greeting and a named one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"github.com/example/myapp/internal/config"
	"github.com/example/myapp/internal/greeting"
)

func main() {
	fmt.Printf("%s %s\n", config.AppName, config.Version)
	fmt.Println(greeting.Default())

	g, err := greeting.Greet("Gopher")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(g)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
myapp 0.1.0
[myapp 0.1.0] World says hello
[myapp 0.1.0] Gopher says hello
```

### Tests

The test lives in `package greeting`, so it is part of the same compilation unit
as the code it checks. It is table-driven, runs subtests in parallel, asserts the
exact formatted string on the success rows, and asserts the sentinel with
`errors.Is` on the failure rows. The `Example` gives a `go test`-verified snippet
of the API in use.

Create `internal/greeting/greeting_test.go`:

```go
package greeting

import (
	"errors"
	"fmt"
	"testing"
)

func TestGreet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{name: "plain", input: "Gopher", want: "[myapp 0.1.0] Gopher says hello"},
		{name: "trims", input: "  Gopher  ", want: "[myapp 0.1.0] Gopher says hello"},
		{name: "default", input: "World", want: "[myapp 0.1.0] World says hello"},
		{name: "empty", input: "", wantErr: ErrEmptyName},
		{name: "whitespace only", input: "  \t", wantErr: ErrEmptyName},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Greet(tc.input)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Greet(%q) err = %v, want %v", tc.input, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Greet(%q) unexpected err = %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("Greet(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestDefault(t *testing.T) {
	t.Parallel()

	if got, want := Default(), "[myapp 0.1.0] World says hello"; got != want {
		t.Fatalf("Default() = %q, want %q", got, want)
	}
}

func ExampleGreet() {
	g, _ := Greet("Ada")
	fmt.Println(g)
	// Output: [myapp 0.1.0] Ada says hello
}
```

## Review

The greeting package is correct when `Greet` is a pure function of its trimmed
input: a non-empty trimmed name yields the exact `"[myapp 0.1.0] <name> says
hello"` string, and an empty or whitespace-only name yields `ErrEmptyName` and
nothing else. The test proves both halves — exact-string equality on the success
rows and `errors.Is(err, ErrEmptyName)` on the failure rows — which is why the
sentinel is a package-level value wrapped-and-comparable rather than a bare
string. Two mistakes to avoid: do not compare the error with `==` against a
freshly constructed error or match its message text (use `errors.Is`, so wrapping
with `%w` upstream keeps working); and remember `cmd/demo` sees only exported
identifiers, so anything the demo needs must be exported deliberately, not a raw
field. Run `go test -race ./...` to confirm the whole module builds and the
package tests pass.

## Resources

- [Go 1.4 internal packages release note](https://go.dev/doc/go1.4#internalpackages) — the original rule that `internal/` is compiler-enforced.
- [Organizing a Go module](https://go.dev/doc/modules/layout) — official guidance on `internal/`, `cmd/`, and package layout.
- [`errors` package](https://pkg.go.dev/errors) — `errors.New` and `errors.Is` for sentinel errors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-cli-thin-main-testable.md](02-cli-thin-main-testable.md)
