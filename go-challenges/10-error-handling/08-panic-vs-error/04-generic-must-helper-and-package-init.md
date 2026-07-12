# Exercise 4: Generic Must[T] and Fail-Fast Package Initialization

`regexp.MustCompile` and `template.Must` encode one rule: if a *constant* the
developer controls fails to parse, the program is misbuilt and must never start.
A generic `Must[T any](v T, err error) T` captures that pattern once, and this
module uses it to initialize package assets so a broken constant crashes at
startup instead of serving broken responses — while the runtime paths keep
returning errors.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
mustinit/                    independent module: example.com/mustinit
  go.mod                     go 1.26
  mustinit.go                Must[T]; package-level emailRe and greeting template; ValidateEmail, Render
  cmd/
    demo/
      main.go                runnable demo: init assets used at runtime
  mustinit_test.go           Must returns value / panics on err; assets compiled; Must-vs-Compile table
```

Files: `mustinit.go`, `cmd/demo/main.go`, `mustinit_test.go`.
Implement: `Must[T any](v T, err error) T`; package-level `var emailRe = Must(regexp.Compile(...))` and a `var greeting = template.Must(template.New(...).Parse(...))`; a runtime `ValidateEmail(string) error` and `Render(name string) (string, error)`.
Test: `Must` returns the value on nil error and panics on a non-nil error; the package assets are non-nil and usable; a table proving `Must(regexp.Compile(bad))` panics while the plain `regexp.Compile(bad)` path returns an error.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/08-panic-vs-error/04-generic-must-helper-and-package-init/cmd/demo
cd go-solutions/10-error-handling/08-panic-vs-error/04-generic-must-helper-and-package-init
go mod edit -go=1.26
```

### Why Must is correct at init and wrong at runtime

`Must[T any](v T, err error) T` collapses the two-value `(T, error)` result of a
constructor into a single `T`, panicking if the error is non-nil. That is
intolerable for anything that can fail on valid usage — but it is exactly right for
a *package-level variable initialized from a constant*. The input is fixed at
compile time; the only way `Must` can panic is if the developer shipped a broken
constant (a regexp that does not compile, a template with a syntax error). In that
case there is no sensible runtime behavior: the program cannot do its job, so
crashing during package initialization — before it accepts a single request — is
the safest possible outcome. A regexp that failed to compile but was ignored would
silently match nothing and let every malformed email through.

The trap is using `Must` on dynamic input. `Must(regexp.Compile(userPattern))`
turns a routine bad request into a process crash: a user typing a malformed regex
takes down the service. The rule is mechanical: `Must` is for trusted, constant,
init-time (and test-setup) input only. Anything touching runtime or user data uses
the error-returning `regexp.Compile` / `template.Parse` and handles the error.

The `emailRe` and `greeting` vars are the init-time side; `ValidateEmail` and
`Render` are the runtime side, and they never panic — they use the pre-compiled
assets and return errors for bad *input*, which is what a correct caller can hit.

Create `mustinit.go`:

```go
package mustinit

import (
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// Must collapses a (value, error) constructor result into the value, panicking
// on a non-nil error. Use it ONLY for trusted, constant, init-time input: a
// failure means the program is misbuilt and must not start.
func Must[T any](v T, err error) T {
	if err != nil {
		panic(fmt.Sprintf("mustinit.Must: %v", err))
	}
	return v
}

// Package assets built from constants at init. A broken constant crashes the
// program at startup rather than serving broken responses.
var (
	emailRe  = Must(regexp.Compile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`))
	greeting = template.Must(template.New("greeting").Parse("Hello, {{.}}!"))
)

// ValidateEmail is a runtime path: it never panics. Bad input is an expected
// failure and returns an error.
func ValidateEmail(addr string) error {
	if !emailRe.MatchString(addr) {
		return fmt.Errorf("invalid email address: %q", addr)
	}
	return nil
}

// Render is a runtime path using the pre-compiled template.
func Render(name string) (string, error) {
	var sb strings.Builder
	if err := greeting.Execute(&sb, name); err != nil {
		return "", fmt.Errorf("render greeting: %w", err)
	}
	return sb.String(), nil
}

// EmailPattern exposes the compiled pattern string for the demo/tests.
func EmailPattern() string { return emailRe.String() }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mustinit"
)

func main() {
	// The package-level assets compiled at init, or the program would have
	// crashed before main ran. Use them at runtime with error returns.
	fmt.Println("email pattern:", mustinit.EmailPattern())

	if err := mustinit.ValidateEmail("ops@example.com"); err == nil {
		fmt.Println("ops@example.com: valid")
	}
	if err := mustinit.ValidateEmail("not-an-email"); err != nil {
		fmt.Println("not-an-email:", err)
	}

	msg, _ := mustinit.Render("Ada")
	fmt.Println(msg)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
email pattern: ^[^@\s]+@[^@\s]+\.[^@\s]+$
ops@example.com: valid
not-an-email: invalid email address: "not-an-email"
Hello, Ada!
```

### Tests

`TestMustReturnsValueOnNilErr` and `TestMustPanicsOnErr` pin the helper's two
branches. `TestInitAssetsCompiled` proves the package-level assets are non-nil and
usable (if their constants had been broken, the test binary would have crashed at
init and never reached the assertions). `TestMustVsCompile` is the load-bearing
table: it proves `Must(regexp.Compile(bad))` panics while the plain
`regexp.Compile(bad)` path returns an error, encoding "Must is for trusted constant
input only".

Create `mustinit_test.go`:

```go
package mustinit

import (
	"regexp"
	"strings"
	"testing"
)

func TestMustReturnsValueOnNilErr(t *testing.T) {
	t.Parallel()
	got := Must(42, error(nil))
	if got != 42 {
		t.Fatalf("Must(42, nil) = %d, want 42", got)
	}
}

func TestMustPanicsOnErr(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Must with non-nil error did not panic")
		}
	}()
	_ = Must(regexp.Compile(`(`)) // unbalanced parenthesis: does not compile
}

func TestInitAssetsCompiled(t *testing.T) {
	t.Parallel()
	if emailRe == nil {
		t.Fatal("emailRe is nil; init failed")
	}
	if greeting == nil {
		t.Fatal("greeting template is nil; init failed")
	}
	if err := ValidateEmail("ops@example.com"); err != nil {
		t.Fatalf("ValidateEmail rejected a valid address: %v", err)
	}
	if err := ValidateEmail("nope"); err == nil {
		t.Fatal("ValidateEmail accepted an invalid address")
	}
	msg, err := Render("Grace")
	if err != nil {
		t.Fatalf("Render err = %v", err)
	}
	if msg != "Hello, Grace!" {
		t.Fatalf("Render = %q, want %q", msg, "Hello, Grace!")
	}
}

// TestMustVsCompile pins the rule: Must is for trusted constant input (it panics
// on a bad one), while the error-returning path is for dynamic input.
func TestMustVsCompile(t *testing.T) {
	t.Parallel()
	const bad = `[unterminated`

	// The error-returning path: no panic, just an error.
	if _, err := regexp.Compile(bad); err == nil {
		t.Fatal("regexp.Compile(bad) returned nil error")
	}

	// The Must path: a panic whose message names Must.
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("Must(regexp.Compile(bad)) did not panic")
			}
			if msg, ok := r.(string); !ok || !strings.Contains(msg, "Must") {
				t.Fatalf("panic value %v does not mention Must", r)
			}
		}()
		_ = Must(regexp.Compile(bad))
	}()
}
```

## Review

The design is correct when the init-time and runtime paths stay separated. `Must`
panics only on the constant assets, so a broken regexp or template crashes the
program at package initialization — before it serves anything — which is strictly
safer than compiling nothing and silently letting bad input through. The runtime
functions `ValidateEmail` and `Render` never panic; a malformed email is an
expected failure and returns an error. The `TestMustVsCompile` table is the guard
against the classic misuse: `Must(regexp.Compile(userInput))` would turn a bad
request into an outage, so the panic there is the *anti*-pattern demonstrated, not
the recommendation. Run `go vet ./...` — vet flags `regexp` and `template` misuse
in some cases and confirms the constants are well-formed.

## Resources

- [`regexp.MustCompile`](https://pkg.go.dev/regexp#MustCompile) — the canonical fail-fast-at-init constructor.
- [`text/template.Must`](https://pkg.go.dev/text/template#Must) — the same pattern for templates.
- [Go generics: type parameters](https://go.dev/blog/intro-generics) — how `Must[T any]` is written.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-rethrow-runtime-errors-vs-deliberate-panics.md](05-rethrow-runtime-errors-vs-deliberate-panics.md)
