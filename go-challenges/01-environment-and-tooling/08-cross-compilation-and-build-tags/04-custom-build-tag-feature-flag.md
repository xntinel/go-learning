# Exercise 4: Custom -tags Feature Flags for Debug and Tracing Builds

A `//go:build debug` tag is compile-time selection, not a runtime `if`: the code
behind it is physically absent from a default build. This module builds a debug
diagnostics switch that way and proves — with `go list` — that the debug code is
linked in only when `-tags debug` is set.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test. Nothing here imports another exercise.

## What you'll build

```text
featureflag/                   module example.com/featureflag
  go.mod                       package featureflag
  flag.go                      debugEnabled; Enabled(); Diagnose(w, ...)
  debug.go                     //go:build debug -> init() flips debugEnabled, prints banner
  flag_test.go                 default build: Enabled()==false, Diagnose is a no-op
  cmd/demo/main.go             calls Diagnose; behaves differently under -tags debug
```

- Files: `flag.go`, `debug.go`, `cmd/demo/main.go`, `flag_test.go`.
- Implement: a package-level `debugEnabled` (false by default), an `Enabled()` accessor, a `Diagnose` that writes only when enabled, and a `//go:build debug` file whose `init()` enables it.
- Test: assert the default build reports `Enabled()==false` and `Diagnose` writes nothing.
- Verify: `go test -race ./...`; then `go run ./cmd/demo` versus `go run -tags debug ./cmd/demo`; then `go list -tags debug -f '{{.GoFiles}}'` to see `debug.go` appear only with the tag.

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/08-cross-compilation-and-build-tags/04-custom-build-tag-feature-flag/cmd/demo
cd go-solutions/01-environment-and-tooling/08-cross-compilation-and-build-tags/04-custom-build-tag-feature-flag
```

### Compiled out, not skipped

The reason to reach for a build tag rather than a runtime `if debug { ... }` is
that the tag *removes the code from the artifact*. A runtime flag leaves the debug
code — and anything it references, including verbose assertions or embedded
credentials for a diagnostic endpoint — sitting in the shipped binary, reachable
by anyone who flips the flag or reverse-engineers it. A build tag means the
default binary was linked without that code ever being present. `go list` makes
the difference visible: `debug.go` is in the file set only when `-tags debug` is
passed.

The mechanism here is a package-level `debugEnabled` that is `false` in the base
`flag.go`. The `debug.go` file carries `//go:build debug`, so it is compiled only
under the tag; its `init()` flips `debugEnabled` to `true` and prints a one-line
stderr banner so an operator can tell a debug binary apart at a glance. `Diagnose`
is the guarded logging call — a no-op when `debugEnabled` is false, which in a
default build the compiler can see is always the case. Because the whole `debug.go`
file is absent by default, there is no dead branch and nothing to strip.

Create `flag.go`:

```go
// Package featureflag shows a compile-time debug switch: the diagnostic code is
// physically absent from a default build and only linked in under -tags debug.
package featureflag

import (
	"fmt"
	"io"
)

// debugEnabled is false in a default build. The debug.go file, compiled only
// under -tags debug, flips it to true from an init function.
var debugEnabled bool

// Enabled reports whether the debug build tag was set at compile time.
func Enabled() bool { return debugEnabled }

// Diagnose writes a diagnostic line to w only in a debug build; in a default
// build it is a no-op because debugEnabled is false and stays false.
func Diagnose(w io.Writer, format string, args ...any) {
	if !debugEnabled {
		return
	}
	fmt.Fprintf(w, "[debug] "+format+"\n", args...)
}
```

Create `debug.go` — present only under `-tags debug`:

```go
//go:build debug

package featureflag

import (
	"fmt"
	"os"
)

// init runs only in a -tags debug build: it enables Diagnose and prints a banner
// so an operator can see, on stderr, that a debug binary is running.
func init() {
	debugEnabled = true
	fmt.Fprintln(os.Stderr, "[debug] featureflag diagnostics enabled")
}
```

### The runnable demo

The demo calls `Diagnose` on every request and prints whether debug is compiled
in. In a default build the diagnostic line never appears and `Enabled()` is
`false`; rebuild with `-tags debug` and the banner plus the diagnostic line show
up on stderr.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/featureflag"
)

func main() {
	featureflag.Diagnose(os.Stderr, "starting request id=%d", 42)
	fmt.Printf("processed request (debug=%t)\n", featureflag.Enabled())
}
```

Run it both ways:

```bash
go run ./cmd/demo
go run -tags debug ./cmd/demo
```

Expected output, default build:

```text
processed request (debug=false)
```

Expected output, `-tags debug` (the first two lines are stderr):

```text
[debug] featureflag diagnostics enabled
[debug] starting request id=42
processed request (debug=true)
```

Confirm the file set differs — `debug.go` is compiled in only with the tag:

```bash
go list -f '{{.GoFiles}}' .
go list -tags debug -f '{{.GoFiles}}' .
```

```text
[flag.go]
[debug.go flag.go]
```

### The test

The default test path is the one the gate runs (no tag), so it asserts the
production reality: `Enabled()` is `false` and `Diagnose` writes nothing. That the
`debug.go` code is truly excluded is proved by the `go list` output above, not by a
runtime assertion — a runtime check could not tell "excluded" from "present but
disabled", which is exactly the distinction that matters.

Create `flag_test.go`:

```go
package featureflag

import (
	"strings"
	"testing"
)

func TestDefaultBuildHasNoDiagnostics(t *testing.T) {
	t.Parallel()
	if Enabled() {
		t.Fatal("Enabled() = true in a default build; debug.go must be excluded without -tags debug")
	}
	var sb strings.Builder
	Diagnose(&sb, "should not appear")
	if sb.Len() != 0 {
		t.Fatalf("Diagnose wrote %q in a default build; want nothing", sb.String())
	}
}
```

## Review

The switch is correct when the default build reports `Enabled()==false` and the
`-tags debug` build both flips it and links in `debug.go` (visible in the `go list`
diff). The trap this design avoids is the runtime `if debug` that leaves diagnostic
code in the shipped binary; here the code is gone unless the tag is set. To gate
the diagnostics on both the tag *and* a platform, combine terms:
`//go:build debug && linux`. The same `-tags` flag works for `go build`,
`go run`, `go test`, and `go vet`, which is why the identical mechanism drives
tracing builds and enterprise-versus-OSS editions.

## Resources

- [Build constraints (cmd/go)](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — how `-tags` satisfies a `//go:build` term.
- [`go build` flags (cmd/go)](https://pkg.go.dev/cmd/go#hdr-Compile_packages_and_dependencies) — the `-tags` flag across build/run/test/vet.
- [`go/build` constraints](https://pkg.go.dev/go/build#hdr-Build_Constraints) — the constraint grammar for compound tags like `debug && linux`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-file-suffix-implicit-constraints.md](05-file-suffix-implicit-constraints.md)
