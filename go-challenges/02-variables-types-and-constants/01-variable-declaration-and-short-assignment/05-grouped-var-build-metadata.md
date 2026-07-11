# Exercise 5: Grouped var Blocks for Build and Version Metadata

Every deployable Go binary answers "what version am I?" through a set of
package-level string `var`s patched at link time with `-ldflags "-X"`. This
exercise builds that `buildinfo` package: a grouped `var` block for the cohesive
Version/Commit/BuildTime set, and a `runtime/debug.ReadBuildInfo` fallback for when
no ldflags were supplied.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
buildinfo/                     independent module: example.com/buildinfo
  go.mod                       module example.com/buildinfo
  buildinfo.go                 var (Version, Commit, BuildTime); Info; Resolve; ResolveFrom
  cmd/
    demo/
      main.go                  resolves from synthetic build info, deterministic
  buildinfo_test.go            fallback fills Commit/BuildTime; ldflags-set wins; String()
```

- Files: `buildinfo.go`, `cmd/demo/main.go`, `buildinfo_test.go`.
- Implement: a grouped `var` block of default metadata, an `Info` summary with `String()`, `Resolve()` reading live build info, and a pure `ResolveFrom` that fills `Commit`/`BuildTime` from `debug.BuildInfo.Settings` only when the version is still the default.
- Test: fallback fills from `Settings`; an ldflags-set version ignores build info; defaults returned when build info is unavailable; `String()` formats every field.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/buildinfo/cmd/demo
cd ~/go-exercises/buildinfo
go mod init example.com/buildinfo
```

### Why these must be package-level string `var`s

Build metadata is injected at link time:

```bash
go build -ldflags "-X example.com/buildinfo.Version=1.4.2 -X example.com/buildinfo.Commit=$(git rev-parse HEAD)"
```

The linker's `-X importpath.name=value` flag can only patch a *package-level*
variable of type *string*. Not a `const` — a `const` is folded into the code at
compile time and there is nothing left to patch. Not a non-string — `-X` writes
raw string bytes and silently does nothing to an `int` or `bool`. Not a local — the
linker addresses the symbol by import path and name. So the declaration form is not
a preference; it is the only form the mechanism accepts. Getting this wrong fails
*silently*: the build succeeds and the variable keeps its default, which is why the
fallback below matters.

### Why a grouped `var` block

The three fields are one cohesive concept — the identity of this build — so they
live in one grouped `var` block. Grouping communicates that they belong together
and are meant to be set together, and it keeps each with a sane default so a binary
built with no ldflags at all still reports *something* honest rather than an empty
string.

```go
var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = "unknown"
)
```

### The ReadBuildInfo fallback

When no ldflags are supplied — a `go run`, a `go install` from a checkout, a
`go build` without the flags — `Version` is still `"dev"`. In that case the package
consults `runtime/debug.ReadBuildInfo()`, whose `Settings` carry the VCS stamp the
Go toolchain records for modules built from a version-control checkout:
`vcs.revision`, `vcs.time`, and `vcs.modified`. The resolver only consults build
info when the version is the default, so an explicit ldflags version always wins
and is never overwritten by whatever the local checkout looks like.

`ReadBuildInfo` returns `(*debug.BuildInfo, bool)`; the `ok` guard is mandatory
because a binary built in some unusual ways has no embedded build info at all. The
resolution logic is split into a pure `ResolveFrom` that takes the inputs
explicitly, so it is deterministically testable, and a thin `Resolve` that supplies
the live values.

Create `buildinfo.go`:

```go
package buildinfo

import (
	"fmt"
	"runtime/debug"
)

// Version, Commit, and BuildTime are the cohesive build-identity set. They are
// package-level string vars because go build -ldflags "-X" can only patch that
// form. Defaults keep an unstamped binary honest.
var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = "unknown"
)

// Info is a resolved, self-describing build summary.
type Info struct {
	Version   string
	Commit    string
	BuildTime string
	Modified  bool
}

// String renders the summary, flagging a dirty working tree.
func (i Info) String() string {
	dirty := ""
	if i.Modified {
		dirty = " (modified)"
	}
	return fmt.Sprintf("%s commit %s built %s%s", i.Version, i.Commit, i.BuildTime, dirty)
}

// Resolve returns the build summary, using ldflags-set vars when present and the
// runtime/debug VCS fallback otherwise.
func Resolve() Info {
	info, ok := debug.ReadBuildInfo()
	return ResolveFrom(Version, Commit, BuildTime, info, ok)
}

// ResolveFrom is the pure core of Resolve. When version is still the "dev"
// default, it fills Commit/BuildTime/Modified from the build info's VCS settings;
// otherwise it trusts the (ldflags-set) inputs and ignores build info.
func ResolveFrom(version, commit, buildTime string, info *debug.BuildInfo, ok bool) Info {
	out := Info{Version: version, Commit: commit, BuildTime: buildTime}
	if version != "dev" || !ok || info == nil {
		return out
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			out.Commit = s.Value
		case "vcs.time":
			out.BuildTime = s.Value
		case "vcs.modified":
			out.Modified = s.Value == "true"
		}
	}
	return out
}
```

### The runnable demo

`Resolve()` reads live build info, which differs by environment, so the demo drives
the deterministic `ResolveFrom` with a synthetic `*debug.BuildInfo` to produce a
stable, checkable output. In a real binary you would call `Resolve()`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime/debug"

	"example.com/buildinfo"
)

func main() {
	// No ldflags, no build info: pure defaults.
	fmt.Println(buildinfo.ResolveFrom("dev", "none", "unknown", nil, false))

	// No ldflags, but VCS build info is present: fallback fills it in.
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abc1234"},
		{Key: "vcs.time", Value: "2026-07-02T10:00:00Z"},
		{Key: "vcs.modified", Value: "true"},
	}}
	fmt.Println(buildinfo.ResolveFrom("dev", "none", "unknown", info, true))

	// ldflags set the version: build info is ignored.
	fmt.Println(buildinfo.ResolveFrom("1.4.2", "deadbeef", "2026-07-01T09:00:00Z", info, true))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
dev commit none built unknown
dev commit abc1234 built 2026-07-02T10:00:00Z (modified)
1.4.2 commit deadbeef built 2026-07-01T09:00:00Z
```

The middle line shows the VCS fallback filling `Commit`/`BuildTime` and flagging a
dirty tree; the last line shows an ldflags-set version taking precedence and the
build info being ignored.

### Tests

Create `buildinfo_test.go`:

```go
package buildinfo

import (
	"fmt"
	"runtime/debug"
	"testing"
)

func vcsInfo() *debug.BuildInfo {
	return &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abc1234"},
		{Key: "vcs.time", Value: "2026-07-02T10:00:00Z"},
		{Key: "vcs.modified", Value: "false"},
	}}
}

func TestResolveFromUsesVCSFallback(t *testing.T) {
	t.Parallel()
	got := ResolveFrom("dev", "none", "unknown", vcsInfo(), true)

	want := Info{
		Version:   "dev",
		Commit:    "abc1234",
		BuildTime: "2026-07-02T10:00:00Z",
		Modified:  false,
	}
	if got != want {
		t.Fatalf("ResolveFrom = %+v, want %+v", got, want)
	}
}

func TestResolveFromFlagsModified(t *testing.T) {
	t.Parallel()
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.modified", Value: "true"},
	}}
	if got := ResolveFrom("dev", "none", "unknown", info, true); !got.Modified {
		t.Fatalf("Modified = false, want true")
	}
}

func TestResolveFromLdflagsWins(t *testing.T) {
	t.Parallel()
	// A set version must ignore VCS info entirely.
	got := ResolveFrom("1.4.2", "deadbeef", "2026-07-01T09:00:00Z", vcsInfo(), true)
	if got.Commit != "deadbeef" || got.BuildTime != "2026-07-01T09:00:00Z" {
		t.Fatalf("ldflags values were overwritten: %+v", got)
	}
}

func TestResolveFromNoBuildInfo(t *testing.T) {
	t.Parallel()
	got := ResolveFrom("dev", "none", "unknown", nil, false)
	want := Info{Version: "dev", Commit: "none", BuildTime: "unknown"}
	if got != want {
		t.Fatalf("ResolveFrom(no info) = %+v, want defaults %+v", got, want)
	}
}

func TestInfoString(t *testing.T) {
	t.Parallel()
	i := Info{Version: "1.0.0", Commit: "abc", BuildTime: "now", Modified: true}
	if got, want := i.String(), "1.0.0 commit abc built now (modified)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func ExampleResolveFrom() {
	fmt.Println(ResolveFrom("dev", "none", "unknown", nil, false))
	// Output: dev commit none built unknown
}
```

`TestResolveFromLdflagsWins` is
the guard that a real release version is never clobbered by whatever the build
checkout looks like.

## Review

The package is correct when metadata is patchable and honest. The three fields are
package-level string `var`s (the only `-ldflags -X` target) grouped to signal one
cohesive set, each with a default so an unstamped build still says something true.
`ResolveFrom` consults `debug.BuildInfo.Settings` only when `Version` is `"dev"`, so
an ldflags-set version always wins; the `ok` guard handles binaries with no embedded
build info.

The mistakes to avoid: making these `const` or non-string (silently un-patchable),
letting the fallback overwrite an explicit version, and forgetting the `ok` guard on
`ReadBuildInfo`. Run `go test -race`; then try a real `go build -ldflags "-X ..."`
and call `Resolve()` to see the injected values.

## Resources

- [runtime/debug.ReadBuildInfo and BuildInfo.Settings](https://pkg.go.dev/runtime/debug#ReadBuildInfo)
- [cmd/link: -X flag documentation](https://pkg.go.dev/cmd/link)
- [Go Modules Reference: build metadata / VCS stamping](https://go.dev/ref/mod)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-comma-ok-cache-lookup.md](04-comma-ok-cache-lookup.md) | Next: [06-blank-identifier-interface-guards.md](06-blank-identifier-interface-guards.md)
