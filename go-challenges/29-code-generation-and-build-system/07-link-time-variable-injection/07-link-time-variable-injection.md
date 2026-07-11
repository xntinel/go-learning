# 7. Link-Time Variable Injection

Every production binary should identify itself: its version, the commit it was built from, and when it was compiled. The naive approach -- hardcoding a version string in source -- means a code change for every release. Link-time variable injection via `-ldflags -X` eliminates this: the version is stamped into the binary at link time by the build system, not by a developer editing a file.

The technique is simple but has sharp edges. Only `string`-typed package-level `var` declarations can be injected. The full import path of the package must be used, not the directory path. And the default values in source must be sensible for local development, because most developers run `go build` without any ldflags.

## Concepts

### How -ldflags -X works

The Go linker (`cmd/link`) accepts a `-X` flag that sets the value of a named symbol:

```
-X 'importpath.VarName=value'
```

At link time, the linker replaces the initial value of the named string variable with the given string. This happens after compilation; the source file is not modified. The resulting binary contains the injected value as if it had been written there.

### Requirements for injection

Three conditions must all hold:

1. The variable must be a package-level `var` declaration.
2. The type must be `string` (not `[]byte`, not a named type over string, not `const`).
3. The import path must match exactly what `go list -m` and `go list ./...` would report.

A `const` cannot be injected -- constants are inlined by the compiler before the linker runs. An `int` variable cannot be injected -- `-X` always produces a string. If you need a numeric version at runtime, inject a string and parse it with `strconv.Atoi`.

### The package path

The path used in `-X` is the full import path, not the file system path. For a module `github.com/example/myapp` with a file at `internal/buildinfo/info.go` declaring `package buildinfo`, the correct flag is:

```
-X 'github.com/example/myapp/internal/buildinfo.Version=1.2.3'
```

A common mistake is using the directory name (`buildinfo.Version`) without the module prefix. That silently fails: no error is reported, but the variable keeps its default value.

### Defaults for local development

Developers running `go build` or `go run` without ldflags should still get a working binary. Provide sensible defaults:

```go
var (
    Version   = "dev"
    Commit    = "unknown"
    BuildTime = "unknown"
)
```

These strings are recognizable as "not a real release" and prevent accidental deployments of unversioned binaries.

### Build script integration

In a Makefile:

```makefile
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILT    := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
PKG      := github.com/example/myapp/buildinfo

build:
	go build \
	  -ldflags "-X '$(PKG).Version=$(VERSION)' \
	            -X '$(PKG).Commit=$(COMMIT)' \
	            -X '$(PKG).BuildTime=$(BUILT)'" \
	  -o bin/myapp ./cmd/myapp
```

The single quotes around each `-X` value are important: they prevent shell word-splitting when the value contains spaces (as timestamps often do).

### runtime/debug.ReadBuildInfo

Since Go 1.18, `go build` embeds VCS metadata (commit, dirty flag, build settings) into the binary automatically, readable via `runtime/debug.ReadBuildInfo()`. This is complementary to ldflags injection, not a replacement:

- `ReadBuildInfo` gives you the module version from `go.sum` and VCS state.
- ldflags injection gives you arbitrary strings set by your CI system -- semantic version tags, build IDs, branch names.

Use both: ldflags for the human-readable version string, `ReadBuildInfo` as a structured fallback.

### Trade-offs

Ldflags injection is simple and zero-cost at runtime. The injected string is a compile-time constant from the linker's perspective. The downsides:

- The injection is invisible in source code. Reading the source file does not tell you the current version.
- Wrong package paths fail silently. Always verify with `./binary --version` after building.
- It only works with `string`. Complex metadata (struct, map) must be encoded as a string and decoded at runtime.

## Exercises

### Exercise 1: Build metadata package

Create a `buildinfo` package with the injectable variables and a formatting function.

```go.mod
module link-time-injection

go 1.26
```

Create `buildinfo/info.go`:

```go
package buildinfo

import "fmt"

// These variables hold build metadata. Their values are replaced at link time
// by passing -ldflags "-X 'link-time-injection/buildinfo.Version=1.2.3'" etc.
// The defaults are intentionally recognizable as local/development builds.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// String returns a single-line summary of the build metadata.
func String() string {
	return fmt.Sprintf("version=%s commit=%s built=%s", Version, Commit, BuildTime)
}
```

Create `buildinfo/info_test.go`:

```go
package buildinfo

import (
	"strings"
	"testing"
)

func TestStringNonEmpty(t *testing.T) {
	s := String()
	if s == "" {
		t.Fatal("String() returned empty string")
	}
}

func TestStringContainsVersion(t *testing.T) {
	s := String()
	if !strings.Contains(s, "version=") {
		t.Fatalf("String() missing version= field: %q", s)
	}
}

func TestStringContainsCommit(t *testing.T) {
	s := String()
	if !strings.Contains(s, "commit=") {
		t.Fatalf("String() missing commit= field: %q", s)
	}
}

func TestDefaultsAreSensible(t *testing.T) {
	// Without ldflags, defaults must be non-empty recognizable strings.
	if Version == "" {
		t.Error("Version default must not be empty")
	}
	if Commit == "" {
		t.Error("Commit default must not be empty")
	}
	if BuildTime == "" {
		t.Error("BuildTime default must not be empty")
	}
}

func TestVarsAreAccessible(t *testing.T) {
	// Confirm the exported vars are readable -- they must be var, not const.
	v := Version
	c := Commit
	b := BuildTime
	_ = v
	_ = c
	_ = b
}
```

### Exercise 2: CLI entry point

Create `cmd/version/main.go`:

```go
package main

import (
	"fmt"

	"link-time-injection/buildinfo"
)

func main() {
	fmt.Println(buildinfo.String())
}
```

### Exercise 3: Build and inject

Build without injection to see defaults:

```bash
go build -o bin/version ./cmd/version
./bin/version
```

Expected output:

```
version=dev commit=unknown built=unknown
```

Build with injection:

```bash
go build \
  -ldflags "-X 'link-time-injection/buildinfo.Version=1.2.3' \
            -X 'link-time-injection/buildinfo.Commit=abc1234' \
            -X 'link-time-injection/buildinfo.BuildTime=2026-06-24T10:00:00Z'" \
  -o bin/version \
  ./cmd/version
./bin/version
```

Expected output:

```
version=1.2.3 commit=abc1234 built=2026-06-24T10:00:00Z
```

A Makefile that derives the values from git at build time:

```makefile
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILT   := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
PKG     := link-time-injection/buildinfo

.PHONY: build
build:
	go build \
	  -ldflags "-X '$(PKG).Version=$(VERSION)' \
	            -X '$(PKG).Commit=$(COMMIT)' \
	            -X '$(PKG).BuildTime=$(BUILT)'" \
	  -o bin/version \
	  ./cmd/version
```

## Common Mistakes

Wrong: using `const` instead of `var`.

```go
const Version = "dev"
```

What happens: the linker reports no error, but the injected value is silently discarded. The binary always shows "dev". This is because constants are substituted by the compiler; by the time the linker runs, there is no named symbol to overwrite.

Fix: declare as `var Version = "dev"`.

---

Wrong: omitting the module prefix in the -X path.

```bash
go build -ldflags "-X buildinfo.Version=1.2.3" .
```

What happens: no error is reported, but the binary shows "dev". The linker cannot find a symbol named `buildinfo.Version` without the module path prefix. The flag is silently ignored.

Fix: use the full import path: `-X 'link-time-injection/buildinfo.Version=1.2.3'`.

---

Wrong: injecting into a non-string variable.

```go
var Port = 8080  // int
```

```bash
-ldflags "-X 'mypkg.Port=9090'"
```

What happens: the build fails with a hard linker error and no binary is produced. The linker emits a diagnostic such as `cannot set with -X: not a var of type string (type:int)` and exits with a non-zero status. This is distinct from the const and wrong-path cases above, which exit 0 and silently leave the value unchanged. Only `string` variables can be written by `-X`; any other type is a build-time error.

Fix: declare the variable as `string` and parse it at startup: `port, _ := strconv.Atoi(buildinfo.Port)`.

---

Wrong: missing quotes around values containing spaces.

```bash
go build -ldflags "-X mypkg.BuildTime=2026-06-24 10:00:00Z" .
```

What happens: the shell splits the value at the space. The linker sees `-X mypkg.BuildTime=2026-06-24` and a stray argument `10:00:00Z`. The build fails or produces a wrong value.

Fix: wrap the value in single quotes inside the double-quoted ldflags string:

```bash
go build -ldflags "-X 'mypkg.BuildTime=2026-06-24T10:00:00Z'" .
```

Use ISO 8601 timestamps (`T` separator, no spaces) to avoid quoting issues entirely.

## Verification

Run from the module root:

```bash
go vet ./...
go build ./...
go test -count=1 -race ./...
go build -ldflags "-X 'link-time-injection/buildinfo.Version=test-version'" -o /dev/null ./cmd/version
```

The first three commands verify correctness. The fourth verifies that ldflags injection does not break the build.

## Summary

- `-ldflags "-X 'importpath.VarName=value'"` replaces a string variable's value at link time
- Only package-level `var` declarations of type `string` can be injected; `const` and wrong-path cases fail silently (exit 0, value unchanged), while non-string `var` types (e.g. `int`) cause a hard linker error (exit 1, no binary produced)
- The import path in `-X` must be the full module-qualified path, not just the package name
- Always provide sensible defaults (`"dev"`, `"unknown"`) for local development builds
- Wrap injected values in single quotes inside the ldflags string to handle spaces in timestamps

## What's Next

Next: [Exercise 29.8: Plugin System](../08-plugin-system/08-plugin-system.md).

## Resources

- [go build -ldflags documentation](https://pkg.go.dev/cmd/go#hdr-Compile_packages_and_dependencies)
- [cmd/link -X flag](https://pkg.go.dev/cmd/link)
- [runtime/debug.ReadBuildInfo](https://pkg.go.dev/runtime/debug#ReadBuildInfo)
- [go list command](https://pkg.go.dev/cmd/go#hdr-List_packages_or_modules)
- [Makefile patterns for Go builds](https://www.alexedwards.net/blog/an-overview-of-go-tooling#building-executables)
