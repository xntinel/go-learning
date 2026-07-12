# Exercise 1: Platform Abstraction Behind //go:build Constraints

The canonical use of build constraints is to give one exported symbol a different
implementation per operating system while the callers stay platform-agnostic.
This module builds that pattern the way a real service does: one `Fact()` per
platform, plus a fallback so no target in the release matrix is ever left
undefined.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test. Nothing here imports another exercise.

## What you'll build

```text
platformfact/                  module example.com/platformfact
  go.mod                       package platform
  platform_linux.go            //go:build linux    -> Fact()
  platform_darwin.go           //go:build darwin   -> Fact()
  platform_windows.go          //go:build windows  -> Fact()
  platform_other.go            //go:build !linux && !darwin && !windows -> Fact()
  platform_test.go             asserts the host did NOT fall through to the fallback
  cmd/demo/main.go             prints runtime.GOOS/GOARCH and platform.Fact()
```

- Files: `platform_linux.go`, `platform_darwin.go`, `platform_windows.go`, `platform_other.go`, `cmd/demo/main.go`, `platform_test.go`.
- Implement: `func Fact() string` once per platform, guarded by `//go:build <goos>`, plus a negated-fallback implementation so every unlisted target still compiles.
- Test: assert `Fact()` on the host returns a real platform note, not the fallback string.
- Verify: `go build ./... && go test -race ./...`, then `GOOS=freebsd GOARCH=amd64 go build ./cmd/demo` to prove the fallback keeps every target compilable.

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/08-cross-compilation-and-build-tags/01-platform-abstraction-build-tags/cmd/demo
cd go-solutions/01-environment-and-tooling/08-cross-compilation-and-build-tags/01-platform-abstraction-build-tags
```

### Why one symbol, four files, and a fallback

The design goal is that callers write `platform.Fact()` and never branch on the
operating system themselves. Each platform file declares the same exported
function under a `//go:build <goos>` constraint, so exactly one of them is
compiled for any given target. The `go build` toolchain then sees a single
definition of `Fact` and the other files simply do not exist as far as that build
is concerned.

The subtle, load-bearing file is `platform_other.go`. Its constraint
`//go:build !linux && !darwin && !windows` is the negation of the three explicit
platforms, so it is compiled for — and only for — every target you did *not* write
a dedicated file for. Without it, the first time someone runs
`GOOS=freebsd GOARCH=amd64 go build` the build fails with `undefined:
platform.Fact`, because no file supplies the symbol on freebsd. The fallback turns
that hard failure into a graceful default. This is exactly the surprise that bites
a team when a new architecture (a Pi fleet, a wasm target) is added to the release
matrix months after the platform files were written.

Note also the blank line after each `//go:build` line and before `package`. That
blank line is mandatory: without it, the constraint degrades into a doc comment
and the file compiles everywhere, so all four `Fact` definitions collide.

Create `platform_linux.go`:

```go
//go:build linux

package platform

// Fact returns a one-line platform note used by the demo CLI.
func Fact() string {
	return "linux: default filesystems are ext4 and xfs; getpwuid via cgo NSS or pure-Go."
}
```

Create `platform_darwin.go`:

```go
//go:build darwin

package platform

// Fact returns a one-line platform note used by the demo CLI.
func Fact() string {
	return "darwin: default filesystem is APFS; case-insensitive by default."
}
```

Create `platform_windows.go`:

```go
//go:build windows

package platform

// Fact returns a one-line platform note used by the demo CLI.
func Fact() string {
	return "windows: default filesystem is NTFS; path separator is backslash."
}
```

Create `platform_other.go` — the negated fallback that keeps every unlisted
target compilable:

```go
//go:build !linux && !darwin && !windows

package platform

import (
	"fmt"
	"runtime"
)

// Fact is the fallback so every GOOS in the release matrix keeps compiling.
func Fact() string {
	return fmt.Sprintf("%s/%s: no platform note available.", runtime.GOOS, runtime.GOARCH)
}
```

### The runnable demo

The demo prints the host OS/arch through `runtime.GOOS`/`runtime.GOARCH` (the
runtime mirrors of the build-time `GOOS`/`GOARCH`) and then the platform-selected
`Fact()`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime"

	"example.com/platformfact"
)

func main() {
	fmt.Printf("os=%s arch=%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Println(platform.Fact())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output on darwin/arm64:

```text
os=darwin arch=arm64
darwin: default filesystem is APFS; case-insensitive by default.
```

### The test

The test cannot know which platform the CI host is, but it can assert a property
that holds on *every* real platform: the host build selected a dedicated file, not
the fallback. The fallback is the only `Fact()` that ends in `no platform note
available.`, so rejecting that suffix proves the constraint selection worked.

Create `platform_test.go`:

```go
package platform

import (
	"strings"
	"testing"
)

func TestFactIsRealNotFallback(t *testing.T) {
	t.Parallel()
	got := Fact()
	if got == "" {
		t.Fatal("Fact() returned empty string")
	}
	if strings.HasSuffix(got, "no platform note available.") {
		t.Fatalf("Fact() = %q; host build fell through to the fallback file", got)
	}
}
```

## Review

The abstraction is correct when exactly one `Fact` is compiled per target and no
target is left undefined. Prove it two ways: `go build ./...` and `go test -race
./...` pass on the host (so the host file is selected and non-fallback), and
`GOOS=freebsd GOARCH=amd64 go build ./cmd/demo` succeeds (so the fallback covers a
platform with no dedicated file). If you delete `platform_other.go`, that second
command fails with `undefined: platform.Fact` — the exact failure mode the
fallback exists to prevent. The most common self-inflicted bug is dropping the
blank line after a `//go:build` line: the constraint is then ignored, all four
files compile together, and you get a duplicate-definition error instead — run
`go vet` to catch it early.

## Resources

- [Build constraints (cmd/go)](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — the grammar and matching rules for `//go:build`.
- [`runtime.GOOS` and `runtime.GOARCH`](https://pkg.go.dev/runtime#pkg-constants) — the runtime mirrors of the build-time target.
- [`go/build` constraint documentation](https://pkg.go.dev/go/build#hdr-Build_Constraints) — how file suffixes and `//go:build` combine.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-cross-compile-release-targets.md](02-cross-compile-release-targets.md)
