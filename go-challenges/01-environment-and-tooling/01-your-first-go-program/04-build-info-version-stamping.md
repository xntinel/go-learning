# Exercise 4: Stamp and read binary version metadata

During an incident, the first question is "what commit is this process running?"
A binary that can answer that in one flag saves minutes when minutes matter. This
module builds a version-selection function with a well-defined fallback ladder,
tests it by injecting synthetic build info, and shows how to stamp and read the
metadata from the outside.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
buildinfo/                  independent module: example.com/buildinfo
  go.mod                    go 1.26
  version.go                SelectVersion (pure ladder) + VersionString (reads real build info)
  cmd/demo/main.go          runnable demo of the selection ladder with synthetic inputs
  version_test.go           injects a synthetic *debug.BuildInfo across the ladder
```

Files: `version.go`, `cmd/demo/main.go`, `version_test.go`.
Implement: `SelectVersion(ldVersion string, info *debug.BuildInfo, ok bool)` that
prefers an ldflags value, then `Main.Version`, then the truncated `vcs.revision`
(marked `+dirty` when `vcs.modified` is true), then a `dev` placeholder; plus a
thin `VersionString()` that reads the real build info.
Test: drive every rung of the ladder by passing a synthetic `*debug.BuildInfo`,
so `debug.ReadBuildInfo` is never called in the unit test.
Verify: `go test -count=1 ./...`, `gofmt -l` empty; a shell harness confirms
`-ldflags '-X ...'` and `go version -m`.

### Why the logic is split from ReadBuildInfo

`debug.ReadBuildInfo()` reads metadata the linker embedded into the running
binary; you cannot make it return a chosen value in a unit test. So the testable
design separates the *decision* from the *source*: `SelectVersion` is a pure
function that takes the ldflags string and a `*debug.BuildInfo` (plus the `ok`
bool `ReadBuildInfo` returns) and applies the fallback ladder; `VersionString`
is the two-line glue that calls `ReadBuildInfo` and forwards to `SelectVersion`.
The test constructs a synthetic `&debug.BuildInfo{...}` for each rung and asserts
the result, never touching the real build info. This is the same "extract the
decision, leave the I/O as glue" move as the exit-code policy in Exercise 3.

The ladder, in priority order:

1. **ldflags override.** If `-ldflags '-X main.version=...'` set a non-empty,
   non-`dev` string, use it. This is how release automation stamps a value that
   is not in the module graph — a build number, a channel, an image digest.
2. **Module version.** `info.Main.Version` is the tagged module version (for
   example `v1.4.0`) when the binary was built from a released module. It is the
   literal `(devel)` for an untagged local build, which the ladder skips.
3. **VCS revision.** `info.Settings` carries `vcs.revision` and `vcs.modified`
   when the build happened in a Git checkout. The revision is truncated to 12
   characters (the conventional short SHA) and suffixed `+dirty` when the working
   tree had uncommitted changes, which is exactly the "is this an unclean build?"
   signal you want during forensics.
4. **Placeholder.** If none of the above is present, `dev`.

Create `version.go`:

```go
package buildinfo

import "runtime/debug"

// SelectVersion applies the version fallback ladder. It is pure: pass the
// ldflags-injected string, the *debug.BuildInfo, and the ok bool that
// debug.ReadBuildInfo returns, and it decides the version to display.
func SelectVersion(ldVersion string, info *debug.BuildInfo, ok bool) string {
	if ldVersion != "" && ldVersion != "dev" {
		return ldVersion
	}
	if ok && info != nil {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
		var revision string
		var modified bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
		if revision != "" {
			if len(revision) > 12 {
				revision = revision[:12]
			}
			if modified {
				return revision + "+dirty"
			}
			return revision
		}
	}
	return "dev"
}

// version is overridable at link time: -ldflags '-X main-style var'. Here it
// lives in this package so SelectVersion can be exercised standalone.
var version = "dev"

// VersionString is the production entry point: it reads the real embedded build
// info and forwards to the pure SelectVersion ladder.
func VersionString() string {
	info, ok := debug.ReadBuildInfo()
	return SelectVersion(version, info, ok)
}
```

### The runnable demo

The demo drives each rung of the ladder with synthetic inputs, so the output is
deterministic and shows exactly what a caller would see in each situation.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime/debug"

	"example.com/buildinfo"
)

func main() {
	// 1. ldflags override wins outright.
	fmt.Println(buildinfo.SelectVersion("1.2.3", nil, false))

	// 2. Tagged module version.
	tagged := &debug.BuildInfo{Main: debug.Module{Version: "v1.4.0"}}
	fmt.Println(buildinfo.SelectVersion("dev", tagged, true))

	// 3. VCS revision, dirty working tree.
	vcs := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef0123"},
		{Key: "vcs.modified", Value: "true"},
	}}
	fmt.Println(buildinfo.SelectVersion("dev", vcs, true))

	// 4. Nothing available.
	fmt.Println(buildinfo.SelectVersion("dev", nil, false))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
1.2.3
v1.4.0
0123456789ab+dirty
dev
```

### Tests

Create `version_test.go`:

```go
package buildinfo

import (
	"fmt"
	"runtime/debug"
	"testing"
)

func TestSelectVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		ldVersion string
		info      *debug.BuildInfo
		ok        bool
		want      string
	}{
		{
			name:      "ldflags override wins",
			ldVersion: "2.0.0",
			info:      &debug.BuildInfo{Main: debug.Module{Version: "v1.4.0"}},
			ok:        true,
			want:      "2.0.0",
		},
		{
			name:      "module version when no ldflags",
			ldVersion: "dev",
			info:      &debug.BuildInfo{Main: debug.Module{Version: "v1.4.0"}},
			ok:        true,
			want:      "v1.4.0",
		},
		{
			name:      "devel module version falls through to revision",
			ldVersion: "dev",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "0123456789abcdef0123"},
					{Key: "vcs.modified", Value: "false"},
				},
			},
			ok:   true,
			want: "0123456789ab",
		},
		{
			name:      "dirty working tree is suffixed",
			ldVersion: "dev",
			info: &debug.BuildInfo{
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "abcdefabcdefabcdef"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			ok:   true,
			want: "abcdefabcdef+dirty",
		},
		{
			name:      "no info falls back to dev",
			ldVersion: "dev",
			info:      nil,
			ok:        false,
			want:      "dev",
		},
		{
			name:      "short revision is not truncated",
			ldVersion: "dev",
			info: &debug.BuildInfo{
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}},
			},
			ok:   true,
			want: "abc123",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SelectVersion(tc.ldVersion, tc.info, tc.ok); got != tc.want {
				t.Fatalf("SelectVersion(%q, ...) = %q, want %q", tc.ldVersion, got, tc.want)
			}
		})
	}
}

func ExampleSelectVersion() {
	info := &debug.BuildInfo{Main: debug.Module{Version: "v3.1.0"}}
	fmt.Println(SelectVersion("dev", info, true))
	// Output: v3.1.0
}
```

To observe the metadata from outside, a shell harness stamps and reads it:

```bash
# Force a value that is not in the module graph.
go build -trimpath -ldflags='-X example.com/buildinfo.version=1.9.0' -o bin/demo ./cmd/demo

# Read the embedded module and VCS metadata back out of the finished binary.
go version -m bin/demo | grep -E 'path|vcs.revision'
```

## Review

The version logic is correct when the ladder is exact and pure. Each rung is
asserted by a synthetic `*debug.BuildInfo`, so the test never depends on how or
where the binary was built — that is what makes it deterministic. The two subtle
rungs are the `(devel)` skip (an untagged local build must fall through to the
revision, not display the literal `(devel)`) and the `+dirty` suffix (an unclean
working tree must be visibly marked, because a build from uncommitted code is a
forensics landmine).

The trap that ruins this in the field is the ldflags target. `-X` rewrites a
string variable identified by its full path; naming any package other than the
one that declares `version` silently does nothing and you ship `dev` believing
you stamped a release. Confirm the target with `go version -m` on the built
binary, which reads the same metadata the process sees. Do not call
`debug.ReadBuildInfo` inside the unit test; inject a synthetic value instead.

## Resources

- [runtime/debug.ReadBuildInfo](https://pkg.go.dev/runtime/debug#ReadBuildInfo) — reading embedded module and VCS metadata at runtime.
- [runtime/debug.BuildInfo](https://pkg.go.dev/runtime/debug#BuildInfo) — the `Main` module and `Settings` fields.
- [cmd/go: build and test flags](https://pkg.go.dev/cmd/go#hdr-Build_and_test_caching) — where `-ldflags` and `-trimpath` are documented.
- [go version -m](https://pkg.go.dev/cmd/go#hdr-Print_Go_version) — reading build metadata back out of a binary.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-urlcheck-command-adapter.md](03-urlcheck-command-adapter.md) | Next: [05-error-contract-is-as-join.md](05-error-contract-is-as-join.md)
