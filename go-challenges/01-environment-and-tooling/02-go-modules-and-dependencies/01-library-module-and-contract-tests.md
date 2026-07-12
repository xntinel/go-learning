# Exercise 1: A Library Module With Contract Tests

The smallest honest unit of Go work is a versioned library module with a real
test suite. This exercise builds one from nothing: its own `go.mod`, a package
that exposes a sentinel error, and a table-driven parallel test that asserts the
contract through `errors.Is` rather than string matching.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
greeter/                    independent module: example.com/greeter
  go.mod                    module path + go directive
  greeter.go                package greeter: ErrEmptyName, Greeting, MustGreeting
  cmd/
    demo/
      main.go               runs Greeting on a couple of inputs
  greeter_test.go           table-driven parallel tests; errors.Is; panic recovery
```

- Files: `greeter.go`, `cmd/demo/main.go`, `greeter_test.go`.
- Implement: `ErrEmptyName` sentinel, `Greeting(name) (string, error)` that trims and rejects empty, `MustGreeting(name) string` that panics on error.
- Test: table-driven parallel cases over success and `ErrEmptyName`, a Unicode trim case, and a `recover`-based panic test for `MustGreeting`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/02-go-modules-and-dependencies/01-library-module-and-contract-tests/cmd/demo
cd go-solutions/01-environment-and-tooling/02-go-modules-and-dependencies/01-library-module-and-contract-tests
```

`go mod init` writes a two-line `go.mod`: the module path and the `go` directive.
That path is the root of every import inside the module — `cmd/demo` will import
the library as `example.com/greeter`, not by a relative path. Go has no relative
imports; the module path is how packages inside one module find each other.

### Why a real package, not package main

The library is `package greeter`, not `package main`. That is deliberate: a
`*_test.go` in the same directory and same package can reach unexported
identifiers, so the contract can be tested from the inside, and a separate
`package main` under `cmd/demo` can only touch the *exported* surface — which
forces the library to have a clean public API. A lesson whose "test" is a `main`
that prints an expected line is not tested at all; a `main` does not fail when the
code regresses. The verification here is the `*_test.go`.

The one branchable failure — an empty name — is exposed as a package-level
sentinel `ErrEmptyName` created with `errors.New`. Callers branch on it with
`errors.Is`, which sees through `%w` wrapping and does not break when the message
is reworded. `MustGreeting` is the panic-on-error convenience wrapper: correct for
tests and constant inputs, never for user input.

Create `greeter.go`:

```go
package greeter

import (
	"errors"
	"fmt"
	"strings"
)

// ErrEmptyName is returned by Greeting when the trimmed name is empty. Callers
// branch on it with errors.Is, never on the message text.
var ErrEmptyName = errors.New("name must not be empty")

// Greeting builds a polite greeting for name. Surrounding whitespace is
// trimmed (Unicode-aware) and an empty result is rejected, because there is
// nothing useful to say to a blank input.
func Greeting(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ErrEmptyName
	}
	return fmt.Sprintf("Hello, %s!", trimmed), nil
}

// MustGreeting wraps Greeting and panics on error. Use it for tests and
// constant inputs; never call it on user input.
func MustGreeting(name string) string {
	g, err := Greeting(name)
	if err != nil {
		panic(err)
	}
	return g
}
```

### The runnable demo

`cmd/demo` is a separate `package main`. It imports the library through the module
path and can only use the exported `Greeting`/`MustGreeting` — which is exactly
why those are exported.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/greeter"
)

func main() {
	fmt.Println(greeter.MustGreeting("World"))
	fmt.Println(greeter.MustGreeting("  Gopher  "))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Hello, World!
Hello, Gopher!
```

### The contract test

The test is table-driven and parallel. Success rows assert the returned string;
error rows assert the sentinel via `errors.Is`. Two rows pin the trimming
contract: leading/trailing spaces collapse to nothing, and the trim is rune-based
(`strings.TrimSpace` is Unicode-aware, so `"  José  "` trims correctly). A
separate test recovers from `MustGreeting("")` to prove it panics. Modern Go
loop-variable scoping means no `tc := tc` shim is needed before `t.Parallel`.

Create `greeter_test.go`:

```go
package greeter

import (
	"errors"
	"fmt"
	"testing"
)

func TestGreeting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{name: "plain", input: "Go", want: "Hello, Go!"},
		{name: "trims outer spaces", input: "  Go  ", want: "Hello, Go!"},
		{name: "keeps inner spaces", input: "Mary Jane", want: "Hello, Mary Jane!"},
		{name: "unicode trim", input: "  José  ", want: "Hello, José!"},
		{name: "empty", input: "", wantErr: ErrEmptyName},
		{name: "only whitespace", input: "   \t\n", wantErr: ErrEmptyName},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Greeting(tc.input)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Greeting(%q) err = %v, want %v", tc.input, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Greeting(%q) unexpected err = %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("Greeting(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestMustGreetingPanicsOnEmpty(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustGreeting(\"\") did not panic")
		}
	}()
	_ = MustGreeting("")
}

func ExampleGreeting() {
	g, _ := Greeting("Gopher")
	fmt.Println(g)
	// Output: Hello, Gopher!
}
```

Run the gate:

```bash
gofmt -l .
go vet ./...
go build ./...
go test -count=1 -race ./...
go mod verify
```

`go test` runs `TestGreeting`, `TestMustGreetingPanicsOnEmpty`, and
`ExampleGreeting` (whose stdout is diffed against the `// Output:` line), all
under `-race`. `go mod verify` re-checks the cached module bytes against `go.sum`
and prints nothing on success.

## Review

The contract is correct when `Greeting` returns `ErrEmptyName` exactly for inputs
that trim to empty and the exact `"Hello, X!"` string otherwise, and the tests
prove both halves. The traps this exercise trains you out of: do not assert
`err.Error() == "name must not be empty"` — the row asserting `ErrEmptyName` via
`errors.Is` keeps working after any rewording or `%w` wrap, the string comparison
would not. Do not make the library `package main`; a same-package test could not
then reach unexported state, and the demo could not distinguish public from
private API. Keep `MustGreeting` out of any user-input path; its panic is a
programmer-error signal, which is why the panic case has its own recovery test.
Run `go test -race` to confirm nothing here trips the detector even though the
type holds no shared state yet — the habit matters once it does.

## Resources

- [Tutorial: Create a Go module](https://go.dev/doc/tutorial/create-module) — the canonical first-module walkthrough.
- [go.mod file reference](https://go.dev/ref/mod#go-mod-file) — the `module` and `go` directives and their grammar.
- [`errors` package](https://pkg.go.dev/errors) — `errors.New`, `errors.Is`, and sentinel-error semantics.
- [Testable Examples in Go](https://go.dev/blog/examples) — how `Example` functions with `// Output:` are compiled and run.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-cmd-binary-imports-internal-package.md](02-cmd-binary-imports-internal-package.md)
