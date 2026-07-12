# Exercise 9: Example Functions as Compile-Checked, Runnable Documentation

Documentation drifts. A README code snippet that once worked rots the moment the
API changes, and nobody notices until a new hire copies it. Go's answer is the
`Example` function: documentation that `go test` compiles and runs, comparing its
printed output against an `// Output:` comment. If the API changes, the example
stops compiling; if the behavior changes, the output comparison fails. This
exercise gives a small matcher and a small registry compile-checked examples,
including the map-iteration case that needs `// Unordered output:`.

This module is self-contained: it bundles a minimal matcher and registry. Nothing
here imports another exercise.

## What you'll build

```text
exampledocs/                       module: example.com/exampledocs
  go.mod
  matcher.go                       package exampledocs: Matcher, Match (bundled)
  registry.go                      Registry: Register, Sorted, Names (map-order)
  example_test.go                  ExampleMatcher_Match, Example, ExampleRegistry_Names (unordered)
  cmd/demo/main.go                 exercises both types
```

- Files: `matcher.go`, `registry.go`, `example_test.go`, `cmd/demo/main.go`.
- Implement: a substring matcher and a name registry with a sorted view and a map-order view.
- Test: `Example` functions with `// Output:` (deterministic) and `// Unordered output:` (map iteration), all verified by `go test`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/01-package-declaration-and-imports/09-example-tests-as-compiled-docs/cmd/demo
cd go-solutions/11-packages-and-modules/01-package-declaration-and-imports/09-example-tests-as-compiled-docs
go mod edit -go=1.26
```

### How Example functions are verified

An `Example` function is named by convention: `Example` for a package-level
example, `ExampleMatcher` for the `Matcher` type, `ExampleMatcher_Match` for the
`Match` method (the underscore separates type from method). `go test` runs each,
captures its stdout, and compares it against the trailing `// Output:` comment; a
mismatch is a test failure exactly like an assertion. Because the example is real
compiled code calling the real API, it cannot lie: rename `Match` and the example
stops building; change what `Match` returns and the output comparison fails. The
same functions are what `godoc`/`pkg.go.dev` render as the package's usage
examples, so one artifact serves as both enforced test and published documentation.

For output whose order is not deterministic — ranging a Go map, whose iteration
order is randomized — use `// Unordered output:` instead. `go test` then compares
the printed lines as a set (sorted before comparing), so the example passes
regardless of iteration order while still pinning exactly which lines appear.

First the two small types the examples document. Create `matcher.go`:

```go
package exampledocs

import (
	"bufio"
	"io"
	"strings"
)

// Result is one matching line and its one-indexed position.
type Result struct {
	LineNum int
	Line    string
}

// Matcher scans an io.Reader for lines containing Substr.
type Matcher struct {
	Substr string
}

// Match returns every line of r that contains Substr.
func (m *Matcher) Match(r io.Reader) []Result {
	sc := bufio.NewScanner(r)
	var out []Result
	n := 0
	for sc.Scan() {
		n++
		if strings.Contains(sc.Text(), m.Substr) {
			out = append(out, Result{LineNum: n, Line: sc.Text()})
		}
	}
	return out
}
```

Create `registry.go`. `Sorted` returns names in a deterministic order (for an
`// Output:` example); `Names` ranges the map directly, so its order is
non-deterministic (for an `// Unordered output:` example).

```go
package exampledocs

import "sort"

// Registry holds a set of registered names.
type Registry struct {
	names map[string]bool
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{names: make(map[string]bool)}
}

// Register adds a name to the set.
func (r *Registry) Register(name string) {
	r.names[name] = true
}

// Sorted returns the registered names in ascending order (deterministic).
func (r *Registry) Sorted() []string {
	out := make([]string, 0, len(r.names))
	for n := range r.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Names returns the registered names in map-iteration order (non-deterministic).
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.names))
	for n := range r.names {
		out = append(out, n)
	}
	return out
}
```

### The demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/exampledocs"
)

func main() {
	m := &exampledocs.Matcher{Substr: "err"}
	for _, r := range m.Match(strings.NewReader("ok\nerror: disk\nfine")) {
		fmt.Printf("%d:%s\n", r.LineNum, r.Line)
	}

	reg := exampledocs.NewRegistry()
	reg.Register("json")
	reg.Register("gob")
	fmt.Println("sorted:", reg.Sorted())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
2:error: disk
sorted: [gob json]
```

### The examples

The example file is `package exampledocs` (in-package) so it uses the exported API
the way documentation would. `ExampleMatcher_Match` and the package-level `Example`
use deterministic `// Output:`; `ExampleRegistry_Names` uses `// Unordered output:`
because it prints map-iteration order.

Create `example_test.go`:

```go
package exampledocs

import (
	"fmt"
	"strings"
)

func Example() {
	m := &Matcher{Substr: "hello"}
	res := m.Match(strings.NewReader("hello world"))
	fmt.Println(res[0].LineNum, res[0].Line)
	// Output: 1 hello world
}

func ExampleMatcher_Match() {
	m := &Matcher{Substr: "500"}
	for _, r := range m.Match(strings.NewReader("GET /a 200\nPOST /b 500\nGET /c 500")) {
		fmt.Printf("%d:%s\n", r.LineNum, r.Line)
	}
	// Output:
	// 2:POST /b 500
	// 3:GET /c 500
}

func ExampleRegistry_Names() {
	r := NewRegistry()
	r.Register("json")
	r.Register("gob")
	r.Register("xml")
	for _, n := range r.Names() {
		fmt.Println(n)
	}
	// Unordered output:
	// json
	// gob
	// xml
}

func ExampleRegistry_Sorted() {
	r := NewRegistry()
	r.Register("json")
	r.Register("gob")
	fmt.Println(r.Sorted())
	// Output: [gob json]
}
```

If you were to change an `// Output:` block to the wrong value — say
`// Output: 2 hello world` on `Example` — `go test` would fail with a diff between
"got" and "want", which is precisely the enforcement: the example cannot silently
disagree with the code. That is why examples are documentation you can trust.

## Review

The examples are correct when every `// Output:` block matches a real run and the
one `// Unordered output:` block lists exactly the lines the map produces in any
order. The value is that these are not comments — `go test` executes them, so they
are tests that also render as docs on `pkg.go.dev`, and they can never drift from
the API without breaking the build or the test. The single trap to internalize:
use `// Unordered output:` whenever the printed order comes from a map (or any
non-deterministic source), because a plain `// Output:` block would flake as
iteration order changes. Keep examples small and realistic; they are the first
thing a reader copies. Run `go test -race -run Example` to execute just the
examples.

## Resources

- [`testing` package: Examples](https://pkg.go.dev/testing#hdr-Examples) — naming conventions, `// Output:` and `// Unordered output:`.
- [Go Blog: Testable Examples in Go](https://go.dev/blog/examples) — how examples work as tests and documentation.
- [Go Spec: Package clause](https://go.dev/ref/spec#Package_clause) — the in-package test that lets examples use the exported API.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-embed-migrations-fs.md](08-embed-migrations-fs.md) | Next: [../02-exported-vs-unexported/00-concepts.md](../02-exported-vs-unexported/00-concepts.md)
