# Exercise 3: Auditing Which Files and Dependencies Enter a Binary

"What code is actually compiled into this platform's binary?" is a supply-chain
question, and `go list` is the tool that answers it per target. This module pairs
a live `go list -f '{{.GoFiles}}'` query with the expected answer, so intent can be
diffed against reality for every target.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test. Nothing here imports another exercise.

## What you'll build

```text
auditfiles/                    module example.com/auditfiles
  go.mod                       package audit
  audit.go                     Note(); ExpectedGoFiles(goos) []string
  platform_linux.go            //go:build linux    -> platformNote()
  platform_darwin.go           //go:build darwin   -> platformNote()
  platform_windows.go          //go:build windows  -> platformNote()
  platform_other.go            //go:build !linux && !darwin && !windows
  audit_test.go                asserts the expected file set per GOOS
  cmd/demo/main.go             runs `go list` per target and prints the real file set
```

- Files: `audit.go`, `platform_*.go`, `cmd/demo/main.go`, `audit_test.go`.
- Implement: platform files each contributing `platformNote()`, and `ExpectedGoFiles(goos)` returning the file set that should compile for a target.
- Test: assert the expected set is exactly `audit.go` plus the one selected `platform_*.go` for each GOOS.
- Verify: `go test -race ./...`, then `go run ./cmd/demo` to see the live `go list` output agree with the expected sets.

Set up the module:

```bash
mkdir -p ~/go-exercises/auditfiles/cmd/demo
cd ~/go-exercises/auditfiles
go mod init example.com/auditfiles
```

### go list as a GOOS-parameterized query

`go list -f '{{.GoFiles}}' <pkg>` prints the Go source files the toolchain would
compile for the current target, applying every build constraint. Prefix it with
`GOOS=` and `GOARCH=` and it reports the file set *for that target* without
building anything:

```bash
GOOS=linux   GOARCH=amd64 go list -f '{{.GoFiles}}' example.com/auditfiles
GOOS=darwin  GOARCH=arm64 go list -f '{{.GoFiles}}' example.com/auditfiles
GOOS=windows GOARCH=amd64 go list -f '{{.GoFiles}}' example.com/auditfiles
GOOS=freebsd GOARCH=amd64 go list -f '{{.GoFiles}}' example.com/auditfiles
```

Because exactly one `platform_*.go` satisfies its constraint per GOOS, the output
proves the selection is unambiguous: linux gets `platform_linux.go`, freebsd (with
no dedicated file) falls to `platform_other.go`, and `audit.go` is always present.
The companion query `go list -deps example.com/auditfiles` prints the transitive
dependency set — the evidence a supply-chain review needs for "which packages,
across all my dependencies, end up in this target's binary", which legitimately
differs across targets (a Windows build pulls in `internal/syscall/windows`, a
Linux build does not).

The `audit.go` file encodes the *expected* answer so a test can assert the audit
rather than eyeball it. `platformNote` is defined once per platform (guarded by
`//go:build`), with a negated fallback so every target compiles; `Note()` simply
surfaces the selected one so each platform file carries a real symbol `go list`
can be seen to include.

Create `audit.go`:

```go
// Package audit answers "which source files compile into this target's binary?"
// It pairs a live go list query (in the demo) with the expected answer per
// target, so a supply-chain review can diff intent against reality.
package audit

// Note reports the platform-selected note; it exists so each platform_*.go file
// carries a real symbol that go list -f '{{.GoFiles}}' can be seen to include.
func Note() string { return platformNote() }

// ExpectedGoFiles is the set of source files that should compile for a target.
// Exactly one platform_*.go is selected per GOOS; audit.go is always present.
func ExpectedGoFiles(goos string) []string {
	platform := "platform_other.go"
	switch goos {
	case "linux", "darwin", "windows":
		platform = "platform_" + goos + ".go"
	}
	return []string{"audit.go", platform}
}
```

Create `platform_linux.go`:

```go
//go:build linux

package audit

func platformNote() string { return "linux implementation" }
```

Create `platform_darwin.go`:

```go
//go:build darwin

package audit

func platformNote() string { return "darwin implementation" }
```

Create `platform_windows.go`:

```go
//go:build windows

package audit

func platformNote() string { return "windows implementation" }
```

Create `platform_other.go`:

```go
//go:build !linux && !darwin && !windows

package audit

func platformNote() string { return "fallback implementation" }
```

### The runnable demo

The demo runs the real audit: for each target it shells out to `go list`,
capturing the actual file set the toolchain reports, and prints it. This is the
live version of the `bash` queries above — the same data a CI audit step would
collect and archive with each release.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

var targets = []struct{ goos, goarch string }{
	{"linux", "amd64"},
	{"darwin", "arm64"},
	{"windows", "amd64"},
	{"freebsd", "amd64"},
}

func main() {
	for _, t := range targets {
		cmd := exec.Command("go", "list", "-f", "{{.GoFiles}}", "example.com/auditfiles")
		cmd.Env = append(os.Environ(), "GOOS="+t.goos, "GOARCH="+t.goarch)
		out, err := cmd.Output()
		if err != nil {
			fmt.Printf("%s/%s: error: %v\n", t.goos, t.goarch, err)
			continue
		}
		files := strings.Trim(strings.TrimSpace(string(out)), "[]")
		fmt.Printf("%s/%s: %s\n", t.goos, t.goarch, files)
	}
}
```

Run it from the module root:

```bash
go run ./cmd/demo
```

Expected output:

```text
linux/amd64: audit.go platform_linux.go
darwin/arm64: audit.go platform_darwin.go
windows/amd64: audit.go platform_windows.go
freebsd/amd64: audit.go platform_other.go
```

### The test

The test asserts the audit expectation for each GOOS: exactly `audit.go` plus the
one platform file that GOOS selects, with freebsd folding to the fallback. If a
new platform file is added without updating the expectation, or a constraint is
mistyped so two files match, the table catches the drift.

Create `audit_test.go`:

```go
package audit

import (
	"slices"
	"testing"
)

func TestExpectedGoFiles(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"linux":   {"audit.go", "platform_linux.go"},
		"darwin":  {"audit.go", "platform_darwin.go"},
		"windows": {"audit.go", "platform_windows.go"},
		"freebsd": {"audit.go", "platform_other.go"},
	}
	for goos, want := range cases {
		if got := ExpectedGoFiles(goos); !slices.Equal(got, want) {
			t.Errorf("ExpectedGoFiles(%q) = %v; want %v", goos, got, want)
		}
	}
}

func TestNoteIsNonEmpty(t *testing.T) {
	t.Parallel()
	if Note() == "" {
		t.Fatal("Note() empty; no platform file selected on host")
	}
}
```

## Review

The audit is correct when the demo's live `go list` output matches
`ExpectedGoFiles` for every target and `Note()` is non-empty on the host (proving
one platform file was selected). The value of `go list` over reading the source
tree by eye is that it applies the *real* constraint logic: a file whose
`//go:build` line lost its blank line, or a mis-spelled `_darvin` suffix, shows up
immediately as an unexpected file in the wrong target's set. Pair `go list -f
'{{.GoFiles}}'` with `go list -deps` when the question is not just "my files" but
"every package in the transitive closure", which is the supply-chain review a
release needs before it ships.

## Resources

- [`go list` documentation (cmd/go)](https://pkg.go.dev/cmd/go#hdr-List_packages_or_modules) — the `-f` template fields including `.GoFiles` and `.Deps`.
- [Build constraints (cmd/go)](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — how `go list` applies GOOS/GOARCH to file selection.
- [`text/template`](https://pkg.go.dev/text/template) — the template language `go list -f` uses.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-custom-build-tag-feature-flag.md](04-custom-build-tag-feature-flag.md)
