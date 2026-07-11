# Exercise 1: Extracting Verifiable Build Provenance from a Go Binary

Go stamps every binary with a machine-readable build record; this exercise turns
that record into a provenance object and gates it against policy. You build an
extractor that reads the record two ways — from the running process and from the
textual `go version -m` output — normalizes it, and rejects builds that are dirty,
unstamped, or replaced by a local path.

This module is fully self-contained: its own `go mod init`, everything inline, its
own demo and tests. It depends only on the standard library, so it builds and
gates offline.

## What you'll build

```text
provenance/                 independent module: example.com/provenance
  go.mod                    go 1.26
  provenance.go             Provenance, Module, VCSInfo; Extract, Parse, FromRunning; Policy + sentinels
  cmd/
    demo/
      main.go               parses an embedded go-version record and runs the policy gate
  provenance_test.go        table-driven parse + policy tests, Example with // Output
```

- Files: `provenance.go`, `cmd/demo/main.go`, `provenance_test.go`.
- Implement: `Parse(text)` and `Extract(*debug.BuildInfo)` normalizing the record into a `Provenance`, plus `Policy.Check` rejecting dirty source, missing VCS stamps, and local `replace` directives with `%w`-wrapped sentinels.
- Test: feed `debug.ParseBuildInfo` a fixed multi-line record; assert the toolchain version, main version, VCS settings, and dep checksums; assert each policy rejection.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/provenance/cmd/demo
cd ~/go-exercises/provenance
go mod init example.com/provenance
go mod edit -go=1.26
```

### Two ways to read the record, and the trap in one of them

Go exposes the embedded build record through two entry points in
`runtime/debug`. Inside a running program, `debug.ReadBuildInfo() (*debug.BuildInfo, bool)`
hands you the record directly. Against a binary on disk, `go version -m ./binary`
prints the record as text, and `debug.ParseBuildInfo(data string) (*debug.BuildInfo, error)`
parses that text back into the same struct. A provenance tool wants both: the
running path for a service that reports its own provenance, and the text path for
a pipeline that inspects an artifact it just built.

There is a real trap in `ParseBuildInfo` that you must handle. It recognizes the
`path`, `mod`, `dep`, `=>`, and `build` lines — but not the leading
`go\tgo1.26.0` line. So after parsing `go version -m` text, `BuildInfo.GoVersion`
is empty. `ReadBuildInfo` hides this by injecting `runtime.Version()` after
parsing, but `ParseBuildInfo` does not, and a naive extractor built on it reports
an empty toolchain version. `Parse` therefore recovers the version by scanning the
text for the `go\t` prefix itself. Driving unit tests through `ParseBuildInfo` on
fixed text (rather than `ReadBuildInfo` on the live process, which can return
`ok=false` under the test harness) is what makes this logic deterministic and
gate-quality.

### What the record contains and how the struct maps it

The text has one line per fact, tab-separated:

```text
go	go1.26.0
path	example.com/checkout
mod	example.com/checkout	v1.4.0	h1:mainsum==
dep	github.com/google/uuid	v1.6.0	h1:uuidsum==
=>	../lib	(devel)
build	vcs.revision=9c1f2b3a4d5e6f
build	vcs.modified=false
```

`bi.Main` is the main module (`debug.Module{Path, Version, Sum, Replace}`);
`bi.Deps` is the slice of dependency modules; `bi.Settings` is a slice of
`debug.BuildSetting{Key, Value}` carrying the `vcs.*`, `GOOS`, `GOARCH`,
`CGO_ENABLED`, and `-buildvcs` facts. A `=>` line immediately after a `dep`
attaches to it as `Module.Replace`. `Extract` flattens all of this into a
`Provenance` with a settings map for easy lookup and a `VCSInfo` that pulls the
three `vcs` facts into typed fields — crucially, `Modified` (from
`vcs.modified=true`) and `Stamped` (whether a `vcs.revision` is present at all).

### The policy is where trust decisions live

`Policy.Check` is the gate. Its default posture rejects three things. A build with
`vcs.modified=true` is dirty — its revision does not describe the bytes — so it is
`ErrDirtySource`. A build with no `vcs.revision` was made outside a repo or with
`-buildvcs=false`, so under `RequireVCS` it is `ErrNoVCSStamp`. A dependency
replaced by a local filesystem path (a `replace` directive pointing at `./`,
`../`, or an absolute path) means a build input came from outside the module cache
and the checksum database, so it is `ErrExternalReplace`. Each is a sentinel
wrapped with `%w`, so a caller classifies failures with `errors.Is` without
string matching.

Create `provenance.go`:

```go
package provenance

import (
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
)

var (
	ErrNoVCSStamp      = errors.New("provenance: build has no VCS revision stamp")
	ErrDirtySource     = errors.New("provenance: build made from modified (dirty) source tree")
	ErrExternalReplace = errors.New("provenance: dependency replaced by a path outside the module cache")
)

// Module is one node of the build's module graph.
type Module struct {
	Path    string
	Version string
	Sum     string // go.sum h1: dirhash, or "" when replaced by a local path
	Replace string // replacement module path, or "" if not replaced
}

// VCSInfo is the version-control provenance the toolchain stamps in.
type VCSInfo struct {
	System   string
	Revision string
	Time     string
	Modified bool
	Stamped  bool
}

// Provenance is the normalized, policy-checkable view of a build record.
type Provenance struct {
	GoVersion string
	Main      Module
	VCS       VCSInfo
	Deps      []Module
	Settings  map[string]string
}

// Extract normalizes a *debug.BuildInfo into a Provenance.
func Extract(bi *debug.BuildInfo) Provenance {
	p := Provenance{
		GoVersion: bi.GoVersion,
		Main:      toModule(&bi.Main),
		Settings:  make(map[string]string, len(bi.Settings)),
	}
	for _, d := range bi.Deps {
		p.Deps = append(p.Deps, toModule(d))
	}
	for _, s := range bi.Settings {
		p.Settings[s.Key] = s.Value
	}
	p.VCS = VCSInfo{
		System:   p.Settings["vcs"],
		Revision: p.Settings["vcs.revision"],
		Time:     p.Settings["vcs.time"],
		Modified: p.Settings["vcs.modified"] == "true",
		Stamped:  p.Settings["vcs.revision"] != "",
	}
	return p
}

func toModule(m *debug.Module) Module {
	out := Module{Path: m.Path, Version: m.Version, Sum: m.Sum}
	if m.Replace != nil {
		out.Replace = m.Replace.Path
	}
	return out
}

// Parse reads the textual `go version -m <binary>` record. It recovers the Go
// toolchain version itself, because debug.ParseBuildInfo does not parse the
// leading "go\t" line and leaves GoVersion empty.
func Parse(text string) (Provenance, error) {
	bi, err := debug.ParseBuildInfo(text)
	if err != nil {
		return Provenance{}, fmt.Errorf("parse build info: %w", err)
	}
	p := Extract(bi)
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, "go\t"); ok {
			p.GoVersion = strings.TrimSpace(v)
			break
		}
	}
	return p, nil
}

// FromRunning reads the current binary's own build record. Under a test harness
// ReadBuildInfo may report ok=false, which is why unit tests drive Parse instead.
func FromRunning() (Provenance, error) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return Provenance{}, errors.New("provenance: build info unavailable (built with -buildvcs=false or read under a test harness)")
	}
	return Extract(bi), nil
}

// Policy is the deploy gate's posture.
type Policy struct {
	RequireVCS           bool
	AllowDirty           bool
	AllowExternalReplace bool
}

// Check applies the policy, returning a %w-wrapped sentinel on the first failure.
func (pol Policy) Check(p Provenance) error {
	if pol.RequireVCS && !p.VCS.Stamped {
		return fmt.Errorf("policy: %w", ErrNoVCSStamp)
	}
	if !pol.AllowDirty && p.VCS.Modified {
		return fmt.Errorf("policy: revision %s: %w", p.VCS.Revision, ErrDirtySource)
	}
	if !pol.AllowExternalReplace {
		for _, d := range p.Deps {
			if isLocalReplace(d.Replace) {
				return fmt.Errorf("policy: %s => %s: %w", d.Path, d.Replace, ErrExternalReplace)
			}
		}
	}
	return nil
}

// isLocalReplace reports whether a replace target is a filesystem path (and thus
// outside the module cache and checksum database) rather than a module version.
func isLocalReplace(r string) bool {
	if r == "" {
		return false
	}
	return r == "." || r == ".." ||
		strings.HasPrefix(r, "./") ||
		strings.HasPrefix(r, "../") ||
		strings.HasPrefix(r, "/")
}
```

### The runnable demo

The demo embeds a real `go version -m` record as a string so its output is
deterministic. In a pipeline you would capture that string by running the command
against the artifact; embedding it keeps the demo self-contained and its output
stable. It parses the record, prints a provenance summary, and runs the gate.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/provenance"
)

// buildInfoText is the exact textual form `go version -m ./checkout` prints for a
// release build. Capturing it with that command is the real workflow; embedding
// it here keeps the demo output deterministic.
const buildInfoText = "go\tgo1.26.0\n" +
	"path\texample.com/checkout\n" +
	"mod\texample.com/checkout\tv1.4.0\th1:tYd9r6mmain==\n" +
	"dep\tgithub.com/google/uuid\tv1.6.0\th1:PXT1lz1t0z2Ffwtx==\n" +
	"dep\tgolang.org/x/crypto\tv0.31.0\th1:cryptoSum9182==\n" +
	"build\t-buildmode=exe\n" +
	"build\tCGO_ENABLED=0\n" +
	"build\tGOARCH=arm64\n" +
	"build\tGOOS=linux\n" +
	"build\tvcs=git\n" +
	"build\tvcs.revision=9c1f2b3a4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f90\n" +
	"build\tvcs.time=2026-06-01T12:00:00Z\n" +
	"build\tvcs.modified=false\n"

func main() {
	prov, err := provenance.Parse(buildInfoText)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Printf("toolchain: %s\n", prov.GoVersion)
	fmt.Printf("module:    %s@%s\n", prov.Main.Path, prov.Main.Version)
	fmt.Printf("revision:  %s (dirty=%v)\n", prov.VCS.Revision, prov.VCS.Modified)
	fmt.Printf("platform:  %s/%s cgo=%s\n", prov.Settings["GOOS"], prov.Settings["GOARCH"], prov.Settings["CGO_ENABLED"])
	fmt.Printf("deps:      %d\n", len(prov.Deps))
	for _, d := range prov.Deps {
		fmt.Printf("  - %s %s %s\n", d.Path, d.Version, d.Sum)
	}

	pol := provenance.Policy{RequireVCS: true}
	if err := pol.Check(prov); err != nil {
		fmt.Printf("policy:    REJECT (%v)\n", err)
		os.Exit(1)
	}
	fmt.Println("policy:    ACCEPT")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
toolchain: go1.26.0
module:    example.com/checkout@v1.4.0
revision:  9c1f2b3a4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f90 (dirty=false)
platform:  linux/arm64 cgo=0
deps:      2
  - github.com/google/uuid v1.6.0 h1:PXT1lz1t0z2Ffwtx==
  - golang.org/x/crypto v0.31.0 h1:cryptoSum9182==
policy:    ACCEPT
```

### Tests

`TestParseExtractsProvenance` proves the extraction against a fixed record,
including that `GoVersion` is recovered (the trap) and that a dependency's `h1:`
checksum survives. `TestPolicyCheck` is table-driven over the three rejections
plus the two accept paths, asserting each rejection with `errors.Is` against the
sentinel. The `Example` shows the dirty-source rejection producing its exact
wrapped message.

Create `provenance_test.go`:

```go
package provenance

import (
	"errors"
	"fmt"
	"testing"
)

const cleanBuild = "go\tgo1.26.0\n" +
	"path\texample.com/checkout\n" +
	"mod\texample.com/checkout\tv1.4.0\th1:mainsum==\n" +
	"dep\tgithub.com/google/uuid\tv1.6.0\th1:uuidsum==\n" +
	"dep\tgolang.org/x/crypto\tv0.31.0\th1:cryptosum==\n" +
	"build\tGOOS=linux\n" +
	"build\tvcs=git\n" +
	"build\tvcs.revision=9c1f2b3a4d5e6f\n" +
	"build\tvcs.time=2026-06-01T12:00:00Z\n" +
	"build\tvcs.modified=false\n"

func TestParseExtractsProvenance(t *testing.T) {
	t.Parallel()
	prov, err := Parse(cleanBuild)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if prov.GoVersion != "go1.26.0" {
		t.Errorf("GoVersion = %q, want go1.26.0 (ParseBuildInfo drops it; we recover it)", prov.GoVersion)
	}
	if prov.Main.Version != "v1.4.0" {
		t.Errorf("Main.Version = %q, want v1.4.0", prov.Main.Version)
	}
	if prov.VCS.Revision != "9c1f2b3a4d5e6f" || prov.VCS.Modified {
		t.Errorf("VCS = %+v, want revision set and Modified=false", prov.VCS)
	}
	if len(prov.Deps) != 2 {
		t.Fatalf("Deps = %d, want 2", len(prov.Deps))
	}
	if prov.Deps[0].Sum != "h1:uuidsum==" {
		t.Errorf("Deps[0].Sum = %q, want h1:uuidsum==", prov.Deps[0].Sum)
	}
}

func TestPolicyCheck(t *testing.T) {
	t.Parallel()
	dirty := "path\texample.com/checkout\n" +
		"mod\texample.com/checkout\t(devel)\t\n" +
		"build\tvcs.revision=abc123\n" +
		"build\tvcs.modified=true\n"
	noStamp := "path\texample.com/checkout\n" +
		"mod\texample.com/checkout\t(devel)\t\n" +
		"build\tGOOS=linux\n"
	localReplace := "path\texample.com/checkout\n" +
		"mod\texample.com/checkout\t(devel)\t\n" +
		"dep\texample.com/internal/lib\tv0.0.0\t\n" +
		"=>\t../lib\t(devel)\t\n" +
		"build\tvcs.revision=abc123\n" +
		"build\tvcs.modified=false\n"

	tests := []struct {
		name    string
		text    string
		policy  Policy
		wantErr error
	}{
		{"clean passes", cleanBuild, Policy{RequireVCS: true}, nil},
		{"dirty rejected", dirty, Policy{RequireVCS: true}, ErrDirtySource},
		{"missing stamp rejected", noStamp, Policy{RequireVCS: true}, ErrNoVCSStamp},
		{"local replace rejected", localReplace, Policy{RequireVCS: true}, ErrExternalReplace},
		{"dirty allowed when configured", dirty, Policy{RequireVCS: false, AllowDirty: true}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prov, err := Parse(tc.text)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = tc.policy.Check(prov)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Check = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Check = %v, want errors.Is %v", err, tc.wantErr)
			}
		})
	}
}

func ExamplePolicy_Check() {
	prov, _ := Parse("path\texample.com/app\n" +
		"mod\texample.com/app\t(devel)\t\n" +
		"build\tvcs.revision=deadbeef\n" +
		"build\tvcs.modified=true\n")
	err := Policy{RequireVCS: true}.Check(prov)
	fmt.Println(err)
	// Output: policy: revision deadbeef: provenance: build made from modified (dirty) source tree
}
```

## Review

The extractor is correct when the record round-trips faithfully: `Parse` must
recover `GoVersion` (because `ParseBuildInfo` silently drops the `go\t` line), the
main module and every dependency keep their versions and `h1:` sums, and a `=>`
line attaches to its dependency as `Replace`. The policy is correct when each
rejection fires on exactly its condition — dirty on `vcs.modified=true`, no-stamp
on an absent `vcs.revision` under `RequireVCS`, external-replace on a `./`, `../`,
or absolute replacement — and each is a `%w`-wrapped sentinel that `errors.Is`
classifies.

The mistakes to avoid: trusting `ReadBuildInfo` inside a test (it can return
`ok=false`), reporting an empty toolchain version because you leaned on
`ParseBuildInfo` for it, and treating any binary with a revision as trustworthy
without checking the modified flag. Run `go test -count=1 -race ./...`; the table
covers every branch of `Check`, so a regression in the gate fails a case rather
than passing silently.

## Resources

- [`runtime/debug` — ReadBuildInfo, ParseBuildInfo, BuildInfo](https://pkg.go.dev/runtime/debug) — the two entry points and the struct they return.
- [`go version` command documentation](https://pkg.go.dev/cmd/go#hdr-Print_Go_version) — the `-m` flag that prints the embedded module record.
- [Go Modules Reference: go.sum and the checksum database](https://go.dev/ref/mod#go-sum-files) — what the `h1:` value is and how it is verified.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-cyclonedx-sbom-from-modules.md](02-cyclonedx-sbom-from-modules.md)
