# Exercise 3: Migrating Constants and Type Aliases Safely

Functions are not the only things that move. When you relocate a severity type and
its named levels from a legacy `logging` package into a focused `severity`
package, `//go:fix inline` migrates references to both — but only under strict
rules: a constant is eligible only if it refers to another *named* constant, and a
type is eligible only if it is an *alias*. This exercise migrates the eligible
cases and shows, in prose, why the ineligible ones are silently skipped.

This module is self-contained: the successor package, the deprecated re-export
package, a consumer, a demo, and tests. Nothing here imports another exercise.

## What you'll build

```text
units/                         independent module: example.com/units
  go.mod                       go 1.26
  severity/
    severity.go                type Level + Info/Warn/Error + String (the successors)
  logging/
    logging.go                 type Level alias + Info/Warn/Error consts (Deprecated, //go:fix inline)
    logging_test.go            value-equality + alias-identity tests + Example
  consumer/
    consumer.go                uses logging.Level and logging.Warn (what go fix rewrites)
  cmd/
    demo/
      main.go                  runnable demo
```

- Files: `severity/severity.go`, `logging/logging.go`, `consumer/consumer.go`, `cmd/demo/main.go`, `logging/logging_test.go`.
- Implement: `severity.Level` with `Info`/`Warn`/`Error` and a `String`; a `Deprecated`, `//go:fix inline` type alias `logging.Level = severity.Level` and constants `logging.Info/Warn/Error` that each refer to the matching `severity` constant.
- Test: a test asserting each re-exported constant equals its target value and that the alias is the identical type (interchangeable); an `Example`.
- Verify: `go test -count=1 -race ./...`, then `go fix -diff ./...`.

Set up the module:

```bash
mkdir -p go-solutions/48-modern-go-language-and-stdlib/16-go-fix-inline-modernization/03-inline-constants-and-type-aliases/severity go-solutions/48-modern-go-language-and-stdlib/16-go-fix-inline-modernization/03-inline-constants-and-type-aliases/logging go-solutions/48-modern-go-language-and-stdlib/16-go-fix-inline-modernization/03-inline-constants-and-type-aliases/consumer go-solutions/48-modern-go-language-and-stdlib/16-go-fix-inline-modernization/03-inline-constants-and-type-aliases/cmd/demo
cd go-solutions/48-modern-go-language-and-stdlib/16-go-fix-inline-modernization/03-inline-constants-and-type-aliases
go mod edit -go=1.26
```

### The successor package

The new home defines the real type, the named constants, and a `String` method so
levels print readably.

Create `severity/severity.go`:

```go
// Package severity is the successor home for the log-severity type and its
// levels, moved out of package logging.
package severity

// Level is a log severity.
type Level int

// The severity levels, in increasing order.
const (
	Info Level = iota
	Warn
	Error
)

func (l Level) String() string {
	switch l {
	case Info:
		return "INFO"
	case Warn:
		return "WARN"
	case Error:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}
```

### The deprecated re-exports, and the eligibility rules

The old package keeps compatibility shims that `go fix` can inline. Two rules
govern eligibility, and both are satisfied deliberately here:

- The type shim is an *alias*: `type Level = severity.Level`. An alias is
  genuinely the same type as its target, so replacing `logging.Level` with
  `severity.Level` everywhere cannot change meaning. A *defined* type
  (`type Level severity.Level`, no `=`) would be a distinct type and is not
  eligible — the directive on it is silently ignored.
- Each constant shim refers to another *named* constant:
  `const Warn = severity.Warn`. That is what makes it inlinable. A constant whose
  value is a *computed expression* — say `const Warn = 1` or
  `const Timeout = 30 * time.Second` — is not eligible; the directive is silently
  not applied.

The directive has three legal placements for constants: before a single `const`
declaration, before one member inside a `const (...)` group, or before an entire
group (applying to every member). This file uses the per-member form inside a
group.

Create `logging/logging.go`:

```go
// Package logging is the pre-split home of the severity type and levels. The
// declarations here are Deprecated, inlinable re-exports of package severity.
package logging

import "example.com/units/severity"

// Level is the log severity type.
//
// Deprecated: moved to package severity. Use [severity.Level]. Level is an alias
// kept for one release so consumers can run `go fix`.
//
//go:fix inline
type Level = severity.Level

// The severity levels, re-exported from package severity.
//
// Deprecated: use severity.Info, severity.Warn, and severity.Error.
const (
	//go:fix inline
	Info = severity.Info

	//go:fix inline
	Warn = severity.Warn

	//go:fix inline
	Error = severity.Error
)
```

### The consumer

The consumer names both the aliased type and a re-exported constant. `go fix`
rewrites both references to the `severity` package and swaps the import.

Create `consumer/consumer.go`:

```go
// Package consumer still refers to logging.Level and logging.Warn. Running
// `go fix ./...` rewrites both to package severity and swaps the import.
package consumer

import (
	"fmt"

	"example.com/units/logging"
)

// Describe renders a severity for a log line.
func Describe(l logging.Level) string {
	return fmt.Sprintf("level=%s", l)
}

// DefaultLevel is the level used when none is configured.
func DefaultLevel() logging.Level {
	return logging.Warn
}
```

### The runnable demo

The demo prints a re-exported level and shows that a `logging.Level` and a
`severity.Level` are interchangeable, which is exactly what makes the alias safe to
inline.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/units/logging"
	"example.com/units/severity"
)

func main() {
	l := logging.Warn
	fmt.Printf("logging.Warn = %s (%d)\n", l, l)

	var s severity.Level = logging.Error // alias: freely interchangeable
	fmt.Printf("severity.Level from logging.Error = %s (%d)\n", s, s)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
logging.Warn = WARN (1)
severity.Level from logging.Error = ERROR (2)
```

### Proving the re-exports are faithful

The test asserts two things the inline relies on: each re-exported constant equals
its target value, and the alias is the identical type (a value flows between
`logging.Level` and `severity.Level` with no conversion).

Create `logging/logging_test.go`:

```go
package logging

import (
	"fmt"
	"testing"

	"example.com/units/severity"
)

func TestConstantsEqualTargets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		old  Level
		want severity.Level
	}{
		{"info", Info, severity.Info},
		{"warn", Warn, severity.Warn},
		{"error", Error, severity.Error},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.old != tc.want {
				t.Fatalf("%s = %d, want %d", tc.name, tc.old, tc.want)
			}
		})
	}
}

func TestAliasIsIdentical(t *testing.T) {
	t.Parallel()
	// Because Level is an alias (type Level = severity.Level), a logging.Level
	// and a severity.Level are the same type, interchangeable with no conversion.
	var a Level = severity.Error
	var b severity.Level = a
	if b != severity.Error {
		t.Fatalf("alias round-trip = %v, want %v", b, severity.Error)
	}
}

func Example() {
	fmt.Println(Warn)
	// Output: WARN
}
```

### The migration, as a diff

From the module root:

```bash
go fix -diff ./...
```

Every reference to the alias and the re-exported constant is rewritten to the
successor package, and the import is swapped:

```text
--- consumer/consumer.go (old)
+++ consumer/consumer.go (new)
 import (
 	"fmt"
 
-	"example.com/units/logging"
+	"example.com/units/severity"
 )
 
 // Describe renders a severity for a log line.
-func Describe(l logging.Level) string {
+func Describe(l severity.Level) string {
 	return fmt.Sprintf("level=%s", l)
 }
 
 // DefaultLevel is the level used when none is configured.
-func DefaultLevel() logging.Level {
-	return logging.Warn
+func DefaultLevel() severity.Level {
+	return severity.Warn
 }
```

### Why the ineligible forms are skipped

It is worth seeing the two shims that `go fix` will *not* migrate, so you do not
ship a directive that silently does nothing:

```go
// NOT eligible: the value is a computed expression, not a reference to a named
// constant. The directive is silently ignored.
//
//go:fix inline
const MaxRetries = 3 * 2

// NOT eligible: this is a defined type, not an alias (no `=`). Only aliases can
// be inlined. The directive is silently ignored.
//
//go:fix inline
type Level severity.Level
```

To make the first eligible, introduce a named constant in the successor package
and forward to it (`const MaxRetries = defaults.MaxRetries`). To make the second
eligible, write it as an alias (`type Level = severity.Level`). The rule is not
arbitrary: a computed constant has no single named target to redirect to, and a
defined type is a genuinely different type whose substitution could change program
behavior — so the inliner, which never changes behavior, declines both.

## Review

The re-exports are correct when each constant equals its target and the alias is
type-identical, which `TestConstantsEqualTargets` and `TestAliasIsIdentical`
pin down; those two properties are exactly what let `go fix` replace
`logging.Warn` with `severity.Warn` and `logging.Level` with `severity.Level`
without a single conversion or behavior change.

The mistakes here are eligibility mistakes. Annotating a computed constant
(`const X = 30 * time.Second`) or a defined type (`type X severity.Level`) looks
right but does nothing — move the literal to a named constant and use `type X = Y`
so the shims qualify. And, as in every migration, keep the deprecated package for
a release after annotating it; the directive enables the rewrite but does not
perform it. Confirm with `go test -race ./...` and preview with
`go fix -diff ./...`.

## Resources

- [Automating your API migrations with go fix inline](https://go.dev/blog/inliner) — the constant and type-alias inline forms and their restrictions.
- [`gofix` analyzer](https://pkg.go.dev/golang.org/x/tools/gopls/internal/analysis/gofix) — the eligibility rules and const-group placement forms.
- [Go 1.9 type aliases](https://go.dev/blog/alias-names) — why `type X = Y` is the same type, and a defined type is not.

---

Back to [02-migrating-consumers-across-a-major-version.md](02-migrating-consumers-across-a-major-version.md) | Next: [04-running-the-modernizer-suite-in-ci.md](04-running-the-modernizer-suite-in-ci.md)
