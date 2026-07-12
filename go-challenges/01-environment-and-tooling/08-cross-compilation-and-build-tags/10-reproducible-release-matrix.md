# Exercise 10: A Reproducible Multi-Platform Release Matrix

This is the on-the-job artifact producer: one `go run build.go` that builds every
target with `CGO_ENABLED=0`, `-trimpath`, and a chosen `GOAMD64` level, then emits
a `SHA256SUMS` manifest. The reusable logic lives in a gated package; the
orchestrator itself is a `//go:build ignore` file run standalone.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test. Nothing here imports another exercise.

## What you'll build

```text
releasematrix/                 module example.com/releasematrix
  go.mod                       package releasematrix
  release.go                   Target; Matrix, Artifact, Env; Sum, SumsFile
  release_test.go              artifact naming, env (CGO/GOAMD64), sum determinism, sorted manifest
  build.go                     //go:build ignore -> orchestrator, run with `go run build.go`
  cmd/demo/main.go             prints the matrix and a deterministic SHA256SUMS
```

- Files: `release.go`, `cmd/demo/main.go`, `release_test.go`, `build.go`.
- Implement: a `Target{GOOS,GOARCH,AMD64Level}`, a `Matrix()`, `Artifact`/`Env` per target, `Sum` (SHA-256 hex), and `SumsFile` (sorted manifest); plus a `//go:build ignore` orchestrator that cross-builds and writes `dist/SHA256SUMS`.
- Test: assert naming, that `Env()` sets `CGO_ENABLED=0` and `GOAMD64` only for amd64, that `Sum` is deterministic, and that `SumsFile` is sorted.
- Verify: `go test -race ./...`; `go run build.go` produces `dist/` artifacts with a verifiable `SHA256SUMS`; rebuilding yields identical checksums.

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/08-cross-compilation-and-build-tags/10-reproducible-release-matrix/cmd/demo
cd go-solutions/01-environment-and-tooling/08-cross-compilation-and-build-tags/10-reproducible-release-matrix
```

### Separating the logic from the orchestrator

A release script has two parts, and it pays to keep them apart. The *policy* — the
target matrix, the naming convention, the per-target environment, the hashing and
manifest format — is ordinary library code in `release.go`, so it is unit-tested
like anything else. The *orchestration* — actually shelling out to `go build` for
each target and writing files — lives in `build.go`, marked `//go:build ignore` so
it is excluded from the package build and run standalone with `go run build.go`.
The `ignore` convention is exactly for this: a runnable script that ships inside
the repo without becoming part of any importable package. `go list` confirms it is
excluded (`IgnoredGoFiles=[build.go]`), and `go vet ./...`/`go build ./...` never
touch it, though `gofmt` still keeps it formatted.

Three details make the output reproducible and fleet-safe. `CGO_ENABLED=0` gives a
static, pure-Go binary with no libc dependency. `-trimpath` strips absolute build
paths so the bytes do not depend on where the build ran (this is what makes two
builds on two machines produce identical checksums). And `GOAMD64=v2` sets a
*minimum* microarchitecture for the amd64 targets — high enough to let the compiler
use post-2009 instructions, low enough that the binary still starts on any
reasonable server; a `v3`/`v4` binary would abort on a CPU lacking AVX2/AVX-512, so
the level must match the least-capable machine in the fleet. Non-amd64 targets get
no `GOAMD64` at all, which `Env()` enforces.

The manifest is sorted by file name so it is byte-identical across runs — a
manifest whose line order depended on map iteration would defeat the whole point of
reproducibility.

Create `release.go`:

```go
// Package releasematrix models a reproducible multi-platform release: the target
// matrix (with a GOAMD64 microarchitecture level), deterministic artifact names,
// and a SHA256SUMS manifest whose lines are sorted for byte-stable output.
package releasematrix

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Target is one build in the matrix. AMD64Level is the GOAMD64 microarchitecture
// (e.g. "v2") for amd64 targets and empty otherwise.
type Target struct {
	GOOS       string
	GOARCH     string
	AMD64Level string
}

// Matrix is the set of targets shipped: linux on both architectures at a
// conservative GOAMD64=v2, Apple Silicon, and Windows on x86-64.
func Matrix() []Target {
	return []Target{
		{"linux", "amd64", "v2"},
		{"linux", "arm64", ""},
		{"darwin", "arm64", ""},
		{"windows", "amd64", "v2"},
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

// Env returns the extra environment a reproducible build sets for this target.
func (t Target) Env() []string {
	env := []string{
		"GOOS=" + t.GOOS,
		"GOARCH=" + t.GOARCH,
		"CGO_ENABLED=0",
	}
	if t.AMD64Level != "" {
		env = append(env, "GOAMD64="+t.AMD64Level)
	}
	return env
}

// Sum is the lowercase hex SHA-256 of data. Identical input yields an identical
// sum, which is the reproducibility check for a rebuilt artifact.
func Sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// SumsFile renders a shasum-compatible manifest: "<hex>  <name>" per line, sorted
// by file name so the manifest is byte-identical across runs.
func SumsFile(sums map[string]string) string {
	names := make([]string, 0, len(sums))
	for name := range sums {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s  %s\n", sums[name], name)
	}
	return b.String()
}
```

Create `build.go` — the standalone orchestrator, excluded from the package build:

```go
//go:build ignore

// Command build produces the reproducible release matrix: it builds every target
// with CGO_ENABLED=0, -trimpath, and the target's GOAMD64 level, then writes a
// SHA256SUMS manifest. Run it standalone with: go run build.go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

type target struct {
	goos, goarch, amd64Level string
}

func matrix() []target {
	return []target{
		{"linux", "amd64", "v2"},
		{"linux", "arm64", ""},
		{"darwin", "arm64", ""},
		{"windows", "amd64", "v2"},
	}
}

func (t target) artifact() string {
	name := fmt.Sprintf("app-%s-%s", t.goos, t.goarch)
	if t.goos == "windows" {
		name += ".exe"
	}
	return name
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "build failed:", err)
		os.Exit(1)
	}
}

func run() error {
	if err := os.MkdirAll("dist", 0o755); err != nil {
		return err
	}
	sums := map[string]string{}
	for _, t := range matrix() {
		out := filepath.Join("dist", t.artifact())
		cmd := exec.Command("go", "build", "-trimpath",
			"-ldflags", "-s -w", "-o", out, "./cmd/demo")
		env := append(os.Environ(),
			"GOOS="+t.goos, "GOARCH="+t.goarch, "CGO_ENABLED=0")
		if t.amd64Level != "" {
			env = append(env, "GOAMD64="+t.amd64Level)
		}
		cmd.Env = env
		if combined, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", t.artifact(), err, combined)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			return err
		}
		h := sha256.Sum256(data)
		sums[t.artifact()] = hex.EncodeToString(h[:])
		fmt.Printf("built %s\n", out)
	}

	names := make([]string, 0, len(sums))
	for name := range sums {
		names = append(names, name)
	}
	sort.Strings(names)
	var manifest []byte
	for _, name := range names {
		manifest = append(manifest, fmt.Sprintf("%s  %s\n", sums[name], name)...)
	}
	return os.WriteFile(filepath.Join("dist", "SHA256SUMS"), manifest, 0o644)
}
```

### The runnable demo

The demo prints the matrix with each target's build environment, then a
`SHA256SUMS` computed over a deterministic stand-in for the binary bytes (so the
demo is stable and offline). It is the shape of the manifest the real orchestrator
writes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/releasematrix"
)

func main() {
	sums := map[string]string{}
	fmt.Println("release matrix:")
	for _, t := range releasematrix.Matrix() {
		name := t.Artifact("app")
		// Stand-in for real binary bytes so the demo is deterministic offline.
		content := []byte(name + "\n")
		sums[name] = releasematrix.Sum(content)
		fmt.Printf("  %-24s env: %s\n", name, strings.Join(t.Env(), " "))
	}
	fmt.Print("\nSHA256SUMS:\n")
	fmt.Print(releasematrix.SumsFile(sums))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
release matrix:
  app-linux-amd64          env: GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOAMD64=v2
  app-linux-arm64          env: GOOS=linux GOARCH=arm64 CGO_ENABLED=0
  app-darwin-arm64         env: GOOS=darwin GOARCH=arm64 CGO_ENABLED=0
  app-windows-amd64.exe    env: GOOS=windows GOARCH=amd64 CGO_ENABLED=0 GOAMD64=v2

SHA256SUMS:
28e830c9673d56c7167665a87777bc36f755d2a6247ca2e729366c8ef48d91a8  app-darwin-arm64
3e2e85dee32d65ac1f0279e655c46844e87a854fdb6a67c08ed5a67310b73793  app-linux-amd64
f483c244a37c21e02db0185105205d29d3f4021949f713ab01ad3c25d7c6395f  app-linux-arm64
c4fe5059195e36b054e481071e0e30132d8417014ecb7467bcf48690d63391e2  app-windows-amd64.exe
```

Run the real orchestrator to produce actual binaries and verify the manifest from
the `dist` directory:

```bash
go run build.go
cd dist && shasum -a 256 -c SHA256SUMS
```

```text
built dist/app-linux-amd64
built dist/app-linux-arm64
built dist/app-darwin-arm64
built dist/app-windows-amd64.exe
app-darwin-arm64: OK
app-linux-amd64: OK
app-linux-arm64: OK
app-windows-amd64.exe: OK
```

Rebuild `dist/` a second time and diff the two `SHA256SUMS`: because of
`-trimpath` and `CGO_ENABLED=0`, they are byte-identical — the reproducibility
property a verifiable supply chain depends on.

### The tests

The tests pin the release policy: artifact naming, that `Env()` sets
`CGO_ENABLED=0` and attaches `GOAMD64` only to amd64 targets, that `Sum` is
deterministic and 64 hex chars, and that `SumsFile` output is sorted regardless of
map order.

Create `release_test.go`:

```go
package releasematrix

import (
	"strings"
	"testing"
)

func TestArtifactNaming(t *testing.T) {
	t.Parallel()
	cases := map[Target]string{
		{"linux", "amd64", "v2"}:   "app-linux-amd64",
		{"darwin", "arm64", ""}:    "app-darwin-arm64",
		{"windows", "amd64", "v2"}: "app-windows-amd64.exe",
	}
	for target, want := range cases {
		if got := target.Artifact("app"); got != want {
			t.Errorf("Artifact for %+v = %q; want %q", target, got, want)
		}
	}
}

func TestEnvIncludesCGOAndLevel(t *testing.T) {
	t.Parallel()
	env := strings.Join(Target{"linux", "amd64", "v2"}.Env(), " ")
	for _, want := range []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOAMD64=v2"} {
		if !strings.Contains(env, want) {
			t.Errorf("Env() = %q; missing %q", env, want)
		}
	}
	arm := strings.Join(Target{"linux", "arm64", ""}.Env(), " ")
	if strings.Contains(arm, "GOAMD64") {
		t.Errorf("arm64 Env() = %q; must not set GOAMD64", arm)
	}
}

func TestSumIsDeterministic(t *testing.T) {
	t.Parallel()
	a := Sum([]byte("hello"))
	b := Sum([]byte("hello"))
	if a != b {
		t.Fatalf("Sum not deterministic: %q vs %q", a, b)
	}
	if a == Sum([]byte("world")) {
		t.Fatal("distinct inputs produced the same sum")
	}
	if len(a) != 64 {
		t.Fatalf("Sum length = %d; want 64 hex chars", len(a))
	}
}

func TestSumsFileIsSorted(t *testing.T) {
	t.Parallel()
	got := SumsFile(map[string]string{
		"b.bin": "22",
		"a.bin": "11",
		"c.bin": "33",
	})
	want := "11  a.bin\n22  b.bin\n33  c.bin\n"
	if got != want {
		t.Fatalf("SumsFile = %q; want sorted %q", got, want)
	}
}
```

## Review

The producer is correct when `go run build.go` writes one artifact per matrix
entry, `shasum -c` verifies the manifest from `dist/`, and a second run yields an
identical `SHA256SUMS`. The separation is the lesson: policy in a tested library,
orchestration in a `//go:build ignore` script that never pollutes the package. The
two failure modes to keep in mind are shipping too high a `GOAMD64` level (a hard
startup abort on older CPUs, not a graceful fallback) and a non-deterministic
manifest (unsorted lines, or paths leaked without `-trimpath`) that breaks the
reproducibility guarantee. This module closes the loop the chapter opened: one
source tree, a `go tool dist list` of targets, `CGO_ENABLED=0` static binaries,
stamped and provenance-carrying, produced reproducibly from a single CI job.

## Resources

- [`go build` flags: -trimpath, -ldflags (cmd/go)](https://pkg.go.dev/cmd/go#hdr-Compile_packages_and_dependencies) — the reproducibility flags the orchestrator sets.
- [Environment variables: GOAMD64, GOARM64 (cmd/go)](https://pkg.go.dev/cmd/go#hdr-Environment_variables) — the microarchitecture level and its fleet implications.
- [`crypto/sha256`](https://pkg.go.dev/crypto/sha256) — the hash behind the `SHA256SUMS` manifest.
- [`os/exec.Cmd`](https://pkg.go.dev/os/exec#Cmd) — `Command`, `Env`, and `CombinedOutput` as used by the orchestrator.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../../02-variables-types-and-constants/01-variable-declaration-and-short-assignment/01-variable-declaration-and-short-assignment.md](../../02-variables-types-and-constants/01-variable-declaration-and-short-assignment/01-variable-declaration-and-short-assignment.md)
