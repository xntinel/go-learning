# Exercise 1: A Typed Library with a Sentinel Error

Every `go get` and `go install` operation in this lesson acts on a real package,
so we build that package first: a small `hello` library with a sentinel error, a
table-driven parallel test, and an `Example` that doubles as documentation. It is
deliberately production-shaped — trimmed input, a typed error you can match with
`errors.Is`, and a test that pins both the happy path and the failure path.

This module is fully self-contained: its own `go mod init`, its own code, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
greetlib/                       independent module: example.com/greetlib
  go.mod                        module path + go version
  internal/hello/
    hello.go                    ErrEmptyName sentinel; Greet(name) (string, error)
    hello_test.go               table-driven parallel test + ExampleGreet
  cmd/demo/
    main.go                     runnable demo printing greetings
```

Files: `internal/hello/hello.go`, `internal/hello/hello_test.go`, `cmd/demo/main.go`.
Implement: `Greet(name string) (string, error)` that trims whitespace and returns `ErrEmptyName` on empty input.
Test: table cases (plain, trims, empty, whitespace) asserting the sentinel via `errors.Is`, plus `ExampleGreet`.
Verify: `go test -count=1 -race ./...`

### Why a sentinel error, not a string

The empty-name case is a *predictable* outcome that a caller may want to handle
differently from an unexpected failure — for a CLI, print a usage hint; for an
HTTP handler, return 400 rather than 500. Encoding it as a package-level sentinel
(`var ErrEmptyName = errors.New(...)`) lets callers branch on
`errors.Is(err, hello.ErrEmptyName)` without string-matching the message, which
means the message text stays a presentation detail you can reword freely. The
library never calls `os.Exit` or prints; it returns values and errors, so it is
testable in-process and reusable by both the CLI and the tests. Trimming
whitespace before the empty check makes `"  "` and `"\t\n"` behave like `""` — a
single definition of "empty" that the test pins explicitly.

Create `internal/hello/hello.go`:

```go
// internal/hello/hello.go
package hello

import (
	"errors"
	"fmt"
	"strings"
)

// ErrEmptyName is returned by Greet when the name is empty after trimming.
// Callers match it with errors.Is rather than comparing message strings.
var ErrEmptyName = errors.New("name must not be empty")

// Greet returns a greeting for name. Surrounding whitespace is trimmed; a name
// that is empty after trimming is rejected with ErrEmptyName.
func Greet(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ErrEmptyName
	}
	return fmt.Sprintf("Hello, %s!", trimmed), nil
}
```

### The demo

The demo prints a couple of greetings so you can watch the library work. It is a
convenience, not the verification — that is the test's job.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/greetlib/internal/hello"
)

func main() {
	for _, name := range []string{"World", "  Gopher  "} {
		g, err := hello.Greet(name)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Println(g)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
Hello, World!
Hello, Gopher!
```

### The test

The test is table-driven and parallel. The happy path asserts exact string
output; the failure path asserts the sentinel with `errors.Is`, never a string
compare. `ExampleGreet` is verified by `go test` against its `// Output:` comment,
so the package's documentation cannot drift from its behavior.

Create `internal/hello/hello_test.go`:

```go
// internal/hello/hello_test.go
package hello

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
		{name: "plain", input: "Gopher", want: "Hello, Gopher!"},
		{name: "trims", input: "  Gopher  ", want: "Hello, Gopher!"},
		{name: "empty", input: "", wantErr: ErrEmptyName},
		{name: "whitespace", input: "  \t\n", wantErr: ErrEmptyName},
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

func ExampleGreet() {
	g, _ := Greet("Gopher")
	fmt.Println(g)
	// Output: Hello, Gopher!
}
```

## Review

The library is correct when `Greet` is a pure function of its input: it returns
`ErrEmptyName` exactly when the trimmed name is empty and a formatted greeting
otherwise, with no I/O and no process exit. The two traps are matching the
sentinel by message text instead of `errors.Is` (which breaks the moment you
reword the message) and checking emptiness before trimming (which lets `"  "`
through). Run `go vet ./...` and `gofmt -l .` — both must be clean — and
`go test -race ./...` to confirm the table and the `Example` pass. This `hello`
package is the artifact the next exercises install tools around and add
dependencies to.

## Resources

- [`errors` package](https://pkg.go.dev/errors) — `errors.New`, `errors.Is`, and sentinel-error semantics.
- [`strings.TrimSpace`](https://pkg.go.dev/strings#TrimSpace) — exact trimming behavior.
- [Testable Examples in Go](https://go.dev/blog/examples) — how `Example` functions and `// Output:` comments are verified.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-demo-cli-with-run-pattern.md](02-demo-cli-with-run-pattern.md)
