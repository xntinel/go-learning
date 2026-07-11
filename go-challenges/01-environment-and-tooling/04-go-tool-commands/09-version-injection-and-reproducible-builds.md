# Exercise 9: Reproducible Release Binaries — -ldflags, -trimpath, go version -m

A shipped binary should be reproducible and self-describing. This module stamps
version, commit, and date into the binary at link time with `-ldflags -X`, scrubs
build-machine paths with `-trimpath`, shrinks the artifact with `-s -w`, and reads
the embedded metadata back out of the shipped binary with `go version -m`.

## What you'll build

```text
versioned/                     module example.com/versioned
  go.mod
  internal/
    circle/
      circle.go                Area(radius) float64
      circle_test.go
  cmd/
    demo/
      main.go                  version/commit/date vars; -version flag; formatVersion
      main_test.go             tests formatVersion (pure, no linker needed)
```

- Files: `internal/circle/circle.go`, `internal/circle/circle_test.go`, `cmd/demo/main.go`, `cmd/demo/main_test.go`.
- Implement: package-level `version`, `commit`, `date` vars, a `-version` flag, and a pure `formatVersion` helper.
- Test: `formatVersion` renders the expected line for known inputs.
- Verify: a `-ldflags -X` build reports the injected version; `go version -m` reads the module metadata back out; a `-trimpath` build embeds no absolute source path.

Create the module:

```bash
mkdir -p versioned/cmd/demo versioned/internal/circle
cd versioned
go mod init example.com/versioned
```

### Link-time configuration

Three levers configure a release binary at link time, without a code change.

`-ldflags "-X importpath.name=value"` sets a package-level string `var` at link
time. Declare `var version = "dev"` in `package main` and
`-X main.version=1.4.0` overwrites it in the linked binary. This is the standard
way to stamp version, commit SHA, and build date — the source stays generic, the
build injects the specifics. `-X` only works on string variables, and the path is
the full import path of the package (`main` for the command package).

`-trimpath` removes absolute filesystem paths from the binary. Without it, the
binary embeds the build machine's source directory (for panics and DWARF), which
both leaks a path and breaks reproducibility because two machines produce
different bytes. With it, paths are rewritten to module-relative form.

`-ldflags "-s -w"` strips the symbol table (`-s`) and DWARF debug info (`-w`),
shrinking the artifact substantially — useful for a container image, at the cost
of losing symbolized stack traces from the stripped binary.

On the read side, the toolchain embeds a `runtime/debug.BuildInfo` in every
binary: module path, module version, the build settings, and — in a VCS
checkout — `vcs.revision`, `vcs.time`, and `vcs.modified` stamps. `go version -m`
prints it, and Go 1.25's `go version -m -json` emits it as JSON. Ops can query
what a deployed artifact actually is without asking the author.

Create `internal/circle/circle.go`:

```go
package circle

import "math"

// Area returns the area of a circle with the given radius.
func Area(radius float64) float64 { return math.Pi * radius * radius }
```

Create `internal/circle/circle_test.go`:

```go
package circle

import (
	"math"
	"testing"
)

func TestArea(t *testing.T) {
	t.Parallel()
	if math.Abs(Area(2)-4*math.Pi) > 1e-9 {
		t.Fatalf("Area(2) = %v", Area(2))
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"flag"
	"fmt"

	"example.com/versioned/internal/circle"
)

// version, commit, and date are overwritten at link time with the linker's
// -X flag; see the build commands in the lesson. They default to placeholders.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// formatVersion renders the build metadata line. It is a pure function so it can
// be tested without linker flags.
func formatVersion(version, commit, date string) string {
	return fmt.Sprintf("circle-tool %s (commit %s, built %s)", version, commit, date)
}

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	radius := flag.Float64("radius", 5.0, "circle radius")
	flag.Parse()

	if *showVersion {
		fmt.Println(formatVersion(version, commit, date))
		return
	}
	fmt.Printf("radius %.1f -> area %.5f\n", *radius, circle.Area(*radius))
}
```

Create `cmd/demo/main_test.go`:

```go
package main

import "testing"

func TestFormatVersion(t *testing.T) {
	t.Parallel()
	got := formatVersion("1.4.0", "abc1234", "2026-07-02")
	want := "circle-tool 1.4.0 (commit abc1234, built 2026-07-02)"
	if got != want {
		t.Fatalf("formatVersion = %q, want %q", got, want)
	}
}
```

### Building the default and the release binary

A plain build leaves the placeholders in place:

```bash
go build -o bin/demo ./cmd/demo
./bin/demo -version
```

```text
circle-tool dev (commit none, built unknown)
```

A release build injects the metadata, trims paths, and strips symbols:

```bash
go build -trimpath \
	-ldflags "-X main.version=1.4.0 -X main.commit=abc1234def -X main.date=2026-07-02 -s -w" \
	-o bin/demo-rel ./cmd/demo
./bin/demo-rel -version
```

```text
circle-tool 1.4.0 (commit abc1234def, built 2026-07-02)
```

The same source now reports a real version, commit, and date because the linker
overwrote the `main` package vars.

### Reading metadata back out

`go version -m` reads the embedded `BuildInfo` from the shipped binary:

```bash
go version -m bin/demo-rel
```

```text
bin/demo-rel: go1.26
	path	example.com/versioned/cmd/demo
	mod	example.com/versioned	(devel)
	build	-buildmode=exe
	build	-trimpath=true
	...
```

(The first line reports the toolchain that built it; a VCS checkout adds
`vcs.revision`, `vcs.time`, and `vcs.modified` lines.) Go 1.25 adds JSON output
for machine consumption:

```bash
go version -m -json bin/demo-rel
```

It prints the JSON encoding of the `runtime/debug.BuildInfo` structs — the same
data, parseable by a deploy pipeline.

### Proving -trimpath scrubs the path

The `-trimpath` build must not contain the absolute source directory. Grep the
binary for your home path:

```bash
strings bin/demo-rel | grep "$HOME/Documents" && echo "LEAK" || echo "clean: no build-machine path"
```

```text
clean: no build-machine path
```

A build *without* `-trimpath` would embed that path, both leaking it and making
the artifact non-reproducible across machines. The `-s -w` strip also cuts the
binary size noticeably versus the default build — measurable with `ls -l bin`.

### A note on the build cache

Incremental builds are content-addressed: unchanged inputs are not recompiled.
`-a` forces a full rebuild and `go clean -cache` empties the cache. Reaching for a
blanket `-a` in CI to "fix" a stale result cargo-cults around the cache instead of
understanding it; use `go clean -cache` only with actual evidence of corruption.

## Review

The module is correct when `go test ./...` passes (covering `formatVersion` and
`Area`), the `-ldflags -X` build reports `circle-tool 1.4.0 ...`, and
`go version -m bin/demo-rel` shows the module path. The traps: shipping without
`-trimpath` (embedding an absolute build-machine path and breaking
reproducibility) and shipping without `-ldflags -X` (a deployed binary with no
queryable version, so ops cannot tell what is running). `formatVersion` is pure so
the version-rendering logic is tested without invoking the linker at all.

## Resources

- [Command go — build and test caching / -ldflags](https://pkg.go.dev/cmd/go#hdr-Compile_packages_and_dependencies) — `-ldflags`, `-trimpath`, and `-a`.
- [Command link](https://pkg.go.dev/cmd/link) — the `-X`, `-s`, and `-w` linker flags.
- [runtime/debug — BuildInfo](https://pkg.go.dev/runtime/debug#BuildInfo) — the metadata `go version -m` reads back out.
- [Go 1.25 release notes](https://go.dev/doc/go1.25) — the new `go version -m -json`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-go-list-dependency-audit.md](08-go-list-dependency-audit.md) | Next: [10-ci-toolchain-gate.md](10-ci-toolchain-gate.md)
