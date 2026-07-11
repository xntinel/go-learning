# Exercise 1: Extract A Shared Platform Library As Its Own Module

Every service platform has a nucleus of shared code — formatting, validation, the
domain vocabulary — that many services import. This exercise elevates that
nucleus into a genuine standalone module, `example.com/platform/text`, with a
real table-driven test and the empty-input contract the platform depends on. It
compiles and passes entirely on its own, with no workspace present; that
independence is the whole point of making it a module.

## What you'll build

```text
text/                          module: example.com/platform/text
  go.mod                       go 1.26
  text.go                      package text; func Greet(name string) string
  cmd/
    demo/
      main.go                  runnable demo: greets a few names from os.Args
  text_test.go                 table-driven TestGreet (normal, empty contract, unicode) + Example
```

- Files: `text.go`, `cmd/demo/main.go`, `text_test.go`.
- Implement: `Greet(name string) string` returning `"Hello, " + name`, with the contract that `Greet("") == "Hello, "`.
- Test: a table-driven `TestGreet` covering a normal name, the empty-string contract, and a unicode name; an `Example` with `// Output:`.
- Verify: `go test -count=1 -race ./...` inside the module with no `go.work` anywhere.

Set up the module. This is one module, released on its own cadence, that other
platform modules will import by its path:

```bash
mkdir -p ~/platform/text/cmd/demo
cd ~/platform/text
go mod init example.com/platform/text
go mod edit -go=1.26
```

### Why the library is its own module

A shared library that lives inside a service's module cannot be imported by a
second service without dragging in the whole service. Promoting it to its own
module, `example.com/platform/text`, gives it an import path independent of any
consumer, its own `go.mod`, and its own release tags. The next exercises tie this
module to a consuming service through a workspace; here the discipline is simply
that the library builds, vets, and tests *in isolation*. If it needs a workspace
to compile, it is not a real module — it is a fragment. So this exercise runs the
full gate on the module alone.

`Greet` is deliberately total: it never errors and never panics, and it defines
one contract worth pinning — the empty name. `Greet("")` returns `"Hello, "`, not
`"Hello, <something>"` and not an error. Downstream services rely on that exact
behavior when a name is missing, so the test asserts it directly. The unicode
case exists to prove the function is byte-agnostic: Go strings are UTF-8 and
concatenation does not care about rune boundaries, so a multibyte name round-trips
unchanged.

Create `text.go`:

```go
// text.go
package text

// Greet builds a greeting for name. It is total: an empty name yields the
// bare prefix "Hello, " (the contract the platform's services depend on).
func Greet(name string) string {
	return "Hello, " + name
}
```

### The runnable demo

Because `cmd/demo` is a separate `package main`, it can only reach the exported
`Greet`, exactly as a consuming service would. It greets each command-line
argument, defaulting to `world` when none is given.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"os"

	"example.com/platform/text"
)

func main() {
	names := os.Args[1:]
	if len(names) == 0 {
		names = []string{"world"}
	}
	for _, name := range names {
		fmt.Println(text.Greet(name))
	}
}
```

Run it:

```bash
go run ./cmd/demo world alice
```

Expected output:

```
Hello, world
Hello, alice
```

### Tests

The test is table-driven and parallel. The empty-name row is the contract the old
verification step demanded; the unicode row proves byte-agnostic concatenation.

Create `text_test.go`:

```go
// text_test.go
package text

import (
	"fmt"
	"testing"
)

func TestGreet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"normal", "world", "Hello, world"},
		{"empty contract", "", "Hello, "},
		{"unicode", "José", "Hello, José"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Greet(tc.in); got != tc.want {
				t.Fatalf("Greet(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func ExampleGreet() {
	fmt.Println(Greet("platform"))
	// Output: Hello, platform
}
```

## Review

The module is correct when it stands alone: `go test -count=1 -race ./...` and
`go vet ./...` pass from inside `~/platform/text` with no `go.work` in any parent
directory. That isolation is what makes it a shareable module rather than a fragment
welded to one service. The empty-name row is not a formality — services call
`Greet` with a missing name and rely on `"Hello, "`, so if a future refactor makes
`Greet("")` return an error or a placeholder, this row fails and catches the
contract break. Keep `cmd/demo` reaching only the exported `Greet`; if the demo
needs an unexported field it is a sign the API is missing an accessor, not that
the field should be exported.

## Resources

- [Go Modules Reference](https://go.dev/ref/mod) — what a module is and how its path and `go.mod` define it.
- [Tutorial: Create a Go module](https://go.dev/doc/tutorial/create-module) — the canonical walkthrough for `go mod init` and a first exported function.
- [`testing` package](https://pkg.go.dev/testing) — table-driven tests, `t.Parallel`, and `Example` functions with `// Output:`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-service-consumes-library-by-import-path.md](02-service-consumes-library-by-import-path.md)
