# Exercise 5: File-Name Suffixes as Implicit Build Constraints

A file named `foo_linux.go` carries an implicit `//go:build linux`, and
`foo_linux_amd64.go` implies both. This module puts the suffix style next to an
explicit `//go:build` expression, and demonstrates the sharp failure mode: a
mis-spelled suffix is not a constraint at all and compiles on every platform.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test. Nothing here imports another exercise.

## What you'll build

```text
suffixdemo/                    module example.com/suffixdemo
  go.mod                       package suffixdemo
  suffixdemo.go                registry; Notes(); ExpectedGoFiles(goos,goarch)
  note_freebsd.go              implicit _freebsd suffix, no //go:build line
  note_linux_amd64.go          implicit _linux_amd64 double suffix
  note_unix_amd64.go           explicit //go:build (linux || darwin) && amd64
  note_pluto.go                mis-spelled suffix: compiles EVERYWHERE (the bug)
  suffixdemo_test.go           asserts the mis-suffix leaks + the file set per target
  cmd/demo/main.go             prints the notes registered on the host
```

- Files: `suffixdemo.go`, `note_freebsd.go`, `note_linux_amd64.go`, `note_unix_amd64.go`, `note_pluto.go`, `cmd/demo/main.go`, `suffixdemo_test.go`.
- Implement: a small registry each constrained file writes to from `init()`, plus `ExpectedGoFiles(goos,goarch)` encoding which files compile per target.
- Test: assert the mis-suffixed file is compiled on the host (the failure mode) and that the expected file set matches per target.
- Verify: `go test -race ./...`, then `GOOS=linux GOARCH=386` versus `GOOS=linux GOARCH=amd64` `go list` to see the double-suffix and explicit files appear only for amd64.

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/08-cross-compilation-and-build-tags/05-file-suffix-implicit-constraints/cmd/demo
cd go-solutions/01-environment-and-tooling/08-cross-compilation-and-build-tags/05-file-suffix-implicit-constraints
```

### Three legitimate styles and one trap

Go recognizes a file's constraint from its name: the last underscore-separated
tokens before `.go`, if they are a known `GOOS`, a known `GOARCH`, or `GOOS_GOARCH`,
act as an implicit `//go:build`. `note_freebsd.go` compiles only on freebsd;
`note_linux_amd64.go` compiles only on linux/amd64. No comment line is needed, and
adding one is redundant. The moment the constraint stops being a plain conjunction
of one OS and/or one arch — say "(linux or darwin) and amd64" — the suffix cannot
express it, and you must write an explicit `//go:build (linux || darwin) && amd64`
on a normally-named file (`note_unix_amd64.go` here, whose suffix `_amd64` is real
but whose OS condition lives in the `//go:build` line). When a file has *both* a
suffix and a `//go:build`, the two are ANDed: both must hold.

The trap is `note_pluto.go`. "pluto" is not a `GOOS`, so `_pluto` is not
recognized as a constraint — the file is treated as an ordinary source file and
compiled on *every* platform. This is the silent inverse of the intended behavior:
you meant to scope the file to one platform, mis-typed the suffix (`_darvin`,
`_widows`, a made-up name), and now that code ships everywhere. Nothing warns you;
`go list` is the only way to see it.

Each constrained file registers a distinct note from its `init()`, so on any given
target the registry reflects exactly which files compiled. Because the files
register *different* strings (and each has its own `init`), the double-suffix file
and the explicit file can both be present on linux/amd64 without a symbol
collision.

Create `suffixdemo.go`:

```go
// Package suffixdemo contrasts implicit file-name build constraints with an
// explicit //go:build expression, and demonstrates the failure mode of a
// mis-spelled suffix that is silently compiled on every platform.
package suffixdemo

import "sort"

// registry collects notes contributed by the constrained files that compile for
// the current target. Each such file registers exactly one note from its init.
var registry []string

func register(note string) { registry = append(registry, note) }

// Notes returns the sorted set of notes registered for the current target.
func Notes() []string {
	out := append([]string(nil), registry...)
	sort.Strings(out)
	return out
}

// ExpectedGoFiles is the file set that compiles for a target, given that
// note_pluto.go has a mis-spelled (non-GOOS) suffix and so compiles everywhere.
func ExpectedGoFiles(goos, goarch string) []string {
	files := []string{"note_pluto.go", "suffixdemo.go"}
	if goos == "freebsd" {
		files = append(files, "note_freebsd.go")
	}
	if goos == "linux" && goarch == "amd64" {
		files = append(files, "note_linux_amd64.go")
	}
	if (goos == "linux" || goos == "darwin") && goarch == "amd64" {
		files = append(files, "note_unix_amd64.go")
	}
	sort.Strings(files)
	return files
}
```

Create `note_freebsd.go` — implicit single suffix, no `//go:build`:

```go
package suffixdemo

// This file is selected purely by its _freebsd suffix; there is no //go:build line.
func init() { register("freebsd: selected by the _freebsd file-name suffix") }
```

Create `note_linux_amd64.go` — implicit double suffix:

```go
package suffixdemo

// This file is selected by the _linux_amd64 double suffix (GOOS and GOARCH).
func init() { register("linux/amd64: selected by the _linux_amd64 double suffix") }
```

Create `note_unix_amd64.go` — explicit boolean expression the suffix cannot
express:

```go
//go:build (linux || darwin) && amd64

package suffixdemo

// This file needs a boolean expression, so it uses an explicit //go:build.
func init() { register("linux-or-darwin/amd64: selected by an explicit //go:build") }
```

Create `note_pluto.go` — the failure mode, compiled on every platform:

```go
package suffixdemo

// FAILURE MODE: "pluto" is not a valid GOOS, so _pluto is not a constraint at
// all. This file is compiled on every platform, the opposite of the intent.
func init() { register("misfiled: _pluto is not a GOOS, so this compiles on every platform") }
```

### The runnable demo

The demo prints the notes registered on the host. On any real platform the
misfiled note is always present — the whole point — plus whichever
correctly-suffixed files matched the host.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime"

	"example.com/suffixdemo"
)

func main() {
	fmt.Printf("host %s/%s registered notes:\n", runtime.GOOS, runtime.GOARCH)
	for _, n := range suffixdemo.Notes() {
		fmt.Printf("  - %s\n", n)
	}
}
```

Run it (output shown on darwin/arm64, where no correctly-suffixed file matches):

```bash
go run ./cmd/demo
```

Expected output on darwin/arm64:

```text
host darwin/arm64 registered notes:
  - misfiled: _pluto is not a GOOS, so this compiles on every platform
```

Confirm the double-suffix and explicit files appear only for amd64:

```bash
GOOS=linux GOARCH=386   go list -f '{{.GoFiles}}' .
GOOS=linux GOARCH=amd64 go list -f '{{.GoFiles}}' .
GOOS=freebsd GOARCH=amd64 go list -f '{{.GoFiles}}' .
```

```text
[note_pluto.go suffixdemo.go]
[note_linux_amd64.go note_pluto.go note_unix_amd64.go suffixdemo.go]
[note_freebsd.go note_pluto.go suffixdemo.go]
```

### The test

The first test pins the failure mode: the misfiled note must be present on the
host, proving a mis-spelled suffix leaks onto every platform. The second asserts
the file set per target, including that `linux/386` gets neither the double-suffix
nor the explicit file (both require amd64) while `linux/amd64` gets both.

Create `suffixdemo_test.go`:

```go
package suffixdemo

import (
	"slices"
	"strings"
	"testing"
)

func TestMisfiledSuffixCompilesEverywhere(t *testing.T) {
	t.Parallel()
	found := false
	for _, n := range Notes() {
		if strings.HasPrefix(n, "misfiled:") {
			found = true
		}
	}
	if !found {
		t.Fatal("note_pluto.go was not compiled on the host; a mis-spelled suffix must compile everywhere")
	}
}

func TestExpectedGoFiles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		goos, goarch string
		want         []string
	}{
		{"darwin", "arm64", []string{"note_pluto.go", "suffixdemo.go"}},
		{"freebsd", "amd64", []string{"note_freebsd.go", "note_pluto.go", "suffixdemo.go"}},
		{"linux", "386", []string{"note_pluto.go", "suffixdemo.go"}},
		{"linux", "amd64", []string{"note_linux_amd64.go", "note_pluto.go", "note_unix_amd64.go", "suffixdemo.go"}},
	}
	for _, c := range cases {
		if got := ExpectedGoFiles(c.goos, c.goarch); !slices.Equal(got, c.want) {
			t.Errorf("ExpectedGoFiles(%s,%s) = %v; want %v", c.goos, c.goarch, got, c.want)
		}
	}
}
```

## Review

The lesson lands when the `go list` output matches `ExpectedGoFiles` for each
target and the host build always compiles the misfiled `note_pluto.go`. The rule
to internalize: use the suffix when the constraint is exactly one GOOS and/or one
GOARCH (it is shorter and impossible to forget the blank line for), and use an
explicit `//go:build` for any boolean expression. The real-world defect this guards
against is the typo'd suffix — `_darvin.go`, `_windwos.go` — which silently
compiles everywhere; the only defense is auditing the per-target file set with
`go list`, exactly as the demo does.

## Resources

- [Build constraints: file name conventions (cmd/go)](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — how GOOS/GOARCH suffixes are matched.
- [`go/build` package: file name constraints](https://pkg.go.dev/go/build#hdr-Build_Constraints) — the exact suffix-parsing rules.
- [`go list`](https://pkg.go.dev/cmd/go#hdr-List_packages_or_modules) — auditing the selected file set per target.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-tagged-and-platform-tests.md](06-tagged-and-platform-tests.md)
