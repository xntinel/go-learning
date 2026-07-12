# Exercise 3: Testable Examples as Executable Documentation

Go's `Example` functions are two things at once: they render as usage examples in
godoc, and `go test` compiles them, runs them, and diffs their stdout against an
`// Output:` comment. That makes documentation that cannot silently rot — when the
code changes, the example fails.

This module is fully self-contained: its own `go mod init`, all code inline, its
own tests. Nothing here imports another exercise.

## What you'll build

```text
greetdoc/                   independent module: example.com/greetdoc
  go.mod
  greeter.go                package greeter: ErrEmptyName, Greeting
  cmd/
    demo/
      main.go               prints a greeting
  example_test.go           external package greeter_test: ExampleGreeting + error example
  greeter_test.go           one internal unit test
```

- Files: `greeter.go`, `cmd/demo/main.go`, `example_test.go`, `greeter_test.go`.
- Implement: `Greeting(name) (string, error)` with an `ErrEmptyName` sentinel.
- Test: `ExampleGreeting` and `ExampleGreeting_error` in an external `greeter_test` package, each with an `// Output:` comment; one internal unit test.
- Verify: `go test -count=1 -race ./...`

### How an Example becomes a test

A function named `ExampleGreeting` (the `Example` + exported-identifier naming is
what associates it with the `Greeting` symbol in godoc) is collected by `go test`.
If its body ends in a comment of the form `// Output: <text>`, the test harness
runs the function, captures everything it writes to `os.Stdout`, and compares that
capture to the text after `// Output:`. A mismatch fails the test. So the example
is a regression test whose assertion is literally the documented output; if you
change `Greeting` to emit `"Hi, %s"`, `ExampleGreeting` fails until you fix the
comment, which keeps the docs honest.

Two placement details matter. First, examples live in `_test.go` files, so they
never ship in the built binary. Second, putting them in the *external* test
package `greeter_test` (note the `_test` suffix on the package name) forces them
to import the library through its public path and use only exported API — so the
example demonstrates exactly what a real consumer would write, not privileged
internal access. A second example named `ExampleGreeting_error` (the
`_<suffix>` convention) documents the error branch; godoc groups it under the same
symbol.

Create `greeter.go`:

```go
package greeter

import (
	"errors"
	"fmt"
	"strings"
)

// ErrEmptyName is returned when the trimmed name is empty.
var ErrEmptyName = errors.New("name must not be empty")

// Greeting builds a greeting for name, trimming surrounding whitespace and
// rejecting an empty result.
func Greeting(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ErrEmptyName
	}
	return fmt.Sprintf("Hello, %s!", trimmed), nil
}
```

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/greetdoc"
)

func main() {
	g, _ := greeter.Greeting("Gopher")
	fmt.Println(g)
}
```

The import path is `example.com/greetdoc`, but the package it declares is
`greeter` (the package name is the last identifier in `greeter.go`'s `package`
clause, independent of the final path element). Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Hello, Gopher!
```

### The examples

`ExampleGreeting` prints a successful greeting; `ExampleGreeting_error` prints the
sentinel error's message. Each ends with an `// Output:` comment that `go test`
diffs against real stdout. To watch the mechanism, temporarily change one comment
to the wrong text and run `go test` — it fails with a `got/want` diff — then
restore it.

Create `example_test.go`:

```go
package greeter_test

import (
	"fmt"

	"example.com/greetdoc"
)

func ExampleGreeting() {
	g, err := greeter.Greeting("Gopher")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(g)
	// Output: Hello, Gopher!
}

func ExampleGreeting_error() {
	_, err := greeter.Greeting("   ")
	fmt.Println(err)
	// Output: name must not be empty
}
```

Create `greeter_test.go` (an internal unit test, so the package is still verified
by an assertion and not only by the examples):

```go
package greeter

import (
	"errors"
	"testing"
)

func TestGreetingRejectsBlank(t *testing.T) {
	t.Parallel()

	if _, err := Greeting("   "); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("Greeting(blank) err = %v, want ErrEmptyName", err)
	}
	if g, err := Greeting("Ada"); err != nil || g != "Hello, Ada!" {
		t.Fatalf("Greeting(Ada) = %q, %v; want %q, nil", g, err, "Hello, Ada!")
	}
}
```

Run the gate:

```bash
gofmt -l .
go vet ./...
go build ./...
go test -count=1 -race ./...
```

`go test` reports the examples alongside the unit test, each pass proving its
`// Output:` still matches. To read them as documentation:

```bash
go doc example.com/greetdoc Greeting
```

## Review

The examples are correct when each one's printed stdout matches its `// Output:`
comment exactly — trailing spaces and line breaks included, since the harness
compares after a trim of surrounding whitespace but not of interior formatting.
The mistake this trains you out of: treating a demo `main` as documentation. A
`main` prints whatever it prints and never fails CI; an `Example` with
`// Output:` *is* checked on every `go test`, so it stays true. Keep examples in
the external `greeter_test` package so they exercise only the public API, and use
the `ExampleFn_suffix` naming so godoc groups variants under one symbol. Break a
comment deliberately once to see the failure, then restore it — that is the whole
value proposition made visible.

## Resources

- [Testable Examples in Go](https://go.dev/blog/examples) — the Go blog on `Example` functions, `// Output:`, and naming.
- [`testing` package: Examples](https://pkg.go.dev/testing#hdr-Examples) — the exact rules for output matching and example naming.
- [`go doc` command](https://pkg.go.dev/cmd/go#hdr-Show_documentation_for_package_or_symbol) — how examples surface as documentation.

---

Back to [02-cmd-binary-imports-internal-package.md](02-cmd-binary-imports-internal-package.md) | Next: [04-add-and-verify-third-party-dependency.md](04-add-and-verify-third-party-dependency.md)
