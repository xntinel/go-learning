# Exercise 3: Add runnable Example functions that render on pkg.go.dev and run in CI

The usage snippets a consumer copies off pkg.go.dev are only trustworthy if the
same code runs in your CI. This exercise adds `Example` functions ending in an
`// Output:` comment, so the documentation on pkg.go.dev shows real usage *and*
`go test` executes it — failing the build the moment the documented output drifts
from actual behavior. This is how a shared library keeps its examples honest.

This module is fully self-contained: its own `go mod init`, the library, a demo,
and an `example_test.go` whose functions are simultaneously the docs and the tests.

## What you'll build

```text
publicstr/                 independent module: example.com/publicstr
  go.mod                   go 1.26
  strings.go               Slugify, Truncate; var ErrEmpty
  cmd/
    demo/
      main.go              runnable demo
  example_test.go          Example, ExampleSlugify, ExampleTruncate with // Output:
```

- Files: `strings.go`, `cmd/demo/main.go`, `example_test.go`.
- Implement: `Example` (package-level), `ExampleSlugify`, and `ExampleTruncate`, each printing results and pinning them with an `// Output:` comment.
- Test: the examples are the tests — `go test -run Example` compiles and runs them and fails if any output drifts.
- Verify: `go test -count=1 -race -run Example ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/publicstr/cmd/demo
cd ~/go-exercises/publicstr
go mod init example.com/publicstr
```

### Why examples cannot rot, and how the naming maps to rendering

An `Example` function is a first-class testing construct. When its body ends in a
comment of the form `// Output:` (followed by the expected stdout), `go test`
captures the function's standard output, compares it to that block, and fails the
test if they differ. So a documented example is not prose that drifts — it is an
assertion. Change `Slugify` to emit a different separator and `ExampleSlugify`'s
`// Output:` line no longer matches; the build breaks until the docs or the code
are reconciled. That is the entire value: the example on pkg.go.dev is provably the
same behavior CI runs.

The *naming* determines where pkg.go.dev attaches the example:

- `func Example()` — the package-level example, shown at the top of the package
  docs.
- `func ExampleSlugify()` — attached to the `Slugify` symbol.
- `func ExampleTruncate_short()` — a *named variant* of an example for
  `Truncate`; the suffix after the underscore is a lowercase-initial label, so a
  symbol can have several illustrative examples (`ExampleTruncate`,
  `ExampleTruncate_short`, `ExampleTruncate_long`).

There is also `// Unordered output:` for cases where the order of printed lines is
not deterministic (e.g. ranging a map): `go test` then compares the lines as a
set. Use ordinary `// Output:` whenever order is stable, as it is here.

Create `strings.go`:

```go
package publicstr

import (
	"errors"
	"strings"
	"unicode"
)

// ErrEmpty is returned when the input reduces to no slug-able characters.
var ErrEmpty = errors.New("publicstr: empty string")

// Slugify converts s to a URL-safe slug: lowercase, ASCII alphanumerics only,
// non-alphanumeric runs collapsed to a single hyphen, edges trimmed.
func Slugify(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", ErrEmpty
	}
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case unicode.IsSpace(r), r == '-', r == '_':
			if !prevHyphen && b.Len() > 0 {
				b.WriteRune('-')
				prevHyphen = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return "", ErrEmpty
	}
	return out, nil
}

// Truncate returns s bounded to at most n bytes; when longer, "..." is appended
// and counts toward the limit.
func Truncate(s string, n int) (string, error) {
	if s == "" {
		return "", ErrEmpty
	}
	if n <= 3 {
		return strings.Repeat(".", n), nil
	}
	if len(s) <= n {
		return s, nil
	}
	return s[:n-3] + "...", nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/publicstr"
)

func main() {
	slug, _ := publicstr.Slugify("Go 1.26 Release")
	fmt.Println(slug)
	trunc, _ := publicstr.Truncate("deployment", 6)
	fmt.Println(trunc)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the `.` in `1.26` is not a separator, so it is dropped rather
than collapsed to a hyphen):

```
go-126-release
dep...
```

### The examples-as-tests

Each example prints results and pins them with `// Output:`. `go test` runs them;
change any `// Output:` line to a wrong value and the suite fails, proving it
catches documentation drift.

Create `example_test.go`:

```go
package publicstr_test

import (
	"errors"
	"fmt"

	"example.com/publicstr"
)

func Example() {
	slug, _ := publicstr.Slugify("Hello, World!")
	fmt.Println(slug)
	// Output: hello-world
}

func ExampleSlugify() {
	slug, _ := publicstr.Slugify("Foo 123 Bar")
	fmt.Println(slug)
	// Output: foo-123-bar
}

func ExampleSlugify_empty() {
	_, err := publicstr.Slugify("   ")
	fmt.Println(errors.Is(err, publicstr.ErrEmpty))
	// Output: true
}

func ExampleTruncate() {
	out, _ := publicstr.Truncate("hello world", 5)
	fmt.Printf("%q (%d bytes)\n", out, len(out))
	// Output: "he..." (5 bytes)
}

func ExampleTruncate_short() {
	out, _ := publicstr.Truncate("hi", 10)
	fmt.Println(out)
	// Output: hi
}
```

## Review

The examples are correct when `go test -run Example` passes: every printed line
matches its `// Output:` block exactly. The point of this exercise is the failure
mode — comment out the correct `// Output: hello-world` and write
`// Output: hello_world`, run the test, and watch it fail. That failure is the
guarantee: the usage a consumer copies from pkg.go.dev is provably the behavior CI
runs. Note the examples live in `package publicstr_test` (the external test
package), so they exercise only the exported surface, exactly as a consumer would;
this also means an example that reaches for an unexported helper will not compile,
keeping the examples honest about what is public. Prefer `// Output:` over
`// Unordered output:` unless the printed order is genuinely nondeterministic.

## Resources

- [testing package: Examples](https://pkg.go.dev/testing#hdr-Examples) — naming rules and the `// Output:` / `// Unordered output:` conventions.
- [Go Blog: Testable Examples in Go](https://go.dev/blog/examples) — how examples become both documentation and tests.
- [`go test` documentation](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — how example output is captured and compared.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-package-doc-and-godoc-contract.md](02-package-doc-and-godoc-contract.md) | Next: [04-functional-options-for-an-evolvable-api.md](04-functional-options-for-an-evolvable-api.md)
