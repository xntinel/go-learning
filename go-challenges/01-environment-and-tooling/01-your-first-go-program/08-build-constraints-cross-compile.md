# Exercise 8: Ship platform-specific code and cross-compile

The CLI writes a small cache, and the sensible default location differs per
operating system. That is a job for build constraints: one file per platform, a
shared fallback, and the compiler selecting the right one. Then you cross-compile
the whole thing for a Linux container from your dev host — the everyday
container-delivery task.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
platform/                   independent module: example.com/platform
  go.mod                    go 1.26
  platform.go               DefaultCachePath, Platform (portable, calls the gated helper)
  cachedir_linux.go         //go:build linux
  cachedir_darwin.go        //go:build darwin
  cachedir_other.go         //go:build !linux && !darwin
  cmd/demo/main.go          runnable demo printing the platform cache path
  platform_test.go          asserts the selected file matches runtime.GOOS
```

Files: `platform.go`, `cachedir_linux.go`, `cachedir_darwin.go`,
`cachedir_other.go`, `cmd/demo/main.go`, `platform_test.go`.
Implement: `DefaultCachePath(name)` built on a per-platform `defaultCacheDir()`
selected by `//go:build` constraints, with an `other` fallback.
Test: assert `Platform()` matches `runtime.GOOS` (or `other`) and that
`DefaultCachePath` joins a directory onto the name.
Verify: `go test -count=1 ./...` on the host; a shell harness cross-compiles for
`linux/amd64` and `linux/arm64` and inspects the selected file set with
`go list`. `gofmt -l` empty.

### One file per platform, selected at compile time

A `//go:build` constraint at the top of a file (followed by a blank line, then the
`package` clause) tells the compiler when to include that file. Three files each
define the same unexported symbols — `platformName` and `defaultCacheDir()` — under
mutually exclusive constraints, so exactly one compiles for any target:

- `cachedir_linux.go` under `//go:build linux`
- `cachedir_darwin.go` under `//go:build darwin`
- `cachedir_other.go` under `//go:build !linux && !darwin` (the fallback for every
  other OS, so the module builds anywhere)

The constraints are mutually exclusive and exhaustive, which is the property that
matters: never zero matches (that would be an undefined-symbol build error) and
never two (that would be a duplicate-definition error). The filename suffix
convention (`_linux.go`, `_darwin.go`) *also* implies a GOOS constraint, so the
explicit `//go:build` line here is belt-and-suspenders and, more importantly,
documents intent; for the `_other.go` file the explicit constraint is required
because its name carries no implicit one.

The portable `platform.go` calls the gated helper and knows nothing about which
platform it is on.

Create `platform.go`:

```go
package platform

import "path/filepath"

// DefaultCachePath returns the platform-appropriate cache path for name.
func DefaultCachePath(name string) string {
	return filepath.Join(defaultCacheDir(), name)
}

// Platform reports the compiled-in platform identifier.
func Platform() string { return platformName }
```

Create `cachedir_linux.go`:

```go
//go:build linux

package platform

const platformName = "linux"

func defaultCacheDir() string { return "/var/cache/urlcheck" }
```

Create `cachedir_darwin.go`:

```go
//go:build darwin

package platform

const platformName = "darwin"

func defaultCacheDir() string { return "/Library/Caches/urlcheck" }
```

Create `cachedir_other.go`:

```go
//go:build !linux && !darwin

package platform

const platformName = "other"

func defaultCacheDir() string { return "/tmp/urlcheck" }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/platform"
)

func main() {
	fmt.Printf("platform=%s cache=%s\n", platform.Platform(), platform.DefaultCachePath("results.db"))
}
```

Run it (output depends on the host OS):

```bash
go run ./cmd/demo
```

Expected output on macOS:

```
platform=darwin cache=/Library/Caches/urlcheck/results.db
```

On Linux the same run prints `platform=linux cache=/var/cache/urlcheck/results.db`.

### Tests

The test is host-agnostic: it derives the expected platform from `runtime.GOOS`,
so it passes whether the gate runs on macOS, Linux, or anything else, and proves
the constraint machinery selected the file that matches the build target.

Create `platform_test.go`:

```go
package platform

import (
	"runtime"
	"strings"
	"testing"
)

func TestPlatformMatchesGOOS(t *testing.T) {
	t.Parallel()
	want := runtime.GOOS
	switch want {
	case "linux", "darwin":
		if Platform() != want {
			t.Fatalf("Platform() = %q, want %q", Platform(), want)
		}
	default:
		if Platform() != "other" {
			t.Fatalf("Platform() = %q, want other for GOOS=%q", Platform(), want)
		}
	}
}

func TestDefaultCachePath(t *testing.T) {
	t.Parallel()
	got := DefaultCachePath("results.db")
	if !strings.HasSuffix(got, "results.db") {
		t.Fatalf("DefaultCachePath = %q, want it to end in results.db", got)
	}
	if got == "results.db" {
		t.Fatalf("DefaultCachePath = %q, missing a directory prefix", got)
	}
}
```

To prove cross-compilation and file selection, a shell harness builds for two
Linux architectures and lists the files the toolchain would compile for a given
GOOS:

```bash
# Cross-compile for Linux, no libc dependency (for a scratch/distroless image).
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/demo
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /dev/null ./cmd/demo

# See which cache file the constraints select per platform.
GOOS=linux  go list -f '{{.GoFiles}}' .   # includes cachedir_linux.go
GOOS=darwin go list -f '{{.GoFiles}}' .   # includes cachedir_darwin.go
GOOS=windows go list -f '{{.GoFiles}}' .  # includes cachedir_other.go
```

## Review

The design is correct when the three constrained files are mutually exclusive and
exhaustive: any GOOS compiles exactly one `defaultCacheDir`, so there is never an
undefined symbol and never a duplicate. The host test proves it by deriving the
expected platform from `runtime.GOOS` instead of hardcoding one, which is what
lets the same test pass on every build target. Cross-compilation is just three
environment variables — `GOOS`, `GOARCH`, `CGO_ENABLED` — turning your dev host
into a builder for the container's platform.

The traps: forgetting `CGO_ENABLED=0` when targeting a `scratch` or `distroless`
image produces a binary that needs a libc that is not there, and it fails at
startup, not at build time. And a `//go:build` line must be followed by a blank
line before the `package` clause or the compiler treats it as an ordinary comment
and the constraint is silently ignored — `gofmt` enforces the blank line, which is
one more reason it is in the gate.

## Resources

- [Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — `//go:build` syntax and filename suffixes.
- [go/build: GOOS and GOARCH values](https://pkg.go.dev/internal/platform) — the valid target list (see also `go tool dist list`).
- [Managing dependencies: cross-compilation](https://go.dev/wiki/GcToolchainTricks) — building for another platform from one host.
- [cgo](https://pkg.go.dev/cmd/cgo) — why `CGO_ENABLED=0` matters for a static Linux binary.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-benchmarks-and-fuzzing.md](07-benchmarks-and-fuzzing.md) | Next: [09-vet-and-static-analysis-gate.md](09-vet-and-static-analysis-gate.md)
