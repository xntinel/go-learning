# Exercise 8: Reading Embedded Build Info for Release Forensics

Every module-built Go binary carries its own provenance: the Go version, module
path, dependency versions and sums, and the VCS revision and dirty flag. This
module reads that back both in-process (`runtime/debug.ReadBuildInfo`) and from a
binary on disk (`debug/buildinfo.ReadFile`) — the tool you reach for to identify a
binary pulled off a crashing production host.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test. Nothing here imports another exercise.

## What you'll build

```text
forensics/                     module example.com/forensics
  go.mod                       package forensics
  forensics.go                 Self(); Inspect(path); Setting(bi, key)
  forensics_test.go            Self() exposes GoVersion; Inspect(os.Executable) matches runtime
  cmd/demo/main.go             prints the running binary's Go version, module, vcs.*
```

- Files: `forensics.go`, `cmd/demo/main.go`, `forensics_test.go`.
- Implement: `Self()` over `runtime/debug.ReadBuildInfo`, `Inspect(path)` over `debug/buildinfo.ReadFile`, and a `Setting` helper that scans `BuildInfo.Settings` for a key.
- Test: assert `Self()` returns build info with a non-empty `GoVersion`, and that inspecting the running test binary agrees with `runtime.Version()`.
- Verify: `go test -race ./...`; `go run ./cmd/demo`; and `go version -m <binary>` on a built binary to cross-check.

Set up the module:

```bash
mkdir -p ~/go-exercises/forensics/cmd/demo
cd ~/go-exercises/forensics
go mod init example.com/forensics
```

### Two entry points, one BuildInfo

Go records build provenance into the binary automatically. There are two ways to
read it. `runtime/debug.ReadBuildInfo()` reads the provenance of the *currently
running* process and returns `(*debug.BuildInfo, bool)` — `ok` is false only for a
binary built without module support. `debug/buildinfo.ReadFile(path)` reads the
same structure from a binary *on disk that you never executed*, returning
`(*debug.BuildInfo, error)`. The second is the forensics workhorse: copy a binary
off a host that is crash-looping and read exactly which commit and dependency set
it was built from, without running untrusted code. The command-line equivalent is
`go version -m <binary>`.

Both return the same `debug.BuildInfo`: `GoVersion`, `Main.Path` (the module),
`Deps` (each dependency's version and sum), and `Settings` — a slice of key/value
`BuildSetting`s. The forensically interesting settings are `vcs.revision` (the git
SHA), `vcs.time` (the commit time), and `vcs.modified` (`true` if the tree was
dirty at build time). These are populated by `-buildvcs` (on by default when
building inside a VCS tree) and are *absent* when the build ran outside a repo or
with `-buildvcs=false` — which is itself a signal that the binary was built in an
unusual way. `Setting` is a small linear scan because `Settings` is a slice, not a
map.

Create `forensics.go`:

```go
// Package forensics reads the build provenance Go embeds in every binary: the
// Go version, module path, dependency versions, and VCS settings (revision,
// time, dirty flag). It works both in-process and against a binary on disk.
package forensics

import (
	"debug/buildinfo"
	"runtime/debug"
)

// Self returns the build info of the running binary, read in-process.
func Self() (*debug.BuildInfo, bool) {
	return debug.ReadBuildInfo()
}

// Inspect reads the build info of a Go binary at path without running it. This
// is the forensics path: point it at a binary pulled off a production host.
func Inspect(path string) (*debug.BuildInfo, error) {
	return buildinfo.ReadFile(path)
}

// Setting returns the value of a build setting (e.g. "vcs.revision",
// "vcs.modified") and whether it was present. VCS settings are absent when the
// build ran outside a repo or with -buildvcs=false.
func Setting(bi *debug.BuildInfo, key string) (string, bool) {
	for _, s := range bi.Settings {
		if s.Key == key {
			return s.Value, true
		}
	}
	return "", false
}
```

### The runnable demo

The demo prints the running binary's Go version, module path, and the three VCS
settings, marking each absent one explicitly. Whether the `vcs.*` lines are
populated depends on how it was built: from a committed git checkout they carry the
SHA and dirty flag; from a directory that is not a repo, or with `-buildvcs=false`,
they read `(absent)`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/forensics"
)

func main() {
	bi, ok := forensics.Self()
	if !ok {
		fmt.Fprintln(os.Stderr, "no build info")
		os.Exit(1)
	}
	fmt.Printf("go version: %s\n", bi.GoVersion)
	fmt.Printf("module path: %s\n", bi.Main.Path)
	for _, key := range []string{"vcs.revision", "vcs.time", "vcs.modified"} {
		if v, ok := forensics.Setting(bi, key); ok {
			fmt.Printf("%s: %s\n", key, v)
		} else {
			fmt.Printf("%s: (absent)\n", key)
		}
	}
}
```

Build and run it inside a committed git checkout:

```bash
go build -o forensics-bin ./cmd/demo
./forensics-bin
```

Expected output (the revision and time reflect your commit):

```text
go version: go1.26.0
module path: example.com/forensics
vcs.revision: b55254765b86b2615364d1fb7eedf938704cdb19
vcs.time: 2026-07-02T13:17:55Z
vcs.modified: false
```

Cross-check with the toolchain's own reader, and observe the same fields:

```bash
go version -m forensics-bin
```

```text
forensics-bin: go1.26.0
	build	vcs=git
	build	vcs.revision=b55254765b86b2615364d1fb7eedf938704cdb19
	build	vcs.time=2026-07-02T13:17:55Z
	build	vcs.modified=false
```

Build with `-buildvcs=false` (or from a directory that is not a repo) and the
`vcs.*` lines become `(absent)`.

### The tests

The tests assert the two properties that hold regardless of VCS state:
`ReadBuildInfo` succeeds for a module-built binary and exposes a non-empty
`GoVersion`, and inspecting the running test binary on disk (via `os.Executable`)
reports the same Go version the process was built with. That second test exercises
`ReadFile` on a real Go binary — the test executable itself — without building a
separate artifact.

Create `forensics_test.go`:

```go
package forensics

import (
	"os"
	"runtime"
	"testing"
)

func TestSelfHasGoVersion(t *testing.T) {
	t.Parallel()
	bi, ok := Self()
	if !ok {
		t.Fatal("Self() ok=false; a module-built test binary must expose build info")
	}
	if bi.GoVersion == "" {
		t.Fatal("Self().GoVersion is empty")
	}
}

func TestInspectMatchesRuntime(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	bi, err := Inspect(exe)
	if err != nil {
		t.Fatalf("Inspect(%q): %v", exe, err)
	}
	if bi.GoVersion != runtime.Version() {
		t.Errorf("Inspect GoVersion = %q; runtime.Version() = %q", bi.GoVersion, runtime.Version())
	}
}
```

## Review

The reader is correct when `Self()` and `Inspect(os.Executable())` agree on the Go
version and the demo's `vcs.*` output matches `go version -m` on the same binary.
The forensic value is concrete: given a binary and nothing else, you recover its
Go toolchain, its module and dependency versions (with sums, so you can detect
tampering), and the exact commit plus dirty flag. The gotcha to internalize is that
`vcs.*` is *not* guaranteed present — a build outside the repo or with
`-buildvcs=false` omits it, so treat absence as information ("built oddly"), not as
an error. This automatic provenance complements the manual `-ldflags -X` stamping
of the previous exercise: `-X` gives you a human-facing version string, and the
embedded build info gives you the machine-verifiable commit and dependency set.

## Resources

- [`runtime/debug.ReadBuildInfo` and `BuildInfo`](https://pkg.go.dev/runtime/debug#ReadBuildInfo) — the in-process reader and the `BuildInfo`/`BuildSetting` types.
- [`debug/buildinfo`](https://pkg.go.dev/debug/buildinfo) — `ReadFile` and `Read` for inspecting a binary on disk.
- [`go version -m` and -buildvcs (cmd/go)](https://pkg.go.dev/cmd/go#hdr-Print_Go_version) — the command-line equivalent and the VCS-stamping control.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-integration-test-tag-gating.md](09-integration-test-tag-gating.md)
