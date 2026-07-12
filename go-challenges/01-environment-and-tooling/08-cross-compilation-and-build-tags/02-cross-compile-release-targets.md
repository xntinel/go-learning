# Exercise 2: Cross-Compiling One Source Tree for Three Targets

Cross-compilation is a two-variable operation: set `GOOS` and `GOARCH`, keep cgo
off, and one `go build` emits a native binary for that pair. This module models
the release matrix in Go, produces the binaries from the same package, and
verifies each one's container format with `file`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test. Nothing here imports another exercise.

## What you'll build

```text
releasetargets/                module example.com/releasetargets
  go.mod                       package releasetargets
  targets.go                   type Target; Matrix, Artifact, FileFormat
  targets_test.go              artifact naming + expected file(1) format per target
  cmd/demo/main.go             prints the matrix with per-target artifact + format
```

- Files: `targets.go`, `cmd/demo/main.go`, `targets_test.go`.
- Implement: a `Target{GOOS,GOARCH}`, a `Matrix()` of the four shipped pairs, `Artifact(base)` (adds `.exe` on Windows), and `FileFormat()` (the `file(1)` signature the built binary must carry).
- Test: assert artifact names and the expected format string per target.
- Verify: `go test -race ./...`, then the cross-build loop below plus `file` on the outputs.

### The matrix as data, and the actual cross-build

The release matrix is not prose in a runbook; it is data the build iterates. The
`Target` type pairs a `GOOS` and `GOARCH`, `Matrix()` lists the four targets this
service ships to, and two methods encode the two facts a release step needs: what
the output file is named (`Artifact`, which appends `.exe` on Windows because that
is the platform convention), and what `file(1)` should report for a correct build
(`FileFormat`). Linux binaries are ELF, Darwin binaries are Mach-O, Windows
binaries are PE — a mismatch there means the wrong `GOOS` was set.

The build itself is the two-variable operation, run once per target with
`CGO_ENABLED=0` so the result is a pure-Go, statically linked binary that needs no
libc on the target:

```bash
mkdir -p bin
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o bin/app-linux-amd64      ./cmd/demo
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -o bin/app-linux-arm64      ./cmd/demo
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o bin/app-darwin-arm64     ./cmd/demo
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/app-windows-amd64.exe ./cmd/demo
```

Confirm each binary's container format and that the targets are real pairs:

```bash
file bin/*
go tool dist list | grep -E '^(linux/amd64|linux/arm64|darwin/arm64|windows/amd64)$'
```

Real `file` output on a darwin/arm64 builder (note the ELF binaries are
`statically linked` because cgo is off):

```text
bin/app-darwin-arm64:      Mach-O 64-bit executable arm64
bin/app-linux-amd64:       ELF 64-bit LSB executable, x86-64, ... statically linked, ...
bin/app-linux-arm64:       ELF 64-bit LSB executable, ARM aarch64, ... statically linked, ...
bin/app-windows-amd64.exe: PE32+ executable (console) x86-64, for MS Windows
```

The `linux/arm64` binary reports `ARM aarch64`, not `x86-64`: that is the whole
point of `GOARCH` being independent of `GOOS`. Had you forgotten `GOARCH` on the
Apple laptop, both Linux builds would have come out `aarch64` and the amd64
server would have refused to run them.

Create `targets.go`:

```go
// Package releasetargets models the OS/arch matrix a service ships to and the
// container format each target's binary carries, so a build can name artifacts
// and a check can confirm file(1) reported the expected format.
package releasetargets

import "fmt"

// Target is one GOOS/GOARCH pair in the release matrix.
type Target struct {
	GOOS   string
	GOARCH string
}

// Matrix is the set of targets this service ships: linux servers on both
// architectures, Apple Silicon laptops, and Windows on x86-64.
func Matrix() []Target {
	return []Target{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"darwin", "arm64"},
		{"windows", "amd64"},
	}
}

// Artifact is the output file name for a target, adding .exe on Windows.
func (t Target) Artifact(base string) string {
	name := fmt.Sprintf("%s-%s-%s", base, t.GOOS, t.GOARCH)
	if t.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// FileFormat is the substring file(1) prints for a CGO_ENABLED=0 binary built
// for this target: ELF for Linux, Mach-O for Darwin, PE for Windows.
func (t Target) FileFormat() string {
	switch t.GOOS {
	case "linux":
		return "ELF 64-bit"
	case "darwin":
		return "Mach-O 64-bit"
	case "windows":
		return "PE32+"
	default:
		return "unknown"
	}
}
```

### The runnable demo

The demo prints the matrix exactly as a release job would enumerate it, so you can
eyeball the artifact names and the format each build must produce before running
the cross-build loop.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/releasetargets"
)

func main() {
	fmt.Println("release matrix (CGO_ENABLED=0):")
	for _, t := range releasetargets.Matrix() {
		fmt.Printf("  GOOS=%-7s GOARCH=%-5s -> bin/%-24s expect: %s\n",
			t.GOOS, t.GOARCH, t.Artifact("app"), t.FileFormat())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
release matrix (CGO_ENABLED=0):
  GOOS=linux   GOARCH=amd64 -> bin/app-linux-amd64          expect: ELF 64-bit
  GOOS=linux   GOARCH=arm64 -> bin/app-linux-arm64          expect: ELF 64-bit
  GOOS=darwin  GOARCH=arm64 -> bin/app-darwin-arm64         expect: Mach-O 64-bit
  GOOS=windows GOARCH=amd64 -> bin/app-windows-amd64.exe    expect: PE32+
```

### The test

The test pins the two facts the cross-build depends on: that artifact names follow
the platform convention (with `.exe` only on Windows) and that each `GOOS` maps to
the container format `file` must report. If a future edit swaps ELF and Mach-O, or
forgets the Windows suffix, the table fails before a single binary is produced.

Create `targets_test.go`:

```go
package releasetargets

import "testing"

func TestArtifactNaming(t *testing.T) {
	t.Parallel()
	cases := []struct {
		target Target
		want   string
	}{
		{Target{"linux", "amd64"}, "app-linux-amd64"},
		{Target{"linux", "arm64"}, "app-linux-arm64"},
		{Target{"darwin", "arm64"}, "app-darwin-arm64"},
		{Target{"windows", "amd64"}, "app-windows-amd64.exe"},
	}
	for _, c := range cases {
		if got := c.target.Artifact("app"); got != c.want {
			t.Errorf("Artifact(app) for %+v = %q; want %q", c.target, got, c.want)
		}
	}
}

func TestFileFormat(t *testing.T) {
	t.Parallel()
	cases := map[Target]string{
		{"linux", "amd64"}:   "ELF 64-bit",
		{"darwin", "arm64"}:  "Mach-O 64-bit",
		{"windows", "amd64"}: "PE32+",
	}
	for target, want := range cases {
		if got := target.FileFormat(); got != want {
			t.Errorf("FileFormat for %+v = %q; want %q", target, got, want)
		}
	}
}

func TestMatrixIsComplete(t *testing.T) {
	t.Parallel()
	if got := len(Matrix()); got != 4 {
		t.Fatalf("Matrix has %d targets; want 4", got)
	}
}
```

## Review

The build is correct when each produced binary's `file` output contains the
`FileFormat` string its `Target` predicts, and every pair in `Matrix()` appears in
`go tool dist list`. The reproducibility-relevant detail is `CGO_ENABLED=0`:
without it, a build that pulls in a cgo package fails to cross-compile at all (no C
cross-toolchain) or links against libc and stops being a `statically linked`
scratch-ready artifact. The failure this module guards against is the silent one:
setting `GOOS` and forgetting `GOARCH`, which makes every "Linux" build inherit the
builder's architecture. Compare the `ARM aarch64` and `x86-64` labels in the `file`
output to confirm both Linux binaries are genuinely for different CPUs.

## Resources

- [`go tool dist list`](https://pkg.go.dev/cmd/dist) — the authoritative GOOS/GOARCH matrix for your toolchain.
- [Environment variables (cmd/go): GOOS, GOARCH, CGO_ENABLED](https://pkg.go.dev/cmd/go#hdr-Environment_variables) — how the target selectors and cgo toggle interact.
- [Installing Go from source: environment](https://go.dev/doc/install/source#environment) — the reference table of supported values.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-audit-included-files-and-deps.md](03-audit-included-files-and-deps.md)
