# Exercise 7: Stamping Version, Commit, and Build Date with -ldflags -X

CI stamps a version, git SHA, and build date into a binary at link time without
editing source, using `-ldflags -X`. This module builds the `internal/buildmeta`
package and `version` subcommand that a real service ships, and drives the exact
link-flag invocation the release job uses.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test. Nothing here imports another exercise.

## What you'll build

```text
app/                           module example.com/app
  go.mod
  internal/buildmeta/
    buildmeta.go               var Version/Commit/Date = "dev"; String(); stringOf()
    buildmeta_test.go          defaults are "dev"; format is stable
    example_test.go            ExampleString with // Output
  cmd/app/main.go              `version` subcommand prints buildmeta.String()
```

- Files: `internal/buildmeta/buildmeta.go`, `internal/buildmeta/buildmeta_test.go`, `internal/buildmeta/example_test.go`, `cmd/app/main.go`.
- Implement: exported package-level string vars defaulting to `dev`, a `String()` renderer, and a `version` subcommand.
- Test: assert the un-stamped defaults are `dev` and the render format is stable.
- Verify: `go test -race ./...`; a default `go run ./cmd/app version` prints `dev`; a `-ldflags -X` build then prints the injected values.

### Why the variables must be package-level strings

`-ldflags "-X importpath.name=value"` sets a variable at link time, and it works
on exactly one kind of target: a package-level `string` variable. It silently
no-ops on a `const`, an `int`, or a function-local — the linker only patches a
string symbol in the data section, and anything else is left untouched with no
error. So the metadata lives as `var Version, Commit, Date string`, initialized to
`dev`/`none`/`unknown` so a developer's plain `go build` is self-describing. The
key in the `-X` flag must be the *full import path*
(`example.com/app/internal/buildmeta.Version`), not the short package name
`buildmeta.Version` — a common mistake that produces a binary still showing `dev`
with no warning.

`String()` renders the three into one line. It delegates to an unexported
`stringOf` so a test can assert the layout on fixed inputs without mutating the
stamped globals. The `version` subcommand simply prints `String()` — this is the
`myapp version` you run to identify a binary in the field.

Create `internal/buildmeta/buildmeta.go`:

```go
// Package buildmeta holds release metadata stamped in at link time with
// -ldflags -X. The defaults ("dev") are what a plain `go build` produces; CI
// overrides them with the real version, commit, and build date.
package buildmeta

import "fmt"

// These are package-level string variables precisely because -ldflags -X can
// only set package-level strings. A const or an int here would be un-stampable.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String renders the metadata as a single line, e.g. "v1.4.0 (a1b2c3d) built 2026-07-02".
func String() string {
	return stringOf(Version, Commit, Date)
}

// stringOf renders arbitrary values in the same format as String, so a test can
// assert the layout without mutating the package-level stamped variables.
func stringOf(version, commit, date string) string {
	return fmt.Sprintf("%s (%s) built %s", version, commit, date)
}
```

Create `cmd/app/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/app/internal/buildmeta"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(buildmeta.String())
		return
	}
	fmt.Println("app running; try the 'version' subcommand")
}
```

### The release build

The stamped build is the on-the-job invocation: `-X` for each field, `-s -w` to
strip the symbol and DWARF tables (shrinking the binary), and `-trimpath` to
remove absolute build paths so the artifact is reproducible and does not leak
`$HOME`.

```bash
# default build: metadata is the dev placeholder
go run ./cmd/app version

# release build: inject real values, strip, trim paths
go build \
  -ldflags "-s -w \
    -X 'example.com/app/internal/buildmeta.Version=v1.4.0' \
    -X 'example.com/app/internal/buildmeta.Commit=$(git rev-parse --short HEAD)' \
    -X 'example.com/app/internal/buildmeta.Date=$(date -u +%Y-%m-%d)'" \
  -trimpath -o app ./cmd/app
./app version
```

Expected output — default run, then the stamped binary:

```text
dev (none) built unknown
v1.4.0 (a1b2c3d) built 2026-07-02
```

The `-s -w` strip is not cosmetic: on this toolchain the demo binary drops from
roughly 2.5 MB to 1.7 MB. Confirm it yourself:

```bash
go build -o app-plain ./cmd/app
go build -ldflags "-s -w" -o app-stripped ./cmd/app
ls -l app-plain app-stripped
```

(Exact sizes vary by toolchain and platform; the stripped binary is consistently
smaller.)

### The tests

`TestDefaultsAreDev` pins the un-stamped defaults (the state the gate builds in, no
`-X` flags), and `TestStringFormat` locks the render layout on fixed inputs. The
`Example` doubles as documentation and an auto-verified format check.

Create `internal/buildmeta/buildmeta_test.go`:

```go
package buildmeta

import (
	"strings"
	"testing"
)

func TestDefaultsAreDev(t *testing.T) {
	t.Parallel()
	if Version != "dev" {
		t.Errorf("Version = %q; want the un-stamped default %q", Version, "dev")
	}
	if !strings.HasPrefix(String(), "dev (") {
		t.Errorf("String() = %q; want it to start with the dev default", String())
	}
}

func TestStringFormat(t *testing.T) {
	t.Parallel()
	got := stringOf("v1.4.0", "a1b2c3d", "2026-07-02")
	want := "v1.4.0 (a1b2c3d) built 2026-07-02"
	if got != want {
		t.Errorf("stringOf = %q; want %q", got, want)
	}
}
```

Create `internal/buildmeta/example_test.go`:

```go
package buildmeta

import "fmt"

func ExampleString() {
	fmt.Println(stringOf("v2.1.0", "deadbee", "2026-01-15"))
	// Output: v2.1.0 (deadbee) built 2026-01-15
}
```

## Review

Stamping is correct when the default binary reports `dev` and the `-X` build
reports the injected values. The two failure modes to remember: using the short
package name in the `-X` key (the value silently does not take), and targeting a
non-string — a `const Version` or an `int BuildNumber` cannot be stamped at all,
also silently. Keep the stamped identifiers as package-level `var ... string`. Pair
`-X` with `-s -w` for size and `-trimpath` for reproducibility; the next exercise
reads a different, automatic form of this metadata (the embedded VCS info) back out
of a shipped binary.

## Resources

- [`cmd/link` flags: -X, -s, -w](https://pkg.go.dev/cmd/link) — what each linker flag does and the string-only rule for `-X`.
- [`go build` build flags: -ldflags, -trimpath (cmd/go)](https://pkg.go.dev/cmd/go#hdr-Compile_packages_and_dependencies) — passing linker flags and trimming paths.
- [Go 1.18 release notes: version control information in binaries](https://go.dev/doc/go1.18) — how the toolchain also records version and build metadata, complementing manual `-X` stamping.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-embedded-buildinfo-forensics.md](08-embedded-buildinfo-forensics.md)
