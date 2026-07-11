# 6. Build Constraints and File Suffixes

Build constraints are a compile-time mechanism, not a runtime one. The Go toolchain decides which source files enter a build before any code is compiled. Getting this wrong means silent omissions: a function you think is included simply is not, and you get a linker error with no helpful message about which constraint caused the exclusion.

The two mechanisms -- file-name suffixes and `//go:build` directives -- look similar but differ in precedence, expressiveness, and error behavior. Understanding both is required for writing portable libraries and feature-toggled binaries.

## Concepts

### How the Go toolchain selects files

Before parsing or compiling, `go build` runs a two-pass filter over every `.go` file in the package directory:

1. File-name suffix filter: if the base name (before `.go`) ends in `_GOOS`, `_GOARCH`, or `_GOOS_GOARCH`, the file is only included when the current `GOOS`/`GOARCH` match. This is checked mechanically; no `//go:build` line is required.

2. Build constraint filter: if the file contains a `//go:build` line in the preamble (before `package`), it is evaluated as a boolean expression. The file is excluded unless the expression evaluates to true.

Both filters must pass. A file named `foo_linux.go` with `//go:build !linux` at the top is excluded on Linux by the constraint and on every other OS by the suffix. That combination produces a file that is never compiled -- a silent dead-weight file.

### File-name suffixes

The recognized suffixes are defined in the `go/build` package. Common GOOS values:

- `_linux`, `_darwin`, `_windows`, `_freebsd`, `_openbsd`

Common GOARCH values:

- `_amd64`, `_arm64`, `_386`, `_arm`

Combined: `_linux_amd64` (both must match).

The suffix is stripped when determining the package identity. `platform_linux.go` and `platform_darwin.go` can both declare `package platform` and the toolchain merges whichever one compiles.

There is no error if a suffix file is excluded. The toolchain silently skips it. This means you must provide a fallback file covering every platform without a dedicated suffix file, or you will get undefined-symbol errors at link time.

### //go:build directives

The directive syntax (canonical since Go 1.17):

```
//go:build expr
```

Where `expr` is a boolean expression using:

- Tag names: bare identifiers (`linux`, `debug`, `enterprise`, `ignore`)
- Logical operators: `&&` (AND), `||` (OR), `!` (NOT)
- Parentheses for grouping

Built-in tags include all `GOOS` and `GOARCH` values, `go1.N` version tags, and `cgo`. User-defined tags are activated with `-tags "tag1 tag2"` at build time.

The directive must appear in the preamble: before the `package` statement, separated from it by at least one blank line. A `//go:build` directive that appears after `package` is a compile error in modern Go: `go build` reports `misplaced compiler directive` and `go vet` reports `misplaced //go:build comment` (exit code 1). The file does not silently compile; the build fails. (The legacy `// +build` comment form placed after `package` is silently ignored, but the modern `//go:build` form is not.)

### The "always provide a fallback" rule

Custom tags create an asymmetry: the tag-enabled file is included only when the tag is active, but the symbols it defines are also needed when the tag is absent. This requires a paired fallback file with the negated constraint:

```
feature_debug.go     //go:build debug
feature.go           //go:build !debug
```

If you omit the fallback, every build without `-tags debug` fails with "undefined: someFunc". The compiler does not tell you which build constraint caused the omission -- it just says the symbol is missing.

### Trade-offs vs. runtime feature flags

Build constraints produce smaller binaries because excluded code is never compiled. The trade-off is that the binary is not reconfigurable at runtime. Use build constraints for:

- Platform-specific syscall wrappers
- Debug instrumentation (logging, tracing) that must not ship in production
- Optional external dependencies (CGO-based vs. pure-Go fallback)

Use runtime flags when behavior must be configurable without recompilation.

### Failure modes

- **Duplicate symbol**: two files both define the same function for the same build environment. The compiler reports a redeclaration error.
- **Missing symbol**: no file provides a function for the current build environment. The linker reports "undefined: funcName".
- **Directive misplaced**: `//go:build` placed after `package` is a compile error: `go build` exits with `misplaced compiler directive` and `go vet` reports `misplaced //go:build comment`. The build fails; the file is not silently included. A space after `//` (`// go:build`) is a separate mistake: the toolchain does not recognize it as a build constraint and the file is always compiled.
- **Old syntax confusion**: `// +build` was the pre-1.17 syntax. Modern Go treats `//go:build` as canonical. `go vet` reports an error if you use `// +build` without a matching `//go:build` line.

## Exercises

### Exercise 1: Platform abstraction library with file-suffix dispatch

Build a `platform` package. The public API lives in a single file; implementations are in suffix-named and constraint-guarded files so that exactly one set of implementations compiles per platform.

```go.mod
module build-constraints

go 1.26
```

Create `platform/platform.go`:

```go
package platform

// Name returns the current operating system name.
func Name() string { return platformName() }

// ConfigDir returns the platform-appropriate configuration directory.
func ConfigDir() string { return configDir() }
```

Create `platform/platform_darwin.go`:

```go
package platform

func platformName() string { return "macOS (Darwin)" }

func configDir() string { return "/Library/Application Support/myapp" }
```

Create `platform/platform_linux.go`:

```go
package platform

func platformName() string { return "Linux" }

func configDir() string { return "/etc/myapp" }
```

Create `platform/platform_other.go`:

```go
//go:build !linux && !darwin

package platform

func platformName() string { return "other" }

func configDir() string { return "/tmp/myapp" }
```

`platform_other.go` carries a `//go:build` constraint in addition to its generic name. Without the constraint, on darwin both `platform_darwin.go` (selected by suffix) and `platform_other.go` (no constraint, always included) would compile simultaneously, causing duplicate symbol errors for `platformName` and `configDir`. The `!linux && !darwin` expression excludes the fallback precisely on the platforms that have a suffix file.

Create `platform/platform_test.go`:

```go
package platform

import "testing"

func TestNameNonEmpty(t *testing.T) {
	got := Name()
	if got == "" {
		t.Fatal("Name() returned empty string")
	}
}

func TestConfigDirNonEmpty(t *testing.T) {
	got := ConfigDir()
	if got == "" {
		t.Fatal("ConfigDir() returned empty string")
	}
}
```

### Exercise 2: Feature toggle via custom build tag

Add a `feature` package with a debug-logging implementation enabled only with `-tags debug`.

Create `feature/feature.go`:

```go
//go:build !debug

package feature

import "io"

// Logger returns a writer that discards all output when the debug tag is absent.
func Logger() io.Writer { return io.Discard }

// DebugEnabled reports whether the debug tag was active at compile time.
const DebugEnabled = false
```

Create `feature/feature_debug.go`:

```go
//go:build debug

package feature

import (
	"io"
	"os"
)

// Logger returns os.Stderr when the debug tag is active.
func Logger() io.Writer { return os.Stderr }

// DebugEnabled reports whether the debug tag was active at compile time.
const DebugEnabled = true
```

Create `feature/feature_test.go`:

```go
package feature

import "testing"

func TestLoggerNotNil(t *testing.T) {
	if Logger() == nil {
		t.Fatal("Logger() must not return nil")
	}
}

func TestDebugEnabledIsBool(t *testing.T) {
	// DebugEnabled is a compile-time constant; confirm it is accessible.
	_ = DebugEnabled
}
```

### Exercise 3: Main program

Create `main.go`:

```go
package main

import (
	"fmt"

	"build-constraints/feature"
	"build-constraints/platform"
)

func main() {
	fmt.Println("platform:", platform.Name())
	fmt.Println("config dir:", platform.ConfigDir())
	fmt.Println("debug:", feature.DebugEnabled)
	fmt.Fprintln(feature.Logger(), "startup complete")
}
```

## Common Mistakes

Wrong: naming the fallback file without a constraint when suffix files exist.

In `platform/platform_other.go` (no `//go:build` line):

```
package platform

func platformName() string { return "other" }
func configDir() string    { return "/tmp/myapp" }
```

What happens: on darwin, both `platform_darwin.go` (selected by suffix) and `platform_other.go` (no constraint, always included) compile. The compiler reports "platformName redeclared in this block".

Fix: add `//go:build !linux && !darwin` to the fallback.

---

Wrong: using a space in the directive.

```
// go:build debug
```

What happens: the toolchain does not recognize `// go:build` (space after `//`) as a build constraint. The file is always compiled regardless of tags. If you also have a negated fallback file, both compile simultaneously and you get redeclaration errors.

Fix: write `//go:build debug` with no space between `//` and `go`.

---

Wrong: putting the directive after the package statement.

In `feature/feature_debug.go`:

```
package mypackage

//go:build debug
```

What happens: in modern Go this is a compile error, not a silent no-op. `go build` exits with `misplaced compiler directive` and `go vet` reports `misplaced //go:build comment` (both exit code 1). The build fails; the file is not silently compiled without the tag.

Fix: the `//go:build` line must appear before `package`, separated from it by a blank line.

---

Wrong: declaring a constant in only one branch of a tag pair.

In `feature/feature_debug.go` with only:

```
//go:build debug

package feature

const DebugEnabled = true
```

What happens: without `-tags debug`, `DebugEnabled` is undefined. Any code referencing it fails to compile with "undefined: DebugEnabled".

Fix: declare `DebugEnabled` in both the tag-enabled and fallback files, each with the appropriate value.

## Verification

Run from the module root:

```bash
go vet ./...
go build ./...
go build -tags debug ./...
go test -count=1 -race ./...
```

All four commands must complete without error. `go build -tags debug` verifies that the debug feature path compiles cleanly on its own.

## Summary

- File suffixes (`_linux.go`, `_darwin.go`) are implicit constraints evaluated before parsing; no directive is needed
- `//go:build expr` is the canonical directive syntax since Go 1.17; it must appear before `package`
- Custom tags are activated with `-tags "t1 t2"` and must be paired with negated fallback files
- A file without any suffix or directive is always compiled; add a constraint to exclude it when suffix files exist for specific platforms
- Build constraints have zero runtime cost -- excluded files are not compiled, linked, or present in the binary

## What's Next

Next: [Exercise 29.7: Link-Time Variable Injection](../07-link-time-variable-injection/07-link-time-variable-injection.md).

## Resources

- [Build constraints specification](https://pkg.go.dev/cmd/go#hdr-Build_constraints)
- [go/build package documentation](https://pkg.go.dev/go/build)
- [Go 1.17 release notes: new //go:build syntax](https://go.dev/doc/go1.17#go-command)
- [GOOS and GOARCH values](https://go.dev/doc/install/source#environment)
- [go vet buildtag analyzer](https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/buildtag)
