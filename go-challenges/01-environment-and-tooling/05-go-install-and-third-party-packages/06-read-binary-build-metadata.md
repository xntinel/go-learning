# Exercise 6: Auditing a Binary's Build Metadata

Every Go binary carries embedded provenance: the main module's path and version,
every dependency's version and hash, the build settings, and the VCS revision it
was built from. `go version -m binary` reads it from the command line;
`runtime/debug.ReadBuildInfo` reads it from inside the running program. This
exercise builds a `provenance` package that formats a `debug.BuildInfo` into an
audit line, tests it deterministically, and reads the *real* build info at
runtime.

This module is self-contained and uses only the standard library.

## What you'll build

```text
provenance/                     independent module: example.com/provenance
  go.mod
  provenance.go                 Summary(*debug.BuildInfo) string; setting lookup
  provenance_test.go            deterministic Summary test + skip-guarded real read
  cmd/demo/
    main.go                     formats a sample BuildInfo (stable output)
```

Files: `provenance.go`, `provenance_test.go`, `cmd/demo/main.go`.
Implement: `Summary(bi *debug.BuildInfo) string` and `Setting(bi, key) string`.
Test: `Summary` over a constructed `BuildInfo` (deterministic) plus a real `debug.ReadBuildInfo` read that skips when info is absent.
Verify: `go test -count=1 -race ./...`

### Why format `BuildInfo` from a value, not the live binary

`debug.BuildInfo` is a plain struct: `Path` (the main package import path), `Main`
(a `debug.Module` with `Path`/`Version`/`Sum`), `Deps` (the dependency modules),
`Settings` (key/value build settings like `vcs.revision`, `-race`, `GOOS`), and
`GoVersion`. The audit logic — "turn this into one readable block" — is a pure
function of that struct, so `Summary` takes a `*debug.BuildInfo` argument rather
than calling `debug.ReadBuildInfo` itself. That separation is what makes it
testable: the test constructs a `BuildInfo` with known values and asserts the
exact formatted output, with no dependence on how the test binary happens to be
built.

Reading the *live* build info is a one-liner — `bi, ok := debug.ReadBuildInfo()`
— but its contents vary by how the program was built. In a `go test` binary the
main module path is often empty and `Settings` may be absent, while
`GoVersion` is always populated; a binary produced by `go install` from a clean
checkout carries a concrete `Main.Version` and a `vcs.revision` setting (from
`-buildvcs=true`, the default). This is the same data `go version -m binary`
prints from outside. The runtime read is exercised in a skip-guarded test so it
never flakes on the parts that are environment-dependent.

Create `provenance.go`:

```go
// provenance.go
package provenance

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// Setting returns the value of the named build setting (for example
// "vcs.revision" or "-race"), or "" if it is absent.
func Setting(bi *debug.BuildInfo, key string) string {
	for _, s := range bi.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

// Summary formats the audit-relevant fields of a BuildInfo into a stable,
// multi-line block: the main module, the Go toolchain, each dependency, and the
// VCS revision if one was stamped in.
func Summary(bi *debug.BuildInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "main: %s@%s\n", bi.Main.Path, bi.Main.Version)
	fmt.Fprintf(&b, "go: %s\n", bi.GoVersion)
	fmt.Fprintf(&b, "deps: %d\n", len(bi.Deps))
	for _, d := range bi.Deps {
		fmt.Fprintf(&b, "  %s@%s\n", d.Path, d.Version)
	}
	if rev := Setting(bi, "vcs.revision"); rev != "" {
		fmt.Fprintf(&b, "vcs.revision: %s\n", rev)
	}
	return b.String()
}
```

### The demo

The demo formats a *constructed* `BuildInfo` so the output is stable and
reproducible in this document. A comment shows how to swap in the live read.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"runtime/debug"

	"example.com/provenance"
)

func main() {
	// A representative BuildInfo. In a real audit you would use the live read:
	//   bi, ok := debug.ReadBuildInfo()
	// which returns the metadata embedded in THIS binary.
	bi := &debug.BuildInfo{
		Path:      "example.com/service/cmd/api",
		Main:      debug.Module{Path: "example.com/service", Version: "v1.4.0"},
		GoVersion: "go1.26.0",
		Deps: []*debug.Module{
			{Path: "golang.org/x/text", Version: "v0.14.0"},
			{Path: "rsc.io/quote/v3", Version: "v3.1.0"},
		},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc1234def5678"},
			{Key: "-race", Value: "true"},
		},
	}
	fmt.Print(provenance.Summary(bi))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
main: example.com/service@v1.4.0
go: go1.26.0
deps: 2
  golang.org/x/text@v0.14.0
  rsc.io/quote/v3@v3.1.0
vcs.revision: abc1234def5678
```

### The test

Two tests. `TestSummary` builds a known `BuildInfo` and asserts the exact
formatted block — a pure, deterministic check. `TestLiveBuildInfo` reads the real
build info and, if present, asserts only the field that is reliably populated in
any build (`GoVersion` starts with `go`); it skips when build info is absent so it
never flakes.

Create `provenance_test.go`:

```go
// provenance_test.go
package provenance

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestSummary(t *testing.T) {
	t.Parallel()

	bi := &debug.BuildInfo{
		Main:      debug.Module{Path: "example.com/service", Version: "v1.4.0"},
		GoVersion: "go1.26.0",
		Deps: []*debug.Module{
			{Path: "golang.org/x/text", Version: "v0.14.0"},
		},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "deadbeef"},
		},
	}

	want := "main: example.com/service@v1.4.0\n" +
		"go: go1.26.0\n" +
		"deps: 1\n" +
		"  golang.org/x/text@v0.14.0\n" +
		"vcs.revision: deadbeef\n"

	if got := Summary(bi); got != want {
		t.Fatalf("Summary() =\n%q\nwant\n%q", got, want)
	}
}

func TestSettingAbsent(t *testing.T) {
	t.Parallel()
	bi := &debug.BuildInfo{}
	if got := Setting(bi, "vcs.revision"); got != "" {
		t.Fatalf("Setting on empty BuildInfo = %q, want empty", got)
	}
}

func TestLiveBuildInfo(t *testing.T) {
	t.Parallel()
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("build info not available in this build")
	}
	if !strings.HasPrefix(bi.GoVersion, "go") {
		t.Fatalf("GoVersion = %q, want a go... version string", bi.GoVersion)
	}
}
```

## Review

The formatter is correct when `Summary` is a pure function of its `*BuildInfo`
argument: the same struct always yields the same block, and `Setting` returns
`""` for an absent key rather than panicking. The trap is calling
`debug.ReadBuildInfo` *inside* `Summary`, which couples the output to how the
binary was built and makes the deterministic test impossible. Confirm with
`go test -race ./...`, and read a real binary's metadata from the command line
with `go version -m "$(go env GOPATH)/bin/goimports"` (or any installed tool) to
see the same fields `ReadBuildInfo` exposes.

## Resources

- [`runtime/debug.ReadBuildInfo`](https://pkg.go.dev/runtime/debug#ReadBuildInfo) — the live read and the `BuildInfo`/`Module`/`BuildSetting` types.
- [`go version -m`](https://pkg.go.dev/cmd/go#hdr-Print_Go_version) — reading embedded metadata from a binary on disk.
- [Go 1.18 release notes: embedded VCS information](https://go.dev/doc/go1.18#go-version) — the `vcs.revision`/`vcs.time`/`vcs.modified` stamps and `-buildvcs`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-indirect-dependency-marker.md](05-indirect-dependency-marker.md) | Next: [07-reproducible-tooling-with-tool-directive.md](07-reproducible-tooling-with-tool-directive.md)
